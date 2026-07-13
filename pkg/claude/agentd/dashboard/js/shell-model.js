import {
  activitySummary,
  aggregateActivity,
  styledBotsHTML,
  styledWizardBotsHTML,
  themedSummaryText,
} from './group-activity.js';

const USAGE_BAR_WIDTH = 8;

function usageBarColor(pct) {
  if (pct >= 80) return '#f85149';
  if (pct >= 60) return '#d29922';
  return '#3fb950';
}

function usageWindow(key, label, win, hidden = false) {
  const pct = Number(win?.pct || 0);
  return {
    key,
    kind: 'window',
    label,
    pct: Math.round(pct),
    color: usageBarColor(pct),
    filled: Math.max(0, Math.min(USAGE_BAR_WIDTH, Math.round((pct / 100) * USAGE_BAR_WIDTH))),
    remaining: win?.remaining ? `(${win.remaining})` : '',
    hidden,
  };
}

function subscriptionWindows(source, prefix, hideMissing = false) {
  if (!source?.available) return [];
  const zero = { pct: 0, remaining: '' };
  return [
    usageWindow(`${prefix}-5h`, '5h', source.five_hour || zero, hideMissing && !source.five_hour),
    usageWindow(`${prefix}-7d`, '7d', source.seven_day || zero, hideMissing && !source.seven_day),
  ];
}

function fmtCost(cost) {
  return cost >= 0.005 ? '$' + cost.toFixed(2) : '<1¢';
}

function costToken(today, mtd) {
  return {
    key: 'api-cost',
    kind: 'cost',
    label: 'api',
    today: today > 0 ? fmtCost(today) : '',
    mtd: fmtCost(mtd),
  };
}

export function usageView(usage) {
  const titles = [];
  const claude = subscriptionWindows(usage, 'claude');
  if (claude.length) titles.push('Claude subscription usage limits — 5-hour and 7-day rolling windows');

  const codexUsage = usage?.codex;
  const codex = subscriptionWindows(codexUsage, 'codex', true);
  const codexPeriods = [];
  if (codexUsage?.five_hour) codexPeriods.push('5-hour');
  if (codexUsage?.seven_day) codexPeriods.push('weekly');
  if (codex.length && codexPeriods.length) {
    const noun = codexPeriods.length === 1 ? 'limit' : 'limits';
    const windowNoun = codexPeriods.length === 1 ? 'window' : 'windows';
    titles.push(`Codex subscription usage ${noun} — ${codexPeriods.join(' and ')} rolling ${windowNoun}`);
  }

  const mtd = Number(usage?.total_cost_usd || 0);
  const today = Number(usage?.today_cost_usd || 0);
  const cost = mtd > 0 ? costToken(today, mtd) : null;
  if (cost) {
    let title = `API cost month-to-date: $${mtd.toFixed(4)}, summed across agent sessions recorded in tclaude's DB`;
    if (today > 0) title += ` · today: $${today.toFixed(4)}`;
    titles.push(title + ' · click to open the Costs tab');
  }

  if (codex.length) {
    const lines = [];
    if (claude.length) lines.push({ key: 'claude', label: 'Claude:', tokens: claude });
    lines.push({ key: 'codex', label: 'Codex:', tokens: codex });
    if (cost) lines.push({ key: 'cost', label: '', tokens: [cost] });
    return { na: false, multiline: true, title: titles.join(' · '), lines };
  }

  const tokens = [...claude];
  if (cost) tokens.push(cost);
  if (tokens.length) {
    return { na: false, multiline: false, title: titles.join(' · '), lines: [{ key: 'usage', label: null, tokens }] };
  }
  return {
    na: true,
    multiline: false,
    title: 'Subscription usage data is currently unavailable',
    text: 'usage: n/a',
    lines: [],
  };
}

export function messagesBadgeView(snapshot) {
  const accessPending = Number(snapshot?.access_requests_pending || 0);
  const total = Number(snapshot?.messages_unread || 0) + accessPending;
  return { text: total > 99 ? '99+' : String(total), hidden: total === 0, blink: accessPending > 0 };
}

export function footerMetaView(snapshot) {
  if (!snapshot) return null;
  return {
    version: snapshot.version || 'unknown',
    base: snapshot.popup_base || '',
    generatedAt: snapshot.generated_at || '',
  };
}

export function globalActivityView(snapshot, wizard = false) {
  if (!snapshot) return { markup: '', title: '' };
  const groups = snapshot.groups || [];
  const lists = groups.map((group) => group.members || []);
  lists.push(snapshot.ungrouped || []);
  const summary = aggregateActivity(lists);
  const styles = snapshot.activity_bots || {};
  const regularStyle = styles.regular || 'emoji';
  const slopStyle = styles.slop || 'sprites';
  const wizardStyle = styles.wizard || 'emoji';
  const wrap = (className, inner) => inner ? `<span class="${className} level-${summary.level}">${inner}</span>` : '';
  const regular = wrap('ga-regular', styledBotsHTML(summary, regularStyle));
  const slop = wrap('ga-slop', styledBotsHTML(summary, slopStyle));
  const wizardMarkup = wrap('ga-wizard', wizardStyle === 'off' ? '' : styledWizardBotsHTML(summary, wizardStyle));
  const markup = regular + slop + wizardMarkup;
  if (!markup) return { markup: '', title: '' };

  const theme = wizard ? 'wizard' : '';
  const lines = [];
  for (const group of groups) {
    const groupSummary = activitySummary(group.members || []);
    if (groupSummary.present.length && groupSummary.level !== 'offline') {
      lines.push(`${group.name}: ${themedSummaryText(groupSummary, theme)}`);
    }
  }
  const ungrouped = activitySummary(snapshot.ungrouped || []);
  if (ungrouped.present.length && ungrouped.level !== 'offline') {
    lines.push(`Ungrouped: ${themedSummaryText(ungrouped, theme)}`);
  }
  return {
    markup,
    title: `Activity across all groups — ${themedSummaryText(summary, theme)}`
      + (lines.length ? `\n${lines.join('\n')}` : ''),
  };
}
