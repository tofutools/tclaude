import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

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
