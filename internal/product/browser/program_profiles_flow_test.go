package browser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProgramConfigurationAuthorsPinsAndPreservesConflict(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	page.MustElement("main:not([inert])")
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#processes summary", "^Program configurations$").MustClick()
	page.MustElementR("#program-profiles button", "^New program configuration$").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Literal check")
	page.MustElement("#editor [name=executable]").MustInput("/usr/bin/printf")
	page.MustElement("#editor [name=sandbox]").MustSelect("unconfined")
	page.MustElement("#editor [name=arguments]").MustSelectAllText().MustInput(`["%s", "literal $(not-executed)", ""]`)
	page.MustElement("#editor [name=environment]").MustSelectAllText().MustInput(`{"CHECK_VALUE":"literal $HOME"}`)
	// Lose the response only after the production API commits, then retry the unchanged form.
	page.MustEval(`()=>{const fetch=window.fetch.bind(window);window.fetch=async(...args)=>{const result=await fetch(...args);if(String(args[0]).endsWith('/v2/program-profiles')&&args[1]?.method==='POST'){window.fetch=fetch;throw new Error('simulated profile response loss')}return result}}`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "simulated profile response loss")
	page.MustWait(`()=>!submitting`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	var profiles []model.ProgramProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/program-profiles", nil, &profiles))
	require.Len(t, profiles, 1)
	require.Equal(t, model.Revision(1), profiles[0].Revision)
	var saved app.ProgramProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/program-profiles/"+string(profiles[0].ID), nil, &saved))
	require.Equal(t, []string{"%s", "literal $(not-executed)", ""}, saved.Revision.ArgumentPrefix)
	require.Equal(t, map[string]string{"CHECK_VALUE": "literal $HOME"}, saved.Revision.Environment)
	require.Equal(t, time.Minute, saved.Revision.Timeout)
	// Use the browser-authored revision in the actual process editor.
	page.MustElementR("#definition-list button", "^New process$").MustClick()
	page.MustElementR("#process-editor button", "^Add task$").MustClick()
	page.MustElement("#process-editor [aria-label='Performer kind']").MustSelect("program")
	page.MustElement("#process-inspector [name=profile]").MustSelect("Literal check · revision 1")
	page.MustElementR("#process-inspector button", "^Apply changes$").MustClick()
	page.MustElementR("#process-inspector button", "^Make entry$").MustClick()
	page.MustElementR("#process-inspector button", "^Connect$").MustClick()
	page.MustElementR("#process-editor button", "^Save revision$").MustClick()
	page.MustElementR("#process-editor-message", "Revision 1 · saved")
	page.MustElementR("#process-editor button", "^Close editor$").MustClick()
	// An overlapping writer must not be overwritten by a stale edit.
	page.MustElementR("#program-profiles button", "^Edit program configuration$").MustClick()
	require.Eventually(t, func() bool { return page.MustEval(`()=>document.getElementById('editor').open`).Bool() }, 5*time.Second, 20*time.Millisecond, "editor did not open: %s", page.MustElement("#error").MustText())
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("My unsaved name")
	require.NoError(t, operator.Call(ctx, "POST", "/v2/program-profiles", map[string]any{"request_id": "other", "id": saved.Profile.ID, "expected_revision": saved.Profile.Revision, "name": "Other edit", "executable": "/usr/bin/printf", "argument_prefix": []string{"changed"}, "sandbox": "unconfined", "timeout": int64(time.Minute), "output_limit_bytes": 1024, "effect_authority": saved.Revision.EffectAuthority}, nil))
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "The saved state changed")
	require.Equal(t, "My unsaved name", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	var definitions []model.Definition
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions", nil, &definitions))
	require.Len(t, definitions, 1)
	var definition app.DefinitionResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/definitions/"+string(definitions[0].ID), nil, &definition))
	found := false
	for _, node := range definition.Revision.Process.Graph.Nodes {
		if node.Performer != nil && node.Performer.Program != nil {
			require.Equal(t, saved.Revision.ID, node.Performer.Program.Profile.RevisionID)
			found = true
		}
	}
	require.True(t, found)
	var snapshot struct {
		Executions []any `json:"executions"`
		WorkRuns   []any `json:"work_runs"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions)
	require.Empty(t, snapshot.WorkRuns)
}
