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

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
  return { promise, resolve, reject };
}

async function mountGroups(t, harness, groups, actions, prefsInitial = {}) {
  const [{ createGroupsState }, { GroupsList }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
  ]);
  const state = createGroupsState({
    prefs: memoryPrefs({
      'tclaude.dash.ungrouped.groups': '0',
      'tclaude.dash.retired.groups': '0',
      ...prefsInitial,
    }),
    resetOffsets: () => {},
    ...stateDependencies(),
  });
  state.initialize();
  state.publish(snapshot(groups));
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${GroupsList} host=${host} state=${state} actions=${actions} />
  `, host);
  t.after(() => host.remove());
  return { state, host, mounted };
}

test('Groups list preserves keyed disclosure, focus and nodes across reorder/activity polls', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsList }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
  ]);
  const prefs = memoryPrefs({
    'tclaude.dash.ungrouped.groups': '0',
    'tclaude.dash.retired.groups': '0',
  });
  const state = createGroupsState({ prefs, resetOffsets: () => {}, ...stateDependencies() });
  state.initialize();
  state.publish(snapshot([
    { name: 'alpha', descr: 'old', members: [{ conv_id: 'alpha-member', title: 'Alpha member', online: true, state: { status: 'working' } }] },
    { name: 'beta', members: [{ conv_id: 'beta-member', title: 'Beta member', online: true, state: { status: 'idle' } }] },
  ]));
  const actions = { sort: () => {}, page: () => {}, setPageSize: () => {} };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${GroupsList}
      host=${host}
      state=${state}
      actions=${actions}
    />
  `, host);

  const alpha = host.querySelector('details[data-group-key="alpha"]');
  const beta = host.querySelector('details[data-group-key="beta"]');
  const inspect = getByRole(alpha, 'button', { name: 'Alpha member' });
  const descr = alpha.querySelector('.group-descr');
  alpha.open = true;
  inspect.focus();

  await harness.act(() => harness.fireEvent(descr, 'click'));
  const editor = alpha.querySelector('.group-descr-input');
  await harness.input(editor, 'draft survives poll');

  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [{ conv_id: 'beta-member', title: 'Beta member', online: true, state: { status: 'awaiting_input' } }] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', title: 'Alpha member', online: true, state: { status: 'working' } }] },
  ])));

  assert.equal(host.querySelector('details[data-group-key="alpha"]'), alpha);
  assert.equal(host.querySelector('details[data-group-key="beta"]'), beta);
  assert.equal(host.querySelector('details:first-child'), beta, 'snapshot reorder moves keyed nodes');
  assert.equal(alpha.open, true, 'live disclosure state is not reset by a reorder');
  assert.equal(alpha.querySelector('.group-descr-input'), editor, 'native inline editor identity survives publish');
  assert.equal(editor.value, 'draft survives poll', 'poll data cannot overwrite a live draft');
  assert.ok(beta.querySelector('.group-activity'));
  await harness.act(() => harness.fireEvent(editor, 'keydown', { key: 'Escape' }));
  assert.match(alpha.querySelector('.group-descr').textContent, /new/, 'cancel reveals the latest published value');
  inspect.focus();

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
    { name: 'beta', members: [{ conv_id: 'beta-member', title: 'Beta member', online: true, state: { status: 'awaiting_input' } }] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', title: 'Alpha member', online: true, state: { status: 'working' } }] },
  ])));
  assert.equal(machine.firstElementChild, activeReel,
    'a same-status publish preserves an in-flight imperative reel pull');

  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [{ conv_id: 'beta-member', title: 'Beta member', online: true, state: { status: 'idle' } }] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', title: 'Alpha member', online: true, state: { status: 'working' } }] },
  ])));
  assert.equal(machine.dataset.status, 'idle');
  assert.equal(machine.querySelector('span').textContent, '7️⃣',
    'a changed status replaces imperative reel children after a manual pull');

  await harness.act(() => state.publish(snapshot([
    { name: 'beta', members: [] },
    { name: 'alpha', descr: 'new', members: [{ conv_id: 'alpha-member', title: 'Alpha member', online: true, state: { status: 'working' } }] },
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
  const loose = host.querySelector('tr[data-dnd-source-ungrouped][data-key="loose"]');
  assert.ok(loose);
  assert.equal(loose.querySelector('.rowname-text').dataset.editorKey,
    'member:virtual:ungrouped:loose:name',
    'virtual member tables contribute their own interaction identity');

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

test('native group and member menus share dismissal, focus, flip and publish state', async (t) => {
  const harness = await createPreactHarness(t);
  const actions = { sort: () => {}, page: () => {}, setPageSize: () => {} };
  const group = {
    name: 'alpha',
    members: [{
      conv_id: 'member-1', agent_id: 'agt-member-1', title: 'Member one', online: true,
      state: { status: 'idle' },
    }],
  };
  const { state, host, mounted } = await mountGroups(t, harness, [group], actions);
  const details = host.querySelector('details[data-group-key="alpha"]');
  const groupCog = details.querySelector('.group-header-cog .cog-btn');
  const groupMenu = details.querySelector('.group-header-cog .action-menu');
  const memberCog = details.querySelector('tr[data-key="member-1"] .cog-btn');
  const memberMenu = details.querySelector('tr[data-key="member-1"] .action-menu');

  Object.defineProperty(harness.window, 'innerHeight', { configurable: true, value: 700 });
  groupMenu.getBoundingClientRect = () => ({ bottom: 900, height: 180 });
  groupCog.getBoundingClientRect = () => ({ top: 650 });
  await harness.act(() => harness.fireEvent(groupCog, 'click'));
  assert.equal(groupCog.getAttribute('aria-expanded'), 'true');
  assert.equal(groupMenu.classList.contains('open'), true);
  assert.equal(groupMenu.classList.contains('opens-up'), true, 'overflowing menus flip above the cog');

  await harness.act(() => state.publish(snapshot([{
    ...group, descr: 'published while open', members: [{ ...group.members[0], state: { status: 'working' } }],
  }])));
  assert.equal(details.querySelector('.group-header-cog .action-menu'), groupMenu);
  assert.equal(groupMenu.classList.contains('open'), true, 'keyed menu state survives a snapshot publish');

  await harness.act(() => harness.fireEvent(memberCog, 'click'));
  assert.equal(groupCog.getAttribute('aria-expanded'), 'false');
  assert.equal(groupMenu.classList.contains('open'), false);
  assert.equal(memberCog.getAttribute('aria-expanded'), 'true');
  assert.equal(memberMenu.classList.contains('open'), true, 'member and group menus are mutually exclusive');

  const delegatedItem = memberMenu.querySelector('button[data-act="term"]');
  await harness.act(() => harness.fireEvent(delegatedItem, 'click'));
  assert.equal(delegatedItem.isConnected, true, 'dismissal leaves the delegated action target connected');
  assert.equal(memberMenu.classList.contains('open'), false, 'a menu item dismisses its menu');

  await harness.act(() => harness.fireEvent(memberCog, 'click'));
  await harness.act(() => harness.fireEvent(harness.document.body, 'click'));
  assert.equal(memberMenu.classList.contains('open'), false, 'outside click dismisses the menu');

  await harness.act(() => harness.fireEvent(groupCog, 'click'));
  const focusedItem = groupMenu.querySelector('button[data-act="add-member"]');
  focusedItem.focus();
  await harness.act(() => harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' }));
  assert.equal(groupMenu.classList.contains('open'), false);
  assert.equal(harness.document.activeElement, groupCog, 'Escape returns focus to the owning cog');

  await mounted.unmount();
});

test('multi-group member copies isolate menus, editors, focus and unmount cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const member = {
    conv_id: 'shared-member', agent_id: 'agt-shared-member', title: 'Shared member', online: true,
    state: { status: 'idle', harness: 'claude' },
  };
  const alpha = { name: 'alpha', members: [member] };
  const beta = { name: 'beta', members: [member] };
  const actions = {
    sort: () => {}, page: () => {}, setPageSize: () => {},
    renameAgent: async () => true,
  };
  const { state, host, mounted } = await mountGroups(t, harness, [alpha, beta], actions);
  const alphaDetails = host.querySelector('details[data-group-key="alpha"]');
  const betaDetails = host.querySelector('details[data-group-key="beta"]');
  const alphaRow = alphaDetails.querySelector('tr[data-key="shared-member"]');
  const betaRow = betaDetails.querySelector('tr[data-key="shared-member"]');
  const alphaCog = alphaRow.querySelector('.cog-btn');
  const betaCog = betaRow.querySelector('.cog-btn');
  const alphaMenu = alphaRow.querySelector('.action-menu');
  const betaMenu = betaRow.querySelector('.action-menu');

  await harness.act(() => harness.fireEvent(alphaCog, 'click'));
  assert.equal(alphaMenu.classList.contains('open'), true);
  assert.equal(betaMenu.classList.contains('open'), false,
    'opening one membership copy does not open the same agent in another group');

  const alphaTrigger = alphaRow.querySelector('.rowname-text');
  await harness.act(() => harness.fireEvent(alphaTrigger, 'click'));
  const alphaInput = alphaRow.querySelector('.rowname-input');
  assert.ok(alphaInput);
  assert.equal(betaRow.querySelector('.rowname-input'), null,
    'editing one membership copy does not replace the other group copy');
  assert.equal(alphaRow.draggable, false);
  assert.equal(betaRow.draggable, true);
  await harness.act(() => harness.fireEvent(alphaInput, 'keydown', { key: 'Escape' }));
  const restoredAlphaTrigger = alphaRow.querySelector('.rowname-text');
  assert.equal(harness.document.activeElement, restoredAlphaTrigger);
  assert.equal(restoredAlphaTrigger.dataset.editorKey,
    'member:group:alpha:agt-shared-member:name');

  await harness.act(() => harness.fireEvent(betaCog, 'click'));
  assert.equal(betaMenu.classList.contains('open'), true);
  await harness.act(() => state.publish(snapshot([{
    ...beta, members: [{ ...member, title: 'Published shared member' }],
  }])));
  const survivingBeta = host.querySelector('details[data-group-key="beta"]');
  const survivingCog = survivingBeta.querySelector('tr[data-key="shared-member"] .cog-btn');
  const survivingMenu = survivingBeta.querySelector('tr[data-key="shared-member"] .action-menu');
  assert.equal(survivingMenu.classList.contains('open'), true,
    'removing another membership leaves the surviving menu registration live');
  survivingMenu.querySelector('button[data-act="term"]').focus();
  await harness.act(() => harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' }));
  assert.equal(survivingMenu.classList.contains('open'), false);
  assert.equal(harness.document.activeElement, survivingCog,
    'Escape returns focus through the surviving registration after sibling unmount');

  await mounted.unmount();
});

test('native member and group editors preserve drafts, park DnD and surface busy/error state', async (t) => {
  const harness = await createPreactHarness(t);
  const calls = [];
  let renameAgentResult = async () => true;
  let renameGroupResult = async () => true;
  let patchResult = async () => true;
  let profileChoicesResult = async () => [{ value: 'profile-b', label: 'Profile B' }];
  let setProfileResult = async () => true;
  const actions = {
    sort: () => {}, page: () => {}, setPageSize: () => {},
    reportError: (error) => calls.push(['report-error', error.message || String(error)]),
    renameAgent: (member, value) => { calls.push(['rename-agent', member.conv_id, value]); return renameAgentResult(); },
    renameGroup: (group, value) => { calls.push(['rename-group', group.name, value]); return renameGroupResult(); },
    patchGroup: (group, field, value) => { calls.push(['patch', group.name, field, value]); return patchResult(); },
    pickGroupDirectory: async () => false,
    groupProfileChoices: (kind) => { calls.push(['choices', kind]); return profileChoicesResult(); },
    setGroupProfile: (group, kind, name) => { calls.push(['profile', group.name, kind, name]); return setProfileResult(); },
    openNewGroupProfile: (kind, onSaved) => { calls.push(['new-profile', kind, onSaved]); },
  };
  const group = {
    name: 'alpha', descr: 'old descr', default_cwd: '/tmp/old', max_members: 2,
    default_profile: 'profile-a', sandbox_profile: 'sandbox-a',
    members: [{
      conv_id: 'member-1', agent_id: 'agt-member-1', title: 'Member one', online: true,
      state: { status: 'idle', harness: 'claude' },
    }],
  };
  const { state, host, mounted } = await mountGroups(t, harness, [group], actions);
  const details = host.querySelector('details[data-group-key="alpha"]');
  const summary = details.querySelector('summary');
  const row = details.querySelector('tr[data-key="member-1"]');

  const agentSave = deferred();
  renameAgentResult = () => agentSave.promise;
  await harness.act(() => harness.fireEvent(row.querySelector('.rowname-text'), 'click'));
  const nameInput = row.querySelector('.rowname-input');
  assert.equal(row.draggable, false);
  await harness.input(nameInput, 'Member draft');
  await harness.act(() => state.publish(snapshot([{
    ...group, members: [{ ...group.members[0], title: 'Published title' }],
  }])));
  assert.equal(row.querySelector('.rowname-input'), nameInput);
  assert.equal(nameInput.value, 'Member draft', 'member rename draft survives a publish');
  harness.fireEvent(nameInput, 'keydown', { key: 'Enter' });
  await Promise.resolve();
  assert.equal(nameInput.disabled, true);
  assert.equal(row.draggable, false, 'member row stays parked throughout persistence');
  await harness.act(() => agentSave.resolve(true));
  await harness.act(() => Promise.resolve());
  assert.equal(row.querySelector('.rowname-input'), null);
  assert.equal(row.draggable, true);
  assert.deepEqual(calls.shift(), ['rename-agent', 'member-1', 'Member draft']);

  renameAgentResult = async () => { throw new Error('rename rejected'); };
  await harness.act(() => harness.fireEvent(row.querySelector('.rowname-text'), 'click'));
  const failedName = row.querySelector('.rowname-input');
  await harness.input(failedName, 'Bad title');
  await harness.act(() => harness.fireEvent(failedName, 'keydown', { key: 'Enter' }));
  await harness.act(() => Promise.resolve());
  assert.equal(failedName.disabled, false);
  assert.equal(failedName.getAttribute('aria-invalid'), 'true');
  assert.equal(failedName.title, 'rename rejected');
  await harness.act(() => harness.fireEvent(failedName, 'keydown', { key: 'Escape' }));
  assert.equal(harness.document.activeElement.dataset.editorKey,
    'member:group:alpha:agt-member-1:name');
  calls.shift();

  const descrTrigger = summary.querySelector('.group-descr');
  await harness.act(() => harness.fireEvent(descrTrigger, 'click'));
  const descrInput = summary.querySelector('.group-descr-input');
  await harness.input(descrInput, 'description draft');
  assert.equal(summary.draggable, false);
  await harness.act(() => state.publish(snapshot([{ ...group, descr: 'new poll descr' }])));
  assert.equal(summary.querySelector('.group-descr-input'), descrInput);
  assert.equal(descrInput.value, 'description draft');
  await harness.act(() => harness.fireEvent(descrInput, 'keydown', { key: 'Escape' }));
  assert.equal(summary.draggable, true);
  assert.equal(harness.document.activeElement.dataset.editorKey, 'group:alpha:descr');

  const cwdSave = deferred();
  patchResult = () => cwdSave.promise;
  await harness.act(() => harness.fireEvent(summary.querySelector('.group-default-cwd'), 'click'));
  const cwdInput = summary.querySelector('.group-default-cwd-input');
  await harness.input(cwdInput, ' /tmp/new ');
  harness.fireEvent(cwdInput, 'keydown', { key: 'Enter' });
  await Promise.resolve();
  assert.equal(cwdInput.disabled, true);
  assert.equal(summary.draggable, false);
  await harness.act(() => cwdSave.resolve(true));
  await harness.act(() => Promise.resolve());
  assert.equal(summary.querySelector('.group-default-cwd-input'), null);
  assert.deepEqual(calls.shift(), ['patch', 'alpha', 'default_cwd', '/tmp/new']);

  await harness.act(() => harness.fireEvent(summary.querySelector('.group-max-members'), 'click'));
  const maxInput = summary.querySelector('.group-max-members-input');
  await harness.input(maxInput, '-1');
  await harness.act(() => harness.fireEvent(maxInput, 'keydown', { key: 'Enter' }));
  await harness.act(() => Promise.resolve());
  assert.equal(maxInput.getAttribute('aria-invalid'), 'true');
  assert.match(maxInput.title, /non-negative integer/);
  assert.equal(summary.draggable, false, 'validation errors keep the editor and DnD park live');
  await harness.act(() => harness.fireEvent(maxInput, 'blur'));

  const groupCog = summary.querySelector('.group-header-cog .cog-btn');
  await harness.act(() => harness.fireEvent(groupCog, 'click'));
  await harness.act(() => harness.fireEvent(summary.querySelector('[data-act="rename-group"]'), 'click'));
  const renameInput = summary.querySelector('.group-rename-input');
  assert.equal(summary.draggable, false);
  await harness.act(() => harness.fireEvent(renameInput, 'keydown', { key: 'Escape' }));
  await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement, groupCog, 'group rename Escape returns to its menu cog');

  const choicesLoad = deferred();
  profileChoicesResult = () => choicesLoad.promise;
  const profileTrigger = summary.querySelector('.group-default-model');
  await harness.act(() => harness.fireEvent(profileTrigger, 'click'));
  const profileSelect = summary.querySelector('.group-default-profile-select');
  assert.equal(profileSelect.disabled, true);
  assert.equal(summary.draggable, false);
  await harness.act(() => state.publish(snapshot([{ ...group, default_profile: 'published-profile' }])));
  assert.equal(summary.querySelector('.group-default-profile-select'), profileSelect,
    'profile picker identity survives a publish while choices load');
  await harness.act(() => choicesLoad.resolve([{ value: 'profile-b', label: 'Profile B' }]));
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(profileSelect.disabled, false);
  assert.equal(harness.document.activeElement, profileSelect);
  const profileSave = deferred();
  setProfileResult = () => profileSave.promise;
  profileSelect.querySelector('option[value="profile-b"]').selected = true;
  harness.fireEvent(profileSelect, 'change');
  await Promise.resolve();
  assert.equal(profileSelect.disabled, true);
  await harness.act(() => profileSave.resolve(true));
  await harness.act(() => Promise.resolve());
  assert.equal(summary.querySelector('.group-default-profile-select'), null);
  assert.deepEqual(calls.splice(0, 2), [
    ['choices', 'profile'],
    ['profile', 'alpha', 'profile', 'profile-b'],
  ]);

  profileChoicesResult = async () => [{ value: 'sandbox-b', label: 'Sandbox B' }];
  setProfileResult = async () => { throw new Error('sandbox rejected'); };
  const sandboxTrigger = summary.querySelector('.group-sandbox-profile');
  await harness.act(() => harness.fireEvent(sandboxTrigger, 'click'));
  const sandboxSelect = summary.querySelector('.group-default-profile-select');
  await harness.act(() => Promise.resolve());
  sandboxSelect.querySelector('option[value="sandbox-b"]').selected = true;
  await harness.act(() => harness.fireEvent(sandboxSelect, 'change'));
  await harness.act(() => Promise.resolve());
  assert.equal(sandboxSelect.disabled, false);
  assert.equal(sandboxSelect.getAttribute('aria-invalid'), 'true');
  assert.equal(sandboxSelect.title, 'sandbox rejected');
  assert.equal(harness.document.activeElement, sandboxSelect, 'failed persistence restores picker focus');
  await harness.act(() => harness.fireEvent(sandboxSelect, 'keydown', { key: 'Escape' }));
  await harness.act(() => Promise.resolve());
  const restoredSandboxTrigger = summary.querySelector('.group-sandbox-profile');
  assert.equal(sandboxTrigger.isConnected, false, 'the picker replaces its original trigger node');
  assert.equal(harness.document.activeElement, restoredSandboxTrigger,
    'Escape focuses the locally committed replacement profile trigger');
  assert.equal(restoredSandboxTrigger.dataset.editorKey, 'group:alpha:sandbox_profile');
  calls.splice(0, 2);

  profileChoicesResult = async () => [];
  await harness.act(() => harness.fireEvent(summary.querySelector('.group-sandbox-profile'), 'click'));
  const newSandbox = summary.querySelector('.group-default-profile-select');
  await harness.act(() => Promise.resolve());
  newSandbox.querySelector('option[value="/new-profile"]').selected = true;
  await harness.act(() => harness.fireEvent(newSandbox, 'change'));
  assert.equal(summary.querySelector('.group-default-profile-select'), null);
  assert.equal(calls[0][0], 'choices');
  assert.equal(calls[1][0], 'new-profile');
  assert.equal(calls[1][1], 'sandbox');
  assert.equal(calls[1][2]('created-sandbox'), undefined,
    'post-create assignment is reported asynchronously instead of escaping the editor');
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.deepEqual(calls.slice(2, 4), [
    ['profile', 'alpha', 'sandbox', 'created-sandbox'],
    ['report-error', 'sandbox rejected'],
  ]);

  // A successful rename still uses the same native editor/action boundary.
  renameGroupResult = async () => true;
  await harness.act(() => harness.fireEvent(groupCog, 'click'));
  await harness.act(() => harness.fireEvent(summary.querySelector('[data-act="rename-group"]'), 'click'));
  const savedRename = summary.querySelector('.group-rename-input');
  await harness.input(savedRename, 'beta');
  await harness.act(() => harness.fireEvent(savedRename, 'keydown', { key: 'Enter' }));
  assert.deepEqual(calls.at(-1), ['rename-group', 'alpha', 'beta']);

  await mounted.unmount();
});

test('native member rows preserve the legacy field, capability and selector matrix', async (t) => {
  const harness = await createPreactHarness(t);
  const actions = {
    sort: () => {}, page: () => {}, setPageSize: () => {}, renameAgent: async () => true,
  };
  const rich = {
    conv_id: 'conv-rich', agent_id: 'agt-rich', title: 'Rich agent', online: true,
    created_at: '2026-07-14T12:00:00Z', role: 'builder', owner: true,
    descr: 'Ships UI', tags: ['frontend', 'tf:native'], notify: 'on', notify_effective: true,
    startup_dir: '/home/user/git/tclaude', current_dir: '/home/user/git/tclaude-wt',
    startup_branch: 'main', startup_branch_url: 'https://github.com/example/tclaude/compare/main',
    branch: 'feature', branch_url: 'https://github.com/example/tclaude/compare/feature',
    branch_pr_number: 42, branch_pr_url: 'https://github.com/example/tclaude/pull/42', branch_pr_state: 'open',
    presented_prs: [
      { number: 43, url: 'https://github.com/example/tclaude/pull/43', state: 'merged', summary: 'Follow-up' },
      { number: 99, url: 'javascript:alert(1)', summary: 'unsafe' },
    ],
    task_ref_url: 'https://linear.app/example/issue/TCL-465', task_ref_label: 'TCL-465',
    task_ref_label_override: 'Native tables',
    state: {
      status: '', status_detail: '', last_hook: '2026-07-15T00:00:00Z', harness: 'claude',
      model: 'Opus 4.8 (1M context)', effort_level: 'high', cost_usd: 0.004,
      virtual_cost_usd: 0.42, remote_control: true, sandbox_mode: 'workspace-write',
      context_pct: 41, context_window_size: 1000000, tokens_input: 400000, tokens_output: 10000,
      subagent_count: 2,
    },
  };
  const fixed = {
    conv_id: 'conv-fixed', agent_id: 'agt-fixed', title: 'Fixed agent', online: false,
    role: '', descr: '', tags: [], state: { harness: 'codex', exit_reason: 'unexpected', status_detail: 'stale detail' },
  };
  const fullSnapshot = {
    ...snapshot([{ name: 'alpha', members: [rich, fixed] }]),
    harnesses: [
      { name: 'claude', can_rename: true, can_remote_control: true },
      { name: 'codex', can_rename: false, can_remote_control: false },
    ],
    sudo: [{ conv_id: 'conv-rich', slug: 'groups.manage', remaining_seconds: 90 }],
  };
  const [{ createGroupsState }, { GroupsList }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
  ]);
  const state = createGroupsState({ prefs: memoryPrefs(), resetOffsets: () => {}, ...stateDependencies() });
  state.initialize();
  state.publish(fullSnapshot);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${GroupsList} host=${host} state=${state} actions=${actions} />`, host);
  const table = host.querySelector('details[data-group-key="alpha"] table');
  const rows = [...table.querySelectorAll('tbody tr')];
  const richRow = rows.find((row) => row.dataset.key === 'conv-rich');
  const fixedRow = rows.find((row) => row.dataset.key === 'conv-fixed');
  assert.equal(table.querySelectorAll('thead th').length, 11);
  assert.equal(richRow.children.length, 11, 'header/body alignment follows the shared visible-column list');
  assert.equal(richRow.className, 'dnd-draggable');
  assert.equal(richRow.draggable, true);
  assert.equal(richRow.dataset.dndSourceGroup, 'alpha');
  assert.equal(richRow.dataset.dndConv, 'conv-rich');
  assert.equal(richRow.dataset.dndAgent, 'agt-rich');
  assert.equal(richRow.dataset.dndLabel, 'Rich agent');

  const dot = richRow.querySelector('.status-dot');
  assert.equal(dot.dataset.act, 'dot-toggle');
  assert.equal(dot.dataset.online, '1');
  assert.match(dot.title, /Claude Code · Opus 4\.8/);
  const harnessLine = richRow.querySelector('.agent-harness');
  assert.match(harnessLine.textContent, /CC·O4\.8 1Mhigh<1¢≈\$0\.42📱/);
  assert.match(harnessLine.title, /WHAT-IF cost this session/);
  assert.equal(richRow.querySelector('.sandbox-badge').textContent, '🔒 workspace-write');
  assert.equal(richRow.querySelector('.remote-badge').dataset.act, 'web-open-window');

  assert.equal(richRow.querySelector('.state-pill').textContent, 'online');
  assert.equal(richRow.querySelector('.state-pill').classList.contains('state-idle'), true,
    'blank online status keeps the legacy idle color');
  assert.equal(richRow.querySelector('.slop-machine').dataset.status, 'idle');
  assert.equal(richRow.querySelector('.slop-machine').textContent, '7️⃣7️⃣7️⃣');
  assert.match(richRow.querySelector('.wizard-pill').textContent, /Meditating/);
  assert.equal(richRow.querySelectorAll('.ctx-meter').length, 2);
  assert.equal(richRow.querySelectorAll('.ctx-seg').length, 10);
  assert.equal(richRow.querySelectorAll('.ctx-seg.lit-green').length, 4);
  assert.equal(richRow.querySelector('.badge-subagents').textContent, '🤖+2');

  assert.equal(richRow.querySelector('.role-edit').dataset.owner, '1');
  assert.match(richRow.querySelector('.role-edit').textContent, /builder owner/);
  assert.equal(richRow.querySelector('.descr-edit').dataset.tags, 'frontend, tf:native');
  assert.equal(richRow.querySelector('.agent-tag-tf').title, 'task force: native');
  assert.equal(richRow.querySelector('.task-link').draggable, false);
  assert.equal(richRow.querySelector('.task-edit').dataset.currentTaskLabel, 'Native tables');
  assert.equal(richRow.querySelectorAll('.loc-pair').length, 2);
  assert.equal(richRow.querySelector('.branch-link').draggable, false);
  assert.equal(richRow.querySelector('.pr-state-open').textContent, '#42');
  assert.equal(richRow.querySelector('.pr-state-merged').textContent, '#43');
  assert.equal(richRow.querySelectorAll('[href^="javascript:"]').length, 0);
  assert.match(richRow.querySelector('.sudo-badge').title, /groups\.manage \(expires in 1m30s\)/);

  const menu = richRow.querySelector('.action-menu');
  assert.equal(menu.getAttribute('role'), 'menu');
  assert.equal(menu.querySelectorAll('button:not([role="menuitem"])').length, 0);
  assert.equal(menu.querySelector('[data-act="toggle-remote-control"]').dataset.intent, 'off');
  assert.equal(menu.querySelector('[data-act="retire-agent"]').hasAttribute('data-agent'), false,
    'retire remains deliberately conv-keyed for dangling-agent recovery');
  assert.equal(menu.querySelector('[data-act="perm-edit"]').hasAttribute('data-agent'), false);
  assert.equal(menu.querySelector('[data-act="sudo-grant"]').hasAttribute('data-agent'), false);
  assert.equal(menu.querySelector('[data-act="cron-new"]').hasAttribute('data-conv'), false);
  assert.equal(menu.querySelector('[data-act="cron-new"]').hasAttribute('data-agent'), false);
  assert.match(menu.querySelector('[data-act="cron-new"]').dataset.prefill, /"target":"agt-rich"/);
  assert.equal(menu.querySelectorAll('.menu-sep').length, 3);

  assert.equal(fixedRow.querySelector('.rowname-fixed').textContent, 'Fixed agent');
  assert.equal(fixedRow.querySelector('[data-act="rename-name"]'), null);
  assert.equal(fixedRow.querySelector('[data-act="toggle-remote-control"]'), null);
  assert.equal(fixedRow.querySelector('.state-pill').classList.contains('state-crashed'), true);
  assert.equal(fixedRow.querySelector('.slop-machine').title, 'crashed: stale detail');
  assert.equal(fixedRow.querySelector('.wizard-pill').title, 'crashed: stale detail');

  harness.document.body.classList.add('wizard');
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:wizard', { detail: { active: true } },
  )));
  assert.equal(menu.querySelector('[data-act="view-agent-messages"] .theme-copy-wizard').textContent, 'view missives');
  assert.equal(table.querySelectorAll('thead .theme-copy-wizard')[0].textContent, 'Class');

  harness.document.body.classList.remove('wizard');
  await mounted.unmount();
  host.remove();
});
