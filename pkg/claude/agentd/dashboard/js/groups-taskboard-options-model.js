// groups-taskboard-options-model.js -- the Board options drawer's model
// (docs/taskboard.md "Board options"): no DOM, no requests.
//
// The daemon answers GET /v1/groups/{g}/taskboard/options with
//
//   { options,    what the board itself sets (nested: idle, events)
//     resolved,   what is in force, each value { value, source }
//     inherited,  what each value would be if the board set nothing
//     globals, version, updated_by, updated_at, keys }
//
// and the drawer saves with a PATCH of the keys it changed ({set, reset})
// at the version it read. The drawer edits a flat draft, { key: value }, keyed by the CLI's
// option keys (idle.grace, events.comment.to, …): a key in the draft is set
// on the board, a key absent is inherited. Provenance is then where a value
// comes from: "board" for a drafted key, else the inherited source (default
// or global).

// The recipients a stall may be told to, and an event message's, in the
// daemon's order (boardopts.idleTellRecipients / EventRecipients).
export const IDLE_TELL = ['assigner', 'owners', 'operator'];
export const EVENT_TO = ['assignee', 'assigner', 'creator', 'owners', 'operator'];

export const RECIPIENT_LABEL = {
  assignee: 'Assignee', assigner: 'Assigner', creator: 'Creator', owners: 'Owners', operator: 'Operator',
};

export const RECIPIENT_TITLE = {
  assignee: 'The agent that holds the task (for an unassign, the one that held it)',
  assigner: 'Whoever assigned the task: an agent by inbox mail, the operator in Messages',
  creator: 'The agent that created the task',
  owners: "The group's owner agents",
  operator: "The operator's Messages",
};

// The board's events in display order, each with a short label for the
// drawer's table; the daemon's own label is its tooltip. The daemon's list
// (resolved.events) is the authority: an event this table does not know
// shows under the daemon's label.
export const EVENTS = [
  ['assigned', 'Assigned'],
  ['started', 'Started'],
  ['unassigned', 'Unassigned'],
  ['reopened', 'Reopened'],
  ['moved_away', 'Moved to another board'],
  ['comment', 'Comment added'],
  ['report_done', 'Report: done'],
  ['report_failed', 'Report: failed'],
  ['unblocked', 'Dependency closed'],
  ['epic_close', 'Epic done: close it?'],
  ['limit_reached', 'Limit reached'],
];
const EVENT_SHORT = Object.fromEntries(EVENTS);

// eventRows are the events the board's node knows, in its order:
// [{ name, label, title }].
export function eventRows(view) {
  const sent = view?.resolved?.events;
  if (!Array.isArray(sent) || !sent.length) return EVENTS.slice(0, 10).map(([name, label]) => ({ name, label, title: '' }));
  return sent.map((event) => ({ name: event.name, label: EVENT_SHORT[event.name] || event.label || event.name, title: event.label || '' }));
}

// EVENT_DEFAULT_TO is each event's built-in recipients. A daemon that sends
// `inherited` is the authority; this covers a linked node on an older
// tclaude that sends only `resolved`, where a board that sets an event's
// recipients hides the default.
const EVENT_DEFAULT_TO = {
  assigned: ['assignee'], started: ['assignee'], unassigned: ['assignee'], reopened: ['assignee'], moved_away: ['assignee'],
  comment: ['assignee', 'creator'], report_done: ['assigner'], report_failed: ['assigner'], unblocked: ['assignee'], epic_close: ['creator'],
  limit_reached: ['assignee', 'assigner'],
};

export const IDLE_KEYS = ['idle.enabled', 'idle.grace', 'idle.nudge', 'idle.tell', 'idle.check_columns'];
const IDLE_FIELD = { 'idle.enabled': 'enabled', 'idle.grace': 'grace', 'idle.nudge': 'nudge', 'idle.tell': 'tell', 'idle.check_columns': 'check_columns' };

export const eventKey = (name, field) => `events.${name}.${field}`;

// The drawer's tabs and the sections a deep link (#idle) may name. Limits
// arrives with the per-board limits (tk_68919cad PR 3).
export const TABS = [['triggers', 'Triggers'], ['columns', 'Columns']];
export const SECTION_TAB = { idle: 'triggers', events: 'triggers', triggers: 'triggers', columns: 'columns' };

export const DEFAULT_GRACE = '5m';
const MIN_GRACE_MS = 60_000;
const MAX_GRACE_MS = 168 * 3_600_000;

// flatOptions is a stored document as a draft: one entry per key it sets.
export function flatOptions(options) {
  const out = {};
  const idle = options?.idle || {};
  for (const key of IDLE_KEYS) {
    const value = idle[IDLE_FIELD[key]];
    if (value !== undefined && value !== null && value !== '') out[key] = Array.isArray(value) ? [...value] : value;
  }
  for (const [name, event] of Object.entries(options?.events || {})) {
    if (!event) continue;
    if (typeof event.enabled === 'boolean') out[eventKey(name, 'enabled')] = event.enabled;
    if (Array.isArray(event.to)) out[eventKey(name, 'to')] = [...event.to];
  }
  return out;
}

function idleValue(block, key) {
  return block?.idle?.[IDLE_FIELD[key]];
}

function eventValue(block, key) {
  const [, name, field] = key.split('.');
  return (block?.events || []).find((event) => event.name === name)?.[field];
}

// resolvedOf is the value in force for key, as the daemon read it.
export function resolvedOf(view, key) {
  return (key.startsWith('idle.') ? idleValue(view?.resolved, key) : eventValue(view?.resolved, key)) || null;
}

// inheritedOf is what key falls back to when the board does not set it:
// { value, source } with source default or global.
export function inheritedOf(view, key) {
  const sent = key.startsWith('idle.') ? idleValue(view?.inherited, key) : eventValue(view?.inherited, key);
  if (sent) return sent;
  const resolved = resolvedOf(view, key);
  if (resolved && resolved.source !== 'board') return resolved;
  const globals = view?.globals || {};
  switch (key) {
    case 'idle.enabled':
      return typeof globals.idle_check === 'boolean' ? { value: globals.idle_check, source: 'global' } : { value: false, source: 'default' };
    case 'idle.grace':
      return globals.idle_grace && globals.idle_grace !== '0' ? { value: globals.idle_grace, source: 'global' } : { value: DEFAULT_GRACE, source: 'default' };
    case 'idle.nudge': return { value: true, source: 'default' };
    case 'idle.tell': return { value: ['assigner'], source: 'default' };
    case 'idle.check_columns': return { value: [], source: 'default' };
    default: {
      const [, name, field] = key.split('.');
      return field === 'enabled' ? { value: true, source: 'default' } : { value: [...(EVENT_DEFAULT_TO[name] || [])], source: 'default' };
    }
  }
}

// effectiveOf is what key will be once the draft is saved.
export function effectiveOf(view, draft, key) {
  if (draft && Object.hasOwn(draft, key)) return { value: draft[key], source: 'board' };
  return inheritedOf(view, key);
}

function sameValue(a, b) {
  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
    const sorted = (list) => [...list].sort();
    return sorted(a).every((value, i) => value === sorted(b)[i]);
  }
  return a === b;
}

// ordered keeps a recipient or column list in its canonical order, as the
// daemon stores it.
function ordered(value, order) {
  return order.filter((item) => value.includes(item));
}

// setOption sets key in the draft. A key the saved board does not set, put
// back to the value it inherits, is left unset again rather than pinned, so
// toggling a value there and back is no change.
export function setOption(view, saved, draft, key, value, columns = []) {
  let next = value;
  if (key === 'idle.tell') next = ordered(value, IDLE_TELL);
  else if (key === 'idle.check_columns') next = ordered(value, columns);
  else if (key.endsWith('.to')) next = ordered(value, EVENT_TO);
  const out = { ...draft };
  if (!Object.hasOwn(saved || {}, key) && sameValue(next, inheritedOf(view, key).value)) delete out[key];
  else out[key] = next;
  return out;
}

// resetOption drops key (or every key under a prefix such as "events.")
// from the draft: it inherits again once saved.
export function resetOption(draft, key) {
  const out = { ...draft };
  for (const k of Object.keys(out)) {
    if (k === key || (key.endsWith('.') && k.startsWith(key))) delete out[k];
  }
  return out;
}

// optionText is a draft value as the options' text form (the CLI's
// `options set key=value`): on / off, a comma list or none, a duration.
export function optionText(value) {
  if (value === true) return 'on';
  if (value === false) return 'off';
  if (Array.isArray(value)) return value.length ? value.join(',') : 'none';
  return String(value ?? '');
}

// patchOf is the change a draft makes, as the PATCH the drawer saves with:
// { set: { key: text }, reset: [key] }. Only the keys the drawer changed are
// sent, so a part of the board's options it does not show (one a newer node
// added) stays as it is. columns are the board's custom column slugs now: a
// column removed while the drawer was open drops out of idle.check_columns.
export function patchOf(saved, draft, columns = null) {
  const next = { ...draft };
  if (columns && Array.isArray(next['idle.check_columns'])) {
    next['idle.check_columns'] = next['idle.check_columns'].filter((slug) => columns.includes(slug));
  }
  const set = {};
  const reset = [];
  for (const key of changedKeys(saved, next)) {
    if (Object.hasOwn(next, key)) set[key] = optionText(next[key]);
    else reset.push(key);
  }
  return { set, reset };
}

// changedKeys are the keys a draft changes against what was read.
export function changedKeys(saved, draft) {
  const keys = new Set([...Object.keys(saved || {}), ...Object.keys(draft || {})]);
  return [...keys].filter((key) => Object.hasOwn(saved || {}, key) !== Object.hasOwn(draft || {}, key)
    || (Object.hasOwn(draft || {}, key) && !sameValue(saved[key], draft[key])));
}

// rebaseDraft carries a draft's changes onto a newer read of the board (a
// save refused as stale): the newer document, then this draft's own edits.
export function rebaseDraft(oldSaved, draft, newSaved) {
  const out = { ...newSaved };
  for (const key of changedKeys(oldSaved, draft)) {
    if (Object.hasOwn(draft, key)) out[key] = draft[key];
    else delete out[key];
  }
  return out;
}

const GRACE_UNITS = { ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };

// graceMs reads a Go duration ("10m", "1h30m") as milliseconds; NaN when it
// is not one.
export function graceMs(text) {
  const s = String(text || '').trim();
  if (!s) return NaN;
  if (s === '0') return 0;
  const re = /(\d+(?:\.\d+)?|\.\d+)(ns|us|µs|ms|s|m|h)/gy;
  let total = 0;
  let at = 0;
  for (let match = re.exec(s); match; match = re.exec(s)) {
    total += Number(match[1]) * GRACE_UNITS[match[2]];
    at = re.lastIndex;
  }
  return at === s.length ? total : NaN;
}

// graceError is why the daemon would refuse a grace, or ''.
export function graceError(text) {
  const ms = graceMs(text);
  if (Number.isNaN(ms)) return 'A duration such as 10m or 1h30m';
  if (ms < MIN_GRACE_MS || ms > MAX_GRACE_MS) return 'Between 1m and 168h; turn the check off with its switch';
  return '';
}

// draftErrors are the draft's invalid keys, { key: why }.
export function draftErrors(draft) {
  const out = {};
  if (Object.hasOwn(draft || {}, 'idle.grace')) {
    const why = graceError(draft['idle.grace']);
    if (why) out['idle.grace'] = why;
  }
  return out;
}

// sourceText names where a value comes from, as the CLI's options show does.
export function sourceText(source) {
  if (source === 'board') return 'set on this board';
  if (source === 'global') return 'global';
  return 'default';
}

// valueText is a value as the drawer shows it beside its source.
export function valueText(value) {
  if (Array.isArray(value)) return value.length ? value.join(', ') : 'no one';
  if (value === true) return 'on';
  if (value === false) return 'off';
  return String(value ?? '');
}

// optionsSummary is the section line's summary of what a board's options
// change from the inherited behaviour, from its context entry's options
// ({ version, idle_active, idle_globally_off, idle_source, set }): one chip
// per part, each with the section of the drawer it opens.
export function optionsSummary(options) {
  if (!options) return [];
  const set = Array.isArray(options.set) ? options.set : [];
  const out = [];
  const idleSet = set.filter((key) => key.startsWith('idle.'));
  if (options.idle_globally_off && set.includes('idle.enabled')) {
    out.push({ section: 'idle', text: 'idle check off globally', title: 'This board turns the idle check on, but taskboard.idle_grace "0" turns it off on every board' });
  } else if (options.idle_active) {
    out.push({ section: 'idle', text: 'idle check on', title: `The idle check runs on this board (${options.idle_source === 'board' ? 'set on this board' : sourceText(options.idle_source)})` });
  } else if (idleSet.length) {
    out.push({ section: 'idle', text: 'idle check off', title: 'This board sets idle check options: ' + idleSet.join(', ') });
  }
  const events = new Set(set.filter((key) => key.startsWith('events.')).map((key) => key.split('.')[1]));
  if (events.size) {
    out.push({ section: 'events', text: `${events.size} ${events.size === 1 ? 'message' : 'messages'} changed`,
      title: `This board changes the messages it sends for: ${[...events].join(', ')}` });
  }
  return out;
}

// A board's options route value: the group as its own node names it, after
// the node's id and a colon for a linked node's board
// (?board-options=inst_x:crew). A local group whose own name would read as
// that (inst_x:…) is written after a bare colon.
const NODE_PREFIX = /^inst_[0-9a-z]+:/;

export function formatBoardOptionsRoute({ group, node = '' }) {
  const name = String(group || '');
  if (node) return `${node}:${name}`;
  return NODE_PREFIX.test(name) ? `:${name}` : name;
}

export function parseBoardOptionsRoute(value) {
  const text = String(value || '');
  if (text.startsWith(':')) return text.length > 1 ? { node: '', group: text.slice(1) } : null;
  if (NODE_PREFIX.test(text)) {
    const colon = text.indexOf(':');
    const group = text.slice(colon + 1);
    return group ? { node: text.slice(0, colon), group } : null;
  }
  return text ? { node: '', group: text } : null;
}

// sectionOfHash is the drawer section a link's #fragment names, or ''.
export function sectionOfHash(hash) {
  const section = String(hash || '').replace(/^#/, '');
  return Object.hasOwn(SECTION_TAB, section) ? section : '';
}
