package transport

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// Native history/effects are doubled; the public mux, application, SQLite,
// credential resources and Git checkout are production implementations.
type workFlowProvider struct {
	*deliveredLifecycleProvider
	texts []string
}

func (p *workFlowProvider) History() ports.HistoryReader { return workFlowHistory{} }
func (p *workFlowProvider) Prepare(ctx context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	prepared, err := p.deliveredLifecycleProvider.Prepare(ctx, r)
	if err != nil {
		return nil, err
	}
	return &workFlowPrepared{PreparedAttempt: prepared, p: p}, nil
}

type workFlowPrepared struct {
	ports.PreparedAttempt
	p *workFlowProvider
}

func (p *workFlowPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	result, err := p.PreparedAttempt.Release(ctx, permit)
	if result.Runtime != nil {
		result.Runtime = &workFlowRuntime{Runtime: result.Runtime, p: p.p}
	}
	return result, err
}
func (p *workFlowProvider) Recover(ctx context.Context, r ports.RecoveryRequest) (ports.RecoveryResult, error) {
	result, err := p.deliveredLifecycleProvider.Recover(ctx, r)
	if result.Runtime != nil {
		result.Runtime = &workFlowRuntime{Runtime: result.Runtime, p: p}
	}
	return result, err
}

type workFlowRuntime struct {
	ports.Runtime
	p *workFlowProvider
}

func (r *workFlowRuntime) Interact(_ context.Context, i ports.Interaction) (ports.InteractionResult, error) {
	r.p.texts = append(r.p.texts, i.Text)
	return ports.InteractionResult{Disposition: ports.EffectAccepted, Evidence: lifecycleEvidence()}, nil
}

type workFlowHistory struct{}

func (workFlowHistory) Capabilities() ports.HistoryCapabilities {
	return ports.HistoryCapabilities{MetadataDiscovery: true, ContentRead: true, ForkPrecision: ports.HistoryPrecisionNone}
}
func (workFlowHistory) Discover(context.Context, ports.HistoryDiscoveryRequest) (ports.HistoryDiscoveryResult, error) {
	coverage := model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoveragePartial, SourceRevision: "v1", RefreshedAt: time.Now()}
	return ports.HistoryDiscoveryResult{Coverage: coverage, Histories: []ports.DiscoveredHistory{{Native: model.NativeConversationEvidence{Namespace: "fixture", Reference: "prior"}, SourceToken: "private-token", SourceFingerprint: "fingerprint", Title: "Earlier work", Availability: model.HistoryContent, Coverage: coverage, Evidence: lifecycleEvidence()}}}, nil
}
func (workFlowHistory) Read(context.Context, ports.HistorySourceSelection) (ports.HistoryReadResult, error) {
	return ports.HistoryReadResult{Turns: []ports.HistoryTurn{{Role: "user", Parts: []ports.HistoryPart{{Kind: ports.HistoryPartText, Text: "Earlier design conclusion"}}}}, Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageComplete, SourceRevision: "v1", RefreshedAt: time.Now()}, Evidence: lifecycleEvidence()}, nil
}

type workFlowSources struct{}

func (workFlowSources) HistorySource(h, n string) (ports.HistoryDiscoveryScope, bool) {
	return ports.HistoryDiscoveryScope{}, h == "test-native" && n == "fixture"
}

func TestPublicWorkHistoryHandoffOutcomeAndRestart(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-b", "main", repo)
	git("-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base")
	checkout, err := host.NewCheckoutHost("")
	require.NoError(t, err)
	native := &workFlowProvider{deliveredLifecycleProvider: &deliveredLifecycleProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")}, receipts: map[model.ExecutionID]ports.ActionCredentialReceipt{}, runtimes: map[model.ExecutionID]*lifecycleRuntime{}}}
	var store *sqlite.Store
	var service *app.Service
	var handler *Handler
	open := func() {
		var err error
		store, err = sqlite.Open(filepath.Join(root, "state.sqlite"))
		require.NoError(t, err)
		service = app.New(store, providers.NewRegistry(native)).WithAgentAPIEndpoint(filepath.Join(root, "api.sock")).WithWorkspaceHost(checkout).WithHistorySources(workFlowSources{})
		handler = testHandler(t, service)
		require.NoError(t, handler.RegisterJourneyAPI(service))
	}
	open()
	defer func() { require.NoError(t, store.Close()) }()
	call := func(method, path string, body, out any) {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		w := request(handler, method, path, string(data), testCredential)
		require.Less(t, w.Code, 300, "%s %s", path, w.Body)
		if out != nil {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), out), "%s", w.Body)
		}
	}
	var history app.HistorySearchResult
	call("POST", "/v2/history/refresh", map[string]any{"harness": "test-native", "source": "fixture"}, &history)
	require.Len(t, history.Entries, 1)
	var read app.HistoryReadResult
	call("POST", "/v2/history/read", map[string]any{"selection": model.HistorySelection{ConversationID: history.Entries[0].ConversationID, ExpectedConversationRevision: history.Entries[0].Revision}}, &read)
	require.Equal(t, "Earlier design conclusion", read.Turns[0].Parts[0].Text)
	var workspace app.WorkspaceResult
	path := filepath.Join(root, "checkout")
	call("POST", "/v2/workspaces/create", map[string]any{"request_id": "create", "id": "workspace_work", "intent": model.WorkspaceIntent{Repository: repo, IntendedPath: path, BaseRevision: "main", Branch: "worker"}}, &workspace)
	desired := model.DesiredConfiguration{Harness: "test-native", WorkingDirectory: path, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	var agent model.Agent
	call("POST", "/v2/agents", map[string]any{"id": "worker", "name": "Worker", "desired": desired}, &agent)
	spec := model.WorkRunSpec{SourceMode: model.WorkSourceFreshHandoff, FreshHandoff: read.Turns[0].Parts[0].Text, WorkspaceID: workspace.Workspace.ID, WorkspaceRevision: workspace.Workspace.Revision, WorkerAgentID: agent.ID, WorkerAgentRevision: agent.Revision, WorkerDesired: desired, Brief: "Implement bounded result", Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision}}
	start := map[string]any{"request_id": "work-request", "id": "work-one", "spec": spec}
	var work workResultView
	call("POST", "/v2/work", start, &work)
	require.NoError(t, func() error { _, err := service.ReconcilePendingWork(ctx); return err }())
	call("GET", "/v2/work/work-one", nil, &work)
	if work.Run.State != model.WorkRunWaiting {
		record, _ := store.WorkRun(ctx, work.Run.ID)
		t.Fatalf("work failed: %+v", record.Run.Attempts)
	}
	call("POST", "/v2/work", start, &work)
	require.Equal(t, 1, native.releases)
	require.Len(t, native.texts, 1)
	require.Contains(t, native.texts[0], "Earlier design conclusion")
	require.NoError(t, store.Close())
	open()
	_, err = service.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, native.releases)
	require.Len(t, native.texts, 1)
	call("GET", "/v2/work/work-one", nil, &work)
	call("POST", "/v2/work/evidence", map[string]any{"request_id": "evidence", "work_run_id": work.Run.ID, "expected_revision": work.Run.Revision, "step": model.WorkStepAwaitEvidence, "attempt": 1, "kind": model.WorkEvidenceWorkerReport, "artifact_revision": "result-commit", "detail": "Bounded result ready"}, &work)
	call("POST", "/v2/work/decision", map[string]any{"request_id": "decision", "work_run_id": work.Run.ID, "expected_revision": work.Run.Revision, "step": model.WorkStepAwaitEvidence, "attempt": 1, "decision": model.WorkDecisionAccept, "reason": "Human verified result"}, &work)
	require.NotNil(t, work.Decision)
	require.Equal(t, model.PrincipalOperator, work.Decision.Decider.Kind)
	require.DirExists(t, path)
	data, _ := json.Marshal(map[string]any{"request_id": "early-remove", "workspace_id": workspace.Workspace.ID, "expected_revision": workspace.Workspace.Revision})
	denied := request(handler, "POST", "/v2/workspaces/remove", string(data), testCredential)
	require.Equal(t, 409, denied.Code, "live worker claim survives outcome: %s", denied.Body)
	call("POST", "/v2/stop", map[string]any{"request_id": "stop-worker", "execution_id": work.Run.WorkerExecutionID, "force": true}, nil)
	call("GET", "/v2/workspaces/workspace_work", nil, &workspace)
	call("POST", "/v2/workspaces/remove", map[string]any{"request_id": "remove", "workspace_id": workspace.Workspace.ID, "expected_revision": workspace.Workspace.Revision}, nil)
	require.NoDirExists(t, path)
}
