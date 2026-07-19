import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function widgetFakes(document) {
  const counts = {
    termDispose: 0, fitDispose: 0, interactionDispose: 0,
    dataDispose: 0, resizeDispose: 0, observerDisconnect: 0,
    copy: 0, focus: 0, write: 0,
  };
  const sockets = [];
  let terminal;
  let observer;
  let interactionOptions;

  class FakeFitAddon {
    constructor() { this.fitCount = 0; this.disposed = false; }
    fit() { if (!this.disposed) this.fitCount += 1; }
    dispose() {
      if (this.disposed) return;
      this.disposed = true;
      counts.fitDispose += 1;
    }
  }

  class FakeTerminal {
    constructor(options) {
      this.options = options;
      this.cols = 80;
      this.rows = 24;
      this.addons = [];
      terminal = this;
    }
    loadAddon(addon) { this.addons.push(addon); }
    open(host) {
      const owned = document.createElement('textarea');
      owned.dataset.xtermOwned = 'true';
      host.append(owned);
    }
    onData(handler) {
      this.dataHandler = handler;
      return { dispose: () => { counts.dataDispose += 1; } };
    }
    onResize(handler) {
      this.resizeHandler = handler;
      return { dispose: () => { counts.resizeDispose += 1; } };
    }
    focus() { counts.focus += 1; }
    write() { counts.write += 1; }
    dispose() {
      counts.termDispose += 1;
      for (const addon of this.addons) addon.dispose?.();
    }
  }

  class FakeResizeObserver {
    constructor(handler) { this.handler = handler; observer = this; }
    observe(host) { this.host = host; }
    disconnect() { counts.observerDisconnect += 1; }
  }

  class FakeWebSocket {
    static OPEN = 1;
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.sent = [];
      this.closeCount = 0;
      sockets.push(this);
    }
    open() { this.readyState = FakeWebSocket.OPEN; this.onopen?.(); }
    disconnect() { this.readyState = 3; this.onclose?.(); }
    send(value) { this.sent.push(value); }
    close() { this.readyState = 3; this.closeCount += 1; }
  }

  const interactionsFactory = (options) => {
    interactionOptions = options;
    let disposed = false;
    return {
      copySelection: async () => { counts.copy += 1; },
      dispose() {
        if (disposed) return;
        disposed = true;
        counts.interactionDispose += 1;
      },
    };
  };

  return {
    counts, sockets, FakeFitAddon, FakeTerminal, FakeResizeObserver, FakeWebSocket,
    interactionsFactory,
    terminal: () => terminal,
    observer: () => observer,
    interactionOptions: () => interactionOptions,
  };
}

test('opaque widget owns one host and disposes every imperative edge exactly once', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountTerminalWidget } = await harness.importDashboardModule('js/terminals-core.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const fakes = widgetFakes(harness.document);
  const statuses = [];
  const reconnect = [];
  const selections = [];
  const composeMessage = () => {};
  let disconnected = 0;
  const added = [];
  const removed = [];
  const originalAdd = harness.document.addEventListener.bind(harness.document);
  const originalRemove = harness.document.removeEventListener.bind(harness.document);
  harness.document.addEventListener = (type, handler, options) => {
    if (type.startsWith('tclaude:')) added.push([type, handler]);
    return originalAdd(type, handler, options);
  };
  harness.document.removeEventListener = (type, handler, options) => {
    if (type.startsWith('tclaude:')) removed.push([type, handler]);
    return originalRemove(type, handler, options);
  };

  const widget = mountTerminalWidget({
    host,
    wsPath: '/api/term-ws/agt_one',
    onStatus: (value) => statuses.push(value),
    onReconnectChange: (value) => reconnect.push(value),
    onSelectionChange: (value) => selections.push(value),
    onComposeMessage: composeMessage,
    onDisconnect: () => { disconnected += 1; },
    fetchImpl: async () => ({ ok: true }),
    TerminalCtor: fakes.FakeTerminal,
    FitAddonCtor: fakes.FakeFitAddon,
    WebSocketCtor: fakes.FakeWebSocket,
    ResizeObserverCtor: fakes.FakeResizeObserver,
    locationRef: { protocol: 'https:', host: 'dashboard.test' },
    documentRef: harness.document,
    interactionsFactory: fakes.interactionsFactory,
  });

  assert.equal(host.childElementCount, 1, 'xterm alone populated the stable opaque host');
  assert.equal(host.firstElementChild.dataset.xtermOwned, 'true');
  assert.equal(await widget.connect(), true);
  assert.deepEqual(statuses, ['authenticating…', 'connecting…']);
  assert.equal(fakes.sockets.length, 1);
  assert.equal(fakes.sockets[0].url, 'wss://dashboard.test/api/term-ws/agt_one');
  fakes.sockets[0].open();
  assert.equal(statuses.at(-1), 'connected');
  assert.equal(fakes.sockets[0].sent.length, 1, 'open sends the initial resize');

  fakes.terminal().dataHandler('hello');
  assert.ok(fakes.sockets[0].sent.at(-1) instanceof Uint8Array, 'terminal input stays binary');
  fakes.interactionOptions().onSelectionChange(true);
  assert.deepEqual(selections, [true]);
  assert.equal(fakes.interactionOptions().onComposeMessage, composeMessage);
  await widget.copy();
  assert.equal(fakes.counts.copy, 1);

  const lateMessage = fakes.sockets[0].onmessage;
  const lateClose = fakes.sockets[0].onclose;
  const statusCount = statuses.length;
  widget.dispose();
  widget.dispose();
  lateMessage({ data: 'late output' });
  lateClose();
  fakes.observer().handler();
  harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:wizard'));
  fakes.interactionOptions().onSelectionChange(false);
  await widget.copy();

  assert.equal(statuses.length, statusCount, 'late socket/theme callbacks are inert');
  assert.deepEqual(selections, [true], 'late interaction callbacks are inert');
  assert.equal(disconnected, 0, 'intentional disposal never reports a disconnect');
  assert.equal(fakes.counts.write, 0, 'late socket data never reaches xterm');
  assert.equal(fakes.counts.copy, 1, 'copy becomes inert after disposal');
  assert.deepEqual({
    termDispose: fakes.counts.termDispose,
    fitDispose: fakes.counts.fitDispose,
    interactionDispose: fakes.counts.interactionDispose,
    dataDispose: fakes.counts.dataDispose,
    resizeDispose: fakes.counts.resizeDispose,
    observerDisconnect: fakes.counts.observerDisconnect,
  }, {
    termDispose: 1, fitDispose: 1, interactionDispose: 1,
    dataDispose: 1, resizeDispose: 1, observerDisconnect: 1,
  });
  assert.equal(fakes.sockets[0].closeCount, 1);
  assert.deepEqual(added.map(([type]) => type).sort(), ['tclaude:terminal-palette', 'tclaude:wizard']);
  assert.deepEqual(removed, added, 'the exact document listeners are removed');
});

test('dispose aborts auth and prevents a late preflight from creating a socket', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountTerminalWidget } = await harness.importDashboardModule('js/terminals-core.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const fakes = widgetFakes(harness.document);
  const auth = deferred();
  let capturedSignal;
  const statuses = [];
  const widget = mountTerminalWidget({
    host,
    wsPath: '/api/open-window-ws/agt_one',
    onStatus: (value) => statuses.push(value),
    fetchImpl: (_url, options) => { capturedSignal = options.signal; return auth.promise; },
    TerminalCtor: fakes.FakeTerminal,
    FitAddonCtor: fakes.FakeFitAddon,
    WebSocketCtor: fakes.FakeWebSocket,
    ResizeObserverCtor: fakes.FakeResizeObserver,
    locationRef: { protocol: 'http:', host: 'dashboard.test' },
    documentRef: harness.document,
    interactionsFactory: fakes.interactionsFactory,
  });

  const connecting = widget.connect();
  assert.deepEqual(statuses, ['authenticating…']);
  widget.dispose();
  assert.equal(capturedSignal.aborted, true);
  auth.resolve({ ok: true });
  assert.equal(await connecting, false);
  assert.equal(fakes.sockets.length, 0, 'a disposed auth generation cannot create a WebSocket');
});

test('reconnect invalidates every handler from the replaced socket', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountTerminalWidget } = await harness.importDashboardModule('js/terminals-core.js');
  const fakes = widgetFakes(harness.document);
  const statuses = [];
  const widget = mountTerminalWidget({
    host: harness.document.body.appendChild(harness.document.createElement('div')),
    wsPath: '/api/term-ws/agt_one',
    authenticate: false,
    onStatus: (value) => statuses.push(value),
    TerminalCtor: fakes.FakeTerminal,
    FitAddonCtor: fakes.FakeFitAddon,
    WebSocketCtor: fakes.FakeWebSocket,
    ResizeObserverCtor: fakes.FakeResizeObserver,
    locationRef: { protocol: 'http:', host: 'dashboard.test' },
    documentRef: harness.document,
    interactionsFactory: fakes.interactionsFactory,
  });

  await widget.connect();
  const old = fakes.sockets[0];
  const oldOpen = old.onopen;
  const oldClose = old.onclose;
  await widget.connect();
  const beforeLate = [...statuses];
  oldOpen();
  oldClose();
  assert.deepEqual(statuses, beforeLate, 'replaced socket callbacks cannot affect the new generation');
  assert.equal(old.closeCount, 1);
  widget.dispose();
});

test('initial retry covers failed and briefly unstable sockets, then yields to manual reconnect', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountTerminalWidget } = await harness.importDashboardModule('js/terminals-core.js');
  const fakes = widgetFakes(harness.document);
  const statuses = [];
  const reconnect = [];
  const timers = [];
  let clock = 0;
  let disconnected = 0;
  const widget = mountTerminalWidget({
    host: harness.document.body.appendChild(harness.document.createElement('div')),
    wsPath: '/api/open-window-ws/agt_one',
    authenticate: false,
    initialRetry: true,
    initialRetryDelays: [10, 20],
    initialRetryStabilityMs: 100,
    setTimeoutImpl(handler, delay) {
      const timer = { handler, delay, cleared: false };
      timers.push(timer);
      return timer;
    },
    clearTimeoutImpl(timer) { timer.cleared = true; },
    now: () => clock,
    onStatus: (value) => statuses.push(value),
    onReconnectChange: (value) => reconnect.push(value),
    onDisconnect: () => { disconnected += 1; },
    TerminalCtor: fakes.FakeTerminal,
    FitAddonCtor: fakes.FakeFitAddon,
    WebSocketCtor: fakes.FakeWebSocket,
    ResizeObserverCtor: fakes.FakeResizeObserver,
    locationRef: { protocol: 'http:', host: 'dashboard.test' },
    documentRef: harness.document,
    interactionsFactory: fakes.interactionsFactory,
  });

  await widget.connect();
  fakes.sockets[0].disconnect();
  assert.equal(statuses.at(-1), 'retrying…');
  assert.equal(timers[0].delay, 10);
  assert.equal(disconnected, 0, 'an intermediate initial failure is not user-visible disconnect');
  timers[0].handler();
  await Promise.resolve();
  assert.equal(fakes.sockets.length, 2);

  fakes.sockets[1].open();
  clock = 50;
  fakes.sockets[1].disconnect();
  assert.equal(statuses.at(-1), 'retrying…', 'a connection inside the stability window retries');
  assert.equal(timers[1].delay, 20);
  timers[1].handler();
  await Promise.resolve();
  assert.equal(fakes.sockets.length, 3);

  fakes.sockets[2].open();
  clock = 200;
  fakes.sockets[2].disconnect();
  assert.equal(statuses.at(-1), 'disconnected');
  assert.equal(disconnected, 1, 'a stable connection returns to ordinary disconnect handling');
  assert.equal(reconnect.at(-1), true);
  assert.equal(timers.length, 2, 'the bounded retry budget creates no permanent loop');
  widget.dispose();
});

test('disposing an initial retry cancels its timer and prevents a late socket', async (t) => {
  const harness = await createPreactHarness(t);
  const { mountTerminalWidget } = await harness.importDashboardModule('js/terminals-core.js');
  const fakes = widgetFakes(harness.document);
  let timer;
  const widget = mountTerminalWidget({
    host: harness.document.body.appendChild(harness.document.createElement('div')),
    wsPath: '/api/term-ws/agt_one',
    authenticate: false,
    initialRetry: true,
    initialRetryDelays: [10],
    setTimeoutImpl(handler) { timer = { handler, cleared: false }; return timer; },
    clearTimeoutImpl(value) { value.cleared = true; },
    TerminalCtor: fakes.FakeTerminal,
    FitAddonCtor: fakes.FakeFitAddon,
    WebSocketCtor: fakes.FakeWebSocket,
    ResizeObserverCtor: fakes.FakeResizeObserver,
    locationRef: { protocol: 'http:', host: 'dashboard.test' },
    documentRef: harness.document,
    interactionsFactory: fakes.interactionsFactory,
  });
  await widget.connect();
  fakes.sockets[0].disconnect();
  widget.dispose();
  assert.equal(timer.cleared, true);
  timer.handler();
  await Promise.resolve();
  assert.equal(fakes.sockets.length, 1);
});
