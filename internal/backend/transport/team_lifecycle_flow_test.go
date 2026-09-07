//go:build linux || darwin

package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// teamPublicProvider is the only doubled boundary in this journey: it stands
// in for an external harness process. Authentication, HTTP routing, application
// admission/reconciliation, SQLite, messaging and checkout effects are real.
type teamPublicProvider struct {
	ports.Provider
	runtimes map[model.ExecutionID]*lifecycleRuntime
}

func (*teamPublicProvider) Name() string { return "test-native" }
func (*teamPublicProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{PreparedInitialInput: true}
}
func (p *teamPublicProvider) Prepare(_ context.Context, request ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return &teamPublicPrepared{provider: p, spec: request.Spec, sink: request.Observations, initialInput: request.InitialInput}, nil
}
func (p *teamPublicProvider) Recover(ctx context.Context, request ports.RecoveryRequest) (ports.RecoveryResult, error) {
	runtime := p.runtimes[request.ExecutionID]
	if runtime == nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown}, nil
	}
	runtime.sink = request.Observations
	observation, err := runtime.Observe(ctx)
	if err != nil {
		return ports.RecoveryResult{}, err
	}
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: runtime, Observation: observation, Evidence: lifecycleEvidence(), Attempt: request.Attempt}, nil
}

type teamPublicPrepared struct {
	provider     *teamPublicProvider
	spec         model.ResolvedExecutionSpec
	sink         ports.PrimaryObservationSink
	initialInput *ports.PreparedInitialInput
}

func (p *teamPublicPrepared) Describe() ports.PreparedDescription {
	description := ports.PreparedDescription{ExecutionID: p.spec.ExecutionID, Topology: ports.TopologyTerminalAuthoritative, Evidence: lifecycleEvidence(), EffectivePolicy: ports.EffectivePolicy{Approval: p.spec.Approval, Sandbox: p.spec.Sandbox, ApprovalEnforced: true, SandboxEnforced: true}}
	if p.initialInput != nil {
		description.InitialInput = &ports.PreparedInitialInputDescription{Supported: true, Correlation: p.initialInput.Correlation}
	}
	return description
}
func (*teamPublicPrepared) Abort(context.Context) error { return nil }
func (p *teamPublicPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	runtime := &lifecycleRuntime{id: p.spec.ExecutionID, attempt: p.spec.Attempt, sink: p.sink}
	p.provider.runtimes[p.spec.ExecutionID] = runtime
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: runtime, Evidence: lifecycleEvidence()}, nil
}

func TestPublicTeamLifecycleSurvivesRestartAndIsolatesReinforcement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.sqlite")
	checkoutPath := filepath.Join(root, "team-checkout")
	repository := teamLifecycleRepository(t)
	checkoutHost, err := host.NewCheckoutHost("git")
	require.NoError(t, err)
	provider := &teamPublicProvider{runtimes: map[model.ExecutionID]*lifecycleRuntime{}}

	var store *sqlite.Store
	var service *app.Service
	var handler *Handler
	open := func() {
		store, err = sqlite.Open(databasePath)
		require.NoError(t, err)
		service = app.New(store, providers.NewRegistry(provider)).WithWorkspaceHost(checkoutHost)
		handler = testHandler(t, service)
		require.NoError(t, handler.RegisterOrchestrationAPI(service))
	}
	open()
	defer func() { require.NoError(t, store.Close()) }()
	call := func(method, path string, body, result any) string {
		t.Helper()
		var encoded []byte
		if body != nil {
			encoded, err = json.Marshal(body)
			require.NoError(t, err)
		}
		response := request(handler, method, path, string(encoded), testCredential)
		require.Less(t, response.Code, 300, "%s %s: %s", method, path, response.Body)
		if result != nil {
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), result))
		}
		return response.Body.String()
	}

	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: "/authored/placeholder", Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	call(http.MethodPost, "/v2/agents", map[string]any{"id": "shared_member", "name": "shared member", "desired": desired}, nil)
	call(http.MethodPost, "/v2/groups", map[string]any{"id": "reinforcement_group", "name": "reinforcement target", "members": []model.AgentID{"shared_member"}}, nil)
	teamV1 := model.TeamDefinition{
		WorkspacePolicy: model.WorkspacePolicyShared,
		Members:         []model.TeamMemberSpec{{Key: "builder", Name: "builder", Desired: desired, Required: true}, {Key: "reviewer", Name: "reviewer", Desired: desired, Required: true}},
		Waves:           []model.TeamWave{{ID: "build", MemberKeys: []string{"builder"}, RequiredReady: true, RequiredBriefs: true}, {ID: "review", MemberKeys: []string{"reviewer"}, DependsOn: []string{"build"}, RequiredReady: true}},
		Briefings:       []model.TeamBriefing{{ID: "ready", Body: "coordinate after readiness", Timing: model.BriefingAfterReady, Required: true, MemberKeys: []string{"builder"}}},
		AdvisoryPhases:  []string{"investigate", "recommend"},
	}
	var definition app.DefinitionResult
	call(http.MethodPost, "/v2/definitions", map[string]any{"request_id": "save_team_v1", "draft": app.DefinitionDraft{ID: "public_team", RevisionID: "public_team_v1", Name: "public team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "v1", Team: &teamV1}}, &definition)
	refV1 := model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}
	deployBody := map[string]any{"request_id": "deploy_public_team", "deployment_id": "public_deployment", "instantiation": model.TeamInstantiation{
		Definition: refV1, Mission: "ship through public routes", Target: model.TeamDeploymentTarget{Kind: model.TeamTargetNewGroup, GroupID: "public_team_group"},
		Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "public_team_workspace", CreateIntent: &model.WorkspaceIntent{Repository: repository, IntendedPath: checkoutPath, BaseRevision: "HEAD", Branch: "feature/public-team", RetainOnFinish: true}}},
	}}
	var deployed app.TeamDeploymentResult
	body := call(http.MethodPost, "/v2/teams/deploy", deployBody, &deployed)
	require.NotContains(t, body, "request_digest")
	require.NotContains(t, body, "requester_json")
	require.DirExists(t, checkoutPath)
	for range 12 {
		_, reconcileErr := service.ReconcilePendingWork(ctx)
		require.NoError(t, reconcileErr)
	}
	call(http.MethodGet, "/v2/teams/deployments/"+string(deployed.Deployment.ID), nil, &deployed)
	if deployed.Deployment.State != model.DeploymentReady {
		run, readErr := store.WorkRun(ctx, deployed.Deployment.WorkRunID)
		t.Logf("deployment work: %+v (read error: %v)", run.Run, readErr)
	}
	require.Equal(t, model.DeploymentReady, deployed.Deployment.State, "%+v", deployed.Deployment)
	require.NotEmpty(t, deployed.Deployment.BriefingOperationIDs["builder"])

	workspace, err := store.Workspace(ctx, "public_team_workspace")
	require.NoError(t, err)
	reinforcementBody := map[string]any{"request_id": "deploy_reinforcement", "deployment_id": "public_reinforcement", "instantiation": model.TeamInstantiation{
		Definition: refV1, Mission: "reinforce", Target: model.TeamDeploymentTarget{Kind: model.TeamTargetExistingGroup, GroupID: "reinforcement_group"},
		Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: workspace.ID, ExpectedRevision: workspace.Revision}},
	}}
	var reinforcement app.TeamDeploymentResult
	call(http.MethodPost, "/v2/teams/deploy", reinforcementBody, &reinforcement)
	for range 12 {
		_, reconcileErr := service.ReconcilePendingWork(ctx)
		require.NoError(t, reconcileErr)
	}
	call(http.MethodGet, "/v2/teams/deployments/"+string(reinforcement.Deployment.ID), nil, &reinforcement)
	require.Equal(t, model.DeploymentReady, reinforcement.Deployment.State, "%+v", reinforcement.Deployment)

	require.NoError(t, store.Close())
	open()
	var listed []app.TeamDeploymentResult
	call(http.MethodGet, "/v2/teams/deployments", nil, &listed)
	require.Len(t, listed, 2)
	var repeated app.TeamDeploymentResult
	call(http.MethodPost, "/v2/teams/deploy", deployBody, &repeated)
	require.Equal(t, deployed.Deployment.Revision, repeated.Deployment.Revision)
	_, err = service.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)

	teamV2 := teamV1
	teamV2.Briefings = []model.TeamBriefing{{ID: "ready", Body: "coordinate with the revised pattern", Timing: model.BriefingAfterReady, Required: true, MemberKeys: []string{"builder"}}}
	var revised app.DefinitionResult
	call(http.MethodPost, "/v2/definitions", map[string]any{"request_id": "save_team_v2", "expected_revision": definition.Definition.Revision, "draft": app.DefinitionDraft{ID: definition.Definition.ID, RevisionID: "public_team_v2", Name: "public team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "v2", Team: &teamV2}}, &revised)
	refV2 := model.DefinitionRef{DefinitionID: revised.Definition.ID, RevisionID: revised.Revision.ID, ContentHash: revised.Revision.ContentHash, Kind: model.DefinitionTeam}
	call(http.MethodPost, "/v2/teams/rebrief", map[string]any{"request_id": "rebrief_public", "deployment_id": deployed.Deployment.ID, "expected_revision": deployed.Deployment.Revision, "definition": refV2}, &deployed)
	require.Equal(t, refV1, deployed.Deployment.Definition)
	require.Equal(t, refV2, deployed.Deployment.Rebriefs[0].Definition)
	call(http.MethodPost, "/v2/teams/advance-phase", map[string]any{"request_id": "advance_public", "deployment_id": deployed.Deployment.ID, "expected_revision": deployed.Deployment.Revision}, &deployed)
	require.Equal(t, uint32(1), deployed.Deployment.AdvisoryPhase)
	call(http.MethodPost, "/v2/teams/stand-down", map[string]any{"request_id": "stand_down_public", "deployment_id": deployed.Deployment.ID, "expected_revision": deployed.Deployment.Revision, "reason": "complete"}, &deployed)
	require.Equal(t, model.DeploymentStopped, deployed.Deployment.State)
	call(http.MethodPost, "/v2/teams/stand-down", map[string]any{"request_id": "stand_down_reinforcement", "deployment_id": reinforcement.Deployment.ID, "expected_revision": reinforcement.Deployment.Revision, "reason": "complete"}, &reinforcement)
	require.Equal(t, model.DeploymentStopped, reinforcement.Deployment.State)
	require.DirExists(t, checkoutPath)
	shared, err := store.Agent(ctx, "shared_member")
	require.NoError(t, err)
	require.Equal(t, model.AgentActive, shared.Lifecycle)
	group, err := store.Group(ctx, "reinforcement_group")
	require.NoError(t, err)
	require.Contains(t, group.Members, shared.ID)
}

func teamLifecycleRepository(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "tests@example.invalid"}, {"config", "user.name", "Team Lifecycle Test"}} {
		command := exec.Command("git", args...)
		command.Dir = directory
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, "README.md"), []byte("team lifecycle\n"), 0o600))
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = directory
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	return directory
}
