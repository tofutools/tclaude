import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// Every way of showing an agent goes through terminals-tab.js. Each entry point
// here must end at the place that holds the agent, with no Terminals pane.
const AGENT = 'agt_0123456789abcdef0123456789abcdef';

async function setup(t, { reveal = () => true, panes = [], shellPanes = [] } = {}) {
  const harness = await createPreactHarness(t);
  const facade = await harness.importDashboardModule('js/companion-facade.js');
  const tab = await harness.importDashboardModule('js/terminals-tab.js');
  const opened = [];
  const asked = [];
  t.after(facade.registerCompanionController({ open: () => true, float: () => true, reveal: (selector, options) => { asked.push([selector, options.tabs]); return reveal(selector, options); } }));
  t.after(tab.registerTerminalShellController({
    openPane: (seed) => { opened.push(seed); return { key: seed.key }; },
    focusForSelectors: (selectors) => selectors.some((selector) => panes.includes(selector) || shellPanes.includes(selector)),
    hasAgentPane: (selectors) => selectors.some((selector) => panes.includes(selector)),
  }));
  return { harness, tab, opened, asked };
}

test('opening an agent held elsewhere opens no pane', async (t) => {
  const { tab, opened, asked } = await setup(t);
  assert.deepEqual(await tab.openWebWindowPane(AGENT, 'a'), { key: `window:${AGENT}`, floating: true });
  assert.deepEqual(opened, []);
  assert.deepEqual(asked, [[AGENT, false]]);
});

test('a raw open of the live session, as the spawn and process paths do, goes the same way', async (t) => {
  const { tab, opened } = await setup(t);
  await tab.openTerminalPane({ ws: `/api/open-window-ws/${AGENT}`, label: 'a', hideConv: AGENT, agent: AGENT });
  await tab.openTerminalPane({ ws: '/api/spawn-focus-ws/label', label: 'a', agent: AGENT });
  assert.deepEqual(opened, []);
});

test('an agent\'s shell, a peer and a held nowhere agent still open a pane', async (t) => {
  const { tab, opened } = await setup(t, { reveal: () => false });
  await tab.openWebWindowPane(AGENT, 'a');
  assert.equal(opened.length, 1);
  const held = await setup(t);
  await held.tab.openTerminalPane({ ws: `/api/term-ws/${AGENT}?which=current`, label: 's', agent: AGENT, key: 'k' });
  await held.tab.openTerminalPane({ ws: '/api/federation/terminal?x=1', label: 'p', agent: AGENT, key: 'p', remote: { peer: 'p', peerLabel: '', agent: AGENT } });
  assert.equal(held.opened.length, 2);
});

test('the focus route: a window first, then a live pane, then a pinned tab', async (t) => {
  const inWindow = await setup(t, { reveal: (_, { tabs }) => !tabs, panes: [AGENT] });
  assert.equal(inWindow.tab.focusTerminalForConv([AGENT]), true);
  const pane = await setup(t, { reveal: (_, { tabs }) => tabs, panes: [AGENT] });
  assert.equal(pane.tab.focusTerminalForConv([AGENT]), true);
  assert.deepEqual(pane.asked, [[AGENT, false]], 'a live pane beats a pinned tab');
  const tab = await setup(t, { reveal: (_, { tabs }) => tabs });
  assert.equal(tab.tab.focusTerminalForConv([AGENT]), true);
  const nowhere = await setup(t, { reveal: () => false });
  assert.equal(nowhere.tab.focusTerminalForConv([AGENT]), false, 'the caller opens it');
});

test('a shell pane of the agent does not hide its companion tab', async (t) => {
  const { tab, asked } = await setup(t, { reveal: (_, { tabs }) => tabs, shellPanes: [AGENT] });
  assert.equal(tab.focusTerminalForConv([AGENT]), true);
  assert.deepEqual(asked, [[AGENT, false], [AGENT, true]]);
});

test('a jump, as a row, a message link and the palette do it, stops at the place', async (t) => {
  const { harness, opened } = await setup(t);
  const { jumpAgent } = await harness.importDashboardModule('js/agent-jump.js');
  const calls = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url) => { calls.push(url); return { ok: true }; };
  t.after(() => { globalThis.fetch = previous; });
  const toasts = [];
  await jumpAgent(AGENT, 'a', true, '', (text) => toasts.push(text));
  await jumpAgent(AGENT, 'a', false, '', (text) => toasts.push(text));
  assert.deepEqual(opened, []);
  assert.deepEqual(calls, [], 'no native window either');
  assert.deepEqual(toasts, ['focused: a', 'focused: a']);
});
