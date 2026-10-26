import { h } from 'preact';
import htm from 'htm';
import { openUsageRulesManager } from './modal-usage-rules.js';

const html = htm.bind(h);

const ACTION_TEXT = {
  notify: 'warning',
  wrap_up: 'agents asked to wrap up',
  hard_stop: 'agents stopped',
  block_spawn: 'spawns blocked',
};

const PERIOD_TEXT = { day: 'today', week: 'this week', month: 'this month', window: 'this window' };

function amount(limit, value) {
  return limit.metric === 'window_pct' ? `${Math.round(value)}%` : `$${value.toFixed(2)}`;
}

function limitLabel(limit) {
  const scope = limit.scope === 'group' ? `Group ${limit.group}` : 'Global';
  return `${scope} limit "${limit.name}"`;
}

// SpendLimitBanner shows every global or group usage limit that has crossed
// a step of its ladder, from the snapshot's usage.usage_limits.
export function SpendLimitBanner({ state }) {
  const limits = state.snapshot.value?.usage?.usage_limits || [];
  const crossed = limits.filter(limit => limit.reached && !limit.dry_run);
  if (!crossed.length) return null;
  const over = crossed.some(limit => limit.reached !== 'notify' || limit.pct >= 100);
  const text = crossed.map(limit =>
    `${limitLabel(limit)}: ${amount(limit, limit.value)} / ${amount(limit, limit.budget)} ${PERIOD_TEXT[limit.period] || ''} (${Math.round(limit.pct)}%, ${ACTION_TEXT[limit.reached] || limit.reached})`
  ).join(' · ');
  const estimates = crossed.some(limit => limit.metric !== 'window_pct');
  return html`<div class=${`spend-limit-banner${over ? ' over-limit' : ''}`} role="status">
    ${text}.${estimates ? ' Cost-ledger estimates may differ from charges.' : ''}
    <button type="button" class="tool spend-limit-manage" onClick=${() => openUsageRulesManager()}>Manage limits…</button>
  </div>`;
}
