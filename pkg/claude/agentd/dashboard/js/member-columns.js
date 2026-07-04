// member-columns.js — per-user show/hide for the Groups tab members table.
//
// The members table has grown a lot of columns (id, state, last, age, cwd,
// branch, role, description — and more arriving from other features), and
// not every column is useful to every operator. This lets each hideable
// column be toggled off from the "▾ view" popover. The choice persists in
// dashPrefs (server-backed SQLite, like the per-table sort state) so it
// survives the 2s refresh, reloads, browser profiles and daemon restarts —
// the random per-start port would reset a plain-localStorage copy.
//
// MEMBER_COLS (sort.js) is the single source of truth for the column set.
// This module reads the `hideable` (togglable) and `defaultHidden` (starts
// off) flags off it, and BOTH the header (sortHead, fed visibleMemberCols())
// and the body (memberRowHTML, which emits only visible keys) filter through
// the same store — so they can never drift out of alignment. A new column
// plugs in by adding one MEMBER_COLS entry: it then appears in the table, in
// this menu, and (if `hideable`) becomes toggleable, with no change here. A
// column that most users won't want (e.g. an optional link column) sets
// `defaultHidden: true` and starts hidden until the user opts it in.
//
// Storage is tri-state per column, stored MINIMALLY: the pref holds only the
// columns whose visibility DEVIATES from their own default. An absent key
// means "follow the column's default". So a later change to a column's
// default automatically reaches every user who never touched that column,
// and the deviation set doubles as the "how many columns differ from the
// default view" badge count.

import { dashPrefs } from './prefs.js';
import { MEMBER_COLS } from './sort.js';

// One JSON object under a single pref key: { <colKey>: true|false } where
// the value is the explicit hidden-state, present only when it deviates from
// the column's default (mirrors the sort state's single-key storage).
const COLS_LS_KEY = 'tclaude.dash.members.hidden';

// hideableMemberCols: the columns the menu offers to hide, in table order.
function hideableMemberCols() {
  return MEMBER_COLS.filter((c) => c.hideable);
}

// colByKey resolves a column spec by key (null if unknown / not a real col).
function colByKey(key) {
  return MEMBER_COLS.find((c) => c.key === key) || null;
}

// defaultHidden reports a column's out-of-the-box hidden state.
function defaultHidden(c) {
  return !!(c && c.defaultHidden);
}

// deviations reads the persisted deviation map ({ key: bool }). A missing or
// malformed value reads as "{}" (every column at its default) — the safe
// default that never surprises a fresh user.
function deviations() {
  try {
    const obj = JSON.parse(dashPrefs.getItem(COLS_LS_KEY));
    return obj && typeof obj === 'object' && !Array.isArray(obj) ? obj : {};
  } catch (_) {
    return {};
  }
}

// memberColHidden reports whether a column key is EFFECTIVELY hidden: the
// user's explicit choice if they made one, else the column's own default.
// Only hideable columns can be hidden; the controls + Name columns are
// always on, so they always answer false.
function memberColHidden(key) {
  const c = colByKey(key);
  if (!c || !c.hideable) return false;
  const dev = deviations();
  return Object.prototype.hasOwnProperty.call(dev, key) ? !!dev[key] : defaultHidden(c);
}

// setMemberColHidden persists one column's hidden state. It stores the
// value only when it DEVIATES from the column's default, and drops the entry
// when the user sets it back to the default (so the column reverts to
// following the default). Every write prunes the map to real hideable keys,
// so a removed/renamed column can't leave a stale entry behind.
function setMemberColHidden(key, hidden) {
  const c = colByKey(key);
  if (!c || !c.hideable) return;
  const valid = new Set(hideableMemberCols().map((x) => x.key));
  const dev = deviations();
  const next = {};
  // Carry forward existing deviations for still-valid columns.
  for (const [k, v] of Object.entries(dev)) {
    if (valid.has(k)) next[k] = !!v;
  }
  if (hidden === defaultHidden(c)) {
    delete next[key]; // back to following the default
  } else {
    next[key] = hidden;
  }
  dashPrefs.setItem(COLS_LS_KEY, JSON.stringify(next));
}

// visibleMemberCols returns MEMBER_COLS minus the effectively-hidden ones —
// the exact, ordered column list that BOTH the header and every row render
// through.
function visibleMemberCols() {
  return MEMBER_COLS.filter((c) => !(c.hideable && memberColHidden(c.key)));
}

// memberColDeviationCount counts the hideable columns whose current
// visibility differs from their default — feeds the "▾ view" badge so a
// glance shows the table's columns have been customised (a column sitting at
// its default, hidden or shown, is NOT a deviation and doesn't count).
function memberColDeviationCount() {
  const valid = new Set(hideableMemberCols().map((c) => c.key));
  return Object.keys(deviations()).filter((k) => valid.has(k)).length;
}

export {
  hideableMemberCols,
  memberColHidden,
  setMemberColHidden,
  visibleMemberCols,
  memberColDeviationCount,
};
