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

// noteConnected: agentd answered this poll (any HTTP status). Clears the
// failure streak and, if the disconnected state was active, lets the music
// resume. The shell reacts to the same state change.
export function noteConnected() {
  const wasDisconnected = isDisconnected();
  consecutiveFails = 0;
  dashboardState.setConnection('connected');
  if (wasDisconnected) setConnectionLost(false);
}

// noteDisconnected: the /api/snapshot fetch REJECTED this poll — agentd is
// unreachable. Crossing FAIL_THRESHOLD in a row raises the banner. Once we're
// already down there's nothing left to escalate, so we bail early — which also
// keeps consecutiveFails from climbing unbounded through a long outage.
export function noteDisconnected() {
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
