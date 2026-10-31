import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const peer = (id, label, online = true) => ({ instance_id: id, label, trusted: true, online, level: 'restricted' });
const status = (peers) => ({ enabled: true, instance_id: 'inst_self', name: 'desk', hub: { state: 'connected' }, peers });
const three = status([peer('inst_forge', 'forge'), peer('inst_lab', 'lab', false)]);
const eight = status(['forge', 'lab', 'ci', 'render', 'review', 'docs', 'qa'].map((n, i) => peer(`inst_${n}0000`, n, i !== 3)));

async function load(t) {
  const harness = await createPreactHarness(t);
  const [scope, model, stateMod, island] = await Promise.all([
    harness.importDashboardModule('js/fleet-scope.js'), harness.importDashboardModule('js/fleet-model.js'),
    harness.importDashboardModule('js/fleet-state.js'), harness.importDashboardModule('js/fleet-island.js'),
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
  const state = stateMod.createFleetState({ activeTab, search: '' });
  state.setStatus(three);
  const nav = []; const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} remote="" switchNode=${(id) => switched.push(id)} />`);
  const c = mounted.container;
  assert.ok(getByRole(c, 'button', { name: 'desk (this node)' }));
  const lab = getByRole(c, 'button', { name: 'lab, offline, restricted peer' });
  assert.equal(lab.disabled, true, 'an offline peer cannot be opened');
  assert.match(lab.title, /lab is offline: its dashboard opens again once it reconnects/);
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
  assert.match(c.querySelector('.node-fuse-pop').textContent, /Other tabs show this node: ⌂ desk/);
  await harness.act(() => harness.fireEvent(box('inst_self'), 'change'));
  assert.equal(state.fused.value, null, 'one ticked node is that node alone');
  assert.deepEqual(switched, ['inst_forge']);
  await harness.act(() => { state.setFused(['inst_self', 'inst_lab']); });
  await harness.act(() => harness.fireEvent(getByRole(c, 'button', { name: /Fused view of 2 of 3 nodes/ }), 'click'));
  await harness.act(() => harness.fireEvent(box('inst_self'), 'change'));
  assert.deepEqual(state.fused.value, ['inst_self', 'inst_lab'], 'unticking down to an offline peer keeps the set');
  assert.deepEqual(switched, ['inst_forge']);
  await mounted.unmount(); state.dispose();
});

test('fusing from the map lands in Groups; picking the shown node leaves the fused view in place', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('map');
  const state = stateMod.createFleetState({ activeTab, search: '' });
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
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '' });
  state.setStatus(eight);
  const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${() => {}} remote="inst_review0000" switchNode=${(id) => switched.push(id)} />`);
  const c = mounted.container;
  assert.equal(c.querySelectorAll('button.node-chip:not(.node-pick-btn):not(.node-fuse):not(.fleet-entry)').length, 0, 'no per-node buttons');
  const pick = getByRole(c, 'button', { name: 'Node: review. Choose a node' });
  await harness.act(() => harness.fireEvent(pick, 'click'));
  const input = c.querySelector('.node-pop-filter input');
  await harness.act(() => { input.value = 're'; harness.fireEvent(input, 'input'); });
  const rows = [...c.querySelectorAll('.node-pop-row')].map((r) => r.querySelector('.node-pop-name').textContent);
  assert.deepEqual(rows, ['⌂ desk', 'render', 'review']);
  const render = c.querySelector('.node-pop-row.offline');
  assert.match(render.textContent, /render.*offline/);
  assert.equal(render.disabled, true, 'an offline peer cannot be picked');
  assert.match(render.title, /render is offline: its dashboard opens again/);
  assert.match(c.querySelector('.node-pop-row.sel').textContent, /review.*shown/);
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'ArrowDown' }));
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(switched, [], 'Enter on an offline row does nothing');
  await mounted.unmount(); state.dispose();
});

test('a per-node tab in a fused view names this node, with no primary to switch', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('usage');
  const state = stateMod.createFleetState({ activeTab, search: '?nodes=inst_self,inst_forge' });
  state.setStatus(three);
  const doc = harness.document;
  const mounted = await harness.mount(harness.html`<${island.ScopeBanner} state=${state} doc=${doc} />`);
  const c = mounted.container;
  assert.match(c.textContent, /shows this node, ⌂ desk\. Open another node's own view from its chip/);
  assert.equal(c.querySelector('button'), null, 'no primary switcher');
  await harness.act(() => { activeTab.value = 'terminals'; });
  assert.equal(c.textContent, '', 'fused tabs need no banner');
  await harness.act(() => { activeTab.value = 'usage'; state.setFused(null); });
  assert.equal(c.textContent, '');
  await mounted.unmount(); state.dispose();
});

test('machine tabs in a fused view stay on this node and offer each peer\'s own where it opens', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('access');
  const state = stateMod.createFleetState({ activeTab, search: '?nodes=all' });
  // forge grants this node unrestricted (we grant it restricted: peer.level
  // is our side); ci grants us restricted.
  state.setStatus(status([peer('inst_forge', 'forge'), peer('inst_lab', 'lab', false), { ...peer('inst_ci00', 'ci'), level: 'unrestricted' }]));
  const theirs = { inst_forge: 'unrestricted', inst_ci00: 'restricted' };
  const mounted = await harness.mount(harness.html`<${island.ScopeBanner} state=${state} doc=${harness.document} loc=${{ search: '?nodes=all&wizard=1' }}
    peerSnapshot=${(id) => (theirs[id] ? { peer_view: { level: theirs[id] } } : null)} />`);
  const c = mounted.container;
  const link = c.querySelector('.scope-peer-link');
  assert.equal(link.textContent.startsWith('forge'), true);
  assert.equal(link.getAttribute('href'), '/access?wizard=1&node=inst_forge', "an unrestricted peer's Access opens on its own view, leaving the fused set");
  const locks = [...c.querySelectorAll('.scope-peer-locked .lock-why')].map((l) => l.dataset.why);
  assert.deepEqual(locks.sort(), ['ci does not share its access with you (needs unrestricted trust from ci)', 'lab is offline']);
  await harness.act(() => { activeTab.value = 'config'; });
  assert.equal(c.querySelector('.scope-peer-link'), null, "a peer's Config is never served");
  assert.ok([...c.querySelectorAll('.scope-peer-locked .lock-why')].some((l) => l.dataset.why === "forge's config is set on forge itself"));
  await harness.act(() => { activeTab.value = 'usage'; });
  assert.equal(c.querySelector('.scope-peers'), null, 'other tabs keep the plain strip');
  await mounted.unmount(); state.dispose();
});

test('fusing from a peer\'s page goes to this node\'s page with the set', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '' });
  state.setStatus(three);
  const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${() => {}} remote="inst_forge" switchNode=${(id) => switched.push(id)} />`);
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'Show several nodes together' }), 'click'));
  assert.equal(state.fused.value, 'all');
  assert.deepEqual(switched, [''], 'the fused view loads as this node\'s page');
  await mounted.unmount(); state.dispose();
  const opened = [];
  island.openNodeView('inst_forge', { pathname: '/groups', search: '?nodes=all&wizard=1' }, (url) => opened.push(url));
  assert.deepEqual(opened, ['/groups?wizard=1&node=inst_forge'], "opening a peer's own view leaves the fused set");
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

test('a failed status read keeps the fused set; a fleet that is gone drops it; /fleet opens every node fused', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '?nodes=all' });
  const sync = await harness.mount(harness.html`<${island.ScopeSync} state=${state} navigate=${() => {}} remote="" />`);
  await harness.act(() => { state.markStatusLoaded(); });
  assert.equal(state.fused.value, 'all', 'status not known yet (or failed): keep the selection');
  await harness.act(() => { state.setStatus(three); });
  assert.equal(state.fused.value, 'all');
  await harness.act(() => { state.clearFleet(); });
  assert.equal(state.fused.value, null, 'no fleet any more: nothing left to fuse');
  await sync.unmount(); state.dispose();
  const legacy = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '', pathname: '/fleet' });
  assert.equal(legacy.fused.value, 'all');
  legacy.dispose();
});


test('a node popover hides a while after the pointer leaves it, unless it comes back', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), search: '?nodes=all' });
  state.setStatus(three);
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${() => {}} remote="" switchNode=${() => {}} />`);
  const c = mounted.container;
  const fuse = c.querySelector('button.node-fuse');
  const root = fuse.closest('.node-pick');
  const { CLICK_CLOSE_MS } = await harness.importDashboardModule('js/hover-popover.js');
  const wait = (ms) => harness.act(() => new Promise((r) => setTimeout(r, ms)));
  await harness.act(() => harness.fireEvent(fuse, 'click'));
  assert.ok(c.querySelector('.node-fuse-pop'));
  await harness.act(() => harness.fireEvent(root, 'mouseleave'));
  await wait(CLICK_CLOSE_MS / 2);
  await harness.act(() => harness.fireEvent(root, 'mouseenter'));
  await wait(CLICK_CLOSE_MS);
  assert.ok(c.querySelector('.node-fuse-pop:not(.closing)'), 'coming back keeps it open');
  c.querySelector('.node-fuse-pop input')?.focus();
  await harness.act(() => harness.fireEvent(root, 'mouseleave'));
  await wait(CLICK_CLOSE_MS + 50);
  assert.ok(c.querySelector('.node-fuse-pop.closing'), 'it fades out, focus or not');
  await harness.act(() => harness.fireEvent(root, 'mouseenter'));
  assert.ok(c.querySelector('.node-fuse-pop:not(.closing)'), 'coming back during the fade keeps it open');
  await harness.act(() => harness.fireEvent(root, 'mouseleave'));
  await wait(CLICK_CLOSE_MS + 50);
  assert.ok(c.querySelector('.node-fuse-pop.closing'));
  await wait(300);
  assert.equal(c.querySelector('.node-fuse-pop'), null);
  assert.equal(fuse.getAttribute('aria-expanded'), 'false');
  await mounted.unmount(); state.dispose?.();
});
