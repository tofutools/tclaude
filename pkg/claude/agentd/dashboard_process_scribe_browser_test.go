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

// TestDashboardProcessScribeFocusChrome complements the deterministic Preact
// interaction suite with Chrome's native inert/focus timing. Both skins mount
// the same ProcessEditorApp/ScribeDialog DOM; the two states prove that the
// shared implementation blurs the editor, focuses the preview, and removes
// inertness before restoring the invoking control.
func TestDashboardProcessScribeFocusChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke - set TCLAUDE_DASHSNAP=1 (needs local Chrome/Chromium)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "process-scribe-focus-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		States: []dashsnap.State{
			processScribeFocusState(false),
			processScribeFocusState(true),
		},
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
		t.Fatalf("process-scribe focus browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("process-scribe focus browser smoke: %s", filepath.Join(outDir, "index.html"))
}

func processScribeFocusState(wizard bool) dashsnap.State {
	skin := "plain"
	if wizard {
		skin = "wizard"
	}
	return dashsnap.State{
		Key:     "process-scribe-native-focus-" + skin,
		Title:   "Process scribe native focus - " + skin,
		Caption: "Chrome verifies the shared preview DOM applies inertness before modal focus and removes it before invoker restoration.",
		Wizard:  wizard,
		JS: processEditorStateJS(`
  var invoker = document.querySelector('.process-scribe-action');
  var editorRoot = document.querySelector('.process-editor');
  if (!invoker || !editorRoot) throw new Error('scribe focus fixture is incomplete');
  window.__scribeFocusEvents = [];
  window.__scribeFocusRecord = function(event) {
    var label = event.target === invoker ? 'invoker' :
      event.target && event.target.classList.contains('process-scribe-prompt') ? 'preview' : 'other';
    window.__scribeFocusEvents.push({type:event.type, label:label, inert:editorRoot.inert});
  };
  document.addEventListener('focus', window.__scribeFocusRecord, true);
  document.addEventListener('blur', window.__scribeFocusRecord, true);
  invoker.focus();
  if (document.activeElement !== invoker) throw new Error('editor invoker did not accept focus');
  window.__scribeFocusInvoker = invoker;
  window.__scribeFocusEditor = editorRoot;
  window.__scribeFocusPending = ed.scribePreviewModal({
    kind:'template', prompt:'Review native focus timing.', context:'{"templateId":"release-train"}'
  });
  await editorPaint();
  var overlay = document.querySelector('.process-scribe-preview-overlay');
  var prompt = overlay && overlay.querySelector('.process-scribe-prompt');
  if (!overlay || !prompt) throw new Error('scribe preview did not open');
  if (!editorRoot.inert || !editorRoot.hasAttribute('inert')) throw new Error('editor is not natively inert behind preview');
  if (document.activeElement !== prompt) throw new Error('preview prompt did not receive native focus');
  var events = window.__scribeFocusEvents;
  var blurIndex = events.findIndex(function(event){ return event.type === 'blur' && event.label === 'invoker'; });
  var previewIndex = events.findIndex(function(event){ return event.type === 'focus' && event.label === 'preview'; });
  if (blurIndex < 0 || previewIndex <= blurIndex) throw new Error('native invoker blur did not precede preview focus: ' + JSON.stringify(events));
  if (!events[previewIndex].inert) throw new Error('preview received focus before editor inertness applied');
`),
		Actions: []dashsnap.BrowserAction{
			{Kind: "mouse-down-at", JS: `
var overlay = document.querySelector('.process-scribe-preview-overlay');
var rect = overlay.getBoundingClientRect();
for (var y = rect.top + 3; y < Math.min(rect.bottom, rect.top + 60); y += 6) {
  for (var x = rect.left + 3; x < Math.min(rect.right, rect.left + 60); x += 6) {
    if (document.elementFromPoint(x, y) === overlay) return {x:x, y:y};
  }
}
throw new Error('scribe overlay has no bare backdrop point');
`},
			{Kind: "mouse-up"},
			{Kind: "eval", JS: `
return (async function(){
  var result = await window.__scribeFocusPending;
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var invoker = window.__scribeFocusInvoker;
  var editorRoot = window.__scribeFocusEditor;
  var events = window.__scribeFocusEvents;
  if (result !== null) throw new Error('backdrop returned a sendable prompt');
  if (document.querySelector('.process-scribe-preview-overlay')) throw new Error('backdrop left preview mounted');
  if (editorRoot.inert || editorRoot.hasAttribute('inert')) throw new Error('preview close left editor inert');
  if (document.activeElement !== invoker) throw new Error('preview close did not restore its connected invoker');
  var previewIndex = events.findIndex(function(event){ return event.type === 'focus' && event.label === 'preview'; });
  var restoreIndex = events.findIndex(function(event, index){
    return index > previewIndex && event.type === 'focus' && event.label === 'invoker';
  });
  if (restoreIndex <= previewIndex) throw new Error('invoker restoration did not follow preview focus: ' + JSON.stringify(events));
  if (events[restoreIndex].inert) throw new Error('invoker regained focus before inertness was removed');
  document.removeEventListener('focus', window.__scribeFocusRecord, true);
  document.removeEventListener('blur', window.__scribeFocusRecord, true);
  delete window.__scribeFocusRecord;
  delete window.__scribeFocusPending;
})();
`},
		},
		SettleMS: 200,
	}
}
