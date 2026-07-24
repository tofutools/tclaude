export const SNAPSHOT_POLL_MS = 2000;
export const SNAPSHOT_HIDDEN_POLL_MS = 10000;

// A poll-initiated refresh that has been running this long stops holding the
// cadence back, so a wedged refresh can never silence the poll permanently —
// the property the pre-guard "always start a fresh generation" tick had.
// refresh() bounds its own fetches well below this (SNAPSHOT_REQUEST_TIMEOUT_MS
// in refresh.js), so reaching it means the refresh is stuck somewhere the fetch
// timeout cannot see, and resuming the cadence is the safer failure mode.
export const SNAPSHOT_STALL_MS = 30000;

// waitForInitialSnapshot deliberately does NOT race on the bootstrap attempt's
// completion. refresh() can return without publishing state when a newer poll
// supersedes it, or after an HTTP/network failure. Only an actual
// tclaude:snapshot signal (passed as snapshotReady) or the bounded paint-curtain
// timeout may release URL restoration.
//
// The attempt itself belongs to startSnapshotPoll (its boot attempts), not to
// this helper: the in-flight guard below only covers refreshes the poll started,
// so a bootstrap fetch issued outside it would be cancelled by the very first
// tick — exactly the starvation the guard exists to prevent.
export async function waitForInitialSnapshot(snapshotReady, timeout) {
  await Promise.race([snapshotReady, timeout]);
}

// startSnapshotPoll is the sole periodic scheduler for /api/snapshot. Manual
// refresh/retry/mutation calls still route to the same refresh function; no
// island owns a timer. Injection keeps the scheduling contract testable without
// waiting on wall-clock time.
export function startSnapshotPoll(refresh, {
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
  documentImpl = globalThis.document,
  // Monotonic where available: this measures elapsed time, so a backwards
  // system-clock adjustment must not silently extend the stall bound below.
  nowImpl = () => globalThis.performance?.now?.() ?? Date.now(),
  immediate = true,
  // Options handed to every BOOT attempt — the `immediate` one and any retry
  // taken while bootUntil is still pending. Boot uses it to ask for the SNAPSHOT
  // ALONE: on a normal tick the Groups tab's paginated
  // retired/conversations/replaced lists ride the same Promise.all, and those
  // are the heavy, ever-growing ones that were moved out of the snapshot
  // precisely for being expensive. Their virtual groups default to hidden, but
  // that visibility is a persisted preference — so for anyone who ever revealed
  // one, the paint curtain was blocking on them before the first frame could
  // render. They land on the first tick after boot instead.
  bootOptions = undefined,
  // Promise-like whose settlement ends the boot window. Boot narrowing must
  // outlive the first attempt: a first attempt that FAILS (agentd restarting,
  // say) leaves the paint curtain up, and the t=2s/4s/6s retries behind it are
  // as much "boot" as the first one was. Pass the bound the curtain WAITS ON —
  // the first published snapshot or the bounded bail — so the lists resume as
  // soon as nothing is blocking on them. (The curtain is pulled a little later
  // still, after layout settles; a tick landing in that short window fetches the
  // lists behind a curtain that is no longer waiting for anything. Harmless, and
  // the alternative — narrowing until the reveal — would keep the first painted
  // frame's lists empty for no gain.)
  bootUntil = undefined,
  stallMs = SNAPSHOT_STALL_MS,
} = {}) {
  if (typeof refresh !== 'function') throw new TypeError('snapshot poll requires refresh');
  if (typeof setTimeoutImpl !== 'function') throw new TypeError('snapshot poll requires setTimeout');
  if (typeof clearTimeoutImpl !== 'function') throw new TypeError('snapshot poll requires clearTimeout');
  if (typeof nowImpl !== 'function') throw new TypeError('snapshot poll requires now');
  // Both directions: bootOptions without a bound would narrow FOREVER (the
  // Groups lists never fetched again for the life of the page), and a bound
  // without options would silently mean no boot narrowing at all — the exact
  // failure this option pair was added to close.
  if (bootOptions !== undefined && typeof bootUntil?.then !== 'function') {
    throw new TypeError('snapshot poll requires bootUntil alongside bootOptions');
  }
  if (bootUntil !== undefined && bootOptions === undefined) {
    throw new TypeError('snapshot poll requires bootOptions alongside bootUntil');
  }

  let timer = null;
  let stopped = false;
  // Start of the poll-initiated refresh currently running (nowImpl units), or
  // null.
  let inFlightSince = null;
  // True while boot attempts should stay narrowed. Settlement is enough — the
  // curtain comes down on a rejection too, and boot narrowing must never
  // outlive the curtain.
  let booting = bootOptions !== undefined;
  if (booting) {
    const endBoot = () => { booting = false; };
    bootUntil.then(endBoot, endBoot);
  }

  const delay = () => documentImpl?.hidden ? SNAPSHOT_HIDDEN_POLL_MS : SNAPSHOT_POLL_MS;
  const schedule = () => {
    if (!stopped) timer = setTimeoutImpl(tick, delay());
  };
  // run funnels every poll-initiated refresh through an in-flight guard. Each
  // refresh() bumps a shared request generation, and a refresh whose generation
  // is no longer current bails without publishing or dispatching
  // tclaude:snapshot. So a poll that fires while the previous one is still
  // running does not merely duplicate work — it CANCELS it. Once one cycle
  // (fetch + parse + the first full render of every tab) costs more than the
  // poll interval, which a cold first load on a large roster does, every
  // attempt is starved by its successor and nothing ever paints: boot sits on
  // "Fetching data…" until its bounded bail, and in steady state the page
  // silently stops updating. Dropping the overlapping tick lets the slow cycle
  // finish and commit.
  const run = () => {
    if (inFlightSince !== null && nowImpl() - inFlightSince < stallMs) return;
    const startedAt = nowImpl();
    inFlightSince = startedAt;
    // Only the generation that set the marker may clear it: past stallMs a
    // successor takes ownership, and the straggler must not then unlock the
    // guard the successor is holding.
    const done = () => { if (inFlightSince === startedAt) inFlightSince = null; };
    Promise.resolve(refresh(booting ? bootOptions : undefined)).then(done, done);
  };
  const tick = () => {
    run();
    schedule();
  };
  const visibilityChanged = () => {
    if (timer !== null) clearTimeoutImpl(timer);
    // A visible dashboard should repaint immediately rather than waiting for
    // the remainder of the background cadence.
    if (!documentImpl.hidden) run();
    schedule();
  };

  if (immediate) run();
  schedule();
  documentImpl?.addEventListener?.('visibilitychange', visibilityChanged);

  return () => {
    stopped = true;
    if (timer !== null) clearTimeoutImpl(timer);
    documentImpl?.removeEventListener?.('visibilitychange', visibilityChanged);
  };
}
