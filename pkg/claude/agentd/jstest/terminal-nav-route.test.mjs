// terminal-nav-route.test.mjs — the Terminals tab's half of path routing
// (/terminals/<agent-id>).
//
// This suite drives the real module (js/terminal-nav-route.js) against a real
// terminal shell state, with only the pane LAUNCHER faked — opening a pane for
// real would need the xterm runtime and a live WebSocket. What is under test is
// the contract with the router: which announcements are navigations, which are
// corrections, and what happens when the agent a URL names is gone.
//
//   node --test pkg/claude/agentd/jstest/terminal-nav-route.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_alpha';
const OTHER = 'agt_beta';

function seedFor(agent) {
  return { ws: `/api/open-window-ws/${agent}`, key: `window:${agent}`, agent, label: agent };
}

// setup builds a fresh state + binding and records every location event the
// router would see, so assertions read as "what did the tab tell the router".
async function setup(t, { openAgentPane, snapshot = null } = {}) {
  const harness = await createPreactHarness(t);
  const { createTerminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const { bindTerminalNavRouting } = await harness.importDashboardModule('js/terminal-nav-route.js');
  // persistPresentation off: tab-group prefs are irrelevant here and would
  // leak state between tests through the shared prefs store.
  const state = createTerminalShellState({ persistPresentation: false });
  const announced = [];
  const doc = harness.document;
  const onNavigated = (event) => announced.push(event.detail);
  doc.addEventListener('tclaude:navigated', onNavigated);
  let left = 0;
  const opened = [];
  const unbind = bindTerminalNavRouting({
    state,
    snapshot,
    documentRef: doc,
    leaveTab: () => { left += 1; },
    openAgentPane: openAgentPane || ((agent, label) => {
      opened.push({ agent, label });
      return state.openPane(seedFor(agent));
    }),
  });
  t.after(() => {
    unbind();
    doc.removeEventListener('tclaude:navigated', onNavigated);
    state.dispose();
  });
  const restore = (location) => {
    doc.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', {
      detail: { location },
    }));
    // The restore path is async (reattach may await a runtime load); let the
    // microtask queue drain before asserting.
    return new Promise((resolve) => setTimeout(resolve, 0));
  };
  return {
    state, announced, restore, unbind, doc, opened, left: () => left, harness,
  };
}

test('a hard refresh on /terminals/<agent-id> reattaches that agent', async (t) => {
  const { state, restore, opened, announced } = await setup(t);
  // The cold-load reality: nothing is open, because panes do not survive a
  // reload. The URL alone has to be enough to bring the terminal back.
  assert.equal(state.panes.value.length, 0);

  await restore({ tab: 'terminals', selection: AGENT });

  assert.deepEqual(opened.map((o) => o.agent), [AGENT]);
  assert.equal(state.panes.value.length, 1);
  assert.equal(state.view.value.activeKey, `window:${AGENT}`);
  // Restoring is the browser arriving where it already is — it must never be
  // announced as a fresh navigation, or it would push a duplicate entry for a
  // move the operator never made.
  assert.deepEqual(announced, []);
});

test('a deep link to an already-open terminal activates it instead of reopening', async (t) => {
  const { state, restore, opened, announced } = await setup(t);
  state.openPane(seedFor(AGENT));
  state.openPane(seedFor(OTHER));
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(state.view.value.activeKey, `window:${OTHER}`);
  announced.length = 0; // those two opens were real navigations; this test is about what follows

  await restore({ tab: 'terminals', selection: AGENT });

  assert.equal(state.view.value.activeKey, `window:${AGENT}`);
  assert.equal(state.panes.value.length, 2, 'no duplicate pane');
  assert.deepEqual(opened, [], 'no reattach needed');
  assert.deepEqual(announced, [], 'a restore is not a navigation');
});

test('switching terminals announces the new agent as a navigation', async (t) => {
  const { state, announced } = await setup(t);
  state.openPane(seedFor(AGENT));
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(announced.map((a) => a.location.selection), [AGENT]);

  state.openPane(seedFor(OTHER));
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.deepEqual(announced.map((a) => a.location.selection), [AGENT, OTHER]);
  assert.ok(announced.every((a) => !a.correction), 'operator moves are navigations, not corrections');
  assert.ok(announced.every((a) => a.location.tab === 'terminals'));
});

test('a dead deep link with other terminals open corrects to the live one', async (t) => {
  // Reattach refuses (the agent is retired). Other panes are open, so the tab
  // is still somewhere to be — the URL must be corrected, never pushed.
  const { state, restore, announced, left } = await setup(t, {
    openAgentPane: () => null,
  });
  state.openPane(seedFor(OTHER));
  await new Promise((resolve) => setTimeout(resolve, 0));
  announced.length = 0;

  await restore({ tab: 'terminals', selection: 'agt_retired' });

  assert.equal(announced.length, 1);
  assert.equal(announced[0].correction, true, 'a refused restore corrects, it does not push');
  assert.equal(announced[0].location.selection, OTHER, 'corrected to the pane we are actually on');
  assert.equal(left(), 0, 'the tab still has something to show');
});

test('a dead deep link with nothing open leaves the tab', async (t) => {
  // The cold case: no panes at all, so the tab is hidden and there is nothing
  // to correct TO. Leaving is the only honest outcome.
  const { restore, announced, left, state } = await setup(t, {
    openAgentPane: () => null,
  });

  await restore({ tab: 'terminals', selection: 'agt_retired' });

  assert.equal(state.panes.value.length, 0);
  assert.equal(left(), 1);
  // The leave is an ordinary nav click that the router records by itself;
  // announcing it too would double-count the move.
  assert.deepEqual(announced, []);
});

test('a restore that throws is contained and treated as a refusal', async (t) => {
  const { restore, left } = await setup(t, {
    openAgentPane: () => { throw new Error('socket refused'); },
  });

  await restore({ tab: 'terminals', selection: AGENT });

  assert.equal(left(), 1, 'a thrown reattach must not wedge the router');
});

test('the restored label comes from the snapshot roster, not the URL', async (t) => {
  // Only the agent id travels in the URL, so a renamed agent deep-links to its
  // CURRENT name. The roster is keyed by agent_id or conv_id, matching the
  // selector the row actions route by.
  const snapshot = {
    value: {
      agents: [
        { agent_id: AGENT, conv_id: 'conv-1', title: 'lead' },
        { conv_id: 'conv-legacy', title: 'pre-identity agent' },
      ],
    },
  };
  const { restore, opened } = await setup(t, { snapshot });

  await restore({ tab: 'terminals', selection: AGENT });
  await restore({ tab: 'terminals', selection: 'conv-legacy' });
  await restore({ tab: 'terminals', selection: 'agt_unknown' });

  assert.deepEqual(opened, [
    { agent: AGENT, label: 'lead' },
    { agent: 'conv-legacy', label: 'pre-identity agent' },
    // Unknown to the roster: fall back to the selector itself rather than
    // rendering an empty tab.
    { agent: 'agt_unknown', label: 'agt_unknown' },
  ]);
});

test('a bare /terminals restore does not disturb the active pane', async (t) => {
  const { state, restore, opened, announced } = await setup(t);
  state.openPane(seedFor(AGENT));
  state.openPane(seedFor(OTHER));
  await new Promise((resolve) => setTimeout(resolve, 0));
  announced.length = 0;

  await restore({ tab: 'terminals' });

  assert.equal(state.view.value.activeKey, `window:${OTHER}`);
  assert.deepEqual(opened, []);
  assert.deepEqual(announced, []);
});

test('a restore aimed at another tab is ignored', async (t) => {
  const { restore, opened, left } = await setup(t);
  await restore({ tab: 'processes', subtab: 'templates', selection: 'tmpl-1' });
  assert.deepEqual(opened, []);
  assert.equal(left(), 0);
});

test('unbinding stops both announcing and restoring', async (t) => {
  const { state, restore, announced, unbind, opened } = await setup(t);
  unbind();

  state.openPane(seedFor(AGENT));
  await new Promise((resolve) => setTimeout(resolve, 0));
  await restore({ tab: 'terminals', selection: OTHER });

  assert.deepEqual(announced, []);
  assert.deepEqual(opened, []);
});

test('a pane without an agent is not addressable', async (t) => {
  // Group terminals and hand-fed seeds carry no agent, so there is no stable id
  // to put in the URL. The location degrades to the bare tab rather than
  // inventing a second id space.
  const { state, announced } = await setup(t);
  state.openPane({ ws: '/api/group-term-ws/tclaude', key: 'groupterm:tclaude', label: 'tclaude' });
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.deepEqual(state.location.value, { tab: 'terminals' });
  assert.deepEqual(announced, [], 'nothing to announce for an unaddressable pane');
});
