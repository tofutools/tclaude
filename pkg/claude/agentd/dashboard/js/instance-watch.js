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
  let probing = false;
  let inFlight = null;
  let visibilityBound = false;

  function hidden() {
    return documentRef?.visibilityState === 'hidden';
  }

  // read returns the live instance id, or null when agentd did not answer with
  // one. Single-flight: several terminals opening, dying, or waking at once
  // share the request rather than each asking the daemon the same question.
  //
  // `unsupported` separates the one null worth giving up on — a daemon that
  // answered cleanly and simply has no such field — from every null that just
  // means "ask again".
  function read() {
    if (inFlight) return inFlight;
    inFlight = (async () => {
      try {
        const response = await fetchImpl(url, { credentials: 'same-origin', cache: 'no-store' });
        // A non-2xx is NOT treated as a verdict. A transient 503, a proxy hiccup
        // or a momentary auth race would otherwise permanently abandon every
        // waiting terminal for that outage; the bounded budget already stops us
        // asking forever.
        if (!response.ok) return { id: null, unsupported: false };
        const body = await response.json();
        const id = typeof body?.instance_id === 'string' && body.instance_id ? body.instance_id : null;
        // Answered, parsed, and no id: this agentd does not publish one, and no
        // amount of asking will change that.
        return { id, unsupported: id === null };
      } catch (_) {
        // Refused / network down / unparseable: agentd is simply not back yet.
        return { id: null, unsupported: false };
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
  }

  // abandon drops waiters without notifying them: they asked to hear about a
  // restart, and none was observed. Each falls back to whatever it does when
  // nobody answers — for a terminal, the Reconnect control it already shows.
  function abandon(doomed) {
    for (const waiter of doomed) waiters.delete(waiter);
    if (waiters.size === 0) stop();
  }

  // Each waiter owns its own budget rather than sharing a round's. A terminal
  // that starts waiting late — while some earlier one is most of the way
  // through its schedule — must get its own full run of probes, or the outage
  // it registered for would be abandoned before it was ever asked about.
  function pruneExhausted() {
    abandon([...waiters].filter((waiter) => waiter.attempt >= delays.length));
  }

  function schedule() {
    if (probing || timer !== null) return;
    pruneExhausted();
    if (waiters.size === 0) return;
    // Nothing is watching this tab, so nothing needs repairing yet. Browsers
    // throttle background timers to a crawl anyway; resuming on visibility
    // means the repair happens exactly when someone looks at the page, and
    // spends no requests while nobody does. A waiter therefore outlives its
    // nominal budget while hidden — the budget counts probes, and a hidden tab
    // takes none.
    if (hidden()) return;
    // The soonest any waiter wants to be asked. One request answers for all of
    // them, so the earliest need sets the cadence.
    const delay = Math.min(...[...waiters].map(
      (waiter) => Math.max(0, Number(delays[waiter.attempt]) || 0),
    ));
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
    // One probe costs every waiter one slot: they were all asked.
    for (const waiter of waiters) waiter.attempt += 1;
    if (outcome.id === null) {
      // An OK response with no id means an agentd too old to publish the field.
      // More polling cannot change that answer, so stop asking entirely.
      // Anything else — refused, unreachable, a non-2xx from a proxy or a
      // session that needs re-auth — is simply not an answer yet, and being
      // unreachable is exactly what a restart looks like from here.
      if (outcome.unsupported) abandon([...waiters]);
      else schedule();
      return;
    }
    const restarted = [...waiters].filter((waiter) => waiter.baseline !== outcome.id);
    abandon(restarted);
    for (const waiter of restarted) {
      // Contained: one waiter throwing must not deny the others their repair.
      try { waiter.onRestart(outcome.id); } catch (error) {
        console.warn('restart watcher listener failed:', error);
      }
    }
    schedule();
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
      const waiter = { baseline, onRestart, attempt: 0 };
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
      abandon([...waiters]);
      stop();
      if (visibilityBound && documentRef?.removeEventListener) {
        documentRef.removeEventListener('visibilitychange', onVisible);
        visibilityBound = false;
      }
    },
  });
}

export const restartWatcher = createRestartWatcher();
