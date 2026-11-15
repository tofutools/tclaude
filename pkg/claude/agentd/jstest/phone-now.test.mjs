// js/phone-now.js, phone-approval.js and phone-now-model.js: the phone's Now
// view in the mock's shape, driven through what the operator taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ASKER = 'agt_00000000000000000000000000000001';
const WORKER = 'agt_00000000000000000000000000000002';
const BROKEN = 'agt_00000000000000000000000000000003';
const STEWARD = 'agt_00000000000000000000000000000004';

const SNAPSHOT = {
  groups: [
    { name: 'webshop', members: [
      { agent_id: ASKER, conv_id: 'c1', title: 'payments-fix', online: true, state: { status: 'awaiting_permission' } },
      { agent_id: WORKER, conv_id: 'c2', title: 'lead', online: true, state: { status: 'working' } },
      { agent_id: BROKEN, conv_id: 'c3', title: 'release-bot', online: true, state: { status: 'error', status_detail: 'rate limited until 14:00' } },
    ] },
    { name: 'steward', scribe: true, members: [{ agent_id: STEWARD, conv_id: 'c4', title: 'steward', online: true, state: { status: 'idle' } }] },
  ],
  access_requests: [{ id: 'ar_1', perm: 'git.push', agent_id: ASKER, conv_title: 'payments-fix', body: 'push PR #4351\nmore', target_group: 'webshop', deadline: new Date(Date.now() + 42_500).toISOString(), status: 'pending' }],
  messages: [{ id: 9, from_agent: STEWARD, from_title: 'steward', subject: 'Where should the health check live?', body: 'Admin server or a new one.', options: ['Sidecar', 'Admin server', 'New server'], recommended: 2 }],
  // Claude's windows are the usage object's own; other subscriptions nest.
  usage: { available: true, five_hour: { pct: 62, remaining: '2h' }, seven_day: { pct: 46 }, codex: { available: true, five_hour: { pct: 12 } } },
  authored_open_prs: { available: true, total: 1, items: [{ url: 'https://github.com/o/r/pull/1482', title: 'Checkout redesign', checks: { state: 'passing' } }] },
};

async function setup(t, snapshot = SNAPSHOT, { stored = null } = {}) {
  const harness = await createPreactHarness(t);
  const store = new Map(stored ? [['tclaude.phone.now.chip', stored]] : []);
  const storage = { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => { store.set(k, v); } };
  const saved = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { value: storage, configurable: true });
  t.after(() => { if (saved) Object.defineProperty(globalThis, 'localStorage', saved); else delete globalThis.localStorage; });
  const [{ createDashboardState }, now, sheet] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-now.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  t.after(() => sheet.closePhoneSheet());
  const state = createDashboardState();
  state.beginRequest();
  state.commitRequest(1, snapshot);
  const calls = [];
  const posts = [];
  const record = (name) => (...args) => { calls.push([name, ...args]); };
  const actions = { notify: record('notify'), navigate: record('navigate'), openTerminal: record('openTerminal'), openAccessRequest: record('openAccessRequest') };
  const fetchImpl = async (url, init) => { posts.push([url, JSON.parse(init.body)]); return { ok: true }; };
  const { signal } = harness.signals;
  const processSource = { context: signal(null), status: signal('ready'), error: signal(''), refresh: () => Promise.resolve() };
  const mounted = await harness.mount(harness.html`<div><${now.NowView} state=${state} actions=${actions} fetchImpl=${fetchImpl} processSource=${processSource} /><${sheet.PhoneSheetHost} /></div>`);
  const tap = (element) => harness.act(() => element.click());
  return { harness, mounted, calls, posts, tap, store, root: mounted.container };
}

test('the usage strip shows each subscription\'s fullest window and opens Usage', async (t) => {
  const { harness, root, calls, tap } = await setup(t);
  const strip = harness.getByRole(root, 'button', { name: 'Usage: Claude 62% of 5h, Codex 12% of 5h. Open Usage' });
  assert.match(strip.textContent, /Claude\s*62%\s*5h/);
  await tap(strip);
  assert.deepEqual(calls.at(-1), ['navigate', '/usage']);
});

test('an access request is an amber card: countdown, Deny / +5 min / Approve, and its text opens Mail', async (t) => {
  const { harness, root, calls, posts, tap } = await setup(t);
  const card = harness.getByRole(root, 'group', { name: 'payments-fix asks: git.push: push PR #4351' });
  assert.ok(card.classList.contains('phone-box-key'));
  assert.equal(card.querySelector('.phone-av-badge').textContent, '🔑');
  assert.match(card.querySelector('.phone-pill-key').textContent, /^0:4[12]$/);
  assert.match(card.querySelector('.phone-who').textContent, /payments-fix\s*webshop/);
  const buttons = [...card.querySelectorAll('.phone-btns .ui-btn')].map((b) => b.textContent.trim());
  assert.deepEqual(buttons, ['Deny', '+5 min', 'Approve']);
  await tap(harness.getByRole(card, 'button', { name: '+5 min' }));
  await tap(harness.getByRole(card, 'button', { name: 'Approve' }));
  assert.deepEqual(posts, [['/api/access-requests/ar_1/decision', { decision: 'extend' }], ['/api/access-requests/ar_1/decision', { decision: 'approve' }]]);
  await tap(harness.getByRole(card, 'button', { name: "Open payments-fix's request in Mail" }));
  assert.deepEqual(calls.at(-1), ['openAccessRequest', 'ar_1']);
});

test('a decision is a purple card with the recommended option and one more; ⋯ has the rest, a note and Dismiss', async (t) => {
  const { harness, root, posts, tap } = await setup(t);
  const card = harness.getByRole(root, 'group', { name: 'Decision: Where should the health check live?' });
  assert.ok(card.classList.contains('phone-box-ask'));
  assert.equal(card.querySelector('.phone-av').textContent, '✦', 'the Steward asks');
  const labels = [...card.querySelectorAll('.phone-btns .ui-btn')].map((b) => b.textContent.trim());
  assert.deepEqual(labels, ['Sidecar', '★ Admin server', '⋯'], 'in their order, the recommended one always shown');
  await tap(harness.getByRole(card, 'button', { name: 'Admin server (recommended)' }));
  assert.deepEqual(posts.at(-1), ['/api/human-messages/reply', { id: 9, choice: 2, body: '' }]);

  await tap(harness.getByRole(card, 'button', { name: 'All options, a note, or dismiss' }));
  const sheet = root.querySelector('[role="dialog"]');
  assert.equal(sheet.getAttribute('aria-label'), 'Where should the health check live?');
  assert.equal(harness.document.activeElement, harness.getByRole(sheet, 'button', { name: 'Close' }), 'focus lands on Close, not on an answer');
  assert.match(sheet.textContent, /Admin server or a new one\./);
  await harness.input(harness.getByRole(sheet, 'textbox', { name: 'Note to send with your choice' }), 'keep it small');
  await tap([...sheet.querySelectorAll('.ui-btn')].find((b) => b.textContent.trim() === 'New server'));
  assert.deepEqual(posts.at(-1), ['/api/human-messages/reply', { id: 9, choice: 3, body: 'keep it small' }]);
  await harness.act(async () => {});
  assert.equal(root.querySelector('[role="dialog"]'), null, 'a pick closes the sheet');
});

test('few online: every online agent is a row, at work and in error first, the idle ones too; a row opens its terminal', async (t) => {
  const { harness, root, calls, tap } = await setup(t);
  const online = harness.getByRole(root, 'group', { name: 'Online' });
  const rows = [...online.querySelectorAll('.phone-row')];
  assert.deepEqual(rows.map((r) => [r.querySelector('.phone-who-name').textContent, r.querySelector('.phone-pill').textContent]),
    [['release-bot', 'error'], ['lead', 'working'], ['steward', 'idle']], 'the asker is a card in Needs you, not a row');
  assert.match(online.querySelector('.phone-section-head').textContent, /Online · 3/);
  assert.equal(rows[0].querySelector('.phone-card-line').textContent, 'rate limited until 14:00', 'an error row says why');
  assert.equal(rows[1].querySelector('.phone-card-line'), null);
  assert.equal(root.querySelector('.phone-now-all'), null, 'nothing hidden, so no Show all');
  await tap(rows[1]);
  assert.equal(calls.at(-1)[0], 'openTerminal');
  assert.equal(calls.at(-1)[1].agentId, WORKER);
});

test('many online: only the ones at work or in error, and Show all lists every group\'s', async (t) => {
  const idle = Array.from({ length: 10 }, (_, i) => ({
    agent_id: `agt_1000000000000000000000000000000${i.toString(16)}`, conv_id: `i${i}`, title: `idle-${i}`, online: true, state: { status: 'idle' },
  }));
  const offline = { agent_id: 'agt_20000000000000000000000000000000', conv_id: 'o', title: 'gone', online: false, state: { status: 'idle' } };
  const snapshot = { ...SNAPSHOT, groups: [...SNAPSHOT.groups, { name: 'tclaude', members: [...idle, offline] }] };
  const { harness, root, tap } = await setup(t, snapshot);
  const working = harness.getByRole(root, 'group', { name: 'Active' });
  assert.deepEqual([...working.querySelectorAll('.phone-who-name')].map((n) => n.textContent), ['release-bot', 'lead']);
  const all = root.querySelector('.phone-now-all');
  assert.match(all.textContent, /Show all 13 online/, 'the offline one is not counted');
  assert.equal(all.getAttribute('aria-expanded'), 'false');
  await tap(all);
  const online = harness.getByRole(root, 'group', { name: 'Online' });
  const names = [...online.querySelectorAll('.phone-who-name')].map((n) => n.textContent);
  assert.equal(names.length, 13);
  assert.deepEqual(names.slice(0, 2), ['release-bot', 'lead'], 'the busy ones stay first');
  assert.ok(names.includes('idle-0') && names.includes('steward'), 'every group\'s online agents');
  assert.ok(!names.includes('gone'));
  await tap(root.querySelector('.phone-now-all'));
  assert.ok(harness.getByRole(root, 'group', { name: 'Active' }), 'Only the recent ones folds it back');
});

test('open PRs are collapsed under their header until a tap anywhere on it', async (t) => {
  const { harness, root, tap } = await setup(t);
  const prs = harness.getByRole(root, 'group', { name: 'Open PRs' });
  assert.equal(prs.querySelector('.phone-row'), null);
  const head = prs.querySelector('.phone-section-head button');
  assert.equal(head.getAttribute('aria-expanded'), 'false');
  assert.match(head.textContent, /Open PRs · 1/, 'the whole head row is the button, not just its chevron');
  await tap(head);
  assert.equal(head.getAttribute('aria-expanded'), 'true');
  const row = prs.querySelector('a.phone-row');
  assert.equal(row.getAttribute('href'), 'https://github.com/o/r/pull/1482');
  assert.equal(row.querySelector('.phone-pill-work').textContent, 'passing');
});

test('the Now model: the strip, a request in one line, the options a card shows', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-now-model.js');
  assert.deepEqual(model.usageStrip(SNAPSHOT.usage).map((s) => [s.name, s.pct, s.window]), [['Claude', 62, '5h'], ['Codex', 12, '5h']]);
  assert.deepEqual(model.usageStrip({ available: true, five_hour: { pct: 10 }, seven_day: { pct: 80 } }).map((s) => [s.pct, s.window]), [[80, '7d']], 'the fullest window');
  assert.deepEqual(model.usageStrip(null), []);
  assert.equal(model.approvalContext({ perm: 'git.push', body: '\n  push it\nmore' }), 'git.push: push it');
  assert.equal(model.approvalContext({ perm: 'sudo.git.push,agent.spawn' }), 'Plan: git.push, agent.spawn');
  assert.equal(model.approvalContext({ perm: 'http.get', method: 'GET', path: '/api/x' }), 'http.get: GET /api/x');
  const pick = (options, recommended) => model.cardOptions({ options, recommended }).map((o) => o.label);
  assert.deepEqual(pick(['a', 'b', 'c'], 0), ['a', 'b']);
  assert.deepEqual(pick(['a', 'b', 'c'], 3), ['a', 'c'], 'the recommended one is never hidden');
  assert.equal(model.countdown(42), '0:42');
  assert.equal(model.countdown(125), '2:05');
  assert.equal(model.countdown(86_396), '23h 59m', 'a long deadline in hours');
});

test('the chips: All by default, counts on them, each shows its view, and the pick is remembered', async (t) => {
  const hook = (min) => new Date(Date.now() - min * 60_000).toISOString();
  const snapshot = { ...SNAPSHOT, groups: [
    { name: 'webshop', members: [
      SNAPSHOT.groups[0].members[0],
      { ...SNAPSHOT.groups[0].members[1], state: { status: 'working', last_hook: hook(40) } },
      SNAPSHOT.groups[0].members[2],
    ] },
    { name: 'steward', scribe: true, members: [{ ...SNAPSHOT.groups[1].members[0], state: { status: 'idle', last_hook: hook(5) } }] },
  ] };
  const { harness, root, tap, store } = await setup(t, snapshot);
  const chips = harness.getByRole(root, 'tablist', { name: 'Show on Now' });
  const chip = (name) => harness.getByRole(chips, 'tab', { name });
  assert.equal(chip(/^All/).getAttribute('aria-selected'), 'true');
  assert.match(chip(/^All/).textContent, /All 3/, 'online agents but the asker, as the rows count them');
  assert.match(chip(/^Needs you/).querySelector('.ui-badge-danger').textContent, /^\d+$/);
  assert.equal(chip(/^Stuck\?/).querySelector('.ui-badge-warn').textContent, '1', 'lead has been quiet 40m');

  await tap(chip(/^Needs you/));
  assert.ok(harness.getByRole(root, 'group', { name: 'Needs you' }));
  assert.equal(root.querySelector('.phone-usage-strip'), null, 'only the cards');
  assert.equal(root.querySelector('.phone-rows'), null);
  assert.equal(store.get('tclaude.phone.now.chip'), 'needs', 'remembered for this viewer');

  await tap(chip(/^Idle/));
  const idle = harness.getByRole(root, 'group', { name: 'Idle' });
  assert.match(idle.querySelector('.phone-now-group').textContent, /steward · 1/);
  assert.match(idle.querySelector('.phone-now-when').textContent, /^idle 5m$/);

  await tap(chip(/^Recent/));
  const recent = harness.getByRole(root, 'group', { name: 'Recent' });
  assert.deepEqual([...recent.querySelectorAll('.phone-who-name')].map((n) => n.textContent).slice(0, 2), ['steward', 'lead']);

  await tap(chip(/^Stuck\?/));
  const stuck = harness.getByRole(root, 'group', { name: 'Stuck?' });
  const when = stuck.querySelector('.phone-now-when');
  assert.match(when.textContent, /^last update 40m ago$/);
  assert.ok(when.classList.contains('quiet'), 'amber past the threshold');
});

test('a remembered chip comes back; a bad one falls back to All', async (t) => {
  const { harness, root } = await setup(t, SNAPSHOT, { stored: 'idle' });
  assert.ok(harness.getByRole(root, 'group', { name: 'Idle' }));
  const { storedChip } = await harness.importDashboardModule('js/phone-now.js');
  assert.equal(storedChip({ getItem: () => 'nonsense' }), 'all');
  assert.equal(storedChip({ getItem: () => { throw new Error('blocked'); } }), 'all');
});
