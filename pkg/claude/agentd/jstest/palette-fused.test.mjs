import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('the palette spawns into and messages another node\'s group in the fused view on that node', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ buildFusedNodeCommands }, nodeApi] = await Promise.all([
    harness.importDashboardModule('js/palette-fused.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  nodeApi.setSelfNode('inst_self');
  t.after(() => nodeApi.setSelfNode(''));
  const node = (id, name, local = false) => ({ id, name, local });
  const listing = {
    groups: [
      { name: 'ops@desk', fleet_node: node('inst_self', 'desk', true) },
      { name: 'build@forge', fleet_node: node('inst_forge', 'forge'), members: [
        { agent_id: 'agt_builder01', title: 'builder', state: { online: true } },
        { agent_id: 'agt_sleeper01', title: 'sleeper', state: { online: false } },
      ] },
      { name: 'qa@lab', fleet_node: node('inst_lab', 'lab') },
    ],
  };
  const snapshots = {
    inst_forge: { peer_view: { level: 'unrestricted' } },
    inst_lab: { peer_view: { included: ['messaging'], omitted: [{ feature: 'spawn.request', requires: 'spawn.request' }] } },
  };
  const spawned = []; const messaged = []; const terminals = [];
  const cmds = buildFusedNodeCommands(listing, {
    snapshotOf: (id) => snapshots[id] || null,
    openSpawn: (o) => spawned.push(o), openMessage: (...args) => messaged.push(args), openTerminal: (o) => terminals.push(o),
  });
  assert.deepEqual(cmds.map((c) => c.label), [
    'Spawn agent in build@forge…', 'Message group: build@forge…', 'Open terminal: builder@forge',
    'Spawn agent in qa@lab…', 'Message group: qa@lab…',
  ], 'the page\'s own groups keep their ordinary commands; an offline agent has no terminal');
  assert.ok(cmds.every((c) => c.node), 'each names its node');
  const terminal = cmds.splice(2, 1)[0];
  terminal.run();
  assert.deepEqual(terminals, [{ instance: 'inst_forge', agent: 'agt_builder01', peerLabel: 'forge', label: 'builder' }]);
  assert.deepEqual(cmds.map((c) => c.enabled !== false), [true, true, false, true]);
  assert.match(cmds[2].disabledReason, /lab does not share spawning with you \(needs spawn\.request\)/);
  cmds[0].run();
  assert.deepEqual(spawned, [{ nodeId: 'inst_forge', node: 'forge', groupName: 'build', snapshot: snapshots.inst_forge }]);
  cmds[3].run();
  assert.deepEqual(messaged, [[{ targetMode: 'group', groupName: 'qa' }, 'inst_lab', 'lab']]);
});

test('a command naming its node keeps its own gate on a peer\'s page', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const cmds = [{ label: 'Create new group…' }, { label: 'Spawn agent in build@forge…', node: 'inst_forge' }];
  const out = mod.limitPeerViewCommands(cmds, { peer_view: { included: [] } }, { id: 'inst_lab' });
  assert.deepEqual(out.map((c) => c.enabled !== false), [false, true]);
  assert.equal(out[1], cmds[1]);
});
