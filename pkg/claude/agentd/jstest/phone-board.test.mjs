// js/phone-board-model.js, phone-board.js and phone-task-page.js: the phone
// Board as the approved mock draws it (one column at a time, readiness
// sections, cards with tags, Move ▸ and Comment, questions answered in
// place) and a task's own phone page.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';
const LEAD = 'agt_00000000000000000000000000000002';

// A webshop board: one of each place a task can stand.
function board() {
  return {
    group: 'webshop',
    counts: { live: 8, waiting_on_you: 2 },
    columns: [{ slug: 'in-review', label: 'In review', count: 1 }],
    tasks: [
      { id: 'tk_ready', title: 'Checkout page redesign', status: 'open', stage: 'next', held: true,
        assignee: { agent_id: AGENT, name: 'checkout-ui' },
        created_by: `agent:${LEAD}`, created_by_name: 'lead', created_at: '2026-11-14T00:00:00Z',
        assigned_by: `agent:${LEAD}`, assigned_at: '2026-11-14T00:00:30Z',
        last_edited_by: 'human:operator', last_edited_by_name: 'operator', last_edited_at: '2026-11-14T00:01:00Z',
        refs: [{ kind: 'github', label: '#1482', url: 'https://github.com/acme/webshop/pull/1482', pr: { number: 1482, checks: { state: 'passing', total: 4, passed: 4 } } }] },
      { id: 'tk_coupon', title: 'Coupon lookup p95 regression', status: 'open', stage: 'next', kind: 'bug' },
      { id: 'tk_carrier', title: 'Which carrier?', status: 'open', stage: 'next',
        questions: [{ n: 1, title: 'Which carrier?', state: 'open', asked_by: `agent:${LEAD}`, recommend: 'dhl',
          options: [{ key: 'dhl', label: 'DHL' }, { key: 'ups', label: 'UPS' }, { key: 'fedex', label: 'FedEx' }] }] },
      { id: 'tk_queued', title: 'After the redesign', status: 'open', stage: 'next', unmet: ['tk_ready'] },
      { id: 'tk_stuck', title: 'Upgrade the SDK', status: 'blocked', stage: 'next', block_reason: 'waiting on the vendor' },
      { id: 'tk_doing', title: 'Search relevance v2', status: 'open', stage: 'next', assignee: { agent_id: LEAD, name: 'lead' } },
      { id: 'tk_mine', title: 'Approve the contract', status: 'open', stage: 'next', assignee: { agent_id: 'operator', operator: true } },
      { id: 'tk_review', title: 'Onboarding copy', status: 'open', stage: 'next', column: 'in-review' },
      { id: 'tk_idea', title: 'Gift cards', status: 'open', stage: 'idea' },
      { id: 'tk_parked', title: 'Loyalty points', status: 'open', stage: 'backlog' },
    ],
    closed: [
      { id: 'tk_old', title: 'Older fix', status: 'done', closed_at: '2026-11-10T00:00:00Z' },
      { id: 'tk_new', title: 'Newer fix', status: 'done', closed_at: '2026-11-14T00:00:00Z' },
      { id: 'tk_dropped', title: 'Dropped', status: 'dropped', closed_at: '2026-11-14T00:00:00Z' },
    ],
  };
}

test('the board is one column at a time, split into readiness sections', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/phone-board-model.js');
  const entry = board();
  assert.deepEqual(m.boardColumns(entry).map((c) => [c.label, c.count]),
    [['Idea', 1], ['Next', 6], ['Started', 1], ['In review', 1], ['Done', 2]], 'a backlog task and a dropped one are in none; one the operator holds stays in Next');
  assert.deepEqual(m.columnSections(entry, 'next').map((s) => [s.label, s.tasks.map((task) => task.id)]), [
    ['Ready', ['tk_ready', 'tk_coupon']],
    ['Waiting on you', ['tk_carrier', 'tk_mine']],
    ['Queued', ['tk_queued']],
    ['Blocked', ['tk_stuck']],
  ]);
  assert.deepEqual(m.columnSections(entry, 'idea').map((s) => s.label), ['Ready', 'Waiting on you', 'Blocked'], 'Queued only when it holds something');
  assert.deepEqual(m.columnSections(entry, 'done')[0].tasks.map((task) => task.id), ['tk_new', 'tk_old'], 'Done is the latest first');
  assert.equal(m.defaultColumn(entry), 'next', 'the board opens on the column with something waiting on you');
  assert.equal(m.defaultColumn({ tasks: [{ id: 'a', status: 'open', stage: 'idea' }] }), 'idea');
});

test('a card shows who holds it, its PR and kind, and a question with its options', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/phone-board-model.js');
  const [ready, coupon, carrier] = board().tasks;
  const names = new Map([[LEAD, { name: 'lead' }]]);
  assert.deepEqual(m.cardView(ready, names).tags.map((tag) => tag.text), ['@checkout-ui', 'PR #1482']);
  assert.deepEqual(m.cardView(coupon, names).tags.map((tag) => tag.text), ['bug']);
  const view = m.cardView(carrier, names);
  assert.equal(view.accent, 'q', 'waiting on you: the blue edge');
  assert.equal(view.question.asker, 'lead');
  assert.deepEqual(view.question.options.map((o) => [o.label, o.recommended]), [['DHL', true], ['UPS', false], ['FedEx', false]]);
  assert.equal(view.question.inline, true);
});

test('Move offers the columns a task can go to, and its place among its section', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/phone-board-model.js');
  const entry = board();
  const [ready, coupon] = entry.tasks;
  const moves = m.columnMoves(coupon, entry);
  assert.deepEqual(moves.map((c) => [c.key, c.current, c.disabled]), [
    ['idea', false, false], ['next', true, false], ['started', false, true], ['col:in-review', false, false], ['done', false, false],
  ], 'an unassigned task cannot start');
  assert.equal(m.columnMoves(ready, entry).find((c) => c.key === 'started').disabled, false, "its agent holds it: it can start");
  assert.deepEqual(m.positionMoves([ready, coupon], coupon), { top: { before: 'tk_ready', stage: 'next' }, up: { before: 'tk_ready', stage: 'next' }, down: null });
});

// withTask is a board with extra fields on its tk_ready.
function withTask(b, extra) {
  return { ...b, tasks: b.tasks.map((task) => (task.id === 'tk_ready' ? { ...task, ...extra } : task)) };
}

async function setupView(t, { actions = {}, activity = null, task = null, row = {} } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, boardMod, sheetMod, pageMod] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-board.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
    harness.importDashboardModule('js/phone-page.js'),
  ]);
  const { signal } = harness.signals;
  const state = createDashboardState();
  state.beginRequest();
  state.commitRequest(1, { groups: [{ name: 'webshop', members: [
    { agent_id: AGENT, conv_id: 'c1', title: 'checkout-ui', online: true, state: { status: 'working' } },
    { agent_id: LEAD, conv_id: 'c2', title: 'lead', online: true, state: { status: 'idle' } },
  ] }] });
  const writes = [];
  let refreshes = 0;
  const source = {
    attach: () => () => {}, status: signal('ready'), error: signal(''),
    context: signal({ groups: [withTask(board(), row)] }),
    refresh() { refreshes += 1; return Promise.resolve(); },
  };
  const record = (name) => async (...args) => { writes.push([name, ...args]); return {}; };
  const api = {
    answer: record('answer'), comment: record('comment'), stage: record('stage'), start: record('start'),
    hold: record('hold'), status: record('status'), move: record('move'), create: record('create'),
    task: task || (async (id) => ({ ...board().tasks.find((one) => one.id === id), body: 'The **address** step is done.' })),
    activity: activity || (async () => ({ events: [
      { id: 1, at: '2026-11-14T00:00:00Z', actor: 'human:operator', actor_name: 'operator', kind: 'created', detail: 'in next' },
      { id: 2, at: '2026-11-14T00:01:00Z', actor: `agent:${AGENT}`, actor_name: 'checkout-ui', kind: 'comment', comment: { body: 'PR is up.' } },
    ] })),
  };
  t.after(() => { boardMod.boardTask.value = null; sheetMod.closePhoneSheet(); pageMod.phonePage.value = null; pageMod.phoneBar.value = null; });
  const { html } = harness;
  const mounted = await harness.mount(html`<div>
    <${boardMod.BoardView} source=${source} state=${state} actions=${actions} api=${api} />
    <${sheetMod.PhoneSheetHost} />
  </div>`);
  const c = mounted.container;
  const tap = (element) => harness.act(() => element.click());
  const settle = () => harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  const card = (id) => c.querySelector(`[data-task-id="${id}"]`);
  const sheet = () => c.querySelector('[role="dialog"]');
  const button = (root, name) => harness.getByRole(root, 'button', { name });
  // A submit button's form, submitted (LinkeDOM does not submit on a click).
  const submit = (btn) => harness.act(() => harness.fireEvent(btn.closest('form'), 'submit'));
  return { harness, c, tap, submit, settle, card, sheet, button, writes, refreshes: () => refreshes, pageMod, boardMod, source };
}

test('the Board draws the chosen column: chips, sections, cards, and a question answered in place', async (t) => {
  const { harness, c, tap, settle, card, button, writes, pageMod } = await setupView(t);
  assert.deepEqual([...c.querySelectorAll('.phone-chips [role="tab"]')].map((chip) => chip.textContent.replace(/\s+/g, ' ').trim()),
    ['Idea 1', 'Next 6', 'Started 1', 'In review 1', 'Done 2']);
  assert.deepEqual([...c.querySelectorAll('.phone-section')].map((s) => s.getAttribute('aria-label')), ['Ready', 'Waiting on you', 'Queued', 'Blocked']);
  assert.equal(pageMod.phoneBar.value.menu.sub, 'webshop', 'the group is in the title');
  assert.deepEqual(pageMod.phoneBar.value.actions.map((a) => a.label), ['Add a task']);
  assert.match(card('tk_ready').textContent, /@checkout-ui.*PR #1482/);
  assert.ok(button(card('tk_ready'), 'Move ▸') && button(card('tk_ready'), 'Comment'));
  assert.ok(card('tk_carrier').classList.contains('phone-box-q'));
  assert.match(card('tk_carrier').textContent, /asked by lead · 3 options/);
  await tap(button(card('tk_carrier'), /UPS/));
  await settle();
  assert.deepEqual(writes.at(-1), ['answer', 'tk_carrier', ['ups'], '', 1]);
  assert.match(card('tk_stuck').textContent, /waiting on the vendor/);

  await tap(harness.getByRole(c, 'tab', { name: /Done/ }));
  assert.deepEqual([...c.querySelectorAll('.phone-board-card .phone-board-title')].map((e) => e.textContent), ['Newer fix', 'Older fix']);
  assert.equal(c.querySelector('[data-task-id="tk_new"] .phone-btn'), null, 'a done task has no Move or Comment');
});

test('Move ▸ is a sheet: another column, or up and down among its section', async (t) => {
  const { tap, settle, card, sheet, button, writes } = await setupView(t);
  await tap(button(card('tk_coupon'), 'Move ▸'));
  assert.equal(sheet().getAttribute('aria-label'), 'Move');
  assert.equal(button(sheet(), /^Started/).disabled, true, 'nobody holds it to start it');
  await tap(button(sheet(), /^In review/));
  await settle();
  assert.deepEqual(writes.at(-1), ['stage', 'tk_coupon', 'in-review']);
  assert.equal(sheet(), null, 'a pick closes the sheet');

  await tap(button(card('tk_coupon'), 'Move ▸'));
  assert.equal(button(sheet(), '↓ Down').disabled, true, 'last in Ready');
  await tap(button(sheet(), '↑ Up'));
  await settle();
  assert.deepEqual(writes.at(-1), ['move', 'tk_coupon', { before: 'tk_ready', stage: 'next' }]);

  await tap(button(card('tk_ready'), 'Move ▸'));
  await tap(button(sheet(), /^Started/));
  await settle();
  assert.deepEqual(writes.at(-1), ['start', 'tk_ready'], 'its agent starts it');

  await tap(button(card('tk_coupon'), 'Move ▸'));
  await tap(button(sheet(), /^Done/));
  await settle();
  assert.deepEqual(writes.at(-1), ['status', 'tk_coupon', 'done']);
});

test('Comment and ＋ write through a sheet with the field focused', async (t) => {
  const { harness, tap, submit, settle, card, sheet, button, writes, pageMod } = await setupView(t);
  await tap(button(card('tk_coupon'), 'Comment'));
  await settle();
  const field = harness.getByRole(sheet(), 'textbox', { name: 'Comment' });
  assert.equal(harness.document.activeElement, field, 'the operator asked to write');
  await harness.input(field, 'On it');
  await submit(button(sheet(), 'Comment'));
  await settle();
  assert.equal(writes.at(-1)[0], 'comment');
  assert.deepEqual(writes.at(-1).slice(1, 3), ['tk_coupon', 'On it']);
  assert.equal(sheet(), null);

  await harness.act(() => pageMod.phoneBar.value.actions[0].onClick());
  await harness.input(harness.getByRole(sheet(), 'textbox', { name: 'Title' }), 'Free shipping banner');
  await submit(button(sheet(), 'Add'));
  await settle();
  assert.deepEqual(writes.at(-1), ['create', 'webshop', { title: 'Free shipping banner', stage: 'next' }]);
});

test("a task's page: its brief, refs and activity, a comment box, and Back to the board", async (t) => {
  const { harness, c, tap, submit, settle, card, button, writes, pageMod, boardMod } = await setupView(t);
  await tap(card('tk_ready').querySelector('.phone-board-open'));
  await settle();
  await settle();
  assert.deepEqual(boardMod.boardTask.value, { id: 'tk_ready', group: 'webshop' });
  const page = pageMod.phonePage.value;
  assert.equal(page.title, 'webshop', 'the bar names the board');
  assert.equal(c.querySelector('.phone-task-page-title').textContent, 'Checkout page redesign');
  assert.equal(page.view, 'board', 'a page over the Board');
  const text = c.querySelector('.phone-task-page').textContent;
  assert.match(text, /address/, 'the brief');
  assert.match(text, /PR #1482/, 'its refs');
  assert.match(text, /PR is up\./, 'a comment in the activity');
  assert.notEqual(harness.document.activeElement?.tagName, 'TEXTAREA', 'no field takes the focus by itself');
  await harness.input(harness.getByRole(c, 'textbox', { name: 'Comment' }), 'Looks good');
  await submit(button(c.querySelector('.phone-task-comment-box'), 'Comment'));
  await settle();
  assert.deepEqual(writes.at(-1).slice(0, 3), ['comment', 'tk_ready', 'Looks good']);

  await harness.act(() => page.onBack());
  assert.equal(boardMod.boardTask.value, null);
  assert.ok(card('tk_ready'), 'back on the board');
  assert.equal(pageMod.phonePage.value, null);
});

test('a task opened from another task\'s page goes Back to that page, then to the board', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/phone-board.js');
  const a = { id: 'tk_a', group: 'webshop' };
  const b = m.openFrom(a, 'tk_b');
  const c = m.openFrom(b, 'tk_c');
  assert.deepEqual(c, { id: 'tk_c', group: 'webshop', back: ['tk_a', 'tk_b'] });
  assert.deepEqual(m.closeTask(c), { id: 'tk_b', group: 'webshop', back: ['tk_a'] });
  assert.deepEqual(m.closeTask(m.closeTask(c)), { id: 'tk_a', group: 'webshop', back: [] });
  assert.equal(m.closeTask(a), null, 'from the first, Back is the board');
});

test("a question is also answered in words on the task's page", async (t) => {
  const { harness, c, submit, settle, button, writes, boardMod } = await setupView(t);
  await harness.act(() => boardMod.openBoardTask('tk_carrier', 'webshop'));
  await settle();
  const q = c.querySelector('.phone-task-q');
  assert.ok(q, 'the open question');
  await harness.input(harness.getByRole(q, 'textbox', { name: 'Answer in words' }), 'DHL for the EU only');
  await submit(button(q, 'Answer'));
  await settle();
  assert.deepEqual(writes.at(-1), ['answer', 'tk_carrier', [], 'DHL for the EU only', 1]);
});

test('Move out of a custom column lands where it was sent, as the desktop drop does', async (t) => {
  const harness = await createPreactHarness(t);
  const { moveTo } = await harness.importDashboardModule('js/phone-board.js');
  const m = await harness.importDashboardModule('js/phone-board-model.js');
  const calls = [];
  const api = new Proxy({}, { get: (_, name) => async (...args) => { calls.push([name, ...args]); return {}; } });
  const run = async (task, column) => { calls.length = 0; await moveTo(api, task, column); return calls.map((c) => c.slice(0, 3)); };
  const startedInReview = { id: 'tk_s', status: 'open', stage: 'next', column: 'in-review', assignee: { agent_id: AGENT } };
  const heldInReview = { id: 'tk_h', status: 'open', stage: 'next', column: 'in-review', held: true, assignee: { agent_id: AGENT } };
  const freeInReview = { id: 'tk_f', status: 'open', stage: 'next', column: 'in-review' };
  assert.deepEqual(await run(startedInReview, 'next'), [['stage', 'tk_s', 'next'], ['hold', 'tk_s', 'next']], 'Next: out of the column, not started');
  assert.deepEqual(await run(startedInReview, 'started'), [['stage', 'tk_s', 'next']], 'Started: back to In progress');
  assert.deepEqual(await run(heldInReview, 'started'), [['stage', 'tk_h', 'next'], ['start', 'tk_h']]);
  assert.deepEqual(await run(startedInReview, 'idea'), [['hold', 'tk_s', 'idea']], 'Idea: its agent keeps it, not started');
  assert.deepEqual(await run(freeInReview, 'idea'), [['stage', 'tk_f', 'idea']]);
  assert.deepEqual(await run({ id: 'tk_w', status: 'open', stage: 'next', assignee: { agent_id: AGENT } }, 'idea'), [['hold', 'tk_w', 'idea']]);
  assert.deepEqual(await run(freeInReview, 'done'), [['status', 'tk_f', 'done']]);
  const entry = { columns: [{ slug: 'in-review', label: 'In review' }, { slug: 'qa', label: 'QA', state: 'cordoned' }], tasks: [startedInReview] };
  const moves = m.columnMoves(startedInReview, entry);
  assert.equal(moves.find((c) => c.key === 'started').disabled, false, 'a started task in a column can go back to In progress');
  assert.deepEqual(moves.find((c) => c.key === 'col:qa'), { key: 'col:qa', label: 'QA', current: false, disabled: true, why: 'cordoned' });
});

test("a task's page over a tab's unrouted page counts as a layer above it", async (t) => {
  const harness = await createPreactHarness(t);
  const { layerDepth } = await harness.importDashboardModule('js/phone-history.js');
  const tabPage = { title: 'standup-ping' };
  assert.equal(layerDepth(tabPage, null), 1);
  assert.equal(layerDepth({ view: 'board', title: 'webshop', covered: tabPage }, null), 2, 'Back closes the task page first');
  assert.equal(layerDepth({ view: 'board', title: 'webshop', covered: { title: 'mail', routed: true } }, null), 1, 'a routed page is the router\'s');
});

test("Agents' Board › opens a group's board, even one with nothing live yet", async (t) => {
  const { harness, c, pageMod, boardMod, source } = await setupView(t);
  t.after(() => boardMod.openBoardGroup(''));
  await harness.act(() => { source.context.value = { groups: [board(), { group: 'steward', counts: { live: 0 }, tasks: [], closed: [] }] }; });
  await harness.act(() => boardMod.openBoardGroup('steward'));
  assert.equal(pageMod.phoneBar.value.menu.sub, 'steward');
  assert.equal(c.querySelectorAll('.phone-board-card').length, 0);
  // A group with no board at all shows the first board there is.
  await harness.act(() => boardMod.openBoardGroup('nowhere'));
  assert.equal(pageMod.phoneBar.value.menu.sub, 'webshop');
});

// tk_2d942cb6 items 1, 2, 8 and 9: the id with ⧉, the meta line, changes in
// the desktop's words, and a question with its state, options and answer.
test("a task's page: its id with copy, who made and holds it, changes in words, and a question's answer under it", async (t) => {
  const copied = [];
  const activity = async () => ({ entries: [
    { id: 1, at: '2026-11-14T00:00:00Z', actor: `agent:${LEAD}`, actor_name: 'lead', kind: 'created', detail: 'in next' },
    { id: 2, at: '2026-11-14T00:01:00Z', actor: `agent:${LEAD}`, actor_name: 'lead', kind: 'edited',
      changes: [{ field: 'status', before: 'open', after: 'blocked' }, { field: 'body', before: 'a', after: 'b' }] },
    { id: 3, at: '2026-11-14T00:02:00Z', actor: `agent:${LEAD}`, kind: 'comment', comment: { id: 10, at: '2026-11-14T00:02:00Z', author: `agent:${LEAD}`, author_name: 'lead',
      body: 'EU only for now.',
      question: { n: 1, title: 'Which carrier?', state: 'answered', recommend: 'dhl',
        options: [{ key: 'dhl', label: 'DHL' }, { key: 'ups', label: 'UPS' }],
        answer: { picked: ['dhl'], text: '', answered_by: 'human:operator', answered_at: '2026-11-14T00:03:00Z' } } } },
    { id: 4, at: '2026-11-14T00:03:00Z', actor: 'human:operator', actor_name: 'operator', kind: 'comment', comment: { id: 11, author: 'human:operator', author_name: 'operator', body: 'DHL', answers: 1 } },
    { id: 5, at: '2026-11-14T00:03:30Z', actor: 'human:operator', actor_name: 'operator', kind: 'comment_edited', comment: { id: 11, author: 'human:operator', author_name: 'operator', body: 'DHL' } },
    { id: 6, at: '2026-11-14T00:04:00Z', actor: 'peer:node-b', actor_name: 'node-b', kind: 'comment', comment: { id: 12, author: 'peer:node-b', author_name: 'node-b', body: '', deleted_at: '2026-11-14T00:05:00Z' } },
  ] });
  const { harness, c, settle, boardMod } = await setupView(t, { activity });
  const history = await harness.importDashboardModule('js/phone-task-history.js');
  await harness.act(() => boardMod.openBoardTask('tk_ready', 'webshop'));
  await settle();
  await settle();
  const idLine = c.querySelector('.phone-task-id');
  assert.equal(idLine.querySelector('code').textContent, 'tk_ready');
  assert.equal(idLine.querySelector('button').getAttribute('aria-label'), 'Copy tk_ready');
  const line = await harness.mount(harness.html`<${history.TaskIdLine} id="tk_x" copy=${(id) => copied.push(id)} />`);
  await harness.act(() => line.container.querySelector('button').click());
  assert.deepEqual(copied, ['tk_x'], '⧉ copies the id');
  const toasts = [];
  await history.copyId('tk_ready', async () => false, (text, bad) => toasts.push([text, !!bad]));
  assert.deepEqual(toasts, [['Could not copy tk_ready', true]]);
  const meta = c.querySelector('[data-task-meta]').textContent;
  assert.match(meta, /^created by lead /);
  assert.match(meta, /assigned to checkout-ui by lead /);
  assert.match(meta, /last edited by you /, 'the board\'s last edit, the operator as "you"');
  const items = [...c.querySelectorAll('.phone-task-activity > li')];
  assert.equal(items.length, 4, 'created, the edit, the question and a deleted comment; the answer sits under its question, the edit note is no entry');
  assert.equal(items[3].querySelector('.phone-task-who').textContent, 'node-b', 'a peer by the name the server sent');
  assert.equal(items[3].querySelector('.phone-task-what').textContent, 'deleted');
  assert.equal(items[1].querySelector('.phone-task-what').textContent, 'status: open → blocked · edited the brief', 'changeText words');
  const q = c.querySelector('[data-question-n="1"]');
  assert.match(q.querySelector('.phone-task-q-kind').textContent, /Q1 · answered/);
  assert.equal(q.querySelector('.phone-task-q-title').textContent, 'Which carrier?');
  assert.deepEqual([...q.querySelectorAll('.phone-task-q-opts li')].map((li) => li.textContent.trim()), ['● DHL recommended', '○ UPS']);
  assert.ok(q.querySelector('[data-answer-of="1"]').textContent.includes('you'), 'who answered');
});

test("a task's page names who worked on it, each a tap from their terminal or menu", async (t) => {
  const GONE = 'agt_00000000000000000000000000000009';
  const opened = [];
  const actions = {
    openTerminal: (agent) => opened.push(['terminal', agent.agentId]),
    agentMenu: (agent) => opened.push(['menu', agent.agentId]),
  };
  const activity = [
    { id: 1, at: '2026-11-14T00:00:00Z', actor: 'human:operator', actor_name: 'operator', kind: 'created' },
    { id: 2, at: '2026-11-14T00:01:00Z', actor: `agent:${AGENT}`, actor_name: 'checkout-ui', kind: 'taken' },
    { id: 3, at: '2026-11-14T00:02:00Z', actor: `agent:${GONE}`, actor_name: 'reviewer', kind: 'comment',
      comment: { id: 3, author: `agent:${GONE}`, author_name: 'reviewer', body: 'Looks fine.', at: '2026-11-14T00:02:00Z' } },
    { id: 4, at: '2026-11-14T00:03:00Z', actor: `agent:${AGENT}`, actor_name: 'checkout-ui', kind: 'reported', detail: 'done: shipped' },
  ];
  const { c, tap, settle, card } = await setupView(t, { actions, activity: async () => ({ events: activity }) });
  await tap(card('tk_ready').querySelector('.phone-board-open'));
  await settle();
  await settle();
  const people = [...c.querySelectorAll('.phone-task-person')];
  assert.deepEqual(people.map((li) => li.dataset.person), [AGENT, GONE, LEAD], 'the assignee first, then the rest, the latest first');
  assert.match(people[2].textContent, /created it/, 'who made the task');
  assert.match(people[0].textContent, /assignee · took it · reported done/);
  assert.match(people[1].textContent, /commented/);
  assert.match(people[1].textContent, /offline/, 'an agent the snapshot does not list is offline');
  assert.equal(people[1].querySelector('.phone-task-person-open').disabled, true, 'and is no jump: it has no terminal or menu here');
  assert.equal(people[1].querySelector('.phone-task-person-more'), null);
  await tap(people[0].querySelector('.phone-task-person-open'));
  assert.deepEqual(opened, [['terminal', AGENT]], 'an online agent: its terminal');
  await tap(people[0].querySelector('.phone-task-person-more'));
  assert.deepEqual(opened.at(-1), ['menu', AGENT], '⋯ is its menu');
  // An actor in the Activity is the same jump; the operator is plain text.
  const names = [...c.querySelectorAll('.phone-task-activity .phone-task-who')];
  assert.equal(names[0].tagName, 'SPAN', 'you are not a link');
  assert.equal(names[0].textContent, 'you', 'the operator is "you"');
  assert.equal(c.querySelector(`.phone-task-activity .phone-task-who-link[data-person="${GONE}"]`), null, 'nor an unlisted agent');
  await tap(c.querySelector(`.phone-task-activity .phone-task-who-link[data-person="${AGENT}"]`));
  assert.deepEqual(opened.at(-1), ['terminal', AGENT]);
});

test("a task's page lists its files, and a comment its kind, edits and own files", async (t) => {
  const file = (id, name) => ({ id, name, size: 2048, content_type: 'text/plain', added_by: 'human:operator' });
  const activity = [
    { id: 1, at: '2026-11-14T00:01:00Z', actor: `agent:${AGENT}`, kind: 'comment',
      comment: { id: 1, kind: 'report_note', author: `agent:${AGENT}`, author_name: 'checkout-ui', body: 'Shipped, log attached.',
        at: '2026-11-14T00:01:00Z', edited_at: '2026-11-14T00:05:00Z', attachments: [file('a2', 'ci.log')] } },
    { id: 2, at: '2026-11-14T00:02:00Z', actor: 'human:operator', kind: 'comment',
      comment: { id: 2, author: 'human:operator', body: 'gone', at: '2026-11-14T00:02:00Z', deleted_at: '2026-11-14T00:03:00Z' } },
    { id: 3, at: '2026-11-14T00:05:00Z', actor: `agent:${AGENT}`, kind: 'comment_edited', comment_id: 1, previous_body: 'Shipped.',
      comment: { id: 1, kind: 'report_note', author: `agent:${AGENT}`, body: 'Shipped, log attached.', at: '2026-11-14T00:01:00Z', edited_at: '2026-11-14T00:05:00Z' } },
  ];
  const { c, tap, settle, card } = await setupView(t, { activity: async () => ({ events: activity }), row: { attachments: [file('a1', 'notes.txt')] } });
  await tap(card('tk_ready').querySelector('.phone-board-open'));
  await settle();
  await settle();
  const files = c.querySelector('[aria-label="Files"]');
  assert.ok(files, 'a Files section');
  assert.match(files.textContent, /notes\.txt/);
  const items = [...c.querySelectorAll('.phone-task-activity > li')];
  assert.equal(items.length, 2, 'an edit is shown on its comment, not as a second copy');
  assert.equal(items[0].querySelector('[data-comment-kind]')?.textContent, 'report');
  assert.match(items[0].textContent, /edited/);
  assert.match(items[0].querySelector('.phone-task-comment-files').textContent, /ci\.log/, "the comment's own file");
  assert.match(items[1].textContent, /deleted/);
  assert.doesNotMatch(items[1].textContent, /gone/, "a deleted comment's words are not shown");
});
