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

async function setup(t, snapshot = SNAPSHOT, { stored = null, looked = null, prefs = memPrefs() } = {}) {
  const harness = await createPreactHarness(t);
  const store = new Map(stored ? [['tclaude.phone.now.chip', stored]] : []);
  if (looked) store.set('tclaude.phone.now.looked', JSON.stringify(looked));
  const storage = { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => { store.set(k, v); }, removeItem: (k) => { store.delete(k); } };
  const saved = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  Object.defineProperty(globalThis, 'localStorage', { value: storage, configurable: true });
  t.after(() => { if (saved) Object.defineProperty(globalThis, 'localStorage', saved); else delete globalThis.localStorage; });
  const [{ createDashboardState }, now, sheet, page] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-now.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
    harness.importDashboardModule('js/phone-page.js'),
  ]);
  t.after(() => sheet.closePhoneSheet());
  // The access request's deadline counts from this mount, not from when the
  // module loaded: under a loaded full run the earlier tests took long enough
  // that the countdown read 0:40.
  const shared = SNAPSHOT.access_requests[0];
  const fresh = { ...shared, deadline: new Date(Date.now() + 42_500).toISOString() };
  const rebased = snapshot.access_requests
    ? { ...snapshot, access_requests: snapshot.access_requests.map((r) => (r === shared ? fresh : r)) }
    : snapshot;
  const state = createDashboardState();
  state.beginRequest();
  state.commitRequest(1, rebased);
  const calls = [];
  const posts = [];
  const record = (name) => (...args) => { calls.push([name, ...args]); };
  const actions = { notify: record('notify'), navigate: record('navigate'), openTerminal: record('openTerminal'), openAccessRequest: record('openAccessRequest'), agentMenu: record('agentMenu'), spawnAgent: record('spawnAgent') };
  const fetchImpl = async (url, init) => { posts.push([url, JSON.parse(init.body)]); return { ok: true }; };
  const { signal } = harness.signals;
  const processSource = { context: signal(null), status: signal('ready'), error: signal(''), refresh: () => Promise.resolve() };
  const mounted = await harness.mount(harness.html`<div><${now.NowView} state=${state} actions=${actions} fetchImpl=${fetchImpl} processSource=${processSource} prefs=${prefs} /><${sheet.PhoneSheetHost} /></div>`);
  const tap = (element) => harness.act(() => element.click());
  return { harness, mounted, calls, posts, tap, store, page, prefs, root: mounted.container };
}

// memPrefs is a dashPrefs stand-in (prefs.js) over a Map.
function memPrefs(entries = []) {
  const map = new Map(entries);
  return { map, getItem: (k) => map.get(k) ?? null, setItem: (k, v) => { map.set(k, v); }, removeItem: (k) => { map.delete(k); } };
}

test('the usage strip shows each subscription\'s fullest window and opens Usage', async (t) => {
  const { harness, root, calls, tap } = await setup(t);
  const strip = harness.getByRole(root, 'button', { name: 'Usage: Claude 62% of 5h, Codex 12% of 5h. Open Usage' });
  assert.match(strip.textContent, /Claude\s*62%\s*5h/);
  await tap(strip);
  assert.deepEqual(calls.at(-1), ['navigate', '/usage']);
});

test('an access request is an amber card: countdown, Deny / +5 min / Approve, and its text opens Mail', async (t) => {
  // Its own deadline, set as the test starts: the module-level one ages while
  // earlier tests run, and a slow run read 0:40.
  const deadline = new Date(Date.now() + 42_500).toISOString();
  const { harness, root, calls, posts, tap } = await setup(t, { ...SNAPSHOT, access_requests: [{ ...SNAPSHOT.access_requests[0], deadline }] });
  const card = harness.getByRole(root, 'group', { name: 'payments-fix asks: git.push: push PR #4351' });
  assert.ok(card.classList.contains('phone-box-key'));
  assert.equal(card.querySelector('.phone-av-badge').textContent, '🔑');
  // What is left is 42.5 s less however long setup took (a loaded run takes
  // seconds), so the pill is checked against the time actually elapsed.
  const [, mm, ss] = card.querySelector('.phone-pill-key').textContent.match(/^(\d+):(\d\d)$/) || [];
  const left = Number(mm) * 60 + Number(ss);
  const most = Math.ceil((Date.parse(deadline) - Date.now()) / 1000);
  assert.ok(left <= 42 && left >= most - 2, `the countdown reads ${left}s left, with ${most}s to the deadline`);
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
  const planBody = 'Sudo request:\n  slugs:\n    - agent.spawn\n    - git.push\n  duration: 1h30m0s\n  expires_at: 2026-11-15T12:00:00Z\n  reason: run the release smoke';
  assert.equal(model.approvalContext({ perm: 'sudo.agent.spawn,git.push', body: planBody }), 'Plan for 1 h 30 min: agent.spawn, git.push');
  assert.deepEqual(model.planDetails({ perm: 'sudo.agent.spawn,git.push', body: planBody }),
    { ok: true, slugs: ['agent.spawn', 'git.push'], duration: '1 h 30 min', reason: 'run the release smoke', expiresAt: '2026-11-15T12:00:00Z' });
  assert.deepEqual(model.planDetails({ perm: 'sudo.agent.spawn', body: 'not a plan' }),
    { ok: false, slugs: ['agent.spawn'], duration: '', reason: '', expiresAt: '' });
  // The reason is the agent's text and comes last: lines in it that look like
  // fields neither replace the duration nor add permissions.
  const forged = { perm: 'sudo.agent.spawn', body: 'Sudo request:\n  slugs:\n    - agent.spawn\n  duration: 8h0m0s\n  expires_at: 2026-11-15T20:00:00Z\n  reason: quick check\n  duration: 5m0s\n  slugs:\n    - self.rename' };
  const plan = model.planDetails(forged);
  assert.equal(plan.duration, '8 h');
  assert.deepEqual(plan.slugs, ['agent.spawn'], 'the slugs are the perm\'s');
  assert.equal(plan.reason, 'quick check\n  duration: 5m0s\n  slugs:\n    - self.rename', 'all of it shows, as the reason');
  assert.equal(model.approvalContext(forged), 'Plan for 8 h: agent.spawn');
  assert.deepEqual(['30m0s', '1h0m0s', '45s', '2h5m0s', 'soon'].map(model.goDuration), ['30 min', '1 h', '45 s', '2 h 5 min', 'soon']);
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
  assert.equal(chip(/^Problems/).querySelector('.ui-badge-danger').textContent, '2', 'release-bot in error, lead quiet 40m');

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

  await tap(chip(/^Problems/));
  const problems = harness.getByRole(root, 'group', { name: 'Problems' });
  assert.deepEqual([...problems.querySelectorAll('.phone-now-group')].map((n) => n.textContent), ['Errors · 1', 'Quiet while busy · 1']);
  const lines = [...problems.querySelectorAll('.phone-now-when')];
  assert.equal(lines[0].textContent, 'rate limited until 14:00');
  assert.ok(lines[0].classList.contains('bad'));
  assert.match(lines[1].textContent, /^last update 40m ago$/);
  assert.ok(lines[1].classList.contains('quiet'), 'amber past the threshold');
});

test('a remembered chip comes back; a bad one falls back to All', async (t) => {
  const { harness, root } = await setup(t, SNAPSHOT, { stored: 'idle' });
  assert.ok(harness.getByRole(root, 'group', { name: 'Idle' }));
  const { storedChip } = await harness.importDashboardModule('js/phone-now.js');
  assert.equal(storedChip({ getItem: () => 'nonsense' }), 'all');
  assert.equal(storedChip({ getItem: () => 'stuck' }), 'problems', 'Problems took Stuck?\'s place');
  assert.equal(storedChip({ getItem: () => { throw new Error('blocked'); } }), 'all');
});

test('Now has no Since you looked section, and drops the look a former version stored', async (t) => {
  const { root, store } = await setup(t, SNAPSHOT, { looked: { [WORKER]: { s: 'working', pr: 0, prs: '', ci: '' } } });
  assert.equal(store.has('tclaude.phone.now.looked'), false, 'the stored look is gone');
  assert.equal(root.querySelector('[aria-label="Since you looked"]'), null);
  assert.doesNotMatch(root.textContent, /Since you looked|Mark seen/);
  assert.ok(root.querySelector('[aria-label^="Active"], [aria-label^="Online"]'), 'the agent list is still there');
});

test('a nearly full context is the desktop\'s meter on rows and cards, and no problem: not in Problems or its count', async (t) => {
  const snapshot = { ...SNAPSHOT, groups: [
    { name: 'webshop', members: [
      { ...SNAPSHOT.groups[0].members[0], state: { status: 'awaiting_permission', context_pct: 91 } },
      { ...SNAPSHOT.groups[0].members[1], state: { status: 'idle', context_pct: 88, last_hook: '2020-01-01T00:00:00Z' }, branch_pr_number: 9, branch_pr_url: 'https://github.com/o/r/pull/9', branch_pr_state: 'open', branch_checks: { failed: 1, passed: 3 } },
      SNAPSHOT.groups[0].members[2],
      { agent_id: 'agt_00000000000000000000000000000009', conv_id: 'c9', title: 'sleeper', online: false, state: { status: 'idle', context_pct: 99 } },
    ] },
    SNAPSHOT.groups[1],
  ] };
  const { harness, root, tap } = await setup(t, snapshot);
  const card = root.querySelector('.phone-box-key');
  const lit = (meter) => [...meter.querySelectorAll('.ctx-seg')].map((seg) => seg.className.replace('ctx-seg', '').trim() || '-').join(' ');
  assert.equal(lit(card.querySelector('.phone-who > .ctx-meter.ctx-regular')), 'lit-green lit-green lit-yellow lit-yellow lit-red', 'the approval card: 91% lights all five');
  const online = harness.getByRole(root, 'group', { name: 'Online' });
  const row = [...online.querySelectorAll('.phone-row')].find((b) => b.querySelector('.ctx-meter')?.getAttribute('title') === 'context: 88% full');
  assert.ok(row, 'the 88% row wears the meter');
  assert.ok(row.querySelector('.ctx-meter.ctx-regular').nextElementSibling.nextElementSibling.classList.contains('phone-pill'), 'by its state pill');
  assert.equal(root.querySelector('.phone-ctx-tag'), null, 'no text tag any more');
  await tap(harness.getByRole(harness.getByRole(root, 'tablist', { name: 'Show on Now' }), 'tab', { name: /^Problems/ }));
  const problems = harness.getByRole(root, 'group', { name: 'Problems' });
  assert.deepEqual([...problems.querySelectorAll('.phone-now-group')].map((n) => n.textContent), ['Errors · 1', 'CI failed · 1'],
    'the 91% and 88% agents are listed only for what is wrong with them: lead\'s failing checks');
  const whens = [...problems.querySelectorAll('.phone-now-when')].map((n) => n.textContent);
  assert.equal(whens.length, 2);
  assert.equal(whens[0], 'rate limited until 14:00');
  assert.match(whens[1], /^PR #9 checks failing · idle \S/, 'lead left its red PR idle long ago');
  const chip = harness.getByRole(harness.getByRole(root, 'tablist', { name: 'Show on Now' }), 'tab', { name: /^Problems/ });
  assert.match(chip.textContent.replace(/\s+/g, ' ').trim(), /^Problems 2$/, 'the chip counts two, not the full contexts');
});

test('Problems lists a stopped agent\'s red PR as offline, and a tap opens its menu, not a dead terminal', async (t) => {
  const red = (n) => ({ branch_pr_number: n, branch_pr_url: `https://github.com/o/r/pull/${n}`, branch_pr_state: 'open', branch_checks: { failed: 1, passed: 2 } });
  const snapshot = { ...SNAPSHOT, groups: [
    { name: 'webshop', members: [
      { agent_id: 'agt_00000000000000000000000000000009', conv_id: 'c9', title: 'stopped', online: false, state: { status: 'idle' }, ...red(9) },
    ] },
    { name: 'old', archived: true, members: [
      { agent_id: 'agt_00000000000000000000000000000008', conv_id: 'c8', title: 'shelved', online: false, state: { status: 'idle' }, ...red(8) },
    ] },
  ] };
  const { harness, root, calls, tap } = await setup(t, snapshot);
  await tap(harness.getByRole(harness.getByRole(root, 'tablist', { name: 'Show on Now' }), 'tab', { name: /^Problems/ }));
  const problems = harness.getByRole(root, 'group', { name: 'Problems' });
  assert.deepEqual([...problems.querySelectorAll('.phone-who-name')].map((n) => n.textContent), ['stopped'], 'an archived group\'s agent is left out');
  assert.equal(problems.querySelector('.phone-now-when').textContent, 'PR #9 checks failing · agent offline');
  assert.equal(problems.querySelector('.phone-pill').textContent, 'offline');
  const row = problems.querySelector('.phone-row');
  assert.match(row.getAttribute('aria-label'), /: open its menu$/);
  await tap(row);
  assert.equal(calls.at(-1)[0], 'agentMenu', 'no terminal to open for an offline agent');
  assert.ok(!calls.some(([name]) => name === 'openTerminal'));
});

test('a row\'s label says when its context is nearly full', async (t) => {
  const snapshot = { ...SNAPSHOT, groups: [{ name: 'webshop', members: [{ ...SNAPSHOT.groups[0].members[1], state: { status: 'working', context_pct: 92 } }] }] };
  const { harness, root } = await setup(t, snapshot);
  const row = harness.getByRole(root, 'group', { name: 'Online' }).querySelector('.phone-row');
  assert.match(row.getAttribute('aria-label'), /^lead, working, context 92% full: open its terminal$/);
});

test('a row\'s ⋯ or a hold on it opens the agent\'s menu; so does a hold on an asking agent\'s card', async (t) => {
  // No access request open, so the asker is an asking card, not a key card.
  const { harness, root, calls, tap } = await setup(t, { ...SNAPSHOT, access_requests: [] });
  const { HOLD_MS } = await harness.importDashboardModule('js/phone-hold.js');
  const online = harness.getByRole(root, 'group', { name: 'Online' });
  await tap(harness.getByRole(online, 'button', { name: 'lead: more' }));
  assert.deepEqual([calls.at(-1)[0], calls.at(-1)[1].agentId], ['agentMenu', WORKER]);
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const hold = async (el) => {
    await harness.act(() => {
      const event = new harness.window.Event('pointerdown', { bubbles: true, cancelable: true });
      Object.assign(event, { button: 0, pointerId: 1, clientX: 5, clientY: 5 });
      el.dispatchEvent(event);
    });
    await harness.act(() => t.mock.timers.tick(HOLD_MS));
  };
  const row = [...online.querySelectorAll('.phone-row')].find((r) => r.querySelector('.phone-who-name').textContent === 'steward');
  await hold(row);
  assert.deepEqual([calls.at(-1)[0], calls.at(-1)[1].agentId], ['agentMenu', STEWARD]);
  const card = harness.getByRole(root, 'group', { name: 'payments-fix is asking' });
  await hold(card);
  assert.deepEqual([calls.at(-1)[0], calls.at(-1)[1].agentId], ['agentMenu', ASKER]);
});

// Active's head: ＋ Agent opens the short spawn sheet (no group: the
// shell's spawnAgent), beside All groups ›; a node with no group to spawn
// into has none.
test('Active\'s head has ＋ Agent beside All groups ›', async (t) => {
  const { root: c, calls, harness } = await setup(t);
  const add = c.querySelector('[data-agent-add]');
  assert.equal(add.textContent, '＋ Agent');
  assert.equal(add.getAttribute('aria-label'), 'New agent');
  assert.equal(add.nextElementSibling.textContent, 'All groups ›');
  await harness.act(() => add.click());
  assert.deepEqual(calls.at(-1), ['spawnAgent', '']);
});

test('no group to spawn into, no ＋ Agent', async (t) => {
  const { root: c } = await setup(t, { ...SNAPSHOT, groups: (SNAPSHOT.groups || []).map((g) => ({ ...g, archived: true })) });
  assert.equal(c.querySelector('[data-agent-add]'), null);
});

test('a group owner wears the desktop owner tag by its group, and a screen reader hears it', async (t) => {
  const owned = structuredClone(SNAPSHOT);
  owned.groups[0].members[1].owner = true;
  const { root } = await setup(t, owned);
  const rowOf = (name) => [...root.querySelectorAll('.phone-now-agent')].find((el) => el.querySelector('.phone-who-name')?.textContent === name);
  const tag = rowOf('lead').querySelector('.ui-tag.ui-tag-own.owner-badge');
  assert.ok(tag, 'the owner tag, as on the desktop member table');
  assert.equal(tag.textContent, 'owner');
  assert.equal(tag.previousElementSibling.textContent, 'webshop', 'it says it owns the group shown beside it');
  assert.match(rowOf('lead').querySelector('.phone-row').getAttribute('aria-label'), /^lead, working, owner of webshop/);
  assert.equal(rowOf('release-bot').querySelector('.owner-badge'), null, 'a member that owns nothing has none');
});

test('the sliders open View: Group by group first sorts each section by group, dots the sliders, is remembered, and Reset undoes it', async (t) => {
  const ago = (min) => new Date(Date.now() - min * 60_000).toISOString();
  const m = (n, title, status, min) => ({ agent_id: `agt_${String(n).padStart(32, '0')}`, conv_id: `c${n}`, title, online: true, state: { status, last_hook: ago(min) } });
  const snapshot = { groups: [
    { name: 'alpha', members: [m(1, 'old', 'idle', 20 * 60), m(2, 'alpha-busy', 'working', 30)] },
    { name: 'tclaude', members: [m(3, 'fresh', 'idle', 1), m(4, 'lead', 'working', 0)] },
  ] };
  const prefs = memPrefs();
  const { harness, root, page } = await setup(t, snapshot, { prefs });
  const names = () => [...harness.getByRole(root, 'group', { name: 'Online' }).querySelectorAll('.phone-who-name')].map((n) => n.textContent);
  const view = () => page.phoneBar.value.actions.find((act) => act.key === 'view');
  assert.deepEqual(names(), ['lead', 'alpha-busy', 'fresh', 'old'], 'off: most recent first in each section');
  assert.equal(view().dot, false);
  await harness.act(() => view().onClick());
  const box = root.querySelector('[data-now-view] input[name="now-view-bygroup"]');
  const reset = () => root.querySelector('[data-now-view] .phone-agents-view-reset');
  assert.ok(box, 'the View sheet has the toggle');
  assert.equal(reset().disabled, true);
  await harness.act(() => { box.checked = true; harness.fireEvent(box, 'change'); });
  assert.deepEqual(names(), ['alpha-busy', 'lead', 'old', 'fresh'], 'on: by group, then most recent first');
  assert.equal(view().dot, true, 'off its default: the sliders carry a dot');
  assert.equal(prefs.map.get('tclaude.dash.phone.now.bygroup'), '1', 'kept per viewer');
  await harness.act(() => reset().click());
  assert.deepEqual(names(), ['lead', 'alpha-busy', 'fresh', 'old']);
  assert.equal(prefs.map.has('tclaude.dash.phone.now.bygroup'), false);
});

test('a remembered Group by group first comes back on the next visit', async (t) => {
  const ago = (min) => new Date(Date.now() - min * 60_000).toISOString();
  const m = (n, title, min) => ({ agent_id: `agt_${String(n).padStart(32, '0')}`, conv_id: `c${n}`, title, online: true, state: { status: 'idle', last_hook: ago(min) } });
  const snapshot = { groups: [{ name: 'alpha', members: [m(1, 'old', 600)] }, { name: 'tclaude', members: [m(2, 'fresh', 1)] }] };
  const { harness, root, page } = await setup(t, snapshot, { prefs: memPrefs([['tclaude.dash.phone.now.bygroup', '1']]) });
  const names = [...harness.getByRole(root, 'group', { name: 'Online' }).querySelectorAll('.phone-who-name')].map((n) => n.textContent);
  assert.deepEqual(names, ['old', 'fresh']);
  assert.equal(page.phoneBar.value.actions.find((act) => act.key === 'view').dot, true);
});
