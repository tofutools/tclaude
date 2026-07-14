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
import { createAgentRosterReconciler, mountMux, normalizeSeed } from './terminals-core.js';
import { dashboardState } from './snapshot-store.js';

let mux = null;
const reconcileAgentRoster = createAgentRosterReconciler();

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
// pane count `n`. Mirrors the Costs / Plugins island visibility effects:
// body.hide-terminals removes the nav button + section via CSS, and
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
  $$('nav [data-tab]').forEach(b => b.classList.toggle('active', b.dataset.tab === name));
  $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-' + name));
  dashboardState.setActiveTab(name);
  // Revealing the Terminals tab (opening a web terminal) is a real user
  // navigation — signal the history router to push /terminals (one-way event,
  // no import; see nav-history.js). The count-0 auto-leave to Groups is
  // INVOLUNTARY and deliberately left unsignalled: nav-history reconciles it as
  // a URL replace, not a pushed back-entry.
  if (name === 'terminals') document.dispatchEvent(new CustomEvent('tclaude:navigated'));
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

// openWebWindowPane opens (or focuses) an in-browser terminal attached to an
// agent's LIVE session in the Terminals tab — the web-terminal equivalent of
// raising / attaching a native OS window. Used by the "web window" row action,
// and by the plain "focus" / "open window" actions (the row button, bulk
// windows modal, the ⌘ palette, and a message's focus button) when config
// dashboard.default_terminal = "web". `agent` is the stable action selector
// (agent_id ?? conv-id) — it
// drives the WS path, the detach (hideConv), the focus-match (seed.agent) AND
// the pane key, so ONE agent maps to ONE pane no matter which entry point opens
// it (a caller that keyed on a different identity, e.g. conv-id, would slip past
// the openPane key-dedup and open a duplicate). hideConv makes closing the pane
// run the reliable server-side detach — a live-session attach forks the tmux
// client, so without the detach the session stays "attached" and can't be
// reattached.
export function openWebWindowPane(agent, label) {
  openTerminalPane({
    ws: `/api/open-window-ws/${encodeURIComponent(agent)}`,
    label,
    key: `window:${agent}`,
    hideConv: agent,
    agent,
  });
}

// openWebTermPane opens an in-browser throwaway shell terminal in a chosen
// directory in the Terminals tab — the web-terminal equivalent of a native
// terminal window. Used by the "web term" row action, and by "open terminal" /
// a CWD path click when config dashboard.default_terminal = "web".
// `whichOrPromise` is the dir choice, or a Promise of it (the which-dir
// picker) — a null/cancelled pick is a no-op, so the tab is never revealed for
// nothing. Keyed on the `agent` selector + `which` so re-opening the same dir
// focuses the existing pane (see openWebWindowPane on the keying rationale). No
// hideConv: a web-term is a throwaway session with nothing to detach on close
// (the `agent` field still lets "focus" jump to it).
export function openWebTermPane(agent, label, whichOrPromise) {
  openTerminalPane(
    Promise.resolve(whichOrPromise).then((which) => (which
      ? {
        ws: `/api/term-ws/${encodeURIComponent(agent)}?which=${encodeURIComponent(which)}`,
        label,
        key: `term:${agent}:${which}`,
        agent,
      }
      : null)),
  );
}

// openGroupWebTermPane opens an in-browser throwaway shell in a GROUP's default
// directory (agent_groups.default_cwd) in the Terminals tab — the group
// counterpart of openWebTermPane, backing the group ⚙ menu's "open web terminal"
// item. Connects to /api/group-term-ws/{group}, which resolves the group's
// default dir server-side (a group with no default dir 404s → the socket closes
// and the pane surfaces the error). Keyed on `groupterm:{group}` so re-opening
// the same group focuses the existing pane. No hideConv / agent: a group term is
// a throwaway session with nothing to detach on close and no agent to focus-jump.
export function openGroupWebTermPane(group, label) {
  openTerminalPane({
    ws: `/api/group-term-ws/${encodeURIComponent(group)}`,
    label,
    key: `groupterm:${group}`,
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

// reconcileTerminalsForAgentRoster closes all per-agent panes whose owner left
// the active roster between two accepted dashboard snapshots. Retirement is an
// actor-level roster transition, so this catches retires initiated from any
// browser, the CLI, or another agent instead of depending on one UI action
// path. A deletion has the same safe cleanup result. Reincarnation keeps the
// stable agent_id present, so panes keyed by that canonical selector survive.
export function reconcileTerminalsForAgentRoster(nextAgents, authoritative) {
  const departed = reconcileAgentRoster(nextAgents, authoritative);
  if (departed.length && mux) mux.closeForAgents(departed);
}
