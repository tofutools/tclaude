import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

export function SpendLimitBanner({ state }) {
  const limits = state.snapshot.value?.usage?.spend_limits || [];
  const crossed = limits.filter(limit => limit.threshold >= 80);
  if (!crossed.length) return null;
  const over = crossed.some(limit => limit.threshold >= 100);
  const text = crossed.map(limit =>
    `${limit.period === 'daily' ? 'Daily' : 'Monthly'} estimated API spend: $${limit.spend_usd.toFixed(2)} / $${limit.limit_usd.toFixed(2)} (${limit.threshold >= 100 ? 'limit reached' : '80% warning'})`
  ).join(' · ');
  return html`<div class=${`spend-limit-banner${over ? ' over-limit' : ''}`} role="status">
    ${text}. Cost-ledger estimates may differ from charges; subscription WHAT-IF costs excluded. Limits do not block spawns.
  </div>`;
}
