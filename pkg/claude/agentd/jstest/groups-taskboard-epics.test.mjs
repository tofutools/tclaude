// Epics on the Task board (groups-taskboard-epics.js, tk_0a821784): the
// strip of open epics, the epic chip on their tasks, the Epic filter, the
// "close epic?" nudge, New epic, and the epic in the edit form.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], ...extra });
const progress = (extra = {}) => ({ total: 0, done: 0, dropped: 0, blocked: 0, in_progress: 0, in_column: 0, next: 0, ideas: 0, ...extra });

const EPIC = row('tk_e0000001', 'Checkout revamp', { kind: 'epic', progress: progress({ total: 3, done: 1, next: 1, in_progress: 1 }) });
const DONE_EPIC = row('tk_e0000002', 'Search v2', { kind: 'epic', progress: progress({ total: 2, done: 2 }), close_suggested_at: '2026-11-09T10:00:00Z' });
const IDEA_EPIC = row('tk_e0000003', 'Personalisation', { kind: 'epic', stage: 'idea', progress: progress() });

function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 6, next: 3, ideas: 2 },
    tasks: [
      EPIC, DONE_EPIC, IDEA_EPIC,
      row('tk_00000001', 'Saved carts', { parent_id: EPIC.id }),
      row('tk_00000002', 'Gift cards', { stage: 'idea' }),
      row('tk_00000003', 'Plan the release'),
    ],
    closed: [],
  };
}

function fakeApi({ children = [] } = {}) {
  const calls = [];
  const answer = (verb, value = () => ({})) => async (...args) => { calls.push([verb, ...args]); return value(...args); };
  return {
    calls,
    create: answer('create', (group, task) => ({ id: 'tk_00000009', title: task.title })),
    edit: answer('edit', (id) => ({ id })),
    status: answer('status'),
    task: async (id) => (id === EPIC.id ? { ...EPIC, body: 'Make checkout fast.', children } : { ...board('x').tasks.find((t) => t.id === id), body: '' }),
    activity: async () => ({ entries: [] }),
    move: answer('move'), stage: answer('stage'), assign: answer('assign'), unassign: answer('unassign'),
  };
}

async function mountBoard(t, name, { api = fakeApi(), entry = board(name), view = 'lanes', prefs = {} } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, view);
  for (const [key, value] of Object.entries(prefs)) dashPrefs.setItem(`tclaude.dash.groups.taskboard.${key}.${name}`, value);
  const source = {
    context: harness.signals.signal({ groups: [entry] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {},
  };
  const group = { name, members: [{ agent_id: 'agt_dev', title: 'dev', online: true }] };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 10)));
  const click = async (el) => { await harness.act(async () => { harness.fireEvent(el, 'click'); }); await settle(); };
  return { harness, c, api, click, settle, dashPrefs };
}

const cardOf = (c, id) => c.querySelector(`.tb-card[data-task-id="${id}"]`);

test('open epics are a strip above the board, not cards in its columns; an idea epic stays in Ideas', async (t) => {
  const { c } = await mountBoard(t, 'ep-strip');
  const strip = c.querySelector('.tb-epic-strip');
  assert.ok(strip, 'the strip shows');
  assert.deepEqual([...strip.querySelectorAll('[data-epic]')].map((el) => el.getAttribute('data-epic')), [EPIC.id, DONE_EPIC.id]);
  assert.match(strip.querySelector(`[data-epic="${EPIC.id}"]`).textContent, /1\/3 done · 1 in progress · 1 next/);
  assert.equal(cardOf(c, EPIC.id), null, 'an open epic is no card in a column');
  const idea = cardOf(c, IDEA_EPIC.id);
  assert.ok(idea, 'an idea epic is a card in Ideas');
  assert.match(idea.textContent, /◆ epic/);
  const child = cardOf(c, 'tk_00000001');
  assert.equal(child.querySelector('.tb-epic-chip').textContent, 'Checkout revamp', 'its task carries the epic\'s chip');
  assert.equal(cardOf(c, 'tk_00000003').querySelector('.tb-epic-chip'), null, 'a task in no epic has none');
});

test('a chip filters the board to its epic: the others dim, or hide', async (t) => {
  const { harness, c, click, dashPrefs } = await mountBoard(t, 'ep-filter');
  await click(cardOf(c, 'tk_00000001').querySelector('.tb-epic-chip'));
  assert.ok(!cardOf(c, 'tk_00000001').classList.contains('tb-epic-dim'), 'its tasks stay');
  assert.ok(cardOf(c, 'tk_00000003').classList.contains('tb-epic-dim'), 'the others dim');
  assert.match(c.querySelector('.tb-epic-filter-btn').textContent, /Epic: Checkout revamp/);
  assert.match(dashPrefs.getItem('tclaude.dash.groups.taskboard.epic-filter.ep-filter'), /tk_e0000001/, 'kept for this viewer');
  // Hide: only its tasks are drawn.
  await click(c.querySelector('.tb-epic-filter-btn'));
  const hide = [...c.querySelectorAll('input[type="radio"]')].find((el) => el.parentElement.textContent.trim() === 'hide');
  hide.checked = true;
  await harness.act(async () => { harness.fireEvent(hide, 'change'); });
  assert.equal(cardOf(c, 'tk_00000003'), null, 'the others are hidden');
  assert.ok(cardOf(c, 'tk_00000001'));
  // The chip again shows every task.
  await click(cardOf(c, 'tk_00000001').querySelector('.tb-epic-chip'));
  assert.ok(cardOf(c, 'tk_00000003'), 'every task again');
});

test('an epic whose tasks are all closed asks to close it; Not yet puts it off', async (t) => {
  const first = await mountBoard(t, 'ep-nudge');
  const card = first.c.querySelector(`[data-epic="${DONE_EPIC.id}"]`);
  assert.ok(card.querySelector('[data-epic-nudge]'), 'the nudge shows');
  assert.equal(first.c.querySelector(`[data-epic="${EPIC.id}"] [data-epic-nudge]`), null, 'not on an epic with open tasks');
  await first.click(card.querySelector('.tb-epic-close'));
  assert.deepEqual(first.api.calls.find((call) => call[0] === 'status'), ['status', DONE_EPIC.id, 'done'], 'Close epic closes it, never on its own');
  await first.click(first.c.querySelector(`[data-epic="${DONE_EPIC.id}"] .tb-epic-later`));
  assert.equal(first.c.querySelector(`[data-epic="${DONE_EPIC.id}"] [data-epic-nudge]`), null, 'Not yet hides it');
  assert.equal(first.dashPrefs.getItem(`tclaude.dash.groups.taskboard.epic-nudge.${DONE_EPIC.id}`), DONE_EPIC.close_suggested_at,
    'until another of its tasks changes');
});

test('New epic adds an epic, with no assignee or dependencies', async (t) => {
  const { harness, c, api, click } = await mountBoard(t, 'ep-new');
  await click(c.querySelector('[data-taskboard-new]'));
  await click(c.querySelector('[data-taskboard-new-epic]'));
  const dialog = c.querySelector('#taskboard-new-modal');
  assert.match(dialog.textContent, /New epic · ep-new/);
  assert.equal(dialog.querySelector('.tb-new-assignee'), null);
  assert.equal(dialog.querySelector('.tb-deps-picker'), null);
  assert.equal(dialog.querySelector('.tb-new-epic'), null, 'an epic is in no epic');
  await harness.input(dialog.querySelector('.tb-new-title'), 'Payments hardening');
  await click(dialog.querySelector('#taskboard-new-submit'));
  assert.deepEqual(api.calls.find((call) => call[0] === 'create'), ['create', 'ep-new',
    { title: 'Payments hardening', body: '', ref: '', resolveRef: false, kind: 'epic', stage: 'idea', status: '', to: '' }]);
});

test('an epic opens in the edit form with its progress and tasks; tasks are added and taken out at once', async (t) => {
  const children = [row('tk_00000001', 'Saved carts', { parent_id: EPIC.id, assignee: { agent_id: 'agt_dev', name: 'dev' } }),
    row('tk_00000004', 'Fix the badge', { parent_id: EPIC.id, status: 'done' })];
  const { harness, c, api, click, settle } = await mountBoard(t, 'ep-edit', { api: fakeApi({ children }) });
  await click(c.querySelector(`[data-epic="${EPIC.id}"] .tb-epic-name`));
  await settle();
  const dialog = c.querySelector('#taskboard-edit-modal');
  assert.ok(dialog, 'the epic opens in the edit form');
  assert.match(dialog.textContent, /Edit epic/);
  assert.match(dialog.textContent, /1\/3 done/);
  assert.equal(dialog.querySelector('.tb-edit-assignee'), null, 'an epic has no assignee');
  assert.equal(dialog.querySelector('.tb-edit-epic'), null, 'nor an epic of its own');
  assert.equal(dialog.querySelector('[data-stage-pick="taskboard-edit-where"]'), null, 'an open epic is no stage work: no stage picker');
  assert.ok(dialog.querySelector('.tb-edit-epic-status option[value="open"]'), 'a Status select stands in for the stage picker');
  assert.equal(dialog.querySelector('.tb-edit-epic-promote'), null, 'it is promoted already');
  const list = dialog.querySelector('[data-epic-children]');
  assert.deepEqual([...list.querySelectorAll('[data-epic-child]')].map((el) => el.getAttribute('data-epic-child')), ['tk_00000001', 'tk_00000004']);
  assert.match(list.querySelector('[data-epic-child="tk_00000001"]').textContent, /In progress.*dev/);
  assert.match(list.querySelector('[data-epic-child="tk_00000004"]').textContent, /Done/);

  await harness.input(list.querySelector('.tb-epic-add-title'), 'Address autocomplete');
  await click(list.querySelector('.tb-epic-add-new'));
  assert.deepEqual(api.calls.find((call) => call[0] === 'create'), ['create', 'ep-edit', { title: 'Address autocomplete', parent: EPIC.id }]);

  // The board's tasks in no epic (not epics) can be added.
  const pick = list.querySelector('.tb-epic-add-pick');
  assert.deepEqual([...pick.querySelectorAll('option')].map((o) => o.value), ['', 'tk_00000002', 'tk_00000003']);
  for (const option of pick.querySelectorAll('option')) { option.removeAttribute('selected'); option.selected = false; }
  const chosen = pick.querySelector('option[value="tk_00000003"]');
  chosen.setAttribute('selected', '');
  chosen.selected = true;
  await harness.act(() => harness.fireEvent(pick, 'change'));
  await click(list.querySelector('.tb-epic-add-existing'));
  assert.deepEqual(api.calls.find((call) => call[0] === 'edit'), ['edit', 'tk_00000003', { parent: EPIC.id }]);

  await click(list.querySelector('[data-epic-child="tk_00000001"] .tb-epic-child-rm'));
  assert.deepEqual(api.calls.filter((call) => call[0] === 'edit').at(-1), ['edit', 'tk_00000001', { clearParent: true }]);
});

test('the edit plan moves a task between epics, and closes an epic with open tasks by force', async (t) => {
  const harness = await createPreactHarness(t);
  const { editPlan, draftOf } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const task = row('tk_00000003', 'Plan the release');
  const calls = [];
  const api = { edit: async (...args) => calls.push(['edit', ...args]), status: async (...args) => calls.push(['status', ...args]) };
  for (const step of editPlan(task, { ...draftOf(task), parent: EPIC.id })) await step.run(api);
  assert.deepEqual(calls, [['edit', 'tk_00000003', { parent: EPIC.id }]]);
  calls.length = 0;
  const inEpic = { ...task, parent_id: EPIC.id };
  for (const step of editPlan(inEpic, { ...draftOf(inEpic), parent: '' })) await step.run(api);
  assert.deepEqual(calls, [['edit', 'tk_00000003', { clearParent: true }]]);
  calls.length = 0;
  for (const step of editPlan(EPIC, { ...draftOf(EPIC), where: 'done' })) await step.run(api);
  assert.deepEqual(calls, [['status', EPIC.id, 'done', '', { force: true }]]);
});

test('boardEntry and the filter, and an epic\'s colour', async (t) => {
  const harness = await createPreactHarness(t);
  const { boardEntry, epicColour, epicMatches, NO_EPIC } = await harness.importDashboardModule('js/groups-taskboard-epics.js');
  const entry = board('x');
  assert.deepEqual(boardEntry(entry).tasks.map((task) => task.id), [IDEA_EPIC.id, 'tk_00000001', 'tk_00000002', 'tk_00000003']);
  assert.deepEqual(boardEntry(entry, { epic: EPIC.id, hide: true }).tasks.map((task) => task.id), ['tk_00000001']);
  assert.deepEqual(boardEntry(entry, { epic: NO_EPIC, hide: true }).tasks.map((task) => task.id), ['tk_00000002', 'tk_00000003']);
  assert.ok(epicMatches(row('a', 'a', { parent_id: EPIC.id }), EPIC.id));
  assert.ok(!epicMatches(row('a', 'a'), EPIC.id));
  assert.equal(epicColour(EPIC.id), epicColour(EPIC.id), 'the same every time');
  assert.ok(epicColour(EPIC.id) >= 1 && epicColour(EPIC.id) <= 8);
});

test('an idea epic is promoted to an open epic, not moved to a stage', async (t) => {
  const { harness, c, api, click, settle } = await mountBoard(t, 'ep-promote', { view: 'list' });
  await click(c.querySelector(`tr[data-task-id="${IDEA_EPIC.id}"] .tb-title`));
  await settle();
  const dialog = c.querySelector('#taskboard-edit-modal');
  assert.match(dialog.textContent, /Edit epic/);
  assert.equal(dialog.querySelector('[data-stage-pick="taskboard-edit-where"]'), null);
  const promote = dialog.querySelector('.tb-edit-epic-promote');
  assert.ok(promote, 'an idea epic offers Promote to open epic');
  promote.checked = true;
  await harness.act(async () => { harness.fireEvent(promote, 'change'); });
  await click(dialog.querySelector('#taskboard-edit-save'));
  assert.deepEqual(api.calls.find((call) => call[0] === 'stage'), ['stage', IDEA_EPIC.id, 'next']);
});

test('an epic is no work of its own: no assign, block or column in its menu or its drags, and no "+ more" for the strip', async (t) => {
  const { harness, c, click } = await mountBoard(t, 'ep-work', { view: 'list' });
  assert.doesNotMatch(c.textContent, /more · tclaude agent taskboard ls/, 'the strip\'s epics are on the board, not past its cap');
  await click(c.querySelector(`tr[data-task-id="${IDEA_EPIC.id}"] .tb-act-more`));
  const keys = [...c.querySelectorAll('[data-task-menu]')].map((el) => el.getAttribute('data-task-menu'));
  for (const key of ['assign', 'block', 'move-to', 'start', 'hold']) assert.ok(!keys.includes(key), `no ${key} on an epic: ${keys}`);
  await harness.act(async () => { harness.fireEvent(globalThis.document, 'keydown', { key: 'Escape' }); });
  await click(c.querySelector('tr[data-task-id="tk_00000002"] .tb-act-more'));
  assert.ok([...c.querySelectorAll('[data-task-menu]')].some((el) => el.getAttribute('data-task-menu') === 'assign'), 'a task still has Assign');

  const { laneDrop } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  assert.equal(laneDrop(IDEA_EPIC, { lane: '', column: 'ideas' }, { lane: 'agt_dev', column: 'progress', member: true }), null, 'not onto an agent');
  assert.equal(laneDrop(IDEA_EPIC, { lane: '', column: 'ideas' }, { lane: '', column: 'blocked' }), null, 'not blocked');
  assert.ok(laneDrop(IDEA_EPIC, { lane: '', column: 'ideas' }, { lane: '', column: 'next' }), 'Ideas to Next still is a move');
});

test('a paste into the New epic title stays one epic', async (t) => {
  const harness = await createPreactHarness(t);
  const { newTaskPlan } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  const plan = newTaskPlan({ kind: 'epic', title: 'Checkout', list: 'a\nb', start: 'idea' });
  assert.notEqual(plan.kind, 'lines');
});

test('a task whose epic was closed with it open is in no epic', async (t) => {
  const harness = await createPreactHarness(t);
  const { boardEntry, epicCandidates, NO_EPIC } = await harness.importDashboardModule('js/groups-taskboard-epics.js');
  const entry = board('x');
  entry.tasks.push(row('tk_00000004', 'Left open', { parent_id: 'tk_eclosed1' }));
  assert.ok(boardEntry(entry, { epic: NO_EPIC, hide: true }).tasks.some((task) => task.id === 'tk_00000004'));
  assert.ok(epicCandidates(entry.tasks).some((task) => task.id === 'tk_00000004'));
});

test('lanes by epic: a lane per open epic and No epic; a drag to another epic\'s lane moves the task there, with Undo', async (t) => {
  const { harness, c, api, settle } = await mountBoard(t, 'ep-lanes', { prefs: { 'lanes-by': 'epic' } });
  const heads = [...c.querySelectorAll('[data-board-row-head]')].map((el) => el.getAttribute('data-board-row-head'));
  assert.deepEqual(heads, [EPIC.id, DONE_EPIC.id, 'none'], 'the strip\'s epics in order, then No epic');
  assert.equal(c.querySelector('.tb-epic-strip'), null, 'the lane heads are the epics: no strip above');
  const cell = (row, column) => c.querySelector(`.tb-cell[data-board-row="${row}"][data-column="${column}"]`);
  assert.ok(cell(EPIC.id, 'next').querySelector('.tb-card[data-task-id="tk_00000001"]'), 'an epic\'s task is in its lane');
  assert.equal(cell(EPIC.id, 'next').querySelector('.tb-epic-chip'), null, 'the lane says the epic: no chip');
  assert.ok(cell('none', 'ideas').querySelector(`.tb-card[data-task-id="${IDEA_EPIC.id}"]`), 'an idea epic is a card in No epic\'s Ideas');
  assert.match(c.querySelector(`[data-epic-lane="${EPIC.id}"]`).textContent, /Checkout revamp.*1\/3 done/);

  const transfer = { setData() {}, effectAllowed: '', dropEffect: '' };
  const card = (id) => c.querySelector(`.tb-card[data-task-id="${id}"]`);
  const drag = async (id, row, column) => {
    await harness.act(async () => { harness.fireEvent(card(id), 'dragstart', { dataTransfer: transfer }); });
    const over = harness.fireEvent(cell(row, column), 'dragover', { dataTransfer: transfer });
    await harness.act(async () => {});
    await harness.act(async () => { harness.fireEvent(cell(row, column), 'drop', { dataTransfer: transfer }); await new Promise((r) => setTimeout(r, 0)); });
    await settle();
    return over.defaultPrevented;
  };
  assert.equal(await drag(IDEA_EPIC.id, EPIC.id, 'ideas'), false, 'an epic goes in no epic');
  assert.equal(await drag('tk_00000003', EPIC.id, 'next'), true);
  assert.deepEqual(api.calls.filter((call) => call[0] === 'edit'), [['edit', 'tk_00000003', { parent: EPIC.id }]]);
  const { shellState } = await harness.importDashboardModule('js/shell-state.js');
  const toast = shellState.toast.value;
  assert.match(toast.message, /to Checkout revamp/);
  assert.equal(toast.action?.label, 'Undo');
  await harness.act(async () => { await toast.action.run(); });
  assert.deepEqual(api.calls.filter((call) => call[0] === 'edit').at(-1), ['edit', 'tk_00000003', { clearParent: true }], 'Undo puts it back in no epic');

  api.calls.length = 0;
  assert.equal(await drag('tk_00000001', 'none', 'next'), true);
  assert.deepEqual(api.calls.filter((call) => call[0] === 'edit'), [['edit', 'tk_00000001', { clearParent: true }]], 'onto No epic takes it out');
});

test('epicDrop: another epic\'s row moves the task there first, then the column drop', async (t) => {
  const harness = await createPreactHarness(t);
  const { epicDrop } = await harness.importDashboardModule('js/groups-taskboard-lanes.js');
  const task = row('tk_00000005', 'x', { parent_id: EPIC.id });
  const from = { task, lane: '', column: 'next', epic: EPIC.id };
  assert.equal(epicDrop(from, { lane: '', column: 'next', epic: EPIC.id }, null), null, 'the same row is the lane drop');
  const calls = [];
  const api = { edit: async (...a) => calls.push(['edit', ...a]), stage: async (...a) => calls.push(['stage', ...a]) };
  const both = epicDrop(from, { lane: '', column: 'ideas', epic: DONE_EPIC.id, epicName: 'Search v2' }, { kind: 'stage', label: 'to Ideas', run: (a) => a.stage(task.id, 'idea') });
  assert.equal(both.kind, 'epic+stage');
  await both.run(api);
  assert.deepEqual(calls, [['edit', task.id, { parent: DONE_EPIC.id }], ['stage', task.id, 'idea']]);
  assert.equal(epicDrop(from, { lane: 'agt_x', column: 'progress', epic: '' }, null), null, 'a refused column drop is no epic move either');
});

test('the List groups the queue by epic, each group folding', async (t) => {
  const { c, click } = await mountBoard(t, 'ep-list', { view: 'list', prefs: { 'list-by-epic': '1' } });
  const order = () => [...c.querySelectorAll('.tb-table tbody tr')].map((tr) => tr.getAttribute('data-epic-group') || tr.getAttribute('data-task-id')).filter(Boolean);
  const before = order();
  assert.deepEqual(before.slice(0, 5), [EPIC.id, 'tk_00000001', DONE_EPIC.id, 'none', 'tk_00000003']);
  assert.equal(c.querySelector('tr[data-task-id="tk_00000001"] .tb-epic-chip'), null, 'its group says the epic: no chip');
  await click(c.querySelector(`tr[data-epic-group="${EPIC.id}"] .tb-epic-group-fold`));
  assert.ok(!order().includes('tk_00000001'), 'folded, its tasks hide');
});
