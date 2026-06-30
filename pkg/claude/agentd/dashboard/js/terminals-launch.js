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
    const fresh = isBlank(win);
    if (!seed || !seed.ws) {
      // Cancelled: don't leave a blank tab we just popped. Only close one we
      // freshly created (about:blank) — never an existing multiplexer tab.
      if (win && fresh) { try { win.close(); } catch (_) { /* not ours / already gone */ } }
      return;
    }
    if (!win) { toast('Allow pop-ups for this site to open a terminal tab', true); return; }
    const hash = 'open=' + encodeURIComponent(JSON.stringify(seed));
    if (fresh) win.location = '/terminals#' + hash;  // new tab → load the page
    else win.location.hash = hash;                   // open tab → hashchange, no reload
  });
}

// isBlank reports whether `win` is a freshly created, not-yet-navigated tab
// (about:blank) versus an already-open multiplexer tab. Reading .location is
// same-origin here; the catch is belt-and-suspenders.
function isBlank(win) {
  if (!win) return false;
  try { return !win.location || win.location.href === 'about:blank'; }
  catch (_) { return false; }
}
