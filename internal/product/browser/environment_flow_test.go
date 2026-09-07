package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserLaunchEnvironmentComposesLiteralPinnedValues(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Environment worker")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor [name=model]").MustInput("fixture")
	add := func(name, value string) {
		t.Helper()
		page.MustElementR("#editor button", "^Add variable$").MustClick()
		rows := page.MustElements("#editor .launch-environment-row")
		row := rows[len(rows)-1]
		row.MustElement("input").MustInput(name)
		row.MustElement("textarea").MustInput(value)
	}
	add("HOME", "/alternate")
	page.MustElement("#editor button[type=submit]").MustClick()
	require.Contains(t, page.MustElement("#editor").MustText(), "reserved for runtime control")
	require.True(t, page.MustElement("#editor").MustVisible())
	page.MustElement("#editor .launch-environment-row button").MustClick()
	add("SHARED", "profile")
	add("PROFILE_ONLY", "literal $HOME\nwith=equals")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "env_group", "name": "Environment team"}, nil))
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElement("#refresh").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Launch defaults$").MustClick()
	page.MustElement("#editor [name=profile]").MustSelect("Environment worker")
	add("SHARED", "group")
	add("GROUP_ONLY", "group literal")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	// The dialog closes before its post-save snapshot refresh finishes.
	page.MustWait(`()=>!submitting`)
	page.MustElementR("#group-management button", "^Create member from default$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Contains(t, page.MustElement("#editor .environment-effective").MustText(), "SHARED = \"profile\"")
	add("SHARED", "explicit")
	require.Contains(t, page.MustElement("#editor .environment-effective").MustText(), "SHARED = \"explicit\"")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Empty(t, snapshot.Executions)
	require.Equal(t, model.Environment{"SHARED": "explicit", "GROUP_ONLY": "group literal", "PROFILE_ONLY": "literal $HOME\nwith=equals"}, snapshot.Agents[0].Desired.Environment)
	page.MustReload()
	page.MustElement("main:not([inert])")
	page.MustElementR("#roster button", "^Save settings as configuration$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	require.Contains(t, page.MustElement("#editor .environment-effective").MustText(), "SHARED = \"explicit\"")
	require.Len(t, page.MustElements("#editor .launch-environment-row"), 3)
	page.MustElement("#editor button[value=cancel]").MustClick()
}
