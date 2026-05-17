package agentd

import (
	"strings"
	"testing"
)

// The dashboard tells a clean exit from an unexpected death entirely in
// dashboard.html's embedded JS — statePill reads state.exit_reason.
// There is no Go path and no JS test runner, so this guard pins the
// contract: an offline agent whose exit_reason is 'unexpected' (the
// reaper stamps that when no SessionEnd hook fired) renders as a
// "crashed" pill; every other offline agent — a clean exit, or an
// unknown/blank reason — stays a plain "offline".
func TestDashboardHTML_CrashedPill(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	must("state.exit_reason",
		"statePill must read the exit_reason field off the snapshot")
	must("'unexpected'",
		"only an explicit 'unexpected' reason is treated as a crash")
	must("state-pill state-crashed",
		"the crash case renders a .state-crashed pill")
	must(">crashed</span>",
		"the unexpected-death pill is labelled 'crashed'")
	must(".state-crashed ",
		"the .state-crashed CSS rule must be defined")
}
