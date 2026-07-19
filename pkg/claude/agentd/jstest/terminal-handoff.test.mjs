import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function messageEvent(windowRef, { origin, source, data }) {
  const event = new windowRef.Event('message');
  Object.defineProperties(event, {
    origin: { value: origin },
    source: { value: source },
    data: { value: data },
  });
  return event;
}

test('handoff receiver consumes deep links and acknowledges only same-origin terminal requests', async (t) => {
  const harness = await createPreactHarness(t);
  const handoff = await harness.importDashboardModule('js/terminal-handoff.js');
  const seed = { ws: '/api/term-ws/agt_one', key: 'one', label: 'one' };
  const opened = [];
  const replaced = [];
  const acks = [];
  let focused = 0;
  const locationRef = {
    origin: 'https://dashboard.test', protocol: 'https:', host: 'dashboard.test',
    pathname: '/terminals', search: '', hash: handoff.encodeTerminalOpenHash(seed),
  };
  const source = { postMessage: (data, origin) => acks.push({ data, origin }) };
  harness.window.focus = () => { focused += 1; };
  const historyRef = {
    replaceState: (_state, _unused, url) => {
      replaced.push(url);
      const hashAt = url.indexOf('#');
      locationRef.hash = hashAt < 0 ? '' : url.slice(hashAt);
    },
  };
  const cleanup = handoff.bindTerminalHandoffReceiver({
    openSeed: async (next) => { opened.push(next); return next; },
    windowRef: harness.window,
    locationRef,
    historyRef,
  });
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(opened.length, 1);
  assert.equal(opened[0].initialRetry, true);
  assert.deepEqual(replaced, ['/terminals']);

  harness.window.dispatchEvent(messageEvent(harness.window, {
    origin: 'https://evil.test', source,
    data: { type: handoff.TERMINAL_REATTACH_REQUEST, id: 'bad', seed },
  }));
  await Promise.resolve();
  assert.equal(opened.length, 1, 'cross-origin request is ignored');

  harness.window.dispatchEvent(messageEvent(harness.window, {
    origin: locationRef.origin, source,
    data: { type: handoff.TERMINAL_REATTACH_REQUEST, id: 'good', seed },
  }));
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(opened.length, 2);
  assert.equal(opened[1].initialRetry, true);
  assert.deepEqual(acks, [{
    data: { type: handoff.TERMINAL_REATTACH_ACK, id: 'good', accepted: true },
    origin: locationRef.origin,
  }]);
  assert.match(replaced[1], /^\/terminals#open=/, 'the opener retains the claimed seed before ACK');
  assert.equal(replaced[2], '/terminals', 'the opener clears the seed only after opening succeeds');
  assert.equal(focused, 1);
  cleanup();
  cleanup();
});

test('receiver claims a slow handoff immediately and retains failed work for reload', async (t) => {
  const harness = await createPreactHarness(t);
  const handoff = await harness.importDashboardModule('js/terminal-handoff.js');
  const seed = { ws: '/api/term-ws/agt_slow', key: 'slow' };
  const locationRef = {
    origin: 'https://dashboard.test', pathname: '/groups', search: '?view=active', hash: '',
  };
  const acks = [];
  let finishOpen;
  const source = { postMessage: (data, origin) => acks.push({ data, origin }) };
  const cleanup = handoff.bindTerminalHandoffReceiver({
    openSeed: () => new Promise((resolve) => { finishOpen = resolve; }),
    windowRef: harness.window,
    locationRef,
    historyRef: {
      replaceState: (_state, _unused, url) => {
        const hashAt = url.indexOf('#');
        locationRef.hash = hashAt < 0 ? '' : url.slice(hashAt);
      },
    },
  });

  harness.window.dispatchEvent(messageEvent(harness.window, {
    origin: locationRef.origin, source,
    data: { type: handoff.TERMINAL_REATTACH_REQUEST, id: 'slow-request', seed },
  }));
  assert.deepEqual(acks, [{
    data: { type: handoff.TERMINAL_REATTACH_ACK, id: 'slow-request', accepted: true },
    origin: locationRef.origin,
  }], 'the exact opener claims before a cold runtime load can hit the sender timeout');
  assert.match(locationRef.hash, /^#open=/, 'claimed work is durable in the opener URL');

  finishOpen(false);
  await Promise.resolve();
  await Promise.resolve();
  assert.match(locationRef.hash, /^#open=/, 'failed work remains available for a reload retry');
  cleanup();
});

test('receiver rejects a concurrent claim instead of overwriting durable handoff work', async (t) => {
  const harness = await createPreactHarness(t);
  const handoff = await harness.importDashboardModule('js/terminal-handoff.js');
  const first = { ws: '/api/term-ws/agt_first', key: 'first' };
  const second = { ws: '/api/term-ws/agt_second', key: 'second' };
  const locationRef = { origin: 'https://dashboard.test', pathname: '/terminals', search: '', hash: '' };
  const acks = [];
  let finishFirst;
  const historyRef = {
    replaceState: (_state, _unused, url) => {
      const hashAt = url.indexOf('#');
      locationRef.hash = hashAt < 0 ? '' : url.slice(hashAt);
    },
  };
  const source = { postMessage: (data) => acks.push(data) };
  const cleanup = handoff.bindTerminalHandoffReceiver({
    openSeed: () => new Promise((resolve) => { finishFirst = resolve; }),
    windowRef: harness.window,
    locationRef,
    historyRef,
  });

  for (const [id, seed] of [['first', first], ['second', second]]) {
    harness.window.dispatchEvent(messageEvent(harness.window, {
      origin: locationRef.origin, source,
      data: { type: handoff.TERMINAL_REATTACH_REQUEST, id, seed },
    }));
  }
  assert.deepEqual(acks.map((ack) => [ack.id, ack.accepted]), [
    ['first', true], ['second', false],
  ]);
  assert.equal(handoff.decodeTerminalOpenHash(locationRef.hash).key, 'first');

  finishFirst(false);
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(handoff.decodeTerminalOpenHash(locationRef.hash).key, 'first');
  cleanup();
});

test('reattach request validates the exact opener acknowledgement', async (t) => {
  const harness = await createPreactHarness(t);
  const handoff = await harness.importDashboardModule('js/terminal-handoff.js');
  const origin = 'https://dashboard.test';
  const sent = [];
  const opener = {
    closed: false,
    postMessage(data, targetOrigin) {
      sent.push({ data, targetOrigin });
      queueMicrotask(() => harness.window.dispatchEvent(messageEvent(harness.window, {
        origin, source: opener,
        data: { type: handoff.TERMINAL_REATTACH_ACK, id: data.id, accepted: true },
      })));
    },
  };
  const accepted = await handoff.requestTerminalReattach({
    seed: { ws: '/api/open-window-ws/agt_one', key: 'one' },
    targetWindow: opener,
    windowRef: harness.window,
    locationRef: { origin },
  });
  assert.equal(accepted, true);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].targetOrigin, origin);
  assert.equal(sent[0].data.seed.initialRetry, true);
});
