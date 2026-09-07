package browser

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// Only native workload preparation/release/observation is doubled. Browser,
// credentials, current delegation, workspace host, application and SQLite are real.
type automationTeamProvider struct {
	ports.Provider
	delivery host.ActionCredentialHost
	briefs   chan string
}

func (*automationTeamProvider) Name() string { return "team-fixture" }
func (*automationTeamProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{PreparedInitialInput: true}
}
func (p *automationTeamProvider) ActionCredentials() ports.ActionCredentialDelivery {
	return p.delivery
}
func (p *automationTeamProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	receipt, err := p.delivery.PrepareActionCredential(ctx, *req.ActionCredential)
	if err != nil {
		return nil, err
	}
	p.briefs <- req.InitialInput.Body
	return &automationTeamPrepared{accessBrowserPrepared: accessBrowserPrepared{spec: req.Spec, receipt: receipt}, input: req.InitialInput}, nil
}

type automationTeamPrepared struct {
	accessBrowserPrepared
	input *ports.PreparedInitialInput
}

func (p *automationTeamPrepared) Describe() ports.PreparedDescription {
	d := p.accessBrowserPrepared.Describe()
	d.Evidence.Provider = "team-fixture"
	d.Requirements.WorkingDirectory = p.spec.WorkingDirectory
	d.InitialInput = &ports.PreparedInitialInputDescription{Supported: true, Correlation: p.input.Correlation}
	return d
}
func (p *automationTeamPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &automationTeamRuntime{accessBrowserRuntime: accessBrowserRuntime{id: p.spec.ExecutionID}}, Evidence: p.Describe().Evidence}, nil
}

type automationTeamRuntime struct{ accessBrowserRuntime }

func (*automationTeamRuntime) Observe(context.Context) (ports.Observation, error) {
	return ports.Observation{ObservedAt: time.Now(), Workload: ports.WorkloadRunning, Context: ports.ContextReady}, nil
}

func TestBrowserAutomationTeamCanAuthorizeAndDeployFutureGroup(t *testing.T) {
	for _, target := range []string{"new_group", "existing_group"} {
		t.Run(target, func(t *testing.T) { browserAutomationTeamDeployment(t, target) })
	}
}
func browserAutomationTeamDeployment(t *testing.T, target string) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		output, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	provider := &automationTeamProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(root, "credentials")}, briefs: make(chan string, 4)}
	ctx, page, operator := processEditorBrowser(t, provider)
	if target == "existing_group" {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "future_group", "name": "Future group", "members": []string{}}, nil))
	}
	var space app.WorkspaceResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/workspaces/create", map[string]any{"request_id": "space", "id": "space", "intent": model.WorkspaceIntent{Repository: repo, IntendedPath: filepath.Join(root, "checkout"), Branch: "worker", BaseRevision: "main", RetainOnFinish: true}}, &space))
	desired := model.DesiredConfiguration{Harness: "team-fixture", Model: "fixture", WorkingDirectory: space.Workspace.Observation.ActualPath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "wave", MemberKeys: []string{"worker"}, RequiredReady: true}}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/definitions", map[string]any{"request_id": "team", "draft": app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Scheduled team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "scheduled team", Team: &team}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=automation]").MustClick()
	page.MustElementR("#automation-list button", "^Schedule selected template$").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Team schedule")
	page.MustElement("#editor [name=anchor]").MustInput(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	page.MustElement("#editor [name=mission]").MustInput("Review the change")
	page.MustElement("#editor [name=team_target]").MustSelect(target)
	if target == "new_group" {
		page.MustElement("#editor [name=new_group]").MustInput("future_group")
	} else {
		page.MustElement("#editor [name=team_group]").MustSelect("Future group")
	}
	page.MustElement("#editor [name=team_member_scope]").MustSelect("Include members of the explicit target group")
	page.MustElement("#editor [name=allowed_actions]").MustSelect("workspace.inspect", "execution.launch", "work.start", "automation.run")
	page.MustElement("#editor [name=allowed_resources]").MustSelect("This automation rule", "Workspace: space")
	if target == "existing_group" {
		page.MustElement("#editor [name=allowed_actions]").MustSelect("group.membership.manage")
		page.MustElement("#editor [name=allowed_resources]").MustSelect("Group: Future group")
	}
	page.MustElement("#editor [name=harnesses]").MustInput("team-fixture")
	page.MustElement("#editor [name=models]").MustInput("fixture")
	page.MustElement("#editor [name=roots]").MustInput(space.Workspace.Observation.ActualPath)
	page.MustElement("#editor [name=approvals]").MustSelect("supervised")
	page.MustElement("#editor [name=sandboxes]").MustSelect("workspace_write")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#automation-list button", "^Enable$").MustClick()
	page.MustElementR("#automation-list button", "^Run now$").MustClick()
	var rules []model.AutomationRule
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules", nil, &rules))
	require.Len(t, rules, 1)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var records []app.OccurrenceResult
		err := operator.Call(ctx, "GET", "/v2/automation/occurrences?rule_id="+string(rules[0].ID), nil, &records)
		require.NoError(c, err)
		require.Len(c, records, 1)
		require.Equal(c, model.OccurrenceDelivered, records[0].Occurrence.State, "%+v", records)
		require.NotEmpty(c, records[0].Occurrence.DeploymentID)
	}, 15*time.Second, 100*time.Millisecond)
	select {
	case body := <-provider.briefs:
		require.Contains(t, body, "Review the change")
	default:
		t.Fatal("team workload was not prepared")
	}
	require.False(t, page.MustElement("#error").MustVisible())
}
