package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_RemoteControlWired guards the per-agent remote-control
// surfaces (JOH-259): the at-a-glance bare-📱 indicator (appended to the
// harness line after the effort/cost tokens), the ⚙-menu toggle gated on the
// harness's Remote Access capability, and the click handler that POSTs the
// intent to the dashboard route. The pieces span four files (helpers.js builds
// + exports the indicator/menu-item/capability helper and appends the
// indicator inside harnessLine, render.js threads the capability into the
// member cell, row-actions.js dispatches the toggle, dashboard.css styles the
// indicator); a rename in one silently breaks the feature in the browser, and
// the repo has no JS test runner, so this asserts on the embedded
// concatenation at `go test ./...` — the same guard style as the
// harness-line/sandbox-badge test.
func TestDashboardHTML_RemoteControlWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// helpers.js: the capability lookup mirrors harnessCanRename, reading the
	// snapshot's harness catalog flag — so the toggle hides for a harness with
	// no Remote Access (Codex).
	must("function harnessCanRemoteControl(snapshot, name)", "remote-control capability lookup is defined")
	must("h.can_remote_control", "the lookup reads can_remote_control off the harness catalog")

	// helpers.js: the indicator reads the best-known state off the agent. It
	// shows ONLY when remote control is on, and is a bare 📱 glyph (no text
	// label) so it stays small — the explanation rides in the title tooltip.
	must("function remoteControlBadge(m)", "remoteControlBadge helper is defined")
	must("m.state.remote_control", "the indicator reads the best-known flag off the agent's state")
	must(`title="${esc(tip)}">📱</span>`, "the indicator is a bare 📱 glyph with the explanation in its title")
	must("sandboxBadge, remoteControlBadge,", "remoteControlBadge is exported from helpers.js")
	must("harnessCanRename, harnessCanRemoteControl,", "harnessCanRemoteControl is exported from helpers.js")

	// helpers.js: harnessLine appends the indicator to the END of the harness
	// line, trailing the effort/cost tokens — "CC · O4.8 1M high 📱" — rather
	// than a standalone chip in the control cell, so the symbol reads as part
	// of the agent's at-a-glance line.
	must("const remoteEl = remoteControlBadge(m);", "harnessLine builds the remote indicator")
	must("effortEl + costEl + whatifEl + remoteEl", "the remote indicator trails the effort/cost (incl. WHAT-IF) tokens on the harness line")

	// helpers.js: the ⚙-menu toggle item is gated on the capability (returns
	// '' when the harness can't), carries the OPPOSITE intent as data-intent,
	// and dispatches via data-act="toggle-remote-control".
	must("function remoteControlMenuItem(m, canRemote)", "the toggle menu item is defined")
	must("if (!canRemote) return ''", "the toggle is hidden for a harness with no Remote Access")
	must(`data-act="toggle-remote-control"`, "the toggle dispatches the remote-control action")

	// render.js: the capability is computed where lastSnapshot is in scope and
	// threaded into the action builders. The indicator itself is no longer a
	// standalone cell element — harnessLine renders it inline — so render.js
	// only wires the toggle's capability here.
	must("harnessCanRemoteControl(lastSnapshot, state.harness)", "the row gates the toggle on the agent's harness")
	must("ungroupedMemberActions(m, canRemote)", "the ungrouped row threads the capability into its actions")
	must("memberActions(ctx.group, m, canRemote)", "the group row threads the capability into its actions")

	// row-actions.js: the handler POSTs {intent} to the dashboard's cookie-auth
	// route (NOT /v1 directly) and refreshes to reconcile the best-known state.
	must("case 'toggle-remote-control':", "the toggle has a dispatch case")
	must("`/api/agents/${encodeURIComponent(conv)}/remote-control`", "the toggle POSTs the dashboard remote-control route")
	must("body: JSON.stringify({ intent }),", "the toggle sends the intent body")

	// CSS: the indicator has a style rule.
	must(".remote-badge", "the remote indicator has a style rule")
}
