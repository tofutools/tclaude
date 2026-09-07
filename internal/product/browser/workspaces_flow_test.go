package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserWorkspaceInspectionFiltersAndRetainingCleanup(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page = page.Timeout(45 * time.Second)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	checkout := filepath.Join(root, "checkout")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	var owned app.WorkspaceResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/workspaces/create", map[string]any{"request_id": "owned", "id": "owned", "intent": model.WorkspaceIntent{Repository: repo, IntendedPath: checkout, Branch: "worker", BaseRevision: "main", RetainOnFinish: true}}, &owned))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/workspaces/register", map[string]any{"request_id": "external", "id": "external", "intent": model.WorkspaceIntent{IntendedPath: repo, Ownership: model.WorkspaceExternal, Provenance: model.WorkspaceRegistered, RetainOnFinish: true}}, nil))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "retained.txt"), []byte("retain until explicit cleanup"), 0600))
	page.MustElement("[data-tab=workspaces]").MustClick()
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-workspace-id=owned]")
	page.MustElement("[aria-label='Workspace ownership']").MustSelect("Owned checkouts")
	page.MustElementR("#workspace-list button", "^Inspect visible workspaces$").MustClick()
	page.MustElementR("[data-workspace-id=owned] dd", "^Changes observed$")
	page.MustElement("[aria-label='Workspace Git status']").MustSelect("Changes observed")
	require.NotContains(t, page.MustElement("#workspace-list").MustText(), "external ·")
	page.MustEval(`() => {window.workspaceExport='';const original=URL.createObjectURL;URL.createObjectURL=blob=>{blob.text().then(t=>window.workspaceExport=t);return original(blob)};document.addEventListener('click',e=>{if(e.target.download)e.preventDefault()})}`)
	page.MustElementR("#workspace-list button", "^Export workspace inventory$").MustClick()
	page.MustWait(`() => window.workspaceExport.length > 0`)
	require.Equal(t, 1, page.MustEval(`() => JSON.parse(window.workspaceExport).workspaces.length`).Int())
	require.Equal(t, "owned", page.MustEval(`() => JSON.parse(window.workspaceExport).workspaces[0].ID`).Str())
	// Refuse dirty cleanup by default; only a separate explicit discard can remove it.
	page.MustElementR("[data-workspace-id=owned] button", "^Remove checkout$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput("owned")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor-error:not([hidden])")
	data, err := os.ReadFile(filepath.Join(checkout, "retained.txt"))
	require.NoError(t, err)
	require.Equal(t, "retain until explicit cleanup", string(data))
	page.MustElement("#editor button[value=cancel]").MustClick()
	// After the operator commits the retained changes, ordinary cleanup succeeds.
	for _, args := range [][]string{{"-C", checkout, "add", "retained.txt"}, {"-C", checkout, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "retain work"}} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	page.MustElement("[aria-label='Workspace Git status']").MustSelect("All Git observations")
	page.MustElementR("#workspace-list button", "^Inspect visible workspaces$").MustClick()
	page.MustElementR("[data-workspace-id=owned] dd", "^Clean when observed$")
	page.MustElementR("[data-workspace-id=owned] button", "^Remove checkout$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput("owned")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	page.MustElementR("[data-workspace-id=owned] .status", "^removed$")
	page.MustElementR("[data-workspace-id=owned] button", "^Restore checkout$").MustClick()
	page.MustElementR("[data-workspace-id=owned] .status", "^available$")
	data, err = os.ReadFile(filepath.Join(checkout, "retained.txt"))
	require.NoError(t, err)
	require.Equal(t, "retain until explicit cleanup", string(data))
	page.MustElement("[aria-label='Workspace ownership']").MustSelect("External directories")
	require.NotContains(t, page.MustElement("[data-workspace-id=external]").MustText(), "Remove checkout")
}

func TestBrowserWorkspaceShowsExactActiveClaim(t *testing.T) {
	// Unavailable native provider leaves admitted work pending with its claim.
	ctx, page, operator := processEditorBrowser(t)
	repo := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	var space app.WorkspaceResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/workspaces/create", map[string]any{"request_id": "space", "id": "claimed", "intent": model.WorkspaceIntent{Repository: repo, IntendedPath: filepath.Join(t.TempDir(), "checkout"), Branch: "worker", BaseRevision: "main", RetainOnFinish: true}}, &space))
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: space.Workspace.Observation.ActualPath, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	var agent model.Agent
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "worker", "name": "Worker", "desired": desired}, &agent))
	spec := model.WorkRunSpec{SourceMode: model.WorkSourceFreshHandoff, FreshHandoff: "Pinned handoff", WorkspaceID: space.Workspace.ID, WorkspaceRevision: space.Workspace.Revision, WorkerAgentID: agent.ID, WorkerAgentRevision: agent.Revision, WorkerDesired: desired, Brief: "Perform bounded work", Outcome: model.WorkOutcomePolicy{Mode: model.WorkOutcomeHumanDecision}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/work", map[string]any{"request_id": "work", "id": "claimed-work", "spec": spec}, nil))
	page.MustElement("[data-tab=workspaces]").MustClick()
	page.MustElement("#refresh").MustClick()
	page.MustElementR("[data-workspace-id=claimed] p", "Active claim")
	var snapshot struct {
		WorkspaceUses []model.WorkspaceUse `json:"workspace_uses"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.NotEmpty(t, snapshot.WorkspaceUses)
	card := page.MustElement("[data-workspace-id=claimed]")
	require.Contains(t, card.MustText(), string(snapshot.WorkspaceUses[0].ID))
	require.Contains(t, card.MustText(), "claimed-work")
	require.True(t, page.MustElementR("[data-workspace-id=claimed] button", "^Remove checkout$").MustProperty("disabled").Bool())
	require.Contains(t, card.MustText(), "Cleanup is blocked by active claims")
}
