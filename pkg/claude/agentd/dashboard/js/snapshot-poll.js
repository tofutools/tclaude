export const SNAPSHOT_POLL_MS = 2000;

// startSnapshotPoll is the sole periodic scheduler for /api/snapshot. Manual
// refresh/retry/mutation calls still route to the same refresh function; no
// island owns a timer. Injection keeps the scheduling contract testable without
// waiting on wall-clock time.
export function startSnapshotPoll(refresh, { setIntervalImpl = globalThis.setInterval } = {}) {
  if (typeof refresh !== 'function') throw new TypeError('snapshot poll requires refresh');
  if (typeof setIntervalImpl !== 'function') throw new TypeError('snapshot poll requires setInterval');
  void refresh();
  return setIntervalImpl(refresh, SNAPSHOT_POLL_MS);
}
