import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function memoryPrefs() {
  const values = new Map([
    ['tclaude.dash.ungrouped.groups', '0'],
    ['tclaude.dash.retired.groups', '0'],
  ]);
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

function stateDependencies() {
  return {
    columns: {
      list: () => [{ key: 'id', label: 'ID' }],
      hidden: () => false,
      setHidden: () => {},
      deviationCount: () => 0,
    },
    reorder: (groups) => groups,
  };
}

function snapshot(groups, extra = {}) {
  return {
    groups,
    agents: [],
    pending: [],
    ungrouped: [],
    retired: [],
    conversations: [],
    replaced: [],
    paging: {},
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
    ...extra,
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
  return { promise, resolve, reject };
}

test('add-member candidate and request models preserve pool, metadata and selector boundaries', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ buildAddMemberCandidates }, {
    addExistingMemberRequest, loadAddMemberPromotionPool,
  }] = await Promise.all([
    harness.importDashboardModule('js/add-member-dialog-island.js'),
    harness.importDashboardModule('js/add-member-actions.js'),
  ]);
  const snap = snapshot([
    { name: 'alpha', members: [{ conv_id: 'existing', title: 'Existing' }] },
    { name: 'beta', members: [{ conv_id: 'reviewer', role: 'reviewer', descr: 'checks releases' }] },
  ], {
    ungrouped: [{ conv_id: 'online', title: 'Alpha', online: true }],
    agents: [
      { conv_id: 'reviewer', agent_id: 'agt-reviewer', title: 'Bravo', online: true, groups: ['beta'] },
      { conv_id: 'offline', agent_id: 'agt-offline', title: 'Dormant', online: false },
      { conv_id: 'existing', title: 'Existing', online: true },
    ],
  });
  const pool = [
    { conv_id: 'plain', title: 'Charlie', online: true },
    { conv_id: 'online', title: 'duplicate', online: true },
  ];
  const defaultRows = buildAddMemberCandidates({ snapshot: snap, promotionPool: pool, group: 'alpha' });
  assert.deepEqual(defaultRows.map((row) => row.title), ['Alpha', 'Bravo', 'Charlie']);
  assert.equal(defaultRows.find((row) => row.conv_id === 'plain')._promote, true);
  assert.deepEqual(buildAddMemberCandidates({
    snapshot: snap, promotionPool: pool, group: 'alpha', query: 'checks',
  }).map((row) => row.conv_id), ['reviewer'], 'role/description metadata participates in search');
  assert.ok(buildAddMemberCandidates({
    snapshot: snap, promotionPool: pool, group: 'alpha', includeOffline: true,
  }).some((row) => row.conv_id === 'offline'));

  const requests = [];
  const fetchImpl = async (url, options = {}) => {
    requests.push([url, options]);
    if (url === '/api/conversations') {
      return new Response(JSON.stringify({ rows: pool }), { status: 200 });
    }
    return new Response(JSON.stringify({ conv_id: 'reviewer' }), { status: 200 });
  };
  assert.deepEqual(await loadAddMemberPromotionPool({ fetchImpl }), pool);
  await addExistingMemberRequest({ group: 'alpha group', candidate: defaultRows[1], fetchImpl });
  assert.deepEqual(requests.map(([url]) => url), [
    '/api/conversations',
    '/api/groups/alpha%20group/members',
  ]);
  assert.deepEqual(JSON.parse(requests[1][1].body), { conv: 'agt-reviewer' },
    'enrolled candidates use their rotation-stable agent selector');
});

test('group menu launches a polling-stable dirty picker and restores its cog focus', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsList }, { GroupsAddMemberDialog }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
    harness.importDashboardModule('js/add-member-dialog-island.js'),
  ]);
  const state = createGroupsState({
    prefs: memoryPrefs(), resetOffsets: () => {}, ...stateDependencies(),
  });
  state.initialize();
  state.publish(snapshot([{ name: 'alpha', members: [], online: 0 }], {
    ungrouped: [{ conv_id: 'worker', title: 'Worker', online: true }],
  }));
  let allowDiscard = false;
  let discardChecks = 0;
  const actions = {
    sort: () => {}, page: () => {}, setPageSize: () => {},
    openAddMember: state.openAddMember,
    loadAddMemberPromotionPool: async () => [],
    addExistingMember: async () => true,
  };
  const listHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const dialogHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const list = await harness.mount(harness.html`<${GroupsList}
    host=${listHost} state=${state} actions=${actions}
  />`, listHost);
  const dialog = await harness.mount(harness.html`<${GroupsAddMemberDialog}
    state=${state} actions=${actions}
    confirmDiscard=${async () => { discardChecks++; return allowDiscard; }}
  />`, dialogHost);
  const cog = listHost.querySelector('details[data-group-key="alpha"] .cog-btn');
  await harness.act(() => harness.fireEvent(cog, 'click'));
  const add = listHost.querySelector('button[data-act="add-member"]');
  await harness.act(() => harness.fireEvent(add, 'click'));
  await harness.act(() => Promise.resolve());
  const search = dialogHost.querySelector('#add-member-search');
  assert.equal(harness.document.activeElement, search);
  assert.equal(cog.getAttribute('aria-expanded'), 'false');
  await harness.input(search, 'work');
  await harness.act(() => state.publish(snapshot([{ name: 'alpha', members: [], online: 0 }], {
    ungrouped: [{ conv_id: 'worker', title: 'Worker updated', online: true }],
  })));
  assert.equal(dialogHost.querySelector('#add-member-search'), search,
    'snapshot polling cannot remount the live search transaction');
  assert.equal(search.value, 'work', 'snapshot polling cannot overwrite the query');
  await harness.input(search, 'missing candidate');
  assert.match(dialogHost.querySelector('.add-member-empty').textContent,
    /No matching conversations.*Include offline/,
    'an exhausted online-filtered query explains how to widen the pool');

  harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  await harness.act(() => Promise.resolve());
  assert.equal(discardChecks, 1);
  assert.ok(dialogHost.querySelector('#add-member-modal'), 'rejected discard preserves the picker');
  allowDiscard = true;
  harness.fireEvent(harness.document, 'keydown', { key: 'Escape' });
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(state.addMemberDialog.value, null);
  assert.equal(harness.document.activeElement, cog, 'dismissal restores the group-menu cog');

  await dialog.unmount();
  await list.unmount();
  dialogHost.remove();
  listHost.remove();
});

test('add-member picker owns async pool retry, IME navigation and optimistic add retry', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsAddMemberDialog }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/add-member-dialog-island.js'),
  ]);
  const state = createGroupsState({
    prefs: memoryPrefs(), resetOffsets: () => {}, ...stateDependencies(),
  });
  state.publish(snapshot([{ name: 'alpha', members: [], online: 0 }], {
    ungrouped: [
      { conv_id: 'alpha-worker', agent_id: 'agt-alpha', title: 'Alpha', online: true },
      { conv_id: 'bravo-worker', agent_id: 'agt-bravo', title: 'Bravo', online: true },
    ],
    agents: [{ conv_id: 'dormant', agent_id: 'agt-dormant', title: 'Dormant', online: false }],
  }));
  state.openAddMember({ name: 'alpha' });
  let poolCalls = 0;
  const firstPool = deferred();
  let addCalls = 0;
  let addResult = deferred();
  const actions = {
    loadAddMemberPromotionPool: async () => {
      poolCalls++;
      if (poolCalls === 1) return firstPool.promise;
      return [{ conv_id: 'charlie-plain', title: 'Charlie', online: true }];
    },
    addExistingMember: async (descriptor, candidate) => {
      addCalls++;
      await addResult.promise;
      state.optimisticAddMember(descriptor.group, candidate);
    },
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${GroupsAddMemberDialog}
    state=${state} actions=${actions} confirmDiscard=${async () => true}
  />`, host);
  assert.equal(host.querySelector('#add-member-list').getAttribute('aria-busy'), 'true');
  firstPool.reject(new Error('promotion pool unavailable'));
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('[role="alert"]').textContent, /promotion pool unavailable/);
  const failedPoolCalls = poolCalls;
  await harness.act(() => harness.fireEvent(host.querySelector('#add-member-pool-retry'), 'click'));
  await harness.act(() => Promise.resolve());
  assert.equal(poolCalls, failedPoolCalls + 1);
  assert.deepEqual([...host.querySelectorAll('.add-member-row .rowname')].map((node) => node.textContent),
    ['Alpha', 'Bravo', 'Charlie']);

  const search = host.querySelector('#add-member-search');
  assert.equal(host.querySelector('.add-member-row.highlighted .rowname').textContent, 'Alpha');
  await harness.act(() => harness.fireEvent(search, 'keydown', { key: 'ArrowDown' }));
  assert.equal(host.querySelector('.add-member-row.highlighted .rowname').textContent, 'Bravo');
  await harness.act(() => harness.fireEvent(search, 'keydown', {
    key: 'Enter', isComposing: true,
  }));
  assert.equal(addCalls, 0, 'IME composition cannot select a highlighted candidate');
  await harness.act(() => harness.fireEvent(search, 'keydown', { key: 'Enter' }));
  assert.equal(addCalls, 1);
  assert.equal(search.disabled, true, 'an in-flight membership write blocks dialog controls');
  await harness.act(() => harness.fireEvent(search, 'keydown', { key: 'Enter' }));
  assert.equal(addCalls, 1, 'busy state is single-flight');
  addResult.reject(new Error('stale candidate'));
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.match(host.querySelector('#add-member-error').textContent, /stale candidate/);

  addResult = deferred();
  await harness.act(() => harness.fireEvent(host.querySelector('#add-member-retry'), 'click'));
  assert.equal(addCalls, 2);
  addResult.resolve();
  await harness.act(() => Promise.resolve());
  await harness.act(() => Promise.resolve());
  assert.equal(state.snapshot.value.groups[0].members[0].conv_id, 'bravo-worker');
  assert.ok(host.querySelector('#add-member-modal'), 'successful add stays open for add-another');
  assert.equal(host.textContent.includes('Bravo'), false, 'optimistic membership removes the added candidate');

  const includeOffline = host.querySelector('#add-member-all');
  includeOffline.checked = true;
  await harness.act(() => harness.fireEvent(includeOffline, 'change'));
  assert.ok(host.textContent.includes('Dormant'));

  await mounted.unmount();
  host.remove();
});
