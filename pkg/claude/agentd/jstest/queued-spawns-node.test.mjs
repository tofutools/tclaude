import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test("another node's queued spawn opens and discards on that node, under its own group name", async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/agent-spawn-controller.js', `
    export const opened = [];
    export function openAgentSpawnModal(options) { opened.push(options); return true; }
  `);
  const [m, controller, nodeApi] = await Promise.all([
    harness.importDashboardModule('js/queued-spawns.js'),
    harness.importDashboardModule('js/agent-spawn-controller.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  const release = nodeApi.setNodeSnapshotSource((id) => (id === 'inst_forge1234' ? { groups: [{ name: 'crew' }] } : null));
  t.after(release);
  const urls = [];
  m.configureQueuedSpawns({ fetchImpl: async (url) => { urls.push(url); return new Response('', { status: 200 }); }, confirm: async () => true, notify: () => {}, refresh: () => {} });
  const entry = { id: 7, group: 'crew@forge', own_group: 'crew', fleet_node: 'inst_forge1234', fleet_node_name: 'forge', draft: { group: 'crew', name: 'db' } };
  m.launchQueuedSpawn(entry);
  const options = controller.opened.at(-1);
  assert.equal(options.autoSpawn, true);
  assert.equal(options.queued.group, 'crew', 'the dialog takes the node\'s own group name');
  assert.deepEqual({ node: options.target.node, nodeId: options.target.nodeId, name: options.target.name }, { node: 'inst_forge1234', nodeId: 'inst_forge1234', name: 'forge' });
  assert.deepEqual(options.target.snapshot.groups.map((g) => g.name), ['crew'], 'and that node\'s snapshot');
  m.editQueuedSpawn({ id: 8, group: 'crew', draft: {} });
  assert.equal(controller.opened.at(-1).target, undefined, 'the page\'s own queued spawn opens as before');
  assert.equal(await m.discardQueuedSpawn(entry), true);
  await m.discardQueuedSpawn({ id: 9, group: 'crew', draft: {} });
  assert.deepEqual(urls, ['/api/peer/inst_forge1234/queued-spawns/7', '/api/queued-spawns/9']);
});
