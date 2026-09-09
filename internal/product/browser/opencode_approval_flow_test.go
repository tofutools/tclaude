package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
	"testing"
)

func TestBrowserOpenCodeDenyApprovalSavesAndReopens(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &opencode.Provider{}, &copilot.Provider{})
	page.MustElement("[data-tab=configurations]").MustClick()
	page.MustElement("#new-configuration").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Unattended reader")
	page.MustElement("#editor [name=harness]").MustSelect("opencode")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput(t.TempDir())
	page.MustElement("#editor [name=sandbox]").MustSelect("unconfined")
	page.MustElement("#editor [name=approval]").MustSelect("deny")
	page.MustElementR("#editor [aria-label='Configured launch support']", "opencode adapter.*Selected policy is supported")
	page.MustElementR("#editor [aria-label='Configured launch support']", "including bash, remain allowed")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`()=>!document.querySelector('#editor').open`)
	var entries []model.ConfigurationProfile
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles", nil, &entries))
	require.Len(t, entries, 1)
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/configuration-profiles/"+string(entries[0].ID), nil, &saved))
	require.NotNil(t, saved.Revision.Options)
	require.NotNil(t, saved.Revision.Options.Approval)
	require.Equal(t, model.ApprovalDeny, *saved.Revision.Options.Approval)
	page.MustElementR("#configuration-list button", "^Edit configuration$").MustClick()
	require.Equal(t, "deny", page.MustElement("#editor [name=approval]").MustProperty("value").Str())
	page.MustElement("#editor button[value=cancel]").MustClick()
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=key]").MustInput("worker")
	page.MustElement("#team-editor [name=name]").MustInput("Worker")
	page.MustElement("#team-editor [name=profile]").MustSelect("Unattended reader")
	page.MustElement("#team-editor [name=override_harness]").MustClick()
	page.MustElement("#team-editor [name=harness]").MustSelect("copilot")
	page.MustElementR("#team-editor [aria-label='Configured launch support']", "copilot adapter.*Selected policy is supported")
	require.Equal(t, "automatic", page.MustElement("#team-editor [name=approval]").MustProperty("value").Str())
	page.MustElement("#team-editor [name=override_approval]").MustClick()
	page.MustElement("#team-editor [name=approval]").MustSelect("deny")
	page.MustElementR("#team-editor [aria-label='Configured launch support']", "Unsupported selected approval deny")
	require.Equal(t, "deny", page.MustElement("#team-editor [name=approval]").MustProperty("value").Str())
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Executions)
}
