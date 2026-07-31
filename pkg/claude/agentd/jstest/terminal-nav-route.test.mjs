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
async function setup(t, { openAgentPane, snapshot = null, tabActive = true } = {}) {
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
  let onTab = tabActive;
  const opened = [];
  const unbind = bindTerminalNavRouting({
    state,
    snapshot,
    documentRef: doc,
    isTabActive: () => onTab,
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
    state,
    announced,
    restore,
    unbind,
    doc,
    opened,
    left: () => left,
    setTabActive: (next) => { onTab = next; },
    harness,
    settle: () => new Promise((resolve) => setTimeout(resolve, 0)),
  };
}

// A roster the restore will accept — knownAgent refuses a selector the snapshot
// does not list, so tests that expect an attach must supply one (or none at
// all, which means "no snapshot to judge by").
function rosterOf(...agents) {
  return { value: { agents: agents.map((agent) => ({ agent_id: agent, title: agent })) } };
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

test('a retired agent is refused before a pane is ever opened', async (t) => {
  // Opening a pane succeeds for ANY well-formed seed — the socket only 404s
  // later — so the roster is the only thing that can tell a dead deep link from
  // a live one early enough to matter. Without this check the operator would be
  // left staring at a terminal that can never connect.
  const { state, restore, opened, left } = await setup(t, { snapshot: rosterOf(OTHER) });

  await restore({ tab: 'terminals', selection: 'agt_retired' });

  assert.deepEqual(opened, [], 'no pane is opened for an agent the daemon does not know');
  assert.equal(state.panes.value.length, 0);
  assert.equal(left(), 1, 'nothing open and nothing to attach — leave the tab');
});

test('an empty roster attempts the attach rather than refusing everything', async (t) => {
  // An absent snapshot means "nothing to judge by", not "no agents exist".
  // Refusing here would break every deep link on a shell mounted without a poll.
  const { restore, opened } = await setup(t, { snapshot: { value: { agents: [] } } });

  await restore({ tab: 'terminals', selection: AGENT });

  assert.deepEqual(opened.map((o) => o.agent), [AGENT]);
});

test('a dead deep link with other terminals open corrects to the live one', async (t) => {
  // Other panes are open, so the tab is still somewhere to be — the URL must be
  // corrected, never pushed.
  const { state, restore, announced, left, settle } = await setup(t, {
    snapshot: rosterOf(OTHER),
  });
  state.openPane(seedFor(OTHER));
  await settle();
  announced.length = 0;

  await restore({ tab: 'terminals', selection: 'agt_retired' });

  assert.equal(announced.length, 1);
  assert.equal(announced[0].correction, true, 'a refused restore corrects, it does not push');
  assert.equal(announced[0].location.selection, OTHER, 'corrected to the pane we are actually on');
  assert.equal(left(), 0, 'the tab still has something to show');
});

test('a dead cold deep link corrects the URL BEFORE leaving the tab', async (t) => {
  // Order matters. leaveTab clicks a nav button, which the router records as an
  // ordinary navigation. Correcting this entry to the fallback first makes that
  // record a duplicate the router suppresses; the other order pushes a fresh
  // entry and leaves Back bouncing off the dead link forever.
  const { restore, announced, left, state } = await setup(t, { snapshot: rosterOf(OTHER) });

  await restore({ tab: 'terminals', selection: 'agt_retired' });

  assert.equal(state.panes.value.length, 0);
  assert.equal(left(), 1);
  assert.deepEqual(announced, [{ location: { tab: 'groups' }, correction: true }]);
});

test('a restore that throws is contained and treated as a refusal', async (t) => {
  const { restore, left } = await setup(t, {
    snapshot: rosterOf(AGENT),
    openAgentPane: () => { throw new Error('socket refused'); },
  });

  await restore({ tab: 'terminals', selection: AGENT });

  assert.equal(left(), 1, 'a thrown reattach must not wedge the router');
});

test('a background open does not move the address bar', async (t) => {
  // Ctrl/Cmd-clicking "focus" collects an agent's terminal while the operator
  // stays on Groups. openPane sets activeKey regardless of reveal, so without a
  // tab-active gate this would push a history entry AND rewrite the URL to a
  // terminal that is not on screen — so the next hard refresh would land
  // somewhere the operator never was, defeating the whole feature.
  const { state, announced, settle } = await setup(t, { tabActive: false });

  state.openPane(seedFor(AGENT), { reveal: false });
  state.openPane(seedFor(OTHER), { reveal: false });
  await settle();

  assert.equal(state.panes.value.length, 2, 'the panes are still collected');
  assert.deepEqual(announced, [], 'but none of it is the operator navigating');
});

test('an involuntary pane close corrects rather than pushes', async (t) => {
  // An agent retires and the snapshot poll closes its pane; an heir is elected.
  // The URL should track the heir — but the operator never asked to go there,
  // so a push would put a terminal they never chose into their Back history.
  const { state, announced, settle } = await setup(t);
  state.openPane(seedFor(AGENT));
  state.openPane(seedFor(OTHER));
  await settle();
  announced.length = 0;

  state.removePane(`window:${OTHER}`); // the pane being viewed vanishes underneath them
  await settle();

  assert.equal(announced.length, 1);
  assert.equal(announced[0].location.selection, AGENT, 'the URL tracks the heir');
  assert.equal(announced[0].correction, true, 'but as a correction, not a navigation');
});

test('closing a pane the operator is NOT on is not a location change at all', async (t) => {
  const { state, announced, settle } = await setup(t);
  state.openPane(seedFor(AGENT));
  state.openPane(seedFor(OTHER));
  await settle();
  announced.length = 0;

  state.removePane(`window:${AGENT}`); // not the active one
  await settle();

  assert.deepEqual(announced, []);
});

test('a mid-flight operator switch is announced, not swallowed', async (t) => {
  // A cold restore waits on the xterm runtime — a network fetch. If the
  // operator switches terminals during that window, suppressing the
  // announcement would leave the URL naming the restored agent while a
  // different one is on screen, with nothing to ever reconcile it.
  const THIRD = 'agt_gamma';
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  let state;
  const bound = await setup(t, {
    snapshot: rosterOf(AGENT, OTHER, THIRD),
    openAgentPane: async (agent) => {
      await gate;
      return state.openPane(seedFor(agent));
    },
  });
  ({ state } = bound);
  const { restore, announced, settle } = bound;
  state.openPane(seedFor(OTHER));
  state.openPane(seedFor(THIRD));
  await settle();
  announced.length = 0;

  const inFlight = restore({ tab: 'terminals', selection: AGENT });
  // The operator moves to a DIFFERENT already-open terminal while the reattach
  // is still pending.
  state.activatePane(`window:${OTHER}`);
  release();
  await inFlight;
  await settle();

  assert.ok(announced.length >= 1, 'the operator move must reach the router');
  const last = announced[announced.length - 1];
  assert.equal(last.location.selection, OTHER);
  assert.equal(last.correction, false, 'they really did navigate');
});

test('an overlapping restore lets the newest win', async (t) => {
  // Two quick Back presses. The older restore resolving late must not drag the
  // tab back to a location the browser has already left.
  const { state, restore, announced, settle } = await setup(t, {
    snapshot: rosterOf(AGENT, OTHER),
  });

  const first = restore({ tab: 'terminals', selection: AGENT });
  const second = restore({ tab: 'terminals', selection: OTHER });
  await Promise.all([first, second]);
  await settle();

  assert.equal(state.view.value.activeKey, `window:${OTHER}`);
  assert.deepEqual(announced, [], 'neither restore is a navigation');
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

  assert.deepEqual(opened, [
    { agent: AGENT, label: 'lead' },
    { agent: 'conv-legacy', label: 'pre-identity agent' },
  ]);
});

test('with no roster to judge by, the label falls back to the selector', async (t) => {
  // The attach is attempted (see the empty-roster test above), so it still
  // needs a label — the bare selector rather than an empty tab.
  const { restore, opened } = await setup(t, { snapshot: { value: { agents: [] } } });

  await restore({ tab: 'terminals', selection: 'agt_unlisted' });

  assert.deepEqual(opened, [{ agent: 'agt_unlisted', label: 'agt_unlisted' }]);
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
