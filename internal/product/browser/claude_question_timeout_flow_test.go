package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"testing"
)

func TestBrowserClaudeAskUserQuestionTimeoutSaveAndReopen(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{})
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Claude question timeout")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor [name=sandbox]").MustSelect("workspace_write")
	for _, mode := range []string{"5m", "inherit", "never", "60s", "10m"} {
		page.MustElement("#editor [name=ask_user_question_timeout]").MustSelect(map[string]string{"5m": "5 minutes", "inherit": "Inherit Claude settings", "never": "Never auto-continue", "60s": "60 seconds", "10m": "10 minutes"}[mode])
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustWait(`()=>!document.querySelector('#editor').open&&!submitting`)
		var entries []model.ConfigurationProfile
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &entries))
		require.Len(t, entries, 1)
		var saved app.ConfigurationProfileResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(entries[0].ID), nil, &saved))
		require.NotNil(t, saved.Revision.Options)
		require.NotNil(t, saved.Revision.Options.AskUserQuestionTimeout)
		require.Equal(t, model.AskUserQuestionTimeout(mode), *saved.Revision.Options.AskUserQuestionTimeout)
		page.MustElementR("#configuration-list button", "^Edit configuration$").MustClick()
		page.MustWait(`()=>document.querySelector('#editor').open`)
		require.Equal(t, mode, page.MustElement("#editor [name=ask_user_question_timeout]").MustProperty("value").Str())
	}
	page.MustElement("#editor button[value=cancel]").MustClick()
	var snapshot app.Snapshot
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Executions)
}
