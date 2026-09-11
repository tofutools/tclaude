import { $ } from './helpers.js';
import { dashPrefs } from './prefs.js';

const COST_HIDDEN_KEY = 'tclaude.dash.agentCost.hidden';
const listeners = new Set();

export function agentCostsHidden() {
  return dashPrefs.getItem(COST_HIDDEN_KEY) === '1';
}

export function subscribeAgentCosts(listener) {
  listeners.add(listener);
  listener(agentCostsHidden());
  return () => listeners.delete(listener);
}

export function toggleAgentCosts() {
  const hidden = !agentCostsHidden();
  dashPrefs.setItem(COST_HIDDEN_KEY, hidden ? '1' : '0');
  document.body.classList.toggle('agent-cost-hidden', hidden);
  for (const listener of listeners) listener(hidden);
}

// Groups and Terminals share one presentation preference. Each surface owns
// its button; only the body-level visibility class is shared DOM state.
export function bindCostDisplayToggle() {
  const button = $('#groups-cost-toggle');
  if (!button) return;
  const unsubscribe = subscribeAgentCosts((hidden) => {
    document.body.classList.toggle('agent-cost-hidden', hidden);
    button.setAttribute('aria-pressed', hidden ? 'false' : 'true');
    button.classList.toggle('off', hidden);
  });
  button.addEventListener('click', toggleAgentCosts);
  return () => {
    unsubscribe();
    button.removeEventListener('click', toggleAgentCosts);
  };
}
