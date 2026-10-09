import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const links = [
  { peer: 'inst_forge', label: 'forge', level: 'restricted', kind: 'grant', direction: 'in', slugs: ['groups.roster.read', 'message.direct'], online: false, last_seen: '2026-10-09T20:00:00Z' },
  { peer: 'inst_forge', label: 'forge', level: 'restricted', kind: 'grant', direction: 'in', pool: 'rigs', slugs: ['routes.consume'], online: false },
  { peer: 'inst_lab', label: 'lab', level: 'unrestricted', kind: 'route', direction: 'out', remote: 'builders', online: true },
];

test('marker summarises linked nodes and describes each link', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/group-federation-marker.js');
  assert.equal(mod.markerView([]), null);
  assert.equal(mod.markerView(undefined), null);
  const view = mod.markerView(links);
  assert.deepEqual(view.nodes, ['forge', 'lab']); assert.equal(view.live, true);
  assert.equal(mod.markerView(links.slice(0, 2)).live, false);
  const direct = mod.linkView(links[0]);
  assert.equal(direct.how, 'direct grant'); assert.equal(direct.can, 'peer can: groups.roster.read, message.direct'); assert.match(direct.state, /^offline · seen /);
  assert.equal(mod.linkView(links[1]).how, 'pool grant (rigs)');
  const route = mod.linkView(links[2]);
  assert.equal(route.direction, 'out'); assert.equal(route.how, 'route → builders'); assert.equal(route.state, 'live'); assert.equal(route.unrestricted, true);
});

test('marker popover opens without toggling the group and jumps to the node', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/group-federation-marker.js');
  const opened = []; let toggles = 0;
  const group = { name: 'builders', federation_links: links };
  const mounted = await harness.mount(harness.html`<details onClick=${() => { toggles++; }}><summary><${mod.GroupFederationMarker} group=${group} openNode=${(id) => opened.push(id)} /></summary></details>`);
  const chip = getByRole(mounted.container, 'button', { name: /🌐/ });
  assert.equal(chip.getAttribute('aria-expanded'), 'false');
  await harness.act(() => harness.fireEvent(chip, 'click'));
  assert.equal(toggles, 0, 'the click stays inside the marker');
  const pop = mounted.container.querySelector('[role="dialog"]');
  assert.ok(pop); assert.equal(pop.querySelectorAll('.gfm-row').length, 3);
  await harness.act(() => harness.fireEvent(pop.querySelectorAll('.gfm-open')[2], 'click'));
  assert.deepEqual(opened, ['inst_lab']);
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'jumping closes the popover');
  await harness.act(() => harness.fireEvent(chip, 'click'));
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(mounted.container.querySelector('[role="dialog"]'), null, 'Escape closes it');
  await mounted.unmount();
});

test('groups without federation links render no marker', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/group-federation-marker.js');
  const mounted = await harness.mount(harness.html`<${mod.GroupFederationMarker} group=${{ name: 'quiet' }} />`);
  assert.equal(mounted.container.innerHTML, '');
  await mounted.unmount();
});
