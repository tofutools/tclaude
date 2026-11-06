import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// tcl-usrm7h: the Collab panel's ⚙ is the Groups tab's own edit menu.
const SNAP = {
  groups: [
    { id: 1, name: 'crew', members: [{ conv_id: 'ca', agent_id: 'agt_a', title: 'alpha', owner: true }] },
    { id: 2, name: 'lab', members: [] },
  ],
  agents: [{ agent_id: 'agt_a', conv_id: 'ca', title: 'alpha', groups: ['crew'] }],
  group_resources: [
    { kind: 'board', id: 'plan', group_id: 1, title: 'Plan', state: 'closed' },
    { kind: 'board', id: 'loose', group_id: 0, title: 'Loose', state: 'closed' },
  ],
};

const settle = (harness) => harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));

test('a board\'s ⚙ is its Groups row menu: Rename…, Move to group › and Delete…', async (t) => {
  const harness = await createPreactHarness(t);
  const gear = await harness.importDashboardModule('js/collab-rail-gear.js');
  const calls = [];
  const notes = [];
  const api = { moveBoard: async (...a) => calls.push(['move', ...a]), deleteBoard: async (...a) => calls.push(['delete', ...a]) };
  let renamed = 0;
  const item = { key: 'board:plan', kind: 'board', title: 'Plan', group: 'crew', resource: SNAP.group_resources[0] };
  const { container } = await harness.mount(harness.html`<${gear.RailGear} item=${item} snapshot=${{ value: SNAP }}
    notify=${(...a) => notes.push(a)} onRename=${() => { renamed++; }} ctx=${{ api, source: { refresh: () => {} }, confirm: async () => true }} />`);
  const button = container.querySelector('.cr-gear-btn');
  assert.equal(button.textContent, '⚙');
  assert.equal(button.getAttribute('title'), 'Edit Plan');
  await harness.act(() => harness.fireEvent(button, 'click'));
  const labels = () => [...container.querySelectorAll('.gr-add-menu [role="menuitem"], .gr-add-menu button')].map((el) => el.textContent.trim());
  assert.deepEqual(labels(), ['Rename…', 'Move to group ›', 'Delete…']);
  await harness.act(() => harness.fireEvent(container.querySelector('[data-resource-rename="plan"]'), 'click'));
  assert.equal(renamed, 1, 'Rename… asks the row for its name field');
  await harness.act(() => harness.fireEvent(button, 'click'));
  await harness.act(() => harness.fireEvent(container.querySelector('[data-resource-move="plan"]'), 'click'));
  assert.deepEqual(labels(), ['‹ Back', 'lab'], 'the other groups');
  await harness.act(() => harness.fireEvent(container.querySelector('[data-move-to="lab"]'), 'click'));
  await settle(harness);
  await harness.act(() => harness.fireEvent(button, 'click'));
  await harness.act(() => harness.fireEvent(container.querySelector('[data-board-delete="plan"]'), 'click'));
  await settle(harness);
  assert.deepEqual(calls, [['move', 'plan', 'lab'], ['delete', 'plan']]);
  assert.deepEqual(notes, [['Moved Plan to lab'], ['Deleted Plan']]);
});

test('an agent\'s ⚙ is its Groups row ⚙ for the group it is listed under, with Move to group', async (t) => {
  const harness = await createPreactHarness(t);
  const [gear, interactions] = await Promise.all([
    harness.importDashboardModule('js/collab-rail-gear.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
  ]);
  const calls = [];
  const groupOps = {
    context: () => 'ctx',
    move: async (payload, to) => calls.push(['move', payload.conv, payload.sourceGroup, to]),
    remove: async (payload) => calls.push(['remove', payload.conv, payload.sourceGroup]),
    add: async () => {},
  };
  const item = { key: 'pane:agent:a', kind: 'terminal', title: 'alpha', group: 'crew', status: { agent: SNAP.agents[0] } };
  const { container } = await harness.mount(harness.html`<${interactions.GroupsInteractionProvider}>
    <${gear.RailGear} item=${item} snapshot=${{ value: SNAP }} notify=${() => {}} ctx=${{ groupOps }} /><//>`);
  await harness.act(() => harness.fireEvent(container.querySelector('.cr-gear .cog-btn'), 'click'));
  const menu = container.querySelector('.cr-gear .action-menu');
  assert.ok(menu.classList.contains('open'));
  assert.ok(menu.querySelector('[data-act="remove-member"][data-group="crew"]'), 'the group\'s own items, for crew');
  assert.ok(menu.querySelector('[data-act="revoke-owner"][data-group="crew"]'), 'read from its crew member row (an owner)');
  assert.equal(menu.querySelector('[data-act="edit-member"]'), null, 'not the Groups row\'s inline editor');
  assert.ok(menu.querySelector('[data-act="perm-edit"]'), 'the agent-level items too');
  assert.deepEqual([...menu.querySelectorAll('[data-rail-move-to]')].map((el) => el.dataset.railMoveTo), ['lab', '']);
  // The Groups row items are handled on the document (row-actions.js): a
  // click on one must get there.
  const reached = [];
  const spy = (event) => { const act = event.target.closest?.('[data-act]'); if (act) reached.push([act.dataset.act, act.dataset.group || '']); };
  harness.document.addEventListener('click', spy);
  t.after(() => harness.document.removeEventListener('click', spy));
  await harness.act(() => harness.fireEvent(menu.querySelector('[data-act="remove-member"]'), 'click'));
  assert.deepEqual(reached, [['remove-member', 'crew']], 'a data-act item reaches the document handler');
  await harness.act(() => harness.fireEvent(container.querySelector('.cr-gear .cog-btn'), 'click'));
  await harness.act(() => harness.fireEvent(menu.querySelector('[data-rail-move-to="lab"]'), 'click'));
  await harness.act(() => harness.fireEvent(container.querySelector('.cr-gear .cog-btn'), 'click'));
  await harness.act(() => harness.fireEvent(container.querySelector('[data-rail-move-to=""]'), 'click'));
  await settle(harness);
  assert.deepEqual(calls, [['move', 'ca', 'crew', 'lab'], ['remove', 'ca', 'crew']], 'through the Groups tab\'s drop operations');
});

test('a shell, a process and another node\'s agent get no ⚙', async (t) => {
  const harness = await createPreactHarness(t);
  const gear = await harness.importDashboardModule('js/collab-rail-gear.js');
  assert.equal(gear.railGearKind({ key: 'shell:crew', kind: 'shell', group: 'crew' }), '');
  assert.equal(gear.railGearKind({ key: 'process:p', kind: 'process', group: 'crew' }), '');
  assert.equal(gear.railGearKind({ key: 'pane:x', kind: 'terminal', status: { agent: SNAP.agents[0] }, pane: { seed: { remote: { peer: 'p', agent: 'x' } } } }), '');
  assert.equal(gear.railGearKind({ key: 'page:p', kind: 'page', title: 'P', group: '' }), '', 'a page with no group row: nowhere known to go');
  assert.deepEqual(gear.agentMoveTargets(SNAP, ''), ['crew', 'lab'], 'an agent in no group can only go into one');
});

test('the panel lists a board in no group under Not in a group, and its ⚙ renames it in place', async (t) => {
  const harness = await createPreactHarness(t);
  const [rail, pc] = await Promise.all([
    harness.importDashboardModule('js/collab-rail.js'),
    harness.importDashboardModule('js/process-context.js'),
  ]);
  const renamed = [];
  const real = pc.processRunApi.renameBoard;
  pc.processRunApi.renameBoard = async (...args) => renamed.push(args);
  t.after(() => { pc.processRunApi.renameBoard = real; });
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.collab.filter', JSON.stringify({ states: ['closed', 'open', 'background'], kinds: ['boards'] }));
  const { container } = await harness.mount(harness.html`<${rail.CollabRail} panes=${[]} actions=${{}} snapshot=${{ value: SNAP }} />`);
  const root = container.querySelector('[data-collab-rail]');
  const none = root.querySelector('[data-rail-group="Not in a group"]');
  assert.ok(none?.querySelector('[data-rail-item="board:loose"]'), 'the loose board is listed');
  const row = none.querySelector('[data-rail-item="board:loose"]');
  await harness.act(() => harness.fireEvent(row.querySelector('.cr-gear-btn'), 'click'));
  await harness.act(() => harness.fireEvent(row.querySelector('[data-resource-rename="loose"]'), 'click'));
  const input = root.querySelector('[data-rail-item="board:loose"] input[data-resource-rename-input]');
  assert.ok(input, 'the name turns into a field');
  input.value = 'Roadmap';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  await settle(harness);
  assert.deepEqual(renamed, [['loose', 'Roadmap']]);
});
