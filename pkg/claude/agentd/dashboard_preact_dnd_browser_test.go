package agentd_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
)

// TestDashboardPreactDnDChrome is the focused real-browser acceptance check for
// TCL-359. It drives native Chrome input (not synthetic DragEvent fixtures) and
// proves a shared snapshot publish neither replaces an active drag source nor
// closes/focus-drops a dock menu. Escape then exercises the browser's cancelled
// HTML5-drag path and every imperative binder's cleanup.
func TestDashboardPreactDnDChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "preact-dnd-"+time.Now().Format("20060102-150405.000"))
	states := []dashsnap.State{
		dockMenuPublishState(false),
		dockMenuPublishState(true),
		dockDragCancelState(false),
		dockDragCancelState(true),
		memberDragCancelState(),
		groupDragCancelState(),
	}
	if filter := os.Getenv("TCLAUDE_DASHSNAP_FILTER"); filter != "" {
		filtered := states[:0]
		for _, state := range states {
			if strings.Contains(state.Key, filter) {
				filtered = append(filtered, state)
			}
		}
		if len(filtered) == 0 {
			t.Fatalf("TCLAUDE_DASHSNAP_FILTER %q matched no focused DnD state", filter)
		}
		states = filtered
	}
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		States:  states,
	})
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}
	var failed []string
	for _, shot := range shots {
		if shot.Err != "" {
			failed = append(failed, shot.State.Key+": "+shot.Err)
		}
	}
	if len(failed) != 0 {
		t.Fatalf("Preact DnD browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("Preact DnD browser smoke: %s", filepath.Join(outDir, "index.html"))
}

const openGroupsAndDockJS = `
document.querySelector('nav [data-tab="groups"]').click();
document.querySelectorAll('details[data-dnd-target-group]').forEach(function(d){ d.open = true; });
document.body.classList.add('dock-open');
`

// waitForSnapshotPublishJS waits for the next scheduled /api/snapshot request,
// then two animation frames for the Signals-driven Preact render to flush.
const waitForSnapshotPublishJS = `
return (async function(){
  var before = performance.getEntriesByType('resource').filter(function(e){ return e.name.indexOf('/api/snapshot') >= 0; }).length;
  var deadline = Date.now() + 5000;
  while (performance.getEntriesByType('resource').filter(function(e){ return e.name.indexOf('/api/snapshot') >= 0; }).length <= before && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 80); });
  }
  if (performance.getEntriesByType('resource').filter(function(e){ return e.name.indexOf('/api/snapshot') >= 0; }).length <= before) throw new Error('scheduled snapshot publish did not arrive');
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
})();
`

func dockMenuPublishState(wizard bool) dashsnap.State {
	key := "preact-dock-menu-publish"
	if wizard {
		key = "wizard-" + key
	}
	return dashsnap.State{
		Key:     key,
		Title:   "Preact dock menu survives publish",
		Caption: "The same keyed dock card and focused menu item survive a scheduled snapshot publish; Escape restores focus to the cog.",
		Wizard:  wizard,
		JS: openGroupsAndDockJS + `
return (async function(){
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var card = document.querySelector('.dock-card[data-dock-kind="profiles"]');
  if (!card) throw new Error('profile dock card missing');
  var cog = card.querySelector('.dock-card-manage');
  cog.click();
  await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  var item = card.querySelector('.dock-card-menu-item[data-dock-act="edit-item"]');
  if (!item || !card.querySelector('.dock-card-menu.open')) throw new Error('dock card menu did not open');
  item.focus();
  window.__tcl359Card = card;
  window.__tcl359Item = item;
})();`,
		Actions: []dashsnap.BrowserAction{
			{Kind: "eval", JS: waitForSnapshotPublishJS},
			{Kind: "eval", JS: `
if (document.querySelector('.dock-card[data-dock-kind="profiles"]') !== window.__tcl359Card) throw new Error('snapshot publish replaced keyed dock card');
if (!window.__tcl359Card.querySelector('.dock-card-menu.open')) throw new Error('snapshot publish closed dock menu');
if (document.activeElement !== window.__tcl359Item) throw new Error('snapshot publish dropped menu focus');
`},
			{Kind: "key", Key: "Escape"},
			{Kind: "eval", JS: `
if (window.__tcl359Card.querySelector('.dock-card-menu.open')) throw new Error('Escape did not close dock menu');
if (document.activeElement !== window.__tcl359Card.querySelector('.dock-card-manage')) throw new Error('Escape did not restore cog focus');
`},
		},
	}
}

func dockDragCancelState(wizard bool) dashsnap.State {
	key := "preact-dock-drag-cancel"
	if wizard {
		key = "wizard-" + key
	}
	return dragCancelState(key, "Preact dock drag cancellation", wizard,
		`.dock-card[draggable="true"][data-dock-kind="profiles"]`,
		`.dock-card.dock-drag-source`, 0, 80, `
if (document.querySelector('.dock-drag-source')) throw new Error('cancel left dock source dimmed');
if (document.querySelector('.dock-drop-over')) throw new Error('cancel left dock target highlighted');
`)
}

func memberDragCancelState() dashsnap.State {
	return dragCancelState("preact-member-drag-cancel", "Preact member drag cancellation", false,
		`.dnd-draggable[data-dnd-conv="f1000000-0000-4000-8000-000000000001"]`,
		`.dnd-draggable.dnd-source-row`, 80, 0, `
if (document.querySelector('.dnd-source-row')) throw new Error('cancel left member source highlighted');
if (document.querySelector('.dnd-drop-over')) throw new Error('cancel left member target highlighted');
if (document.querySelector('#dnd-trash.show')) throw new Error('cancel left retire bin visible');
var filter = document.querySelector('#filter-groups');
filter.value = '';
filter.dispatchEvent(new Event('input', {bubbles:true}));
`)
}

func groupDragCancelState() dashsnap.State {
	return dragCancelState("preact-group-drag-cancel", "Preact group reorder cancellation", false,
		`summary[data-group-reorder="frontend-squad"]`, `details.group-reorder-source`, 80, 0, `
if (document.querySelector('.group-reorder-source')) throw new Error('cancel left group source highlighted');
if (document.querySelector('.group-drop-before, .group-drop-after, .group-drop-into')) throw new Error('cancel left reorder target highlighted');
`)
}

func dragCancelState(key, title string, wizard bool, selector, activeSelector string, dx, dy float64, cleanupChecks string) dashsnap.State {
	mouseDown := dashsnap.BrowserAction{Kind: "mouse-down", Selector: selector}
	if strings.Contains(key, "group-drag") {
		// The group summary intentionally suppresses drags begun over its many
		// interactive chips. Scan its live box for a point whose hit target is the
		// bare summary itself, then press that deterministic native drag handle.
		mouseDown = dashsnap.BrowserAction{Kind: "mouse-down-at", JS: `
var summary = document.querySelector('summary[data-group-reorder="frontend-squad"]');
var rect = summary.getBoundingClientRect();
for (var y = rect.top + 4; y < rect.bottom - 4; y += 4) {
  for (var x = rect.right - 4; x > rect.left + 4; x -= 4) {
    if (document.elementFromPoint(x, y) === summary) return {x:x, y:y};
  }
}
throw new Error('group summary has no bare reorder handle point');
`}
	}
	actions := []dashsnap.BrowserAction{
		mouseDown,
		{Kind: "move-by", DX: dx, DY: dy, Steps: 12},
		{Kind: "eval", JS: waitForSnapshotPublishJS},
		{Kind: "eval", JS: `
if (!window.__tcl359DragSource.isConnected) throw new Error('unchanged snapshot publish detached active drag source');
if (!document.querySelector(` + "`" + activeSelector + "`" + `)) throw new Error('Chrome did not start the native drag; events=' + JSON.stringify(window.__tcl359DragEvents) + '; draggable=' + window.__tcl359DragSource.draggable);
`},
	}
	caption := "Real Chrome native drag held across a scheduled snapshot publish, then released on an inert target; source identity and cancellation cleanup are self-checked."
	if strings.Contains(key, "member-drag") {
		caption = "Real Chrome member drag survives a publish, then a Preact filter structurally removes its source before release; source-local cancellation cleanup is self-checked."
		actions = append(actions, dashsnap.BrowserAction{Kind: "eval", JS: `
return (async function(){
  var filter = document.querySelector('#filter-groups');
  filter.value = 'tcl359-no-such-group-or-member';
  filter.dispatchEvent(new Event('input', {bubbles:true}));
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (window.__tcl359DragSource.isConnected) throw new Error('structural filter did not detach member drag source');
})();
`})
	}
	actions = append(actions,
		dashsnap.BrowserAction{Kind: "mouse-up"},
		dashsnap.BrowserAction{Kind: "eval", JS: cleanupChecks + `
if (window.__tcl359DragSource.classList.contains('dnd-source-row') || window.__tcl359DragSource.classList.contains('dock-drag-source')) throw new Error('detached source missed terminal cleanup');
var pill = document.querySelector('#dnd-pill');
if (pill && pill.classList.contains('show')) throw new Error('cancel left drag pill visible');
if (document.querySelector('.modal-overlay.show')) throw new Error('cancelled drag opened a modal');
`},
	)
	return dashsnap.State{
		Key:     key,
		Title:   title,
		Caption: caption,
		Wizard:  wizard,
		JS: openGroupsAndDockJS + `
var source = document.querySelector(` + "`" + selector + "`" + `);
if (!source) throw new Error('native drag source missing');
window.__tcl359DragSource = source;
window.__tcl359DragEvents = [];
document.addEventListener('pointerdown', function(e){ window.__tcl359DragEvents.push('pointerdown:' + e.target.tagName + '.' + e.target.className); }, {once:true, capture:true});
document.addEventListener('dragstart', function(e){ window.__tcl359DragEvents.push('dragstart:' + e.target.tagName + '.' + e.target.className); }, {once:true, capture:true});
`,
		Actions: actions,
	}
}
