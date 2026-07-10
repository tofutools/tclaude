import test from 'node:test';
import assert from 'node:assert/strict';
import {
  OPERATOR_ASSIGNEE, WORKLIST_VIEWS, kindMeta, actorLabel, nudgeLine,
  fmtAge, fmtDue, fmtClock, dueBucket, sortItems, viewItems, viewCounts,
  groupWaitingOn, actionableCount, isActionable, isDestructiveAction,
  advertisedAction, buildWorklistAction, mintUUID, retainedActionKey,
} from '../dashboard/js/process-worklist-core.js';

// A fixed "now" keeps every duration/bucket assertion deterministic.
const NOW = Date.parse('2026-07-10T12:00:00Z');
const iso = (offsetMs) => new Date(NOW + offsetMs).toISOString();
const HOUR = 60 * 60 * 1000;

function item({ id, ...overrides }) {
  return {
    id: 'wi_' + id,
    run: 'run-1', node: 'decide', attempt: 1,
    kind: 'decision-needed', assignee: OPERATOR_ASSIGNEE, status: 'pending',
    createdAt: iso(-2 * HOUR), summary: 'Approve the release?',
    availableActions: ['approve', 'reject'],
    links: { runId: 'run-1', nodeId: 'decide' },
    ...overrides,
  };
}

const fixture = [
  item({ id: 1, kind: 'decision-needed', dueAt: iso(2 * HOUR) }),                    // mine, due soon
  item({ id: 2, kind: 'review-needed', assignee: 'human:oncall', dueAt: iso(-HOUR) }), // overdue review, not mine
  item({ id: 3, kind: 'human-wait', dueAt: iso(48 * HOUR) }),                        // mine, far future due
  item({
    id: 4, kind: 'blocked', assignee: OPERATOR_ASSIGNEE,
    createdAt: undefined, dueAt: undefined, nudge: undefined,                        // TCL-303 gaps
    availableActions: ['retry', 'skip', 'cancel'], summary: 'node exhausted its budget',
  }),
  item({ id: 5, kind: 'agent-obligation', assignee: 'agent:agt_1', availableActions: [] }),
  item({ id: 6, status: 'satisfied', createdAt: iso(-3 * HOUR) }),                   // resolved → recent only
  item({ id: 7, kind: 'human-wait', assignee: 'role:reviewer', createdAt: iso(-30 * HOUR) }), // old, pending
];

test('viewItems: my-work is the operator’s pending items only', () => {
  const ids = viewItems(fixture, 'my-work', NOW).map(i => i.id);
  assert.deepEqual(new Set(ids), new Set(['wi_1', 'wi_3', 'wi_4']));
});

test('viewItems: kind views filter pending items of that kind', () => {
  assert.deepEqual(viewItems(fixture, 'blocked', NOW).map(i => i.id), ['wi_4']);
  assert.deepEqual(viewItems(fixture, 'decision', NOW).map(i => i.id), ['wi_1']);
  assert.deepEqual(viewItems(fixture, 'review', NOW).map(i => i.id), ['wi_2']);
});

test('viewItems: due view keeps only overdue/due-soon, overdue first', () => {
  assert.deepEqual(viewItems(fixture, 'due', NOW).map(i => i.id), ['wi_2', 'wi_1']);
});

test('viewItems: recent view includes resolved items and recent creations, newest first', () => {
  const ids = viewItems(fixture, 'recent', NOW).map(i => i.id);
  assert.ok(ids.includes('wi_6'), 'satisfied item belongs to recently-changed');
  assert.ok(!ids.includes('wi_7'), '30h-old pending item is not recent');
  assert.equal(ids[0], 'wi_1', 'newest creation sorts first');
});

test('recent view is BOUNDED: old resolved items age out, fresh resolutions sort first', () => {
  const items = [
    item({ id: 1 }),                                                     // pending, created 2h ago
    item({ id: 2, status: 'satisfied', createdAt: iso(-72 * HOUR), changedAt: iso(-10 * 60 * 1000) }), // resolved 10m ago
    item({ id: 3, status: 'satisfied', createdAt: iso(-72 * HOUR), changedAt: iso(-70 * HOUR) }),      // resolved 70h ago
    item({ id: 4, status: 'satisfied', createdAt: iso(-30 * HOUR) }),    // no changedAt, old createdAt fallback
    item({ id: 5, kind: 'blocked', status: 'pending', createdAt: undefined }), // no timestamps at all (TCL-303)
  ];
  const ids = viewItems(items, 'recent', NOW).map(i => i.id);
  assert.deepEqual(ids, ['wi_2', 'wi_1'],
    'fresh resolution first, then the recent creation; stale/undated items excluded');
  assert.ok(!ids.includes('wi_3'), '70h-old resolution must age out (the chip cannot grow forever)');
  assert.ok(!ids.includes('wi_4'), 'resolved item whose only timestamp is 30h old ages out too');
  assert.ok(!ids.includes('wi_5'), 'an item with no recorded timestamp cannot claim recency');
});

test('sortItems: overdue → due-soon → dated → undated, stable id tiebreak', () => {
  const sorted = sortItems(fixture.filter(i => i.status === 'pending'), NOW).map(i => i.id);
  assert.equal(sorted[0], 'wi_2', 'overdue first');
  assert.equal(sorted[1], 'wi_1', 'due-soon second');
  assert.ok(sorted.indexOf('wi_4') > sorted.indexOf('wi_3'), 'undated blocked item after dated ones');
});

test('groupWaitingOn: humans, then agents, then roles', () => {
  const groups = groupWaitingOn(viewItems(fixture, 'waiting-on', NOW));
  const assignees = groups.map(g => g.assignee);
  assert.deepEqual(assignees, [
    'human:oncall', OPERATOR_ASSIGNEE, 'agent:agt_1', 'role:reviewer',
  ]);
  assert.equal(groups[1].items.length, 3);
});

test('actionableCount: pending, non-agent-obligation, with actions', () => {
  assert.equal(actionableCount(fixture), 5);
  assert.equal(isActionable(item({ id: 9, kind: 'agent-obligation' })), false);
  assert.equal(isActionable(item({ id: 9, status: 'satisfied' })), false);
  assert.equal(isActionable(item({ id: 9, availableActions: [] })), false);
});

test('viewCounts covers every declared view', () => {
  const counts = viewCounts(fixture, NOW);
  for (const v of WORKLIST_VIEWS) assert.ok(v.key in counts, v.key);
  assert.equal(counts['my-work'], 3);
  assert.equal(counts.blocked, 1);
});

test('dueBucket thresholds', () => {
  assert.equal(dueBucket(item({ id: 9, dueAt: iso(-1) }), NOW), 'overdue');
  assert.equal(dueBucket(item({ id: 9, dueAt: iso(23 * HOUR) }), NOW), 'due-soon');
  assert.equal(dueBucket(item({ id: 9, dueAt: iso(25 * HOUR) }), NOW), '');
  assert.equal(dueBucket(item({ id: 9, dueAt: undefined }), NOW), '');
});

test('formatters render honest em-dashes for absent data, never fabricate', () => {
  assert.equal(fmtAge(undefined, NOW), '—');
  assert.equal(fmtDue(undefined, NOW), '—');
  assert.equal(fmtAge(iso(-2 * HOUR), NOW), '2h ago');
  assert.equal(fmtDue(iso(3 * HOUR), NOW), 'in 3h');
  assert.match(fmtDue(iso(-HOUR), NOW), /^⚠ overdue 1h$/);
  assert.equal(fmtClock('not-a-date'), '');
});

test('nudgeLine renders the visible schedule and a paused marker', () => {
  const nudge = {
    lastContactAt: iso(-46 * 60 * 1000), nextContactAt: iso(-16 * 60 * 1000),
    budgetUsed: 2, budgetMax: 5, escalationTarget: 'human:oncall', paused: false,
  };
  const line = nudgeLine(nudge);
  assert.match(line, /^last nudged \d\d:\d\d · next \d\d:\d\d · 2\/5 · escalates to 👤 oncall$/);
  assert.match(nudgeLine({ ...nudge, paused: true }), /^⏸ paused · last nudged /);
  assert.equal(nudgeLine({ ...nudge, lastContactAt: undefined }).startsWith('not yet nudged'), true);
  assert.equal(nudgeLine(undefined), '');
});

test('kindMeta and actorLabel pair a glyph WITH text (never color/glyph-only)', () => {
  for (const kind of ['human-wait', 'decision-needed', 'review-needed', 'blocked', 'agent-obligation']) {
    const meta = kindMeta(kind);
    assert.ok(meta.glyph.length > 0 && meta.label.length > 0, kind);
  }
  assert.equal(actorLabel('human:operator'), '👤 operator');
  assert.equal(actorLabel('agent:agt_12'), '🤖 agt_12');
  assert.equal(actorLabel('role:reviewer'), '🎭 reviewer');
  assert.equal(actorLabel(''), '— unassigned');
});

test('advertisedAction resolves case-insensitively to the advertised spelling', () => {
  const capital = item({ id: 9, availableActions: ['Approve', 'Reject'] });
  assert.equal(advertisedAction(capital, 'approve'), 'Approve');
  assert.equal(advertisedAction(capital, 'REJECT'), 'Reject');
  assert.equal(advertisedAction(capital, 'skip'), '');
});

test('buildWorklistAction: advertised spelling + comment + idempotency key, or null', () => {
  const capital = item({ id: 9, availableActions: ['Approve'] });
  const req = buildWorklistAction(capital, 'approve', '  looks good  ', 'key-1');
  assert.equal(req.path, '/v1/process/worklist/wi_9/action');
  assert.deepEqual(req.body, { action: 'Approve', comment: 'looks good', idempotencyKey: 'key-1' });
  assert.equal(buildWorklistAction(capital, 'approve', '   ', 'key-1'), null, 'blank comment refused');
  assert.equal(buildWorklistAction(capital, 'reject', 'why', 'key-1'), null, 'unadvertised action refused');
  assert.equal(buildWorklistAction(capital, 'approve', 'why', ''), null, 'missing idempotency key refused');
});

test('item ids are URL-escaped into the action path', () => {
  const odd = item({ id: 9 });
  odd.id = 'wi/../oops';
  const req = buildWorklistAction(odd, 'approve', 'c', 'k');
  assert.equal(req.path, '/v1/process/worklist/wi%2F..%2Foops/action');
});

test('mintUUID prefers crypto.randomUUID and falls back to getRandomValues v4', () => {
  assert.equal(mintUUID({ randomUUID: () => 'from-randomUUID' }), 'from-randomUUID');
  // Insecure-context shape: getRandomValues only (crypto.randomUUID is
  // secure-context-only, absent on plain-http non-loopback dashboards).
  const insecure = {
    getRandomValues(bytes) {
      for (let i = 0; i < bytes.length; i++) bytes[i] = (i * 37 + 11) & 0xff;
      return bytes;
    },
  };
  const id = mintUUID(insecure);
  assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    'fallback must be a well-formed RFC-4122 v4 uuid (version nibble 4, variant 8-b)');
  assert.notEqual(mintUUID(insecure), '', 'fallback never returns empty');
});

test('retainedActionKey: a retry of the same logical action replays the SAME key', () => {
  let minted = 0;
  const mint = () => `key-${++minted}`;
  const store = new Map();
  const decision = item({ id: 9, availableActions: ['Approve', 'Reject'] });

  const first = retainedActionKey(store, decision, 'approve', 'ship it', mint);
  const retry = retainedActionKey(store, decision, 'APPROVE', 'ship it', mint);
  assert.equal(retry.key, first.key, 'same payload (case-insensitive action) → same retained key');
  assert.equal(minted, 1, 'the retry must NOT mint a fresh key');

  // A payload change is a NEW logical action → new key (matches the
  // backend's same-key/different-payload 409 contract).
  const edited = retainedActionKey(store, decision, 'approve', 'ship it now', mint);
  assert.notEqual(edited.key, first.key);
  const other = retainedActionKey(store, decision, 'reject', 'ship it', mint);
  assert.notEqual(other.key, first.key);

  // Definitive success clears the entry; the next identical action is a new
  // submission with a new key.
  store.delete(first.payload);
  const after = retainedActionKey(store, decision, 'approve', 'ship it', mint);
  assert.notEqual(after.key, first.key);
});

test('isDestructiveAction matches case-insensitively', () => {
  assert.equal(isDestructiveAction('Reject'), true);
  assert.equal(isDestructiveAction('CANCEL'), true);
  assert.equal(isDestructiveAction('skip'), true);
  assert.equal(isDestructiveAction('approve'), false);
  assert.equal(isDestructiveAction('retry'), false);
});
