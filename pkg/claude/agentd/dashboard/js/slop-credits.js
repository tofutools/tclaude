// slop-credits.js — gamifies the dashboard in slop mode.
//
// Bridges the `tclaude:slopfx` win events that slop-fx.js emits into the
// credits Signals state consumed by the Preact counter and leaderboard:
//
//   1. A 🪙 credits counter in the header that climbs on every jackpot
//      (and bumps with a little animation each time).
//   2. A 🏆 "High rollers" leaderboard in the Vegas tab ranking agents
//      by how many jackpots they've landed this session, with a 🔥
//      hot-streak flame on any agent that's been winning rapidly.
//
// "Wins" are the bus's win-* effects. The meaningful one is 'win-idle'
// — an agent genuinely finishing a turn (working → idle), the closest
// thing the dashboard has to productivity — so it pays the most and,
// like 'win-pull' (a hand-pulled 7-7-7), is attributed to the owning
// agent for the leaderboard. 'win-spawn' / 'win-mega' aren't tied to a
// single row, so they add to the global credits pot but not the board.
//
// Everything here is session-only and purely cosmetic: nothing is
// persisted, nothing leaves the page, and the numbers reset on reload.
// Companion to slop-fx.js (emitter), slop-audio.js, and slop-spectacle.js.

import { creditsState } from './credits-state.js';
import { dashboardState } from './snapshot-store.js';

export function bindSlopCredits({
  state = creditsState,
  documentRef = document,
  getSnapshot = () => dashboardState.snapshot.value,
  schedule = queueMicrotask,
  registerCleanup,
} = {}) {
  state.publishSnapshot(getSnapshot());

  const onSlopFx = (e) => {
    const d = e.detail || {};
    state.recordWin(d.fx, d.conv);
  };

  // Publish fresh snapshots so early wins that showed a short conv-id pick up
  // the real agent title and the 🔥 hot window can decay. Preact reacts to
  // the state update; this bridge never owns either DOM host.
  // refresh.js currently dispatches the event immediately before committing the
  // shared Signal. Defer one microtask so the default getSnapshot reads the
  // accepted value, while keeping the scheduling seam injectable in tests.
  let active = true;
  const onSnapshot = () => schedule(() => {
    if (!active) return;
    state.publishSnapshot(getSnapshot());
  });
  documentRef.addEventListener('tclaude:slopfx', onSlopFx);
  documentRef.addEventListener('tclaude:snapshot', onSnapshot);

  const cleanup = () => {
    if (!active) return;
    active = false;
    documentRef.removeEventListener('tclaude:slopfx', onSlopFx);
    documentRef.removeEventListener('tclaude:snapshot', onSnapshot);
  };
  if (typeof registerCleanup === 'function') registerCleanup(cleanup);
  return cleanup;
}
