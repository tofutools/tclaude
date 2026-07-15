import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

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
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
  };
}

function testPresentation() {
  return {
    memberTable(members, context) {
      return `<table><tbody>${members.map((member) => {
        const status = member.state?.status || '';
        return `<tr data-key="${member.conv_id || member.title}"${context.ungrouped ? ' data-dnd-source-ungrouped="1"' : ''}><td><button data-act="inspect" aria-label="inspect ${context.group?.name || 'ungrouped'}">inspect</button></td><td class="state-cell"><span class="slop-machine" data-status="${status}"><span>${status}</span></span></td></tr>`;
      }).join('')}</tbody></table>`;
    },
  };
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
    { name: 'alpha', descr: 'old', members: [{ conv_id: 'alpha-member', online: true, state: { status: 'working' } }] },
    { name: 'beta', members: [{ conv_id: 'beta-member', online: true, state: { status: 'idle' } }] },
  ]));
  const actions = { sort: () => {}, page: () => {}, setPageSize: () => {} };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${GroupsList}
      host=${host}
      state=${state}
      actions=${actions}
      presentation=${testPresentation()}
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
    { name: 'beta', members: [{ conv_id: 'beta-member', online: true, state: { status: 'awaiting_input' } }] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', online: true, state: { status: 'working' } }] },
  ])));

  assert.equal(host.querySelector('details[data-group-key="alpha"]'), alpha);
  assert.equal(host.querySelector('details[data-group-key="beta"]'), beta);
  assert.equal(host.querySelector('details:first-child'), beta, 'snapshot reorder moves keyed nodes');
  assert.equal(alpha.open, true, 'live disclosure state is not reset by a reorder');
  assert.equal(harness.document.activeElement, inspect, 'focused legacy action remains focused');
  assert.equal(alpha.querySelector('.group-descr'), descr, 'inline-edit host identity survives publish');
  assert.match(descr.textContent, /new/, 'restored inline-edit host receives later updates');
  assert.ok(beta.querySelector('.group-activity'));

  harness.document.body.classList.add('wizard');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:wizard', { detail: { active: true } },
  )));
  assert.equal(host.querySelector('details[data-group-key="alpha"]'), alpha,
    'the theme repaint preserves keyed group disclosure identity');

  const machine = beta.querySelector('.slop-machine');
  const activeReel = harness.document.createElement('span');
  activeReel.textContent = 'spinning';
  machine.replaceChildren(activeReel); // manual reel pull installs foreign children
  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [{ conv_id: 'beta-member', online: true, state: { status: 'awaiting_input' } }] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', online: true, state: { status: 'working' } }] },
  ])));
  assert.equal(machine.firstElementChild, activeReel,
    'a same-status publish preserves an in-flight imperative reel pull');

  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [{ conv_id: 'beta-member', online: true, state: { status: 'idle' } }] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', online: true, state: { status: 'working' } }] },
  ])));
  assert.equal(machine.querySelector('span').textContent, 'idle',
    'a changed status replaces imperative reel children after a manual pull');

  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', online: true, state: { status: 'working' } }] },
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
    list: () => [{ key: 'role', label: 'Role', wizardLabel: 'Class' }],
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
  assert.equal(filter.placeholder, 'Filter (group name + member title/role/descr/cwd/branch)');
  await harness.input(filter, 'alpha');
  assert.equal(state.view.value.groups.filter((group) => !group.virtual).length, 1);
  const count = mounted.container.querySelector('#filter-groups-count');
  assert.equal(count.querySelector('.theme-copy-regular').textContent, '1 / 2');
  assert.equal(count.querySelector('.theme-copy-wizard').textContent, '1 / 2');
  assert.equal(resets, 1);

  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:wizard', { detail: { active: true } },
  )));
  assert.equal(filter.getAttribute('aria-label'), 'Filter parties');
  assert.equal(filter.placeholder, 'Filter (party name + familiar title/class/lore/grove/branch)');

  const view = getByRole(mounted.container, 'button', { name: '▾ view' });
  await harness.act(() => harness.fireEvent(view, 'click'));
  assert.equal(view.getAttribute('aria-expanded'), 'true');
  assert.ok(mounted.container.querySelector('#filter-groups-view-menu').classList.contains('open'));

  const conversations = mounted.container.querySelector('#filter-groups-conversations');
  conversations.checked = true;
  await harness.act(() => harness.fireEvent(conversations, 'change'));
  assert.equal(state.visibility.value.conversations, true);
  assert.equal(mounted.container.querySelector('#filter-groups-view-badge').textContent, '1');

  const role = mounted.container.querySelector('#filter-groups-col-role');
  const roleLabel = role.closest('label');
  assert.equal(roleLabel.querySelector('.theme-copy-regular').textContent, 'Role');
  assert.equal(roleLabel.querySelector('.theme-copy-wizard').textContent, 'Class');
  assert.equal(roleLabel.title, 'Show the "Class" column');
  harness.document.body.classList.remove('wizard');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:wizard', { detail: { active: false } },
  )));
  assert.equal(roleLabel.title, 'Show the "Role" column', 'the full label tooltip swaps with the theme');
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

test('native group chrome preserves hierarchy, virtual DnD and shared menu contracts', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsList }, { setHoveredGroupKey }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
    harness.importDashboardModule('js/group-hover-state.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  const state = createGroupsState({
    prefs: memoryPrefs({
      'tclaude.dash.ungrouped.groups': '1',
      'tclaude.dash.retired.groups': '1',
    }),
    resetOffsets: () => {},
    ...stateDependencies(),
  });
  state.initialize();
  const chromeSnapshot = ({ pendingAgentID, retiredAgentID, ungroupedOnline = true } = {}) => ({
    ...snapshot([
      {
        name: 'parent', members: [], mission: 'ship it', source_template: 'delivery',
        pending: [{ label: 'spawn-1', name: 'worker', online: true }],
      },
      { name: 'child', parent: 'parent', members: [] },
    ]),
    ungrouped: [{ conv_id: 'loose', title: 'Loose', online: ungroupedOnline }],
    pending: [{ label: 'gate-1', name: 'Gated', online: true, agent_id: pendingAgentID }],
    retired: [{ conv_id: 'retired-1', title: 'Retired', agent_id: retiredAgentID }],
    links: [{ id: 7, from: 'parent', to: 'child', mode: 'direct' }],
  });
  state.publish(chromeSnapshot());
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'groups-list';
  const mounted = await harness.mount(harness.html`<${GroupsList}
    host=${host} state=${state}
    actions=${{ sort: () => {}, page: () => {}, setPageSize: () => {} }}
    presentation=${testPresentation()}
  />`, host);

  const parent = host.querySelector('details[data-dnd-target-group="parent"]');
  const child = host.querySelector('details[data-dnd-target-group="child"]');
  setHoveredGroupKey('parent');
  await harness.act(() => state.rerender());
  assert.equal(parent.classList.contains('quick-hover'), true);
  dashPrefs.syncItem('tclaude.dash.quickpin.parent', '1');
  await harness.act(() => state.rerender());
  assert.equal(parent.classList.contains('quick-pinned'), true);
  dashPrefs.syncItem('tclaude.dash.quickpin.parent', null);
  await harness.act(() => state.rerender());
  assert.equal(parent.classList.contains('quick-hover'), true,
    'unpinning under a stationary cursor re-stamps the tracked hover class');
  assert.ok(parent.querySelector('.group-subgroups').contains(child));
  assert.equal(parent.querySelector('.cog-btn').dataset.act, 'group-menu');
  assert.equal(parent.querySelector('.cog-btn').getAttribute('aria-expanded'), 'false');
  assert.ok(parent.querySelector('.group-force-block'));
  assert.ok(parent.querySelector('.group-pending-block tr[data-dnd-pending]'));
  assert.ok(parent.querySelector('.group-links-section button[data-act="link-edit"]'));
  assert.match(parent.querySelector('[data-act="power-on-group"]').title, /nothing new is created/);
  assert.match(parent.querySelector('[data-act="stand-down-force"]').title, /Not a delete/);
  assert.match(parent.querySelector('[data-act="cleanup-worktrees-group"]').title, /protected/);
  assert.equal(parent.querySelector('[data-act="link-delete"]').title, 'Remove this link');
  assert.equal(host.querySelector('details[data-dnd-target-ungrouped]').dataset.groupKey, ' ungrouped-virtual');
  assert.ok(host.querySelector('tr[data-dnd-source-ungrouped][data-key="loose"]'));

  const pending = host.querySelector('tr[data-key="gate-1"]');
  const retired = host.querySelector('tr[data-key="retired-1"]');
  assert.match(pending.querySelector('[data-act="focus-pending"]').title, /startup gate/);
  assert.match(retired.querySelector('[data-act="reinstate-agent"]').title, /Reinstate/);
  await harness.act(() => state.publish(chromeSnapshot({
    pendingAgentID: 'agt-pending', retiredAgentID: 'agt-retired',
  })));
  assert.equal(host.querySelector('tr[data-key="gate-1"]'), pending,
    'pending row identity stays label-keyed when agent metadata materializes');
  assert.equal(host.querySelector('tr[data-key="retired-1"]'), retired,
    'retired row identity stays conv-id-keyed when agent metadata materializes');

  assert.match(retired.closest('details').querySelector('.group-virtual-badge').title,
    /Drag an agent here to retire it/);
  const offlineToggle = harness.document.createElement('input');
  offlineToggle.id = 'filter-groups-offline';
  offlineToggle.checked = false;
  harness.document.body.appendChild(offlineToggle);
  harness.document.body.classList.add('wizard');
  await harness.act(() => state.publish(chromeSnapshot({ ungroupedOnline: false })));
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:wizard', { detail: { active: true } },
  )));
  const ungrouped = host.querySelector('details[data-dnd-target-ungrouped]');
  assert.match(ungrouped.querySelector('.subtable .theme-copy-wizard').textContent,
    /slumbering familiar.*show slumbering/);
  assert.match(ungrouped.querySelector('.group-virtual-badge').title,
    /cannot be renamed, dispelled, whispered to, or scheduled/);

  setHoveredGroupKey(null);
  harness.document.body.classList.remove('wizard');
  offlineToggle.remove();

  await mounted.unmount();
  host.remove();
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
  state.setVisible('retired', true);
  state.publish({ ...snapshot([]), retired: [{
    conv_id: 'retired-1', title: 'Retired', retired_at: '2026-01-01T00:00:00Z',
  }], paging: { retired: { offset: 0, limit: 50, total: 80 } } });
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${GroupsList}
      host=${host}
      state=${state}
      actions=${actions}
      presentation=${testPresentation()}
    />
  `, host);

  await harness.act(() => harness.fireEvent(host.querySelector('th[data-sort-col]'), 'click'));
  await harness.act(() => harness.fireEvent(host.querySelector('button[data-pager="next"]'), 'click'));
  host.querySelector('select option[value="100"]').selected = true;
  await harness.act(() => harness.fireEvent(host.querySelector('select'), 'change'));
  assert.deepEqual(calls, [
    ['sort', 'retired', 'id'],
    ['page', 'retired', 'next', 80],
    ['size', 'retired', '100'],
  ]);

  await mounted.unmount();
  host.remove();
});
