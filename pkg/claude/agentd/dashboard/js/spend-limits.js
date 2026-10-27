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
  if (limit.scope === 'fleet') return `Fleet limit "${limit.name}" (${limit.target_label})`;
  const scope = limit.scope === 'group' ? `Group ${limit.group}` : 'Global';
  return `${scope} limit "${limit.name}"`;
}

// SpendLimitBanner shows every global or group usage limit that has crossed
// a step of its ladder, from the snapshot's usage.usage_limits.
export function SpendLimitBanner({ state }) {
  const usage = state.snapshot.value?.usage || {};
  const crossed = [
    ...(usage.usage_limits || []),
    // This node's fleet limits (spend summed across nodes).
    ...(usage.fleet_limits || []).map(limit => ({ ...limit, scope: 'fleet' })),
  ].filter(limit => limit.reached && !limit.dry_run);
  // Peers' fleet limits blocking or stopping anything on this node.
  const controls = usage.fleet_controls || [];
  if (!crossed.length && !controls.length) return null;
  const over = controls.length > 0 || crossed.some(limit => limit.reached !== 'notify' || limit.pct >= 100);
  const text = [
    ...crossed.map(limit =>
      `${limitLabel(limit)}: ${amount(limit, limit.value)} / ${amount(limit, limit.budget)} ${PERIOD_TEXT[limit.period] || ''} (${Math.round(limit.pct)}%, ${ACTION_TEXT[limit.reached] || limit.reached})`),
    ...controls.map(c => `${c.peer_label}'s fleet limit "${c.rule_name}" (${Math.round(c.pct)}%) ${[c.block ? 'blocks spawns' : '', c.stop ? 'stops agents' : ''].filter(Boolean).join(' and ')} here`),
  ].join(' · ');
  const estimates = crossed.some(limit => limit.metric !== 'window_pct');
  const fleetOnly = !globalThis.__tclaudeRemoteNode?.id && !crossed.some(limit => limit.scope !== 'fleet');
  return html`<div class=${`spend-limit-banner${over ? ' over-limit' : ''}`} role="status">
    ${text}.${estimates ? ' Cost-ledger estimates may differ from charges.' : ''}
    <button type="button" class="tool spend-limit-manage" onClick=${() => openUsageRulesManager(fleetOnly ? { tab: 'fleet' } : {})}>Manage limits…</button>
  </div>`;
}
