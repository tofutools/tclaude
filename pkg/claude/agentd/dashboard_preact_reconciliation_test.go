package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's recurring publishers must leave interactive DOM under
// bounded Preact owners. This protects focus, selection, form state and keyed
// row identity without reviving the former global DOM reconciler.
func TestDashboardPreactReconciliationWired(t *testing.T) {
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/morph.js"); err == nil {
		t.Fatal("retired custom DOM reconciler is still embedded")
	}
	for _, forbidden := range []string{"morphInto", "data-morph-owned", "morphAttributes"} {
		if dashboardSourceContains(dashboardAssets, forbidden) {
			t.Errorf("dashboard assets retain reconciler contract %q", forbidden)
		}
	}

	present := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — %s", needle, why)
		}
	}

	// Recurring snapshot surfaces use feature-local state and stable keys.
	present("function GroupsList(", "Groups has a Preact owner")
	present("element.getAttribute('data-group-key')", "Groups promotes stable identities to Preact keys")
	present("function LinksList(", "Links has a Preact owner")
	present("key=${String(link.id)}", "Links rows carry stable Preact keys")
	present("function DebugApp(", "Debug has a Preact owner")
	present("function CreditsLeaderboard(", "Vegas leaderboard has a Preact owner")
	present("key=${entry.conv}", "leaderboard rows carry stable Preact keys")
	present("function Usage({ state })", "usage has a Preact owner")
	present("function FooterMeta({ state })", "footer metadata has a Preact owner")
	present("function JobsApp(", "Jobs has a Preact owner")
	present("function AccessApp(", "Access has a Preact owner")
	present("key=${grant.id}", "Access rows carry stable Preact keys")
	present("key=${`cron-${row.cron?.id}`}", "cron rows carry stable Preact keys")
	present("key=${`export-${row.export?.id}`}", "export rows carry stable Preact keys")
	present("key=${plugin.name} plugin=${plugin}", "installed plugin cards carry stable Preact keys")
	present("list.map((template) => html`<${TemplateCard} key=${template.name}", "template cards carry stable Preact keys")
	present("function LogsApp(", "Logs has a Preact owner")
	present("function AuditApp(", "Audit has a Preact owner")
	present("key=${entry.id}", "Audit rows carry stable Preact keys")
	present("key=${`${agent.conv_id}:${agent.day}`}", "cost rows carry stable compound keys")

	for _, forbidden := range []string{
		"$('#groups-list').innerHTML = renderGroups(",
		"$('#jobs-list').innerHTML = renderJobs(",
		"$('#sudo-list').innerHTML = renderSudo(",
		"$('#links-list').innerHTML = renderLinks(",
		"$('#permissions-body').innerHTML = renderPermissions(",
		"$('#slugs-body').innerHTML = renderSlugs(",
		"$('#plugins-list').innerHTML",
		"$('#logs-list').innerHTML",
		"$('#audit-list').innerHTML",
		"$('#meta').textContent = data.popup_base",
	} {
		if strings.Contains(dashboardAssets, forbidden) {
			t.Errorf("dashboard assets retain recurring wholesale write %q", forbidden)
		}
	}
}

// The animation synchronizers now run over keyed Preact nodes. They must only
// restamp when an animation identity changes, otherwise each snapshot publish
// visibly restarts the bot or wizard orbit.
func TestDashboardPreactAnimationStampsStable(t *testing.T) {
	for _, needle := range []string{
		"const sig = cs.animationName + ' ' + period;",
		"if (botStampSig.get(el) === sig && el.style.animationDelay) continue;",
		"orbitStampSig.delete(pill);",
	} {
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — animation stamp stability regressed", needle)
		}
	}
}
