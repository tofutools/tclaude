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

// TestDashboardHTML_CodexUsageColumnAlignment guards the two-line readout's
// column alignment: a monospace block where each field reserves a worst-case
// `ch` width, so the Claude/Codex rows line up field-for-field and the layout
// doesn't shift as a countdown ticks down or a percent crosses 99→100. CSS
// only, so it lives with the other asset-wiring guards.
func TestDashboardHTML_CodexUsageColumnAlignment(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// The block is monospace so `ch` widths are predictable, and the source
	// label is inline-block (a bare inline span ignores min-width/text-align).
	must("#usage.multiline {", "the multiline container is styled as a block")
	must("display: inline-block", "fields reserve width via inline-block")
	// Worst-case reserved widths: source label, percent, and reset hint.
	must("min-width: 7ch;", "label / reset-hint columns reserve their worst case")
	must("min-width: 4ch;", "the percent column reserves up to \"100%\"")
	must("#usage.multiline .upct", "the percent column is fixed-width + right-aligned")
	must("#usage.multiline .urem", "the reset-hint column is fixed-width")

	// render.js must NOT reintroduce the leading space before .urem (it would
	// become a stray flex item and break the monospace column widths).
	must("'<span class=\"urem\">(' + esc(win.remaining)", "no leading space before the reset-hint span")
}
