package agentd

import (
	"strings"
	"testing"
)

// Clickable column sorting lives inside the bounded Groups and Links islands.
// This guard pins the shared helpers and the local click ownership so a global
// document handler cannot accidentally cycle a sort twice.
func TestDashboardHTML_SortableColumnsWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// Core sort infrastructure.
	must("function sortHead(", "thead builder")
	must("function applySort(", "row sorter")
	must("function cycleSort(", "asc -> desc -> off cycle")
	must("const sortHeader = event.target.closest('th[data-sort-table]')", "Groups owns header clicks")
	must("onClick=${() => state.cycleSort(column.col)}", "Links owns header clicks")
	if strings.Contains(dashboardAssets, "function bindSortHeaders(") {
		t.Error("retired global sort delegation remains")
	}

	// Every primary table opts in by rendering a sortHead(...) header.
	// The virtual sub-tables (Retired / Conversations / Pending / Replaced
	// generations) are included: they're the "non-real" groups that gained
	// the same clickable headers as real groups.
	for _, table := range []string{
		"members",
		"retired", "conversations", "pending", "replaced",
	} {
		must("sortHead('"+table+"'", table+" table renders a sortable header")
	}
	must("shortAgentId(p.agent_id, '') || p.label", "pending ID cell leads with its reserved stable id and preserves the full legacy label")
	must("LINK_COLS.map(", "Links island uses the shared Links column specification")
	// Jobs is the Preact pilot: its component maps the same JOBS_COLS spec to
	// interactive keyed headers instead of emitting legacy sortHead HTML.
	must("function SortHead(", "Jobs island renders its sortable header component")
	must("JOBS_COLS.map(", "Jobs island uses the shared Jobs column specification")
	// Access owns sudo sorting inside its Preact feature boundary.
	must("SUDO_COLUMNS.map(", "Access island renders its sortable sudo headers")

	// The headers must carry the attributes the click handler reads.
	must("data-sort-table=", "headers tag their table key")
	must("data-sort-col=", "headers tag their column key")
}
