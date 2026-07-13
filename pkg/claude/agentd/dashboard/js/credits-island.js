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

export function mountCreditsIsland({
  host,
  state = creditsState,
  registerCleanup,
}) {
  if (!host) throw new TypeError('credits island requires a host');
  if (typeof registerCleanup !== 'function') {
    throw new TypeError('credits island requires registerCleanup');
  }
  registerCleanup(() => render(null, host));
  render(html`<${CreditsCounter} state=${state} />`, host);
}
