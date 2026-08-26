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

// TestDashboardToolbarSelectChrome proves the global profile controls no
// longer depend on Chrome's platform-native select popup. Real pointer input
// opens each HTML listbox after its live host is re-homed into the fixed dock;
// the browser top layer keeps it visible inside the viewport, Escape restores
// trigger focus, and an outside pointer light-dismisses it.
func TestDashboardToolbarSelectChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()
	outDir := filepath.Join(dashSnapOutRoot(t), "toolbar-select-"+time.Now().Format("20060102-150405.000"))

	ready := `return (async function(){
document.querySelector('nav [data-tab="groups"]').click();
(await import('/static/js/dock.js')).setDockOpen(true);
var deadline = Date.now() + 5000;
while ((!document.querySelector('#dock-actions-profile #dashboard-default-profile') ||
        !document.querySelector('#dock-actions-profile #dashboard-default-sandbox-profile')) && Date.now() < deadline) {
  await new Promise(function(resolve){ setTimeout(resolve, 50); });
}
if (!document.querySelector('#dock-actions-profile #dashboard-default-profile')) throw new Error('profile trigger was not re-homed into dock');
if (!document.querySelector('#dock-actions-profile #dashboard-default-sandbox-profile')) throw new Error('sandbox trigger was not re-homed into dock');
})();`
	assertOpen := func(trigger string) string {
		return `return new Promise(function(resolve,reject){requestAnimationFrame(function(){requestAnimationFrame(function(){try{
var trigger = document.querySelector('` + trigger + `');
var popup = document.querySelector('.toolbar-profile-popover:popover-open');
if (!popup) throw new Error('trusted click did not open an HTML top-layer listbox');
if (popup.getAttribute('role') !== 'listbox' || popup.localName !== 'div') throw new Error('picker is not the shared HTML listbox');
if (document.activeElement !== popup) throw new Error('open listbox did not receive focus');
if (trigger.getAttribute('aria-expanded') !== 'true') throw new Error('trigger does not expose open state');
var rect = popup.getBoundingClientRect();
if (rect.left < 7 || rect.top < 7 || rect.right > innerWidth - 7 || rect.bottom > innerHeight - 7) {
  throw new Error('listbox escaped viewport: ' + JSON.stringify({left:rect.left,top:rect.top,right:rect.right,bottom:rect.bottom,width:innerWidth,height:innerHeight}));
}
resolve();
}catch(error){reject(error);}});});});`
	}

	state := dashsnap.State{
		Key:     "toolbar-select-dock-top-layer",
		Title:   "Dock profile selectors — HTML top layer",
		Caption: "Real Chrome opens both re-homed profile selectors as viewport-contained HTML listboxes; Escape and outside-click dismissal are self-checked.",
		JS:      ready,
		Actions: []dashsnap.BrowserAction{
			{Kind: "click", Selector: "#dock-actions-profile #dashboard-default-profile"},
			{Kind: "eval", JS: assertOpen("#dock-actions-profile #dashboard-default-profile")},
			{Kind: "key", Key: "Escape"},
			{Kind: "eval", JS: `var trigger=document.querySelector('#dashboard-default-profile');
if (document.querySelector('.toolbar-profile-popover:popover-open')) throw new Error('Escape did not close profile listbox');
if (document.activeElement !== trigger || trigger.getAttribute('aria-expanded') !== 'false') throw new Error('Escape did not restore profile trigger focus');`},
			{Kind: "click", Selector: "#dock-actions-profile #dashboard-default-sandbox-profile"},
			{Kind: "eval", JS: assertOpen("#dock-actions-profile #dashboard-default-sandbox-profile")},
			{Kind: "click", Selector: "#dock-body"},
			{Kind: "eval", JS: `return new Promise(function(resolve,reject){setTimeout(function(){try{
if (document.querySelector('.toolbar-profile-popover:popover-open')) throw new Error('outside click did not dismiss sandbox listbox');
if (document.querySelector('#dashboard-default-sandbox-profile').getAttribute('aria-expanded') !== 'false') throw new Error('dismissed sandbox trigger remained expanded');
resolve();
}catch(error){reject(error);}},50);});`},
			{Kind: "click", Selector: "#dock-actions-profile #dashboard-default-sandbox-profile"},
			{Kind: "eval", JS: assertOpen("#dock-actions-profile #dashboard-default-sandbox-profile")},
		},
	}
	shots, err := dashsnap.Capture(dashsnap.Config{BaseURL: srv.URL, OutDir: outDir, States: []dashsnap.State{state}})
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
		t.Fatalf("toolbar Select browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("toolbar Select browser smoke: %s", filepath.Join(outDir, "index.html"))
}
