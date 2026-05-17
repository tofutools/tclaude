package agentd

import (
	"strings"
	"testing"
)

// The dashboard once had two overlapping tabs: a "Groups" tab and a
// standalone "Agents" tab. The Groups view was the superset, so the
// Agents tab was removed and the Groups tab relabelled "Agents" — a
// label-only rename: every internal identifier stays keyed "groups".
//
// This guards that structure: the dropped tab can't creep back, the
// rename stays label-only, and the rich cleanup modal the old Agents
// tab owned keeps a home on the renamed tab.
func TestDashboardHTML_NoStandaloneAgentsTab(t *testing.T) {
	absent := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html still contains %q (%s)", needle, why)
		}
	}
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// The standalone Agents tab — its nav button and its section — is gone.
	absent(`data-tab="agents"`, "the standalone Agents nav button was removed")
	absent(`id="tab-agents"`, "the standalone Agents tab section was removed")

	// The Groups tab survived and is now labelled "Agents"...
	present(`data-tab="groups">Agents<`, "the groups tab is relabelled Agents")
	// ...but the rename is label-only: internal identifiers stay "groups".
	present(`id="tab-groups"`, "the tab section keeps its internal groups id")
	present("function renderGroupsTab(", "the tab still renders via renderGroupsTab")

	// The dead Agents-tab JS went with it.
	for _, gone := range []string{
		"function renderAgentsTab(",
		"function renderAgents(",
		"function renderRetired(",
		"function renderConversations(",
	} {
		absent(gone, "dead Agents-tab renderer removed")
	}

	// The rich multi-category cleanup modal the Agents tab owned is
	// kept, repointed onto the renamed tab's "clean up" button; the
	// now-orphaned 'all-groups' cleanup mode was retired with it.
	present("openCleanupModal({ mode: 'agents' })", "cleanup button keeps the rich 'agents' modal")
	absent("'all-groups'", "the orphaned all-groups cleanup mode was removed")
}
