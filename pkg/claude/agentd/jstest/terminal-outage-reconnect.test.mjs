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

// The fake widget mirrors the real adapter's two reported signals so a suite
// can stage the exact mix an outage leaves behind: settled-dead panes, live
// panes, and panes still mid-dial. Like the real one it clears
// reconnectAvailable for the whole of a dial and never lets a transient status
// message (a 'copied' flash) imply anything about the connection.
function fakeWidgetFactory(harness) {
  const widgets = [];
  const factory = (options) => {
    options.host.append(harness.document.createElement('textarea'));
    const widget = {
      options,
      connectCount: 0,
      disposeCount: 0,
      currentStatus: 'disconnected',
      offersReconnect: false,
      connect() {
        this.connectCount += 1;
        this.settle('connected', false);
        return Promise.resolve(true);
      },
      // settle drives both signals the way the adapter does, so no suite can
      // stage a state the production widget could never report.
      settle(status, reconnect) {
        this.currentStatus = status;
        this.offersReconnect = reconnect;
        options.onStatus(status);
        options.onReconnectChange(reconnect);
      },
      copy() { return Promise.resolve(); },
      fit() {},
      focus() {},
      setActive() {},
      status() { return this.currentStatus; },
      reconnectAvailable() { return this.offersReconnect; },
      isDisposed() { return this.disposeCount > 0; },
      // Repeat-safe, like the real adapter: an explicit close and a component
      // unmount converge on the same widget.
      dispose() { if (this.disposeCount === 0) this.disposeCount = 1; },
    };
    widgets.push(widget);
    return widget;
  };
  return { factory, widgets };
}
test('a refused poll followed by a healthy one is the recovery edge', async (t) => {
  const harness = await createPreactHarness(t);
  const connection = await harness.importDashboardModule('js/connection.js');

  const restored = [];
  const unsubscribe = connection.onConnectionRestored(() => restored.push('a'));
  connection.onConnectionRestored(() => { throw new Error('a rude listener'); });
  connection.onConnectionRestored(() => restored.push('b'));

  connection.noteConnected();
  connection.noteConnected();
  assert.deepEqual(restored, [], 'a page that never lost agentd notifies nobody');

  // ONE refused poll is enough. The two-failure threshold is the banner's, and
  // waiting for it would miss most restarts: the poll runs every 2s visible and
  // every 10s hidden, so a quick restart is often seen by a single tick.
  connection.noteDisconnected();
  assert.equal(connection.isDisconnected(), false, 'one failure is still below the banner');
  connection.noteConnected();
  assert.deepEqual(restored, ['a', 'b'],
    'every listener runs on the recovery edge, even after one of them throws');

  connection.noteConnected();
  assert.deepEqual(restored, ['a', 'b'], 'later healthy polls are not fresh recoveries');

  // A long outage that does raise the banner still resolves to exactly one edge.
  connection.noteDisconnected();
  connection.noteDisconnected();
  connection.noteDisconnected();
  assert.equal(connection.isDisconnected(), true);
  connection.noteConnected();
  assert.deepEqual(restored, ['a', 'b', 'a', 'b'], 'one edge per outage, however long');

  unsubscribe();
  connection.noteDisconnected();
  connection.noteConnected();
  assert.deepEqual(restored, ['a', 'b', 'a', 'b', 'b'], 'an unsubscribed listener stays quiet');
});

test('a changed daemon instance id is a restart even when no poll was refused', async (t) => {
  const harness = await createPreactHarness(t);
  const connection = await harness.importDashboardModule('js/connection.js');

  const restored = [];
  connection.onConnectionRestored(() => restored.push('restored'));

  // This is the case the refused-poll path cannot see: agentd is restarted
  // between two successful polls — trivially possible with the hidden tab's
  // 10s cadence — so every terminal socket died without the page noticing.
  assert.equal(connection.noteServerIdentity('agentd-1'), false, 'the first id is only a baseline');
  connection.noteConnected();
  assert.equal(connection.noteServerIdentity('agentd-1'), false, 'the same process is not a restart');
  connection.noteConnected();
  assert.deepEqual(restored, []);

  assert.equal(connection.noteServerIdentity('agentd-2'), true);
  assert.deepEqual(restored, ['restored'], 'a new process is a restart, banner or not');
  connection.noteConnected();
  assert.deepEqual(restored, ['restored'], 'the polls that follow it are not further recoveries');

  // An agentd too old to publish the field must degrade to the refused-poll
  // path rather than read as an identity change on every single poll.
  assert.equal(connection.noteServerIdentity(undefined), false);
  assert.equal(connection.noteServerIdentity(''), false);
  connection.noteConnected();
  assert.deepEqual(restored, ['restored']);

  // A restart observed BOTH ways — refused polls and then a new id — is still
  // one repair pass, not two.
  connection.noteDisconnected();
  connection.noteDisconnected();
  connection.noteServerIdentity('agentd-3');
  connection.noteConnected();
  assert.deepEqual(restored, ['restored', 'restored'], 'the two signals do not double-fire');
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

  for (const key of ['one', 'two', 'three', 'four']) {
    await harness.act(async () => {
      controller.openTerminalPane({
        ws: `/${key}`, key, label: key, agent: `agt_${key}`, hideConv: `agt_${key}`,
      });
      await Promise.resolve();
    });
  }
  const [dead, alive, dialing, flashing] = fake.widgets;
  assert.deepEqual(fake.widgets.map((widget) => widget.connectCount), [1, 1, 1, 1],
    'mounting a pane dials it once');

  // agentd restarts. One pane settled at disconnected; one survived (or was
  // reopened elsewhere); one is still inside its own dial; and one is dead but
  // showing a copy flash, because the operator grabbed its scrollback while it
  // was down — the status string lies there, the Reconnect offer does not.
  await harness.act(() => {
    dead.settle('disconnected', true);
    dialing.settle('retrying…', false);
    flashing.settle('disconnected', true);
    flashing.currentStatus = 'copied';
  });

  await harness.act(() => announce());
  assert.equal(dead.connectCount, 2, 'the dead pane is redialed');
  assert.equal(flashing.connectCount, 2, 'a transient status message does not disqualify a dead pane');
  assert.equal(alive.connectCount, 1, 'a live terminal is left alone');
  assert.equal(dialing.connectCount, 1, 'a dial already in flight is not raced');

  // One pass per outage: the redial failing right back to disconnected must not
  // turn into a retry loop, and a later healthy poll is not a new outage.
  await harness.act(() => dead.settle('disconnected', true));
  assert.equal(dead.connectCount, 2, 'nothing retries on its own between outages');

  // The explicit control still works, and the next outage gets its own pass.
  harness.fireEvent(getByRole(host, 'button', { name: 'Reconnect' }), 'click');
  assert.equal(dead.connectCount, 3, 'the operator can always redial by hand');
  await harness.act(() => dead.settle('disconnected', true));
  await harness.act(() => announce());
  assert.equal(dead.connectCount, 4, 'a second outage earns a second pass');

  // A disposed widget is inert: closing a pane during the outage must not
  // resurrect it.
  await harness.act(async () => {
    await controller.closeTerminalsForConvs(['agt_one']);
  });
  assert.equal(dead.disposeCount, 1);
  const dialsBeforeClose = dead.connectCount;
  await harness.act(() => announce());
  assert.equal(dead.connectCount, dialsBeforeClose, 'a closed pane is never redialed');

  cleanup();
  assert.equal(unsubscribed, 1, 'the feature releases the watchdog subscription');
});

test('the modal terminal defers to its own disconnect prompt', async (t) => {
  const harness = await createPreactHarness(t);
  const { modalHost } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const { openTermModal } = await harness.importDashboardModule('js/terminals-tab.js');

  let announce = null;
  let answerPrompt = null;
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    // The real confirm renders a dialog and waits for the operator; hold it
    // open so the outage lands while the question is on screen.
    confirm: () => new Promise((resolve) => { answerPrompt = resolve; }),
    onConnectionRestored: (listener) => { announce = listener; return () => {}; },
  });
  await harness.act(() => openTermModal({ wsPath: '/scratch', label: 'scratch' }));
  const [modal] = fake.widgets;
  assert.equal(modal.connectCount, 1);

  await harness.act(async () => {
    modal.settle('disconnected', true);
    modal.options.onDisconnect();
    await Promise.resolve();
  });
  assert.ok(answerPrompt, 'the modal asks the operator what to do with its dead terminal');

  await harness.act(() => announce());
  assert.equal(modal.connectCount, 1,
    'the open prompt owns the decision — the outage pass must not answer it behind the operator');

  await harness.act(async () => {
    answerPrompt(true);
    await Promise.resolve();
    await Promise.resolve();
  });
  assert.equal(modal.connectCount, 2, 'answering Reconnect still reconnects');
  assert.ok(modalHost.querySelector('#term-session-modal'), 'the modal stays open');

  // With the prompt answered, a later outage repairs the modal like any pane.
  await harness.act(() => modal.settle('disconnected', true));
  await harness.act(() => announce());
  assert.equal(modal.connectCount, 3);

  cleanup();
});
