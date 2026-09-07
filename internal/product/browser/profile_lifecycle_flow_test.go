package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserArchivesRestoresAndProtectsDefaultConfiguration(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "save", "id": "profile", "revision_id": "one", "name": "Saved worker", "desired": model.DesiredConfiguration{Harness: "claude", Model: "fixture", Effort: "high", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}, &profile))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-defaults", map[string]any{"request_id": "default", "global": profile.Revision.Ref}, nil))
	require.Error(t, operator.Call(ctx, "POST", "/v2/configuration-profiles/profile/archive", map[string]any{"request_id": "missing_intent", "expected_revision": 1}, nil))
	page.MustElement("[data-tab=configurations]").MustClick()
	require.True(t, page.MustElementR("#configuration-list button", "^Archive configuration$").MustProperty("disabled").Bool())
	require.Contains(t, page.MustElement("#configuration-list").MustText(), "Clear or replace")
	page.MustElementR("#configuration-list button", "^Clear global$").MustClick()
	page.MustWait(`() => Array.from(document.querySelectorAll('#configuration-list button')).some(b=>b.textContent==='Archive configuration'&&!b.disabled)`)
	page.MustElementR("#configuration-list button", "^Archive configuration$").MustClick()
	page.MustElementR("#configuration-list p", "No active configurations")
	page.MustElement("[aria-label='Configuration status']").MustSelect("Archived")
	page.MustElementR("#configuration-list button", "^Restore configuration$")
	require.False(t, page.MustEval(`() => Array.from(document.querySelectorAll('#configuration-list button')).some(b=>b.textContent==='Create agent'||b.textContent==='Edit configuration')`).Bool())
	page.MustElementR("#configuration-list button", "^Inspect saved revision$").MustClick()
	page.MustElementR("#configuration-list pre", `"Effort": "high"`)
	page.MustElementR("#configuration-list button", "^Restore configuration$").MustClick()
	page.MustElementR("#configuration-list p", "No archived configurations")
	page.MustElement("[aria-label='Configuration status']").MustSelect("Active")
	page.MustElementR("#configuration-list button", "^Create agent$").MustClick()
	page.MustElementR("#editor-title", "Create agent from configuration")
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Restored worker")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	var snapshot struct {
		Agents []model.Agent `json:"agents"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, "high", snapshot.Agents[0].Desired.Effort)
}
