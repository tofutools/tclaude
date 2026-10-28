import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const FORGE = 'inst_forge0001';
const ok = (body = {}) => ({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });

async function setup(t, { level = 'unrestricted', agents, respond = () => ok({}) } = {}) {
  const harness = await createPreactHarness(t);
  const [ops, nodeApi] = await Promise.all([harness.importDashboardModule('js/dnd-ops.js'), harness.importDashboardModule('js/node-api.js')]);
  nodeApi.setSelfNode('inst_desk0001');
  const forge = { groups: [{ name: 'build', members: [{ conv_id: 'c_a', agent_id: 'agt_a', cwd: '/srv/a' }] }, { name: 'ops', members: [] }], peer_view: { level, ...(agents ? { agents } : {}) } };
  t.after(nodeApi.setNodeSnapshotSource((node) => (node === FORGE ? forge : null)));
  const page = { groups: [{ name: 'build', members: [{ conv_id: 'c_p', agent_id: 'agt_p' }] }, { name: 'ops', members: [] }] };
  const calls = []; const toasts = []; const clones = []; const retires = []; const announced = []; let renders = 0;
  t.after(nodeApi.onNodeChanged((node) => announced.push(node)));
  const bodies = [];
  const fetchImpl = async (url, init = {}) => { calls.push(`${init.method || 'GET'} ${url}`); bodies.push(init.body ? JSON.parse(init.body) : null); return respond(url, init); };
  const confirms = [];
  let answerConfirm = () => true;
  const dnd = ops.createDndOps({
    confirm: async (options) => { confirms.push(options); return answerConfirm(options); }, toast: (m, err) => toasts.push([m, !!err]), refresh: async () => {}, render: () => { renders += 1; },
    getSnapshot: () => page, openClone: (...args) => clones.push(args), openRetire: async (...args) => { retires.push(args); return null; }, fetchImpl,
  });
  return { harness, ops, dnd, calls, bodies, confirms, toasts, clones, retires, announced, page, forge, renders: () => renders,
    onConfirm: (fn) => { answerConfirm = fn; } };
}

test("a drop on another node's group moves the agent there in one call through that node", async (t) => {
  const { dnd, calls, bodies, confirms, announced, renders } = await setup(t);
  const ctx = dnd.context({ node: FORGE, name: 'forge' });
  assert.equal(dnd.allowed(ctx), true);
  await dnd.move({ conv: 'c_a', agent: 'agt_a', sourceGroup: 'build@forge', label: 'a' }, 'ops@forge', ctx);
  assert.deepEqual(calls, [`POST /api/peer/${FORGE}/groups/build/members/agt_a/move`], "forge's own group names, on forge");
  assert.deepEqual(bodies, [{ to: 'ops', keep_permissions: false, keep_sandbox: false, keep_environment: false }]);
  assert.deepEqual(confirms[0].checks.map((check) => check.key), ['permissions', 'sandbox', 'environment']);
  assert.equal(renders(), 0, "no optimistic splice into this page's snapshot");
  assert.deepEqual(announced, [FORGE], 'the fused view rereads forge');
});

const noMoveRoute = (url) => (url.endsWith('/move') ? { ok: false, status: 404, text: async () => 'not found' } : ok({}));

test('a node without the move route takes the add-then-remove pair', async (t) => {
  const { dnd, calls } = await setup(t, { respond: noMoveRoute });
  await dnd.move({ conv: 'c_a', agent: 'agt_a', sourceGroup: 'build@forge', label: 'a' }, 'ops@forge', dnd.context({ node: FORGE, name: 'forge' }));
  assert.deepEqual(calls, [
    `POST /api/peer/${FORGE}/groups/build/members/agt_a/move`,
    `POST /api/peer/${FORGE}/groups/ops/members`,
    `DELETE /api/peer/${FORGE}/groups/build/members/agt_a`,
  ]);
});

test('a node without the move route refuses a move that keeps a setting', async (t) => {
  const { dnd, calls, toasts, onConfirm } = await setup(t, { respond: noMoveRoute });
  onConfirm((options) => { options.checks.find((check) => check.key === 'environment').checked = false; return true; });
  await dnd.move({ conv: 'c_a', agent: 'agt_a', sourceGroup: 'build@forge', label: 'a' }, 'ops@forge', dnd.context({ node: FORGE, name: 'forge' }));
  assert.deepEqual(calls, [`POST /api/peer/${FORGE}/groups/build/members/agt_a/move`]);
  assert.equal(toasts.at(-1)[1], true);
  assert.match(toasts.at(-1)[0], /tick every box/);
});

test("a drop on the page's own groups moves in one call, optimistically", async (t) => {
  const { dnd, calls, bodies, confirms, page, renders } = await setup(t);
  await dnd.move({ conv: 'c_p', agent: 'agt_p', sourceGroup: 'build', label: 'p' }, 'ops');
  assert.deepEqual(calls, ['POST /api/groups/build/members/agt_p/move']);
  assert.deepEqual(bodies, [{ to: 'ops', keep_permissions: false, keep_sandbox: false, keep_environment: false }], 'new group defaults by default');
  assert.deepEqual(confirms[0].checks.map((check) => check.key), ['permissions', 'sandbox', 'environment']);
  assert.deepEqual(page.groups[1].members.map((m) => m.conv_id), ['c_p']);
  assert.equal(renders(), 1);
});

test('unticking the permissions box keeps the group left\'s permissions', async (t) => {
  const { dnd, bodies, toasts, onConfirm } = await setup(t, { respond: () => ok({ kept_permissions: true }) });
  onConfirm((options) => { options.checks.find((check) => check.key === 'permissions').checked = false; return true; });
  await dnd.move({ conv: 'c_p', agent: 'agt_p', sourceGroup: 'build', label: 'p' }, 'ops');
  assert.deepEqual(bodies, [{ to: 'ops', keep_permissions: true, keep_sandbox: false, keep_environment: false }]);
  assert.deepEqual(toasts.at(-1), ['moved p: build → ops', false]);
});

test('a node that ignores keep_permissions is reported, not toasted as kept', async (t) => {
  const { dnd, toasts, onConfirm } = await setup(t);
  onConfirm((options) => { options.checks.find((check) => check.key === 'permissions').checked = false; return true; });
  await dnd.move({ conv: 'c_p', agent: 'agt_p', sourceGroup: 'build', label: 'p' }, 'ops');
  assert.equal(toasts.at(-1)[1], true);
  assert.match(toasts.at(-1)[0], /did not keep build's permissions/);
});

test('unticking a move checkbox keeps that setting on the group left', async (t) => {
  const { dnd, bodies, onConfirm } = await setup(t);
  onConfirm((options) => { options.checks.find((check) => check.key === 'sandbox').checked = false; return true; });
  await dnd.move({ conv: 'c_p', agent: 'agt_p', sourceGroup: 'build', label: 'p' }, 'ops');
  assert.deepEqual(bodies, [{ to: 'ops', keep_permissions: false, keep_sandbox: true, keep_environment: false }]);
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

test('a restricted peer takes the clone and retire drops its grants on the agent cover', async (t) => {
  const { dnd, toasts, retires, forge } = await setup(t, { level: 'restricted', agents: { agt_a: ['groups.members.clone'] } });
  const ctx = dnd.context({ node: FORGE, name: 'forge' });
  assert.equal(dnd.allowed(ctx, 'clone', 'agt_a'), true, 'clone is granted');
  assert.equal(dnd.allowed(ctx, 'retire', 'agt_a'), false, 'retire is not');
  assert.equal(dnd.allowed(ctx, '', 'agt_a'), false, 'a move between its groups never is');
  assert.equal(dnd.allowed(ctx, 'clone', 'agt_a', 'ops@forge'), false, 'a clone joins a group only with the clone grant there');
  forge.peer_view.groups = { ops: ['groups.members.clone'] };
  assert.equal(dnd.allowed(ctx, 'clone', 'agt_a', 'ops@forge'), true);
  dnd.refused(ctx, 'retire');
  assert.match(toasts.at(-1)[0], /forge does not share this agent's retire with you \(needs groups\.members\.retire\)/);
  // Another node's retire names the stable agent_id, which its route takes.
  await dnd.retire({ conv: 'c_a', agent: 'agt_a', label: 'a' }, ctx);
  assert.deepEqual(retires.at(-1), ['agt_a', 'a', FORGE]);
  await dnd.retire({ conv: 'c_p', agent: 'agt_p', label: 'p' }, dnd.context());
  assert.deepEqual(retires.at(-1), ['c_p', 'p', ''], "the page's own stays conv-keyed");
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
