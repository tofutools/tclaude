import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByLabelText, getByRole } from './preact-harness.mjs';

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

function snapshot(groups) {
  return {
    groups,
    pending: [],
    ungrouped: [],
    retired: [],
    conversations: [],
    replaced: [],
    paging: {},
  };
}

function testRenderer(groups) {
  if (!groups.length) return '<div class="empty">No groups</div>';
  return groups.map((group) => `
    <details data-group-key="${group.name}">
      <summary><strong class="group-name">${group.name}</strong></summary>
      ${group.members?.length ? `
        <span class="group-activity">${group.members[0].state?.status}</span>
        <span class="slop-machine" data-status="${group.members[0].state?.status}"><span>${group.members[0].state?.status}</span></span>
      ` : ''}
      <span class="group-descr">${group.descr || ''}</span>
      <button data-act="inspect" data-group="${group.name}" aria-label="inspect ${group.name}">inspect</button>
    </details>
  `).join('');
}

function stateDependencies() {
  return {
    columns: {
      list: () => [{ key: 'id', label: 'ID' }, { key: 'role', label: 'Role' }],
      hidden: () => false,
      setHidden: () => {},
      deviationCount: () => 0,
    },
    reorder: (groups) => groups,
  };
}

test('Groups list preserves keyed disclosure, focus and nodes across reorder/activity polls', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsList }, { mountTransientSiblingEditor }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
    harness.importDashboardModule('js/transient-editor.js'),
  ]);
  const prefs = memoryPrefs({
    'tclaude.dash.ungrouped.groups': '0',
    'tclaude.dash.retired.groups': '0',
  });
  const state = createGroupsState({ prefs, resetOffsets: () => {}, ...stateDependencies() });
  state.initialize();
  state.publish(snapshot([
    { name: 'alpha', descr: 'old', members: [{ state: { status: 'working' } }] },
    { name: 'beta', members: [{ state: { status: 'idle' } }] },
  ]));
  const actions = { sort: () => {}, page: () => {}, setPageSize: () => {} };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${GroupsList}
      host=${host}
      state=${state}
      actions=${actions}
      renderGroupsHTML=${testRenderer}
    />
  `, host);

  const alpha = host.querySelector('details[data-group-key="alpha"]');
  const beta = host.querySelector('details[data-group-key="beta"]');
  const inspect = getByRole(alpha, 'button', { name: 'inspect alpha' });
  const descr = alpha.querySelector('.group-descr');
  alpha.open = true;
  inspect.focus();

  const editor = harness.document.createElement('input');
  const restoreEditor = mountTransientSiblingEditor(descr, editor);
  assert.equal(descr.isConnected, true, 'the managed inline-edit host stays connected');
  assert.equal(descr.hidden, true);
  restoreEditor();
  assert.equal(descr.hidden, false);
  assert.equal(editor.isConnected, false);

  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [{ state: { status: 'asking' } }] },
    { name: 'alpha', descr: 'new', members: [{ state: { status: 'working' } }] },
  ])));

  assert.equal(host.querySelector('details[data-group-key="alpha"]'), alpha);
  assert.equal(host.querySelector('details[data-group-key="beta"]'), beta);
  assert.equal(host.querySelector('details:first-child'), beta, 'snapshot reorder moves keyed nodes');
  assert.equal(alpha.open, true, 'live disclosure state is not reset by a reorder');
  assert.equal(harness.document.activeElement, inspect, 'focused legacy action remains focused');
  assert.equal(alpha.querySelector('.group-descr'), descr, 'inline-edit host identity survives publish');
  assert.equal(descr.textContent, 'new', 'restored inline-edit host receives later updates');
  assert.equal(beta.querySelector('.group-activity').textContent, 'asking');

  const machine = beta.querySelector('.slop-machine');
  machine.innerHTML = '<span>working</span>'; // manual-pull restore creates foreign children
  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [{ state: { status: 'idle' } }] },
    { name: 'alpha', members: [{ state: { status: 'working' } }] },
  ])));
  assert.equal(machine.querySelector('span').textContent, 'idle',
    'a status render replaces imperative reel children after a manual pull');

  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [] },
    { name: 'alpha', members: [] },
  ])));
  assert.equal(harness.document.activeElement, inspect,
    'a neighboring activity chip disappearing does not replace the keyed action');

  await mounted.unmount();
  host.remove();
});

test('Groups controls own query, visibility, columns, badge and dropdown behavior', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsControls }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
  ]);
  let hiddenRole = false;
  let refreshes = 0;
  let resets = 0;
  const columns = {
    list: () => [{ key: 'role', label: 'Role' }],
    hidden: () => hiddenRole,
    setHidden: (_key, value) => { hiddenRole = value; },
    deviationCount: () => Number(hiddenRole),
  };
  const state = createGroupsState({
    prefs: memoryPrefs(), resetOffsets: () => { resets++; }, columns,
    reorder: (groups) => groups,
  });
  state.initialize();
  state.publish(snapshot([
    { name: 'alpha', members: [] },
    { name: 'beta', members: [] },
  ]));
  const mounted = await harness.mount(harness.html`
    <${GroupsControls} state=${state} actions=${{ refresh: () => { refreshes++; } }} />
  `);

  const filter = getByRole(mounted.container, 'textbox', { name: 'Filter groups' });
  await harness.input(filter, 'alpha');
  assert.equal(state.view.value.groups.filter((group) => !group.virtual).length, 1);
  assert.equal(mounted.container.querySelector('#filter-groups-count').textContent, '1 / 2');
  assert.equal(resets, 1);

  const view = getByRole(mounted.container, 'button', { name: '▾ view' });
  await harness.act(() => harness.fireEvent(view, 'click'));
  assert.equal(view.getAttribute('aria-expanded'), 'true');
  assert.ok(mounted.container.querySelector('#filter-groups-view-menu').classList.contains('open'));

  const conversations = getByLabelText(mounted.container, 'show conversations');
  conversations.checked = true;
  await harness.act(() => harness.fireEvent(conversations, 'change'));
  assert.equal(state.visibility.value.conversations, true);
  assert.equal(mounted.container.querySelector('#filter-groups-view-badge').textContent, '1');

  const role = getByLabelText(mounted.container, 'Role');
  role.checked = false;
  await harness.act(() => harness.fireEvent(role, 'change'));
  assert.equal(hiddenRole, true);
  assert.equal(mounted.container.querySelector('#filter-groups-view-badge').textContent, '2');

  await new Promise((resolve) => setTimeout(resolve, 275));
  assert.equal(refreshes, 1, 'query and visibility changes share one debounced refresh');
  await harness.act(() => harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' }));
  assert.equal(view.getAttribute('aria-expanded'), 'false');
  assert.equal(harness.document.activeElement, view);

  await mounted.unmount();
});

test('Groups list owns sort and virtual-list pager event routing', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsList }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
  ]);
  const state = createGroupsState({
    prefs: memoryPrefs({
      'tclaude.dash.ungrouped.groups': '0',
      'tclaude.dash.retired.groups': '0',
    }),
    resetOffsets: () => {},
    ...stateDependencies(),
  });
  state.initialize();
  state.publish({ ...snapshot([]), paging: { retired: { total: 80 } } });
  const calls = [];
  const actions = {
    sort: (...args) => calls.push(['sort', ...args]),
    page: (...args) => calls.push(['page', ...args]),
    setPageSize: (...args) => calls.push(['size', ...args]),
  };
  const renderer = () => `
    <table><thead><tr><th data-sort-table="members" data-sort-col="title">Name</th></tr></thead></table>
    <button data-pager="next" data-list="retired">next</button>
    <select data-pager="size" data-list="retired"><option value="100" selected>100/page</option></select>
  `;
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${GroupsList}
      host=${host}
      state=${state}
      actions=${actions}
      renderGroupsHTML=${renderer}
    />
  `, host);

  await harness.act(() => harness.fireEvent(host.querySelector('th'), 'click'));
  await harness.act(() => harness.fireEvent(host.querySelector('button'), 'click'));
  await harness.act(() => harness.fireEvent(host.querySelector('select'), 'change'));
  assert.deepEqual(calls, [
    ['sort', 'members', 'title'],
    ['page', 'retired', 'next', 80],
    ['size', 'retired', '100'],
  ]);

  await mounted.unmount();
  host.remove();
});
