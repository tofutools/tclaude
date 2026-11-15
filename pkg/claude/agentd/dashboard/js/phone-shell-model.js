// phone-shell-model.js -- what the phone shell shows, as plain data: the five
// bottom-nav slots, the More list, the Now view's "what needs you" and the
// Board view's lanes. No DOM, no Preact: phone-shell-island.js renders these.
//
// The phone shell (data-layout="phone", see layout.js) replaces the desktop
// header, tab bar, footer, dock and overlay windows with a top bar and a
// bottom nav: Now · Agents · Mail · Board · More. Now and Board are the shell's
// own views; Agents, Mail and every More entry show a dashboard tab.

import { companionAgents, pendingAccess, pendingApprovals, pendingDecisions } from './companion-model.js';

// The bottom nav. A slot with a `tab` shows that dashboard tab; the others
// are the shell's own views (now, board) or the More sheet.
export const PHONE_SLOTS = Object.freeze([
  Object.freeze({ id: 'now', label: 'Now', icon: '◉' }),
  Object.freeze({ id: 'agents', label: 'Agents', icon: '🤖', tab: 'groups' }),
  Object.freeze({ id: 'mail', label: 'Mail', icon: '✉', tab: 'messages' }),
  Object.freeze({ id: 'board', label: 'Board', icon: '▤' }),
  Object.freeze({ id: 'more', label: 'More', icon: '☰' }),
]);

// The shell's own views: what the content area shows instead of a tab.
export const OWN_VIEWS = Object.freeze(['now', 'board']);

// The More sheet's entries: every other tab a phone may open, in this order,
// each with its scope: what a phone can do there (read-only, or a few things).
// A tab the dashboard hides (no plugins, no usage yet, a peer page that does
// not offer it) is left out by the availability check the island passes in.
export const MORE_TABS = Object.freeze([
  // Collab's strip, rail and drawing tools are desktop things: a phone lists
  // the sessions open there instead (phone-sessions.js).
  Object.freeze({ tab: 'terminals', label: 'Open sessions', icon: '🖥', path: '/terminals', scope: Object.freeze({ kind: 'simp', text: 'list, open' }) }),
  Object.freeze({ tab: 'usage', label: 'Usage', icon: '📊', path: '/usage', scope: Object.freeze({ kind: 'ro', text: 'read-only' }) }),
  Object.freeze({ tab: 'jobs', label: 'Automations', icon: '⏱', path: '/automations', scope: Object.freeze({ kind: 'simp', text: 'on/off, run now' }) }),
  Object.freeze({ tab: 'plugins', label: 'Plugins', icon: '🔧', path: '/plugins', scope: Object.freeze({ kind: 'simp', text: 'on/off, check' }) }),
  Object.freeze({ tab: 'access', label: 'Access', icon: '🔐', path: '/access', scope: Object.freeze({ kind: 'simp', text: 'approve, revoke' }) }),
  Object.freeze({ tab: 'diagnostics', label: 'Diagnostics', icon: '🩺', path: '/diagnostics', scope: Object.freeze({ kind: 'ro', text: 'read-only' }) }),
  Object.freeze({ tab: 'config', label: 'Settings', icon: '⚙', path: '/config', scope: Object.freeze({ kind: 'simp', text: 'a few' }) }),
  // Fleet's nav anchor is always hidden (the desktop reaches it from the
  // node chips), so the router will not route to its path: it is always
  // offered and opened through its anchor (byAnchor), and its view says when
  // there is no fleet.
  Object.freeze({ tab: 'fleet-admin', label: 'Fleet', icon: '🌐', path: '/fleet-admin', byAnchor: true, scope: Object.freeze({ kind: 'ro', text: 'nodes, read-only' }) }),
]);
const TAB_TITLES = Object.freeze({
  groups: 'Agents',
  messages: 'Mail',
  ...Object.fromEntries(MORE_TABS.map((entry) => [entry.tab, entry.label])),
  'drawing-boards': 'Drawing boards',
  'collab-processes': 'Processes',
  // A phone's Runs live in Automations (phone-automations-island.js): the
  // Processes tab is their island, under the Automations title.
  processes: 'Automations',
  map: 'Fleet map',
  'fleet-admin': 'Fleet',
  vegas: 'Vegas',
});

// slotFor is the bottom-nav slot to highlight: the shell's own view when one
// is up, else the slot whose tab is active; any other tab is under More.
export function slotFor(view, activeTab) {
  if (OWN_VIEWS.includes(view)) return view;
  return PHONE_SLOTS.find((slot) => slot.tab === activeTab)?.id || 'more';
}

// ROOT_BARS is what the shell itself puts in a root view's top bar (no page
// pushed), by bottom-nav slot: the node chip, ⏻ for the power actions, and
// 🔍 search. A view adds its own actions through phoneBar (phone-page.js).
// Mail's 🔍 is its own (phone-mail-island.js): it searches the open mailbox.
const ROOT_BARS = Object.freeze({
  now: Object.freeze({ node: true, search: true }),
  agents: Object.freeze({ power: true, search: true }),
});
const NO_BAR = Object.freeze({});

// rootBarFor is the shell's own top bar furniture for a slot.
export function rootBarFor(slot) {
  return ROOT_BARS[slot] || NO_BAR;
}

// titleFor is the top bar's title.
export function titleFor(view, activeTab) {
  if (OWN_VIEWS.includes(view)) return PHONE_SLOTS.find((slot) => slot.id === view).label;
  return TAB_TITLES[activeTab] || 'tclaude';
}

// The agent states the Now view lists, loudest first.
const NEEDS = new Set(['awaiting_permission', 'awaiting_input']);
const WORKING = new Set(['working', 'main_agent_idle', 'restarting']);
const ERRORS = new Set(['error', 'crashed', 'crash loop / backoff']);

// nowView is the Now view's data: what waits on the operator, then what is
// going on.
//
//   approvals  access requests a card can answer here (companion-model's
//              pendingApprovals)
//   elsewhere  how many other pending requests only Messages can answer
//   decisions  open notify-human --option decisions, oldest first
//   asking     agents at a prompt (a question or a permission prompt) with no
//              access request card above: those are already answered there
//   errors     live agents in an error state
//   working    agents doing something
//   needs      the count the Now slot's badge shows
export function nowView(snapshot) {
  const agents = [...companionAgents(snapshot).values()];
  const approvals = pendingApprovals(snapshot);
  const elsewhere = pendingAccess(snapshot).length - approvals.length;
  const decisions = pendingDecisions(snapshot);
  const carded = new Set(approvals.map((request) => request.agent_id).filter(Boolean));
  const byName = (a, b) => a.name.localeCompare(b.name);
  const asking = agents.filter((agent) => agent.online && NEEDS.has(agent.status) && !carded.has(agent.agentId)).sort(byName);
  const errors = agents.filter((agent) => agent.online && ERRORS.has(agent.status)).sort(byName);
  const working = agents.filter((agent) => agent.online && WORKING.has(agent.status)).sort(byName);
  // The rest online: neither asking (a card above) nor at work, most often
  // idle between turns. Grouped, so a group's agents sit together.
  const resting = agents.filter((agent) => agent.online && !NEEDS.has(agent.status) && !ERRORS.has(agent.status) && !WORKING.has(agent.status))
    .sort((a, b) => a.group.localeCompare(b.group) || byName(a, b));
  return {
    approvals,
    elsewhere,
    decisions,
    asking,
    errors,
    working,
    resting,
    online: agents.filter((agent) => agent.online),
    needs: approvals.length + elsewhere + decisions.length + asking.length,
  };
}

// FEW_ONLINE is how many online agents Now lists whole. Above it, All lists
// the ones at work or in error and those heard from recently, and "Show all"
// opens the rest.
export const FEW_ONLINE = 10;

// RECENT_MS is how lately an agent must have called a hook to count as
// recently active in All when there are many agents online.
export const RECENT_MS = 15 * 60 * 1000;

// STUCK_AFTER_MS is how long a busy agent may stay quiet (no hook) before
// Stuck? marks it and counts it.
export const STUCK_AFTER_MS = 15 * 60 * 1000;

// hookAt is when an agent last called a hook, in ms, or null if never.
export function hookAt(agent) {
  const at = Date.parse(agent?.lastHook || '');
  return Number.isFinite(at) ? at : null;
}

// rowState is an agent's state word for its row's pill.
export function rowState(agent) {
  if (ERRORS.has(agent.status)) return 'error';
  if (WORKING.has(agent.status)) return 'working';
  if (NEEDS.has(agent.status)) return 'asking';
  return 'idle';
}

// agentRows is All's rows of agents: { rows: [{ agent, state }], all (every
// online agent is listed), total (how many are online, askers aside) }.
// With few online, all of them; above that, the ones at work or in error
// and the rest heard from (a hook) within RECENT_MS. Agents asking the
// operator are cards in Needs you, always, and not rows here.
export function agentRows(view, { showAll = false, few = FEW_ONLINE, now = Date.now() } = {}) {
  const busy = [...view.errors.map((agent) => ({ agent, state: 'error' })), ...view.working.map((agent) => ({ agent, state: 'working' }))];
  const rest = view.resting.map((agent) => ({ agent, state: 'idle' }));
  const total = busy.length + rest.length;
  const all = showAll || total <= few;
  const recent = ({ agent }) => { const at = hookAt(agent); return at !== null && now - at <= RECENT_MS; };
  return { rows: all ? [...busy, ...rest] : [...busy, ...rest.filter(recent)], all, total };
}

// idleGroups is the Idle chip: the online idle agents by group, each group's
// longest-idle last.
export function idleGroups(view) {
  const groups = new Map();
  for (const agent of view.resting) {
    if (!groups.has(agent.group)) groups.set(agent.group, []);
    groups.get(agent.group).push(agent);
  }
  const latest = (agent) => hookAt(agent) ?? -Infinity;
  return [...groups].map(([group, agents]) => ({ group, agents: agents.sort((a, b) => latest(b) - latest(a)) }));
}

// recentRows is the Recent chip: every online agent, the latest hook first;
// an agent that never called one goes last.
export function recentRows(view) {
  const latest = (agent) => hookAt(agent) ?? -Infinity;
  return [...view.online].sort((a, b) => latest(b) - latest(a) || a.name.localeCompare(b.name))
    .map((agent) => ({ agent, state: rowState(agent) }));
}

// stuckRows is the Stuck? chip: the agents at work, quiet the longest first,
// each marked quiet once STUCK_AFTER_MS has passed without a hook; quiet is
// how many are. One that never called a hook cannot be timed and goes last.
export function stuckRows(view, now = Date.now()) {
  const oldest = (agent) => hookAt(agent) ?? Infinity;
  const rows = [...view.working].sort((a, b) => oldest(a) - oldest(b) || a.name.localeCompare(b.name)).map((agent) => {
    const at = hookAt(agent);
    return { agent, state: 'working', quiet: at !== null && now - at > STUCK_AFTER_MS };
  });
  return { rows, quiet: rows.filter((row) => row.quiet).length };
}

// askerName names whoever put a decision to the operator, as the companion's
// decide queue does.
export function askerName(snapshot, agents = companionAgents(snapshot)) {
  return (decision) => agents.get(decision.from_agent)?.name || decision.from_title || '';
}

// boardGroups lists the groups that have a board with live tasks, busiest
// first by what waits on the operator, then by name.
export function boardGroups(context) {
  return (context?.groups || [])
    .filter((entry) => entry?.group && (entry.counts?.live || 0) > 0)
    .map((entry) => ({ group: entry.group, live: entry.counts.live, waiting: entry.counts.waiting_on_you || 0 }))
    .sort((a, b) => (b.waiting - a.waiting) || a.group.localeCompare(b.group));
}

