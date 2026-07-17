// nav-history-core.js — the DOM-free heart of dashboard back/forward
// navigation (TCL-317 / TCL-333).
//
// This module is a pure, dependency-free reducer over a virtual "location
// stack": an ordered list of visited locations plus a current index. It is
// deliberately importable under plain Node (no `window`, no `document`, no
// `history`) so its behaviour — traversal order, duplicate suppression,
// forward-tail truncation, stale-target fallback, and the path <-> location
// mapping — can be unit-tested with `node --test`
// without a browser or a bundler.
//
// Why a virtual stack at all? A `popstate` event does not report which location
// index it represents. We keep our own stack and mirror its index into
// `history.state` so browser Back/Forward navigation maps deterministically to
// dashboard locations. The DOM/History adapter (js/nav-history.js, TCL-334)
// owns those side effects; this file owns only the data.
//
// URL scheme decision (TCL-317): the PATH encodes the location ("where you
// are") and query params encode view-state ("how it's filtered" — slop/wizard,
// checkboxes, text filters). So this module maps location <-> pathname only;
// the DOM layer merges/preserves the query string separately.

// DEFAULT_TAB is the location the dashboard falls back to for an empty/unknown
// path and the ultimate parent for stale-target resolution. It mirrors the
// dashboard's default active tab (#tab-groups is `.active` at rest).
export const DEFAULT_TAB = 'groups';

// KNOWN_TABS is the set of top-level nav tabs this router will PARSE from a
// path. It is intentionally the widest of three related sets:
//   KNOWN_TABS (parse-tolerant, here)
//     ⊇ ROUTABLE_TABS (js/nav-history.js — tabs we actually push a URL for)
//     ⊇ dashboardAppTabs (dashboard.go — paths the server serves)
// Terminals/Vegas live in KNOWN_TABS (so a hand-typed URL degrades gracefully)
// but are not routed or server-served. Keep the three in step when adding or
// removing a tab. Kept in sync with the nav buttons in dashboard.html
// (data-tab=...). An unknown first segment parses back to the default location
// rather than an invalid tab, which keeps a stale/typo'd URL from breaking
// navigation (AC #5).
export const KNOWN_TABS = new Set([
  'groups', 'terminals', 'jobs', 'processes', 'plugins',
  'access', 'messages', 'usage', 'costs', 'audit', 'logs', 'debug', 'config', 'vegas',
]);

// KNOWN_SUBTABS enumerates the valid second-segment values per tab that has a
// sub-navigation. Only these two tabs have one today:
//   - Access: the segmented control (permissions / slugs / sudo).
//   - Processes: the subtab switcher (templates / runs / worklist).
// A second segment not listed here is dropped on parse (fall back to the tab's
// default view) so a renamed/removed subtab degrades gracefully.
export const KNOWN_SUBTABS = {
  access: new Set(['permissions', 'slugs', 'sudo']),
  processes: new Set(['templates', 'runs', 'worklist']),
};

// defaultLocation returns a fresh copy of the fallback location. Returned as a
// new object each call so callers can never alias/mutate a shared default.
export function defaultLocation() {
  return { tab: DEFAULT_TAB };
}

// normalizeLocation coerces an arbitrary input into a canonical location shape
// `{ tab, subtab?, selection? }`, dropping empty/unknown pieces. Optional keys
// are OMITTED (not set to undefined) so structural equality via locEquals and
// JSON round-trips stay stable. An unknown tab collapses to the default.
export function normalizeLocation(loc) {
  const tab = loc && KNOWN_TABS.has(loc.tab) ? loc.tab : DEFAULT_TAB;
  const out = { tab };
  const subs = KNOWN_SUBTABS[tab];
  if (loc && loc.subtab && subs && subs.has(loc.subtab)) {
    out.subtab = loc.subtab;
  }
  // A selection is a free-form entity id (e.g. a Processes run id). It is only
  // meaningful under a tab/subtab that has a detail view; today that is the
  // Processes "runs" subtab. Keep it only where it can apply.
  if (loc && loc.selection && tab === 'processes' && out.subtab === 'runs') {
    out.selection = String(loc.selection);
  }
  return out;
}

// locEquals is structural equality on the canonical location model. It backs
// duplicate suppression (re-selecting the current location is a no-op push —
// AC #4) and popstate no-op detection. Both sides are normalized so callers
// can pass raw locations without pre-canonicalizing.
export function locEquals(a, b) {
  const x = normalizeLocation(a);
  const y = normalizeLocation(b);
  return x.tab === y.tab && x.subtab === y.subtab && x.selection === y.selection;
}

// ---- The virtual stack ---------------------------------------------------
//
// A stack state is `{ entries: Location[], index: number }`. `index` points at
// the current entry; entries after it are the "forward" history. All reducer
// functions are pure: they return a NEW state (or the same reference when there
// is nothing to change) and never mutate their input.

// initialState seeds a stack with a single entry (defaults to the default
// location). index 0, no back, no forward.
export function initialState(loc) {
  return { entries: [normalizeLocation(loc || defaultLocation())], index: 0 };
}

// current returns the location at the stack's current index.
export function current(state) {
  return state.entries[state.index];
}

// push appends `loc` as the new current entry and returns the new state.
//
//   - Duplicate suppression: if `loc` equals the current location, the state is
//     returned UNCHANGED (same reference). This is AC #4 — repeated selection
//     of the current location must not add an entry.
//   - Forward-tail truncation: pushing while not at the tip discards every
//     entry after the current index first, matching browser semantics (a new
//     navigation after going Back erases the old forward history).
export function push(state, loc) {
  const next = normalizeLocation(loc);
  if (locEquals(current(state), next)) return state;
  const kept = state.entries.slice(0, state.index + 1);
  kept.push(next);
  return { entries: kept, index: kept.length - 1 };
}

// replaceCurrent swaps the current entry's location in place (no new entry,
// index unchanged), returning the same reference when it already equals `loc`.
// The adapter uses it to reconcile an INVOLUNTARY re-location — a tab the
// dashboard auto-left because it became hidden (e.g. the Terminals tab when the
// last terminal closes), or a stale-target fallback — which must correct the
// URL without forging a back/forward step the user never took.
export function replaceCurrent(state, loc) {
  const next = normalizeLocation(loc);
  if (locEquals(current(state), next)) return state;
  const entries = state.entries.slice();
  entries[state.index] = next;
  return { entries, index: state.index };
}

// go moves to an absolute index (used by the popstate adapter, which learns the
// target index from history.state). Out-of-range indices are clamped-ignored:
// an unknown index returns the state unchanged rather than corrupting it.
export function go(state, index) {
  if (!Number.isInteger(index) || index < 0 || index >= state.entries.length) return state;
  if (index === state.index) return state;
  return { entries: state.entries, index };
}

// indexOf returns the index of the LAST stack entry equal to `loc`, or -1 if
// none. The adapter uses it as a popstate fallback: if a history entry's
// `state` was clobbered by other code (so the stamped index is gone), we can
// still relocate WITHIN the existing stack by URL — preserving back/forward
// depth — instead of blowing the stack away. Last-match is the right pick for a
// browser Back, which moves toward the most recent occurrence. Defense-in-depth
// alongside every writer preserving history.state.
export function indexOf(state, loc) {
  const target = normalizeLocation(loc);
  for (let i = state.entries.length - 1; i >= 0; i--) {
    if (locEquals(state.entries[i], target)) return i;
  }
  return -1;
}

// resolvePopstate decides where a browser Back/Forward lands, from the popped
// URL's location `loc` and the `navIndex` stamped in that entry's history.state
// (pass -1 / non-integer when absent). It is the trust decision at the heart of
// popstate handling, extracted here so it is unit-testable without a DOM.
//
// A stamped navIndex is meaningful ONLY for the stack instance that created it.
// A reload builds a fresh (usually smaller) stack while older same-document
// history entries keep their pre-reload indices — so an old index can be
// coincidentally in range yet point at a different location. We therefore trust
// navIndex only when it is in range AND its entry actually equals the popped
// location; otherwise we relocate within the stack by URL (preserving depth),
// or reseed from the URL as a last resort. This keeps the active tab and URL
// consistent after any reload + traversal.
export function resolvePopstate(state, loc, navIndex) {
  const target = normalizeLocation(loc);
  if (Number.isInteger(navIndex) && navIndex >= 0 && navIndex < state.entries.length &&
      locEquals(state.entries[navIndex], target)) {
    return go(state, navIndex);
  }
  const idx = indexOf(state, target);
  return idx >= 0 ? go(state, idx) : initialState(target);
}

// ---- history.state persistence -------------------------------------------
//
// The whole virtual stack is stored in each entry's history.state, not just the
// current index. history.state survives a reload for the current entry, so on
// reload we can reconstruct the full stack (depth and all) instead of reseeding
// to a single entry. Entries are tiny ({tab, subtab?, selection?}), so even a
// long history is well within the structured-clone size the History API allows.

// NAV_STATE_VERSION tags the persisted shape. A future schema change bumps it so
// a stale payload (from an older deploy still in the tab's history) is ignored
// rather than misread — reviveState returns null and we seed fresh from the URL.
export const NAV_STATE_VERSION = 1;

// serializeStack produces the object to hand to pushState/replaceState.
export function serializeStack(state) {
  return { v: NAV_STATE_VERSION, navIndex: state.index, navStack: state.entries };
}

// reviveState reconstructs a stack from a persisted history.state payload `raw`
// after a reload, given the current URL's location `loc`. It returns the
// rebuilt stack ONLY when the payload is well-formed, current-version, its index
// addresses the stack, AND the addressed entry matches `loc` (so a stale or
// cross-URL payload can never place us on the wrong entry). Otherwise null — the
// caller seeds a fresh single-entry stack from the URL. Every entry is
// normalized, so a payload with an unknown tab degrades gracefully.
export function reviveState(raw, loc) {
  const target = normalizeLocation(loc);
  if (raw && raw.v === NAV_STATE_VERSION && Array.isArray(raw.navStack) && raw.navStack.length &&
      Number.isInteger(raw.navIndex) && raw.navIndex >= 0 && raw.navIndex < raw.navStack.length) {
    const entries = raw.navStack.map(normalizeLocation);
    if (locEquals(entries[raw.navIndex], target)) return { entries, index: raw.navIndex };
  }
  return null;
}

// ---- Path <-> location ---------------------------------------------------

// toPath serializes a location to an absolute dashboard pathname. The default
// location maps to "/" (the bare dashboard root), everything else to
// "/tab[/subtab[/selection]]". Query params (view-state) are NOT this module's
// concern — the DOM layer merges them onto the result.
export function toPath(loc) {
  const l = normalizeLocation(loc);
  const segs = [l.tab];
  if (l.subtab) segs.push(l.subtab);
  if (l.selection) segs.push(encodeURIComponent(l.selection));
  // The default bare view reads best as "/" rather than "/groups".
  if (segs.length === 1 && l.tab === DEFAULT_TAB) return '/';
  return '/' + segs.join('/');
}

// fromPath parses a dashboard pathname back into a canonical location. Unknown
// tabs / subtabs / stray trailing segments degrade to the nearest valid view
// via normalizeLocation, so a stale or hand-typed URL never yields an invalid
// location (AC #5). Both "/" and "/dashboard" mean the default location.
export function fromPath(pathname) {
  const clean = String(pathname || '').split('?')[0].split('#')[0];
  const parts = clean.split('/').filter(Boolean);
  if (parts.length === 0 || parts[0] === 'dashboard') return defaultLocation();
  const [tab, subtab, selection] = parts;
  return normalizeLocation({
    tab,
    subtab,
    selection: selection != null ? decodeURIComponent(selection) : undefined,
  });
}

// ---- Stale-target resolution --------------------------------------------

// resolveStale returns the nearest still-valid location for `loc`, given an
// `isValidSelection(selection, loc)` predicate the caller supplies from live
// snapshot data (e.g. "does this Processes run id still exist?"). If the
// location carries no selection, or the selection is still valid, `loc` is
// returned normalized and unchanged. Otherwise the selection is dropped,
// falling back to the parent list view (its tab/subtab) — never throwing,
// never leaving the stack pointing at a dead entity (AC #5 / #7).
//
// `isValidSelection` is optional: when omitted, any selection is treated as
// valid (no snapshot available to check against — leave the location as-is).
export function resolveStale(loc, isValidSelection) {
  const l = normalizeLocation(loc);
  if (!l.selection) return l;
  const ok = typeof isValidSelection === 'function'
    ? !!isValidSelection(l.selection, l)
    : true;
  if (ok) return l;
  const { selection, ...parent } = l; // drop the dead selection, keep tab/subtab
  return normalizeLocation(parent);
}
