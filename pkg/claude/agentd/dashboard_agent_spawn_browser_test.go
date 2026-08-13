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

// TestDashboardAgentSpawnChrome is the focused real-browser visual smoke for
// the Preact spawn-dialog owner. Behavioral transitions remain deterministic
// Node tests; this matrix proves their production CSS, both skins, and native
// browser file/focus controls paint coherently in the real dashboard.
func TestDashboardAgentSpawnChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "agent-spawn-"+time.Now().Format("20060102-150405.000"))
	states := agentSpawnBrowserStates()
	if filter := os.Getenv("TCLAUDE_DASHSNAP_FILTER"); filter != "" {
		filtered := states[:0]
		for _, state := range states {
			if strings.Contains(state.Key, filter) {
				filtered = append(filtered, state)
			}
		}
		if len(filtered) == 0 {
			t.Fatalf("TCLAUDE_DASHSNAP_FILTER %q matched no agent-spawn state", filter)
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
		t.Fatalf("agent-spawn browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("agent-spawn browser smoke: %s", filepath.Join(outDir, "index.html"))
}

func agentSpawnBrowserStates() []dashsnap.State {
	return []dashsnap.State{
		agentSpawnResizePersistenceBrowserState(),
		agentSpawnBrowserState("plain-blank", "Plain blank spawn", false, ""),
		agentSpawnBrowserState("wizard-blank", "Wizard blank spawn", true, ""),
		agentSpawnBrowserState("plain-profile", "Plain saved-profile spawn", false, `
var profile = document.querySelector('#agent-spawn-load-profile');
await waitFor(function(){ return profile.options.length > 1; }, 'spawn profiles');
profile.value = 'opus-fast';
profile.dispatchEvent(new Event('change', {bubbles:true}));
await waitFor(function(){ return profile.value === 'opus-fast'; }, 'selected spawn profile');
`),
		agentSpawnBrowserState("wizard-profile", "Wizard saved-pattern spawn", true, `
var profile = document.querySelector('#agent-spawn-load-profile');
await waitFor(function(){ return profile.options.length > 1; }, 'spawn patterns');
profile.value = 'sonnet-review';
profile.dispatchEvent(new Event('change', {bubbles:true}));
await waitFor(function(){ return profile.value === 'sonnet-review'; }, 'selected spawn pattern');
`),
		agentSpawnBrowserState("plain-custom-model", "Plain custom-model spawn", false, `
var model = document.querySelector('#agent-spawn-model');
if (!model) throw new Error('catalog model selector missing');
model.value = '__custom__';
model.dispatchEvent(new Event('change', {bubbles:true}));
await waitFor(function(){ return !document.querySelector('#agent-spawn-model-custom-row').hidden; }, 'custom model row');
var custom = document.querySelector('#agent-spawn-model-custom');
custom.value = 'provider/experimental-42';
custom.dispatchEvent(new InputEvent('input', {bubbles:true, data:'provider/experimental-42'}));
custom.focus();
`),
		// The Codex built-in network gap is disclosure copy: the row carries a
		// warning-coloured [!], and the paragraph appears only once the operator
		// opens it. This state paints both halves — the closed row keeps its
		// height, the opened popover states the caveat in full — because the
		// whole point of moving the copy was that the dialog stays readable.
		agentSpawnBrowserState("plain-codex-sandbox-caveat", "Plain Codex sandbox caveat", false, `
var harness = document.querySelector('#agent-spawn-harness');
harness.value = 'codex';
harness.dispatchEvent(new Event('change', {bubbles:true}));
await waitFor(function(){ return !document.querySelector('#agent-spawn-sandbox-impl-row').hidden; }, 'sandbox row');
var impl = document.querySelector('#agent-spawn-sandbox-impl');
impl.value = 'harness-builtin';
impl.dispatchEvent(new Event('change', {bubbles:true}));
var trigger = document.querySelector('#agent-spawn-sandbox-impl-row .spawn-field-help-trigger');
await waitFor(function(){ return trigger.classList.contains('warn'); }, 'caveat trigger');
if (trigger.textContent !== '!') throw new Error('a caveat must not wear the plain help glyph');
if (document.querySelector('#agent-spawn-sandbox-impl-hint')) throw new Error('the caveat leaked back into the dialog body');
trigger.click();
await waitFor(function(){ return trigger.getAttribute('aria-expanded') === 'true'; }, 'open disclosure');
if (!document.querySelector('#agent-spawn-sandbox-impl-help').textContent.includes('no filtered network sandbox yet')) {
  throw new Error('the opened disclosure does not carry the caveat');
}
document.querySelector('#agent-spawn-sandbox-impl-row').scrollIntoView({block:'center'});
`),
		agentSpawnBrowserState("wizard-validation", "Wizard validation error", true, `
document.querySelector('#agent-spawn-submit').click();
await waitFor(function(){ return document.querySelector('#agent-spawn-error').textContent.includes('name'); }, 'validation error');
`),
		agentSpawnBrowserState("plain-attachment", "Plain attachment preview", false, `
var png = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2n1cAAAAASUVORK5CYII='), function(ch){ return ch.charCodeAt(0); });
var file = new File([png], 'spawn-preview.png', {type:'image/png'});
var transfer = new DataTransfer();
transfer.items.add(file);
var modal = document.querySelector('#agent-spawn-modal');
modal.dispatchEvent(new DragEvent('dragenter', {bubbles:true, cancelable:true, dataTransfer:transfer}));
modal.dispatchEvent(new DragEvent('drop', {bubbles:true, cancelable:true, dataTransfer:transfer}));
await waitFor(function(){ return document.querySelectorAll('#agent-spawn-attachments-list li').length === 1; }, 'attachment preview');
`),
		agentSpawnBrowserState("wizard-busy", "Wizard busy spawn", true, `
// Behavior is exercised with a pending action in the Node suite. Force only
// its controlled paint here so visual smoke never launches a real agent.
var submit = document.querySelector('#agent-spawn-submit');
submit.disabled = true;
submit.setAttribute('aria-busy', 'true');
submit.classList.add('slop-pull-active');
submit.textContent = 'Spawning…';
for (var field of document.querySelectorAll('#agent-spawn-modal input, #agent-spawn-modal textarea, #agent-spawn-modal select, #agent-spawn-modal button')) {
  field.disabled = true;
}
submit.scrollIntoView({block:'end'});
`),
	}
}

func agentSpawnResizePersistenceBrowserState() dashsnap.State {
	state := agentSpawnBrowserState("plain-resize-persistence", "Plain resized spawn reopen", false, "")
	state.Actions = []dashsnap.BrowserAction{
		{Kind: "mouse-down-at", JS: `
var card = document.querySelector('#agent-spawn-modal .cron-create-modal');
var rect = card.getBoundingClientRect();
return {x:rect.right - 2, y:rect.bottom - 2};
`},
		{Kind: "move-by", DX: 160, Steps: 12},
		{Kind: "mouse-up"},
		{Kind: "eval", JS: `
return (async function(){
  var card = document.querySelector('#agent-spawn-modal .cron-create-modal');
  var draggedWidth = card.offsetWidth;
  if (draggedWidth < 750) throw new Error('native resize did not widen the spawn card: ' + draggedWidth);
  var prefs = await import('/static/js/prefs.js');
  var saved = JSON.parse(prefs.dashPrefs.getItem('tclaude.dash.modalSize.agent-spawn'));
  if (!saved || saved.w !== draggedWidth) throw new Error('dragged width was not cached: ' + JSON.stringify(saved));
  if (Object.prototype.hasOwnProperty.call(saved, 'h')) throw new Error('spawn resize persisted height: ' + JSON.stringify(saved));
  document.querySelector('#agent-spawn-cancel').click();
  var deadline = Date.now() + 2000;
  while (document.querySelector('#agent-spawn-modal') && Date.now() < deadline) await new Promise(function(resolve){ setTimeout(resolve, 20); });
  if (document.querySelector('#agent-spawn-modal')) throw new Error('spawn modal did not close');
  var controller = await import('/static/js/agent-spawn-controller.js');
  controller.openAgentSpawnModal({groupName:'frontend-squad'});
  while (!document.querySelector('#agent-spawn-modal') && Date.now() < deadline) await new Promise(function(resolve){ setTimeout(resolve, 20); });
  card = document.querySelector('#agent-spawn-modal .cron-create-modal');
  if (!card) throw new Error('spawn modal did not reopen');
  if (card.offsetWidth !== draggedWidth) throw new Error('reopened width reset: dragged=' + draggedWidth + ' reopened=' + card.offsetWidth);
})();
`},
	}
	return state
}

func agentSpawnBrowserState(key, title string, wizard bool, body string) dashsnap.State {
	return dashsnap.State{
		Key:     "agent-spawn-" + key,
		Title:   title,
		Caption: "TCL-458: production Preact ownership paints the complete controlled spawn form without the retired static modal.",
		Wizard:  wizard,
		JS: `return (async function(){
var waitFor = async function(predicate, label) {
  var deadline = Date.now() + 4000;
  while (!predicate() && Date.now() < deadline) await new Promise(function(resolve){ setTimeout(resolve, 25); });
  if (!predicate()) throw new Error('timed out waiting for ' + label);
};
var controller = await import('/static/js/agent-spawn-controller.js');
controller.openAgentSpawnModal({groupName:'frontend-squad'});
await waitFor(function(){ return document.querySelector('#agent-spawn-modal'); }, 'spawn modal');
await waitFor(function(){ return document.querySelector('#agent-spawn-load-profile').options.length > 0; }, 'spawn form');
await waitFor(function(){ return !document.querySelector('#agent-spawn-worktree').textContent.includes('loading'); }, 'worktree state');
` + body + `
var root = document.querySelector('#agent-spawn-root');
if (!root || root.children.length !== 1) throw new Error('spawn root is not exclusively Preact-owned');
if (document.querySelectorAll('#agent-spawn-modal').length !== 1) throw new Error('spawn modal ownership is not atomic');
})();`,
		SettleMS: 200,
	}
}
