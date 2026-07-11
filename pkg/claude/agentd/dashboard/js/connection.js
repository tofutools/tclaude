// connection.js — the "disconnected from agentd" watchdog.
//
// The dashboard polls /api/snapshot every 2s (refresh.js). When agentd goes
// away — killed, restarted, crashed, the laptop slept — that fetch REJECTS
// (connection refused / network error), which is distinct from a non-OK HTTP
// status (agentd answered, just unhappy). refresh() reports each poll's
// outcome here: noteConnected() when agentd answered at all, noteDisconnected()
// when the /api/snapshot fetch threw.
//
// After FAIL_THRESHOLD consecutive throws we declare a disconnect: raise the
// big #disconnect-overlay banner AND stop the Vegas radio, keeping it stopped
// (vegas.js setConnectionLost) so a dead dashboard isn't left streaming lounge
// music as if all were well — the whole point of this module. The moment a
// poll gets through again we clear both.
//
// The banner is deliberately NOT a .modal-overlay: refreshSuspended() (in
// refresh.js) must stay false so the 2s poll keeps running underneath it —
// otherwise we'd never see the reconnect that clears the banner.

import { setConnectionLost } from './vegas.js';
import { dashboardState } from './snapshot-store.js';

// One transient blip (a single slow/refused tick) shouldn't nuke the screen,
// so we require a couple of consecutive failures — ~2 poll cycles — before
// declaring the connection lost. A real agentd-down refuses instantly, so the
// banner still appears within a few seconds.
const FAIL_THRESHOLD = 2;

let consecutiveFails = 0;
let disconnected = false;

// noteConnected: agentd answered this poll (any HTTP status). Clears the
// failure streak and, if the disconnect banner was up, lifts it and lets the
// music resume.
export function noteConnected() {
  consecutiveFails = 0;
  dashboardState.setConnection('connected');
  if (disconnected) setDisconnected(false);
}

// noteDisconnected: the /api/snapshot fetch REJECTED this poll — agentd is
// unreachable. Crossing FAIL_THRESHOLD in a row raises the banner. Once we're
// already down there's nothing left to escalate, so we bail early — which also
// keeps consecutiveFails from climbing unbounded through a long outage.
export function noteDisconnected() {
  if (disconnected) return;
  consecutiveFails++;
  dashboardState.setConnection(
    consecutiveFails >= FAIL_THRESHOLD ? 'disconnected' : 'retrying',
    { consecutiveFailures: consecutiveFails },
  );
  if (consecutiveFails >= FAIL_THRESHOLD) setDisconnected(true);
}

function setDisconnected(on) {
  if (on === disconnected) return;
  disconnected = on;
  const overlay = document.getElementById('disconnect-overlay');
  if (overlay) {
    overlay.classList.toggle('show', on);
    // Rewrite the status line when RAISING the banner so the role="alert"
    // region registers a DOM mutation while it's visible. Some screen readers
    // don't announce an alert that only became visible via an ancestor's
    // display flip with unchanged content; textContent replaces the text node
    // even for an identical string, which is the mutation the live region
    // announces. Order matters — the .show above makes it visible first, then
    // this mutates inside the now-live region.
    if (on) {
      const status = document.getElementById('disconnect-status');
      if (status) status.textContent = 'Reconnecting…';
    }
  }
  // Stop (and keep stopped) all music while disconnected; on reconnect this
  // lets vegas.js reconcile the radio back to whatever the live theme wants.
  setConnectionLost(on);
}

// isDisconnected exposes the live state for any consumer (and for tests).
export function isDisconnected() { return disconnected; }
