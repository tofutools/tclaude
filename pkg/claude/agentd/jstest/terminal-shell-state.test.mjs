import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  const writes = [];
  return {
    writes,
    getItem(key) { return values.get(key) ?? null; },
    setItem(key, value) { values.set(key, String(value)); writes.push([key, String(value)]); },
    removeItem(key) { values.delete(key); },
  };
}

test('terminal shell state owns stable pane, active, reveal, and modal descriptors', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTerminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const state = createTerminalShellState();

  assert.equal(state.openPane(null), null);
  assert.equal(state.openPane({ ws: 'https://elsewhere.test/socket' }), null);
  const first = state.openPane({ ws: '/one', key: 'agent:one', label: 'one', agent: 'agt_one' });
  assert.equal(first.id, 'terminal-pane-1');
  assert.equal(state.activeKey.value, 'agent:one');
  assert.equal(state.view.value.count, 1);
  const revealAfterFirst = state.revealRequest.value;

  const duplicate = state.openPane({ ws: '/replacement', key: 'agent:one', label: 'ignored' });
  assert.equal(duplicate, first, 'a duplicate key focuses instead of replacing the live descriptor');
  assert.equal(state.panes.value[0].seed.ws, '/one');
  assert.equal(state.revealRequest.value, revealAfterFirst + 1);

  const second = state.openPane({ ws: '/two', key: 'agent:two', label: 'two', agent: 'agt_two' });
  assert.equal(state.activeKey.value, second.key);
  const revealBeforeBackgroundFocus = state.revealRequest.value;
  assert.equal(state.openPane({ ws: '/ignored', key: 'agent:one' }, { reveal: false }), first,
    'a background duplicate selects the existing pane');
  assert.equal(state.activeKey.value, first.key);
  assert.equal(state.revealRequest.value, revealBeforeBackgroundFocus,
    'background duplicate selection does not request a tab reveal');
  const background = state.openPane({
    ws: '/background', key: 'agent:background', label: 'background', agent: 'agt_background',
  }, { reveal: false });
  assert.equal(state.activeKey.value, background.key);
  assert.equal(state.view.value.count, 3, 'a background request still creates its pane');
  assert.equal(state.revealRequest.value, revealBeforeBackgroundFocus,
    'opening a new background pane does not request a tab reveal');
  assert.equal(state.activatePane(first.key), true);
  assert.equal(state.activeKey.value, first.key);
  assert.equal(state.removePane(first.key), first);
  assert.equal(state.activeKey.value, second.key, 'closing the active pane activates the next stable descriptor');
  assert.equal(state.removePane('missing'), null);
  assert.equal(state.findPaneKey(['agt_two']), second.key);
  assert.equal(state.findPaneKey(['agt_missing']), null);

  const modalOne = state.openModal({ wsPath: '/modal-one', label: 'first', hideConv: 'agt_one' });
  const modalTwo = state.openModal({ wsPath: '/modal-two', label: 'second' });
  assert.notEqual(modalOne.id, modalTwo.id, 'replacement gets a new lifecycle generation');
  assert.equal(state.closeModal(modalOne.id), null, 'a stale modal generation cannot close its successor');
  assert.equal(state.closeModal(modalTwo.id), modalTwo);

  state.dispose();
  assert.deepEqual(state.panes.value, []);
  assert.equal(state.activeKey.value, null);
  assert.equal(state.modal.value, null);
});

test('terminal pane order persists, restores known keys, appends new keys, and never changes active identity', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTerminalShellState, TERMINAL_PANE_ORDER_KEY } =
    await harness.importDashboardModule('js/terminal-shell-state.js');
  const prefs = memoryPrefs({
    'tclaude.dash.terminals.order': JSON.stringify(['two', 'one', 'closed']),
  });
  const state = createTerminalShellState({ prefs });

  const one = state.openPane({ ws: '/one', key: 'one', label: 'one' });
  const two = state.openPane({ ws: '/two', key: 'two', label: 'two' });
  const three = state.openPane({ ws: '/three', key: 'three', label: 'three' });
  assert.deepEqual(state.panes.value.map((pane) => pane.key), ['two', 'one', 'three'],
    'known keys use remembered order and a genuinely new key appends');
  assert.equal(state.activeKey.value, 'three');
  assert.equal(prefs.writes.length, 0,
    'ordinary opens cannot overwrite an explicit order written by another dashboard client');

  const moved = state.reorderPane('three', 'two');
  assert.deepEqual(state.panes.value.map((pane) => pane.key), ['three', 'two', 'one']);
  assert.deepEqual(moved, { pane: three, index: 0, count: 3 });
  assert.equal(state.activeKey.value, 'three', 'moving the active pane never switches terminals');
  assert.equal(state.panes.value.find((pane) => pane.key === 'one'), one,
    'reordering preserves stable pane descriptors');
  assert.equal(state.panes.value.find((pane) => pane.key === 'two'), two);
  assert.deepEqual(JSON.parse(prefs.getItem(TERMINAL_PANE_ORDER_KEY)),
    ['three', 'two', 'one', 'closed'], 'closed remembered keys remain behind the visible order');

  state.movePaneByOffset('three', 1);
  assert.deepEqual(state.panes.value.map((pane) => pane.key), ['two', 'three', 'one']);
  assert.equal(state.activeKey.value, 'three');
  assert.equal(state.movePaneByOffset('two', -1), null, 'moving past the first position is inert');

  state.removePane('three');
  assert.deepEqual(state.panes.value.map((pane) => pane.key), ['two', 'one']);
  assert.equal(state.activeKey.value, 'two', 'closing the active pane follows the existing activation rule');
  const reopened = state.openPane({ ws: '/three-again', key: 'three', label: 'three again' });
  assert.deepEqual(state.panes.value.map((pane) => pane.key), ['two', 'three', 'one'],
    'reopening a known key restores its remembered relative position');
  assert.equal(state.activeKey.value, 'three');
  assert.notEqual(reopened, three, 'closing and reopening still creates a fresh terminal lifecycle');
});

test('terminal order retention is bounded and solo shells never persist dashboard order', async (t) => {
  const harness = await createPreactHarness(t);
  const {
    createTerminalShellState, MAX_REMEMBERED_TERMINAL_PANES, MAX_TERMINAL_PANE_ORDER_BYTES,
    TERMINAL_PANE_ORDER_KEY,
  } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const remembered = Array.from({ length: MAX_REMEMBERED_TERMINAL_PANES + 80 }, (_, index) => `key-${index}`);
  const prefs = memoryPrefs({ [TERMINAL_PANE_ORDER_KEY]: JSON.stringify(remembered) });
  const state = createTerminalShellState({ prefs });
  state.openPane({ ws: '/first', key: 'key-0' });
  state.openPane({ ws: '/last', key: remembered.at(-1) });
  state.reorderPane(remembered.at(-1), 'key-0');
  assert.equal(JSON.parse(prefs.getItem(TERMINAL_PANE_ORDER_KEY)).length,
    MAX_REMEMBERED_TERMINAL_PANES, 'the persisted preference cannot grow without bound');

  const longKeys = Array.from({ length: MAX_REMEMBERED_TERMINAL_PANES },
    (_, index) => `long-${index}-${'å'.repeat(180)}`);
  const longPrefs = memoryPrefs({ [TERMINAL_PANE_ORDER_KEY]: JSON.stringify(longKeys) });
  const longState = createTerminalShellState({ prefs: longPrefs });
  longState.openPane({ ws: '/long-one', key: longKeys[0] });
  longState.openPane({ ws: '/long-two', key: longKeys[1] });
  longState.reorderPane(longKeys[1], longKeys[0]);
  const persistedBytes = new TextEncoder()
    .encode(longPrefs.getItem(TERMINAL_PANE_ORDER_KEY)).byteLength;
  assert.ok(persistedBytes <= MAX_TERMINAL_PANE_ORDER_BYTES,
    `persisted order uses ${persistedBytes} bytes, above ${MAX_TERMINAL_PANE_ORDER_BYTES}`);
  assert.ok(JSON.parse(longPrefs.getItem(TERMINAL_PANE_ORDER_KEY)).length
    < MAX_REMEMBERED_TERMINAL_PANES, 'byte retention is independent of the entry-count cap');

  const soloPrefs = memoryPrefs({ [TERMINAL_PANE_ORDER_KEY]: JSON.stringify(['dashboard-pane']) });
  const solo = createTerminalShellState({ prefs: soloPrefs, persistOrder: false });
  solo.openPane({ ws: '/solo-one', key: 'solo-one' });
  solo.openPane({ ws: '/solo-two', key: 'solo-two' });
  solo.reorderPane('solo-two', 'solo-one');
  assert.deepEqual(soloPrefs.writes, [], 'a standalone pop-out never writes dashboard tab order');
});

function fakeWidget() {
  let disposed = false;
  return {
    disposeCount: 0,
    connectCount: 0,
    currentStatus: 'connected',
    dispose() { if (!disposed) { disposed = true; this.disposeCount += 1; } },
    connect() { this.connectCount += 1; },
    status() { return this.currentStatus; },
  };
}

test('terminal actions sequence socket disposal, detach, pop-out, and external hide safely', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTerminalShellState }, { createTerminalShellActions }] = await Promise.all([
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-shell-actions.js'),
  ]);
  const state = createTerminalShellState();
  const requests = [];
  const opened = [];
  const actions = createTerminalShellActions({
    state,
    fetchImpl: async (url, options) => { requests.push([url, options]); return { ok: true }; },
    windowRef: {
      open(url, target) {
        const tab = { opened: [url, target], location: { replace: (next) => opened.push(next) } };
        opened.push(tab);
        return tab;
      },
    },
    documentRef: harness.document,
  });

  const live = actions.openPane({ ws: '/live', key: 'live', label: 'live', hideConv: 'agt_live', agent: 'agt_live' });
  const liveWidget = fakeWidget();
  actions.registerWidget(live.id, liveWidget);
  await actions.closePane(live.key);
  assert.equal(liveWidget.disposeCount, 1);
  assert.equal(state.panes.value.length, 0, 'pane chrome closes synchronously');
  assert.equal(requests[0][0], '/api/hide/agt_live');

  const external = actions.openPane({ ws: '/external', key: 'external', hideConv: 'agt_external' });
  const externalWidget = fakeWidget();
  actions.registerWidget(external.id, externalWidget);
  actions.closeForHide(['agt_external']);
  await Promise.resolve();
  assert.equal(externalWidget.disposeCount, 1);
  assert.equal(requests.length, 1, 'an external hide never repeats the server detach');

  const stale = actions.openPane({ ws: '/stale', key: 'same', hideConv: 'agt_same' });
  const staleWidget = fakeWidget();
  actions.registerWidget(stale.id, staleWidget);
  const replacement = await actions.receiveHandoffPane({
    ws: '/replacement', key: 'same', hideConv: 'agt_same', initialRetry: true,
  });
  assert.equal(staleWidget.disposeCount, 1, 'a duplicate handoff replaces its stale widget');
  assert.notEqual(replacement.id, stale.id);
  assert.equal(replacement.seed.ws, '/replacement');
  assert.equal(replacement.seed.initialRetry, true);
  assert.equal(requests.length, 1, 'replacing a handoff duplicate does not detach the session again');

  const popped = actions.openPane({ ws: '/pop', key: 'pop', label: 'pop', hideConv: 'agt_pop' });
  const poppedWidget = fakeWidget();
  actions.registerWidget(popped.id, poppedWidget);
  await actions.popOutPane(popped.key);
  assert.equal(poppedWidget.disposeCount, 1);
  assert.equal(requests.at(-1)[0], '/api/hide/agt_pop');
  assert.match(opened.at(-1), /^\/terminals\?solo=1#open=/);
  const payload = JSON.parse(decodeURIComponent(opened.at(-1).split('#open=')[1]));
  assert.equal(payload.hideConv, 'agt_pop');

  actions.dispose();
  actions.dispose();
});

test('modal confirmation keeps reconnect and close mutually exclusive and preserves view semantics', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createTerminalShellState }, { createTerminalShellActions }] = await Promise.all([
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-shell-actions.js'),
  ]);
  const state = createTerminalShellState();
  const confirmations = [];
  const pending = [];
  const requests = [];
  const actions = createTerminalShellActions({
    state,
    confirm: (options) => {
      confirmations.push(options);
      return new Promise((resolve) => pending.push(resolve));
    },
    fetchImpl: async (url) => { requests.push(url); return { ok: true }; },
    documentRef: harness.document,
    windowRef: harness.window,
  });

  const modal = actions.openModal({ wsPath: '/modal', label: 'live', hideConv: 'agt_live' });
  const widget = fakeWidget();
  actions.registerWidget(modal.id, widget);
  const closing = actions.confirmModalClose(modal.id);
  actions.onModalDisconnect(modal.id);
  assert.equal(confirmations.length, 1, 'disconnect cannot stack over the close confirmation');
  assert.equal(confirmations[0].okLabel, 'Detach');
  widget.currentStatus = 'disconnected';
  pending.shift()(false);
  await closing;
  await Promise.resolve();
  assert.equal(confirmations.length, 2, 'keeping a dropped terminal re-offers reconnect after the close prompt');
  assert.equal(confirmations[1].okLabel, 'Reconnect');
  pending.shift()(true);
  await Promise.resolve();
  assert.equal(widget.connectCount, 1);

  await actions.detachModal(modal.id);
  assert.equal(widget.disposeCount, 1);
  assert.deepEqual(requests, ['/api/hide/agt_live']);

  const throwaway = actions.openModal({ wsPath: '/throwaway', label: 'scratch' });
  const throwawayWidget = fakeWidget();
  actions.registerWidget(throwaway.id, throwawayWidget);
  const throwawayClose = actions.confirmModalClose(throwaway.id);
  assert.equal(confirmations.at(-1).okLabel, 'Close terminal');
  pending.shift()(true);
  await throwawayClose;
  assert.equal(throwawayWidget.disposeCount, 1);
  assert.equal(requests.length, 1, 'a throwaway terminal never hides the agent live session');
});
