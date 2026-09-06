package app_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestJourneyPersistsScopedHistoryOwnedWorkspaceAndExactOutcome(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "journey.db")
	store, err := backendsqlite.Open(dbPath)
	require.NoError(t, err)
	provider := newJourneyProvider()
	host := &journeyWorkspaceHost{}
	service := journeyService(store, provider, host)
	operator := model.OperatorPrincipal()

	refreshed, err := service.RefreshHistory(ctx, app.RefreshHistoryRequest{Principal: operator, Harness: "journey", SourceName: "default"})
	require.NoError(t, err)
	require.Len(t, refreshed.Entries, 1)
	require.Equal(t, model.HistoryCoverageComplete, refreshed.Coverage.Metadata)
	entry := refreshed.Entries[0]
	metadataRequest := app.SetConversationMetadataRequest{Context: request(operator, "set_history_metadata"), ConversationID: entry.ConversationID, ExpectedRevision: entry.Revision, Title: "Renamed prior work", Archived: false}
	metadata, err := service.SetConversationMetadata(ctx, metadataRequest)
	require.NoError(t, err)
	repeatedMetadata, err := service.SetConversationMetadata(ctx, metadataRequest)
	require.NoError(t, err)
	require.Equal(t, metadata.Entries[0].Revision, repeatedMetadata.Entries[0].Revision)
	entry = metadata.Entries[0]
	resolved, err := store.ResolveHistory(ctx, model.HistorySelection{ConversationID: entry.ConversationID, ExpectedConversationRevision: entry.Revision})
	require.NoError(t, err)
	require.NotEmpty(t, resolved.Source.SourceToken, "provider source token is private but durably resolvable")

	read, err := service.ReadHistory(ctx, app.ReadHistoryRequest{Principal: operator, Selection: model.HistorySelection{ConversationID: entry.ConversationID, ExpectedConversationRevision: entry.Revision}})
	require.NoError(t, err)
	require.Equal(t, "earlier work", read.Turns[0].Parts[0].Text)
	require.Empty(t, read.Turns[0].PointID, "provider point tokens are not exposed as platform IDs")
	found, err := service.SearchHistory(ctx, app.SearchHistoryRequest{Principal: operator, Query: "earlier work"})
	require.NoError(t, err)
	require.Len(t, found.Entries, 1)
	entry = found.Entries[0]

	workspacePath := filepath.Join(t.TempDir(), "checkout")
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(operator, "create_workspace"), ID: "workspace_one", Intent: model.WorkspaceIntent{Repository: "repo", IntendedPath: workspacePath, BaseRevision: "main", Branch: "work"}})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceAvailable, workspace.Workspace.State)
	require.Equal(t, 1, host.creates)
	require.NotContains(t, fmt.Sprintf("%+v", workspace.Workspace), "ownership-marker", "public workspace view strips host receipt")

	desired := model.DesiredConfiguration{Harness: "journey", Model: "test", WorkingDirectory: workspacePath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker_one", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	run, err := service.StartWork(ctx, app.StartWorkRequest{Context: request(operator, "start_work"), ID: "work_one", Spec: model.WorkRunSpec{SourceMode: model.WorkSourceFork, History: model.HistorySelection{ConversationID: entry.ConversationID, ExpectedConversationRevision: entry.Revision}, WorkspaceID: workspace.Workspace.ID, WorkspaceRevision: workspace.Workspace.Revision, WorkerAgentID: agent.Agent.ID, WorkerAgentRevision: agent.Agent.Revision, WorkerDesired: desired, Brief: "make the bounded change", Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision}}})
	require.NoError(t, err)
	require.Equal(t, model.WorkRunRunning, run.Run.State)
	require.Len(t, run.Run.Attempts, 6)
	require.NotEmpty(t, run.Run.HistoryUseID)
	require.NotEmpty(t, run.Run.WorkspaceUseID)

	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(dbPath)
	require.NoError(t, err)
	service = journeyService(store, provider, host)
	reconcile, err := service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Empty(t, reconcile.Pending)
	require.Equal(t, 1, host.creates, "restart sweep does not replay an admitted native effect")
	require.Equal(t, 1, provider.releases)
	require.Equal(t, 1, provider.deliveries)
	run, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: operator, WorkRunID: "work_one"})
	require.NoError(t, err)

	recorded, err := service.RecordWorkEvidence(ctx, app.RecordWorkEvidenceRequest{Context: request(operator, "report_evidence"), WorkRunID: "work_one", Step: model.WorkStepAwaitEvidence, Attempt: 1, Kind: model.WorkEvidenceWorkerReport, ArtifactRevision: "abc", Detail: "done", ExpectedRunRevision: run.Run.Revision})
	require.NoError(t, err)
	repeatedEvidence, err := service.RecordWorkEvidence(ctx, app.RecordWorkEvidenceRequest{Context: request(operator, "report_evidence"), WorkRunID: "work_one", Step: model.WorkStepAwaitEvidence, Attempt: 1, Kind: model.WorkEvidenceWorkerReport, ArtifactRevision: "abc", Detail: "done", ExpectedRunRevision: run.Run.Revision})
	require.NoError(t, err)
	require.Equal(t, recorded.Run.Revision, repeatedEvidence.Run.Revision)
	decided, err := service.DecideWork(ctx, app.DecideWorkRequest{Context: request(operator, "decide_report"), WorkRunID: "work_one", Step: model.WorkStepAwaitEvidence, Attempt: 1, Decision: model.WorkDecisionAccept, Reason: "authorized human review", ExpectedRunRevision: recorded.Run.Revision})
	require.NoError(t, err)
	repeatedDecision, err := service.DecideWork(ctx, app.DecideWorkRequest{Context: request(operator, "decide_report"), WorkRunID: "work_one", Step: model.WorkStepAwaitEvidence, Attempt: 1, Decision: model.WorkDecisionAccept, Reason: "authorized human review", ExpectedRunRevision: recorded.Run.Revision})
	require.NoError(t, err)
	require.Equal(t, decided.Run.Revision, repeatedDecision.Run.Revision)
	require.Equal(t, model.WorkRunSucceeded, decided.Run.State)
	require.NotNil(t, decided.Decision)

	removed, err := service.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: request(operator, "remove_workspace"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceRemoved, removed.Workspace.State)
	require.Equal(t, 1, host.removes)
}

func TestWorkRunPinsRevisionsAndBlocksSharedHistoryAndWorkspaceCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "journey.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newJourneyProvider()
	host := &journeyWorkspaceHost{}
	service := journeyService(store, provider, host)
	operator := model.OperatorPrincipal()
	history, err := service.RefreshHistory(ctx, app.RefreshHistoryRequest{Principal: operator, Harness: "journey", SourceName: "default"})
	require.NoError(t, err)
	workspacePath := filepath.Join(t.TempDir(), "checkout")
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(operator, "create"), ID: "workspace_two", Intent: model.WorkspaceIntent{IntendedPath: workspacePath}})
	require.NoError(t, err)
	desired := model.DesiredConfiguration{Harness: "journey", Model: "test", WorkingDirectory: workspacePath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	worker, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker_two", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	spec := model.WorkRunSpec{SourceMode: model.WorkSourceFork, History: model.HistorySelection{ConversationID: history.Entries[0].ConversationID, ExpectedConversationRevision: history.Entries[0].Revision}, WorkspaceID: workspace.Workspace.ID, WorkspaceRevision: workspace.Workspace.Revision, WorkerAgentID: worker.Agent.ID, WorkerAgentRevision: worker.Agent.Revision, WorkerDesired: desired, Brief: "bounded", Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision}}
	run, err := service.StartWork(ctx, app.StartWorkRequest{Context: request(operator, "run_one"), ID: "run_one", Spec: spec})
	require.NoError(t, err)
	_, err = service.StartWork(ctx, app.StartWorkRequest{Context: request(operator, "run_two"), ID: "run_two", Spec: spec})
	require.Error(t, err, "provider-required exclusive history use is durable")
	_, err = service.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: request(operator, "remove_busy"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Destructive: true})
	require.ErrorIs(t, err, app.ErrConflict, "destructive intent does not bypass active exact-use claims")
	_, err = service.CancelWork(ctx, app.CancelWorkRequest{Context: request(operator, "cancel"), WorkRunID: run.Run.ID, ExpectedRunRevision: run.Run.Revision, Reason: "operator cancelled"})
	require.NoError(t, err)
}

func TestShellOwnsExactWorkspaceUntilStopped(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "journey.db")
	store, err := backendsqlite.Open(dbPath)
	require.NoError(t, err)
	host := &journeyWorkspaceHost{}
	shell := &journeyShellHost{}
	service := journeyService(store, newJourneyProvider(), host).WithShellHost(shell)
	operator := model.OperatorPrincipal()
	workspace, err := service.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: request(operator, "create_shell_workspace"), ID: "shell_workspace", Intent: model.WorkspaceIntent{Repository: "repo", IntendedPath: filepath.Join(t.TempDir(), "checkout"), BaseRevision: "main", Branch: "shell"}})
	require.NoError(t, err)
	started, err := service.StartShell(ctx, app.StartShellRequest{Context: request(operator, "start_shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision, Sandbox: model.SandboxUnconfined})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionWorkloadShell, started.Execution.Workload)
	require.Equal(t, model.ExecutionRunning, started.Execution.State)
	_, err = service.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: request(operator, "remove_active_shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision})
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = journeyService(store, newJourneyProvider(), host).WithShellHost(shell)
	recovered, err := service.Recover(ctx, app.RecoverRequest{Principal: operator})
	require.NoError(t, err)
	require.Equal(t, []model.ExecutionID{started.Execution.ID}, recovered.Controlled)
	stopped, err := service.Stop(ctx, app.StopRequest{RequestContext: request(operator, "stop_shell"), ExecutionID: started.Execution.ID})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stopped.Execution.State)
	removed, err := service.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: request(operator, "remove_stopped_shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: workspace.Workspace.Revision})
	require.NoError(t, err)
	restored, err := service.RestoreCheckout(ctx, app.RestoreCheckoutRequest{Context: request(operator, "restore_stopped_shell"), WorkspaceID: workspace.Workspace.ID, ExpectedRevision: removed.Workspace.Revision})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceAvailable, restored.Workspace.State)
	require.Equal(t, 2, host.creates)
}

type journeyProvider struct {
	reader               *journeyHistoryReader
	releases, deliveries int
}

func newJourneyProvider() *journeyProvider { return &journeyProvider{reader: &journeyHistoryReader{}} }
func (*journeyProvider) Name() string      { return "journey" }
func (p *journeyProvider) Prepare(_ context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return &journeyPrepared{provider: p, request: request}, nil
}
func (*journeyProvider) Recover(context.Context, ports.RecoveryRequest) (ports.RecoveryResult, error) {
	return ports.RecoveryResult{State: ports.RecoveryUnknown}, nil
}
func (p *journeyProvider) History() ports.HistoryReader { return p.reader }

type journeyPrepared struct {
	provider *journeyProvider
	request  ports.PreparationRequest
}

func (p *journeyPrepared) Describe() ports.PreparedDescription {
	return ports.PreparedDescription{ExecutionID: p.request.Spec.ExecutionID, Attempt: p.request.Spec.Attempt, Topology: ports.TopologyTerminalAuthoritative, Requirements: ports.RuntimeRequirements{WorkingDirectory: p.request.Spec.WorkingDirectory, Terminal: &ports.TerminalRequirement{Interactive: true}, Policy: ports.PolicyRequirements{SupportedApproval: []model.ApprovalMode{p.request.Spec.Approval}, SupportedSandbox: []model.SandboxMode{p.request.Spec.Sandbox}}}, EffectivePolicy: ports.EffectivePolicy{Approval: p.request.Spec.Approval, Sandbox: p.request.Spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: true}, Evidence: model.ProviderEvidence{Provider: "journey", Version: 1}}
}
func (p *journeyPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	p.provider.releases++
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &journeyRuntime{id: p.request.Spec.ExecutionID, provider: p.provider}, Evidence: model.ProviderEvidence{Provider: "journey", Version: 1}}, nil
}
func (*journeyPrepared) Abort(context.Context) error { return nil }

type journeyRuntime struct {
	id       model.ExecutionID
	provider *journeyProvider
}

func (r *journeyRuntime) ExecutionID() model.ExecutionID { return r.id }
func (r *journeyRuntime) Observe(context.Context) (ports.Observation, error) {
	return ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Context: ports.ContextReady, Evidence: model.ProviderEvidence{Provider: "journey", Version: 1}}, nil
}
func (r *journeyRuntime) Interact(context.Context, ports.Interaction) (ports.InteractionResult, error) {
	r.provider.deliveries++
	return ports.InteractionResult{Disposition: ports.EffectAccepted, Evidence: model.ProviderEvidence{Provider: "journey", Version: 1}}, nil
}
func (*journeyRuntime) Attach(context.Context, ports.AttachmentRequest) (ports.AttachmentResult, error) {
	return ports.AttachmentResult{Disposition: ports.EffectUnsupported}, nil
}
func (*journeyRuntime) ChangeContext(context.Context, ports.ContextChange) (ports.ContextChangeResult, error) {
	return ports.ContextChangeResult{Disposition: ports.EffectUnsupported}, nil
}
func (*journeyRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	return ports.StopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: true}, nil
}

type journeyHistoryReader struct{}

func (*journeyHistoryReader) Capabilities() ports.HistoryCapabilities {
	return ports.HistoryCapabilities{MetadataDiscovery: true, ContentRead: true, ContinuationPrecision: ports.HistoryPrecisionHead, ForkPrecision: ports.HistoryPrecisionHead, ForkRequiresExclusive: true}
}
func (*journeyHistoryReader) Discover(context.Context, ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	now := time.Unix(1700000000, 0).UTC()
	coverage := model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoveragePartial, SourceRevision: "source-v1", RefreshedAt: now}
	return ports.HistoryDiscoveryResult{Histories: []ports.DiscoveredHistory{{Native: model.NativeConversationEvidence{Namespace: "journey", Reference: "native-one", ObservedAt: now}, SourceToken: "private-root-one", SourceFingerprint: "fingerprint-v1", Title: "Prior work", WorkspaceHint: "/repo", ModifiedAt: now, Availability: model.HistoryContent, Coverage: coverage, Evidence: model.ProviderEvidence{Provider: "journey", Version: 1, Payload: []byte("source-evidence")}}}, Coverage: coverage}, nil
}
func (*journeyHistoryReader) Read(context.Context, ports.HistorySourceSelection) (ports.HistoryReadResult, error) {
	coverage := model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, SourceRevision: "source-v1", RefreshedAt: time.Unix(1700000010, 0).UTC()}
	return ports.HistoryReadResult{Turns: []ports.HistoryTurn{{Role: "user", Parts: []ports.HistoryPart{{Kind: ports.HistoryPartText, Text: "earlier work"}}}}, Coverage: coverage, Evidence: model.ProviderEvidence{Provider: "journey", Version: 1}}, nil
}

type journeyWorkspaceHost struct{ creates, removes int }

func (h *journeyWorkspaceHost) CreateCheckout(ctx context.Context, req ports.CheckoutCreateRequest, permit ports.EffectPermit) (ports.WorkspaceEffectResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.WorkspaceEffectResult{}, err
	}
	h.creates++
	return ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Observation: model.WorkspaceObservation{ActualPath: req.Intent.IntendedPath, RepositoryRoot: req.Intent.Repository, Revision: req.Intent.BaseRevision, Branch: req.Intent.Branch, ObservedAt: time.Now()}, Resource: model.WorkspaceResourceEvidence{Owner: "journey-host", Version: 1, Payload: []byte("ownership-marker")}}, nil
}

type journeyShellHost struct{}

func (*journeyShellHost) PrepareShell(_ context.Context, request ports.ShellPreparationRequest) (ports.PreparedShell, error) {
	return &journeyPreparedShell{request: request}, nil
}
func (*journeyShellHost) RecoverShell(_ context.Context, request ports.ShellRecoveryRequest) (ports.ShellRecoveryResult, error) {
	return ports.ShellRecoveryResult{State: ports.RecoveryControlled, Runtime: &journeyHostRuntime{id: request.ExecutionID}, Observation: ports.HostObservation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Evidence: modelShellEvidence()}, Evidence: modelShellEvidence()}, nil
}

type journeyPreparedShell struct{ request ports.ShellPreparationRequest }

func (p *journeyPreparedShell) Describe() ports.ShellPreparedDescription {
	return ports.ShellPreparedDescription{ExecutionID: p.request.ExecutionID, Attempt: p.request.Attempt, Evidence: modelShellEvidence()}
}
func (p *journeyPreparedShell) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ShellReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ShellReleaseResult{}, err
	}
	return ports.ShellReleaseResult{State: ports.ReleaseStarted, Runtime: &journeyHostRuntime{id: p.request.ExecutionID}, Evidence: modelShellEvidence()}, nil
}
func (*journeyPreparedShell) Abort(context.Context) error { return nil }

type journeyHostRuntime struct{ id model.ExecutionID }

func (r *journeyHostRuntime) ExecutionID() model.ExecutionID { return r.id }
func (*journeyHostRuntime) ObserveHost(context.Context) (ports.HostObservation, error) {
	return ports.HostObservation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Evidence: modelShellEvidence()}, nil
}
func (*journeyHostRuntime) AttachHost(context.Context, ports.AttachmentRequest) (ports.HostAttachmentResult, error) {
	return ports.HostAttachmentResult{Disposition: ports.EffectUnsupported, Evidence: modelShellEvidence()}, nil
}
func (*journeyHostRuntime) StopHost(context.Context, ports.StopRequest) (ports.HostStopResult, error) {
	return ports.HostStopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: true, Evidence: modelShellEvidence()}, nil
}
func modelShellEvidence() ports.ShellResourceEvidence {
	return ports.ShellResourceEvidence{Owner: "shell-host", Version: 1, Payload: []byte("private-shell-receipt")}
}
func (h *journeyWorkspaceHost) InspectWorkspace(context.Context, model.Workspace) (ports.WorkspaceEffectResult, error) {
	return ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted}, nil
}
func (h *journeyWorkspaceHost) RemoveCheckout(ctx context.Context, req ports.CheckoutRemoveRequest, permit ports.EffectPermit) (ports.WorkspaceEffectResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.WorkspaceEffectResult{}, err
	}
	if string(req.Resource.Payload) != "ownership-marker" {
		return ports.WorkspaceEffectResult{Disposition: ports.EffectRefused}, nil
	}
	h.removes++
	return ports.WorkspaceEffectResult{Disposition: ports.EffectAccepted, Observation: req.Observation, Resource: req.Resource}, nil
}

func request(principal model.Principal, id model.RequestID) app.RequestContext {
	return app.RequestContext{Principal: principal, RequestID: id}
}

type journeySources struct{}

func (journeySources) HistorySource(harness, name string) (ports.HistoryDiscoveryScope, bool) {
	if harness != "journey" || name != "default" {
		return ports.HistoryDiscoveryScope{}, false
	}
	return ports.HistoryDiscoveryScope{Source: "configured-root"}, true
}
func journeyService(store app.Store, provider *journeyProvider, host *journeyWorkspaceHost) *app.Service {
	return app.New(store, providers.NewRegistry(provider)).WithHistorySources(journeySources{}).WithWorkspaceHost(host)
}
