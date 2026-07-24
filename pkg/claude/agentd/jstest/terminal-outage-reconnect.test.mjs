// Restarting agentd kills every web terminal's WebSocket while the dashboard
// itself survives and reconnects on its next successful poll. These suites pin
// the repair path: the connection watchdog publishes the disconnected →
// connected EDGE, and the terminal shell answers it with exactly one redial of
// the panes that are sitting disconnected — never a background retry loop, and
// never a terminal somebody may have deliberately reopened elsewhere.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function installHosts(harness) {
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  for (const name of ['groups', 'terminals']) {
    const button = nav.appendChild(harness.document.createElement('a'));
    button.dataset.tab = name;
    button.href = `/${name === 'groups' ? '' : name}`;
    button.textContent = name;
  }
  nav.querySelector('[data-tab="groups"]').classList.add('active');
  const main = harness.document.body.appendChild(harness.document.createElement('main'));
  const groups = main.appendChild(harness.document.createElement('section'));
  groups.id = 'tab-groups';
  groups.classList.add('active');
  const terminals = main.appendChild(harness.document.createElement('section'));
  terminals.id = 'tab-terminals';
  const host = terminals.appendChild(harness.document.createElement('div'));
  host.id = 'terminals-root';
  const badgeHost = nav.appendChild(harness.document.createElement('span'));
  badgeHost.id = 'terminals-badge-root';
  const modalHost = harness.document.body.appendChild(harness.document.createElement('div'));
  modalHost.id = 'terminal-session-root';
  return { host, badgeHost, modalHost };
}

// The fake widget reports whatever status the test last set, so a suite can
// stage the exact mix a restart leaves behind: dead panes, live panes, and
// panes still mid-dial.
function fakeWidgetFactory(harness) {
  const widgets = [];
  const factory = (options) => {
    options.host.append(harness.document.createElement('textarea'));
    const widget = {
      options,
      connectCount: 0,
      disposeCount: 0,
      currentStatus: 'disconnected',
      connect() {
        this.connectCount += 1;
        this.currentStatus = 'connected';
        options.onStatus('connected');
        return Promise.resolve(true);
      },
      copy() { return Promise.resolve(); },
      fit() {},
      focus() {},
      setActive() {},
      status() { return this.currentStatus; },
      isDisposed() { return this.disposeCount > 0; },
      dispose() { this.disposeCount += 1; },
    };
    widgets.push(widget);
    return widget;
  };
  return { factory, widgets };
}

test('the watchdog announces the reconnect edge, not every successful poll', async (t) => {
  const harness = await createPreactHarness(t);
  const connection = await harness.importDashboardModule('js/connection.js');

  const restored = [];
  const unsubscribe = connection.onConnectionRestored(() => restored.push('a'));
  connection.onConnectionRestored(() => { throw new Error('a rude listener'); });
  connection.onConnectionRestored(() => restored.push('b'));

  // A single refused poll is only "retrying": no banner was raised, so there is
  // no outage to recover from and nothing must fire.
  connection.noteDisconnected();
  assert.equal(connection.isDisconnected(), false);
  connection.noteConnected();
  assert.deepEqual(restored, [], 'a transient blip is not an outage');

  // Two refused polls in a row is the real thing.
  connection.noteDisconnected();
  connection.noteDisconnected();
  assert.equal(connection.isDisconnected(), true);
  connection.noteConnected();
  assert.deepEqual(restored, ['a', 'b'],
    'every listener runs on the recovery edge, even after one of them throws');

  connection.noteConnected();
  connection.noteConnected();
  assert.deepEqual(restored, ['a', 'b'], 'later healthy polls are not fresh recoveries');

  unsubscribe();
  connection.noteDisconnected();
  connection.noteDisconnected();
  connection.noteConnected();
  assert.deepEqual(restored, ['a', 'b', 'b'], 'an unsubscribed listener stays quiet');
});

test('an agentd outage redials the dead panes once when the dashboard comes back', async (t) => {
  const harness = await createPreactHarness(t);
  const { host } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');

  let announce = null;
  let unsubscribed = 0;
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    confirm: async () => true,
    onConnectionRestored: (listener) => {
      announce = listener;
      return () => { unsubscribed += 1; };
    },
  });

  for (const key of ['one', 'two', 'three']) {
    await harness.act(async () => {
      controller.openTerminalPane({ ws: `/${key}`, key, label: key, agent: `agt_${key}` });
      await Promise.resolve();
    });
  }
  const [dead, alive, dialing] = fake.widgets;
  assert.deepEqual(fake.widgets.map((widget) => widget.connectCount), [1, 1, 1],
    'mounting a pane dials it once');

  // agentd restarts: this pane's socket closed and settled at disconnected,
  // one pane survived (or was reopened elsewhere), and one is still mid-dial.
  await harness.act(() => {
    dead.currentStatus = 'disconnected';
    dead.options.onStatus('disconnected');
    dead.options.onReconnectChange(true);
    dialing.currentStatus = 'connecting…';
  });

  await harness.act(() => announce());
  assert.equal(dead.connectCount, 2, 'the dead pane is redialed');
  assert.equal(alive.connectCount, 1, 'a live terminal is left alone');
  assert.equal(dialing.connectCount, 1, 'a dial already in flight is not raced');

  // One attempt per outage: the redial failing right back to disconnected must
  // not turn into a retry loop, and a later healthy poll is not a new outage.
  await harness.act(() => {
    dead.currentStatus = 'disconnected';
    dead.options.onStatus('disconnected');
    dead.options.onReconnectChange(true);
  });
  assert.equal(dead.connectCount, 2, 'nothing retries on its own between outages');

  // The explicit control still works, and the next outage gets its own attempt.
  harness.fireEvent(getByRole(host, 'button', { name: 'Reconnect' }), 'click');
  assert.equal(dead.connectCount, 3, 'the operator can always redial by hand');
  await harness.act(() => {
    dead.currentStatus = 'disconnected';
    dead.options.onStatus('disconnected');
  });
  await harness.act(() => announce());
  assert.equal(dead.connectCount, 4, 'a second outage earns a second attempt');

  cleanup();
  assert.equal(unsubscribed, 1, 'the feature releases the watchdog subscription');
});
