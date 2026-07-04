package agentd

import (
	"strings"
	"testing"
)

// TestDashboardJS_DragToRetireBinWired guards the drag-to-retire bin
// (#dnd-trash) — a fixed overlay on the right edge that lets you retire an
// agent by dropping its row on it, instead of dragging all the way down to
// the possibly-offscreen virtual "Retired" group.
//
// It is a pure-frontend feature with no server code path a flow test could
// exercise (a drop reuses the existing retire endpoint via runDndRetire), so
// — like the other dashboard-asset guards in this package — it string-searches
// the embedded HTML/CSS/JS to pin the load-bearing wiring against a silent
// refactor break.
//
// The pieces that must stay connected:
//   - dashboard.html carries the #dnd-trash element tagged
//     data-dnd-target-retired, so it routes through the exact same retire path
//     as the virtual Retired group (dnd.js keys off that attribute);
//   - dnd.js lists #dnd-trash in the shared drop-target selector, so every
//     hover/drop handler sees it as a target;
//   - dnd.js reveals the bin on dragstart only for a retireable source and
//     hides it on dragend;
//   - dashboard.css styles the bin and its armed (drop-over) state.
func TestDashboardJS_DragToRetireBinWired(t *testing.T) {
	for _, c := range []struct{ needle, why string }{
		// dashboard.html: the bin element, tagged as a retire drop target so
		// dnd.js's data-dnd-target-retired-keyed drop handler runs runDndRetire.
		{`id="dnd-trash" data-dnd-target-retired="1"`,
			"dashboard.html declares the bin as a retire drop target"},
		// dnd.js: the bin is part of the shared drop-target selector, or no
		// handler would ever recognise a drop on it.
		{`,#dnd-trash'`,
			"dnd.js includes #dnd-trash in DND_TARGET_SEL"},
		// dnd.js: show on dragstart (gated on a retireable source) + hide on dragend.
		{`showDndTrash(!sourceRetired && !sourceConversation)`,
			"dnd.js reveals the bin on dragstart only for a retireable source"},
		{`function showDndTrash(`, "dnd.js defines the bin show helper"},
		{`function hideDndTrash(`, "dnd.js defines the bin hide helper"},
		// dashboard.css: the bin and its armed state are styled.
		{`#dnd-trash {`, "dashboard.css styles the bin"},
		{`#dnd-trash.dnd-drop-over {`, "dashboard.css styles the armed (drop-over) bin"},
	} {
		if !strings.Contains(dashboardAssets, c.needle) {
			t.Errorf("dashboard assets missing %q — %s", c.needle, c.why)
		}
	}

	// The bin must be hidden on dragend. Assert hideDndTrash() is called from
	// the dragend handler (between the dragend listener and the dragover one),
	// so a cancelled or missed drop can never strand the bin on screen.
	dragend := strings.Index(dashboardAssets, "addEventListener('dragend'")
	dragover := strings.Index(dashboardAssets, "addEventListener('dragover'")
	hide := strings.Index(dashboardAssets, "hideDndTrash();")
	if dragend < 0 || dragover < 0 || hide < 0 {
		t.Fatalf("dashboard assets: dragend=%d dragover=%d hideDndTrash=%d — expected all present", dragend, dragover, hide)
	}
	if dragend >= hide || hide >= dragover {
		t.Errorf("hideDndTrash() (at %d) must be called inside the dragend handler (between %d and %d) so a drag-end always hides the bin", hide, dragend, dragover)
	}
}
