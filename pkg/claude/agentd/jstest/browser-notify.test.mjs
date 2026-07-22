import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A hand-driven clock: startBrowserNotifyPoll schedules its next tick from
// the previous tick's completion, so the test advances it explicitly rather
// than waiting on wall-clock time.
function fakeClock() {
  let pending = null;
  return {
    setTimeoutImpl: (callback) => { pending = callback; return 1; },
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
