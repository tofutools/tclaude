// "+ New task" (groups-taskboard-new.js, tcl-kp5ngc): the add menu, the
// Write it myself form and its submit, and Ask an agent.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import { comboChoose, comboKeys } from './combobox-helpers.mjs';

const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], ...extra });

function board(group, extra = {}) {
  return {
    group, group_id: 1,
    counts: { live: 2, next: 1, ideas: 1 },
    tasks: [row('tk_00000001', 'first'), row('tk_00000002', 'an idea', { stage: 'idea' })],
    closed: [],
    ...extra,
  };
}

// The owner is listed as the snapshot lists a non-member owner (role
// "owner"); the server takes an owner as well as a member.
const members = () => [
  { agent_id: 'agt_dev', title: 'dev', role: 'dev', online: true },
  { agent_id: 'agt_lead', title: 'lead', role: 'owner', online: true, owner: true },
  { agent_id: 'agt_away', title: 'away', online: false },
];

// fakeApi records each write as [verb, ...args]; `hold` makes create wait
// until released, `refuse` makes the named verb throw.
function fakeApi({ hold = false, refuse = '' } = {}) {
  const calls = [];
  let release = () => {};
  const answer = (verb, value) => async (...args) => {
    calls.push([verb, ...args]);
    if (refuse === verb) throw new Error(`${verb} refused`);
    if (verb === 'create' && hold) await new Promise((resolve) => { release = resolve; });
    return value(...args);
  };
  return {
    calls, release: () => release(),
    create: answer('create', (group, task) => ({ id: 'tk_00000009', title: task.title })),
    capture: answer('capture', (group, text) => ({ created: text.split('\n').length, results: text.split('\n').map((input, i) => ({ input, task: { id: `tk_0000001${i}` } })) })),
    edit: answer('edit', (id) => ({ id })),
    assign: answer('assign', () => ({})),
    ask: answer('ask', () => ({ message_id: 7 })),
    importRefs: answer('importRefs', () => ({ created: 0, results: [] })),
    move: answer('move', () => ({})), stage: answer('stage', () => ({})), status: answer('status', () => ({})), unassign: answer('unassign', () => ({})),
  };
}

// selectValue picks an option as a browser does (LinkeDOM's select.value
// has no setter).
async function selectValue(harness, select, value) {
  for (const option of select.querySelectorAll('option')) { option.removeAttribute('selected'); option.selected = false; }
  const picked = select.querySelector(`option[value="${value}"]`);
  picked.setAttribute('selected', '');
  picked.selected = true;
  await harness.act(() => harness.fireEvent(select, 'change'));
}

async function mountBoard(t, name, { api = fakeApi(), entry = board(name), groupMembers = members() } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  let refreshes = 0;
  const source = {
    context: harness.signals.signal({ groups: [entry] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => { refreshes += 1; },
  };
  const group = { name, members: groupMembers };
  const mounted = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const c = mounted.container;
  const settle = () => new Promise((r) => setTimeout(r, 0));
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await settle(); });
  const choose = async (what) => {
    await click(c.querySelector('[data-taskboard-new]'));
    await click(c.querySelector(`[data-taskboard-new-${what}]`));
  };
  const dialog = () => c.querySelector('#taskboard-new-modal');
  return { harness, c, api, click, choose, dialog, settle, refreshes: () => refreshes };
}

test('"+ New task" opens a menu of the ways to add, modelled on the Resources ＋ menu', async (t) => {
  const { c, click } = await mountBoard(t, 'tbn-menu');
  const button = c.querySelector('[data-taskboard-new]');
  assert.equal(button.textContent.trim(), '＋▾', 'icon-only, as the Resources ＋');
  assert.equal(button.getAttribute('aria-label'), 'New task');
  assert.match(button.getAttribute('title'), /^New task: /);
  assert.equal(button.getAttribute('aria-haspopup'), 'menu');
  assert.equal(button.getAttribute('aria-expanded'), 'false');
  assert.equal(c.querySelector('.tb-import-open'), null, 'the import moved into the menu');
  await click(button);
  assert.equal(c.querySelector('[data-taskboard-new]').getAttribute('aria-expanded'), 'true');
  const names = [...c.querySelectorAll('.tb-new-menu .gr-new-name')].map((el) => el.textContent);
  assert.deepEqual(names, ['Write it myself', 'Ask an agent', 'New epic', 'From a tracker…']);
  assert.equal(c.querySelector('[data-taskboard-new-ask]').disabled, false);
});

test('Ask an agent is off, with the reason, when no member who may write is online', async (t) => {
  const offline = await mountBoard(t, 'tbn-off', { groupMembers: [{ agent_id: 'agt_away', title: 'away', online: false }] });
  await offline.click(offline.c.querySelector('[data-taskboard-new]'));
  const ask = offline.c.querySelector('[data-taskboard-new-ask]');
  assert.equal(ask.disabled, true);
  assert.match(ask.textContent, /No member of this group is online/);
});

test('the writer lock narrows who Ask an agent may mail', async (t) => {
  const harness = await createPreactHarness(t);
  const { askableMembers, askUnavailable, defaultAskee } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  const group = { name: 'crew', members: members() };
  assert.deepEqual(askableMembers(group, board('crew')).map((m) => m.agent_id), ['agt_dev', 'agt_lead'], 'online members, unlocked');
  assert.equal(defaultAskee(askableMembers(group, board('crew'))).agent_id, 'agt_lead', 'the owner leads');
  const locked = board('crew', { locked: true, writers: ['agt_dev', 'agt_away'] });
  assert.deepEqual(askableMembers(group, locked).map((m) => m.agent_id), ['agt_dev']);
  assert.match(askUnavailable(group, board('crew', { locked: true, writers: [] })), /writer lock/);
});

test('the context\'s askable list says who Ask an agent may mail: a member that only tracks its own work is not offered', async (t) => {
  const harness = await createPreactHarness(t);
  const { askableMembers, askUnavailable } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  const group = { name: 'crew', members: members() };
  assert.deepEqual(askableMembers(group, board('crew', { askable: ['agt_lead'] })).map((m) => m.agent_id), ['agt_lead'],
    'only the owner, though the plain member is online');
  assert.deepEqual(askableMembers(group, board('crew', { askable: [] })), []);
  assert.match(askUnavailable(group, board('crew', { askable: [] })), /taskboard\.write/);
  assert.match(askUnavailable(group, board('crew', { askable: [], locked: true, writers: [] })), /writer lock/);
});

test('Write it myself sends every field, the assignee included, in one add', async (t) => {
  const { harness, c, api, choose, dialog, click, refreshes } = await mountBoard(t, 'tbn-form');
  await choose('form');
  assert.ok(dialog(), 'the form opens as a dialog');
  assert.equal(dialog().querySelector('[role="dialog"]').getAttribute('aria-labelledby'), 'taskboard-new-title');
  const submit = () => dialog().querySelector('#taskboard-new-submit');
  assert.equal(submit().disabled, true, 'the title is required');
  await harness.input(dialog().querySelector('.tb-new-title'), 'Port the sweep');
  await harness.input(dialog().querySelector('.tb-new-body textarea'), 'see the review notes');
  await harness.input(dialog().querySelector('.tb-edit-ref'), 'tcl-a1b2c3');
  await comboChoose(harness, dialog().querySelector('[data-stage-pick="taskboard-new-stage"]'), 'next');
  // After: the type-to-filter list adds a dependency, shown as a chip.
  const deps = dialog().querySelector('.tb-deps-picker input');
  await harness.act(async () => { harness.fireEvent(deps, 'focus'); });
  await harness.act(async () => { harness.fireEvent(dialog().querySelector('.tb-deps-picker [role="option"][data-key="tk_00000001"]'), 'mousedown'); });
  assert.ok(dialog().querySelector('.tb-edit-dep[data-dep="tk_00000001"]'), 'the chosen dependency is a chip');
  const assignee = dialog().querySelector('.tb-new-assignee');
  assert.deepEqual([...assignee.querySelectorAll('option')].map((o) => o.value), ['', 'operator', 'agt_dev', 'agt_lead', 'agt_away']);
  await selectValue(harness, assignee, 'agt_dev');
  await click(submit());
  assert.deepEqual(api.calls, [
    ['create', 'tbn-form', { title: 'Port the sweep', body: 'see the review notes', ref: 'tcl-a1b2c3', stage: 'next', after: ['tk_00000001'], resolveRef: true,
      status: '', to: 'agt_dev', hold: true }],
  ]);
  assert.equal(dialog(), null, 'closes when the task landed');
  assert.ok(refreshes() > 0, 'the board is read again');
});

test('the assignee picker shows only with can.assign', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { dialog, choose } = await mountBoard(t, 'tbn-noassign', { api });
  await choose('form');
  assert.ok(dialog().querySelector('.tb-new-title'));
  assert.equal(dialog().querySelector('.tb-new-assignee'), null);
});

test('submit is single-flight and busy', async (t) => {
  const api = fakeApi({ hold: true });
  const { harness, dialog, choose, click, settle } = await mountBoard(t, 'tbn-busy', { api });
  await choose('form');
  await harness.input(dialog().querySelector('.tb-new-title'), 'once');
  const title = dialog().querySelector('.tb-new-title');
  await harness.act(async () => { harness.fireEvent(title, 'keydown', { key: 'Enter' }); await settle(); });
  await harness.act(async () => { harness.fireEvent(title, 'keydown', { key: 'Enter' }); await settle(); });
  assert.equal(api.calls.length, 1, 'Enter twice sends once');
  assert.equal(dialog().querySelector('#taskboard-new-submit').disabled, true, 'busy while it is in flight');
  assert.match(dialog().querySelector('#taskboard-new-submit').textContent, /Adding/);
  await harness.act(async () => { api.release(); await settle(); });
  assert.equal(dialog(), null);
});

test('a refusal stays in the form as an error', async (t) => {
  const refusing = await mountBoard(t, 'tbn-refuse', { api: fakeApi({ refuse: 'create' }) });
  await refusing.choose('form');
  await refusing.harness.input(refusing.dialog().querySelector('.tb-new-title'), 'nope');
  await refusing.click(refusing.dialog().querySelector('#taskboard-new-submit'));
  assert.ok(refusing.dialog(), 'the form stays open');
  assert.equal(refusing.dialog().querySelector('[role="alert"]').textContent, 'create refused');
  assert.equal(refusing.dialog().querySelector('#taskboard-new-submit').disabled, false, 'and can be sent again');
  assert.equal(refusing.dialog().querySelector('.tb-new-title').value, 'nope', 'the input is kept');
});

test('Cancel closes a clean form', async (t) => {
  const { dialog, choose, click } = await mountBoard(t, 'tbn-cancel');
  await choose('form');
  await click(dialog().querySelector('#taskboard-new-cancel'));
  assert.equal(dialog(), null);
});

test('a pasted list is one task per line, and a title that is only a tracker id becomes that ref', async (t) => {
  const { harness, api, dialog, choose, click } = await mountBoard(t, 'tbn-list');
  await choose('form');
  const title = dialog().querySelector('.tb-new-title');
  await harness.act(async () => {
    harness.fireEvent(title, 'onpaste' in title ? 'paste' : 'Paste', { clipboardData: { getData: () => 'one\ntcl-fb0bfz\n' } });
  });
  const list = dialog().querySelector('.tb-new-list');
  assert.equal(list.value, 'one\ntcl-fb0bfz\n');
  assert.equal(dialog().querySelector('.tb-new-body'), null, 'a list takes no brief');
  assert.match(dialog().querySelector('#taskboard-new-submit').textContent, /Add 2 tasks/);
  assert.doesNotMatch(dialog().querySelector('.modal-meta').textContent, /not used/, 'nothing else was filled in');
  await click(dialog().querySelector('#taskboard-new-submit'));
  assert.deepEqual(api.calls, [['capture', 'tbn-list', 'one\ntcl-fb0bfz', { next: false }]]);

  api.calls.length = 0;
  await choose('form');
  await harness.input(dialog().querySelector('.tb-new-title'), 'LIN-231');
  assert.equal(dialog().querySelector('.tb-edit-ref').disabled, true, 'the title is the ref');
  await harness.input(dialog().querySelector('.tb-new-body textarea'), 'brief');
  await click(dialog().querySelector('#taskboard-new-submit'));
  assert.deepEqual(api.calls, [
    ['capture', 'tbn-list', 'LIN-231', { next: false }],
    ['edit', 'tk_00000010', { body: 'brief', addDeps: undefined }],
  ]);
});

test('Ask an agent mails the lead by default, and any online member who may write can be picked', async (t) => {
  const { harness, c, api, choose, click } = await mountBoard(t, 'tbn-ask');
  await choose('ask');
  const modal = () => c.querySelector('#taskboard-ask-modal');
  assert.ok(modal());
  const to = modal().querySelector('.tb-ask-to');
  assert.deepEqual([...to.querySelectorAll('option')].map((o) => o.value), ['agt_dev', 'agt_lead'], 'online members only');
  assert.equal(modal().querySelector('#taskboard-ask-submit').disabled, true, 'the request is required');
  await harness.input(modal().querySelector('.tb-ask-prompt'), '  the three follow-ups from the review  ');
  await click(modal().querySelector('#taskboard-ask-submit'));
  assert.deepEqual(api.calls, [['ask', 'tbn-ask', 'agt_lead', 'the three follow-ups from the review']], 'the owner is the default');
  assert.equal(modal(), null);

  // Another member can be picked.
  api.calls.length = 0;
  await choose('ask');
  await selectValue(harness, modal().querySelector('.tb-ask-to'), 'agt_dev');
  await harness.input(modal().querySelector('.tb-ask-prompt'), 'more');
  await click(modal().querySelector('#taskboard-ask-submit'));
  assert.deepEqual(api.calls, [['ask', 'tbn-ask', 'agt_dev', 'more']]);
});

test('the Ask dialog falls back to the default when its pick goes offline', async (t) => {
  const harness = await createPreactHarness(t);
  const { AskAgentDialog } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  const calls = [];
  const api = { ask: async (...args) => { calls.push(args); return {}; } };
  const group = { name: 'crew', members: members() };
  const view = (g) => harness.html`<${AskAgentDialog} group=${g} entry=${board('crew')} api=${api} onClose=${() => {}} />`;
  const mounted = await harness.mount(view(group));
  const modal = () => mounted.container.querySelector('#taskboard-ask-modal');
  await selectValue(harness, modal().querySelector('.tb-ask-to'), 'agt_dev');
  await harness.input(modal().querySelector('.tb-ask-prompt'), 'tasks');
  const gone = { ...group, members: members().map((m) => (m.agent_id === 'agt_dev' ? { ...m, online: false } : m)) };
  await mounted.rerender(view(gone));
  const submit = modal().querySelector('#taskboard-ask-submit');
  assert.equal(submit.disabled, false, 'still ready: the owner stands in');
  await harness.act(async () => { harness.fireEvent(submit, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  assert.deepEqual(calls, [['crew', 'agt_lead', 'tasks']]);
});

test('the client sends create and ask to their routes with only the fields given', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTaskboardApi } = await harness.importDashboardModule('js/taskboard-actions.js');
  const sent = [];
  const api = createTaskboardApi({ fetchImpl: async (path, init) => {
    sent.push([init.method, path, JSON.parse(init.body)]);
    return { ok: true, status: 200, json: async () => ({}) };
  } });
  await api.create('crew', { title: 'plain' });
  await api.create('crew', { title: 't', body: 'b', ref: 'tcl-a1b2c3', stage: 'next', after: ['tk_00000001'], resolveRef: true });
  await api.edit('tk_00000001', { body: 'b', addDeps: ['tk_00000002'] });
  await api.ask('my crew', 'agt_lead', 'make tasks');
  assert.deepEqual(sent, [
    ['POST', '/v1/groups/crew/taskboard/tasks', { title: 'plain' }],
    ['POST', '/v1/groups/crew/taskboard/tasks', { title: 't', body: 'b', ref: 'tcl-a1b2c3', resolve_ref: true, stage: 'next', after: ['tk_00000001'] }],
    ['PATCH', '/v1/taskboard/tasks/tk_00000001', { body: 'b', add_deps: ['tk_00000002'] }],
    ['POST', '/v1/groups/my%20crew/taskboard/ask', { to: 'agt_lead', prompt: 'make tasks' }],
  ]);
});

// Fleet- and federation-wide: in the fused view a linked node's group is
// name@node, while that node's board, grants and routes know it as name.
test('on a linked node\'s group in the fused view, the board is found and every write names the group as that node does', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ TaskboardSection, TaskboardSummaryChip }, { dashPrefs }, { setNodeSnapshotSource }] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  setNodeSnapshotSource((node) => (node === 'inst_forge' ? { peer_view: { level: 'restricted', groups: { 'tbn-peer': ['taskboard.write', 'taskboard.assign'] } } } : null));
  dashPrefs.setItem('tclaude.dash.groups.fused.taskboard.tbn-peer@forge', '1');
  const api = fakeApi();
  const source = { context: harness.signals.signal({ groups: [board('tbn-peer')] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const asked = [];
  const boards = (node) => { asked.push(node); return { source, api }; };
  const group = { name: 'tbn-peer@forge', fleet_node: { id: 'inst_forge', name: 'forge', local: false }, members: members() };
  const mounted = await harness.mount(harness.html`<div>
    <${TaskboardSummaryChip} group=${group} boards=${boards} />
    <${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} boards=${boards} />
  </div>`);
  const c = mounted.container;
  const settle = () => new Promise((r) => setTimeout(r, 0));
  const click = (el) => harness.act(async () => { harness.fireEvent(el, 'click'); await settle(); });
  assert.ok(asked.length && asked.every((node) => node === 'inst_forge'), 'read and written through the group\'s node');
  assert.ok(c.querySelector('[data-fused-taskboard="tbn-peer@forge"]'), 'the board is found under the group\'s own name');
  assert.ok(c.querySelector('[data-taskboard-chip="tbn-peer@forge"]'));

  // Write it myself, with an assignee (the peer grants taskboard.assign).
  await click(c.querySelector('[data-taskboard-new]'));
  await click(c.querySelector('[data-taskboard-new-form]'));
  const form = c.querySelector('#taskboard-new-modal');
  await harness.input(form.querySelector('.tb-new-title'), 'from the fleet view');
  await selectValue(harness, form.querySelector('.tb-new-assignee'), 'agt_dev');
  await click(form.querySelector('#taskboard-new-submit'));
  assert.deepEqual(api.calls.map(([verb, group, payload]) => [verb, group, payload?.to]), [['create', 'tbn-peer', 'agt_dev']]);

  // Ask an agent.
  api.calls.length = 0;
  await click(c.querySelector('[data-taskboard-new]'));
  await click(c.querySelector('[data-taskboard-new-ask]'));
  await harness.input(c.querySelector('#taskboard-ask-modal .tb-ask-prompt'), 'plan it');
  await click(c.querySelector('#taskboard-ask-submit'));
  assert.deepEqual(api.calls, [['ask', 'tbn-peer', 'agt_lead', 'plan it']]);
});

test('the form starts a task in any column: In progress needs an assignee, Blocked a reason, Done logs work', async (t) => {
  const { harness, api, choose, dialog, click } = await mountBoard(t, 'tbn-start');
  await choose('form');
  const box = () => dialog().querySelector('[data-stage-pick="taskboard-new-stage"]');
  assert.deepEqual(await comboKeys(harness, box()), ['idea', 'next', 'progress', 'blocked', 'done']);
  const pick = (key) => comboChoose(harness, box(), key);
  const submit = () => dialog().querySelector('#taskboard-new-submit');
  await harness.input(dialog().querySelector('.tb-new-title'), 'started');

  await pick('progress');
  assert.equal(submit().disabled, true, 'in progress needs its assignee');
  await selectValue(harness, dialog().querySelector('.tb-new-assignee'), 'agt_dev');
  assert.equal(submit().disabled, false);

  await pick('blocked');
  assert.ok(dialog().querySelector('.tb-new-reason'), 'blocked asks why');
  assert.equal(submit().disabled, true, 'blocked needs its reason');
  await harness.input(dialog().querySelector('.tb-new-reason'), 'waiting on review');
  await click(submit());
  assert.deepEqual(api.calls.at(-1), ['create', 'tbn-start', { title: 'started', body: '', ref: '', after: [], resolveRef: false,
    stage: 'next', status: 'blocked', blockReason: 'waiting on review', to: 'agt_dev' }]);

  await choose('form');
  await harness.input(dialog().querySelector('.tb-new-title'), 'already did it');
  await pick('done');
  await click(submit());
  assert.deepEqual(api.calls.at(-1), ['create', 'tbn-start', { title: 'already did it', body: '', ref: '', after: [], resolveRef: false,
    stage: 'idea', status: 'done', to: '' }], 'done needs no assignee');
});

test('without the assign grant the form offers no In progress', async (t) => {
  const api = fakeApi();
  api.can = { assign: false };
  const { harness, dialog, choose } = await mountBoard(t, 'tbn-start-noassign', { api });
  await choose('form');
  const box = dialog().querySelector('[data-stage-pick="taskboard-new-stage"]');
  assert.deepEqual(await comboKeys(harness, box), ['idea', 'next', 'progress', 'blocked', 'done']);
  assert.equal(box.querySelector('[role="option"][data-key="progress"]').getAttribute('aria-disabled'), 'true', 'In progress is offered off, with why');
});

test('a tracker id with a start state is one add with the id as its ref, so nobody is mailed for done work', async (t) => {
  const harness = await createPreactHarness(t);
  const { newTaskPlan } = await harness.importDashboardModule('js/groups-taskboard-new.js');
  assert.equal(newTaskPlan({ title: 'tcl-a1b2c3', start: 'next' }).kind, 'tracker', 'Ideas or Next unassigned: the import, titled from the tracker');
  assert.deepEqual(newTaskPlan({ title: 'tcl-a1b2c3', start: 'done', to: 'agt_dev' }), {
    kind: 'add', task: { title: 'tcl-a1b2c3', body: '', ref: 'tcl-a1b2c3', after: [], resolveRef: true, stage: 'idea', status: 'done', to: 'agt_dev' },
  });
  assert.equal(newTaskPlan({ title: 'tcl-a1b2c3', start: 'progress', to: 'agt_dev' }).task.to, 'agt_dev');
});

test('the After picker leaves out the task itself and the tasks that wait on it', async (t) => {
  const harness = await createPreactHarness(t);
  const { depChoices, waitersOf } = await harness.importDashboardModule('js/groups-taskboard-deps.js');
  const tasks = [
    { id: 'tk_a', title: 'a', depends_on: [] },
    { id: 'tk_b', title: 'b', depends_on: ['tk_a'] },
    { id: 'tk_c', title: 'c', depends_on: ['tk_b'] },
    { id: 'tk_d', title: 'd', depends_on: [] },
  ];
  assert.deepEqual([...waitersOf('tk_a', tasks)].sort(), ['tk_b', 'tk_c']);
  assert.deepEqual(depChoices(tasks, 'tk_a').map((item) => item.key), ['tk_d'], 'b and c wait on a: a cycle');
  assert.deepEqual(depChoices(tasks, 'tk_c').map((item) => item.key), ['tk_a', 'tk_b', 'tk_d']);
  assert.deepEqual(depChoices(tasks).map((item) => item.key), ['tk_a', 'tk_b', 'tk_c', 'tk_d'], 'a new task waits on any');
});
