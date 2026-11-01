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

test('a terminal with no key of its own is found by its route', async (t) => {
  const harness = await createPreactHarness(t);
  const { openTerminalPane, setFloatingTerminals } = await harness.importDashboardModule('js/terminals-tab.js');
  const asked = [];
  setFloatingTerminals((key) => { asked.push(key); return true; });
  assert.deepEqual(await openTerminalPane({ ws: '/api/term-ws/a?which=current', label: 'x' }), { key: '/api/term-ws/a?which=current', floating: true });
  assert.deepEqual(asked, ['/api/term-ws/a?which=current']);
  setFloatingTerminals(() => false);
});

test('a saved window nothing shows gives up its place, so the terminal can open', async (t) => {
  const harness = await createPreactHarness(t);
  const { openTerminalPane } = await harness.importDashboardModule('js/terminals-tab.js');
  const { overlayWindows } = await harness.importDashboardModule('js/overlay-instances.js');
  await harness.importDashboardModule('js/terminal-window.js');
  const { terminalWindowOf } = await harness.importDashboardModule('js/terminal-window-model.js');
  overlayWindows.value = [terminalWindowOf(groupShell)];
  assert.equal(await openTerminalPane(groupShell.seed), null, 'it falls through to the Terminals tab, which is not mounted here');
  assert.deepEqual(overlayWindows.value, []);
});

test('a saved window only connects to a shell route', async (t) => {
  const { terminalWindowId, validTerminalWindow } = await model(t);
  const saved = (ws, paneKey = 'p') => ({ id: terminalWindowId(paneKey), kind: 'terminal', paneKey, ws });
  assert.equal(validTerminalWindow(saved('/api/group-term-ws/g')), true);
  assert.equal(validTerminalWindow(saved('/api/open-window-ws/x')), false);
  assert.equal(validTerminalWindow(saved('https://example.com/api/term-ws/x')), false);
  assert.equal(validTerminalWindow(saved('/api/term-ws/x/../../shutdown')), false);
  assert.equal(validTerminalWindow({ ...saved('/api/group-term-ws/g'), id: 'terminal:other' }), false);
  for (const sneaky of ['/api/term-ws/..\\..\\api\\open-window-ws\\x', '/api/term-ws/..', '/api/term-ws/%2e%2e', '/api/term-ws/%2E./x', '/api/term-ws/a%2fb', '/api/term-ws/a%5Cb']) {
    assert.equal(validTerminalWindow(saved(sneaky)), false, sneaky);
  }
});
