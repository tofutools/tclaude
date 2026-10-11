import { $ } from './helpers.js';
import { dashPrefs } from './prefs.js';

// Groups and Terminals each have their own $ toggle (the operator often wants
// costs in one but not the other). Each persists its own preference and owns
// one body class that its surface's CSS hides cost badges under.
function costToggle(key, bodyClass, fallbackKey = '') {
  const listeners = new Set();
  const hidden = () => {
    const value = dashPrefs.getItem(key);
    // A terminal preference never set yet starts from the shared one it
    // replaced, so splitting them changes nothing until it is toggled.
    if (value == null && fallbackKey) return dashPrefs.getItem(fallbackKey) === '1';
    return value === '1';
  };
  const apply = (value) => {
    document.body.classList.toggle(bodyClass, value);
    for (const listener of listeners) listener(value);
  };
  return {
    hidden,
    // refresh re-applies the current value: a toggle still following the
    // one it falls back to follows it live, not just after a reload.
    refresh() { if (listeners.size) apply(hidden()); },
    subscribe(listener) {
      listeners.add(listener);
      document.body.classList.toggle(bodyClass, hidden());
      listener(hidden());
      return () => listeners.delete(listener);
    },
    toggle() {
      const next = !hidden();
      dashPrefs.setItem(key, next ? '1' : '0');
      apply(next);
    },
  };
}

const GROUP_COST_HIDDEN_KEY = 'tclaude.dash.agentCost.hidden';
const groupCosts = costToggle(GROUP_COST_HIDDEN_KEY, 'agent-cost-hidden');
const terminalCosts = costToggle('tclaude.dash.terminalCost.hidden', 'terminal-cost-hidden', GROUP_COST_HIDDEN_KEY);

export const agentCostsHidden = groupCosts.hidden;
export const subscribeAgentCosts = groupCosts.subscribe;
export function toggleAgentCosts() {
  groupCosts.toggle();
  terminalCosts.refresh();
}
export const terminalCostsHidden = terminalCosts.hidden;
export const subscribeTerminalCosts = terminalCosts.subscribe;
export const toggleTerminalCosts = terminalCosts.toggle;

export function bindCostDisplayToggle() {
  const button = $('#groups-cost-toggle');
  if (!button) return;
  const unsubscribe = subscribeAgentCosts((hidden) => {
    button.setAttribute('aria-pressed', hidden ? 'false' : 'true');
    button.classList.toggle('off', hidden);
  });
  button.addEventListener('click', toggleAgentCosts);
  return () => {
    unsubscribe();
    button.removeEventListener('click', toggleAgentCosts);
  };
}
