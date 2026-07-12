package agentd

import (
	"strings"
	"testing"
)

// TestDashboardNavHistory_Wired pins the client-side wiring for the dashboard's
// native browser back/forward navigation (TCL-317). It string-searches the
// embedded source (the same approach as the other dashboard content tests) so
// a refactor that drops the History API calls or the boot hook fails here
// instead of only in a browser. The pure traversal/duplicate/stale logic is
// covered behaviourally by jstest/nav-history-core.test.mjs.
func TestDashboardNavHistory_Wired(t *testing.T) {
	for _, needle := range []string{
		// The History API is actually driven (AC #2), and boot installs the router.
		`history.pushState(`,
		`history.replaceState(`,
		`addEventListener('popstate'`,
		`initNavHistory()`,
		// The popstate path validates the stamped index against the popped URL
		// (a reload leaves stale cross-instance indices) rather than trusting it.
		`resolvePopstate(`,
		// The whole stack is persisted in history.state and reconstructed on
		// reload, so browser traversal keeps its location mapping across a refresh.
		`reviveState(`,
		`serializeStack(`,
		// Subtab switches signal the router (→ /access/sudo, /processes/runs) and
		// the poll reconciles the URL after an involuntary tab switch.
		`tclaude:navigated`,
		`reconcileLocation`,
		// The theme toggle must PRESERVE the current history.state (slop.js), not
		// replace it with {} — otherwise it strips the navIndex nav-history.js
		// stamped and desyncs back/forward after a theme change. Regression guard
		// for the cold-review blocker.
		`window.history.replaceState(window.history.state,`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard nav-history wiring missing %q", needle)
		}
	}
	for _, unwanted := range []string{`id="nav-back"`, `id="nav-forward"`, `.nav-hist-btn`} {
		if strings.Contains(dashboardAssets, unwanted) {
			t.Errorf("dashboard still contains redundant navigation control %q", unwanted)
		}
	}
}

// TestDashboardNavLinks_Wired pins the "nav controls are real web links"
// contract (TCL-364): the location-changing tabs/subtabs are <a href> anchors
// (so hover previews the URL and Cmd/Ctrl/middle-click open a new tab), and
// their click handlers bail on a modified/middle click so the browser's native
// new-tab wins while a plain click stays in-SPA. String-searched over the
// embedded source, like the other dashboard content tests, so a refactor that
// reverts an anchor to a <button> or drops the guard fails here instead of only
// in a browser.
func TestDashboardNavLinks_Wired(t *testing.T) {
	for _, needle := range []string{
		// Top-level tabs are anchors whose href is the location's real path
		// (toPath): Groups → "/", the rest → "/<tab>".
		`<a class="active" data-tab="groups" href="/">`,
		`<a data-tab="jobs" href="/jobs"`,
		`<a data-tab="access" href="/access"`,
		`<a data-tab="config" href="/config"`,
		// Vegas is NOT URL-routed, so it stays a <button> (no href to hover).
		`<button data-tab="vegas">`,
		// Subtabs are anchors too, carrying their nested path (/access/sudo,
		// /processes/runs) while keeping tablist ARIA.
		`data-subtab=${subtab} href=${` + "`/access/${subtab}`" + `} role="tab"`,
		"data-process-subtab=${name} href=${`/processes/${name}`} role=\"tab\"",
		// The shared SPA-link guard: a modified/middle click is left to the
		// browser (native new tab); a plain left-click / synthetic element.click()
		// returns false and the handler preventDefaults + switches in place.
		`function isModifiedClick(e)`,
		`e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0`,
		// The tab + subtab handlers actually apply the guard and cancel the
		// anchor's own navigation on a plain click.
		`if (isModifiedClick(e)) return;`,
		// <a> tabs activate on Enter only; a Space shim restores the former
		// <button> keyboard parity (Space selects the focused tab).
		`e.key !== ' ' && e.key !== 'Spacebar'`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard nav-links contract missing %q", needle)
		}
	}
}
