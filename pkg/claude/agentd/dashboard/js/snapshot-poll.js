export const SNAPSHOT_POLL_MS = 2000;
export const SNAPSHOT_HIDDEN_POLL_MS = 10000;

// waitForInitialSnapshot starts the explicit bootstrap attempt but deliberately
// does NOT race on that attempt's completion. refresh() can return without
// publishing state when a newer poll supersedes it, or after an HTTP/network
// failure. Only an actual tclaude:snapshot signal (passed as snapshotReady) or
// the bounded paint-curtain timeout may release URL restoration.
export async function waitForInitialSnapshot(refresh, snapshotReady, timeout) {
  void refresh();
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
  immediate = true,
} = {}) {
  if (typeof refresh !== 'function') throw new TypeError('snapshot poll requires refresh');
  if (typeof setTimeoutImpl !== 'function') throw new TypeError('snapshot poll requires setTimeout');
  if (typeof clearTimeoutImpl !== 'function') throw new TypeError('snapshot poll requires clearTimeout');

  let timer = null;
  let stopped = false;
  const delay = () => documentImpl?.hidden ? SNAPSHOT_HIDDEN_POLL_MS : SNAPSHOT_POLL_MS;
  const schedule = () => {
    if (!stopped) timer = setTimeoutImpl(tick, delay());
  };
  const tick = () => {
    void refresh();
    schedule();
  };
  const visibilityChanged = () => {
    if (timer !== null) clearTimeoutImpl(timer);
    // A visible dashboard should repaint immediately rather than waiting for
    // the remainder of the background cadence.
    if (!documentImpl.hidden) void refresh();
    schedule();
  };

  if (immediate) void refresh();
  schedule();
  documentImpl?.addEventListener?.('visibilitychange', visibilityChanged);

  return () => {
    stopped = true;
    if (timer !== null) clearTimeoutImpl(timer);
    documentImpl?.removeEventListener?.('visibilitychange', visibilityChanged);
  };
}
