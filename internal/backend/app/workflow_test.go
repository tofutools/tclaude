package app_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestWorkflowPersistsEffectsMailAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "replacement.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	provider := newFakeProvider()
	service := testService(store, provider)

	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_alpha")
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "group_main", Name: "Main", Members: []model.AgentID{agent.ID}})
	require.NoError(t, err)

	launchRequest := app.LaunchRequest{RequestContext: effect(operator, "request_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}}
	launched, err := service.Launch(ctx, launchRequest)
	require.NoError(t, err)
	require.Equal(t, model.OperationSucceeded, launched.Operation.State)
	require.Equal(t, model.ExecutionRunning, launched.Execution.State)
	require.Equal(t, 1, provider.releases)
	require.True(t, provider.permitConsumed)

	interaction := app.InteractRequest{RequestContext: effect(model.OperatorPrincipal(), "request_interact"), ExecutionID: launched.Execution.ID, Text: "hello"}
	first, err := service.Interact(ctx, interaction)
	require.NoError(t, err)
	second, err := service.Interact(ctx, interaction)
	require.NoError(t, err)
	require.Equal(t, first.Operation.ID, second.Operation.ID)
	require.True(t, second.Repeated)
	require.Equal(t, 1, provider.runtime.interactions)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: interaction.RequestContext, ExecutionID: launched.Execution.ID})
	require.ErrorIs(t, err, app.ErrConflict, "a request identity cannot be reused for a different effect")
	require.False(t, provider.runtime.stopped)

	attached, err := service.Attach(ctx, app.AttachRequest{RequestContext: effect(operator, "request_attach"), ExecutionID: launched.Execution.ID, Kind: ports.AttachmentTerminal})
	require.NoError(t, err)
	require.NotNil(t, attached.Attachment)
	require.Equal(t, ports.TopologyIndependentServer, provider.preparedTopology)
	require.Equal(t, 1, provider.runtime.attachments)
	require.False(t, provider.runtime.stopped, "attachment is not workload ownership")
	require.NoError(t, attached.Attachment.Close())

	messageResult, err := service.SendMessage(ctx, app.SendMessageRequest{RequestContext: effect(operator, "request_message"), RecipientAgentIDs: []model.AgentID{agent.ID}, Body: "durable hello"})
	require.NoError(t, err)
	read, err := service.MarkMessageRead(ctx, app.MarkMessageReadRequest{RequestContext: effect(model.AgentPrincipal(agent.ID), "request_read"), MessageID: messageResult.Message.ID, AgentID: agent.ID})
	require.NoError(t, err)
	require.NotNil(t, read.Message.Recipients[0].ReadAt)

	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	restarted := testService(store, provider)
	snapshot, err := restarted.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Groups, 1)
	require.Len(t, snapshot.Messages, 1)
	require.NotNil(t, snapshot.Messages[0].Recipients[0].ReadAt)
	report, err := restarted.Recover(ctx, app.RecoverRequest{Principal: operator})
	require.NoError(t, err)
	require.Equal(t, []model.ExecutionID{launched.Execution.ID}, report.Controlled)
	require.Equal(t, 1, provider.recoveries)

	stopped, err := restarted.Stop(ctx, app.StopRequest{RequestContext: effect(operator, "request_stop"), ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stopped.Execution.State)
}

func TestFailedPreparationLeavesOfflineAgentAndNoRelease(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	provider.prepareErr = errors.New("missing runtime")
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_offline")

	result, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_failed_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.EqualError(t, err, "missing runtime")
	require.Equal(t, model.OperationFailed, result.Operation.State)
	require.Equal(t, 0, provider.releases)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, model.ExecutionFailed, snapshot.Executions[0].State)
}

func TestRefusesUnmetConfinementBeforeRelease(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	provider.enforceSandbox = false
	service := testService(store, provider)
	agent := createAgentWithSandbox(t, ctx, service, model.SandboxWorkspaceWrite, "agent_confined")

	result, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_confined"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.ErrorIs(t, err, app.ErrInvalid)
	require.Equal(t, model.OperationFailed, result.Operation.State)
	require.Equal(t, 0, provider.releases)

	unconfined := createAgentWithSandbox(t, ctx, service, model.SandboxUnconfined, "agent_unconfined")
	provider.enforceSandbox = true
	_, err = service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_unconfined"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: unconfined.ID, ExpectedRevision: unconfined.Revision}}})
	require.NoError(t, err)
	require.Equal(t, 1, provider.releases)
}

func TestUncertainReleaseIsDurableAndNeverReplayed(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	provider.uncertainRelease = true
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_uncertain")
	request := app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_uncertain"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}}

	first, err := service.Launch(ctx, request)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.Equal(t, model.OperationUncertain, first.Operation.State)
	require.Equal(t, model.ExecutionUnknown, first.Execution.State)
	second, err := service.Launch(ctx, request)
	require.NoError(t, err)
	require.True(t, second.Repeated)
	require.Equal(t, first.Operation.ID, second.Operation.ID)
	require.Equal(t, 1, provider.releases)
}

func TestStandaloneLaunchDoesNotManufactureAgent(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider)
	desired := model.DesiredConfiguration{Harness: "fake", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}

	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_plain"), Target: app.LaunchTarget{Standalone: &app.StandaloneLaunchTarget{Desired: desired}}})
	require.NoError(t, err)
	require.Empty(t, launched.Execution.AgentID)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Empty(t, snapshot.Agents)
	require.Len(t, snapshot.Executions, 1)
}

func TestResetDispatchWithoutProviderEvidencePreservesConfirmedContext(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	provider.runtime.omitNativeOnChange = true
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_reset")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_reset_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	changed, err := service.ChangeContext(ctx, app.ChangeContextRequest{RequestContext: effect(model.OperatorPrincipal(), "request_reset"), ExecutionID: launched.Execution.ID, Intent: ports.ContextReset, ExpectedConversationID: launched.Execution.ConversationID, ExpectedAssociationRevision: 1})
	require.NoError(t, err)
	persisted, err := store.Execution(ctx, changed.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, model.OperationRunning, changed.Operation.State)
	require.NotNil(t, persisted.NativeConversation)
	require.Equal(t, "native_initial", persisted.NativeConversation.Reference)
	require.Equal(t, launched.Execution.ConversationID, persisted.ConversationID)
}

func TestLateInteractionCannotResurrectExitedExecution(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_ordering")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_order_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	provider.runtime.onInteract = func() {
		_, stopErr := service.Stop(ctx, app.StopRequest{RequestContext: effect(model.OperatorPrincipal(), "request_nested_stop"), ExecutionID: launched.Execution.ID})
		require.NoError(t, stopErr)
	}
	_, err = service.Interact(ctx, app.InteractRequest{RequestContext: effect(model.OperatorPrincipal(), "request_late_interact"), ExecutionID: launched.Execution.ID, Text: "hello"})
	require.NoError(t, err)
	persisted, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, persisted.State)
}

func TestLateObservationCannotResurrectExitedExecution(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_observe_order")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_observe_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	provider.runtime.onObserve = func() {
		_, stopErr := service.Stop(ctx, app.StopRequest{RequestContext: effect(model.OperatorPrincipal(), "request_observe_stop"), ExecutionID: launched.Execution.ID})
		require.NoError(t, stopErr)
	}
	observed, err := service.Observe(ctx, app.ObserveRequest{Principal: model.OperatorPrincipal(), ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, observed.Execution.State)
}

func TestClientCancellationDuringEffectDoesNotCancelSettlement(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_disconnect")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_disconnect_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	requestCtx, cancelRequest := context.WithCancel(ctx)
	provider.runtime.onInteract = cancelRequest
	result, err := service.Interact(requestCtx, app.InteractRequest{RequestContext: effect(model.OperatorPrincipal(), "request_disconnect_interact"), ExecutionID: launched.Execution.ID, Text: "continue after disconnect"})
	require.NoError(t, err)
	require.Equal(t, model.OperationSucceeded, result.Operation.State)
	persisted, err := store.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, model.OperationSucceeded, persisted.Operations[len(persisted.Operations)-1].State)
}

func TestClientCancellationAfterContextAdmissionDoesNotStrandOperation(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_context_disconnect")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_context_disconnect_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	requestCtx, cancelRequest := context.WithCancel(ctx)
	service = testService(cancelAfterAdmissionStore{Store: store, cancel: cancelRequest}, provider)
	_, err = service.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	result, err := service.ChangeContext(requestCtx, app.ChangeContextRequest{RequestContext: effect(model.OperatorPrincipal(), "request_context_disconnect"), ExecutionID: launched.Execution.ID, Intent: ports.ContextClear, ExpectedConversationID: launched.Execution.ConversationID, ExpectedAssociationRevision: 1})
	require.NoError(t, err)
	require.Equal(t, model.OperationSucceeded, result.Operation.State)
}

func TestContextChangeAndResumeUseStoredNativeEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider)
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "agent_context")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "request_context_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	oldConversation := launched.Execution.ConversationID
	changed, err := service.ChangeContext(ctx, app.ChangeContextRequest{RequestContext: effect(model.OperatorPrincipal(), "request_context_change"), ExecutionID: launched.Execution.ID, Intent: ports.ContextClear, ExpectedConversationID: oldConversation, ExpectedAssociationRevision: 1})
	require.NoError(t, err)
	require.NotEqual(t, oldConversation, changed.Execution.ConversationID)
	repeated, err := service.ChangeContext(ctx, app.ChangeContextRequest{RequestContext: effect(model.OperatorPrincipal(), "request_context_change"), ExecutionID: launched.Execution.ID, Intent: ports.ContextClear, ExpectedConversationID: oldConversation, ExpectedAssociationRevision: 1})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	_, err = service.ChangeContext(ctx, app.ChangeContextRequest{RequestContext: effect(model.OperatorPrincipal(), "request_stale_context"), ExecutionID: launched.Execution.ID, Intent: ports.ContextClear, ExpectedConversationID: oldConversation, ExpectedAssociationRevision: 1})
	require.ErrorIs(t, err, app.ErrConflict)
	require.Equal(t, 1, provider.runtime.contextChanges)

	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	updatedAgent := snapshot.Agents[0]
	_, err = service.Resume(ctx, app.ResumeRequest{RequestContext: effect(model.OperatorPrincipal(), "request_resume_while_live"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: updatedAgent.Revision}}, ConversationID: changed.Execution.ConversationID, ExpectedAssociationRevision: 2})
	require.ErrorIs(t, err, app.ErrConflict)
	require.Equal(t, 1, provider.releases)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: effect(model.OperatorPrincipal(), "request_context_stop"), ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	resumed, err := service.Resume(ctx, app.ResumeRequest{RequestContext: effect(model.OperatorPrincipal(), "request_resume"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: updatedAgent.Revision}}, ConversationID: changed.Execution.ConversationID, ExpectedAssociationRevision: 2})
	require.NoError(t, err)
	require.NotEqual(t, launched.Execution.ID, resumed.Execution.ID)
	require.Equal(t, ports.StartContinue, provider.lastPreparation.Intent)
	require.Equal(t, "native_rotated", provider.lastPreparation.Continuation.Reference)
	require.NotEmpty(t, provider.lastPreparation.PriorEvidence.Payload)
}

func effect(principal model.Principal, requestID model.RequestID) app.RequestContext {
	return app.RequestContext{Principal: principal, RequestID: requestID}
}

func createAgent(t *testing.T, ctx context.Context, service *app.Service, principal model.Principal, id model.AgentID) model.Agent {
	return createAgentWithSandboxPrincipal(t, ctx, service, principal, model.SandboxUnconfined, id)
}

func createAgentWithSandbox(t *testing.T, ctx context.Context, service *app.Service, sandbox model.SandboxMode, id model.AgentID) model.Agent {
	return createAgentWithSandboxPrincipal(t, ctx, service, model.OperatorPrincipal(), sandbox, id)
}

func createAgentWithSandboxPrincipal(t *testing.T, ctx context.Context, service *app.Service, principal model.Principal, sandbox model.SandboxMode, id model.AgentID) model.Agent {
	result, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: principal, ID: id, Name: string(id), Desired: model.DesiredConfiguration{Harness: "fake", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: sandbox}})
	require.NoError(t, err)
	return result.Agent
}

func testService(store app.Store, provider *fakeProvider) *app.Service {
	sequence := 0
	return app.New(store, fakeRegistry{provider: provider}).WithIDGenerator(func(prefix string) string { sequence++; return prefix + strconv.Itoa(sequence) })
}

type fakeRegistry struct{ provider ports.Provider }

func (r fakeRegistry) Provider(harness string) (ports.Provider, bool) {
	return r.provider, harness == "fake"
}

type cancelAfterAdmissionStore struct {
	app.Store
	cancel context.CancelFunc
}

func (s cancelAfterAdmissionStore) AdmitExecutionOperation(ctx context.Context, admission app.ExecutionOperationAdmission) (app.AdmissionResult, error) {
	result, err := s.Store.AdmitExecutionOperation(ctx, admission)
	if err == nil {
		s.cancel()
	}
	return result, err
}

type fakeProvider struct {
	mu                   sync.Mutex
	runtime              *fakeRuntime
	prepareErr           error
	enforceSandbox       bool
	preparedTopology     ports.WorkloadTopology
	lastPreparation      ports.PreparationRequest
	releases, recoveries int
	permitConsumed       bool
	uncertainRelease     bool
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{runtime: &fakeRuntime{}, enforceSandbox: true, preparedTopology: ports.TopologyIndependentServer}
}
func (p *fakeProvider) Name() string                             { return "fake" }
func (p *fakeProvider) Capabilities() ports.ProviderCapabilities { return ports.ProviderCapabilities{} }
func (p *fakeProvider) Prepare(_ context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastPreparation = request
	if p.prepareErr != nil {
		return nil, p.prepareErr
	}
	evidence, _ := model.NewProviderEvidence("fake", 1, []byte("prepared:"+request.Spec.ExecutionID))
	p.runtime.id = request.Spec.ExecutionID
	p.runtime.attempt = request.Spec.Attempt
	p.runtime.observations = request.Observations
	p.runtime.initialEmitted = false
	p.runtime.order = 0
	p.runtime.evidence = evidence
	p.runtime.native = model.NativeConversationEvidence{Namespace: "fake", Reference: "native_initial", ObservedAt: time.Now()}
	return &fakePrepared{provider: p, description: ports.PreparedDescription{ExecutionID: request.Spec.ExecutionID, Attempt: request.Spec.Attempt, Topology: p.preparedTopology, Requirements: ports.RuntimeRequirements{WorkingDirectory: request.Spec.WorkingDirectory, Loopback: &ports.LoopbackRequirement{Protocol: "http"}}, EffectivePolicy: ports.EffectivePolicy{Approval: request.Spec.Approval, Sandbox: request.Spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: p.enforceSandbox}, Evidence: evidence}}, nil
}
func (p *fakeProvider) Recover(_ context.Context, request ports.RecoveryRequest) (ports.RecoveryResult, error) {
	p.recoveries++
	p.runtime.id = request.ExecutionID
	p.runtime.attempt = request.Attempt
	p.runtime.observations = request.Observations
	p.runtime.evidence = request.Evidence
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: p.runtime, Observation: p.runtime.observation(), Evidence: request.Evidence, Attempt: request.Attempt}, nil
}

type fakePrepared struct {
	provider    *fakeProvider
	description ports.PreparedDescription
}

func (p *fakePrepared) Describe() ports.PreparedDescription { return p.description }
func (p *fakePrepared) Abort(context.Context) error         { return nil }
func (p *fakePrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	p.provider.permitConsumed = true
	p.provider.releases++
	if p.provider.uncertainRelease {
		return ports.ReleaseResult{State: ports.ReleaseUncertain, Runtime: p.provider.runtime, Evidence: p.description.Evidence}, nil
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: p.provider.runtime, Evidence: p.description.Evidence}, nil
}

type fakeRuntime struct {
	id                                        model.ExecutionID
	evidence                                  model.ProviderEvidence
	native                                    model.NativeConversationEvidence
	interactions, attachments, contextChanges int
	stopped                                   bool
	omitNativeOnChange                        bool
	onInteract                                func()
	onObserve                                 func()
	attempt                                   model.AttemptGeneration
	observations                              ports.PrimaryObservationSink
	order                                     int
	initialEmitted                            bool
}

func (r *fakeRuntime) ExecutionID() model.ExecutionID { return r.id }
func (r *fakeRuntime) observation() ports.Observation {
	return ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Context: ports.ContextReady, NativeConversation: &r.native, Evidence: r.evidence}
}
func (r *fakeRuntime) Observe(context.Context) (ports.Observation, error) {
	if r.onObserve != nil {
		callback := r.onObserve
		r.onObserve = nil
		callback()
	}
	if !r.initialEmitted && r.observations != nil {
		r.order++
		r.initialEmitted = true
		_ = r.observations.ObservePrimaryContext(context.Background(), ports.PrimaryContextEvidence{ExecutionID: r.id, Attempt: r.attempt, Provider: "fake", PrimaryCorrelation: "primary", Disposition: ports.PrimaryContextInitial, NextBinding: &model.NativeBinding{Namespace: r.native.Namespace, Reference: r.native.Reference}, ProviderOrder: strconv.Itoa(r.order), ObservedAt: time.Now()})
	}
	return r.observation(), nil
}
func (r *fakeRuntime) Interact(context.Context, ports.Interaction) (ports.InteractionResult, error) {
	r.interactions++
	if r.onInteract != nil {
		r.onInteract()
	}
	return ports.InteractionResult{Disposition: ports.EffectAccepted, Evidence: r.evidence}, nil
}
func (r *fakeRuntime) Attach(context.Context, ports.AttachmentRequest) (ports.AttachmentResult, error) {
	r.attachments++
	return ports.AttachmentResult{Disposition: ports.EffectAccepted, Attachment: &fakeAttachment{}, Evidence: r.evidence}, nil
}
func (r *fakeRuntime) ChangeContext(ctx context.Context, change ports.ContextChange) (ports.ContextChangeResult, error) {
	r.contextChanges++
	if r.omitNativeOnChange {
		return ports.ContextChangeResult{Disposition: ports.EffectAccepted, Evidence: r.evidence}, nil
	}
	prior := model.NativeBinding{Namespace: r.native.Namespace, Reference: r.native.Reference}
	r.native = model.NativeConversationEvidence{Namespace: "fake", Reference: "native_rotated", ObservedAt: time.Now()}
	r.order++
	if r.observations != nil {
		err := r.observations.ObservePrimaryContext(ctx, ports.PrimaryContextEvidence{ExecutionID: r.id, Attempt: r.attempt, Provider: "fake", PrimaryCorrelation: "primary", Disposition: ports.PrimaryContextReset, PriorBinding: &prior, NextBinding: &model.NativeBinding{Namespace: r.native.Namespace, Reference: r.native.Reference}, TransitionCorrelation: change.TransitionCorrelation, ExpectedConversation: change.ExpectedConversation, ExpectedAssociationRevision: change.ExpectedAssociationRevision, PriorProviderOrder: strconv.Itoa(r.order - 1), ProviderOrder: strconv.Itoa(r.order), ObservedAt: time.Now()})
		if err != nil {
			return ports.ContextChangeResult{}, err
		}
	}
	return ports.ContextChangeResult{Disposition: ports.EffectAccepted, NativeConversation: &r.native, Evidence: r.evidence}, nil
}
func (r *fakeRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	r.stopped = true
	return ports.StopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: true, Evidence: r.evidence}, nil
}

type fakeAttachment struct{ bytes.Buffer }

func (*fakeAttachment) Close() error               { return nil }
func (*fakeAttachment) Kind() ports.AttachmentKind { return ports.AttachmentTerminal }

var _ io.ReadWriteCloser = (*fakeAttachment)(nil)
