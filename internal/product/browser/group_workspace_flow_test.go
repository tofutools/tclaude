package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBrowserGroupSpawnCreatesCheckoutAndRetriesLostReply(t *testing.T) {
	provider := &groupFallbackProvider{automationTeamProvider{name: "claude", delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}}
	ctx, page, operator := processEditorBrowser(t, provider)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	checkout := filepath.Join(root, "worker")
	for _, args := range [][]string{{"init", "-b", "main", repo}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base"}} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='team')`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Spawn member$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Checkout worker")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElement("#editor [name=checkout]").MustSelect("Create a new checkout")
	page.MustElement("#editor [name=repository]").MustInput(repo)
	page.MustElement("#editor [name=path]").MustInput(checkout)
	page.MustElement("#editor [name=branch]").MustInput("worker")
	page.MustElement("#editor [name=brief]").MustInput("Inspect the checkout")
	page.MustEval(`()=>{const original=window.fetch;window.workspaceCreateCalls=0;let lost=false;window.fetch=async(...args)=>{if(String(args[0])==='/v2/workspaces/create')window.workspaceCreateCalls++;const response=await original(...args);if(!lost&&String(args[0])==='/v2/groups/team/agents'&&args[1]?.method==='POST'&&response.ok){lost=true;throw new Error('fixture lost group reply')}return response}}`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "fixture lost group reply")
	page.MustWait(`()=>!submitting`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.Equal(t, 1, page.MustEval(`()=>window.workspaceCreateCalls`).Int())
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Workspaces []struct {
			ID          model.WorkspaceID
			Revision    model.Revision
			Observation model.WorkspaceObservation
		} `json:"workspaces"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Workspaces, 1)
	require.Equal(t, checkout, snapshot.Agents[0].Desired.WorkingDirectory)
	require.Equal(t, checkout, snapshot.Workspaces[0].Observation.ActualPath)
	err := operator.Call(ctx, "POST", "/v2/workspaces/remove", map[string]any{"request_id": "remove", "workspace_id": snapshot.Workspaces[0].ID, "expected_revision": snapshot.Workspaces[0].Revision}, nil)
	require.Error(t, err)
	select {
	case body := <-provider.briefs:
		require.Equal(t, "Inspect the checkout", body)
	case <-time.After(time.Second):
		t.Fatal("no prepared first input")
	}
	// The same dialog can select the now-known checkout without another Git effect.
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "second", "name": "Second"}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='second')`)
	summary := page.MustElementR("summary", "^Group settings$")
	if !summary.MustEval(`()=>this.parentElement.open`).Bool() {
		summary.MustClick()
	}
	page.MustElementR("#group-management [data-group-id=second] button", "^Create member$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Offline worker")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElement("#editor [name=checkout]").MustSelect(checkout)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 2)
	require.Len(t, snapshot.Workspaces, 1)
	for _, agent := range snapshot.Agents {
		require.Equal(t, checkout, agent.Desired.WorkingDirectory)
	}
}
