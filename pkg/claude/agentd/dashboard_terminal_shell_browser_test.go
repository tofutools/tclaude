package agentd_test

// dashboard_terminal_shell_browser_test.go is the deterministic REAL-BROWSER
// smoke for the Preact terminal shells (TCL-490, follow-up to TCL-459): a live
// end-to-end terminal session — headless Chrome ↔ xterm ↔ WebSocket ↔ server
// PTY — driven across pane reveal/refocus, keyboard round trip, copy, kill →
// reconnect, the modal's detach/close confirmation, pop-out to the
// /terminals?solo=1 window, fit/resize, and exact-once teardown.
//
// Determinism: the PTY never touches host tmux or `tclaude session attach`.
// SetTermWSHookForTest swaps the spawned command for a fixed banner + `cat`
// echo program and counts PTY starts / applied resizes / teardowns; the test
// server adds /testhook/term/* routes so the page's own JS can kill the live
// PTY and poll those counters with bounded waits.
//
// Opacity: the smoke never renders into, mutates, or drives xterm's subtree —
// the component/adapter boundary under test stays intact. It DOES read what a
// user sees (rendered row text, keyboard focus location) because that is the
// entire point of an end-to-end smoke; those read-only probes are centralized
// in __smokePaneText / __smokeAssertXtermFocused so an xterm renderer change
// touches one place.
//
// Environment skips vs product failures: a missing TCLAUDE_DASHSNAP env gate
// or an unusable local Chrome (dashsnap.ErrBrowserUnavailable) SKIPS; any
// per-state error after the browser is up is a hard product FAILURE.
//
// Run it explicitly (CI compiles it but never launches a browser):
//
//	TCLAUDE_DASHSNAP=1 go test ./pkg/claude/agentd/ -run TestDashboardTerminalShellLiveChrome -v -count=1 -timeout 300s

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
)

// termSmokeMember is the seeded ONLINE fixture member every state opens its
// live terminal on (fe-lead in seedDashSnapFixture) — pinned by conv id so DOM
// order changes in the Groups tree can never route the smoke to an offline row.
const termSmokeMember = "f1000000-0000-4000-8000-000000000001"

// termSmokeShellCommand replaces the tmux/attach command behind every terminal
// WebSocket: a fixed two-line banner, then `cat` so typed input echoes back
// through the PTY. SMOKEREADY proves a fresh PTY stream; COPYTOKEN42 is the
// word the copy state selects (no xterm word separators, so a double click
// selects exactly the token).
const termSmokeShellCommand = `printf 'SMOKEREADY\nCOPYTOKEN42\n'; exec cat`

// termSmokeExpectedStarts is the exact number of PTYs the state matrix opens:
// reveal/refocus 1, copy 1, reconnect 2, modal confirm 2, pop-out+reattach 3,
// resize 1.
const termSmokeExpectedStarts = 10

// termSmokeCounters is the server-side ledger behind /testhook/term/*: PTY
// starts, completed teardowns, applied resizes (count + last geometry), and
// the child processes the kill route signals. "Exact once" here means every
// PTY the matrix opened is torn down exactly once END TO END — the ledger
// catches leaks (fewer teardowns than starts) and stray extra connections
// (more starts than a state accounts for); client-side double-dispose
// symptoms surface separately through the page-error capture, since the
// widget's dispose path is deliberately repeat-safe and silent when correct.
type termSmokeCounters struct {
	mu        sync.Mutex
	starts    int
	teardowns int
	resizes   int
	lastCols  int
	lastRows  int
	procs     []*os.Process
}

func (c *termSmokeCounters) onStart(proc *os.Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	c.procs = append(c.procs, proc)
}

func (c *termSmokeCounters) onResize(cols, rows int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resizes++
	c.lastCols, c.lastRows = cols, rows
}

func (c *termSmokeCounters) onTeardown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teardowns++
}

func (c *termSmokeCounters) snapshot() (starts, teardowns int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts, c.teardowns
}

func (c *termSmokeCounters) handleStats(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"starts": c.starts, "teardowns": c.teardowns, "resizes": c.resizes,
		"last_cols": c.lastCols, "last_rows": c.lastRows,
	})
}

// handleKill SIGKILLs every PTY child seen so far. Children already reaped by
// runPTYOverWS's teardown (cmd.Wait) make Kill a guarded no-op inside
// os.Process, so only the currently-live PTY actually dies — no PID-reuse
// hazard.
func (c *termSmokeCounters) handleKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, proc := range c.procs {
		_ = proc.Kill()
	}
	w.WriteHeader(http.StatusNoContent)
}

// TestDashboardTerminalShellLiveChrome drives the full live-terminal matrix in
// one headless Chrome over the real dashboard handler. Gated like the other
// browser smokes; see the file header for the run line and skip semantics.
func TestDashboardTerminalShellLiveChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke - set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)

	// Native windows must never pop during the smoke, and forcing the native
	// path to fail is also what routes the plain "open window" action into the
	// in-page terminal MODAL the confirmation state exercises.
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		return errors.New("terminal shell smoke: native windows disabled")
	}))

	counters := &termSmokeCounters{}
	t.Cleanup(agentd.SetTermWSHookForTest(&agentd.TermWSHook{
		RewriteCommand: func(_, tmuxSession string) (string, string) {
			// Keep the tmux session name so teardown still walks the real
			// detach path (against the flow harness's tmux simulator).
			return termSmokeShellCommand, tmuxSession
		},
		OnStart:    counters.onStart,
		OnResize:   counters.onResize,
		OnTeardown: counters.onTeardown,
	}))

	mux := http.NewServeMux()
	mux.Handle("/", agentd.BuildDashboardHandlerForTest())
	mux.HandleFunc("/testhook/term/stats", counters.handleStats)
	mux.HandleFunc("/testhook/term/kill", counters.handleKill)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	states := []dashsnap.State{
		terminalLiveRevealTypeState(),
		terminalLiveCopyState(),
		terminalLiveReconnectState(),
		terminalLiveModalConfirmState(),
		terminalLivePopOutState(),
		terminalLiveResizeState(),
	}
	for i := range states {
		states[i].JS = termSmokeHelpersJS + states[i].JS
	}

	outDir := filepath.Join(dashSnapOutRoot(t), "terminal-shell-live-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL:        srv.URL,
		OutDir:         outDir,
		GrantClipboard: true,
		// Live states chain several bounded in-page waits (connect, kill,
		// reconnect, popup), so give each state more than the default budget.
		StateTimeoutMS: 90000,
		States:         states,
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
		starts, teardowns := counters.snapshot()
		t.Fatalf("terminal shell live browser smoke failed:\n%s\nPTY ledger: starts=%d teardowns=%d\ncontact sheet: %s",
			strings.Join(failed, "\n"), starts, teardowns, filepath.Join(outDir, "index.html"))
	}

	// Exact-once teardown, end to end: after the browser is gone every PTY the
	// matrix opened must have completed its teardown exactly once — no leaks
	// (fewer teardowns than starts) and no stray extra connection the in-page
	// assertions did not account for (starts must land EXACTLY on the matrix's
	// expected total).
	deadline := time.Now().Add(10 * time.Second)
	for {
		starts, teardowns := counters.snapshot()
		if starts == termSmokeExpectedStarts && teardowns == starts {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("PTY ledger did not converge: starts=%d teardowns=%d, want both exactly %d",
				starts, teardowns, termSmokeExpectedStarts)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("terminal shell live browser smoke: %s", filepath.Join(outDir, "index.html"))
}

// termSmokeHelpersJS is prepended to every state's JS: page-error capture and
// the bounded-poll / stats / open-pane helpers the states share. All waits are
// deadline-bounded so a hung state reports WHAT it waited for instead of
// silently eating the state timeout.
const termSmokeHelpersJS = `
window.__smokeErrors = [];
window.addEventListener('error', function (event) {
  __smokeErrors.push('error: ' + event.message);
});
window.addEventListener('unhandledrejection', function (event) {
  var reason = event.reason;
  __smokeErrors.push('rejection: ' + (reason && reason.message ? reason.message : String(reason)));
});
window.__smokeNoPageErrors = function () {
  if (__smokeErrors.length) throw new Error('page errors: ' + __smokeErrors.join(' | '));
};
window.__smokePoll = async function (label, fn, timeoutMs) {
  var deadline = Date.now() + (timeoutMs || 15000);
  for (;;) {
    var lastErr = null;
    try {
      var value = await fn();
      if (value) return value;
    } catch (err) { lastErr = err; }
    if (Date.now() > deadline) {
      throw new Error('timed out waiting for ' + label +
        (lastErr ? ' (last: ' + (lastErr.message || lastErr) + ')' : ''));
    }
    await new Promise(function (resolve) { setTimeout(resolve, 60); });
  }
};
window.__smokeStats = async function () {
  var response = await fetch('/testhook/term/stats', { cache: 'no-store' });
  if (!response.ok) throw new Error('stats endpoint: HTTP ' + response.status);
  return response.json();
};
window.__smokePaneStatus = function () {
  var span = document.querySelector('.mux-pane.active .mux-pane-status');
  return span ? span.textContent : '';
};
window.__smokePaneText = function () {
  var rows = document.querySelector('.mux-pane.active .xterm-rows');
  return rows ? rows.textContent : '';
};
window.__smokeCount = function (haystack, needle) {
  return haystack.split(needle).length - 1;
};
window.__smokeTwoFrames = function () {
  return new Promise(function (resolve) {
    requestAnimationFrame(function () { requestAnimationFrame(resolve); });
  });
};
window.__smokeOpenLivePane = async function () {
  window.__s0 = await __smokeStats();
  document.querySelector('nav [data-tab="groups"]').click();
  var action = document.querySelector(
    'button[data-act="web-open-window"][data-conv="` + termSmokeMember + `"]');
  if (!action) throw new Error('web window action for the online fixture member missing');
  action.click();
  await __smokePoll('pane connected', function () { return __smokePaneStatus() === 'connected'; });
  await __smokePoll('live PTY banner', function () {
    return __smokePaneText().indexOf('SMOKEREADY') !== -1;
  });
};
window.__smokeAssertXtermFocused = function () {
  var pane = document.querySelector('.mux-pane.active');
  if (!pane) throw new Error('no active terminal pane');
  if (pane.getClientRects().length === 0) throw new Error('active pane is not rendered');
  var textarea = pane.querySelector('textarea.xterm-helper-textarea');
  if (!textarea) throw new Error('active pane has no xterm textarea');
  if (document.activeElement !== textarea) {
    var found = document.activeElement;
    throw new Error('keyboard focus is not in the active xterm; activeElement=' +
      (found ? found.tagName + '.' + found.className : 'null'));
  }
};
window.__smokeCloseAllPanesAndVerify = async function (expectStarts) {
  var close = document.querySelector('.mux-tab-close');
  while (close) {
    close.click();
    await __smokeTwoFrames();
    close = document.querySelector('.mux-tab-close');
  }
  await __smokePoll('all panes closed', function () {
    return document.querySelectorAll('.mux-pane').length === 0;
  });
  await __smokePoll('xterm DOM disposed', function () {
    return document.querySelectorAll('.xterm').length === 0;
  });
  await __smokePoll('exact-once teardown (' + expectStarts + ' PTYs)', async function () {
    var s = await __smokeStats();
    return s.starts - __s0.starts === expectStarts &&
      s.teardowns - __s0.teardowns === expectStarts;
  });
  __smokeNoPageErrors();
};
`

// terminalLiveRevealTypeState proves the LIVE pane basics: opening from Groups
// connects a real PTY, revealing from another tab refits/refocuses the SAME
// opaque xterm subtree without reconnecting, and real keystrokes travel
// browser → WebSocket → PTY and echo back into the rendered rows.
func terminalLiveRevealTypeState() dashsnap.State {
	return dashsnap.State{
		Key:     "live-reveal-refocus-type",
		Title:   "Live pane: reveal, refocus, keyboard round trip",
		Caption: "A Groups-row web window connects a live PTY; re-revealing reuses the same xterm subtree and socket; typed keys echo back through the PTY.",
		JS: `
return (async function () {
  await __smokeOpenLivePane();
  await __smokeTwoFrames();
  __smokeAssertXtermFocused();
  // The opacity boundary itself: Preact owns the host div but must never
  // render a sibling next to the adapter-owned subtree inside it.
  var host = document.querySelector('.mux-pane.active .mux-pane-xterm-fit');
  if (!host || host.childElementCount !== 1) {
    throw new Error('opaque host must hold exactly one adapter-owned child');
  }
  // Leave the Terminals tab, park focus elsewhere, then re-trigger the same
  // terminal: the reveal must reuse the pane and return the keyboard to xterm.
  document.querySelector('nav [data-tab="groups"]').click();
  var filter = document.querySelector('#filter-groups');
  if (filter) filter.focus();
  document.querySelector('button[data-act="web-open-window"][data-conv="` + termSmokeMember + `"]').click();
  await __smokeTwoFrames();
  if (document.querySelectorAll('.mux-pane').length !== 1) throw new Error('re-reveal opened a second pane');
  __smokeAssertXtermFocused();
  if (__smokePaneStatus() !== 'connected') throw new Error('re-reveal dropped the live socket: ' + __smokePaneStatus());
  // A remounted widget would have reconnected (mount always dials the PTY),
  // so a stable starts count proves the reveal reused the live widget rather
  // than rebuilding it — without reaching into xterm's subtree for identity.
  var s = await __smokeStats();
  if (s.starts - __s0.starts !== 1) throw new Error('re-reveal opened a second PTY: ' + (s.starts - __s0.starts));
})();`,
		Actions: []dashsnap.BrowserAction{
			{Kind: "type-text", Text: "smoketype123\r"},
			{Kind: "eval", JS: `
return (async function () {
  await __smokePoll('typed input echoed back through the PTY', function () {
    return __smokeCount(__smokePaneText(), 'smoketype123') >= 2;
  });
  await __smokeCloseAllPanesAndVerify(1);
})();`},
		},
	}
}

// terminalLiveCopyState selects the banner token with a REAL double click and
// copies it via the pane's Copy button — asserting the actual
// navigator.clipboard round trip, not a stubbed selection.
func terminalLiveCopyState() dashsnap.State {
	return dashsnap.State{
		Key:     "live-copy",
		Title:   "Live pane: double-click select + Copy button",
		Caption: "Double-clicking the rendered PTY banner selects the token; the Copy button lands it on the real browser clipboard.",
		JS: `
return (async function () {
  await __smokeOpenLivePane();
})();`,
		Actions: []dashsnap.BrowserAction{
			{Kind: "dblclick", JS: `
var rows = document.querySelector('.mux-pane.active .xterm-rows');
if (!rows) throw new Error('active pane has no xterm rows');
var target = null;
var all = rows.querySelectorAll('*');
for (var i = 0; i < all.length; i++) {
  if (all[i].textContent.indexOf('COPYTOKEN42') !== -1 && all[i].getClientRects().length > 0) {
    target = all[i]; // keep the DEEPEST (last) match — closest to the glyphs
  }
}
if (!target) throw new Error('no rendered element contains COPYTOKEN42');
var rect = target.getBoundingClientRect();
return { x: rect.left + Math.min(40, Math.max(4, rect.width / 2)), y: rect.top + rect.height / 2 };`},
			{Kind: "eval", JS: `
return (async function () {
  await __smokePoll('double click produced a selection', function () {
    var copy = document.querySelector('.mux-pane.active .mux-pane-header button[data-has-selection]');
    return copy && copy.dataset.hasSelection === 'true';
  });
})();`},
			{Kind: "click", Selector: `.mux-pane.active .mux-pane-header button[data-has-selection="true"]`},
			{Kind: "eval", JS: `
return (async function () {
  await __smokePoll('clipboard carries the selected token', async function () {
    var text = await navigator.clipboard.readText();
    return text.trim() === 'COPYTOKEN42';
  });
  await __smokeCloseAllPanesAndVerify(1);
})();`},
		},
	}
}

// terminalLiveReconnectState kills the live PTY server-side and proves the
// pane surfaces the disconnect, offers Reconnect, and reconnects onto a FRESH
// PTY — with the dead one torn down exactly once.
func terminalLiveReconnectState() dashsnap.State {
	return dashsnap.State{
		Key:     "live-reconnect",
		Title:   "Live pane: PTY death → disconnected → Reconnect",
		Caption: "Killing the server-side PTY flips the pane to disconnected with a Reconnect control; reconnecting opens a fresh PTY and replays the banner.",
		JS: `
return (async function () {
  await __smokeOpenLivePane();
  await fetch('/testhook/term/kill', { method: 'POST' });
  await __smokePoll('pane disconnected after PTY kill', function () {
    return __smokePaneStatus() === 'disconnected';
  });
  var reconnect = await __smokePoll('Reconnect control appears', function () {
    var buttons = document.querySelectorAll('.mux-pane.active .mux-pane-header button');
    for (var i = 0; i < buttons.length; i++) {
      if (buttons[i].textContent === 'Reconnect') return buttons[i];
    }
    return null;
  });
  await __smokePoll('killed PTY torn down exactly once', async function () {
    var s = await __smokeStats();
    return s.starts - __s0.starts === 1 && s.teardowns - __s0.teardowns === 1;
  });
  reconnect.click();
  await __smokePoll('pane reconnected', function () { return __smokePaneStatus() === 'connected'; });
  await __smokePoll('fresh PTY banner after reconnect', function () {
    return __smokeCount(__smokePaneText(), 'SMOKEREADY') >= 2;
  });
  await __smokeCloseAllPanesAndVerify(2);
})();`,
	}
}

// terminalLiveModalConfirmState exercises the in-page terminal MODAL (the
// "open window" browser fallback): × asks before detaching and cancel keeps
// the live session; a PTY death raises the disconnect prompt whose OK
// reconnects; Detach closes immediately.
func terminalLiveModalConfirmState() dashsnap.State {
	return dashsnap.State{
		Key:     "live-modal-confirm",
		Title:   "Live modal: close confirmation, disconnect prompt, detach",
		Caption: "The open-window modal asks before detaching (cancel keeps the live PTY), prompts on disconnect (OK reconnects), and Detach closes at once.",
		JS: `
return (async function () {
  window.__s0 = await __smokeStats();
  document.querySelector('nav [data-tab="groups"]').click();
  var action = document.querySelector(
    'button[data-act="open-window"][data-conv="` + termSmokeMember + `"]');
  if (!action) throw new Error('open window action for the online fixture member missing');
  action.click();
  var statusOf = function () {
    var span = document.querySelector('#term-session-status');
    return span ? span.textContent : '';
  };
  await __smokePoll('terminal modal opens (browser fallback)', function () {
    return document.querySelector('#term-session-modal') !== null;
  });
  await __smokePoll('modal connected', function () { return statusOf() === 'connected'; });
  await __smokePoll('modal PTY banner', function () {
    var rows = document.querySelector('#term-session-modal .xterm-rows');
    return rows && rows.textContent.indexOf('SMOKEREADY') !== -1;
  });
  // Close (×) must ask first; cancelling must keep the live session attached.
  document.querySelector('#term-session-close').click();
  await __smokePoll('detach confirmation shows', function () {
    return document.querySelector('#confirm-modal').classList.contains('show') &&
      document.querySelector('#confirm-title').textContent === 'Detach terminal?';
  });
  document.querySelector('#confirm-cancel').click();
  await __smokePoll('confirmation dismissed', function () {
    return !document.querySelector('#confirm-modal').classList.contains('show');
  });
  if (!document.querySelector('#term-session-modal')) throw new Error('cancel closed the modal');
  if (statusOf() !== 'connected') throw new Error('cancel dropped the live PTY: ' + statusOf());
  // A dying PTY must raise the disconnect prompt; OK ("Reconnect") revives it.
  await fetch('/testhook/term/kill', { method: 'POST' });
  await __smokePoll('disconnect prompt shows', function () {
    return document.querySelector('#confirm-modal').classList.contains('show') &&
      document.querySelector('#confirm-title').textContent === 'Terminal disconnected';
  });
  document.querySelector('#confirm-ok').click();
  await __smokePoll('modal reconnected onto a fresh PTY', async function () {
    var s = await __smokeStats();
    return statusOf() === 'connected' &&
      s.starts - __s0.starts === 2 && s.teardowns - __s0.teardowns === 1;
  });
  // Detach closes immediately — no confirmation — and tears down exactly once.
  document.querySelector('#term-session-detach').click();
  await __smokePoll('modal closed by Detach', function () {
    return document.querySelector('#term-session-modal') === null;
  });
  await __smokePoll('exact-once teardown for both modal PTYs', async function () {
    var s = await __smokeStats();
    return s.starts - __s0.starts === 2 && s.teardowns - __s0.teardowns === 2;
  });
  if (document.querySelectorAll('.xterm').length !== 0) throw new Error('modal close leaked xterm DOM');
  __smokeNoPageErrors();
})();`,
	}
}

// terminalLivePopOutState pops the live pane out into the real
// /terminals?solo=1 window via the ⧉ tab button (a REAL click, so window.open
// runs with user activation), asserts the solo page connects its own PTY, and
// reattaches it to the exact opener dashboard, and proves the solo page closes
// without firing a late detach over the replacement client.
func terminalLivePopOutState() dashsnap.State {
	return dashsnap.State{
		Key:     "live-pop-out",
		Title:   "Live pane: ⧉ pop-out and ↩ dashboard reattach",
		Caption: "Popping out moves the PTY to /terminals?solo=1; reattach returns it to the exact opener, closes the solo tab, and tears each replaced client down once.",
		JS: `
return (async function () {
  await __smokeOpenLivePane();
})();`,
		Actions: []dashsnap.BrowserAction{
			{Kind: "click", Selector: `.mux-pane.active .mux-pane-header button[title="Move this terminal to its own browser tab"]`},
			{Kind: "eval", JS: `
return (async function () {
  await __smokePoll('dashboard pane closed by pop-out', function () {
    return document.querySelectorAll('.mux-pane').length === 0;
  });
})();`},
			{Kind: "popup-eval", Selector: "solo=1", JS: `
return (async function () {
  var deadline = Date.now() + 20000;
  var poll = async function (label, fn) {
    for (;;) {
      var value = null;
      var lastErr = null;
      try { value = await fn(); } catch (err) { lastErr = err; }
      if (value) return value;
      if (Date.now() > deadline) {
        throw new Error('popup: timed out waiting for ' + label +
          (lastErr ? ' (last: ' + (lastErr.message || lastErr) + ')' : ''));
      }
      await new Promise(function (resolve) { setTimeout(resolve, 60); });
    }
  };
  await poll('solo pane connected', function () {
    var span = document.querySelector('.mux-pane.active .mux-pane-status');
    return span && span.textContent === 'connected';
  });
  await poll('solo PTY banner', function () {
    var rows = document.querySelector('.mux-pane.active .xterm-rows');
    return rows && rows.textContent.indexOf('SMOKEREADY') !== -1;
  });
  if (document.querySelectorAll('.xterm').length !== 1) {
    throw new Error('solo page should own exactly one xterm');
  }
})();`},
			{Kind: "eval", JS: `
return (async function () {
  await __smokePoll('solo window opened its own PTY', async function () {
    var s = await __smokeStats();
    return s.starts - __s0.starts === 2 && s.teardowns - __s0.teardowns === 1;
  });
})();`},
			{Kind: "popup-eval", Selector: "solo=1", JS: `
var button = document.querySelector('[title="Move this terminal back to its dashboard tab"]');
if (!button) throw new Error('popup: reattach button missing');
button.click();`},
			{Kind: "eval", JS: `
return (async function () {
  await __smokePoll('terminal reattached in dashboard', function () {
    var pane = document.querySelector('.mux-pane.active');
    var status = pane && pane.querySelector('.mux-pane-status');
    return status && status.textContent === 'connected';
  });
  await __smokePoll('pop-out and reattach teardown', async function () {
    var s = await __smokeStats();
    return s.starts - __s0.starts === 3 && s.teardowns - __s0.teardowns === 2;
  });
  await __smokeCloseAllPanesAndVerify(3);
  __smokeNoPageErrors();
})();`},
		},
	}
}

// terminalLiveResizeState proves fit/resize end to end: the PTY receives the
// fitted geometry on connect, shrinking the pane container makes the fit
// addon re-measure and the server apply a NARROWER PTY, and restoring the
// width widens it again.
func terminalLiveResizeState() dashsnap.State {
	return dashsnap.State{
		Key:     "live-fit-resize",
		Title:   "Live pane: fit + PTY resize round trip",
		Caption: "Connect applies the fitted geometry to the PTY; shrinking the pane container narrows the PTY; restoring it widens the PTY again.",
		JS: `
return (async function () {
  await __smokeOpenLivePane();
  // __s0 was snapshotted BEFORE this state's pane opened and every earlier
  // state verified its PTYs torn down, so requiring the resize COUNT to
  // advance past the baseline pins this geometry to THIS connection — stale
  // last_cols from an earlier state can never satisfy it.
  var first = await __smokePoll('initial PTY geometry applied by this connection', async function () {
    var s = await __smokeStats();
    return (s.resizes > __s0.resizes && s.last_cols > 0 && s.last_rows > 0) ? s : null;
  });
  var panes = document.querySelector('.mux-panes');
  panes.style.width = '520px';
  var shrunk = await __smokePoll('PTY narrowed after container shrink', async function () {
    var s = await __smokeStats();
    return (s.last_cols > 0 && s.last_cols < first.last_cols) ? s : null;
  });
  panes.style.width = '';
  await __smokePoll('PTY widened after container restore', async function () {
    var s = await __smokeStats();
    return s.last_cols > shrunk.last_cols;
  });
  await __smokeCloseAllPanesAndVerify(1);
})();`,
	}
}
