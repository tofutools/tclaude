// phone-shell-model.js -- what the phone shell shows, as plain data: the five
// bottom-nav slots, the More list, the Now view's "what needs you" and the
// Board view's lanes. No DOM, no Preact: phone-shell-island.js renders these.
//
// The phone shell (data-layout="phone", see layout.js) replaces the desktop
// header, tab bar, footer, dock and overlay windows with a top bar and a
// bottom nav: Now · Agents · Mail · Board · More. Now and Board are the shell's
// own views; Agents, Mail and every More entry show a dashboard tab.

import { companionAgents, pendingAccess, pendingApprovals, pendingDecisions } from './companion-model.js';
import { taskRowState } from './taskboard-context.js';

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

// The Board view's lanes, in reading order, each from the row states
// taskRowState derives (the same states the desktop lanes use). A custom
// column's tasks read as in progress here: the phone does not lay out columns.
export const BOARD_LANES = Object.freeze([
  Object.freeze({ id: 'waiting', label: 'Waiting on you', states: ['waiting'] }),
  Object.freeze({ id: 'blocked', label: 'Blocked', states: ['blocked', 'stalled'] }),
  Object.freeze({ id: 'progress', label: 'In progress', states: ['in-progress'] }),
  Object.freeze({ id: 'next', label: 'Next', states: ['open', 'queued'] }),
  Object.freeze({ id: 'ideas', label: 'Ideas', states: ['idea'] }),
]);

// boardGroups lists the groups that have a board with live tasks, busiest
// first by what waits on the operator, then by name.
export function boardGroups(context) {
  return (context?.groups || [])
    .filter((entry) => entry?.group && (entry.counts?.live || 0) > 0)
    .map((entry) => ({ group: entry.group, live: entry.counts.live, waiting: entry.counts.waiting_on_you || 0 }))
    .sort((a, b) => (b.waiting - a.waiting) || a.group.localeCompare(b.group));
}

// boardLanes splits one group's open tasks into BOARD_LANES, keeping the
// board's own order inside each lane. Lanes with no tasks are left out.
export function boardLanes(entry) {
  const lanes = BOARD_LANES.map((lane) => ({ ...lane, tasks: [] }));
  for (const task of entry?.tasks || []) {
    let state = taskRowState(task);
    if (state.startsWith('col:')) state = 'in-progress';
    lanes.find((lane) => lane.states.includes(state))?.tasks.push(task);
  }
  return lanes.filter((lane) => lane.tasks.length > 0);
}

// taskPath is the dashboard location that opens a task: the Groups tab with
// the task's dialog over it (the same ?task= deep link the desktop uses).
export function taskPath(task, group) {
  return `/?task=${encodeURIComponent(`${task.id}:${group}`)}`;
}
