import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The Task board's Backlog (groups-taskboard-backlog.js, tk_5d2547b0):
// the toolbar chip shows or hides it in one click, remembered; parked rows
// stay out of the list until shown; ⋯ parks and brings back; the chip takes
// a dropped card; Drop… offers to park instead; and the Lanes' drops in and
// out of the Backlog column map to the CLI's verbs.

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });

function board(group, { backlog = true } = {}) {
  const counts = { live: 2, needs_you: 0, next: 1, ideas: 1, done_this_week: 0 };
  if (backlog) counts.backlog = 2;
  return {
    group, group_id: 1, counts,
    tasks: [
      row('tk_00000001', 'queued'),
      row('tk_00000002', 'an idea', { stage: 'idea' }),
      ...(backlog ? [row('tk_00000003', 'parked one', { stage: 'backlog' }), row('tk_00000004', 'parked two', { stage: 'backlog' })] : []),
    ],
    closed: [],
  };
}

function fakeApi() {
  const calls = [];
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return {}; };
  return { calls, edit: record('edit'), move: record('move'), stage: record('stage'), status: record('status'),
    assign: record('assign'), unassign: record('unassign'), reopen: record('reopen'), archive: record('archive') };
}

async function mount(t, name, { view = 'list', backlog = true } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, view);
  const api = fakeApi();
  const source = {
    context: harness.signals.signal({ groups: [board(name, { backlog })] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {},
  };
  const group = { name, members: [{ agent_id: 'agt_builder', title: 'builder', online: true }] };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const settle = () => new Promise((r) => setTimeout(r, 0));
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await settle(); });
  // The section starts folded unless something needs the operator: open it.
  const head = c.querySelector('[data-fused-taskboard] .gf-section-toggle, [data-fused-taskboard] summary, [data-fused-taskboard] [aria-expanded="false"]');
  if (head && !c.querySelector('.tb-toolbar')) await click(head);
  const chip = () => c.querySelector('.tb-backlog-chip');
  const rowOf = (id) => c.querySelector(`tr[data-task-id="${id}"]`);
  const menuItems = () => [...c.querySelectorAll('.gr-add-menu [role="menuitem"]')];
  const pick = async (id, label) => {
    await click(rowOf(id).querySelector('.tb-act-more'));
    const item = menuItems().find((b) => b.textContent.trim() === label);
    assert.ok(item, `menu item ${label}: ${menuItems().map((b) => b.textContent.trim()).join(', ')}`);
    await click(item);
  };
  return { harness, c, api, chip, rowOf, click, pick, dashPrefs, settle };
}

test('the chip shows and hides the backlog in one click, remembered; parked rows stay out until shown', async (t) => {
  const { c, chip, rowOf, click, dashPrefs } = await mount(t, 'tbb-toggle');
  assert.ok(chip(), 'the chip is in the toolbar');
  assert.ok(chip().closest('.tb-toolbar'), 'beside the view switch, not in a menu');
  assert.match(chip().textContent, /Backlog\s*2\s*▸/);
  assert.equal(chip().getAttribute('aria-pressed'), 'false');
  assert.ok(rowOf('tk_00000001'), 'a queued row shows');
  assert.equal(rowOf('tk_00000003'), null, 'a parked row is hidden by default');
  assert.equal(c.querySelector('[data-tb-backlog-band]'), null);

  await click(chip());
  assert.equal(chip().getAttribute('aria-pressed'), 'true');
  assert.match(chip().textContent, /▾/);
  assert.ok(c.querySelector('[data-tb-backlog-band]'), 'the Backlog band heads the parked rows');
  assert.ok(rowOf('tk_00000003') && rowOf('tk_00000004'));
  assert.equal(rowOf('tk_00000003').querySelector('.tb-state').textContent.trim(), 'backlog');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.backlog.tbb-toggle'), '1', 'remembered per viewer and group');

  await click(chip());
  assert.equal(rowOf('tk_00000003'), null);
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.taskboard.backlog.tbb-toggle'), null);
});

test('⋯ Move to backlog parks a task, and Back to ideas brings a parked one back', async (t) => {
  const { api, chip, click, pick, rowOf } = await mount(t, 'tbb-menu');
  await pick('tk_00000001', 'Move to backlog');
  assert.deepEqual(api.calls.at(-1), ['stage', 'tk_00000001', 'backlog']);
  await click(chip());
  await pick('tk_00000003', 'Back to ideas');
  assert.deepEqual(api.calls.at(-1), ['stage', 'tk_00000003', 'idea']);
  // The row's first action on a parked task is ↩ Back to ideas.
  const back = [...rowOf('tk_00000004').querySelectorAll('.tb-row-actions > button')].find((b) => b.getAttribute('aria-label') === 'Back to ideas');
  assert.ok(back);
  await click(back);
  assert.deepEqual(api.calls.at(-1), ['stage', 'tk_00000004', 'idea']);
});

test('a card dropped on the folded chip is parked', async (t) => {
  const { harness, api, chip, settle } = await mount(t, 'tbb-drop');
  const types = ['application/x-tclaude-task'];
  const dataTransfer = { types, getData: (type) => (type === types[0] ? 'tk_00000002' : ''), dropEffect: '' };
  await harness.act(async () => { harness.fireEvent(chip(), 'dragover', { dataTransfer }); await settle(); });
  assert.match(chip().textContent, /Park in backlog/, 'it says what the drop does');
  await harness.act(async () => { harness.fireEvent(chip(), 'drop', { dataTransfer }); await settle(); });
  assert.deepEqual(api.calls, [['stage', 'tk_00000002', 'backlog']]);
  assert.equal(chip().getAttribute('aria-pressed'), 'false', 'the backlog stays folded');
});

test('Drop… offers to park the task instead', async (t) => {
  const { harness, api, pick, click } = await mount(t, 'tbb-dropdialog');
  await pick('tk_00000001', 'Drop…');
  const park = harness.document.querySelector('#taskboard-reason-park');
  assert.ok(park, 'the gentler way is offered');
  await click(park);
  assert.deepEqual(api.calls, [['stage', 'tk_00000001', 'backlog']]);
  assert.equal(harness.document.querySelector('#taskboard-reason-modal'), null);
});

test('a board whose node predates the backlog draws no chip and offers no park', async (t) => {
  const { c, pick, harness } = await mount(t, 'tbb-old', { backlog: false });
  assert.equal(c.querySelector('.tb-backlog-chip'), null);
  await pick('tk_00000001', 'Drop…');
  assert.equal(harness.document.querySelector('#taskboard-reason-park'), null);
});

test('Lanes: the Backlog column draws when shown, and its drops map to the CLI verbs', async (t) => {
  const { c, chip, click } = await mount(t, 'tbb-lanes', { view: 'lanes' });
  assert.equal(c.querySelector('[data-tb-backlog-column]'), null, 'hidden by default');
  assert.equal(c.querySelector('.tb-card[data-task-id="tk_00000003"]'), null, 'a parked card is in no lane');
  await click(chip());
  const column = c.querySelector('[data-tb-backlog-column]');
  assert.ok(column);
  assert.deepEqual([...column.querySelectorAll('.tb-card')].map((el) => el.dataset.taskId), ['tk_00000003', 'tk_00000004']);

  const harness = await createPreactHarness(t);
  const { backlogDrop } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  const run = async (drop) => { const calls = []; const rec = (verb) => async (...a) => { calls.push([verb, ...a]); }; await drop.run({ stage: rec('stage'), hold: rec('hold'), assign: rec('assign'), unassign: rec('unassign'), start: rec('start'), status: rec('status') }); return calls; };
  const open = row('tk_a', 'a');
  const parked = row('tk_p', 'p', { stage: 'backlog' });
  const held = row('tk_h', 'h', { stage: 'backlog', assignee: { agent_id: 'agt_b' }, held: true });
  // In: parked, whoever holds it.
  assert.deepEqual(await run(backlogDrop(open, { lane: '', column: 'next' }, { lane: '', column: 'backlog' })), [['stage', 'tk_a', 'backlog']]);
  assert.equal(backlogDrop(parked, { lane: '', column: 'backlog' }, { lane: '', column: 'backlog' }), null);
  assert.equal(backlogDrop(row('tk_e', 'e', { kind: 'epic' }), { lane: '', column: 'ideas' }, { lane: '', column: 'backlog' }), null, 'an epic is not parked');
  // Out: back to a stage, held by its agent, given to another, or unassigned.
  assert.deepEqual(await run(backlogDrop(parked, { lane: '', column: 'backlog' }, { lane: '', column: 'ideas' })), [['stage', 'tk_p', 'idea']]);
  assert.deepEqual(await run(backlogDrop(held, { lane: 'agt_b', column: 'backlog' }, { lane: 'agt_b', column: 'next' })), [['hold', 'tk_h', 'next']]);
  assert.deepEqual(await run(backlogDrop(held, { lane: 'agt_b', column: 'backlog' }, { lane: 'agt_b', column: 'progress' })), [['start', 'tk_h']]);
  assert.deepEqual(await run(backlogDrop(held, { lane: 'agt_b', column: 'backlog' }, { lane: '', column: 'ideas' })), [['unassign', 'tk_h'], ['stage', 'tk_h', 'idea']]);
  assert.deepEqual(await run(backlogDrop(parked, { lane: '', column: 'backlog' }, { lane: 'agt_c', name: 'c', column: 'ideas' })), [['assign', 'tk_p', 'agt_c', { hold: 'idea' }]]);
  assert.equal(backlogDrop(parked, { lane: '', column: 'backlog' }, { lane: 'agt_c', column: 'ideas' }, { assign: false }), null, 'a lane change needs the assign right');
  assert.equal(backlogDrop(held, { lane: 'agt_b', column: 'backlog' }, { lane: 'agt_b', column: 'next' }, { assign: false }), null, 'holding another\'s task needs the assign right');
});

test('✎ Edit keeps a parked task parked, and moves one in or out by its Where', async (t) => {
  const harness = await createPreactHarness(t);
  const { editPlan, draftOf, setWhere } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const names = (task, draft) => editPlan(task, draft).map((s) => s.name);
  const parked = row('tk_p', 'p', { stage: 'backlog' });
  const held = row('tk_h', 'h', { stage: 'backlog', assignee: { agent_id: 'agt_b' }, held: true });
  assert.equal(draftOf(parked).where, 'backlog');
  assert.deepEqual(names(parked, { ...draftOf(parked), title: 'renamed' }), ['title']);
  assert.deepEqual(names(held, { ...draftOf(held), title: 'renamed' }), ['title']);
  assert.deepEqual(names(parked, setWhere(draftOf(parked), 'idea')), ['back to the ideas']);
  const open = row('tk_o', 'o');
  assert.deepEqual(names(open, setWhere(draftOf(open), 'backlog')), ['into the backlog']);
});

test('List: with the backlog hidden and only parked tasks live, the history still shows', async (t) => {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.view.tbb-hist', 'list');
  const entry = {
    group: 'tbb-hist', group_id: 1, counts: { live: 0, needs_you: 0, next: 0, ideas: 0, done_this_week: 1, backlog: 1 },
    tasks: [row('tk_00000003', 'parked one', { stage: 'backlog' })],
    closed: [row('tk_00000009', 'shipped it', { status: 'done', closed_at: ago(1) })],
  };
  const source = { context: harness.signals.signal({ groups: [entry] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name: 'tbb-hist', members: [] };
  const { container: c } = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${fakeApi()} />`);
  const head = c.querySelector('[data-fused-taskboard] .gf-section-toggle, [data-fused-taskboard] summary, [data-fused-taskboard] [aria-expanded="false"]');
  if (head && !c.querySelector('.tb-toolbar')) await harness.act(async () => { harness.fireEvent(head, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(c.querySelector('tr[data-task-id="tk_00000003"]'), null);
  const history = c.querySelector('.tb-history');
  assert.ok(history, 'the history line is reachable');
  assert.equal(history.closest('tr'), null, 'drawn as a line, not a row of a table that is not there');
});
