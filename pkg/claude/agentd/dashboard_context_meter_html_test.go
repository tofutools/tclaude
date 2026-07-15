package agentd

import (
	"strings"
	"testing"
)

// The context meter's pct-to-lit-blocks arithmetic lives entirely in
// dashboard.html's embedded JS (contextMeter()) — there is no Go code
// path to exercise, and the repo has no JS test runner, so the numeric
// mapping itself is verified by reasoning + manual inspection. This
// guard pins the *formula* so a future edit cannot silently regress it.
//
// Regression it guards: the meter used Math.ceil, which ran a full
// block ahead of the true percentage — 41% lit 3 of 5 blocks (read as
// ~60%). The fix rounds to the nearest block instead, with max(1, …)
// so any non-zero usage still lights one block and min(CTX_SEGMENTS)
// clamping the top.
//
// With CTX_SEGMENTS = 5 (20%-wide bands) the corrected formula
//
//	pct > 0 ? min(5, max(1, round(pct / 20))) : 0
//
// produces this mapping — the contract this guard exists to protect:
//
//	  pct |  0 |  1 | 41 | 50 | 70 | 100
//	blocks |  0 |  1 |  2 |  3 |  4 |   5
//
// (round(41/20)=round(2.05)=2; round(50/20)=round(2.5)=3.)
func TestDashboardHTML_ContextMeterHonestRounding(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html still contains %q (%s)", needle, why)
		}
	}

	// The over-filling bug — ceil over the percentage — must be gone.
	mustNot("Math.ceil(pct", "the ceil over-fill bug must not return")

	// The corrected formula: round to the nearest block, floor of one
	// for any non-zero usage, clamp at the segment count.
	must("Math.round(pct / 20)", "lit count rounds to nearest 20%-wide block")
	must("Math.max(1, Math.round(", "non-zero usage still lights at least one block")
	must("Math.min(5, Math.max(1,", "lit count is clamped to five segments")

	// pct == 0 (or an unknown snapshot, which pins pct to 0) must light
	// zero blocks — the gate that keeps a freshly-spawned agent's meter
	// empty.
	must("const filled = pct > 0", "pct == 0 / unknown lights no blocks")
}
