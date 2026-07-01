package agentd

import (
	"strings"
	"testing"
)

// When an agent already has an open web terminal / window pane in the dashboard
// Terminals tab, pressing "focus" for that agent should jump to the pane rather
// than raising a native OS window. These structural guards pin that wiring
// against the embedded JS (no JS test runner). readDashboardJS lives in
// dashboard_terminals_hide_test.go (same package).

// TestFocusJumpsToOpenPane pins the core + tab plumbing and the two focus
// callers' short-circuit.
func TestFocusJumpsToOpenPane(t *testing.T) {
	// The core can find a pane by agent and (re)activate it.
	core := readDashboardJS(t, "terminals-core.js")
	for _, needle := range []string{"function findPaneKey(", "activatePane: activate"} {
		if !strings.Contains(core, needle) {
			t.Errorf("terminals-core.js missing %q — focus-to-open-pane plumbing broken", needle)
		}
	}
	// The tab exposes the focus entry point.
	if tab := readDashboardJS(t, "terminals-tab.js"); !strings.Contains(tab, "export function focusTerminalForConv(") {
		t.Error("terminals-tab.js must export focusTerminalForConv for the focus callers")
	}

	// The per-agent 'jump' row action must consult the open pane BEFORE the
	// native /api/jump — otherwise it would raise an OS window even when the
	// live view is the in-browser terminal.
	rows := readDashboardJS(t, "row-actions.js")
	focusIdx := strings.Index(rows, "focusTerminalForConv([agent])")
	jumpIdx := strings.Index(rows, "/api/jump/")
	if focusIdx < 0 || jumpIdx < 0 || focusIdx > jumpIdx {
		t.Error("row-actions.js 'jump' case must call focusTerminalForConv([agent]) BEFORE POSTing " +
			"/api/jump — so an open web pane is preferred over a native window")
	}

	// The palette per-agent focus mirrors it.
	pal := readDashboardJS(t, "palette.js")
	pFocus := strings.Index(pal, "focusTerminalForConv([conv])")
	pJump := strings.Index(pal, "/api/jump/")
	if pFocus < 0 || pJump < 0 || pFocus > pJump {
		t.Error("palette.js jumpAgent must call focusTerminalForConv([conv]) BEFORE POSTing /api/jump")
	}
}

// TestPaneSeedsCarryAgent pins that BOTH pane kinds tag their seed with a
// standalone `agent` field — findPaneKey matches on seed.agent, so a missing
// tag would silently make focus never jump to that pane. Guards against the
// field being dropped while the (identically-valued but differently-purposed)
// `hideConv: agent` stays, which would leave focus broken but detach working.
func TestPaneSeedsCarryAgent(t *testing.T) {
	rows := readDashboardJS(t, "row-actions.js")
	for _, c := range []struct{ name, anchor string }{
		{"web-open-window", "key: `window:${conv}`"},
		{"web-term", "key: `term:${conv}:${which}`"},
	} {
		at := strings.Index(rows, c.anchor)
		if at < 0 {
			t.Fatalf("row-actions.js %s seed not found (anchor %q)", c.name, c.anchor)
		}
		block := rows[at:min(len(rows), at+400)]
		// A bare `agent,` field, distinct from `hideConv: agent,` (present only
		// on the live-session seed): more `agent,` than `hideConv: agent,`.
		if strings.Count(block, "agent,") <= strings.Count(block, "hideConv: agent,") {
			t.Errorf("row-actions.js %s seed must carry a standalone `agent` field so focus can "+
				"jump to it (not just `hideConv: agent`)", c.name)
		}
	}
}
