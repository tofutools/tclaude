import {
  activitySummary,
  activityModeViews,
  aggregateActivity,
  themedSummaryText,
} from './group-activity.js';
import { scribeGroupVisible } from './scribe-groups.js';

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
    authSession: snapshot.auth_session || null,
  };
}

// activityMembersForVisibility preserves the top bar's status overview while
// respecting the Groups tab's view policy. A hidden group still contributes
// live agents (the header is global, not a mirror of the current tab), but its
// dormant members stay out of the offline count. This deliberately ignores
// disclosure/collapse state: a folded, otherwise-visible group still belongs
// in the overview.
function activityMembersForVisibility(members, visible) {
  const list = members || [];
  return visible ? list : list.filter((member) => member?.online);
}

// Activity detail is deliberately a separate, richer view model from the
// glanceable bot summary. The summary collapses awaiting_permission/input into
// one bot and suppresses clean-offline bots while anything live is present;
// the hover panel must retain the exact state and every visible member so an
// operator can find the worker behind each count.
const ACTIVITY_TITLE_RE = /^[A-Za-z0-9_\-[\]{}() ]+$/;
const ACTIVITY_DETAIL_STATE_ORDER = [
  'recovery-backoff', 'recovery-suppressed', 'recovery-restarting', 'recovery-recovered',
  'error', 'awaiting_permission', 'awaiting_input', 'working', 'main_agent_idle',
  'idle', 'recovery-crashed', 'crashed', 'waking', 'offline', 'unknown',
];
const ACTIVITY_DETAIL_STATES = Object.freeze({
  'recovery-backoff': { label: 'Crash loop / backoff', wizardLabel: 'Crash loop / backoff' },
  'recovery-suppressed': { label: 'Recovery suppressed', wizardLabel: 'Recovery suppressed' },
  'recovery-restarting': { label: 'Restarting', wizardLabel: 'Restarting' },
  'recovery-recovered': { label: 'Recovered automatically', wizardLabel: 'Recovered automatically' },
  error: { label: 'Error / stuck', wizardLabel: 'Spell backfired / stuck' },
  awaiting_permission: { label: 'Awaiting permission', wizardLabel: 'Awaiting a decree' },
  awaiting_input: { label: 'Awaiting input', wizardLabel: 'Awaiting a key' },
  working: { label: 'Working', wizardLabel: 'Channeling' },
  main_agent_idle: { label: 'Idle + background work', wizardLabel: 'Meditating + work' },
  idle: { label: 'Idle', wizardLabel: 'Meditating' },
  'recovery-crashed': { label: 'Crashed — recovery pending', wizardLabel: 'Slain — recovery pending' },
  crashed: { label: 'Crashed', wizardLabel: 'Slain by a grue' },
  waking: { label: 'Starting up', wizardLabel: 'Awakening' },
  offline: { label: 'Offline', wizardLabel: 'Departed' },
  unknown: { label: 'Online', wizardLabel: 'Channeling' },
});

function activityText(value, max = 160) {
  return String(value || '')
    .replace(/[\u0000-\u001f\u007f]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, max);
}

// Titles are persisted operator/agent input. Keep the same 1–64 character
// boundary and charset as member-editor-island.js before letting a title cross
// into the detail model. Every eventual render is still a Preact text child;
// this gate is an additional bound on model size and display noise.
export function activityMemberTitle(member) {
  const candidates = [member?.title, member?.agent_id, member?.conv_id];
  for (const candidate of candidates) {
    // Apply the same raw-value gate as validMemberTitle; normalising first
    // would accidentally turn a tab/newline into an accepted single space.
    const value = String(candidate || '').trim();
    if (value && value.length <= 64 && !value.includes('  ') && ACTIVITY_TITLE_RE.test(value)) return value;
  }
  return 'unnamed worker';
}

function activityDetailState(member) {
  const state = member?.state || {};
  const recovery = String(state.recovery_status || '').trim();
  if (recovery === 'backoff') return 'recovery-backoff';
  if (recovery === 'suppressed') return 'recovery-suppressed';
  if (recovery === 'restarting') return 'recovery-restarting';
  if (recovery === 'recovered') return 'recovery-recovered';
  if (recovery === 'crashed') return 'recovery-crashed';
  if (!member?.online) {
    if (state.exit_reason === 'unexpected') return 'crashed';
    return member?.waking ? 'waking' : 'offline';
  }
  const status = String(state.status || '').trim();
  return ACTIVITY_DETAIL_STATES[status] ? status : status ? 'unknown' : 'idle';
}

function activityDetailEntry(member, key) {
  const state = member?.state || {};
  const stateKey = activityDetailState(member);
  const vocabulary = ACTIVITY_DETAIL_STATES[stateKey] || ACTIVITY_DETAIL_STATES.unknown;
  const detail = activityText(state.status_detail);
  return {
    key,
    name: activityMemberTitle(member),
    state: stateKey,
    label: vocabulary.label,
    wizardLabel: vocabulary.wizardLabel,
    detail,
  };
}

// Build a first-seen, conv_id-deduplicated roster. An agent can be listed in
// several groups, just as aggregateActivity sees it; assigning the first copy
// to its first group keeps the panel's names/counts one-to-one. Legacy rows
// without conv_id remain distinct, matching aggregateActivity's contract.
function activityDetailView(lists, groupNames, summary) {
  const seen = new Set();
  const groups = [];
  let total = 0;
  for (const [groupIndex, list] of (lists || []).entries()) {
    const stateGroups = new Map();
    for (const [memberIndex, member] of (list || []).entries()) {
      // Keep this identity check byte-for-byte aligned with aggregateActivity:
      // it dedups truthy conv_id values without trimming or coercing them.
      const convID = member?.conv_id;
      if (convID && seen.has(convID)) continue;
      if (convID) seen.add(convID);
      const stateKey = activityDetailState(member);
      const key = convID || `${groupIndex}:${memberIndex}:${total}`;
      const entry = activityDetailEntry(member, key);
      let state = stateGroups.get(stateKey);
      if (!state) {
        const vocabulary = ACTIVITY_DETAIL_STATES[stateKey] || ACTIVITY_DETAIL_STATES.unknown;
        state = { key: stateKey, label: vocabulary.label, wizardLabel: vocabulary.wizardLabel, members: [] };
        stateGroups.set(stateKey, state);
      }
      state.members.push(entry);
      total++;
    }
    if (!stateGroups.size) continue;
    const states = [...stateGroups.values()].sort((left, right) => {
      const a = ACTIVITY_DETAIL_STATE_ORDER.indexOf(left.key);
      const b = ACTIVITY_DETAIL_STATE_ORDER.indexOf(right.key);
      return (a < 0 ? ACTIVITY_DETAIL_STATE_ORDER.length : a)
        - (b < 0 ? ACTIVITY_DETAIL_STATE_ORDER.length : b);
    });
    groups.push({
      key: String(groupNames?.[groupIndex] || `group-${groupIndex}`),
      name: String(groupNames?.[groupIndex] || 'Unnamed group'),
      states,
    });
  }
  return {
    total,
    groups,
    // A clean-offline count is still in `summary.counts` even when its bot is
    // suppressed beside live work. Keep the fact explicit so the panel never
    // appears to disagree with the pulse row.
    suppressedOffline: summary?.counts?.offline > 0 && !summary.present.includes('offline')
      ? summary.counts.offline : 0,
  };
}

export function globalActivityView(snapshot, wizard = false, visibility = {}) {
  if (!snapshot) return { modes: [], title: '', animationKey: '', details: { total: 0, groups: [], suppressedOffline: 0 } };
  const groups = snapshot.groups || [];
  const showOfflineScribes = visibility.scribe ?? false;
  const showUngrouped = visibility.ungrouped ?? true;
  const lists = groups.map((group) => activityMembersForVisibility(
    group.members,
    scribeGroupVisible(group, showOfflineScribes),
  ));
  lists.push(activityMembersForVisibility(snapshot.ungrouped, showUngrouped));
  const summary = aggregateActivity(lists);
  const modes = activityModeViews(summary, snapshot.activity_bots);
  const details = activityDetailView(lists, [...groups.map((group) => group.name), 'Ungrouped'], summary);
  if (!modes.length) return { modes: [], title: '', animationKey: '', details };

  const theme = wizard ? 'wizard' : '';
  const lines = [];
  for (const [index, group] of groups.entries()) {
    const groupSummary = activitySummary(lists[index]);
    if (groupSummary.present.length && groupSummary.level !== 'offline') {
      lines.push(`${group.name}: ${themedSummaryText(groupSummary, theme)}`);
    }
  }
  const ungrouped = activitySummary(lists[groups.length]);
  if (ungrouped.present.length && ungrouped.level !== 'offline') {
    lines.push(`Ungrouped: ${themedSummaryText(ungrouped, theme)}`);
  }
  return {
    modes,
    animationKey: modes.map((mode) => `${mode.key}:${mode.style}:${mode.bots.map((bot) => `${bot.key}:${bot.count}`).join(',')}`).join('|'),
    title: `Activity across all groups — ${themedSummaryText(summary, theme)}`
      + (lines.length ? `\n${lines.join('\n')}` : ''),
    details,
  };
}
