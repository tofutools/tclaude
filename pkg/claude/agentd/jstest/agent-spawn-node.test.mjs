import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const NODE = 'inst_forge0001';
const harnesses = [{
  name: 'claude', display_name: 'Claude Code',
  models: ['sonnet', 'opus'], effort_levels: ['low', 'high'],
  can_sandbox: true, sandbox_modes: ['inherit', 'on'], default_sandbox: 'inherit', sandbox_mode_help: {},
  can_approval: true, approval_modes: ['inherit', 'plan'], default_approval: 'inherit', approval_mode_help: {},
  can_tools: false, tools_modes: [], default_tools: '', tools_mode_help: {},
  can_ask_timeout: false, ask_timeout_modes: [], default_ask_timeout: '',
  can_remote_control: true, can_auto_memory: true, can_dir_trust: true, dir_trust_store: '~/.claude.json',
}];
const respond = (body, status = 200) => ({ ok: status < 300, status, json: async () => body, text: async () => JSON.stringify(body) });

async function flush(harness, turns = 8) {
  await harness.act(async () => {
    for (let turn = 0; turn < turns; turn += 1) await Promise.resolve();
  });
}

test("a node's spawn actions send every request to that node and keep this machine's tools here", async (t) => {
  const harness = await createPreactHarness(t);
  const { createAgentSpawnActions } = await harness.importDashboardModule('js/agent-spawn-actions.js');
  const calls = []; const terminals = []; const notes = [];
  const fetchImpl = async (url, init = {}) => {
    calls.push([String(url), init.method || 'GET', init.body ? JSON.parse(init.body) : null]);
    if (String(url).endsWith('/spawn-profiles')) return respond([{ name: 'fast' }]);
    if (String(url).endsWith('/spawn-profile-default')) return respond({ name: 'fast' });
    return respond({ agent_id: 'agt_new1', conv_id: 'c1' });
  };
  const local = createAgentSpawnActions({ fetchImpl, notify: (m) => notes.push(m), openNodeTerminal: async (o) => { terminals.push(o); } });
  assert.equal(local.canSaveProfile, true);
  const actions = local.forNode({ node: NODE, nodeId: NODE, name: 'forge' });
  assert.equal(actions.canSaveProfile, false, "the profile editor saves to the page's node");
  assert.deepEqual((await actions.loadProfiles()).map((p) => p.name), ['fast']);
  assert.equal(actions.dashboardDefaultProfile(), 'fast', "the node's global default profile pre-fills");
  assert.match((await actions.pickDirectory({})).error, /forge's folders/);
  await actions.spawn({ url: '/api/groups/build/spawn', body: { name: 'w', auto_focus: true, auto_focus_web: true } });
  await actions.complete({ agent_id: 'agt_new1' }, { name: 'w', group: 'build', autoFocus: true });
  assert.deepEqual(calls.map(([url]) => url), [
    `/api/peer/${NODE}/spawn-profiles`, `/api/peer/${NODE}/spawn-profile-default`, `/api/peer/${NODE}/groups/build/spawn`,
  ]);
  assert.deepEqual(calls[2][2], { name: 'w', auto_focus: false }, "no window opens on the node's desktop");
  assert.deepEqual(terminals, [{ instance: NODE, agent: 'agt_new1', peerLabel: 'forge' }], 'the browser terminal opens the new agent');
  assert.match(notes[0], /spawned w → build/);
});

test("the spawn dialog on another node is the local dialog over that node's snapshot", async (t) => {
  const harness = await createPreactHarness(t);
  const [{ AgentSpawnApp }, { createAgentSpawnState }] = await Promise.all([
    harness.importDashboardModule('js/agent-spawn-island.js'),
    harness.importDashboardModule('js/agent-spawn-state.js'),
  ]);
  const urls = [];
  const fetchImpl = async (url) => {
    urls.push(String(url));
    if (String(url).includes('/saved-prompts')) return respond([{ name: 'house-rules', body: 'Follow the house rules.' }]);
    if (String(url).includes('/spawn-profiles') || String(url).includes('/sandbox-profiles')) return respond([]);
    if (String(url).includes('/worktrees')) return respond({ is_repo: false, worktrees: [], branches: [] });
    return respond({});
  };
  const prevFetch = harness.window.fetch;
  harness.window.fetch = fetchImpl; globalThis.fetch = fetchImpl;
  t.after(() => { harness.window.fetch = prevFetch; });
  const { createAgentSpawnActions } = await harness.importDashboardModule('js/agent-spawn-actions.js');
  const actions = createAgentSpawnActions({ fetchImpl, loadProfiles: () => { throw new Error("this node's profiles must not load"); } });
  const state = createAgentSpawnState({ getSnapshot: () => ({ groups: [{ name: 'local-only' }], harnesses }) });
  harness.window.HTMLElement.prototype.scrollIntoView = () => {};
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${AgentSpawnApp} state=${state} actions=${actions} confirmDiscard=${async () => true} notify=${() => {}} />`, host);
  state.open({ groupName: 'build', target: { node: NODE, nodeId: NODE, name: 'forge', snapshot: { groups: [{ name: 'build' }], harnesses } } });
  await flush(harness);
  await harness.act(() => new Promise((r) => setTimeout(r, 300)));
  await flush(harness);
  const q = (sel) => host.querySelector(sel);
  assert.match(q('#agent-spawn-title').textContent, /on forge/);
  assert.equal(q('#agent-spawn-remote-intro'), null, 'no request-mode banner');
  assert.deepEqual([...q('#agent-spawn-group').querySelectorAll('option')].map((o) => o.value).filter(Boolean), ['build'], "the node's groups, not this node's");
  assert.ok(q('.saved-prompt-chip-manage'), 'Manage… saved prompts is there, as locally');
  assert.ok([...q('.saved-prompt-chip-add').querySelectorAll('option')].some((o) => o.value === 'house-rules'), "the node's prompt library");
  for (const id of ['agent-spawn-cwd-browse', 'agent-spawn-wt-repo-browse', 'agent-spawn-save-profile']) {
    assert.equal(q(`#${id}`).disabled, true, `${id} stays with this machine`);
  }
  for (const id of ['agent-spawn-cwd', 'agent-spawn-harness', 'agent-spawn-queue', 'agent-spawn-attach-btn', 'agent-spawn-focus']) {
    assert.equal(q(`#${id}`).disabled, false, `${id} works as locally`);
  }
  assert.ok(urls.length > 0 && urls.every((u) => u.startsWith(`/api/peer/${NODE}/`)), `every request reaches forge: ${urls.join(', ')}`);
  await mounted.unmount();
});

test('a spawn on an unrestricted peer or on this node opens the local dialog there; a restricted peer takes a request', async (t) => {
  const harness = await createPreactHarness(t);
  const [controller, nodeApi] = await Promise.all([
    harness.importDashboardModule('js/agent-spawn-controller.js'),
    harness.importDashboardModule('js/node-api.js'),
  ]);
  const opened = [];
  t.after(controller.registerAgentSpawnController({ open: (o) => opened.push(o) }));
  nodeApi.setSelfNode('inst_desk0001');
  const snap = { groups: [{ name: 'build' }], peer_view: { level: 'unrestricted' } };
  await controller.openRemoteAgentSpawn({ nodeId: NODE, node: 'forge', groupName: 'build', snapshot: snap });
  assert.deepEqual(opened.pop(), { groupName: 'build', target: { node: NODE, nodeId: NODE, name: 'forge', snapshot: snap } });
  await controller.openRemoteAgentSpawn({ nodeId: 'inst_desk0001', node: 'desk', groupName: 'ops', snapshot: { groups: [] } });
  assert.equal(opened.pop().target.nodeId, '', "this node's own agent opens no peer terminal");
  const prepared = [];
  await controller.openRemoteAgentSpawn({ nodeId: NODE, node: 'forge', groupName: 'build', snapshot: { peer_view: { level: 'restricted' } },
    prepare: async (o) => { prepared.push(o.group); return { options: { defaults: {} } }; } });
  assert.deepEqual(prepared, ['build']);
  assert.ok(opened.pop().remote, 'a restricted peer opens request mode');
});
