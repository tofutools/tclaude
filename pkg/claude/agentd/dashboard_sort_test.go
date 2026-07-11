package agentd

import (
	"strings"
	"testing"
)

// Clickable column sorting lives entirely in dashboard.html's embedded
// JS — there's no server code path to exercise with a flow test. This
// guards against the markup/JS being silently dropped in a future
// refactor of that file: it asserts the core helpers exist, that the
// click delegation is actually installed, and that every primary table
// opts in via a sortHead(...) call.
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
	must("function bindSortHeaders(", "header click delegation")
	must("bindSortHeaders();", "delegation is actually installed at startup")

	// Every primary table opts in by rendering a sortHead(...) header.
	// The virtual sub-tables (Retired / Conversations / Pending / Replaced
	// generations) are included: they're the "non-real" groups that gained
	// the same clickable, agent-id-leading headers as real groups.
	for _, table := range []string{
		"members", "sudo", "links",
		"retired", "conversations", "pending", "replaced",
	} {
		must("sortHead('"+table+"'", table+" table renders a sortable header")
	}
	// Jobs is the Preact pilot: its component maps the same JOBS_COLS spec to
	// interactive keyed headers instead of emitting legacy sortHead HTML.
	must("function SortHead(", "Jobs island renders its sortable header component")
	must("JOBS_COLS.map(", "Jobs island uses the shared Jobs column specification")

	// The headers must carry the attributes the click handler reads.
	must("data-sort-table=", "headers tag their table key")
	must("data-sort-col=", "headers tag their column key")
}
