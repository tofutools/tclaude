import {
  activitySummary,
  activityModeViews,
  aggregateActivity,
  memberVariant,
  VARIANT_ORDER,
  themedSummaryText,
} from './group-activity.js';
import { scribeGroupVisible } from './scribe-groups.js';
import { fmtExactUSD, fmtUSD } from './costs-model.js';

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

function costToken(today, mtd) {
  return {
    key: 'api-cost',
    kind: 'cost',
    label: 'api',
    today: today > 0 ? fmtUSD(today) : '',
    mtd: fmtUSD(mtd),
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
    let title = `API cost month-to-date: ${fmtExactUSD(mtd)}, summed across agent sessions recorded in tclaude's DB`;
    if (today > 0) title += ` · today: ${fmtExactUSD(today)}`;
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
    generatedAt: snapshot.generated_at || '',
  };
}

export function authoredOpenPRsView(snapshot, filter = 'all') {
  const source = snapshot?.authored_open_prs;
  if (!source?.available) return { available: false, items: [], total: 0 };
  const all = (source.items || []).filter((pr) => /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9][0-9]*(?:\/.*)?$/.test(pr?.url || ''));
  const needsAttention = (pr) => ['failing', 'pending'].includes(pr?.checks?.state);
  const filtered = all.filter((pr) => {
    if (filter === 'attention') return needsAttention(pr);
    if (filter === 'unattached') return !pr?.agent_id;
    return true;
  });
  return {
    available: true,
    total: Number(source.total || all.length),
    truncated: !!source.truncated,
    updatedAt: source.updated_at || '',
    searchURL: /^https:\/\/github\.com\//.test(source.search_url || '') ? source.search_url : '',
    attention: all.filter(needsAttention).length,
    unattached: all.filter((pr) => !pr?.agent_id).length,
    items: filtered,
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
// glanceable bot summary. Its top-level buckets intentionally mirror
// memberVariant/activitySummary exactly: the hover panel must never disagree
// with the six counts shown by the pulse. Finer states and recovery lifecycle
// information stay as bounded, fixed-vocabulary member annotations.
// The summary suppresses clean-offline bots while anything live is present;
// the hover panel includes those members and calls out the suppression
// explicitly.
const ACTIVITY_TITLE_RE = /^[A-Za-z0-9_\-[\]{}() ]+$/;
const ACTIVITY_DETAIL_STATES = Object.freeze(Object.assign(Object.create(null), {
  error: { label: 'Error / stuck', wizardLabel: 'Spell backfired / stuck' },
  asking: { label: 'Awaiting permission or input', wizardLabel: 'Awaiting a decree or key' },
  working: { label: 'Working', wizardLabel: 'Channeling' },
  idle: { label: 'Idle', wizardLabel: 'Meditating' },
  crashed: { label: 'Crashed', wizardLabel: 'Slain by a grue' },
  offline: { label: 'Offline', wizardLabel: 'Departed' },
}));
const ACTIVITY_KNOWN_STATUSES = new Set([
  '', 'error', 'awaiting_permission', 'awaiting_input', 'working',
  'main_agent_idle', 'idle', 'exited',
]);

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
    const value = String(candidate || '');
    if (value && value.length <= 64 && !value.includes('  ') && ACTIVITY_TITLE_RE.test(value)) return value;
  }
  return 'unnamed worker';
}

function activityDetailState(member) {
  // Keep the panel's sections aligned with the native pulse. dashboard.go can
  // leave `recovered` on a live member while its status is still working or
  // awaiting permission, so recovery belongs below as an annotation.
  return memberVariant(member);
}

function activityRecoveryAnnotation(member) {
  const recovery = String(member?.state?.recovery_status || '').trim();
  switch (recovery) {
    case 'recovered': return 'recovered';
    case 'restarting': return 'restarting';
    case 'backoff': return 'crash loop / backoff';
    case 'suppressed': return 'recovery suppressed';
    case 'crashed': return 'crashed — recovery pending';
    default: return '';
  }
}

function activityMemberAnnotation(member, stateKey) {
  const state = member?.state || {};
  const annotations = [];
  if (stateKey === 'working' && state.status === 'main_agent_idle') {
    annotations.push('background activity still running');
  } else if (stateKey === 'idle' && state.status === 'exited') {
    annotations.push('exited');
  } else if (member?.online && !ACTIVITY_KNOWN_STATUSES.has(state.status || '')) {
    annotations.push('status unavailable');
  }
  // group-activity's offline-first classifier deliberately does not make
  // waking a new bucket. Keep the annotation so a waking offline member is
  // discoverable while counts and suppression remain those of its pulse bot.
  if (!member?.online && member?.waking) annotations.push('starting up');
  const recovery = activityRecoveryAnnotation(member);
  if (recovery) annotations.push(recovery);
  return annotations.join(' · ');
}

function activityDetailEntry(member, key) {
  const stateKey = activityDetailState(member);
  const vocabulary = Object.hasOwn(ACTIVITY_DETAIL_STATES, stateKey)
    ? ACTIVITY_DETAIL_STATES[stateKey] : ACTIVITY_DETAIL_STATES.idle;
  const detail = stateKey === 'error' || stateKey === 'asking'
    ? activityText(member?.state?.status_detail) : '';
  return {
    key,
    name: activityMemberTitle(member),
    state: stateKey,
    label: vocabulary.label,
    wizardLabel: vocabulary.wizardLabel,
    detail,
    annotation: activityMemberAnnotation(member, stateKey),
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
        const vocabulary = Object.hasOwn(ACTIVITY_DETAIL_STATES, stateKey)
          ? ACTIVITY_DETAIL_STATES[stateKey] : ACTIVITY_DETAIL_STATES.idle;
        state = { key: stateKey, label: vocabulary.label, wizardLabel: vocabulary.wizardLabel, members: [] };
        stateGroups.set(stateKey, state);
      }
      state.members.push(entry);
      total++;
    }
    if (!stateGroups.size) continue;
    const states = [...stateGroups.values()].sort((left, right) => {
      const a = VARIANT_ORDER.indexOf(left.key);
      const b = VARIANT_ORDER.indexOf(right.key);
      return (a < 0 ? VARIANT_ORDER.length : a)
        - (b < 0 ? VARIANT_ORDER.length : b);
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
    suppressedOffline: summary?.counts?.offline > 0 && !summary?.present?.includes('offline')
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
