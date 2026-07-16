package agentd

import (
	"io/fs"
	"regexp"
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
	present("function GroupsNativeList(", "Groups renders native group and virtual-group shells")
	present("function MemberTable(", "Groups renders native member tables")
	present("key=${member.conv_id}", "member rows carry stable conversation keys")
	present("key=${node.key}", "group shells carry stable view-model keys")
	present("function GroupsInteractionProvider(", "one Preact owner holds menu/editor interaction state")
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

	groupsList, err := fs.ReadFile(dashboardAssetsFS, "js/groups-list.js")
	if err != nil {
		t.Fatalf("read native Groups list: %v", err)
	}
	groupsSource := string(groupsList)
	for _, forbidden := range []string{"renderGroupsHTML", "dangerouslySetInnerHTML", ".innerHTML", "trustedHTMLToVNodes"} {
		if strings.Contains(groupsSource, forbidden) {
			t.Errorf("native Groups list retains forbidden renderer seam %q", forbidden)
		}
	}
	memberTable, err := fs.ReadFile(dashboardAssetsFS, "js/groups-member-table.js")
	if err != nil {
		t.Fatalf("read native Groups member table: %v", err)
	}
	for _, forbidden := range []string{"dangerouslySetInnerHTML", ".innerHTML", "trustedHTMLToVNodes"} {
		if strings.Contains(string(memberTable), forbidden) {
			t.Errorf("native Groups member table retains forbidden renderer seam %q", forbidden)
		}
	}
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/render.js"); err == nil {
		t.Error("retired legacy Groups string renderer is still embedded")
	}
}

// Production components must consume renderer models directly. Reintroducing
// a template/innerHTML parser would revive the transitional dual-render path
// even if it were hidden behind a renamed helper.
func TestDashboardNoProductionHTMLToVNodeBridge(t *testing.T) {
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/html-vnodes.js"); err == nil {
		t.Fatal("retired HTML-to-VNode bridge is still embedded")
	}
	for _, name := range dashboardJSModules() {
		body, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source := string(body)
		for _, forbidden := range []string{
			"trustedHTMLToVNodes", "./html-vnodes.js", "createElement('template')",
			"createElement(\"template\")", "template.content.childNodes",
		} {
			if strings.Contains(source, forbidden) {
				t.Errorf("production module %s retains HTML-to-VNode parser bridge %q", name, forbidden)
			}
		}
	}
	for _, name := range []string{"js/dock-island.js", "js/shell-island.js"} {
		body, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, forbidden := range []string{".innerHTML", "dangerouslySetInnerHTML", "markup="} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("native renderer %s retains innerHTML compatibility bridge %q", name, forbidden)
			}
		}
	}
}

// row-actions.js is deliberately only a cross-feature integration router. Pin
// every retained switch branch to a live producer literal so compatibility
// cases cannot accumulate after their last button moves into a native owner.
func TestDashboardDelegatedActionsHaveLiveProducers(t *testing.T) {
	rowBody, err := fs.ReadFile(dashboardAssetsFS, "js/row-actions.js")
	if err != nil {
		t.Fatalf("read row action router: %v", err)
	}
	producerBody, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard shell: %v", err)
	}
	producers := string(producerBody)
	for _, name := range dashboardJSModules() {
		if name == "js/row-actions.js" {
			continue
		}
		body, readErr := fs.ReadFile(dashboardAssetsFS, name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		producers += "\n" + string(body)
	}

	cases := regexp.MustCompile(`case '([^']+)':`).FindAllStringSubmatch(string(rowBody), -1)
	if len(cases) == 0 {
		t.Fatal("row action router has no dispatch cases")
	}
	for _, match := range cases {
		action := match[1]
		if !strings.Contains(producers, `"`+action+`"`) &&
			!strings.Contains(producers, `'`+action+`'`) {
			t.Errorf("delegated action %q has no producer outside row-actions.js", action)
		}
	}

	allProduction := producers + string(rowBody)
	for _, retired := range []string{
		"cycle-group-offline", "filter-bar-menu", "toggle-force-fold", "toggle-quick-pin",
	} {
		if strings.Contains(string(rowBody), "case '"+retired+"':") {
			t.Errorf("retired action %q still has a delegated switch branch", retired)
		}
		if strings.Contains(allProduction, `data-act="`+retired+`"`) ||
			strings.Contains(allProduction, `data-act='`+retired+`'`) {
			t.Errorf("retired action %q still has a production data-act producer", retired)
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
