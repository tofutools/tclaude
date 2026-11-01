import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_0123456789abcdef0123456789abcdef';
const shell = { key: `term:${AGENT}:worktree:g`, label: 'shell · a', seed: { ws: `/api/term-ws/${AGENT}?which=worktree&group=g`, key: `term:${AGENT}:worktree:g`, agent: AGENT } };
const groupShell = { key: 'groupterm:g', label: 'g', seed: { ws: '/api/group-term-ws/g', key: 'groupterm:g' } };

async function model(t) {
  const harness = await createPreactHarness(t);
  return harness.importDashboardModule('js/terminal-window-model.js');
}

test('a directory or group shell floats; an agent, a board and a peer do not', async (t) => {
  const { overlayablePane } = await model(t);
  assert.equal(overlayablePane(shell), true);
  assert.equal(overlayablePane(groupShell), true);
  assert.equal(overlayablePane({ key: `window:${AGENT}`, seed: { ws: `/api/open-window-ws/${AGENT}`, agent: AGENT } }), false);
  assert.equal(overlayablePane({ key: 'board:b', seed: { board: 'b' } }), false);
  assert.equal(overlayablePane({ key: 'x', seed: { ws: shell.seed.ws, remote: { peer: 'p' } } }), false);
  assert.equal(overlayablePane(null), false);
});

test('a window keeps what brings the same tab back', async (t) => {
  const { paneSeedOf, terminalWindowOf, validTerminalWindow } = await model(t);
  const window = terminalWindowOf(shell);
  assert.equal(validTerminalWindow(window), true);
  assert.deepEqual(paneSeedOf(window), { ws: shell.seed.ws, key: shell.key, label: 'shell · a', agent: AGENT });
  assert.deepEqual(paneSeedOf(terminalWindowOf(groupShell)), { ws: '/api/group-term-ws/g', key: 'groupterm:g', label: 'g' });
});

test('a saved window only connects to a shell route', async (t) => {
  const { terminalWindowId, validTerminalWindow } = await model(t);
  const saved = (ws, paneKey = 'p') => ({ id: terminalWindowId(paneKey), kind: 'terminal', paneKey, ws });
  assert.equal(validTerminalWindow(saved('/api/group-term-ws/g')), true);
  assert.equal(validTerminalWindow(saved('/api/open-window-ws/x')), false);
  assert.equal(validTerminalWindow(saved('https://example.com/api/term-ws/x')), false);
  assert.equal(validTerminalWindow(saved('/api/term-ws/x/../../shutdown')), false);
  assert.equal(validTerminalWindow({ ...saved('/api/group-term-ws/g'), id: 'terminal:other' }), false);
});
