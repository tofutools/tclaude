package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardConfigPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	html := read("dashboard.html")
	markup := read("js/config-form-markup.js")
	adapter := read("js/config-form-adapter.js")
	state := read("js/config-state.js")
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Config state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	for _, needle := range []string{
		`<div id="config-root"></div>`,
		"mountConfigFeature({ toast, isCyclingTabs }),",
		"export function ConfigApp(",
		"state: configState",
		`id="cfg-save"`,
		`id="cfg-sudo-json"`,
		`id="cfg-agent-persisttoken-keychain"`,
		"a.persist_operator_token_keychain",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Config Preact wiring missing %q", needle)
		}
	}
	for _, retired := range []string{"js/config.js", "import { bindConfigTab } from './config-form-adapter.js'"} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("Config migration left retired path %q", retired)
		}
	}
	for _, retired := range []string{"innerHTML", "function cfgStringRow(", "function configDiffModal("} {
		if strings.Contains(adapter, retired) {
			t.Errorf("Config adapter retains imperative renderer %q", retired)
		}
	}
	if got := strings.Count(adapter, "addEventListener("); got != 1 || !strings.Contains(adapter, "navBtn?.addEventListener('click', activate)") {
		t.Errorf("Config adapter has %d manual listeners; only external tab activation may remain", got)
	}
	for _, component := range []string{"function StringList(", "function TransitionList(", "function ThresholdList(", "function ConfigDiffModal("} {
		if !strings.Contains(dashboardAssets, component) {
			t.Errorf("Config Preact component missing %q", component)
		}
	}
	if strings.Contains(html, `id="cfg-save"`) || strings.Contains(html, `id="cfg-sudo-json"`) {
		t.Error("Config form controls remain dual-owned by static dashboard HTML")
	}
	if !strings.Contains(markup, `id="cfg-save"`) || !strings.Contains(markup, `id="cfg-sudo-json"`) {
		t.Error("Config form controls are not owned by the Preact markup component")
	}
	windowFocus := strings.Index(markup, `id="cfg-focus-window-title"`)
	terminalAttach := strings.Index(markup, `class="cfg-field cfg-terminal-attach-field"`)
	agentHide := strings.Index(markup, `id="cfg-dashboard-show-agent-hide-btn"`)
	if windowFocus < 0 || terminalAttach < windowFocus || agentHide < terminalAttach {
		t.Error("web terminal attachment controls are not grouped after Window focus and before the remaining terminal settings")
	}
	for _, needle := range []string{
		`<${ConfigSelect} id="cfg-terminal" aria-label="Terminal emulator">`,
		`<option value="">Auto-detect (default)</option>`,
		`class="cfg-terminal-attach-timings"`,
		`id="cfg-terminal-attach-mode"`,
		`id="cfg-terminal-attach-initial-delay"`,
		`id="cfg-terminal-attach-repair-delay"`,
		`id="cfg-terminal-attach-pre-delay"`,
	} {
		if !strings.Contains(markup, needle) {
			t.Errorf("web terminal attachment layout missing %q", needle)
		}
	}
	if strings.Contains(markup, `id="cfg-terminal-list"`) {
		t.Error("terminal picker retains the retired datalist instead of the standard config dropdown")
	}
	for _, needle := range []string{
		"checked = !!a.persist_operator_token_keychain",
		"a.persist_operator_token_keychain = true",
		"delete a.persist_operator_token_keychain",
	} {
		if !strings.Contains(adapter, needle) {
			t.Errorf("Config adapter missing keychain persistence round-trip %q", needle)
		}
	}
}

// The footer's Open PRs knobs are only reachable from the Config tab, so the
// markup control, its load binding and its save binding must all exist — a
// control the adapter never reads silently discards whatever the operator
// typed.
func TestDashboardConfigOpenPRControlsRoundTrip(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	markup := read("js/config-form-markup.js")
	adapter := read("js/config-form-adapter.js")
	for _, needle := range []string{
		`id="cfg-dashboard-always-show-open-prs"`,
		`id="cfg-dashboard-recent-pr-window-days"`,
	} {
		if !strings.Contains(markup, needle) {
			t.Errorf("Config markup missing Open PRs control %q", needle)
		}
		id := strings.TrimSuffix(strings.TrimPrefix(needle, `id="`), `"`)
		if got := strings.Count(adapter, "#"+id); got < 2 {
			t.Errorf("Config adapter references %q %d times; it must both load and save it", id, got)
		}
	}
	for _, needle := range []string{"dashboard.always_show_open_prs", "dashboard.recent_pr_window_days"} {
		if !strings.Contains(adapter, needle) {
			t.Errorf("Config adapter never writes %q", needle)
		}
	}
}
