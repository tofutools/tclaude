import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });
const builder = { agent_id: 'agt_builder', name: 'builder' };

function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 6, needs_you: 2, next: 1, ideas: 1, blocked: 1, stalled: 1, in_progress: 1, done_this_week: 1 },
    tasks: [
      row('tk_00000001', 'queued one'),
      row('tk_00000002', 'building', { assignee: builder, assigned_at: ago(7) }),
      row('tk_00000003', 'held', { status: 'blocked', block_reason: 'waiting on CI' }),
      row('tk_00000004', 'an idea', { stage: 'idea' }),
      row('tk_00000006', 'gone quiet', { assignee: { agent_id: 'agt_lead', name: 'lead' }, stalled: true, idle_since: ago(9) }),
    ],
    closed: [
      row('tk_00000005', 'finished', { status: 'done', closed_at: ago(30), assignee: builder }),
      row('tk_00000007', 'long ago', { status: 'done', closed_at: ago(60 * 24 * 9) }),
      row('tk_00000008', 'dropped', { status: 'dropped', closed_at: ago(10) }),
    ],
  };
}

function fakeApi() {
  const calls = [];
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return {}; };
  return { calls, capture: record('capture'), importRefs: record('importRefs'), edit: record('edit'), move: record('move'),
    stage: record('stage'), status: record('status'), assign: record('assign'), unassign: record('unassign'),
    create: async (...args) => { calls.push(['create', ...args]); return { id: 'tk_00000099', title: args[1].title }; } };
}

const members = () => [
  { agent_id: 'agt_lead', title: 'lead', role: 'lead', online: true, owner: true, state: { status: 'idle' } },
  { agent_id: 'agt_builder', title: 'builder', role: 'dev', online: true, state: { status: 'working' } },
  { agent_id: 'agt_tester', title: 'tester', role: 'dev', online: true },
  { agent_id: 'agt_old', title: 'old', role: 'researcher', online: false },
  { conv_id: 'conv-no-agent', title: 'plain' },
];

async function mountLanes(t, name, { api = fakeApi(), by = null, groupExtra = {} } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, 'lanes');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  if (by) dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, by);
  const source = {
    context: harness.signals.signal({ groups: [board(name)] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {},
  };
  const group = { name, members: members(), ...groupExtra };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const cell = (lane, column) => c.querySelector(`.tb-cell[data-lane="${lane}"][data-column="${column}"]`);
  const ids = (lane, column) => [...(cell(lane, column)?.querySelectorAll('.tb-card') || [])].map((card) => card.dataset.taskId);
  const card = (id) => c.querySelector(`.tb-card[data-task-id="${id}"]`);
  const transfer = { setData() {}, effectAllowed: '', dropEffect: '' };
  const drag = async (id, lane, column) => {
    await harness.act(async () => { harness.fireEvent(card(id), 'dragstart', { dataTransfer: transfer }); });
    const over = harness.fireEvent(cell(lane, column), 'dragover', { dataTransfer: transfer });
    await harness.act(async () => {});
    const label = cell(lane, column).dataset.tbDrop || '';
    await harness.act(async () => { harness.fireEvent(cell(lane, column), 'drop', { dataTransfer: transfer }); await new Promise((r) => setTimeout(r, 0)); });
    return { taken: over.defaultPrevented, label };
  };
  return { harness, c, api, cell, ids, card, drag, dashPrefs };
}

test('laneColumn derives the five columns as the design says', async (t) => {
  const harness = await createPreactHarness(t);
  const { laneColumn, laneDrop, striped } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  const b = board('x');
  const all = [...b.tasks, ...b.closed];
  assert.deepEqual(all.map((task) => laneColumn(task)), ['next', 'progress', 'blocked', 'ideas', 'blocked', 'done', null, null]);
  assert.equal(striped('', 'progress'), true);
  assert.equal(striped('agt_builder', 'ideas'), true);
  assert.equal(striped('agt_builder', 'progress'), false);

  const [queued, building, held, idea] = b.tasks;
  const at = (lane, column) => ({ lane, column, name: lane });
  const kind = (task, from, to, opts) => laneDrop(task, from, to, opts)?.kind || null;
  // Same lane: stage and status changes.
  assert.equal(kind(idea, at('', 'ideas'), at('', 'next')), 'promote');
  assert.equal(kind(queued, at('', 'next'), at('', 'ideas')), 'demote');
  assert.equal(kind(queued, at('', 'next'), at('', 'blocked')), 'block');
  assert.equal(kind(held, at('', 'blocked'), at('', 'next')), 'unblock');
  assert.equal(kind(queued, at('', 'next'), at('', 'done')), 'done');
  assert.equal(kind(b.closed[0], at('agt_builder', 'done'), at('agt_builder', 'progress')), 'reopen');
  assert.equal(kind(held, at('', 'blocked'), at('', 'ideas')), null, 'a blocked task is not demoted');
  // Across lanes: assign and unassign.
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'progress')), 'assign');
  assert.equal(kind(idea, at('', 'ideas'), at('agt_builder', 'progress')), 'assign', 'assigning an idea promotes it');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('agt_tester', 'progress')), 'assign', 'reassign');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('', 'next')), 'unassign');
  assert.equal(kind(held, at('', 'blocked'), at('agt_builder', 'progress')), null, 'the server refuses a blocked assign');
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'ideas')), null, 'striped');
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'done')), null);
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'progress'), { assign: false }), null, 'no assign grant');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('agt_builder', 'done'), { assign: false }), null, "no closing someone's task without assign");
  assert.equal(kind(queued, at('', 'next'), at('', 'done'), { assign: false }), 'done', 'an unassigned task closes on write');
  // Across lanes AND columns: the lane's write, then the column's.
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'blocked')), 'assign+block');
  assert.equal(kind(idea, at('', 'ideas'), at('agt_builder', 'blocked')), 'assign+block');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('', 'ideas')), 'unassign+demote');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('', 'blocked')), 'unassign+block');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('agt_tester', 'blocked')), 'assign+block', 'reassign and block');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('', 'done')), null, 'unassign and close is not one move');
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'blocked'), { assign: false }), null);
});

test('the lanes draw the board by role, with the Unassigned lane first and offline lane groups folded', async (t) => {
  const { c, ids, cell, harness } = await mountLanes(t, 'tbl-role', { by: 'role' });
  assert.ok(c.querySelector('[data-taskboard-lanes="tbl-role"][data-lanes-by="role"]'));
  assert.equal(c.querySelector('table.tb-table'), null, 'no list in the lanes view');
  const lanes = [...c.querySelectorAll('[data-lane-row]')].map((el) => el.dataset.laneRow);
  assert.deepEqual(lanes, ['unassigned', 'agt_lead', 'agt_builder', 'agt_tester'], 'the offline researcher folds');
  assert.deepEqual([...c.querySelectorAll('[data-lane-group]')].map((el) => [...el.children].map((part) => part.textContent.trim())),
    [['lead', '1 agent'], ['dev', '2 agents'], ['researcher', '1 agent · offline', '▸ show offline lanes']]);
  assert.deepEqual(ids('unassigned', 'ideas'), ['tk_00000004']);
  assert.deepEqual(ids('unassigned', 'next'), ['tk_00000001']);
  assert.deepEqual(ids('unassigned', 'blocked'), ['tk_00000003']);
  assert.deepEqual(ids('agt_builder', 'progress'), ['tk_00000002']);
  assert.deepEqual(ids('agt_lead', 'blocked'), ['tk_00000006'], 'stalled needs you');
  assert.deepEqual(ids('agt_builder', 'done'), ['tk_00000005'], 'done this week only; dropped never');
  assert.ok(cell('unassigned', 'progress').classList.contains('tb-cell-striped'));
  assert.ok(cell('agt_builder', 'ideas').classList.contains('tb-cell-striped'));
  assert.match(c.querySelector('[data-lane-agent="agt_lead"]').textContent, /idle · owner/);
  assert.match(c.querySelector('[data-lane-agent="agt_lead"]').textContent, /holds 1/);
  assert.match(c.querySelector('.tb-card[data-task-id="tk_00000006"]').textContent, /idle 9m, no report/);

  const fold = c.querySelector('.tb-lane-fold');
  await harness.act(async () => { harness.fireEvent(fold, 'click'); });
  assert.ok(c.querySelector('[data-lane-row="agt_old"]'), 'unfolded on click');
});

test('List | Lanes and the lanes-by switch are remembered per group; lanes default to by agent', async (t) => {
  const { c, harness, dashPrefs } = await mountLanes(t, 'tbl-prefs');
  const button = (text) => [...c.querySelectorAll('.tb-view button')].find((b) => b.textContent.trim() === text);
  // By agent, the default: no role heads ("no role" never shows), the
  // offline agents fold under one.
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-by.tbl-prefs'), null);
  assert.ok(c.querySelector('[data-lanes-by="agent"]'));
  assert.equal(button('agent').getAttribute('aria-pressed'), 'true');
  assert.deepEqual([...c.querySelectorAll('[data-lane-group]')].map((el) => el.dataset.laneGroup), ['offline']);
  await harness.act(async () => { harness.fireEvent(button('role'), 'click'); });
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-by.tbl-prefs'), 'role', 'a choice is remembered and wins');
  assert.ok(c.querySelector('[data-lanes-by="role"]'));
  await harness.act(async () => { harness.fireEvent(button('☰ List'), 'click'); });
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.view.tbl-prefs'), 'list');
  assert.ok(c.querySelector('table.tb-table'));
  assert.equal(c.querySelector('.tb-lanes'), null);
  assert.equal(button('agent'), undefined, 'the lanes-by switch is a Lanes control');
});

test('lanes by subgroup fold a subgroup\'s members under it', async (t) => {
  const harness = await createPreactHarness(t);
  const { laneModel } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  const group = { name: 'top', members: members() };
  const snapshot = { groups: [group, { name: 'top-sub', parent: 'top', members: [{ agent_id: 'agt_tester' }] }] };
  const { groups } = laneModel(board('top'), group, 'subgroup', snapshot);
  assert.deepEqual(groups.map((g) => [g.label, g.lanes.map((lane) => lane.id)]),
    [['top', ['agt_builder', 'agt_lead', 'agt_old']], ['↳ top-sub', ['agt_tester']]]);
});

test('dragging a card maps to the CLI routes: stage, status, assign and unassign', async (t) => {
  const { api, drag, c, harness } = await mountLanes(t, 'tbl-drag');
  let out = await drag('tk_00000004', 'unassigned', 'next');
  assert.deepEqual(api.calls.pop(), ['stage', 'tk_00000004', 'next']);
  assert.equal(out.label, 'promote to next');
  await drag('tk_00000001', 'unassigned', 'done');
  assert.deepEqual(api.calls.pop(), ['status', 'tk_00000001', 'done']);
  await drag('tk_00000003', 'unassigned', 'next');
  assert.deepEqual(api.calls.pop(), ['status', 'tk_00000003', 'open']);
  out = await drag('tk_00000001', 'agt_builder', 'progress');
  assert.equal(out.label, 'assign to builder');
  assert.deepEqual(api.calls.pop(), ['assign', 'tk_00000001', 'agt_builder']);
  await drag('tk_00000002', 'unassigned', 'next');
  assert.deepEqual(api.calls.pop(), ['unassign', 'tk_00000002']);

  // A striped cell takes nothing.
  out = await drag('tk_00000001', 'agt_builder', 'ideas');
  assert.equal(out.taken, false);
  assert.deepEqual(api.calls, []);

  // → Blocked asks the reason first.
  await drag('tk_00000001', 'unassigned', 'blocked');
  assert.deepEqual(api.calls, []);
  const form = c.querySelector('.tb-lanes-block');
  assert.ok(form);
  harness.input(form.querySelector('input'), 'needs a design call');
  await harness.act(async () => {
    harness.fireEvent([...form.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Block'), 'click');
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.deepEqual(api.calls, [['status', 'tk_00000001', 'blocked', 'needs a design call']]);
  assert.equal(c.querySelector('.tb-lanes-block'), null);
});

test('without the assign grant, agent lanes take no drops', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { drag, api: calls } = await mountLanes(t, 'tbl-noassign', { api });
  const out = await drag('tk_00000001', 'agt_builder', 'progress');
  assert.equal(out.taken, false);
  await drag('tk_00000004', 'unassigned', 'next');
  assert.deepEqual(calls.calls, [['stage', 'tk_00000004', 'next']], 'stage changes still work');
});

test('a read-only board draws the lanes without drags or a cell +', async (t) => {
  const ro = await mountLanes(t, 'tbl-readonly', { api: null });
  assert.ok(ro.c.querySelector('.tb-lanes'));
  assert.equal(ro.c.querySelector('.tb-card[draggable]'), null);
  assert.equal(ro.c.querySelector('.tb-cell-add'), null);
});

test('a diagonal drag runs both writes through the real handlers, the lane first', async (t) => {
  const { api, drag, c, harness } = await mountLanes(t, 'tbl-diagonal');
  // Unassigned · Next → builder · Blocked: assign, then block with a reason.
  let out = await drag('tk_00000001', 'agt_builder', 'blocked');
  assert.equal(out.taken, true);
  assert.equal(out.label, 'assign to builder · block (asks why)');
  assert.deepEqual(api.calls, [], 'nothing runs before the reason');
  const form = c.querySelector('.tb-lanes-block');
  assert.match(form.textContent, /assign to builder · block #1/);
  await harness.input(form.querySelector('input'), 'waits on the API');
  await harness.act(async () => {
    harness.fireEvent([...form.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Block'), 'click');
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.deepEqual(api.calls.splice(0), [['assign', 'tk_00000001', 'agt_builder'], ['status', 'tk_00000001', 'blocked', 'waits on the API']]);

  // builder · In progress → Unassigned · Ideas: unassign, then demote.
  out = await drag('tk_00000002', 'unassigned', 'ideas');
  assert.equal(out.label, 'unassign · demote to ideas');
  assert.deepEqual(api.calls.splice(0), [['unassign', 'tk_00000002'], ['stage', 'tk_00000002', 'idea']]);

  // Unassigned · Ideas → tester · In progress: assign (an idea is promoted on assign).
  out = await drag('tk_00000004', 'agt_tester', 'progress');
  assert.deepEqual(api.calls.splice(0), [['assign', 'tk_00000004', 'agt_tester']]);

  // lead · Blocked (stalled) → builder · Next: reassign.
  await drag('tk_00000006', 'agt_builder', 'next');
  assert.deepEqual(api.calls.splice(0), [['assign', 'tk_00000006', 'agt_builder']]);

  // A move the board cannot make in one go is not taken.
  out = await drag('tk_00000002', 'unassigned', 'done');
  assert.equal(out.taken, false);
  assert.deepEqual(api.calls, []);
});

test('a cell + opens the New task form filled from the cell: the stage from the column, the assignee from the lane', async (t) => {
  const { api, c, cell, harness } = await mountLanes(t, 'tbl-add');
  const plus = (lane, column) => cell(lane, column).querySelector('.tb-cell-add');
  assert.ok(plus('unassigned', 'ideas') && plus('unassigned', 'next') && plus('agt_builder', 'next') && plus('agt_builder', 'progress'));
  assert.equal(plus('unassigned', 'progress'), null, 'striped');
  assert.equal(plus('agt_builder', 'ideas'), null, 'striped');
  assert.equal(plus('unassigned', 'blocked'), null, 'a new task does not land blocked');
  assert.equal(plus('agt_builder', 'done'), null);
  assert.match(plus('agt_builder', 'progress').getAttribute('aria-label'), /builder · In progress/);
  assert.ok(cell('agt_builder', 'progress').classList.contains('tb-cell-addable'), 'the cell keeps room for its +');
  assert.equal(c.querySelector('.tb-cell-add-title'), null, 'no inline quick-add');

  const addIn = async (lane, column, title) => {
    await harness.act(async () => { harness.fireEvent(plus(lane, column), 'click'); });
    const modal = c.querySelector('#taskboard-new-modal');
    assert.ok(modal, `the form opens from ${lane} · ${column}`);
    await harness.input(modal.querySelector('.tb-new-title'), title);
    await harness.act(async () => { harness.fireEvent(modal.querySelector('#taskboard-new-submit'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
    assert.equal(c.querySelector('#taskboard-new-modal'), null, 'the form closes after it adds');
  };
  await addIn('unassigned', 'ideas', 'an idea from the cell');
  await addIn('unassigned', 'next', 'queued from the cell');
  await addIn('agt_builder', 'progress', 'for builder');
  assert.deepEqual(api.calls, [
    ['create', 'tbl-add', { title: 'an idea from the cell', body: '', ref: '', stage: 'idea', after: [], resolveRef: false }],
    ['create', 'tbl-add', { title: 'queued from the cell', body: '', ref: '', stage: 'next', after: [], resolveRef: false }],
    ['create', 'tbl-add', { title: 'for builder', body: '', ref: '', stage: 'next', after: [], resolveRef: false }],
    ['assign', 'tk_00000099', 'agt_builder'],
  ], 'each form carries its cell\'s stage and assignee');
});

test('without the assign grant, agent lanes have no +', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { cell } = await mountLanes(t, 'tbl-add-noassign', { api });
  assert.ok(cell('unassigned', 'next').querySelector('.tb-cell-add'));
  assert.equal(cell('agt_builder', 'next').querySelector('.tb-cell-add'), null);
  assert.equal(cell('agt_builder', 'progress').querySelector('.tb-cell-add'), null);
});

test('a long agent name ends in an ellipsis with the full name as its title; the avatar keeps its own box', async (t) => {
  const long = 'better-resource-handling-for-the-lanes-view';
  const { c } = await mountLanes(t, 'tbl-long', { groupExtra: { members: [...members(), { agent_id: 'agt_long', title: long, role: 'dev', online: true }] } });
  const name = c.querySelector('[data-lane-agent="agt_long"] .tb-lane-name');
  assert.equal(name.getAttribute('title'), long);
  assert.equal(name.querySelector('.tb-lane-label').textContent, long, 'the label is its own span, so it can end in …');
  assert.equal(name.querySelector('.tb-avatar').textContent, 'B');
});

test("a linked node's group shows that node's board in lanes", async (t) => {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.view.tbl-peer', 'lanes');
  dashPrefs.setItem('tclaude.dash.groups.fused.taskboard.tbl-peer', '1');
  const peer = board('tbl-peer');
  peer.tasks = [row('tk_000000p1', 'on the peer')];
  peer.closed = [];
  const peerSource = { context: harness.signals.signal({ groups: [peer] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const local = { context: harness.signals.signal({ groups: [] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name: 'tbl-peer', members: members(), fleet_node: { id: 'nd_peer', local: false } };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${local}
    boards=${() => ({ source: peerSource, api: null })} />`);
  assert.deepEqual([...mounted.container.querySelectorAll('.tb-cell[data-lane="unassigned"][data-column="next"] .tb-card')].map((el) => el.dataset.taskId), ['tk_000000p1']);
});

test('a former member keeps an unfolded lane for what it holds, and takes no assign', async (t) => {
  const harness = await createPreactHarness(t);
  const { laneModel, laneDrop } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  const b = board('x');
  b.tasks.push(row('tk_00000009', 'left behind', { status: 'blocked', block_reason: 'x', assignee: { agent_id: 'agt_gone', name: 'gone' } }));
  const { groups, cells } = laneModel(b, { name: 'x', members: members() }, 'role');
  const former = groups.at(-1);
  assert.deepEqual([former.key, former.label, former.offline, former.lanes.map((l) => l.id)], ['former', 'no longer members', false, ['agt_gone']]);
  assert.deepEqual(cells.get('agt_gone|blocked').map((task) => task.id), ['tk_00000009']);
  const queued = b.tasks[0];
  assert.equal(laneDrop(queued, { lane: '', column: 'next' }, { lane: 'agt_gone', column: 'progress', member: false }), null);
});

test('a drag that is not a task drag drives no cell; the Done header counts the week', async (t) => {
  const { drag, cell, card, harness, api, c } = await mountLanes(t, 'tbl-types');
  assert.match(c.querySelector('.tb-col-done').textContent, /1$/);
  // A card drag whose dragend never came, then a text drag over a cell.
  await harness.act(async () => { harness.fireEvent(card('tk_00000004'), 'dragstart', { dataTransfer: { setData() {} } }); });
  const text = { types: ['text/plain'], dropEffect: '' };
  assert.equal(harness.fireEvent(cell('unassigned', 'next'), 'dragover', { dataTransfer: text }).defaultPrevented, false);
  await harness.act(async () => { harness.fireEvent(cell('unassigned', 'next'), 'drop', { dataTransfer: text }); });
  assert.deepEqual(api.calls, []);
  // A task drag still works.
  const out = await drag('tk_00000004', 'unassigned', 'next');
  assert.equal(out.taken, true);
});
