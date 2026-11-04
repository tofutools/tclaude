import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const wait = () => new Promise((resolve) => setTimeout(resolve, 0));

// tcl-k6vici: a Groups-tab resource row reads one state (the pill), its
// primary action follows it, the kind adds its own action, ⋯ holds Delete
// behind a confirm, and the group's shell is a row of its own.
test('resource rows: pill, primary action by state, per-kind action, delete behind confirm, shell row', async (t) => {
  const harness = await createPreactHarness(t);
  const [mod, places] = await Promise.all([
    harness.importDashboardModule('js/groups-resources.js'),
    harness.importDashboardModule('js/resource-places.js'),
  ]);
  const group = { id: 7, name: 'crew', default_cwd: '/src/crew' };
  const snapshot = { value: { group_resources: [
    { kind: 'page', id: 'pg_live', group_id: 7, title: 'App', state: 'running' },
    { kind: 'page', id: 'pg_shut', group_id: 7, title: 'Old', state: 'closed' },
    { kind: 'board', id: 'plan', group_id: 7, title: 'Plan', state: 'closed' },
    { kind: 'shell', id: 'crew', group_id: 7, title: 'Group shell', state: 'running', session: 'tclaude-groupterm-abc', detail: '/src/crew' },
  ] } };
  const calls = [];
  const confirms = [];
  const api = {
    reloadGroupPage: async (id) => { calls.push(['reload', id]); },
    deleteGroupPage: async (id) => { calls.push(['delete', id]); },
    deleteBoard: async (id) => { calls.push(['deleteBoard', id]); },
    reopenGroupPage: async (id) => { calls.push(['reopen', id]); },
    setResourceLock: async () => ({}),
  };
  const confirm = async (opts) => { confirms.push(opts.title); return true; };
  const notes = [];
  const notify = (msg) => notes.push(msg);
  await harness.act(() => { places.stripPaneKeys.value = ['board:plan']; });
  const pages = [{ id: 'pg_live', name: 'app', title: 'App', owner: 'agt_x', kind: 'port', state: 'open' },
    { id: 'pg_shut', name: 'old', title: 'Old', owner: 'agt_x', kind: 'port', state: 'closed', closedReason: 'the daemon restarted' }];
  const boards = [{ id: 'plan', title: 'Plan' }];
  const { container } = await harness.mount(harness.html`<${mod.GroupResources} group=${group} boards=${boards} pages=${pages}
    api=${api} notify=${notify} confirm=${confirm} snapshot=${snapshot} nameOf=${() => 'builder'} />`);
  const row = (sel) => container.querySelector(sel);
  const state = (el) => el.querySelector('[data-resource-state]').dataset.resourceState;
  const primary = (el) => el.querySelector('.gr-primary').textContent;

  const live = row('[data-resource-page="pg_live"]');
  assert.equal(state(live), 'background', 'a live page open nowhere here runs in the background');
  assert.equal(primary(live), 'Open');
  assert.equal(live.querySelector('[data-resource-state]').textContent, 'live', 'a page says live, not background');
  assert.equal(row('[data-resource-shell="crew"]').querySelector('[data-resource-state]').textContent, 'background');
  const shut = row('[data-resource-page="pg_shut"]');
  assert.equal(state(shut), 'closed');
  assert.equal(primary(shut), 'Reopen');
  assert.match(shut.textContent, /the daemon restarted/);
  assert.equal(shut.querySelector('[data-page-reload]'), null, 'a closed page has nothing to reload');
  const board = row('[data-resource-board="plan"]');
  assert.equal(state(board), 'open-tab', 'a board in the Collab strip is open here');
  assert.equal(primary(board), 'Focus');
  assert.equal(board.lastElementChild.dataset.resourceLock, 'board:plan', 'the lock stays at the row end');
  const shell = row('[data-resource-shell="crew"]');
  assert.equal(state(shell), 'background');
  assert.equal(primary(shell), 'Attach');

  await harness.act(async () => { harness.fireEvent(live.querySelector('[data-page-reload]'), 'click'); await wait(); });
  assert.deepEqual(calls.at(-1), ['reload', 'pg_live']);
  await harness.act(async () => { harness.fireEvent(shut.querySelector('.gr-primary'), 'click'); await wait(); });
  assert.deepEqual(calls.at(-1), ['reopen', 'pg_shut']);

  await harness.act(() => harness.fireEvent(live.querySelector('.gr-more'), 'click'));
  await harness.act(async () => { harness.fireEvent(live.querySelector('[data-page-delete]'), 'click'); await wait(); await wait(); });
  assert.deepEqual(confirms, ['Delete page App?']);
  assert.deepEqual(calls.at(-1), ['delete', 'pg_live']);

  // The state follows this browser: the board leaves the strip.
  await harness.act(() => { places.stripPaneKeys.value = []; });
  assert.equal(state(row('[data-resource-board="plan"]')), 'closed');
  assert.equal(primary(row('[data-resource-board="plan"]')), 'Open');

  // A shell open here is attached: no kill, only Focus.
  await harness.act(() => { places.stripPaneKeys.value = ['groupterm:crew']; });
  assert.equal(state(row('[data-resource-shell="crew"]')), 'open-tab');
  assert.equal(row('[data-shell-kill]'), null, 'an attached shell is not killed from here');
});

test('a process pill reads its own group\'s entry: one template pinned in two groups', async (t) => {
  const harness = await createPreactHarness(t);
  const pill = await harness.importDashboardModule('js/resource-pill.js');
  const snap = { group_resources: [
    { kind: 'process', id: 'deploy', group_id: 3, state: 'closed' },
    { kind: 'process', id: 'deploy', group_id: 9, state: 'running' },
  ] };
  assert.equal(pill.resourceView(snap, 3, 'process', 'deploy'), 'closed');
  assert.equal(pill.resourceView(snap, 9, 'process', 'deploy'), 'running');
});

test('＋ New lists Process, Board, Live page and Shell with a line each; Shell needs the group\'s directory', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-resources.js');
  const { container } = await harness.mount(harness.html`<div>
    <${mod.AddResource} group=${{ id: 1, name: 'crew', default_cwd: '/src/crew' }} api=${{}} onAddProcess=${() => {}} />
    <${mod.AddResource} group=${{ id: 2, name: 'bare', default_cwd: '' }} api=${{}} onAddProcess=${() => {}} />
  </div>`);
  const [withDir, bare] = container.querySelectorAll('.gr-add');
  await harness.act(() => harness.fireEvent(withDir.querySelector('[data-resource-add]'), 'click'));
  const names = [...withDir.querySelectorAll('.gr-new-name')].map((el) => el.textContent);
  assert.deepEqual(names, ['Process', 'Board', 'Live page', 'Shell']);
  assert.match(withDir.querySelector('[data-resource-add-shell]').textContent, /A terminal in \/src\/crew/);
  assert.equal(withDir.querySelector('[data-resource-add-shell]').disabled, false);
  await harness.act(() => harness.fireEvent(bare.querySelector('[data-resource-add]'), 'click'));
  assert.equal(bare.querySelector('[data-resource-add-shell]').disabled, true);
});
