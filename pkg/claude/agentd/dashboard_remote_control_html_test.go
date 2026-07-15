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
// indicator inside HarnessLine, groups-member-table.js threads the capability into the
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

	// The native indicator reads the best-known state off the agent. It
	// shows ONLY when remote control is on, and is a bare 📱 glyph (no text
	// label) so it stays small — the explanation rides in the title tooltip.
	must("function RemoteBadge({ member })", "RemoteBadge component is defined")
	must("member.state?.remote_control", "the indicator reads the best-known flag off the agent's state")
	must(`title=${title}>📱</span>`, "the indicator is a bare 📱 glyph with the explanation in its title")
	must("harnessCanRename, harnessCanRemoteControl,", "harnessCanRemoteControl is exported from helpers.js")

	// helpers.js: the indicator is ALSO a click affordance — it carries the
	// SAME data-act/conv/agent/label the ⚙-menu "web window" button uses, so
	// clicking the 📱 opens the agent's live session (its Claude Code TUI) in a
	// web terminal via the shared row-actions dispatch. A rename of the action
	// token or the attributes silently unclicks the glyph in the browser.
	must(`class="remote-badge" data-act="web-open-window" ...${memberAttrs(member)}`, "the indicator dispatches the web-window action")
	must(`'data-agent': member.agent_id || member.conv_id`, "the clickable indicator carries the agent selector + label the action needs")
	// CSS: the clickable indicator takes a pointer cursor + hover lift.
	must(".remote-badge:hover", "the clickable remote indicator has a hover affordance")

	// helpers.js: harnessLine appends the indicator to the END of the harness
	// line, trailing the effort/cost tokens — "CC · O4.8 1M high 📱" — rather
	// than a standalone chip in the control cell, so the symbol reads as part
	// of the agent's at-a-glance line.
	must("const remote = html`<${RemoteBadge} member=${member} />`;", "HarnessLine builds the remote indicator")
	must("${remote}", "the remote indicator trails the effort/cost tokens on the harness line")

	// helpers.js: the ⚙-menu toggle item is gated on the capability (returns
	// '' when the harness can't), carries the OPPOSITE intent as data-intent,
	// and dispatches via data-act="toggle-remote-control".
	must("function RemoteMenuItem({ member, canRemote })", "the toggle menu item is defined")
	must("if (!canRemote) return null", "the toggle is hidden for a harness with no Remote Access")
	must(`act="toggle-remote-control"`, "the toggle dispatches the remote-control action")

	// MemberMenu computes the capability from its accepted snapshot and threads
	// it into the native action component. HarnessLine renders the indicator inline.
	must("harnessCanRemoteControl(snapshot, member.state?.harness)", "the row gates the toggle on the agent's harness")
	must("<${RemoteMenuItem} member=${member} canRemote=${canRemote} />", "the native member menu threads the capability into its action")

	// row-actions.js: the handler POSTs {intent} to the dashboard's cookie-auth
	// route (NOT /v1 directly) and refreshes to reconcile the best-known state.
	must("case 'toggle-remote-control':", "the toggle has a dispatch case")
	must("`/api/agents/${encodeURIComponent(agent)}/remote-control`", "the toggle POSTs the dashboard remote-control route")
	must("body: JSON.stringify({ intent }),", "the toggle sends the intent body")

	// CSS: the indicator has a style rule.
	must(".remote-badge", "the remote indicator has a style rule")
}
