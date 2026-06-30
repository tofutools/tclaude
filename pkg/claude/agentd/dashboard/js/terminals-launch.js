// terminals-launch.js — open (or focus) the standalone terminals multiplexer
// tab (js/terminals.js, served at /terminals) and hand it a terminal to show.
//
// Used by the dashboard's "web term" / "web window" row actions and the
// in-page terminal modal's "open in terminals tab" button. A single NAMED
// window target means repeated opens accumulate as panes in ONE tab rather
// than spawning a fresh tab each time; the seed is passed via the URL hash
// (#open=<json>) so an ALREADY-open tab picks it up through a hashchange
// WITHOUT reloading (a reload would drop its other live panes).

import { toast } from './refresh.js';

const WIN_NAME = 'tclaude_terminals';

// launchInTerminals opens/focuses the multiplexer tab and adds a pane for
// `seed` ({ ws, label, key }). `seed` may be a value or a Promise (e.g. the
// which-dir picker for "web term"): the tab is opened SYNCHRONOUSLY on the
// click gesture — so a pop-up blocker can't eat it — then navigated once the
// seed resolves. A Promise that resolves to null/undefined (the user
// cancelled) closes the freshly-opened blank tab instead of stranding it.
export function launchInTerminals(seedOrPromise) {
  const win = window.open('', WIN_NAME);
  Promise.resolve(seedOrPromise).then((seed) => {
    // Only ADD via the hash (no reload) when the tab is already showing the
    // multiplexer; for a freshly popped blank tab (about:blank → pathname
    // "blank"), or anything else, do a full navigation. This is more robust
    // than sniffing about:blank, whose href/pathname vary across browsers.
    const onTerminals = isOnTerminals(win);
    if (!seed || !seed.ws) {
      // Cancelled (e.g. the which-dir picker was dismissed): close the blank
      // tab we just popped, but never an existing multiplexer tab.
      if (win && !onTerminals) { try { win.close(); } catch (_) { /* already gone */ } }
      return;
    }
    if (!win) { toast('Allow pop-ups for this site to open a terminal tab', true); return; }
    const hash = 'open=' + encodeURIComponent(JSON.stringify(seed));
    try {
      if (onTerminals) win.location.hash = hash;       // open tab → hashchange, no reload
      else win.location = '/terminals#' + hash;        // new/blank tab → load the page
    } catch (_) { /* tab closed mid-flight — the human can click again */ }
  });
}

// isOnTerminals reports whether `win` is already showing the multiplexer page
// (so we can hand it a new terminal via a hashchange instead of reloading and
// dropping its live panes). Reading .location is same-origin here; the catch
// guards a just-closed window.
function isOnTerminals(win) {
  if (!win) return false;
  try { return !!win.location && win.location.pathname === '/terminals'; }
  catch (_) { return false; }
}
