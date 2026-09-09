package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"testing"
)

func TestBrowserClaudeAutoCompactWindowSaveAndReopen(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{})
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Claude compaction window")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor [name=sandbox]").MustSelect("workspace_write")
	for _, mode := range []string{"450k", "0.5M"} {
		page.MustElement("#editor [name=auto_compact_window]").MustSelectAllText().MustInput(mode)
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustWait(`()=>!document.querySelector('#editor').open&&!submitting`)
		var entries []model.ConfigurationProfile
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &entries))
		require.Len(t, entries, 1)
		var saved app.ConfigurationProfileResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(entries[0].ID), nil, &saved))
		require.NotNil(t, saved.Revision.Options)
		require.NotNil(t, saved.Revision.Options.AutoCompactWindow)
		require.Equal(t, model.AutoCompactWindow(map[string]string{"450k": "450000", "0.5M": "500000"}[mode]), *saved.Revision.Options.AutoCompactWindow)
		page.MustElementR("#configuration-list button", "^Edit configuration$").MustClick()
		page.MustWait(`()=>document.querySelector('#editor').open`)
		require.Equal(t, map[string]string{"450k": "450000", "0.5M": "500000"}[mode], page.MustElement("#editor [name=auto_compact_window]").MustProperty("value").Str())
	}
	page.MustElement("#editor button[value=cancel]").MustClick()
	var snapshot app.Snapshot
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Executions)
}
