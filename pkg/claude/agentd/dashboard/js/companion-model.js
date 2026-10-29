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
// The one write is boards.write: drawing on a shared board with the operator
// is conversation, not configuration.
export const STEWARD_SLUGS = Object.freeze([
  'process.templates.read',
  'process.runs.read',
  'triggers.read',
  'usage.read',
  'host.read',
  'costs.read',
  'dashboard.view.read',
  'dashboard.guide',
  'boards.read',
  'boards.write',
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
  const add = (member, steward) => {
    const agentId = member?.agent_id;
    if (!isAgentId(agentId)) return;
    const prev = out.get(agentId);
    if (prev && (prev.online || member.online !== true)) return;
    out.set(agentId, Object.freeze({
      agentId,
      convId: String(member.conv_id || ''),
      name: String(member.title || member.name || agentId.slice(0, 12)),
      online: member.online === true,
      status: String(member.state?.status || ''),
      detail: String(member.state?.status_detail || ''),
      harness: String(member.harness || member.state?.harness || ''),
      steward: steward || prev?.steward === true,
    }));
  };
  for (const group of snapshot?.groups || []) {
    const steward = group?.scribe === true && group?.name === STEWARD_NAME;
    for (const member of group?.members || []) add(member, steward);
  }
  // Agents outside every group are still agents the operator can pin.
  for (const agent of [...(snapshot?.agents || []), ...(snapshot?.ungrouped || [])]) add(agent, false);
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

// composeCompanionMessage builds the operator-mail body for the input row.
export function composeCompanionMessage(text, context = '') {
  const body = String(text || '').trim().slice(0, COMPANION_MESSAGE_MAX);
  if (!body) return '';
  const suffix = context ? `\n\n[dashboard view: ${context}]` : '';
  return clipUtf8(body, COMPANION_MESSAGE_MAX_BYTES - new TextEncoder().encode(suffix).length) + suffix;
}

// STEWARD_STARTERS are one-click prompts shown above the message row while
// the Steward's tab is open and nothing is typed: the usual ways in to
// guided setup (tcl-bifqbb).
export const STEWARD_STARTERS = Object.freeze([
  { label: 'What can I do here?', text: 'What can I do on this page? Point me at the important controls.' },
  { label: 'Review my setup', text: 'Review my tclaude setup (groups, spawn profiles, sandbox profiles, schedules) and tell me what is missing or worth changing. Do not change anything yet.' },
  { label: 'Set up my repos', text: 'Help me set up a group for each of my repos, with sensible defaults. Ask me which repos first.' },
  { label: 'Create a dev flow', text: 'Help me create a development process (implement, review, rework, merge, with me approving the merge) as a process template.' },
  { label: 'Sketch on a board', text: 'Open a shared board for us and sketch how my groups and processes fit together right now.' },
]);

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
    '- See and point at the dashboard: `tclaude agent guide view` tells you which view the operator',
    '  is on; `tclaude agent guide show --tab <tab> | --path <path> [--target "<visible text>"]',
    '  [--note "<short note>"]` moves their dashboard there and highlights that control. Point',
    '  rather than describe where to click. A cue never presses anything; the operator acts.',
    '  To point at several things at once, draw marks on their view: `--circle "<text>"`,',
    '  `--underline "<text>"` and `--arrow "<from text> -> <to text>"` (repeatable, up to 6).',
    '- Draw with the operator on shared boards (Collab → Boards). When they invite you, you get',
    '  "[board <id>]" mail as their edits settle. Read a board as shapes with',
    '  `tclaude agent board show <id>`, or look at it with `tclaude agent board snapshot <id> -o',
    '  /tmp/<id>.png` and view the image. Draw back with `tclaude agent board add|move|text|rm`',
    '  (arrows join shapes with --from/--to). Build a flow step by step with `board add <id>',
    '  rectangle --right-of <prev> --link --text "<step>"`, and tidy it with `board layout <id>`.',
    '  See `tclaude agent board --help`. Sketch a plan,',
    '  annotate theirs, or answer a question as a diagram when a picture is clearer than words.',
    '- "[annotation]" mail is the operator marking up the dashboard view they are on: a numbered',
    '  list of what they circled, underlined or pointed at, plus their note. Answer about those',
    '  controls, and point back with `tclaude agent guide show --target` when that helps.',
    '',
    'You are read-only by default. Before changing anything, show a short concrete plan. For a',
    'multi-step setup, ask for exactly the permissions the plan needs, once, for a bounded time:',
    '`tclaude agent sudo request <slug>... --duration 30m --reason "<the plan, step by step>"`.',
    'The operator approves or denies it as a card in your companion window. For a single change,',
    'run that one command with `--ask-human 120s` instead. Never retry a denied request; ask the',
    'operator what to change.',
    '',
    'For setup work (groups for repos, spawn profiles, sandbox profiles, schedules) follow the',
    'tclaude-setup skill: inspect, ask only what you cannot infer, plan, request once, execute,',
    'verify, then point at the result. Process templates: the process-templates skill. Group',
    'templates: the agent-circles skill. Boards: create one with `tclaude agent board create',
    '<id> --title "<title>"` when the operator asks for a sketch, draw on it, and open it for',
    'them with `tclaude agent guide show --path /boards/<id>`.',
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

// approvalsFor lists the pending access requests (a `tclaude agent sudo`
// plan, or one --ask-human command) the given agent is waiting on, so the
// companion can show them where the operator is already talking to it.
export function approvalsFor(snapshot, agent) {
  if (!agent) return [];
  const convs = new Set([agent.convId].filter(Boolean));
  return (snapshot?.access_requests || []).filter((request) => {
    if (request?.status && request.status !== 'pending') return false;
    if (request?.proposal) return false; // federation proposals have their own flow
    if (request?.agent_id) return request.agent_id === agent.agentId;
    return convs.has(request?.conv_id) || convs.has(request?.current_conv_id);
  });
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
