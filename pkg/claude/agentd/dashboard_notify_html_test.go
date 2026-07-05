package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_NotifyBellsWired guards the notification-filter UI
// across the files it spans (render.js + helpers.js build the controls,
// row-actions.js dispatches the clicks, refresh.js repaints the master
// bell, dashboard.html hosts it, dashboard.css styles it). The repo has
// no JS test runner, so this asserts on the embedded concatenation at
// `go test ./...`; the daemon-side behaviour behind these endpoints is
// covered by dashboard_notify_filter_flow_test.go.
//
// The per-group and per-agent controls used to be an always-visible
// header chip / member-row bell. They moved INTO the ⚙ options menus to
// declutter the UI; only the top-bar master bell stays always-visible.
func TestDashboardHTML_NotifyBellsWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still contains %q (%s)", needle, why)
		}
	}

	// The per-agent + per-group notify controls render as ⚙ options-menu
	// rows; the master bell is still its own painter.
	must("function notifyMenuItem(m)", "per-agent notify menu-item builder is defined")
	must("function groupNotifyMenuItem(g)", "per-group notify menu-item builder is defined")
	must(`data-act="toggle-agent-notify"`, "agent menu item carries the agent-toggle action")
	must(`data-act="toggle-group-notify"`, "group menu item carries the group-toggle action")
	must("function renderNotifyGlobal(enabled)", "master bell painter is defined")

	// They are wired INTO the cog menus — not the always-visible header /
	// row surfaces.
	must("+ notifyMenuItem(m) + remoteControlMenuItem(m, canRemote)", "agent notify sits in the row cog menu")
	must("+ groupNotifyMenuItem(g)", "group notify sits in the group cog menu")

	// The old always-visible surfaces are gone — no standalone per-agent
	// bell in the agent-ctl cell, no per-group chip in the summary strip.
	mustNot("function memberNotifyBell", "the old per-agent bell builder is removed")
	mustNot(`class="notify-bell${`, "no standalone per-agent bell remains in the row")
	mustNot(`class="group-notify`, "no per-group notify chip remains in the summary")

	// row-actions.js: each action hits its daemon endpoint (unchanged by
	// the move — only the buttons' DOM position changed).
	must("case 'toggle-group-notify':", "group toggle is routed")
	must("notify_enabled: !cur", "group toggle PATCHes the flipped value")
	must("case 'toggle-agent-notify':", "agent toggle is routed")
	must("/notify`", "agent toggle POSTs to /api/agents/{conv}/notify")

	// The tri-state cycle: inherit → off → on → inherit.
	must("cur === 'inherit' ? 'off' : cur === 'off' ? 'on' : 'inherit'", "agent notify cycles the tri-state")

	// dashboard.html + refresh.js: the master bell exists and repaints
	// from every snapshot poll.
	must(`id="notify-global"`, "top-bar master bell element exists")
	must("renderNotifyGlobal(!!data.notifications_enabled)", "master bell repaints from the snapshot")

	// The master bell now OPENS a popover (notify-menu.js) instead of
	// being a one-click toggle: the master on/off + per-type checklist +
	// human-message + access-request knobs live inside it, all backed by
	// /api/notifications.
	mustNot("case 'toggle-global-notify':", "the blind one-click master toggle is gone")
	must("function bindNotifyMenu()", "the bell popover binder is defined")
	must("bindNotifyMenu();", "the popover binder is wired into bootstrap")
	must(`id="notify-pop"`, "the popover element exists")
	must(`id="notify-pop-enabled"`, "the popover's master on/off checkbox exists")
	must(`data-notify-type="exited"`, "the per-type checklist carries the exited type")
	must(`data-notify-type="awaiting_permission"`, "the per-type checklist carries the awaiting_permission type")
	must(`id="notify-pop-human"`, "the human-message knob exists in the popover")
	must(`id="notify-pop-access"`, "the access-request knob exists in the popover")
	must("access_requests: access.checked", "the access-request knob posts its state")
	must("'/api/notifications'", "the popover reads/writes /api/notifications")
	must(`nav button[data-tab="config"]`, "the 'Config tab ↗' link jumps to the Config tab")

	// dashboard.css: the master bell, its popover, and the action-menu
	// items are styled.
	must(".notify-bell {", "master bell style")
	must("#notify-pop {", "popover style")
	must(".action-menu button {", "menu items (incl. the relocated notify toggles) are styled")
}
