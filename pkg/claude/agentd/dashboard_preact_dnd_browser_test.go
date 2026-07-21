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
// TCL-359/TCL-362. It drives native Chrome input (not synthetic DragEvent
// fixtures) and proves shared snapshot publishes preserve keyed menus, form
// state, disclosures and active drags. It also proves the one retained refresh
// suspension contract protects the transient inline rename editor.
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
		groupMenuPublishState(),
		preactLinkEditorPublishState(),
		inlineRenameSuspensionState(),
		dockMenuPublishState(false),
		dockMenuPublishState(true),
		dockDragCancelState(false),
		dockDragCancelState(true),
		memberDragCancelState(),
		groupDragCancelState(),
		terminalTabNativeDragState(),
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

func groupMenuPublishState() dashsnap.State {
	return dashsnap.State{
		Key:     "preact-group-menu-publish",
		Title:   "Preact group disclosure and menu survive publish",
		Caption: "The same keyed group disclosure, open menu and focused item survive an unsuspended snapshot publish.",
		JS: openGroupsAndDockJS + `
return (async function(){
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var group = document.querySelector('details[data-group-key="frontend-squad"]');
  var cog = group && group.querySelector('.group-actions .cog-btn');
  if (!group || !cog) throw new Error('group menu controls missing');
  cog.click();
  var menu = group.querySelector('.group-actions .action-menu.open');
  var item = menu && menu.querySelector('button');
  if (!menu || !item) throw new Error('group menu did not open');
  item.focus();
  window.__tcl362Group = group;
  window.__tcl362GroupMenu = menu;
  window.__tcl362GroupItem = item;
})();`,
		Actions: []dashsnap.BrowserAction{
			{Kind: "eval", JS: waitForSnapshotPublishJS},
			{Kind: "eval", JS: `
if (document.querySelector('details[data-group-key="frontend-squad"]') !== window.__tcl362Group) throw new Error('publish replaced keyed group disclosure');
if (!window.__tcl362Group.open) throw new Error('publish collapsed group disclosure');
if (!window.__tcl362GroupMenu.classList.contains('open')) throw new Error('publish closed group action menu');
if (document.activeElement !== window.__tcl362GroupItem) throw new Error('publish dropped group menu focus');
`},
			{Kind: "key", Key: "Escape"},
		},
	}
}

func preactLinkEditorPublishState() dashsnap.State {
	return dashsnap.State{
		Key:     "preact-link-editor-publish",
		Title:   "Preact Links editor survives publish",
		Caption: "The stacked Preact editor remains open and retains controlled form values and focus while its management list receives a live snapshot publish.",
		JS: `
return (async function(){
document.querySelector('nav [data-tab="groups"]').click();
document.querySelector('.filter-bar-cog .cog-btn').click();
document.querySelector('#links-manage-open').click();
await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
document.querySelector('#link-new-open').click();
await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
var from = document.querySelector('#link-modal-from');
var to = document.querySelector('#link-modal-to');
var mode = document.querySelector('#link-modal-mode');
var bidir = document.querySelector('#link-modal-bidir');
if (!document.querySelector('#link-modal.show') || !from || !to || !mode || !bidir) throw new Error('link modal did not open');
from.value = 'frontend-squad';
from.dispatchEvent(new Event('change', {bubbles:true}));
to.value = 'infra-crew';
to.dispatchEvent(new Event('change', {bubbles:true}));
mode.value = 'owners->members';
mode.dispatchEvent(new Event('change', {bubbles:true}));
bidir.checked = true;
bidir.dispatchEvent(new Event('change', {bubbles:true}));
mode.focus();
window.__tcl362LinkForm = {from:from, to:to, mode:mode, bidir:bidir};
})();
`,
		Actions: []dashsnap.BrowserAction{
			{Kind: "eval", JS: waitForSnapshotPublishJS},
			{Kind: "eval", JS: `
var form = window.__tcl362LinkForm;
if (!document.querySelector('#link-modal.show')) throw new Error('publish closed Preact link editor');
if (document.querySelector('#link-modal-from') !== form.from || document.querySelector('#link-modal-to') !== form.to || document.querySelector('#link-modal-mode') !== form.mode) throw new Error('publish replaced Preact form controls');
if (form.from.value !== 'frontend-squad' || form.to.value !== 'infra-crew' || form.mode.value !== 'owners->members' || !form.bidir.checked) throw new Error('publish changed Preact form state: ' + JSON.stringify({from:form.from.value,to:form.to.value,mode:form.mode.value,bidir:form.bidir.checked}));
if (document.activeElement !== form.mode) throw new Error('publish dropped Preact editor focus');
document.querySelector('#link-modal-cancel').click();
`},
		},
	}
}

func inlineRenameSuspensionState() dashsnap.State {
	return dashsnap.State{
		Key:     "inline-rename-suspends-publish",
		Title:   "Inline rename is the sole refresh suspension",
		Caption: "A transient sibling editor blocks the next scheduled snapshot request, preserves value/selection/focus, then allows polling to resume after Escape.",
		JS: openGroupsAndDockJS + `
return new Promise(function(resolve, reject) {
  var timeout = setTimeout(function(){ reject(new Error('initial snapshot publish did not arrive')); }, 5000);
  document.addEventListener('tclaude:snapshot', function opened() {
    clearTimeout(timeout);
    var chip = document.querySelector('details[data-group-key="frontend-squad"] .rowname-text[data-act="rename-name"]');
    if (!chip) { reject(new Error('rename chip missing')); return; }
    chip.click();
    var input = chip.parentElement.querySelector('.rowname-input');
    if (!input) { reject(new Error('inline rename editor did not open')); return; }
    input.value = 'rename stays local';
    input.setSelectionRange(2, 9);
    var before = performance.getEntriesByType('resource').filter(function(e){ return e.name.indexOf('/api/snapshot') >= 0; }).length;
    setTimeout(function() {
      var after = performance.getEntriesByType('resource').filter(function(e){ return e.name.indexOf('/api/snapshot') >= 0; }).length;
      if (after !== before) { reject(new Error('snapshot request started while inline editor was open')); return; }
      if (!input.isConnected || input.value !== 'rename stays local' || input.selectionStart !== 2 || input.selectionEnd !== 9 || document.activeElement !== input) {
        reject(new Error('inline editor state changed while refresh was suspended')); return;
      }
      var resumed = setTimeout(function(){ reject(new Error('snapshot polling did not resume after inline editor closed')); }, 5000);
      document.addEventListener('tclaude:snapshot', function complete() { clearTimeout(resumed); resolve(); }, {once:true});
      input.dispatchEvent(new KeyboardEvent('keydown', {key:'Escape', bubbles:true}));
    }, 2300);
  }, {once:true});
});`,
	}
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

// terminalTabNativeDragState uses Chrome's input domain for the whole gesture:
// no synthetic DragEvent or hand-built DataTransfer participates. The terminal
// sockets intentionally point at missing routes; this smoke owns tab chrome and
// keyed pane identity, while the live terminal smoke covers PTY connections.
func terminalTabNativeDragState() dashsnap.State {
	return dashsnap.State{
		Key:     "terminal-tab-native-drag",
		Title:   "Terminal tabs: native drag reorder",
		Caption: "Real Chrome drags the first terminal tab past the second; the active key and keyed pane nodes survive, and transient drag chrome clears.",
		JS: `
return (async function(){
  var terminals = await import('/static/js/terminals-tab.js');
  await terminals.openTerminalPane({ws:'/testhook/missing-terminal-one', key:'native-dnd-one', label:'one'}, {reveal:false});
  await terminals.openTerminalPane({ws:'/testhook/missing-terminal-two', key:'native-dnd-two', label:'two'}, {reveal:false});
  document.querySelector('nav [data-tab="terminals"]').click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var tabs = document.querySelectorAll('.mux-tab');
  if (tabs.length !== 2) throw new Error('terminal drag fixture did not open two tabs');
  window.__terminalDnDSource = tabs[0];
  window.__terminalDnDTarget = tabs[1];
  window.__terminalDnDEvents = [];
  ['dragstart','dragover','drop','dragend'].forEach(function(type){
    document.addEventListener(type, function(event){
      window.__terminalDnDEvents.push(type + ':' + event.target.tagName + '.' + event.target.className);
    }, {capture:true});
  });
  window.__terminalDnDPanes = {};
  document.querySelectorAll('.mux-pane').forEach(function(pane){ window.__terminalDnDPanes[pane.id] = pane; });
})();`,
		Actions: []dashsnap.BrowserAction{
			{Kind: "mouse-down", Selector: `.mux-tab:first-child .mux-tab-label`},
			{Kind: "move-to-at", Steps: 12, JS: `
var rect = window.__terminalDnDTarget.getBoundingClientRect();
var y = rect.top + rect.height / 2;
for (var x = rect.left + rect.width * 0.55; x < rect.right - 3; x += 2) {
  var hit = document.elementFromPoint(x, y);
  if (hit && hit.closest('.mux-tab') === window.__terminalDnDTarget && !hit.closest('button')) return {x:x, y:y};
}
throw new Error('terminal target has no non-button point in its right half');
`},
			{Kind: "eval", JS: `
if (!window.__terminalDnDSource.classList.contains('dragging')) throw new Error('Chrome did not start the terminal-tab native drag');
if (!window.__terminalDnDTarget.classList.contains('drop-after')) throw new Error('native drag did not show the right-edge insertion marker');
`},
			{Kind: "mouse-up"},
			{Kind: "eval", JS: `
return (async function(){
  var labels = Array.from(document.querySelectorAll('.mux-tab-label')).map(function(label){ return label.textContent; });
  if (labels.join(',') !== 'two,one') throw new Error('native drop order: ' + labels.join(',') + '; events=' + JSON.stringify(window.__terminalDnDEvents));
  var active = document.querySelector('.mux-tab[aria-selected="true"] .mux-tab-label');
  if (!active || active.textContent !== 'two') throw new Error('native reorder changed the active terminal');
  document.querySelectorAll('.mux-pane').forEach(function(pane){
    if (window.__terminalDnDPanes[pane.id] !== pane) throw new Error('native reorder replaced keyed pane ' + pane.id);
  });
  if (document.querySelector('.mux-tab.dragging, .mux-tab.drop-before, .mux-tab.drop-after')) throw new Error('native drop left transient drag chrome');
  for (;;) {
    var close = document.querySelector('.mux-tab-close');
    if (!close) break;
    close.click();
    await new Promise(function(resolve){ requestAnimationFrame(resolve); });
  }
})();`},
		},
	}
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
