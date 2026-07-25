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
// `live` is whatever the daemon would answer right now: an id while it is up,
// null while it is unreachable.
function recordingWatcher(live = 'agentd-1') {
  const watcher = {
    live,
    reads: 0,
    watches: [],
    currentID() {
      watcher.reads += 1;
      return Promise.resolve(watcher.live);
    },
    watchForRestart(baseline, onRestart) {
      const entry = { baseline, onRestart, cancelled: 0 };
      watcher.watches.push(entry);
      return () => { entry.cancelled += 1; };
    },
  };
  return watcher;
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

test('a terminal whose daemon has gone waits for a new one, then reattaches once', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes, statuses } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  assert.equal(statuses.at(-1), 'connected');
  assert.equal(watcher.watches.length, 0, 'a healthy terminal watches nothing');

  // agentd is restarted: the socket closes with no warning, and the daemon is
  // not answering when we ask what happened.
  watcher.live = null;
  fakes.sockets[0].drop();
  await settle();
  assert.equal(statuses.at(-1), 'disconnected');
  assert.equal(widget.reconnectAvailable(), true, 'the operator can still redial by hand');
  assert.equal(watcher.watches.length, 1);
  assert.equal(watcher.watches[0].baseline, 'agentd-1',
    'the question asked is "is it still the process I was attached to?"');
  assert.equal(fakes.sockets.length, 1, 'asking is not reconnecting');

  // The daemon comes back as a different process.
  watcher.live = 'agentd-2';
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

test('a terminal displaced while agentd was alive is never taken back', async (t) => {
  // The collision this whole design exists to avoid: the operator reopens this
  // terminal in another browser tab, on their phone, or in a native window.
  // tmux attaches with -d, so THIS socket is the one that dies while the
  // session runs happily under the new client. A restart an hour later must not
  // drag it back here.
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();

  // agentd is alive and unchanged — it is not what killed this socket.
  fakes.sockets[0].drop();
  await settle();
  assert.equal(watcher.reads, 2, 'the disconnect is checked against the live daemon');
  assert.equal(watcher.watches.length, 0,
    'a disconnect the daemon did not cause is not one a reattach may repair');
  assert.equal(fakes.sockets.length, 1);
  assert.equal(widget.status(), 'disconnected');

  // Even after a real restart, nothing here is waiting to hear about it.
  watcher.live = 'agentd-2';
  await settle();
  assert.equal(fakes.sockets.length, 1, 'a later restart cannot resurrect a displaced terminal');

  widget.dispose();
});

test('a restart that already happened is reattached without waiting', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();

  // Quick restart: by the time the disconnect is checked, the new process is
  // already answering. There is nothing to wait for.
  watcher.live = 'agentd-2';
  fakes.sockets[0].drop();
  await settle();
  assert.equal(watcher.watches.length, 0, 'no watch is needed for a question already answered');
  assert.equal(fakes.sockets.length, 2, 'it reattaches immediately');

  widget.dispose();
});

test('a reattach that fails needs a further restart, not a retry loop', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  watcher.live = null;
  fakes.sockets[0].drop();
  await settle();

  watcher.live = 'agentd-2';
  watcher.watches[0].onRestart('agentd-2');
  await settle();
  // The daemon's listener is up but the session is not ready yet, so the
  // reattach dies immediately.
  fakes.sockets[1].drop();
  await settle();

  assert.equal(fakes.sockets.length, 2, 'a failed reattach does not immediately try again');
  assert.equal(watcher.watches.length, 1,
    'the daemon is up and unchanged since the reattach, so there is nothing to wait for');

  // Only a further, different restart could move it again — and merely being a
  // different process later is not enough on its own, because this terminal is
  // no longer waiting to hear about one.
  watcher.live = 'agentd-3';
  await settle();
  assert.equal(fakes.sockets.length, 2, 'it never dials itself in the meantime');
  widget.dispose();
});

test('a socket that dies while its baseline is still being read still repairs', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  let releaseRead = null;
  const pending = new Promise((resolve) => { releaseRead = resolve; });
  let firstRead = true;
  const slowWatcher = {
    ...watcher,
    watches: watcher.watches,
    currentID() {
      if (!firstRead) return Promise.resolve(slowWatcher.live);
      firstRead = false;
      return pending;
    },
    live: 'agentd-1',
    watchForRestart: watcher.watchForRestart,
  };
  const { widget, fakes } = await mountWidget(harness, { watcher: slowWatcher });

  await widget.connect();
  fakes.sockets[0].open();
  // The daemon dies before the baseline read comes back.
  fakes.sockets[0].drop();
  await settle();
  assert.equal(slowWatcher.watches.length, 0, 'nothing to reason about yet');

  slowWatcher.live = null;
  releaseRead('agentd-1');
  await settle();
  assert.equal(slowWatcher.watches.length, 1,
    'the late baseline is put to use instead of losing this connection its repair');
  assert.equal(slowWatcher.watches[0].baseline, 'agentd-1');
  widget.dispose();
});

test('a terminal with no baseline does not guess', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher(null);
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

test('a surface that answers its own disconnect keeps the decision', async (t) => {
  // The modal terminal raises a blocking "reconnect or close?" dialog. A
  // reattach sliding in behind it would leave the operator answering a stale
  // question — and "Close terminal" would then kill a live session.
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes } = await mountWidget(harness, { watcher, autoReattach: false });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  watcher.live = null;
  fakes.sockets[0].drop();
  await settle();

  assert.equal(watcher.watches.length, 0, 'no watch is armed for a surface that opted out');
  watcher.live = 'agentd-2';
  await settle();
  assert.equal(fakes.sockets.length, 1, 'and a restart never dials it behind the dialog');
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
  watcher.live = null;
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

test('disposal releases the watch', async (t) => {
  const harness = await createPreactHarness(t);
  const watcher = recordingWatcher('agentd-1');
  const { widget, fakes } = await mountWidget(harness, { watcher });

  await widget.connect();
  fakes.sockets[0].open();
  await settle();
  watcher.live = null;
  fakes.sockets[0].drop();
  await settle();
  assert.equal(watcher.watches.length, 1);

  widget.dispose();
  assert.equal(watcher.watches[0].cancelled, 1);
});
