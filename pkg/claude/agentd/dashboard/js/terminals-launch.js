// terminals-launch.js — open (or focus) the standalone terminals multiplexer
// tab (js/terminals.js, served at /terminals) and hand it a terminal to show.
//
// Used by the dashboard's "web term" / "web window" row actions and the
// in-page terminal modal's "open in terminals tab" button. A single NAMED
// window target means repeated opens accumulate as panes in ONE tab rather
// than spawning a fresh tab each time.
//
// Handoff is over a BroadcastChannel with a ready+ack handshake, NOT a bare
// URL hash, because the hash is a single mutable slot: if a SECOND open is
// issued while the multiplexer tab is still cold-loading, its navigation
// would replace the first, silently dropping a terminal. Instead every seed
// is held in `unacked` until the multiplexer acknowledges opening it; when a
// (re)loading tab announces 'ready' we resend everything still pending. The
// first seed of a freshly opened tab also rides the navigation hash so the
// tab shows a terminal immediately, before the handshake round-trips. The
// multiplexer dedupes by key, so the belt-and-suspenders double-delivery is
// harmless. (BroadcastChannel is origin-scoped; the dashboard and terminals
// tabs share the daemon's origin within a run, so they're on the same
// channel. A browser without BroadcastChannel falls back to the hash slot.)

import { toast } from './refresh.js';

const WIN_NAME = 'tclaude_terminals';
const CHANNEL = 'tclaude_terminals';

let bc = null;
try { bc = new BroadcastChannel(CHANNEL); } catch (_) { /* unsupported — hash fallback */ }

// Seeds handed off but not yet acknowledged by the multiplexer. Keyed by
// seedKey so a re-flush never double-opens, and so an acknowledged seed is
// dropped — a later tab reopen then resurrects nothing the human had closed.
const unacked = new Map();

if (bc) {
  bc.onmessage = (e) => {
    const d = e.data;
    if (!d) return;
    if (d.type === 'ready') flush();                       // a (re)loaded tab — resend pending
    else if (d.type === 'opened' && d.key != null) unacked.delete(d.key);
  };
}

function seedKey(seed) { return seed.key || seed.ws; }

function flush() {
  if (!bc) return;
  for (const seed of unacked.values()) bc.postMessage({ type: 'open', seed });
}

// launchInTerminals opens/focuses the multiplexer tab and adds a pane for
// `seed` ({ ws, label, key }). `seed` may be a value or a Promise (e.g. the
// which-dir picker for "web term"): the tab is opened SYNCHRONOUSLY on the
// click gesture — so a pop-up blocker can't eat it — then driven once the
// seed resolves. A Promise that resolves to null/undefined (the user
// cancelled) closes the freshly-opened blank tab instead of stranding it.
export function launchInTerminals(seedOrPromise) {
  const win = window.open('', WIN_NAME);
  Promise.resolve(seedOrPromise).then((seed) => {
    const onTerminals = isOnTerminals(win);
    if (!seed || !seed.ws) {
      // Cancelled: close the blank tab we just popped, never an existing
      // multiplexer tab.
      if (win && !onTerminals) { try { win.close(); } catch (_) { /* already gone */ } }
      return;
    }
    if (!win) { toast('Allow pop-ups for this site to open a terminal tab', true); return; }
    unacked.set(seedKey(seed), seed);
    if (onTerminals) {
      // Tab already up: deliver over the channel (it acks, clearing `unacked`).
      // No navigation — that would reload and drop its other live panes. With
      // no BroadcastChannel, fall back to the hash slot (best-effort).
      if (bc) bc.postMessage({ type: 'open', seed });
      else { try { win.location.hash = 'open=' + encodeURIComponent(JSON.stringify(seed)); } catch (_) { /* closed */ } }
    } else {
      // Fresh/blank tab: load /terminals with this seed in the hash so it
      // shows immediately; the tab's 'ready' then flushes any sibling seeds
      // that were queued while it loaded.
      try { win.location = '/terminals#open=' + encodeURIComponent(JSON.stringify(seed)); } catch (_) { /* closed mid-flight */ }
    }
  });
}

// isOnTerminals reports whether `win` is already showing the multiplexer page.
// Reading .location is same-origin here; the catch guards a just-closed window.
function isOnTerminals(win) {
  if (!win) return false;
  try { return !!win.location && win.location.pathname === '/terminals'; }
  catch (_) { return false; }
}
