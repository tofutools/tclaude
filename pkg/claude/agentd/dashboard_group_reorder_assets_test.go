package agentd

import (
	"strings"
	"testing"
)

// TestDashboardJS_GroupReorderWired guards the drag-to-reorder-groups
// feature (group-reorder.js). It is a pure-frontend feature with no server
// code path a flow test could exercise, so — like the other dashboard-asset
// guards in this package — it string-searches the embedded JS/HTML to pin
// the load-bearing wiring against a silent refactor break.
//
// The pieces that must stay connected:
//   - render.js makes each real group's title (.group-name) the draggable
//     reorder handle, carrying the group name (escaped) as the drag identity;
//   - group-reorder.js drives the drag and persists the order under the
//     dashPrefs key, using a custom drag MIME;
//   - dnd.js (member-row DnD) explicitly ignores that custom MIME so the
//     two document-level drop handlers stay isolated;
//   - tabs.js renders real groups through sortGroupsByPref;
//   - dashboard.js binds the feature at boot;
//   - refresh.js suspends auto-refresh while a reorder drag is in flight.
func TestDashboardJS_GroupReorderWired(t *testing.T) {
	for _, c := range []struct{ needle, why string }{
		// render.js: the group title IS the reorder drag handle — draggable,
		// names its group (escaped), and stays the click-to-toggle label.
		{`class="group-name" data-group-name="${esc(g.name)}" data-group-reorder="${esc(g.name)}" draggable="true"`,
			"render.js makes each real group's title the escaped, draggable reorder handle"},
		// group-reorder.js: the custom MIME and the persisted pref key.
		{`'application/x-tclaude-group'`, "group-reorder.js uses a dedicated drag MIME"},
		{`tclaude.dash.groupOrder`, "group-reorder.js persists the order under its dashPrefs key"},
		{`function sortGroupsByPref(`, "the shared order-applying helper exists"},
		// dnd.js: the explicit isolation guard keeps the two drop handlers apart.
		{`e.dataTransfer.types.includes('application/x-tclaude-group')`,
			"dnd.js's drop handler explicitly ignores a group-reorder drop"},
		// tabs.js: real groups render in the persisted order.
		{`sortGroupsByPref(realGroups.slice())`, "renderGroupsTab applies the saved group order"},
		// dashboard.js: the feature is bound at boot.
		{`bindGroupReorder()`, "dashboard.js wires the reorder binder at boot"},
		// refresh.js: a reorder drag suspends auto-refresh on its own flag.
		{`if (groupReorderActive) return true;`,
			"refreshSuspended() pauses auto-refresh during a reorder drag"},
	} {
		if !strings.Contains(dashboardAssets, c.needle) {
			t.Errorf("dashboard assets missing %q — %s", c.needle, c.why)
		}
	}
}
