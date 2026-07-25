// connection.js — the "disconnected from agentd" watchdog.
//
// The dashboard polls /api/snapshot every 2s (refresh.js). When agentd goes
// away — killed, restarted, crashed, the laptop slept — that fetch REJECTS
// (connection refused / network error), which is distinct from a non-OK HTTP
// status (agentd answered, just unhappy). refresh() reports each poll's
// outcome here: noteConnected() when agentd answered at all, noteDisconnected()
// when the /api/snapshot fetch threw.
//
// After FAIL_THRESHOLD consecutive throws we declare a disconnect through the
// shared connection Signal and stop the Vegas radio. The Preact shell derives
// #disconnect-overlay from that Signal, while vegas.js keeps the radio stopped
// so a dead dashboard isn't left streaming lounge music as if all were well.
// The moment a poll gets through again we clear both.
//
// The 2s poll keeps running underneath the banner so a reconnect clears it.

import { setConnectionLost } from './vegas.js';
import { dashboardState } from './snapshot-store.js';

// One transient blip (a single slow/refused tick) shouldn't nuke the screen,
// so we require a couple of consecutive failures — ~2 poll cycles — before
// declaring the connection lost. A real agentd-down refuses instantly, so the
// banner still appears within a few seconds.
const FAIL_THRESHOLD = 2;

let consecutiveFails = 0;
// Any refused poll since the last successful one. Deliberately NOT the banner:
// the banner is a UI decision (don't flash the screen for one slow tick), while
// this is a factual one — the page lost its connection to agentd, so anything
// held open across that connection (every browser-terminal WebSocket) is dead.
let sawFailure = false;
// The agentd process the last snapshot came from. Null until the first poll
// parses one; a CHANGE from a known value is proof of a restart.
let instanceID = null;

// Listeners for the "agentd went away and is back" edge. This is deliberately
// an edge, not a state: a consumer that wants to repair something the outage
// broke (the terminal shell redials its dead sockets) must act once per outage,
// not once per poll.
//
// Two things raise it, because either alone misses real restarts:
//   - a refused poll followed by a successful one (covers an unreachable
//     daemon, a slept laptop, a dropped network — cases where nothing about
//     agentd's identity changed);
//   - a changed daemon instance id (covers a restart quick enough that no poll
//     was ever refused — the common case, since the poll runs every 2s while
//     the tab is visible and only every 10s while it is hidden).
// Neither fires on a merely healthy poll, so a page that never lost agentd
// never notifies anyone.
const restoredListeners = new Set();

// onConnectionRestored registers a listener for that edge and returns its
// unsubscribe. A listener that throws is contained here — one bad consumer
// must not stop the others, nor the poll that called us.
export function onConnectionRestored(listener) {
  if (typeof listener !== 'function') return () => {};
  restoredListeners.add(listener);
  return () => { restoredListeners.delete(listener); };
}

function notifyRestored() {
  for (const listener of [...restoredListeners]) {
    try { listener(); } catch (error) { console.warn('connection restored listener failed:', error); }
  }
}

// noteServerIdentity: the instance id carried by a parsed snapshot. The first
// one seen is only a baseline — a page that loads while agentd is already
// running has no outage to report. A later different id means the daemon we
// were talking to is gone, whether or not a poll ever noticed.
//
// An absent id (an older agentd across a mixed-version upgrade) is ignored
// rather than treated as a change, so it degrades to the refused-poll path.
export function noteServerIdentity(id) {
  if (typeof id !== 'string' || !id) return false;
  const previous = instanceID;
  instanceID = id;
  if (previous === null || previous === id) return false;
  // A restart means the failure streak, if any, belonged to the old process.
  sawFailure = false;
  notifyRestored();
  return true;
}

// noteConnected: agentd answered this poll (any HTTP status). Clears the
// failure streak and, if the disconnected state was active, lets the music
// resume. The shell reacts to the same state change.
export function noteConnected() {
  const wasDisconnected = isDisconnected();
  const recovered = sawFailure;
  consecutiveFails = 0;
  sawFailure = false;
  dashboardState.setConnection('connected');
  if (wasDisconnected) setConnectionLost(false);
  if (recovered) notifyRestored();
}

// noteDisconnected: the /api/snapshot fetch REJECTED this poll — agentd is
// unreachable. Crossing FAIL_THRESHOLD in a row raises the banner. Once we're
// already down there's nothing left to escalate, so we bail early — which also
// keeps consecutiveFails from climbing unbounded through a long outage.
export function noteDisconnected() {
  sawFailure = true;
  if (isDisconnected()) return;
  consecutiveFails++;
  const status = consecutiveFails >= FAIL_THRESHOLD ? 'disconnected' : 'retrying';
  dashboardState.setConnection(
    status,
    { consecutiveFailures: consecutiveFails },
  );
  if (status === 'disconnected') setConnectionLost(true);
}

// isDisconnected exposes the live state for any consumer (and for tests).
export function isDisconnected() {
  return dashboardState.connection.value.status === 'disconnected';
}
