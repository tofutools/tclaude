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

import { mountMux } from './terminals-core.js';

const solo = new URLSearchParams(location.search).has('solo');
if (solo) document.body.classList.add('solo');

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
  const seed = decodeOpenHash();
  // Clear the hash either way so a manual reload doesn't re-open a stale seed,
  // and so the NEXT open — even an identical one — still changes the hash and
  // fires hashchange. The core dedupes by key, so a repeat is harmless.
  if (location.hash) history.replaceState(null, '', location.pathname + location.search);
  if (seed) mux.openPane(seed);
}

window.addEventListener('hashchange', consumeHash);
consumeHash();
