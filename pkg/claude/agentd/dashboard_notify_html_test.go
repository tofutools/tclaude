package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_NotifyBellsWired guards the notification-filter UI
// — the top-bar master bell, the per-group header chip and the
// per-agent member-row bell — across the files it spans (render.js
// builds the chips, row-actions.js dispatches the clicks, refresh.js
// repaints the master bell, dashboard.html hosts it, dashboard.css
// styles the states). The repo has no JS test runner, so this asserts
// on the embedded concatenation at `go test ./...`; the daemon-side
// behaviour behind these endpoints is covered by
// dashboard_notify_filter_flow_test.go.
func TestDashboardHTML_NotifyBellsWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// render.js: the three bell surfaces exist and carry the data-act
	// hooks the click router dispatches on.
	must("function memberNotifyBell(m)", "per-agent bell builder is defined")
	must(`data-act="toggle-agent-notify"`, "member bell carries the agent-toggle action")
	must(`data-act="toggle-group-notify"`, "group header chip carries the group-toggle action")
	must("function renderNotifyGlobal(enabled)", "master bell painter is defined")

	// row-actions.js: each action hits its daemon endpoint.
	must("case 'toggle-group-notify':", "group toggle is routed")
	must("notify_enabled: !cur", "group toggle PATCHes the flipped value")
	must("case 'toggle-agent-notify':", "agent toggle is routed")
	must("/notify`", "agent toggle POSTs to /api/agents/{conv}/notify")
	must("case 'toggle-global-notify':", "master toggle is routed")
	must("'/api/notifications'", "master toggle POSTs /api/notifications")

	// The tri-state cycle: inherit → off → on → inherit.
	must("cur === 'inherit' ? 'off' : cur === 'off' ? 'on' : 'inherit'", "agent bell cycles the tri-state")

	// dashboard.html + refresh.js: the master bell exists and repaints
	// from every snapshot poll.
	must(`id="notify-global"`, "top-bar master bell element exists")
	must("renderNotifyGlobal(!!data.notifications_enabled)", "master bell repaints from the snapshot")

	// dashboard.css: the states are styled.
	must(".group-notify.muted", "muted group chip style")
	must(".notify-bell.inherit", "dimmed inherit bell style")
}
