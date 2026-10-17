import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const dashboardStub = `
  export const lastSnapshot = { groups: [], ungrouped: [], paging: {} };
  export function sudoBadge() { return ''; }
  export function setLastSnapshot() {}
  export function webTerminalDefault() { return false; }
`;

test("a fused group's ⚙ control names its node, and its requests and refreshes go to that node", async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', dashboardStub);
  const [{ actionDescriptor }, { handleRowAction }, nodeApi] = await Promise.all([
    harness.importDashboardModule('js/row-actions.js'),
    harness.importDashboardModule('js/row-action-handler.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  nodeApi.setSelfNode('inst_self01');
  const requests = [];
  const fetchBefore = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    // The page's own refresh is not the operation.
    if (!String(url).startsWith('/api/snapshot')) requests.push([String(url), init?.method || 'GET', init?.body ? JSON.parse(init.body) : null]);
    return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  t.after(() => { globalThis.fetch = fetchBefore; });
  // The fused view feeds the dialogs each node's snapshot; until forge's
  // arrives its controls wait.
  let forge = null;
  t.after(nodeApi.setNodeSnapshotSource((node) => (node === 'inst_forge7' ? forge : null)));
  const changed = [];
  const off = nodeApi.onNodeChanged((node) => changed.push(node));
  t.after(off);

  harness.document.body.innerHTML = `<details data-fleet-node="inst_forge7" data-fleet-node-name="forge">
    <button id="bell" data-act="toggle-group-notify" data-group="ops@forge" data-enabled="1"></button>
    <button id="windows" data-act="window-modal-group" data-group="ops@forge"></button></details>
    <details data-fleet-node="inst_self01" data-fleet-node-name="here" data-fleet-home>
    <button id="mine" data-act="set-group-remote-control" data-group="ops@here" data-next="deny"></button></details>`;
  const el = (id) => harness.document.getElementById(id);
  const bell = actionDescriptor(el('bell'));
  assert.equal(bell.fleetNode, 'inst_forge7');
  assert.equal(bell.fleetName, 'forge');

  await handleRowAction(bell);
  assert.deepEqual(requests, [], "never against the page's same-named group");
  forge = { groups: [{ name: 'ops', notify_enabled: true }] };
  await handleRowAction(bell);
  assert.deepEqual(requests, [['/api/peer/inst_forge7/groups/ops', 'PATCH', { notify_enabled: false }]]);
  assert.deepEqual(changed, ['inst_forge7'], "the fused view re-reads the node it changed");

  // A control whose handler is not node-aware never runs on another node.
  await handleRowAction(actionDescriptor(el('windows')));
  assert.equal(requests.length, 1);

  // The page's own node takes the page's plain routes.
  await handleRowAction(actionDescriptor(el('mine')));
  assert.deepEqual(requests[1], ['/api/groups/ops', 'PATCH', { remote_control_policy: 'deny' }]);
});
