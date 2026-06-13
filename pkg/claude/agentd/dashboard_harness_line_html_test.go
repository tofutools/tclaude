package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_HarnessLineWired guards the per-agent harness/model
// line — "CC · O4.8 1M" under the row's dot/focus/cog cluster — plus
// its appearance in the status-dot tooltip. The pieces span three files
// (helpers.js builds + exports it, render.js wires it into the member
// cell, dashboard.css styles it); a rename in one silently breaks the
// feature in the browser, and the repo has no JS test runner, so this
// asserts on the embedded concatenation at `go test ./...`.
//
// The model itself comes from state.model — surfaced by the dashboard
// snapshot from the sessions.model column the statusline hook records.
func TestDashboardHTML_HarnessLineWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// helpers.js: the builders are defined, read state.model, and are
	// exported for render.js.
	must("function harnessLine(m)", "harnessLine helper is defined")
	must("function harnessModel(m)", "harnessModel helper (tooltip text) is defined")
	must("m.state.model", "the line reads the model off the agent's state")
	must("agentStatusDot, harnessLine,", "harnessLine is exported from helpers.js")

	// render.js: wired into the member control cell — same column as the
	// dot/actions, NOT a new <td>.
	must("${agentStatusDot(m)}${actions}</div>${harnessLine(m)}", "harnessLine renders in the agent-ctl cell")

	// The always-visible label is shortModel()-compressed; the FULL name
	// stays in the tooltip (the title attr / the status-dot tip).
	must("function shortModel(", "shortModel compressor is defined")
	must("${esc(shortModel(model))}", "the visible chip uses the shortened model")
	must("Model: ${model}", "harnessLine's tooltip keeps the FULL model name")

	// The reasoning-effort level (JOH-37) trails the model — "CC · O4.8 1M
	// high" — read off state.effort_level, with its own styled span and a
	// tooltip clause. Omitted when absent so models without effort support
	// stay at "CC · O4.8 1M".
	must("m.state.effort_level", "harnessLine reads the effort level off the agent's state")
	must("harness-effort", "the effort token has its own span")
	must("Effort: ${effort}", "harnessLine's tooltip names the effort level when present")

	// Status-dot tooltip surfaces the harness+model on hover (the brief's
	// second ask), using the full model via harnessModel.
	must("running on ${hm}", "the status-dot tooltip appends the harness/model")

	// CSS: the line and its prefix/separator are styled (no chip/box — one
	// continuous string).
	must(".agent-harness", "harness line has a style rule")
	must(".harness-sep", "the middot separator is styled")
	must(".harness-effort", "the effort token is styled")

	// The harness is a frontend constant (only Claude Code exists), not a
	// DB column — the label says "CC".
	must("const HARNESS_SHORT = 'CC'", "harness short label is the CC constant")
}

// TestDashboardHTML_ShortModelRules pins the shortModel() compression
// rules. The transform lives entirely in helpers.js JS and the repo has
// no JS test runner, so — as with the context-meter formula — this guards
// the *rules* by asserting their source substrings survive, and the
// contract is documented here for the manual-verification trail:
//
//	"Opus 4.8 (1M context)" → "O4.8 1M"   (initial+version, size token)
//	"Opus 4.8"              → "O4.8"       (initial glued to version)
//	"Sonnet 4.6"            → "S4.6"
//	"Haiku 4.5"             → "H4.5"
//
// A regression in any rule (dropping the parenthetical peel, the size
// extraction, or the no-space initial+version glue) trips one of these.
func TestDashboardHTML_ShortModelRules(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// Rule 1: peel a trailing "(…)" parenthetical and pull a size token.
	must(`main.match(/\(([^)]*)\)\s*$/)`, "peels the trailing parenthetical")
	must(`paren[1].match(/\d+\s*[KMBkmb]/)`, "extracts the window size token (1M / 200K)")

	// Rule 2: family initial glued onto the version with no space.
	must("parts[0].charAt(0).toUpperCase() + parts.slice(1).join(' ')", "initial + version, no space")

	// Rule 2b: the size token is appended after a space when present.
	must("size ? `${core} ${size}` : core", "size token joins with a space, else core alone")
}
