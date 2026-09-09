package browser

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"testing"
)

func TestBrowserPartialProfilePreservesInheritedFieldsAndExplicitOff(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{})
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Portable worker")
	require.Equal(t, "", page.MustElement("#editor [name=harness]").MustProperty("value").String())
	page.MustElement("#editor [name=auto_review]").MustSelect("No automatic approval review")
	page.MustElement("#editor [name=model]").MustInput("reusable-model")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => (!document.querySelector('#editor').open && !submitting) || !document.querySelector('#editor-error').hidden`)
	require.Equal(t, "", page.MustElement("#editor-error").MustText())
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Len(t, profiles, 1)
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &saved))
	require.NotNil(t, saved.Revision.Options)
	require.Nil(t, saved.Revision.Options.Harness)
	require.Nil(t, saved.Revision.Options.WorkingDirectory)
	require.Nil(t, saved.Revision.Options.Approval)
	require.NotNil(t, saved.Revision.Options.AutoReview)
	require.False(t, *saved.Revision.Options.AutoReview)
	page.MustReload()
	page.MustElementR("#connection", "^Updated")
	page.MustElement("[data-tab=configurations]").MustClick()
	// Select from the currently rendered list in one browser turn; restoring
	// the selected tab after reload can replace its asynchronous profile list.
	page.MustWait(`() => {const edit=[...document.querySelectorAll('#configuration-list button')].find(b=>b.dataset.uiText==='Edit configuration');if(!edit)return false;edit.click();return true}`)
	page.MustElement("#editor").MustWaitVisible()
	require.Equal(t, "off", page.MustElement("#editor [name=auto_review]").MustProperty("value").String())
	require.Equal(t, "", page.MustElement("#editor [name=harness]").MustProperty("value").String())
	page.MustElement("#editor [name=model]").MustSelectAllText().MustInput("changed-model")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => (!document.querySelector('#editor').open && !submitting) || !document.querySelector('#editor-error').hidden`)
	require.Equal(t, "", page.MustElement("#editor-error").MustText())
	var reopened app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &reopened))
	require.Equal(t, "changed-model", *reopened.Revision.Options.Model)
	require.Nil(t, reopened.Revision.Options.WorkingDirectory)
	require.NotNil(t, reopened.Revision.Options.AutoReview)
	require.False(t, *reopened.Revision.Options.AutoReview)
	page.MustElementR("#configuration-list button", "^Create agent$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Launch worker")
	cwd := t.TempDir()
	page.MustElement("#editor [name=cwd]").MustInput(cwd)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => (!document.querySelector('#editor').open && !submitting) || !document.querySelector('#editor-error').hidden`)
	require.Equal(t, "", page.MustElement("#editor-error").MustText())
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []any         `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, cwd, snapshot.Agents[0].Desired.WorkingDirectory)
	require.Equal(t, "changed-model", snapshot.Agents[0].Desired.Model)
	require.Empty(t, snapshot.Executions, "creation must not start native work")
	var afterLaunch app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profiles[0].ID), nil, &afterLaunch))
	require.Equal(t, reopened, afterLaunch)

}

func TestBrowserPartialProfileGlobalAndGroupLaunchContext(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{})
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "portable", "id": "portable", "revision_id": "one", "name": "Portable defaults", "options": model.ConfigurationOptions{}}, &profile))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "group", "name": "Team"}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configuration-list button", "^Use as default$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=scope]").MustSelect("Global")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open && !submitting`)
	page.MustElementR("#configuration-list button", "^Create from global$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=name]").MustInput("Global member")
	cwd := t.TempDir()
	page.MustElement("#editor [name=cwd]").MustInput(cwd)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => (!document.querySelector('#editor').open && !submitting) || !document.querySelector('#editor-error').hidden`)
	require.Equal(t, "", page.MustElement("#editor-error").MustText())
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/group/configuration", map[string]any{"profile": profile.Revision.Ref}, nil))
	page.MustElement("[data-tab=groups]").MustClick()
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Create member from default$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Group member")
	page.MustElement("#editor [name=cwd]").MustInput(cwd)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => (!document.querySelector('#editor').open && !submitting) || !document.querySelector('#editor-error').hidden`)
	require.Equal(t, "", page.MustElement("#editor-error").MustText())
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Groups     []model.Group `json:"groups"`
		Executions []any         `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 2)
	require.Len(t, snapshot.Groups[0].Members, 1)
	require.Empty(t, snapshot.Executions)
	for _, agent := range snapshot.Agents {
		require.Equal(t, cwd, agent.Desired.WorkingDirectory)
	}
	var retained app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/portable", nil, &retained))
	require.Equal(t, profile, retained)
}

func TestBrowserPartialProfileTransferPreservesAuthoredOptions(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	off, mode := false, model.FastModeOn
	options := &model.ConfigurationOptions{AutoReview: &off, FastMode: &mode}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "partial", "id": "partial", "revision_id": "one", "name": "Portable", "options": options}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElementR("#configuration-list button", "^Export configurations$").MustClick()
	page.MustElement("dialog[aria-label='Export configurations'] input[type=checkbox]")
	page.MustEval(`() => {const original=URL.createObjectURL;URL.createObjectURL=blob=>{window.partialExport=blob.text();return original(blob)}}`)
	page.MustElementR("dialog[aria-label='Export configurations'] button", "^Export$").MustClick()
	page.MustWait(`() => !document.querySelector('dialog[aria-label="Export configurations"]')`)
	raw := page.MustEval(`async () => await window.partialExport`).Str()
	var bundle app.ConfigurationBundle
	require.NoError(t, json.Unmarshal([]byte(raw), &bundle))
	require.Len(t, bundle.Profiles, 1)
	require.Equal(t, options, bundle.Profiles[0].Options)
	page.MustElementR("#configuration-list button", "^Import configurations$").MustClick()
	page.MustElement("[aria-label='Configuration bundle']").MustInput(raw)
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Preview$").MustClick()
	page.MustElement("[aria-label='Import name for Portable']")
	page.MustElementR("dialog[aria-label='Import configurations'] button", "^Import selected$").MustClick()
	page.MustWait(`() => !document.querySelector('dialog[aria-label="Import configurations"]')`)
	var profiles []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &profiles))
	require.Len(t, profiles, 2)
	for _, profile := range profiles {
		var saved app.ConfigurationProfileResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(profile.ID), nil, &saved))
		require.Equal(t, options, saved.Revision.Options)
		require.True(t, saved.Revision.Desired.Equal(model.DesiredConfiguration{}))
	}
}
