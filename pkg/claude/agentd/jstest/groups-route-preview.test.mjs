import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const LEASE = { id: `hlease_${'a'.repeat(32)}`, endpoint: '127.0.0.1:33789', consumer_kind: 'host' };
const PREVIEW = `http://route-${'a'.repeat(32)}.localhost:33789/`;

async function setup(t, { hostname = '127.0.0.1', remote = false, consumers = [LEASE] } = {}) {
  const harness = await createPreactHarness(t);
  harness.window.location = new URL(`http://${hostname}:8321/`);
  const priorRemote = globalThis.__tclaudeRemoteNode;
  if (remote) globalThis.__tclaudeRemoteNode = { id: 'inst_remote' };
  else delete globalThis.__tclaudeRemoteNode;
  t.after(() => {
    if (priorRemote === undefined) delete globalThis.__tclaudeRemoteNode;
    else globalThis.__tclaudeRemoteNode = priorRemote;
  });
  const [{ createGroupsState }, { GroupsRouteMap }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-route-map.js'),
  ]);
  const state = createGroupsState({
    prefs: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    resetOffsets: () => {}, reorder: (groups) => groups,
    columns: { list: () => [], hidden: () => false, setHidden: () => {}, deviationCount: () => 0 },
  });
  state.initialize();
  const publish = (leases) => state.publish({
    groups: [{ name: 'team', members: [] }], groups_route_map_enabled: true,
    route_map: { routes: [{ id: 'rte_preview', name: 'preview', group: 'team', state: 'ready', generation_health: 'current', consumers: leases }] },
  });
  publish(consumers);
  state.setRouteGroup('team');
  state.setSubview('routes', 'rte_preview');
  const mounted = await harness.mount(harness.html`<${GroupsRouteMap} state=${state} />`);
  return { harness, publish, root: mounted.container };
}

test('local preview links use a lease-specific cookie hostname', async (t) => {
  const { root } = await setup(t);
  const link = root.querySelector('a.route-map-preview');
  assert.ok(link);
  assert.equal(link.getAttribute('href'), PREVIEW);
  assert.equal(link.textContent, PREVIEW);
  assert.equal(link.getAttribute('target'), '_blank');
  assert.equal(link.getAttribute('rel'), 'noopener noreferrer');
  assert.notEqual(new URL(link.getAttribute('href')).hostname, '127.0.0.1');
});

for (const settings of [{ hostname: 'daemon.example' }, { remote: true }]) {
  test(`remote previews show tunnel guidance without a browser-loopback link: ${JSON.stringify(settings)}`, async (t) => {
    const { root } = await setup(t, settings);
    assert.equal(root.querySelector('a.route-map-preview'), null);
    assert.match(root.textContent, /127\.0\.0\.1:33789/);
    assert.match(root.textContent, /SSH tunnel/);
  });
}

test('success links disappear with their exact lease, even if its port is reused', async (t) => {
  const { harness, root, publish } = await setup(t, { consumers: [] });
  t.mock.method(globalThis, 'fetch', async () => ({ ok: true, json: async () => LEASE }));
  await harness.act(async () => {
    harness.fireEvent(getByRole(root, 'button', { name: 'Open on host' }), 'click');
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
  assert.equal(root.querySelector('[role="status"] a').getAttribute('href'), PREVIEW);
  await harness.act(() => publish([LEASE]));
  assert.ok(root.querySelector('[role="status"] a'));
  const replacement = { ...LEASE, id: `hlease_${'b'.repeat(32)}` };
  await harness.act(() => publish([replacement]));
  assert.equal(root.querySelector('[role="status"]'), null);
  assert.equal(root.querySelector('a.route-map-preview').getAttribute('href'), PREVIEW.replaceAll('a'.repeat(32), 'b'.repeat(32)));
  await harness.act(() => publish([]));
  assert.equal(root.querySelector('a.route-map-preview'), null);
});

test('an endpoint closed before the first snapshot clears the immediate success link', async (t) => {
  const { harness, root, publish } = await setup(t, { consumers: [] });
  t.mock.method(globalThis, 'fetch', async () => ({ ok: true, json: async () => LEASE }));
  await harness.act(async () => {
    harness.fireEvent(getByRole(root, 'button', { name: 'Open on host' }), 'click');
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
  assert.ok(root.querySelector('[role="status"] a'));
  await harness.act(() => publish([]));
  assert.equal(root.querySelector('[role="status"]'), null);
});

test('preview URL validation refuses malformed leases and the dashboard cookie hostname', async (t) => {
  const harness = await createPreactHarness(t);
  const { hostRoutePreviewURL } = await harness.importDashboardModule('js/groups-route-preview.js');
  const local = { local: true, hostname: 'localhost' };
  assert.equal(hostRoutePreviewURL(LEASE, local), PREVIEW);
  assert.equal(hostRoutePreviewURL(LEASE, { local: false, hostname: 'localhost' }), '');
  assert.equal(hostRoutePreviewURL(LEASE, { local: true, hostname: new URL(PREVIEW).hostname }), '');
  assert.equal(hostRoutePreviewURL(LEASE, { local: true }), '');
  for (const lease of [{ ...LEASE, endpoint: 'example.com:80' }, { ...LEASE, endpoint: '127.0.0.1:65536' }, { ...LEASE, id: 'hlease_evil.example/' }]) {
    assert.equal(hostRoutePreviewURL(lease, local), '');
  }
});
