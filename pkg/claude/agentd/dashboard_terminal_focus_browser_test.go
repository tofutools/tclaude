package agentd_test

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
)

// TestDashboardTerminalRevealFocusChrome is the real-browser acceptance check
// for TCL-538: revealing a terminal from another dashboard tab must leave the
// keyboard in the active xterm.
//
// This has to run in Chrome. Panes live under `section { display: none }` until
// the Terminals tab is revealed, and only a real browser refuses to focus an
// unrendered element - LinkeDOM accepts that focus and reports the very
// activeElement the regression lacks, so the LinkeDOM suites cannot see this
// bug. Each state self-checks that the pane is actually rendered
// (getClientRects) before trusting its activeElement assertion.
func TestDashboardTerminalRevealFocusChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke - set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "terminal-focus-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		States: []dashsnap.State{
			terminalOpenFromGroupsFocusState(),
			terminalRevealFromOtherTabFocusState(),
		},
	})
	if errors.Is(err, dashsnap.ErrBrowserUnavailable) {
		t.Skipf("environment: %v", err)
	}
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
		t.Fatalf("terminal reveal-focus browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("terminal reveal-focus browser smoke: %s", filepath.Join(outDir, "index.html"))
}

// pickTerminalJS is an async JS expression that opens the ⚙ "terminals &
// windows…" chooser on the row matching rowSel (appended to the menu item's
// selector) and picks act from it, as a click would. The chosen action is then
// dispatched like the former dedicated row button.
func pickTerminalJS(rowSel, act string) string {
	return `(async function () {
  var opener = document.querySelector('button[data-act="terminal-chooser"]` + rowSel + `');
  if (!opener) throw new Error('terminals & windows row action missing');
  opener.click();
  for (var i = 0; i < 100; i++) {
    var pick = document.querySelector('#term-chooser-modal [data-term-pick="` + act + `"]');
    if (pick) { pick.click(); return; }
    await new Promise(function (r) { setTimeout(r, 20); });
  }
  throw new Error('terminal chooser option ` + act + ` never rendered');
})()`
}

// openWebWindowFromGroupsJS picks the first member's "open session in
// browser" option from the terminals chooser. That action builds its seed
// synchronously (no round trip), so the pane mount and the tab reveal run
// without waiting on the terminal socket. It awaits, so it runs inside an
// async function body.
var openWebWindowFromGroupsJS = `
document.querySelector('nav [data-tab="groups"]').click();
await ` + pickTerminalJS("", "web-open-window") + `;
`

// assertActiveXtermFocusedJS proves the reveal left the keyboard in the active
// pane's xterm. The rendered-ness check comes first so a focus regression is
// never mistaken for a fixture that failed to open a pane at all.
const assertActiveXtermFocusedJS = `
var section = document.querySelector('#tab-terminals');
if (!section || !section.classList.contains('active')) throw new Error('Terminals tab was not revealed');
if (document.body.classList.contains('hide-terminals')) throw new Error('Terminals tab stayed hidden');
var pane = document.querySelector('.mux-pane.active');
if (!pane) throw new Error('no active terminal pane');
if (pane.getClientRects().length === 0) throw new Error('active pane is not rendered; a real browser drops focus here');
var textarea = pane.querySelector('textarea.xterm-helper-textarea');
if (!textarea) throw new Error('active pane has no xterm textarea');
if (document.activeElement !== textarea) {
  var found = document.activeElement;
  throw new Error('keyboard focus is not in the active xterm; activeElement=' +
    (found ? found.tagName + '.' + found.className : 'null'));
}
`

const waitTwoFramesJS = `
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
`

func terminalOpenFromGroupsFocusState() dashsnap.State {
	return dashsnap.State{
		Key:     "terminal-open-from-groups-focuses-xterm",
		Title:   "Opening the first web window from Groups focuses xterm",
		Caption: "The first terminal opened from a Groups row reveals the Terminals tab and leaves the keyboard in the active xterm.",
		JS: `
return (async function(){
` + openWebWindowFromGroupsJS + waitTwoFramesJS + assertActiveXtermFocusedJS + `
})();`,
	}
}

func terminalRevealFromOtherTabFocusState() dashsnap.State {
	return dashsnap.State{
		Key:     "terminal-reveal-from-other-tab-focuses-xterm",
		Title:   "Revealing an existing pane from another tab focuses xterm",
		Caption: "With a pane already open, navigating back to Groups and re-triggering the same terminal reveals the Terminals tab and returns the keyboard to the active xterm.",
		JS: `
return (async function(){
` + openWebWindowFromGroupsJS + waitTwoFramesJS + `
  // Leave the Terminals tab and park focus elsewhere, so the reveal below has
  // to move the keyboard back into xterm rather than merely leaving it there.
  document.querySelector('nav [data-tab="groups"]').click();
  var filter = document.querySelector('#filter-groups');
  if (filter) filter.focus();
  if (document.querySelector('#tab-terminals').classList.contains('active')) throw new Error('Groups tab did not take over');
  await ` + pickTerminalJS("", "web-open-window") + `;
` + waitTwoFramesJS + `
  if (document.querySelectorAll('.mux-pane').length !== 1) throw new Error('re-revealing the same agent opened a second pane');
` + assertActiveXtermFocusedJS + `
})();`,
	}
}
