// Dragging a task to another group's board (groups-taskboard-transfer.js,
// tcl-0r0obl): a drop on another group's Task board section or header row
// moves it there after the Groups view's confirmation; Ctrl/Cmd clones.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const row = (group, id, title, extra = {}) => ({ id, group, title, stage: 'next', status: 'open', depends_on: [], ...extra });

function board(group, ids) {
  return {
    group, group_id: ids[0],
    counts: { live: 2, next: 1, ideas: 1 },
    tasks: [
      row(group, `tk_0000000${ids[0]}`, `${group} first`),
      row(group, `tk_0000000${ids[1]}`, `${group} waits`, { stage: 'idea', depends_on: [`tk_0000000${ids[0]}`] }),
    ],
    closed: [],
  };
}

const members = () => [{ agent_id: 'agt_dev', title: 'dev', role: 'dev', online: true }];

async function mountTwo(t, { lanes = false, otherNode = '' } = {}) {
  const harness = await createPreactHarness(t);
  const [{ TaskboardSection }, { dashPrefs }, transfer] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/groups-taskboard-transfer.js'),
  ]);
  for (const name of ['tbt-a', 'tbt-b']) dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  if (lanes) dashPrefs.setItem('tclaude.dash.groups.taskboard.view.tbt-b', 'lanes');
  const confirms = [];
  const toasts = [];
  let answer = true;
  const saved = { ...transfer.transferDeps };
  transfer.transferDeps.confirm = async (options) => { confirms.push(options); return answer; };
  transfer.transferDeps.toast = (text) => toasts.push(text);
  t.after(() => Object.assign(transfer.transferDeps, saved));
  const calls = [];
  const remote = [];
  let awayFails = 0;
  transfer.transferDeps.nodeApi = async (node) => ({
    transferIn: async (...args) => { remote.push([node, 'transferIn', ...args]); return { id: 'tk_000000aa' }; },
  });
  const api = {
    moveToGroup: async (...args) => { calls.push(args); return { task: { id: args[0] }, group: args[1], dropped_dependents: ['tk_00000002'] }; },
    movedAway: async (...args) => {
      calls.push(['movedAway', ...args]);
      if (awayFails > 0) { awayFails--; throw new Error('node offline'); }
      return { id: args[0], status: 'dropped' };
    },
    move: async () => ({}), assign: async () => ({}), status: async () => ({}), stage: async () => ({}),
  };
  const source = { context: harness.signals.signal({ groups: [board('tbt-a', [1, 2]), board('tbt-b', [3, 4])] }),
    status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const card = (name, node = '') => {
    const group = { name, members: members() };
    return harness.html`<details open data-group-key=${name} data-dnd-target-group=${node ? undefined : name} data-fleet-node=${node || undefined} data-fleet-node-name=${node ? 'other' : undefined}>
      <summary>${name}</summary>
      <${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />
    </details>`;
  };
  const mounted = await harness.mount(harness.html`<div>${card('tbt-a')}${card('tbt-b', otherNode)}<details data-group-key="virtual:ungrouped"><summary>Ungrouped</summary></details></div>`);
  const c = mounted.container;
  const dt = () => ({ setData() {}, setDragImage() {}, effectAllowed: '', dropEffect: '' });
  const handle = (id) => c.querySelector(`tr[data-task-id="${id}"] .tb-handle`) || c.querySelector(`.tb-card[data-task-id="${id}"]`);
  const start = async (id, transferData) => { await harness.act(async () => { harness.fireEvent(handle(id), 'dragstart', { dataTransfer: transferData }); }); };
  const over = (el, transferData, init = {}) => harness.fireEvent(el, 'dragover', { dataTransfer: transferData, ...init });
  const drop = async (el, transferData, init = {}) => {
    await harness.act(async () => { harness.fireEvent(el, 'drop', { dataTransfer: transferData, ...init }); await new Promise((r) => setTimeout(r, 0)); });
    await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  };
  const section = (name) => c.querySelector(`[data-fused-taskboard="${name}"]`);
  const summary = (name) => c.querySelector(`details[data-group-key="${name}"] > summary`);
  return { harness, c, calls, remote, confirms, toasts, dt, start, over, drop, section, summary, setAnswer: (v) => { answer = v; },
    failAway: (n) => { awayFails = n; }, transfer };
}

test('a task dropped on another group\'s board moves there after the confirmation', async (t) => {
  const { calls, confirms, toasts, dt, start, over, drop, section } = await mountTwo(t);
  const data = dt();
  await start('tk_00000001', data);
  assert.equal(data.effectAllowed, 'copyMove', 'Ctrl/Cmd may turn it into a copy');
  const target = section('tbt-b').querySelector('.gf-section-head');
  assert.equal(over(target, data).defaultPrevented, true, 'another group\'s board takes the task');
  assert.equal(section('tbt-b').getAttribute('data-tb-transfer'), 'move to tbt-b · Ideas');
  assert.ok(section('tbt-b').classList.contains('tb-transfer-over'));
  await drop(target, data);
  assert.equal(confirms.length, 1);
  assert.equal(confirms[0].title, 'Move task to another group?');
  assert.equal(confirms[0].okLabel, 'Move');
  assert.match(confirms[0].body, /Move "tbt-a first" from group "tbt-a" to group "tbt-b"/);
  assert.match(confirms[0].body, /"tbt-a waits" no longer waits for it/, 'the dialog names the dependency it drops');
  assert.deepEqual(calls, [['tk_00000001', 'tbt-b', { clone: false, stage: 'idea' }]]);
  assert.match(toasts[0], /Moved "tbt-a first" to tbt-b \(1 dependency dropped\)/);
  assert.equal(section('tbt-b').classList.contains('tb-transfer-over'), false, 'the marker goes with the drag');
});

test('Ctrl/Cmd on another group\'s header clones; a cancelled confirmation changes nothing', async (t) => {
  const { calls, confirms, dt, start, over, drop, summary, setAnswer } = await mountTwo(t);
  let data = dt();
  await start('tk_00000001', data);
  assert.equal(over(summary('tbt-b'), data, { ctrlKey: true }).defaultPrevented, true, 'the header row takes it too');
  assert.equal(data.dropEffect, 'copy');
  assert.equal(summary('tbt-b').getAttribute('data-tb-transfer'), 'clone to tbt-b · Ideas');
  await drop(summary('tbt-b'), data, { ctrlKey: true });
  assert.equal(confirms[0].title, 'Clone task to another group?');
  assert.equal(confirms[0].okLabel, 'Clone');
  assert.deepEqual(calls, [['tk_00000001', 'tbt-b', { clone: true, stage: 'idea' }]]);

  setAnswer(false);
  data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data);
  assert.equal(confirms.length, 2);
  assert.equal(calls.length, 1, 'cancelled: nothing runs');
});

test('a drop on another group\'s Next cell lands it in next', async (t) => {
  const { calls, dt, start, over, drop, section } = await mountTwo(t, { lanes: true });
  const data = dt();
  await start('tk_00000001', data);
  const cell = section('tbt-b').querySelector('.tb-cell[data-lane="unassigned"][data-column="next"]');
  assert.ok(cell);
  assert.equal(over(cell, data).defaultPrevented, true);
  assert.equal(section('tbt-b').getAttribute('data-tb-transfer'), 'move to tbt-b · Next');
  await drop(cell, data);
  assert.deepEqual(calls, [['tk_00000001', 'tbt-b', { clone: false, stage: 'next' }]]);
});

test('its own board and a virtual group are not targets', async (t) => {
  const { calls, c, dt, start, over, drop, summary } = await mountTwo(t);
  const data = dt();
  await start('tk_00000001', data);
  assert.equal(over(summary('tbt-a'), data).defaultPrevented, false, 'its own group');
  const virtual = c.querySelector('details[data-group-key="virtual:ungrouped"] > summary');
  assert.equal(over(virtual, data).defaultPrevented, false, 'a virtual group');
  await drop(virtual, data);
  assert.deepEqual(calls, []);
});

test('a drop on a group on another node creates it there first, then drops the original', async (t) => {
  const { calls, remote, confirms, toasts, dt, start, over, drop, summary } = await mountTwo(t, { otherNode: 'nd_other' });
  const data = dt();
  await start('tk_00000001', data);
  assert.equal(over(summary('tbt-b'), data).defaultPrevented, true, 'another node\'s group takes it');
  await drop(summary('tbt-b'), data);
  assert.equal(confirms[0].title, 'Move task to another node?');
  assert.match(confirms[0].body, /created there in Ideas.*only then closed here as dropped \("moved to other\/tbt-b"\)/);
  assert.deepEqual(remote, [['nd_other', 'transferIn', 'tbt-b', { title: 'tbt-a first', body: '', ref: '', refLabel: '',
    stage: 'idea', clone: false, origin: { node: '', group: 'tbt-a', task: 'tk_00000001' } }]], 'the target node creates it');
  assert.deepEqual(calls, [['movedAway', 'tk_00000001', { node: 'other', group: 'tbt-b', task: 'tk_000000aa' }]],
    'then the source drops it, naming the new task');
  assert.match(toasts[0], /Moved "tbt-a first" to tbt-b on other/);
});

test('a clone to another node only creates; a failed drop is retried once, then warns of both copies', async (t) => {
  const { calls, remote, toasts, dt, start, drop, summary, failAway } = await mountTwo(t, { otherNode: 'nd_other' });
  let data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data, { ctrlKey: true });
  assert.equal(remote.length, 1);
  assert.equal(remote[0][3].clone, true);
  assert.deepEqual(calls, [], 'a clone leaves the original alone');

  failAway(1);
  data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data);
  assert.equal(calls.length, 2, 'one retry');
  assert.match(toasts.at(-1), /Moved/);

  failAway(2);
  calls.length = 0;
  data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data);
  assert.equal(calls.length, 2);
  assert.match(toasts.at(-1), /could not be closed here \(node offline\): both copies are live, drop this one by hand/);
});

test('the confirmation says what a move keeps and undoes', async (t) => {
  const harness = await createPreactHarness(t);
  const { transferMessage } = await harness.importDashboardModule('js/groups-taskboard-transfer.js');
  const task = { id: 'tk_1', title: 'ship it', status: 'blocked', depends_on: ['tk_0'], assignee: { agent_id: 'agt_x' } };
  const tasks = [{ id: 'tk_0', title: 'first' }, task];
  const away = transferMessage({ task, from: 'a', to: 'b', stage: 'idea', tasks, members: [] });
  assert.match(away.body, /It lands in Ideas\./);
  assert.match(away.body, /not a member of "b", so it is unassigned/);
  assert.match(away.body, /Its block is lifted\./);
  assert.match(away.body, /no longer waits for "first"/);
  const stays = transferMessage({ task: { ...task, status: 'open', depends_on: [] }, from: 'a', to: 'b', stage: 'idea', tasks, members: [{ agent_id: 'agt_x' }] });
  assert.match(stays.body, /It stays assigned and lands in Next\./);
  assert.doesNotMatch(stays.body, /unassigned/);
});
