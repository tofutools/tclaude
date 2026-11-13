import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// Reordering by hand (groups-taskboard-order.js): the ⋯ move items and
// Alt+keys on a List row or a lanes card, a drag inside a lanes cell, a drop
// into another cell at a slot, and the optimistic order while the write runs.

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });

function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 5, next: 3, ideas: 2, done_this_week: 2 },
    tasks: [
      row('tk_00000001', 'one'),
      row('tk_00000002', 'two'),
      row('tk_00000003', 'three'),
      row('tk_00000004', 'idea a', { stage: 'idea' }),
      row('tk_00000005', 'idea b', { stage: 'idea' }),
    ],
    closed: [
      row('tk_00000008', 'done late', { status: 'done', closed_at: ago(5) }),
      row('tk_00000009', 'done early', { status: 'done', closed_at: ago(50) }),
    ],
  };
}

// fakeApi records the writes; hold, when set, keeps move pending until
// release() and refuse makes it fail.
function fakeApi() {
  const calls = [];
  const api = { calls, refuse: false, gate: null };
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return {}; };
  Object.assign(api, { edit: record('edit'), stage: record('stage'), status: record('status'), assign: record('assign'),
    unassign: record('unassign'), reopen: record('reopen'), archive: record('archive'), start: record('start'), hold: record('hold') });
  api.move = async (...args) => {
    calls.push(['move', ...args]);
    if (api.gate) await api.gate;
    if (api.refuse) throw new Error('the board changed meanwhile');
    return {};
  };
  return api;
}

async function mount(t, name, { view = 'lanes', by = 'agent', api = fakeApi() } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, view);
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, by);
  const source = {
    context: harness.signals.signal({ groups: [board(name)] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {},
  };
  const group = { name, members: [{ agent_id: 'agt_builder', title: 'builder', online: true }] };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const settle = () => harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const cell = (lane, column) => c.querySelector(`.tb-cell[data-lane="${lane}"][data-column="${column}"]`);
  const ids = (lane, column) => [...(cell(lane, column)?.querySelectorAll('.tb-card') || [])].map((el) => el.dataset.taskId);
  const card = (id) => c.querySelector(`.tb-card[data-task-id="${id}"]`);
  const rowIds = () => [...c.querySelectorAll('tr[data-task-id]')].map((el) => el.dataset.taskId);
  const transfer = { setData() {}, effectAllowed: '', dropEffect: '', types: ['application/x-tclaude-task'] };
  // over points the drag at the lower half of card `on` (after) or its
  // upper half (before).
  const at = (on, where) => {
    const el = card(on);
    el.getBoundingClientRect = () => ({ top: 100, height: 20 });
    return { target: el, clientY: where === 'after' ? 115 : 105 };
  };
  const dragTo = async (id, lane, column, slot = null) => {
    await harness.act(async () => { harness.fireEvent(card(id), 'dragstart', { dataTransfer: transfer }); });
    const point = slot ? at(slot.on, slot.where) : { target: cell(lane, column), clientY: 0 };
    const over = harness.fireEvent(point.target, 'dragover', { dataTransfer: transfer, clientY: point.clientY });
    await harness.act(async () => {});
    const label = cell(lane, column).dataset.tbDrop || '';
    const marked = slot ? card(slot.on).className : '';
    await harness.act(async () => { harness.fireEvent(point.target, 'drop', { dataTransfer: transfer, clientY: point.clientY }); });
    await settle();
    return { taken: over.defaultPrevented, label, marked };
  };
  const menuItems = () => [...c.querySelectorAll('.gr-add-menu [role="menuitem"]')];
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  return { harness, c, api, source, cell, ids, card, rowIds, dragTo, menuItems, click, settle };
}

test('order helpers: moves among the same stage, keys, and the pending order', async (t) => {
  const harness = await createPreactHarness(t);
  const { orderMoves, orderKey, applyPendingMove } = await harness.importDashboardModule('js/groups-taskboard-order.js');
  const list = [row('a', 'a'), row('b', 'b', { status: 'blocked' }), row('x', 'x', { stage: 'idea', status: 'blocked' }), row('c', 'c')];
  assert.deepEqual(orderMoves(list, list[1]), {
    top: { before: 'a', stage: 'next' }, up: { before: 'a', stage: 'next' },
    down: { after: 'c', stage: 'next' }, bottom: { after: 'c', stage: 'next' },
  }, 'the blocked idea in the same cell is not a neighbour');
  assert.deepEqual(orderMoves(list, list[2]), { top: null, up: null, down: null, bottom: null }, 'alone in its stage');
  assert.equal(orderMoves([row('d', 'd', { status: 'done' })], row('d', 'd', { status: 'done' })), null, 'a done task has no order');

  assert.equal(orderKey({ altKey: true, key: 'ArrowUp' }), 'up');
  assert.equal(orderKey({ altKey: true, key: 'End' }), 'bottom');
  assert.equal(orderKey({ altKey: true, shiftKey: true, key: 'ArrowUp' }), '', 'other modifiers are not ours');
  assert.equal(orderKey({ key: 'ArrowUp' }), '');

  const entry = { tasks: [row('a', 'a'), row('b', 'b'), row('i', 'i', { stage: 'idea' })] };
  assert.deepEqual(applyPendingMove(entry, { id: 'b', before: 'a', stage: 'next' }).tasks.map((t) => t.id), ['b', 'a', 'i']);
  const crossed = applyPendingMove(entry, { id: 'a', after: 'i' }).tasks;
  assert.deepEqual(crossed.map((t) => `${t.id}:${t.stage}`), ['b:next', 'i:idea', 'a:idea'], 'a move across takes the anchor\'s stage');
  assert.equal(applyPendingMove(entry, null), entry);
  assert.equal(applyPendingMove(entry, { id: 'gone', before: 'a' }), entry, 'a task no longer there leaves the board as it is');
});

test('a lanes card drags to a slot in its own cell: one move, the slot marked while it points there', async (t) => {
  const { api, ids, dragTo } = await mount(t, 'tbo-cell');
  assert.deepEqual(ids('unassigned', 'next'), ['tk_00000001', 'tk_00000002', 'tk_00000003']);
  const out = await dragTo('tk_00000003', 'unassigned', 'next', { on: 'tk_00000001', where: 'before' });
  assert.ok(out.taken);
  assert.match(out.label, /Move before “one”/);
  assert.match(out.marked, /tb-drop-before/);
  assert.deepEqual(api.calls, [['move', 'tk_00000003', { before: 'tk_00000001', stage: 'next' }]]);

  // Released on itself (a drag given up, or a twitch), the card stays put.
  api.calls.length = 0;
  const self = await dragTo('tk_00000001', 'unassigned', 'next', { on: 'tk_00000001', where: 'before' });
  assert.equal(self.taken, false);
  assert.deepEqual(api.calls, [], 'no move to the bottom');

  // A drop where the card already is takes nothing.
  api.calls.length = 0;
  const still = await dragTo('tk_00000002', 'unassigned', 'next', { on: 'tk_00000001', where: 'after' });
  assert.equal(still.taken, false);
  assert.deepEqual(api.calls, []);
});

test('Done is ordered by close time: a drag inside it says so and writes nothing; its cards have no move items', async (t) => {
  const { api, c, card, dragTo, menuItems, click } = await mount(t, 'tbo-done');
  assert.match(c.querySelector('[data-col="done"]').textContent, /by close time/);
  const out = await dragTo('tk_00000008', 'unassigned', 'done', { on: 'tk_00000009', where: 'after' });
  assert.equal(out.taken, false);
  assert.equal(out.label, 'Done is ordered by when tasks closed');
  assert.deepEqual(api.calls, []);
  await click(card('tk_00000009').querySelector('.tb-card-more'));
  assert.ok(!menuItems().some((b) => /^Move/.test(b.textContent.trim())), menuItems().map((b) => b.textContent).join(','));
});

test('an idea dropped into Next at a slot is promoted, then put at the slot', async (t) => {
  const { api, dragTo } = await mount(t, 'tbo-promote');
  const out = await dragTo('tk_00000004', 'unassigned', 'next', { on: 'tk_00000002', where: 'before' });
  assert.ok(out.taken);
  assert.match(out.label, /before “two”/);
  assert.deepEqual(api.calls, [['stage', 'tk_00000004', 'next'], ['move', 'tk_00000004', { before: 'tk_00000002', stage: 'next' }]]);
});

test('a card\'s ⋯ moves it within its cell, and Alt+↑ does the same from the keyboard', async (t) => {
  const { api, card, menuItems, click, harness, settle } = await mount(t, 'tbo-menu');
  await click(card('tk_00000002').querySelector('.tb-card-more'));
  const labels = menuItems().map((b) => b.textContent.trim());
  assert.deepEqual(labels.slice(0, 4), ['Move to top', 'Move up', 'Move down', 'Move to bottom']);
  await click(menuItems().find((b) => b.textContent.trim() === 'Move to bottom'));
  assert.deepEqual(api.calls, [['move', 'tk_00000002', { after: 'tk_00000003', stage: 'next' }]]);

  // The first card's Move up and Move to top are greyed.
  await click(card('tk_00000001').querySelector('.tb-card-more'));
  const top = menuItems().find((b) => b.textContent.trim() === 'Move to top');
  assert.ok(top.disabled || top.getAttribute('aria-disabled') === 'true');
  await click(card('tk_00000001').querySelector('.tb-card-more'));

  api.calls.length = 0;
  await harness.act(async () => { harness.fireEvent(card('tk_00000005').querySelector('.tb-card-title button') || card('tk_00000005'), 'keydown', { key: 'ArrowUp', altKey: true }); });
  await settle();
  assert.deepEqual(api.calls, [['move', 'tk_00000005', { before: 'tk_00000004', stage: 'idea' }]]);
});

test('the List: Alt+↓ on a row moves it among its rows; the order shows at once and a refusal puts it back', async (t) => {
  const api = fakeApi();
  let release;
  api.gate = new Promise((r) => { release = r; });
  const { rowIds, harness, c, settle } = await mount(t, 'tbo-list', { view: 'list', api });
  assert.deepEqual(rowIds(), ['tk_00000001', 'tk_00000002', 'tk_00000003', 'tk_00000004', 'tk_00000005']);
  const rowEl = c.querySelector('tr[data-task-id="tk_00000001"]');
  await harness.act(async () => { harness.fireEvent(rowEl, 'keydown', { key: 'ArrowDown', altKey: true }); });
  assert.deepEqual(api.calls, [['move', 'tk_00000001', { after: 'tk_00000002', stage: 'next' }]]);
  assert.deepEqual(rowIds().slice(0, 3), ['tk_00000002', 'tk_00000001', 'tk_00000003'], 'drawn at its new place while the write runs');

  // The board refuses: the board as it was comes back (no refresh changed it).
  api.refuse = true;
  await harness.act(async () => { release(); await new Promise((r) => setTimeout(r, 0)); });
  await settle();
  assert.deepEqual(rowIds().slice(0, 3), ['tk_00000001', 'tk_00000002', 'tk_00000003'], 'snapped back');
  // The last idea has no Alt+↓.
  api.calls.length = 0;
  api.gate = null;
  await harness.act(async () => { harness.fireEvent(c.querySelector('tr[data-task-id="tk_00000005"]'), 'keydown', { key: 'ArrowDown', altKey: true }); });
  assert.deepEqual(api.calls, []);
});
