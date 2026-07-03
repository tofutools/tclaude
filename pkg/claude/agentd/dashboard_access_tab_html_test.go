package agentd

import (
	"strings"
	"testing"
)

// The dashboard once had three separate nav tabs that all governed agent
// access control — "Permissions", "Slug registry" and "Sudo". They were
// collapsed into a single "Access" tab whose body switches between the
// three as sub-views via an internal segmented control.
//
// This guards that merge: the three standalone tabs can't creep back, the
// one Access tab and its three sub-panels exist, the renderers stay wired
// to the new per-panel mount points, and the 🔓 sudo deep link routes
// through the merged tab.
func TestDashboardHTML_AccessTabMerged(t *testing.T) {
	absent := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard still contains %q (%s)", needle, why)
		}
	}
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}

	// The three standalone nav buttons and their top-level sections are gone.
	absent(`data-tab="sudo"`, "the standalone Sudo nav button was merged away")
	absent(`data-tab="permissions"`, "the standalone Permissions nav button was merged away")
	absent(`data-tab="slugs"`, "the standalone Slug-registry nav button was merged away")
	absent(`id="tab-sudo"`, "the standalone Sudo section was merged away")
	absent(`id="tab-permissions"`, "the standalone Permissions section was merged away")
	absent(`id="tab-slugs"`, "the standalone Slug-registry section was merged away")

	// One merged Access tab, with its three sub-views behind a segmented
	// control.
	present(`data-tab="access"`, "the merged Access nav button")
	present(`>Access<`, "the Access nav button keeps its label")
	present(`id="tab-access"`, "the merged Access tab section")
	present(`class="access-subnav"`, "the Access tab's segmented sub-nav")
	present(`data-subtab="permissions"`, "the Permissions sub-tab button")
	present(`data-subtab="slugs"`, "the Slug-registry sub-tab button")
	present(`data-subtab="sudo"`, "the Sudo sub-tab button")
	present(`id="access-permissions"`, "the Permissions sub-panel")
	present(`id="access-slugs"`, "the Slug-registry sub-panel")
	present(`id="access-sudo"`, "the Sudo sub-panel")

	// The renderers stay wired to the new per-panel mount divs, and the
	// sudo filter bar + grant button moved intact into the Access tab. Both
	// now reconcile via morphInto (the copy-paste DOM-morph fix) rather than a
	// wholesale innerHTML swap, but still target the same per-panel mounts.
	present(`morphInto($('#permissions-body'), renderPermissions(`, "permissions renderer morphs into #permissions-body")
	present(`morphInto($('#slugs-body'), renderSlugs(`, "slugs renderer morphs into #slugs-body")
	present(`id="sudo-list"`, "the sudo list mount moved into the Access tab")
	present(`id="sudo-grant-open"`, "the + Grant sudo button moved into the Access tab")
	present(`id="filter-sudo"`, "the sudo filter input moved into the Access tab")

	// The sub-tab switcher exists, and the 🔓 sudo-manage deep link routes
	// through the merged tab rather than a now-gone standalone Sudo tab.
	present(`function bindAccessSubtabs(`, "the sub-tab click binder")
	present(`function activateAccessSubtab(`, "the sub-tab activator")
	present(`function showAccessTab(`, "the Access-tab deep-link helper")
	present(`showAccessTab('sudo')`, "the sudo-manage badge deep-links into the Access tab's Sudo sub-view")

	// The segmented-control styling ships.
	present(`.access-subnav`, "segmented-control CSS")
	present(`.access-subtab.active`, "segmented-control active-state CSS")
}
