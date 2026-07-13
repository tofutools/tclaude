package agentd

import (
	"io/fs"
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
		// dnd.js: show on dragstart (gated on a retireable source — or a
		// pending spawn, whose only valid target IS the bin) + hide on dragend.
		{`showDndTrash(sourcePending || (!sourceRetired && !sourceConversation))`,
			"dnd.js reveals the bin on dragstart for a retireable source or a pending spawn"},
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

	// The shared terminal handler is registered on both document and the source
	// row, so it still hides the bin when a structural Preact render detached the
	// row before dragend could bubble.
	b, err := fs.ReadFile(dashboardAssetsFS, "js/dnd.js")
	if err != nil {
		t.Fatalf("read dnd.js: %v", err)
	}
	dnd := string(b)
	start := strings.Index(dnd, "const endDndDrag = (e) => {")
	end := strings.Index(dnd, "  listen(document, 'dragstart'")
	if start < 0 || end <= start {
		t.Fatalf("dnd.js terminal handler bounds: start=%d end=%d", start, end)
	}
	terminal := dnd[start:end]
	if !strings.Contains(terminal, "hideDndTrash();") {
		t.Error("endDndDrag must hide the retire bin on every terminal path")
	}
	if !strings.Contains(dnd, "row.addEventListener('dragend', endDndDrag, { once: true });") {
		t.Error("member source must own a terminal listener for detached-node cleanup")
	}
}

// TestDashboardJS_PendingDeleteWired guards the pending-spawn cleanup
// escape hatch — the per-row 🗑 delete button and the drag-to-trash gesture,
// both of which discard a spawn wedged behind a startup gate it will never
// clear (POST /api/pending/delete/{label}).
//
// A pending spawn is not an agent (no conv-id / group / permissions), so it
// can't take the conv-keyed retire path; this is its own dedicated wiring.
// Like the other dashboard-asset guards, it string-searches the embedded JS
// to pin the load-bearing pieces against a silent refactor break.
func TestDashboardJS_PendingDeleteWired(t *testing.T) {
	for _, c := range []struct{ needle, why string }{
		// render.js: the per-row button + the draggable pending row tagged so
		// dnd.js recognises it as a pending source.
		{`data-act="delete-pending"`,
			"render.js emits the per-row 🗑 delete button"},
		{`data-dnd-pending="1"`,
			"render.js tags the pending row as a draggable pending source"},
		// row-actions.js: the button handler POSTs to the delete endpoint.
		{`case 'delete-pending':`,
			"row-actions.js handles the delete-pending click"},
		{"`/api/pending/delete/${encodeURIComponent(label)}`",
			"row-actions.js POSTs to the pending-delete endpoint"},
		// dnd.js: the pending source flag, its trash-only routing, and the
		// delete handler the drop invokes.
		{`row.hasAttribute('data-dnd-pending')`,
			"dnd.js reads the pending source flag on dragstart"},
		{`if (dndSourcePending) return !box.hasAttribute('data-dnd-target-retired');`,
			"dnd.js makes a pending source inert over every target but the trash"},
		{`await runDndDeletePending(payload);`,
			"dnd.js routes a pending drop to the delete handler"},
		{`async function runDndDeletePending(`,
			"dnd.js defines the pending-delete drop handler"},
	} {
		if !strings.Contains(dashboardAssets, c.needle) {
			t.Errorf("dashboard assets missing %q — %s", c.needle, c.why)
		}
	}
}

// TestDashboardJS_DragToRetireBinWizardSkin guards the 🧙 wizard-mode
// re-skin of the bin: retiring an agent is "banishing a familiar", so the
// trashcan becomes a swirling banishment portal with a "Banish" label. Like
// the other per-theme label swaps (Summon / Awaken / Slumber), both voices
// are always emitted and CSS picks one per theme — so a wizard-mode user
// must never see the bare "Retire"/trashcan chrome.
func TestDashboardJS_DragToRetireBinWizardSkin(t *testing.T) {
	for _, c := range []struct{ needle, why string }{
		// dashboard.html: both label voices emitted + the portal glyph.
		{`<span class="dnd-trash-label-regular">Retire</span><span class="dnd-trash-label-wizard">Banish</span>`,
			"the bin emits both the regular (Retire) and wizard (Banish) label voices"},
		{`class="dnd-trash-glyph-wizard"`,
			"the bin carries the wizard-mode portal glyph element"},
		// dashboard.css: the wizard theme swaps the label + glyph and skins the box.
		{`body.wizard .dnd-trash-label-regular { display: none; }`,
			"wizard mode hides the regular Retire label"},
		{`body.wizard .dnd-trash-label-wizard { display: inline; }`,
			"wizard mode shows the Banish label"},
		{`body.wizard .dnd-trash-icon { display: none; }`,
			"wizard mode hides the mundane trashcan SVG"},
		{`body.wizard .dnd-trash-glyph-wizard {`,
			"wizard mode reveals + styles the banishment portal glyph"},
		{`body.wizard #dnd-trash {`,
			"wizard mode skins the bin box (arcane chrome)"},
		{`body.wizard #dnd-trash.dnd-drop-over {`,
			"wizard mode skins the armed (drop-over) bin (crimson flare)"},
	} {
		if !strings.Contains(dashboardAssets, c.needle) {
			t.Errorf("dashboard assets missing %q — %s", c.needle, c.why)
		}
	}
}
