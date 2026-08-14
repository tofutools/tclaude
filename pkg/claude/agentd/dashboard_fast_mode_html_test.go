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
	must("function FastModeMenuItem({ member })", "the row cog menu owns the directional control")
	must(`act="fast-mode-set"`, "the row menu dispatches the directional action")
	must("<${FastModeMenuItem} member=${member} />", "the Fast mode control sits in the row cog menu")
	must("/fast-mode/${enabling ? 'enable' : 'disable'}`", "the browser posts to the guarded directional endpoint")
	must("server re-checks the live state", "confirmation discloses the directional toggle re-check")
	must("disabled=${!member.online}", "unknown live state does not disable the control")
	must(".fast-mode-badge", "the indicator has compact dashboard styling")
}
