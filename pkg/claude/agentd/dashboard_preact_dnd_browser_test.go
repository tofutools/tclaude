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
// state, disclosures and active drags.
//
// It is a MANUALLY run smoke (env-gated, never in CI), so a state that drifts
// away from the dashboard it covers goes unnoticed and rots. TCL-1162 retired
// five such states that had drifted against the Preact migration's asynchronous
// renders rather than repairing assertions no one was running; what remains is
// what still earns its keep, including the macOS group-clone regression state.
// Prefer retiring a stale state here over teaching it to wait.
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
		preactLinkEditorPublishState(),
		dockMenuPublishState(),
		dockDragCancelState(),
		memberDragCancelState(),
		groupDragCancelState(),
		groupCloneModifierDropState(),
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

const openGroupsAndDockJS = `
document.querySelector('nav [data-tab="groups"]').click();
document.querySelectorAll('details[data-dnd-target-group]').forEach(function(d){ d.open = true; });
document.body.classList.add('dock-open');
`

// waitForSnapshotPublishJS waits for the next scheduled snapshot publish, then
// two animation frames for the Signals-driven Preact render to flush.
//
// It listens for refresh.js's `tclaude:snapshot` rather than polling
// performance.getEntriesByType('resource') for /api/snapshot: the dashboard
// loads well over 200 ES modules, so the document's resource timing buffer
// (250 entries by default, and NOT resizable from a state's JS — it runs long
// after load) is already full by the time a state starts. Once full, no further
// resource entry is recorded and the poll could never observe a snapshot fetch,
// whatever the page actually did (TCL-1155). The event is also the more precise
// signal: these states assert what survives a PUBLISH, not a request.
const waitForSnapshotPublishJS = `
return (async function(){
  await new Promise(function(resolve, reject){
    var timer = setTimeout(function(){
      document.removeEventListener('tclaude:snapshot', published);
      reject(new Error('scheduled snapshot publish did not arrive'));
    }, 5000);
    function published(){
      clearTimeout(timer);
      document.removeEventListener('tclaude:snapshot', published);
      resolve();
    }
    document.addEventListener('tclaude:snapshot', published);
  });
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
})();
`

func dockMenuPublishState() dashsnap.State {
	return dashsnap.State{
		Key:     "preact-dock-menu-publish",
		Title:   "Preact dock menu survives publish",
		Caption: "The same keyed dock card and focused menu item survive a scheduled snapshot publish; Escape restores focus to the cog.",
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

func dockDragCancelState() dashsnap.State {
	return dragCancelState("preact-dock-drag-cancel", "Preact dock drag cancellation",
		`.dock-card[draggable="true"][data-dock-kind="profiles"]`,
		`.dock-card.dock-drag-source`, 0, 80, `
if (document.querySelector('.dock-drag-source')) throw new Error('cancel left dock source dimmed');
if (document.querySelector('.dock-drop-over')) throw new Error('cancel left dock target highlighted');
`)
}

func memberDragCancelState() dashsnap.State {
	return dragCancelState("preact-member-drag-cancel", "Preact member drag cancellation",
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
	return dragCancelState("preact-group-drag-cancel", "Preact group reorder cancellation",
		`summary[data-group-reorder="frontend-squad"]`, `details.group-reorder-source`, 80, 0, `
if (document.querySelector('.group-reorder-source')) throw new Error('cancel left group source highlighted');
if (document.querySelector('.group-drop-before, .group-drop-after, .group-drop-into')) throw new Error('cancel left reorder target highlighted');
`)
}

// groupCloneModifierDropState reproduces macOS Chrome showing a valid clone
// dragover but reaching dragend without a usable drop event or dropEffect. The
// still-live green plan must open the clone dialog, while Escape/document-leave
// cancellation explicitly invalidates that plan first.
func groupCloneModifierDropState() dashsnap.State {
	return dashsnap.State{
		Key:     "preact-group-clone-modifier-drop",
		Title:   "Group clone survives a missing drop event",
		Caption: "A green Cmd-drag clone target still opens the clone dialog when macOS Chrome reaches dragend with neither a usable drop event nor copy dropEffect.",
		JS: openGroupsAndDockJS + `
return (async function(){
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var source = document.querySelector('summary[data-group-reorder="frontend-squad"]');
  var target = document.querySelector('details[data-group-key="infra-crew"]');
  if (!source || !target) throw new Error('group clone drag fixture missing');
  var transfer = new DataTransfer();
  function fire(element, type, options) {
    var event = new DragEvent(type, Object.assign({bubbles:true, cancelable:true, dataTransfer:transfer}, options || {}));
    if (event.dataTransfer !== transfer) Object.defineProperty(event, 'dataTransfer', {value:transfer});
    element.dispatchEvent(event);
  }
  fire(source, 'dragstart');
  var rect = target.getBoundingClientRect();
  fire(target, 'dragover', {metaKey:true, clientX:rect.left + rect.width / 2, clientY:rect.top + rect.height / 2});
  if (!target.classList.contains('group-drop-clone')) throw new Error('Cmd dragover did not paint clone intent');
  // A copy completed outside the document must not consume the cached internal
  // placement. The null relatedTarget is the native leave-document signal.
  fire(target, 'dragleave', {relatedTarget:null, clientX:0, clientY:0});
  var outsideEnd = new DragEvent('dragend', {bubbles:true, cancelable:false, dataTransfer:transfer});
  Object.defineProperty(outsideEnd, 'dataTransfer', {value:{dropEffect:'copy'}});
  source.dispatchEvent(outsideEnd);
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (document.querySelector('#group-clone-modal.show')) throw new Error('outside copy dragend consumed a stale clone plan');

  fire(source, 'dragstart');
  fire(target, 'dragover', {metaKey:true, clientX:rect.left + rect.width / 2, clientY:rect.top + rect.height / 2});
  document.dispatchEvent(new KeyboardEvent('keydown', {key:'Escape', bubbles:true}));
  var cancelEnd = new DragEvent('dragend', {bubbles:true, cancelable:false, dataTransfer:transfer});
  Object.defineProperty(cancelEnd, 'dataTransfer', {value:{dropEffect:'none'}});
  source.dispatchEvent(cancelEnd);
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (document.querySelector('#group-clone-modal.show')) throw new Error('Escape dragend consumed a cancelled clone plan');

  var platformDescriptor = Object.getOwnPropertyDescriptor(navigator, 'platform');
  var userAgentDescriptor = Object.getOwnPropertyDescriptor(navigator, 'userAgent');
  var originalUserAgent = navigator.userAgent;
  Object.defineProperty(navigator, 'platform', {value:'MacIntel', configurable:true});
  Object.defineProperty(navigator, 'userAgent', {value:
    'Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0', configurable:true});
  fire(source, 'dragstart');
  fire(target, 'dragover', {metaKey:true, clientX:rect.left + rect.width / 2, clientY:rect.top + rect.height / 2});
  fire(target, 'dragleave', {relatedTarget:null, clientX:0, clientY:0, screenX:0, screenY:0});
  var edgeEnd = new DragEvent('dragend', {bubbles:true, cancelable:false, dataTransfer:transfer});
  Object.defineProperty(edgeEnd, 'dataTransfer', {value:{dropEffect:'none'}});
  source.dispatchEvent(edgeEnd);
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (document.querySelector('#group-clone-modal.show')) throw new Error('macOS Edge zero-event bypassed ordinary exit cleanup');

  Object.defineProperty(navigator, 'userAgent', {value:originalUserAgent, configurable:true});
  fire(source, 'dragstart');
  fire(target, 'dragover', {metaKey:true, clientX:rect.left + rect.width / 2, clientY:rect.top + rect.height / 2});
  // Emulate macOS Chrome for its all-zero mouse-release dragleave. Linux Chrome
  // used by this smoke must keep the ordinary outside-document behavior above.
  fire(target, 'dragleave', {relatedTarget:null, clientX:0, clientY:0, screenX:0, screenY:0});
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (!document.querySelector('#group-clone-modal.show')) throw new Error('macOS zero-event did not immediately finish the green clone plan');
  var dragend = new DragEvent('dragend', {bubbles:true, cancelable:false, dataTransfer:transfer});
  Object.defineProperty(dragend, 'dataTransfer', {value:{dropEffect:'none'}});
  source.dispatchEvent(dragend);
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (document.querySelectorAll('#group-clone-modal.show').length !== 1) throw new Error('delayed dragend duplicated the clone dialog');
  if (platformDescriptor) Object.defineProperty(navigator, 'platform', platformDescriptor);
  else delete navigator.platform;
  if (userAgentDescriptor) Object.defineProperty(navigator, 'userAgent', userAgentDescriptor);
  else delete navigator.userAgent;
  document.querySelector('#group-clone-cancel').click();
})();`,
	}
}

func dragCancelState(key, title string, selector, activeSelector string, dx, dy float64, cleanupChecks string) dashsnap.State {
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
	if strings.Contains(key, "member-drag") {
		// dnd.js turns the row's draggable OFF for any gesture begun over an
		// in-row control, so a press on the row's centre (a cwd link, as the
		// columns are laid out today) is a click, not a drag — by design. Scan
		// the row for a point that belongs to no such control, mirroring that
		// suppression list, so the smoke presses a real drag handle whatever the
		// visible columns happen to be.
		mouseDown = dashsnap.BrowserAction{Kind: "mouse-down-at", JS: `
var row = document.querySelector(` + "`" + selector + "`" + `);
var rect = row.getBoundingClientRect();
var y = rect.top + rect.height / 2;
for (var x = rect.left + 4; x < rect.right - 4; x += 4) {
  var hit = document.elementFromPoint(x, y);
  if (hit && row.contains(hit) && !hit.closest('button, a, input, select, textarea, label, [data-act], [contenteditable]')) return {x:x, y:y};
}
throw new Error('member row has no bare drag handle point');
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
