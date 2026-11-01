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
export const COMPANION_MAX_TABS = 8;

export const COMPANION_MIN_W = 360;
export const COMPANION_MIN_H = 220;
export const COMPANION_DEFAULT_W = 640;
export const COMPANION_DEFAULT_H = 420;
// Distance from a viewport edge at which a drag offers to snap.
export const COMPANION_SNAP_EDGE = 28;
// The share of the viewport a snapped window takes.
export const COMPANION_SNAP_SHARE = 0.42;
// Room kept for the fixed footer bar below a placement.
export const COMPANION_FOOTER_ROOM = 29;

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
  const w = Math.min(COMPANION_DEFAULT_W, Math.max(COMPANION_MIN_W, viewport.width - 48));
  const h = Math.min(COMPANION_DEFAULT_H, Math.max(COMPANION_MIN_H, viewport.height - 140));
  return { x: viewport.width - w - 24, y: viewport.height - h - COMPANION_FOOTER_ROOM - 56, w, h, snap: '' };
}

export function parsePlacement(raw) {
  try {
    const p = JSON.parse(raw || 'null');
    if (!p || typeof p !== 'object') return null;
    const nums = ['x', 'y', 'w', 'h'].every((k) => Number.isFinite(p[k]));
    if (!nums) return null;
    const snap = ['left', 'right', 'bottom', 'max'].includes(p.snap) ? p.snap : '';
    const placement = { x: p.x, y: p.y, w: p.w, h: p.h, snap };
    return snap === 'max' && ['left', 'right', 'bottom'].includes(p.restoreSnap) ? { ...placement, restoreSnap: p.restoreSnap } : placement;
  } catch {
    return null;
  }
}

// clampPlacement keeps a floating window reachable: at least its title bar
// stays on screen whatever the viewport did since it was saved.
export function clampPlacement(p, viewport) {
  const maxW = Math.max(COMPANION_MIN_W, viewport.width - 8);
  const maxH = Math.max(COMPANION_MIN_H, viewport.height - COMPANION_FOOTER_ROOM - 8);
  const w = Math.round(Math.min(maxW, Math.max(COMPANION_MIN_W, p.w)));
  const h = Math.round(Math.min(maxH, Math.max(COMPANION_MIN_H, p.h)));
  const x = Math.round(Math.min(viewport.width - 80, Math.max(80 - w, p.x)));
  const y = Math.round(Math.min(viewport.height - COMPANION_FOOTER_ROOM - 36, Math.max(0, p.y)));
  return { x, y, w, h, snap: p.snap || '' };
}

// fitPlacement is where a window opens: exactly its saved place when that fits,
// otherwise moved in, and smaller only if the viewport cannot hold it. Unlike
// clampPlacement it keeps the whole title bar on screen, because the close and
// minimize controls sit at its right end. A viewport narrower than the minimum
// width keeps that right end. The result is never saved, so a larger browser
// brings back the operator's own size.
export function fitPlacement(p, viewport) {
  const { w, h, snap } = clampPlacement(p, viewport);
  const right = viewport.width - w;
  const x = Math.round(Math.max(Math.min(0, right), Math.min(p.x, right)));
  const y = Math.round(Math.max(0, Math.min(p.y, viewport.height - COMPANION_FOOTER_ROOM - h)));
  return { x, y, w, h, snap };
}

// snapZone names the edge a dragged window would snap to for a pointer
// position, or '' to keep floating.
export function snapZone(pointer, viewport) {
  if (pointer.x >= viewport.width - COMPANION_SNAP_EDGE) return 'right';
  if (pointer.x <= COMPANION_SNAP_EDGE) return 'left';
  if (pointer.y >= viewport.height - COMPANION_FOOTER_ROOM - COMPANION_SNAP_EDGE) return 'bottom';
  return '';
}

// viewportOf is the area a fixed window can use. innerWidth and innerHeight
// include page scrollbars, which would cover a window snapped to that edge.
export function viewportOf(win) {
  const root = win?.document?.documentElement;
  return {
    width: root?.clientWidth || win?.innerWidth || 1280,
    height: root?.clientHeight || win?.innerHeight || 800,
  };
}

// zoneRect is the rectangle a snapped window occupies: a full-height side
// panel below the top bar, a full-width bottom drawer above the footer, or for
// a maximized window all the room between the two.
export function zoneRect(zone, viewport, top = 0) {
  const bottom = viewport.height - COMPANION_FOOTER_ROOM;
  if (zone === 'max') return { x: 0, y: top, w: viewport.width, h: Math.max(COMPANION_MIN_H, bottom - top), snap: zone };
  if (zone === 'right' || zone === 'left') {
    const w = Math.min(viewport.width, Math.max(COMPANION_MIN_W, Math.round(viewport.width * COMPANION_SNAP_SHARE)));
    return { x: zone === 'right' ? viewport.width - w : 0, y: top, w, h: Math.max(COMPANION_MIN_H, bottom - top), snap: zone };
  }
  if (zone === 'bottom') {
    const h = Math.max(COMPANION_MIN_H, Math.round((bottom - top) * COMPANION_SNAP_SHARE));
    return { x: 0, y: bottom - h, w: viewport.width, h, snap: zone };
  }
  return null;
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
  const convs = new Set([agent.convId].filter(Boolean));
  return pendingApprovals(snapshot).filter((request) => {
    if (request?.agent_id) return request.agent_id === agent.agentId;
    return convs.has(request?.conv_id) || convs.has(request?.current_conv_id);
  });
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
