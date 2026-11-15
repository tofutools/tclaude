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
      { agent_id: BROKEN, conv_id: 'c3', title: 'release-bot', online: true, state: { status: 'error' } },
    ] },
    { name: 'steward', scribe: true, members: [{ agent_id: STEWARD, conv_id: 'c4', title: 'steward', online: true, state: { status: 'idle' } }] },
  ],
  access_requests: [{ id: 'ar_1', perm: 'git.push', agent_id: ASKER, conv_title: 'payments-fix', body: 'push PR #4351\nmore', target_group: 'webshop', deadline: new Date(Date.now() + 42_500).toISOString(), status: 'pending' }],
  messages: [{ id: 9, from_agent: STEWARD, from_title: 'steward', subject: 'Where should the health check live?', body: 'Admin server or a new one.', options: ['Sidecar', 'Admin server', 'New server'], recommended: 2 }],
  // Claude's windows are the usage object's own; other subscriptions nest.
  usage: { available: true, five_hour: { pct: 62, remaining: '2h' }, seven_day: { pct: 46 }, codex: { available: true, five_hour: { pct: 12 } } },
  authored_open_prs: { available: true, total: 1, items: [{ url: 'https://github.com/o/r/pull/1482', title: 'Checkout redesign', checks: { state: 'passing' } }] },
};

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, now, sheet] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-now.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  t.after(() => sheet.closePhoneSheet());
  const state = createDashboardState();
  state.beginRequest();
  state.commitRequest(1, SNAPSHOT);
  const calls = [];
  const posts = [];
  const record = (name) => (...args) => { calls.push([name, ...args]); };
  const actions = { notify: record('notify'), navigate: record('navigate'), openTerminal: record('openTerminal'), openAccessRequest: record('openAccessRequest') };
  const fetchImpl = async (url, init) => { posts.push([url, JSON.parse(init.body)]); return { ok: true }; };
  const { signal } = harness.signals;
  const processSource = { context: signal(null), status: signal('ready'), error: signal(''), refresh: () => Promise.resolve() };
  const mounted = await harness.mount(harness.html`<div><${now.NowView} state=${state} actions=${actions} fetchImpl=${fetchImpl} processSource=${processSource} /><${sheet.PhoneSheetHost} /></div>`);
  const tap = (element) => harness.act(() => element.click());
  return { harness, mounted, calls, posts, tap, root: mounted.container };
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
  assert.match(sheet.textContent, /Admin server or a new one\./);
  await harness.input(harness.getByRole(sheet, 'textbox', { name: 'Note to send with your choice' }), 'keep it small');
  await tap([...sheet.querySelectorAll('.ui-btn')].find((b) => b.textContent.trim() === 'New server'));
  assert.deepEqual(posts.at(-1), ['/api/human-messages/reply', { id: 9, choice: 3, body: 'keep it small' }]);
  await harness.act(async () => {});
  assert.equal(root.querySelector('[role="dialog"]'), null, 'a pick closes the sheet');
});

test('agents at work and in error are rows in one card, with pills, that open their terminal', async (t) => {
  const { harness, root, calls, tap } = await setup(t);
  const working = harness.getByRole(root, 'group', { name: 'Working' });
  const rows = [...working.querySelectorAll('.phone-row')];
  assert.deepEqual(rows.map((r) => [r.querySelector('.phone-who-name').textContent, r.querySelector('.phone-pill').textContent]),
    [['release-bot', 'error'], ['lead', 'working']]);
  assert.match(working.querySelector('.phone-section-head').textContent, /Working · 2/);
  await tap(rows[1]);
  assert.equal(calls.at(-1)[0], 'openTerminal');
  assert.equal(calls.at(-1)[1].agentId, WORKER);
});

test('open PRs are collapsed under their header until it is tapped', async (t) => {
  const { harness, root, tap } = await setup(t);
  const prs = harness.getByRole(root, 'group', { name: 'Open PRs' });
  assert.equal(prs.querySelector('.phone-row'), null);
  await tap(harness.getByRole(prs, 'button', { name: 'Show open PRs' }));
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
