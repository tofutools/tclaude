// phone-shell-model.js -- what the phone shell shows, as plain data: the five
// bottom-nav slots, the More list, the Now view's "what needs you" and the
// Board view's lanes. No DOM, no Preact: phone-shell-island.js renders these.
//
// The phone shell (data-layout="phone", see layout.js) replaces the desktop
// header, tab bar, footer, dock and overlay windows with a top bar and a
// bottom nav: Now · Agents · Mail · Board · More. Now and Board are the shell's
// own views; Agents, Mail and every More entry show a dashboard tab.

import { companionAgents, pendingAccess, pendingApprovals, pendingDecisions } from './companion-model.js';
import { agentPR } from './phone-agents-model.js';

// The bottom nav. A slot with a `tab` shows that dashboard tab; the others
// are the shell's own views (now, board) or the More sheet.
export const PHONE_SLOTS = Object.freeze([
  Object.freeze({ id: 'now', label: 'Now', icon: '◉' }),
  // The Groups slot's id stays 'agents' (internal): it shows the groups tab.
  Object.freeze({ id: 'agents', label: 'Groups', icon: 'group', tab: 'groups' }),
  Object.freeze({ id: 'mail', label: 'Mail', icon: '✉\uFE0E', tab: 'messages' }),
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
  groups: 'Groups',
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
// pushed), by bottom-nav slot: the node chip and 🔍 search. Powering every
// agent on or off is in ☰ More. A view adds its own actions through phoneBar (phone-page.js).
// Mail's 🔍 is its own (phone-mail-island.js): it searches the open mailbox.
const ROOT_BARS = Object.freeze({
  now: Object.freeze({ node: true, search: true }),
  agents: Object.freeze({ search: true }),
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

// CONTEXT_HIGH_PCT is how full an agent's context window must be for a Now
// row's label to say so. Only that: a full context is no problem, as the
// harness compacts or the agent reincarnates on its own, so it is not in
// Problems; the meter by the pill (context-meter.js) shows it.
export const CONTEXT_HIGH_PCT = 85;

// agentFacts is, per agent id, what Now reads beyond companionAgents: how
// full its context is (null if not reported), the state its context meter
// reads (context-meter.js), and its PR (phone-agents-model agentPR: number,
// state, ci). It keeps the same membership as
// companionAgents: the first, unless a later one is online and it is not.
function agentFacts(snapshot) {
  const out = new Map();
  const add = (member) => {
    const id = member?.agent_id;
    const prev = id && out.get(id);
    if (!id || (prev && (prev.online || member.online !== true))) return;
    const pct = Number(member.state?.context_pct);
    out.set(id, { online: member.online === true, ctx: member.state?.context_pct != null && Number.isFinite(pct) ? Math.round(pct) : null, ctxState: member.state || {}, pr: agentPR(member) });
  };
  for (const group of snapshot?.groups || []) for (const member of group?.members || []) add(member);
  for (const member of [...(snapshot?.agents || []), ...(snapshot?.ungrouped || [])]) add(member);
  return out;
}

// contextHigh reports whether an agent's context is at CONTEXT_HIGH_PCT or
// over.
export const contextHigh = (agent) => agent?.ctx !== null && agent?.ctx !== undefined && agent.ctx >= CONTEXT_HIGH_PCT;

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
//
// errors, working and resting list the most recently active first (the
// latest hook; never heard from last), so the agent heard from a minute ago
// is not buried under ones idle since yesterday. With byGroup (Now's View
// sheet, nowSettings) each is by group A–Z first, then most recent first.
export function nowView(snapshot, { byGroup = false } = {}) {
  const facts = agentFacts(snapshot);
  const archived = new Set((snapshot?.groups || []).filter((group) => group?.archived).map((group) => group.name));
  const agents = [...companionAgents(snapshot).values()].map((agent) => {
    const { ctx = null, ctxState = null, pr = null } = facts.get(agent.agentId) || {};
    return { ...agent, ctx, ctxState, pr };
  });
  const approvals = pendingApprovals(snapshot);
  const elsewhere = pendingAccess(snapshot).length - approvals.length;
  const decisions = pendingDecisions(snapshot);
  const carded = new Set(approvals.map((request) => request.agent_id).filter(Boolean));
  const byName = (a, b) => a.name.localeCompare(b.name);
  const asking = agents.filter((agent) => agent.online && NEEDS.has(agent.status) && !carded.has(agent.agentId)).sort(byName);
  const order = byGroup ? (a, b) => a.group.localeCompare(b.group) || byRecency(a, b) : byRecency;
  const errors = agents.filter((agent) => agent.online && ERRORS.has(agent.status)).sort(order);
  const working = agents.filter((agent) => agent.online && WORKING.has(agent.status)).sort(order);
  // The rest online: neither asking (a card above) nor at work, most often
  // idle between turns.
  const resting = agents.filter((agent) => agent.online && !NEEDS.has(agent.status) && !ERRORS.has(agent.status) && !WORKING.has(agent.status))
    .sort(order);
  return {
    approvals,
    elsewhere,
    decisions,
    asking,
    errors,
    working,
    resting,
    online: agents.filter((agent) => agent.online),
    // Offline members: Problems lists one whose open PR's checks fail, as
    // nobody is iterating on it. An archived group's are left out: the
    // operator put that work away.
    offline: agents.filter((agent) => !agent.online && !archived.has(agent.group)).sort(byRecency),
    needs: approvals.length + elsewhere + decisions.length + asking.length,
  };
}

// Now's View settings (its View sheet, the top bar's sliders), per viewer
// like the Agents view's: byGroup sorts each section by group first. It is
// kept only when on (off is the default). Reading or writing the store never
// throws: a store that fails reads as the defaults and forgets the change.
export const NOW_BY_GROUP_PREF = 'tclaude.dash.phone.now.bygroup';
export const NOW_DEFAULTS = Object.freeze({ byGroup: false });

export function nowSettings(prefs) {
  try {
    return { byGroup: prefs?.getItem(NOW_BY_GROUP_PREF) === '1' };
  } catch {
    return { ...NOW_DEFAULTS };
  }
}

// saveNowSettings keeps settings, forgetting each one at its default.
export function saveNowSettings(prefs, settings) {
  try {
    if (settings.byGroup) prefs.setItem(NOW_BY_GROUP_PREF, '1');
    else prefs.removeItem(NOW_BY_GROUP_PREF);
  } catch {
    // Not kept: this session still shows the change.
  }
}

// FEW_ONLINE is how many online agents Now lists whole. Above it, All lists
// the ones at work or in error and those heard from recently, and "Show all"
// opens the rest.
export const FEW_ONLINE = 10;

// RECENT_MS is how lately an agent must have called a hook to count as
// recently active in All when there are many agents online.
export const RECENT_MS = 15 * 60 * 1000;

// STUCK_AFTER_MS is how long a busy agent may stay quiet (no hook) before
// Problems lists it as quiet while busy.
export const STUCK_AFTER_MS = 15 * 60 * 1000;

// hookAt is when an agent last called a hook, in ms, or null if never.
export function hookAt(agent) {
  const at = Date.parse(agent?.lastHook || '');
  return Number.isFinite(at) ? at : null;
}

// latest is an agent's hookAt for sorting: one that never called a hook
// sorts as the oldest.
const latest = (agent) => hookAt(agent) ?? -Infinity;

// byRecency orders agents by their latest hook, newest first, then by name;
// those never heard from go last.
const byRecency = (a, b) => latest(b) - latest(a) || a.name.localeCompare(b.name);

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

// idleGroups is the Idle chip: the online idle agents by group, the groups
// A–Z, each group's longest-idle last.
export function idleGroups(view) {
  const groups = new Map();
  for (const agent of view.resting) {
    if (!groups.has(agent.group)) groups.set(agent.group, []);
    groups.get(agent.group).push(agent);
  }
  return [...groups].sort(([a], [b]) => a.localeCompare(b))
    .map(([group, agents]) => ({ group, agents: agents.sort(byRecency) }));
}

// recentRows is the Recent chip: every online agent, the latest hook first;
// an agent that never called one goes last.
export function recentRows(view) {
  return [...view.online].sort(byRecency).map((agent) => ({ agent, state: rowState(agent) }));
}

// stuckRows is the agents at work, quiet the longest first (Problems' quiet
// while busy),
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

// CI_LEFT_AFTER_MS is how long an agent may sit idle, its open PR's checks
// failing, before Problems lists the PR: iterating on red CI is normal while
// the agent is at it. It is the quiet-while-busy threshold, so one timer
// answers "has this agent stopped?" for both.
export const CI_LEFT_AFTER_MS = STUCK_AFTER_MS;

// openPRFailing reports whether an agent's open PR has failing checks.
const openPRFailing = (agent) => agent.pr?.ci === 'failing' && agent.pr.state !== 'merged' && agent.pr.state !== 'closed';

// ciLeft reports whether an agent with a failing open PR has left it: it is
// offline, or idle (not at work, not asking the operator) and silent past
// CI_LEFT_AFTER_MS, or idle and never heard from. An agent at work, asking
// (Needs you shows it) or idle only briefly is still iterating.
export function ciLeft(agent, now = Date.now()) {
  if (!agent.online) return true;
  if (WORKING.has(agent.status) || NEEDS.has(agent.status)) return false;
  const at = hookAt(agent);
  return at === null || now - at > CI_LEFT_AFTER_MS;
}

// PROBLEMS are the kinds Problems lists, worst first.
export const PROBLEMS = Object.freeze([
  { kind: 'error', title: 'Errors' },
  { kind: 'ci', title: 'CI failed' },
  { kind: 'quiet', title: 'Quiet while busy' },
]);

// problemRows is the Problems chip: one go-look list of agents, each once,
// under the worst of its problems: in error, its open PR's checks failing
// with nobody iterating on them (ciLeft: offline, or idle past
// CI_LEFT_AFTER_MS), or at work but quiet past STUCK_AFTER_MS (quiet the
// longest first). Only the CI kind lists offline agents. [{ kind, agent,
// state }]
export function problemRows(view, now = Date.now()) {
  const taken = new Set();
  const rows = [];
  const take = (kind, list) => {
    for (const agent of list) {
      if (taken.has(agent.agentId)) continue;
      taken.add(agent.agentId);
      rows.push({ kind, agent, state: agent.online ? rowState(agent) : 'offline' });
    }
  };
  const online = view.online;
  take('error', view.errors);
  take('ci', [...online, ...(view.offline || [])].filter((agent) => openPRFailing(agent) && ciLeft(agent, now)));
  take('quiet', stuckRows(view, now).rows.filter((row) => row.quiet).map((row) => row.agent));
  return rows;
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

