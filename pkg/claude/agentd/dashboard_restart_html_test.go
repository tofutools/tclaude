package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_AgentRestartWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	must("function RestartMenuItem({ member })",
		"the agent cog owns an ordinary restart item")
	must(`member=${member} act="restart" regular=${regular}`,
		"the menu item dispatches the ordinary restart action")
	must("re-resolving sandbox-profile rules",
		"the confirmation explains why a normal restart is useful")
	must("case 'restart':", "the delegated action handler routes the click")
	must("/restart`", "the browser posts to the dedicated agent endpoint")
	must("no background agents or shell commands",
		"the confirmation explains the authoritative idle gate")
}
