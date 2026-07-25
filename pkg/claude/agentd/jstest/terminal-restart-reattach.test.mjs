// The widget half of restart repair: a terminal remembers which agentd its
// socket was attached to, and when that socket settles dead it asks to be told
// if the daemon is ever a different process. Nothing else redials it.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function widgetFakes(document) {
  const sockets = [];
  class FakeFitAddon {
    fit() {}
    dispose() {}
  }
  class FakeTerminal {
    constructor() { this.cols = 80; this.rows = 24; }
    loadAddon() {}
    open(host) { host.append(document.createElement('textarea')); }
    onData() { return { dispose() {} }; }
    onResize() { return { dispose() {} }; }
    focus() {}
    write() {}
    dispose() {}
  }
  class FakeWebSocket {
    static OPEN = 1;
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      sockets.push(this);
    }
    open() { this.readyState = FakeWebSocket.OPEN; this.onopen?.(); }
    // How agentd going away looks from here: the socket simply closes.
    drop() { this.readyState = 3; this.onclose?.(); }
    send() {}
    close() { this.readyState = 3; }
  }
  const interactionsFactory = () => ({ copySelection: async () => {}, dispose() {} });
  return { sockets, FakeFitAddon, FakeTerminal, FakeWebSocket, interactionsFactory };
}

// A stand-in for instance-watch.js, so this suite tests the widget's use of the
// contract rather than the watcher's own polling (covered in instance-watch).
function recordingWatcher(id = 'agentd-1') {
  const watches = [];
  return {
    currentID: () => Promise.resolve(watches.currentID ?? id),
    watchForRestart(baseline, onRestart) {
      const entry = { baseline, onRestart, cancelled: 0 };
      watches.push(entry);
      return () => { entry.cancelled += 1; };
    },
    watches,
  };
}

async function mountWidget(harness, { watcher, fetchImpl, ...overrides } = {}) {
  const { mountTerminalWidget } = await harness.importDashboardModule('js/terminals-core.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const fakes = widgetFakes(harness.document);
  const statuses = [];
  const widget = mountTerminalWidget({
    host,
    wsPath: '/api/term-ws/agt_one',
    onStatus: (value) => statuses.push(value),
    fetchImpl: fetchImpl || (async () => ({ ok: true })),
    restartWatcher: watcher,
    TerminalCtor: fakes.FakeTerminal,
    FitAddonCtor: fakes.FakeFitAddon,
    WebSocketCtor: fakes.FakeWebSocket,
    locationRef: { protocol: 'https:', host: 'dashboard.test' },
    documentRef: harness.document,
    interactionsFactory: fakes.interactionsFactory,
    ...overrides,
  });
  return { widget, fakes, statuses };
}

const settle = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };

test('a dead terminal waits for a new daemon, then reattaches once', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes, statuses } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  assert.equal(statuses.at(-1), 'connected');
  assert.equal(watcher.watches.length, 0, 'a healthy terminal watches nothing');

  // agentd is restarted: the socket closes with no warning.
  fakes.sockets[0].drop();
  await settle();
  assert.equal(statuses.at(-1), 'disconnected');
  assert.equal(widget.reconnectAvailable(), true, 'the operator can still redial by hand');
  assert.equal(watcher.watches.length, 1);
  assert.equal(watcher.watches[0].baseline, 'agentd-1',
    'the question asked is "is it still the process I was attached to?"');
  assert.equal(fakes.sockets.length, 1, 'asking is not reconnecting');

  // The daemon comes back as a different process.
  watcher.watches[0].onRestart('agentd-2');
  await settle();
  assert.equal(fakes.sockets.length, 2, 'the terminal reattaches itself');
  assert.equal(fakes.sockets[1].url, 'wss://dashboard.test/api/term-ws/agt_one');

  // The reattach lands on the new daemon, which becomes the new baseline.
  fakes.sockets[1].open();
  await settle();
  assert.equal(statuses.at(-1), 'connected');
  assert.equal(watcher.watches.length, 1, 'a connected terminal is not waiting for anything');

  widget.dispose();
});

test('a reattach that fails needs a further restart, not a retry loop', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  fakes.sockets[0].drop();
  await settle();

  watcher.watches[0].onRestart('agentd-2');
  await settle();
  // The daemon's listener is up but the session is not ready yet, so the
  // reattach dies immediately.
  fakes.sockets[1].drop();
  await settle();

  assert.equal(fakes.sockets.length, 2, 'a failed reattach does not immediately try again');
  assert.equal(watcher.watches.length, 2, 'it goes back to waiting');
  assert.equal(watcher.watches[1].baseline, 'agentd-2',
    'against the daemon it just reached — so only a FURTHER restart can trigger it');
});

test('an ordinary disconnect never reattaches on its own', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  fakes.sockets[0].drop();
  await settle();

  // This is the case the whole design is built around: the session ended, or
  // this terminal was deliberately reopened somewhere else. The daemon is the
  // same process, so the watcher never calls back and nothing dials.
  assert.equal(watcher.watches.length, 1);
  assert.equal(fakes.sockets.length, 1);
  assert.equal(widget.status(), 'disconnected');

  widget.dispose();
  assert.equal(watcher.watches[0].cancelled, 1, 'disposal releases the watch');
});

test('a terminal with no baseline does not guess', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  watcher.currentID = () => Promise.resolve(null);
  const { widget, fakes } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  fakes.sockets[0].drop();
  await settle();

  assert.equal(watcher.watches.length, 0,
    'an unreadable baseline costs this connection its self-repair, and nothing else');
  assert.equal(widget.reconnectAvailable(), true);
  widget.dispose();
});

test('an unreachable daemon at dial time is also worth waiting on', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  let authFails = false;
  const { widget, fakes } = await mountWidget(harness, {
    watcher,
    fetchImpl: async () => {
      if (authFails) throw new TypeError('Failed to fetch');
      return { ok: true };
    },
  });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();

  // The operator hits Reconnect while agentd is still down, so the dial never
  // gets past its auth preflight. That is a settled disconnect too.
  authFails = true;
  fakes.sockets[0].drop();
  await settle();
  const watchesAfterDrop = watcher.watches.length;
  await widget.connect();
  await settle();

  assert.equal(widget.status(), 'disconnected');
  assert.equal(watcher.watches.length, watchesAfterDrop + 1,
    'a dial that could not reach agentd goes back to waiting for a new one');
  assert.equal(watcher.watches.at(-1).baseline, 'agentd-1');
  widget.dispose();
});
