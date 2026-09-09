package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"path/filepath"
	"testing"
)

func TestBrowserGroupDefaultDirectorySaveUseClearAndReopen(t *testing.T) {
	provider := &groupFallbackProvider{automationTeamProvider{name: "claude", delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}}
	ctx, page, operator := processEditorBrowser(t, provider)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='team')`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Launch defaults$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	dir := filepath.Join(t.TempDir(), "future")
	page.MustElement("#editor [name=cwd]").MustInput(dir)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	summary := page.MustElementR("summary", "^Group settings$")
	if !summary.MustEval(`()=>this.parentElement.open`).Bool() {
		summary.MustClick()
	}
	page.MustElementR("#group-management button", "^Launch defaults$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, dir, page.MustElement("#editor [name=cwd]").MustProperty("value").Str())
	page.MustElementR("#editor button", "^Cancel$").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#group-management button", "^Create member$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Directory worker")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	var snapshot struct {
		Agents []model.Agent `json:"agents"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, dir, snapshot.Agents[0].Desired.WorkingDirectory)
	page.MustElementR("#group-management button", "^Launch defaults$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=cwd]").MustSelectAllText().MustInput("")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	var defaults model.GroupConfiguration
	require.NoError(t, operator.Call(ctx, "GET", "/v2/groups/team/configuration", nil, &defaults))
	require.Empty(t, defaults.DefaultDirectory)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Equal(t, dir, snapshot.Agents[0].Desired.WorkingDirectory)
}
