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
