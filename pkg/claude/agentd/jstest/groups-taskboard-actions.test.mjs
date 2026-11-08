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
      row('tk_00000002', 'second', { assignee: { agent_id: 'agt_builder', name: 'builder' }, assigned_by: 'human:operator' }),
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
  return { calls, capture: record('capture'), importRefs: record('importRefs'), edit: record('edit'), move: record('move'), stage: record('stage'), status: record('status'),
    assign: record('assign'), unassign: record('unassign'), reopen: record('reopen'), archive: record('archive'), archiveDone: record('archiveDone'), archiveClosed: record('archiveClosed') };
}

async function mountBoard(t, name, { api = fakeApi(), context = null } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  // These cover the List; Lanes is the default view.
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, 'list');
  let refreshes = 0;
  const source = {
    context: harness.signals.signal(context || { groups: [board(name)] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => { refreshes += 1; },
  };
  const group = { name, members: [
    { agent_id: 'agt_builder', title: 'builder', online: true },
    { agent_id: 'agt_tester', title: 'tester', online: false },
    { conv_id: 'conv-no-agent', title: 'plain' },
  ] };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const rowOf = (id) => c.querySelector(`tr[data-task-id="${id}"]`);
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  // A row's icon cluster, by aria-label: ✓ Done or Next, ↺ Reopen, ✎ Edit.
  const cluster = (id) => [...rowOf(id).querySelectorAll('.tb-row-actions > button')].map((b) => b.getAttribute('aria-label'));
  const button = (id, label) => [...rowOf(id).querySelectorAll('.tb-row-actions button')].find((b) => b.getAttribute('aria-label') === label);
  const more = (id) => rowOf(id).querySelector('.tb-act-more');
  const menuItems = () => [...c.querySelectorAll('.gr-add-menu [role="menuitem"]')];
  // menu opens a row's ⋯ and reads its items, then closes it.
  const menu = async (id) => {
    await click(more(id));
    const items = menuItems().map((b) => b.textContent.trim());
    await click(more(id));
    return items;
  };
  // pick opens a row's ⋯ and picks the path's items in turn.
  const pick = async (id, ...path) => {
    await click(more(id));
    for (const label of path) {
      const item = menuItems().find((b) => b.textContent.trim().replace(/ ›$/, '') === label);
      assert.ok(item, `menu item ${label}: ${menuItems().map((b) => b.textContent.trim()).join(', ')}`);
      await click(item);
    }
  };
  return { harness, c, api, source, group, rowOf, button, cluster, menu, pick, click, refreshes: () => refreshes };
}

test('row actions map to the CLI verbs and re-read the board', async (t) => {
  const { harness, c, api, rowOf, button, cluster, menu, pick, click, refreshes } = await mountBoard(t, 'tba-rows');
  assert.ok(c.querySelector('[data-fused-taskboard]'), 'blocked work opens the section');
  // The cluster: the state's one action and Edit; the rest is in ⋯.
  assert.deepEqual(cluster('tk_00000001'), ['Done', 'Edit']);
  assert.deepEqual(cluster('tk_00000003'), ['Done', 'Edit']);
  assert.deepEqual(cluster('tk_00000004'), ['Next', 'Edit']);
  assert.deepEqual(await menu('tk_00000001'), ['Assign ›', 'Block…', 'Drop…']);
  assert.deepEqual(await menu('tk_00000002'), ['Assign ›', 'Block…', 'Unassign', 'Drop…']);
  assert.deepEqual(await menu('tk_00000003'), ['Unblock', 'Drop…']);
  assert.deepEqual(await menu('tk_00000004'), ['Assign ›', 'Block…', 'Drop…']);

  await click(button('tk_00000004', 'Next'));
  await click(button('tk_00000001', 'Done'));
  await pick('tk_00000003', 'Unblock');
  assert.deepEqual(api.calls, [['stage', 'tk_00000004', 'next'], ['status', 'tk_00000001', 'done'], ['status', 'tk_00000003', 'open']]);
  assert.equal(refreshes(), 3);

  // Block asks for a reason in the shared dialog, not inline under the row;
  // it cannot be empty, the field (a textarea) has focus, Escape cancels,
  // Enter adds a line and Ctrl/Cmd+Enter submits.
  api.calls.length = 0;
  const dialog = () => harness.document.querySelector('#taskboard-reason-modal');
  const field = () => dialog().querySelector('textarea.tb-form-reason');
  const submitButton = () => dialog().querySelector('#taskboard-reason-submit');
  const key = (el, k, extra = {}) => harness.act(async () => { harness.fireEvent(el, 'keydown', { key: k, ...extra }); await new Promise((r) => setTimeout(r, 0)); });
  await pick('tk_00000002', 'Block…');
  assert.equal(c.querySelector('.tb-form-row'), null, 'no inline form under the row');
  assert.ok(dialog(), 'Block… opens the reason dialog');
  assert.equal(dialog().querySelector('[data-task-form]').dataset.taskForm, 'block');
  assert.match(dialog().textContent, /Block task[\s\S]*“second”/);
  assert.equal(harness.document.activeElement, field(), 'the reason field has focus');
  assert.equal(submitButton().disabled, true, 'a block reason is required');
  await key(field(), 'Enter', { ctrlKey: true });
  assert.deepEqual(api.calls, [], 'Ctrl+Enter on an empty reason writes nothing');
  await harness.input(field(), '  \n ');
  assert.equal(submitButton().disabled, true, 'a whitespace-only reason is still empty');
  assert.match(dialog().querySelector('#taskboard-reason-hint').textContent, /Ctrl\/Cmd\+Enter/);
  await key(field(), 'Escape');
  assert.equal(dialog(), null, 'Escape cancels');
  assert.deepEqual(api.calls, []);
  await pick('tk_00000002', 'Block…');
  await harness.input(field(), 'waiting on CI\nthen on review');
  assert.equal(submitButton().disabled, false);
  await key(field(), 'Enter');
  assert.deepEqual(api.calls, [], 'a plain Enter adds a line, it does not submit');
  assert.ok(dialog());
  await key(field(), 'Enter', { metaKey: true });
  assert.deepEqual(api.calls, [['status', 'tk_00000002', 'blocked', 'waiting on CI\nthen on review']], 'the reason keeps its line break');
  assert.equal(dialog(), null, 'the dialog closes on success');

  // ✎ opens the edit form; it saves only what changed.
  api.calls.length = 0;
  await click(button('tk_00000001', 'Edit'));
  const edit = harness.document.querySelector('#taskboard-edit-modal');
  assert.ok(edit, 'the edit form opens');
  await harness.input(edit.querySelector('.tb-edit-title'), 'first, renamed');
  await harness.act(async () => { harness.fireEvent(edit.querySelector('.tb-edit-title'), 'keydown', { key: 'Enter' }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['edit', 'tk_00000001', { title: 'first, renamed' }]]);
  assert.equal(harness.document.querySelector('#taskboard-edit-modal'), null, 'a full Save closes it');

  // Drop takes an optional reason.
  api.calls.length = 0;
  await pick('tk_00000002', 'Drop…');
  assert.equal(dialog().querySelector('[data-task-form]').dataset.taskForm, 'drop');
  assert.equal(submitButton().disabled, false, 'a drop reason is optional');
  await click(submitButton());
  assert.deepEqual(api.calls, [['status', 'tk_00000002', 'dropped', '']]);
  assert.equal(dialog(), null);
  api.calls.length = 0;
  await pick('tk_00000004', 'Drop…');
  await harness.input(field(), 'parked in awb');
  await click(submitButton());
  assert.deepEqual(api.calls, [['status', 'tk_00000004', 'dropped', 'parked in awb']]);

  // A closed task reopens from history, a row of the same table.
  api.calls.length = 0;
  await click(c.querySelector('.tb-history-toggle'));
  const closed = rowOf('tk_00000005');
  assert.ok(closed.matches('.tb-closed') && closed.closest('table') === rowOf('tk_00000001').closest('table'), 'history rows share the list\'s table');
  assert.deepEqual(cluster('tk_00000005'), ['Reopen', 'Edit']);
  assert.deepEqual((await menu('tk_00000005')).filter((label) => label.startsWith('Reopen')), ['Reopen', 'Reopen as an idea'],
    'the ⋯ menu reopens it where it was, or elsewhere');
  await click(button('tk_00000005', 'Reopen'));
  assert.deepEqual(api.calls, [['reopen', 'tk_00000005']]);
});

test('the section has no capture box: it adds through ＋ New task', async (t) => {
  const list = await mountBoard(t, 'tba-capture-list');
  assert.equal(list.c.querySelector('[data-taskboard-capture]'), null);
  assert.equal(list.c.querySelector('[data-taskboard-new]').getAttribute('aria-label'), 'New task');
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
  // The row drags as a whole too, not only from its ⋮⋮.
  api.calls.length = 0;
  await harness.act(async () => {
    harness.fireEvent(rowOf('tk_00000004'), 'dragstart', { dataTransfer: transfer });
    harness.fireEvent(rowOf('tk_00000002'), 'dragover', { dataTransfer: transfer, clientY: 10 });
  });
  await harness.act(async () => { harness.fireEvent(rowOf('tk_00000002'), 'drop', { dataTransfer: transfer, clientY: 10 }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['move', 'tk_00000004', { after: 'tk_00000002' }]], 'a row dragged by its body reorders');
  // A drop with no row drag in flight is ignored (an agent row, a file).
  api.calls.length = 0;
  await harness.act(async () => { harness.fireEvent(rowOf('tk_00000001'), 'drop', { dataTransfer: transfer }); });
  assert.deepEqual(api.calls, []);
});

test('"+ New task › From a tracker… › Several ids" imports tracker ids with the tracker prefix', async (t) => {
  const { harness, c, api, click } = await mountBoard(t, 'tba-import');
  await click(c.querySelector('[data-taskboard-new]'));
  await click(c.querySelector('[data-taskboard-new-import]'));
  const dialog = () => harness.document.querySelector('#taskboard-tracker-modal');
  assert.ok(dialog(), 'the tracker form opens as a dialog');
  await click(dialog().querySelector('[data-tracker-mode="many"]'));
  await harness.input(dialog().querySelector('.tb-import-ids'), 'tcl-a1b2c3, tcl-d4e5f6');
  await click(dialog().querySelector('#taskboard-tracker-submit'));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['importRefs', 'tba-import', ['awb:tcl-a1b2c3', 'awb:tcl-d4e5f6'], { next: false }]]);
  assert.equal(dialog(), null, 'a clean import closes the dialog');
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
  await api.edit('tk_00000001', { ref: 'LIN-12', resolveRef: true });
  await api.move('tk_00000001', { before: 'tk_00000002' });
  await api.status('tk_00000001', 'blocked', 'why');
  assert.deepEqual(sent.map(({ path, method, body }) => [method, path, body]), [
    ['POST', '/v1/groups/crew/taskboard/tasks', { title: 'plain title' }],
    ['POST', '/v1/groups/crew/taskboard/tasks', { lines: ['tcl-fb0bfz'], stage: 'next' }],
    ['POST', '/v1/groups/crew/taskboard/tasks', { lines: ['a', 'b'] }],
    ['PATCH', '/v1/taskboard/tasks/tk_00000001', { clear_ref: true }],
    ['PATCH', '/v1/taskboard/tasks/tk_00000001', { ref: 'LIN-12', resolve_ref: true }],
    ['POST', '/v1/taskboard/tasks/tk_00000001/move', { before: 'tk_00000002' }],
    ['POST', '/v1/taskboard/tasks/tk_00000001/status', { status: 'blocked', reason: 'why' }],
  ]);
  assert.ok(sent.every((s) => /^[0-9a-f-]{36}$/.test(s.key)), 'every write carries a key');

  // A long import goes in batches of 50 and the results merge in order.
  sent.length = 0;
  const lines = Array.from({ length: 120 }, (_, i) => `task ${i}`).join('\n');
  const batched = createTaskboardApi({ fetchImpl: async (path, init) => {
    const body = JSON.parse(init.body);
    sent.push(body.lines.length);
    return { ok: true, status: 200, json: async () => ({ created: body.lines.length, results: body.lines.map((input) => ({ input, task: {} })) }) };
  } });
  const out = await batched.capture('crew', lines);
  assert.deepEqual(sent, [50, 50, 20]);
  assert.equal(out.created, 120);
  assert.equal(out.results[119].input, 'task 119');

  const refusing = createTaskboardApi({ fetchImpl: async () => ({ ok: false, status: 409, json: async () => ({ error: 'cap', message: 'board is full' }) }) });
  await assert.rejects(refusing.status('tk_00000001', 'done'), (err) => err.message === 'board is full' && err.status === 409);
});

test('a section that opened on its own stays open when the last blocked task clears', async (t) => {
  const name = 'tba-latch';
  const { harness, c, source } = await mountBoard(t, name);
  assert.ok(c.querySelector('.tb-table'), 'opened: something is blocked');
  const cleared = board(name);
  cleared.counts = { ...cleared.counts, needs_you: 0, blocked: 0 };
  cleared.tasks = cleared.tasks.filter((task) => task.status !== 'blocked');
  await harness.act(async () => { source.context.value = { groups: [cleared] }; });
  assert.ok(c.querySelector('.tb-table'), 'still open');
});

test('a long import that fails part way returns the added results and the items not sent', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTaskboardApi } = await harness.importDashboardModule('js/taskboard-actions.js');
  let n = 0;
  const api = createTaskboardApi({ fetchImpl: async (path, init) => {
    n += 1;
    if (n === 2) return { ok: false, status: 502, json: async () => ({ message: 'bad gateway' }) };
    const body = JSON.parse(init.body);
    return { ok: true, status: 200, json: async () => ({ created: body.refs.length, results: body.refs.map((input) => ({ input, task: {} })) }) };
  } });
  const refs = Array.from({ length: 70 }, (_, i) => `awb:tcl-x${i}`);
  const out = await api.importRefs('crew', refs);
  assert.equal(out.created, 50);
  assert.equal(out.failed.message, 'bad gateway');
  assert.deepEqual(out.failed.remaining, refs.slice(50));
});

test('Assign › lists the members; Unassign takes the task back', async (t) => {
  const { c, api, click, pick } = await mountBoard(t, 'tba-assign');
  await click(c.querySelector('tr[data-task-id="tk_00000004"] .tb-act-more'));
  await click([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].find((b) => b.textContent.trim() === 'Assign ›'));
  assert.deepEqual([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].map((b) => b.textContent.trim()), ['‹ Back', 'You (operator)', 'builder', 'tester (offline)'],
    'members with a stable agent id, online first');
  await click([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].find((b) => b.textContent.trim() === 'tester (offline)'));
  assert.equal(c.querySelector('.gr-add-menu'), null, 'picking closes the menu');
  await pick('tk_00000002', 'Unassign');
  assert.deepEqual(api.calls, [['assign', 'tk_00000004', 'agt_tester'], ['unassign', 'tk_00000002']]);
});

test('a task dragged onto an agent row of its own card is assigned; the unassign zone takes it back', async (t) => {
  const name = 'tba-dragassign';
  const { harness, c, api, rowOf } = await mountBoard(t, name);
  // The group card's member rows, as groups-member-table.js draws them.
  const card = document.createElement('details');
  card.dataset.groupKey = name;
  card.innerHTML = `<table><tbody>
    <tr data-dnd-agent="agt_builder" data-dnd-source-group="${name}"><td class="name-cell">builder</td></tr>
    <tr data-dnd-agent="conv-no-agent" data-dnd-source-group="${name}"><td class="name-cell">plain</td></tr>
  </tbody></table><details data-group-key="sub"><table><tbody>
    <tr data-dnd-agent="agt_sub" data-dnd-source-group="sub"><td class="name-cell">sub</td></tr></tbody></table></details>`;
  document.body.appendChild(card);
  t.after(() => card.remove());
  const transfer = { setData() {}, setDragImage() {}, effectAllowed: '', dropEffect: '' };
  const [builder, plain] = card.querySelectorAll('tr');
  const sub = card.querySelector('[data-group-key="sub"] tr');
  await harness.act(async () => { harness.fireEvent(rowOf('tk_00000001').querySelector('.tb-handle'), 'dragstart', { dataTransfer: transfer }); });
  const overBuilder = harness.fireEvent(builder.querySelector('td'), 'dragover', { dataTransfer: transfer });
  assert.equal(overBuilder.defaultPrevented, true, 'a member row takes the task');
  assert.equal(builder.querySelector('td').dataset.tbAssign, 'assign “first” to builder');
  assert.equal(harness.fireEvent(plain.querySelector('td'), 'dragover', { dataTransfer: transfer }).defaultPrevented, false, 'no stable agent id');
  assert.equal(harness.fireEvent(sub.querySelector('td'), 'dragover', { dataTransfer: transfer }).defaultPrevented, false, "a subgroup's agent");
  assert.equal(builder.classList.contains('tb-assign-over'), false, 'the marker moves off');
  await harness.act(async () => { harness.fireEvent(builder.querySelector('td'), 'drop', { dataTransfer: transfer }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['assign', 'tk_00000001', 'agt_builder']]);
  // The drag is over: the agent rows no longer take drops.
  assert.equal(harness.fireEvent(builder.querySelector('td'), 'dragover', { dataTransfer: transfer }).defaultPrevented, false);

  // Dragging an assigned task shows the unassign zone.
  api.calls.length = 0;
  assert.equal(c.querySelector('.tb-unassign-drop'), null);
  await harness.act(async () => { harness.fireEvent(rowOf('tk_00000002').querySelector('.tb-handle'), 'dragstart', { dataTransfer: transfer }); });
  const zone = c.querySelector('.tb-unassign-drop');
  assert.ok(zone);
  await harness.act(async () => {
    harness.fireEvent(zone, 'dragover', { dataTransfer: transfer });
    harness.fireEvent(zone, 'drop', { dataTransfer: transfer });
    await new Promise((r) => setTimeout(r, 0));
  });
  assert.deepEqual(api.calls, [['unassign', 'tk_00000002']]);
  assert.equal(c.querySelector('.tb-unassign-drop'), null, 'the zone goes with the drag');

  // A blocked task is not offered to an agent: the assign would be refused.
  await harness.act(async () => { harness.fireEvent(rowOf('tk_00000003').querySelector('.tb-handle'), 'dragstart', { dataTransfer: transfer }); });
  assert.equal(harness.fireEvent(builder.querySelector('td'), 'dragover', { dataTransfer: transfer }).defaultPrevented, false, 'blocked');
  await harness.act(async () => { harness.fireEvent(rowOf('tk_00000003').querySelector('.tb-handle'), 'dragend', {}); });

  // A drag whose dragend never came leaves no listeners behind: the next
  // drop assigns once.
  api.calls.length = 0;
  for (let i = 0; i < 2; i++) {
    await harness.act(async () => { harness.fireEvent(rowOf('tk_00000001').querySelector('.tb-handle'), 'dragstart', { dataTransfer: transfer }); });
  }
  await harness.act(async () => { harness.fireEvent(builder.querySelector('td'), 'drop', { dataTransfer: transfer }); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['assign', 'tk_00000001', 'agt_builder']]);
});

test("an agent's row shows the board tasks assigned to it", async (t) => {
  const harness = await createPreactHarness(t);
  const { AgentTaskIndicator } = await harness.importDashboardModule('js/taskboard-agent-indicator.js');
  const entry = board('tba-indicator');
  entry.tasks[3] = { ...entry.tasks[3], assignee: { agent_id: 'agt_builder' } };
  const source = { context: harness.signals.signal({ groups: [entry] }), attach: () => () => {} };
  const group = { name: 'tba-indicator' };
  const mounted = await harness.mount(harness.html`<div>
    <${AgentTaskIndicator} group=${group} member=${{ agent_id: 'agt_builder' }} source=${source} />
    <${AgentTaskIndicator} group=${group} member=${{ agent_id: 'agt_tester' }} source=${source} />
  </div>`);
  const shown = mounted.container.querySelectorAll('.tb-agent-tasks');
  assert.equal(shown.length, 1, 'nothing for an agent with no tasks');
  assert.equal(shown[0].textContent.replace(/\s+/g, ' ').trim(), '☰ 2 tasks · second');

  // A linked node's group reads that node's board, not this node's.
  const peerEntry = board('tba-indicator');
  peerEntry.tasks = [peerEntry.tasks[0]].map((task) => ({ ...task, title: 'on the peer', assignee: { agent_id: 'agt_builder' } }));
  const peerSource = { context: harness.signals.signal({ groups: [peerEntry] }), attach: () => () => {} };
  const asked = [];
  const boards = (node) => { asked.push(node); return { source: peerSource }; };
  const peer = await harness.mount(harness.html`<${AgentTaskIndicator} group=${{ ...group, fleet_node: { id: 'nd_peer', local: false } }}
    member=${{ agent_id: 'agt_builder' }} source=${source} boards=${boards} />`);
  assert.equal(peer.container.textContent.replace(/\s+/g, ' ').trim(), '☰ 1 task · on the peer');
  assert.ok(asked.length && asked.every((node) => node === 'nd_peer'));
  // In the fused view the group is name@node; the peer's board knows it as name.
  const fused = await harness.mount(harness.html`<${AgentTaskIndicator} group=${{ ...group, name: `${group.name}@forge`, fleet_node: { id: 'nd_peer', name: 'forge', local: false } }}
    member=${{ agent_id: 'agt_builder' }} source=${source} boards=${boards} />`);
  assert.equal(fused.container.textContent.replace(/\s+/g, ' ').trim(), '☰ 1 task · on the peer');
});

test('a board whose api cannot assign draws no assign controls', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { cluster, menu } = await mountBoard(t, 'tba-noassign', { api });
  assert.deepEqual(cluster('tk_00000001'), ['Done', 'Edit']);
  assert.deepEqual(await menu('tk_00000001'), ['Block…', 'Drop…']);
  // Closing someone's task is the assignee's or an assigner's (the status
  // route refuses it otherwise): no Done or Drop on an assigned row.
  assert.deepEqual(cluster('tk_00000002'), ['Edit']);
  assert.deepEqual(await menu('tk_00000002'), ['Block…']);
  assert.deepEqual(cluster('tk_00000004'), ['Next', 'Edit']);
  assert.deepEqual(await menu('tk_00000004'), ['Block…', 'Drop…']);
});

test('a task archives from its ⋯ or a right-click, and the history line archives the done tasks', async (t) => {
  const { harness, c, api, rowOf, menu, click } = await mountBoard(t, 'tba-archive');
  assert.ok(!(await menu('tk_00000001')).includes('Archive'), 'a live task is dropped first, which asks why');

  // A right-click on a row opens its ⋯, in place of the browser's menu.
  await click(c.querySelector('.tb-history-toggle'));
  const ctx = await harness.act(async () => harness.fireEvent(rowOf('tk_00000005'), 'contextmenu'));
  assert.ok(c.querySelector('.gr-add-menu'), 'the row menu opens');
  assert.ok([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].some((b) => b.textContent.trim() === 'Archive'));
  void ctx;
  api.calls.length = 0;
  await click([...c.querySelectorAll('.gr-add-menu [role="menuitem"]')].find((b) => b.textContent.trim() === 'Archive'));
  assert.deepEqual(api.calls, [['archive', 'tk_00000005']]);

  // Archive closed ▾ › Done tasks: every done task, or those closed longer
  // ago than an age. A board with no dropped task offers only done.
  api.calls.length = 0;
  await click(c.querySelector('.tb-archive-done-btn'));
  assert.deepEqual([...c.querySelectorAll('[data-archive-kind]')].map((b) => b.dataset.archiveKind), ['done']);
  await click(c.querySelector('[data-archive-kind="done"]'));
  assert.deepEqual([...c.querySelectorAll('[data-archive-age]')].map((b) => b.dataset.archiveAge), ['all', '1d', '7d', '30d']);
  assert.equal(c.querySelector('[data-archive-age="7d"]').getAttribute('title'), 'taskboard archive tba-archive --done --older-than 7d');
  await click(c.querySelector('[data-archive-age="7d"]'));
  assert.deepEqual(api.calls, [['archiveDone', 'tba-archive', '7d']], 'done alone keeps the route every node has');
});

test('Archive closed covers the dropped tasks too: dropped, or done and dropped, with the age', async (t) => {
  const context = { groups: [{ ...board('tba-dropped'), counts: { ...board('tba-dropped').counts, dropped: 1 },
    closed: [...board('tba-dropped').closed, row('tk_00000006', 'parked', { status: 'dropped', closed_at: ago(90) })] }] };
  const { c, api, click } = await mountBoard(t, 'tba-dropped', { context });
  const open = async (kind) => {
    await click(c.querySelector('.tb-archive-done-btn'));
    await click(c.querySelector(`[data-archive-kind="${kind}"]`));
  };
  await click(c.querySelector('.tb-archive-done-btn'));
  assert.equal(c.querySelector('.tb-archive-done-btn').textContent.trim(), 'Archive closed ▾');
  assert.deepEqual([...c.querySelectorAll('[data-archive-kind]')].map((b) => b.dataset.archiveKind), ['done', 'dropped', 'both']);
  await click(c.querySelector('.tb-archive-done-btn'));

  await open('dropped');
  assert.equal(c.querySelector('[data-archive-age="all"]').getAttribute('title'), 'taskboard archive tba-dropped --dropped');
  await click(c.querySelector('[data-archive-age="all"]'));
  await open('both');
  assert.equal(c.querySelector('[data-archive-age="30d"]').getAttribute('title'), 'taskboard archive tba-dropped --done --dropped --older-than 30d');
  await click(c.querySelector('[data-archive-age="30d"]'));
  assert.deepEqual(api.calls, [['archiveClosed', 'tba-dropped', ['dropped'], ''], ['archiveClosed', 'tba-dropped', ['done', 'dropped'], '30d']]);
});

test('a board whose api predates archiveClosed offers done only', async (t) => {
  const context = { groups: [{ ...board('tba-old'), closed: [...board('tba-old').closed, row('tk_00000006', 'parked', { status: 'dropped' })] }] };
  const api = fakeApi();
  delete api.archiveClosed;
  const { c, click } = await mountBoard(t, 'tba-old', { context, api });
  await click(c.querySelector('.tb-archive-done-btn'));
  assert.deepEqual([...c.querySelectorAll('[data-archive-kind]')].map((b) => b.dataset.archiveKind), ['done']);
});

test('a tracker ticket is added by its link, and a label never stands in for a bare id', async (t) => {
  const harness = await createPreactHarness(t);
  const { trackerTaskPlan, sameRef } = await harness.importDashboardModule('js/groups-taskboard-tracker.js');
  const linked = trackerTaskPlan({ ref: 'LIN-7', link: 'https://linear.app/acme/issue/LIN-7', title: 'Checkout', label: 'Cart', start: 'next' });
  assert.equal(linked.task.ref, 'https://linear.app/acme/issue/LIN-7');
  assert.equal(linked.task.refLabel, 'Cart');
  assert.equal(linked.task.stage, 'next');
  const bare = trackerTaskPlan({ ref: 'LIN-7', title: 'Checkout', label: 'Cart' });
  assert.equal(bare.task.ref, 'LIN-7');
  assert.equal(bare.task.refLabel, '', 'with no link the id is the ref\'s name');
  const live = [{ id: 'tk_1', title: 'x', status: 'open', ref: { kind: 'linear', label: 'LIN-7', url: 'https://linear.app/acme/issue/LIN-7' } }];
  assert.equal(sameRef(live, { kind: 'linear', label: 'LIN-7', url: 'https://linear.app/acme/issue/LIN-7/' })?.id, 'tk_1');
});
