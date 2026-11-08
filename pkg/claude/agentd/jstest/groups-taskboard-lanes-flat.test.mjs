import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });
const builder = { agent_id: 'agt_builder', name: 'builder' };
const operator = { agent_id: 'operator', name: 'operator', operator: true };

function board(group, extra = []) {
  return {
    group, group_id: 1,
    counts: { live: 5, next: 1, ideas: 1, blocked: 0, in_progress: 2, done_this_week: 2 },
    tasks: [
      row('tk_00000001', 'queued one'),
      row('tk_00000002', 'building', { assignee: builder, assigned_at: ago(7) }),
      row('tk_00000003', 'which colour?', { assignee: operator, assigned_by: 'agent:agt_builder' }),
      row('tk_00000004', 'an idea', { stage: 'idea' }),
      ...extra,
    ],
    closed: [
      row('tk_00000005', 'finished', { status: 'done', closed_at: ago(30), assignee: builder }),
      row('tk_00000006', 'closed unassigned', { status: 'done', closed_at: ago(40) }),
      row('tk_00000007', 'answered', { status: 'done', closed_at: ago(50), assignee: operator }),
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
  { agent_id: 'agt_builder', title: 'builder', role: 'dev', online: true, state: { status: 'working' } },
  { agent_id: 'agt_tester', title: 'tester', role: 'dev', online: true },
];

async function mountFlat(t, name, { api = fakeApi(), mode = 'triage', extra = [] } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, 'lanes');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  if (mode) dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, mode);
  const source = {
    context: harness.signals.signal({ groups: [board(name, extra)] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {},
  };
  const group = { name, members: members() };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const cell = (r, column) => c.querySelector(`.tb-cell[data-board-row="${r}"][data-column="${column}"]`);
  const ids = (r, column) => [...(cell(r, column)?.querySelectorAll('.tb-card') || [])].map((card) => card.dataset.taskId);
  const card = (id) => c.querySelector(`.tb-card[data-task-id="${id}"]`);
  const rows = () => [...c.querySelectorAll('[data-board-row-head]')].map((el) => el.dataset.boardRowHead);
  const transfer = { setData() {}, effectAllowed: '', dropEffect: '' };
  const drag = async (id, r, column) => {
    await harness.act(async () => { harness.fireEvent(card(id), 'dragstart', { dataTransfer: transfer }); });
    const over = harness.fireEvent(cell(r, column), 'dragover', { dataTransfer: transfer });
    await harness.act(async () => {});
    const label = cell(r, column).dataset.tbDrop || '';
    await harness.act(async () => { harness.fireEvent(cell(r, column), 'drop', { dataTransfer: transfer }); await new Promise((res) => setTimeout(res, 0)); });
    await harness.act(async () => { harness.fireEvent(card(id), 'dragend', { dataTransfer: transfer }); });
    return { taken: over.defaultPrevented, label };
  };
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((res) => setTimeout(res, 0)); });
  return { harness, c, api, cell, ids, card, rows, drag, click, dashPrefs };
}

test('lanes: triage draws You, Unassigned and Agents, each card marked with who holds it', async (t) => {
  const { c, ids, card, rows } = await mountFlat(t, 'tlf-triage');
  assert.equal(c.querySelector('[data-taskboard-lanes]').dataset.lanesBy, 'triage');
  assert.deepEqual(rows(), ['operator', 'unassigned', 'agents']);
  assert.deepEqual(ids('operator', 'progress'), ['tk_00000003']);
  assert.deepEqual(ids('unassigned', 'next'), ['tk_00000001']);
  assert.deepEqual(ids('unassigned', 'ideas'), ['tk_00000004']);
  assert.deepEqual(ids('agents', 'progress'), ['tk_00000002']);
  assert.deepEqual(ids('agents', 'done'), ['tk_00000005']);
  assert.ok(c.querySelector('[data-board-row-head="operator"]').classList.contains('tb-lane-operator'), 'You is tinted as the operator lane is');

  const marker = (id) => card(id).querySelector('.tb-card-marker');
  assert.ok(marker('tk_00000001').classList.contains('tb-marker-unassigned'), 'unassigned: the yellow !');
  assert.equal(marker('tk_00000001').textContent, '!');
  assert.ok(card('tk_00000001').classList.contains('tb-card-unassigned'));
  assert.ok(marker('tk_00000003').classList.contains('tb-marker-operator'), 'the operator\'s: the red !');
  assert.equal(marker('tk_00000003').getAttribute('title'), 'Assigned to you');
  assert.ok(card('tk_00000003').classList.contains('tb-card-operator'));
  assert.ok(marker('tk_00000002').classList.contains('tb-avatar'), 'an agent\'s: its avatar');
  assert.equal(marker('tk_00000002').textContent, 'B');
  assert.equal(marker('tk_00000002').getAttribute('title'), 'builder');
  assert.equal(marker('tk_00000006'), null, 'a done task needs nobody: no !');
  assert.ok(marker('tk_00000007').classList.contains('tb-avatar-operator'), 'the operator\'s done task: their avatar, no !');
  assert.equal(marker('tk_00000007').textContent, '★');
});

test('the lanes switch offers triage and one row, remembered per group; one row holds every task', async (t) => {
  const { c, ids, rows, click, dashPrefs } = await mountFlat(t, 'tlf-mode', { mode: null });
  const modes = [...c.querySelectorAll('[aria-label="Lanes"] button')];
  assert.deepEqual(modes.map((b) => b.textContent), ['agent', 'role', 'subgroup', 'triage', 'one row']);
  assert.equal(c.querySelector('.tb-card-marker'), null, 'lanes by agent: the lane says who holds it, no markers');
  await click(modes[4]);
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.lanes-by.tlf-mode'), 'row');
  assert.deepEqual(rows(), ['row']);
  assert.deepEqual(ids('row', 'progress'), ['tk_00000002', 'tk_00000003'], 'agents\' and the operator\'s tasks share the row, in board order');
  assert.deepEqual(ids('row', 'done'), ['tk_00000005', 'tk_00000006', 'tk_00000007']);
  assert.equal(c.querySelector('.tb-col-lane'), null, 'no row heads in one row');
});

test('Triage drags: onto You assigns to the operator, onto Unassigned unassigns, Agents takes only an agent\'s own card', async (t) => {
  const { drag, api } = await mountFlat(t, 'tlf-drag');
  assert.equal((await drag('tk_00000001', 'operator', 'progress')).taken, true);
  assert.deepEqual(api.calls, [['assign', 'tk_00000001', 'operator']]);
  api.calls.length = 0;
  await drag('tk_00000002', 'unassigned', 'next');
  assert.deepEqual(api.calls, [['unassign', 'tk_00000002']]);
  api.calls.length = 0;
  const out = await drag('tk_00000001', 'agents', 'progress');
  assert.equal(out.taken, false, 'Agents does not say which agent');
  assert.equal((await drag('tk_00000003', 'agents', 'progress')).taken, false, 'nor for the operator\'s card');
  assert.deepEqual(api.calls, []);
  await drag('tk_00000002', 'agents', 'done');
  assert.deepEqual(api.calls, [['status', 'tk_00000002', 'done']], 'an agent\'s card moves along Agents');
});

test('one-row drags change the column and keep the assignee', async (t) => {
  const { drag, api } = await mountFlat(t, 'tlf-all-drag', { mode: 'row' });
  await drag('tk_00000004', 'row', 'next');
  assert.deepEqual(api.calls, [['stage', 'tk_00000004', 'next']]);
  api.calls.length = 0;
  assert.equal((await drag('tk_00000001', 'row', 'progress')).taken, false, 'an unassigned task is not in progress without an agent');
  await drag('tk_00000002', 'row', 'done');
  assert.deepEqual(api.calls, [['status', 'tk_00000002', 'done']]);
});

test('a triage cell draws five cards and "+ n more"', async (t) => {
  const extra = ['a1', 'a2', 'a3', 'a4', 'a5', 'a6'].map((n) => row(`tk_000000${n}`, `idea ${n}`, { stage: 'idea' }));
  const { cell, ids, click } = await mountFlat(t, 'tlf-cap', { extra });
  assert.equal(ids('unassigned', 'ideas').length, 5, 'seven ideas, five drawn');
  const more = cell('unassigned', 'ideas').querySelector('.tb-cell-more');
  assert.equal(more.textContent, '+ 2 more');
  await click(more);
  assert.equal(ids('unassigned', 'ideas').length, 7);
});

test('a read-only triage has no drags and no cell +', async (t) => {
  const ro = await mountFlat(t, 'tlf-ro', { api: null });
  assert.equal(ro.c.querySelector('.tb-card[draggable]'), null);
  assert.equal(ro.c.querySelector('.tb-cell-add'), null);
  assert.deepEqual(ro.rows(), ['operator', 'unassigned', 'agents'], 'You still shows while the operator holds a task');
});

test('without the assign grant, triage takes no drop onto You or Unassigned from another lane; columns still move', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { drag, cell } = await mountFlat(t, 'tlf-noassign', { api });
  assert.equal((await drag('tk_00000001', 'operator', 'progress')).taken, false, 'no assign to the operator');
  assert.equal((await drag('tk_00000002', 'unassigned', 'next')).taken, false, 'no unassign');
  await drag('tk_00000004', 'unassigned', 'next');
  assert.deepEqual(api.calls, [['stage', 'tk_00000004', 'next']], 'a column move in a lane still works');
  assert.ok(cell('unassigned', 'next').querySelector('.tb-cell-add'), 'Unassigned keeps its +');
});
