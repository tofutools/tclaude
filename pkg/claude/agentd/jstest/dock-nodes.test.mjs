import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const NODES = [
  { id: 'inst_brain1', name: 'brain', color: '#3fb950' },
  { id: 'inst_herc31', name: 'herc3', color: '#58a6ff' },
  { id: 'inst_mac111', name: 'mac', color: '#d29922' },
];

test('mergeCatalog makes one entry per name with each node present, different or missing', async (t) => {
  const harness = await createPreactHarness(t);
  const { mergeCatalog, canonical } = await harness.importDashboardModule('js/dock-nodes.js');
  const merged = mergeCatalog(NODES, {
    inst_brain1: [{ id: 1, name: 'opus', model: 'opus' }, { id: 2, name: 'sol', model: 'gpt' }],
    inst_herc31: [{ id: 9, name: 'opus', model: 'opus', updated_at: 'x' }, { id: 3, name: 'luna', model: 'gpt' }],
    inst_mac111: [{ id: 4, name: 'opus', model: 'haiku' }],
  }, (item) => item.name, 'inst_brain1');
  assert.deepEqual(merged.map((entry) => entry.name), ['luna', 'opus', 'sol']);
  const opus = merged.find((entry) => entry.name === 'opus');
  assert.equal(opus.ref, 'inst_brain1', 'the page node holds the copy the card shows');
  assert.deepEqual(opus.state, { inst_brain1: 'present', inst_herc31: 'present', inst_mac111: 'differs' },
    'store ids and timestamps do not make copies differ');
  const luna = merged.find((entry) => entry.name === 'luna');
  assert.equal(luna.ref, 'inst_herc31', 'without a page copy the first holder stands for it');
  assert.deepEqual(luna.state, { inst_brain1: 'missing', inst_herc31: 'present', inst_mac111: 'missing' });
  // A node that keeps the section has no pills at all.
  const partial = mergeCatalog(NODES, { inst_brain1: [{ name: 'a' }] }, (item) => item.name, 'inst_brain1');
  assert.deepEqual(partial[0].state, { inst_brain1: 'present' });
  assert.equal(canonical({ b: 1, a: [2, { c: 3 }] }), canonical({ a: [2, { c: 3 }], b: 1 }));
});

test('catalogWithheld names the grant a restricted peer needs for its catalogs', async (t) => {
  const harness = await createPreactHarness(t);
  const { catalogWithheld } = await harness.importDashboardModule('js/dock-nodes.js');
  const mac = NODES[2];
  assert.equal(catalogWithheld(mac, null), 'loading');
  assert.equal(catalogWithheld(mac, { profiles: [] }), '', 'this node shares its own');
  assert.equal(catalogWithheld(mac, { peer_view: { level: 'unrestricted' } }), '');
  assert.equal(catalogWithheld(mac, { peer_view: { level: 'restricted', included: ['groups', 'spawn.catalog'] } }), '');
  const why = catalogWithheld(mac, { peer_view: { level: 'restricted', included: ['groups'], omitted: [{ feature: 'spawn.catalog', requires: 'node.spawn.read' }] } });
  assert.match(why, /mac keeps its catalogs.*node\.spawn\.read/);
  assert.match(catalogWithheld(mac, null, { status: 0, code: 'network' }), /mac is not answering/);
  assert.match(catalogWithheld(mac, null, { status: 403, code: 'not_trusted' }), /no longer trusts/);
});

test('publishFusedDock shows again a node that left the fused set', async (t) => {
  const harness = await createPreactHarness(t);
  const { publishFusedDock, hiddenNodes, toggleHiddenNode } = await harness.importDashboardModule('js/dock-nodes.js');
  toggleHiddenNode('inst_mac111');
  toggleHiddenNode('inst_herc31');
  publishFusedDock({ nodes: NODES.slice(0, 2), snapshots: {}, page: 'inst_brain1' });
  assert.deepEqual(hiddenNodes.value, ['inst_herc31']);
  publishFusedDock(null);
  assert.deepEqual(hiddenNodes.value, ['inst_herc31'], 'leaving the fused view keeps the filter');
});

function section(calls) {
  return {
    key: 'profiles',
    icon: () => '⚙',
    title: () => 'Agent profiles',
    empty: () => 'no profiles yet',
    items: (snapshot) => snapshot?.profiles || [],
    name: (item) => item.name,
    chips: (item) => [{ text: item.model, more: false }],
    drag: true,
    itemPath: (name) => `/api/spawn-profiles/${name}`,
    listPath: '/api/spawn-profiles',
    onManageItem: (item) => calls.push(['edit', item.name]),
    onCloneItem: (item) => calls.push(['clone', item.name]),
    onDeleteItem: (item) => calls.push(['delete', item.name]),
    onManageAll: () => calls.push(['manage']),
  };
}

test('The fused dock lists every shown node, with where pills, a node filter and withheld nodes locked', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDockState }, { Dock }] = await Promise.all([
    harness.importDashboardModule('js/dock-state.js'),
    harness.importDashboardModule('js/dock-island.js'),
  ]);
  const { signal } = harness.signals;
  const state = createDockState();
  state.publish({ profiles: [{ name: 'opus', model: 'opus' }, { name: 'sol', model: 'gpt' }] });
  const fused = signal({
    page: 'inst_brain1',
    nodes: NODES,
    snapshots: {
      inst_herc31: { peer_view: { level: 'unrestricted' }, profiles: [{ name: 'opus', model: 'opus' }, { name: 'luna', model: 'gpt' }] },
      inst_mac111: { peer_view: { level: 'restricted', included: ['groups'], omitted: [{ feature: 'spawn.catalog', requires: 'node.spawn.read' }] }, profiles: [] },
    },
    fetchFor: (id) => (path, init) => { calls.push(['fetch', id, path, init?.method]); return Promise.resolve({ ok: true, json: async () => ({}) }); },
  });
  const hidden = signal([]);
  const calls = [];
  const ops = {
    open: (node) => calls.push(['open', node.id]),
    rename: (sec, name, node) => calls.push(['rename', name, node.id]),
    remove: (sec, name, node) => calls.push(['remove', name, node.id]),
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.getBoundingClientRect = () => ({ top: 0, bottom: 800 });
  const mounted = await harness.mount(harness.html`
    <${Dock} host=${host} state=${state} sections=${[section(calls)]} isSectionOpen=${() => true} setSectionOpen=${() => {}}
      fusedState=${fused} hiddenState=${hidden} nodeOps=${ops} />
  `, host);

  const names = () => [...host.querySelectorAll('.dock-card')].map((card) => card.dataset.dockName);
  assert.deepEqual(names(), ['luna', 'opus', 'sol'], 'one card per name across the nodes');
  const head = host.querySelector('.dock-section-nodes');
  assert.deepEqual([...head.querySelectorAll('.dock-where-pill')].map((pill) => pill.textContent.replace('🔒', '').trim()), ['brain', 'herc3', 'mac'],
    'the heading names the nodes it covers');
  const lock = head.querySelector('.is-withheld .lock-why');
  assert.match(lock.dataset.why, /node\.spawn\.read/, 'a node keeping its catalogs says why');

  const opus = host.querySelector('[data-dock-name="opus"]');
  assert.deepEqual([...opus.querySelectorAll('.dock-where-pill')].map((pill) => `${pill.textContent}:${pill.className.match(/is-(present|differs|missing)/)[1]}`),
    ['brain:present', 'herc3:present'], 'a withheld node gets no pill');
  const luna = host.querySelector('[data-dock-name="luna"]');
  assert.equal(luna.querySelector('.is-missing').textContent, '+ brain');
  assert.match(luna.querySelector('.dock-grip').title, /coming soon/, 'only the page node copy drags onto a group');
  assert.equal(opus.querySelector('.dock-grip').title, 'drag onto a group to spawn');

  // A page copy runs the page node's own hooks.
  const opusCog = getByRole(opus, 'button', { name: 'Actions for opus' });
  await harness.act(() => harness.fireEvent(opusCog, 'click'));
  await harness.act(() => harness.fireEvent(getByRole(opus, 'menuitem', { name: 'Edit' }), 'click'));
  assert.deepEqual(calls.at(-1), ['edit', 'opus']);

  // Picking herc3's copy acts on herc3.
  await harness.act(() => harness.fireEvent(opus.querySelector('[data-dock-node="inst_herc31"]'), 'click'));
  assert.equal(opus.querySelector('[data-dock-node="inst_herc31"]').getAttribute('aria-pressed'), 'true');
  await harness.act(() => harness.fireEvent(opusCog, 'click'));
  await harness.act(() => harness.fireEvent(getByRole(opus, 'menuitem', { name: 'Open on herc3' }), 'click'));
  assert.deepEqual(calls.at(-1), ['open', 'inst_herc31']);
  await harness.act(() => harness.fireEvent(opusCog, 'click'));
  await harness.act(() => harness.fireEvent(getByRole(opus, 'menuitem', { name: 'Delete' }), 'click'));
  assert.deepEqual(calls.at(-1), ['remove', 'opus', 'inst_herc31']);

  // A restricted node's copy stays locked with the reason.
  fused.value = { ...fused.value, snapshots: { ...fused.value.snapshots, inst_mac111: { peer_view: { level: 'restricted', included: ['groups', 'spawn.catalog'] }, profiles: [{ name: 'sol', model: 'gpt' }] } } };
  await harness.act(() => {});
  const sol = host.querySelector('[data-dock-name="sol"]');
  await harness.act(() => harness.fireEvent(sol.querySelector('[data-dock-node="inst_mac111"]'), 'click'));
  await harness.act(() => harness.fireEvent(getByRole(sol, 'button', { name: 'Actions for sol' }), 'click'));
  const locked = sol.querySelector('.dock-card-menu-item.is-locked[data-dock-act="delete-item"]');
  assert.match(locked.querySelector('.lock-why').dataset.why, /mac keeps .* unrestricted trust/);
  await harness.act(() => harness.fireEvent(locked, 'click'));
  assert.notDeepEqual(calls.at(-1), ['remove', 'sol', 'inst_mac111']);
  await harness.act(() => harness.fireEvent(harness.document.body, 'click'));

  // The filter row narrows the nodes.
  const filter = getByRole(host, 'group', { name: 'Nodes listed in the panel' });
  await harness.act(() => harness.fireEvent(filter.querySelector('[data-dock-node="inst_herc31"]'), 'click'));
  assert.deepEqual(hidden.value, ['inst_herc31']);
  assert.deepEqual(names(), ['opus', 'sol'], 'a hidden node lists nothing');
  assert.equal(filter.querySelector('[data-dock-node="inst_herc31"]').getAttribute('aria-pressed'), 'false');

  // Off the fused view the dock is the page node's again.
  await harness.act(() => { fused.value = null; });
  assert.equal(host.querySelector('.dock-node-filter'), null);
  assert.equal(host.querySelector('.dock-where'), null);
  assert.deepEqual(names(), ['opus', 'sol']);
  await mounted.unmount();
  host.remove();
});

test('copyRoute copies directly to a trusting node, offers this node own item to another, else says why', async (t) => {
  const harness = await createPreactHarness(t);
  const { copyRoute } = await harness.importDashboardModule('js/dock-nodes.js');
  const entry = { byNode: { inst_brain1: { name: 'opus' } } };
  const herc = NODES[1];
  const mac = NODES[2];
  assert.equal(copyRoute(entry, herc, { peer_view: { level: 'unrestricted' } }, 'inst_brain1', 'roles').mode, 'direct');
  assert.equal(copyRoute(entry, mac, { peer_view: { level: 'restricted' } }, 'inst_brain1', 'roles').mode, 'offer');
  const none = copyRoute({ byNode: { inst_herc31: {} } }, mac, { peer_view: { level: 'restricted' } }, '', 'roles');
  assert.equal(none.mode, '');
  assert.match(none.why, /only this node's own roles can be offered/);
});

test('A dashed pill and the card menu copy an item to another node', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDockState }, { Dock }] = await Promise.all([
    harness.importDashboardModule('js/dock-state.js'),
    harness.importDashboardModule('js/dock-island.js'),
  ]);
  const { signal } = harness.signals;
  const state = createDockState();
  state.publish({ profiles: [{ name: 'opus', model: 'opus' }] });
  const calls = [];
  const fused = signal({
    page: 'inst_brain1',
    self: 'inst_brain1',
    nodes: NODES,
    snapshots: {
      inst_herc31: { peer_view: { level: 'unrestricted' }, profiles: [{ name: 'luna', model: 'gpt' }] },
      inst_mac111: { peer_view: { level: 'restricted', included: ['groups', 'spawn.catalog'] }, profiles: [{ name: 'luna', model: 'gpt' }] },
    },
    fetchFor: (id) => id,
  });
  const ops = {
    open: () => {}, rename: () => {}, remove: () => {},
    copy: (sec, item, { to, direct, self }) => calls.push([item.name, to.id, to.fetch, direct, self.id]),
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.getBoundingClientRect = () => ({ top: 0, bottom: 800 });
  const mounted = await harness.mount(harness.html`
    <${Dock} host=${host} state=${state} sections=${[section([])]} isSectionOpen=${() => true} setSectionOpen=${() => {}}
      fusedState=${fused} hiddenState=${signal([])} nodeOps=${ops} />
  `, host);
  const opus = host.querySelector('[data-dock-name="opus"]');
  await harness.act(() => harness.fireEvent(opus.querySelector('[data-dock-node="inst_herc31"]'), 'click'));
  assert.deepEqual(calls.at(-1), ['opus', 'inst_herc31', 'inst_herc31', true, 'inst_brain1'], 'an unrestricted node gets it directly');
  await harness.act(() => harness.fireEvent(opus.querySelector('[data-dock-node="inst_mac111"]'), 'click'));
  assert.deepEqual(calls.at(-1), ['opus', 'inst_mac111', 'inst_mac111', false, 'inst_brain1'], 'a restricted node gets an offer');
  await harness.act(() => harness.fireEvent(getByRole(opus, 'button', { name: 'Actions for opus' }), 'click'));
  assert.ok(getByRole(opus, 'menuitem', { name: 'Copy to herc3…' }));
  await harness.act(() => harness.fireEvent(getByRole(opus, 'menuitem', { name: 'Offer to mac…' }), 'click'));
  assert.equal(calls.length, 3);

  // herc3's own item cannot be offered from here to a restricted node.
  const luna = host.querySelector('[data-dock-name="luna"]');
  assert.deepEqual([...luna.querySelectorAll('.dock-where-pill')].map((pill) => pill.textContent.replace('🔒', '').trim()), ['+ brain', 'herc3', 'mac']);
  await harness.act(() => harness.fireEvent(luna.querySelector('[data-dock-node="inst_brain1"]'), 'click'));
  assert.deepEqual(calls.at(-1), ['luna', 'inst_brain1', 'inst_brain1', true, 'inst_brain1'], 'this node takes it directly');

  // A node the filter hides is not offered a copy it may already hold.
  await mounted.rerender(harness.html`
    <${Dock} host=${host} state=${state} sections=${[section([])]} isSectionOpen=${() => true} setSectionOpen=${() => {}}
      fusedState=${fused} hiddenState=${signal(['inst_herc31'])} nodeOps=${ops} />
  `);
  const shown = host.querySelector('[data-dock-name="opus"]');
  await harness.act(() => harness.fireEvent(getByRole(shown, 'button', { name: 'Actions for opus' }), 'click'));
  assert.equal([...shown.querySelectorAll('[role="menuitem"]')].some((el) => /herc3/.test(el.textContent)), false);
  assert.ok(getByRole(shown, 'menuitem', { name: 'Offer to mac…' }));
  await mounted.unmount();
  host.remove();
});
