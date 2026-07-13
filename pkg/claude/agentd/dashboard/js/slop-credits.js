// slop-credits.js — gamifies the dashboard in slop mode.
//
// Two surfaces, both fed by the `tclaude:slopfx` win events that
// slop-fx.js emits:
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

import { esc } from './helpers.js';
import { morphInto } from './morph.js';
import { isSlopActive } from './slop.js';
import { creditsState } from './credits-state.js';
import { dashboardState } from './snapshot-store.js';

export function renderCreditsLeaderboard({
  state = creditsState,
  documentRef = document,
} = {}) {
  const host = documentRef.getElementById('vegas-leaderboard');
  if (!host) return;
  const entries = state.view.value.entries;
  if (!entries.length) {
    morphInto(host,
      '<h3 class="vegas-leaderboard-title">🏆 High rollers</h3>' +
      '<div class="vegas-leaderboard-empty">No jackpots yet — put the agents to work, ' +
      'or pull a few levers to prime the pump.</div>');
    return;
  }
  const rows = entries.map((entry) => {
    const who = (entry.hot ? '🔥 ' : '') + esc(entry.title);
    // Keyed by conv id (unique per entry) so a rank shuffle moves the row
    // intact rather than rewriting a neighbour's name under a selection.
    return `<li class="${entry.hot ? 'hot' : ''}" data-key="${esc(entry.conv)}">` +
      `<span class="rank">${entry.rank}</span>` +
      `<span class="who">${who}</span>` +
      `<span class="wins">${entry.wins} 🎰</span>` +
      `</li>`;
  }).join('');
  // Morph rather than swap: the board rebuilds every 2s while slop+Vegas are
  // active, so a plain innerHTML swap would wipe a selection each tick.
  morphInto(host,
    '<h3 class="vegas-leaderboard-title">🏆 High rollers ' +
    '<span class="vegas-leaderboard-sub">this session</span></h3>' +
    `<ol class="vegas-leaderboard-list">${rows}</ol>`);
}

export function bindSlopCredits({
  state = creditsState,
  documentRef = document,
  getSnapshot = () => dashboardState.snapshot.value,
  isSlopActiveImpl = isSlopActive,
  schedule = queueMicrotask,
  registerCleanup,
} = {}) {
  state.publishSnapshot(getSnapshot());
  renderCreditsLeaderboard({ state, documentRef });

  const onSlopFx = (e) => {
    const d = e.detail || {};
    const result = state.recordWin(d.fx, d.conv);
    if (result.attributed) renderCreditsLeaderboard({ state, documentRef });
  };

  // Re-render the board when a fresh snapshot arrives so early wins that
  // showed a short conv-id pick up the real agent title, and so the 🔥
  // flame decays as the hot window rolls forward. Gated on slop — the
  // board only shows in the Vegas tab.
  // refresh.js currently dispatches the event immediately before committing the
  // shared Signal. Defer one microtask so the default getSnapshot reads the
  // accepted value, while keeping the scheduling seam injectable in tests.
  let active = true;
  const onSnapshot = () => schedule(() => {
    if (!active) return;
    state.publishSnapshot(getSnapshot());
    if (isSlopActiveImpl()) renderCreditsLeaderboard({ state, documentRef });
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
