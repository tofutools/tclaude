// process-worklist-core.js — pure view/format logic for the Processes tab's
// Worklist sub-view (TCL-297). No DOM, no fetch, no imports: everything here
// is a deterministic function of (items, now), so jstest/process-worklist.test.mjs
// can drive it under plain Node. process-worklist.js owns the DOM half.
//
// The item shape mirrors the /v1/process/worklist REST contract (TCL-295):
//   {id, run, node, attempt, kind, assignee, status, createdAt?, dueAt?,
//    nudge?: {lastContactAt?, nextContactAt?, budgetUsed, budgetMax,
//             escalationTarget, paused}, summary, availableActions?, links}
// Blocked items may omit createdAt/dueAt/nudge until TCL-303 persists them —
// every formatter here renders an honest em-dash for absent data, never a
// fabricated value.

// The dashboard's human operator as the backend spells it (the action route
// stamps human callers as this actor, and human obligations are assigned to it).
export const OPERATOR_ASSIGNEE = 'human:operator';

// A due/overdue horizon of 24h: anything due within it counts as "due soon".
export const DUE_SOON_MS = 24 * 60 * 60 * 1000;
// "Recently changed" window: items created (or resolved — see viewItems) in
// the last 24h.
export const RECENT_MS = 24 * 60 * 60 * 1000;

// The seven §8c views, in display order. Keys double as the chip data-attr
// and the persisted dashPref value.
export const WORKLIST_VIEWS = [
  { key: 'my-work', label: 'My work' },
  { key: 'waiting-on', label: 'Waiting on' },
  { key: 'due', label: 'Due / overdue' },
  { key: 'blocked', label: 'Blocked' },
  { key: 'decision', label: 'Needs decision' },
  { key: 'review', label: 'Needs review' },
  { key: 'recent', label: 'Recently changed' },
];

// Kind presentation: glyph + text label together (never color-only).
export const KIND_META = {
  'human-wait': { glyph: '⏳', label: 'human wait' },
  'decision-needed': { glyph: '⚖', label: 'decision' },
  'review-needed': { glyph: '🔍', label: 'review' },
  'blocked': { glyph: '⛔', label: 'blocked' },
  'agent-obligation': { glyph: '🤖', label: 'agent' },
};

export function kindMeta(kind) {
  return KIND_META[kind] || { glyph: '•', label: kind || 'unknown' };
}

// Destructive worklist actions get a confirm step before the POST. Matched
// case-insensitively because decision edges advertise their own spelling
// ("Reject" vs "reject").
const DESTRUCTIVE_ACTIONS = new Set(['reject', 'cancel', 'skip']);

export function isDestructiveAction(action) {
  return DESTRUCTIVE_ACTIONS.has(String(action || '').toLowerCase());
}

// advertisedAction resolves a clicked action back to the EXACT spelling the
// item advertises (the API matches case-insensitively but we send the
// advertised form). Returns '' when the action isn't available at all.
export function advertisedAction(item, action) {
  const want = String(action || '').toLowerCase();
  for (const a of item.availableActions || []) {
    if (String(a).toLowerCase() === want) return a;
  }
  return '';
}

// buildWorklistAction assembles the exact request the action endpoint expects:
// the advertised action spelling, the operator's comment (required by the API),
// and a caller-supplied fresh idempotency key (one per click — a retry of the
// SAME click reuses the key, a new click mints a new one).
// Returns null when the action isn't advertised on the item or the comment is
// blank, so the caller can surface the problem instead of collecting a 4xx.
export function buildWorklistAction(item, action, comment, idempotencyKey) {
  const advertised = advertisedAction(item, action);
  const trimmed = String(comment || '').trim();
  if (!advertised || !trimmed || !idempotencyKey) return null;
  return {
    path: `/v1/process/worklist/${encodeURIComponent(item.id)}/action`,
    body: { action: advertised, comment: trimmed, idempotencyKey },
  };
}

// isActionable: can the operator act on this item from the worklist surface?
// Agent obligations are reported through the run/node report route (the action
// endpoint 409s them), so they render without buttons.
export function isActionable(item) {
  return item.status === 'pending' && item.kind !== 'agent-obligation'
    && (item.availableActions || []).length > 0;
}

// actionableCount drives the Worklist sub-nav badge.
export function actionableCount(items) {
  return (items || []).filter(isActionable).length;
}

function timeMs(iso) {
  if (!iso) return NaN;
  const t = Date.parse(iso);
  return Number.isNaN(t) ? NaN : t;
}

// dueBucket classifies an item's deadline: 'overdue' | 'due-soon' | '' (no
// dueAt, or comfortably in the future).
export function dueBucket(item, now) {
  const due = timeMs(item.dueAt);
  if (Number.isNaN(due)) return '';
  if (due <= now) return 'overdue';
  if (due - now <= DUE_SOON_MS) return 'due-soon';
  return '';
}

// coarseSpan renders a millisecond span as the dashboard's coarse s/m/h/d
// figure (mirrors helpers.relTime, but parameterized on `now` so it is
// deterministic under test).
export function coarseSpan(ms) {
  const sec = Math.max(0, Math.floor(ms / 1000));
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h';
  return Math.floor(sec / 86400) + 'd';
}

// fmtAge: "3h ago" for createdAt; honest em-dash when absent (blocked items
// don't carry createdAt until TCL-303).
export function fmtAge(iso, now) {
  const t = timeMs(iso);
  if (Number.isNaN(t)) return '—';
  return coarseSpan(now - t) + ' ago';
}

// fmtDue: "⚠ overdue 2h" / "in 3h" for dueAt; em-dash when absent.
// The ⚠ glyph carries the overdue signal alongside the CSS tint (never
// color-only).
export function fmtDue(iso, now) {
  const t = timeMs(iso);
  if (Number.isNaN(t)) return '—';
  if (t <= now) return '⚠ overdue ' + coarseSpan(now - t);
  return 'in ' + coarseSpan(t - now);
}

// fmtClock: local wall-clock "10:14" for the nudge schedule line (the spec's
// example format). Zero-pad both fields.
export function fmtClock(iso) {
  const t = timeMs(iso);
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  const pad = (n) => String(n).padStart(2, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// actorLabel renders an ActorRef ("human:operator", "agent:agt_…", "role:…")
// as glyph + name. The glyph accompanies text, never replaces it.
export function actorLabel(ref) {
  const s = String(ref || '');
  if (!s) return '— unassigned';
  const at = s.indexOf(':');
  if (at > 0) {
    const kind = s.slice(0, at), name = s.slice(at + 1);
    if (kind === 'human') return '👤 ' + name;
    if (kind === 'agent') return '🤖 ' + name;
    if (kind === 'role') return '🎭 ' + name;
  }
  return s;
}

// nudgeLine renders the visible contact schedule — the point of the surface:
//   "last nudged 10:14 · next 10:44 · 2/5 · escalates to 👤 oncall"
// plus a leading "⏸ paused" marker when the schedule is paused. Absent
// fields are simply omitted (never fabricated); a fully-empty schedule
// renders ''.
export function nudgeLine(nudge) {
  if (!nudge) return '';
  const parts = [];
  if (nudge.paused) parts.push('⏸ paused');
  const last = fmtClock(nudge.lastContactAt);
  parts.push(last ? 'last nudged ' + last : 'not yet nudged');
  const next = fmtClock(nudge.nextContactAt);
  if (next) parts.push('next ' + next);
  if (nudge.budgetMax > 0) parts.push(`${nudge.budgetUsed || 0}/${nudge.budgetMax}`);
  if (nudge.escalationTarget) parts.push('escalates to ' + actorLabel(nudge.escalationTarget));
  return parts.join(' · ');
}

// sortItems orders a view's rows by urgency: overdue first, then due-soon,
// then by dueAt, then createdAt, then id (the stable tiebreak that keeps the
// morph reconciler's row keys from shuffling between polls).
export function sortItems(items, now) {
  const rank = { overdue: 0, 'due-soon': 1, '': 2 };
  return [...items].sort((a, b) => {
    const ra = rank[dueBucket(a, now)], rb = rank[dueBucket(b, now)];
    if (ra !== rb) return ra - rb;
    const da = timeMs(a.dueAt), db = timeMs(b.dueAt);
    if (Number.isNaN(da) !== Number.isNaN(db)) return Number.isNaN(da) ? 1 : -1;
    if (!Number.isNaN(da) && da !== db) return da - db;
    const ca = timeMs(a.createdAt), cb = timeMs(b.createdAt);
    if (Number.isNaN(ca) !== Number.isNaN(cb)) return Number.isNaN(ca) ? 1 : -1;
    if (!Number.isNaN(ca) && ca !== cb) return ca - cb;
    return String(a.id).localeCompare(String(b.id));
  });
}

// viewItems filters the full item list down to one view. All views except
// 'recent' show only pending work (resolved items belong to 'recent').
export function viewItems(items, view, now) {
  const all = items || [];
  const pending = all.filter(i => i.status === 'pending');
  switch (view) {
    case 'my-work':
      return sortItems(pending.filter(i => i.assignee === OPERATOR_ASSIGNEE), now);
    case 'waiting-on':
      return sortItems(pending, now);
    case 'due':
      return sortItems(pending.filter(i => dueBucket(i, now) !== ''), now);
    case 'blocked':
      return sortItems(pending.filter(i => i.kind === 'blocked'), now);
    case 'decision':
      return sortItems(pending.filter(i => i.kind === 'decision-needed'), now);
    case 'review':
      return sortItems(pending.filter(i => i.kind === 'review-needed'), now);
    case 'recent': {
      // Resolved items (satisfied/canceled) plus anything created inside the
      // window. The backend has no changed-at timestamp yet, so this is the
      // honest approximation: resolution flips status, creation stamps
      // createdAt. Newest creations first; undated (blocked, TCL-303) last.
      const recent = all.filter(i => i.status !== 'pending'
        || (!Number.isNaN(timeMs(i.createdAt)) && now - timeMs(i.createdAt) <= RECENT_MS));
      return recent.sort((a, b) => {
        const ca = timeMs(a.createdAt), cb = timeMs(b.createdAt);
        if (Number.isNaN(ca) !== Number.isNaN(cb)) return Number.isNaN(ca) ? 1 : -1;
        if (!Number.isNaN(ca) && ca !== cb) return cb - ca;
        return String(a.id).localeCompare(String(b.id));
      });
    }
    default:
      return sortItems(pending, now);
  }
}

// groupWaitingOn buckets the 'waiting-on' view by whom the work waits on —
// one group per distinct assignee, humans first, then agents, roles, others,
// alphabetical within each class. Items inside a group keep their sortItems
// order (the caller passes an already-sorted list).
export function groupWaitingOn(items) {
  const groups = new Map();
  for (const item of items || []) {
    const key = item.assignee || '';
    if (!groups.has(key)) groups.set(key, { assignee: key, label: actorLabel(key), items: [] });
    groups.get(key).items.push(item);
  }
  const classRank = (a) => a.startsWith('human:') ? 0 : a.startsWith('agent:') ? 1 : a.startsWith('role:') ? 2 : 3;
  return [...groups.values()].sort((a, b) => {
    const ra = classRank(a.assignee), rb = classRank(b.assignee);
    if (ra !== rb) return ra - rb;
    return a.assignee.localeCompare(b.assignee);
  });
}

// viewCounts computes each view's chip count in one pass (the chips show
// them permanently, so this runs on every poll).
export function viewCounts(items, now) {
  const counts = {};
  for (const v of WORKLIST_VIEWS) counts[v.key] = viewItems(items, v.key, now).length;
  return counts;
}
