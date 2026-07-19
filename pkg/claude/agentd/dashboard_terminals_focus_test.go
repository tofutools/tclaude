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
	state := readDashboardJS(t, "terminal-shell-state.js")
	actions := readDashboardJS(t, "terminal-shell-actions.js")
	for _, needle := range []string{"function findPaneKey(", "function focusForSelectors("} {
		if !strings.Contains(state+actions, needle) {
			t.Errorf("terminal shell missing %q — focus-to-open-pane plumbing broken", needle)
		}
	}
	// The tab exposes the focus entry point.
	if tab := readDashboardJS(t, "terminals-tab.js"); !strings.Contains(tab, "export function focusTerminalForConv(") {
		t.Error("terminals-tab.js must export focusTerminalForConv for the focus callers")
	}

	// The per-agent 'jump' row action must consult the open pane BEFORE the
	// native /api/jump — otherwise it would raise an OS window even when the
	// live view is the in-browser terminal.
	rows := readDashboardJS(t, "row-action-handler.js")
	focusIdx := strings.Index(rows, "focusTerminalForConv([agent], terminalPaneOptions)")
	jumpIdx := strings.Index(rows, "/api/jump/")
	if focusIdx < 0 || jumpIdx < 0 || focusIdx > jumpIdx {
		t.Error("row-action-handler.js 'jump' case must call focusTerminalForConv([agent], terminalPaneOptions) BEFORE POSTing " +
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

// TestGroupTerminalActionsSupportBackgroundOpen pins the modifier intent from
// the delegated click boundary through both Groups-tab launchers and into the
// terminal shell's reveal option. Ctrl and Cmd deliberately share one boolean
// so either works regardless of the operator's platform.
func TestGroupTerminalActionsSupportBackgroundOpen(t *testing.T) {
	rows := readDashboardJS(t, "row-actions.js") + readDashboardJS(t, "row-action-handler.js")
	for _, needle := range []string{
		"Boolean(event?.ctrlKey || event?.metaKey)",
		"openInBackground: Boolean(",
		"BACKGROUND_CONTEXT_ACTIONS = new Set(['jump', 'web-open-window'])",
		"document.addEventListener('contextmenu', onContextMenu)",
		"document.addEventListener('mousedown', clearContextActivation)",
		"if (source === contextActivatedSource)",
		"const terminalPaneOptions = { reveal: action.openInBackground !== true }",
		"focusTerminalForConv([agent], terminalPaneOptions)",
		"openWebWindowPane(agent, label, terminalPaneOptions)",
	} {
		if !strings.Contains(rows, needle) {
			t.Errorf("Groups terminal background-open wiring missing %q", needle)
		}
	}

	shell := readDashboardJS(t, "terminals-tab.js") +
		readDashboardJS(t, "terminal-shell-actions.js") +
		readDashboardJS(t, "terminal-shell-state.js")
	for _, needle := range []string{
		"openTerminalPane(seedOrPromise, { reveal = true } = {})",
		"current.openPane(seed, { reveal })",
		"state.openPane(seed, options)",
		"if (reveal) requestReveal()",
		"state.activatePane(key, options)",
	} {
		if !strings.Contains(shell, needle) {
			t.Errorf("terminal shell background-open wiring missing %q", needle)
		}
	}
}

// TestBulkFocusUsesWebPanesByDefault guards the top-bar/group "windows…"
// modal against bypassing dashboard.default_terminal. Its native endpoint is
// best-effort OS-window focus and cannot create an in-browser pane, so the web
// branch must precede the payload + fetch and open every checked candidate via
// the same helper as the dedicated "web window" action.
func TestBulkFocusUsesWebPanesByDefault(t *testing.T) {
	actions := readDashboardJS(t, "transaction-dialog-actions.js")
	branch := strings.Index(actions, "if (request.direction === 'focus' && request.webTerminal) {")
	open := strings.Index(actions, "openWebWindowPane(target.selector, target.label)")
	fetch := strings.Index(actions, "fetchImpl('/api/agent-windows'")
	if branch < 0 || open < branch || fetch < open {
		t.Fatal("transaction actions must open each selected web pane before the native-only " +
			"/api/agent-windows path when web terminals are the default")
	}
}

// TestPaneSeedsCarryAgent pins that BOTH pane kinds tag their seed with a
// standalone `agent` field — findPaneKey matches on seed.agent, so a missing
// tag would silently make focus never jump to that pane. Guards against the
// field being dropped while the (identically-valued but differently-purposed)
// `hideConv: agent` stays, which would leave focus broken but detach working.
// The seeds live in terminals-tab.js's shared openWebWindowPane /
// openWebTermPane helpers (the "web window" / "web term" buttons AND the
// default-terminal routing both call them), so assert there.
func TestPaneSeedsCarryAgent(t *testing.T) {
	tab := readDashboardJS(t, "terminals-tab.js")
	for _, c := range []struct{ name, anchor string }{
		{"openWebWindowPane", "key: `window:${agent}`"},
		{"openWebTermPane", "key: `term:${agent}:${which}`"},
	} {
		at := strings.Index(tab, c.anchor)
		if at < 0 {
			t.Fatalf("terminals-tab.js %s seed not found (anchor %q)", c.name, c.anchor)
		}
		block := tab[at:min(len(tab), at+400)]
		// A bare `agent,` field, distinct from `hideConv: agent,` (present only
		// on the live-session seed): more `agent,` than `hideConv: agent,`.
		if strings.Count(block, "agent,") <= strings.Count(block, "hideConv: agent,") {
			t.Errorf("terminals-tab.js %s seed must carry a standalone `agent` field so focus can "+
				"jump to it (not just `hideConv: agent`)", c.name)
		}
	}
}
