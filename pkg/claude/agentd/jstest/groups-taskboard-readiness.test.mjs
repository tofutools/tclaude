import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// Grouping a column by readiness (groups-taskboard-readiness.js,
// tk_3876b835): the viewer's own view, ready cards first, then queued, then
// blocked, each in the board's order, with no write; while grouped a card
// moves within its group.

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });

// The Next column in the board's order: a ready card on top, then a queued
// one (a dependency), a ready one, a queued one (in line for a full lane),
// a ready one. Two ideas, one queued, and two parked ones, one queued.
function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 9, next: 5, ideas: 2, backlog: 2, done_this_week: 0 },
    tasks: [
      row('tk_a', 'alpha'),
      row('tk_b', 'bravo', { depends_on: ['tk_x'], unmet: ['tk_x'], queued: true, queued_reason: 'waiting on tk_x' }),
      row('tk_c', 'charlie'),
      row('tk_d', 'delta', { queued: true, queued_for: 'in_progress', queued_reason: 'In progress is full' }),
      row('tk_e', 'echo'),
      row('tk_i1', 'idea queued', { stage: 'idea', depends_on: ['tk_x'], unmet: ['tk_x'], queued: true }),
      row('tk_i2', 'idea ready', { stage: 'idea' }),
      row('tk_p1', 'parked queued', { stage: 'backlog', depends_on: ['tk_x'], unmet: ['tk_x'], queued: true }),
      row('tk_p2', 'parked ready', { stage: 'backlog' }),
    ],
    closed: [],
  };
}

function fakeApi() {
  const calls = [];
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return {}; };
  return { calls, move: record('move'), stage: record('stage'), status: record('status'), edit: record('edit'), assign: record('assign'),
    unassign: record('unassign'), reopen: record('reopen'), archive: record('archive'), start: record('start'), hold: record('hold') };
}

async function mount(t, name, { view = 'lanes', by = 'agent', prefs = {} } = {}) {
  const api = fakeApi();
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, view);
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, by);
  for (const [key, value] of Object.entries(prefs)) dashPrefs.setItem(`tclaude.dash.groups.taskboard.${key}.${name}`, value);
  const source = {
    context: harness.signals.signal({ groups: [board(name)] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {},
  };
  const group = { name, members: [{ agent_id: 'agt_builder', title: 'builder', online: true }] };
  const { container: c } = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const settle = () => harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  const cell = (column) => c.querySelector(`.tb-cell[data-lane="unassigned"][data-column="${column}"]`);
  const ids = (el) => [...(el?.querySelectorAll('[data-task-id]') || [])].map((x) => x.dataset.taskId);
  const heads = (el) => [...(el?.querySelectorAll('[data-tb-ready-head]') || [])].map((x) => x.textContent.replace(/\s+/g, ' ').trim());
  const card = (id) => c.querySelector(`.tb-card[data-task-id="${id}"]`);
  const menuItems = () => [...harness.document.querySelectorAll('.gr-add-menu [role="menuitem"]')];
  const colMenu = async (column) => {
    const head = [...c.querySelectorAll('.tb-lane-columns .tb-col-more')].find((b) => (b.getAttribute('aria-label') || '').includes(`the ${column} column`));
    assert.ok(head, `${column} has a ⋯`);
    await click(head);
  };
  return { harness, c, api, dashPrefs, settle, click, cell, ids, heads, card, menuItems, colMenu };
}

test('readiness helpers: ready, queued and blocked; a stable grouping; heads only when something is not ready', async (t) => {
  const harness = await createPreactHarness(t);
  const { readiness, byReadiness, readinessPeers, groupable } = await harness.importDashboardModule('js/groups-taskboard-readiness.js');
  assert.equal(readiness(row('a', 'a')), 'ready');
  assert.equal(readiness(row('a', 'a', { unmet: ['x'] })), 'queued', 'an unmet dependency');
  assert.equal(readiness(row('a', 'a', { queued: true, queued_for: 'in_progress' })), 'queued', 'in line for a full lane');
  assert.equal(readiness(row('a', 'a', { status: 'blocked' })), 'blocked');
  assert.equal(readiness(row('a', 'a', { stalled: true, assignee: { agent_id: 'agt_x' } })), 'blocked', 'stalled with its agent, as the Lanes draw it');
  assert.equal(readiness(row('a', 'a', { status: 'done' })), '');

  const list = [row('w1', 'w1', { unmet: ['x'] }), row('r1', 'r1'), row('b1', 'b1', { status: 'blocked' }), row('r2', 'r2'), row('w2', 'w2', { queued: true })];
  const grouped = byReadiness(list);
  assert.deepEqual(grouped.list.map((x) => x.id), ['r1', 'r2', 'w1', 'w2', 'b1'], 'each group keeps its order');
  assert.deepEqual([...grouped.heads].map(([id, h]) => `${id}:${h.key}:${h.count}`), ['r1:ready:2', 'w1:queued:2', 'b1:blocked:1']);
  assert.deepEqual(readinessPeers(grouped.list, list[0]).map((x) => x.id), ['w1', 'w2']);

  const ready = byReadiness([row('r1', 'r1'), row('r2', 'r2')]);
  assert.deepEqual(ready.list.map((x) => x.id), ['r1', 'r2']);
  assert.equal(ready.heads.size, 0, 'a column with nothing queued draws no heads');

  assert.ok(groupable({ key: 'next' }) && groupable({ key: 'ideas' }) && groupable({ key: 'progress' }) && groupable({ key: 'col:review' }) && groupable({ key: 'backlog' }));
  assert.ok(!groupable({ key: 'blocked' }) && !groupable({ key: 'done' }));
});

test('Lanes: Next\'s ⋯ groups it by readiness, remembered; nothing is written; the chip ungroups it', async (t) => {
  const { c, api, dashPrefs, click, cell, ids, heads, menuItems, colMenu } = await mount(t, 'rd-lanes');
  assert.deepEqual(ids(cell('next')), ['tk_a', 'tk_b', 'tk_c'], 'the board\'s order, three drawn');
  assert.equal(c.querySelector('[data-tb-ready="next"]'), null);

  await colMenu('Next');
  const item = menuItems().find((b) => b.textContent.trim() === 'Group by readiness');
  assert.ok(item, menuItems().map((b) => b.textContent).join(','));
  await click(item);
  await click(cell('next').querySelector('.tb-cell-more'));
  assert.deepEqual(ids(cell('next')), ['tk_a', 'tk_c', 'tk_e', 'tk_b', 'tk_d'], 'ready first: alpha stays on top, bravo and delta go under');
  assert.deepEqual(heads(cell('next')), ['Ready 3', 'Queued 2']);
  assert.deepEqual(api.calls, [], 'a view: no write');
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.groups.taskboard.readiness.rd-lanes')), ['next'], 'remembered per viewer and board');
  assert.deepEqual(ids(cell('ideas')), ['tk_i1', 'tk_i2'], 'Ideas is not grouped: only the column asked');

  const chip = c.querySelector('[data-col="next"] [data-tb-ready="next"]');
  assert.ok(chip, 'the grouped head says so');
  await click(chip);
  assert.deepEqual(ids(cell('next')), ['tk_a', 'tk_b', 'tk_c', 'tk_d', 'tk_e'], 'the board\'s order again');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.readiness.rd-lanes'), null);

  // Blocked and Done hold no readiness to group by.
  assert.equal(c.querySelector('[data-col="blocked"] .tb-col-more'), null);
});

test('Lanes, grouped: a card\'s ⋯ and Alt keys move it within its group, and a drag across groups is refused', async (t) => {
  const { harness, api, card, cell, click, menuItems, settle } = await mount(t, 'rd-moves', { prefs: { readiness: '["next"]' } });
  await click(cell('next').querySelector('.tb-cell-more'));
  // Echo's Move to top goes before alpha, the first ready card.
  await click(card('tk_e').querySelector('.tb-card-more'));
  await click(menuItems().find((b) => b.textContent.trim() === 'Move to top'));
  assert.deepEqual(api.calls, [['move', 'tk_e', { before: 'tk_a', stage: 'next' }]]);
  // Bravo is the first queued card: no Move up, though echo is drawn above.
  await click(card('tk_b').querySelector('.tb-card-more'));
  const up = menuItems().find((b) => b.textContent.trim() === 'Move up');
  assert.ok(up.disabled || up.getAttribute('aria-disabled') === 'true', 'bravo is on top of its group');
  await click(card('tk_b').querySelector('.tb-card-more'));
  // Delta's Alt+Home goes before bravo.
  api.calls.length = 0;
  await harness.act(async () => { harness.fireEvent(card('tk_d').querySelector('.tb-title'), 'keydown', { key: 'Home', altKey: true }); });
  await settle();
  assert.deepEqual(api.calls, [['move', 'tk_d', { before: 'tk_b', stage: 'next' }]]);

  // A drag of delta (queued) onto alpha (ready) is refused and says why.
  api.calls.length = 0;
  const transfer = { setData() {}, effectAllowed: '', dropEffect: '', types: ['application/x-tclaude-task'] };
  const at = (id) => { const el = card(id); el.getBoundingClientRect = () => ({ top: 100, height: 20 }); return el; };
  await harness.act(async () => { harness.fireEvent(card('tk_d'), 'dragstart', { dataTransfer: transfer }); });
  const over = harness.fireEvent(at('tk_a'), 'dragover', { dataTransfer: transfer, clientY: 105 });
  await harness.act(async () => {});
  assert.equal(over.defaultPrevented, false);
  assert.match(cell('next').dataset.tbDrop || '', /within its group/);
  await harness.act(async () => { harness.fireEvent(at('tk_a'), 'drop', { dataTransfer: transfer, clientY: 105 }); });
  await settle();
  assert.deepEqual(api.calls, []);
  // Onto bravo (queued), it moves.
  await harness.act(async () => { harness.fireEvent(card('tk_d'), 'dragstart', { dataTransfer: transfer }); });
  const ok = harness.fireEvent(at('tk_b'), 'dragover', { dataTransfer: transfer, clientY: 105 });
  await harness.act(async () => {});
  assert.ok(ok.defaultPrevented);
  await harness.act(async () => { harness.fireEvent(at('tk_b'), 'drop', { dataTransfer: transfer, clientY: 105 }); });
  await settle();
  assert.deepEqual(api.calls, [['move', 'tk_d', { before: 'tk_b', stage: 'next' }]]);
});

test('Lanes, grouped: an idea dropped into Next at a slot of its own group is put there; at another group\'s slot it keeps its place', async (t) => {
  const { harness, api, card, cell, settle } = await mount(t, 'rd-slotted', { prefs: { readiness: '["next"]' } });
  const transfer = { setData() {}, effectAllowed: '', dropEffect: '', types: ['application/x-tclaude-task'] };
  const at = (id) => { const el = card(id); el.getBoundingClientRect = () => ({ top: 100, height: 20 }); return el; };
  const drop = async (id, on) => {
    await harness.act(async () => { harness.fireEvent(card(id), 'dragstart', { dataTransfer: transfer }); });
    harness.fireEvent(at(on), 'dragover', { dataTransfer: transfer, clientY: 105 });
    await harness.act(async () => {});
    await harness.act(async () => { harness.fireEvent(at(on), 'drop', { dataTransfer: transfer, clientY: 105 }); });
    await settle();
  };
  // The ready idea onto charlie (ready): promoted, then put before charlie.
  await drop('tk_i2', 'tk_c');
  assert.deepEqual(api.calls, [['stage', 'tk_i2', 'next'], ['move', 'tk_i2', { before: 'tk_c', stage: 'next' }]]);
  // The queued idea onto alpha (ready): promoted only.
  api.calls.length = 0;
  await drop('tk_i1', 'tk_a');
  assert.deepEqual(api.calls, [['stage', 'tk_i1', 'next']]);
  assert.ok(cell('next'));
});

test('Lanes triage and the Backlog column group too', async (t) => {
  const { c, ids, heads, colMenu, menuItems, click } = await mount(t, 'rd-triage', { by: 'triage', prefs: { readiness: '["ideas"]', backlog: '1' } });
  const ideas = c.querySelector('.tb-cell[data-board-row="unassigned"][data-column="ideas"]');
  assert.deepEqual(ids(ideas), ['tk_i2', 'tk_i1']);
  assert.deepEqual(heads(ideas), ['Ready 1', 'Queued 1']);

  const backlog = c.querySelector('[data-tb-backlog-column]');
  assert.deepEqual(ids(backlog), ['tk_p1', 'tk_p2']);
  await click(backlog.querySelector('.tb-col-more'));
  await click(menuItems().find((b) => b.textContent.trim() === 'Group by readiness'));
  assert.deepEqual(ids(c.querySelector('[data-tb-backlog-column]')), ['tk_p2', 'tk_p1']);
  assert.ok(c.querySelector('[data-tb-backlog-column] [data-tb-ready="backlog"]'));

  await colMenu('Ideas');
  assert.ok(menuItems().some((b) => b.textContent.trim() === 'Show in board order'), 'a grouped column offers its order back');
});

test('List: the Filter\'s group by readiness groups each section under heading rows; a row moves within its group', async (t) => {
  const { harness, c, api, click, settle } = await mount(t, 'rd-list', { view: 'list' });
  const rowIds = () => [...c.querySelectorAll('tr[data-task-id]')].map((x) => x.dataset.taskId);
  assert.deepEqual(rowIds(), ['tk_a', 'tk_b', 'tk_c', 'tk_d', 'tk_e', 'tk_i1', 'tk_i2']);
  await click(c.querySelector('.tb-list-filter'));
  const box = c.querySelector('#tb-filter-rd-list-by-readiness');
  assert.ok(box, 'a Filter entry');
  box.checked = true;
  await harness.act(() => harness.fireEvent(box, 'change'));
  assert.deepEqual(rowIds(), ['tk_a', 'tk_c', 'tk_e', 'tk_b', 'tk_d', 'tk_i2', 'tk_i1'], 'the queue and the ideas, each grouped');
  assert.deepEqual([...c.querySelectorAll('tr.tb-ready-row')].map((x) => x.textContent.replace(/\s+/g, ' ').trim()),
    ['Ready 3', 'Queued 2', 'Ready 1', 'Queued 1']);
  assert.deepEqual(api.calls, []);

  // Alt+↑ on delta goes before bravo, its group's first.
  const rowOf = (id) => c.querySelector(`tr[data-task-id="${id}"]`);
  await harness.act(async () => { harness.fireEvent(rowOf('tk_d'), 'keydown', { key: 'ArrowUp', altKey: true }); });
  await settle();
  assert.deepEqual(api.calls, [['move', 'tk_d', { before: 'tk_b', stage: 'next' }]]);

  // A drag of bravo onto echo (ready) takes no drop.
  api.calls.length = 0;
  const transfer = { setData() {}, setDragImage() {}, effectAllowed: '', dropEffect: '' };
  await harness.act(async () => { harness.fireEvent(rowOf('tk_b').querySelector('.tb-handle'), 'dragstart', { dataTransfer: transfer }); });
  const over = harness.fireEvent(rowOf('tk_e'), 'dragover', { dataTransfer: transfer, clientY: 10 });
  assert.equal(over.defaultPrevented, false);
  await harness.act(async () => { harness.fireEvent(rowOf('tk_e'), 'drop', { dataTransfer: transfer, clientY: 10 }); });
  await settle();
  assert.deepEqual(api.calls, []);
});
