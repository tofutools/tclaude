import { h, render } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { applySort, MEMBER_ACCESSORS } from './sort.js';
import { visibleMemberCols, memberColHidden } from './member-columns.js';
import {
  shortAgentId, idTooltip, relTime, shortCwd, harnessCanRename, harnessCanRemoteControl,
  SLOP_SYMBOLS,
} from './helpers.js';
import { isWizardActive } from './slop.js';
import { ActionMenu, InlineEditor, useGroupsInteractions } from './groups-interactions.js';
import {
  humanNotificationSenderQuery, memberUnreadHumanCount,
} from './human-notification-attention.js';

const html = htm.bind(h);

function ThemeText({ regular, wizard = regular }) {
  return html`<span class="theme-copy-regular">${regular}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function wizardCopy(regular, wizard) {
  return isWizardActive() ? wizard : regular;
}

function memberAttrs(member) {
  return {
    'data-conv': member.conv_id,
    'data-agent': member.agent_id || member.conv_id,
    'data-label': member.title || member.conv_id,
  };
}

function AgentStatusDot({ member }) {
  const state = member.state || {};
  const label = member.title || member.conv_id;
  const online = !!member.online;
  const errored = online && state.status === 'error';
  const detail = errored ? (state.status_detail || 'error') : '';
  const recovery = state.recovery_status || '';
  let title = recovery
    ? `${statusInfo(state, online).title} — click to ${online ? 'turn off' : 'retry now'} ${label}`
    : errored
    ? `errored (${detail}) — click to turn off ${label} (asks first: soft exit or force kill)`
    : online
      ? `online — click to turn off ${label} (asks first: soft exit or force kill)`
      : `offline — click to turn on (wake ${label})`;
  const model = state.model || '';
  if (model) title += ` · ${online ? 'running on' : 'last used'} ${harnessLong(state.harness)} · ${model}`;
  const className = errored
    ? 'status-dot status-dot-error'
    : online ? 'status-dot status-dot-online' : 'status-dot status-dot-offline';
  return html`<button
    type="button" class=${className} data-act="dot-toggle" ...${memberAttrs(member)}
    data-online=${online ? '1' : '0'} title=${title} aria-label=${title}
  >${online ? '●' : '○'}</button>`;
}

const HARNESS_LABELS = {
  claude: { short: 'CC', long: 'Claude Code' },
  codex: { short: 'Codex', long: 'Codex CLI' },
};

function harnessLabels(name) {
  if (!name) return HARNESS_LABELS.claude;
  return HARNESS_LABELS[name] || { short: name, long: name };
}

function harnessLong(name) {
  return harnessLabels(name).long;
}

function shortModel(model) {
  let main = String(model || '').trim();
  if (!main) return '';
  let size = '';
  const paren = main.match(/\(([^)]*)\)\s*$/);
  if (paren) {
    main = main.slice(0, paren.index).trim();
    const match = paren[1].match(/\d+\s*[KMBkmb]/);
    if (match) size = match[0].replace(/\s+/g, '').toUpperCase();
  }
  const parts = main.split(/\s+/);
  const core = parts.length >= 2 ? parts[0][0].toUpperCase() + parts.slice(1).join(' ') : main;
  return size ? `${core} ${size}` : core;
}

function RemoteBadge({ member }) {
  if (!member.online || !member.state?.remote_control) return null;
  const title = 'Remote Access is ON — this agent is reachable from the Claude app/phone. Click to open its live session (Claude Code TUI) in a web terminal; Ctrl/Cmd-click opens it without leaving this tab. Best-known state (the harness has no readback); toggle it from the row’s ⚙ menu.';
  return html`<span class="remote-badge" data-act="web-open-window" ...${memberAttrs(member)} title=${title}>📱</span>`;
}

// BrokerRefusalBadge warns that agentd is REFUSING this agent's brokered
// telemetry callbacks (TCL-761). It matters because the refusal is silent
// everywhere else: the agent keeps working, but its status, cost, context
// meter and directory stop advancing — so the rest of this row quietly
// becomes fiction, and without the badge the operator's only clue is a
// row that looks merely idle.
//
// The count is per resolved session row and daemon-derived; a refused
// request's own claimed session id never puts a badge anywhere.
function BrokerRefusalBadge({ member }) {
  const state = member.state || {};
  const count = Number(state.broker_refusals || 0);
  if (!(count > 0)) return null;
  const since = relTime(state.broker_refusal_since);
  const detail = state.broker_refusal_detail || '';
  const title = `⚠ agentd has refused ${count} brokered callback${count === 1 ? '' : 's'} resolved to this session${since ? `, starting ${since}` : ''}`
    + `${detail ? ` (${detail})` : ''}.`
    + ' While this lasts, everything else on this row — status, model, cost, context, directory — is FROZEN at its last accepted value and may be badly out of date.'
    + ' The usual cause is a dead session row recorded against the same pid as this agent’s pane, so the daemon places the callback on the wrong row; the other is an agent presenting a session id that disagrees with the one its process ancestry resolves to.';
  return html`<span class="broker-refusal-badge" role="note" aria-label=${title} title=${title}>🚫</span>`;
}

export function HarnessLine({ member, snapshot }) {
  const state = member.state || {};
  const offline = !member.online;
  const metadataClass = `runtime-meta${offline ? ' runtime-meta-offline' : ''}`;
  const harness = state.harness || '';
  const labels = harnessLabels(harness);
  const model = state.model || '';
  // Both indicators trail the metadata text as bare glyphs, tightly packed:
  // "CC · O4.8 1M high 🔒 📱". The sandbox one used to own a framed chip on its
  // own line below, which cost a whole row of height to say what a padlock says.
  const sandbox = html`<${SandboxBadge} member=${member}
    showDetails=${!!snapshot?.recorded_sandbox_details_enabled} />`;
  const remote = html`<${RemoteBadge} member=${member} />`;
  const refused = html`<${BrokerRefusalBadge} member=${member} />`;
  if (!model) {
    // A pre-tick row has no metadata text to trail, but an armed indicator is
    // still worth a minimal line — including a sandbox verdict, which is
    // recorded at launch and so is known before the first statusline hook.
    //
    // A refusal warning belongs here most of all: the model is stamped by the
    // status line, so an agent whose brokered callbacks are ALL being refused
    // never gets one, and this branch is the only one it ever renders through.
    if (!harness || harness === 'claude') {
      const indicated = (member.online && state.remote_control)
        || !!sandboxIndicator(member)
        || Number(state.broker_refusals || 0) > 0;
      return indicated ? html`<div class="agent-harness">${sandbox}${remote}${refused}</div>` : null;
    }
    const title = `${offline ? 'Last used harness' : 'Harness'}: ${labels.long}`;
    return html`<div class="agent-harness" title=${title}><span class=${metadataClass} role="note" aria-label=${title}><span class="harness-name">${labels.short}</span></span>${sandbox}${remote}${refused}</div>`;
  }
  const effort = state.effort_level || '';
  const cost = Number(state.cost_usd || 0);
  const virtualCost = Number(state.virtual_cost_usd || 0);
  let title = `${offline ? 'Last used harness' : 'Harness'}: ${labels.long} — ${offline ? 'Last used model' : 'Model'}: ${model}`;
  if (effort) title += ` — ${offline ? 'Last used effort' : 'Effort'}: ${effort}`;
  if (cost > 0) title += ` — API cost this session: $${cost.toFixed(4)} (API/enterprise pricing — no subscription limits)`;
  if (virtualCost > 0) title += ` — WHAT-IF cost this session: $${virtualCost.toFixed(4)} (estimated if billed pay-per-token — you're on a subscription, so this is hypothetical, not a real charge)`;
  return html`<div class="agent-harness" title=${title}>
    <span class=${metadataClass} role="note" aria-label=${title}><span class="harness-name">${labels.short}</span><span class="harness-sep">·</span><span class="harness-model">${shortModel(model)}</span>
      ${effort ? html`<span class="harness-effort">${effort}</span>` : null}
      ${cost > 0 ? html`<span class="harness-cost">${cost >= 0.005 ? `$${cost.toFixed(2)}` : '<1¢'}</span>` : null}
      ${virtualCost > 0 ? html`<span class="harness-cost harness-cost-whatif" title="Estimated pay-per-token-equivalent cost this session — hypothetical, not a real charge (subscription)">${virtualCost >= 0.005 ? `≈$${virtualCost.toFixed(2)}` : '≈<1¢'}</span>` : null}
    </span>${sandbox}${remote}${refused}
  </div>`;
}

// osSandboxBadge reduces a recorded launch verdict to the glyph's safety
// posture. The detailed provenance used to become a very long native tooltip;
// the Groups tab only needs the resolved ON/OFF summary now.
function osSandboxBadge(mode, state, unverified, implementation) {
  if (state === 'on') {
    const danger = implementation === 'tclaude-layer' || implementation === 'stacked'
      ? false
      : implementation && implementation !== 'harness-builtin'
        ? true
        : unverified;
    return {
      status: danger ? 'UNKNOWN' : 'ON',
      danger,
    };
  }
  if (mode === 'on' || mode === 'off') return { status: 'OFF', danger: true };
  return null;
}

// sandboxIndicator resolves an agent's sandbox posture to the glyph that trails
// its harness line, or null when there is nothing to say.
function sandboxIndicator(member) {
  const mode = member.state?.sandbox_mode || '';
  const offline = !member.online;
  const stacked = member.state?.sandbox_implementation === 'stacked';
  const unenforcedOverride = (member.state?.sandbox_access_notices || []).some((notice) =>
    notice?.class === 'degradation'
      && notice?.axis === 'network'
      && notice?.reason === 'operator_unenforced_launch_override'
      && notice?.effect === 'not_enforced');
  // A recorded verdict wins: it is the resolved outcome, where the mode is only
  // the request. Absent one (a pre-column row, or Codex — whose --sandbox mode
  // IS its posture) the mode-driven branch below is unchanged.
  const state = member.state?.os_sandbox_state || '';
  if (state) {
    const badge = osSandboxBadge(mode, state,
      !!member.state?.os_sandbox_unverified, member.state?.sandbox_implementation || '');
    if (!badge) return null;
    if (unenforcedOverride) {
      return { status: 'NOT ENFORCED', danger: true, offline, glyph: '' };
    }
    return {
      status: badge.status, danger: badge.danger, offline,
      glyph: stacked && !badge.danger ? '🔒²' : '',
    };
  }
  if (!mode || mode === 'inherit') return null;
  // `off` is Claude-only (no other harness offers it) and means the OS sandbox
  // is disabled outright, so it is a danger glyph on a pre-verdict row too —
  // otherwise every legacy `off` agent keeps a padlock it has not earned.
  const danger = mode === 'danger-full-access' || mode === 'off' || unenforcedOverride;
  return {
    status: unenforcedOverride ? 'NOT ENFORCED' : danger ? 'OFF' : 'ON',
    danger,
    offline,
  };
}

function sandboxImplementationLabel(member, badge) {
  if (badge.status === 'OFF') return 'None';
  const implementation = member.state?.sandbox_implementation || 'harness-builtin';
  if (implementation === 'tclaude-layer') return 'TClaude';
  if (implementation === 'stacked') {
    return `${harnessLabels(member.state?.harness || 'claude').short}+TClaude`;
  }
  if (implementation === 'harness-builtin') {
    return harnessLabels(member.state?.harness || 'claude').short;
  }
  return 'Unknown';
}

function sandboxProfileLabel(member) {
  const names = (member.state?.sandbox_profiles || [])
    .map((profile) => profile.name)
    .filter(Boolean);
  if (names.length) return names.join(' + ');
  return member.state?.sandbox_profiles_recorded ? 'None' : 'Not recorded';
}

function sandboxTooltip(member, badge, actionable, unlocked) {
  const lines = [
    `Status: ${unlocked && badge.status === 'OFF' ? 'TEMP OFF' : badge.status}`,
    `Implementation: ${sandboxImplementationLabel(member, badge)}`,
    `Profile: ${sandboxProfileLabel(member)}`,
  ];
  for (const notice of member.state?.sandbox_access_notices || []) {
    if (notice?.detail) lines.push(`Warning: ${notice.detail}`);
  }
  if (actionable) {
    lines.push(unlocked ? 'Click to restore normal sandbox' : 'Click to temporarily disable');
  }
  return lines.join('\n');
}

// Exact producer literals only. Do not replace these keys with substring
// matching: profile names and operator-controlled provenance can occur in
// neighbouring recorded strings.
const OPENCODE_PRIVATE_NOTE = 'OpenCode state isolation: per-agent-private XDG state; auth.json and mcp-auth.json are seeded once from ambient credentials, then refresh independently per agent';
const OPENCODE_LEGACY_NOTE = 'OpenCode state isolation: legacy shared XDG retained for this existing conversation; start a new agent for per-agent-private state.';
const SANDBOX_FIDELITY = new Map([
  // TCL-790 ruling: Linux host-open keeps ambient host networking/sockets.
  ['tclaude-layer\0tclaude-layer (bubblewrap; host network)',
    'Known partial boundary: host networking and ambient host Unix sockets remained reachable.'],
  // TCL-790 ruling: Linux isolated owns a private network/PID/root boundary.
  ['tclaude-layer\0tclaude-layer (bubblewrap; isolated network; host loopback/IDE bridge unavailable; isolated PIDs; constructed root; agentd socket allowlisted)',
    'Recorded fidelity: isolated network and PIDs, a constructed root, and only the allowlisted agentd control socket.'],
  // TCL-790 ruling: the OpenCode server, not its attach pane/control path, is confined.
  ['tclaude-layer\0tclaude-layer (bubblewrap; OpenCode tool-executing server confined)',
    'Known partial boundary: the attach pane stayed outside; authenticated loopback control traffic, host networking, and ambient host Unix sockets remained reachable.'],
  // TCL-781 + TCL-790 rulings: Linux OpenCode with private XDG state.
  [`tclaude-layer\0tclaude-layer (bubblewrap; OpenCode tool-executing server confined); ${OPENCODE_PRIVATE_NOTE}`,
    'Known partial boundary: the attach pane stayed outside; authenticated loopback control traffic, host networking, and ambient host Unix sockets remained reachable. Per-agent-private XDG state was recorded.'],
  // TCL-781 + TCL-790 rulings: Linux OpenCode legacy shared XDG state.
  [`tclaude-layer\0tclaude-layer (bubblewrap; OpenCode tool-executing server confined); ${OPENCODE_LEGACY_NOTE}`,
    'Known partial boundary: the attach pane stayed outside; authenticated loopback control traffic, host networking, and ambient host Unix sockets remained reachable. Legacy shared XDG state was recorded.'],
  // TCL-790 ruling: Darwin host-open has Seatbelt path filtering, not namespaces.
  ['tclaude-layer\0tclaude-layer (Seatbelt/sandbox-exec; host network)',
    'Known partial boundary: no mount namespace; hidden paths remained enumerable; host networking and ambient host Unix sockets remained reachable.'],
  // TCL-790 ruling: Darwin isolated still has no PID namespace or constructed root.
  ['tclaude-layer\0tclaude-layer (Seatbelt/sandbox-exec; isolated network; host loopback/IDE bridge unavailable; agentd socket allowlisted)',
    'Known partial boundary: no PID isolation or constructed root, and hidden paths remained enumerable.'],
  // TCL-781 + TCL-790 rulings: Darwin OpenCode keeps config-base writes ambient.
  ['tclaude-layer\0tclaude-layer (Seatbelt/sandbox-exec; OpenCode tool-executing server confined; mutable XDG privacy covers data/cache/state only; config-base writes are not redirected)',
    'Known partial boundary: the attach pane stayed outside; mutable XDG privacy covered data/cache/state only, and config-base writes were not redirected.'],
  // TCL-781 + TCL-790 rulings: Darwin OpenCode with private XDG state.
  [`tclaude-layer\0tclaude-layer (Seatbelt/sandbox-exec; OpenCode tool-executing server confined; mutable XDG privacy covers data/cache/state only; config-base writes are not redirected); ${OPENCODE_PRIVATE_NOTE}`,
    'Known partial boundary: the attach pane stayed outside; mutable XDG privacy covered data/cache/state only, and config-base writes were not redirected. Per-agent-private XDG state was recorded.'],
  // TCL-781 + TCL-790 rulings: Darwin OpenCode legacy shared XDG state.
  [`tclaude-layer\0tclaude-layer (Seatbelt/sandbox-exec; OpenCode tool-executing server confined; mutable XDG privacy covers data/cache/state only; config-base writes are not redirected); ${OPENCODE_LEGACY_NOTE}`,
    'Known partial boundary: the attach pane stayed outside; mutable XDG privacy covered data/cache/state only, and config-base writes were not redirected. Legacy shared XDG state was recorded.'],
  // TCL-790 ruling: a host-open stacked outer wall retains ambient host sockets.
  ['stacked\0Stacked: tclaude bwrap (host-open) + Claude SRT bwrap/seccomp',
    'Known partial boundary: both recorded walls were active, but the outer wall retained host networking and ambient host Unix sockets.'],
  // TCL-790 ruling: a host-open stacked outer wall retains ambient host sockets.
  ['stacked\0Stacked: tclaude bwrap (host-open) + Codex bwrap managed profile',
    'Known partial boundary: both recorded walls were active, but the outer wall retained host networking and ambient host Unix sockets.'],
  // TCL-790 ruling: isolated stacked launches record both walls plus namespace fidelity.
  ['stacked\0Stacked: tclaude bwrap (isolated network/PIDs; constructed root) + Claude SRT bwrap/seccomp',
    'Recorded fidelity: both walls were active inside an isolated network/PID namespace and constructed root.'],
  // TCL-790 ruling: isolated stacked launches record both walls plus namespace fidelity.
  ['stacked\0Stacked: tclaude bwrap (isolated network/PIDs; constructed root) + Codex bwrap managed profile',
    'Recorded fidelity: both walls were active inside an isolated network/PID namespace and constructed root.'],
]);

function sandboxRecordedDetails(member) {
  const state = member.state || {};
  const implementation = state.sandbox_implementation || 'harness-builtin';
  const source = state.os_sandbox_source || state.sandbox_mode_source || '';
  const lines = [
    `Status: ${state.os_sandbox_state || state.sandbox_mode || 'Not recorded'}`,
    `Implementation: ${implementation}`,
    `Mode: ${state.sandbox_mode || 'Not recorded'}`,
    `Profile: ${sandboxProfileLabel(member)}`,
    `Source: ${source || 'Not recorded'}`,
  ];
  for (const notice of state.sandbox_access_notices || []) {
    if (notice?.detail) lines.push(`Notice: ${notice.detail}`);
  }
  const fidelity = SANDBOX_FIDELITY.get(`${implementation}\0${source}`);
  if (fidelity) lines.push(`Fidelity: ${fidelity}`);
  else if (state.os_sandbox_unverified) {
    lines.push('Fidelity: Recorded as unverified; no further fidelity detail was recorded.');
  }
  return lines.join('\n');
}

export function SandboxBadge({ member, showDetails = false }) {
  const badge = sandboxIndicator(member);
  if (!badge) return null;
  const unlocked = !!member.state?.temporary_sandbox_mode;
  // A warning can also mean a normally unconfined launch or an unverified
  // verdict. Only the warning produced by this temporary override is a
  // restore shortcut; otherwise clicking a warning would misleadingly offer
  // to "unlock" an agent that is already unconfined.
  const actionable = !!member.online && (unlocked || !badge.danger);
  const action = unlocked ? 'restore' : 'unlock';
  const title = sandboxTooltip(member, badge, actionable, unlocked);
  const className = `sandbox-badge${badge.danger ? ' sandbox-danger' : ''}${badge.offline ? ' runtime-meta-offline' : ''}${actionable ? ' sandbox-action' : ''}`;
  // aria-label carries the same full text as the tooltip: a glyph-only
  // indicator whose whole meaning is the hover would otherwise be pointer-only.
  return html`<span class="sandbox-badge-group"><span class=${className} role=${actionable ? 'button' : 'note'}
    tabindex=${actionable ? '0' : null}
    data-act=${actionable ? 'sandbox-restart' : null}
    data-action=${actionable ? action : null}
    ...${actionable ? memberAttrs(member) : {}}
    aria-label=${title} title=${title}>${badge.danger ? '⚠' : (badge.glyph || '🔒')}</span>${showDetails ? html`<button
    type="button" class="sandbox-details-chevron" data-act="sandbox-details"
    data-details=${sandboxRecordedDetails(member)} ...${memberAttrs(member)}
    title="Recorded sandbox details" aria-label="Recorded sandbox details">›</button>` : null}</span>`;
}

function statusInfo(state, online) {
  const recovery = state?.recovery_status || '';
  if (recovery) {
    const labels = {
      crashed: 'crashed', restarting: 'restarting', backoff: 'crash loop / backoff',
      recovered: 'recovered automatically', suppressed: 'recovery suppressed',
    };
    const status = labels[recovery] || recovery;
    const detail = state?.recovery_detail || '';
    return { status, detail, title: detail ? `${status}: ${detail}` : status };
  }
  if (!online) {
    const status = state?.exit_reason === 'unexpected' ? 'crashed' : 'offline';
    const age = relTime(state?.last_hook);
    return {
      status,
      detail: '',
      title: status === 'crashed'
        ? `process ended without a clean exit — crash, kill, or reboot${age ? ` · last active ${age}` : ''}`
        : age ? `offline — last active ${age}` : 'offline',
    };
  }
  const status = state?.status || '';
  const detail = state?.status_detail || '';
  return { status: status || 'online', detail, title: status && detail ? `${status}: ${detail}` : status || 'online' };
}

function StatePill({ state, online, conv }) {
  const info = statusInfo(state, online);
  let className = online ? 'state-idle' : 'state-offline';
  if (info.status === 'crashed') className = 'state-crashed';
  else if (info.status === 'restarting' || info.status === 'recovered automatically') className = 'state-working';
  else if (info.status === 'crash loop / backoff' || info.status === 'recovery suppressed') className = 'state-error';
  else if (info.status === 'working' || info.status === 'main_agent_idle') className = 'state-working';
  else if (info.status === 'idle') className = 'state-idle';
  else if (info.status === 'awaiting_permission' || info.status === 'awaiting_input') className = 'state-awaiting';
  else if (info.status === 'error') className = 'state-error';
  else if (info.status === 'exited') className = 'state-exited';
  // The activity badges already carry the live sub-agent/background-shell
  // counts. Keep this transitional state compact instead of repeating those
  // counts across most of the row; the pill tooltip retains the full detail.
  const backgroundActive = info.status === 'main_agent_idle';
  const label = backgroundActive
    ? 'idle + work'
    : info.detail ? `${info.status}: ${info.detail}` : info.status;
  const ariaLabel = backgroundActive
    ? `idle; background work is still running${info.detail ? `: ${info.detail}` : ''}`
    : info.title;
  return html`<span class="state-pill-wrap" tabindex=${'0'} title=${info.title}
    aria-label=${ariaLabel} data-full-status=${info.title}>
    <span class=${`state-pill ${className}`} aria-hidden="true">${label}</span>
    <${SlopMachine} state=${state} online=${online} conv=${conv} />
    <${WizardPill} state=${state} online=${online} conv=${conv} />
  </span>`;
}

const SLOP_STOPPED = {
  idle: ['7️⃣', '7️⃣', '7️⃣'], awaiting_permission: ['⏳', '❓', '⏳'],
  awaiting_input: ['⏳', '❓', '⏳'], error: ['💥', '❌', '💥'],
  crashed: ['💀', '💀', '💀'], exited: ['—', '—', '—'], offline: ['—', '—', '—'],
};

function slopHash(value) {
  let hash = 5381;
  for (let i = 0; i < value.length; i++) hash = ((hash << 5) + hash + value.charCodeAt(i)) >>> 0;
  return hash;
}

function SlopReels({ status, conv }) {
  const stopped = SLOP_STOPPED[status];
  if (stopped) return stopped.map((glyph, i) => html`<span key=${i} class="slop-reel slop-static">${glyph}</span>`);
  const hash = slopHash(conv);
  const offsets = [hash % 8, (hash >>> 3) % 8, (hash >>> 7) % 8];
  return offsets.map((offset, reel) => html`<span key=${reel} class="slop-reel"><span class="slop-strip">
    ${[...Array(9)].map((_, i) => html`<span key=${i}>${SLOP_SYMBOLS[(i + offset) % 8]}</span>`)}
  </span></span>`);
}

export function SlopMachine({ state, online, conv }) {
  const hostRef = useRef(null);
  const status = online
    ? (state?.status || 'idle')
    : state?.exit_reason === 'unexpected' ? 'crashed' : 'offline';
  useLayoutEffect(() => {
    const host = hostRef.current;
    // The parent Groups root renders this empty outer host forever and never
    // reconciles beneath it. Each status identity receives a fresh nested-root
    // host: slop-fx may replace the OUTER host's children during a manual pull,
    // detaching the old root without corrupting the parent tree. On hand-back
    // we replace the foreign children first and mount into a new root, so Preact
    // never reconciles bookkeeping against nodes the pull already replaced.
    const root = document.createElement('span');
    root.className = 'slop-reels-root';
    root.setAttribute('data-preact-root', 'slop-reels');
    host.replaceChildren(root);
    render(html`<${SlopReels} status=${status} conv=${conv || ''} />`, root);
    return () => {
      // If slop-fx detached this root, it is already an opaque abandoned tree;
      // rendering into it would reconcile against foreign/missing DOM. Its
      // pure reel VNodes own no effects and become collectible with this closure.
      if (root.parentNode !== host) return;
      render(null, root);
      root.remove();
    };
  }, [status, conv]);
  return html`<span ref=${hostRef} class="slop-machine" data-opaque-host="slop-reels"
    data-status=${status} data-conv=${conv || ''} aria-hidden="true"></span>`;
}

const WIZARD_STATE = {
  working: ['⚗️', 'Channeling'], main_agent_idle: ['⚗️', 'Channeling'], idle: ['🕯️', 'Meditating'],
  awaiting_permission: ['📜', 'Awaiting decree'], awaiting_input: ['🗝️', 'Awaiting a key'],
  error: ['💥', 'Spell backfired'], crashed: ['💀', 'Slain by a grue'], exited: ['🪦', 'Departed'], offline: ['🪦', 'Departed'],
};

function WizardPill({ state, online, conv }) {
  const status = online
    ? (state?.status || 'idle')
    : state?.exit_reason === 'unexpected' ? 'crashed' : 'offline';
  const [glyph, label] = WIZARD_STATE[status] || ['✨', status];
  return html`<span class="wizard-pill" data-status=${status} data-conv=${conv || ''}
    aria-hidden="true"><span class="wizard-pill-glyph">${glyph}</span> ${label}</span>`;
}

function fmtTokens(value) {
  const n = Number(value) || 0;
  if (n >= 1000000) return `${(n / 1000000).toFixed(n % 1000000 === 0 ? 0 : 1)}M`;
  if (n >= 1000) return `${Math.round(n / 1000)}k`;
  return String(n);
}

function ContextMeter({ state }) {
  const pct = Math.max(0, Math.min(100, Number(state?.context_pct || 0)));
  const known = pct > 0 || Number(state?.context_window_size || 0) > 0;
  const filled = pct > 0 ? Math.min(5, Math.max(1, Math.round(pct / 20))) : 0;
  const total = Number(state?.tokens_input || 0) + Number(state?.tokens_output || 0);
  // The window here is already the effective one and context_pct is measured
  // against it, so the regular usage figures are sufficient on their own.
  const win = Number(state?.context_window_size || 0);
  const regularTitle = !known ? 'context window: usage not reported yet'
    : win > 0 && total > 0 ? `context: ${fmtTokens(total)} / ${fmtTokens(win)} tokens — ${Math.round(pct)}%`
      : `context: ${Math.round(pct)}% full`;
  const wizardTitle = !known ? '🔮 Mana reserves: not yet divined'
    : win > 0 && total > 0 ? `🔮 Mana: ${fmtTokens(total)} / ${fmtTokens(win)} channeled — ${Math.round(pct)}%`
      : `🔮 Mana: ${Math.round(pct)}% channeled`;
  const segments = [...Array(5)].map((_, i) => {
    const band = i >= 4 ? 'red' : i >= 2 ? 'yellow' : 'green';
    return html`<span key=${i} class=${`ctx-seg${i < filled ? ` lit-${band}` : ''}`}></span>`;
  });
  const unknown = known ? '' : ' ctx-unknown';
  return html`<span class=${`ctx-meter ctx-regular${unknown}`} title=${regularTitle}>${segments}</span><span class=${`ctx-meter ctx-mana${unknown}`} title=${wizardTitle}>${segments}</span>`;
}

function ActivityBadges({ state }) {
  const subagents = Number(state?.subagent_count || 0);
  const shells = Number(state?.bg_shell_count || 0);
  if (subagents <= 0 && shells <= 0) return null;
  const subagentTitle = `${subagents} sub-agent${subagents === 1 ? '' : 's'} still running under this agent`;
  // Background shells are the other reason an "idle" agent isn't done:
  // a `Bash` launched with run_in_background outlives the turn, and the
  // count is reconciled against the agent's live descendant processes.
  const shellTitle = `${shells} background shell command${shells === 1 ? '' : 's'} still running under this agent`;
  return html`<span class="activity-badges">${subagents > 0 ? html`<span class="activity-badge badge-subagents" title=${subagentTitle} aria-label=${subagentTitle}>🤖+${subagents}</span>` : null}${shells > 0 ? html`<span class="activity-badge badge-bg-shells" title=${shellTitle} aria-label=${shellTitle}>⚙+${shells}</span>` : null}</span>`;
}

function StateCell({ member }) {
  const state = member.state || {};
  return html`<td class="state-cell"><${ContextMeter} state=${state} /><${StatePill}
    state=${state} online=${member.online} conv=${member.conv_id} />${member.online
    ? html`<${ActivityBadges} state=${state} />` : null}</td>`;
}

function EyeIcon({ hidden = false }) {
  return hidden
    ? html`<svg class="eye-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"></path><line x1="1" y1="1" x2="23" y2="23"></line></svg>`
    : html`<svg class="eye-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"></path><circle cx="12" cy="12" r="3"></circle></svg>`;
}

function TrashIcon() {
  return html`<svg class="trash-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path><line x1="10" y1="11" x2="10" y2="17"></line><line x1="14" y1="11" x2="14" y2="17"></line></svg>`;
}

function menuMemberAttrs(member, selector) {
  if (!member) return {};
  const attrs = { 'data-label': member.title || member.conv_id };
  if (selector !== 'label') attrs['data-conv'] = member.conv_id;
  if (selector === 'agent') attrs['data-agent'] = member.agent_id || member.conv_id;
  return attrs;
}

function MenuButton({ member, selector = 'agent', group, act, regular, wizard = regular, title, className, disabled, attrs = {} }) {
  return html`<button role="menuitem" class=${className} data-act=${act} ...${menuMemberAttrs(member, selector)} data-group=${group?.name} ...${attrs} title=${title} disabled=${disabled}><${ThemeText} regular=${regular} wizard=${wizard} /></button>`;
}

function NotifyMenuItem({ member }) {
  const label = member.title || member.conv_id;
  const mode = member.notify || 'inherit';
  const effective = !!member.notify_effective;
  const glyph = mode === 'off' || (mode === 'inherit' && !effective) ? '🔕' : '🔔';
  let regular, wizard, title;
  if (mode === 'off') {
    regular = `${glyph} notify: off`; wizard = `${glyph} omens: silent`;
    title = wizardCopy(`notifications muted for ${label} — click to force ON (overrides a group mute)`, `omens silenced for familiar ${label} — click to restore them (overrides a party silence)`);
  } else if (mode === 'on') {
    regular = `${glyph} notify: on`; wizard = `${glyph} omens: on`;
    title = wizardCopy(`notifications forced ON for ${label} (overrides a group mute) — click to inherit from group`, `omens forced ON for familiar ${label} (overrides a party silence) — click to inherit from the party`);
  } else {
    regular = `${glyph} notify: inherit (${effective ? 'on' : 'off'})`; wizard = `${glyph} omens: inherit (${effective ? 'on' : 'silent'})`;
    title = wizardCopy(`notifications inherit (currently ${effective ? 'on' : 'off — a group is muted'}) for ${label} — click to mute`, `omens inherit from the party (currently ${effective ? 'on' : 'silent'}) for familiar ${label} — click to silence`);
  }
  return html`<${MenuButton} member=${member} act="toggle-agent-notify" attrs=${{ 'data-mode': mode }} regular=${regular} wizard=${wizard} title=${title} />`;
}

function RemoteMenuItem({ member, canRemote }) {
  if (!canRemote) return null;
  const label = member.title || member.conv_id;
  const on = !!member.state?.remote_control;
  const glyph = on ? '📱' : '📴';
  const title = on
    ? wizardCopy(`Remote Access is ON for ${label} — reachable from the Claude app/phone. Click to turn it OFF.`, `Remote scrying is ON for familiar ${label} — reachable from the Claude app/phone. Click to close it.`)
    : wizardCopy(`Remote Access is OFF for ${label}. Click to turn it ON — expose this agent to the Claude app/phone.`, `Remote scrying is OFF for familiar ${label}. Click to open it to the Claude app/phone.`);
  return html`<${MenuButton} member=${member} act="toggle-remote-control" attrs=${{ 'data-intent': on ? 'off' : 'on' }} regular=${`${glyph} remote: ${on ? 'on' : 'off'}`} wizard=${`${glyph} remote scrying: ${on ? 'on' : 'off'}`} title=${title} />`;
}

function RestartMenuItem({ member }) {
  const label = member.title || member.conv_id;
  const regular = '↻ restart';
  const title = !member.online
    ? `${regular} is unavailable while ${label} is offline`
    : `Stop and restart ${label} under its current launch configuration. Re-resolves sandbox-profile rules. Requires the agent to be fully idle with no background agents or shell commands.`;
  return html`<${MenuButton}
    member=${member} act="restart" regular=${regular} wizard="↻ reincant familiar"
    title=${title} disabled=${!member.online}
  />`;
}

function SandboxRestartMenuItem({ member }) {
  const unlocked = !!member.state?.temporary_sandbox_mode;
  const label = member.title || member.conv_id;
  const regular = unlocked ? '🔒 restore sandbox + restart' : '⚠ restart without sandbox';
  const wizard = unlocked ? '🔒 restore ward + reincant' : '⚠ reincant without ward';
  const title = !member.online
    ? `${regular} is unavailable while ${label} is offline`
    : unlocked
      ? `Stop and restart ${label} with its preserved normal sandbox configuration. Requires the agent to be fully idle with no background agents or shell commands.`
      : `Stop and restart ${label} with its sandbox OFF and full machine access. Its normal sandbox configuration is preserved for restoration. Requires the agent to be fully idle with no background agents or shell commands.`;
  return html`<${MenuButton}
    member=${member} act="sandbox-restart" className=${unlocked ? undefined : 'danger'}
    attrs=${{ 'data-action': unlocked ? 'restore' : 'unlock' }}
    regular=${regular} wizard=${wizard} title=${title} disabled=${!member.online}
  />`;
}

function MenuSeparator() {
  return html`<div class="menu-sep" role="separator"></div>`;
}

function MemberMenu({ member, group, snapshot, actions, ungrouped }) {
  const interactions = useGroupsInteractions();
  const label = member.title || member.conv_id;
  const canRemote = harnessCanRemoteControl(snapshot, member.state?.harness);
  const prefill = JSON.stringify({ targetMode: 'solo', target: member.agent_id || member.conv_id, owner: member.agent_id || member.conv_id });
  return html`
    <${MenuButton} member=${member} act="view-agent-messages" regular="view messages" wizard="view missives" title=${wizardCopy("Open this agent's messages in the Messages tab", "Open this familiar's missives in the Messages tab")} />
    <${MenuButton} member=${member} act="term" regular="term" wizard="scrying portal" title=${wizardCopy("Open a terminal in this agent's working directory", "Open a scrying portal in this familiar's working directory")} />
    <${MenuButton} member=${member} act="web-term" regular="web term" wizard="web scrying portal" title=${wizardCopy("Open a terminal in this agent's working directory, in the browser (always a web terminal — never a native window)", "Open a browser scrying portal in this familiar's working directory")} />
    <${MenuButton} member=${member} act="open-window" regular="open window" wizard="reveal portal" title=${wizardCopy("Open a terminal window attached to this agent's live session (its Claude Code TUI)", "Reveal a scrying portal onto this familiar's live session")} />
    <${MenuButton} member=${member} act="web-open-window" regular="web window" wizard="web portal" title=${wizardCopy("Open a terminal attached to this agent's live session (its Claude Code TUI), in the browser (always a web terminal — never a native window). Ctrl/Cmd-click opens it without leaving this tab.", "Reveal a browser scrying portal onto this familiar's live session. Ctrl/Cmd-click opens it without leaving this tab.")} />
    <${MenuButton} member=${member} act="export-summary" regular="summary…" wizard="inscribe scroll…" disabled=${!member.online} title=${member.online ? wizardCopy('Ask this agent to produce a shareable export of the conversation (a summary / report) and download it here. Multiple files are zipped automatically.', 'Ask this familiar to inscribe a shareable account of its conversation and bring it here. Multiple scrolls are bundled automatically.') : wizardCopy('Export needs a running agent — it produces the file in its own session. Unavailable while the agent is offline.', 'The familiar must be channeling to inscribe an export. Unavailable while it slumbers.')} />
    <${MenuSeparator} />
    ${!ungrouped ? html`<${MenuButton} member=${member} group=${group} act="edit-member" attrs=${{ onClick: (event) => {
      event.preventDefault();
      event.stopPropagation();
      interactions.closeMenu(true);
      actions.openMemberEditor(member, group, 'title');
    } }} regular="edit" wizard="enchant" title=${wizardCopy('Edit this agent — title, role, description, ownership, permissions', 'Enchant this familiar — title, class, description, party ownership, and grimoire')} />
      <${MenuButton} member=${member} group=${group} act=${member.owner ? 'revoke-owner' : 'grant-owner'} className=${member.owner ? 'warn' : undefined} regular=${member.owner ? 'revoke owner' : 'make owner'} wizard=${member.owner ? 'revoke party owner' : 'make party owner'} title=${wizardCopy(member.owner ? 'Revoke group owner status' : 'Make this agent an owner of the group', member.owner ? 'Revoke party owner status' : 'Make this familiar an owner of the party')} />` : null}
    <${MenuButton} member=${member} selector="conv" act="perm-edit" regular="permissions" wizard="grimoire" title=${wizardCopy("Edit this agent's permanent permissions (grant / deny / inherit-default)", "Open this familiar's grimoire of permanent boons and bindings")} />
    <${MenuButton} member=${member} selector="conv" act="sudo-grant" regular="+ sudo" wizard="+ sudo" title=${wizardCopy('Grant a time-bounded sudo elevation to this agent', 'Grant this familiar a time-bounded sudo boon')} />
    <${NotifyMenuItem} member=${member} />
    <${RemoteMenuItem} member=${member} canRemote=${canRemote} />
    <${MenuButton} member=${member} selector="label" act="cron-new" attrs=${{ 'data-prefill': prefill }} regular="⏰ schedule…" wizard="⏳ bind ritual…" title=${wizardCopy(`Schedule a recurring nudge for ${label}`, `Bind a recurring ritual for familiar ${label}`)} />
    <${MenuSeparator} />
    <${MenuButton} member=${member} act="clone" attrs=${{ 'data-cwd': member.state?.cwd || member.cwd || '' }} regular="clone" wizard="mirror familiar" title=${wizardCopy('Fork a sibling agent that inherits identity (groups, perms, ownership). The original keeps running.', 'Mirror this familiar into a sibling that inherits its parties, boons, and ownership. The original keeps channeling.')} />
    <${MenuButton} member=${member} act="reincarnate" regular="reincarnate" wizard="reincarnate familiar" title=${wizardCopy('Reincarnate this agent — by default ask it to do so itself (it writes its own handoff); or force an immediate daemon-driven reincarnation.', 'Reincarnate this familiar — by default ask it to write its own handoff; or force its immediate return in a fresh vessel.')} />
    <${RestartMenuItem} member=${member} />
    <${SandboxRestartMenuItem} member=${member} />
    <${MenuSeparator} />
    ${ungrouped
      ? html`<${MenuButton} member=${member} selector="conv" act="retire-agent" className="warn" regular="retire" wizard="banish" title=${wizardCopy('Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored).', 'Banish this familiar — return it to a plain conversation, revoking its party memberships and boons. Reversible via reinstate.')} /><${MenuButton} member=${member} act="delete-agent" className="danger" regular="delete" wizard="erase familiar" title=${wizardCopy('Permanently delete this agent and conversation', 'Permanently erase this familiar and its conversation scroll')} />`
      : html`<${MenuButton} member=${member} group=${group} act="remove-member" className="danger" regular="remove" wizard="dismiss from party" title=${wizardCopy('Remove this agent from the group', 'Remove this familiar from the party')} /><${MenuButton} member=${member} selector="conv" act="retire-agent" className="warn" regular="retire" wizard="banish" title=${wizardCopy('Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored).', 'Banish this familiar — return it to a plain conversation, revoking its party memberships and boons. Reversible via reinstate.')} />`}
  `;
}

function MemberActions({ member, group, snapshot, actions, ungrouped, menuKey }) {
  const offlineWhy = member.online ? '' : ' — unavailable while the agent is offline';
  return html`<div class="row-actions">
    <button class="icon-btn" data-act="jump" ...${memberAttrs(member)} disabled=${!member.online} title=${`Focus this agent's terminal window; when using web terminals, Ctrl/Cmd-click keeps this tab open${offlineWhy}`} aria-label="Focus window"><${EyeIcon} /></button>
    <button class="icon-btn" data-act="hide" ...${memberAttrs(member)} disabled=${!member.online} title=${`Hide this agent's terminal window — detaches its tmux client. The agent keeps running.${offlineWhy}`} aria-label="Hide window"><${EyeIcon} hidden=${true} /></button>
    <button class="icon-btn warn" data-act="retire-agent" data-conv=${member.conv_id} data-label=${member.title || member.conv_id} title="Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored)." aria-label="Retire agent"><${TrashIcon} /></button>
    <${ActionMenu} menuKey=${menuKey} kind="row-menu"><${MemberMenu} member=${member} group=${group} snapshot=${snapshot} actions=${actions} ungrouped=${ungrouped} /><//>
  </div>`;
}

function SudoBadge({ grants, conv }) {
  if (!grants?.length) return null;
  const fmt = (seconds) => seconds <= 0 ? 'expired' : seconds < 60 ? `${seconds}s` : seconds < 3600 ? `${Math.floor(seconds / 60)}m${seconds % 60 ? `${seconds % 60}s` : ''}` : `${Math.floor(seconds / 3600)}h${Math.floor((seconds % 3600) / 60) ? `${Math.floor((seconds % 3600) / 60)}m` : ''}`;
  const title = `${grants.length} active sudo grant${grants.length === 1 ? '' : 's'} — click to manage:\n${grants.map((grant) => `${grant.slug} (expires in ${fmt(grant.remaining_seconds)})`).join('\n')}`;
  return html`<span class="sudo-badge" data-act="sudo-manage" data-conv=${grants[0].conv_id || conv || ''} title=${title}>🔓</span>`;
}

function HumanNotificationAttention({ member, snapshot }) {
  const unread = memberUnreadHumanCount(member, snapshot);
  if (!unread) return null;
  const label = member.title || member.conv_id;
  const title = `${unread} unread notification${unread === 1 ? '' : 's'} from ${label} — open in Messages`;
  return html`<button type="button" class="human-notification-attention"
    data-act="view-human-notifications" ...${memberAttrs(member)}
    data-sender=${humanNotificationSenderQuery(member)}
    aria-label=${title} title=${title}><span aria-hidden="true">!</span></button>`;
}

function MemberName({ member, snapshot, actions, grants, editorKey }) {
  const state = member.state || {};
  const canRename = harnessCanRename(snapshot, state.harness);
  const idPrefix = memberColHidden('id') ? `${idTooltip(member.agent_id, member.conv_id)} — ` : '';
  if (!canRename) return html`<div class="rowname"><${HumanNotificationAttention} member=${member} snapshot=${snapshot} /><span class="rowname-text rowname-fixed" title=${`${idPrefix}This agent's harness does not support renaming`}>${member.title || '(unnamed)'}</span><${SudoBadge} grants=${grants} conv=${member.conv_id} /></div>`;
  return html`<div class="rowname"><${HumanNotificationAttention} member=${member} snapshot=${snapshot} /><${InlineEditor}
    editorKey=${editorKey} value=${member.title || ''} className="rowname-input"
    placeholder="1-64 chars: A-Za-z0-9 _ - [ ] { } ( ) — Enter saves, Esc cancels"
    onCommit=${(value) => actions.renameAgent(member, value)}
    triggerProps=${{
      class: 'rowname-text', role: 'button', tabindex: '0', 'data-act': 'rename-name',
      ...memberAttrs(member), 'data-current': member.title || '', 'data-editor-key': editorKey,
      title: `${idPrefix}Click to rename this agent — Enter saves, Esc cancels`,
    }}
  >${member.title || '(unnamed)'}<//><${SudoBadge} grants=${grants} conv=${member.conv_id} /></div>`;
}

function openMemberCellEditor(event, actions, member, group, focus) {
  event.preventDefault();
  event.stopPropagation();
  event.currentTarget.focus();
  actions.openMemberEditor(member, group, focus);
}

function editableMemberCellAttrs(member, group, actions, act, focus) {
  return {
    'data-act': act, 'data-group': group.name, ...memberAttrs(member),
    role: 'button', tabindex: '0',
    onClick: (event) => openMemberCellEditor(event, actions, member, group, focus),
    onKeyDown: (event) => {
      if (event.isComposing || event.keyCode === 229) return;
      if (event.key !== 'Enter' && event.key !== ' ') return;
      openMemberCellEditor(event, actions, member, group, focus);
    },
  };
}

function RoleCell({ member, group, actions }) {
  const hasRole = member.role && member.role !== 'owner';
  const owner = member.owner ? html`<span class="owner-badge">owner</span>` : null;
  const pureOwner = member.owner && member.role === 'owner';
  if (!group || pureOwner) return member.owner ? html`${hasRole ? member.role : null}${hasRole ? ' ' : null}${owner}` : (member.role || '');
  return html`<span class="role-edit" ...${editableMemberCellAttrs(member, group, actions, 'edit-role', 'role')} title="Edit role, ownership and permissions">${hasRole ? member.role : null}${hasRole && member.owner ? ' ' : null}${owner || (!hasRole ? html`<span class="role-add">+ role</span>` : null)}</span>`;
}

function TagChips({ tags }) {
  if (!Array.isArray(tags) || !tags.length) return null;
  return html`<span class="agent-tags">${tags.map((tag) => html`<span key=${tag} class=${tag.startsWith('tf:') ? 'agent-tag agent-tag-tf' : 'agent-tag'} title=${tag.startsWith('tf:') ? `task force: ${tag.slice(3)}` : `tag: ${tag}`}>${tag}</span>`)}</span>`;
}

function DescrCell({ member, group, actions }) {
  const text = String(member.descr || '').trim();
  const body = html`${text ? html`<span class="descr-text">${text}</span>` : null}<${TagChips} tags=${member.tags} />`;
  const pureOwner = member.owner && member.role === 'owner';
  if (!group || pureOwner) return text || member.tags?.length ? body : html`<span class="muted">—</span>`;
  return html`<span class="descr-edit" ...${editableMemberCellAttrs(member, group, actions, 'edit-descr', 'descr')} title="Edit description and tags">${text || member.tags?.length ? body : html`<span class="descr-add">+ descr / tags</span>`}</span>`;
}

function StackedLocation({ start, current, differ }) {
  if (!differ) return current || start;
  return html`<div class="loc-pair"><span class="loc-row"><span class="loc-tag">init</span>${start}</span><span class="loc-row"><span class="loc-tag">now</span>${current}</span></div>`;
}

function CwdCell({ member }) {
  const startup = member.startup_dir || member.state?.cwd || '';
  const current = member.current_dir || '';
  const path = (value, which) => value
    ? html`<span class="cwd cwd-link" data-act="term-dir" ...${memberAttrs(member)} data-which=${which} title=${`Open a terminal here — ${value}`}>${shortCwd(value)}</span>`
    : html`<span class="cwd">—</span>`;
  return html`<${StackedLocation} start=${path(startup, 'start')} current=${path(current, 'worktree')} differ=${!!current && !!startup && current !== startup} />`;
}

function BranchCell({ member }) {
  const branch = (name, url, prNumber, prURL, prState) => {
    if (!name) return html`<span class="muted">—</span>`;
    const branchNode = url
      ? html`<a class="branch branch-link" href=${url} target="_blank" rel="noopener noreferrer" draggable=${false} title=${`Open branch on GitHub — ${name}`}>⎇ ${name}</a>`
      : html`<span class="branch" title=${`git branch: ${name}`}>⎇ ${name}</span>`;
    const stateClass = ['open', 'merged', 'closed'].includes(prState) ? `pr-state-${prState}` : 'pr-state-unknown';
    const stateLabel = prState ? prState[0].toUpperCase() + prState.slice(1) : 'Pull request';
    return html`${branchNode}${prNumber && prURL ? html` <a class=${`pr-link ${stateClass}`} href=${prURL} target="_blank" rel="noopener noreferrer" draggable=${false} title=${`${stateLabel} pull request #${prNumber}`}>#${prNumber}</a>` : null}`;
  };
  const start = branch(member.startup_branch || '', member.startup_branch_url || '', member.startup_pr_number || 0, member.startup_pr_url || '', member.startup_pr_state || '');
  const current = branch(member.branch || '', member.branch_url || '', member.branch_pr_number || 0, member.branch_pr_url || '', member.branch_pr_state || '');
  const seen = new Set([member.startup_pr_url, member.branch_pr_url].filter(Boolean));
  const presented = (member.presented_prs || []).filter((pr) => {
    const url = String(pr.url || '').trim();
    if (!url || seen.has(url) || !/^https?:\/\//i.test(url)) return false;
    seen.add(url); return true;
  });
  return html`<${StackedLocation} start=${start} current=${current} differ=${(member.startup_branch || '') !== (member.branch || '')} />${presented.length ? html` <span class="presented-prs">${presented.map((pr) => {
    const stateClass = ['open', 'merged', 'closed'].includes(pr.state) ? `pr-state-${pr.state}` : 'pr-state-unknown';
    return html`<a key=${pr.url} class=${`pr-link ${stateClass}`} href=${pr.url} target="_blank" rel="noopener noreferrer" draggable=${false} title=${pr.summary ? `${pr.summary} — ${pr.url}` : `Presented pull request — ${pr.url}`}>${pr.number ? `#${pr.number}` : pr.summary || 'PR'}</a>`;
  })}</span>` : null}`;
}

function TaskCell({ member }) {
  const url = String(member.task_ref_url || '').trim();
  const label = member.task_ref_label || url;
  const attrs = {
    role: 'button', tabindex: '0', 'data-act': 'edit-task', ...memberAttrs(member),
    'data-current': url, 'data-current-task-label': member.task_ref_label_override || '',
    title: url ? `Edit this task link or its display name — ${url}` : 'Click to attach a task link',
  };
  if (!url) return html`<span class="task-edit task-attach" ...${attrs}><${ThemeText} regular="＋ attach" wizard="✧ bind quest" /></span>`;
  const display = /^https?:\/\//i.test(url)
    ? html`<a class="task-ref task-link" href=${url} target="_blank" rel="noopener noreferrer" draggable=${false} title=${`Open task reference — ${url}`}>🔗 ${label}</a>`
    : html`<span class="task-ref muted" title=${url}>🔗 ${label}</span>`;
  return html`<span class="task-value">${display}<span class="task-edit task-edit-icon" ...${attrs} aria-label="Edit task link">✎</span></span>`;
}

function MemberCell({ column, member, group, snapshot, actions, grants, ungrouped, menuKey, editorKey }) {
  const state = member.state || {};
  switch (column.key) {
    // HarnessLine carries the sandbox/remote indicators itself — they trail its
    // metadata text instead of claiming a line of their own under the cell.
    case 'ctl': return html`<td><div class="agent-ctl"><${AgentStatusDot} member=${member} /><${MemberActions} member=${member} group=${group} snapshot=${snapshot} actions=${actions} ungrouped=${ungrouped} menuKey=${menuKey} /></div><${HarnessLine} member=${member} snapshot=${snapshot} /></td>`;
    case 'id': return html`<td class="id" title=${idTooltip(member.agent_id, member.conv_id)}>${shortAgentId(member.agent_id, member.conv_id)}</td>`;
    case 'title': return html`<td class="name-cell"><${MemberName} member=${member} snapshot=${snapshot} actions=${actions} grants=${grants} editorKey=${editorKey} /></td>`;
    case 'state': return html`<${StateCell} member=${member} />`;
    case 'last': return html`<td><span class="last-hook">${relTime(state.last_hook)}</span></td>`;
    case 'age': return html`<td><span class="last-hook" title=${member.created_at || ''}>${relTime(member.created_at)}</span></td>`;
    case 'cwd': return html`<td><${CwdCell} member=${member} /></td>`;
    case 'branch': return html`<td><${BranchCell} member=${member} /></td>`;
    case 'role': return html`<td><${RoleCell} member=${member} group=${group} actions=${actions} /></td>`;
    case 'task': return html`<td class="task-cell"><${TaskCell} member=${member} /></td>`;
    case 'descr': return html`<td class="descr-cell"><${DescrCell} member=${member} group=${group} actions=${actions} /></td>`;
    default: return html`<td></td>`;
  }
}

function MemberRow({ member, group, ungrouped, snapshot, actions, columns, tableKey }) {
  const interactions = useGroupsInteractions();
  const memberKey = member.agent_id || member.conv_id;
  const menuKey = `member:${tableKey}:${memberKey}:menu`;
  const editorKey = `member:${tableKey}:${memberKey}:name`;
  const grants = (snapshot?.sudo || []).filter((grant) => grant.conv_id === member.conv_id);
  return html`<tr
    class="dnd-draggable" draggable=${interactions.editorKey !== editorKey} data-key=${member.conv_id}
    data-dnd-source-ungrouped=${ungrouped ? '1' : undefined}
    data-dnd-source-group=${ungrouped ? undefined : group.name}
    data-dnd-conv=${member.conv_id} data-dnd-agent=${member.agent_id || member.conv_id}
    data-dnd-label=${member.title || member.conv_id}
  >${columns.map((column) => html`<${MemberCell} key=${column.key} column=${column} member=${member} group=${group} ungrouped=${ungrouped} snapshot=${snapshot} actions=${actions} grants=${grants} menuKey=${menuKey} editorKey=${editorKey} />`)}</tr>`;
}

export function MemberTable({ members, group, tableKey, ungrouped = false, snapshot, actions, SortHead }) {
  const columns = visibleMemberCols();
  return html`<table><${SortHead} table="members" columns=${columns} /><tbody>${applySort('members', members, MEMBER_ACCESSORS).map((member) => html`<${MemberRow} key=${member.conv_id} member=${member} group=${group} tableKey=${tableKey} ungrouped=${ungrouped} snapshot=${snapshot} actions=${actions} columns=${columns} />`)}</tbody></table>`;
}
// dashboard-imperative-boundary: media-effects
