// instance-watch.js — "is this a NEW agentd?" watcher for clients holding
// something that dies with the daemon.
//
// A browser terminal's PTY WebSocket is exactly that: restart agentd and every
// one of them closes, even though the page is fine and the tmux session on the
// other side is still there waiting to be reattached. The terminal cannot tell
// that from the ordinary reasons a socket closes (the session ended, another
// client took the attach, the operator closed it elsewhere) — and redialing on
// the ordinary ones is precisely the collision we must avoid.
//
// /api/instance answers that question directly: a client remembers the daemon
// instance id it connected under, and while it is down it polls for one that
// differs. A different id is PROOF the daemon restarted and therefore proof the
// socket died for a reason a reattach fixes. Same id — or no answer at all —
// means keep waiting, and eventually give up and leave it to the operator.
//
// This module owns no DOM, imports nothing, and knows nothing about the
// dashboard: the standalone terminal pop-out, which polls no snapshot and has
// no dashboard state, gets the same behaviour as a dashboard pane.

export const INSTANCE_URL = '/api/instance';

// Probe schedule, one entry per attempt: fine-grained while an ordinary restart
// would land, coarser for a slow rebuild-and-restart, then done. ~80s and 38
// requests in the worst case — shared by every waiting terminal, not per
// terminal — after which a pane keeps only its explicit Reconnect control.
export const RESTART_PROBE_DELAYS_MS = Object.freeze([
  ...Array(20).fill(1000),
  ...Array(10).fill(2000),
  ...Array(8).fill(5000),
]);

// createRestartWatcher builds an independent watcher. Production uses the
// shared instance exported below; tests build their own with fakes.
export function createRestartWatcher({
  fetchImpl = (...args) => globalThis.fetch(...args),
  setTimeoutImpl = (...args) => globalThis.setTimeout(...args),
  clearTimeoutImpl = (...args) => globalThis.clearTimeout(...args),
  documentRef = globalThis.document,
  delays = RESTART_PROBE_DELAYS_MS,
  url = INSTANCE_URL,
} = {}) {
  const waiters = new Set();
  let timer = null;
  let attempt = 0;
  let probing = false;
  let inFlight = null;
  let visibilityBound = false;

  function hidden() {
    return documentRef?.visibilityState === 'hidden';
  }

  // read returns the live instance id, or null when agentd did not answer with
  // one. Single-flight: several terminals opening (or waking) at once share the
  // request rather than each asking the daemon the same question.
  //
  // The two null cases are deliberately NOT distinguished by the return value —
  // both mean "no usable answer" — but they differ for the poll loop, so the
  // outcome is reported separately to it.
  function read() {
    if (inFlight) return inFlight;
    inFlight = (async () => {
      try {
        const response = await fetchImpl(url, { credentials: 'same-origin', cache: 'no-store' });
        if (!response.ok) return { id: null, reachable: true };
        const body = await response.json();
        const id = typeof body?.instance_id === 'string' && body.instance_id ? body.instance_id : null;
        return { id, reachable: true };
      } catch (_) {
        // Refused / network down: agentd is simply not back yet.
        return { id: null, reachable: false };
      } finally {
        inFlight = null;
      }
    })();
    return inFlight;
  }

  function stop() {
    if (timer !== null) {
      clearTimeoutImpl(timer);
      timer = null;
    }
    attempt = 0;
  }

  // giveUp ends the whole round. Waiters are dropped WITHOUT being notified:
  // they asked to hear about a restart, and none was observed within the
  // budget. Each one falls back to whatever it does when nobody answers —
  // for a terminal, the Reconnect control it is already showing.
  function giveUp() {
    stop();
    waiters.clear();
  }

  function schedule() {
    if (probing || timer !== null || waiters.size === 0) return;
    // Nothing is watching this tab, so nothing needs repairing yet. Browsers
    // throttle background timers to a crawl anyway; resuming on visibility
    // means the repair happens exactly when someone looks at the page, and
    // spends no requests while nobody does.
    if (hidden()) return;
    if (attempt >= delays.length) {
      giveUp();
      return;
    }
    const delay = Math.max(0, Number(delays[attempt]) || 0);
    attempt += 1;
    timer = setTimeoutImpl(() => {
      timer = null;
      void probe();
    }, delay);
  }

  async function probe() {
    if (probing || waiters.size === 0) return;
    probing = true;
    let outcome;
    try {
      outcome = await read();
    } finally {
      probing = false;
    }
    if (waiters.size === 0) return;
    if (outcome.id === null) {
      // Answered but unusable (an agentd too old to publish the field, or an
      // expired session) is not something more polling will fix. Not answering
      // at all is exactly what a restart looks like from here — keep going.
      if (outcome.reachable) giveUp();
      else schedule();
      return;
    }
    for (const waiter of [...waiters]) {
      if (waiter.baseline === outcome.id) continue;
      waiters.delete(waiter);
      try { waiter.onRestart(outcome.id); } catch (error) {
        console.warn('restart watcher listener failed:', error);
      }
    }
    if (waiters.size === 0) stop();
    else schedule();
  }

  function onVisible() {
    if (hidden() || waiters.size === 0) return;
    // Coming back to the tab is the moment to answer "is it alive again?", so
    // check now instead of waiting out a timer that a hidden tab may not even
    // have been running.
    if (timer !== null) {
      clearTimeoutImpl(timer);
      timer = null;
    }
    void probe();
  }

  function bindVisibility() {
    if (visibilityBound || !documentRef?.addEventListener) return;
    visibilityBound = true;
    documentRef.addEventListener('visibilitychange', onVisible);
  }

  return Object.freeze({
    // currentID reads the id a caller should remember as its baseline. Called
    // when a terminal's socket opens, so what it later compares against is the
    // process it is actually attached to.
    async currentID() {
      const { id } = await read();
      return id;
    },

    // watchForRestart asks to be told once, when the daemon is observed to be a
    // different process than `baseline`. Returns a cancel function; the waiter
    // is also dropped after it fires and when the round's budget runs out, so
    // it can never turn into a standing retry loop.
    //
    // A caller with no baseline cannot be served: without knowing which process
    // it was attached to, any id at all would look like a restart. It gets an
    // inert cancel rather than a guess.
    watchForRestart(baseline, onRestart) {
      if (typeof baseline !== 'string' || !baseline || typeof onRestart !== 'function') {
        return () => {};
      }
      // A round's budget starts fresh when the first waiter arrives. Terminals
      // that die together (the case this exists for) share one round; a late
      // joiner shares whatever is left of it.
      if (waiters.size === 0) attempt = 0;
      const waiter = { baseline, onRestart };
      waiters.add(waiter);
      bindVisibility();
      schedule();
      return () => {
        if (!waiters.delete(waiter)) return;
        if (waiters.size === 0) stop();
      };
    },

    // Test/inspection surface: how many waiters are outstanding.
    waiting: () => waiters.size,

    dispose() {
      giveUp();
      if (visibilityBound && documentRef?.removeEventListener) {
        documentRef.removeEventListener('visibilitychange', onVisible);
        visibilityBound = false;
      }
    },
  });
}

export const restartWatcher = createRestartWatcher();
