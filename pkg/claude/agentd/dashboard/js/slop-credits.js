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
import { lastSnapshot } from './dashboard.js';
import { isSlopActive } from './slop.js';

// Credit payouts per win kind. Round, slot-machine-y numbers; the idle
// transition is the "real" win so it pays best, the mega is the rare
// 777 windfall.
const PAYOUT = {
  'win-idle': 100,
  'win-pull': 50,
  'win-spawn': 25,
  'win-mega': 777,
};

// Hot streak: an agent with ≥ HOT_THRESHOLD wins inside the trailing
// HOT_WINDOW_MS gets a 🔥 on the board. Enough to reward a genuinely
// churning agent without flagging the whole list the moment things get
// busy.
const HOT_WINDOW_MS = 60 * 1000;
const HOT_THRESHOLD = 3;
const LEADERBOARD_MAX = 8;

let credits = 0;
const winsByConv = new Map();    // conv-id → total wins this session
const winTimesByConv = new Map(); // conv-id → [recent win timestamps]

// titleFor resolves a conv-id to its friendly agent title via the last
// snapshot (same source slop-fx's marquee uses), falling back to a short
// id slice before the first snapshot lands.
function titleFor(conv) {
  const snap = lastSnapshot;
  if (snap) {
    const a = (snap.agents || []).find((x) => x.conv_id === conv);
    if (a && a.title) return a.title;
  }
  return conv.slice(0, 8);
}

function isHot(conv) {
  const times = winTimesByConv.get(conv);
  if (!times) return false;
  const cutoff = Date.now() - HOT_WINDOW_MS;
  return times.filter((t) => t >= cutoff).length >= HOT_THRESHOLD;
}

function renderCounter(bump) {
  const el = document.getElementById('slop-credits');
  if (!el) return;
  el.textContent = '🪙 ' + credits.toLocaleString();
  if (!bump) return;
  // Restart the bump animation: drop the class, force a reflow, re-add.
  el.classList.remove('slop-credits-bump');
  void el.offsetWidth;
  el.classList.add('slop-credits-bump');
}

function renderLeaderboard() {
  const host = document.getElementById('vegas-leaderboard');
  if (!host) return;
  const entries = Array.from(winsByConv.entries())
    .sort((a, b) => b[1] - a[1])
    .slice(0, LEADERBOARD_MAX);
  if (!entries.length) {
    morphInto(host,
      '<h3 class="vegas-leaderboard-title">🏆 High rollers</h3>' +
      '<div class="vegas-leaderboard-empty">No jackpots yet — put the agents to work, ' +
      'or pull a few levers to prime the pump.</div>');
    return;
  }
  const rows = entries.map(([conv, wins], i) => {
    const hot = isHot(conv);
    const who = (hot ? '🔥 ' : '') + esc(titleFor(conv));
    // Keyed by conv id (unique per entry) so a rank shuffle moves the row
    // intact rather than rewriting a neighbour's name under a selection.
    return `<li class="${hot ? 'hot' : ''}" data-key="${esc(conv)}">` +
      `<span class="rank">${i + 1}</span>` +
      `<span class="who">${who}</span>` +
      `<span class="wins">${wins} 🎰</span>` +
      `</li>`;
  }).join('');
  // Morph rather than swap: the board rebuilds every 2s while slop+Vegas are
  // active, so a plain innerHTML swap would wipe a selection each tick.
  morphInto(host,
    '<h3 class="vegas-leaderboard-title">🏆 High rollers ' +
    '<span class="vegas-leaderboard-sub">this session</span></h3>' +
    `<ol class="vegas-leaderboard-list">${rows}</ol>`);
}

function recordWin(fx, conv) {
  const payout = PAYOUT[fx];
  if (!payout) return;
  credits += payout;
  renderCounter(true);
  // Per-agent attribution only for the row-scoped wins. Spawn/mega carry
  // no conv, so they enrich the pot but not the board.
  if (conv && (fx === 'win-idle' || fx === 'win-pull')) {
    winsByConv.set(conv, (winsByConv.get(conv) || 0) + 1);
    const times = winTimesByConv.get(conv) || [];
    times.push(Date.now());
    // Trim to the hot-streak window so the array can't grow unbounded.
    const cutoff = Date.now() - HOT_WINDOW_MS;
    winTimesByConv.set(conv, times.filter((t) => t >= cutoff));
    renderLeaderboard();
  }
}

export function bindSlopCredits() {
  renderCounter(false);
  renderLeaderboard();

  document.addEventListener('tclaude:slopfx', (e) => {
    const d = e.detail || {};
    recordWin(d.fx, d.conv);
  });

  // Re-render the board when a fresh snapshot arrives so early wins that
  // showed a short conv-id pick up the real agent title, and so the 🔥
  // flame decays as the hot window rolls forward. Gated on slop — the
  // board only shows in the Vegas tab.
  document.addEventListener('tclaude:snapshot', () => {
    if (isSlopActive()) renderLeaderboard();
  });
}
