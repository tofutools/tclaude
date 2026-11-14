// js/phone-access-island.js: the Access tab on a phone, over the real access
// state and recorded actions, driven through what the operator taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function prefs() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

const NOW = Date.parse('2026-11-14T12:00:00Z');

function snapshotWith({ requests = [], slugs = 3 } = {}) {
  return {
    generated_at: new Date(NOW).toISOString(),
    agents: [{ agent_id: 'agt_lead', conv_id: 'c-lead', title: 'lead' }],
    permissions: { defaults: ['self.rename', 'self.task'], overrides: { 'c-lead': { 'agent.spawn': 'grant', 'git.push': 'deny' } } },
    slugs: Array.from({ length: slugs }, (_, i) => ({ slug: `slug.${String(i).padStart(3, '0')}`, description: `does ${i}` })),
    sudo: [{
      id: 7, agent_id: 'agt_lead', conv_id: 'c-lead', conv_title: 'lead', slug: 'human.notify', granted_by: 'operator',
      granted_at: new Date(NOW - 600000).toISOString(), expires_at: new Date(NOW + 3000000).toISOString(), reason: 'UI review',
    }],
    access_requests: requests,
  };
}

async function setup(t, options = {}) {
  const harness = await createPreactHarness(t);
  const [island, { createAccessState }] = await Promise.all([
    harness.importDashboardModule('js/phone-access-island.js'),
    harness.importDashboardModule('js/access-state.js'),
  ]);
  const { signal } = harness.signals;
  const snapshot = signal(options.loaded === false ? null : snapshotWith(options));
  const state = createAccessState({ snapshot, prefs: prefs(), now: () => NOW });
  state.initialize();
  const calls = [];
  const actions = {
    revoke: async (grant) => { calls.push(['revoke', grant.id]); return true; },
    openGrant: () => { calls.push(['openGrant']); },
  };
  const posts = [];
  const fetchImpl = async (url, init) => { posts.push([String(url), JSON.parse(init.body)]); return { ok: true, status: 200 }; };
  const navigations = [];
  harness.document.addEventListener('tclaude:navigated', (event) => navigations.push(event.detail.location));
  const mounted = await harness.mount(harness.html`<${island.PhoneAccessApp} state=${state} actions=${actions} snapshot=${snapshot}
    fetchImpl=${fetchImpl} openInMail=${(id) => calls.push(['openInMail', id])} />`);
  const root = mounted.container;
  const tab = (name) => [...root.querySelectorAll('[role="tab"]')].find((element) => element.textContent.startsWith(name));
  const tap = (element) => harness.act(() => element.click());
  const settle = async () => {
    for (let i = 0; i < 4; i++) await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  };
  return { harness, island, state, snapshot, calls, posts, navigations, root, tab, tap, settle };
}

test('a waiting request leads, answered with the same card as Now', async (t) => {
  const requests = [
    { id: 'r1', perm: 'git.push', agent_id: 'agt_lead', conv_title: 'lead', status: 'pending' },
    { id: 'r2', perm: 'y', agent_id: 'agt_lead', status: 'pending', origin_peer: 'node-2' },
  ];
  const { harness, root, tab, posts, calls, tap, settle } = await setup(t, { requests });
  assert.equal(tab('Requests').getAttribute('aria-selected'), 'true');
  assert.equal(tab('Requests').querySelector('.ui-badge').textContent, '2');
  const cards = root.querySelectorAll('.companion-approval');
  assert.equal(cards.length, 1, 'a peer\'s request is not approved blind');
  await harness.act(() => harness.getByRole(cards[0], 'button', { name: /Approve/ }).click());
  await settle();
  assert.deepEqual(posts.at(-1)[1], { decision: 'approve' });
  await tap([...root.querySelectorAll('.ui-btn')].find((button) => /1 more access request in Mail/.test(button.textContent)));
  assert.deepEqual(calls.at(-1), ['openInMail', undefined]);
});

test('with nothing waiting it opens on the routed subtab; Sudo revokes and grants', async (t) => {
  const { root, tab, calls, navigations, tap } = await setup(t);
  assert.equal(tab('Agents').getAttribute('aria-selected'), 'true', 'the access state\'s default subtab');
  await tap(tab('Sudo'));
  assert.deepEqual(navigations.at(-1), { tab: 'access', subtab: 'sudo' });
  const row = root.querySelector('.phone-access-row[data-key="sudo-7"]');
  assert.match(row.textContent, /human\.notify/);
  assert.match(row.querySelector('.phone-access-left').textContent, /^50m left · UI review$/);
  await tap(row.querySelector('[aria-label="Revoke human.notify from lead"]'));
  assert.deepEqual(calls.at(-1), ['revoke', 7]);
  await tap([...root.querySelectorAll('.ui-btn')].find((button) => button.textContent.includes('+ Grant sudo')));
  assert.deepEqual(calls.at(-1), ['openGrant']);
  await tap(tab('Requests'));
  assert.match(root.textContent, /No access request is waiting/);
  assert.deepEqual(navigations.at(-1), { tab: 'access' }, 'Requests is the bare /access');
});

test('a route that names a subtab wins over the Requests default', async (t) => {
  const requests = [{ id: 'r1', perm: 'git.push', agent_id: 'agt_lead', status: 'pending' }];
  const { harness, state, tab } = await setup(t, { requests });
  assert.equal(tab('Requests').getAttribute('aria-selected'), 'true');
  // nav-history.js restores /access/slugs by clicking the subtab.
  await harness.act(() => tab('Slugs').click());
  assert.equal(state.subtab.value, 'slugs');
  assert.equal(tab('Slugs').getAttribute('aria-selected'), 'true');
  assert.ok(tab('Slugs').matches('.access-subtab[data-subtab="slugs"]'));
  await harness.act(() => state.setSubtab('sudo'));
  assert.equal(tab('Sudo').getAttribute('aria-selected'), 'true');
});

test('Agents lists the overrides read-only and points editing at the desktop', async (t) => {
  const { root, tab, tap } = await setup(t);
  await tap(tab('Agents'));
  assert.match(root.querySelector('.phone-access-defaults summary').textContent, /2 default slugs/);
  const row = root.querySelector('.phone-access-row[data-key="c-lead"]');
  assert.deepEqual([...row.querySelectorAll('.phone-access-slug')].map((tag) => tag.textContent), ['+ agent.spawn', '− git.push']);
  assert.equal(row.querySelector('button'), null);
  assert.match(root.querySelector('.phone-desktop-only').textContent, /desktop only/);
});

test('Slugs shows the first page until searched or asked for all', async (t) => {
  const { harness, island, root, tab, tap } = await setup(t, { slugs: 55 });
  await tap(tab('Slugs'));
  assert.equal(root.querySelectorAll('.phone-access-row').length, island.SLUGS_SHOWN);
  const search = root.querySelector('.phone-access-search');
  await harness.act(() => { search.value = 'slug.05'; harness.fireEvent(search, 'input'); });
  assert.deepEqual([...root.querySelectorAll('.phone-access-row')].map((row) => row.dataset.key), [
    'slug.050', 'slug.051', 'slug.052', 'slug.053', 'slug.054',
  ]);
  await harness.act(() => { search.value = ''; harness.fireEvent(search, 'input'); });
  await tap([...root.querySelectorAll('.ui-btn')].find((button) => /Show all 55 slugs/.test(button.textContent)));
  assert.equal(root.querySelectorAll('.phone-access-row').length, 55);
});

test('before the first snapshot it says it is loading, then picks Requests if any wait', async (t) => {
  const { harness, root, snapshot, tab } = await setup(t, { loaded: false });
  assert.match(root.textContent, /Loading/);
  await harness.act(() => { snapshot.value = snapshotWith({ requests: [{ id: 'r1', perm: 'x', agent_id: 'agt_lead', status: 'pending' }] }); });
  assert.equal(tab('Requests').getAttribute('aria-selected'), 'true');
});

test('a subtab picked before the first snapshot is not overridden by a waiting request', async (t) => {
  const { harness, snapshot, tab } = await setup(t, { loaded: false });
  // nav-history restores /access/sudo by clicking Sudo, which can land
  // before the effect that would pick Requests has run.
  await harness.act(() => {
    tab('Sudo').click();
    snapshot.value = snapshotWith({ requests: [{ id: 'r1', perm: 'x', agent_id: 'agt_lead', status: 'pending' }] });
  });
  assert.equal(tab('Sudo').getAttribute('aria-selected'), 'true');
});
