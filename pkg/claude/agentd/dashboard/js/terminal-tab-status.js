// terminal-tab-status.js — snapshot-backed status for one terminal tab.
//
// A terminal seed carries the stable agent selector that opened it. The
// dashboard snapshot carries both stable agent ids and live conversation ids,
// so this module is the small, pure seam that joins the two without making the
// tab renderer know anything about the server's roster shape.

const STATUS_SYMBOLS = Object.freeze({
  working: '●',
  idle: '○',
  awaiting_permission: '?',
  awaiting_input: '?',
  error: '!',
  crashed: '×',
  offline: '–',
  waking: '◌',
  restarting: '↻',
  exited: '×',
  unknown: '·',
});

const STATUS_LABELS = Object.freeze({
  working: 'working',
  idle: 'idle',
  awaiting_permission: 'awaiting permission',
  awaiting_input: 'awaiting input',
  error: 'error',
  crashed: 'crashed',
  offline: 'offline',
  waking: 'waking',
  restarting: 'restarting',
  exited: 'exited',
  unknown: 'status unavailable',
});

const STATUS_CLASS = Object.freeze({
  working: 'working',
  idle: 'idle',
  awaiting_permission: 'awaiting',
  awaiting_input: 'awaiting',
  error: 'error',
  crashed: 'crashed',
  offline: 'offline',
  waking: 'waking',
  restarting: 'restarting',
  exited: 'exited',
  unknown: 'unknown',
});

const RECOVERY_STATUSES = new Set(['crashed', 'restarting', 'backoff', 'suppressed', 'recovered']);

const CONTROL_CHARS = /[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u180e\u200b-\u200f\u2028\u2029\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]+/g;

function cleanDetail(value) {
  return String(value ?? '')
    .replace(CONTROL_CHARS, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 160)
    .trim();
}

function roster(snapshotValue) {
  return Array.isArray(snapshotValue?.agents) ? snapshotValue.agents : [];
}

function paneSelector(pane) {
  // Older pane seeds only retained hideConv. It is still a valid roster
  // conversation selector, so keep those panes observable after a reload.
  const selector = pane?.seed?.agent || pane?.seed?.hideConv;
  return typeof selector === 'string' ? selector : '';
}

// findTerminalAgent matches either identity because pane seeds created before
// stable agent ids existed still carry a conversation id, while current row
// actions prefer the rotation-immune agent id.
export function findTerminalAgent(pane, snapshotValue) {
  const selector = paneSelector(pane);
  if (!selector) return null;
  return roster(snapshotValue).find((agent) => (
    agent && (agent.agent_id === selector || agent.conv_id === selector)
  )) || null;
}

function recoveryStatus(agent) {
  const recovery = cleanDetail(agent?.state?.recovery_status || agent?.recovery_status);
  if (!RECOVERY_STATUSES.has(recovery)) return '';
  // "recovered" is a transient informational state; if the process is back
  // online the live hook status is more useful than a stale recovery label.
  return recovery === 'recovered' && agent?.online ? '' : recovery;
}

function recoveryAnnotation(agent) {
  const recovery = cleanDetail(agent?.state?.recovery_status || agent?.recovery_status);
  return recovery === 'backoff' ? 'crash loop / backoff'
    : recovery === 'suppressed' ? 'recovery suppressed'
      : recovery === 'recovered' && agent?.online ? 'recovered' : '';
}

function backgroundActivity(agent) {
  const state = agent?.state || {};
  return Number(state.subagent_count || 0) > 0
    || Number(state.bg_shell_count || 0) > 0
    || Number(state.monitor_count || 0) > 0;
}

function statusKey(agent) {
  if (!agent) return 'unknown';
  if (!agent.online && agent.waking) return 'waking';

  const recovery = recoveryStatus(agent);
  if (recovery === 'restarting') return 'restarting';
  if (recovery === 'backoff' || recovery === 'suppressed') return 'error';
  if (recovery === 'crashed') return 'crashed';

  if (!agent.online) {
    return agent.state?.exit_reason === 'unexpected' ? 'crashed' : 'offline';
  }

  const status = agent.state?.status || '';
  if (status === 'working' || status === 'main_agent_idle') return 'working';
  if (status === 'awaiting_permission' || status === 'awaiting_input') return status;
  if (status === 'error') return 'error';
  if (status === 'exited') return 'exited';
  return 'idle';
}

function statusDescription(key, agent) {
  const state = agent?.state || {};
  let description = STATUS_LABELS[key] || STATUS_LABELS.unknown;
  if (key === 'working' && (state.status === 'main_agent_idle' || backgroundActivity(agent))) {
    description = 'working — background activity still running';
  }
  const annotation = recoveryAnnotation(agent);
  if (annotation) description += ` — ${annotation}`;
  if (key === 'error' || key === 'awaiting_permission' || key === 'awaiting_input') {
    const detail = cleanDetail(state.recovery_detail || state.status_detail);
    if (detail) description += ` — ${detail}`;
  }
  return description;
}

// terminalTabStatus is intentionally a plain object with fixed keys. Preact
// uses it for both data-* styling and the text-equivalent tab label, while
// tests can exercise every server state without constructing a DOM.
export function terminalTabStatus(pane, snapshotValue) {
  const agent = findTerminalAgent(pane, snapshotValue);
  const key = statusKey(agent);
  const description = statusDescription(key, agent);
  const label = pane?.label || 'terminal';
  return Object.freeze({
    key,
    className: STATUS_CLASS[key] || STATUS_CLASS.unknown,
    symbol: STATUS_SYMBOLS[key] || STATUS_SYMBOLS.unknown,
    label: STATUS_LABELS[key] || STATUS_LABELS.unknown,
    description,
    title: `${label} — ${description}`,
    ariaLabel: `${label}: ${description}`,
    agent,
  });
}

export const TERMINAL_TAB_STATUS_KEYS = Object.freeze(Object.keys(STATUS_LABELS));
