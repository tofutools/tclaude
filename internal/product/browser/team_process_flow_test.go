package browser

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestBrowserTeamProcessShowsGuidanceAndRecordsNamedMoves(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	provider := &automationTeamProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")}, briefs: make(chan string, 4)}
	ctx, page, operator := processEditorBrowser(t, provider)
	var space app.WorkspaceResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/workspaces/create", map[string]any{"request_id": "space", "id": "space", "intent": model.WorkspaceIntent{Repository: repo, IntendedPath: filepath.Join(root, "checkout"), Branch: "worker", BaseRevision: "main", RetainOnFinish: true}}, &space))
	desired := model.DesiredConfiguration{Harness: "team-fixture", Model: "fixture", WorkingDirectory: space.Workspace.Observation.ActualPath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Labels: model.AgentLabels{Role: "reviewer"}, Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}, RequiredReady: true}}, AdvisoryProcess: []model.TeamPhase{{Name: "Investigate", Roles: []string{"all"}, Criteria: "Record <evidence> literally."}, {Name: "Review", Roles: []string{"reviewer"}, Criteria: "Discuss the findings."}}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "team", "draft": app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Advisory team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`() => snapshot.workspaces?.length === 1`)
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Deploy team$").MustClick()
	page.MustElement("#editor [name=mission]").MustInput("Investigate a change")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !submitting`)
	require.False(t, page.MustElement("#editor").MustProperty("open").Bool(), page.MustElement("#editor-error").MustText())
	select {
	case brief := <-provider.briefs:
		require.Contains(t, brief, "Record <evidence> literally.")
	case <-time.After(10 * time.Second):
		t.Fatal("team did not prepare initial phase guidance")
	}
	page.MustElementR("#definition-list [data-deployment] p", "phase 1/2: Investigate")
	page.MustElementR("#definition-list [data-deployment] summary", "Advisory process").MustClick()
	require.Equal(t, "Record <evidence> literally.", page.MustElement("#definition-list [data-deployment] li pre").MustText())
	// Settle deployment startup before exercising independent phase changes.
	require.NoError(t, page.Wait(rod.Eval(`async () => (await api("/v2/teams/deployments")).some(d => d.Deployment.State === "ready")`).ByPromise()))
	page.MustElementR("#definition-list [data-deployment] button", "^Advance advisory phase$").MustClick()
	page.MustWait(`() => document.getElementById("editor").open`)
	page.MustElement("#editor [name=phase]").MustSelect("Review")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !submitting`)
	require.False(t, page.MustElement("#editor").MustProperty("open").Bool(), page.MustElement("#editor-error").MustText())
	page.MustElementR("#definition-list [data-deployment] p", "phase 2/2: Review")
	page.MustElementR("#definition-list [data-deployment] button", "^Advance advisory phase$").MustClick()
	page.MustWait(`() => document.getElementById("editor").open`)
	page.MustElement("#editor [name=phase]").MustSelect("Investigate")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !submitting`)
	require.False(t, page.MustElement("#editor").MustProperty("open").Bool(), page.MustElement("#editor-error").MustText())
	page.MustReload().MustWaitLoad()
	page.MustElementR("#connection", "^Updated")
	page.MustElementR("#definition-list [data-deployment] p", "phase 1/2: Investigate")
	page.MustElementR("#definition-list [data-deployment] summary", "Advisory process").MustClick()
	page.MustElementR("#definition-list [data-deployment] p", "Investigate → Review")
	page.MustElementR("#definition-list [data-deployment] p", "Review → Investigate")
	var deployments []app.TeamDeploymentResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/teams/deployments", nil, &deployments))
	require.Len(t, deployments, 1)
	require.Len(t, deployments[0].Deployment.PhaseHistory, 2)
	require.Equal(t, team.AdvisoryProcess, deployments[0].Phases)
}

// Phase notices reach the normal native notification seam. Only that native
// boundary is doubled; message admission and delivery receipts remain real.
func (*automationTeamRuntime) Interact(context.Context, ports.Interaction) (ports.InteractionResult, error) {
	return ports.InteractionResult{Disposition: ports.EffectAccepted}, nil
}

func TestBrowserLegacyTeamPhaseNamesRemainEditable(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}, AdvisoryPhases: []string{"Investigate", "Review"}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "legacy", "draft": app.DefinitionDraft{ID: "legacy", RevisionID: "legacy_v1", Name: "Legacy phases", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "name-only compatibility", Team: &team}}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^Edit team template$").MustClick()
	page.MustElementR("#team-editor nav button", "^Workspace and phases$").MustClick()
	page.MustElementR("#team-editor button", "^Edit Review$").MustClick()
	page.MustElement("#team-editor [name=criteria]").MustInput("Review the result.")
	page.MustElementR("#team-editor button", "^Apply changes$").MustClick()
	page.MustElementR("#team-editor button", "^Save team revision$").MustClick()
	page.MustElementR("#team-editor-status", "Revision 2 · saved")
	var result app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/legacy", nil, &result))
	require.Equal(t, "Review the result.", result.Revision.Team.AdvisoryProcess[1].Criteria)
	require.Empty(t, result.Revision.Team.AdvisoryPhases)
}
