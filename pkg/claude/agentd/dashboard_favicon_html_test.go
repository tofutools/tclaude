package agentd

import (
	"strings"
	"testing"
)

// The dashboard favicon and header control share the system tray's
// agent-network asset. This guard keeps those two brand surfaces in lockstep.
func TestDashboardHTML_FaviconAgentNetwork(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// A favicon link must exist.
	must(`<link rel="icon"`, "the page declares a favicon")

	must(`href="/static/tclaude-icon.svg"`, "favicon uses the tray-network asset")
	must(`<img src="/static/tclaude-icon.svg" alt="">`, "header control uses the same asset")
	must(`class="theme-mode-badge"`, "header control retains a compact theme badge")
	must("body.slop header h1 .slop-icon {\n  background: transparent;\n  border: 0;", "slop theme preserves the compact icon button")
	must("body.wizard header h1 .slop-icon {\n  background: transparent;\n  border: 0;", "wizard theme preserves the compact icon button")

	icon := dashboardAssetFile(t, "tclaude-icon.svg")
	for _, needle := range []string{`viewBox="0 0 22 22"`, `cx="11" cy="5.5"`, `cx="6" cy="16"`, `cx="16" cy="16"`} {
		if !strings.Contains(icon, needle) {
			t.Errorf("tclaude-icon.svg missing %q (tray agent-network geometry)", needle)
		}
	}
}
