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

export const COMPANION_TABS_PREF = 'tclaude.dash.companion.tabs';
export const COMPANION_ACTIVE_PREF = 'tclaude.dash.companion.active';
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
// The Steward is read-only by default: it inspects and explains through the
// tclaude CLI's read surfaces. Anything that changes state goes through a
// per-command `--ask-human` approval the operator decides in the dashboard.
export const STEWARD_SLUGS = Object.freeze([
  'process.templates.read',
  'process.runs.read',
  'triggers.read',
  'usage.read',
  'host.read',
  'costs.read',
]);

const AGENT_ID = /^agt_[0-9a-f]{32}$/;

export function isAgentId(value) {
  return typeof value === 'string' && AGENT_ID.test(value);
}

// companionAgents indexes every agent the snapshot knows by its stable
// agent id. An agent in several groups appears once; a live membership wins
// over a stale one so the status reflects the running session.
export function companionAgents(snapshot) {
  const out = new Map();
  for (const group of snapshot?.groups || []) {
    for (const member of group?.members || []) {
      const agentId = member?.agent_id;
      if (!isAgentId(agentId)) continue;
      const prev = out.get(agentId);
      if (prev && (prev.online || member.online !== true)) continue;
      out.set(agentId, Object.freeze({
        agentId,
        convId: String(member.conv_id || ''),
        name: String(member.title || member.name || agentId.slice(0, 12)),
        online: member.online === true,
        status: String(member.state?.status || ''),
        detail: String(member.state?.status_detail || ''),
        harness: String(member.harness || member.state?.harness || ''),
        steward: group?.scribe === true && group?.name === STEWARD_NAME,
      }));
    }
  }
  return out;
}

// stewardAgent picks the live Steward, else the most recently listed one.
export function stewardAgent(agents) {
  let fallback = null;
  for (const agent of agents.values()) {
    if (!agent.steward) continue;
    if (agent.online) return agent;
    fallback = fallback || agent;
  }
  return fallback;
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
    const snap = ['left', 'right', 'bottom'].includes(p.snap) ? p.snap : '';
    return { x: p.x, y: p.y, w: p.w, h: p.h, snap };
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

// snapZone names the edge a dragged window would snap to for a pointer
// position, or '' to keep floating.
export function snapZone(pointer, viewport) {
  if (pointer.x >= viewport.width - COMPANION_SNAP_EDGE) return 'right';
  if (pointer.x <= COMPANION_SNAP_EDGE) return 'left';
  if (pointer.y >= viewport.height - COMPANION_FOOTER_ROOM - COMPANION_SNAP_EDGE) return 'bottom';
  return '';
}

// zoneRect is the rectangle a snapped window occupies: a full-height side
// panel below the top bar, or a full-width bottom drawer above the footer.
export function zoneRect(zone, viewport, top = 0) {
  const bottom = viewport.height - COMPANION_FOOTER_ROOM;
  if (zone === 'right' || zone === 'left') {
    const w = Math.max(COMPANION_MIN_W, Math.round(viewport.width * COMPANION_SNAP_SHARE));
    return { x: zone === 'right' ? viewport.width - w : 0, y: top, w, h: Math.max(COMPANION_MIN_H, bottom - top), snap: zone };
  }
  if (zone === 'bottom') {
    const h = Math.max(COMPANION_MIN_H, Math.round((bottom - top) * COMPANION_SNAP_SHARE));
    return { x: 0, y: bottom - h, w: viewport.width, h, snap: zone };
  }
  return null;
}

const TAB_NAMES = Object.freeze({
  groups: 'Groups', processes: 'Processes', terminals: 'Terminals', messages: 'Messages',
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

// composeCompanionMessage builds the operator-mail body for the input row.
export function composeCompanionMessage(text, context = '') {
  const body = String(text || '').trim().slice(0, COMPANION_MESSAGE_MAX);
  if (!body) return '';
  return context ? `${body}\n\n[dashboard view: ${context}]` : body;
}

export function stewardBrief() {
  return [
    'You are the Steward: the operator\'s always-available helper for tclaude, shown in a floating',
    'companion window on top of the tclaude dashboard. The operator talks to you in this terminal,',
    'or through the window\'s message row, which arrives as operator mail ending with a',
    '"[dashboard view: ...]" line naming the dashboard view they are looking at.',
    '',
    'What you do:',
    '- Explain tclaude and the dashboard view the operator is on ("what can I do here?").',
    '- Inspect their setup through the tclaude CLI read surfaces: `tclaude agent groups ls`,',
    '  `tclaude agent ls`, `tclaude agent profiles ls`, `tclaude agent templates ls`,',
    '  `tclaude agent process-templates ls`, `tclaude agent cron ls`, `tclaude agent permissions ls`,',
    '  `tclaude usage`, and `--help` on any command. Use the installed skills (agent-circles,',
    '  process-templates, agent-coord, ...) for domain knowledge.',
    '- Help set things up: groups for the operator\'s repos, spawn profiles, process templates,',
    '  sandbox profiles, schedules.',
    '',
    'You are read-only by default. Before changing anything, show a short concrete plan and wait',
    'for the operator\'s go-ahead. Then run each mutating command with `--ask-human 120s` so the',
    'operator approves that exact command in the dashboard; never retry a denied request.',
    '',
    'Keep answers short and practical; the window is small. Prefer showing the exact command',
    'or the dashboard place to click.',
  ].join('\n');
}

export function stewardSummonBody() {
  return {
    name: STEWARD_NAME,
    slugs: [...STEWARD_SLUGS],
    scope: { ...STEWARD_SCOPE },
    background: true,
    brief: stewardBrief(),
  };
}
