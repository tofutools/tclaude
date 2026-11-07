// A task's title and its hover card (groups-taskboard-card.js): the List's
// Name cell and a Lanes card show the title only; a click (or Enter) on it
// opens the edit form; hovering it shows the task's card, which reads the
// brief and the latest comment through the board's api once the pointer
// rests.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5),
  created_by: 'human:operator', updated_at: ago(2), attachments: [], ...extra });

function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 2, next: 2 },
    tasks: [
      row('tk_00000001', 'Release notes', { comment_count: 2, ref: { url: 'https://linear.app/x/issue/SHOP-1', label: 'SHOP-1' },
        attachments: [{ id: 3, name: 'qa.md', size: 10, content_type: 'text/markdown' }] }),
      row('tk_00000002', 'held', { status: 'blocked', block_reason: 'waiting on QA', depends_on: ['tk_00000001'] }),
    ],
    closed: [],
  };
}

function fakeApi() {
  const calls = [];
  const api = {
    calls,
    task: async (id) => { calls.push(['task', id]); return { id, body: 'Collect the **merged** PRs.' }; },
    activity: async (id) => { calls.push(['activity', id]); return { id, entries: [
      { id: 1, kind: 'comment', comment: { id: 1, kind: 'comment', body: 'first', author: 'human:operator', at: ago(9) } },
      { id: 2, kind: 'comment', comment: { id: 2, kind: 'comment', body: 'QA has the checklist now', author: 'agent:agt_dev', author_name: 'dev', at: ago(1) } },
    ] }; },
    fileData: async () => ({ data: '', content_type: 'text/markdown' }),
    edit: async () => ({}), status: async () => ({}), move: async () => ({}), stage: async () => ({}),
  };
  return api;
}

async function mountBoard(t, { lanes = false, api = fakeApi() } = {}) {
  const harness = await createPreactHarness(t);
  const [{ TaskboardSection }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  dashPrefs.setItem('tclaude.dash.groups.fused.taskboard.crew', '1');
  if (lanes) dashPrefs.setItem('tclaude.dash.groups.taskboard.view.crew', 'lanes');
  const source = { context: harness.signals.signal({ groups: [board('crew')] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {} };
  const group = { name: 'crew', members: [{ agent_id: 'agt_dev', title: 'dev', online: true }] };
  const { container } = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const settle = async () => { for (let i = 0; i < 3; i += 1) await harness.act(() => new Promise((r) => setTimeout(r, 0))); };
  return { harness, c: container, api, settle };
}

test('the List\'s Name cell shows the title only; a click or Enter on it opens the task', async (t) => {
  const { harness, c, settle } = await mountBoard(t);
  const cell = c.querySelector('tr[data-task-id="tk_00000001"] .tb-title-cell');
  assert.equal(cell.querySelector('.tb-n'), null, 'no order number: the row\'s place says it');
  assert.equal(cell.querySelector('.tb-id'), null, 'no id inline: the card carries it');
  assert.equal(cell.querySelector('.tb-title').textContent, 'Release notes');
  await harness.act(() => harness.fireEvent(cell.querySelector('.tb-title'), 'click'));
  await settle();
  assert.ok(harness.document.querySelector('#taskboard-edit-modal'), 'a click opens the edit form');
  assert.match(harness.document.querySelector('#taskboard-edit-modal').textContent, /tk_00000001/, 'whose header names the id');
});

test('Enter on a focused title opens the task too', async (t) => {
  const { harness, c, settle } = await mountBoard(t);
  const title = c.querySelector('tr[data-task-id="tk_00000002"] .tb-title');
  assert.equal(title.getAttribute('role'), 'button');
  assert.equal(title.getAttribute('tabindex'), '0', 'keyboard-focusable');
  await harness.act(() => harness.fireEvent(title, 'keydown', { key: 'Enter' }));
  await settle();
  assert.ok(harness.document.querySelector('#taskboard-edit-modal'));
});

test('hovering a title shows the task\'s card; resting on it reads the brief and the latest comment', async (t) => {
  const { harness, c, api, settle } = await mountBoard(t);
  const hover = c.querySelector('tr[data-task-id="tk_00000001"] .tb-task-hover');
  await harness.act(() => harness.fireEvent(hover, 'mouseenter'));
  const panel = () => c.querySelector('[data-task-card="tk_00000001"]');
  assert.ok(panel(), 'the card opens at once');
  assert.match(panel().textContent, /tk_00000001/, 'with the id');
  assert.ok(panel().querySelector('.tb-task-copy'), 'and a copy button');
  assert.match(panel().textContent, /next/);
  assert.match(panel().textContent, /SHOP-1/, 'the ref');
  assert.match(panel().textContent, /📎 1 file/);
  assert.match(panel().textContent, /💬 2/);
  assert.deepEqual(api.calls, [], 'nothing is read before the pointer rests');
  // Hover intent: poll for the reads, up to a limit.
  for (let i = 0; i < 100 && !panel().querySelector('.tb-task-panel-latest'); i += 1) await harness.act(() => new Promise((r) => setTimeout(r, 20)));
  assert.deepEqual(api.calls.map((x) => x[0]).sort(), ['activity', 'task']);
  assert.match(panel().querySelector('.tb-task-panel-latest').textContent, /dev: QA has the checklist now/, 'the latest comment');
  for (let i = 0; i < 100 && !panel().querySelector('.tb-task-brief-clamp strong'); i += 1) await harness.act(() => new Promise((r) => setTimeout(r, 20)));
  assert.ok(panel().querySelector('.tb-task-brief-clamp strong'), 'the brief, as Markdown');
  await harness.act(() => harness.fireEvent(panel().querySelector('.tb-task-more'), 'click'));
  await settle();
  assert.ok(harness.document.querySelector('#taskboard-edit-modal'), '"more" opens the task');
});

test('a blocked task\'s card says why; a press in the card is no drag', async (t) => {
  const { harness, c } = await mountBoard(t);
  const { bindTaskRowDrag } = await harness.importDashboardModule('js/row-drag.js');
  t.after(bindTaskRowDrag(harness.document));
  const tr = c.querySelector('tr[data-task-id="tk_00000002"]');
  await harness.act(() => harness.fireEvent(tr.querySelector('.tb-task-hover'), 'mouseenter'));
  const panel = c.querySelector('[data-task-card="tk_00000002"]');
  assert.match(panel.textContent, /blocked: “waiting on QA”/);
  harness.fireEvent(panel.querySelector('.tb-task-panel-facts'), 'pointerdown', {});
  assert.equal(tr.draggable, false, 'a press inside the card is a click, not a row drag');
  harness.fireEvent(harness.document, 'pointerup', {});
});

test('a Lanes card shows the title only, which opens the task; the card is no drag from its title', async (t) => {
  const { harness, c, settle } = await mountBoard(t, { lanes: true });
  const card = c.querySelector('.tb-card[data-task-id="tk_00000001"]');
  assert.ok(card, 'the lanes view');
  assert.equal(card.querySelector('.tb-card-n'), null, 'no order number');
  assert.equal(card.querySelector('.tb-card-title').textContent.trim(), 'Release notes');
  assert.equal(card.hasAttribute('title'), false, 'the card shows the id, not a tooltip');
  const { bindTaskRowDrag } = await harness.importDashboardModule('js/row-drag.js');
  t.after(bindTaskRowDrag(harness.document));
  harness.fireEvent(card.querySelector('.tb-title'), 'pointerdown', {});
  assert.equal(card.draggable, false, 'a press on the title is a click');
  harness.fireEvent(harness.document, 'pointerup', {});
  assert.equal(card.draggable, true);
  await harness.act(() => harness.fireEvent(card.querySelector('.tb-title'), 'click'));
  await settle();
  assert.ok(harness.document.querySelector('#taskboard-edit-modal'));
});
