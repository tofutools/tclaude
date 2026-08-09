import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';
import {
  findTerminalAgent, terminalTabStatus, TERMINAL_TAB_STATUS_KEYS,
} from '../dashboard/js/terminal-tab-status.js';

const pane = (agent = 'agt_one', label = 'one') => ({
  label, seed: { agent },
});
const member = (status, extra = {}) => ({
  agent_id: 'agt_one', conv_id: 'conv-one', online: true,
  state: { status, ...extra },
});

test('terminal status joins pane selectors to either roster identity', () => {
  const byAgent = member('working');
  const byConv = { ...byAgent, agent_id: '', conv_id: 'conv-one' };
  const snapshot = { agents: [byAgent] };
  assert.equal(findTerminalAgent(pane('agt_one'), snapshot), byAgent);
  assert.equal(findTerminalAgent(pane('conv-one'), { agents: [byConv] }), byConv);
  assert.equal(findTerminalAgent({ label: 'legacy', seed: { hideConv: 'conv-one' } }, { agents: [byConv] }), byConv);
  assert.equal(findTerminalAgent(pane('missing'), snapshot), null);
});

test('terminal status maps working, idle, waits, and errors with accessible text', () => {
  const cases = [
    ['working', 'working', '●'],
    ['idle', 'idle', '○'],
    ['awaiting_permission', 'awaiting_permission', '?'],
    ['awaiting_input', 'awaiting_input', '?'],
    ['error', 'error', '!'],
  ];
  for (const [raw, key, symbol] of cases) {
    const status = terminalTabStatus(pane(), { agents: [member(raw)] });
    assert.equal(status.key, key);
    assert.equal(status.symbol, symbol);
    assert.match(status.ariaLabel, new RegExp(key.replace('_', ' ')));
    assert.match(status.title, /one/);
  }
});

test('main-agent-idle and background counts remain visibly working', () => {
  const status = terminalTabStatus(pane(), {
    agents: [member('main_agent_idle', { subagent_count: 1 })],
  });
  assert.equal(status.key, 'working');
  assert.match(status.description, /background activity still running/);
});

test('terminal status distinguishes waking, restarting, crash, exit, and offline', () => {
  const states = [
    [{ ...member('idle'), online: false, waking: true }, 'waking'],
    [{ ...member('working'), online: true, waking: true }, 'working'],
    [{ ...member('idle'), online: false, state: { status: 'idle', recovery_status: 'restarting' } }, 'restarting'],
    [{ ...member('working'), online: false, state: { status: 'working', exit_reason: 'unexpected' } }, 'crashed'],
    [{ ...member('working'), online: false, state: { status: 'working', exit_reason: 'clean' } }, 'offline'],
    [{ ...member('exited'), state: { status: 'exited' } }, 'exited'],
  ];
  for (const [agent, key] of states) {
    assert.equal(terminalTabStatus(pane(), { agents: [agent] }).key, key);
  }
});

test('missing roster data is explicit and never mistaken for idle work', () => {
  const unavailable = terminalTabStatus(pane(), null);
  assert.equal(unavailable.key, 'unknown');
  assert.equal(unavailable.label, 'status unavailable');
  assert.equal(unavailable.symbol, '·');
  assert.match(unavailable.ariaLabel, /status unavailable/);
  assert.ok(TERMINAL_TAB_STATUS_KEYS.includes('awaiting_permission'));
});

test('rendered terminal tabs expose status markers and update from the roster signal', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const [{ createTerminalShellState }, { createTerminalShellActions }, { TerminalTabs }] =
    await Promise.all([
      harness.importDashboardModule('js/terminal-shell-state.js'),
      harness.importDashboardModule('js/terminal-shell-actions.js'),
      harness.importDashboardModule('js/terminal-shell-island.js'),
    ]);
  const state = createTerminalShellState({ persistPresentation: false });
  const actions = createTerminalShellActions({
    state, fetchImpl: async () => ({ ok: true }),
    windowRef: harness.window, documentRef: harness.document,
  });
  const snapshot = harness.signals.signal(null);
  const widgetFactory = (options) => ({
    connect() { options.onStatus('connected'); return Promise.resolve(true); },
    copy() { return Promise.resolve(); },
    fit() {}, focus() {}, setActive() {}, status() { return 'connected'; }, dispose() {},
  });
  const mounted = await harness.mount(harness.html`
    <${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory}
      snapshot=${snapshot} />
  `, host);
  await harness.act(async () => {
    actions.openPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    await Promise.resolve();
  });
  let tab = host.querySelector('[role="tab"]');
  assert.equal(tab.dataset.agentStatus, 'unknown');
  assert.match(tab.getAttribute('aria-label'), /status unavailable/);

  await harness.act(() => {
    snapshot.value = {
      agents: [{
        agent_id: 'agt_one', conv_id: 'conv-one', online: true,
        state: { status: 'awaiting_permission', status_detail: 'needs approval' },
      }],
    };
  });
  tab = host.querySelector('[role="tab"]');
  assert.equal(tab.dataset.agentStatus, 'awaiting_permission');
  assert.equal(tab.querySelector('.mux-tab-status').textContent, '?');
  assert.match(tab.getAttribute('aria-label'), /awaiting permission/);
  assert.match(tab.getAttribute('title'), /needs approval/);

  await mounted.unmount();
  actions.dispose();
});
