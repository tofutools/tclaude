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
    stage: record('stage'), status: record('status'), assign: record('assign'), unassign: record('unassign'), archive: record('archive'),
    start: record('start'), hold: record('hold'),
    create: async (...args) => { calls.push(['create', ...args]); return { id: 'tk_00000099', title: args[1].title }; } };
}

const members = () => [
  { agent_id: 'agt_lead', title: 'lead', role: 'lead', online: true, owner: true, state: { status: 'idle' } },
  { agent_id: 'agt_builder', title: 'builder', role: 'dev', online: true, state: { status: 'working' } },
  { agent_id: 'agt_tester', title: 'tester', role: 'dev', online: true },
  { agent_id: 'agt_old', title: 'old', role: 'researcher', online: false },
  { conv_id: 'conv-no-agent', title: 'plain' },
];

async function mountLanes(t, name, { api = fakeApi(), by = null, groupExtra = {}, extraTasks = [], prefs = {} } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, 'lanes');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  if (by) dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, by);
  for (const [key, value] of Object.entries(prefs)) dashPrefs.setItem(key, value);
  const source = {
    context: harness.signals.signal({ groups: [{ ...board(name), tasks: [...board(name).tasks, ...extraTasks] }] }), status: harness.signals.signal('ready'),
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
  return { harness, c, api, cell, ids, card, drag, dashPrefs, source };
}

test('laneColumn derives the five columns as the design says', async (t) => {
  const harness = await createPreactHarness(t);
  const { laneColumn, laneDrop, striped } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  const b = board('x');
  const all = [...b.tasks, ...b.closed];
  assert.deepEqual(all.map((task) => laneColumn(task)), ['next', 'progress', 'blocked', 'ideas', 'blocked', 'done', null, null]);
  assert.equal(striped('', 'progress'), true);
  assert.equal(striped('agt_builder', 'ideas'), false, 'an agent holds ideas too');
  assert.equal(striped('agt_builder', 'progress'), false);
  // A held task (assigned, not started) sits in its stage; one from a node
  // that predates holding (no held) is in progress.
  assert.equal(laneColumn(row('tk_h1', 'held idea', { stage: 'idea', assignee: builder, held: true })), 'ideas');
  assert.equal(laneColumn(row('tk_h2', 'held next', { assignee: builder, held: true })), 'next');
  assert.equal(laneColumn(row('tk_h3', 'old node', { stage: 'idea', assignee: builder })), 'progress');

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
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'ideas')), 'assign-hold', 'given as an idea, not started');
  assert.equal(kind(queued, at('', 'next'), at('agt_builder', 'next')), 'assign-hold', 'queued for them, not started');
  assert.equal(kind(queued, at('', 'next'), at('', 'progress')), null, 'striped');
  // In the holder's own lane: start a held task, hold a started one.
  const heldIdea = row('tk_h1', 'held idea', { stage: 'idea', assignee: builder, held: true });
  assert.equal(kind(heldIdea, at('agt_builder', 'ideas'), at('agt_builder', 'progress')), 'start');
  assert.equal(kind(heldIdea, at('agt_builder', 'ideas'), at('agt_builder', 'next')), 'hold');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('agt_builder', 'ideas')), 'hold', 'back to ideas, not started');
  assert.equal(kind(building, at('agt_builder', 'progress'), at('agt_builder', 'next'), { assign: false }), null, 'holding needs assign');
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
  // A stalled task (open, shown in its agent's Blocked) onto the Unassigned
  // lane's Blocked is blocked too; a blocked one is only unassigned.
  const stalled = b.tasks.find((task) => task.stalled);
  assert.equal(kind(stalled, at('agt_lead', 'blocked'), at('', 'blocked')), 'unassign+block');
  assert.equal(kind({ ...held, assignee: builder }, at('agt_builder', 'blocked'), at('', 'blocked')), 'unassign');

  // A retry after the second write was refused runs only that write.
  const calls = [];
  let refuse = true;
  const api = {
    unassign: async (id) => { calls.push(['unassign', id]); },
    status: async (id, status) => { calls.push(['status', id, status]); if (refuse) throw new Error('refused'); },
  };
  const drop = laneDrop(stalled, at('agt_lead', 'blocked'), at('', 'blocked'));
  await assert.rejects(drop.run(api, 'why'));
  refuse = false;
  await drop.run(api, 'why');
  assert.deepEqual(calls.map(([verb]) => verb), ['unassign', 'status', 'status'], 'the unassign that landed is not repeated');
});

test('the lanes draw the board by role, with the Unassigned lane first and offline lane groups folded', async (t) => {
  const { c, ids, cell, harness } = await mountLanes(t, 'tbl-role', { by: 'role' });
  assert.ok(c.querySelector('[data-taskboard-lanes="tbl-role"][data-lanes-by="role"]'));
  assert.equal(c.querySelector('table.tb-table'), null, 'no list in the lanes view');
  const lanes = [...c.querySelectorAll('[data-lane-row]')].map((el) => el.dataset.laneRow);
  assert.deepEqual(lanes, ['operator', 'unassigned', 'agt_lead', 'agt_builder', 'agt_tester'], 'the offline researcher folds');
  assert.deepEqual([...c.querySelectorAll('[data-lane-group]')].map((el) => [...el.querySelectorAll('.ui-eyebrow, .tb-lane-sub, .tb-lane-fold')].map((part) => part.textContent.trim())),
    [['lead', '1 agent'], ['dev', '2 agents'], ['researcher', '1 agent · offline', '▸ show offline lanes']]);
  assert.deepEqual(ids('unassigned', 'ideas'), ['tk_00000004']);
  assert.deepEqual(ids('unassigned', 'next'), ['tk_00000001']);
  assert.deepEqual(ids('unassigned', 'blocked'), ['tk_00000003']);
  assert.deepEqual(ids('agt_builder', 'progress'), ['tk_00000002']);
  assert.deepEqual(ids('agt_lead', 'blocked'), ['tk_00000006'], 'stalled needs you');
  assert.deepEqual(ids('agt_builder', 'done'), ['tk_00000005'], 'done this week only; dropped never');
  assert.ok(cell('unassigned', 'progress').classList.contains('tb-cell-striped'));
  assert.ok(!cell('agt_builder', 'ideas').classList.contains('tb-cell-striped'), 'an agent lane holds ideas');
  assert.match(c.querySelector('[data-lane-agent="agt_lead"]').textContent, /idle · owner/);
  assert.match(c.querySelector('[data-lane-agent="agt_lead"]').textContent, /holds 1/);
  assert.match(c.querySelector('.tb-card[data-task-id="tk_00000006"]').textContent, /idle, no report/);

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
  out = await drag('tk_00000001', 'unassigned', 'progress');
  assert.equal(out.taken, false);
  assert.deepEqual(api.calls, []);

  // → Blocked asks the reason first.
  await drag('tk_00000001', 'unassigned', 'blocked');
  assert.deepEqual(api.calls, []);
  const form = harness.document.querySelector('#taskboard-reason-modal');
  assert.ok(form, 'the shared reason dialog asks, not an inline form');
  assert.equal(form.querySelector('#taskboard-reason-submit').disabled, true, 'the reason is required');
  await harness.input(form.querySelector('textarea'), 'needs a design call');
  await harness.act(async () => {
    harness.fireEvent(form.querySelector('#taskboard-reason-submit'), 'click');
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.deepEqual(api.calls, [['status', 'tk_00000001', 'blocked', 'needs a design call']]);
  assert.equal(harness.document.querySelector('#taskboard-reason-modal'), null);
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
  assert.equal(ro.c.querySelector('.tb-card-more'), null, 'no ⋯ on a card');
});

test('a diagonal drag runs both writes through the real handlers, the lane first', async (t) => {
  const { api, drag, c, harness } = await mountLanes(t, 'tbl-diagonal');
  // Unassigned · Next → builder · Blocked: assign, then block with a reason.
  let out = await drag('tk_00000001', 'agt_builder', 'blocked');
  assert.equal(out.taken, true);
  assert.equal(out.label, 'assign to builder · block (asks why)');
  assert.deepEqual(api.calls, [], 'nothing runs before the reason');
  const form = harness.document.querySelector('#taskboard-reason-modal');
  assert.match(form.textContent, /assign to builder · block “queued one”/);
  await harness.input(form.querySelector('textarea'), 'waits on the API');
  await harness.act(async () => {
    harness.fireEvent(form.querySelector('textarea'), 'keydown', { key: 'Enter', ctrlKey: true });
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

  // lead · Blocked (stalled) → builder · Next: reassigned, held queued.
  await drag('tk_00000006', 'agt_builder', 'next');
  assert.deepEqual(api.calls.splice(0), [['assign', 'tk_00000006', 'agt_builder', { hold: 'next' }]]);

  // A move the board cannot make in one go is not taken.
  out = await drag('tk_00000002', 'unassigned', 'done');
  assert.equal(out.taken, false);
  assert.deepEqual(api.calls, []);
});

test('a cell + opens the add menu; its choice starts from the cell: the column, and the lane\'s agent as the assignee', async (t) => {
  const { api, c, cell, harness } = await mountLanes(t, 'tbl-add');
  const plus = (lane, column) => cell(lane, column).querySelector('.tb-cell-add');
  for (const column of ['ideas', 'next', 'blocked', 'done']) assert.ok(plus('unassigned', column), `unassigned · ${column}`);
  for (const column of ['ideas', 'next', 'progress', 'blocked', 'done']) assert.ok(plus('agt_builder', column), `builder · ${column}`);
  assert.equal(plus('unassigned', 'progress'), null, 'striped');
  assert.match(plus('agt_builder', 'progress').getAttribute('aria-label'), /builder · In progress/);
  assert.equal(plus('agt_builder', 'progress').getAttribute('aria-haspopup'), 'menu');
  assert.ok(cell('agt_builder', 'progress').classList.contains('tb-cell-addable'), 'the cell keeps room for its +');

  // The + opens the same menu as the section's ＋▾, anchored at it.
  await harness.act(async () => { harness.fireEvent(plus('agt_builder', 'blocked'), 'click'); });
  const menu = cell('agt_builder', 'blocked').querySelector('.tb-cell-menu');
  assert.ok(menu, 'the add menu opens at the +');
  assert.ok(menu.querySelector('[data-taskboard-new-form]') && menu.querySelector('[data-taskboard-new-ask]') && menu.querySelector('[data-taskboard-new-import]'));
  assert.equal(c.querySelector('#taskboard-new-modal'), null, 'not the form directly');

  const write = async (lane, column, title, extra = async () => {}) => {
    await harness.act(async () => { harness.fireEvent(plus(lane, column), 'click'); });
    await harness.act(async () => { harness.fireEvent(cell(lane, column).querySelector('[data-taskboard-new-form]'), 'click'); });
    const modal = c.querySelector('#taskboard-new-modal');
    assert.ok(modal, `the form opens from ${lane} · ${column}`);
    assert.equal(modal.querySelector('input[name="tb-new-stage"]:checked')?.getAttribute('value'),
      { ideas: 'idea', next: 'next', progress: 'progress', blocked: 'blocked', done: 'done' }[column], 'the cell\'s column is picked');
    await harness.input(modal.querySelector('.tb-new-title'), title);
    await extra(modal);
    await harness.act(async () => { harness.fireEvent(modal.querySelector('#taskboard-new-submit'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
    assert.equal(c.querySelector('#taskboard-new-modal'), null, 'the form closes after it adds');
  };
  api.calls.length = 0;
  await write('unassigned', 'ideas', 'an idea from the cell');
  await write('unassigned', 'next', 'queued from the cell');
  await write('agt_builder', 'progress', 'for builder');
  await write('agt_builder', 'ideas', 'an idea for builder');
  await write('agt_builder', 'next', 'queued for builder');
  await write('agt_builder', 'blocked', 'stuck for builder', (modal) => harness.input(modal.querySelector('.tb-new-reason'), 'waiting on CI'));
  await write('unassigned', 'done', 'logged');
  const base = { body: '', ref: '', after: [], resolveRef: false };
  assert.deepEqual(api.calls, [
    ['create', 'tbl-add', { title: 'an idea from the cell', ...base, stage: 'idea', status: '', to: '' }],
    ['create', 'tbl-add', { title: 'queued from the cell', ...base, stage: 'next', status: '', to: '' }],
    ['create', 'tbl-add', { title: 'for builder', ...base, stage: 'next', status: '', to: 'agt_builder' }],
    ['create', 'tbl-add', { title: 'an idea for builder', ...base, stage: 'idea', status: '', to: 'agt_builder', hold: true }],
    ['create', 'tbl-add', { title: 'queued for builder', ...base, stage: 'next', status: '', to: 'agt_builder', hold: true }],
    ['create', 'tbl-add', { title: 'stuck for builder', ...base, stage: 'next', status: 'blocked', blockReason: 'waiting on CI', to: 'agt_builder' }],
    ['create', 'tbl-add', { title: 'logged', ...base, stage: 'idea', status: 'done', to: '' }],
  ], 'each form carries its cell\'s start state and assignee, in one create');
});

test('a cell\'s menu opens above a + near the viewport\'s bottom, and a scroll closes it', async (t) => {
  const { cell, harness } = await mountLanes(t, 'tbl-add-flip');
  const plus = cell('agt_builder', 'done').querySelector('.tb-cell-add');
  const saved = globalThis.innerHeight;
  globalThis.innerHeight = 600;
  t.after(() => { globalThis.innerHeight = saved; });
  plus.getBoundingClientRect = () => ({ top: 560, bottom: 578, left: 900, right: 918, width: 18, height: 18 });
  await harness.act(async () => { harness.fireEvent(plus, 'click'); });
  const menu = cell('agt_builder', 'done').querySelector('.tb-cell-menu');
  assert.match(menu.getAttribute('style') || '', /bottom: 42px/, 'no room below: it opens above the +');
  await harness.act(async () => { harness.fireEvent(document, 'scroll'); });
  assert.equal(cell('agt_builder', 'done').querySelector('.tb-cell-menu'), null, 'a scroll closes it');
});

test('a cell + carries its start state into Ask an agent and the import', async (t) => {
  const { c, cell, harness } = await mountLanes(t, 'tbl-add-ask');
  const plus = (lane, column) => cell(lane, column).querySelector('.tb-cell-add');
  await harness.act(async () => { harness.fireEvent(plus('agt_builder', 'next'), 'click'); });
  await harness.act(async () => { harness.fireEvent(cell('agt_builder', 'next').querySelector('[data-taskboard-new-ask]'), 'click'); });
  const ask = c.querySelector('#taskboard-ask-modal');
  assert.ok(ask, 'Ask an agent opens');
  assert.match(ask.querySelector('.tb-ask-prompt').value, /Land the tasks in Next for builder/);
  assert.equal(ask.querySelector('#taskboard-ask-submit').disabled, true, 'the opening line alone is not a request');
  await harness.act(async () => { harness.fireEvent(ask.querySelector('.ui-dialog-close, [data-dialog-cancel], button.ui-btn-ghost') || ask, 'keydown', { key: 'Escape' }); });

  await harness.act(async () => { harness.fireEvent(plus('unassigned', 'next'), 'click'); });
  await harness.act(async () => { harness.fireEvent(cell('unassigned', 'next').querySelector('[data-taskboard-new-import]'), 'click'); });
  const panel = c.querySelector('[data-taskboard-import]');
  assert.ok(panel, 'the import opens');
  assert.ok(panel.querySelector('input[type="checkbox"]').checked || panel.querySelector('input[type="checkbox"]').hasAttribute('checked'), 'a Next cell imports into the queue');
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

test('a cell holding many tasks draws the first three and "+ n more", which opens and closes it in place', async (t) => {
  const ideas = ['tk_000000a1', 'tk_000000a2', 'tk_000000a3', 'tk_000000a4', 'tk_000000a5'].map((id) => row(id, `idea ${id}`, { stage: 'idea' }));
  const { c, cell, ids, harness } = await mountLanes(t, 'tbl-cap', { extraTasks: ideas });
  const more = () => cell('unassigned', 'ideas').querySelector('.tb-cell-more');
  assert.equal(ids('unassigned', 'ideas').length, 3, 'six ideas, three drawn');
  assert.equal(more().textContent, '+ 3 more');
  assert.equal(more().getAttribute('aria-expanded'), 'false');
  assert.match(c.querySelector('.tb-col-ideas').textContent, /6$/, 'the column head still counts every idea');
  assert.equal(cell('unassigned', 'next').querySelector('.tb-cell-more'), null, 'a cell within the cap has no toggle');
  await harness.act(async () => { harness.fireEvent(more(), 'click'); });
  assert.equal(ids('unassigned', 'ideas').length, 6, 'opened, every card is drawn');
  const cards = cell('unassigned', 'ideas').querySelector('.tb-cell-cards');
  assert.ok(cards.classList.contains('tb-cell-cards-open'), 'an opened cell scrolls inside past its height cap');
  assert.ok(more().classList.contains('tb-cell-less'), 'and offers Show less');
  assert.equal(more().textContent.trim(), '▴ Show less');
  assert.equal(more().getAttribute('aria-expanded'), 'true');
  assert.equal(cards.contains(more()), false, 'Show less sits outside the scroll area, always in reach');
  cards.scrollTop = 120;
  await harness.act(async () => { harness.fireEvent(more(), 'click'); });
  assert.equal(ids('unassigned', 'ideas').length, 3, 'and closes again');
  assert.equal(cards.scrollTop, 0, 'closing scrolls the cards back to the top');
  assert.equal(cards.classList.contains('tb-cell-cards-open'), false);
  const css = (await import('node:fs')).readFileSync(new URL('../dashboard/dashboard.css', import.meta.url), 'utf8');
  assert.match(css, /\.tb-cell-cards\.tb-cell-cards-open \{[^}]*max-height:[^}]*overflow-y: auto/, 'the open cell\'s height cap scrolls');
  const grid = c.querySelector('.tb-lanes');
  assert.match(grid.getAttribute('style'), /--tb-lane-min: 620px/, 'rows are at least the grid\'s minimum width, so the sideways scroll keeps the lane column pinned');
});

test('a task dropped into a full cell opens it, so the card does not land behind "+ n more"', async (t) => {
  const queued = ['tk_000000b1', 'tk_000000b2', 'tk_000000b3'].map((id) => row(id, `queued ${id}`));
  const { cell, drag, api } = await mountLanes(t, 'tbl-land', { extraTasks: queued });
  const more = () => cell('unassigned', 'next').querySelector('.tb-cell-more');
  assert.equal(more().textContent, '+ 1 more', 'four queued, three drawn');
  const out = await drag('tk_00000004', 'unassigned', 'next');
  assert.equal(out.taken, true);
  assert.ok(api.calls.length > 0, 'the drop ran');
  assert.equal(more().getAttribute('aria-expanded'), 'true', 'the full cell opened for the card dropped into it');
});

test('a card dropped into a long open cell is scrolled into view once it lands', async (t) => {
  const queued = Array.from({ length: 14 }, (_, i) => row(`tk_000000c${i.toString(16)}`, `queued ${i}`));
  const { cell, drag, card, harness, source } = await mountLanes(t, 'tbl-reveal', { extraTasks: queued });
  const seen = [];
  const proto = Object.getPrototypeOf(card('tk_00000004'));
  const had = Object.prototype.hasOwnProperty.call(proto, 'scrollIntoView');
  const before = proto.scrollIntoView;
  proto.scrollIntoView = function scrollIntoView() { seen.push(this.dataset?.taskId || this.className); };
  t.after(() => { if (had) proto.scrollIntoView = before; else delete proto.scrollIntoView; });
  await drag('tk_00000004', 'unassigned', 'next');
  assert.equal(cell('unassigned', 'next').querySelector('.tb-cell-more').getAttribute('aria-expanded'), 'true');
  assert.deepEqual(seen, [], 'nothing to reveal until the card lands');
  // The board refreshes with the idea promoted, at the end of the queue.
  const b = source.context.value.groups[0];
  const tasks = b.tasks.filter((task) => task.id !== 'tk_00000004').concat({ ...b.tasks.find((task) => task.id === 'tk_00000004'), stage: 'next' });
  await harness.act(async () => { source.context.value = { groups: [{ ...b, tasks }] }; });
  assert.deepEqual(seen, ['tk_00000004'], 'the landed card is scrolled into view, once');
  await harness.act(async () => { source.context.value = { groups: [{ ...b, tasks: [...tasks] }] }; });
  assert.deepEqual(seen, ['tk_00000004'], 'a later refresh does not scroll to it again');
});

test('a stored lane width sets the grid\'s minimum width with its columns', async (t) => {
  const { c } = await mountLanes(t, 'tbl-minw', { prefs: { 'tclaude.dash.groups.taskboard.lanes-widths.tbl-minw': JSON.stringify({ lane: 240, stages: [1, 1, 1, 1, 1] }) } });
  const style = c.querySelector('.tb-lanes').getAttribute('style');
  assert.match(style, /--tb-lane-cols: 240px /);
  assert.match(style, /--tb-lane-min: 690px/, 'the stored lane column plus five stage minimums');
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

// Fleet- and federation-wide: in the fused view a linked node's group is
// name@node; its lanes write to that node under the group's own name.
test('on a linked node\'s group in the fused view, a cell + and lane drags write to that node under its own name', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ TaskboardSection }, { dashPrefs }, { setNodeSnapshotSource }] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  setNodeSnapshotSource((node) => (node === 'nd_forge' ? { peer_view: { level: 'unrestricted' } } : null));
  dashPrefs.setItem('tclaude.dash.groups.taskboard.view.tbl-fused@forge', 'lanes');
  dashPrefs.setItem('tclaude.dash.groups.fused.taskboard.tbl-fused@forge', '1');
  const api = fakeApi();
  const source = { context: harness.signals.signal({ groups: [board('tbl-fused')] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name: 'tbl-fused@forge', members: members(), fleet_node: { id: 'nd_forge', name: 'forge', local: false } };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} boards=${() => ({ source, api })} />`);
  const c = mounted.container;
  const cell = (lane, column) => c.querySelector(`.tb-cell[data-lane="${lane}"][data-column="${column}"]`);
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((r) => setTimeout(r, 0)); });

  await click(cell('agt_tester', 'next').querySelector('.tb-cell-add'));
  await click(cell('agt_tester', 'next').querySelector('[data-taskboard-new-form]'));
  const form = c.querySelector('#taskboard-new-modal');
  assert.ok(form, 'the cell + opens the menu, and its first choice the form');
  await harness.input(form.querySelector('.tb-new-title'), 'peer task');
  await click(form.querySelector('#taskboard-new-submit'));
  assert.deepEqual(api.calls.map(([verb, arg, payload]) => [verb, arg, payload?.to]), [['create', 'tbl-fused', 'agt_tester']],
    'one create on that node, with the lane\'s agent');

  api.calls.length = 0;
  const transfer = { setData() {}, effectAllowed: '', dropEffect: '' };
  const card = c.querySelector('.tb-card[data-task-id="tk_00000001"]');
  await harness.act(async () => { harness.fireEvent(card, 'dragstart', { dataTransfer: transfer }); });
  harness.fireEvent(cell('agt_tester', 'next'), 'dragover', { dataTransfer: transfer });
  await harness.act(async () => { harness.fireEvent(cell('agt_tester', 'next'), 'drop', { dataTransfer: transfer }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['assign', 'tk_00000001', 'agt_tester', { hold: 'next' }]], 'an assign drag goes to the group\'s node');

  // A drag onto Blocked asks its reason in the same dialog as a local board,
  // and the block goes to the group's node.
  api.calls.length = 0;
  const card2 = c.querySelector('.tb-card[data-task-id="tk_00000004"]');
  await harness.act(async () => { harness.fireEvent(card2, 'dragstart', { dataTransfer: transfer }); });
  harness.fireEvent(cell('unassigned', 'blocked'), 'dragover', { dataTransfer: transfer });
  await harness.act(async () => { harness.fireEvent(cell('unassigned', 'blocked'), 'drop', { dataTransfer: transfer }); await new Promise((r) => setTimeout(r, 0)); });
  const reason = harness.document.querySelector('#taskboard-reason-modal');
  assert.ok(reason, 'the reason dialog opens on a peer board too');
  await harness.input(reason.querySelector('textarea'), 'peer reason');
  await click(reason.querySelector('#taskboard-reason-submit'));
  assert.deepEqual(api.calls, [['status', 'tk_00000004', 'blocked', 'peer reason']]);
});

test('the operator lane: pinned above Unassigned, "You" with what waits on you, and a drop onto it assigns the operator', async (t) => {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  const name = 'tbl-operator';
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, 'lanes');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  const b = board(name);
  b.tasks.push(row('tk_000000a1', 'which colour?', { assignee: { agent_id: 'operator', name: 'operator', operator: true }, assigned_by: 'agent:agt_builder' }));
  b.counts = { ...b.counts, live: 7, needs_you: 3, waiting_on_you: 1 };
  const source = { context: harness.signals.signal({ groups: [b] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const api = fakeApi();
  const group = { name, members: members() };
  const { container: c } = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const lanes = [...c.querySelectorAll('[data-lane-row]')].map((el) => el.dataset.laneRow);
  assert.deepEqual(lanes.slice(0, 2), ['operator', 'unassigned']);
  const lane = c.querySelector('.tb-lane.tb-lane-operator');
  assert.match(lane.querySelector('.tb-lane-head').textContent, /You\s*1 waiting on you/);
  assert.deepEqual([...c.querySelectorAll('.tb-cell[data-lane="operator"][data-column="progress"] .tb-card')].map((el) => el.dataset.taskId), ['tk_000000a1']);
  assert.match(c.querySelector('.tb-waiting-tag').textContent, /1 waiting on you/);

  const transfer = { setData() {}, effectAllowed: '', dropEffect: '' };
  const target = c.querySelector('.tb-cell[data-lane="operator"][data-column="progress"]');
  await harness.act(async () => { harness.fireEvent(c.querySelector('.tb-card[data-task-id="tk_00000001"]'), 'dragstart', { dataTransfer: transfer }); });
  harness.fireEvent(target, 'dragover', { dataTransfer: transfer });
  await harness.act(async () => {});
  assert.equal(target.dataset.tbDrop, 'assign to you');
  await harness.act(async () => { harness.fireEvent(target, 'drop', { dataTransfer: transfer }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls.find(([verb]) => verb === 'assign'), ['assign', 'tk_00000001', 'operator']);
});

test('a done card dragged out of Done reopens there, in the column\'s stage and the lane\'s hands', async (t) => {
  const harness = await createPreactHarness(t);
  const { laneDrop } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  const done = row('tk_000000d1', 'finished', { status: 'done', closed_at: ago(30), assignee: builder });
  const loose = row('tk_000000d2', 'finished alone', { status: 'done', closed_at: ago(30), stage: 'idea' });
  const at = (lane, column) => ({ lane, column, name: lane });
  const calls = [];
  const api = { reopen: async (...args) => { calls.push(['reopen', ...args]); }, status: async (...args) => { calls.push(['status', ...args]); } };
  const run = async (drop, reason) => { calls.length = 0; await drop.run(api, reason); return calls.slice(); };

  assert.deepEqual(await run(laneDrop(done, at('agt_builder', 'done'), at('agt_builder', 'progress'))), [['reopen', 'tk_000000d1', {}]]);
  assert.deepEqual(await run(laneDrop(done, at('agt_builder', 'done'), at('agt_tester', 'next'))), [['reopen', 'tk_000000d1', { to: 'agt_tester', stage: 'next' }]]);
  assert.deepEqual(await run(laneDrop(done, at('agt_builder', 'done'), at('', 'ideas'))), [['reopen', 'tk_000000d1', { to: '', stage: 'idea' }]]);
  assert.deepEqual(await run(laneDrop(done, at('agt_builder', 'done'), at('operator', 'progress'))), [['reopen', 'tk_000000d1', { to: 'operator', stage: 'next' }]]);
  assert.deepEqual(await run(laneDrop(loose, at('', 'done'), at('', 'next'))), [['reopen', 'tk_000000d2', { stage: 'next' }]]);
  const blocked = laneDrop(done, at('agt_builder', 'done'), at('agt_builder', 'blocked'));
  assert.equal(blocked.kind, 'reopen+block');
  assert.deepEqual(await run(blocked, 'more to do'), [['reopen', 'tk_000000d1', {}], ['status', 'tk_000000d1', 'blocked', 'more to do']]);
  assert.equal(laneDrop(done, at('agt_builder', 'done'), at('agt_tester', 'next'), { assign: false }), null, 'another lane needs assign');
  assert.equal(laneDrop(done, at('agt_builder', 'done'), at('', 'done')), null);
});

test('a lanes card has a ⋯ with Archive, also on a right-click', async (t) => {
  const { api, c, card, harness } = await mountLanes(t, 'tbl-archive');
  const el = card('tk_00000001');
  assert.ok(el.querySelector('.tb-card-more'), 'the card has a ⋯');
  await harness.act(async () => { harness.fireEvent(el, 'contextmenu'); });
  const item = [...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].find((b) => b.textContent.trim() === 'Drop and archive');
  assert.ok(item, 'the right-click opens it');
  api.calls.length = 0;
  await harness.act(async () => { harness.fireEvent(item, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['archive', 'tk_00000001']]);
});
