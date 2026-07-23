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
