// The restart watcher is what lets a dead browser terminal tell "agentd went
// away" apart from every other reason its socket closed. Getting that
// distinction wrong in either direction is a bug with teeth: too eager and a
// terminal steals back an attach somebody deliberately moved elsewhere; too
// shy and the operator hand-reconnects every pane after each restart.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createRestartWatcher, RESTART_PROBE_DELAYS_MS } from '../dashboard/js/instance-watch.js';

// A controllable clock: timers are queued, never real, so a suite can run an
// entire 38-probe budget in no time and assert exactly how many requests it cost.
function fakeClock() {
  let next = 1;
  const pending = new Map();
  return {
    setTimeoutImpl(fn, delay) {
      const id = next++;
      pending.set(id, { fn, delay });
      return id;
    },
    clearTimeoutImpl(id) { pending.delete(id); },
    scheduled: () => [...pending.values()].map((entry) => entry.delay),
    // Run every timer queued right now. Timers queued BY those callbacks wait
    // for the next tick, so each call advances the loop by one round.
    async tick() {
      const due = [...pending.entries()];
      pending.clear();
      for (const [, entry] of due) entry.fn();
      // Let the probe's fetch/JSON promise chain settle.
      for (let i = 0; i < 8; i++) await Promise.resolve();
    },
  };
}

function fakeDocument() {
  const listeners = new Map();
  return {
    visibilityState: 'visible',
    addEventListener(type, handler) { listeners.set(type, handler); },
    removeEventListener(type) { listeners.delete(type); },
    listenerCount: () => listeners.size,
    emit(type) { listeners.get(type)?.(); },
  };
}

// The daemon under test: `id` is whichever process is answering, and `down`
// makes the fetch reject the way a refused connection does.
function fakeDaemon({ id = 'agentd-1', down = false, status = 200, body } = {}) {
  const state = { id, down, status, body, calls: 0 };
  state.fetchImpl = async () => {
    state.calls += 1;
    if (state.down) throw new TypeError('Failed to fetch');
    return {
      ok: state.status >= 200 && state.status < 300,
      status: state.status,
      json: async () => (state.body === undefined ? { instance_id: state.id } : state.body),
    };
  };
  return state;
}

function build(daemon, { documentRef = fakeDocument(), delays } = {}) {
  const clock = fakeClock();
  const watcher = createRestartWatcher({
    fetchImpl: daemon.fetchImpl,
    setTimeoutImpl: clock.setTimeoutImpl,
    clearTimeoutImpl: clock.clearTimeoutImpl,
    documentRef,
    delays: delays || RESTART_PROBE_DELAYS_MS,
  });
  return { watcher, clock, documentRef };
}

test('a waiter hears about a different daemon and nothing else', async () => {
  const daemon = fakeDaemon({ id: 'agentd-1', down: true });
  const { watcher, clock } = build(daemon);

  const seen = [];
  watcher.watchForRestart('agentd-1', (id) => seen.push(id));
  assert.equal(watcher.waiting(), 1);

  // agentd is still down: no answer, so no conclusion — keep waiting.
  await clock.tick();
  assert.deepEqual(seen, []);
  assert.equal(watcher.waiting(), 1, 'an unreachable daemon is exactly what a restart looks like');

  // It answers again, but it is the SAME process: whatever closed the socket,
  // it was not a restart, so this terminal must keep its hands off.
  daemon.down = false;
  await clock.tick();
  assert.deepEqual(seen, []);
  assert.equal(watcher.waiting(), 1);

  // A different process. Now — and only now — the waiter is told, once.
  daemon.id = 'agentd-2';
  await clock.tick();
  assert.deepEqual(seen, ['agentd-2']);
  assert.equal(watcher.waiting(), 0, 'a waiter is one-shot');

  const before = daemon.calls;
  await clock.tick();
  assert.equal(daemon.calls, before, 'with nothing waiting, nothing is polled');
});

test('waiters share one poll, one round, and one budget', async () => {
  const daemon = fakeDaemon({ id: 'agentd-1' });
  const { watcher, clock } = build(daemon, { delays: [10, 10, 10] });

  const fired = [];
  watcher.watchForRestart('agentd-1', () => fired.push('a'));
  watcher.watchForRestart('agentd-1', () => fired.push('b'));
  watcher.watchForRestart('agentd-1', () => fired.push('c'));
  assert.equal(clock.scheduled().length, 1, 'three dead terminals are one poll, not three');

  await clock.tick();
  assert.equal(daemon.calls, 1, 'one request answers the question for all of them');

  daemon.id = 'agentd-2';
  await clock.tick();
  assert.deepEqual(fired, ['a', 'b', 'c'], 'one restart repairs every waiter');
});

test('a late joiner gets its own budget, and one waiter timing out spares the others', async () => {
  // A terminal that disconnected for an ordinary reason keeps politely asking
  // until its budget runs out. A terminal that dies from a real restart in the
  // tail of that round must still get its own full run of probes — and must not
  // be swept away when the first one gives up.
  const daemon = fakeDaemon({ id: 'agentd-1' });
  const { watcher, clock } = build(daemon, { delays: [10, 10, 10] });

  let early = 0;
  let late = 0;
  watcher.watchForRestart('agentd-1', () => { early += 1; });
  await clock.tick();
  await clock.tick();

  watcher.watchForRestart('agentd-1', () => { late += 1; });
  await clock.tick();
  assert.equal(early, 0, 'the early waiter never saw a different daemon');
  assert.equal(watcher.waiting(), 1, 'and has now spent its budget');

  daemon.id = 'agentd-2';
  await clock.tick();
  assert.equal(late, 1, 'the late waiter still had probes of its own left');
});

test('a transient bad answer is not a verdict, an unsupported one is', async () => {
  // A 503 from something in front of the listener, or a momentary auth race,
  // must not permanently abandon every waiting terminal for the whole outage.
  const daemon = fakeDaemon({ status: 503 });
  const { watcher, clock } = build(daemon, { delays: [10, 10, 10] });
  const fired = [];
  watcher.watchForRestart('agentd-1', (id) => fired.push(id));
  await clock.tick();
  assert.equal(watcher.waiting(), 1, 'a non-2xx is just another way of not answering');

  daemon.status = 200;
  daemon.id = 'agentd-2';
  await clock.tick();
  assert.deepEqual(fired, ['agentd-2'], 'and the round survives to see the real answer');

  // An agentd too old to publish the field, by contrast, will never answer this
  // question — asking it 38 times is pure waste.
  const old = fakeDaemon({ body: {} });
  const second = build(old, { delays: [10, 10, 10] });
  let firedOld = 0;
  second.watcher.watchForRestart('agentd-1', () => { firedOld += 1; });
  await second.clock.tick();
  await second.clock.tick();
  assert.equal(old.calls, 1, 'one clean answer with no id is enough to stop asking');
  assert.equal(second.watcher.waiting(), 0);
  assert.equal(firedOld, 0);
});

test('one waiter throwing does not deny the others their repair', async () => {
  const daemon = fakeDaemon({ id: 'agentd-1' });
  const { watcher, clock } = build(daemon, { delays: [10] });
  const fired = [];
  watcher.watchForRestart('agentd-1', () => { throw new Error('a rude waiter'); });
  watcher.watchForRestart('agentd-1', () => fired.push('b'));

  daemon.id = 'agentd-2';
  await clock.tick();
  assert.deepEqual(fired, ['b']);
  assert.equal(watcher.waiting(), 0);
});

test('the budget is finite and gives up silently', async () => {
  const daemon = fakeDaemon({ down: true });
  const { watcher, clock } = build(daemon, { delays: [10, 20, 30] });

  let fired = 0;
  watcher.watchForRestart('agentd-1', () => { fired += 1; });
  for (let round = 0; round < 6; round++) await clock.tick();

  assert.equal(daemon.calls, 3, 'the schedule bounds the requests a dead daemon costs');
  assert.equal(fired, 0, 'giving up is not a reconnect — the operator keeps the explicit control');
  assert.equal(watcher.waiting(), 0);
  assert.deepEqual(clock.scheduled(), [], 'nothing is left running in the background');

  // A fresh waiter brings its own budget.
  daemon.down = false;
  daemon.id = 'agentd-9';
  watcher.watchForRestart('agentd-1', () => { fired += 1; });
  await clock.tick();
  assert.equal(fired, 1);
});

test('a hidden tab spends nothing and repairs the moment it is looked at', async () => {
  const documentRef = fakeDocument();
  documentRef.visibilityState = 'hidden';
  const daemon = fakeDaemon({ id: 'agentd-2' });
  const { watcher, clock } = build(daemon, { documentRef, delays: [10, 10] });

  const fired = [];
  watcher.watchForRestart('agentd-1', (id) => fired.push(id));
  await clock.tick();
  assert.equal(daemon.calls, 0,
    'browsers throttle background timers anyway; polling behind a hidden tab buys nothing');

  documentRef.visibilityState = 'visible';
  documentRef.emit('visibilitychange');
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.deepEqual(fired, ['agentd-2'],
    'coming back to the tab is exactly when the answer is wanted — no timer wait');
});

test('cancel and dispose leave nothing behind', async () => {
  const documentRef = fakeDocument();
  const daemon = fakeDaemon({ id: 'agentd-2' });
  const { watcher, clock } = build(daemon, { documentRef, delays: [10, 10] });

  let fired = 0;
  const cancel = watcher.watchForRestart('agentd-1', () => { fired += 1; });
  assert.equal(documentRef.listenerCount(), 1);
  cancel();
  cancel();
  assert.equal(watcher.waiting(), 0);
  assert.deepEqual(clock.scheduled(), [], 'the poll stops with its last waiter');
  await clock.tick();
  assert.equal(fired, 0);

  watcher.watchForRestart('agentd-1', () => { fired += 1; });
  watcher.dispose();
  assert.equal(watcher.waiting(), 0);
  assert.equal(documentRef.listenerCount(), 0, 'dispose releases the visibility listener');
  await clock.tick();
  assert.equal(fired, 0);
});

test('a caller with no baseline is refused rather than guessed for', async () => {
  const daemon = fakeDaemon({ id: 'agentd-2' });
  const { watcher, clock } = build(daemon, { delays: [10] });

  let fired = 0;
  const cancel = watcher.watchForRestart(null, () => { fired += 1; });
  assert.equal(watcher.waiting(), 0,
    'without knowing which process it was attached to, every id looks like a restart');
  cancel();
  await clock.tick();
  assert.equal(daemon.calls, 0);
  assert.equal(fired, 0);
});

test('concurrent baseline reads share one request', async () => {
  const daemon = fakeDaemon({ id: 'agentd-1' });
  const { watcher } = build(daemon);

  const [first, second] = await Promise.all([watcher.currentID(), watcher.currentID()]);
  assert.equal(first, 'agentd-1');
  assert.equal(second, 'agentd-1');
  assert.equal(daemon.calls, 1, 'terminals opening together ask the daemon once');

  assert.equal(await watcher.currentID(), 'agentd-1');
  assert.equal(daemon.calls, 2, 'a later read is a fresh read, not a cached one');

  daemon.down = true;
  assert.equal(await watcher.currentID(), null, 'an unreachable daemon yields no baseline');
});

test('the production schedule is bounded and front-loaded', () => {
  const total = RESTART_PROBE_DELAYS_MS.reduce((sum, delay) => sum + delay, 0);
  assert.ok(RESTART_PROBE_DELAYS_MS.length <= 60, 'a bounded number of requests per outage');
  assert.ok(total >= 60_000 && total <= 120_000, 'covers a slow rebuild-and-restart, then stops');
  assert.ok(RESTART_PROBE_DELAYS_MS[0] <= 1000, 'an ordinary restart is caught within about a second');
  assert.ok(
    RESTART_PROBE_DELAYS_MS.at(-1) > RESTART_PROBE_DELAYS_MS[0],
    'the tail backs off instead of hammering a daemon that is not coming back',
  );
});
