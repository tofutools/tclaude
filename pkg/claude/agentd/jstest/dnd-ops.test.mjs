import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const FORGE = 'inst_forge0001';
const ok = (body = {}) => ({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });

async function setup(t, { level = 'unrestricted' } = {}) {
  const harness = await createPreactHarness(t);
  const [ops, nodeApi] = await Promise.all([harness.importDashboardModule('js/dnd-ops.js'), harness.importDashboardModule('js/node-api.js')]);
  nodeApi.setSelfNode('inst_desk0001');
  const forge = { groups: [{ name: 'build', members: [{ conv_id: 'c_a', agent_id: 'agt_a', cwd: '/srv/a' }] }, { name: 'ops', members: [] }], peer_view: { level } };
  t.after(nodeApi.setNodeSnapshotSource((node) => (node === FORGE ? forge : null)));
  const page = { groups: [{ name: 'build', members: [{ conv_id: 'c_p', agent_id: 'agt_p' }] }, { name: 'ops', members: [] }] };
  const calls = []; const toasts = []; const clones = []; const announced = []; let renders = 0;
  t.after(nodeApi.onNodeChanged((node) => announced.push(node)));
  const fetchImpl = async (url, init = {}) => { calls.push(`${init.method || 'GET'} ${url}`); return ok({}); };
  const dnd = ops.createDndOps({
    confirm: async () => true, toast: (m, err) => toasts.push([m, !!err]), refresh: async () => {}, render: () => { renders += 1; },
    getSnapshot: () => page, openClone: (...args) => clones.push(args), openRetire: async () => null, fetchImpl,
  });
  return { harness, ops, dnd, calls, toasts, clones, announced, page, renders: () => renders };
}

test("a drop on another node's group moves the agent there through that node", async (t) => {
  const { dnd, calls, announced, renders } = await setup(t);
  const ctx = dnd.context({ node: FORGE, name: 'forge' });
  assert.equal(dnd.allowed(ctx), true);
  await dnd.move({ conv: 'c_a', agent: 'agt_a', sourceGroup: 'build@forge', label: 'a' }, 'ops@forge', ctx);
  assert.deepEqual(calls, [`POST /api/peer/${FORGE}/groups/ops/members`, `DELETE /api/peer/${FORGE}/groups/build/members/agt_a`], "forge's own group names, on forge");
  assert.equal(renders(), 0, "no optimistic splice into this page's snapshot");
  assert.deepEqual(announced, [FORGE], 'the fused view rereads forge');
});

test("a drop on the page's own groups moves as before, optimistically", async (t) => {
  const { dnd, calls, page, renders } = await setup(t);
  await dnd.move({ conv: 'c_p', agent: 'agt_p', sourceGroup: 'build', label: 'p' }, 'ops');
  assert.deepEqual(calls, ['POST /api/groups/ops/members', 'DELETE /api/groups/build/members/agt_p']);
  assert.deepEqual(page.groups[1].members.map((m) => m.conv_id), ['c_p']);
  assert.equal(renders(), 1);
});

test('a Ctrl/Cmd drop opens the clone dialog on the agent\'s node, joining the group dropped on', async (t) => {
  const { dnd, clones, calls } = await setup(t);
  await dnd.clone({ conv: 'c_a', agent: 'agt_a', label: 'a' }, 'ops@forge', dnd.context({ node: FORGE, name: 'forge' }));
  assert.deepEqual(clones, [['agt_a', 'a', '/srv/a', FORGE, 'ops']]);
  assert.deepEqual(calls, [], 'the dialog sends the clone, not the drop');
});

test('a node trusting this one restricted refuses drops on its groups', async (t) => {
  const { dnd, toasts } = await setup(t, { level: 'restricted' });
  const ctx = dnd.context({ node: FORGE, name: 'forge' });
  assert.equal(dnd.allowed(ctx), false);
  assert.equal(dnd.allowed(dnd.context()), true, "the page's node always");
  dnd.refused(ctx);
  assert.match(toasts.at(-1)[0], /forge trusts this node restricted/);
});

test('a box names its node and the group it lists', async (t) => {
  const { harness, ops } = await setup(t);
  const doc = harness.document;
  doc.body.innerHTML = `<details id="peer" data-fleet-node="${FORGE}" data-fleet-node-name="forge" data-group-key="ops@forge"><summary id="in">x</summary></details>
    <details id="local" data-dnd-target-group="ops" data-group-key="ops"></details>`;
  assert.deepEqual(ops.dndNode(doc.getElementById('in')), { node: FORGE, name: 'forge' });
  assert.equal(ops.targetGroupOf(doc.getElementById('peer')), 'ops@forge');
  assert.deepEqual(ops.dndNode(doc.getElementById('local')), { node: '', name: '' });
  assert.equal(ops.targetGroupOf(doc.getElementById('local')), 'ops');
});

test("a group drag acts on the dragged group's node under its own names", async (t) => {
  const { harness, ops, toasts } = await setup(t);
  const doc = harness.document;
  doc.body.innerHTML = `<div id="fleetview-fused-groups-root">
    <details id="peer" data-fleet-node="${FORGE}" data-fleet-node-name="forge" data-group-key="build@forge"><summary id="ph" data-group-reorder="build@forge">b</summary></details>
    <details id="peer2" data-fleet-node="${FORGE}" data-fleet-node-name="forge" data-group-key="ops@forge"><summary>o</summary></details>
    <details id="home" data-fleet-node="inst_desk0001" data-fleet-node-name="desk" data-group-key="ops@desk"><summary>o</summary></details></div>
    <details id="local" data-group-key="ops"><summary id="lh" data-group-reorder="ops">o</summary></details>`;
  const calls = []; let refreshed = 0;
  const ctx = ops.groupDragContext(doc.getElementById('ph'), { refresh: () => { refreshed += 1; }, fetchImpl: async (url) => { calls.push(url); return { ok: true }; } });
  assert.deepEqual([ctx.node, ctx.fused, ctx.own('build@forge'), ctx.own('ops')], [FORGE, true, 'build', 'ops']);
  await ctx.api(`/api/groups/${ctx.own('build@forge')}/parent`, { method: 'PUT' });
  assert.deepEqual(calls, [`/api/peer/${FORGE}/groups/build/parent`]);
  ctx.done();
  assert.equal(refreshed, 1);
  assert.equal(ops.sameGroupTree(ctx, doc.getElementById('peer2')), true, "forge's other group");
  assert.equal(ops.sameGroupTree(ctx, doc.getElementById('home')), false, "never another node's group");
  assert.equal(ops.sameGroupTree(ctx, doc.getElementById('local')), false, "never this page's own Groups view");
  const local = ops.groupDragContext(doc.getElementById('lh'));
  assert.deepEqual([local.node, local.fused, local.own('ops')], ['', false, 'ops'], 'the local Groups view as before');
  assert.equal(ops.nodeAllowsActing(FORGE), true);
  assert.deepEqual(toasts, []);
});
