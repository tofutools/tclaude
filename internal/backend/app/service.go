package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type IDGenerator func(prefix string) string

const (
	admittedEffectTimeout = 5 * time.Minute
	settlementTimeout     = 30 * time.Second
)

type Service struct {
	store     Store
	providers ports.ProviderRegistry
	now       func() time.Time
	newID     IDGenerator

	runtimeMu sync.RWMutex
	runtimes  map[model.ExecutionID]ports.Runtime
}

func New(store Store, providers ports.ProviderRegistry) *Service {
	return &Service{
		store: store, providers: providers, now: time.Now, newID: randomID,
		runtimes: make(map[model.ExecutionID]ports.Runtime),
	}
}

func (s *Service) WithClock(now func() time.Time) *Service { s.now = now; return s }

func (s *Service) WithIDGenerator(generate IDGenerator) *Service { s.newID = generate; return s }

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
	if err := req.ID.Validate(); err != nil {
		return AgentResult{}, fail(ErrInvalid, "%v", err)
	}
	if strings.TrimSpace(req.Name) == "" {
		return AgentResult{}, fail(ErrInvalid, "agent name is required")
	}
	if err := validateDesired(req.Desired); err != nil {
		return AgentResult{}, err
	}
	now := s.now().UTC()
	agent := model.Agent{ID: req.ID, Name: req.Name, Desired: req.Desired, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Agent: agent}, nil
}

func (s *Service) UpdateAgent(ctx context.Context, req UpdateAgentRequest) (AgentResult, error) {
	if err := requireOperator(req.Context); err != nil {
		return AgentResult{}, err
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
	agent, err := s.store.UpdateAgent(ctx, req.ID, req.ExpectedRevision, req.Name, req.Desired, s.now().UTC())
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
	group := model.Group{ID: req.ID, Name: req.Name, Members: append([]model.AgentID(nil), req.Members...), Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateGroup(ctx, group); err != nil {
		return GroupResult{}, err
	}
	return GroupResult{Group: group}, nil
}

func (s *Service) Launch(ctx context.Context, req LaunchRequest) (OperationResult, error) {
	return s.launch(ctx, req, model.OperationLaunch, nil)
}

func (s *Service) launch(ctx context.Context, req LaunchRequest, kind model.OperationKind, continuationRecord *ContinuationRecord) (OperationResult, error) {
	if err := validateEffectContext(req.RequestContext); err != nil {
		return OperationResult{}, err
	}
	if (req.Target.Agent == nil) == (req.Target.Standalone == nil) {
		return OperationResult{}, fail(ErrInvalid, "exactly one launch target is required")
	}
	var agent model.Agent
	var desired model.DesiredConfiguration
	var expected model.Revision
	if req.Target.Agent != nil {
		var err error
		agent, err = s.store.Agent(ctx, req.Target.Agent.AgentID)
		if err != nil {
			return OperationResult{}, err
		}
		if err := requireSelfOrOperator(req.Principal, agent.ID); err != nil {
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
	operationID := model.OperationID(s.newID("op_"))
	spec := resolvedSpec(executionID, agent.ID, desired, conversationID)
	admission, err := s.store.AdmitLaunch(ctx, LaunchAdmission{
		Operation: model.Operation{ID: operationID, RequestID: req.RequestID, Kind: kind, Principal: req.Principal, ExecutionID: executionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now},
		Execution: model.Execution{ID: executionID, AgentID: agent.ID, ConversationID: conversationID, Spec: spec, State: model.ExecutionReserved, Revision: 1, CreatedAt: now, UpdatedAt: now},
		AgentID:   agent.ID, Expected: expected, ExpectedConversationRevision: expectedConversationRevision,
	})
	if err != nil {
		return OperationResult{}, err
	}
	if admission.Repeated {
		return operationResult(admission), nil
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()

	prepared, err := provider.Prepare(workflowCtx, ports.PreparationRequest{Spec: spec, Intent: intent, Continuation: continuation, PriorEvidence: priorEvidence})
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
	if err := validatePrepared(provider.Name(), spec, description); err != nil {
		_ = prepared.Abort(workflowCtx)
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		finished, persistErr := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operationID, OperationState: model.OperationFailed, ResultCode: "invalid_preparation", Detail: err.Error(), ExecutionID: executionID, ExecutionState: model.ExecutionFailed, UpdateExecutionState: true, At: s.now().UTC()})
		if persistErr != nil {
			return OperationResult{}, persistErr
		}
		return operationResult(finished), err
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
	var native *model.NativeConversationEvidence
	if observation, observeErr := released.Runtime.Observe(workflowCtx); observeErr == nil {
		executionState = stateFromObservation(observation)
		native = observation.NativeConversation
		if observation.Evidence.Provider != "" {
			evidence = observation.Evidence
		}
	}
	settlementCtx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()
	finished, err := s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operationID, OperationState: model.OperationSucceeded, ResultCode: "released", ExecutionID: executionID, ExecutionState: executionState, UpdateExecutionState: true, Evidence: evidence, Native: native, At: s.now().UTC()})
	if err != nil {
		return OperationResult{}, err
	}
	return operationResult(finished), nil
}

func (s *Service) Observe(ctx context.Context, req ObserveRequest) (ObservationResult, error) {
	execution, err := s.store.Execution(ctx, req.ExecutionID)
	if err != nil {
		return ObservationResult{}, err
	}
	if err := requireSelfOrOperator(req.Principal, execution.AgentID); err != nil {
		return ObservationResult{}, err
	}
	runtime, err := s.runtimeFor(ctx, execution)
	if err != nil {
		return ObservationResult{}, err
	}
	observation, err := runtime.Observe(ctx)
	if err != nil {
		return ObservationResult{}, err
	}
	updated, err := s.store.RecordRecovery(ctx, execution.ID, stateFromObservation(observation), observation.NativeConversation, observation.Evidence, s.now().UTC())
	if err != nil {
		return ObservationResult{}, err
	}
	return ObservationResult{Execution: updated, Observation: observation}, nil
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
	admission, runtime, err := s.admitRuntimeEffect(ctx, req.RequestContext, req.ExecutionID, model.OperationAttach)
	if err != nil {
		return AttachmentResult{}, err
	}
	if admission.Repeated {
		return AttachmentResult{Operation: admission.Operation}, nil
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()
	result, effectErr := runtime.Attach(workflowCtx, ports.AttachmentRequest{Kind: req.Kind})
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
	return AttachmentResult{Operation: finished.Operation, Attachment: result.Attachment}, nil
}

func (s *Service) Stop(ctx context.Context, req StopRequest) (OperationResult, error) {
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
	execution, err := s.store.Execution(ctx, req.ExecutionID)
	if err != nil {
		return OperationResult{}, err
	}
	if execution.AgentID == "" {
		return OperationResult{}, fail(ErrUnsupported, "standalone context associations are not revisioned")
	}
	if err := requireSelfOrOperator(req.Principal, execution.AgentID); err != nil {
		return OperationResult{}, err
	}
	admission, runtime, err := s.admitRuntimeEffect(ctx, req.RequestContext, req.ExecutionID, model.OperationChangeContext)
	if err != nil {
		return OperationResult{}, err
	}
	if admission.Repeated {
		return operationResult(admission), nil
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()
	association, err := s.store.CurrentConversation(ctx, execution.AgentID)
	if err != nil {
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
	result, effectErr := runtime.ChangeContext(workflowCtx, ports.ContextChange{Intent: req.Intent, ExpectedConversation: req.ExpectedConversationID, ExpectedAssociationRevision: req.ExpectedAssociationRevision})
	completion := completionFromDisposition(admission.Operation, admission.Execution, result.Disposition, result.Evidence, "context_changed", effectErr, s.now().UTC())
	completion.Native = result.NativeConversation
	settlementCtx, cancelSettlement := settlementContext(ctx)
	defer cancelSettlement()
	var finished AdmissionResult
	if result.Disposition == ports.EffectAccepted && admission.Execution.AgentID != "" {
		conversationID := model.ConversationID(s.newID("con_"))
		finished, err = s.store.CompleteContextOperation(settlementCtx, completion, ContextAssociation{ExecutionID: req.ExecutionID, AgentID: admission.Execution.AgentID, ConversationID: conversationID, ExpectedRevision: req.ExpectedAssociationRevision, Native: result.NativeConversation, At: s.now().UTC()})
		if err != nil {
			return OperationResult{}, err
		}
	} else {
		finished, err = s.store.CompleteOperation(settlementCtx, completion)
		if err != nil {
			return OperationResult{}, err
		}
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
		agent, err := s.store.Agent(ctx, agentID)
		if err != nil {
			return OperationResult{}, err
		}
		if err := requireSelfOrOperator(req.Principal, agent.ID); err != nil {
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
	return s.launch(ctx, launch, model.OperationResume, &continuation)
}

func (s *Service) SendMessage(ctx context.Context, req SendMessageRequest) (MessageResult, error) {
	if err := validateEffectContext(req.RequestContext); err != nil {
		return MessageResult{}, err
	}
	if req.Principal.Kind == model.PrincipalAgent {
		if _, err := s.store.Agent(ctx, req.Principal.AgentID); err != nil {
			return MessageResult{}, fail(ErrUnauthorized, "sender is not an admitted agent")
		}
	} else if req.Principal.Kind != model.PrincipalOperator {
		return MessageResult{}, fail(ErrUnauthorized, "unsupported principal")
	}
	if strings.TrimSpace(req.Body) == "" {
		return MessageResult{}, fail(ErrInvalid, "message body is required")
	}
	if len(req.RecipientAgentIDs) == 0 {
		return MessageResult{}, fail(ErrInvalid, "at least one recipient is required")
	}
	recipients := make([]model.MessageRecipient, 0, len(req.RecipientAgentIDs))
	seen := map[model.AgentID]struct{}{}
	for _, id := range req.RecipientAgentIDs {
		if _, duplicate := seen[id]; duplicate {
			return MessageResult{}, fail(ErrInvalid, "duplicate recipient %s", id)
		}
		seen[id] = struct{}{}
		if _, err := s.store.Agent(ctx, id); err != nil {
			return MessageResult{}, err
		}
		recipients = append(recipients, model.MessageRecipient{ID: model.RecipientID(s.newID("rcp_")), AgentID: id})
	}
	now := s.now().UTC()
	message := model.Message{ID: model.MessageID(s.newID("msg_")), Sender: req.Principal, Body: req.Body, Recipients: recipients, CreatedAt: now}
	operationID := model.OperationID(s.newID("op_"))
	result, err := s.store.CreateMessage(ctx, message, req.RequestID, operationID)
	if err != nil {
		return MessageResult{}, err
	}
	return MessageResult{Message: result.Message}, nil
}

func (s *Service) MarkMessageRead(ctx context.Context, req MarkMessageReadRequest) (MessageResult, error) {
	if req.Principal.Kind != model.PrincipalOperator && (req.Principal.Kind != model.PrincipalAgent || req.Principal.AgentID != req.AgentID) {
		return MessageResult{}, fail(ErrUnauthorized, "principal cannot acknowledge this recipient")
	}
	message, err := s.store.MarkMessageRead(ctx, req.MessageID, req.AgentID, s.now().UTC())
	return MessageResult{Message: message}, err
}

func (s *Service) Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	snapshot, err := s.store.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if req.Principal.Kind == model.PrincipalOperator {
		return snapshot, nil
	}
	if req.Principal.Kind != model.PrincipalAgent {
		return Snapshot{}, fail(ErrUnauthorized, "unsupported principal")
	}
	id := req.Principal.AgentID
	filtered := Snapshot{Revision: snapshot.Revision}
	for _, agent := range snapshot.Agents {
		if agent.ID == id {
			filtered.Agents = append(filtered.Agents, agent)
		}
	}
	for _, group := range snapshot.Groups {
		for _, member := range group.Members {
			if member == id {
				filtered.Groups = append(filtered.Groups, group)
				break
			}
		}
	}
	for _, execution := range snapshot.Executions {
		if execution.AgentID == id {
			filtered.Executions = append(filtered.Executions, execution)
		}
	}
	for _, operation := range snapshot.Operations {
		if operation.Principal.Kind == model.PrincipalAgent && operation.Principal.AgentID == id {
			filtered.Operations = append(filtered.Operations, operation)
		}
	}
	for _, message := range snapshot.Messages {
		visible := message.Sender.Kind == model.PrincipalAgent && message.Sender.AgentID == id
		for _, recipient := range message.Recipients {
			visible = visible || recipient.AgentID == id
		}
		if visible {
			filtered.Messages = append(filtered.Messages, message)
		}
	}
	return filtered, nil
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
		provider, ok := s.providers.Provider(execution.Spec.Harness)
		if !ok {
			report.Unknown = append(report.Unknown, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionUnknown, execution.NativeConversation, model.ProviderEvidence{}, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		result, recoverErr := provider.Recover(ctx, ports.RecoveryRequest{ExecutionID: execution.ID, Spec: execution.Spec, Evidence: execution.Evidence})
		if recoverErr != nil || result.State == ports.RecoveryUnknown {
			report.Unknown = append(report.Unknown, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionUnknown, result.Observation.NativeConversation, result.Evidence, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		if result.State == ports.RecoveryExited {
			report.Exited = append(report.Exited, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionExited, result.Observation.NativeConversation, result.Evidence, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		if result.Runtime == nil || result.Runtime.ExecutionID() != execution.ID {
			report.Unknown = append(report.Unknown, execution.ID)
			if _, err := s.store.RecordRecovery(ctx, execution.ID, model.ExecutionUnknown, result.Observation.NativeConversation, result.Evidence, s.now().UTC()); err != nil {
				return RecoveryReport{}, err
			}
			continue
		}
		s.rememberRuntime(result.Runtime)
		report.Controlled = append(report.Controlled, execution.ID)
		if _, err := s.store.RecordRecovery(ctx, execution.ID, stateFromObservation(result.Observation), result.Observation.NativeConversation, result.Evidence, s.now().UTC()); err != nil {
			return RecoveryReport{}, err
		}
	}
	return report, nil
}

func (s *Service) withRuntimeEffect(ctx context.Context, request RequestContext, executionID model.ExecutionID, kind model.OperationKind, effect func(context.Context, ports.Runtime) (ports.EffectDisposition, model.ProviderEvidence, string, error)) (OperationResult, error) {
	admission, runtime, err := s.admitRuntimeEffect(ctx, request, executionID, kind)
	if err != nil {
		return OperationResult{}, err
	}
	if admission.Repeated {
		return operationResult(admission), nil
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()
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

func (s *Service) admitRuntimeEffect(ctx context.Context, request RequestContext, executionID model.ExecutionID, kind model.OperationKind) (AdmissionResult, ports.Runtime, error) {
	if err := validateEffectContext(request); err != nil {
		return AdmissionResult{}, nil, err
	}
	execution, err := s.store.Execution(ctx, executionID)
	if err != nil {
		return AdmissionResult{}, nil, err
	}
	if err := requireSelfOrOperator(request.Principal, execution.AgentID); err != nil {
		return AdmissionResult{}, nil, err
	}
	now := s.now().UTC()
	operation := model.Operation{ID: model.OperationID(s.newID("op_")), RequestID: request.RequestID, Kind: kind, Principal: request.Principal, ExecutionID: executionID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
	admission, err := s.store.AdmitExecutionOperation(ctx, ExecutionOperationAdmission{Operation: operation})
	if err != nil || admission.Repeated {
		return admission, nil, err
	}
	workflowCtx, cancelWorkflow := context.WithTimeout(context.WithoutCancel(ctx), admittedEffectTimeout)
	defer cancelWorkflow()
	runtime, err := s.runtimeFor(workflowCtx, execution)
	if err != nil {
		settlementCtx, cancelSettlement := settlementContext(ctx)
		defer cancelSettlement()
		_, _ = s.store.CompleteOperation(settlementCtx, OperationCompletion{OperationID: operation.ID, OperationState: model.OperationRefused, ResultCode: "runtime_unavailable", Detail: err.Error(), ExecutionID: execution.ID, ExecutionState: execution.State, At: s.now().UTC()})
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
	provider, ok := s.providers.Provider(execution.Spec.Harness)
	if !ok {
		return nil, fail(ErrUnavailable, "harness %q has no provider", execution.Spec.Harness)
	}
	recovered, err := provider.Recover(ctx, ports.RecoveryRequest{ExecutionID: execution.ID, Spec: execution.Spec, Evidence: execution.Evidence})
	if err != nil {
		return nil, err
	}
	if recovered.State != ports.RecoveryControlled || recovered.Runtime == nil || recovered.Runtime.ExecutionID() != execution.ID {
		return nil, fail(ErrUnavailable, "control authority for execution %s is not proven", execution.ID)
	}
	s.rememberRuntime(recovered.Runtime)
	return recovered.Runtime, nil
}

func (s *Service) rememberRuntime(runtime ports.Runtime) {
	s.runtimeMu.Lock()
	s.runtimes[runtime.ExecutionID()] = runtime
	s.runtimeMu.Unlock()
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
	return model.ResolvedExecutionSpec{ExecutionID: executionID, AgentID: agentID, ConversationID: conversationID, Harness: desired.Harness, Model: desired.Model, WorkingDirectory: desired.WorkingDirectory, Approval: desired.Approval, Sandbox: desired.Sandbox}
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
	if request.Principal.Kind != model.PrincipalOperator && request.Principal.Kind != model.PrincipalAgent {
		return fail(ErrUnauthorized, "unsupported principal")
	}
	if request.Principal.Kind == model.PrincipalAgent {
		if err := request.Principal.AgentID.Validate(); err != nil {
			return fail(ErrUnauthorized, "invalid agent principal")
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

func requireSelfOrOperator(principal model.Principal, agentID model.AgentID) error {
	if principal.Kind == model.PrincipalOperator {
		return nil
	}
	if principal.Kind == model.PrincipalAgent && principal.AgentID == agentID {
		return nil
	}
	return fail(ErrUnauthorized, "principal cannot control agent %s", agentID)
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
