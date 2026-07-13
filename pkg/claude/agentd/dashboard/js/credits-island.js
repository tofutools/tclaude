import { h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { creditsState } from './credits-state.js';

const html = htm.bind(h);

export function CreditsCounter({ state }) {
  const current = state.view.value;
  const counterRef = useRef(null);

  useEffect(() => {
    if (!current.bumpVersion || !counterRef.current) return;
    // Restart the existing CSS bump animation for every payout, including two
    // wins close enough together that the class is still present.
    counterRef.current.classList.remove('slop-credits-bump');
    void counterRef.current.offsetWidth;
    counterRef.current.classList.add('slop-credits-bump');
  }, [current.bumpVersion]);

  return html`<span
    ref=${counterRef}
    class="slop-credits"
    id="slop-credits"
    title="Slop credits — climbs on every jackpot this session"
  >🪙 ${current.credits.toLocaleString()}</span>`;
}

export function CreditsLeaderboard({ state }) {
  const entries = state.view.value.entries;
  if (!entries.length) {
    return html`
      <h3 class="vegas-leaderboard-title">🏆 High rollers</h3>
      <div class="vegas-leaderboard-empty">
        No jackpots yet — put the agents to work, or pull a few levers to prime the pump.
      </div>
    `;
  }
  return html`
    <h3 class="vegas-leaderboard-title">
      🏆 High rollers <span class="vegas-leaderboard-sub">this session</span>
    </h3>
    <ol class="vegas-leaderboard-list">
      ${entries.map((entry) => html`
        <li
          key=${entry.conv}
          data-key=${entry.conv}
          class=${entry.hot ? 'hot' : ''}
        >
          <span class="rank">${entry.rank}</span>
          <span class="who">${entry.hot ? '🔥 ' : ''}${entry.title}</span>
          <span class="wins">${entry.wins} 🎰</span>
        </li>
      `)}
    </ol>
  `;
}

export function mountCreditsIsland({
  counterHost,
  leaderboardHost,
  state = creditsState,
  registerCleanup,
}) {
  if (!counterHost || !leaderboardHost) {
    throw new TypeError('credits island requires counter and leaderboard hosts');
  }
  if (typeof registerCleanup !== 'function') {
    throw new TypeError('credits island requires registerCleanup');
  }
  registerCleanup(() => render(null, counterHost));
  registerCleanup(() => render(null, leaderboardHost));
  render(html`<${CreditsCounter} state=${state} />`, counterHost);
  render(html`<${CreditsLeaderboard} state=${state} />`, leaderboardHost);
}
