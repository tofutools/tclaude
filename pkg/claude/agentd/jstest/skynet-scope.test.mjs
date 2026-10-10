import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const peer = (id, label, online = true) => ({ instance_id: id, label, trusted: true, online, level: 'restricted' });
const status = (peers) => ({ enabled: true, instance_id: 'inst_self', name: 'desk', hub: { state: 'connected' }, peers });
const three = status([peer('inst_forge', 'forge'), peer('inst_lab', 'lab', false)]);
const eight = status(['forge', 'lab', 'ci', 'render', 'review', 'docs', 'qa'].map((n, i) => peer(`inst_${n}0000`, n, i !== 3)));

async function load(t) {
  const harness = await createPreactHarness(t);
  const [scope, model, stateMod, island] = await Promise.all([
    harness.importDashboardModule('js/skynet-scope.js'), harness.importDashboardModule('js/skynet-model.js'),
    harness.importDashboardModule('js/skynet-state.js'), harness.importDashboardModule('js/skynet-island.js'),
  ]);
  return { harness, scope, model, stateMod, island };
}

test('the fused set lives in ?nodes=: all, or the ticked IDs; one left is that node alone', async (t) => {
  const { scope, model } = await load(t);
  assert.equal(scope.parseFused(''), null);
  assert.equal(scope.parseFused('?nodes=all'), 'all');
  assert.deepEqual(scope.parseFused('?node=inst_forge&nodes=inst_self,inst_forge,bogus,inst_self'), ['inst_self', 'inst_forge']);
  assert.equal(scope.withFused('?wizard=1&nodes=all', null), '?wizard=1');
  assert.equal(scope.withFused('?node=inst_forge', ['inst_self', 'inst_forge']), '?node=inst_forge&nodes=inst_self%2Cinst_forge');
  const fleet = model.normalizeFleet(three);
  assert.deepEqual(scope.tickedNodes(fleet, ['inst_lab', 'inst_gone', 'inst_self']).map((n) => n.name), ['desk', 'lab'], 'fleet order; unknown IDs ignored');
  assert.deepEqual(scope.toggleNode(fleet, 'all', 'inst_lab'), ['inst_self', 'inst_forge']);
  assert.equal(scope.toggleNode(fleet, ['inst_self', 'inst_forge'], 'inst_lab'), 'all');
  assert.deepEqual(scope.toggleNode(fleet, ['inst_self', 'inst_forge'], 'inst_self'), { only: 'inst_forge' });
  assert.equal(scope.selectorKind(fleet), 'buttons');
  const wide = model.normalizeFleet(eight);
  assert.equal(scope.selectorKind(wide), 'dropdown');
  assert.deepEqual(scope.filterNodes(wide, 'RE').map((n) => n.name), ['desk', 'render', 'review'], 'this node stays pinned whatever the filter');
});

test('up to four nodes get a button each; the fused dropdown ticks nodes and one left shows that node alone', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('usage');
  const state = stateMod.createSkynetState({ activeTab, search: '' });
  state.setStatus(three);
  const nav = []; const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} remote="" switchNode=${(id) => switched.push(id)} />`);
  const c = mounted.container;
  assert.ok(getByRole(c, 'button', { name: 'desk (this node)' }));
  assert.ok(getByRole(c, 'button', { name: 'lab, offline, restricted peer' }));
  const fuse = getByRole(c, 'button', { name: 'Show several nodes together' });
  await harness.act(() => harness.fireEvent(fuse, 'click'));
  assert.equal(state.fused.value, 'all', 'fusing defaults to every node');
  assert.equal(getByRole(c, 'button', { name: 'desk (this node)' }).getAttribute('aria-current'), null, 'no single node is current while fused');
  assert.ok(getByRole(c, 'button', { name: 'desk (this node)' }).classList.contains('ticked'));
  const box = (id) => c.querySelector(`input[data-fuse-node="${id}"]`);
  assert.equal(box('inst_lab').hasAttribute('checked'), true);
  await harness.act(() => harness.fireEvent(box('inst_lab'), 'change'));
  assert.deepEqual(state.fused.value, ['inst_self', 'inst_forge']);
  assert.match(getByRole(c, 'button', { name: /Fused view of 2 of 3 nodes/ }).textContent, /2\/3/);
  assert.match(c.querySelector('.node-fuse-pop').textContent, /Other tabs show one node, the primary: ⌂ desk/);
  await harness.act(() => harness.fireEvent(box('inst_self'), 'change'));
  assert.equal(state.fused.value, null, 'one ticked node is that node alone');
  assert.deepEqual(switched, ['inst_forge']);
  await mounted.unmount(); state.dispose();
});

test('fusing from the map lands in Groups; picking the shown node leaves the fused view in place', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('map');
  const state = stateMod.createSkynetState({ activeTab, search: '' });
  state.setStatus(three);
  const nav = []; const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => { nav.push(tab); activeTab.value = tab; }} remote="" switchNode=${(id) => switched.push(id)} />`);
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'Show several nodes together' }), 'click'));
  assert.deepEqual(nav, ['groups']);
  await harness.act(() => { activeTab.value = 'costs'; });
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'desk (this node)' }), 'click'));
  assert.equal(state.fused.value, null);
  assert.deepEqual(nav, ['groups'], 'already on this node: no navigation, the tab stays');
  assert.deepEqual(switched, []);
  await mounted.unmount(); state.dispose();
});

test('past four nodes one dropdown names the shown node, filters as you type and keeps this node pinned', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const state = stateMod.createSkynetState({ activeTab: harness.signals.signal('groups'), search: '' });
  state.setStatus(eight);
  const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${() => {}} remote="inst_review0000" switchNode=${(id) => switched.push(id)} />`);
  const c = mounted.container;
  assert.equal(c.querySelectorAll('button.node-chip:not(.node-pick-btn):not(.node-fuse)').length, 0, 'no per-node buttons');
  const pick = getByRole(c, 'button', { name: 'Node: review. Choose a node' });
  await harness.act(() => harness.fireEvent(pick, 'click'));
  const input = c.querySelector('.node-pop-filter input');
  await harness.act(() => { input.value = 're'; harness.fireEvent(input, 'input'); });
  const rows = [...c.querySelectorAll('.node-pop-row')].map((r) => r.querySelector('.node-pop-name').textContent);
  assert.deepEqual(rows, ['⌂ desk', 'render', 'review']);
  assert.match(c.querySelector('.node-pop-row.offline').textContent, /render.*offline/);
  assert.match(c.querySelector('.node-pop-row.sel').textContent, /review.*shown/);
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'ArrowDown' }));
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(switched, ['inst_render0000']);
  await mounted.unmount(); state.dispose();
});

test('a per-node tab in a fused view names its node, the primary, and can switch it', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('usage');
  const state = stateMod.createSkynetState({ activeTab, search: '?nodes=inst_self,inst_forge' });
  state.setStatus(three);
  const switched = [];
  const doc = harness.document;
  const mounted = await harness.mount(harness.html`<${island.ScopeBanner} state=${state} remote="" switchNode=${(id) => switched.push(id)} doc=${doc} />`);
  const c = mounted.container;
  assert.match(c.textContent, /shows one node\. Showing ⌂ desk, the primary of your 2 fused nodes/);
  const seg = [...c.querySelectorAll('.scope-banner-seg button')];
  assert.deepEqual(seg.map((b) => b.textContent), ['⌂ desk', 'forge']);
  await harness.act(() => harness.fireEvent(seg[1], 'click'));
  assert.deepEqual(switched, ['inst_forge']);
  await harness.act(() => { activeTab.value = 'terminals'; });
  assert.equal(c.textContent, '', 'fused tabs need no banner');
  await harness.act(() => { activeTab.value = 'usage'; state.setFused(null); });
  assert.equal(c.textContent, '');
  await mounted.unmount(); state.dispose();
});

test('the fused URL is written in place, keeping the page\'s other parameters', async (t) => {
  const { island } = await load(t);
  const writes = [];
  const win = { location: { pathname: '/usage', search: '?wizard=1', hash: '' }, history: { state: { k: 1 }, replaceState: (st, _, url) => writes.push([st, url]) } };
  island.writeScopeURL(['inst_self', 'inst_forge'], win);
  assert.deepEqual(writes, [[{ k: 1 }, '/usage?wizard=1&nodes=inst_self%2Cinst_forge']]);
  island.writeScopeURL(null, win);
  assert.equal(writes.length, 1, 'nothing to change');
});
