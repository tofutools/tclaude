// nav-history.js — the DOM/History adapter that wires the pure location-stack
// core (nav-history-core.js) to the live dashboard: browser Back/Forward, the
// header chrome buttons, and a path-based URL that survives reload (TCL-317).
//
// Split of concerns:
//   - nav-history-core.js owns the DATA — the virtual stack, traversal,
//     duplicate suppression, and path<->location mapping. Pure, unit-tested.
//   - this module owns the SIDE EFFECTS — reading the active tab out of the
//     DOM, activating a tab on traversal, calling history.pushState/back/
//     forward, and toggling the buttons' disabled state.
//
// Why mirror an index into history.state: the History API can't tell you
// whether a forward entry exists, and a popstate event doesn't say which
// direction it moved. We stamp our stack index into each entry's state so a
// native browser Back/Forward (or trackpad swipe) maps deterministically back
// onto our stack — which is what lets the chrome buttons show accurate
// disabled states (AC #3) and keeps browser + button traversal identical
// (AC #2).

import { $, $$ } from './helpers.js';
import {
  DEFAULT_TAB, normalizeLocation, initialState, current,
  push, go, indexOf, canBack, canForward, toPath, fromPath,
} from './nav-history-core.js';

// ROUTABLE_TABS is the set of top-level tabs that own a URL path — the middle
// of three related sets that must stay in step (see KNOWN_TABS in
// nav-history-core.js for the full ordering): it mirrors dashboardAppTabs in
// dashboard.go (the server SPA-fallback allow-list) exactly. Terminals (its own
// /terminals popout route) and Vegas (a conditional soundtrack tab) are
// deliberately NOT routed: navigating to them leaves the URL and history
// untouched, so they never appear as a bookmarkable location or a back/forward
// target.
const ROUTABLE_TABS = new Set([
  'groups', 'jobs', 'processes', 'plugins', 'access',
  'messages', 'costs', 'audit', 'logs', 'config',
]);

// The virtual stack (see nav-history-core.js). Replaced wholesale on every
// mutation — the core reducers are pure.
let stack = initialState();

// `applying` is true only while THIS module drives a tab activation
// programmatically (initial restore or a popstate traversal). The nav-click
// observer checks it so our own synthetic clicks never push a fresh history
// entry — the mirror of refresh.js's `cyclingTabs` guard.
let applying = false;

// activeLocationFromDOM reads the current dashboard location out of the live
// DOM: the active top-level nav button, plus the active subtab for the two tabs
// that have one. Everything is normalized through the core so an unexpected DOM
// state degrades to a valid location rather than a bogus one.
//
// Subtab reading is intentionally read-only here (top-level routing is this
// PR's scope); the /access/<sub> and /processes/<sub> deep paths and their
// stale-target handling are finished under TCL-335. Reading them now means a
// URL that already carries a subtab restores it, at no extra cost.
function activeLocationFromDOM() {
  const navBtn = $$('nav button[data-tab]').find(b => b.classList.contains('active'));
  const tab = navBtn ? navBtn.dataset.tab : DEFAULT_TAB;
  const loc = { tab };
  if (tab === 'access') {
    const sub = $$('#tab-access .access-subtab').find(b => b.classList.contains('active'));
    if (sub) loc.subtab = sub.dataset.subtab;
  } else if (tab === 'processes') {
    const sub = $$('#tab-processes .process-subtab').find(b => b.classList.contains('active'));
    if (sub) loc.subtab = sub.dataset.processSubtab;
  }
  return normalizeLocation(loc);
}

// activate brings `loc` forward in the UI by clicking the matching controls,
// under the `applying` guard so the resulting clicks don't re-enter the
// observer. Going through the real nav button's .click() (rather than poking
// classes) reuses every lazy-loader hung on that button — the same reasoning as
// cycleTab() in refresh.js — so a restored /costs or /logs actually loads its
// data. A non-routable or unknown tab is left as-is.
function activate(loc) {
  if (!ROUTABLE_TABS.has(loc.tab)) return;
  applying = true;
  try {
    const navBtn = $$('nav button[data-tab]').find(b => b.dataset.tab === loc.tab);
    if (navBtn && !navBtn.classList.contains('active')) navBtn.click();
    if (loc.tab === 'access' && loc.subtab) {
      $(`#tab-access .access-subtab[data-subtab="${loc.subtab}"]`)?.click();
    } else if (loc.tab === 'processes' && loc.subtab) {
      $(`#tab-processes .process-subtab[data-process-subtab="${loc.subtab}"]`)?.click();
    }
  } finally {
    applying = false;
  }
}

// preservedQuery keeps view-state query params across a path push. Only the
// cosmetic theme flags are carried (slop.js owns them via replaceState);
// everything else — including consumed-on-load legacy deep-link params like
// ?tab=/?access_request= — is intentionally dropped so the address bar settles
// to a clean canonical location. Returns "" or "?slop=1"/"?wizard=1".
function preservedQuery() {
  const src = new URLSearchParams(window.location.search);
  const out = new URLSearchParams();
  if (src.get('slop') === '1') out.set('slop', '1');
  else if (src.get('wizard') === '1') out.set('wizard', '1');
  const s = out.toString();
  return s ? '?' + s : '';
}

// urlFor builds the full URL (path + preserved query) for a location.
function urlFor(loc) {
  return toPath(loc) + preservedQuery();
}

// updateButtons syncs the header Back/Forward controls to the stack: disabled
// when there is nowhere to go (AC #3). Missing buttons are tolerated so the
// module stays inert if the chrome markup is ever absent.
function updateButtons() {
  const back = $('#nav-back');
  const fwd = $('#nav-forward');
  if (back) back.disabled = !canBack(stack);
  if (fwd) fwd.disabled = !canForward(stack);
}

// record pushes a user-initiated location onto the stack + browser history.
// A duplicate (re-selecting the current location) is suppressed by the core, so
// repeated clicks and passive re-renders never grow history (AC #4).
function record(loc) {
  if (!ROUTABLE_TABS.has(loc.tab)) return;
  const before = stack;
  stack = push(stack, loc);
  if (stack === before) return; // duplicate — no new entry
  history.pushState({ navIndex: stack.index }, '', urlFor(loc));
  updateButtons();
}

// onPopstate handles a browser Back/Forward (button or gesture). It trusts the
// index we stamped into history.state to reposition our stack, then activates
// the target location. A foreign/absent state (an entry from before init, or a
// cross-document nav) falls back to parsing the URL and reseeding the stack, so
// traversal never throws or desyncs.
function onPopstate(e) {
  const st = e.state;
  if (st && Number.isInteger(st.navIndex) && st.navIndex >= 0 && st.navIndex < stack.entries.length) {
    stack = go(stack, st.navIndex);
  } else {
    // Foreign/clobbered state (an entry whose navIndex was stripped by another
    // history writer, or a pre-init entry). Relocate within the existing stack
    // by URL when we can — preserving back/forward depth — and only reseed as a
    // last resort so a lost index never silently truncates history.
    const loc = fromPath(window.location.pathname);
    const idx = indexOf(stack, loc);
    stack = idx >= 0 ? go(stack, idx) : initialState(loc);
  }
  activate(current(stack));
  updateButtons();
}

// initNavHistory boots the router. Call it LATE in dashboard.js boot — after
// every tab binder (bindTabs, bindCostsTab, bindAuditTab, …) is installed —
// because restoring a deep-link URL clicks that tab, and the click must find
// its lazy-loader already wired.
export function initNavHistory() {
  let loc = fromPath(window.location.pathname);
  // Legacy deep-link alias: the approval auto-raise / tray links open
  // /?tab=<tab>. When the path itself is the bare default, fold that query tab
  // onto the path router so the location restores from it (and the URL settles
  // to the canonical /<tab>).
  if (loc.tab === DEFAULT_TAB) {
    const legacyTab = new URLSearchParams(window.location.search).get('tab');
    if (legacyTab && ROUTABLE_TABS.has(legacyTab)) loc = normalizeLocation({ tab: legacyTab });
  }
  stack = initialState(loc);

  // Restore the URL's tab (no-op when it's the already-active default), and
  // rewrite the current history entry so it carries our index + a canonical
  // path (a bare "/" stays "/"; "/costs" stays "/costs").
  activate(loc);
  history.replaceState({ navIndex: 0 }, '', urlFor(loc));

  // Observe user navigation. A delegated listener on <nav> bubbles AFTER each
  // button's own bindTabs handler (which set the .active class), so reading the
  // DOM here sees the post-switch location. Guarded by `applying` so our own
  // programmatic activations don't re-enter.
  document.querySelector('nav')?.addEventListener('click', (e) => {
    if (applying) return;
    if (!e.target.closest('button[data-tab]')) return;
    record(activeLocationFromDOM());
  });

  window.addEventListener('popstate', onPopstate);

  // The chrome buttons defer entirely to the browser history so a click and a
  // native Back/Forward share one code path (onPopstate). The disabled guard is
  // belt-and-suspenders — a disabled <button> won't fire a click anyway.
  $('#nav-back')?.addEventListener('click', () => { if (canBack(stack)) history.back(); });
  $('#nav-forward')?.addEventListener('click', () => { if (canForward(stack)) history.forward(); });

  updateButtons();
}
