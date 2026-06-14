package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_CodexUsageWired guards the top-bar Codex usage readout —
// the labelled "Claude" / "Codex" two-line layout that renders when
// snapshot.usage.codex is present. The pieces span render.js (builds the
// lines off usage.codex) and dashboard.css (the .multiline / .uline / .usrc
// layout); a rename in one silently breaks the feature in the browser, and
// the repo has no JS test runner, so this asserts on the embedded asset
// bundle at `go test ./...`.
func TestDashboardHTML_CodexUsageWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// render.js: the readout reads the nested codex object and renders the
	// labelled lines through the shared helpers.
	must("function subscriptionWindowsHTML(src)", "shared per-source window builder is defined")
	must("function usageLineHTML(label, tokens)", "labelled-line builder is defined")
	must("u && u.codex", "renderUsage reads the codex sub-object off the snapshot")
	must("usageLineHTML('Claude:'", "the Claude line is labelled")
	must("usageLineHTML('Codex:'", "the Codex line is labelled")
	must("classList.add('multiline')", "the two-line layout toggles the multiline class")

	// dashboard.css: the multiline layout + the right-aligned source label
	// column that stacks the colons.
	must("#usage.multiline", "multiline stacks the readout vertically")
	must("#usage .usrc", "the source label column is styled")
}
