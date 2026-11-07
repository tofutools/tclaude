import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });

function board(group) {
  return {
    group, group_id: 1,
    counts: { live: 4, needs_you: 1, next: 2, ideas: 1, blocked: 1, done_this_week: 1 },
    tasks: [
      row('tk_00000001', 'first'),
      row('tk_00000002', 'second'),
      row('tk_00000003', 'held', { status: 'blocked', block_reason: 'waiting' }),
      row('tk_00000004', 'an idea', { stage: 'idea' }),
    ],
    closed: [row('tk_00000005', 'finished', { status: 'done', closed_at: ago(30) })],
  };
}

// fakeApi records every write as [verb, ...args] and answers as the routes do.
function fakeApi() {
  const calls = [];
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return verb === 'capture' || verb === 'importRefs'
    ? { created: 2, results: [{ input: 'a', task: {} }, { input: 'b', task: {} }] } : {}; };
  return { calls, capture: record('capture'), importRefs: record('importRefs'), edit: record('edit'), move: record('move'), stage: record('stage'), status: record('status') };
}

async function mountBoard(t, name) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  let refreshes = 0;
  const source = {
    context: harness.signals.signal({ groups: [board(name)] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => { refreshes += 1; },
  };
  const api = fakeApi();
  const group = { name, members: [] };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const rowOf = (id) => c.querySelector(`tr[data-task-id="${id}"]`);
  const button = (id, text) => [...rowOf(id).querySelectorAll('.tb-actions button')].find((b) => b.textContent.trim() === text);
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  return { harness, c, api, rowOf, button, click, refreshes: () => refreshes };
}

test('row actions map to the CLI verbs and re-read the board', async (t) => {
  const { harness, c, api, rowOf, button, click, refreshes } = await mountBoard(t, 'tba-rows');
  assert.ok(c.querySelector('[data-fused-taskboard]'), 'blocked work opens the section');
  assert.deepEqual([...rowOf('tk_00000001').querySelectorAll('.tb-actions button')].map((b) => b.textContent.trim()), ['Done', 'Block', 'Edit', 'Drop']);
  assert.deepEqual([...rowOf('tk_00000003').querySelectorAll('.tb-actions button')].map((b) => b.textContent.trim()), ['Unblock', 'Edit', 'Drop']);
  assert.deepEqual([...rowOf('tk_00000004').querySelectorAll('.tb-actions button')].map((b) => b.textContent.trim()), ['✓ Next', 'Edit', 'Drop']);

  await click(button('tk_00000004', '✓ Next'));
  await click(button('tk_00000001', 'Done'));
  await click(button('tk_00000003', 'Unblock'));
  assert.deepEqual(api.calls, [['stage', 'tk_00000004', 'next'], ['status', 'tk_00000001', 'done'], ['status', 'tk_00000003', 'open']]);
  assert.equal(refreshes(), 3);

  // Block asks for a reason inline; it cannot be empty.
  api.calls.length = 0;
  await click(button('tk_00000002', 'Block'));
  const form = c.querySelector('[data-task-form="block"]');
  const submit = [...form.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Block');
  assert.equal(submit.disabled, true);
  await harness.input(form.querySelector('input'), 'waiting on CI');
  await click([...c.querySelector('[data-task-form="block"]').querySelectorAll('button')].find((b) => b.textContent.trim() === 'Block'));
  assert.deepEqual(api.calls, [['status', 'tk_00000002', 'blocked', 'waiting on CI']]);
  assert.equal(c.querySelector('[data-task-form]'), null, 'the form closes on success');

  // Edit changes only what changed; an emptied ref clears it.
  api.calls.length = 0;
  await click(button('tk_00000001', 'Edit'));
  const edit = c.querySelector('[data-task-form="edit"]');
  await harness.input(edit.querySelector('.tb-form-title'), 'first, renamed');
  await harness.act(async () => { harness.fireEvent(c.querySelector('.tb-form-title'), 'keydown', { key: 'Enter' }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['edit', 'tk_00000001', { title: 'first, renamed' }]]);

  // Drop takes an optional reason.
  api.calls.length = 0;
  await click(button('tk_00000002', 'Drop'));
  await click([...c.querySelector('[data-task-form="drop"]').querySelectorAll('button')].find((b) => b.textContent.trim() === 'Drop'));
  assert.deepEqual(api.calls, [['status', 'tk_00000002', 'dropped', '']]);

  // A closed task reopens from history.
  api.calls.length = 0;
  await click(c.querySelector('.tb-history-toggle'));
  await click([...c.querySelectorAll('.tb-closed button')].find((b) => b.textContent.trim() === 'Reopen'));
  assert.deepEqual(api.calls, [['status', 'tk_00000005', 'open']]);
});

test('the capture box adds on Enter, keeps the text area for the next line, and adds a pasted list at once', async (t) => {
  const { harness, c, api } = await mountBoard(t, 'tba-capture');
  const box = c.querySelector('.tb-capture-input');
  await harness.input(box, 'try the fused card');
  await harness.act(async () => { harness.fireEvent(box, 'keydown', { key: 'Enter' }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['capture', 'tba-capture', 'try the fused card', { next: false }]]);
  assert.equal(c.querySelector('.tb-capture-input').value, '', 'cleared for the next line');
  assert.match(c.querySelector('.tb-capture-note').textContent, /burst: 2 ideas added/);

  // Shift+Enter is a new line, not an add.
  api.calls.length = 0;
  await harness.act(async () => { harness.fireEvent(box, 'keydown', { key: 'Enter', shiftKey: true }); });
  assert.deepEqual(api.calls, []);

  // A pasted list adds one task per line, queued when "capture as next" is on.
  const toggle = c.querySelector('.tb-capture .ui-check input');
  toggle.checked = true;
  await harness.act(async () => { harness.fireEvent(toggle, 'change'); });
  await harness.act(async () => {
    // Preact listens for "paste" where the element has onpaste (browsers),
    // else for the event name as written ("Paste", LinkeDOM).
    const area = c.querySelector('.tb-capture-input');
    harness.fireEvent(area, 'onpaste' in area ? 'paste' : 'Paste', { clipboardData: { getData: () => 'one\ntcl-fb0bfz\n' } });
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.equal(c.querySelector('.tb-capture .ui-eyebrow').textContent, 'Capture · lands in the queue');
  assert.deepEqual(api.calls, [['capture', 'tba-capture', 'one\ntcl-fb0bfz\n', { next: true }]]);
});

test('dragging a row reorders it; dropping a queued task on the Ideas band demotes it', async (t) => {
  const { harness, c, api, rowOf } = await mountBoard(t, 'tba-drag');
  const transfer = { setData() {}, setDragImage() {}, effectAllowed: '', dropEffect: '' };
  const drag = async (fromId, target, clientY) => {
    await harness.act(async () => {
      harness.fireEvent(rowOf(fromId).querySelector('.tb-handle'), 'dragstart', { dataTransfer: transfer });
      harness.fireEvent(target, 'dragover', { dataTransfer: transfer, clientY });
    });
    await harness.act(async () => {
      harness.fireEvent(target, 'drop', { dataTransfer: transfer, clientY });
      await new Promise((r) => setTimeout(r, 0));
    });
  };
  // LinkeDOM rows have a zero-height box, so any pointer lands in the lower
  // half: "after".
  await drag('tk_00000002', rowOf('tk_00000004'), 10);
  await drag('tk_00000001', c.querySelector('.tb-ideas-divider'), 10);
  await drag('tk_00000004', c.querySelector('.tb-ideas-divider'), 10);
  assert.deepEqual(api.calls, [
    ['move', 'tk_00000002', { after: 'tk_00000004' }],
    ['stage', 'tk_00000001', 'idea'],
  ], 'an idea dropped on the band it already heads stays put');
  // A drop with no row drag in flight is ignored (an agent row, a file).
  api.calls.length = 0;
  await harness.act(async () => { harness.fireEvent(rowOf('tk_00000001'), 'drop', { dataTransfer: transfer }); });
  assert.deepEqual(api.calls, []);
});

test('"+ from awb/Linear…" imports tracker ids with the tracker prefix', async (t) => {
  const { harness, c, api, click } = await mountBoard(t, 'tba-import');
  await click(c.querySelector('.tb-import-open'));
  const panel = c.querySelector('[data-taskboard-import]');
  await harness.input(panel.querySelector('.tb-import-ids'), 'tcl-a1b2c3, tcl-d4e5f6');
  await click([...c.querySelector('[data-taskboard-import]').querySelectorAll('button')].find((b) => b.textContent.trim().startsWith('Import')));
  assert.deepEqual(api.calls, [['importRefs', 'tba-import', ['awb:tcl-a1b2c3', 'awb:tcl-d4e5f6'], { next: false }]]);
  assert.equal(c.querySelector('[data-taskboard-import]'), null, 'a clean import closes the panel');
});

test('the client sends the CLI routes with an idempotency key', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTaskboardApi } = await harness.importDashboardModule('js/taskboard-actions.js');
  const sent = [];
  const api = createTaskboardApi({ fetchImpl: async (path, init) => {
    sent.push({ path, method: init.method, body: init.body && JSON.parse(init.body), key: init.headers?.['Idempotency-Key'] });
    return { ok: true, status: 200, json: async () => ({ created: 1, results: [] }) };
  } });
  await api.capture('crew', 'plain title');
  await api.capture('crew', 'tcl-fb0bfz', { next: true });
  await api.capture('crew', 'a\nb');
  await api.edit('tk_00000001', { clearRef: true });
  await api.move('tk_00000001', { before: 'tk_00000002' });
  await api.status('tk_00000001', 'blocked', 'why');
  assert.deepEqual(sent.map(({ path, method, body }) => [method, path, body]), [
    ['POST', '/v1/groups/crew/taskboard/tasks', { title: 'plain title' }],
    ['POST', '/v1/groups/crew/taskboard/tasks', { lines: ['tcl-fb0bfz'], stage: 'next' }],
    ['POST', '/v1/groups/crew/taskboard/tasks', { lines: ['a', 'b'] }],
    ['PATCH', '/v1/taskboard/tasks/tk_00000001', { clear_ref: true }],
    ['POST', '/v1/taskboard/tasks/tk_00000001/move', { before: 'tk_00000002' }],
    ['POST', '/v1/taskboard/tasks/tk_00000001/status', { status: 'blocked', reason: 'why' }],
  ]);
  assert.ok(sent.every((s) => /^[0-9a-f-]{36}$/.test(s.key)), 'every write carries a key');

  const refusing = createTaskboardApi({ fetchImpl: async () => ({ ok: false, status: 409, json: async () => ({ error: 'cap', message: 'board is full' }) }) });
  await assert.rejects(refusing.status('tk_00000001', 'done'), (err) => err.message === 'board is full' && err.status === 409);
});
