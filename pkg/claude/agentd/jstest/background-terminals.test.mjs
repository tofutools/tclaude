import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ROWS = [
  { kind: 'agent', tmux_session: 'tmux-a', key: 'window:agt_bg', label: 'old-name', agent_id: 'agt_bg', conv_id: 'conv-bg', last_activity: 1000 },
  { kind: 'shell', tmux_session: 'tclaude-term-x-current', key: 'term:agt_bg:current:', label: 'old-name', agent_id: 'agt_bg', conv_id: 'conv-bg', which: 'current' },
  { kind: 'agent', tmux_session: 'tmux-open', key: 'one', label: 'builder-1', agent_id: 'agt_one' },
  { kind: 'other', tmux_session: 'stray', label: 'stray' },
];

async function mountTabs(t) {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const badgeHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const [{ createTerminalShellState }, { createTerminalShellActions }, { TerminalTabs }, background, { registerTerminalShellController }] = await Promise.all([
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-shell-actions.js'),
    harness.importDashboardModule('js/terminal-shell-island.js'),
    harness.importDashboardModule('js/background-terminals.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
  ]);
  const state = createTerminalShellState({ persistPresentation: false });
  const actions = createTerminalShellActions({
    state, fetchImpl: async () => ({ ok: true }), windowRef: harness.window, documentRef: harness.document,
  });
  const unregister = registerTerminalShellController(actions);
  const snapshot = harness.signals.signal({ agents: [{ agent_id: 'agt_bg', conv_id: 'conv-bg', title: 'renamed-worker', online: true, state: { status: 'idle' } }] });
  const widgetFactory = (options) => ({
    connect() { options.onStatus('connected'); return Promise.resolve(true); },
    copy() { return Promise.resolve(); }, fit() {}, focus() {}, setActive() {}, status() { return 'connected'; }, dispose() {},
  });
  const mounted = await harness.mount(harness.html`<${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory} snapshot=${snapshot} />`, host);
  t.after(async () => {
    await mounted.unmount();
    unregister();
    actions.dispose();
    background.backgroundTerminals.value = [];
  });
  return { harness, host, badgeHost, state, actions, background };
}

test('background terminals keep the tab, show a neutral counter and attach from the popover', async (t) => {
  const s = await mountTabs(t);
  const { document } = s.harness;
  await s.harness.act(async () => {
    s.actions.openPane({ ws: '/one', key: 'one', label: 'builder-1', agent: 'agt_one' });
    await Promise.resolve();
  });
  await s.harness.act(() => { s.background.backgroundTerminals.value = ROWS; });

  const trigger = s.host.querySelector('.mux-bg-trigger');
  assert.ok(trigger, 'the counter renders beside the strip');
  assert.ok(trigger.closest('.mux-bg-slot'), 'outside the scrolling tab strip');
  assert.equal(trigger.querySelector('.mux-bg-count').textContent, '3', 'a row whose pane is already open is not counted');
  assert.equal(trigger.textContent.trim(), '3', 'icon and count only, no label');

  await s.harness.act(() => trigger.click());
  const pop = s.host.querySelector('.mux-bg-pop');
  assert.ok(pop, 'a click pins the breakdown open');
  const names = [...pop.querySelectorAll('.mux-bg-name')].map((node) => node.textContent);
  assert.deepEqual(names, ['renamed-worker', 'shell · renamed-worker', 'stray'], 'live roster titles, grouped by kind');
  assert.equal(pop.querySelectorAll('.mux-bg-attach').length, 2, 'an unrecognised session cannot be attached');

  await s.harness.act(async () => {
    pop.querySelector('.mux-bg-attach').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  assert.ok(s.state.panes.value.some((pane) => pane.key === 'window:agt_bg'), 'attach opens the agent as a normal tab');
  assert.equal(s.host.querySelector('.mux-bg-count').textContent, '2', 'the attached row leaves the count at once');

  await s.harness.act(() => s.harness.fireEvent(document, 'keydown', { key: 'Escape' }));
  assert.equal(s.host.querySelector('.mux-bg-pop'), null, 'Escape closes the popover');

  await s.harness.act(async () => {
    await s.actions.closeAllPanes();
    s.background.backgroundTerminals.value = ROWS.filter((row) => row.kind === 'other');
  });
  assert.equal(document.body.classList.contains('hide-terminals'), false, 'a background session keeps the Terminals tab');
  assert.match(s.host.querySelector('.mux-bg-empty').textContent, /1 tclaude session is still running/);

  await s.harness.act(() => { s.background.backgroundTerminals.value = []; });
  assert.equal(document.body.classList.contains('hide-terminals'), true, 'nothing open and nothing in the background hides the tab');
});

test('background terminal helpers', async (t) => {
  const harness = await createPreactHarness(t);
  const { idleText, visibleBackgroundTerminals, startBackgroundTerminalsPoll, backgroundTerminals } = await harness.importDashboardModule('js/background-terminals.js');
  assert.deepEqual(idleText(0), { text: '', stale: false });
  assert.deepEqual(idleText(1000, 1030), { text: 'just now', stale: false });
  assert.deepEqual(idleText(1000, 1000 + 12 * 60), { text: 'idle 12m', stale: false });
  assert.deepEqual(idleText(1000, 1000 + 3 * 3600), { text: 'idle 3h', stale: true });
  assert.deepEqual(idleText(1000, 1000 + 2 * 86400), { text: 'idle 2d', stale: true });
  assert.deepEqual(visibleBackgroundTerminals(ROWS, [{ key: 'one' }]).map((row) => row.tmux_session),
    ['tmux-a', 'tclaude-term-x-current', 'stray']);

  const requests = [];
  const stop = startBackgroundTerminalsPoll({
    intervalMs: 60_000,
    doc: harness.document,
    fetchImpl: async (path) => { requests.push(path); return { ok: true, json: async () => ({ terminals: ROWS.slice(0, 1) }) }; },
  });
  t.after(() => { stop(); backgroundTerminals.value = []; });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(requests, ['/api/background-terminals']);
  assert.equal(backgroundTerminals.value.length, 1);
});
