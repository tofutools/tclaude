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
  const primary = (el) => el.querySelector('.gr-primary').dataset.resourcePrimary;

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
  assert.equal(board.tagName, 'TR', 'a resource is a table row, like an agent');
  const cells = (el) => [...el.children].map((td) => td.textContent.trim());
  assert.deepEqual([...container.querySelectorAll('thead th')].map((th) => th.childNodes[0]?.textContent || ''), ['', 'Name', 'State', 'Kind', 'Lock', 'Detail'],
    'Name, then State, in the agents table\'s order');
  assert.deepEqual([...container.querySelectorAll('thead th[data-sort-table="resources"]')].map((th) => th.dataset.sortCol), ['name', 'state', 'kind', 'lock', 'detail'],
    'every titled header sorts, as an agent header does');
  assert.equal(cells(board)[1], '▦Plan');
  assert.equal(cells(board)[3], 'drawing board', 'a board is a drawing board');
  assert.equal(board.children[4].querySelector('[data-resource-lock]').dataset.resourceLock, 'board:plan', 'the lock has its own column');
  assert.ok(board.firstElementChild.querySelector('.gr-light'), 'the light opens the row, like an agent\'s');
  assert.deepEqual([...board.querySelectorAll('.row-actions > *')].map((el) => el.matches('.gr-primary') ? 'eye' : el.matches('.gr-remove') ? 'trash' : el.querySelector('.gr-cog') ? 'cog' : '?'),
    ['eye', 'trash', 'cog'], 'the same buttons in the same order on every row');
  const shellRow = row('[data-resource-shell="crew"]');
  assert.deepEqual([...shellRow.querySelectorAll('.row-actions > *')].map((el) => el.matches('.gr-primary') ? 'eye' : el.matches('.gr-remove') ? 'trash' : el.querySelector('.gr-cog') ? 'cog' : '?'),
    ['eye', 'trash', 'cog'], 'a shell has the same buttons');
  const shell = row('[data-resource-shell="crew"]');
  assert.equal(state(shell), 'background');
  assert.equal(primary(shell), 'Attach');

  await harness.act(() => harness.fireEvent(live.querySelector('.gr-cog'), 'click'));
  await harness.act(async () => { harness.fireEvent(live.querySelector('[data-page-reload]'), 'click'); await wait(); });
  assert.deepEqual(calls.at(-1), ['reload', 'pg_live']);
  await harness.act(async () => { harness.fireEvent(shut.querySelector('.gr-primary'), 'click'); await wait(); });
  assert.deepEqual(calls.at(-1), ['reopen', 'pg_shut']);

  await harness.act(async () => { harness.fireEvent(live.querySelector('[data-page-trash]'), 'click'); await wait(); await wait(); });
  assert.deepEqual(confirms, ['Delete page App?']);
  assert.deepEqual(calls.at(-1), ['delete', 'pg_live']);

  // The state follows this browser: the board leaves the strip.
  await harness.act(() => { places.stripPaneKeys.value = []; });
  assert.equal(state(row('[data-resource-board="plan"]')), 'closed');
  assert.equal(primary(row('[data-resource-board="plan"]')), 'Open');

  // A shell open here can still be killed: × ends its session, attached or not.
  await harness.act(() => { places.stripPaneKeys.value = ['groupterm:crew']; });
  assert.equal(state(row('[data-resource-shell="crew"]')), 'open-tab');
  const posted = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => { posted.push([url, JSON.parse(init.body)]); return { ok: true, json: async () => ({}) }; };
  t.after(() => { globalThis.fetch = realFetch; });
  await harness.act(async () => { harness.fireEvent(row('[data-shell-kill="crew"]'), 'click'); await wait(); await wait(); });
  assert.equal(confirms.at(-1), 'Kill crew shell?');
  assert.deepEqual(posted.find(([url]) => url === '/api/shells/kill'), ['/api/shells/kill', { tmux_session: 'tclaude-groupterm-abc' }]);
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

test('＋ lists both process scopes, Board, Live page and Shell with a line each; Shell needs the group\'s directory', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-resources.js');
  const { container } = await harness.mount(harness.html`<div>
    <${mod.AddResource} group=${{ id: 1, name: 'crew', default_cwd: '/src/crew' }} api=${{}} onAddProcess=${() => {}} />
    <${mod.AddResource} group=${{ id: 2, name: 'bare', default_cwd: '' }} api=${{}} onAddProcess=${() => {}} />
  </div>`);
  const [withDir, bare] = container.querySelectorAll('.gr-add');
  const trigger = withDir.querySelector('[data-resource-add]');
  // Icon-only, like the Agents section's spawn buttons: no visible word, but
  // an accessible name and tooltip that say what it adds.
  assert.equal(trigger.textContent.replace(/\s+/g, ''), '＋▾');
  assert.equal(trigger.getAttribute('aria-label'), 'Add a resource to crew: a process, board, live page or shell');
  assert.equal(trigger.getAttribute('title'), trigger.getAttribute('aria-label'));
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  const names = [...withDir.querySelectorAll('.gr-new-name')].map((el) => el.textContent);
  assert.deepEqual(names, ['Process for the group', 'Process for one agent', 'Board', 'Live page', 'Shell']);
  assert.match(withDir.querySelector('[data-resource-add-shell]').textContent, /A terminal in \/src\/crew/);
  assert.equal(withDir.querySelector('[data-resource-add-shell]').disabled, false);
  await harness.act(() => harness.fireEvent(bare.querySelector('[data-resource-add]'), 'click'));
  assert.equal(bare.querySelector('[data-resource-add-shell]').disabled, true);
});

// tcl-kostyh: ⋯ renames a page or a board in place, and moves it to another
// group; an agent's page only to groups its agent is in.
test('⋯ renames a row in place and moves it to another group', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-resources.js');
  const group = { id: 7, name: 'crew' };
  const snapshot = { value: {
    groups: [{ id: 7, name: 'crew', members: [{ agent_id: 'agt_x' }] }, { id: 8, name: 'ops', members: [{ agent_id: 'agt_x' }] },
      { id: 9, name: 'design', members: [] }],
    group_resources: [{ kind: 'page', id: 'pg_live', group_id: 7, title: 'App', state: 'running' }, { kind: 'board', id: 'plan', group_id: 7, title: 'Plan', state: 'closed' }],
  } };
  const calls = [];
  const api = {
    renameGroupPage: async (id, title) => { calls.push(['renamePage', id, title]); },
    moveGroupPage: async (id, name) => { calls.push(['movePage', id, name]); },
    renameBoard: async (id, title) => { calls.push(['renameBoard', id, title]); },
    moveBoard: async (id, name) => { calls.push(['moveBoard', id, name]); },
    setResourceLock: async () => ({}),
  };
  const notes = [];
  const { container } = await harness.mount(harness.html`<${mod.GroupResources} group=${group}
    boards=${[{ id: 'plan', title: 'Plan' }]} pages=${[{ id: 'pg_live', name: 'app', title: 'App', owner: 'agt_x', kind: 'port', state: 'open' }]}
    api=${api} notify=${(msg) => notes.push(msg)} confirm=${async () => true} snapshot=${snapshot} />`);
  const menuOf = (row) => harness.act(async () => { harness.fireEvent(row.querySelector('.gr-cog'), 'click'); await wait(); });
  const pick = (sel) => harness.act(async () => { harness.fireEvent(container.querySelector(sel), 'click'); await wait(); await wait(); });

  const page = container.querySelector('[data-resource-page="pg_live"]');
  await menuOf(page);
  await pick('[data-resource-rename="pg_live"]');
  const input = page.querySelector('[data-resource-rename-input]');
  assert.ok(input, 'the name becomes a field');
  input.value = 'Checkout';
  await harness.act(async () => { harness.fireEvent(input, 'keydown', { key: 'Enter' }); await wait(); await wait(); });
  assert.equal(page.querySelector('[data-resource-rename-input]'), null);

  await menuOf(page);
  await pick('[data-resource-move="pg_live"]');
  assert.deepEqual([...container.querySelectorAll('[data-move-to]')].map((el) => el.dataset.moveTo), ['ops'], 'only the groups its agent is in');
  await pick('[data-move-to="ops"]');

  const board = container.querySelector('[data-resource-board="plan"]');
  await menuOf(board);
  await pick('[data-resource-move="plan"]');
  assert.deepEqual([...container.querySelectorAll('[data-move-to]')].map((el) => el.dataset.moveTo), ['ops', 'design']);
  await pick('[data-menu-back]');
  await pick('[data-resource-rename="plan"]');
  const field = board.querySelector('[data-resource-rename-input]');
  field.value = 'Plan';
  await harness.act(async () => { harness.fireEvent(field, 'keydown', { key: 'Enter' }); await wait(); });
  assert.deepEqual(calls, [['renamePage', 'pg_live', 'Checkout'], ['movePage', 'pg_live', 'ops']], 'an unchanged name is not sent');
  assert.deepEqual(notes, ['Renamed App to Checkout', 'Moved App to ops']);
});

test('＋ hands each process item its scope, and processesOnly lists just those two', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/groups-resources.js');
  const scopes = [];
  const { container } = await harness.mount(harness.html`<${mod.AddResource} group=${{ id: 1, name: 'crew' }} api=${{}}
    processesOnly=${true} onAddProcess=${(scope) => scopes.push(scope)} />`);
  const open = () => harness.act(() => harness.fireEvent(container.querySelector('[data-resource-add]'), 'click'));
  await open();
  assert.deepEqual([...container.querySelectorAll('.gr-new-name')].map((el) => el.textContent), ['Process for the group', 'Process for one agent']);
  await harness.act(() => harness.fireEvent(container.querySelector('[data-resource-add-process-agent]'), 'click'));
  assert.equal(container.querySelector('.gr-new-menu'), null, 'picking closes the menu');
  await open();
  await harness.act(() => harness.fireEvent(container.querySelector('[data-resource-add-process]'), 'click'));
  assert.deepEqual(scopes, ['agent', 'group']);
});

test('Resources sorts by its headers, across kinds, and lines Name and State up with the agents table', async (t) => {
  const harness = await createPreactHarness(t);
  const [mod, sort] = await Promise.all([harness.importDashboardModule('js/groups-resources.js'), harness.importDashboardModule('js/sort.js')]);
  const group = { id: 7, name: 'crew' };
  const snapshot = { value: { group_resources: [
    { kind: 'shell', id: 'crew', group_id: 7, title: 'Group shell', state: 'running', session: 's' },
  ] } };
  const boards = [{ id: 'zeta', title: 'Zeta' }, { id: 'alpha', title: 'Alpha' }];
  const pages = [{ id: 'pg', name: 'mid', title: 'Middle', owner: 'agt_x', kind: 'port', state: 'open' }];
  const names = (root) => [...root.querySelectorAll('tbody tr')].map((tr) => tr.querySelector('.gr-name').textContent.slice(1));
  sort.cycleSort('resources', 'name');
  t.after(() => { sort.cycleSort('resources', 'name'); sort.cycleSort('resources', 'name'); });
  const { container } = await harness.mount(harness.html`<${mod.GroupResources} group=${group} boards=${boards} pages=${pages}
    api=${{ setResourceLock: async () => ({}) }} snapshot=${snapshot} nameOf=${() => ''} />`);
  assert.deepEqual(names(container), ['Alpha', 'Group shell', 'Middle', 'Zeta'], 'sorted by name across every kind');
  assert.ok(container.querySelector('th[data-sort-col="name"]').classList.contains('sort-active'));

  // Alignment: the controls column reaches the agents' Name, and Name the
  // agents' State.
  const doc = harness.document;
  const card = doc.body.appendChild(doc.createElement('div'));
  card.innerHTML = '<div data-fused-section="agents"><table class="pending"><thead><tr><th data-sort-col="name"></th></tr></thead></table>'
    + '<table><thead><tr><th></th><th data-sort-col="title"></th><th data-sort-col="state"></th></tr></thead></table></div>'
    + '<div data-fused-section="processes"><table class="gr-rtable"></table></div>';
  const rect = (left) => () => ({ left, right: left, top: 0, bottom: 0, width: 0, height: 0 });
  const [title, state] = card.querySelectorAll('th[data-sort-col]');
  const table = card.querySelector('.gr-rtable');
  title.getBoundingClientRect = rect(280);
  state.getBoundingClientRect = rect(445);
  table.getBoundingClientRect = rect(70);
  assert.deepEqual(mod.agentColumns(table), { ctl: 210, name: 165 }, 'the members table, past a pending-spawns table before it');
  card.querySelector('[data-fused-section="agents"]').remove();
  assert.equal(mod.agentColumns(table), null, 'no agents table, nothing to line up with');
});
