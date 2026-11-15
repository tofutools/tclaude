// phone-agents-model.js -- the phone Agents view as plain data: the groups and
// their agents, one row per agent with what a phone shows of it (state, role,
// harness, context, branch or PR and its checks), the group chips above, and
// what the spawn sheet offers. No DOM, no rendering: phone-agents-island.js
// renders these.

import { VARIANT_ORDER, memberVariant } from './group-activity.js';
import { isAwayPlaceholder } from './agent-presence.js';
import { signal } from '@preact/signals';
import { normalizeSpawnName } from './agent-spawn-model.js';

export const ALL = 'all';
export const UNGROUPED = '-ungrouped';

// agentsFilter is the groups the Agents view shows: their keys
// (groupKey(name) or UNGROUPED), none for every group. Module state, so the
// view keeps it across tab switches and the phone search (phone-search.js)
// can open the view on one group.
export const agentsFilter = signal([]);

// toggleFilter is the filter after a tap on a chip: All clears it, a group's
// chip adds or takes out that group.
export function toggleFilter(filter, key) {
  if (key === ALL) return [];
  return filter.includes(key) ? filter.filter((k) => k !== key) : [...filter, key];
}

// FOLD_PREF is where a phone keeps a group section folded ('1'), in the
// daemon's dashboard prefs (prefs.js) as the desktop keeps its own group
// folds, under keys of its own so a phone's fold never folds the desktop's
// tree. By group name; the agents in no group are under UNGROUPED.
export const FOLD_PREF = 'tclaude.dash.phone.fold.';
export const foldPref = (group) => FOLD_PREF + (group.group ? group.name : UNGROUPED);

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

// The pill a row wears, by what the agent waits on: an approval (amber, 🔑),
// a question (purple, ❓), an error (red, 💥), else its state word. tone is a
// phone-ui.js StatePill tone, badge the corner glyph on its avatar.
export function agentPill(member, variant = memberVariant(member)) {
  const status = member?.state?.status || '';
  if (member?.waking) return { text: 'waking', tone: 'idle', badge: '' };
  if (variant === 'asking' && status === 'awaiting_permission') return { text: 'approval', tone: 'key', badge: '🔑' };
  if (variant === 'asking') return { text: 'asking', tone: 'ask', badge: '❓' };
  if (variant === 'error') return { text: 'error', tone: 'err', badge: '💥' };
  if (variant === 'crashed') return { text: 'crashed', tone: 'err', badge: '💥' };
  if (variant === 'working') return { text: 'working', tone: 'work', badge: '' };
  if (variant === 'offline') return { text: 'offline', tone: 'off', badge: '' };
  return { text: 'idle', tone: 'idle', badge: '' };
}

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
    pill: agentPill(member, variant),
    detail: variant === 'asking' || variant === 'error' ? String(state.status_detail || '') : '',
    ctx: Number.isFinite(pct) && state.context_pct !== undefined && state.context_pct !== null ? Math.round(pct) : null,
    branch: member.branch || member.startup_branch || '',
    cwd: member.current_dir || member.startup_dir || state.cwd || '',
    pr: agentPR(member),
    // The agent that spawned this one (snapshot spawned_by, agent_lineage),
    // or '' (a human added it, or it is a peer's).
    parent: member.spawned_by || '',
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
    const shown = rows.filter(open).sort(byLoudness);
    const offline = rows.filter((row) => !open(row)).sort(byLoudness);
    out.push({
      key, name, group, shown, offline,
      tree: agentTree([...shown, ...offline]),
      summary: stateSummary(rows),
      live: rows.filter((row) => row.online).length,
      total: rows.length,
    });
  };
  for (const group of order(snapshot?.groups || [])) add(groupKey(group.name), group.name, group, group.members);
  add(UNGROUPED, 'Ungrouped', null, snapshot?.ungrouped);
  return out;
}

// agentTree nests a group's workers under the agent that spawned them, one
// level: [{ row, kids }] in the list's order (loudest first, offline last),
// each lead's kids in that order too. A worker whose spawner is in the group
// goes under that spawner's own top-level ancestor there, so a worker's
// workers sit flat under the same lead. One whose spawner is elsewhere (another
// group, retired, a peer's, or none) stays at the top level.
export function agentTree(rows) {
  const byAgent = new Map(rows.map((row) => [row.agent, row]));
  // A chain that loops (corrupt lineage) has no top: the row stays at the
  // top level itself, so nothing drops out of the list.
  const rootOf = (row) => {
    let at = row;
    const seen = new Set([at.agent]);
    while (at.parent && byAgent.has(at.parent)) {
      if (seen.has(at.parent)) return row;
      at = byAgent.get(at.parent);
      seen.add(at.agent);
    }
    return at;
  };
  const kids = new Map();
  const top = [];
  for (const row of rows) {
    const root = rootOf(row);
    if (root === row) top.push(row);
    else kids.set(root.agent, [...(kids.get(root.agent) || []), row]);
  }
  // A lead sits where its loudest agent would, itself or a worker: an offline
  // or idle lead must not take an errored or asking worker down the list.
  // Ties keep the list's order.
  const at = new Map(rows.map((row, i) => [row, i]));
  const nodes = top.map((row) => {
    const own = kids.get(row.agent) || [];
    return { row, kids: own, loudest: Math.min(...[row, ...own].map((r) => at.get(r))) };
  });
  nodes.sort((a, b) => a.loudest - b.loudest);
  return nodes.map(({ row, kids: own }) => ({ row, kids: own }));
}

// SUMMARY_ORDER is how a group's state summary lists its pills: what needs a
// look first, then what is busy, then what rests.
const SUMMARY_ORDER = Object.freeze(['error', 'crashed', 'approval', 'asking', 'working', 'waking', 'idle', 'offline']);

// stateSummary counts a group's agents by the pill they wear, loudest first:
// [{ text, tone, count }], e.g. 1 error · 3 working · 2 idle.
export function stateSummary(rows) {
  const counts = new Map();
  for (const row of rows) {
    const at = counts.get(row.pill.text) || { text: row.pill.text, tone: row.pill.tone, count: 0 };
    at.count += 1;
    counts.set(row.pill.text, at);
  }
  const rank = (text) => (SUMMARY_ORDER.includes(text) ? SUMMARY_ORDER.indexOf(text) : SUMMARY_ORDER.length);
  return [...counts.values()].sort((a, b) => rank(a.text) - rank(b.text));
}

// groupTabs are the chips above the list: All, then each group, with how
// many agents each has.
export function groupTabs(groups) {
  const all = groups.reduce((sum, g) => ({ online: sum.online + g.live, total: sum.total + g.total }), { online: 0, total: 0 });
  return [{ key: ALL, label: 'All', ...all }, ...groups.map((g) => ({ key: g.key, label: g.name, online: g.live, total: g.total }))];
}

// metaParts are a row's second line: role, harness, context, then the PR or
// the branch. An offline agent's ends in "resume ›", what its tap does.
export function metaParts(row) {
  const parts = [row.role, row.harness];
  if (row.ctx !== null && row.online) parts.push(`ctx ${row.ctx}%`);
  if (!row.online) parts.push(row.waking ? 'waking…' : 'resume ›');
  else if (row.pr) parts.push({ pr: row.pr });
  else if (row.branch) parts.push(row.branch);
  return parts.filter(Boolean);
}

// spawnChoices is what the phone's spawn sheet offers: the groups an agent
// can be spawned into, and the spawn profiles, each group with
// the profile its spawns get when none is picked.
export function spawnChoices(snapshot) {
  const groups = (snapshot?.groups || []).filter((g) => g?.name)
    .map((g) => ({ name: g.name, profile: g.default_profile || snapshot?.spawn_profile_default || '' }));
  const profiles = (snapshot?.profiles || []).filter((p) => p?.name && !p.disabled).map((p) => p.name);
  return { groups, profiles };
}

// spawnBody is the request the spawn sheet sends (POST
// /api/groups/<group>/spawn): the prompt, the name when one is given (as the
// desktop form normalizes it) and a profile when one is picked. What it
// leaves out the daemon fills from the group's default profile and
// directory, as for `tclaude agent spawn`, the profile's agent name among it.
export function spawnBody({ prompt = '', name = '', profile = '' } = {}) {
  const body = { initial_message: String(prompt) };
  const named = normalizeSpawnName(name);
  if (named) body.name = named;
  if (profile) body.profile = profile;
  return body;
}

// liveFilter is the filter without the groups that went away.
export function liveFilter(groups, filter) {
  return (filter || []).filter((key) => groups.some((g) => g.key === key));
}

// shownGroups is what a filter shows, in the groups' order: every group when
// none is picked (or every one picked went away).
export function shownGroups(groups, filter) {
  const keys = liveFilter(groups, filter);
  return keys.length ? groups.filter((g) => keys.includes(g.key)) : groups;
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
