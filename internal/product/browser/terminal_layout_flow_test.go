package browser

import (
	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"path/filepath"
	"testing"
	"time"
)

func TestBrowserTerminalSplitAndReloadRetainExactDisconnectedPanes(t *testing.T) {
	provider := &terminalBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, runtimes: map[model.AgentID]*terminalBrowserRuntime{}}
	ctx, page, operator := processEditorBrowser(t, provider)
	for _, id := range []string{"alpha", "beta"} {
		desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
		require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "start_" + id, "target": map[string]any{"agent": map[string]any{"agent_id": id, "expected_revision": 1}}}, nil))
	}
	page.MustElement("#refresh").MustClick()
	for _, id := range []string{"alpha", "beta"} {
		page.MustElement("[data-tab=groups]").MustClick()
		page.MustElementR("#roster .row", id).MustElementR("button", "^Attach$").MustClick()
		page.MustElementR("#terminal-status", "Attached · "+id)
	}
	page.MustElement("#terminal-layout").MustSelect("Split panes")
	require.Len(t, page.MustElements(".terminal-panel:not([hidden])"), 2)
	page.MustElement("#terminal-left").MustClick()
	require.Equal(t, "beta", page.MustElement("#terminal-tabs [role=tab]").MustText())
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustElement("[data-tab=terminals]").MustClick()
	require.Len(t, page.MustElements("#terminal-tabs [role=tab]"), 2)
	require.Equal(t, "beta", page.MustElement("#terminal-tabs [role=tab]").MustText())
	require.Len(t, page.MustElements(".terminal-panel:not([hidden])"), 2)
	require.Contains(t, page.MustElement("#terminal-status").MustText(), "Disconnected · beta")
	require.Eventually(t, func() bool {
		return provider.runtime("alpha").active.Load() == 0 && provider.runtime("beta").active.Load() == 0
	}, 3*time.Second, 10*time.Millisecond)
	page.MustElement("#reconnect-terminal").MustClick()
	page.MustElementR("#terminal-status", "Attached · beta")
	require.EqualValues(t, 0, provider.runtime("alpha").active.Load())
	require.EqualValues(t, 1, provider.runtime("beta").active.Load())
	page.MustElement("#terminal-popout").MustClick()
	var child *rod.Page
	require.Eventually(t, func() bool {
		pages, err := page.Browser().Pages()
		if err != nil {
			return false
		}
		for _, candidate := range pages {
			if candidate.TargetID != page.TargetID {
				child = candidate
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	child = child.Context(ctx)
	defer child.Close()
	child.MustElementR("#terminal-status", "Attached · beta")
	page.MustElementR("#terminal-status", "Disconnected · beta")
	require.Eventually(t, func() bool { return provider.runtime("beta").active.Load() == 1 }, 3*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 2, provider.preparations.Load())
	require.Zero(t, provider.runtime("alpha").stops.Load())
	require.Zero(t, provider.runtime("beta").stops.Load())
}
