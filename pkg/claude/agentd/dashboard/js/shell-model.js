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
import { prReviewState, readyToMerge } from './pr-review-model.js';

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

function costToken(key, today, mtd) {
  return {
    key,
    kind: 'cost',
    label: '',
    today: today > 0 ? fmtUSD(today) : '',
    mtd: fmtUSD(mtd),
  };
}

function unitsToken(key, win) {
  return {
    key, kind: 'units', label: '',
    used: Number(win?.used_units || 0), limit: Number(win?.limit_units || 0),
    remaining: win?.remaining ? `(${win.remaining})` : '',
  };
}

// WHAT_IF_HINT explains the ? marker a usage line carries while it shows a
// WHAT-IF estimate rather than real spend; the label itself stays short.
export const WHAT_IF_HINT = 'WHAT-IF estimate: this usage priced at pay-per-token list rates. '
  + 'It is not a real charge (subscription or sign-in usage is not billed per token).';

function providerLabel(provider) {
  if (provider === 'anthropic') return 'Anthropic';
  if (provider === 'openai') return 'OpenAI';
  if (provider === 'github') return 'Copilot';
  if (provider === 'google') return 'Google';
  if (provider === 'opencode') return 'OpenCode';
  if (provider === 'unknown') return 'Unknown';
  return String(provider || 'Unknown');
}

// Missing subscription windows are initially represented by hidden tokens so
// rows from different sources can share stable columns. A placeholder has no
// alignment work to do, though, when every other quota row leaves that same
// window empty too. Drop only those globally-empty quota placeholders: a
// weekly-only Codex row becomes compact on its own or beside unrelated monthly
// usage / API cost, while a Claude 5h token keeps Codex's missing 5h slot
// reserved.
function trimEmptyUsageColumns(lines) {
  const occupiedWindows = new Set();
  for (const line of lines) {
    for (const token of line.tokens) {
      if (token.kind === 'window' && token.label && !token.hidden) occupiedWindows.add(token.label);
    }
  }
  return lines.map((line) => ({
    ...line,
    tokens: line.tokens.filter((token) => !token.hidden || occupiedWindows.has(token.label)),
  }));
}

export function usageView(usage, modes = {}) {
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

  const copilotUsage = usage?.copilot;
  const copilot = copilotUsage?.available && copilotUsage?.monthly
    ? [usageWindow('copilot-monthly', '', copilotUsage.monthly)]
    : [];
  if (copilot.length) titles.push('GitHub Copilot monthly premium-request (AIC) allowance');

  const whatIfCosts = new Map((usage?.what_if_costs || []).map((item) => [item.provider, item]));
  const subscriptionLine = (key, label, tokens, provider) => {
    const estimate = whatIfCosts.get(provider) || {};
    const canCost = tokens.length > 0 && !!usage?.what_if_enabled;
    const mode = canCost && modes[key] === 'cost' ? 'cost' : 'usage';
    return {
      key, label, mode, modes: canCost ? ['usage', 'cost'] : [], whatIf: mode === 'cost',
      tokens: mode === 'cost'
        ? [costToken(`${key}-whatif`, estimate.today_cost_usd, estimate.total_cost_usd)]
        : tokens,
    };
  };

  const mtd = Number(usage?.total_cost_usd || 0);
  const today = Number(usage?.today_cost_usd || 0);
  const apiCosts = (usage?.api_costs || [])
    .map((item) => ({
      provider: item.provider || 'unknown',
      today: Number(item.today_cost_usd || 0),
      mtd: Number(item.total_cost_usd || 0),
    }))
    .filter((item) => item.mtd > 0);
  // Older daemons supplied only an aggregate. Keep that wire shape readable,
  // but current snapshots always provide provider-attributed rows.
  if (!apiCosts.length && mtd > 0) apiCosts.push({ provider: 'unknown', today, mtd });
  if (apiCosts.length) {
    let title = `API cost month-to-date: ${fmtExactUSD(mtd)}, summed across agent sessions recorded in tclaude's DB`;
    if (today > 0) title += ` · today: ${fmtExactUSD(today)}`;
    titles.push(title + ' · click to open the Costs tab');
  }

  const lines = [];
  if (claude.length) lines.push(subscriptionLine('claude', 'Claude:', claude, 'anthropic'));
  if (codex.length && codexPeriods.length) lines.push(subscriptionLine('codex', 'Codex:', codex, 'openai'));
  if (copilot.length) {
    const canUnits = Number(copilotUsage.monthly.limit_units || 0) > 0;
    const estimate = whatIfCosts.get('github') || {};
    const canCost = !!usage?.what_if_enabled;
    const availableModes = ['usage', ...(canCost ? ['cost'] : []), ...(canUnits ? ['units'] : [])];
    const mode = availableModes.includes(modes.copilot) ? modes.copilot : 'usage';
    lines.push({
      key: 'copilot', label: 'Copilot:', mode, modes: availableModes.length > 1 ? availableModes : [],
      whatIf: mode === 'cost',
      tokens: mode === 'units' ? [unitsToken('copilot-monthly-units', copilotUsage.monthly)]
        : mode === 'cost' ? [costToken('copilot-whatif', estimate.today_cost_usd, estimate.total_cost_usd)]
        : copilot,
    });
  }
  // Some harnesses expose token-priced what-if cost without account quota
  // windows (Gemini, pi and provider-backed OpenCode). Keep those estimates visible
  // without inventing a usage percentage or quota switch.
  const withQuota = new Set([
    ...(claude.length ? ['anthropic'] : []),
    ...(codex.length && codexPeriods.length ? ['openai'] : []),
    ...(copilot.length ? ['github'] : []),
  ]);
  if (usage?.what_if_enabled) for (const [provider, estimate] of whatIfCosts) {
    if (withQuota.has(provider) || !(Number(estimate.total_cost_usd) > 0)) continue;
    lines.push({
      key: `whatif-${provider}`, label: `${providerLabel(provider)}:`,
      mode: 'cost', modes: [], whatIf: true,
      tokens: [costToken(`whatif-cost-${provider}`, estimate.today_cost_usd, estimate.total_cost_usd)],
    });
  }
  for (const item of apiCosts) {
    lines.push({
      key: `cost-${item.provider}`,
      label: `${providerLabel(item.provider)} API:`,
      tokens: [costToken(`api-cost-${item.provider}`, item.today, item.mtd)],
    });
  }
  if (lines.length) {
    return { na: false, multiline: true, title: titles.join(' · '), lines: trimEmptyUsageColumns(lines) };
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

const PR_URL_RE = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/[1-9][0-9]*(?:\/.*)?$/;

// prKey folds a GitHub PR URL to owner/repo#number, case-insensitively, so a
// /files suffix or a differently-cased owner still names the same PR.
export function prKey(raw) {
  const m = /^https:\/\/github\.com\/([^/]+)\/([^/]+)\/pull\/([1-9][0-9]*)(?:[/?#].*)?$/i.exec(String(raw || '').trim());
  return m ? `${m[1]}/${m[2]}#${m[3]}`.toLowerCase() : '';
}

// peerPRAgents indexes the PRs the fused view's peer nodes show next to their
// agents (branch, startup, presented and statusline PRs). The page's daemon
// associates the footer's PRs with its own agents only, so a PR an agent works
// on from another node would otherwise read "no active agent". A restricted
// peer's projection carries no PR links and simply contributes nothing.
export function peerPRAgents(peers) {
  const byPR = new Map();
  for (const { name, snapshot } of peers || []) {
    const rows = [...(snapshot?.agents || []), ...(snapshot?.groups || []).flatMap((g) => g?.members || [])];
    for (const row of rows) {
      const id = row?.agent_id || row?.conv_id;
      if (!id) continue;
      const ref = { agent_id: id, agent_title: `${row.title || id}${name ? `@${name}` : ''}` };
      const urls = [row.branch_pr_url, row.startup_pr_url, ...(row.presented_prs || []).map((pr) => pr?.url)];
      for (const url of urls) {
        const key = prKey(url);
        if (key && !byPR.has(key)) byPR.set(key, ref);
      }
    }
  }
  return byPR;
}

// Ignored PRs. The operator can set a PR aside for a while or for good: it
// leaves every list and count of the footer PR popover and waits under the
// Ignored filter until it is brought back or its time runs out. The set is
// one DB-backed dashboard pref holding a JSON object keyed by prKey; each
// entry keeps enough of the PR (title, repository, number) to stay listable
// after it drops out of the authored search, e.g. once it merges, and whose
// tab it was ignored from ('' for the operator's own; a colleague's login).
export const OPEN_PR_IGNORE_DURATIONS = Object.freeze([
  { id: '1d', label: '1 day', ms: 24 * 3600e3 },
  { id: '7d', label: '1 week', ms: 7 * 24 * 3600e3 },
  { id: '30d', label: '30 days', ms: 30 * 24 * 3600e3 },
  { id: 'forever', label: 'Permanently', ms: 0 },
]);
// Bounds on the stored set. The byte budget is what keeps the pref under the
// daemon's 64 KiB value cap (and the browser's keepalive body quota): past
// it, the oldest ignores are evicted on save.
const IGNORED_PRS_MAX = 200;
const IGNORED_PR_TEXT_MAX = 200;
const IGNORED_PRS_MAX_BYTES = 48 * 1024;

const ignoredText = (value) => String(value || '').slice(0, IGNORED_PR_TEXT_MAX);

function ignoredEntryActive(entry, now) {
  return !!entry && (entry.until === 0 || entry.until > now);
}

// parseIgnoredPRs reads the stored pref, dropping malformed and expired
// entries. A null or corrupt value is an empty set.
export function parseIgnoredPRs(raw, now = Date.now()) {
  const out = Object.create(null);
  let parsed;
  try { parsed = JSON.parse(raw || '{}'); } catch (_) { return out; }
  if (!parsed || typeof parsed !== 'object') return out;
  for (const entry of Object.values(parsed)) {
    const key = prKey(entry?.url);
    if (!key || !PR_URL_RE.test(entry.url)) continue;
    const until = Number(entry.until);
    const at = Number(entry.at);
    if (!Number.isFinite(until) || until < 0 || !Number.isFinite(at)) continue;
    const clean = {
      url: entry.url, title: ignoredText(entry.title), repository: ignoredText(entry.repository),
      number: Math.max(0, Math.trunc(Number(entry.number) || 0)), until, at,
      author: ignoredText(entry.author).slice(0, 39),
    };
    if (ignoredEntryActive(clean, now)) out[key] = clean;
  }
  return out;
}

// ignorePR returns a new set with pr ignored for durationMs (0 = for good).
// Past the cap, the oldest ignores make room.
export function ignorePR(ignored, pr, durationMs, now = Date.now(), author = '') {
  const key = prKey(pr?.url);
  if (!key || !PR_URL_RE.test(pr.url)) return ignored;
  const next = { ...ignored };
  next[key] = {
    url: pr.url, title: ignoredText(pr.title), repository: ignoredText(pr.repository),
    number: Math.max(0, Math.trunc(Number(pr.number) || 0)),
    until: durationMs > 0 ? now + durationMs : 0, at: now,
    author: ignoredText(author).slice(0, 39),
  };
  const keys = Object.keys(next);
  if (keys.length > IGNORED_PRS_MAX) {
    keys.sort((a, b) => next[a].at - next[b].at)
      .slice(0, keys.length - IGNORED_PRS_MAX).forEach((k) => { delete next[k]; });
  }
  return next;
}

export function unignorePR(ignored, url) {
  const key = prKey(url);
  if (!key || !(key in ignored)) return ignored;
  const next = { ...ignored };
  delete next[key];
  return next;
}

// serializeIgnoredPRs is the pref value for a set, or '' when it is empty
// (the caller removes the pref then). Expired entries are dropped, and the
// oldest ignores go first if the value would outgrow its byte budget.
export function serializeIgnoredPRs(ignored, now = Date.now()) {
  const entries = Object.entries(ignored || {}).filter(([, e]) => ignoredEntryActive(e, now))
    .sort(([, a], [, b]) => a.at - b.at);
  const encoder = new TextEncoder();
  while (entries.length) {
    const value = JSON.stringify(Object.fromEntries(entries));
    if (encoder.encode(value).length <= IGNORED_PRS_MAX_BYTES) return value;
    entries.shift();
  }
  return '';
}

// owner names whose list this is — '' for the operator's own, else a tracked
// colleague's login — so the Ignored filter lists only that tab's ignores.
export function authoredOpenPRsView(snapshot, filter = 'all', includeDraftsInAttention = false, peers = [], ignored = null, now = Date.now(), owner = '') {
  const source = snapshot?.authored_open_prs;
  if (!source?.available) return { available: false, items: [], total: 0, recentCount: 0, recentWindowDays: 0, ignoredCount: 0 };
  const peerAgents = peerPRAgents(peers);
  const attach = (pr) => {
    if (pr?.agent_id || !peerAgents.size) return pr;
    const ref = peerAgents.get(prKey(pr?.url));
    return ref ? { ...pr, ...ref } : pr;
  };
  const sane = (list) => (list || []).filter((pr) => PR_URL_RE.test(pr?.url || '')).map(attach);
  const activeIgnores = Object.entries(ignored || {}).filter(([, entry]) => ignoredEntryActive(entry, now));
  const ignoredKeys = new Set(activeIgnores.map(([key]) => key));
  const notIgnored = (pr) => !ignoredKeys.has(prKey(pr?.url));
  const allWithIgnored = sane(source.items);
  const all = allWithIgnored.filter(notIgnored);
  // The recent list is deliberately kept out of `all`: the trigger count, the
  // attention/unattached tallies and their filters are about OPEN work, and a
  // merged PR must never inflate them.
  const recentWindowDays = Math.max(0, Number(source.recent_window_days || 0));
  const recentWithIgnored = recentWindowDays > 0 ? sane(source.recent) : [];
  const recent = recentWithIgnored.filter(notIgnored);
  // An ignored PR the snapshot still lists renders from its live row; one
  // that has dropped out (merged long ago, beyond a truncated page) renders
  // from what was stored when it was ignored.
  const liveByKey = new Map([...recentWithIgnored, ...allWithIgnored].map((pr) => [prKey(pr.url), pr]));
  const ownerKey = String(owner || '').toLowerCase();
  const ignoredItems = activeIgnores
    .filter(([key, entry]) => liveByKey.has(key) || String(entry.author || '').toLowerCase() === ownerKey)
    .sort(([, a], [, b]) => b.at - a.at)
    .map(([key, entry]) => ({ ...(liveByKey.get(key) || entry), ignored: { until: entry.until, at: entry.at } }));
  // A clean run that is still in flight needs time, not operator attention.
  // Everything terminal (red or green) is ready for a decision, while a PR
  // with no checks at all is ready for the operator to notice that absence.
  const needsAttention = (pr) => {
    // A draft is explicitly not ready for an operator decision. Keep it in
    // the Open and Unattached views, but leave it out of the attention signal
    // unless the operator opts into using drafts as part of that queue.
    if (pr?.draft && !includeDraftsInAttention) return false;
    // A reviewer asking for changes is waiting on the author whatever CI says.
    if (prReviewState(pr?.reviews) === 'changes') return true;
    const checks = pr?.checks;
    if (!checks || Number(checks.total || 0) === 0) return true;
    return checks.state === 'failing' || checks.state === 'passing';
  };
  const showingRecent = filter === 'recent' && recentWindowDays > 0;
  const showingIgnored = filter === 'ignored';
  const filtered = showingIgnored ? ignoredItems : showingRecent ? recent : all.filter((pr) => {
    if (filter === 'attention') return needsAttention(pr);
    if (filter === 'ready') return readyToMerge(pr);
    if (filter === 'unattached') return !pr?.agent_id;
    return true;
  });
  const searchURL = (raw) => (/^https:\/\/github\.com\//.test(raw || '') ? raw : '');
  return {
    available: true,
    alwaysShow: !!source.always_show,
    // The daemon's total counts beyond a truncated page; only the ignored
    // PRs this page shows can be taken off it.
    total: Math.max(0, Number(source.total || allWithIgnored.length) - (allWithIgnored.length - all.length)),
    truncated: showingIgnored ? false : showingRecent ? !!source.recent_truncated : !!source.truncated,
    updatedAt: source.updated_at || '',
    searchURL: showingIgnored ? '' : showingRecent ? searchURL(source.recent_search_url) : searchURL(source.search_url),
    attention: all.filter(needsAttention).length,
    ready: all.filter(readyToMerge).length,
    unattached: all.filter((pr) => !pr?.agent_id).length,
    hasDrafts: all.some((pr) => !!pr?.draft),
    recentCount: recent.length,
    recentWindowDays,
    showingRecent,
    showingIgnored,
    ignoredCount: ignoredItems.length,
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
