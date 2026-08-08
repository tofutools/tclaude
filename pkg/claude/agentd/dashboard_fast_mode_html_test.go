package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_CodexFastModeIndicatorWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	must("function FastModeBadge({ member })", "the harness line owns the compact indicator")
	must("member.state?.fast_mode !== true", "unknown and known-off state render no badge")
	must("data-act=${actionable ? 'fast-mode-disable' : null}", "the live indicator dispatches disable")
	must("case 'fast-mode-disable':", "the delegated action handler routes the click")
	must("/fast-mode/disable`", "the browser posts to the guarded endpoint")
	must("server re-checks that Fast mode is still on", "confirmation discloses the toggle re-check")
	must(".fast-mode-badge", "the indicator has compact dashboard styling")
}
