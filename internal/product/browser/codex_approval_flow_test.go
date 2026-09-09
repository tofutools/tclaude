package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"testing"
)

func TestBrowserCodexNativeApprovalModesSaveAndReopen(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &codex.Provider{})
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Codex policies")
	page.MustElement("#editor [name=harness]").MustSelect("codex")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor [name=sandbox]").MustSelect("read_only")
	for _, mode := range []model.ApprovalMode{model.ApprovalUntrusted, model.ApprovalOnFailure, model.ApprovalOnRequest, model.ApprovalNever} {
		page.MustElement("#editor [name=approval]").MustSelect(string(mode))
		if mode == model.ApprovalOnFailure || mode == model.ApprovalUntrusted {
			page.MustElementR("#editor [aria-label='Configured launch support']", "Unsupported selected approval "+string(mode))
		} else {
			page.MustElementR("#editor [aria-label='Configured launch support']", "codex adapter.*Selected policy is supported")
		}
		if mode == model.ApprovalOnFailure {
			page.MustElementR("#editor [aria-label='Configured launch support']", "Deprecated native mode")
		}
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustWait(`()=>!document.querySelector('#editor').open&&!submitting`)
		var entries []model.ConfigurationProfile
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &entries))
		require.Len(t, entries, 1)
		var saved app.ConfigurationProfileResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(entries[0].ID), nil, &saved))
		require.NotNil(t, saved.Revision.Options)
		require.NotNil(t, saved.Revision.Options.Approval)
		require.Equal(t, mode, *saved.Revision.Options.Approval)
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
