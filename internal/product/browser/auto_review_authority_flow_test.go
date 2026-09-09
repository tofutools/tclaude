package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
)

func TestBrowserAutoReviewAuthorityRequiresCheckedOptIn(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &codex.Provider{})
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "recipient", "name": "recipient", "desired": desired}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`() => snapshot.agents?.some(a=>a.ID==='recipient')`)
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElement("#new-grant").MustClick()
	page.MustElement("#editor [name=action]").MustSelect("agent.configuration.update")
	page.MustElement("#editor [name=configuration]").MustSelect("Only the complete allow-lists below")
	page.MustElement("#editor [name=harnesses]").MustInput("codex")
	page.MustElement("#editor [name=models]").MustInput("fixture")
	page.MustElement("#editor [name=roots]").MustInput("/tmp")
	page.MustElement("#editor [name=approvals]").MustSelect("supervised")
	page.MustElement("#editor [name=sandboxes]").MustSelect("workspace_write")
	checkbox := page.MustElement("#editor [name=auto_review]")
	require.Equal(t, "checkbox", checkbox.MustProperty("type").Str())
	require.False(t, checkbox.MustProperty("checked").Bool())
	require.False(t, checkbox.MustProperty("required").Bool())
	save := func(want bool) {
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustElement("#editor").MustWaitInvisible()
		var state app.AuthorityStateResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &state))
		require.Len(t, state.Grants, 1)
		require.Equal(t, want, state.Grants[0].Bounds.AutoReview)
	}
	save(false)
	// An unrelated edit must not turn the default false into a truthy string.
	page.MustElementR("#access-list button", "^Edit grant$").MustClick()
	page.MustWait(`() => document.querySelector("#editor").open`)
	page.MustElement("#editor [name=models]").MustSelectAllText().MustInput("fixture\nsecond")
	save(false)
	page.MustElementR("#access-list button", "^Edit grant$").MustClick()
	page.MustWait(`() => document.querySelector("#editor").open`)
	page.MustElement("#editor [name=auto_review]").MustClick()
	save(true)
	page.MustElementR("#access-list button", "^Edit grant$").MustClick()
	page.MustWait(`() => document.querySelector("#editor").open`)
	require.True(t, page.MustElement("#editor [name=auto_review]").MustProperty("checked").Bool())
	page.MustElement("#editor [name=auto_review]").MustClick()
	save(false)
}
