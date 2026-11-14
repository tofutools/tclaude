// phone-agents-model.js -- the phone Agents view as plain data: the groups and
// their agents, one row per agent with what a phone shows of it (state, role,
// harness, context, branch or PR and its checks), and the group tabs above.
// No DOM, no Preact: phone-agents-island.js renders these.

import { VARIANT_ORDER, memberVariant } from './group-activity.js';
import { isAwayPlaceholder } from './agent-presence.js';

export const ALL = 'all';
export const UNGROUPED = '-ungrouped';

// A group's tab key: prefixed, so no group name collides with All or
// Ungrouped.
export const groupKey = (name) => `g:${name}`;

// The state tag a row shows, by memberVariant.
export const VARIANT_LABELS = Object.freeze({
  error: 'error',
  asking: 'asking',
  working: 'working',
  idle: 'idle',
  crashed: 'crashed',
  offline: 'offline',
});

// ciState folds a PR's check summary ({ passed, failed, pending, skipped })
// into one word: failing wins over running, running over passing.
export function ciState(checks) {
  if (!checks) return '';
  if (checks.failed) return 'failing';
  if (checks.pending) return 'running';
  if (checks.passed) return 'passing';
  return '';
}

// agentPR is the PR a row shows: the current branch's, else the one the
// agent started on.
function agentPR(member) {
  if (member.branch_pr_number && member.branch_pr_url) {
    return { number: member.branch_pr_number, url: member.branch_pr_url, state: member.branch_pr_state || '', title: member.branch_pr_title || '', ci: ciState(member.branch_checks) };
  }
  if (member.startup_pr_number && member.startup_pr_url) {
    return { number: member.startup_pr_number, url: member.startup_pr_url, state: member.startup_pr_state || '', title: member.startup_pr_title || '', ci: ciState(member.startup_checks) };
  }
  return null;
}

// agentRow is one agent as the phone shows it. agent is the selector the row
// actions take (the stable agent id, else the conversation id), as on the
// desktop rows.
export function agentRow(member, group = '') {
  const state = member?.state || {};
  const pct = Number(state.context_pct);
  const variant = memberVariant(member);
  return {
    key: member.agent_id || member.conv_id,
    agent: member.agent_id || member.conv_id,
    conv: member.conv_id || '',
    name: member.title || member.name || String(member.conv_id || member.agent_id || '').slice(0, 12),
    role: member.role || '',
    harness: state.harness || member.harness || '',
    group,
    online: member.online === true,
    waking: member.waking === true,
    variant,
    detail: variant === 'asking' || variant === 'error' ? String(state.status_detail || '') : '',
    ctx: Number.isFinite(pct) && state.context_pct !== undefined && state.context_pct !== null ? Math.round(pct) : null,
    branch: member.branch || member.startup_branch || '',
    cwd: member.current_dir || member.startup_dir || state.cwd || '',
    pr: agentPR(member),
    member,
  };
}

// The order rows sit in: what needs a look first (VARIANT_ORDER, loudest
// first), then by name.
const rank = (row) => VARIANT_ORDER.indexOf(row.variant);
export function byLoudness(a, b) {
  return (rank(a) - rank(b)) || a.name.localeCompare(b.name);
}

// agentGroups lists the groups that have agents, in the operator's group
// order (order is sortGroupsByPref over the dashboard prefs), then the agents
// in no group. Placeholders for agents away on another node are left out.
// Each group: { key, name, group, shown, offline, live, total }: shown is the
// open list (online agents, and crashed ones, which always need a look),
// offline the folded rest, both loudest first; live counts the online ones.
export function agentGroups(snapshot, order = (groups) => groups) {
  const out = [];
  const add = (key, name, group, members) => {
    const rows = (members || []).filter((m) => m && !isAwayPlaceholder(m)).map((m) => agentRow(m, group ? name : ''));
    if (!rows.length) return;
    const open = (row) => row.online || row.variant === 'crashed';
    out.push({
      key, name, group,
      shown: rows.filter(open).sort(byLoudness),
      offline: rows.filter((row) => !open(row)).sort(byLoudness),
      live: rows.filter((row) => row.online).length,
      total: rows.length,
    });
  };
  for (const group of order(snapshot?.groups || [])) add(groupKey(group.name), group.name, group, group.members);
  add(UNGROUPED, 'Ungrouped', null, snapshot?.ungrouped);
  return out;
}

// groupTabs are the tabs above the list: All, then each group with its
// online / total count.
export function groupTabs(groups) {
  const all = groups.reduce((sum, g) => ({ online: sum.online + g.live, total: sum.total + g.total }), { online: 0, total: 0 });
  return [{ key: ALL, label: 'All', ...all }, ...groups.map((g) => ({ key: g.key, label: g.name, online: g.live, total: g.total }))];
}

// shownGroups is what a tab shows; an unknown tab (a group that went away)
// shows All.
export function shownGroups(groups, tab) {
  if (tab === ALL) return groups;
  const picked = groups.filter((g) => g.key === tab);
  return picked.length ? picked : groups;
}

// findRow finds an agent's row by its key, with its group: in the group
// groupKey names when given (an agent can sit in several), else the first.
export function findRow(groups, key, inGroup = '') {
  for (const g of groups) {
    if (inGroup && g.key !== inGroup) continue;
    const row = [...g.shown, ...g.offline].find((r) => r.key === key);
    if (row) return { row, group: g };
  }
  return null;
}
