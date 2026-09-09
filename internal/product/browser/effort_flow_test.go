package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"testing"
	"time"
)

func TestBrowserSavedRequestedEffortRemainsPinned(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{})
	page = page.Timeout(40 * time.Second)
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Effort profile")
	page.MustElement("#editor [name=model]").MustInput("example-model")
	page.MustElement("#editor [name=cwd]").MustInput("/tmp")
	page.MustElement("#editor [name=effort]").MustInput("--invalid")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "must start")
	page.MustElement("#editor [name=effort]").MustSelectAllText().MustInput("low")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	page.MustElementR("#configuration-list button", "^Create agent$").MustClick()
	page.MustElementR("#editor-title", "^Create agent from configuration$")
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Pinned worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	var snapshot struct {
		Agents []model.Agent `json:"agents"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, "low", snapshot.Agents[0].Desired.Effort)
	page.MustElementR("#configuration-list button", "^Edit configuration$").MustClick()
	page.MustElementR("#editor-title", "^Save new configuration revision$")
	require.Equal(t, "low", page.MustElement("#editor [name=effort]").MustProperty("value").Str())
	page.MustElement("#editor [name=effort]").MustSelectAllText().MustInput("high")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Equal(t, "low", snapshot.Agents[0].Desired.Effort)
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Len(t, profiles, 1)
	var selected app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &selected))
	require.NotNil(t, selected.Revision.Options)
	require.NotNil(t, selected.Revision.Options.Effort)
	require.Equal(t, "high", *selected.Revision.Options.Effort)
	page.MustReload()
	page.MustWait(`() => !document.querySelector("main").inert`)
	page.MustElementR("#configuration-list button", "^Edit configuration$").MustClick()
	page.MustElementR("#editor-title", "^Save new configuration revision$")
	require.Equal(t, "high", page.MustElement("#editor [name=effort]").MustProperty("value").Str())
	page.MustElement("#editor button[value=cancel]").MustClick()
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("[aria-label='Copy saved configuration']").MustSelect("Effort profile")
	require.Equal(t, "high", page.MustElement("#team-editor [name=effort]").MustProperty("value").Str())

}
