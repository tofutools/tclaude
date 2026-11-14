// js/phone-agents-model.js and js/phone-agents-island.js: the phone Agents
// view (group tabs, one row per agent, an agent's sheet) driven through what
// the operator sees and taps.
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
        { agent_id: id(2), conv_id: 'c2', title: 'payments-fix', role: 'dev', online: true, state: { status: 'awaiting_input', status_detail: 'Allow npm test?', harness: 'codex' },
          branch_pr_number: 1469, branch_pr_url: 'https://example.test/pr/1469', branch_checks: { passed: 3, failed: 1, pending: 1 } },
        { agent_id: id(3), conv_id: 'c3', title: 'old-worker', role: 'dev', online: false, state: {} },
      ],
    },
    { name: 'steward', members: [{ agent_id: id(4), conv_id: 'c4', title: 'steward', online: true, state: { status: 'working' } }] },
  ],
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

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, island, sheet] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-agents-island.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
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
  const mounted = await harness.mount(harness.html`<main><${island.PhoneAgents} state=${state} order=${(g) => g} actions=${actions} /></main>
    <${sheet.PhoneSheetHost} />`);
  const c = mounted.container;
  const tap = (element) => harness.act(() => element.click());
  const row = (name) => [...c.querySelectorAll('.phone-agent-row')].find((b) => b.textContent.includes(name));
  const menu = (label) => [...c.querySelectorAll('.phone-sheet .phone-menu-item')].find((b) => b.textContent === label);
  return { harness, state, calls, c, tap, row, menu };
}

test('rows show state, meta and PR; the group tabs filter', async (t) => {
  const { c, tap, row } = await setup(t);
  assert.equal(row('payments-fix').querySelector('.phone-agent-state').textContent, 'asking');
  assert.ok(row('payments-fix').textContent.includes('PR #1469 ✗'));
  assert.ok(row('lead').textContent.includes('lead · claude · ctx 42% · ⎇ main'));
  assert.equal(c.querySelector('.phone-agents-offline summary').textContent, 'Offline · 1');
  const tabs = [...c.querySelectorAll('[role=tab]')];
  assert.deepEqual(tabs.map((tab) => tab.textContent), ['All 3/5', 'webshop 2/3', 'steward 1/1', 'Ungrouped 0/1']);
  await tap(tabs[2]);
  assert.deepEqual([...c.querySelectorAll('.phone-agent-row')].map((b) => b.querySelector('.phone-agent-title').textContent), ['steward']);
});

test('a row opens its sheet; the sheet runs actions and closes', async (t) => {
  const { harness, calls, c, tap, row, menu } = await setup(t);
  await tap(row('lead'));
  const dialog = c.querySelector('.phone-sheet[role=dialog]');
  assert.equal(dialog.getAttribute('aria-label'), 'lead');
  assert.equal(c.querySelector('main').inert, true, 'the page behind is inert');
  assert.deepEqual([...dialog.querySelectorAll('.phone-menu-item')].map((b) => b.textContent),
    ['Open terminal', 'Message…', 'Shut down…', 'Restart…', 'Rename…', 'Retire…']);
  await tap(menu('Restart…'));
  assert.deepEqual(calls.at(-1), ['rowAction', 'restart', 'lead']);
  assert.equal(c.querySelector('.phone-sheet'), null, 'the sheet closed');
  assert.equal(c.querySelector('main').inert, false);

  await tap(row('lead'));
  await tap(menu('Rename…'));
  assert.deepEqual(calls.at(-1), ['rename', 'lead', 'webshop']);

  // An offline agent offers Power on and no terminal; Escape closes.
  await tap(row('loner'));
  assert.deepEqual([...c.querySelectorAll('.phone-sheet .phone-menu-item')].map((b) => b.textContent),
    ['Message…', 'Power on', 'Retire…'], 'no rename outside a group');
  await harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  assert.equal(c.querySelector('.phone-sheet'), null);
});

test('the sheet follows the snapshot, and New spawns into the group', async (t) => {
  const { harness, state, calls, c, tap, row } = await setup(t);
  await tap(row('lead'));
  await harness.act(() => {
    state.beginRequest();
    state.commitRequest(2, { groups: [], ungrouped: [] });
  });
  assert.ok(c.querySelector('.phone-sheet').textContent.includes('no longer listed'));
  await harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  await harness.act(() => {
    state.beginRequest();
    state.commitRequest(3, SNAPSHOT);
  });
  await tap(c.querySelector('[aria-label="New agent in webshop"]'));
  assert.deepEqual(calls.at(-1), ['spawn', 'webshop']);
});
