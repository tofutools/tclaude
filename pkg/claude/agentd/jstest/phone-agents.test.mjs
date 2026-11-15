// js/phone-agents-model.js and js/phone-agents-island.js: the phone Agents
// view (group chips, one row per agent, an agent's sheet, the short spawn
// sheet) driven through what the operator sees and taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const id = (n) => `agt_${String(n).padStart(32, '0')}`;

const SNAPSHOT = {
  groups: [
    {
      name: 'webshop',
      members: [
        { agent_id: id(1), conv_id: 'c1', title: 'lead', role: 'lead', online: true, state: { status: 'idle', harness: 'claude', context_pct: 41.6 }, branch: 'main' },
        { agent_id: id(2), conv_id: 'c2', title: 'payments-fix', role: 'dev', online: true, state: { status: 'awaiting_permission', status_detail: 'Allow npm test?', harness: 'codex' },
          branch_pr_number: 1469, branch_pr_url: 'https://example.test/pr/1469', branch_checks: { passed: 3, failed: 1, pending: 1 } },
        { agent_id: id(3), conv_id: 'c3', title: 'old-worker', role: 'dev', online: false, state: {} },
      ],
    },
    { name: 'steward', default_profile: 'companion', members: [{ agent_id: id(4), conv_id: 'c4', title: 'steward', online: true, state: { status: 'working' } }] },
  ],
  profiles: [{ name: 'companion' }, { name: 'reviewer' }, { name: 'off', disabled: true }],
  spawn_profile_default: 'dev',
  steward_active: id(4),
  ungrouped: [{ agent_id: id(5), conv_id: 'c5', title: 'loner', online: false, state: {} }],
};

test('the model: groups in order, loudest first, offline apart, PR and checks', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const groups = model.agentGroups(SNAPSHOT);
  assert.deepEqual(groups.map((g) => g.name), ['webshop', 'steward', 'Ungrouped']);
  const [webshop] = groups;
  assert.deepEqual(webshop.shown.map((r) => r.name), ['payments-fix', 'lead'], 'asking before idle');
  assert.deepEqual(webshop.offline.map((r) => r.name), ['old-worker']);
  const [asking, lead] = webshop.shown;
  assert.equal(asking.variant, 'asking');
  assert.equal(asking.detail, 'Allow npm test?');
  assert.deepEqual({ number: asking.pr.number, ci: asking.pr.ci }, { number: 1469, ci: 'failing' });
  assert.equal(lead.ctx, 42);
  assert.equal(lead.pr, null);
  assert.equal(lead.branch, 'main');
  assert.deepEqual(model.groupTabs(groups).map((tab) => `${tab.label} ${tab.online}/${tab.total}`),
    ['All 3/5', 'webshop 2/3', 'steward 1/1', 'Ungrouped 0/1']);
  assert.equal(model.shownGroups(groups, model.groupKey('steward')).length, 1);
  assert.equal(model.shownGroups(groups, 'gone').length, 3, 'a group that went away shows All');
  assert.equal(model.findRow(groups, id(5)).group.key, model.UNGROUPED);
  assert.equal(model.ciState({ passed: 2, pending: 1 }), 'running');
  assert.equal(model.ciState({ passed: 2 }), 'passing');
  assert.equal(model.ciState(null), '');
  // The pill says what the agent waits on.
  assert.deepEqual(asking.pill, { text: 'approval', tone: 'key', badge: '🔑' });
  assert.deepEqual(model.agentPill({ online: true, state: { status: 'awaiting_input' } }), { text: 'asking', tone: 'ask', badge: '❓' });
  assert.deepEqual(model.agentPill({ online: true, state: { status: 'error' } }), { text: 'error', tone: 'err', badge: '💥' });
  assert.deepEqual(lead.pill, { text: 'idle', tone: 'idle', badge: '' });
  assert.deepEqual(webshop.offline[0].pill, { text: 'offline', tone: 'off', badge: '' });
  assert.deepEqual(model.metaParts(lead), ['lead', 'claude', 'ctx 42%', 'main']);
  assert.deepEqual(model.metaParts(webshop.offline[0]), ['dev', 'resume ›']);
});

test('the model: what the spawn sheet offers and sends', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  assert.deepEqual(model.spawnChoices(SNAPSHOT), {
    groups: [{ name: 'webshop', profile: 'dev' }, { name: 'steward', profile: 'companion' }],
    profiles: ['companion', 'reviewer'],
  });
  assert.deepEqual(model.spawnBody({ prompt: 'Fix the flaky checkout test, please' }),
    { initial_message: 'Fix the flaky checkout test, please' }, 'no name: the daemon and the profile name it');
  assert.deepEqual(model.spawnBody({ prompt: 'x', name: 'my agent', profile: 'reviewer' }),
    { initial_message: 'x', name: 'my-agent', profile: 'reviewer' });
});

test('the model: crashed agents stay in view, group names never collide, an agent in two groups', async (t) => {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/phone-agents-model.js');
  const both = { agent_id: id(6), conv_id: 'c6', title: 'shared', online: true, state: {} };
  const groups = model.agentGroups({ groups: [
    { name: 'all', members: [both, { agent_id: id(7), conv_id: 'c7', title: 'dead', online: false, state: { exit_reason: 'unexpected' } }] },
    { name: 'ungrouped', members: [both] },
  ], ungrouped: [{ agent_id: id(8), conv_id: 'c8', title: 'x', online: true, state: {} }] });
  const [all, named] = groups;
  assert.deepEqual(all.shown.map((r) => r.name), ['shared', 'dead'], 'crashed is not folded away');
  assert.deepEqual({ live: all.live, total: all.total }, { live: 1, total: 2 });
  assert.equal(new Set(groups.map((g) => g.key)).size, 3);
  assert.deepEqual(model.shownGroups(groups, all.key), [all], 'a group named all is its own tab');
  assert.equal(model.findRow(groups, id(6), named.key).group, named);
  assert.equal(model.findRow(groups, id(6)).group, all);
});

// settle lets an async submit (its fetch and what follows) finish.
async function settle(harness) {
  for (let i = 0; i < 5; i += 1) await harness.act(async () => { await Promise.resolve(); });
}

async function setup(t, { fetchImpl = async () => ({ ok: true, json: async () => ({}) }) } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, island, sheet, page, board] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-agents-island.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
    harness.importDashboardModule('js/phone-page.js'),
    harness.importDashboardModule('js/phone-board.js'),
  ]);
  const state = createDashboardState();
  state.beginRequest();
  state.commitRequest(1, SNAPSHOT);
  const calls = [];
  const record = (name) => (...args) => { calls.push([name, ...args.map((a) => a?.name ?? a?.key ?? a)]); };
  const actions = {
    openTerminal: record('openTerminal'), message: record('message'), rename: record('rename'), spawn: record('spawn'),
    rowAction: (act, row) => calls.push(['rowAction', act, row.name]),
  };
  const mounted = await harness.mount(harness.html`<main><${island.PhoneAgents} state=${state} order=${(g) => g} actions=${actions} fetchImpl=${fetchImpl} /></main>
    <${sheet.PhoneSheetHost} />`);
  const c = mounted.container;
  const tap = (element) => harness.act(() => element.click());
  const item = (name) => [...c.querySelectorAll('.phone-agent')].find((li) => li.querySelector('.phone-agent-title').textContent === name);
  const row = (name) => item(name).querySelector('.phone-agent-row');
  const more = (name) => item(name).querySelector('.phone-agent-more');
  const menu = (label) => [...c.querySelectorAll('.phone-sheet .phone-menu-item')].find((b) => b.textContent === label);
  return { harness, state, calls, c, tap, item, row, more, menu, page, board };
}

test('rows: an avatar, a pill, the meta line; offline agents inline; the chips filter', async (t) => {
  const { c, tap, item, row } = await setup(t);
  assert.equal(row('payments-fix').querySelector('.phone-pill').textContent, 'approval');
  assert.ok(row('payments-fix').querySelector('.phone-pill-key'));
  assert.equal(row('payments-fix').querySelector('.phone-av-badge').textContent, '🔑');
  assert.ok(row('payments-fix').textContent.includes('PR #1469 ✗'));
  assert.equal(row('lead').querySelector('.phone-agent-meta').textContent, 'lead · claude · ctx 42% · main');
  assert.equal(row('steward').querySelector('.phone-av').textContent, '✦', 'the steward wears its own glyph');
  // Offline agents sit in their group's list, dimmed, after the online ones.
  const webshop = [...c.querySelector('[aria-label="webshop"]').querySelectorAll('.phone-agent-title')].map((n) => n.textContent);
  assert.deepEqual(webshop, ['payments-fix', 'lead', 'old-worker']);
  assert.ok(item('old-worker').classList.contains('phone-agent-off'));
  assert.ok(row('old-worker').querySelector('.phone-av-dim'));
  assert.equal(row('old-worker').querySelector('.phone-agent-meta').textContent, 'dev · resume ›');
  assert.equal(c.querySelector('details'), null, 'no Offline fold');
  const tabs = [...c.querySelectorAll('[role=tab]')];
  assert.deepEqual(tabs.map((tab) => tab.textContent), ['All 5', 'webshop 3', 'steward 1', 'Ungrouped 1']);
  await tap(tabs[2]);
  assert.deepEqual([...c.querySelectorAll('.phone-agent-title')].map((n) => n.textContent), ['steward']);
});

test('a tap opens an online agent\'s terminal and wakes an offline one; ⋯ opens the sheet', async (t) => {
  const { harness, calls, c, tap, row, more, menu } = await setup(t);
  await tap(row('lead'));
  assert.deepEqual(calls.at(-1), ['openTerminal', 'lead']);
  assert.equal(c.querySelector('.phone-sheet'), null);
  await tap(row('old-worker'));
  assert.deepEqual(calls.at(-1), ['rowAction', 'dot-toggle', 'old-worker']);

  await tap(more('lead'));
  const dialog = c.querySelector('.phone-sheet[role=dialog]');
  assert.equal(dialog.getAttribute('aria-label'), 'lead');
  assert.equal(c.querySelector('main').inert, true, 'the page behind is inert');
  assert.deepEqual([...dialog.querySelectorAll('.phone-menu-item')].map((b) => b.textContent),
    ['Open terminal', 'Message…', 'Shut down…', 'Restart…', 'Rename…', 'Retire…']);
  await tap(menu('Restart…'));
  assert.deepEqual(calls.at(-1), ['rowAction', 'restart', 'lead']);
  assert.equal(c.querySelector('.phone-sheet'), null, 'the sheet closed');
  assert.equal(c.querySelector('main').inert, false);

  await tap(more('lead'));
  await tap(menu('Rename…'));
  assert.deepEqual(calls.at(-1), ['rename', 'lead', 'webshop']);

  // An offline agent offers Power on and no terminal; Escape closes.
  await tap(more('loner'));
  assert.deepEqual([...c.querySelectorAll('.phone-sheet .phone-menu-item')].map((b) => b.textContent),
    ['Message…', 'Power on', 'Retire…'], 'no rename outside a group');
  await harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  assert.equal(c.querySelector('.phone-sheet'), null);
});

test('the sheet follows the snapshot', async (t) => {
  const { harness, state, c, tap, more } = await setup(t);
  await tap(more('lead'));
  await harness.act(() => {
    state.beginRequest();
    state.commitRequest(2, { groups: [], ungrouped: [] });
  });
  assert.ok(c.querySelector('.phone-sheet').textContent.includes('no longer listed'));
});

test('Board › opens the group\'s board', async (t) => {
  const { c, tap, board } = await setup(t);
  const before = board.boardRequest.value;
  await tap(c.querySelector('[aria-label="webshop\'s board"]'));
  assert.equal(board.boardRequest.value, before + 1);
  assert.equal(c.querySelector('[aria-label="Ungrouped\'s board"]'), null, 'no board outside a group');
});

test('＋ in the top bar spawns from a short sheet: group, profile, prompt', async (t) => {
  const sent = [];
  const fetchImpl = async (url, init) => { sent.push([url, JSON.parse(init.body)]); return { ok: true, json: async () => ({ conv_id: 'c9' }) }; };
  const { harness, calls, c, tap, page } = await setup(t, { fetchImpl });
  const bar = page.phoneBar.value;
  assert.equal(bar.tab, 'groups');
  assert.deepEqual(bar.actions.map((a) => a.icon), ['＋']);
  // On a group's chip the sheet starts on that group.
  await tap([...c.querySelectorAll('[role=tab]')][2]);
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  const form = c.querySelector('.phone-sheet form');
  const [groupSel, profileSel] = form.querySelectorAll('select');
  assert.equal(groupSel.value, 'steward');
  assert.deepEqual([...profileSel.options].map((o) => o.textContent), ['Group default (companion)', 'reviewer']);
  assert.equal(form.querySelector('[type=submit]').disabled, true, 'no prompt yet');
  await harness.input(form.querySelector('textarea'), 'Review the open PRs');
  await harness.act(() => {
    for (const option of profileSel.options) option.toggleAttribute('selected', option.value === 'reviewer');
    profileSel.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  });
  await harness.fireEvent(form, 'submit');
  await settle(harness);
  assert.deepEqual(sent, [['/api/groups/steward/spawn', { initial_message: 'Review the open PRs', profile: 'reviewer' }]]);
  assert.equal(c.querySelector('.phone-sheet'), null, 'the sheet closed');

  // All options… is the desktop form, on the group picked.
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  await tap([...c.querySelectorAll('.phone-sheet button')].find((b) => b.textContent === 'All options…'));
  assert.deepEqual(calls.at(-1), ['spawn', 'steward']);
});

test('a refused spawn keeps the sheet open', async (t) => {
  const fetchImpl = async () => ({ ok: false, status: 403, text: async () => 'not allowed' });
  const { harness, c, page } = await setup(t, { fetchImpl });
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  const form = c.querySelector('.phone-sheet form');
  await harness.input(form.querySelector('textarea'), 'go');
  await harness.fireEvent(form, 'submit');
  await settle(harness);
  assert.ok(c.querySelector('.phone-sheet form'), 'still open');
  assert.equal(form.querySelector('[type=submit]').disabled, false);
});

test('the sheet opened from an agent\'s own terminal does not offer that terminal', async (t) => {
  const { harness, state, c, tap, more } = await setup(t);
  const [island, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-agents-island.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  t.after(() => sheet.closePhoneSheet());
  await tap(more('lead'));
  const props = { ...sheet.phoneSheet.value.props, here: true };
  await harness.act(() => sheet.openPhoneSheet({ label: 'lead (terminal)', Body: island.AgentSheet, props: { ...props, state } }));
  assert.deepEqual([...c.querySelectorAll('.phone-sheet .phone-menu-item')].map((b) => b.textContent),
    ['Message…', 'Shut down…', 'Restart…', 'Rename…', 'Retire…']);
});

test('a double tap on Spawn spawns once', async (t) => {
  const sent = [];
  let release;
  const fetchImpl = (url) => { sent.push(url); return new Promise((resolve) => { release = () => resolve({ ok: true, json: async () => ({}) }); }); };
  const { harness, c, page } = await setup(t, { fetchImpl });
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  const form = c.querySelector('.phone-sheet form');
  await harness.input(form.querySelector('textarea'), 'go');
  form.dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
  form.dispatchEvent(new harness.window.Event('submit', { bubbles: true, cancelable: true }));
  await settle(harness);
  assert.equal(sent.length, 1);
  release();
  await settle(harness);
});

test("on a peer's page ＋ is the desktop form, which spawns there", async (t) => {
  globalThis.__tclaudeRemoteNode = { id: 'inst_peer1' };
  t.after(() => { delete globalThis.__tclaudeRemoteNode; });
  const { harness, calls, c, page } = await setup(t);
  await harness.act(() => page.phoneBar.value.actions[0].onClick());
  assert.equal(c.querySelector('.phone-sheet'), null);
  assert.deepEqual(calls.at(-1), ['spawn', '']);
});

test('a row tells a screen reader what the agent waits on', async (t) => {
  const { row } = await setup(t);
  assert.equal(row('payments-fix').getAttribute('aria-label'), 'payments-fix, approval (Allow npm test?): open its terminal');
});
