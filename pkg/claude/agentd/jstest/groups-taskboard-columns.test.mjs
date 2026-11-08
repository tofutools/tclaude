import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A board's custom columns (tcl-plby5h): In review, In CI, … between In
// progress and Blocked, in the lanes, the List and the counts.

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });
const builder = { agent_id: 'agt_builder', name: 'builder' };
const COLUMNS = [{ slug: 'in-review', label: 'In review', count: 1 }, { slug: 'in-ci', label: 'In CI', count: 0 }];

function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 3, needs_you: 0, next: 1, in_progress: 1, in_columns: 1 },
    columns: COLUMNS,
    tasks: [
      row('tk_00000001', 'queued one'),
      row('tk_00000002', 'building', { assignee: builder, assigned_at: ago(7) }),
      row('tk_00000003', 'my PR', { assignee: builder, assigned_at: ago(30), column: 'in-review' }),
    ],
    closed: [],
  };
}

function fakeApi() {
  const calls = [];
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return {}; };
  return { calls, capture: record('capture'), edit: record('edit'), move: record('move'), stage: record('stage'), status: record('status'),
    assign: record('assign'), unassign: record('unassign'), start: record('start'), hold: record('hold'), archive: record('archive'),
    addColumn: record('addColumn'), changeColumn: record('changeColumn'), removeColumn: record('removeColumn') };
}

const members = () => [
  { agent_id: 'agt_builder', title: 'builder', role: 'dev', online: true, state: { status: 'working' } },
  { agent_id: 'agt_tester', title: 'tester', role: 'dev', online: true },
];

async function mount(t, name, { view = 'lanes', api = fakeApi() } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, view);
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  const source = {
    context: harness.signals.signal({ groups: [board(name)] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {},
  };
  const group = { name, members: members() };
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

test('boardColumns puts the custom columns after In progress; laneColumn and laneDrop map them to move --stage', async (t) => {
  const harness = await createPreactHarness(t);
  const { boardColumns, laneColumn, laneDrop, LANE_COLUMNS } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  assert.equal(boardColumns({}), LANE_COLUMNS, 'no custom columns: the five');
  assert.deepEqual(boardColumns({ columns: COLUMNS }).map((c) => c.key), ['ideas', 'next', 'progress', 'col:in-review', 'col:in-ci', 'blocked', 'done']);

  const [queued, building, pr] = board('x').tasks;
  assert.equal(laneColumn(pr), 'col:in-review');
  assert.equal(laneColumn({ ...pr, status: 'blocked' }), 'blocked', 'a blocked task stands in Blocked');
  assert.equal(laneColumn(row('tk_u', 'unassigned PR', { column: 'in-ci' })), 'col:in-ci', 'an unassigned one too');

  const at = (lane, column) => ({ lane, column, name: lane, columns: COLUMNS });
  const drop = (task, from, to, opts) => laneDrop(task, from, to, opts);
  assert.equal(drop(building, at('agt_builder', 'progress'), at('agt_builder', 'col:in-review')).kind, 'column');
  assert.equal(drop(building, at('agt_builder', 'progress'), at('agt_builder', 'col:in-review')).label, 'to In review');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), at('agt_builder', 'col:in-ci')).kind, 'column', 'from one column to another');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), at('agt_builder', 'progress')).kind, 'uncolumn');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), at('agt_builder', 'progress')).label, 'out of In review');
  assert.equal(drop({ ...pr, held: true }, at('agt_builder', 'col:in-review'), at('agt_builder', 'progress')).kind, 'uncolumn+start');
  assert.equal(drop({ ...pr, status: 'blocked' }, at('agt_builder', 'blocked'), at('agt_builder', 'col:in-review')).kind, 'unblock+column');
  assert.equal(drop(queued, at('', 'next'), at('agt_builder', 'col:in-review')).kind, 'assign+column');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), at('', 'col:in-review')).kind, 'unassign+column');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), at('agt_builder', 'next')).kind, 'uncolumn+hold', 'onto Next a started one is held back');
  // Onto another lane's Next or In progress, or after an unblock, it leaves the column too.
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), { ...at('agt_tester', 'next'), member: true }).kind, 'assign-hold+uncolumn');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), { ...at('agt_tester', 'progress'), member: true }).kind, 'assign+uncolumn');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), at('', 'next')).kind, 'unassign+uncolumn');
  assert.equal(drop(pr, at('agt_builder', 'col:in-review'), at('', 'next')).label, 'unassign · out of In review');
  assert.equal(drop({ ...pr, status: 'blocked' }, at('agt_builder', 'blocked'), at('agt_builder', 'progress')).kind, 'unblock+uncolumn');
  assert.equal(drop(queued, at('', 'next'), at('agt_builder', 'col:in-review'), { assign: false }), null, 'another lane needs assign');
  assert.equal(drop({ ...pr, status: 'done' }, at('agt_builder', 'done'), at('agt_builder', 'col:in-review')), null, 'a done card is reopened first');

  // Each drop is the CLI's route: stage with the slug, next to come out.
  const calls = [];
  const api = { stage: async (...a) => calls.push(['stage', ...a]), assign: async (...a) => calls.push(['assign', ...a]), start: async (...a) => calls.push(['start', ...a]),
    hold: async (...a) => calls.push(['hold', ...a]), unassign: async (...a) => calls.push(['unassign', ...a]) };
  await drop(queued, at('', 'next'), at('agt_builder', 'col:in-ci')).run(api);
  await drop(pr, at('agt_builder', 'col:in-review'), at('agt_builder', 'next')).run(api);
  await drop(pr, at('agt_builder', 'col:in-review'), at('', 'next')).run(api);
  assert.deepEqual(calls, [['assign', 'tk_00000001', 'agt_builder'], ['stage', 'tk_00000001', 'in-ci'],
    ['stage', 'tk_00000003', 'next'], ['hold', 'tk_00000003', 'next'],
    ['unassign', 'tk_00000003'], ['stage', 'tk_00000003', 'next']]);
});

test('the lanes draw the custom columns with their counts; a drag in calls stage with the slug', async (t) => {
  const { c, cell, ids, drag, api } = await mount(t, 'tb-cols');
  const heads = [...c.querySelectorAll('.tb-lane-columns > [data-col]')].map((h) => h.dataset.col);
  assert.deepEqual(heads, ['ideas', 'next', 'progress', 'col:in-review', 'col:in-ci', 'blocked', 'done']);
  const review = c.querySelector('.tb-lane-columns > [data-col="col:in-review"]');
  assert.match(review.textContent, /In review/);
  assert.equal(review.querySelector('.tb-col-count').textContent, '1');
  assert.ok(review.classList.contains('tb-col-custom'));
  assert.deepEqual(ids('agt_builder', 'col:in-review'), ['tk_00000003']);
  assert.ok(cell('agt_builder', 'col:in-review').classList.contains('tb-cell-custom'));
  assert.match(cell('agt_builder', 'col:in-review').textContent, /no idle alarm/, 'its card says the idle check is paused');
  assert.match(c.querySelector('.tb-lanes').getAttribute('style'), /repeat\(7, /, 'the grid has a column per stage');

  const { taken, label } = await drag('tk_00000002', 'agt_builder', 'col:in-ci');
  assert.equal(taken, true);
  assert.equal(label, 'to In CI');
  assert.deepEqual(api.calls.at(-1), ['stage', 'tk_00000002', 'in-ci']);
  await drag('tk_00000003', 'agt_builder', 'progress');
  assert.deepEqual(api.calls.at(-1), ['stage', 'tk_00000003', 'next']);
});

test('a custom column head has ⋯ rename, move and remove; ＋ column adds one', async (t) => {
  const { harness, c, api } = await mount(t, 'tb-colmenu');
  const review = () => c.querySelector('.tb-lane-columns > [data-col="col:in-review"]');
  const open = async () => harness.act(() => harness.fireEvent(review().querySelector('.tb-col-more'), 'click'));
  const item = (key) => review().querySelector(`[data-column-menu="${key}"]`);
  await open();
  assert.ok(item('left').disabled, 'the first custom column does not move left');
  await harness.act(async () => { harness.fireEvent(item('right'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls.at(-1), ['changeColumn', 'tb-colmenu', 'in-review', { toEnd: true }]);
  await open();
  await harness.act(async () => { harness.fireEvent(item('remove'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls.at(-1), ['removeColumn', 'tb-colmenu', 'in-review']);

  await open();
  await harness.act(() => harness.fireEvent(item('rename'), 'click'));
  const input = () => harness.document.querySelector('#taskboard-column-input');
  assert.equal(input().value, 'In review');
  input().value = 'Awaiting review';
  await harness.act(() => harness.fireEvent(input(), 'input'));
  await harness.act(async () => { harness.fireEvent(harness.document.querySelector('#taskboard-column-submit'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls.at(-1), ['changeColumn', 'tb-colmenu', 'in-review', { label: 'Awaiting review' }]);

  const add = c.querySelector('.tb-lane-columns > [data-col="done"] .tb-col-add');
  assert.ok(add, 'the last head offers ＋ column');
  await harness.act(() => harness.fireEvent(add, 'click'));
  input().value = 'QA';
  await harness.act(() => harness.fireEvent(input(), 'input'));
  await harness.act(async () => { harness.fireEvent(harness.document.querySelector('#taskboard-column-submit'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls.at(-1), ['addColumn', 'tb-colmenu', 'QA']);
});

test('the List says the column, filters by it, and moves a task in or out from ⋯', async (t) => {
  const { harness, c, api } = await mount(t, 'tb-collist', { view: 'list' });
  const rowOf = (id) => c.querySelector(`tr.tb-row[data-task-id="${id}"]`);
  const pill = rowOf('tk_00000003').querySelector('.tb-state');
  assert.equal(pill.textContent, 'In review');
  assert.ok(pill.classList.contains('tb-state-column'));
  assert.match(rowOf('tk_00000003').querySelector('.tb-state-wrap').getAttribute('title'), /no idle alarm/);

  // The Filter lists the board's columns after in progress, and hides one.
  await harness.act(() => harness.fireEvent(c.querySelector('.tb-list-filter'), 'click'));
  const box = c.querySelector('#tb-filter-tb-collist-col-in-review');
  assert.ok(box, 'a Filter entry for In review');
  box.checked = false;
  await harness.act(() => harness.fireEvent(box, 'change'));
  assert.equal(rowOf('tk_00000003'), null, 'hidden by the filter');
  assert.ok(rowOf('tk_00000002'));
  box.checked = true;
  await harness.act(() => harness.fireEvent(box, 'change'));

  // ⋯ Move to column › In CI, and Back to In progress.
  const menu = async (id) => harness.act(() => harness.fireEvent(rowOf(id).querySelector('.tb-act-more'), 'click'));
  const pick = async (id, key) => harness.act(async () => { harness.fireEvent(rowOf(id).querySelector(`[data-task-menu="${key}"]`), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  await menu('tk_00000003');
  await pick('tk_00000003', 'column');
  const choices = [...rowOf('tk_00000003').querySelectorAll('[data-task-menu="to-column"], [data-task-menu="out-of-column"]')].map((n) => n.textContent);
  assert.deepEqual(choices, ['In review ✓', 'In CI', 'Back to In progress']);
  const ci = [...rowOf('tk_00000003').querySelectorAll('[data-task-menu="to-column"]')].find((n) => n.textContent === 'In CI');
  await harness.act(async () => { harness.fireEvent(ci, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls.at(-1), ['stage', 'tk_00000003', 'in-ci']);
});

test('the counts name each custom column; the layout keeps widths per number of columns', async (t) => {
  const harness = await createPreactHarness(t);
  const { taskboardCountsText, taskRowState } = await harness.importDashboardModule('js/taskboard-context.js');
  assert.equal(taskboardCountsText(board('x').counts, 'you', COLUMNS), '3 · 1 in progress · 1 In review · 1 next');
  assert.equal(taskRowState(board('x').tasks[2]), 'col:in-review');
  assert.equal(taskRowState({ ...board('x').tasks[2], status: 'blocked' }), 'blocked');

  const { laneWidths, setLaneWidths, resizeColumns, gridColumns, gridMinWidth } = await harness.importDashboardModule('js/groups-taskboard-lanes-layout.js');
  setLaneWidths('g', { lane: 200, stages: [1, 1, 1, 1, 1] });
  assert.ok(laneWidths('g'));
  assert.equal(laneWidths('g', 7), null, 'widths stored for five stages read as the default once there are seven');
  assert.equal(gridColumns(null, 7), '170px repeat(7, minmax(90px, 1fr))');
  assert.equal(gridColumns(null), '', 'five stages keep the stylesheet default');
  assert.equal(gridMinWidth(null, 7), 170 + 7 * 90);
  const out = resizeColumns([170, 100, 100, 100, 100, 100, 100, 100], 6, 20); // the right one stops at its 90px minimum
  assert.deepEqual(out.stages, [100, 100, 100, 100, 100, 110, 90], 'the sixth stage edge trades with the seventh');
});
