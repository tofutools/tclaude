package agentd

import (
	"strings"
	"testing"
)

// TestDashboardNavHistory_Wired pins the client-side wiring for the dashboard's
// back/forward navigation (TCL-317). It string-searches the embedded source
// (the same approach as the other dashboard content tests) so a refactor that
// drops the chrome buttons, the History API calls, or the boot hook fails here
// instead of only in a browser. The pure traversal/duplicate/stale logic is
// covered behaviourally by jstest/nav-history-core.test.mjs.
func TestDashboardNavHistory_Wired(t *testing.T) {
	for _, needle := range []string{
		// The header chrome buttons, with accessible labels and a disabled
		// initial state (AC #3).
		`id="nav-back"`,
		`id="nav-forward"`,
		`aria-label="Back — go to the previous dashboard location"`,
		`aria-label="Forward — go to the next dashboard location"`,
		// The History API is actually driven (AC #2), and boot installs the router.
		`history.pushState(`,
		`history.replaceState(`,
		`addEventListener('popstate'`,
		`initNavHistory()`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard nav-history wiring missing %q", needle)
		}
	}
}
