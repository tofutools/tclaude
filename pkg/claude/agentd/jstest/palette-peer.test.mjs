import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('on a restricted peer page the palette keeps its own chrome and the peer\'s profile spawns', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `export let lastSnapshot = null; export function setLastSnapshot(v) { lastSnapshot=v; } export function webTerminalDefault() { return false; }`);
  harness.document.body.innerHTML = '<nav><button data-tab="groups" class="active"></button><button data-tab="processes"></button></nav>';
  const previous = globalThis.__tclaudeRemoteNode;
  globalThis.__tclaudeRemoteNode = { id: 'inst_forge7' };
  t.after(() => { globalThis.__tclaudeRemoteNode = previous; });
  const { buildCommands } = await harness.importDashboardModule('js/palette.js');
  const enabled = (spawn) => {
    const cmds = buildCommands({
      groups: [], agents: [], profiles: [{ name: 'reviewer', harness: 'claude' }], harnesses: [{ name: 'claude' }],
      peer_view: { included: spawn ? ['spawn.request'] : [], omitted: spawn ? [] : [{ feature: 'spawn.request', requires: 'spawn.request' }] },
    });
    return (label) => {
      const cmd = cmds.find((c) => c.label === label);
      assert.ok(cmd, `palette lists ${label}`);
      return cmd.enabled !== false;
    };
  };
  const shared = enabled(true);
  for (const label of ['Switch to slop theme', 'Switch to wizard theme', 'Hide right panel', 'Show right panel', 'Collapse all groups', 'Expand all groups']) {
    assert.equal(shared(label), true, `${label} is this page's own chrome`);
  }
  assert.equal(shared('Spawn reviewer…'), true, 'a peer sharing spawn requests takes a profile spawn');
  assert.equal(enabled(false)('Spawn reviewer…'), false, 'one that does not keeps it locked');
  assert.equal(shared('Create new group…'), false, 'a command the peer keeps stays locked');
});

test('on a peer page the palette opens the peer\'s agent terminals and group shells here', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `export let lastSnapshot = null; export function setLastSnapshot(v) { lastSnapshot=v; } export function webTerminalDefault() { return false; }`);
  harness.document.body.innerHTML = '<nav><button data-tab="groups" class="active"></button></nav>';
  const previous = globalThis.__tclaudeRemoteNode;
  globalThis.__tclaudeRemoteNode = { id: 'inst_forge7' };
  t.after(() => { globalThis.__tclaudeRemoteNode = previous; });
  const { buildCommands } = await harness.importDashboardModule('js/palette.js');
  const snap = (groups) => ({
    agents: [{ agent_id: 'agt_worker01', conv_id: 'c1', title: 'worker', online: true }],
    groups: [{ name: 'ops', default_cwd: '/src', members: [] }],
    peer_view: { included: ['agents.status'], groups },
  });
  const find = (cmds, label) => cmds.find((c) => c.label === label);
  const open = find(buildCommands(snap({})), 'Open terminal: worker');
  assert.ok(open, 'the agent\'s terminal opens here, not as a local window');
  assert.notEqual(open.enabled, false, 'the peer authorises the attach itself');
  assert.equal(find(buildCommands(snap({})), 'Hide window: worker'), undefined, 'no window of its to hide here');
  const locked = find(buildCommands(snap({})), 'Open terminal in group: ops');
  assert.equal(locked.enabled, false);
  assert.match(locked.disabledReason, /needs sessions\.shell/);
  assert.notEqual(find(buildCommands(snap({ ops: ['sessions.shell'] })), 'Open terminal in group: ops').enabled, false, 'its shell grant opens it');
});

test('on a restricted peer page a group\'s shut down and power on follow its grants there', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `export let lastSnapshot = null; export function setLastSnapshot(v) { lastSnapshot=v; } export function webTerminalDefault() { return false; }`);
  harness.document.body.innerHTML = '<nav><button data-tab="groups" class="active"></button></nav>';
  const previous = globalThis.__tclaudeRemoteNode;
  globalThis.__tclaudeRemoteNode = { id: 'inst_forge7' };
  t.after(() => { globalThis.__tclaudeRemoteNode = previous; });
  const { buildCommands } = await harness.importDashboardModule('js/palette.js');
  const cmds = buildCommands({
    agents: [],
    groups: [{ name: 'ops', online: 1, members: [{ conv_id: 'a', online: true }, { conv_id: 'b', online: false }] }],
    peer_view: { included: ['agents.status'], groups: { ops: ['groups.members.stop'] } },
  });
  const find = (label) => cmds.find((c) => c.label === label);
  assert.notEqual(find('Shut down group: ops').enabled, false, 'its stop grant on ops covers it');
  assert.equal(find('Power on group: ops').enabled, false);
  assert.match(find('Power on group: ops').disabledReason, /needs groups\.members\.resume/);
});
