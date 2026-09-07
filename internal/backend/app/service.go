package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type IDGenerator func(prefix string) string

const (
	admittedEffectTimeout = 5 * time.Minute
	settlementTimeout     = 30 * time.Second
)

type Service struct {
	directoryBrowser ports.DirectoryBrowser
	store            Store
	providers        ports.ProviderRegistry
	workspaceHost    ports.WorkspaceHost
	historySources   ports.HistorySourceRegistry
	shellHost        ports.ShellHost
	programHost      ports.ProgramHost
	now              func() time.Time
	newID            IDGenerator
	accessLease      time.Duration
	agentAPIEndpoint string
	callbackIngress  ports.CallbackIngress
	automationFacts  []ports.AutomationFactSource

	runtimeMu       sync.RWMutex
	runtimes        map[model.ExecutionID]ports.Runtime
	hostRuntimes    map[model.ExecutionID]ports.HostRuntime
	programRuntimes map[model.ExecutionID]ports.ProgramRuntime
}

func New(store Store, providers ports.ProviderRegistry) *Service {
	return &Service{
		store: store, providers: providers, now: time.Now, newID: randomID, accessLease: 24 * time.Hour,
		runtimes: make(map[model.ExecutionID]ports.Runtime), hostRuntimes: make(map[model.ExecutionID]ports.HostRuntime), programRuntimes: make(map[model.ExecutionID]ports.ProgramRuntime),
	}
}

func (s *Service) WithClock(now func() time.Time) *Service { s.now = now; return s }

func (s *Service) WithIDGenerator(generate IDGenerator) *Service { s.newID = generate; return s }

func (s *Service) WithWorkspaceHost(host ports.WorkspaceHost) *Service {
	s.workspaceHost = host
	return s
}

func (s *Service) WithHistorySources(sources ports.HistorySourceRegistry) *Service {
	s.historySources = sources
	return s
}

func (s *Service) WithShellHost(host ports.ShellHost) *Service {
	s.shellHost = host
	return s
}

func (s *Service) WithProgramHost(host ports.ProgramHost) *Service {
	s.programHost = host
	return s
}

func (s *Service) WithAccessLease(lease time.Duration) *Service {
	if lease > 0 {
		s.accessLease = lease
	}
	return s
}

// WithAgentAPIEndpoint supplies the explicit private Unix-socket path that a
// cohesive provider exposes to its exact workload as TCLAUDE_BACKEND_SOCKET.
func (s *Service) WithAgentAPIEndpoint(endpoint string) *Service {
	s.agentAPIEndpoint = endpoint
	return s
}

func (s *Service) WithCallbackIngress(ingress ports.CallbackIngress) *Service {
	s.callbackIngress = ingress
	return s
}

// WithAutomationFactSources registers composition-owned read-only collectors.
// Collection runs inside the existing bounded reconciliation worker; it does
// not create another scheduler or expose fact ingestion over the public API.
func (s *Service) WithAutomationFactSources(sources ...ports.AutomationFactSource) *Service {
	s.automationFacts = append([]ports.AutomationFactSource(nil), sources...)
	return s
}

func randomID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("generate id: %v", err))
	}
	return prefix + hex.EncodeToString(value[:])
}

func (s *Service) CreateAgent(ctx context.Context, req CreateAgentRequest) (AgentResult, error) {
	if err := requireOperator(req.Context); err != nil {
		return AgentResult{}, err
	}
	var selectionErr error
	req.ConfigurationProfile, selectionErr = s.selectConfigurationDefault(ctx, req.ConfigurationDefault, req.Desired, req.ConfigurationProfile)
	if selectionErr != nil {
		return AgentResult{}, selectionErr
	}
	req.Desired, req.ConfigurationProfile, selectionErr = s.resolveConfigurationSelection(ctx, req.Desired, req.ConfigurationProfile)
	if selectionErr != nil {
		return AgentResult{}, selectionErr
	}

	if err := req.ID.Validate(); err != nil {
		return AgentResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.Name) == "" {
		return AgentResult{}, fail(ErrInvalid, "agent name is required")
	}
	if err := validateDesired(req.Desired); err != nil {
		return AgentResult{}, err
	}
	if err := validateAgentMetadata(req.TaskReference, req.Notifications); err != nil {
		return AgentResult{}, err
	}
	if req.ParentAgentID == req.ID || req.CloneSourceAgentID == req.ID {
		return AgentResult{}, fail(ErrInvalid, "agent lineage cannot reference itself")
	}
	for _, related := range []model.AgentID{req.ParentAgentID, req.CloneSourceAgentID} {
		if related != "" {
			if _, err := s.store.Agent(ctx, related); err != nil {
				return AgentResult{}, fail(ErrInvalid, "lineage agent %s does not exist", related)
			}
		}
	}
	if req.Notifications.DirectMessage == "" {
		req.Notifications.DirectMessage = model.NotificationIfAvailable
	}
	now := s.now().UTC()
	agent := model.Agent{ID: req.ID, Name: req.Name, TaskReference: req.TaskReference, ParentAgentID: req.ParentAgentID, CloneSourceAgentID: req.CloneSourceAgentID, Lifecycle: model.AgentActive, Notifications: req.Notifications, Desired: req.Desired, ConfigurationProfile: req.ConfigurationProfile, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Agent: agent}, nil
}

func (s *Service) UpdateAgent(ctx context.Context, req UpdateAgentRequest) (AgentResult, error) {
	var selectionErr error
	req.ConfigurationProfile, selectionErr = s.selectConfigurationDefault(ctx, req.ConfigurationDefault, req.Desired, req.ConfigurationProfile)
	if selectionErr != nil {
		return AgentResult{}, selectionErr
	}
	req.Desired, req.ConfigurationProfile, selectionErr = s.resolveConfigurationSelection(ctx, req.Desired, req.ConfigurationProfile)
	if selectionErr != nil {
		return AgentResult{}, selectionErr
	}

	if req.ExpectedRevision == 0 {
		return AgentResult{}, fail(ErrInvalid, "expected revision is required")
	}
	if strings.TrimSpace(req.Name) == "" {
		return AgentResult{}, fail(ErrInvalid, "agent name is required")
	}
	if err := validateDesired(req.Desired); err != nil {
		return AgentResult{}, err
	}
	if err := validateAgentMetadata(req.TaskReference, req.Notifications); err != nil {
		return AgentResult{}, err
	}
	if req.Notifications.DirectMessage == "" {
		current, err := s.store.Agent(ctx, req.ID)
		if err != nil {
			return AgentResult{}, err
		}
		req.Notifications = current.Notifications
	}
	authority := model.AuthorityRequest{Principal: req.Context, Action: model.ActionUpdateConfiguration, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: req.ID}, RequestedConfiguration: &req.Desired}
	agent, err := s.store.UpdateAgent(ctx, req.ID, req.ExpectedRevision, req.Name, req.TaskReference, req.Notifications, req.Desired, req.ConfigurationProfile, authority, s.now().UTC())
	return AgentResult{Agent: agent}, err
}

func (s *Service) CreateGroup(ctx context.Context, req CreateGroupRequest) (GroupResult, error) {
	if err := requireOperator(req.Context); err != nil {
		return GroupResult{}, err
	}
	if err := req.ID.Validate(); err != nil {
		return GroupResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.Name) == "" {
		return GroupResult{}, fail(ErrInvalid, "group name is required")
	}
	seen := make(map[model.AgentID]struct{}, len(req.Members))
	for _, id := range req.Members {
		if _, duplicate := seen[id]; duplicate {
			return GroupResult{}, fail(ErrInvalid, "duplicate group member %s", id)
		}
		seen[id] = struct{}{}
		if _, err := s.store.Agent(ctx, id); err != nil {
			return GroupResult{}, err
		}
	}
	now := s.now().UTC()
	if req.OwnerAgentID != "" {
		if _, ok := seen[req.OwnerAgentID]; !ok {
			return GroupResult{}, fail(ErrInvalid, "group owner must be a member")
		}
	}
	group := model.Group{ID: req.ID, Name: req.Name, Members: append([]model.AgentID(nil), req.Members...), OwnerAgentID: req.OwnerAgentID, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateGroup(ctx, group, req.OwnerBounds); err != nil {
		return GroupResult{}, err
	}
	return GroupResult{Group: group}, nil
}

func (s *Service) Launch(ctx context.Context, req LaunchRequest) (OperationResult, error) {
	return s.launch(ctx, req, model.OperationLaunch, nil, launchOptions{})
}

type launchOptions struct {
	intent      ports.StartIntent
	history     *ports.HistorySourceSelection
	operationID model.OperationID
}

func (s *Service) launch(ctx context.Context, req LaunchRequest, kind model.OperationKind, continuationRecord *ContinuationRecord, options launchOptions) (OperationResult, error) {
	if len(req.InitialMessage) > 32*1024 || !utf8.ValidString(req.InitialMessage) || strings.ContainsRune(req.InitialMessage, 0) {
		return OperationResult{}, fail(ErrInvalid, "initial message must be valid UTF-8 without NUL and at most 32768 bytes")
	}
	if req.InitialMessage != "" && (kind != model.OperationLaunch || continuationRecord != nil) {
		return OperationResult{}, fail(ErrInvalid, "initial message is only supported for an explicit fresh launch")
	}

	if err := validateEffectContext(req.RequestContext); err != nil {
		return OperationResult{}, err
	}
	if (req.Target.Agent == nil) == (req.Target.Standalone == nil) {
		return OperationResult{}, fail(ErrInvalid, "exactly one launch target is required")
	}
	var initialDigest string
	if req.InitialMessage != "" {
		initialDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(req.InitialMessage)))
	}
	if kind == model.OperationLaunch {
		var targetAgent model.AgentID
		if req.Target.Agent != nil {
			targetAgent = req.Target.Agent.AgentID
			if targetAgent.Validate() != nil || req.Target.Agent.ExpectedRevision == 0 {
				return OperationResult{}, ErrInvalid
			}
		}
		prior, repeated, err := s.store.FindLaunchAdmission(ctx, LaunchRetryLookup{Context: req.RequestContext, At: s.now().UTC(), Kind: kind, AgentID: targetAgent, InitialMessageDigest: initialDigest})
		if err != nil {
			return OperationResult{}, err
		}
		if repeated {
			return operationResult(prior), nil
		}
	}
	var agent model.Agent
	var desired model.DesiredConfiguration
	var expected model.Revision
	if req.Target.Agent != nil {
		resource := model.ResourceSelector{Kind: model.ResourceAgent, AgentID: req.Target.Agent.AgentID}
		if req.Principal.Kind != model.PrincipalOperator {
			if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionLaunch, Resource: resource}, s.now().UTC()); err != nil {
				return OperationResult{}, err
			}
		}
		var err error
		agent, err = s.store.Agent(ctx, req.Target.Agent.AgentID)
		if err != nil {
			return OperationResult{}, err
		}
		if req.Target.Agent.ExpectedRevision == 0 {
			return OperationResult{}, fail(ErrInvalid, "expected revision is required")
		}
		desired, expected = agent.Desired, req.Target.Agent.ExpectedRevision
	} else {
		if err := requireOperator(req.Principal); err != nil {
			return OperationResult{}, err
		}
		desired = req.Target.Standalone.Desired
		if err := validateDesired(desired); err != nil {
			return OperationResult{}, err
		}
	}
	provider, ok := s.providers.Provider(desired.Harness)
	if !ok {
		return OperationResult{}, fail(ErrUnavailable, "harness %q has no provider", desired.Harness)
	}

	if req.InitialMessage != "" && !provider.Capabilities().PreparedInitialInput {
		return OperationResult{}, fail(ErrUnsupported, "provider cannot prepare an initial message before first work")
	}

	now := s.now().UTC()
	executionID := model.ExecutionID(s.newID("exe_"))
	conversationID := model.ConversationID(s.newID("con_"))
	if req.Target.Standalone != nil && req.Target.Standalone.ConversationID != "" {
		conversationID = req.Target.Standalone.ConversationID
	}
	intent := ports.StartFresh
	var continuation *model.NativeConversationEvidence
	var priorEvidence model.ProviderEvidence
	var expectedConversationRevision model.Revision
	if continuationRecord != nil {
		conversationID, intent, continuation, priorEvidence = continuationRecord.Conversation.ConversationID, ports.StartContinue, &continuationRecord.Native, continuationRecord.Evidence
		expectedConversationRevision = continuationRecord.Conversation.Revision
	}
	if options.intent != "" {
		intent = options.intent
	}
	operationID := options.operationID
	if operationID == "" {
		operationID = model.OperationID(s.newID("op_"))
	}
	spec := resolvedSpec(executionID, agent.ID, desired, conversationID)
	spec.ConfigurationProfile = agent.ConfigurationProfile
	execution := model.Execution{ID: executionID, AgentID: agent.ID, ConversationID: conversationID, Spec: spec, State: model.ExecutionReserved, Attempt: 1, ContextReadiness: model.ContextReadinessPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.requireNativeGuidanceComposition(ctx, execution); err != nil {
		return OperationResult{}, err
	}
	authorityResource := model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: executionID}
	if agent.ID != "" {
		authorityResource = model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agent.ID}
	}
	authority := model.AuthorityRequest{Principal: req.Principal, Action: model.ActionLaunch, Resource: authorityResource, RequestedConfiguration: &desired}
	var access model.ExecutionAccess
	var credential *ports.ActionCredentialMaterial
	if _, capable := provider.(ports.ActionCredentialProvider); capable {
		if strings.TrimSpace(s.agentAPIEndpoint) == "" {
			return OperationResult{}, fail(ErrUnavailable, "agent API endpoint is required for credential-capable provider")
		}
		secret, err := generateActionCredential()
		if err != nil {
			return OperationResult{}, fail(ErrUnavailable, "generate execution credential: %v", err)
		}
		digest := sha256.Sum256(secret)
		deliveryID := s.newID("delivery_")
		access = model.ExecutionAccess{ExecutionID: executionID, AgentID: agent.ID, Generation: 1, CredentialDigest: digest[:], DeliveryID: deliveryID, State: model.ExecutionAccessInactive, IssuedAt: now, ExpiresAt: now.Add(s.accessLease), Revision: 1}
		credential = &ports.ActionCredentialMaterial{ExecutionID: executionID, Generation: access.Generation, DeliveryID: deliveryID, Secret: secret, ExpiresAt: access.ExpiresAt}
		defer clear(secret)
	}
	var initialInput *ports.PreparedInitialInput
	if req.InitialMessage != "" {
		initialInput = &ports.PreparedInitialInput{Body: req.InitialMessage, Correlation: string(operationID), RequiredBeforeFirstWork: true}
	}
	admission, err := s.store.AdmitLaunch(ctx, LaunchAdmission{
		InitialMessageDigest: initialDigest,
		Operation:            model.Operation{ID: operationID, RequestID: req.RequestID, Kind: kind, Principal: req.Principal, ExecutionID: executionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now},
		Execution:            execution,
		AgentID:              agent.ID, Expected: expected, ExpectedConversationRevision: expectedConversationRevision,
		Authority: authority, Access: access,
	})
	if err != nil {
		return OperationResult{}, err
	}
	if admission.Repeated {
		return operationResult(admission), nil
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()

	prepared, err := provider.Prepare(workflowCtx, ports.PreparationRequest{Spec: spec, Intent: intent, Continuation: continuation, History: options.history, InitialInput: initialInput, PriorEvidence: priorEvidence, ActionCredential: credential, Observations: s.primaryObservationSink(executionID, spec.Attempt, provider.Name()), NativeGuidance: s.boundNativeGuidance(model.Execution{ID: executionID, AgentID: agent.ID, ConversationID: conversationID, Spec: spec, Attempt: spec.Attempt}), AgentAPIEndpoint: s.agentAPIEndpoint, CallbackIngress: s.callbackIngress})
	if err != nil {
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		finished, persistErr := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operationID, OperationState: model.OperationFailed, ResultCode: "prepare_failed", Detail: err.Error(), ExecutionID: executionID, ExecutionState: model.ExecutionFailed, UpdateExecutionState: true, At: s.now().UTC()})
		if persistErr != nil {
			return OperationResult{}, persistErr
		}
		return operationResult(finished), err
	}
	description := prepared.Describe()
	preparationErr := validatePrepared(provider.Name(), spec, description)
	if preparationErr == nil && initialInput != nil && (description.InitialInput == nil || !description.InitialInput.Supported || description.InitialInput.Correlation != initialInput.Correlation) {
		preparationErr = fail(ErrUnsupported, "provider did not confirm exact prepared initial message")
	}
	if err := preparationErr; err != nil {
		_ = prepared.Abort(workflowCtx)
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		finished, persistErr := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operationID, OperationState: model.OperationFailed, ResultCode: "invalid_preparation", Detail: err.Error(), ExecutionID: executionID, ExecutionState: model.ExecutionFailed, UpdateExecutionState: true, At: s.now().UTC()})
		if persistErr != nil {
			return OperationResult{}, persistErr
		}
		return operationResult(finished), err
	}
	if credential != nil {
		if description.AccessDelivery == nil || description.AccessDelivery.ExecutionID != executionID || description.AccessDelivery.Generation != credential.Generation || description.AccessDelivery.DeliveryID != credential.DeliveryID || description.AccessDelivery.Resource == "" || description.AccessDelivery.FileIdentity == "" {
			_ = prepared.Abort(workflowCtx)
			return OperationResult{}, fail(ErrInvalid, "provider did not prove protected credential delivery")
		}
		if _, err := s.store.RecordAccessDelivery(workflowCtx, executionID, credential.Generation, *description.AccessDelivery, s.now().UTC()); err != nil {
			_ = prepared.Abort(workflowCtx)
			return OperationResult{}, err
		}
	}
	preparedCtx, cancelPrepared := settlementContext(ctx)
	_, recordErr := s.store.RecordPrepared(preparedCtx, executionID, operationID, description.Evidence, s.now().UTC())
	cancelPrepared()
	if recordErr != nil {
		_ = prepared.Abort(workflowCtx)
		return OperationResult{}, recordErr
	}
	permit := &releasePermit{store: s.store, executionID: executionID, operationID: operationID, now: s.now}
	released, releaseErr := prepared.Release(workflowCtx, permit)
	if !permit.consumed.Load() && releaseErr == nil {
		releaseErr = fail(ErrInvalid, "provider attempted release without consuming application permit")
	}
	evidence := released.Evidence
	if evidence.Provider == "" {
		evidence = description.Evidence
	} else if err := validateEvidence(provider.Name(), evidence); err != nil {
		releaseErr = err
		evidence = description.Evidence
	}
	if releaseErr != nil || released.State == ports.ReleaseUncertain || released.Runtime == nil {
		detail := "provider reported uncertain release"
		if releaseErr != nil {
			detail = releaseErr.Error()
		}
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		finished, persistErr := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operationID, OperationState: model.OperationUncertain, ResultCode: "release_uncertain", Detail: detail, ExecutionID: executionID, ExecutionState: model.ExecutionUnknown, UpdateExecutionState: true, Evidence: evidence, At: s.now().UTC()})
		if released.Runtime != nil {
			s.rememberRuntime(released.Runtime)
		}
		if persistErr != nil {
			return OperationResult{}, persistErr
		}
		return operationResult(finished), fail(ErrUncertain, "%s", detail)
	}
	if released.Runtime.ExecutionID() != executionID {
		return OperationResult{}, fail(ErrInvalid, "provider released runtime for %s, want %s", released.Runtime.ExecutionID(), executionID)
	}
	s.rememberRuntime(released.Runtime)
	executionState := model.ExecutionReleased
	if observation, observeErr := released.Runtime.Observe(workflowCtx); observeErr == nil {
		executionState = stateFromObservation(observation)
		if observation.Evidence.Provider != "" {
			evidence = observation.Evidence
		}
	}
	settlementCtx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()
	finished, err := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operationID, OperationState: model.OperationSucceeded, ResultCode: "released", ExecutionID: executionID, ExecutionState: executionState, UpdateExecutionState: true, Evidence: evidence, At: s.now().UTC()})
	if err != nil {
		return OperationResult{}, err
	}
	return operationResult(finished), nil
}

func generateActionCredential() ([]byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	encoded := make([]byte, hex.EncodedLen(len(random)))
	hex.Encode(encoded, random[:])
	clear(random[:])
	return encoded, nil
}

func (s *Service) Observe(ctx context.Context, req ObserveRequest) (ObservationResult, error) {
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: req.ExecutionID}}, s.now().UTC()); err != nil {
		return ObservationResult{}, err
	}
	execution, err := s.store.Execution(ctx, req.ExecutionID)
	if err != nil {
		return ObservationResult{}, err
	}
	if execution.Workload == model.ExecutionWorkloadShell {
		runtime, runtimeErr := s.hostRuntimeFor(execution)
		if runtimeErr != nil {
			return ObservationResult{}, errors.Join(runtimeErr, s.resetAgentActivityEpisodes(ctx, execution, s.now().UTC()))
		}
		hostObservation, observeErr := runtime.ObserveHost(ctx)
		if observeErr != nil {
			return ObservationResult{}, errors.Join(observeErr, s.resetAgentActivityEpisodes(ctx, execution, s.now().UTC()))
		}
		observation := ports.Observation{ObservedAt: hostObservation.ObservedAt, Workload: hostObservation.Workload}
		updated, persistErr := s.store.RecordShellRecovery(ctx, execution.ID, stateFromObservation(observation), hostObservation.Evidence, s.now().UTC())
		if persistErr != nil {
			return ObservationResult{}, persistErr
		}
		if factErr := s.appendAgentActivityFacts(ctx, updated, observation); factErr != nil {
			return ObservationResult{}, factErr
		}
		return ObservationResult{Execution: updated, Observation: observation}, nil
	}
	runtime, err := s.runtimeFor(ctx, execution)
	if err != nil {
		return ObservationResult{}, errors.Join(err, s.resetAgentActivityEpisodes(ctx, execution, s.now().UTC()))
	}
	observation, err := runtime.Observe(ctx)
	if err != nil {
		return ObservationResult{}, errors.Join(err, s.resetAgentActivityEpisodes(ctx, execution, s.now().UTC()))
	}
	updated, err := s.store.RecordRecovery(ctx, execution.ID, stateFromObservation(observation), nil, observation.Evidence, s.now().UTC())
	if err != nil {
		return ObservationResult{}, err
	}
	if err = s.appendAgentActivityFacts(ctx, updated, observation); err != nil {
		return ObservationResult{}, err
	}
	return ObservationResult{Execution: updated, Observation: observation}, nil
}

func (s *Service) resetAgentActivityEpisodes(ctx context.Context, execution model.Execution, observedAt time.Time) error {
	if execution.AgentID == "" {
		return nil
	}
	return s.resetAutomationTriggerEpisodes(ctx, model.AutomationSourceApplication, model.AutomationFactResource{Kind: model.FactResourceAgent, ID: string(execution.AgentID)}, observedAt)
}

func (s *Service) appendAgentActivityFacts(ctx context.Context, execution model.Execution, observation ports.Observation) error {
	if execution.AgentID == "" {
		return nil
	}
	idle, awaiting := "unknown", "unknown"
	activityAt := observation.AgentActivityObservedAt.UTC()
	switch observation.AgentActivity {
	case ports.AgentActivityActive:
		idle, awaiting = "false", "false"
	case ports.AgentActivityIdle:
		idle, awaiting = "true", "false"
	case ports.AgentActivityAwaitingInput:
		idle, awaiting = "false", "true"
	}
	if activityAt.IsZero() {
		activityAt = observation.ObservedAt.UTC()
		if activityAt.IsZero() {
			activityAt = s.now().UTC()
		}
		idle, awaiting = "unknown", "unknown"
	}
	resource := model.AutomationFactResource{Kind: model.FactResourceAgent, ID: string(execution.AgentID)}
	identity := fmt.Sprintf("execution:%s:activity:%s:%s", execution.ID, observation.AgentActivity, activityAt.Format(time.RFC3339Nano))
	facts := []model.NormalizedFact{
		internalAutomationFact(model.FactAgentIdle, idle, resource, identity+":idle", activityAt, "", 0),
		internalAutomationFact(model.FactAgentAwaitingInput, awaiting, resource, identity+":awaiting", activityAt, "", 0),
	}
	return s.store.AppendAutomationFacts(ctx, model.AutomationSourceApplication, facts)
}

func (s *Service) Interact(ctx context.Context, req InteractRequest) (OperationResult, error) {
	if strings.TrimSpace(req.Text) == "" {
		return OperationResult{}, fail(ErrInvalid, "interaction text is required")
	}
	return s.withRuntimeEffect(ctx, req.RequestContext, req.ExecutionID, model.OperationInteract, func(workflowCtx context.Context, runtime ports.Runtime) (ports.EffectDisposition, model.ProviderEvidence, string, error) {
		result, err := runtime.Interact(workflowCtx, ports.Interaction{Text: req.Text})
		return result.Disposition, result.Evidence, "interaction", err
	})
}

func (s *Service) Attach(ctx context.Context, req AttachRequest) (AttachmentResult, error) {
	execution, err := s.store.Execution(ctx, req.ExecutionID)
	if err != nil {
		return AttachmentResult{}, err
	}
	if execution.Workload == model.ExecutionWorkloadShell {
		return s.attachShell(ctx, req, execution)
	}
	admission, runtime, err := s.admitRuntimeEffect(ctx, req.RequestContext, req.ExecutionID, model.OperationAttach, "")
	if err != nil {
		return AttachmentResult{}, err
	}
	if admission.Repeated {
		return AttachmentResult{Operation: admission.Operation}, nil
	}
	// The native attachment client belongs to the returned connection, not the
	// setup call. Bound setup without imposing that deadline on the live view.
	attachmentCtx, cancelAttachment := context.WithCancel(context.WithoutCancel(ctx))
	setupTimer := time.AfterFunc(admittedEffectTimeout, cancelAttachment)
	if err := s.store.ConsumeExecutionEffect(attachmentCtx, admission.Operation.ID, s.now().UTC()); err != nil {
		setupTimer.Stop()
		cancelAttachment()
		_, _ = s.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: admission.Operation.ID, OperationState: model.OperationRefused, ResultCode: "authority_revoked", Detail: err.Error(), ExecutionID: admission.Execution.ID, At: s.now().UTC()})
		return AttachmentResult{}, err
	}
	result, effectErr := runtime.Attach(attachmentCtx, ports.AttachmentRequest{Kind: req.Kind})
	setupTimer.Stop()
	transferred := false
	defer func() {
		if !transferred {
			cancelAttachment()
			if result.Attachment != nil {
				_ = result.Attachment.Close()
			}
		}
	}()
	if effectErr == nil && attachmentCtx.Err() != nil {
		effectErr = attachmentCtx.Err()
	}

	completion := completionFromDisposition(admission.Operation, admission.Execution, result.Disposition, result.Evidence, "attachment", effectErr, s.now().UTC())
	settlementCtx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()
	finished, err := s.store.CompleteOperation(settlementCtx, completion)
	if err != nil {
		return AttachmentResult{}, err
	}
	if effectErr != nil {
		return AttachmentResult{Operation: finished.Operation}, effectErr
	}
	if result.Attachment == nil {
		return AttachmentResult{Operation: finished.Operation}, nil
	}
	transferred = true
	_, canStageFile := runtime.(ports.TerminalFileStager)
	return AttachmentResult{Operation: finished.Operation, Attachment: ownAttachment(result.Attachment, cancelAttachment), CanStageFile: canStageFile}, nil
}

func (s *Service) Stop(ctx context.Context, req StopRequest) (OperationResult, error) {
	execution, err := s.store.Execution(ctx, req.ExecutionID)
	if err != nil {
		return OperationResult{}, err
	}
	if execution.Workload == model.ExecutionWorkloadShell {
		return s.stopShell(ctx, req, execution)
	}
	return s.withRuntimeEffect(ctx, req.RequestContext, req.ExecutionID, model.OperationStop, func(workflowCtx context.Context, runtime ports.Runtime) (ports.EffectDisposition, model.ProviderEvidence, string, error) {
		result, err := runtime.Stop(workflowCtx, ports.StopRequest{Force: req.Force})
		code := "stop_acknowledged"
		if result.Exited {
			code = "exited"
		}
		return result.Disposition, result.Evidence, code, err
	})
}

func (s *Service) ChangeContext(ctx context.Context, req ChangeContextRequest) (OperationResult, error) {
	if err := validateEffectContext(req.RequestContext); err != nil {
		return OperationResult{}, err
	}
	admission, runtime, err := s.admitRuntimeEffect(ctx, req.RequestContext, req.ExecutionID, model.OperationChangeContext, "")
	if err != nil {
		return OperationResult{}, err
	}
	if admission.Repeated {
		return operationResult(admission), nil
	}
	execution := admission.Execution
	if execution.AgentID == "" {
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		_, _ = s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: admission.Operation.ID, OperationState: model.OperationRefused, ResultCode: "not_applicable", Detail: "standalone context associations are not revisioned", ExecutionID: execution.ID, At: s.now().UTC()})
		return OperationResult{}, fail(ErrUnsupported, "standalone context associations are not revisioned")
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()
	association, err := s.store.CurrentConversation(workflowCtx, execution.AgentID)
	if err != nil {
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		if _, persistErr := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: admission.Operation.ID, OperationState: model.OperationFailed, ResultCode: "context_read_failed", Detail: err.Error(), ExecutionID: execution.ID, ExecutionState: execution.State, At: s.now().UTC()}); persistErr != nil {
			return OperationResult{}, persistErr
		}
		return OperationResult{}, err
	}
	if association.ConversationID != req.ExpectedConversationID || association.Revision != req.ExpectedAssociationRevision {
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		if _, persistErr := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: admission.Operation.ID, OperationState: model.OperationRefused, ResultCode: "context_conflict", Detail: "context association changed", ExecutionID: execution.ID, ExecutionState: execution.State, At: s.now().UTC()}); persistErr != nil {
			return OperationResult{}, persistErr
		}
		return OperationResult{}, appConflict("context association")
	}
	correlation := s.newID("ctx_")
	if err := s.store.BeginContextTransition(workflowCtx, PendingContextTransition{OperationID: admission.Operation.ID, ExecutionID: execution.ID, ExpectedConversationID: req.ExpectedConversationID, ExpectedAssociationRevision: req.ExpectedAssociationRevision, Correlation: correlation, CreatedAt: s.now().UTC()}); err != nil {
		return OperationResult{}, err
	}
	if err := s.store.ConsumeExecutionEffect(workflowCtx, admission.Operation.ID, s.now().UTC()); err != nil {
		_ = s.store.CancelContextTransition(context.WithoutCancel(ctx), admission.Operation.ID)
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		_, _ = s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: admission.Operation.ID, OperationState: model.OperationRefused, ResultCode: "authority_revoked", Detail: err.Error(), ExecutionID: execution.ID, At: s.now().UTC()})
		return OperationResult{}, err
	}
	settlementCtx, cancelSettlement := settlementContext(ctx)
	dispatched, err := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: admission.Operation.ID, OperationState: model.OperationRunning, ResultCode: "awaiting_context_evidence", ExecutionID: execution.ID, At: s.now().UTC()})
	cancelSettlement()
	if err != nil {
		_ = s.store.CancelContextTransition(context.WithoutCancel(ctx), admission.Operation.ID)
		return OperationResult{}, err
	}
	result, effectErr := runtime.ChangeContext(workflowCtx, ports.ContextChange{Intent: req.Intent, ExpectedConversation: req.ExpectedConversationID, ExpectedAssociationRevision: req.ExpectedAssociationRevision, TransitionCorrelation: correlation})
	if effectErr == nil && result.Disposition == ports.EffectAccepted {
		readCtx, cancelRead := settlementContext(ctx)
		defer cancelRead()
		settled, readErr := s.store.OperationResult(readCtx, admission.Operation.ID)
		if readErr == nil {
			return operationResult(settled), nil
		}
		return operationResult(dispatched), nil
	}
	_ = s.store.CancelContextTransition(context.WithoutCancel(ctx), admission.Operation.ID)
	completion := completionFromDisposition(admission.Operation, admission.Execution, result.Disposition, result.Evidence, "context_dispatch", effectErr, s.now().UTC())
	settlementCtx, cancelSettlement = settlementContext(ctx)
	defer cancelSettlement()
	finished, err := s.store.CompleteOperation(settlementCtx, completion)
	if err != nil {
		return OperationResult{}, err
	}
	if effectErr != nil {
		return operationResult(finished), effectErr
	}
	return operationResult(finished), nil
}

func (s *Service) Resume(ctx context.Context, req ResumeRequest) (OperationResult, error) {
	if err := validateEffectContext(req.RequestContext); err != nil {
		return OperationResult{}, err
	}
	var agentID model.AgentID
	if req.Target.Agent != nil {
		agentID = req.Target.Agent.AgentID
		resource := model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agentID}
		if req.Principal.Kind != model.PrincipalOperator {
			if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionLaunch, Resource: resource}, s.now().UTC()); err != nil {
				return OperationResult{}, err
			}
		}
		agent, err := s.store.Agent(ctx, agentID)
		if err != nil {
			return OperationResult{}, err
		}
		if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agent.ID}, RequestedConfiguration: &agent.Desired}, s.now().UTC()); err != nil {
			return OperationResult{}, err
		}
	} else if err := requireOperator(req.Principal); err != nil {
		return OperationResult{}, err
	}
	continuation, err := s.store.Continuation(ctx, agentID, req.ConversationID, req.ExpectedAssociationRevision)
	if err != nil {
		return OperationResult{}, err
	}
	launch := LaunchRequest{RequestContext: req.RequestContext, Target: req.Target}
	return s.launch(ctx, launch, model.OperationResume, &continuation, launchOptions{})
}

func (s *Service) Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		return Snapshot{}, fail(ErrUnauthorized, "operator snapshot authority required; use scoped queries")
	}
	snapshot, err := s.store.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Service) Recover(ctx context.Context, req RecoverRequest) (RecoveryReport, error) {
	if err := requireOperator(req.Principal); err != nil {
		return RecoveryReport{}, err
	}
	executions, err := s.store.RecoverableExecutions(ctx)
	if err != nil {
		return RecoveryReport{}, err
	}
	var report RecoveryReport
	for _, execution := range executions {
		if execution.Workload == model.ExecutionWorkloadProgram {
			if err := s.recoverProgramExecution(ctx, execution, &report); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		if execution.Workload == model.ExecutionWorkloadShell {
			record, recordErr := s.store.ShellRecovery(ctx, execution.ID)
			if recordErr != nil || s.shellHost == nil {
				report.Unknown = append(report.Unknown, execution.ID)
				if _, persistErr := s.store.RecordShellRecovery(ctx, execution.ID, model.ExecutionUnknown, ports.ShellResourceEvidence{}, s.now().UTC()); persistErr != nil {
					return RecoveryReport{}, persistErr
				}
				continue
			}
			result, recoverErr := s.shellHost.RecoverShell(ctx, ports.ShellRecoveryRequest{ExecutionID: execution.ID, Attempt: execution.Attempt, WorkspaceID: record.WorkspaceID, Evidence: record.Evidence})
			if recoverErr != nil || result.State == ports.RecoveryUnknown || result.Evidence.Owner == "" {
				report.Unknown = append(report.Unknown, execution.ID)
				if _, persistErr := s.store.RecordShellRecovery(ctx, execution.ID, model.ExecutionUnknown, result.Evidence, s.now().UTC()); persistErr != nil {
					return RecoveryReport{}, persistErr
				}
				continue
			}
			if result.State == ports.RecoveryExited {
				report.Exited = append(report.Exited, execution.ID)
				if _, persistErr := s.store.RecordShellRecovery(ctx, execution.ID, model.ExecutionExited, result.Evidence, s.now().UTC()); persistErr != nil {
					return RecoveryReport{}, persistErr
				}
				continue
			}
			if result.Runtime == nil || result.Runtime.ExecutionID() != execution.ID {
				report.Unknown = append(report.Unknown, execution.ID)
				if _, persistErr := s.store.RecordShellRecovery(ctx, execution.ID, model.ExecutionUnknown, result.Evidence, s.now().UTC()); persistErr != nil {
					return RecoveryReport{}, persistErr
				}
				continue
			}
			s.runtimeMu.Lock()
			s.hostRuntimes[execution.ID] = result.Runtime
			s.runtimeMu.Unlock()
			report.Controlled = append(report.Controlled, execution.ID)
			state := model.ExecutionRunning
			if result.Observation.Workload == ports.WorkloadExited {
				state = model.ExecutionExited
			}
			if _, persistErr := s.store.RecordShellRecovery(ctx, execution.ID, state, result.Evidence, s.now().UTC()); persistErr != nil {
				return RecoveryReport{}, persistErr
			}
			continue
		}
		provider, ok := s.providers.Provider(execution.Spec.Harness)
		if !ok {
			report.Unknown = append(report.Unknown, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionUnknown, nil, model.ProviderEvidence{}, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		if err := s.requireNativeGuidanceComposition(ctx, execution); err != nil {
			return report, err
		}
		var accessBindingValue *model.ExecutionAccessBinding
		if access, accessErr := s.store.ExecutionAccess(ctx, execution.ID); accessErr == nil {
			binding := accessBinding(access)
			accessBindingValue = &binding
		}
		result, recoverErr := provider.Recover(ctx, ports.RecoveryRequest{ExecutionID: execution.ID, Spec: execution.Spec, Evidence: execution.Evidence, Attempt: execution.Attempt, Access: accessBindingValue, Observations: s.primaryObservationSink(execution.ID, execution.Attempt, provider.Name()), NativeGuidance: s.boundNativeGuidance(execution), AgentAPIEndpoint: s.agentAPIEndpoint, CallbackIngress: s.callbackIngress})
		if recoverErr != nil || result.State == ports.RecoveryUnknown {
			report.Unknown = append(report.Unknown, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionUnknown, nil, result.Evidence, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		if result.State == ports.RecoveryExited {
			report.Exited = append(report.Exited, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionExited, nil, result.Evidence, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		if result.Runtime == nil || result.Runtime.ExecutionID() != execution.ID {
			report.Unknown = append(report.Unknown, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionUnknown, nil, result.Evidence, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		s.rememberRuntime(result.Runtime)
		report.Controlled = append(report.Controlled, execution.ID)
		if _, err := s.store.RecordRecovery(ctx, execution.ID, stateFromObservation(result.Observation), nil, result.Evidence, s.now().UTC()); err != nil {
			return RecoveryReport{}, err
		}
		if accessBindingValue != nil && result.Attempt == execution.Attempt && result.AccessProof != nil && result.AccessProof.ExecutionID == execution.ID && result.AccessProof.Generation == accessBindingValue.Generation && result.AccessProof.DeliveryID == accessBindingValue.DeliveryID {
			if _, err := s.store.ReactivateExecutionAccess(ctx, execution.ID, accessBindingValue.Generation, *result.AccessProof, s.now().UTC()); err != nil && !errors.Is(err, ErrConflict) {
				return RecoveryReport{}, err
			}
		}
	}
	return report, nil
}

func (s *Service) withRuntimeEffect(ctx context.Context, request RequestContext, executionID model.ExecutionID, kind model.OperationKind, effect func(context.Context, ports.Runtime) (ports.EffectDisposition, model.ProviderEvidence, string, error)) (OperationResult, error) {
	return s.withRuntimeEffectID(ctx, request, executionID, kind, "", effect)
}

func (s *Service) withRuntimeEffectID(ctx context.Context, request RequestContext, executionID model.ExecutionID, kind model.OperationKind, operationID model.OperationID, effect func(context.Context, ports.Runtime) (ports.EffectDisposition, model.ProviderEvidence, string, error)) (OperationResult, error) {
	admission, runtime, err := s.admitRuntimeEffect(ctx, request, executionID, kind, operationID)
	if err != nil {
		return OperationResult{}, err
	}
	if admission.Repeated {
		return operationResult(admission), nil
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()
	if err := s.store.ConsumeExecutionEffect(workflowCtx, admission.Operation.ID, s.now().UTC()); err != nil {
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		_, _ = s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: admission.Operation.ID, OperationState: model.OperationRefused, ResultCode: "authority_revoked", Detail: err.Error(), ExecutionID: admission.Execution.ID, At: s.now().UTC()})
		return OperationResult{}, err
	}
	disposition, evidence, code, effectErr := effect(workflowCtx, runtime)
	completion := completionFromDisposition(admission.Operation, admission.Execution, disposition, evidence, code, effectErr, s.now().UTC())
	if kind == model.OperationStop && disposition == ports.EffectAccepted && code == "exited" {
		completion.ExecutionState = model.ExecutionExited
		completion.UpdateExecutionState = true
	}
	settlementCtx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()
	finished, err := s.store.CompleteOperation(settlementCtx, completion)
	if err != nil {
		return OperationResult{}, err
	}
	if effectErr != nil {
		return operationResult(finished), effectErr
	}
	return operationResult(finished), nil
}

func (s *Service) admitRuntimeEffect(ctx context.Context, request RequestContext, executionID model.ExecutionID, kind model.OperationKind, operationID model.OperationID) (AdmissionResult, ports.Runtime, error) {
	if err := validateEffectContext(request); err != nil {
		return AdmissionResult{}, nil, err
	}
	now := s.now().UTC()
	if operationID == "" {
		operationID = model.OperationID(s.newID("op_"))
	}
	operation := model.Operation{ID: operationID, RequestID: request.RequestID, Kind: kind, Principal: request.Principal, ExecutionID: executionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	authority := model.AuthorityRequest{Principal: request.Principal, Action: actionForOperation(kind), Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: executionID}}
	admission, err := s.store.AdmitExecutionOperation(ctx, ExecutionOperationAdmission{Operation: operation, Authority: authority})
	if err != nil || admission.Repeated {
		return admission, nil, err
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()
	runtime, err := s.runtimeFor(workflowCtx, admission.Execution)
	if err != nil {
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		_, _ = s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operation.ID, OperationState: model.OperationRefused, ResultCode: "runtime_unavailable", Detail: err.Error(), ExecutionID: admission.Execution.ID, ExecutionState: admission.Execution.State, At: s.now().UTC()})
	}
	return admission, runtime, err
}

func (s *Service) runtimeFor(ctx context.Context, execution model.Execution) (ports.Runtime, error) {
	s.runtimeMu.RLock()
	runtime := s.runtimes[execution.ID]
	s.runtimeMu.RUnlock()
	if runtime != nil {
		return runtime, nil
	}
	return nil, fail(ErrUnavailable, "execution %s requires explicit recovery", execution.ID)
}

func (s *Service) rememberRuntime(runtime ports.Runtime) {
	s.runtimeMu.Lock()
	s.runtimes[runtime.ExecutionID()] = runtime
	s.runtimeMu.Unlock()
}

func (s *Service) hostRuntimeFor(execution model.Execution) (ports.HostRuntime, error) {
	s.runtimeMu.RLock()
	runtime := s.hostRuntimes[execution.ID]
	s.runtimeMu.RUnlock()
	if runtime == nil {
		return nil, fail(ErrUnavailable, "shell execution %s requires explicit recovery", execution.ID)
	}
	return runtime, nil
}

func (s *Service) admitHostEffect(ctx context.Context, request RequestContext, execution model.Execution, kind model.OperationKind) (AdmissionResult, error) {
	if err := validateEffectContext(request); err != nil {
		return AdmissionResult{}, err
	}
	now := s.now().UTC()
	op := model.Operation{ID: model.OperationID(s.newID("op_")), RequestID: request.RequestID, Kind: kind, Principal: request.Principal, ExecutionID: execution.ID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	authority := model.AuthorityRequest{Principal: request.Principal, Action: actionForOperation(kind), Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: execution.ID}}
	return s.store.AdmitExecutionOperation(ctx, ExecutionOperationAdmission{Operation: op, Authority: authority})
}

func (s *Service) attachShell(ctx context.Context, req AttachRequest, execution model.Execution) (AttachmentResult, error) {
	admission, err := s.admitHostEffect(ctx, req.RequestContext, execution, model.OperationAttach)
	if err != nil || admission.Repeated {
		return AttachmentResult{Operation: admission.Operation}, err
	}
	runtime, err := s.hostRuntimeFor(execution)
	if err != nil {
		return AttachmentResult{}, err
	}
	workflowCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err = s.store.ConsumeExecutionEffect(workflowCtx, admission.Operation.ID, s.now().UTC()); err != nil {
		cancel()
		return AttachmentResult{}, err
	}
	result, effectErr := runtime.AttachHost(workflowCtx, ports.AttachmentRequest{Kind: req.Kind})
	state := model.OperationSucceeded
	if result.Disposition == ports.EffectUnknown {
		state = model.OperationUncertain
	} else if result.Disposition != ports.EffectAccepted {
		state = model.OperationRefused
	}
	finished, persistErr := s.store.CompleteShell(context.WithoutCancel(ctx), OperationCompletion{OperationID: admission.Operation.ID, OperationState: state, ResultCode: "attachment", ExecutionID: execution.ID, At: s.now().UTC()}, result.Evidence)
	if persistErr != nil {
		cancel()
		return AttachmentResult{}, persistErr
	}
	if effectErr != nil || result.Attachment == nil {
		cancel()
		return AttachmentResult{Operation: finished.Operation}, effectErr
	}
	_, canStageFile := runtime.(ports.TerminalFileStager)
	return AttachmentResult{Operation: finished.Operation, Attachment: ownAttachment(result.Attachment, cancel), CanStageFile: canStageFile}, nil
}

func (s *Service) stopShell(ctx context.Context, req StopRequest, execution model.Execution) (OperationResult, error) {
	admission, err := s.admitHostEffect(ctx, req.RequestContext, execution, model.OperationStop)
	if err != nil || admission.Repeated {
		return operationResult(admission), err
	}
	runtime, err := s.hostRuntimeFor(execution)
	if err != nil {
		return OperationResult{}, err
	}
	workflowCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancel()
	if err = s.store.ConsumeExecutionEffect(workflowCtx, admission.Operation.ID, s.now().UTC()); err != nil {
		return OperationResult{}, err
	}
	result, effectErr := runtime.StopHost(workflowCtx, ports.StopRequest{Force: req.Force})
	opState, executionState, update := model.OperationSucceeded, execution.State, false
	if result.Disposition == ports.EffectUnknown {
		opState, executionState, update = model.OperationUncertain, model.ExecutionUnknown, true
	} else if result.Disposition != ports.EffectAccepted {
		opState = model.OperationRefused
	} else if result.Exited {
		executionState, update = model.ExecutionExited, true
	}
	finished, persistErr := s.store.CompleteShell(context.WithoutCancel(ctx), OperationCompletion{OperationID: admission.Operation.ID, OperationState: opState, ResultCode: "stop", ExecutionID: execution.ID, ExecutionState: executionState, UpdateExecutionState: update, At: s.now().UTC()}, result.Evidence)
	if persistErr != nil {
		return OperationResult{}, persistErr
	}
	return operationResult(finished), effectErr
}

func operationResult(result AdmissionResult) OperationResult {
	execution := result.Execution
	return OperationResult{Operation: result.Operation, Execution: &execution, Repeated: result.Repeated}
}

func completionFromDisposition(operation model.Operation, execution model.Execution, disposition ports.EffectDisposition, evidence model.ProviderEvidence, code string, effectErr error, at time.Time) OperationCompletion {
	completion := OperationCompletion{OperationID: operation.ID, ExecutionID: execution.ID, ExecutionState: execution.State, Evidence: evidence, ResultCode: code, At: at}
	switch disposition {
	case ports.EffectAccepted:
		completion.OperationState = model.OperationSucceeded
	case ports.EffectRefused, ports.EffectUnsupported:
		completion.OperationState = model.OperationRefused
	case ports.EffectUnknown:
		completion.OperationState = model.OperationUncertain
		completion.ExecutionState = model.ExecutionUnknown
		completion.UpdateExecutionState = true
	default:
		completion.OperationState = model.OperationFailed
	}
	if effectErr != nil {
		completion.Detail = effectErr.Error()
		if completion.OperationState != model.OperationUncertain {
			completion.OperationState = model.OperationFailed
		}
	}
	return completion
}

func resolvedSpec(executionID model.ExecutionID, agentID model.AgentID, desired model.DesiredConfiguration, conversationID model.ConversationID) model.ResolvedExecutionSpec {
	return model.ResolvedExecutionSpec{ExecutionID: executionID, Workload: model.ExecutionWorkloadHarness, Attempt: 1, AgentID: agentID, ConversationID: conversationID, Harness: desired.Harness, Model: desired.Model, Effort: desired.Effort, WorkingDirectory: desired.WorkingDirectory, Approval: desired.Approval, Sandbox: desired.Sandbox}
}

func actionForOperation(kind model.OperationKind) model.Action {
	switch kind {
	case model.OperationLaunch, model.OperationResume:
		return model.ActionLaunch
	case model.OperationInteract:
		return model.ActionInteract
	case model.OperationAttach:
		return model.ActionAttach
	case model.OperationStop:
		return model.ActionStop
	case model.OperationChangeContext:
		return model.ActionChangeContext
	default:
		return ""
	}
}

func validatePrepared(provider string, spec model.ResolvedExecutionSpec, description ports.PreparedDescription) error {
	if description.ExecutionID != spec.ExecutionID {
		return fail(ErrInvalid, "provider prepared execution %s, want %s", description.ExecutionID, spec.ExecutionID)
	}
	if err := validateEvidence(provider, description.Evidence); err != nil {
		return err
	}
	if description.Requirements.WorkingDirectory != "" && description.Requirements.WorkingDirectory != spec.WorkingDirectory {
		return fail(ErrInvalid, "provider changed working directory")
	}
	if description.Topology != ports.TopologyTerminalAuthoritative && description.Topology != ports.TopologyIndependentServer {
		return fail(ErrInvalid, "unsupported workload topology %q", description.Topology)
	}
	if description.Requirements.Terminal != nil && description.Requirements.Loopback != nil && description.Topology == ports.TopologyTerminalAuthoritative {
		return fail(ErrInvalid, "terminal-authoritative workload cannot require loopback server")
	}
	if description.EffectivePolicy.Approval != spec.Approval || !description.EffectivePolicy.ApprovalEnforced {
		return fail(ErrInvalid, "provider cannot enforce requested approval policy")
	}
	if description.EffectivePolicy.Sandbox != spec.Sandbox || !description.EffectivePolicy.SandboxEnforced {
		return fail(ErrInvalid, "provider cannot enforce requested sandbox policy")
	}
	return nil
}

func validateEvidence(provider string, evidence model.ProviderEvidence) error {
	if evidence.Provider != provider {
		return fail(ErrInvalid, "evidence provider %q does not match %q", evidence.Provider, provider)
	}
	if err := evidence.Validate(); err != nil {
		return fail(ErrInvalid, "invalid provider evidence: %v", err)
	}
	return nil
}

type releasePermit struct {
	store       Store
	executionID model.ExecutionID
	operationID model.OperationID
	now         func() time.Time
	consumed    atomic.Bool
}

func (p *releasePermit) ExecutionID() model.ExecutionID { return p.executionID }
func (p *releasePermit) OperationID() model.OperationID { return p.operationID }
func (p *releasePermit) Consume(ctx context.Context) error {
	if p.consumed.Load() {
		return appConflict("release permit already consumed")
	}
	if err := p.store.ConsumeRelease(ctx, p.executionID, p.operationID, p.now().UTC()); err != nil {
		return err
	}
	p.consumed.Store(true)
	return nil
}

func appConflict(subject string) error { return fail(ErrConflict, "%s changed", subject) }

func settlementContext(requestContext context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(requestContext), settlementTimeout)
}

func validateDesired(desired model.DesiredConfiguration) error {
	if err := model.ValidateEffort(desired.Effort); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(desired.Harness) == "" {
		return fail(ErrInvalid, "harness is required")
	}
	if strings.TrimSpace(desired.WorkingDirectory) == "" {
		return fail(ErrInvalid, "working directory is required")
	}
	if desired.Approval != model.ApprovalSupervised && desired.Approval != model.ApprovalAutomatic {
		return fail(ErrInvalid, "unsupported approval mode %q", desired.Approval)
	}
	if desired.Sandbox != model.SandboxUnconfined && desired.Sandbox != model.SandboxReadOnly && desired.Sandbox != model.SandboxWorkspaceWrite {
		return fail(ErrInvalid, "unsupported sandbox mode %q", desired.Sandbox)
	}
	return nil
}

func validateEffectContext(request RequestContext) error {
	if err := request.RequestID.Validate(); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	if request.Principal.Kind != model.PrincipalOperator && request.Principal.Kind != model.PrincipalAgent && request.Principal.Kind != model.PrincipalExecution && request.Principal.Kind != model.PrincipalAutomation {
		return fail(ErrUnauthorized, "unsupported principal")
	}
	if request.Principal.Kind == model.PrincipalAgent {
		if err := request.Principal.AgentID.Validate(); err != nil {
			return fail(ErrUnauthorized, "invalid agent principal")
		}
	}
	if request.Principal.Kind == model.PrincipalExecution {
		if err := request.Principal.ExecutionID.Validate(); err != nil || request.Principal.Generation == 0 {
			return fail(ErrUnauthorized, "invalid execution principal")
		}
	}
	if request.Principal.Kind == model.PrincipalAutomation {
		if request.Principal.AutomationRun == "" || request.Principal.Delegation == nil {
			return fail(ErrUnauthorized, "invalid automation principal")
		}
		switch request.Principal.Authority.Kind {
		case model.AuthorityOperator:
		case model.AuthorityAgent:
			if err := request.Principal.Authority.AgentID.Validate(); err != nil {
				return fail(ErrUnauthorized, "invalid automation owner")
			}
		case model.AuthorityExecution:
			if err := request.Principal.Authority.ExecutionID.Validate(); err != nil {
				return fail(ErrUnauthorized, "invalid automation owner")
			}
		default:
			return fail(ErrUnauthorized, "invalid automation owner")
		}
	}
	return nil
}

func requireOperator(principal model.Principal) error {
	if principal.Kind != model.PrincipalOperator {
		return fail(ErrUnauthorized, "operator authority required")
	}
	return nil
}

func stateFromObservation(observation ports.Observation) model.ExecutionState {
	switch observation.Workload {
	case ports.WorkloadRunning, ports.WorkloadStarting:
		return model.ExecutionRunning
	case ports.WorkloadExited:
		return model.ExecutionExited
	default:
		return model.ExecutionUnknown
	}
}

// ownedAttachment transfers native-client cancellation to connection Close.
type ownedAttachment struct {
	ports.Attachment
	cancel   context.CancelFunc
	once     sync.Once
	closeErr error
}

func (a *ownedAttachment) Close() error {
	a.once.Do(func() { a.cancel(); a.closeErr = a.Attachment.Close() })
	return a.closeErr
}

// Preserve focused capabilities when transferring connection lifetime ownership.
// Fixed-size attachments must not advertise resizing merely because we wrap them.
func ownAttachment(attachment ports.Attachment, cancel context.CancelFunc) ports.Attachment {
	owned := &ownedAttachment{Attachment: attachment, cancel: cancel}
	if resizable, ok := attachment.(ports.ResizableAttachment); ok {
		return &ownedResizableAttachment{ownedAttachment: owned, resizable: resizable}
	}
	return owned
}

type ownedResizableAttachment struct {
	*ownedAttachment
	resizable ports.ResizableAttachment
}

func (a *ownedResizableAttachment) Resize(ctx context.Context, size ports.TerminalSize) error {
	return a.resizable.Resize(ctx, size)
}
