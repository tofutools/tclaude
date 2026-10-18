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
// checkboxes, text filters). Page-global view-state (the theme, a peer or
// fused node scope) is the DOM layer's to merge onto every URL. View-state
// that belongs to one location — a tab's search or page, listed in
// TAB_PARAMS — rides on the location as `params`, so Back/Forward and a deep
// link restore it with the rest of the location.

// DEFAULT_TAB is the location the dashboard falls back to for an empty/unknown
// path and the ultimate parent for stale-target resolution. It mirrors the
// dashboard's default active tab (#tab-groups is `.active` at rest).
export const DEFAULT_TAB = 'groups';

// KNOWN_TABS is the set of top-level nav tabs this router will PARSE from a
// path. It is intentionally the widest of three related sets:
//   KNOWN_TABS (parse-tolerant, here)
//     ⊇ ROUTABLE_TABS (js/nav-history.js — tabs we actually push a URL for)
//     ⊇ dashboardAppTabs (dashboard.go — paths the server serves)
// Terminals IS routed and server-served (its own handler covers /terminals and
// the /terminals/<agent-id> subtree, which is why it is absent from
// dashboardAppTabs); Vegas lives in KNOWN_TABS only so a hand-typed URL
// degrades gracefully. Keep the three in step when adding or removing a tab,
// and in sync with the nav buttons in dashboard.html (data-tab=...).
// An unknown first segment parses back to the default location
// rather than an invalid tab, which keeps a stale/typo'd URL from breaking
// navigation (AC #5).
export const KNOWN_TABS = new Set([
  'groups', 'terminals', 'jobs', 'processes', 'plugins',
  'access', 'messages', 'usage', 'activity', 'config', 'vegas',
  'map',
  'fleet-admin',
]);

// KNOWN_SUBTABS enumerates the valid second-segment values per tab that has a
// sub-navigation. These tabs have one today:
//   - Groups: Members (bare), the Route map, the Agents and Processes
//     lenses, and the single-run focus (runs).
//   - Access: the segmented control (permissions / slugs / sudo).
//   - Automations (historical internal tab id: jobs): kind views.
//   - Processes: the template library/editor. Processes renders as a subtab
//     of Automations (/automations/processes/...) but keeps its own internal
//     tab id, so its location model is unchanged; only toPath/fromPath know
//     about the nesting.
//   - Usage: the quota forecasts and the costs views; the bare tab is the
//     Overview that summarises both.
//   - Activity: audit / logs / performance (formerly three top-level tabs).
//   - Config: the settings rail's sections. Keep in step with CONFIG_PANELS
//     in config-form-markup.js.
//   - Fleet config (fleet-admin): its sections; the bare tab is the landing
//     (the setup guide until setup is done, else Peers). Keep in step with
//     SUB_PAGES in fleet-admin-island.js.
// A second segment not listed here is dropped on parse (fall back to the tab's
// default view) so a renamed/removed subtab degrades gracefully.
export const KNOWN_SUBTABS = {
  groups: new Set(['members', 'routes', 'agents', 'processes', 'runs']),
  access: new Set(['permissions', 'slugs', 'sudo']),
  jobs: new Set(['exports', 'cron-jobs', 'standing-orders', 'triggers']),
  processes: new Set(['templates']),
  usage: new Set(['quota', 'costs']),
  activity: new Set(['audit', 'logs', 'performance']),
  config: new Set([
    'appearance', 'terminals', 'notifications', 'agents', 'context', 'costs',
    'assistants', 'daemon', 'logs', 'experimental', 'advanced',
  ]),
  'fleet-admin': new Set([
    'setup', 'peers', 'harnesses', 'run', 'spawns', 'invites', 'grants', 'models',
    'profiles', 'offers', 'jobs', 'moves', 'boards', 'audit', 'hub',
  ]),
};

// LEGACY_TAB_LOCATIONS maps retired top-level tabs to the location that now
// shows them. Audit, Logs and Debug merged into Activity; their old paths (and
// the /?tab=<name> alias) stay reload-safe and heal to the canonical
// /activity/<subtab> in place, like /jobs → /automations. Costs likewise
// became the Usage tab's costs subtab.
export const LEGACY_TAB_LOCATIONS = Object.freeze({
  costs: Object.freeze({ tab: 'usage', subtab: 'costs' }),
  audit: Object.freeze({ tab: 'activity', subtab: 'audit' }),
  logs: Object.freeze({ tab: 'activity', subtab: 'logs' }),
  debug: Object.freeze({ tab: 'activity', subtab: 'performance' }),
});

// SELECTABLE_SUBTABS enumerates the tab/subtab pairs whose view can address a
// SINGLE entity, so a third path segment carries meaning there:
//   - processes/templates/<template-id> — the template editor is open on it.
//   - groups/routes/<route-id> — the Route map has that route selected.
//   - groups/runs/<group> — the single-run focus, on a run of that group.
// A selection parsed under any other tab/subtab is dropped.
export const SELECTABLE_SUBTABS = {
  groups: new Set(['routes', 'runs']),
  processes: new Set(['templates']),
};

// SELECTABLE_TABS enumerates tabs whose selection hangs directly off the TAB,
// with no subtab in between — so the entity lives in the SECOND path segment:
//   - terminals/<agent-id> — the Terminals tab is viewing that agent's terminal.
//   - messages/<folder> — the Messages tab has that folder open.
// These tabs must not also appear in KNOWN_SUBTABS: a tab cannot have both a
// subtab and a tab-level selection, because both would claim segment two.
export const SELECTABLE_TABS = new Set(['terminals', 'messages']);

// ITEM_TABS enumerates tab-level-selection tabs whose selection can itself
// have one entry open, carried as `item` in the THIRD path segment:
//   - messages/<folder>/<message-id> — that message is open in the reader.
// An item without a selection, or under any other tab, is dropped.
export const ITEM_TABS = new Set(['messages']);

// ITEM_SUBTABS is ITEM_TABS for selections under a subtab, the item riding
// in the FOURTH path segment:
//   - groups/runs/<group>/<run-id> — the single-run focus on that run.
export const ITEM_SUBTABS = {
  groups: new Set(['runs']),
};

// TAB_PARAMS enumerates, per tab, the query params that are part of a
// location rather than of the page: two entries that differ only in them are
// distinct history steps. Order here is the order they are written in a URL.
// Any other param on a parsed URL is dropped from the location (the DOM layer
// keeps the page-global ones).
//   - messages: q (the message search), from (the exact sender a Groups
//     notification link narrows to), page (1-based, omitted for page 1),
//     box (the folder filter).
//   - groups: group (the group the Route map shows).
export const TAB_PARAMS = {
  messages: ['q', 'from', 'page', 'box'],
  groups: ['group'],
};

// normalizeParams keeps the tab's known params with a non-empty value, as
// strings, in TAB_PARAMS order; undefined when none survive.
function normalizeParams(tab, params) {
  const keys = TAB_PARAMS[tab];
  if (!keys || !params || typeof params !== 'object') return undefined;
  const out = {};
  for (const key of keys) {
    const value = params[key];
    if (value == null) continue;
    const str = String(value);
    if (str === '' || (key === 'page' && !(Number(str) > 1))) continue;
    out[key] = key === 'page' ? String(Math.floor(Number(str))) : str;
  }
  return Object.keys(out).length ? out : undefined;
}

// defaultLocation returns a fresh copy of the fallback location. Returned as a
// new object each call so callers can never alias/mutate a shared default.
export function defaultLocation() {
  return { tab: DEFAULT_TAB };
}

// normalizeLocation coerces an arbitrary input into a canonical location shape
// `{ tab, subtab?, selection?, item?, params? }`, dropping empty/unknown pieces. Optional keys
// are OMITTED (not set to undefined) so structural equality via locEquals and
// JSON round-trips stay stable. An unknown tab collapses to the default.
export function normalizeLocation(loc) {
  // A retired tab (a history stack persisted before the Activity merge) heals
  // to its new home rather than collapsing to the default.
  if (loc && Object.hasOwn(LEGACY_TAB_LOCATIONS, loc.tab)) loc = LEGACY_TAB_LOCATIONS[loc.tab];
  const tab = loc && KNOWN_TABS.has(loc.tab) ? loc.tab : DEFAULT_TAB;
  const out = { tab };
  const subs = KNOWN_SUBTABS[tab];
  if (loc && loc.subtab && subs && subs.has(loc.subtab)) {
    out.subtab = loc.subtab;
  }
  // A selection is a free-form entity id (a Processes template id, a Terminals
  // agent id). It is only meaningful where the view has a detail form: under a
  // tab/subtab pair (SELECTABLE_SUBTABS) or directly under a subtab-less tab
  // (SELECTABLE_TABS). Keep it only where it can apply.
  const selectable = out.subtab
    ? !!SELECTABLE_SUBTABS[tab]?.has(out.subtab)
    : SELECTABLE_TABS.has(tab);
  if (loc && loc.selection && selectable) {
    out.selection = String(loc.selection);
    const itemable = out.subtab ? !!ITEM_SUBTABS[tab]?.has(out.subtab) : ITEM_TABS.has(tab);
    if (loc.item != null && loc.item !== '' && itemable) out.item = String(loc.item);
  }
  const params = normalizeParams(tab, loc && loc.params);
  if (params) out.params = params;
  return out;
}

// locEquals is structural equality on the canonical location model. It backs
// duplicate suppression (re-selecting the current location is a no-op push —
// AC #4) and popstate no-op detection. Both sides are normalized so callers
// can pass raw locations without pre-canonicalizing.
export function locEquals(a, b) {
  const x = normalizeLocation(a);
  const y = normalizeLocation(b);
  return x.tab === y.tab && x.subtab === y.subtab && x.selection === y.selection && x.item === y.item
    && toQuery(x) === toQuery(y);
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
// to a single entry. Entries are tiny ({tab, subtab?, selection?, item?, params?}), so even a
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

// toQuery serializes a location's params (TAB_PARAMS) as a query string
// without the leading "?" — "" when it has none.
export function toQuery(loc) {
  const l = normalizeLocation(loc);
  return l.params ? new URLSearchParams(l.params).toString() : '';
}

// toPath serializes a location to an absolute dashboard pathname. The default
// location maps to "/" (the bare dashboard root), everything else to
// "/tab[/subtab][/selection[/item]]". Query params (view-state) are NOT this module's
// concern — the DOM layer merges them onto the result.
export function toPath(loc) {
  const l = normalizeLocation(loc);
  // "jobs" remains the historical internal tab/API identifier, but the
  // user-facing workspace and canonical browser route are Automations.
  if (l.tab === 'jobs') {
    return '/automations' + (l.subtab ? '/' + l.subtab : '');
  }
  // Processes is presented as an Automations subtab, so its canonical route
  // nests under /automations; the internal tab id stays "processes".
  const segs = l.tab === 'processes' ? ['automations', 'processes'] : [l.tab];
  if (l.subtab) segs.push(l.subtab);
  if (l.selection) segs.push(encodeSegment(l.selection));
  if (l.item) segs.push(encodeSegment(l.item));
  // The default bare view reads best as "/" rather than "/groups".
  if (segs.length === 1 && l.tab === DEFAULT_TAB) return '/';
  return '/' + segs.join('/');
}

// encodeSegment percent-encodes an entity id for one path segment. A colon is
// legal in a path segment and reads better left alone (Messages group folders
// are "group:<name>").
function encodeSegment(value) {
  return encodeURIComponent(value).replace(/%3A/gi, ':');
}

// decodeSelection percent-decodes a path segment, tolerating a malformed one.
// decodeURIComponent THROWS on a broken escape (e.g. /processes/templates/%zz),
// and fromPath runs inside initNavHistory and the popstate handler — so an
// uncaught URIError there would take out router boot or wedge browser
// navigation on a hand-typed or truncated URL. A segment we cannot decode is
// used verbatim: it simply won't match a real entity, which the caller's
// stale-target handling already copes with.
function decodeSelection(segment) {
  if (segment == null) return undefined;
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

// fromPath parses a dashboard pathname back into a canonical location, taking
// the tab's params (TAB_PARAMS) from `search` — or, when that is omitted, from
// a query string left on `pathname` itself. Unknown
// tabs / subtabs / stray trailing segments degrade to the nearest valid view
// via normalizeLocation, so a stale or hand-typed URL never yields an invalid
// location (AC #5). Both "/" and "/dashboard" mean the default location.
export function fromPath(pathname, search) {
  const [pathPart, inlineQuery = ''] = String(pathname || '').split('#')[0].split('?');
  const query = new URLSearchParams(search ?? inlineQuery);
  const params = {};
  for (const [key, value] of query) params[key] = value;
  const parts = pathPart.split('/').filter(Boolean);
  if (parts.length === 0 || parts[0] === 'dashboard') return defaultLocation();
  let [tab, second, third, fourth] = parts;
  // /jobs is retained as a reload-safe legacy alias. toPath always emits the
  // canonical /automations route, so init/popstate heals old URLs in place.
  // /automations/processes/... is the Processes view nested under
  // Automations. The pre-nesting /processes/... route parses the same way and
  // is healed in place to the canonical form.
  if (tab === 'automations' && second === 'processes') {
    [tab, second, third, fourth] = ['processes', third, parts[3], parts[4]];
  } else if (tab === 'automations') tab = 'jobs';
  // /fleet was the merged all-nodes view; it is now Groups with every node
  // fused (fleet-state.js reads the path for that), healed in place.
  if (tab === 'fleet') return defaultLocation();
  if (Object.hasOwn(LEGACY_TAB_LOCATIONS, tab)) return normalizeLocation({ tab });
  // On a tab-level-selection tab there is no subtab, so segment two IS the
  // entity (/terminals/<agent-id>) — mirroring how toPath emits it. Reading it
  // as a subtab would drop it on the floor, since terminals has no
  // KNOWN_SUBTABS entry to match against. Segment three is its open item
  // (/messages/<folder>/<message-id>), kept only where ITEM_TABS allows.
  if (SELECTABLE_TABS.has(tab)) {
    return normalizeLocation({ tab, selection: decodeSelection(second), item: decodeSelection(third), params });
  }
  return normalizeLocation({ tab, subtab: second, selection: decodeSelection(third), item: decodeSelection(fourth), params });
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
  const { selection, item, ...parent } = l; // drop the dead selection (and its item), keep tab/subtab/params
  return normalizeLocation(parent);
}
