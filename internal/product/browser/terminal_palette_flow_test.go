package browser

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserWizardTerminalPalettePersistsAndUpdatesPopout(t *testing.T) {
	provider := &terminalBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, runtimes: map[model.AgentID]*terminalBrowserRuntime{}}
	ctx, page, operator := processEditorBrowser(t, provider)
	page = page.Timeout(45 * time.Second)
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	for _, id := range []string{"alpha", "beta"} {
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
	require.Len(t, page.MustElements(".xterm-scrollable-element"), 2)
	page.MustElement("#presentation-controls summary").MustClick()
	page.MustElement("#presentation-mode").MustSelect("Wizard")
	page.MustElementR("#presentation-status", "preferences saved")
	page.MustWait(`() => Array.from(document.querySelectorAll('.xterm-scrollable-element')).every(e=>getComputedStyle(e).backgroundColor==='rgb(18, 12, 36)')`)
	page.MustElement("#neutral-terminals").MustClick()
	page.MustWait(`() => Array.from(document.querySelectorAll('.xterm-scrollable-element')).every(e=>getComputedStyle(e).backgroundColor==='rgb(13, 17, 23)')`)
	page.MustElementR("#presentation-status", "preferences saved")
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	require.True(t, page.MustElement("#neutral-terminals").MustProperty("checked").Bool())
	page.MustElement("[data-tab=terminals]").MustClick()
	page.MustElement("#reconnect-terminal").MustClick()
	page.MustElementR("#terminal-status", "Attached · beta")
	page.MustWait(`() => Array.from(document.querySelectorAll('.xterm-scrollable-element')).every(e=>getComputedStyle(e).backgroundColor==='rgb(13, 17, 23)')`)
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
	child.MustWait(`() => Array.from(document.querySelectorAll('.xterm-scrollable-element')).every(e=>getComputedStyle(e).backgroundColor==='rgb(13, 17, 23)')`)
	page.MustElement("#presentation-controls summary").MustClick()
	page.MustElement("#neutral-terminals").MustClick()
	child.MustWait(`() => Array.from(document.querySelectorAll('.xterm-scrollable-element')).every(e=>getComputedStyle(e).backgroundColor==='rgb(18, 12, 36)')`)
	require.True(t, child.MustEval(`() => document.getElementById('radio-audio').paused`).Bool())
	require.EqualValues(t, 2, provider.preparations.Load())
	require.Zero(t, provider.runtime("beta").stops.Load())
	require.Eventually(t, func() bool { return provider.runtime("beta").active.Load() == 1 }, 3*time.Second, 10*time.Millisecond)
}
