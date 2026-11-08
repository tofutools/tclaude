// A task's own dashboard link (task-route.js, docs/taskboard.md "A task's
// link"): /groups?task=<task-id>[@<node>]:<group>. The title on a board is
// that link, ⋯ Copy link copies it, opening the form records a history step
// (Back closes it), and a link (or Back/Forward) to a task opens its form.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5),
  created_by: 'human:operator', updated_at: ago(2), attachments: [], ...extra });

function fakeApi() {
  const calls = [];
  return {
    calls,
    task: async (id) => { calls.push(['task', id]); return row(id, id === 'tk_0000000a' ? 'archived one' : 'x', { body: 'brief' }); },
    activity: async (id) => ({ id, entries: [] }),
    fileData: async () => ({ data: '' }),
    edit: async () => ({}), status: async () => ({}), move: async () => ({}), stage: async () => ({}),
  };
}

async function mountBoard(t) {
  const harness = await createPreactHarness(t);
  const [{ TaskboardSection }, { dashPrefs }, routes, route] = await Promise.all([
    harness.importDashboardModule('js/groups-taskboard.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/overlay-routes.js'),
    harness.importDashboardModule('js/task-route.js'),
  ]);
  dashPrefs.setItem('tclaude.dash.groups.fused.taskboard.crew', '1');
  dashPrefs.setItem('tclaude.dash.groups.taskboard.view.crew', 'list');
  const board = { group: 'crew', group_id: 1, counts: { live: 2, next: 2 }, closed: [],
    tasks: [row('tk_00000001', 'Release notes'), row('tk_00000002', 'Changelog')] };
  const source = { context: harness.signals.signal({ groups: [board] }), status: harness.signals.signal('ready'),
    attach: () => () => {}, refresh: async () => {} };
  const group = { name: 'crew', members: [] };
  const announced = [];
  const onOverlay = (event) => announced.push(event.detail);
  harness.document.addEventListener(routes.OVERLAY_EVENT, onOverlay);
  t.after(() => harness.document.removeEventListener(routes.OVERLAY_EVENT, onOverlay));
  const api = fakeApi();
  const { container } = await harness.mount(harness.html`<${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${api} />`);
  const settle = async () => { for (let i = 0; i < 4; i += 1) await harness.act(() => new Promise((r) => setTimeout(r, 0))); };
  const modal = () => harness.document.querySelector('#taskboard-edit-modal');
  const title = (id) => container.querySelector(`tr[data-task-id="${id}"] .tb-title`);
  return { harness, c: container, api, settle, routes, route, announced, modal, title };
}

test('a task route reads and writes <id>[@<node>]:<group>, the group as is', async (t) => {
  const harness = await createPreactHarness(t);
  const { parseTaskRoute, formatTaskRoute, taskPath, taskURL } = await harness.importDashboardModule('js/task-route.js');
  assert.deepEqual(parseTaskRoute('tk_12ab:crew'), { taskId: 'tk_12ab', node: '', group: 'crew' });
  assert.deepEqual(parseTaskRoute('tk_12ab@inst_x7:a:b@c'), { taskId: 'tk_12ab', node: 'inst_x7', group: 'a:b@c' }, 'a group may hold : and @');
  for (const bad of ['', 'crew', 'tk_1', 'tk_1:', ':crew', 'tk_X:crew', 'tk_1@node:crew', 'tk_1@inst_a@inst_b:crew']) {
    assert.equal(parseTaskRoute(bad), null, bad);
  }
  const target = { taskId: 'tk_12ab', node: 'inst_x7', group: 'a:b@c' };
  assert.deepEqual(parseTaskRoute(formatTaskRoute(target)), target, 'round-trips');
  assert.equal(taskPath({ taskId: 'tk_1', group: 'crew' }), '/groups?task=tk_1%3Acrew');
  assert.equal(taskURL({ taskId: 'tk_1', group: 'my crew' }, 'http://127.0.0.1:7777'), 'http://127.0.0.1:7777/groups?task=tk_1%3Amy+crew');
  const { fromPath } = await harness.importDashboardModule('js/nav-history-core.js');
  assert.equal(fromPath('/groups', '?task=tk_1%3Acrew').params?.task, 'tk_1:crew', 'a link\'s location keeps it');
  assert.equal(fromPath('/groups', '?task=javascript%3Aalert(1)').params?.task, undefined, 'only a task route survives');
  assert.equal(fromPath('/', '?task=tk_1%3Acrew').params?.task, 'tk_1:crew', 'and on the bare /, the Groups tab\'s own path (Forward, a reload)');
});

test('the title is the task\'s link: a plain click opens the form as a history step, Mod-click is the browser\'s', async (t) => {
  const { harness, settle, announced, modal, title, route } = await mountBoard(t);
  assert.equal(title('tk_00000001').tagName, 'A');
  assert.equal(title('tk_00000001').getAttribute('href'), '/groups?task=tk_00000001%3Acrew');
  for (const mod of [{ ctrlKey: true }, { metaKey: true }, { shiftKey: true }, { button: 1 }]) {
    let event;
    await harness.act(() => { event = harness.fireEvent(title('tk_00000001'), 'click', mod); });
    await settle();
    assert.equal(modal(), null, `${JSON.stringify(mod)} leaves the page as it is`);
    assert.equal(event.defaultPrevented, false, `${JSON.stringify(mod)} is the browser's: a new tab`);
  }
  assert.deepEqual(announced, []);
  let click;
  await harness.act(() => { click = harness.fireEvent(title('tk_00000001'), 'click'); });
  await settle();
  assert.equal(click.defaultPrevented, true, 'a plain click stays on the page');
  assert.ok(modal(), 'and opens the form');
  assert.deepEqual(announced, [{ closed: false, replace: false }], 'as a history step');
  assert.deepEqual(route.taskRoute.value, { taskId: 'tk_00000001', node: '', group: 'crew' });
  const cancel = [...modal().querySelectorAll('button')].find((b) => b.textContent.trim() === 'Cancel');
  await harness.act(() => harness.fireEvent(cancel, 'click'));
  await settle();
  assert.equal(modal(), null);
  assert.deepEqual(announced.at(-1), { closed: true, replace: false }, 'closing it steps back');
  assert.equal(route.taskRoute.value, null);
});

test('a link to a task (or Back/Forward) opens its form, and leaving it closes the form', async (t) => {
  const { settle, routes, modal, announced, api } = await mountBoard(t);
  await settle();
  routes.restoreOverlays({ task: 'tk_00000002:crew' });
  await settle();
  assert.ok(modal(), 'the form opens');
  assert.match(modal().textContent, /tk_00000002/);
  assert.deepEqual(announced, [], 'a restore records no step of its own');
  routes.restoreOverlays({ task: '' });
  await settle();
  assert.equal(modal(), null, 'Back closes it');
  routes.restoreOverlays({ task: 'tk_00000001:other' });
  await settle();
  assert.equal(modal(), null, 'another group\'s task is that board\'s');
  routes.restoreOverlays({ task: 'tk_0000000a:crew' });
  await settle();
  assert.ok(api.calls.some(([verb, id]) => verb === 'task' && id === 'tk_0000000a'), 'a task off the list is read');
  assert.ok(modal(), 'and opened');
});

test('⋯ Copy link copies the task\'s whole link', async (t) => {
  const { harness, c, settle } = await mountBoard(t);
  const copied = [];
  const nav = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', { configurable: true, writable: true, value: { clipboard: { writeText: async (text) => { copied.push(text); } } } });
  t.after(() => { if (nav) Object.defineProperty(globalThis, 'navigator', nav); });
  const more = c.querySelector('tr[data-task-id="tk_00000002"] .tb-act-more');
  await harness.act(() => harness.fireEvent(more, 'click'));
  const item = [...harness.document.querySelectorAll('[role="menuitem"]')].find((b) => b.textContent.trim() === 'Copy link');
  assert.ok(item, 'the ⋯ has Copy link');
  await harness.act(async () => { harness.fireEvent(item, 'click'); await new Promise((r) => setTimeout(r, 0)); });
  await settle();
  assert.equal(copied.length, 1);
  assert.match(copied[0], /\/groups\?task=tk_00000002%3Acrew$/);
});
