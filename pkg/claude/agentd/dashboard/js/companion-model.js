// companion-model.js -- the pure half of the dashboard companion (tcl-6i0283):
// an always-available floating window over every dashboard view that hosts
// agents' NATIVE harness terminals (the real tmux pane over the same
// /api/open-window-ws stream the Terminals tab uses), so the operator gets the
// harness's own streaming, prompts and interrupts rather than a re-rendered
// chat.
//
// The window has three states: chips (minimized, one per pinned agent, with a
// "needs you" state), a read-only hover peek, and the open window with one tab
// per agent. The Steward is the default companion: a dashboard-wide helper
// summoned through the ordinary scribe machinery (POST /api/scribe), so it is
// a plain agent in a scribe group, reachable from Terminals for debugging.
//
// Everything here is DOM-free so it can be tested directly.
import { presenceView } from './agent-presence.js';
import { OVERLAY_FOOTER_ROOM, OVERLAY_MIN_H, OVERLAY_MIN_W, fitPlacement } from './overlay-window-model.js';

export const COMPANION_TABS_PREF = 'tclaude.dash.companion.tabs';
// What this window is showing is per browser tab (sessionStorage): an open
// window attaches its terminal, and that takes over from other views.
export const COMPANION_OPEN_PREF = 'tclaude.dash.companion.open';
export const COMPANION_PARKED_PREF = 'tclaude.dash.companion.parked';
export const COMPANION_DETACHED_PREF = 'tclaude.dash.companion.detached';
// The scribes and Stewards the operator closed, which stay closed.
export const COMPANION_CLOSED_PREF = 'tclaude.dash.companion.closed';
export const COMPANION_ACTIVE_PREF = 'tclaude.dash.companion.active';
// The newest hand-over cue (guide show --open) a dashboard tab acted on.
export const COMPANION_CUE_OPEN_PREF = 'tclaude.dash.companion.cue-open';
export const COMPANION_PLACEMENT_PREF = 'tclaude.dash.companion.placement';
// Where the minimized chips rest (tcl-avebwq), and the spot a floating window
// opens at. The second holds until the operator moves the window themselves.
export const COMPANION_CHIP_SPOT_PREF = 'tclaude.dash.companion.chip-spot';
export const COMPANION_WINDOW_SPOT_PREF = 'tclaude.dash.companion.window-spot';
// The last spot in the room, where a row saved in the footer rests when it doesn't fit.
export const COMPANION_CHIP_ROOM_SPOT_PREF = 'tclaude.dash.companion.chip-room-spot';
export const COMPANION_MAX_TABS = 8;

export const COMPANION_DEFAULT_W = 640;
export const COMPANION_DEFAULT_H = 420;

export const STEWARD_NAME = 'steward';
export const STEWARD_SCOPE = Object.freeze({ kind: 'steward' });
// A Steward's access is not named here: the daemon sets it from config
// (steward.reads, steward.writes). Anything beyond that goes through an approval
// the operator decides.

const AGENT_ID = /^agt_[0-9a-f]{32}$/;

export function isAgentId(value) {
  return typeof value === 'string' && AGENT_ID.test(value);
}

// companionAgents indexes every agent the snapshot knows by its stable
// agent id. An agent in several groups appears once; a live membership wins
// over a stale one so the status reflects the running session.
export function companionAgents(snapshot) {
  const out = new Map();
  const customName = String(snapshot?.steward_name || '');
  const activeId = String(snapshot?.steward_active || '');
  const add = (member, steward, scribe, group = '') => {
    const agentId = member?.agent_id;
    if (!isAgentId(agentId)) return;
    const prev = out.get(agentId);
    if (prev && (prev.online || member.online !== true)) {
      // A Steward can sit in other groups too; keep what this listing knows.
      if ((steward && !prev.steward) || (scribe && !prev.scribe)) {
        out.set(agentId, Object.freeze({ ...prev, steward: steward || prev.steward, scribe: scribe || prev.scribe }));
      }
      return;
    }
    out.set(agentId, Object.freeze({
      agentId,
      convId: String(member.conv_id || ''),
      // The operator's name for the Steward means the one in the seat.
      name: ((activeId ? agentId === activeId : steward) && customName) || String(member.title || member.name || agentId.slice(0, 12)),
      online: member.online === true,
      status: String(member.state?.status || ''),
      detail: String(member.state?.status_detail || ''),
      harness: String(member.harness || member.state?.harness || ''),
      cwd: String(member.state?.cwd || member.cwd || prev?.cwd || ''),
      group: String(group || prev?.group || ''),
      presence: member.federation_presence || prev?.presence || null,
      steward: steward || prev?.steward === true,
      active: agentId === activeId,
      scribe: scribe || prev?.scribe === true,
    }));
  };
  for (const group of snapshot?.groups || []) {
    const scribe = group?.scribe === true;
    const steward = scribe && group?.name === STEWARD_NAME;
    for (const member of group?.members || []) add(member, steward, scribe, group?.name);
  }
  // Agents outside every group are still agents the operator can pin.
  for (const agent of [...(snapshot?.agents || []), ...(snapshot?.ungrouped || [])]) add(agent, false, false);
  return out;
}

// companionAgentOf finds an agent by its id, or by the conv id a pane may hold.
export function companionAgentOf(snapshot, selector) {
  const agents = companionAgents(snapshot);
  return agents.get(selector) || [...agents.values()].find((agent) => agent.convId === selector) || null;
}

// companionLaunchData is what a ⚙ "Send elsewhere" item says about this agent,
// for launchAgent (launch-target-dialog.js): the same dialog opens from the
// window, so Clone keeps the working directory and Recall home sees where the
// agent is.
export function companionLaunchData(agent, fleet, now = Date.now()) {
  const view = presenceView({ federation_presence: agent.presence, title: agent.name, agent_id: agent.agentId }, fleet, now);
  return {
    agent: agent.agentId, conv: agent.convId, label: agent.name, harness: agent.harness,
    cwd: agent.cwd, group: agent.group,
    presence: view?.kind || '', homeNode: view?.home?.id || '', homeName: view?.home?.name || '',
  };
}

// stewardAgent picks the Steward in the seat, running or not. With nobody seated
// it is the live Steward, else the most recently listed one.
export function stewardAgent(agents) {
  let fallback = null;
  for (const agent of agents.values()) {
    if (agent.active) return agent;
    if (!agent.steward) continue;
    if (agent.online) return agent;
    fallback = fallback || agent;
  }
  return fallback;
}

// stewardToResume is the roster's Steward that can be brought back: the one in
// the seat, or one running without the seat, once it is not running. A Steward
// on standby is a normal agent, and a retired one is not listed at all.
export function stewardToResume(roster) {
  return (roster || []).find((entry) => (entry.seat === 'active' || entry.seat === 'alongside') && !entry.online) || null;
}

// syncHelperTabs follows the live scribes and Stewards, whichever summoned
// them: a live one gets a tab unless the operator closed it, an ended one
// loses its tab. keep lists tabs to leave alone: the one on screen, and agents
// still starting. Tabs the operator pinned are never pushed out by a helper.
export function syncHelperTabs(tabs, agents, closed, keep) {
  let next = tabs;
  for (const id of tabs) {
    const agent = agents.get(id);
    if (agent?.scribe && !agent.online && !keep.has(id)) next = withoutTab(next, id);
  }
  for (const agent of agents.values()) {
    if (!agent.scribe || !agent.online || closed.has(agent.agentId) || next.includes(agent.agentId)) continue;
    if (next.length < COMPANION_MAX_TABS) next = withTab(next, agent.agentId);
  }
  return next;
}

// parseClosedHelpers reads the helpers the operator closed. Unlike tabs this
// list has no cap: every one of them must stay closed.
export function parseClosedHelpers(raw) {
  try {
    const list = JSON.parse(raw || '[]');
    return new Set(Array.isArray(list) ? list.filter(isAgentId) : []);
  } catch {
    return new Set();
  }
}

// companionStatus folds an agent's runtime state into the four states a chip
// can show. "needs" is the one that matters most: a minimized agent blocked on
// a permission prompt or a question must never look silently busy.
export function companionStatus(agent) {
  if (!agent || !agent.online) return 'offline';
  const status = agent.status;
  if (status === 'awaiting_permission' || status === 'awaiting_input') return 'needs';
  if (status === 'working' || status === 'main_agent_idle' || status === 'restarting') return 'working';
  if (status === 'crashed' || status === 'error' || status === 'crash loop / backoff') return 'error';
  return 'idle';
}

export function companionStatusLabel(status) {
  switch (status) {
    case 'needs': return 'needs you';
    case 'working': return 'working';
    case 'error': return 'error';
    case 'offline': return 'offline';
    default: return 'idle';
  }
}

export function parseTabs(raw) {
  try {
    const list = JSON.parse(raw || '[]');
    if (!Array.isArray(list)) return [];
    return [...new Set(list.filter(isAgentId))].slice(0, COMPANION_MAX_TABS);
  } catch {
    return [];
  }
}

export function withTab(tabs, agentId) {
  if (!isAgentId(agentId)) return tabs;
  if (tabs.includes(agentId)) return tabs;
  return [...tabs, agentId].slice(-COMPANION_MAX_TABS);
}

export function withoutTab(tabs, agentId) {
  return tabs.filter((id) => id !== agentId);
}

// nextActive picks the tab to show after `removed` closes: its right
// neighbour, else its left, else none.
export function nextActive(tabs, removed) {
  const index = tabs.indexOf(removed);
  const rest = withoutTab(tabs, removed);
  if (!rest.length) return '';
  return rest[Math.min(Math.max(index, 0), rest.length - 1)];
}

export function defaultPlacement(viewport) {
  const w = Math.min(COMPANION_DEFAULT_W, Math.max(OVERLAY_MIN_W, viewport.width - 48));
  const h = Math.min(COMPANION_DEFAULT_H, Math.max(OVERLAY_MIN_H, viewport.height - 140));
  return { x: viewport.width - w - 24, y: viewport.height - h - OVERLAY_FOOTER_ROOM - 56, w, h, snap: '' };
}

// The chips' snap spots: where each sits along the room, as shares across and
// down. companion.css lays out the same shares, and its right side stands clear
// of the dock's edge tab.
const CHIP_SPOT_SHARES = Object.freeze({
  'top-left': [0, 0], 'top-q1': [0.25, 0], 'top-center': [0.5, 0], 'top-q3': [0.75, 0], 'top-right': [1, 0],
  'left-q1': [0, 0.25], 'left-center': [0, 0.5], 'left-q3': [0, 0.75],
  'right-q1': [1, 0.25], 'right-center': [1, 0.5], 'right-q3': [1, 0.75],
  'bottom-left': [0, 1], 'bottom-q1': [0.25, 1], 'bottom-center': [0.5, 1], 'bottom-q3': [0.75, 1], 'bottom-right': [1, 1],
  'footer-left': [0, 1], 'footer-center': [0.5, 1], 'footer-right': [1, 1],
});
// The spots in the room. The footer's are offered only while the row fits there.
export const FOOTER_CHIP_SPOTS = Object.freeze(['footer-left', 'footer-center', 'footer-right']);
export const CHIP_SPOTS = Object.freeze(Object.keys(CHIP_SPOT_SHARES).filter((spot) => !FOOTER_CHIP_SPOTS.includes(spot)));
export const DEFAULT_CHIP_SPOT = 'bottom-right';
// The chips' own distance from the viewport edges; companion.css has the same.
const CHIP_INSET_X = 16;
const CHIP_INSET_Y = 12;

export function parseChipSpot(raw) {
  return Object.hasOwn(CHIP_SPOT_SHARES, raw ?? '') ? raw : DEFAULT_CHIP_SPOT;
}

export const isFooterSpot = (spot) => FOOTER_CHIP_SPOTS.includes(spot);

// The space a row keeps from what else is in the footer.
const FOOTER_CHIP_MARGIN = 12;

// freeStretch is the widest stretch between left and right that no rect covers.
export function freeStretch(rects, left, right) {
  let best = null;
  let from = left;
  const keep = (to) => { if (to > from && (!best || to - from > best.right - best.left)) best = { left: from, right: to }; };
  for (const rect of [...rects].sort((a, b) => a.left - b.left)) {
    keep(Math.min(rect.left, right));
    from = Math.max(from, rect.right);
  }
  keep(right);
  return best;
}

export function footerSpotFits(room, width) {
  return !!room && width > 0 && width + 2 * FOOTER_CHIP_MARGIN <= room.right - room.left;
}

// footerSpotLeft is where a row of this width starts at a footer spot.
export function footerSpotLeft(spot, room, width) {
  if (spot === 'footer-left') return Math.round(room.left + FOOTER_CHIP_MARGIN);
  if (spot === 'footer-right') return Math.round(room.right - FOOTER_CHIP_MARGIN - width);
  return Math.round((room.left + room.right - width) / 2);
}

// nearestChipSpot picks the slot whose centre is closest to point. slots are
// {spot, rect} as drawn, so the page's own layout decides where each one is.
export function nearestChipSpot(point, slots) {
  let best = '';
  let bestDistance = Infinity;
  for (const { spot, rect } of slots) {
    const distance = Math.hypot(rect.left + rect.width / 2 - point.x, rect.top + rect.height / 2 - point.y);
    if (distance < bestDistance) {
      best = spot;
      bestDistance = distance;
    }
  }
  return best;
}

// spotPlacement puts a floating window around the chip's spot, so it grows from
// there. It keeps p's size and, through fitPlacement, stays on screen.
export function spotPlacement(spot, p, viewport, top = 0) {
  const [across, down] = CHIP_SPOT_SHARES[spot];
  const [left, low] = [CHIP_INSET_X, top + CHIP_INSET_Y];
  const x = left + across * (viewport.width - p.w - CHIP_INSET_X - left);
  const y = low + down * (viewport.height - OVERLAY_FOOTER_ROOM - p.h - CHIP_INSET_Y - low);
  return fitPlacement({ ...p, x, y, snap: '' }, viewport);
}

// peekAnchor places the hover peek on the open side of a chip at this spot.
export function peekAnchor(spot, rect, viewport) {
  const gap = 8;
  const [across, down] = CHIP_SPOT_SHARES[spot];
  const anchor = down < 0.5 ? { top: rect.bottom + gap } : { bottom: viewport.height - rect.top + gap };
  return across < 0.5 ? { ...anchor, left: rect.left } : { ...anchor, right: viewport.width - rect.right };
}

const TAB_NAMES = Object.freeze({
  groups: 'Groups', processes: 'Processes', terminals: 'Collab', messages: 'Messages',
  jobs: 'Jobs', usage: 'Usage', config: 'Config', access: 'Access', diagnostics: 'Diagnostics',
  plugins: 'Plugins', map: 'Map', vegas: 'Vegas', 'fleet-admin': 'Fleet admin',
});

// viewContext describes what the operator is looking at, as a short line the
// companion attaches to messages sent from its input row. It is orientation
// only: names and a URL path, never page content.
export function viewContext({ activeTab = '', pathname = '', search = '' } = {}) {
  const view = TAB_NAMES[activeTab] || activeTab || 'dashboard';
  const path = `${pathname || '/'}${search || ''}`.slice(0, 240);
  return `${view} (${path})`;
}

export const COMPANION_MESSAGE_MAX = 8000;
// The operator-mail endpoint caps a body at 16 KiB of UTF-8; stay under it.
export const COMPANION_MESSAGE_MAX_BYTES = 15 * 1024;

// clipUtf8 shortens s to at most maxBytes of UTF-8 without splitting a
// character.
export function clipUtf8(s, maxBytes) {
  const encoder = new TextEncoder();
  if (encoder.encode(s).length <= maxBytes) return s;
  let out = '';
  let bytes = 0;
  for (const ch of s) {
    const size = encoder.encode(ch).length;
    if (bytes + size > maxBytes) break;
    out += ch;
    bytes += size;
  }
  return out;
}

const viewSuffix = (context) => (context ? `\n\n[dashboard view: ${context}]` : '');

// DICTATED_MARKER opens a dictated message, so the agent knows why sensible
// symbols may be missing. It is a constant: nothing a person said goes in it.
export const DICTATED_MARKER = '[dictated]';
export const markDictated = (text) => `${DICTATED_MARKER} ${String(text || '').trim()}`;

// companionMessageFits reports whether composeCompanionMessage keeps all of
// text. A caller checks this first and tells the operator, rather than send
// a clipped message as if it had gone out whole.
export function companionMessageFits(text, context = '') {
  const body = String(text || '').trim();
  const encoder = new TextEncoder();
  return body.length <= COMPANION_MESSAGE_MAX
    && encoder.encode(body).length + encoder.encode(viewSuffix(context)).length <= COMPANION_MESSAGE_MAX_BYTES;
}

// composeCompanionMessage builds the operator-mail body for the input row.
export function composeCompanionMessage(text, context = '') {
  const body = String(text || '').trim().slice(0, COMPANION_MESSAGE_MAX);
  if (!body) return '';
  const suffix = viewSuffix(context);
  return clipUtf8(body, COMPANION_MESSAGE_MAX_BYTES - new TextEncoder().encode(suffix).length) + suffix;
}

// The daemon writes a Steward's brief: a handover needs the same one.
export function stewardSummonBody() {
  return {
    name: STEWARD_NAME,
    scope: { ...STEWARD_SCOPE },
    background: true,
  };
}

// decisionsFor lists the decisions the given agent put to the operator
// (`notify-human --option ...`) that are still unanswered, oldest first.
export function decisionsFor(snapshot, agent) {
  if (!agent) return [];
  return (snapshot?.messages || [])
    .filter((m) => Array.isArray(m?.options) && m.options.length > 1 && !m.decision
      && (m.from_agent ? m.from_agent === agent.agentId : m.from_conv === agent.convId))
    .sort((a, b) => a.id - b.id);
}

// actionsFor lists the unread messages from this agent that carry an action
// link, oldest first. Reading one takes it out of chat; it stays in Messages.
export function actionsFor(snapshot, agent) {
  if (!agent) return [];
  return (snapshot?.messages || [])
    .filter((m) => m?.action && !m.read && (m.from_agent ? m.from_agent === agent.agentId : m.from_conv === agent.convId))
    .sort((a, b) => a.id - b.id);
}

// pendingDecisions lists every unanswered decision in the fleet, oldest
// first: the companion's "to decide" queue, so the operator can answer them
// all in one place whichever agent asked.
export function pendingDecisions(snapshot) {
  return (snapshot?.messages || [])
    .filter((m) => Array.isArray(m?.options) && m.options.length > 1 && !m.decision && (m.from_agent || m.from_conv))
    .sort((a, b) => a.id - b.id);
}

// decisionSpeech is what voice says when a decision arrives.
export function decisionSpeech(agent, decision) {
  const question = decision.subject || String(decision.body || '').split('\n')[0];
  const options = decision.options.map((o, i) => (i + 1 === decision.recommended ? `${o}, which it recommends` : o));
  const ended = /[.?!]$/.test(question) ? question : `${question}.`;
  return `${agent.name} asks you to decide: ${ended} ${options.length} options: ${options.join('; ')}.`;
}

// approvalsFor lists the pending access requests (a `tclaude agent sudo`
// plan, or one --ask-human command) the given agent is waiting on, so the
// companion can show them where the operator is already talking to it.
export function approvalsFor(snapshot, agent) {
  if (!agent) return [];
  return pendingApprovals(snapshot).filter((request) => requestFrom(request, agent));
}

function requestFrom(request, agent) {
  if (request?.agent_id) return request.agent_id === agent.agentId;
  return !!agent.convId && (request?.conv_id === agent.convId || request?.current_conv_id === agent.convId);
}

// agentBlockedOnOperator reports whether an agent can't go on without the
// operator: its session is at a prompt, or a command of its is held on an access
// request, which leaves the session working or idle. Every such request counts
// here, not only the ones a card can answer. A decision doesn't count: the agent
// asked and carried on, however long it stays open.
export function agentBlockedOnOperator(snapshot, agent) {
  return companionStatus(agent) === 'needs'
    || pendingAccess(snapshot).some((request) => requestFrom(request, agent));
}

// pendingAccess lists every request waiting on the operator, in the daemon's
// order. A list on screen must not reorder under the pointer.
export function pendingAccess(snapshot) {
  return (snapshot?.access_requests || []).filter((request) => !request?.status || request.status === 'pending');
}

// quickApproval says one card can answer this request. A peer's request, a link
// or proposal and a model gateway grant each need what only the Messages tab
// shows: who asks, for how long, and narrowing the grant.
function quickApproval(request) {
  return !request.proposal && !request.link && !request.origin_peer && !request.usage_gateways?.length
    && !request.http_proxy && !request.max_usd && request.perm !== 'models.proxy';
}

// pendingApprovals are the requests a card answers: the quick panel's list.
export function pendingApprovals(snapshot) {
  return pendingAccess(snapshot).filter(quickApproval);
}

// elevatedAccess is what an agent holds beyond its usual access right now: its
// time-bounded sudo grants and, for a Steward, what the daemon found beyond
// Config. Only the sudo can be ended here. Standing grants stay until they are
// changed in the permissions editor, and inherited ones come from a group, which
// other members share. Null when there is nothing.
export function elevatedAccess(snapshot, agent) {
  if (!agent) return null;
  const sudo = (snapshot?.sudo || []).filter((grant) => grant.agent_id === agent.agentId);
  const { standing = [], inherited = [] } = snapshot?.steward_elevated?.[agent.agentId] || {};
  const stored = new Set(Object.keys(snapshot?.steward_access?.carried || {}));
  // Ending a carried sudo also ends it for later Stewards, so the band says so.
  const sudoStored = sudo.some((grant) => grant.carried);
  return sudo.length || standing.length || inherited.length ? { sudo, standing, inherited, stored, sudoStored } : null;
}

// timeLeft reads a sudo grant's remaining seconds.
export function timeLeft(seconds) {
  return seconds < 60 ? 'under a minute' : `${Math.ceil(seconds / 60)} min`;
}

// approvalSummary describes what approving a request grants, in one line.
export function approvalSummary(request) {
  const perm = String(request?.perm || '');
  if (perm.startsWith('sudo.')) {
    const slugs = perm.slice(5).split(',').filter(Boolean);
    return { kind: 'plan', title: 'Temporary permissions for a plan', slugs };
  }
  return { kind: 'command', title: 'Permission for one command', slugs: perm ? [perm] : [] };
}
