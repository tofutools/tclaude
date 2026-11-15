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

// The More sheet's entries: every other tab a phone may open, in this order.
// A tab the dashboard hides (no plugins, no usage yet, a peer page that does
// not offer it) is left out by the availability check the island passes in.
export const MORE_TABS = Object.freeze([
  Object.freeze({ tab: 'terminals', label: 'Collab', icon: '🖥', path: '/terminals' }),
  Object.freeze({ tab: 'jobs', label: 'Automations', icon: '⏱', path: '/automations' }),
  Object.freeze({ tab: 'plugins', label: 'Plugins', icon: '🔧', path: '/plugins' }),
  Object.freeze({ tab: 'access', label: 'Access', icon: '🔐', path: '/access' }),
  Object.freeze({ tab: 'usage', label: 'Usage', icon: '📊', path: '/usage' }),
  Object.freeze({ tab: 'diagnostics', label: 'Diagnostics', icon: '🩺', path: '/diagnostics' }),
  // Fleet's nav anchor is always hidden (the desktop reaches it from the
  // node chips), so the router will not route to its path: it is always
  // offered and opened through its anchor (byAnchor), and its view says when
  // there is no fleet.
  Object.freeze({ tab: 'fleet-admin', label: 'Fleet', icon: '🌐', path: '/fleet-admin', byAnchor: true }),
  Object.freeze({ tab: 'config', label: 'Settings', icon: '⚙', path: '/config' }),
]);

const TAB_TITLES = Object.freeze({
  groups: 'Agents',
  messages: 'Mail',
  ...Object.fromEntries(MORE_TABS.map((entry) => [entry.tab, entry.label])),
  'drawing-boards': 'Drawing boards',
  'collab-processes': 'Processes',
  processes: 'Processes',
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
const ROOT_BARS = Object.freeze({
  now: Object.freeze({ node: true, search: true }),
  agents: Object.freeze({ power: true, search: true }),
  mail: Object.freeze({ search: true }),
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
  return {
    approvals,
    elsewhere,
    decisions,
    asking,
    errors,
    working,
    needs: approvals.length + elsewhere + decisions.length + asking.length,
  };
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

