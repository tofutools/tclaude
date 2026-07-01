// terminals-tab.js — the in-dashboard "Terminals" tab.
//
// The default surface for the dashboard's "web term" / "web window" row
// actions (and the fallback modal's "⧉ tab" button): instead of popping a
// separate browser tab, they add a pane to a nav tab that lives INSIDE the
// dashboard SPA. Because the dashboard never full-reloads and the 2s poll only
// swaps individual list containers, the live xterm panes here survive untouched
// across refreshes.
//
// The tab is CONDITIONAL — it appears only while ≥1 terminal is open (mirroring
// the Costs/Plugins auto-hide, but driven client-side off the live pane count
// rather than a server flag). Opening the first terminal reveals it and
// switches to it; closing the last one hides it and falls back to Groups.
//
// The pane machinery is the shared core (js/terminals-core.js); this module
// only owns the tab's visibility + the entry point callers use.

import { $, $$ } from './helpers.js';
import { mountMux, normalizeSeed } from './terminals-core.js';

let mux = null;

// initTerminalsTab mounts the multiplexer onto the #tab-terminals section.
// Called once at boot from dashboard.js.
export function initTerminalsTab() {
  const tabsEl = $('#term-tab-tabs');
  const panesEl = $('#term-tab-panes');
  if (!tabsEl || !panesEl) return;
  // manageTitle:false — the dashboard owns document.title. onCount drives the
  // tab's show/hide off the live pane count.
  mux = mountMux({ tabsEl, panesEl, solo: false, manageTitle: false, onCount: applyTerminalsTabVisibility });
  applyTerminalsTabVisibility(0);
}

// applyTerminalsTabVisibility shows/hides the Terminals nav tab off the live
// pane count `n`. Mirrors applyCostTabVisibility / applyPluginsTabVisibility in
// refresh.js: body.hide-terminals removes the nav button + section via CSS, and
// if the tab is the active one when it goes empty (the human closed the last
// terminal) we fall back to Groups so they aren't stranded on a now-invisible
// section.
function applyTerminalsTabVisibility(n) {
  const visible = n > 0;
  document.body.classList.toggle('hide-terminals', !visible);
  const badge = $('#terminals-badge');
  if (badge) { badge.textContent = String(n); badge.hidden = !visible; }
  if (!visible) {
    const sec = document.getElementById('tab-terminals');
    if (sec && sec.classList.contains('active')) selectTab('groups');
  }
}

// selectTab activates a top-level nav tab by name, matching what a nav-button
// click does (refresh.js bindTabs). Used to jump to Terminals on open and back
// to Groups when the tab vanishes.
function selectTab(name) {
  $$('nav button').forEach(b => b.classList.toggle('active', b.dataset.tab === name));
  $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-' + name));
}

// openTerminalPane adds (or focuses) a pane in the Terminals tab and switches
// to it. Accepts a seed { ws, label, key } or a Promise of one — the "web term"
// which-dir picker resolves to the WS path, so callers can hand the picker
// promise straight through. A Promise resolving to null/undefined (the user
// cancelled the picker) is a no-op, so the tab is never revealed for nothing.
export function openTerminalPane(seedOrPromise) {
  Promise.resolve(seedOrPromise).then((seed) => {
    // Validate BEFORE revealing. A cancelled picker resolves to null and a
    // malformed seed fails normalizeSeed — either way we must not reveal +
    // switch to an empty Terminals tab that openPane would then refuse to
    // populate, stranding the user on a blank revealed tab. openPane
    // re-validates, so this is belt-and-suspenders, not the only gate.
    if (!mux || !normalizeSeed(seed)) return;
    // Reveal + switch BEFORE opening so the pane mounts into a laid-out,
    // visible section and its first fit measures the real viewport (the
    // per-pane ResizeObserver is the backstop either way).
    document.body.classList.remove('hide-terminals');
    selectTab('terminals');
    mux.openPane(seed);
  });
}

// focusTerminalForConv reveals the Terminals tab and activates the FIRST open
// pane belonging to an agent in `selectors` (matched on seed.agent). Returns
// true when a pane was found + focused, so the caller (the per-agent "focus"
// button / the palette's "Focus window") can jump to the already-open
// in-browser terminal INSTEAD of raising a native OS window. Returns false when
// no pane is open, so the caller falls through to its native focus path.
export function focusTerminalForConv(selectors) {
  if (!mux) return false;
  const key = mux.findPaneKey(selectors);
  if (!key) return false;
  document.body.classList.remove('hide-terminals');
  selectTab('terminals');
  mux.activatePane(key);
  return true;
}

// closeTerminalsForConvs closes any multiplexer pane attached to an agent's
// LIVE session (seed.hideConv) whose selector is in `selectors` — the reaction
// to an agent window being hidden/detached from OUTSIDE the multiplexer (the
// per-agent eye button, the command palette's per-agent "Hide window"). The
// detach already happened server-side, so the pane is closed WITHOUT re-running
// /api/hide. A throwaway web-term pane (no hideConv) is never matched, and a
// selector with no open pane is a no-op.
export function closeTerminalsForConvs(selectors) {
  if (mux) mux.closeForHide(selectors);
}

// closeTerminalsForWindowOp is the bulk twin: given a /api/agent-windows
// response's `agents` outcome list, close the panes of every agent that was
// actually DETACHED (ignoring focus / no_window / failed). Matches on BOTH the
// stable agent_id and the conv_id, since a pane's hideConv is whichever the row
// carried (agent_id ?? conv_id).
export function closeTerminalsForWindowOp(agents) {
  if (!Array.isArray(agents)) return;
  const sels = [];
  for (const o of agents) {
    if (o && o.outcome === 'detached') {
      if (o.agent_id) sels.push(o.agent_id);
      if (o.conv_id) sels.push(o.conv_id);
    }
  }
  if (sels.length) closeTerminalsForConvs(sels);
}
