package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_NotifyBellsWired guards the notification-filter UI
// across the files it spans. Per-agent/group menu rows are native Preact;
// the global bell + popover are a bounded Preact island with separate state,
// actions and view modules. Behaviour is covered by notify-preact.test.mjs and
// the daemon endpoints by dashboard_notify_filter_flow_test.go.
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
	// rows; the global bell and popover are rendered by their bounded island.
	must("function NotifyMenuItem({ member })", "per-agent notify menu-item component is defined")
	must("function GroupMenuItems({ group, members, snapshot })", "per-group menu component is defined")
	must(`act="toggle-agent-notify"`, "agent menu item carries the agent-toggle action")
	must(`data-act="toggle-group-notify"`, "group menu item carries the group-toggle action")

	// They are wired INTO the cog menus — not the always-visible header /
	// row surfaces.
	must("<${NotifyMenuItem} member=${member} />", "agent notify sits in the row cog menu")
	must("<${ActionMenu} menuKey=${`group:${group.name}`} kind=\"group-menu\"", "group notify sits in the group cog menu")

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

	// During the shell migration dashboard.html remains the host source and the
	// accepted snapshot remains the master bell's source of truth.
	must(`id="notify-global"`, "top-bar master bell element exists")
	must("bellEnabled: !!snap?.notifications_enabled", "Preact bell derives from the accepted snapshot")

	// The master bell now OPENS a popover (notify-menu.js) instead of
	// being a one-click toggle: the master on/off + per-type checklist +
	// human-message + access-request knobs live inside it, all backed by
	// /api/notifications.
	mustNot("case 'toggle-global-notify':", "the blind one-click master toggle is gone")
	must("export function createNotifyState(", "notification state boundary is defined")
	must("export function createNotifyActions(", "notification action boundary is defined")
	must("export function NotifyApp(", "notification Preact view is defined")
	must("export function mountNotifyIsland(", "notification shell mount is defined")
	must(`id="notify-pop"`, "the popover element exists")
	must(`id="notify-pop-enabled"`, "the popover's master on/off checkbox exists")
	must(`'exited'`, "the per-type checklist carries the exited type")
	must(`'awaiting_permission'`, "the per-type checklist carries the awaiting_permission type")
	must("data-notify-type=${type}", "the Preact checklist preserves the type data attribute")
	must(`id="notify-pop-human"`, "the human-message knob exists in the popover")
	must(`id="notify-pop-access"`, "the access-request knob exists in the popover")
	must("{ access_requests: !!enabled }", "the access-request knob posts its state")
	must("'/api/notifications'", "the popover reads/writes /api/notifications")
	must(`nav [data-tab="config"]`, "the 'Config tab ↗' link jumps to the Config tab")
	must("removeEventListener('pointerdown'", "outside-click listener is cleaned up on unmount")
	must("removeEventListener('keydown'", "Escape listener is cleaned up on unmount")

	// dashboard.css: the master bell, its popover, and the action-menu
	// items are styled.
	must(".notify-bell {", "master bell style")
	must("#notify-pop {", "popover style")
	must(".action-menu button {", "menu items (incl. the relocated notify toggles) are styled")
}
