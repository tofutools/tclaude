import { $ } from './helpers.js';

export function renderSpendLimitBanner(limits) {
  const banner = $('#spend-limit-banner');
  const crossed = limits.filter(limit => limit.threshold >= 80);
  banner.hidden = crossed.length === 0;
  banner.classList.toggle('over-limit', crossed.some(limit => limit.threshold >= 100));
  banner.textContent = crossed.map(limit =>
    `${limit.period === 'daily' ? 'Daily' : 'Monthly'} estimated API spend: $${limit.spend_usd.toFixed(2)} / $${limit.limit_usd.toFixed(2)} (${limit.threshold >= 100 ? 'limit reached' : '80% warning'})`
  ).join(' · ') + (crossed.length ? '. Cost-ledger estimates may differ from charges; subscription WHAT-IF costs excluded. Limits do not block spawns.' : '');
}
