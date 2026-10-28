package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_ActivityTabWired pins the Diagnostics tab (formerly
// Activity): Audit, Logs and
// Performance (the former Debug tab) share one nav entry with a subnav, every
// view is always shown, and the retired /audit, /logs and /debug locations
// heal onto their subtab. Behaviour is covered by jstest/activity-tab.test.mjs
// and jstest/nav-history-core.test.mjs.
func TestDashboardHTML_ActivityTabWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	must(`<a data-tab="diagnostics" href="/diagnostics"`, "the one Diagnostics nav anchor")
	must(`<section id="tab-diagnostics">`, "the Diagnostics section the generic switcher toggles")
	must(`role="tablist" aria-label="Diagnostics views"`, "the Diagnostics subnav")
	for _, sub := range []struct{ name, regular, wizard string }{
		{"audit", "Audit", "Deeds"},
		{"logs", "Logs", "Runes"},
		{"performance", "Performance", "Alchemy"},
	} {
		must(`data-subtab="`+sub.name+`" href="/diagnostics/`+sub.name+`"`, sub.name+" subtab carries its deep path")
		must(`<span class="tab-label-regular">`+sub.regular+`</span><span class="tab-label-wizard">`+sub.wizard+`</span>`,
			sub.name+" subtab keeps its plain and arcane labels")
		must(`id="activity-`+sub.name+`" role="tabpanel"`, sub.name+" panel")
	}
	must("bindActivityTab()", "dashboard bootstrap binds the Activity subnav")
	must("diagnostics: new Set(['audit', 'logs', 'performance'])", "the router knows the Diagnostics subtabs")
	must("activity: 'diagnostics',", "the former /activity paths heal to /diagnostics")
	must("debug: Object.freeze({ tab: 'diagnostics', subtab: 'performance' })", "the retired /debug heals to Performance")
	must("diagnostics: 'audit'", "a peer view offers Diagnostics for its audit log")

	for _, retired := range []string{`data-tab="audit"`, `data-tab="logs"`, `data-tab="debug"`, `id="tab-audit"`, `id="tab-logs"`, `id="tab-debug"`} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("dashboard assets still carry the retired top-level tab %q", retired)
		}
	}
}
