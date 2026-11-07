// Dragging a task to another group's board (groups-taskboard-transfer.js,
// tcl-0r0obl): a drop on another group's Task board section or header row
// moves it there after the Groups view's confirmation; Ctrl/Cmd clones.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
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
  let awayStatus = 0;
  let checkRefuses = false;
  transfer.transferDeps.nodeApi = async (node) => ({
    transferIn: async (...args) => { remote.push([node, 'transferIn', ...args]); return { id: 'tk_000000aa' }; },
  });
  const api = {
    moveToGroup: async (...args) => { calls.push(args); return { task: { id: args[0] }, group: args[1], dropped_dependents: ['tk_00000002'] }; },
    movedAway: async (id, to, opts) => {
      if (opts?.check) {
        calls.push(['check', id]);
        if (checkRefuses) throw Object.assign(new Error('requires taskboard.manage'), { status: 403 });
        return { ok: true };
      }
      const args = [id, to];
      calls.push(['movedAway', ...args]);
      if (awayFails > 0) { awayFails--; throw new Error('node offline'); }
      if (awayStatus) { const status = awayStatus; awayStatus = 0; throw Object.assign(new Error(`status ${status}`), { status }); }
      return { id: args[0], status: 'dropped' };
    },
    move: async () => ({}), assign: async () => ({}), status: async (...args) => { calls.push(['status', ...args]); return {}; }, stage: async () => ({}),
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
  const mounted = await harness.mount(harness.html`<div>${card('tbt-a')}${card('tbt-b', otherNode)}<details class="group-virtual" data-group-key="virtual:ungrouped" data-fleet-node=${otherNode || undefined}><summary>Ungrouped</summary></details></div>`);
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
    failAway: (n) => { awayFails = n; }, refuseCheck: () => { checkRefuses = true; },
    awayAnswers: (status) => { awayStatus = status; }, transfer };
}

// pickMenu opens a list row's ⋯ and picks the path's items in turn.
async function pickMenu(harness, c, id, ...path) {
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  await click(c.querySelector(`tr[data-task-id="${id}"] .tb-act-more`));
  for (const label of path) {
    const items = [...c.querySelectorAll('.gr-add-menu [role="menuitem"]')];
    const item = items.find((b) => b.textContent.trim().replace(/ ›$/, '') === label);
    assert.ok(item, `menu item ${label}: ${items.map((b) => b.textContent.trim()).join(', ')}`);
    await click(item);
  }
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
}

test('a list row\'s ⋯ moves or clones a task to the groups a drag reaches, after the same confirmation', async (t) => {
  const { harness, c, calls, confirms } = await mountTwo(t);
  await pickMenu(harness, c, 'tk_00000001', 'Move to group');
  assert.deepEqual([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].map((b) => b.textContent.trim()), ['‹ Back', 'tbt-b'],
    'its own board and a virtual group are not targets');
  await harness.act(async () => { harness.fireEvent([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].find((b) => b.textContent.trim() === 'tbt-b'), 'click'); });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(confirms[0].title, 'Move task to another group?');
  await pickMenu(harness, c, 'tk_00000002', 'Clone to group', 'tbt-b');
  assert.equal(confirms[1].title, 'Clone task to another group?');
  assert.deepEqual(calls, [['tk_00000001', 'tbt-b', { clone: false, stage: 'idea' }], ['tk_00000002', 'tbt-b', { clone: true, stage: 'idea' }]]);
});

test('a list row\'s ⋯ moves a task to a group on another node as a drop does', async (t) => {
  const { harness, c, calls, remote } = await mountTwo(t, { otherNode: 'nd_other' });
  await pickMenu(harness, c, 'tk_00000001', 'Move to group');
  assert.deepEqual([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].map((b) => b.textContent.trim()), ['‹ Back', 'tbt-b'],
    'the fleet view\'s virtual groups carry a node too, and are still no target');
  await harness.act(async () => { harness.fireEvent(c.querySelector('tr[data-task-id="tk_00000001"] .tb-act-more'), 'click'); });
  await pickMenu(harness, c, 'tk_00000001', 'Move to group', 'tbt-b');
  assert.equal(remote[0][0], 'nd_other', 'the target node creates it');
  assert.deepEqual(calls, [['check', 'tk_00000001'], ['movedAway', 'tk_00000001', { node: 'other', group: 'tbt-b', task: 'tk_000000aa' }]]);
});

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
  assert.deepEqual(calls, [['check', 'tk_00000001'], ['movedAway', 'tk_00000001', { node: 'other', group: 'tbt-b', task: 'tk_000000aa' }]],
    'the drop\'s gates first, then the source drops it, naming the new task');
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
  assert.equal(calls.filter((c) => c[0] === 'movedAway').length, 2, 'one retry');
  assert.match(toasts.at(-1), /Moved/);

  failAway(2);
  calls.length = 0;
  data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data);
  assert.equal(calls.filter((c) => c[0] === 'movedAway').length, 2);
  assert.match(toasts.at(-1), /could not be closed here \(node offline\): both copies are live, drop this one by hand/);
});

test('a drop the source would refuse is known before anything is created; a lost answer is not a duplicate', async (t) => {
  const { calls, remote, toasts, dt, start, drop, summary, refuseCheck, awayAnswers, failAway } = await mountTwo(t, { otherNode: 'nd_other' });
  // A first try that landed but whose answer was lost: the retry finds the
  // task closed (409), and that is the move done.
  failAway(1);
  awayAnswers(409);
  let data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data);
  assert.equal(calls.filter((c) => c[0] === 'movedAway').length, 2);
  assert.match(toasts.at(-1), /^Moved/, 'a lost answer then 409 is a move done');

  // A refusal on the drop is not retried.
  awayAnswers(403);
  calls.length = 0;
  data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data);
  assert.equal(calls.filter((c) => c[0] === 'movedAway').length, 1, 'a 403 is not retried');

  refuseCheck();
  remote.length = 0;
  data = dt();
  await start('tk_00000001', data);
  await drop(summary('tbt-b'), data);
  assert.deepEqual(remote, [], 'refused up front: nothing is created on the other node');
});

test('a done task does not move to another node, so no copy is left behind', async (t) => {
  const harness = await createPreactHarness(t);
  const transfer = await harness.importDashboardModule('js/groups-taskboard-transfer.js');
  const saved = { ...transfer.transferDeps };
  t.after(() => Object.assign(transfer.transferDeps, saved));
  const toasts = [];
  let created = 0;
  transfer.transferDeps.toast = (text) => toasts.push(text);
  transfer.transferDeps.confirm = async () => true;
  transfer.transferDeps.nodeApi = async () => ({ transferIn: async () => { created++; return { id: 'tk_x' }; } });
  const out = await transfer.runCrossTransfer({ task: { id: 'tk_1', title: 'shipped', status: 'done' }, group: { name: 'a' },
    actions: { run: async (_k, _l, fn) => fn({ movedAway: async () => ({}) }) }, to: 'b', node: 'nd_other', nodeName: 'other', stage: 'idea', clone: false });
  assert.equal(out, null);
  assert.equal(created, 0);
  assert.match(toasts[0], /only an open or blocked task moves to another node/);
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
  const done = transferMessage({ task: { ...task, status: 'done', depends_on: [] }, from: 'a', to: 'b', stage: 'idea', tasks, members: [{ agent_id: 'agt_x' }] });
  assert.match(done.body, /It stays done and assigned\./);
  assert.doesNotMatch(done.body, /Next/);
  const linked = transferMessage({ task, from: 'a', to: 'b', stage: 'idea', tasks, members: null });
  assert.match(linked.body, /If its assignee is not a member of "b", it is unassigned\./, 'a linked node\'s members are not guessed');
});

test('a drag that is not a task drag is left alone, even by a listener a missed dragend left behind', async (t) => {
  const { calls, confirms, dt, start, over, drop, summary } = await mountTwo(t);
  await start('tk_00000001', dt());
  const agentDrag = { ...dt(), types: ['application/x-tclaude-agent'] };
  assert.equal(over(summary('tbt-b'), agentDrag).defaultPrevented, false, 'an agent drag onto a group is not taken');
  await drop(summary('tbt-b'), agentDrag);
  assert.equal(confirms.length, 0);
  assert.deepEqual(calls, []);
});

test('a list row drags as a whole, as an agent row does: the grab cursor, controls still click, and a drop on another group moves it', async (t) => {
  const { harness, c, calls, confirms, dt, over, drop, section } = await mountTwo(t);
  const row = c.querySelector('tr[data-task-id="tk_00000001"]');
  assert.equal(row.draggable, true, 'the whole row is the drag source, not only its ⋮⋮');
  assert.equal(row.getAttribute('data-dnd-task'), 'tk_00000001');
  assert.equal(row.querySelector('.tb-handle').draggable, false, 'the ⋮⋮ is the row\'s grip, not a second source');
  const closedRows = [...c.querySelectorAll('tr.tb-closed')];
  assert.ok(closedRows.every((r) => !r.draggable), 'a closed task does not drag');
  // The same cursor rule as agent and resource rows.
  const css = readFileSync(new URL('../dashboard/dashboard.css', import.meta.url), 'utf8');
  assert.match(css, /\.dnd-draggable, tr\[data-dnd-resource\]\[draggable="true"\], tr\[data-dnd-task\]\[draggable="true"\] \{\s*cursor: grab;/);

  // The shared guard (row-drag.js): a press on a control is a click.
  const { bindTaskRowDrag } = await harness.importDashboardModule('js/row-drag.js');
  t.after(bindTaskRowDrag(harness.document));
  harness.fireEvent(row.querySelector('.tb-act-more'), 'pointerdown', {});
  assert.equal(row.draggable, false, 'a press on ⋯ does not start a drag');
  harness.fireEvent(harness.document, 'pointerup', {});
  assert.equal(row.draggable, true, 'the row drags again once the press ends');
  harness.fireEvent(row.querySelector('.tb-title'), 'pointerdown', {});
  assert.equal(row.draggable, false, 'the title is a button (it opens the task): a press on it is a click');
  harness.fireEvent(harness.document, 'pointerup', {});
  harness.fireEvent(row.querySelector('.tb-t'), 'pointerdown', {});
  assert.equal(row.draggable, true, 'a press on a plain cell starts a drag');
  harness.fireEvent(harness.document, 'pointerup', {});

  // A drag of a link in the row stays the browser's URL drag.
  const link = harness.document.createElement('a');
  link.setAttribute('href', 'https://example.test/SHOP-1');
  row.querySelector('.tb-title-cell').append(link);
  const linkData = dt();
  await harness.act(async () => { harness.fireEvent(link, 'dragstart', { dataTransfer: linkData }); });
  assert.equal(linkData.effectAllowed, '', 'a link drag is not a task drag');
  link.remove();

  // A drag from the row itself, dropped on another group's board.
  const data = dt();
  await harness.act(async () => { harness.fireEvent(row, 'dragstart', { dataTransfer: data }); });
  const target = section('tbt-b').querySelector('.gf-section-head');
  assert.equal(over(target, data).defaultPrevented, true);
  await drop(target, data);
  assert.equal(confirms[0].title, 'Move task to another group?');
  assert.deepEqual(calls, [['tk_00000001', 'tbt-b', { clone: false, stage: 'idea' }]]);
});

test('a list row dragged as a whole onto a group on another node goes there', async (t) => {
  const { harness, c, calls, remote, dt, drop, summary } = await mountTwo(t, { otherNode: 'nd_other' });
  const data = dt();
  await harness.act(async () => { harness.fireEvent(c.querySelector('tr[data-task-id="tk_00000001"]'), 'dragstart', { dataTransfer: data }); });
  await drop(summary('tbt-b'), data);
  assert.deepEqual(remote.map((call) => call.slice(0, 3)), [['nd_other', 'transferIn', 'tbt-b']], 'the other node creates it');
  assert.deepEqual(calls, [['check', 'tk_00000001'], ['movedAway', 'tk_00000001', { node: 'other', group: 'tbt-b', task: 'tk_000000aa' }]],
    'then the source drops it');
});

// bin is the groups view's #dnd-trash (dashboard.html), as dnd.js shows it.
function addBin(harness) {
  const bin = harness.document.createElement('div');
  bin.id = 'dnd-trash';
  bin.setAttribute('data-dnd-target-retired', '1');
  harness.document.body.append(bin);
  return bin;
}

test('a task dragged to the groups view\'s bin is dropped after the bin\'s confirmation', async (t) => {
  const { harness, calls, confirms, dt, start, over, drop } = await mountTwo(t);
  const bin = addBin(harness);
  assert.equal(bin.classList.contains('show'), false);
  const data = dt();
  await start('tk_00000001', data);
  assert.ok(bin.classList.contains('show'), 'the bin appears when a task drag starts');
  assert.ok(bin.classList.contains('dnd-trash-task-mode'), 'and says Drop, not Retire');
  assert.equal(over(bin, data, { ctrlKey: true }).defaultPrevented, true, 'it takes the task (Ctrl/Cmd means nothing there)');
  assert.ok(bin.classList.contains('dnd-drop-over'));
  assert.equal(data.dropEffect, 'move');
  await drop(bin, data);
  assert.equal(confirms[0].title, 'Drop task?');
  assert.equal(confirms[0].okLabel, 'Drop');
  assert.deepEqual(calls, [['status', 'tk_00000001', 'dropped']], 'the drop route, as ⋯ Drop');
  assert.equal(bin.classList.contains('show'), false, 'the bin goes with the drag');
});

test('a lanes card dragged to the bin is dropped too; a cancelled confirmation changes nothing', async (t) => {
  const { harness, calls, confirms, dt, start, drop, setAnswer } = await mountTwo(t, { lanes: true });
  const bin = addBin(harness);
  setAnswer(false);
  const data = dt();
  await start('tk_00000003', data);
  assert.ok(bin.classList.contains('show'), 'a card drag shows the bin');
  await drop(bin, data);
  assert.equal(confirms.length, 1);
  assert.deepEqual(calls, [], 'cancelled: nothing is dropped');
});

test('the bin is offered only for a task the viewer may drop, as ⋯ Drop is', async (t) => {
  const harness = await createPreactHarness(t);
  const { binDroppable } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { listenForTransfer } = await harness.importDashboardModule('js/groups-taskboard-transfer.js');
  const local = { api: { moveToGroup: async () => ({}) } };
  const peerNoAssign = { api: { moveToGroup: async () => ({}), can: { assign: false } } };
  assert.equal(binDroppable(row('g', 'tk_1', 'a'), local), true, 'the operator drops any live task');
  assert.equal(binDroppable(row('g', 'tk_1', 'a', { status: 'blocked' }), local), true);
  assert.equal(binDroppable(row('g', 'tk_1', 'a', { status: 'done' }), local), false, 'a closed task is not dropped again');
  assert.equal(binDroppable(row('g', 'tk_1', 'a', { assignee: { agent_id: 'agt_dev' } }), peerNoAssign), false,
    'an assigned task on a peer that grants no assign is not this viewer\'s to drop');
  assert.equal(binDroppable(row('g', 'tk_1', 'a'), peerNoAssign), true, 'an unassigned one is');
  const bin = addBin(harness);
  const off = listenForTransfer({ task: row('g', 'tk_1', 'a'), group: { name: 'g' }, actions: local, droppable: false });
  assert.equal(bin.classList.contains('show'), false, 'not droppable: no bin');
  off();
});

test('Ctrl/Cmd over another group shows the groups view\'s clone look, and back to a move when released', async (t) => {
  const { c, dt, start, over, summary, section } = await mountTwo(t);
  const box = c.querySelector('details[data-group-key="tbt-b"]');
  const data = dt();
  await start('tk_00000001', data);
  over(summary('tbt-b'), data);
  assert.ok(box.classList.contains('dnd-drop-over'), 'the target group gets the groups view\'s drop look');
  assert.equal(box.classList.contains('dnd-effect-clone'), false, 'a move');
  over(summary('tbt-b'), data, { ctrlKey: true });
  assert.ok(box.classList.contains('dnd-effect-clone'), 'Ctrl held: the clone look');
  assert.ok(summary('tbt-b').classList.contains('tb-transfer-clone'));
  assert.match(summary('tbt-b').getAttribute('data-tb-transfer'), /^clone to tbt-b/);
  over(section('tbt-b').querySelector('.gf-section-head'), data, { metaKey: true });
  assert.ok(box.classList.contains('dnd-effect-clone'), 'Cmd on the board too');
  // A browser sets each dragover's dropEffect from the keys held: a move
  // once Cmd is released.
  data.dropEffect = 'move';
  over(section('tbt-b').querySelector('.gf-section-head'), data);
  assert.equal(box.classList.contains('dnd-effect-clone'), false, 'released mid-drag: a move again');
  assert.equal(section('tbt-b').classList.contains('tb-transfer-clone'), false);
  over(c.querySelector('details[data-group-key="tbt-a"] > summary'), data);
  assert.equal(box.classList.contains('dnd-drop-over'), false, 'off the target, its look goes');
});

test('a task drag whose dragend never came leaves no bin behind: the next drag of any kind ends it', async (t) => {
  const harness = await createPreactHarness(t);
  const { listenForTransfer, transferDeps } = await harness.importDashboardModule('js/groups-taskboard-transfer.js');
  const confirms = [];
  const saved = { ...transferDeps };
  transferDeps.confirm = async (o) => { confirms.push(o); return false; };
  t.after(() => Object.assign(transferDeps, saved));
  const bin = addBin(harness);
  const actions = { api: { moveToGroup: async () => ({}) }, run: async () => true };
  // A drag on one board, its dragend lost; then a drag on another board.
  listenForTransfer({ task: row('a', 'tk_1', 'stale'), group: { name: 'a' }, actions, droppable: true });
  listenForTransfer({ task: row('b', 'tk_2', 'dragged'), group: { name: 'b' }, actions, droppable: true });
  const types = ['application/x-tclaude-task'];
  harness.fireEvent(bin, 'drop', { dataTransfer: { types } });
  await new Promise((r) => setTimeout(r, 0));
  assert.deepEqual(confirms.map((c) => c.meta), ['dragged'], 'only the task being dragged is asked about');
  // Lost again; then an agent drag starts.
  listenForTransfer({ task: row('b', 'tk_2', 'dragged'), group: { name: 'b' }, actions, droppable: true });
  assert.ok(bin.classList.contains('dnd-trash-task-mode'));
  harness.fireEvent(harness.document.body, 'dragstart', { dataTransfer: { types: ['text/plain'] } });
  assert.equal(bin.classList.contains('dnd-trash-task-mode'), false, 'the agent drag gets the bin as Retire, not Drop');
});

// asMacChrome makes the page's browser macOS Chrome for one test, as
// helpers.js isMacChromeZeroReleaseLeave reads it.
function asMacChrome(t) {
  const desc = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', { configurable: true, writable: true,
    value: { platform: 'MacIntel', userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36' } });
  t.after(() => { if (desc) Object.defineProperty(globalThis, 'navigator', desc); else delete globalThis.navigator; });
}

const zeroRelease = { relatedTarget: null, clientX: 0, clientY: 0, screenX: 0, screenY: 0 };

test('macOS Chrome: a Cmd-clone that ends with no drop is finished from its release, as an agent clone is', async (t) => {
  const { harness, calls, confirms, dt, start, over, summary } = await mountTwo(t);
  // After the harness, which installs its own navigator.
  asMacChrome(t);
  const data = dt();
  await start('tk_00000001', data);
  over(summary('tbt-b'), data, { metaKey: true });
  // The release: an all-zero dragleave, and no drop event.
  await harness.act(async () => { harness.fireEvent(summary('tbt-b'), 'dragleave', { dataTransfer: data, ...zeroRelease }); await new Promise((r) => setTimeout(r, 0)); });
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(confirms[0]?.title, 'Clone task to another group?', 'the clone asks, as a drop would');
  assert.deepEqual(calls, [['tk_00000001', 'tbt-b', { clone: true, stage: 'idea' }]]);
  assert.equal(summary('tbt-b').classList.contains('tb-transfer-over'), false, 'and the look goes');
});

test('a clone left pending is finished on dragend; a real drop, a plain move or Escape leave nothing pending', async (t) => {
  const { harness, c, calls, confirms, dt, start, over, drop, summary } = await mountTwo(t);
  const rowOf = (id) => c.querySelector(`tr[data-task-id="${id}"]`);
  const end = async (id) => {
    await harness.act(async () => { harness.fireEvent(rowOf(id), 'dragend', {}); await new Promise((r) => setTimeout(r, 0)); });
    await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  };
  // Pending, then dragend with no drop: the clone runs.
  let data = dt();
  await start('tk_00000001', data);
  over(summary('tbt-b'), data, { ctrlKey: true });
  await end('tk_00000001');
  assert.equal(confirms.length, 1);
  assert.deepEqual(calls, [['tk_00000001', 'tbt-b', { clone: true, stage: 'idea' }]]);
  // A real drop: one clone, not a second from dragend.
  data = dt();
  await start('tk_00000002', data);
  over(summary('tbt-b'), data, { ctrlKey: true });
  await drop(summary('tbt-b'), data, { ctrlKey: true });
  await end('tk_00000002');
  assert.equal(calls.length, 2, 'the drop ran it once');
  // A move hover, or Escape, leaves no clone for dragend.
  data = dt();
  await start('tk_00000001', data);
  over(summary('tbt-b'), data, { ctrlKey: true });
  data.dropEffect = 'move';
  over(summary('tbt-b'), data);
  await end('tk_00000001');
  data = dt();
  await start('tk_00000001', data);
  over(summary('tbt-b'), data, { ctrlKey: true });
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  await end('tk_00000001');
  assert.equal(calls.length, 2, 'released or Escaped: nothing runs');
  assert.equal(confirms.length, 2);
});
