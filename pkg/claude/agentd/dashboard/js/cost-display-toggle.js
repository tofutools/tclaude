import { $ } from './helpers.js';
import { dashPrefs } from './prefs.js';

const COST_HIDDEN_KEY = 'tclaude.dash.agentCost.hidden';

// The Groups-tab 💲 control is intentionally outside the Costs island: it owns
// a Groups subtree and merely toggles a body-level presentation preference.
export function bindCostDisplayToggle() {
  const button = $('#groups-cost-toggle');
  if (!button) return;
  const apply = (hidden) => {
    document.body.classList.toggle('agent-cost-hidden', hidden);
    button.setAttribute('aria-pressed', hidden ? 'false' : 'true');
    button.classList.toggle('off', hidden);
  };
  apply(dashPrefs.getItem(COST_HIDDEN_KEY) === '1');
  button.addEventListener('click', () => {
    const hidden = !document.body.classList.contains('agent-cost-hidden');
    apply(hidden);
    dashPrefs.setItem(COST_HIDDEN_KEY, hidden ? '1' : '0');
  });
}
