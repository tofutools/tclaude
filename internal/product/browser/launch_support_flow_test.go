package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
)

func TestBrowserLaunchSupportExplainsPoliciesWithoutChangingOfflineSettings(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t, &claude.Provider{}, &copilot.Provider{})
	var support app.LaunchSupportResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/launch-support?harness=claude", nil, &support))
	require.True(t, support.Configured)
	require.Equal(t, []model.SandboxMode{model.SandboxWorkspaceWrite}, support.SandboxModes)
	page.MustElement("#new-agent").MustClick()
	page.MustElementR("#editor [aria-label='Configured launch support']", "Selected policy is supported")
	page.MustElement("#editor [name=harness]").MustSelect("copilot")
	page.MustElementR("#editor [aria-label='Configured launch support']", "Unsupported selected confinement workspace_write")
	require.Equal(t, "workspace_write", page.MustElement("#editor [name=sandbox]").MustProperty("value").Str())
	page.MustElement("#editor [name=sandbox]").MustSelect("unconfined")
	page.MustElementR("#editor [aria-label='Configured launch support']", "Selected policy is supported")
	page.MustElement("#editor [name=harness]").MustSelect("codex")
	page.MustElementR("#editor [aria-label='Configured launch support']", "no provider is configured")
	// Offline intent remains authorable; inspecting declarations does not prepare a process.
	page.MustElement("#editor [name=name]").MustInput("Offline intent")
	page.MustElement("#editor [name=model]").MustInput("fixture")
	page.MustElement("#editor [name=cwd]").MustInput("/tmp")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []any         `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, "codex", snapshot.Agents[0].Desired.Harness)
	require.Equal(t, model.SandboxUnconfined, snapshot.Agents[0].Desired.Sandbox)
	require.Empty(t, snapshot.Executions)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "support-copy", "id": "support-copy", "revision_id": "one", "name": "Copied policy", "desired": model.DesiredConfiguration{Harness: "copilot", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}, nil))
	page.MustElement("[data-tab=processes]").MustClick()
	page.MustElementR("#definition-list button", "^New team template$").MustClick()
	page.MustElementR("#team-editor button", "^Add member$").MustClick()
	page.MustElement("#team-editor [name=harness]").MustSelect("copilot")
	page.MustElementR("#team-editor [aria-label='Configured launch support']", "Unsupported selected confinement workspace_write")
	// Delay a real API response and change the selection: old support cannot overwrite new selection.
	page.MustEval(`()=>{const original=fetch;window.releaseSupport=null;window.supportDelivered=false;window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0]).includes('/v2/launch-support?harness=claude')){await new Promise(resolve=>window.releaseSupport=resolve);const json=response.json.bind(response);response.json=async()=>{const value=await json();setTimeout(()=>window.supportDelivered=true,0);return value}}return response}}`)
	page.MustElement("#team-editor [name=harness]").MustSelect("claude")
	page.MustWait(`()=>typeof window.releaseSupport==='function'`)
	page.MustElement("#team-editor [aria-label='Copy saved configuration']").MustSelect("Copied policy · one")
	page.MustElementR("#team-editor [aria-label='Configured launch support']", "copilot adapter.*Selected policy is supported")
	require.Equal(t, "unconfined", page.MustElement("#team-editor [name=sandbox]").MustProperty("value").Str())
	page.MustEval(`()=>window.releaseSupport()`)
	page.MustWait(`()=>window.supportDelivered`)
	page.MustWait(`()=>document.querySelector('#team-editor [aria-label="Configured launch support"]').textContent.startsWith('copilot adapter')`)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Empty(t, snapshot.Executions)
}
