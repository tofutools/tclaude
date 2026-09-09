package browser

import (
	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"testing"
)

func TestBrowserClaudeNativeApprovalModesSaveAndReopen(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{})
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Claude policies")
	page.MustElement("#editor [name=harness]").MustSelect("claude")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor [name=sandbox]").MustSelect("workspace_write")
	for _, mode := range []model.ApprovalMode{model.ApprovalDefault, model.ApprovalManual, model.ApprovalPlan, model.ApprovalAcceptEdits, model.ApprovalDontAsk, model.ApprovalBypassPermissions, model.ApprovalInherit, model.ApprovalAuto} {
		require.NoError(t, page.MustElement("#editor [name=approval]").Select([]string{"option[value='" + string(mode) + "']"}, true, rod.SelectorTypeCSSSector))
		page.MustElementR("#editor [aria-label='Configured launch support']", "claude adapter.*Selected policy is supported")
		if mode == model.ApprovalInherit {
			page.MustElementR("#editor [aria-label='Configured launch support']", "without a permission-mode override")
		}
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustWait(`()=>!document.querySelector('#editor').open&&!submitting`)
		var entries []model.ConfigurationProfile
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &entries))
		require.Len(t, entries, 1)
		var saved app.ConfigurationProfileResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(entries[0].ID), nil, &saved))
		require.Equal(t, mode, saved.Revision.Desired.Approval)
		page.MustElementR("#configuration-list button", "^Edit configuration$").MustClick()
		page.MustWait(`()=>document.querySelector('#editor').open`)
		require.Equal(t, string(mode), page.MustElement("#editor [name=approval]").MustProperty("value").Str())
	}
	page.MustElement("#editor button[value=cancel]").MustClick()
	var snapshot app.Snapshot
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Executions)
}
