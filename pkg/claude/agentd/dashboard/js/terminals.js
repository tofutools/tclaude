// terminals.js — the standalone terminals page (terminals.html, served at
// /terminals).
//
// Since the multiplexer moved into the dashboard's own "Terminals" tab
// (js/terminals-tab.js), this page exists for ONE job: the per-terminal
// "⧉ tab" pop-out, which opens /terminals?solo=1#open=<seed> — a single
// terminal in its own OS/browser window, the one thing an in-dashboard tab
// can't give you (the browser becomes the window manager for side-by-side).
//
// It reuses the shared pane core (js/terminals-core.js) — the same xterm+WS
// machinery the in-dashboard tab runs — and just feeds it the seed carried in
// the URL hash. Deliberately self-contained: it imports only the core (no
// dashboard SPA), so the page never pulls in dashboard.css / helpers.js.
// Terminal / FitAddon are globals from the vendored classic scripts loaded
// before this module (same arrangement as the dashboard).

import { mountMux, normalizeSeed } from './terminals-core.js';
import { initDashPrefs } from './prefs.js';

const solo = new URLSearchParams(location.search).has('solo');
if (solo) document.body.classList.add('solo');
let soloSeed = null;

// A non-solo /terminals (someone navigated here by hand) still works as a
// multiplexer — nothing wires it anymore, but the core handles both shapes.
const mux = mountMux({
  tabsEl: document.getElementById('mux-tabs'),
  panesEl: document.getElementById('mux-panes'),
  emptyEl: document.getElementById('mux-empty'),
  solo,
  manageTitle: true,
});

// decodeOpenHash pulls the { ws, label, key } seed out of "#open=<encoded
// json>".
function decodeOpenHash() {
  const m = /[#&]open=([^&]+)/.exec(location.hash || '');
  if (!m) return null;
  try { return JSON.parse(decodeURIComponent(m[1])); }
  catch (_) { return null; }  // malformed hash — ignore
}

function consumeHash() {
  const seed = normalizeSeed(decodeOpenHash());
  // Clear the hash so an ordinary manual reload cannot race this page's detach
  // beacon against a newly reconnected pane, and so a later identical open
  // still fires hashchange. Keep the parsed solo seed in memory solely for the
  // explicit auth-recovery event below.
  if (location.hash) history.replaceState(null, '', location.pathname + location.search);
  if (!seed) return;
  // A pop-out inherits the dashboard theme it left. The palette preference is
  // still read independently from the shared SQLite-backed dashboard prefs.
  document.body.classList.toggle('wizard', seed.wizard === true);
  document.dispatchEvent(new CustomEvent('tclaude:wizard', {
    detail: { active: seed.wizard === true },
  }));
  if (solo) soloSeed = seed;
  mux.openPane(seed);
  if (seed.hideConv) armDetachBeacon(seed.hideConv);
}

// armDetachBeacon detaches this popped-out LIVE-session terminal server-side
// when the tab goes away (closed / navigated). A pop-out is a real tmux client;
// without this, closing the tab leaves the session "attached" and
// unreattachable — the same reason the multiplexer's × runs /api/hide. Only
// armed for a live-session seed (hideConv); a throwaway web-term needs no
// detach. sendBeacon survives unload where a fetch would be cancelled (and
// carries the same-origin dashboard cookie); pagehide covers tab-close + bfcache.
// Deduped per conv so multiple seeds on a hand-navigated page don't stack
// duplicate handlers.
const beaconed = new Set();
function armDetachBeacon(conv) {
  if (beaconed.has(conv)) return;
  beaconed.add(conv);
  window.addEventListener('pagehide', () => {
    try { navigator.sendBeacon('/api/hide/' + encodeURIComponent(conv)); } catch (_) { /* best-effort */ }
  });
}

// auth-session.js gives the page one synchronous chance to refine its
// post-login return target. A solo popout's seed no longer lives in the URL, so
// reconstruct it here without changing ordinary reload behavior.
window.addEventListener('tclaude:auth-expired', (event) => {
  if (!solo || !soloSeed || !event.detail) return;
  event.detail.returnTo = location.pathname + location.search
    + '#open=' + encodeURIComponent(JSON.stringify(soloSeed));
});

window.addEventListener('hashchange', consumeHash);
// The standalone pop-out is a separate page, so it must hydrate the same
// server-backed preference cache the dashboard boot hydrates. Mounting first
// keeps terminal startup immediate; once the GET lands, the theme event
// repaints an already-open pane if the persisted choice was disabled.
consumeHash();
initDashPrefs().then(() => {
  document.dispatchEvent(new CustomEvent('tclaude:terminal-palette', {
    detail: { hydrated: true },
  }));
});
