// phone-agents-model.js -- the phone Agents view as plain data: the groups and
// their agents, one row per agent with what a phone shows of it (state, role,
// harness, context, branch or PR and its checks), the group chips above, and
// what the spawn sheet offers. No DOM, no rendering: phone-agents-island.js
// renders these.

import { VARIANT_ORDER, memberVariant } from './group-activity.js';
import { isAwayPlaceholder } from './agent-presence.js';
import { signal } from '@preact/signals';
import { normalizeSpawnName } from './agent-spawn-model.js';
import { buildGroupTree } from './groups-view-model.js';
import { incompleteChecksText } from './pr-checks-hover.js';

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
// into one word: failing wins over running, running over passing. A summary
// read from only part of the checks (too many to read) is 'incomplete': no
// verdict either way.
export function ciState(checks) {
  if (!checks) return '';
  if (checks.failed) return 'failing';
  if (checks.incomplete) return 'incomplete';
  if (checks.pending) return 'running';
  if (checks.passed) return 'passing';
  return '';
}

// agentPR is the PR a row shows: the current branch's, else the one the
// agent started on.
export function agentPR(member) {
  if (member.branch_pr_number && member.branch_pr_url) {
    return { number: member.branch_pr_number, url: member.branch_pr_url, state: member.branch_pr_state || '', title: member.branch_pr_title || '', ci: ciState(member.branch_checks), ciNote: incompleteChecksText(member.branch_checks) };
  }
  if (member.startup_pr_number && member.startup_pr_url) {
    return { number: member.startup_pr_number, url: member.startup_pr_url, state: member.startup_pr_state || '', title: member.startup_pr_title || '', ci: ciState(member.startup_checks), ciNote: incompleteChecksText(member.startup_checks) };
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

// groupEntry is one group as the Agents view shows it: { key, name, group,
// shown, offline, tree, summary, live, total }: shown is the open list
// (online agents, and crashed ones, which always need a look), offline the
// folded rest, both loudest first; tree nests workers under their leads;
// live counts the online ones. Placeholders for agents away on another node
// are left out. group is the snapshot's group, null for the agents in none.
// Without offline (Show offline off) only the open list is kept, and an
// agent waking up: an offline agent is left out, a crashed one never.
export function groupEntry(key, name, group, members, { offline: withOffline = true } = {}) {
  const open = (row) => row.online || row.variant === 'crashed';
  const rows = (members || []).filter((m) => m && !isAwayPlaceholder(m)).map((m) => agentRow(m, group ? name : ''))
    .filter((row) => withOffline || open(row) || row.waking);
  const shown = rows.filter(open).sort(byLoudness);
  const offline = rows.filter((row) => !open(row)).sort(byLoudness);
  return {
    key, name, group, shown, offline,
    tree: agentTree([...shown, ...offline]),
    summary: stateSummary(rows),
    live: rows.filter((row) => row.online).length,
    total: rows.length,
  };
}

// agentGroups lists the groups that have agents (groupEntry), in the
// operator's group order (order is sortGroupsByPref over the dashboard
// prefs), then the agents in no group. offline is Show offline.
export function agentGroups(snapshot, order = (groups) => groups, { offline = true } = {}) {
  const out = [];
  const add = (entry) => { if (entry.total) out.push(entry); };
  for (const group of order(snapshot?.groups || [])) add(groupEntry(groupKey(group.name), group.name, group, group.members, { offline }));
  add(groupEntry(UNGROUPED, 'Ungrouped', null, snapshot?.ungrouped, { offline }));
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

// metaParts are a row's second line: role, harness, then the PR or the
// branch. Its context is the meter by its pill (context-meter.js). An offline agent's ends in "resume ›", what its tap does.
export function metaParts(row) {
  const parts = [row.role, row.harness];
  if (!row.online) parts.push(row.waking ? 'waking…' : 'resume ›');
  else if (row.pr) parts.push({ pr: row.pr });
  else if (row.branch) parts.push(row.branch);
  return parts.filter(Boolean);
}

// spawnChoices is what the phone's spawn sheet offers: the groups an agent
// can be spawned into, and the spawn profiles, each group with
// the profile its spawns get when none is picked.
export function spawnChoices(snapshot) {
  // An archived group refuses spawns (the daemon answers 409).
  const groups = (snapshot?.groups || []).filter((g) => g?.name && !g.archived)
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

// TREE_FOLD_PREF is where the Agents view keeps a group folded ('1') or
// unfolded ('0'); with neither, a group is folded when nothing in it is
// online (its own agents or its subgroups').
export const TREE_FOLD_PREF = 'tclaude.dash.phone.tree.fold.';
export const treeFoldPref = (node) => TREE_FOLD_PREF + (node.group ? node.name : UNGROUPED);

// TREE_FOLD is the view's fold, as the groups list (phone-agents-island.js
// AgentGroupsList) takes it: pref(node) is the remembered fold's key,
// folded(node, saved) whether the group is folded given what was remembered
// ('1', '0' or null), and remember(prefs, node, folded) remembers a fold.
export const TREE_FOLD = Object.freeze({
  pref: treeFoldPref,
  folded: (node, saved) => saved === '1' || (saved !== '0' && !!node.dim),
  remember: (prefs, node, folded) => prefs.setItem(treeFoldPref(node), folded ? '1' : '0'),
});

// The Agents view's settings, from its View sheet (the header's sliders),
// per viewer like the folds: layout ('tree' nests subgroups and puts workers
// under their lead, 'list' is one level), offline (offline agents, and the
// groups with nobody online), empty (groups with no agents), archived
// (archived groups, last) and order (how groups are ordered: ORDERS).
// Each is kept only when it is off its default (VIEW_DEFAULTS); OFFLINE_PREF
// and ARCHIVED_PREF keep the values they had before there was a sheet.
export const LAYOUT_PREF = 'tclaude.dash.phone.agents.layout';
export const OFFLINE_PREF = 'tclaude.dash.phone.agents.offline';
export const EMPTY_PREF = 'tclaude.dash.phone.agents.empty';
export const ARCHIVED_PREF = 'tclaude.dash.phone.agents.archived';
export const ORDER_PREF = 'tclaude.dash.phone.agents.order';
export const LAYOUTS = Object.freeze(['tree', 'list']);
// ORDERS: the operator's group order (as on the desktop), what needs them
// first (errors, approvals, questions), the most active first (working),
// or by name.
export const ORDERS = Object.freeze(['yours', 'needs', 'active', 'name']);
export const VIEW_DEFAULTS = Object.freeze({ layout: 'tree', offline: true, empty: false, archived: false, order: 'yours' });

export function viewSettings(prefs) {
  const layout = prefs.getItem(LAYOUT_PREF);
  const order = prefs.getItem(ORDER_PREF);
  return {
    layout: LAYOUTS.includes(layout) ? layout : VIEW_DEFAULTS.layout,
    offline: prefs.getItem(OFFLINE_PREF) !== '0',
    empty: prefs.getItem(EMPTY_PREF) === '1',
    archived: prefs.getItem(ARCHIVED_PREF) === '1',
    order: ORDERS.includes(order) ? order : VIEW_DEFAULTS.order,
  };
}

// saveViewSettings keeps settings, forgetting each one at its default.
export function saveViewSettings(prefs, settings) {
  const keep = (key, value, at) => { if (at) prefs.removeItem(key); else prefs.setItem(key, value); };
  keep(LAYOUT_PREF, settings.layout, settings.layout === VIEW_DEFAULTS.layout);
  keep(OFFLINE_PREF, '0', settings.offline);
  keep(EMPTY_PREF, '1', !settings.empty);
  keep(ARCHIVED_PREF, '1', !settings.archived);
  keep(ORDER_PREF, settings.order, settings.order === VIEW_DEFAULTS.order);
}

// atDefaults is whether every setting is at its default: the header's
// sliders carry a dot when not.
export const atDefaults = (settings) => Object.keys(VIEW_DEFAULTS).every((key) => settings[key] === VIEW_DEFAULTS[key]);

// groupsTree is every group as the desktop's Groups tree nests them
// (buildGroupTree): a node is a groupEntry with children (its subgroups),
// archived, empty (no agents) and dim (nothing online in it or under it).
// At every level the groups with someone online come first, then those with
// only offline agents, then empty ones, then archived ones, each in the
// operator's order (treeRank; a group counts what is under it too). The
// agents in no group rank like a group. Without showArchived the archived
// groups are left out, and a subgroup that is not archived takes its
// archived parent's place. archived counts them either way, count the
// groups that are not. Without offline (Show offline off) a group with
// nobody online in it or under it (empty, or only offline agents) is left
// out too, unless an agent in it is waking; count counts what is left.
export function groupsTree(snapshot, order = (groups) => groups, { showArchived = false, offline = true } = {}) {
  const groups = order((snapshot?.groups || []).filter((g) => g?.name));
  const toNode = (node) => {
    const group = node.group;
    const children = node.children.map(toNode);
    const entry = groupEntry(groupKey(group.name), group.name, group, group.members, { offline });
    // Empty means no agents at all, not none left after Show offline.
    const empty = !(group.members || []).some((m) => m && !isAwayPlaceholder(m));
    return { ...entry, children, archived: !!group.archived, empty };
  };
  // dim and populated are counted over what is shown: a hidden archived
  // subgroup's agents do not light its parent. A crashed agent needs a look,
  // as in Active, so it lights its group too.
  const lit = (n, children) => {
    const needsLook = n.live > 0 || n.shown.length > 0;
    return {
      ...n, children,
      dim: !needsLook && children.every((child) => child.dim),
      populated: n.total > 0 || children.some((child) => child.populated),
    };
  };
  const ranked = (nodes) => nodes.map((n, i) => [treeRank(n), i, n]).sort((a, b) => (a[0] - b[0]) || (a[1] - b[1])).map(([, , n]) => n);
  const keep = (nodes) => ranked(nodes.flatMap((n) => {
    if (n.archived && !showArchived) return keep(n.children);
    const node = lit(n, keep(n.children));
    // Without offline, what is left in a group is listed (online, crashed or
    // waking), so a group goes when nothing is left in it or under it.
    return !offline && !node.populated ? [] : [node];
  }));
  const all = buildGroupTree(groups, () => true).map(toNode);
  const loose = groupEntry(UNGROUPED, 'Ungrouped', null, snapshot?.ungrouped, { offline });
  if (loose.total) all.push({ ...loose, children: [], archived: false, empty: false });
  const nodes = keep(all);
  const archived = groups.filter((g) => g.archived).length;
  const count = offline ? groups.length - archived : treeNodes(nodes).filter((n) => n.group && !n.archived).length;
  return { nodes, archived, count };
}

// treeRank is where a node sits among its siblings: someone online in it
// or under it (0), only offline agents (1), no agents (2), archived (3).
export function treeRank(node) {
  if (node.archived) return 3;
  if (!node.dim) return 0;
  return node.populated ? 1 : 2;
}

// NEEDS_TONES are the pills that need the operator: an error (or crash), an
// approval, a question.
const NEEDS_TONES = Object.freeze(['err', 'key', 'ask']);
const counted = (n, tones) => treeNodes([n]).reduce((sum, x) => sum + x.summary.filter((s) => tones.includes(s.tone)).reduce((k, s) => k + s.count, 0), 0);

// agentsLayout is what the Agents view lists under its settings
// (viewSettings): the groups as groupsTree has them (offline, archived),
// without the empty ones unless asked, as the tree or one level (each
// subgroup its own section, each worker its own row), ordered by
// settings.order at every level, archived ones last.
export function agentsLayout(snapshot, order = (groups) => groups, settings = VIEW_DEFAULTS) {
  const { nodes } = groupsTree(snapshot, order, { showArchived: settings.archived, offline: settings.offline });
  const flat = settings.layout === 'list'
    ? treeNodes(nodes).map((n) => ({
      ...n, children: [], tree: [...n.shown, ...n.offline].map((row) => ({ row, kids: [] })),
      dim: !(n.live > 0 || n.shown.length > 0), populated: n.total > 0,
    }))
      // Without offline a group with nobody listed goes: the tree kept it
      // only for a subgroup, which is its own section here.
      .filter((n) => settings.offline || n.populated)
    : nodes;
  const prune = (list) => list.flatMap((n) => {
    const children = prune(n.children);
    return !settings.empty && n.empty && !children.length ? [] : [{ ...n, children }];
  });
  const at = new Map(order((snapshot?.groups || []).filter((g) => g?.name)).map((g, i) => [groupKey(g.name), i]));
  const place = (n) => (n.group ? at.get(n.key) ?? Infinity : Infinity);
  const by = {
    yours: (a, b) => place(a) - place(b),
    needs: (a, b) => (counted(b, NEEDS_TONES) - counted(a, NEEDS_TONES)) || (treeRank(a) - treeRank(b)) || (place(a) - place(b)),
    active: (a, b) => (treeRank(a) - treeRank(b)) || (counted(b, ['work']) - counted(a, ['work'])) || (place(a) - place(b)),
    name: (a, b) => (!a.group - !b.group) || a.name.localeCompare(b.name),
  }[settings.order] || ((a, b) => place(a) - place(b));
  const sorted = (list) => list.map((n) => ({ ...n, children: sorted(n.children) }))
    .sort((a, b) => (a.archived - b.archived) || by(a, b) || 0);
  return sorted(prune(flat));
}

// treeNodes is every node of a tree, each before its subgroups.
export function treeNodes(nodes) {
  return nodes.flatMap((n) => [n, ...treeNodes(n.children)]);
}

// treeTabs are All groups' chips: All, then each group in the tree's order.
export function treeTabs(nodes) {
  const flat = treeNodes(nodes);
  const all = flat.reduce((sum, n) => ({ online: sum.online + n.live, total: sum.total + n.total }), { online: 0, total: 0 });
  return [{ key: ALL, label: 'All', ...all }, ...flat.map((n) => ({ key: n.key, label: n.name, online: n.live, total: n.total }))];
}

// shownTree is what a filter shows of the tree: each picked group with its
// subgroups, in the tree's order (one picked under another picked one shows
// once, under it); the whole tree when none is picked (or every one picked
// went away).
export function shownTree(nodes, filter) {
  const keys = new Set((filter || []).filter((key) => treeNodes(nodes).some((n) => n.key === key)));
  if (!keys.size) return nodes;
  const pick = (list) => list.flatMap((n) => (keys.has(n.key) ? [n] : pick(n.children)));
  return pick(nodes);
}

// groupPage is one group's page (the group's name tapped): its groupEntry
// with crumbs (its ancestors' names, outermost first, then its own) and
// children (its subgroups, archived ones last), or null when the snapshot
// has no such group.
export function groupPage(snapshot, name, order = (groups) => groups) {
  const groups = (snapshot?.groups || []).filter((g) => g?.name);
  const group = groups.find((g) => g.name === name);
  if (!group) return null;
  const byName = new Map(groups.map((g) => [g.name, g]));
  const crumbs = [group.name];
  for (let at = byName.get(group.parent); at && !crumbs.includes(at.name); at = byName.get(at.parent)) crumbs.unshift(at.name);
  const { nodes } = groupsTree(snapshot, order, { showArchived: true });
  const node = treeNodes(nodes).find((n) => n.group && n.name === name);
  return { ...(node || groupEntry(groupKey(name), name, group, group.members)), children: node?.children || [], crumbs };
}

// groupSummaryLine is the page's first line: how much of the group is up.
export function groupSummaryLine(entry) {
  if (!entry.total) return 'No agents yet';
  if (!entry.live) return 'Everything offline';
  if (entry.live === entry.total) return 'Everything online';
  return `${entry.live} of ${entry.total} online`;
}

// findAgent is where an agent (its agent id or conversation id) sits in the
// Agents view, offline agents and archived groups counted:
// { row, groupKey, path, archived } with path the keys from the outermost
// group down to its own, and archived whether its group is, or null when no
// group holds it (retired, deleted, or never here). An agent in several
// groups is found in the first one the tree lists.
export function findAgent(snapshot, id, order = (groups) => groups) {
  if (!snapshot || !id) return null;
  const has = (g) => [...g.shown, ...g.offline].find((row) => row.agent === id || row.conv === id || row.member?.agent_id === id);
  const walk = (nodes, above) => {
    for (const node of nodes) {
      const row = has(node);
      if (row) return { row, groupKey: node.key, path: [...above, node.key], archived: node.archived };
      const deeper = walk(node.children, [...above, node.key]);
      if (deeper) return deeper;
    }
    return null;
  };
  return walk(groupsTree(snapshot, order, { showArchived: true, offline: true }).nodes, []);
}
