import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A hand-driven clock: startBrowserNotifyPoll schedules its next tick from
// the previous tick's completion, so the test advances it explicitly rather
// than waiting on wall-clock time.
function fakeClock() {
  let pending = null;
  const delays = [];
  return {
    delays,
    setTimeoutImpl: (callback, ms) => { pending = callback; delays.push(ms); return 1; },
    clearTimeoutImpl: () => { pending = null; },
    async advance() {
      const run = pending;
      pending = null;
      run?.();
      // Let the tick's awaited fetches settle before asserting.
      await new Promise(resolve => setImmediate(resolve));
      await new Promise(resolve => setImmediate(resolve));
    },
    get scheduled() { return pending !== null; },
  };
}

function fakeWindow(permission) {
  const raised = [];
  class FakeNotification {
    static permission = permission;
    static requestPermission = async () => 'granted';
    constructor(title, options) {
      this.title = title;
      this.options = options;
      this.closed = false;
      raised.push(this);
    }
    close() { this.closed = true; }
  }
  let focused = 0;
  return { raised, win: { Notification: FakeNotification, focus: () => { focused += 1; } }, focusCount: () => focused };
}

test('browser notify poll adopts the head cursor and never replays a backlog', async (t) => {
  const harness = await createPreactHarness(t);
  const { startBrowserNotifyPoll } = await harness.importDashboardModule('js/browser-notify.js');
  const clock = fakeClock();
  const { raised, win } = fakeWindow('granted');
  const urls = [];
  const responses = [
    { cursor: 7, notifications: [] },
    { cursor: 9, notifications: [{ id: 8, title: 'Claude: Idle', body: 'abc | proj' }, { id: 9, title: 'Claude: Exited', body: '' }] },
  ];
  const fetchImpl = async (url) => {
    urls.push(url);
    return { ok: true, json: async () => responses.shift() ?? { cursor: 9, notifications: [] } };
  };

  const stop = startBrowserNotifyPoll({ fetchImpl, win, ...clock });
  // The constructor runs its first tick synchronously; let it settle.
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));

  // First call carries no cursor and paints nothing — "from now on".
  assert.equal(urls[0], '/api/browser-notifications');
  assert.equal(raised.length, 0);

  await clock.advance();
  assert.equal(urls[1], '/api/browser-notifications?since=7');
  assert.deepEqual(raised.map(n => n.title), ['Claude: Idle', 'Claude: Exited']);
  assert.equal(raised[0].options.body, 'abc | proj');
  assert.equal(raised[0].options.tag, 'tclaude-bn-8');

  // The cursor advanced past what was painted.
  await clock.advance();
  assert.equal(urls[2], '/api/browser-notifications?since=9');
  assert.equal(raised.length, 2);

  stop();
  assert.equal(clock.scheduled, false);
});

test('browser notify poll stays silent until permission is granted', async (t) => {
  const harness = await createPreactHarness(t);
  const { startBrowserNotifyPoll } = await harness.importDashboardModule('js/browser-notify.js');
  const clock = fakeClock();
  const { win } = fakeWindow('default');
  let fetches = 0;
  const fetchImpl = async () => {
    fetches += 1;
    return { ok: true, json: async () => ({ cursor: 0, notifications: [] }) };
  };

  const stop = startBrowserNotifyPoll({ fetchImpl, win, ...clock });
  await clock.advance();
  assert.equal(fetches, 0, 'no polling at all without permission');

  // Granting later must take effect without a reload.
  win.Notification.permission = 'granted';
  await clock.advance();
  assert.equal(fetches, 1);
  stop();
});

test('browser notify poll survives a failing endpoint and keeps its cursor', async (t) => {
  const harness = await createPreactHarness(t);
  const { startBrowserNotifyPoll } = await harness.importDashboardModule('js/browser-notify.js');
  const clock = fakeClock();
  const { win } = fakeWindow('granted');
  const urls = [];
  let calls = 0;
  const fetchImpl = async (url) => {
    urls.push(url);
    calls += 1;
    if (calls === 1) return { ok: true, json: async () => ({ cursor: 4, notifications: [] }) };
    if (calls === 2) return { ok: false, status: 503 };
    return { ok: true, json: async () => ({ cursor: 4, notifications: [] }) };
  };

  const stop = startBrowserNotifyPoll({ fetchImpl, win, ...clock });
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  await clock.advance(); // the 503
  await clock.advance();
  // Still asking from 4 — a failed poll must not rewind or skip ahead.
  assert.equal(urls[2], '/api/browser-notifications?since=4');
  assert.equal(clock.scheduled, true, 'the loop keeps ticking after a failure');
  stop();
});

test('browser notify poll backs off when the daemon says browser delivery is off', async (t) => {
  const harness = await createPreactHarness(t);
  const { startBrowserNotifyPoll, BROWSER_NOTIFY_POLL_MS, BROWSER_NOTIFY_IDLE_POLL_MS } =
    await harness.importDashboardModule('js/browser-notify.js');
  const clock = fakeClock();
  const { win } = fakeWindow('granted');
  // Permission is per-browser, delivery is per-daemon: this human granted
  // permission but left delivery at the default `os`.
  let enabled = false;
  const fetchImpl = async () => ({ ok: true, json: async () => ({ enabled, cursor: 3, notifications: [] }) });

  const stop = startBrowserNotifyPoll({ fetchImpl, win, ...clock });
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(clock.delays.at(-1), BROWSER_NOTIFY_IDLE_POLL_MS,
    'a switched-off channel must not be polled at the fast cadence forever');

  // Flipping the knob on the daemon is picked up without a reload.
  enabled = true;
  await clock.advance();
  assert.equal(clock.delays.at(-1), BROWSER_NOTIFY_POLL_MS);
  stop();
});

test('browser notify poll refuses to adopt a missing cursor rather than replaying the queue', async (t) => {
  const harness = await createPreactHarness(t);
  const { startBrowserNotifyPoll } = await harness.importDashboardModule('js/browser-notify.js');
  const clock = fakeClock();
  const { win, raised } = fakeWindow('granted');
  const urls = [];
  let broken = true;
  const fetchImpl = async (url) => {
    urls.push(url);
    if (broken) return { ok: true, json: async () => ({ enabled: true }) }; // no cursor
    return { ok: true, json: async () => ({ enabled: true, cursor: 11, notifications: [] }) };
  };

  const stop = startBrowserNotifyPoll({ fetchImpl, win, ...clock });
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  await clock.advance();
  // Still unadopted — never `?since=0`, which would raise every un-expired
  // banner in the queue at once.
  assert.deepEqual(urls, ['/api/browser-notifications', '/api/browser-notifications']);
  assert.equal(raised.length, 0);

  broken = false;
  await clock.advance();
  await clock.advance();
  assert.equal(urls.at(-1), '/api/browser-notifications?since=11');
  stop();
});

test('clicking a browser notification focuses the dashboard and closes the banner', async (t) => {
  const harness = await createPreactHarness(t);
  const { startBrowserNotifyPoll } = await harness.importDashboardModule('js/browser-notify.js');
  const clock = fakeClock();
  const state = fakeWindow('granted');
  const jumped = [];
  const responses = [
    { cursor: 1, notifications: [] },
    { cursor: 2, notifications: [{ id: 2, title: 'Claude: Awaiting permission', body: '', session_id: 'sess-1' }] },
  ];
  const fetchImpl = async () => ({ ok: true, json: async () => responses.shift() ?? { cursor: 2, notifications: [] } });

  const stop = startBrowserNotifyPoll({
    fetchImpl, win: state.win, onJump: item => jumped.push(item.session_id), ...clock,
  });
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  await clock.advance();

  const banner = state.raised[0];
  banner.onclick();
  assert.equal(state.focusCount(), 1);
  assert.equal(banner.closed, true);
  assert.deepEqual(jumped, ['sess-1']);
  stop();
});
