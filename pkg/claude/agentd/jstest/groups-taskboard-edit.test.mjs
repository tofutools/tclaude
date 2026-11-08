// The Task board's edit form (groups-taskboard-edit.js) and a task's files
// (groups-taskboard-files.js, taskboard-actions.js): one Save writes only
// what changed, each through its own route, and names what was refused.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { comboChoose, comboKeys } from './combobox-helpers.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const task = (extra = {}) => ({ id: 'tk_00000001', group: 'crew', title: 'Release notes', body: 'draft them', stage: 'next', status: 'open',
  depends_on: [], created_by: 'human:operator', created_at: ago(60), attachments: [], ...extra });

function fakeApi({ refuse = {} } = {}) {
  const calls = [];
  const record = (verb) => async (...args) => {
    calls.push([verb, ...args]);
    if (refuse[verb]) throw new Error(refuse[verb]);
    return {};
  };
  const api = { calls };
  for (const verb of ['edit', 'stage', 'status', 'assign', 'unassign', 'attach', 'detach', 'create', 'ask', 'capture']) api[verb] = record(verb);
  api.fileData = async () => ({ data: '', content_type: 'image/png' });
  return api;
}

test('the save plan writes only what changed, in an order the routes accept', async (t) => {
  const harness = await createPreactHarness(t);
  const { editPlan, draftOf, setWhere, setAssignee } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const names = (steps) => steps.map((s) => s.name);
  const open = task();
  assert.deepEqual(editPlan(open, draftOf(open)), [], 'an untouched form writes nothing');
  assert.deepEqual(names(editPlan(open, { ...draftOf(open), title: 'Release 2.4 notes', newRef: 'SHOP-1', deps: ['tk_00000002'] })),
    ['title, ref, dependencies']);
  // A current node sends refs: [] for a task with none, and takes several.
  const { refsPlan } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const none = task({ refs: [] });
  assert.deepEqual(refsPlan(none, { ...draftOf(none), refs: ['LIN-1'], newRef: 'tcl-a1b2c3' }),
    { addRefs: ['LIN-1', 'tcl-a1b2c3'], resolveRef: true });

  // Blocked, with its reason, and given to someone: assign while open, then block.
  let draft = setAssignee(draftOf(open), 'agt_dev');
  assert.equal(draft.where, 'progress', 'an assignee puts a queued task in progress');
  draft = { ...setWhere(draft, 'blocked'), reason: 'QA sign-off' };
  assert.equal(draft.to, 'agt_dev', 'blocking keeps the assignee');
  assert.deepEqual(names(editPlan(open, draft)), ['assignee', 'block']);

  // A blocked task given to someone new opens first, and is blocked again.
  const blocked = task({ status: 'blocked', block_reason: 'QA' });
  assert.deepEqual(names(editPlan(blocked, setAssignee(draftOf(blocked), 'agt_dev'))), ['unblock', 'assignee', 'block']);
  // Unblocking into Next.
  assert.deepEqual(names(editPlan(blocked, setWhere(draftOf(blocked), 'next'))), ['unblock']);
  // A task in progress back to the ideas: its assignee holds it there, not
  // started; unassigned, it is demoted.
  const held = task({ assignee: { agent_id: 'agt_dev' } });
  assert.equal(draftOf(held).where, 'progress');
  const calls = [];
  const rec = new Proxy({}, { get: (_, verb) => (...args) => { calls.push([verb, ...args]); } });
  const holdSteps = editPlan(held, setWhere(draftOf(held), 'idea'));
  assert.deepEqual(names(holdSteps), ['not started']);
  holdSteps[0].run(rec);
  assert.deepEqual(calls.splice(0), [['hold', 'tk_00000001', 'idea']]);
  assert.deepEqual(names(editPlan(held, setAssignee(setWhere(draftOf(held), 'idea'), ''))), ['unassign', 'back to the ideas']);
  // A held task (assigned, not started) reads as its stage, and starts.
  const heldIdea = task({ stage: 'idea', assignee: { agent_id: 'agt_dev' }, held: true });
  assert.equal(draftOf(heldIdea).where, 'idea');
  const startSteps = editPlan(heldIdea, setWhere(draftOf(heldIdea), 'progress'));
  assert.deepEqual(names(startSteps), ['start']);
  startSteps[0].run(rec);
  assert.deepEqual(calls.splice(0), [['start', 'tk_00000001']]);
  // An idea given to someone: the assignment queues it, no separate promote;
  // given to someone held as an idea, it stays one.
  const idea = task({ stage: 'idea' });
  assert.deepEqual(names(editPlan(idea, setAssignee(draftOf(idea), 'agt_dev'))), ['assignee']);
  const giveSteps = editPlan(idea, setWhere(setAssignee(draftOf(idea), 'agt_dev'), 'idea'));
  assert.deepEqual(names(giveSteps), ['assignee']);
  giveSteps[0].run(rec);
  assert.deepEqual(calls.splice(0), [['assign', 'tk_00000001', 'agt_dev', { hold: 'idea' }]]);
  // A done task reopened into Next, and one dropped with its reason.
  const done = task({ status: 'done', stage: 'idea' });
  assert.deepEqual(names(editPlan(done, setWhere(draftOf(done), 'next'))), ['reopen', 'into Next']);
  assert.deepEqual(names(editPlan(open, { ...setWhere(draftOf(open), 'dropped'), reason: 'parked in awb' })), ['drop']);
  // A blocked idea stays an idea: blocking keeps the stage, and so does any
  // other edit of it.
  const blockedIdea = task({ stage: 'idea', status: 'blocked', block_reason: 'QA' });
  assert.deepEqual(editPlan(blockedIdea, draftOf(blockedIdea)), [], 'an untouched blocked idea writes nothing');
  assert.deepEqual(names(editPlan(blockedIdea, { ...draftOf(blockedIdea), title: 'renamed' })), ['title']);
  // A done task blocked: reopened first, as a closed task cannot be blocked.
  assert.deepEqual(names(editPlan(done, { ...setWhere(draftOf(done), 'blocked'), reason: 'QA' })), ['reopen', 'block']);
  // Files: one removed, two added.
  const filed = task({ attachments: [{ id: 7, name: 'shot.png' }] });
  assert.deepEqual(names(editPlan(filed, draftOf(filed), { remove: [7], add: [{ name: 'a.md' }, { name: 'b.md' }] })),
    ['remove shot.png', 'attach 2 files']);
});

async function mountEdit(t, the, { api = fakeApi(), canAssign = true, canClose = true, me = 'human:operator' } = {}) {
  const harness = await createPreactHarness(t);
  const { EditTaskDialog } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  let closed = false;
  let saved = 0;
  const group = { name: 'crew', members: [] };
  const members = [{ agent_id: 'agt_dev', title: 'dev', online: true }];
  await harness.mount(harness.html`<${EditTaskDialog} group=${group} entry=${{ tasks: [the, task({ id: 'tk_00000002', title: 'other' })] }} task=${the}
    api=${api} me=${me} canAssign=${canAssign} canClose=${canClose} members=${members} onClose=${() => { closed = true; }} onSaved=${() => { saved += 1; }} />`);
  const dialog = harness.document.querySelector('#taskboard-edit-modal');
  const flush = () => harness.act(() => new Promise((r) => setTimeout(r, 0)));
  const click = async (el) => { await harness.act(() => harness.fireEvent(el, 'click')); await flush(); };
  const where = () => dialog.querySelector('[data-stage-pick="taskboard-edit-where"]');
  const pick = (key) => comboChoose(harness, where(), key);
  const radio = (key) => ({ get disabled() { return where().querySelector(`[role="option"][data-key="${key}"]`)?.getAttribute('aria-disabled') === 'true'; } });
  const select = async (el, value) => {
    for (const option of el.querySelectorAll('option')) {
      const on = option.getAttribute('value') === value;
      option.selected = on;
      if (on) option.setAttribute('selected', ''); else option.removeAttribute('selected');
    }
    await harness.act(() => harness.fireEvent(el, 'change'));
  };
  return { harness, dialog, api, click, pick, radio, where, select, flush, closed: () => closed, saved: () => saved,
    save: () => click(dialog.querySelector('#taskboard-edit-save')) };
}

test('a node with one ref per task: the ref is replaced through ref, linked through its tracker on Save', async (t) => {
  const the = task({ ref: { kind: 'awb', url: 'https://awb.example/#/issues/tcl-a1b2c3', label: 'tcl-a1b2c3' } });
  const { harness, dialog, api, click, save } = await mountEdit(t, the);
  const link = dialog.querySelector('.tb-ref-row a.tb-ref');
  assert.equal(link.getAttribute('href'), 'https://awb.example/#/issues/tcl-a1b2c3');
  assert.match(link.textContent, /tcl-a1b2c3/);
  assert.equal(dialog.querySelector('.tb-edit-ref').disabled, true, 'one ref is all such a node keeps');
  await click(dialog.querySelector('.tb-ref-rm'));
  assert.equal(dialog.querySelector('.tb-ref-row'), null, 'the ref is off');
  await harness.input(dialog.querySelector('.tb-edit-ref'), 'LIN-12');
  await save();
  assert.deepEqual(api.calls.find(([verb]) => verb === 'edit'), ['edit', 'tk_00000001', { ref: 'LIN-12', resolveRef: true }]);
});

const prRef = { kind: 'url', url: 'https://github.com/acme/shop/pull/42', label: 'acme/shop#42',
  pr: { number: 42, state: 'open', title: 'Ship the notes', checks: { total: 3, passed: 3, failed: 0, pending: 0, skipped: 0, state: 'passing' } } };
const awbRef = { kind: 'awb', url: 'https://awb.example/#/issues/tcl-a1b2c3', label: 'tcl-a1b2c3', tracker: { title: 'Notes', status: 'open' } };

test('several refs: each shows by its kind, an issue is looked up on open, and Save takes refs off and adds new ones', async (t) => {
  const the = task({ ref: prRef, refs: [prRef, awbRef] });
  const api = fakeApi();
  api.lookupRef = async (group, ref) => { api.calls.push(['lookupRef', group, ref]); return { title: 'Release notes v2', status: 'In Progress' }; };
  const { harness, dialog, click, save, flush } = await mountEdit(t, the, { api });
  await flush();
  const rows = [...dialog.querySelectorAll('.tb-ref-row')];
  assert.equal(rows.length, 2);
  assert.ok(rows[0].querySelector('.pr-chain'), 'a PR is the agent rows\' chain');
  assert.match(rows[0].textContent, /#42/);
  assert.match(rows[0].textContent, /Ship the notes/);
  assert.match(rows[0].textContent, /GitHub pull request · acme\/shop/);
  assert.deepEqual(api.calls.filter(([verb]) => verb === 'lookupRef'), [['lookupRef', 'crew', 'tcl-a1b2c3']], 'only the issue is looked up');
  const status = rows[1].querySelector('.tb-ref-status');
  assert.equal(status.textContent, 'In Progress', 'the lookup is newer than the board\'s');
  assert.ok(status.classList.contains('ui-tag') && status.classList.contains('ui-tag-warn'));
  assert.match(rows[1].textContent, /Release notes v2/);

  await click(rows[0].querySelector('.tb-ref-rm'));
  const input = dialog.querySelector('.tb-edit-ref');
  await harness.input(input, 'LIN-12');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.equal(input.value, '', 'Enter adds it to the list');
  assert.match(dialog.querySelector('.tb-refs-list').textContent, /LIN-12.*added on Save/);
  await save();
  assert.deepEqual(api.calls.find(([verb]) => verb === 'edit'),
    ['edit', 'tk_00000001', { rmRefs: ['https://github.com/acme/shop/pull/42'], addRefs: ['LIN-12'], resolveRef: true }]);
});

test('the board shows every ref by kind on a card and the first and +n in the List', async (t) => {
  const harness = await createPreactHarness(t);
  const { RefChip, trackerStatusClass } = await harness.importDashboardModule('js/groups-taskboard-card.js');
  const the = task({ ref: prRef, refs: [prRef, awbRef, { kind: 'url', url: 'https://docs.example/x', label: 'docs.example' }] });
  const root = await harness.mount(harness.html`<div><span id="card"><${RefChip} task=${the} /></span><span id="list"><${RefChip} task=${the} max=${1} /></span></div>`);
  const card = harness.document.querySelector('#card');
  assert.ok(card.querySelector('.pr-chain'));
  assert.equal(card.querySelector('.tb-ref-issue .tb-ref-status').textContent, 'open');
  assert.equal(card.querySelector('a.ui-tag.tb-ref').getAttribute('href'), 'https://docs.example/x');
  const list = harness.document.querySelector('#list');
  assert.ok(list.querySelector('.pr-chain'));
  assert.equal(list.querySelector('.tb-refs-more').textContent, '+2');
  assert.equal(list.querySelector('.tb-ref-issue'), null);
  assert.ok(root);
  assert.equal(trackerStatusClass('In Review'), 'ui-tag-accent');
  assert.equal(trackerStatusClass('Done'), '');
  assert.equal(trackerStatusClass('Backlog'), '');
});

test('Where it is, the assignee and a file change land as their own writes', async (t) => {
  const the = task({ attachments: [{ id: 7, name: 'old.log', size: 10, content_type: 'text/plain' }] });
  const { harness, dialog, api, pick, where, select, click, save, closed, saved } = await mountEdit(t, the);
  assert.equal(where().getAttribute('data-value'), 'next');
  await select(dialog.querySelector('.tb-edit-assignee'), 'agt_dev');
  await pick('blocked');
  assert.equal(dialog.querySelector('#taskboard-edit-save').disabled, true, 'a block needs its reason');
  await harness.input(dialog.querySelector('.tb-edit-reason'), 'waiting on QA');
  await click(dialog.querySelector('[data-file="7"] .tb-file-rm'));
  assert.match(dialog.querySelector('[data-file="7"]').textContent, /removed on Save/);
  await save();
  assert.deepEqual(api.calls, [
    ['assign', 'tk_00000001', 'agt_dev'],
    ['status', 'tk_00000001', 'blocked', 'waiting on QA'],
    ['detach', 'tk_00000001', 7],
  ]);
  assert.equal(closed(), true);
  assert.equal(saved(), 1);
});

test('a refused write is named, the rest land, and the form stays open', async (t) => {
  const api = fakeApi({ refuse: { status: 'requires taskboard.assign' } });
  const { harness, dialog, pick, save, closed } = await mountEdit(t, task(), { api });
  await harness.input(dialog.querySelector('.tb-edit-title'), 'Release 2.4 notes');
  await pick('done');
  await save();
  assert.deepEqual(api.calls.map((c) => c[0]), ['edit', 'status']);
  assert.equal(closed(), false);
  const error = harness.document.querySelector('#taskboard-edit-error').textContent;
  assert.match(error, /Saved: title\./);
  assert.match(error, /Not saved: done \(requires taskboard\.assign\)/);
});

test('without can.assign there is no assignee picker, and In progress is off', async (t) => {
  const { harness, dialog, radio, where } = await mountEdit(t, task(), { canAssign: false, canClose: false });
  assert.equal(dialog.querySelector('.tb-edit-assignee'), null);
  await comboKeys(harness, where());
  assert.equal(radio('progress').disabled, true);
  assert.equal(radio('done').disabled, true);
  assert.equal(radio('dropped').disabled, true);
  assert.equal(radio('blocked').disabled, false);
});

test('a file dropped on the edit form is attached on Save; a refused type is left out', async (t) => {
  const { harness, dialog, api, save } = await mountEdit(t, task());
  const notes = new Blob(['# notes'], { type: 'text/markdown' });
  Object.defineProperty(notes, 'name', { value: 'notes.md' });
  const tool = new Blob(['MZ'], { type: 'application/octet-stream' });
  Object.defineProperty(tool, 'name', { value: 'tool.exe' });
  await harness.act(() => harness.fireEvent(dialog, 'drop', { dataTransfer: { types: ['Files'], files: [notes, tool] } }));
  assert.equal(dialog.querySelectorAll('.tb-file-new').length, 1, 'only the allowed file is picked');
  await save();
  assert.equal(api.calls.length, 1);
  assert.equal(api.calls[0][0], 'attach');
  assert.deepEqual(api.calls[0][2].map((f) => f.name), ['notes.md']);
});

test('files go with a create and an ask as multipart; a linked node gets them through the staged upload', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTaskboardApi, taskFileRefusal } = await harness.importDashboardModule('js/taskboard-actions.js');
  const sent = [];
  const ok = (body) => ({ ok: true, status: 201, json: async () => body });
  const api = createTaskboardApi({
    fetchImpl: async (path, init) => { sent.push(['fetch', path, init.method, typeof init.body]); return ok({ id: 'tk_1' }); },
    upload: async (path, init) => { sent.push(['upload', path, init.body instanceof FormData]); return ok({ id: 'tk_1' }); },
  });
  const file = new Blob(['x'], { type: 'text/plain' });
  Object.defineProperty(file, 'name', { value: 'a.txt' });
  await api.create('crew', { title: 'one', files: [file] });
  await api.create('crew', { title: 'two' });
  await api.ask('crew', 'agt_lead', 'please', [file]);
  await api.attach('tk_1', [file]);
  await api.detach('tk_1', 7);
  assert.deepEqual(sent, [
    ['upload', '/v1/groups/crew/taskboard/tasks', true],
    ['fetch', '/v1/groups/crew/taskboard/tasks', 'POST', 'string'],
    ['upload', '/v1/groups/crew/taskboard/ask', true],
    ['upload', '/v1/taskboard/tasks/tk_1/attachments', true],
    ['fetch', '/v1/taskboard/tasks/tk_1/attachments/7', 'DELETE', 'undefined'],
  ]);
  assert.equal(taskFileRefusal(null, 'shot.PNG'), '');
  assert.match(taskFileRefusal(null, 'page.html'), /type not allowed/);
  assert.match(taskFileRefusal(null, 'Makefile'), /type not allowed/);

  // A linked node's board: the files go through this node's staged upload.
  const { nodeTaskboard } = await harness.importDashboardModule('js/groups-taskboard.js');
  const posted = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (path, init) => { posted.push([path, init.method]); return ok({ id: 'tk_2' }); };
  t.after(() => { globalThis.fetch = realFetch; });
  await nodeTaskboard('inst_0123456789abcdef').api.attach('tk_2', [file]);
  assert.equal(posted.length, 1);
  assert.match(posted[0][0], /^\/api\/peer-upload\/inst_0123456789abcdef\?path=%2Fv1%2Ftaskboard%2Ftasks%2Ftk_2%2Fattachments$/);
  assert.equal(posted[0][1], 'POST');
});

test('a screenshot pasted into the New task form goes with the add', async (t) => {
  const harness = await createPreactHarness(t);
  const { NewTaskDialog } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  const api = fakeApi();
  api.create = async (...args) => { api.calls.push(['create', ...args]); return { id: 'tk_9' }; };
  await harness.mount(harness.html`<${NewTaskDialog} group=${{ name: 'crew', members: [] }} entry=${{ tasks: [] }} api=${api} onClose=${() => {}} />`);
  const dialog = harness.document.querySelector('#taskboard-new-modal');
  const shot = new Blob(['png'], { type: 'image/png' });
  // Preact listens for 'paste' only where the element has onpaste; LinkeDOM's
  // has not, so it listens for 'Paste'.
  const paste = 'onpaste' in dialog ? 'paste' : 'Paste';
  await harness.act(() => harness.fireEvent(dialog, paste, { clipboardData: { files: [shot], items: [] } }));
  assert.equal(dialog.querySelectorAll('.tb-files-list li').length, 1, 'the screenshot is listed');
  await harness.input(dialog.querySelector('.tb-new-title'), 'Checkout button overlaps');
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('#taskboard-new-submit'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(api.calls.length, 1);
  const [verb, group, fields] = api.calls[0];
  assert.equal(verb, 'create');
  assert.equal(group, 'crew');
  assert.equal(fields.title, 'Checkout button overlaps');
  assert.equal(fields.files.length, 1);
  assert.equal(fields.files[0].file, shot);
  assert.match(fields.files[0].name, /^pasted-\d{8}-\d{6}-\d+\.png$/, 'a nameless screenshot is sent under the name it was listed with');
  assert.equal(fields.body, '', 'a paste outside the brief links nothing there');
});

test('a screenshot pasted into the brief is a task file and linked in the brief where it was pasted', async (t) => {
  const harness = await createPreactHarness(t);
  const { NewTaskDialog } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  const api = fakeApi();
  api.create = async (...args) => { api.calls.push(['create', ...args]); return { id: 'tk_9' }; };
  await harness.mount(harness.html`<${NewTaskDialog} group=${{ name: 'crew', members: [] }} entry=${{ tasks: [] }} api=${api} onClose=${() => {}} />`);
  const dialog = harness.document.querySelector('#taskboard-new-modal');
  const brief = dialog.querySelector('.tb-new-body textarea');
  await harness.input(brief, 'Look:');
  brief.selectionStart = 5;
  brief.selectionEnd = 5;
  const shot = new Blob(['png'], { type: 'image/png' });
  const paste = 'onpaste' in dialog ? 'paste' : 'Paste';
  await harness.act(() => harness.fireEvent(brief, paste, { clipboardData: { files: [shot], items: [] } }));
  assert.equal(dialog.querySelectorAll('.tb-files-list li').length, 1, 'the screenshot is a file of the task');
  assert.match(brief.value, /^Look:\n!\[pasted-\d{8}-\d{6}-\d+\]\(pasted-\d{8}-\d{6}-\d+\.png\)\n$/, 'and the brief shows it inline');
  await harness.input(dialog.querySelector('.tb-new-title'), 'Checkout button overlaps');
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('#taskboard-new-submit'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  const [, , fields] = api.calls[0];
  assert.ok(fields.body.includes(`(${fields.files[0].name})`), 'the link names the file sent with the add');
});

test('the board context has no brief: the form reads the task in full', async (t) => {
  // From the context: no body.
  const listed = task({ body: undefined });
  const api = fakeApi();
  api.task = async (id) => { api.calls.push(['task', id]); return { ...listed, body: 'the real brief' }; };
  const { harness, dialog, save } = await mountEdit(t, listed, { api });
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.equal(dialog.querySelector('.tb-edit-body textarea').value, 'the real brief', 'the brief is read in full');
  await harness.input(dialog.querySelector('.tb-edit-title'), 'Release 2.4 notes');
  await save();
  assert.deepEqual(api.calls, [['task', 'tk_00000001'], ['edit', 'tk_00000001', { title: 'Release 2.4 notes' }]],
    'an unchanged brief is not sent');
});

test('a brief the form could not read is left as it is', async (t) => {
  const listed = task({ body: undefined });
  const failing = fakeApi();
  failing.task = async () => { throw new Error('offline'); };
  const other = await mountEdit(t, listed, { api: failing });
  await other.harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.equal(other.dialog.querySelector('.tb-edit-body textarea').disabled, true);
  assert.match(other.dialog.textContent, /the brief could not be read/);
  await other.harness.input(other.dialog.querySelector('.tb-edit-title'), 'renamed');
  await other.save();
  assert.deepEqual(failing.calls, [['edit', 'tk_00000001', { title: 'renamed' }]], 'the brief is left as it is');
});

test('what others change while the form is open stands: Save writes only the form\'s changes', async (t) => {
  const harness = await createPreactHarness(t);
  const { EditTaskDialog } = await harness.importDashboardModule('js/groups-taskboard-edit.js');
  const api = fakeApi();
  let setLive;
  function Board() {
    const [live, set] = harness.hooks.useState(task());
    setLive = set;
    return harness.html`<${EditTaskDialog} group=${{ name: 'crew', members: [] }} entry=${{ tasks: [live] }} task=${live}
      api=${api} canAssign=${true} members=${[{ agent_id: 'agt_dev', title: 'dev', online: true }]} onClose=${() => {}} />`;
  }
  await harness.mount(harness.html`<${Board} />`);
  const dialog = harness.document.querySelector('#taskboard-edit-modal');
  await harness.input(dialog.querySelector('.tb-edit-title'), 'Release 2.4 notes');
  // Meanwhile an agent takes the task and adds a dependency (the board polls).
  await harness.act(() => setLive(task({ assignee: { agent_id: 'agt_dev' }, depends_on: ['tk_00000002'] })));
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('#taskboard-edit-save'), 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(api.calls, [['edit', 'tk_00000001', { title: 'Release 2.4 notes' }]],
    'the agent keeps the task, and its dependency stays');
});

test('a dirty Ask an agent dialog asks before it closes', async (t) => {
  const harness = await createPreactHarness(t);
  const { AskAgentDialog } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  let closed = false;
  const group = { name: 'crew', members: [{ agent_id: 'agt_dev', title: 'dev', online: true }] };
  await harness.mount(harness.html`<${AskAgentDialog} group=${group} entry=${{ tasks: [] }} api=${fakeApi()} members=${group.members}
    onClose=${() => { closed = true; }} />`);
  const dialog = harness.document.querySelector('#taskboard-ask-modal');
  const prompt = dialog.querySelector('textarea');
  await harness.input(prompt, 'split the release notes into tasks');
  const cancel = [...dialog.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Cancel');
  const { shellState } = await harness.importDashboardModule('js/shell-state.js');
  await harness.act(async () => { harness.fireEvent(cancel, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(shellState.confirmation.value?.title, 'Discard input?', 'typed input is not dropped without asking');
  assert.equal(closed, false);
  await harness.act(async () => { shellState.resolveConfirmation(true); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(closed, true, 'the dialog closes once the discard is confirmed');
});

test('a comment being written keeps the form open: Save does not send it, and closing asks first', async (t) => {
  const api = fakeApi();
  api.activity = async (id) => ({ id, entries: [] });
  const { harness, dialog, save, closed, flush } = await mountEdit(t, task(), { api });
  await flush();
  assert.match(dialog.textContent, /no comments yet/);
  await harness.input(dialog.querySelector('.tb-comment-new textarea'), 'half a thought');
  await flush();
  await save();
  assert.equal(closed(), false, 'an unsent comment is not lost to Save');
  assert.deepEqual(api.calls, [], 'and Save sends nothing it was not asked to');
  const { shellState } = await harness.importDashboardModule('js/shell-state.js');
  const cancel = [...dialog.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Cancel');
  await harness.act(async () => { harness.fireEvent(cancel, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.equal(shellState.confirmation.value?.title, 'Discard input?', 'closing asks before the comment is lost');
  await harness.act(async () => { shellState.resolveConfirmation(false); });
});

test('files dropped or pasted on the comment box go with the comment, not the task', async (t) => {
  const api = fakeApi();
  api.activity = async (id) => ({ id, entries: [{ id: 1, at: ago(5), actor: 'human:operator', kind: 'comment',
    comment: { id: 1, kind: 'comment', body: 'mine', author: 'human:operator', at: ago(5) } }] });
  api.comment = async (...args) => { api.calls.push(['comment', ...args]); return {}; };
  const { harness, dialog, save, flush } = await mountEdit(t, task(), { api });
  await flush();
  const composer = dialog.querySelector('.tb-comment-composer');
  const shot = new File(['png'], 'shot.png', { type: 'image/png' });
  await harness.act(() => harness.fireEvent(composer.querySelector('.tb-comment-new textarea'), 'drop', { dataTransfer: { types: ['Files'], files: [shot] } }));
  const paste = 'onpaste' in composer ? 'paste' : 'Paste';
  const log = new File(['x'], 'cart.log', { type: 'text/plain' });
  await harness.act(() => harness.fireEvent(composer.querySelector('.tb-comment-new textarea'), paste, { clipboardData: { files: [log], items: [] } }));
  assert.deepEqual([...composer.querySelectorAll('.att-name')].map((n) => n.textContent), ['shot.png', 'cart.log'], 'the comment holds them');
  assert.equal(dialog.querySelectorAll('[data-file-new]').length, 0, 'the task\'s own files do not');
  await harness.input(composer.querySelector('.tb-comment-new textarea'), 'see the shot');
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('#taskboard-comment-send'), 'click'); });
  await flush();
  const sent = api.calls.find((c) => c[0] === 'comment');
  assert.deepEqual(sent[4].map((f) => f.name), ['shot.png', 'cart.log']);

  // An open edit of a comment is unsaved input: Save waits, and writes nothing.
  await harness.input(dialog.querySelector('.tb-edit-title'), 'renamed');
  await harness.act(() => harness.fireEvent(dialog.querySelector('[data-comment="1"] .tb-comment-edit'), 'click'));
  await harness.input(dialog.querySelector('.tb-comment-edit-body textarea'), 'mine, reworded');
  await flush();
  await save();
  assert.equal(api.calls.some((c) => c[0] === 'edit'), false, 'Save does not run while a comment is being edited');
});

// Questions in the edit form (tk_be8d0455): a task's questions head the
// form, the open ones with Answer…, which opens the same Answer dialog as
// the board and answers at once, not with Save.
test('the edit form lists the task\'s questions; Answer… opens the Answer dialog and answers at once', async (t) => {
  const the = task({ assignee: { agent_id: 'agt_dev', name: 'dev' }, questions: [
    { kind: 'question', n: 1, title: 'Old one', asked_by: 'agent:agt_dev', state: 'answered', options: [{ key: 'a', label: 'Alpha' }],
      answer: { picked: ['a'], text: 'fine', answered_by: 'human:operator', answered_at: ago(5) } },
    { kind: 'question', n: 2, title: 'Behind a flag?', body: 'The **flag** question.', asked_by: 'agent:agt_dev', state: 'open', recommend: 'flag',
      options: [{ key: 'flag', label: 'Behind a flag', description: 'safer', preview: 'if (flag) {…}' }, { key: 'on', label: 'On by default' }] },
  ], open_questions: 1 });
  const api = fakeApi();
  api.answer = async (...args) => { api.calls.push(['answer', ...args]); return {}; };
  const { harness, dialog, click, flush, saved } = await mountEdit(t, the, { api });
  const rows = [...dialog.querySelectorAll('.tb-edit-question')];
  assert.deepEqual(rows.map((r) => r.dataset.editQuestion), ['2', '1'], 'the open question comes first');
  assert.match(rows[0].textContent, /Q2 · Behind a flag\?[\s\S]*pick one · 2/);
  assert.match(rows[1].textContent, /answered[\s\S]*Picked Alpha — “fine”/);
  assert.equal(rows[1].querySelector('.tb-answer-open'), null, 'an answered question offers no Answer');
  await click(rows[0].querySelector('.tb-answer-open'));
  const answer = harness.document.querySelector('#taskboard-answer-modal');
  assert.ok(answer, 'Answer… opens the Answer dialog');
  assert.ok(!dialog.contains(answer), 'beside the form, not inside it');
  assert.match(answer.textContent, /Q2 on “Release notes”/);
  for (let i = 0; i < 100 && !answer.querySelector('.tb-answer-context strong'); i += 1) await flush();
  assert.ok(answer.querySelector('.tb-answer-context strong'), 'the question\'s context is rendered Markdown');
  const flag = answer.querySelector('[data-answer-option="flag"]');
  assert.ok(flag.classList.contains('tb-answer-recommended'));
  assert.match(flag.textContent, /Behind a flag[\s\S]*recommended[\s\S]*safer[\s\S]*if \(flag\)/, 'the option shows its label, description and preview');
  await harness.act(async () => { harness.fireEvent(answer.querySelector('[data-answer-option="on"] input'), 'change'); });
  await click(answer.querySelector('#taskboard-answer-submit'));
  assert.deepEqual(api.calls.filter((c) => c[0] === 'answer'), [['answer', 'tk_00000001', ['on'], '', 2]]);
  assert.equal(harness.document.querySelector('#taskboard-answer-modal'), null, 'the dialog closes once the answer landed');
  assert.equal(saved(), 1, 'the board is read again');
  assert.ok(harness.document.querySelector('#taskboard-edit-modal'), 'the form stays open');
});

test('without the assign grant the form shows the questions, with no Answer', async (t) => {
  const the = task({ questions: [{ kind: 'question', n: 1, title: 'Q?', state: 'open', options: [{ key: 'a', label: 'A' }] }], open_questions: 1 });
  const api = fakeApi();
  api.answer = async () => ({});
  api.can = { assign: false };
  const { dialog } = await mountEdit(t, the, { api });
  assert.ok(dialog.querySelector('.tb-edit-question'));
  assert.equal(dialog.querySelector('.tb-answer-open'), null);
});
