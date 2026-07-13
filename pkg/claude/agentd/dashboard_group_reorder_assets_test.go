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
//   - render.js makes each real group's HEADER (<summary>) the draggable
//     reorder handle, carrying the group name (escaped) as the drag identity;
//   - group-reorder.js suppresses that drag for a press on an interactive
//     header child (so the title still folds and the chips still edit), drives
//     the drag, and persists the order under the dashPrefs key via a custom MIME;
//   - dnd.js (member-row DnD) explicitly ignores that custom MIME so the
//     two document-level drop handlers stay isolated;
//   - tabs.js renders real groups through sortGroupsByPref;
//   - dashboard.js binds the feature at boot;
//   - keyed Preact group nodes let refresh continue while a drag is in flight.
func TestDashboardJS_GroupReorderWired(t *testing.T) {
	for _, c := range []struct{ needle, why string }{
		// render.js: the group HEADER is the reorder drag handle — draggable
		// and naming its group (escaped).
		{`<summary draggable="true" data-group-reorder="${esc(g.name)}"`,
			"render.js makes each real group's header the escaped, draggable reorder handle"},
		// group-reorder.js: a press on an interactive header child suppresses the
		// drag so the click (fold / edit) lands instead of starting a reorder.
		{`const summary = e.target.closest('summary[data-group-reorder]');`,
			"group-reorder.js suppresses the header drag for clicks on interactive children"},
		// group-reorder.js: the custom MIME and the persisted pref key.
		{`'application/x-tclaude-group'`, "group-reorder.js uses a dedicated drag MIME"},
		{`tclaude.dash.groupOrder`, "group-reorder.js persists the order under its dashPrefs key"},
		{`function sortGroupsByPref(`, "the shared order-applying helper exists"},
		// dnd.js: the explicit isolation guard keeps the two drop handlers apart.
		{`e.dataTransfer.types.includes('application/x-tclaude-group')`,
			"dnd.js's drop handler explicitly ignores a group-reorder drop"},
		// groups-state.js: real groups render in the persisted order through
		// the injectable order helper.
		{`const list = reorder(distributed.groups.slice());`, "Groups state applies the saved group order"},
		// dashboard.js: the feature is bound at boot.
		{`bindGroupReorder()`, "dashboard.js wires the reorder binder at boot"},
	} {
		if !strings.Contains(dashboardAssets, c.needle) {
			t.Errorf("dashboard assets missing %q — %s", c.needle, c.why)
		}
	}
	if strings.Contains(dashboardAssets, `if (groupReorderActive) return true;`) {
		t.Error("refreshSuspended() must not pause auto-refresh during a keyed Preact group reorder")
	}
}
