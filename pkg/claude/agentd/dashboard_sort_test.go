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
	must("export function SortTh(", "the shared sortable header (ui-components.js)")
	must("function applySort(", "row sorter")
	must("function cycleSort(", "asc -> desc -> off cycle")
	must("function nextSort(", "the one asc -> desc -> off step every 3-state table shares (sort.js)")
	must("export function TableHead(", "the shared header row built from a column list (table-kit.js)")
	must("const sortHeader = event.target.closest('th[data-sort-table]')", "Groups owns header clicks")
	must(`sortTable="links" onSort=${(col) => state.cycleSort(col)}`, "Links owns header clicks")
	if strings.Contains(dashboardAssets, "function bindSortHeaders(") {
		t.Error("retired global sort delegation remains")
	}
	if strings.Contains(dashboardAssets, "function sortHead(") {
		t.Error("the retired HTML-string thead builder remains; render SortTh")
	}

	// Every primary table opts in by rendering the native SortHead component.
	// The virtual sub-tables (Retired / Conversations / Pending / Replaced
	// generations) are included: they're the "non-real" groups that gained
	// the same clickable headers as real groups.
	for _, table := range []string{
		"members",
		"retired", "conversations", "pending", "replaced",
	} {
		must(`SortHead} table="`+table+`"`, table+" table renders a sortable header")
	}
	must("shortAgentId(row.agent_id, '') || row.label", "pending ID cell leads with its reserved stable id and preserves the full legacy label")
	// Cross-node rows splice the This node / Peer side columns into the same
	// shared spec; local-only nodes render LINK_COLS unchanged.
	must(": LINK_COLS;", "Links island uses the shared Links column specification")
	must("<${TableHead} cols=${columns} activeCol=${state.sort.value?.col}", "Links island renders its header from that specification")
	// Jobs is the Preact pilot: its component maps the same JOBS_COLS spec to
	// interactive keyed SortTh headers.
	must("function SortHead(", "Jobs island renders its sortable header component")
	must("<${TableHead} cols=${JOBS_COLS}", "Jobs island uses the shared Jobs column specification")
	// Access owns sudo sorting inside its Preact feature boundary.
	must("<${TableHead} cols=${SUDO_COLUMNS}", "Access island renders its sortable sudo headers from its column specification")

	// The headers must carry the attributes the click handler reads.
	must("data-sort-table=", "headers tag their table key")
	must("data-sort-col=", "headers tag their column key")
}
