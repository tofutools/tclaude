// nav-history.js — the DOM/History adapter that wires the pure location-stack
// core (nav-history-core.js) to the live dashboard: browser Back/Forward and a
// path-based URL that survives reload (TCL-317).
//
// Split of concerns:
//   - nav-history-core.js owns the DATA — the virtual stack, traversal,
//     duplicate suppression, and path<->location mapping. Pure, unit-tested.
//   - this module owns the SIDE EFFECTS — reading the active tab out of the
//     DOM, activating a tab on traversal, and calling history.pushState.
//
// Why mirror an index into history.state: a popstate event doesn't identify the
// dashboard location it represents. We stamp our stack index into each entry's
// state so native browser Back/Forward (or a trackpad swipe) maps
// deterministically back onto our stack.

import { $, $$, isModifiedClick } from './helpers.js';
import {
  DEFAULT_TAB, normalizeLocation, initialState, current, locEquals,
  push, replaceCurrent, toPath, fromPath, resolvePopstate,
  serializeStack, reviveState,
} from './nav-history-core.js';

// ROUTABLE_TABS is the set of top-level tabs that own a URL path. Every content
// tab is routable; only Vegas (a conditional soundtrack tab, no content to
// bookmark) is deliberately left out — navigating to it leaves the URL and
// history untouched. Terminals IS routable: /terminals serves the dashboard SPA
// for the tab (its ?solo=1 popout keeps the same route — see
// handleDashboardTerminals). Kept in step with dashboardAppTabs in dashboard.go
// (the generic SPA-fallback allow-list, which omits terminals only because that
// path has its own handler) — see KNOWN_TABS in nav-history-core.js for the full
// set ordering.
const ROUTABLE_TABS = new Set([
  'groups', 'terminals', 'jobs', 'processes', 'plugins', 'access',
  'messages', 'costs', 'audit', 'logs', 'debug', 'config',
]);

// The virtual stack (see nav-history-core.js). Replaced wholesale on every
// mutation — the core reducers are pure.
let stack = initialState();

// `applying` is true only while THIS module drives a tab activation
// programmatically (initial restore or a popstate traversal). The nav-click
// observer checks it so our own synthetic clicks never push a fresh history
// entry — the mirror of refresh.js's `cyclingTabs` guard.
let applying = false;

// `ready` gates the exported hooks until initNavHistory has run. Subtab
// activators and the poll reconcile fire during boot before the router is set
// up; without this they would push/replace against the uninitialised stack.
let ready = false;

// tabAvailable reports whether a routable tab is actually reachable right now —
// its nav button exists and isn't CSS-hidden (offsetParent === null means a
// display:none up the chain, exactly how the Terminals tab drops out with no
// live terminals, or a disabled Costs/Plugins tab). A restored URL pointing at
// an unavailable tab is a stale target (AC #5): the caller falls back to the
// default rather than stranding on a blank hidden section.
function tabAvailable(tab) {
  const btn = $$('nav [data-tab]').find(b => b.dataset.tab === tab);
  return !!(btn && btn.offsetParent !== null);
}

// activeLocationFromDOM reads the current dashboard location out of the live
// DOM: the active top-level nav button, plus the active subtab for the two tabs
// that have one. Everything is normalized through the core so an unexpected DOM
// state degrades to a valid location rather than a bogus one.
function activeLocationFromDOM() {
  const navBtn = $$('nav [data-tab]').find(b => b.classList.contains('active'));
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
    const navBtn = $$('nav [data-tab]').find(b => b.dataset.tab === loc.tab);
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

// preservedQuery is the view-state query carried across a path push. Theme is
// GLOBAL body state (body.slop / body.wizard, owned by slop.js), not per
// location — so we read it LIVE from the DOM rather than from the previous URL.
// Reading the URL would snapshot a stale theme onto each entry and let a Back
// across a theme toggle desync the URL from the live theme. Everything else —
// including consumed-on-load legacy deep-link params (?tab=/?access_request=) —
// is intentionally dropped so the address bar settles to a clean canonical
// location. Returns "" or "?slop=1"/"?wizard=1".
function preservedQuery() {
  const out = new URLSearchParams();
  if (document.body.classList.contains('slop')) out.set('slop', '1');
  else if (document.body.classList.contains('wizard')) out.set('wizard', '1');
  const s = out.toString();
  return s ? '?' + s : '';
}

// urlFor builds the full URL (path + preserved query) for a location.
function urlFor(loc) {
  return toPath(loc) + preservedQuery();
}

// record pushes a user-initiated location onto the stack + browser history.
// A duplicate (re-selecting the current location) is suppressed by the core, so
// repeated clicks and passive re-renders never grow history (AC #4).
function record(loc) {
  if (!ROUTABLE_TABS.has(loc.tab)) return;
  const before = stack;
  stack = push(stack, loc);
  if (stack === before) return; // duplicate — no new entry
  // Persist the WHOLE stack (not just the index) so a reload can reconstruct
  // depth — see serializeStack. urlFor carries the live theme.
  history.pushState(serializeStack(stack), '', urlFor(loc));
}

// recordCurrentLocation pushes the user's new location. Top-level tab clicks
// are read from the live DOM after their delegated click handler has run.
// Subtab activators may instead include detail.location on their navigation
// event: signal-driven views can emit before Preact commits their active class,
// so reading the DOM there would record the previously rendered subtab. No-ops
// until the router is initialised and while WE are programmatically restoring a
// location, so neither boot nor a popstate activation forges an entry.
function recordCurrentLocation(event) {
  if (!ready || applying) return;
  const announced = event?.detail?.location;
  record(announced ? normalizeLocation(announced) : activeLocationFromDOM());
}

// reconcileLocation corrects the URL after an INVOLUNTARY re-location — a tab
// the dashboard auto-left because it became hidden (the Terminals tab when the
// last terminal closes; a Costs/Plugins tab disabled in config), which switches
// tabs by toggling classes directly rather than through a user click. Driven by
// the `tclaude:snapshot` poll event, it replaces the current entry (never
// pushes) so the URL tracks the visible tab without forging a back/forward step.
// A non-routable active tab (Vegas) is left alone.
function reconcileLocation() {
  if (!ready || applying) return;
  const loc = activeLocationFromDOM();
  if (!ROUTABLE_TABS.has(loc.tab)) return;
  if (locEquals(loc, current(stack))) return; // no drift
  // Reconcile ONLY an involuntary re-location: the entry we're on points at a
  // tab that is no longer available, so the dashboard auto-left it (e.g. the
  // Terminals tab after the last terminal closed). A drift while the current
  // entry's tab is STILL available is a voluntary navigation that pushes its own
  // entry (it emits tclaude:navigated) — replacing it here would clobber a real
  // back-entry, and would also strip a /processes/runs/<id> selection that
  // activeLocationFromDOM can't yet reproduce (a trap for the selection
  // follow-up: teach activeLocationFromDOM/activate about selection before
  // loosening this guard).
  if (tabAvailable(current(stack).tab)) return;
  stack = replaceCurrent(stack, loc);
  history.replaceState(serializeStack(stack), '', urlFor(loc));
}

// onPopstate handles a browser Back/Forward (button or gesture). It trusts the
// index we stamped into history.state to reposition our stack, then activates
// the target location. A foreign/absent state (an entry from before init, or a
// cross-document nav) falls back to parsing the URL and reseeding the stack, so
// traversal never throws or desyncs.
function onPopstate(e) {
  // Decide the target from the popped URL + its stamped index. The core
  // validates the index against the URL (a reload leaves older entries carrying
  // stale, cross-instance indices) and falls back to URL relocation/reseed.
  const loc = fromPath(window.location.pathname);
  const navIndex = e.state && Number.isInteger(e.state.navIndex) ? e.state.navIndex : -1;
  stack = resolvePopstate(stack, loc, navIndex);
  // Stale target: a popped tab may have been hidden while it sat in the
  // back-stack (e.g. terminals closed while you were on another tab), so
  // activating it would strand the user on a blank hidden section. Fall back to
  // the default in place, mirroring the init-time guard.
  if (current(stack).tab !== DEFAULT_TAB && !tabAvailable(current(stack).tab)) {
    stack = replaceCurrent(stack, normalizeLocation({ tab: DEFAULT_TAB }));
  }
  activate(current(stack));
  // Re-stamp the current entry with the fresh full stack (heals stale/partial
  // state after a reload) AND — because theme is global, not per-location —
  // rewrite the URL to carry the LIVE theme so navigating history never leaves
  // the URL and the DOM theme divergent. replaceState never fires popstate.
  history.replaceState(serializeStack(stack), '', urlFor(current(stack)));
}

// initNavHistory boots the router. Call it LATE in dashboard.js boot — after
// every tab binder/island (bindTabs, the Costs and Audit islands, …) is installed —
// because restoring a deep-link URL clicks that tab, and the click must find
// its lazy-loader already wired.
export function initNavHistory() {
  const urlLoc = fromPath(window.location.pathname);
  // On a RELOAD, history.state still holds the stack we persisted for this
  // entry — reconstruct it (full depth) so native browser navigation retains
  // the same location mapping. reviveState validates the payload against the
  // current URL and returns null otherwise.
  const revived = reviveState(window.history.state, urlLoc);
  let loc;
  if (revived) {
    stack = revived;
    loc = current(stack);
  } else {
    // Fresh load. Legacy deep-link alias: the approval auto-raise / tray links
    // open /?tab=<tab>. When the path is the bare default, fold that query tab
    // onto the path router so the location restores from it (URL settles to the
    // canonical /<tab>).
    loc = urlLoc;
    if (loc.tab === DEFAULT_TAB) {
      const legacyTab = new URLSearchParams(window.location.search).get('tab');
      if (legacyTab && ROUTABLE_TABS.has(legacyTab)) loc = normalizeLocation({ tab: legacyTab });
    }
    stack = initialState(loc);
  }

  // Stale-target fallback: a restored URL pointing at a tab that is currently
  // hidden (e.g. /terminals reloaded with no live terminals, so the Terminals
  // tab is CSS-hidden) has nothing to show — fall back to the default rather
  // than stranding on a blank section. Runs here, after the tab binders and
  // the Terminal Shell island set the visibility classes.
  if (loc.tab !== DEFAULT_TAB && !tabAvailable(loc.tab)) {
    loc = normalizeLocation({ tab: DEFAULT_TAB });
    stack = initialState(loc);
  }

  // Restore the tab (no-op when it's the already-active default), and rewrite
  // the current history entry so it carries the full stack + a canonical path
  // (a bare "/" stays "/"; "/costs" stays "/costs").
  activate(loc);
  history.replaceState(serializeStack(stack), '', urlFor(loc));
  ready = true;

  // Observe user navigation. A delegated listener on <nav> bubbles AFTER each
  // button's own bindTabs handler (which set the .active class), so reading the
  // DOM here sees the post-switch location. recordCurrentLocation self-guards on
  // `applying` so our own programmatic activations don't re-enter.
  document.querySelector('nav')?.addEventListener('click', (e) => {
    if (!e.target.closest('[data-tab]')) return;
    // A modified/middle click opens the tab in a new browser tab and does NOT
    // switch this view (bindTabs bails on it too), so there is nothing to
    // record — skip it. A plain click has already flipped the active tab by the
    // time this delegated listener runs, so the DOM read below sees the new
    // location.
    if (isModifiedClick(e)) return;
    recordCurrentLocation();
  });

  window.addEventListener('popstate', onPopstate);

  // Subtab switches (Access / Processes) emit `tclaude:navigated` after they set
  // their active class — record them as user navigation, so /access/sudo and
  // /processes/runs update the URL just like a top-level tab switch.
  document.addEventListener('tclaude:navigated', recordCurrentLocation);

  // Each snapshot poll fires `tclaude:snapshot`; reconcile the URL then, so an
  // involuntary tab switch (a tab auto-hiding) corrects the address bar without
  // forging history. One-way via the event so refresh.js needn't import us.
  document.addEventListener('tclaude:snapshot', reconcileLocation);
}
