import { Fragment, h, render } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { AgentBudgetBadge } from './usage-profile-chip.js';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { applySortState, tableSortState, MEMBER_ACCESSORS } from './sort.js';
import { visibleMemberCols, memberColHidden } from './member-columns.js';
import {
  shortAgentId, idTooltip, relTime, shortCwd, displayModel,
  harnessCanRename, harnessCanRemoteControl, harnessCanCompact,
  SLOP_SYMBOLS,
} from './helpers.js';
import { isWizardActive } from './slop.js';
import { MemberProcessCell } from './groups-process-indicators.js';
import { ActionMenu, InlineEditor, useGroupsInteractions } from './groups-interactions.js';
import { agentTitleHint } from './agent-title.js';
import {
  humanNotificationSenderQuery, memberHumanMessages, memberUnreadHumanCount,
  openHumanNotificationReader,
} from './human-notification-attention.js';
import { bodilessNotice } from './human-attachments.js';
import { HarnessMark } from './harness-mark.js';
import { fmtCredits, fmtExactUSD, fmtUSD } from './costs-model.js';
import { isPendingWake, clearPendingWake } from './waking-state.js';
import { PRChain } from './pr-chain.js';
import { ViewersBadge, remoteViewers, viewersOf } from './remote-viewers.js';
import { fleetState } from './fleet-state.js';
import { nodeRef, presenceView } from './agent-presence.js';
import { hubOffReason } from './fleet-model.js';
import { TELEPORT_ROUTES } from './launch-target-dialog.js';
import { unrestrictedOn } from './node-trust.js';
import { isPageNode } from './node-api.js';
import { discardQueuedSpawn, editQueuedSpawn, launchQueuedSpawn, queuedSpawnLabel } from './queued-spawns.js';

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
    'data-harness': member.state?.harness || '',
  };
}

// memberWaking folds the server-authoritative in-flight-resume flag together
// with this dashboard's own pending-wake optimism (set the moment the resume
// button is pressed, cleared once the row is seen online). Both mean the same
// interval: offline on the wire, seconds from either online or a failure.
function memberWaking(member) {
  const handle = member.agent_id || member.conv_id;
  if (member.online) {
    clearPendingWake(handle);
    return false;
  }
  return !!member.waking || isPendingWake(handle);
}

function AgentStatusDot({ member }) {
  // tclaude can't start it, so this dot has nothing to do when clicked.
  if (member.external_steward) {
    const title = 'External steward: an assistant running outside tclaude';
    return html`<span class="status-dot status-dot-offline status-dot-external" title=${title} aria-label=${title}>○</span>`;
  }
  const state = member.state || {};
  const label = member.title || member.conv_id;
  const online = !!member.online;
  // A resume is in flight: seconds of managed-server boot + sandbox + pane
  // fork during which the row is neither offline nor online. Render a pulsing
  // dot and make it inert — a second click would just queue behind the
  // in-flight wake's launch lock.
  if (memberWaking(member)) {
    const wakingTitle = `waking — ${label} is starting up`;
    return html`<span class="status-dot status-dot-waking" ...${memberAttrs(member)}
      title=${wakingTitle} aria-label=${wakingTitle} role="status">●</span>`;
  }
  const errored = online && state.status === 'error';
  const detail = errored ? (statusInfo(state, online).detail || 'error') : '';
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
  opencode: { short: 'OC', long: 'OpenCode' },
  copilot: { short: 'COP', long: 'GitHub Copilot CLI' },
  gemini: { short: 'GEM', long: 'Gemini CLI' },
  pi: { short: 'pi', long: 'pi' },
};

function harnessLabels(name) {
  if (!name) return HARNESS_LABELS.claude;
  return HARNESS_LABELS[name] || { short: name, long: name };
}

function harnessLong(name) {
  return harnessLabels(name).long;
}

const EFFORT_LABELS = {
  low: 'lw',
  medium: 'md',
  high: 'hi',
  xhigh: 'xi',
  max: 'mx',
};

function shortEffort(effort) {
  return EFFORT_LABELS[effort] || effort;
}

function RemoteBadge({ member }) {
  if (!member.online || !member.state?.remote_control) return null;
  const title = 'Remote Access is ON — this agent is reachable from the Claude app/phone. Click to open its live session (Claude Code TUI) in a web terminal; Ctrl/Cmd-click opens it without leaving this tab. Best-known state (the harness has no readback); toggle it from the row’s ⚙ menu.';
  return html`<span class="remote-badge" data-act="web-open-window" ...${memberAttrs(member)} title=${title}>📱</span>`;
}

// driveTooltip keeps the launch drive and its health on the harness/model
// line's general tooltip. The Groups table used to render separate "api" and
// "app" indicators for these facts; they made an implementation detail look
// like primary row metadata even though the full explanation still required a
// hover. Keep the diagnostic detail, including fail-closed recovery guidance,
// without adding another visible token to the row.
function driveTooltip(state) {
  const codexDrive = state.codex_app_server;
  if (!state.copilot_api && !codexDrive) return '';

  const codexDriveState = state.codex_app_server_state || 'warming';
  const codexDriveHealth = state.codex_app_server_health
    || (codexDriveState === 'unavailable' ? 'disconnected'
      : codexDriveState === 'dead' ? 'crashed' : codexDriveState);
  const driveFailed = (state.copilot_api && state.copilot_api_channel_failed)
    || (codexDrive && (codexDriveHealth === 'disconnected' || codexDriveHealth === 'crashed'));

  if (driveFailed) {
    return codexDrive
      ? `Drive: Codex app-server is ${codexDriveHealth}; messages are held and the explicit drive did not fall back to send-keys. Stop and resume to retry, or explicitly roll back with tclaude agent resume <agent> --send-keys${state.codex_app_server_detail ? `: ${state.codex_app_server_detail}` : ''}`
      : "Drive: Copilot embedded JSON-RPC API channel failed to come up; this agent's messages are being HELD, not delivered, and will be until it is relaunched. Its pane still works if you type into it";
  }
  if (state.copilot_api_connected) {
    return "Drive: Copilot embedded JSON-RPC API (copilot --ui-server), not tmux send-keys";
  }
  if (codexDrive && codexDriveHealth === 'ready') {
    return `Drive: Codex app-server ready${state.codex_app_server_version ? ` (${state.codex_app_server_version})` : ''}${state.codex_app_server_source ? ` via ${state.codex_app_server_source}` : ''}; TUI owns approvals; agentd connects only after TUI thread binding and uses non-subscribing snapshots (context remains rollout-derived)${state.codex_observer_updated ? `; last observation ${state.codex_observer_updated}` : ''}`;
  }
  if (codexDrive && codexDriveHealth === 'degraded') {
    return `Drive: Codex app-server is degraded; the connection or observation is being recovered and messages fail closed${state.codex_app_server_detail ? `: ${state.codex_app_server_detail}` : ''}`;
  }
  return codexDrive
    ? 'Drive: Codex app-server is warming; messages remain held until thread binding and connection verification finish; send-keys fallback is disabled'
    : "Drive: Copilot embedded JSON-RPC API is not connected yet — still starting up, or its channel failed to come up";
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

export function HarnessLine({ member, snapshot, local = true }) {
  if (member.external_steward) return html`<span class="runtime-meta runtime-meta-offline">External steward</span>`;
  const parent = member.spawned_by_external_steward;
  if (parent) return html`<${HarnessLineBody} member=${member} snapshot=${snapshot} local=${local} /><span class="runtime-meta">Started by ${parent}</span>`;
  return html`<${HarnessLineBody} member=${member} snapshot=${snapshot} local=${local} />`;
}

function HarnessLineBody({ member, snapshot, local }) {
  const state = member.state || {};
  const offline = !member.online;
  const metadataClass = `runtime-meta${offline ? ' runtime-meta-offline' : ''}`;
  const harness = state.harness || '';
  const labels = harnessLabels(harness);
  const model = state.model || '';
  // Both indicators trail the metadata text as bare glyphs, tightly packed:
  // "<logo> · Opus 4.8 high 🔒 📱". The sandbox one used to own a framed chip on its
  // own line below, which cost a whole row of height to say what a padlock says.
  const sandbox = html`<${SandboxBadge} member=${member}
    showDetails=${!!snapshot?.recorded_sandbox_details_enabled} />`;
  const fastMode = html`<${FastModeBadge} member=${member} />`;
  const remote = html`<${RemoteBadge} member=${member} />`;
  const refused = html`<${BrokerRefusalBadge} member=${member} />`;
  // A peer watching or typing into this agent's pane right now (federation).
  const viewed = local && member.online && member.agent_id ? viewersOf(remoteViewers.value, member.agent_id).length > 0 : false;
  const viewers = viewed ? html`<${ViewersBadge} agentId=${member.agent_id} compact />` : null;
  const drive = driveTooltip(state);
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
        || Number(state.broker_refusals || 0) > 0
        || viewed;
      return indicated ? html`<div class="agent-harness">${sandbox}${remote}${viewers}${refused}</div>` : null;
    }
    let title = `${offline ? 'Last used harness' : 'Harness'}: ${labels.long}`;
    if (drive) title += ` — ${drive}`;
    return html`<div class="agent-harness" title=${title} tabindex=${drive ? '0' : null}
      data-full-metadata=${drive ? title : null}><span class=${metadataClass} role="note" aria-label=${title}><${HarnessMark} name=${harness} shortLabel=${labels.short} longLabel=${labels.long} tooltip=${drive ? title : labels.long} /></span>${sandbox}${remote}${viewers}${refused}</div>`;
  }
  const effort = state.effort_level || '';
  const cost = Number(state.cost_usd || 0);
  const virtualCost = Number(state.virtual_cost_usd || 0);
  const virtualCredits = Number(state.virtual_cost_credits || 0);
  const copilotCredits = harness === 'copilot' && virtualCredits > 0;
  let title = `${offline ? 'Last used harness' : 'Harness'}: ${labels.long} — ${offline ? 'Last used model' : 'Model'}: ${model}`;
  if (effort) title += ` — ${offline ? 'Last used effort' : 'Effort'}: ${effort}`;
  if (drive) title += ` — ${drive}`;
  if (cost > 0) title += ` — API cost this session: ${fmtExactUSD(cost)} (API/enterprise pricing — no subscription limits)`;
  if (virtualCost > 0) {
    title += copilotCredits
      ? ` — WHAT-IF cost this session: ${fmtCredits(virtualCredits)} — ${fmtExactUSD(virtualCost)} subscription value (estimated if billed pay-per-token — you're on a subscription, so this is hypothetical, not a real charge)`
      : ` — WHAT-IF cost this session: ${fmtExactUSD(virtualCost)} (estimated if billed pay-per-token — you're on a subscription, so this is hypothetical, not a real charge)`;
  }
  const virtualTitle = copilotCredits
    ? `${fmtCredits(virtualCredits)} — ${fmtExactUSD(virtualCost)} subscription value — estimated pay-per-token-equivalent cost this session; hypothetical, not a real charge (subscription)`
    : 'Estimated pay-per-token-equivalent cost this session — hypothetical, not a real charge (subscription)';
  return html`<div class="agent-harness" title=${title} tabindex=${drive ? '0' : null}
    data-full-metadata=${drive ? title : null}>
    <span class=${metadataClass} role="note" aria-label=${title}><${HarnessMark} name=${harness} shortLabel=${labels.short} longLabel=${labels.long} tooltip=${drive ? title : labels.long} /><span class="harness-sep">·</span><span class="harness-model">${displayModel(model, harness)}</span>
      ${effort ? html`<span class="harness-effort" title=${effort}>${shortEffort(effort)}</span>` : null}
      ${cost > 0 ? html`<span class="harness-cost">${fmtUSD(cost)}</span>` : null}
      ${virtualCost > 0 ? html`<span class="harness-cost harness-cost-whatif" title=${virtualTitle}>≈${fmtUSD(virtualCost)}</span>` : null}
    </span>${sandbox}${fastMode}${remote}${viewers}${refused}
  </div>`;
}

// FastModeBadge follows the server's best-known state: a live rollout event
// when available, otherwise an explicit tclaude launch choice. Inherited or
// absent state and known-off state render nothing. Codex exposes /fast as a
// toggle, so only the known-on state is actionable.
export function FastModeBadge({ member }) {
  if (member.state?.fast_mode !== true) return null;
  const actionable = !!member.online;
  const title = actionable
    ? 'Codex Fast mode is ON — click to disable it for this agent'
    : 'Codex Fast mode was ON when this agent was last live';
  return html`<span class=${`fast-mode-badge${actionable ? ' fast-mode-action' : ' runtime-meta-offline'}`}
    role=${actionable ? 'button' : 'note'} tabindex=${actionable ? '0' : null}
    data-act=${actionable ? 'fast-mode-disable' : null}
    ...${actionable ? memberAttrs(member) : {}}
    aria-label=${title} title=${title}>⚡</span>`;
}

// osSandboxBadge reduces a recorded launch verdict to the glyph's safety
// posture. The detailed provenance used to become a very long native tooltip;
// the Groups tab only needs the resolved ON/OFF summary now.
function osSandboxBadge(mode, state, unverified, implementation) {
  if (state === 'on') {
    const danger = implementation === 'tclaude-layer'
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

// sandboxEnforcementGap names the worst gap the launch recorded between the
// configured rules and what is enforced, or '' when there is none. Any reason
// counts, so a rule the launch dropped or loosened can never earn the padlock,
// and neither can a reason added later. Resource limits are not confinement;
// they have their own line in the tooltip.
function sandboxEnforcementGap(member) {
  const effects = (member.state?.sandbox_access_notices || [])
    .filter((notice) => notice?.class === 'degradation' && notice?.axis !== 'resource_limits')
    .map((notice) => notice?.effect);
  if (effects.includes('not_enforced')) return 'NOT ENFORCED';
  return effects.includes('enforced_wider') ? 'PARTLY ENFORCED' : '';
}

// sandboxIndicator resolves an agent's sandbox posture to the glyph that trails
// its harness line, or null when there is nothing to say. `confined` separates
// a partly enforced sandbox, which shares the warning glyph but is still on,
// from the warnings whose agent has no working boundary to take down.
function sandboxIndicator(member) {
  const mode = member.state?.sandbox_mode || '';
  const offline = !member.online;
  const gap = sandboxEnforcementGap(member);
  const confined = gap === 'PARTLY ENFORCED';
  // A recorded verdict wins: it is the resolved outcome, where the mode is only
  // the request. Absent one (a pre-column row, or Codex — whose --sandbox mode
  // IS its posture) the mode-driven branch below is unchanged.
  const state = member.state?.os_sandbox_state || '';
  if (state) {
    const badge = osSandboxBadge(mode, state,
      !!member.state?.os_sandbox_unverified, member.state?.sandbox_implementation || '');
    if (!badge) return null;
    if (gap && !badge.danger) return { status: gap, danger: true, confined, offline };
    return { status: badge.status, danger: badge.danger, confined: !badge.danger, offline };
  }
  if (!mode || mode === 'inherit') return null;
  // These modes explicitly do not provide OS confinement. OpenCode's
  // `access-control` is a soft command/path filter, not a harness sandbox, so
  // it must not earn the padlock that confined Codex modes receive through
  // this pre-verdict fallback.
  const danger = mode === 'danger-full-access' || mode === 'off'
    || mode === 'access-control';
  if (danger) return { status: 'OFF', danger: true, confined: false, offline };
  if (gap) return { status: gap, danger: true, confined, offline };
  return { status: 'ON', danger: false, confined: true, offline };
}

function sandboxImplementationLabel(member, badge) {
  if (badge.status === 'OFF') return 'None';
  const implementation = member.state?.sandbox_implementation || 'harness-builtin';
  if (implementation === 'tclaude-layer') return 'tclaude’s built-in sandbox';
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

// resourceLimitLines describes the per-agent Linux cgroup, which is a separate
// axis from the sandbox posture above: it bounds how MUCH the workload may
// consume, never WHAT it may reach. A `resource-only` agent has a cgroup and no
// access boundary at all, so the two facts cannot share one verdict.
//
// Silent when no cgroup was requested. "Cgroup: off" on every macOS agent and
// every Linux launch without a budget would cost a tooltip line to say nothing;
// the lines appear exactly when there is a boundary to describe. An enforcement
// failure still shows here, because a recorded request that did not take effect
// is precisely what a reader must not be left guessing about.
function resourceLimitLines(member) {
  const state = member.state || {};
  if (!state.resource_cgroup) return [];
  // Both spellings mean the same for this line: the operator's own dashboard
  // override and a host with no delegated cgroup both leave the request
  // unenforced. The notice's own detail follows as a Warning line and names
  // which one it was.
  const unenforced = (state.sandbox_access_notices || []).some((notice) =>
    notice?.axis === 'resource_limits' && notice?.effect === 'not_enforced');
  const cpu = state.resource_cpu_limit;
  const pids = state.resource_pids_limit;
  return [
    `Cgroup: ${unenforced ? 'requested — not enforced' : 'on'}`,
    `Memory limit: ${state.resource_memory_limit || 'unlimited'}`,
    `CPU limit: ${typeof cpu === 'number' ? `${cpu} core${cpu === 1 ? '' : 's'}` : 'unlimited'}`,
    `PID limit: ${typeof pids === 'number' ? `${pids} process${pids === 1 ? '' : 'es'}` : 'unlimited'}`,
  ];
}

const CODEX_OWN_SANDBOX_UNLOCK_UNAVAILABLE = 'Temporary disable unavailable: Codex restores its persisted sandbox policy when this conversation resumes. Use tclaude’s built-in sandbox or start a new Codex conversation without the built-in sandbox.';

function codexOwnSandboxUnlockUnsupported(member, unlocked) {
  const state = member.state || {};
  const implementation = state.sandbox_implementation || 'harness-builtin';
  return !unlocked && state.harness === 'codex'
    && implementation === 'harness-builtin';
}

function sandboxTooltip(member, badge, actionable, unlocked) {
  const unlockUnsupported = badge.confined && codexOwnSandboxUnlockUnsupported(member, unlocked);
  const lines = [
    `Status: ${unlocked && badge.status === 'OFF' ? 'TEMP OFF' : badge.status}`,
    `Implementation: ${sandboxImplementationLabel(member, badge)}`,
    `Profile: ${sandboxProfileLabel(member)}`,
    ...resourceLimitLines(member),
  ];
  if (badge.status === 'PARTLY ENFORCED') {
    lines.push('The sandbox is on, but some rules are enforced more loosely than configured.');
  }
  for (const notice of member.state?.sandbox_access_notices || []) {
    if (notice?.detail) lines.push(`Warning: ${notice.detail}`);
  }
  if (actionable) {
    lines.push(unlocked ? 'Click to restore normal sandbox' : 'Click to temporarily disable');
  } else if (unlockUnsupported) {
    lines.push(CODEX_OWN_SANDBOX_UNLOCK_UNAVAILABLE);
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
]);

function sandboxRecordedDetails(member) {
  const state = member.state || {};
  const implementation = state.sandbox_implementation || 'harness-builtin';
  const source = state.os_sandbox_source || state.sandbox_mode_source || '';
  const lines = [
    `Status: ${state.os_sandbox_state || state.sandbox_mode || 'Not recorded'}`,
    `Implementation: ${implementation === 'tclaude-layer'
      ? sandboxImplementationLabel(member, { status: 'ON' }) : implementation}`,
    // Named for what it holds (TCL-1023): this is the HARNESS'S OWN sandbox
    // setting, not tclaude's enforcement. A tclaude-layer launch stands the
    // harness's inner wall down, so a bare `Mode: off` beside
    // `Implementation: tclaude-layer` read as "unconfined" when it means the
    // opposite. The Implementation line above says who actually enforces.
    `Harness sandbox mode: ${state.sandbox_mode === 'tclaude-layer'
      ? 'tclaude’s built-in sandbox' : state.sandbox_mode || 'Not recorded'}`,
    `Profile: ${sandboxProfileLabel(member)}`,
    `Source: ${source.replaceAll('tclaude-layer', 'tclaude’s sandbox') || 'Not recorded'}`,
    ...resourceLimitLines(member),
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
  // to "unlock" an agent that is already unconfined. A partly enforced
  // sandbox is still on, so its warning keeps the padlock's click.
  const unlockUnsupported = badge.confined && codexOwnSandboxUnlockUnsupported(member, unlocked);
  const actionable = !!member.online && !unlockUnsupported && (unlocked || badge.confined);
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
    aria-label=${title} title=${title}>${badge.danger ? '⚠' : '🔒'}</span>${showDetails ? html`<button
    type="button" class="sandbox-details-chevron" data-act="sandbox-details"
    data-details=${sandboxRecordedDetails(member)} ...${memberAttrs(member)}
    title="Recorded sandbox details" aria-label="Recorded sandbox details">›</button>` : null}</span>`;
}

// App-server observer provenance is retained in the session model for
// diagnostics, but it is not an activity state or an operational detail. Keep
// this UI boundary defensive even when a snapshot contains the raw model value
// (for example, a stale browser response during a daemon upgrade), while
// preserving similarly prefixed operational details.
const APP_SERVER_STATUS_PROVENANCE = new Set([
  'app-server snapshot',
  'app-server daemon reconnect',
  'app-server reconnect',
]);

function statusDetailForDisplay(detail) {
  const value = String(detail || '').trim();
  return APP_SERVER_STATUS_PROVENANCE.has(value.toLowerCase()) ? '' : value;
}

export function statusInfo(state, online) {
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
	    if (state?.harness === 'shell' && String(state?.exit_reason || '').startsWith('command_')) {
	      const outcome = String(state.exit_reason).replaceAll('_', ' ');
	      return { status: outcome, detail: '', title: outcome };
	    }
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
  const detail = statusDetailForDisplay(state?.status_detail);
  return { status: status || 'online', detail, title: status && detail ? `${status}: ${detail}` : status || 'online' };
}

export function StatePill({ state, online, conv, waking }) {
  if (!online && waking) {
    const title = 'resume in flight — server and pane are starting';
    return html`<span class="state-pill-wrap" tabindex=${'0'} title=${title} aria-label=${title}>
      <span class="state-pill state-waking" aria-hidden="true">waking…</span>
    </span>`;
  }
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
    <${MorphPendingBadge} pending=${state?.pending_morph} />
    ${online && state?.status_warning ? html`<span class="state-pill state-awaiting" title=${state.status_warning} aria-label=${state.status_warning}>⚠ status missing</span>` : null}
  </span>`;
}

// MorphPendingBadge marks a requested morph that waits for the agent's next
// idle turn (or lapses at its expiry), so the row shows what the morph
// dialog's toast promises.
export function MorphPendingBadge({ pending }) {
  if (!pending) return null;
  const target = pending.target || {};
  const to = pending.back
    ? 'its previous launch settings'
    : target.profile || [target.harness, target.model, target.effort].filter(Boolean).join(' · ') || 'new launch settings';
  const until = pending.expires_at ? new Date(pending.expires_at).toLocaleTimeString() : '';
  const title = `Morph pending to ${to}: it applies when the agent next goes idle${until ? `, or lapses at ${until}` : ''}`;
  return html`<span class="morph-pending-pill" data-morph-pending="1" title=${title} aria-label=${title}>⇢ morph</span>`;
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
  const configuredMax = Number(state?.context_window_max || 0);
  const known = pct > 0 || configuredMax > 0 || Number(state?.context_window_size || 0) > 0;
  const filled = pct > 0 ? Math.min(5, Math.max(1, Math.round(pct / 20))) : 0;
  const total = Number(state?.tokens_input || 0) + Number(state?.tokens_output || 0);
  // The window here is already the effective one and context_pct is measured
  // against it, so the regular usage figures are sufficient on their own.
  const win = configuredMax > 0 ? configuredMax : Number(state?.context_window_size || 0);
  // 'reported' is a limit the local harness disclosed; 'catalog' came from
  // Copilot's authenticated remote model catalog; 'assumed' is tclaude's
  // static per-model fallback.
  const source = state?.context_window_source === 'configured' ? ' (configured cap)'
    : state?.context_window_source === 'reported' ? ' (reported cap)'
      : state?.context_window_source === 'catalog' ? ' (catalog cap)'
        : state?.context_window_source === 'assumed' ? ' (assumed cap)' : '';
  const regularTitle = !known ? 'context window: usage not reported yet'
    : win > 0 && total > 0 ? `context: ${fmtTokens(total)} / ${fmtTokens(win)} tokens${source} — ${Math.round(pct)}%`
      : `context: ${Math.round(pct)}% full`;
  const wizardTitle = !known ? '🔮 Mana reserves: not yet divined'
    : win > 0 && total > 0 ? `🔮 Mana: ${fmtTokens(total)} / ${fmtTokens(win)} channeled${source} — ${Math.round(pct)}%`
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
  const monitors = Number(state?.monitor_count || 0);
  if (subagents <= 0 && shells <= 0 && monitors <= 0) return null;
  const subagentTitle = `${subagents} sub-agent${subagents === 1 ? '' : 's'} still running under this agent`;
  // Background shells are the other reason an "idle" agent isn't done:
  // a `Bash` launched with run_in_background outlives the turn, and the
  // count is reconciled against the agent's live descendant processes.
  const shellTitle = `${shells} background shell command${shells === 1 ? '' : 's'} still running under this agent`;
  // Monitors are the third: a `Monitor` watch (tailing a log, polling a
  // CI job, holding a websocket) keeps feeding the agent events after its
  // turn ends, so an agent with only monitors is waiting, not finished.
  const monitorTitle = `${monitors} monitor${monitors === 1 ? '' : 's'} still watching for this agent`;
  return html`<span class="activity-badges">${subagents > 0 ? html`<span class="activity-badge badge-subagents" title=${subagentTitle} aria-label=${subagentTitle}>🤖+${subagents}</span>` : null}${shells > 0 ? html`<span class="activity-badge badge-bg-shells" title=${shellTitle} aria-label=${shellTitle}>⚙+${shells}</span>` : null}${monitors > 0 ? html`<span class="activity-badge badge-monitors" title=${monitorTitle} aria-label=${monitorTitle}>👁+${monitors}</span>` : null}</span>`;
}

function StateCell({ member }) {
  const state = member.state || {};
  return html`<td class="state-cell"><${ContextMeter} state=${state} /><${StatePill}
    state=${state} online=${member.online} conv=${member.conv_id} waking=${memberWaking(member)} />${member.online
    ? html`<${ActivityBadges} state=${state} />` : null}<${AgentBudgetBadge} agentID=${member.agent_id} /></td>`;
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
  // A conv-keyed control names its agent too, which another node's route
  // takes (the retire icon does the same).
  else if (selector === 'conv' && member.agent_id) attrs['data-stable-agent'] = member.agent_id;
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
    title = wizardCopy(`notifications muted for ${label} — click to force ON (overrides a group mute)`, `omens silenced for hero ${label} — click to restore them (overrides a party silence)`);
  } else if (mode === 'on') {
    regular = `${glyph} notify: on`; wizard = `${glyph} omens: on`;
    title = wizardCopy(`notifications forced ON for ${label} (overrides a group mute) — click to inherit from group`, `omens forced ON for hero ${label} (overrides a party silence) — click to inherit from the party`);
  } else {
    regular = `${glyph} notify: inherit (${effective ? 'on' : 'off'})`; wizard = `${glyph} omens: inherit (${effective ? 'on' : 'silent'})`;
    title = wizardCopy(`notifications inherit (currently ${effective ? 'on' : 'off — a group is muted'}) for ${label} — click to mute`, `omens inherit from the party (currently ${effective ? 'on' : 'silent'}) for hero ${label} — click to silence`);
  }
  return html`<${MenuButton} member=${member} act="toggle-agent-notify" attrs=${{ 'data-mode': mode }} regular=${regular} wizard=${wizard} title=${title} />`;
}

function RemoteMenuItem({ member, canRemote }) {
  if (!canRemote) return null;
  const label = member.title || member.conv_id;
  const on = !!member.state?.remote_control;
  const glyph = on ? '📱' : '📴';
  const title = on
    ? wizardCopy(`Remote Access is ON for ${label} — reachable from the Claude app/phone. Click to turn it OFF.`, `Remote scrying is ON for hero ${label} — reachable from the Claude app/phone. Click to close it.`)
    : wizardCopy(`Remote Access is OFF for ${label}. Click to turn it ON — expose this agent to the Claude app/phone.`, `Remote scrying is OFF for hero ${label}. Click to open it to the Claude app/phone.`);
  return html`<${MenuButton} member=${member} act="toggle-remote-control" attrs=${{ 'data-intent': on ? 'off' : 'on' }} regular=${`${glyph} remote: ${on ? 'on' : 'off'}`} wizard=${`${glyph} remote scrying: ${on ? 'on' : 'off'}`} title=${title} />`;
}

function CompactMenuItem({ member, snapshot }) {
  if (!harnessCanCompact(snapshot, member.state?.harness)) return null;
  const label = member.title || member.conv_id;
  return html`<${MenuButton} member=${member} act="compact-agent" regular="compact" wizard="distill memory" disabled=${!member.online}
    title=${member.online
    ? wizardCopy(`Compact ${label}'s context window now`, `Distill ${label}'s memory now`)
    : `Compact is unavailable while ${label} is offline`} />`;
}

function FastModeMenuItem({ member }) {
  if (member.state?.harness !== 'codex') return null;
  const label = member.title || member.conv_id;
  const known = typeof member.state?.fast_mode === 'boolean';
  const on = member.state?.fast_mode === true;
  const intent = on ? 'off' : 'on';
  const regular = known ? `⚡ ${on ? 'disable' : 'enable'} fast mode` : '⚡ enable fast mode';
  const title = !member.online
    ? `${regular} is unavailable while ${label} is offline`
    : !known
      ? `Enable Fast mode for ${label}; the current tier is unknown, so tclaude will infer the inherited setting and sync from Codex's next readback.`
      : `${on ? 'Disable' : 'Enable'} Fast mode for ${label}; the agent stays running and the change applies to subsequent turns.`;
  return html`<${MenuButton} member=${member} act="fast-mode-set"
    attrs=${{ 'data-intent': intent }} regular=${regular} wizard=${regular}
    title=${title} disabled=${!member.online} />`;
}

function RestartMenuItem({ member }) {
  const label = member.title || member.conv_id;
  const regular = '↻ restart';
  const title = !member.online
    ? `${regular} is unavailable while ${label} is offline`
    : `Stop and restart ${label} under its current launch configuration. Re-resolves sandbox-profile rules. Requires the agent to be fully idle with no background agents or shell commands.`;
  return html`<${MenuButton}
    member=${member} act="restart" regular=${regular} wizard="↻ reincant hero"
    title=${title} disabled=${!member.online}
  />`;
}

function SandboxRestartMenuItem({ member }) {
  const unlocked = !!member.state?.temporary_sandbox_mode;
  const unlockUnsupported = codexOwnSandboxUnlockUnsupported(member, unlocked);
  const label = member.title || member.conv_id;
  const regular = unlocked ? '🔒 restore sandbox + restart' : '⚠ restart without sandbox';
  const wizard = unlocked ? '🔒 restore ward + reincant' : '⚠ reincant without ward';
  const title = unlockUnsupported
    ? CODEX_OWN_SANDBOX_UNLOCK_UNAVAILABLE
    : !member.online
    ? `${regular} is unavailable while ${label} is offline`
    : unlocked
      ? `Stop and restart ${label} with its preserved normal sandbox configuration. Requires the agent to be fully idle with no background agents or shell commands.`
      : `Stop and restart ${label} with its sandbox OFF and full machine access. Its normal sandbox configuration is preserved for restoration. Requires the agent to be fully idle with no background agents or shell commands.`;
  return html`<${MenuButton}
    member=${member} act="sandbox-restart" className=${unlocked ? undefined : 'danger'}
    attrs=${{ 'data-action': unlocked ? 'restore' : 'unlock' }}
    regular=${regular} wizard=${wizard} title=${title} disabled=${!member.online || unlockUnsupported}
  />`;
}

// SandboxImplMenuItem opens the picker that assigns the sandbox IMPLEMENTATION
// this agent will relaunch under. The implementation is applied by a launch, so
// for a running agent the dialog offers to restart it (stop, assign, resume)
// rather than making the operator stop it first; a stopped agent is assigned and
// left stopped.
function SandboxImplMenuItem({ member }) {
  const label = member.title || member.conv_id;
  const regular = '🧩 sandbox implementation…';
  const title = member.online
    ? `Choose which layer owns OS-level confinement for ${label}. It is running, so assigning restarts it (it must be idle) under the new implementation.`
    : `Choose which layer owns OS-level confinement for ${label}'s next launch — for example a per-agent cgroup (resource-only) for an agent created before that existed. Recorded now, applied when you wake it.`;
  // data-harness is a fallback only. The dialog renders its catalog from the
  // harness the SERVER reports with the posture; this covers the case where that
  // field is absent, and is never consulted otherwise.
  return html`<${MenuButton}
    member=${member} act="sandbox-impl" regular=${regular} wizard="🧩 ward implementation…"
    attrs=${{ 'data-harness': member.state?.harness || '' }}
    title=${title}
  />`;
}

// SendElsewhereItems is the ⚙ menu's "Send elsewhere" section (tcl-o9d5d9):
// each item opens the launch-target dialog on its mode, alongside drag and
// drop. Modes whose routes this dashboard lacks stay out of the menu. Another
// node's row (on its own page, or in the fused view) offers the move here,
// and the morph when that node trusts this one unrestricted.
// sendElsewhereOffReason says why move and teleport are greyed out when
// there is no fleet: this node is not on a hub, or no peer is linked yet.
export function sendElsewhereOffReason(node) {
  const hubOff = hubOffReason(node);
  return hubOff
    ? `Connect this node to a tclaude hub (Fleet → Hub) to move or teleport agents. ${hubOff}`
    : 'No linked nodes to move or teleport agents to yet: trust a peer in ⚙ Fleet config.';
}

function SendElsewhereItems({ member, group, snapshot }) {
  const remote = snapshot?.peer_view ? globalThis.__tclaudeRemoteNode : null;
  if ((snapshot?.peer_view && !remote?.id) || !member.agent_id) return null;
  const fleet = fleetState.fleet.value;
  // Another node's row (in a fused view, or on its own page) can only be
  // pulled here by a move; without a fleet there is nowhere to move or clone
  // to but here.
  const pageNode = remote?.id ? { id: remote.id, name: globalThis.document?.documentElement?.dataset?.remoteNodeName || '' } : null;
  const peerRow = !!pageNode || (!!group?.fleet_node && !group.fleet_node.local);
  // A fused row is its group's node's, on a peer's page too.
  const rowNode = group?.fleet_node || pageNode;
  // A node trusting this one unrestricted morphs its agent as its own
  // dashboard does.
  const acting = peerRow && unrestrictedOn(rowNode.id);
  const view = presenceView(member, fleet);
  const kind = view?.kind || '';
  const label = member.title || member.conv_id;
  // A peer row names its node, for a dialog opened outside the fused Groups
  // view (a Terminals tab's ⚙), which has no group row to read it from.
  const attrs = (mode) => ({
    'data-mode': mode, 'data-presence': kind, 'data-home-node': view?.home?.id || '', 'data-home-name': view?.home?.name || '',
    'data-cwd': member.state?.cwd || member.cwd || '',
    'data-harness': member.state?.harness || '',
    ...(peerRow ? { 'data-node': rowNode.id, 'data-node-name': rowNode.name || '' } : {}),
  });
  const item = (mode, regular, wizard, title, disabled = false) => html`<${MenuButton} member=${member} group=${group} act="send-elsewhere" attrs=${attrs(mode)} regular=${regular} wizard=${wizard} title=${title} disabled=${disabled} />`;
  // Without a fleet (no hub connection, or no linked node yet) move and
  // teleport stay in the menu, greyed out with the reason, as the node chips do.
  const off = fleet ? '' : sendElsewhereOffReason(fleetState.node.value);
  return html`<div class="menu-section-label" role="presentation">send elsewhere</div>
    ${item('move', peerRow ? 'move here…' : 'move to…', 'relocate hero…', off || (peerRow ? `Move ${label} to a group on this node, with its history` : `Move ${label} to a group on another node, with its history; it retires here once its copy runs there`), !!off)}
    ${TELEPORT_ROUTES && !peerRow ? item('teleport', 'teleport to…', 'teleport hero…', off || `Teleport ${label} to another node; its home keeps its place for Recall home`, !!off) : null}
    ${!peerRow ? item('clone', fleet ? 'clone to…' : 'clone…', 'mirror hero to…', `Clone ${label} here or onto another node; the original keeps running`) : null}
    ${!peerRow || acting ? html`<${MenuButton} member=${member} group=${group} act="snapshot-agent" attrs=${peerRow ? attrs('') : {}} regular="📸 snapshot…" wizard="📸 capture hero…" title=${`Keep a named snapshot of ${label} on ${peerRow ? `${rowNode.name || 'its node'}` : 'this node'}; spawn from it, share or publish it later`} />` : null}
    ${!peerRow || acting ? html`<${MenuButton} member=${member} group=${group} act="export-paf" attrs=${peerRow ? attrs('') : {}} regular="⤓ export (PAF)…" wizard="⤓ seal hero (PAF)…" title=${`Export ${label} as a portable PAF file; importing it anywhere creates a new agent`} />` : null}
    ${!peerRow || acting ? item('morph', 'morph…', 'shapeshift hero…', `Relaunch ${label} in place with another spawn profile (model, effort, harness); it keeps its identity`) : null}
    ${TELEPORT_ROUTES && kind === 'visiting' && !pageNode && (!peerRow || view.home.local) ? item('home', '⌂ recall home…', '⌂ recall hero home…', `Send ${label} straight home to ${view.home.name}`) : null}`;
}

// MakeStewardMenuItem hands this agent the Steward role, with a confirmation.
// It is this node's operator's: not on another node's row, and absent with Collab
// off like the rest of the Steward.
function MakeStewardMenuItem({ member, group, snapshot }) {
  const remote = snapshot?.peer_view || (group?.fleet_node && !group.fleet_node.local) || (member.fleet_node?.id && !isPageNode(member.fleet_node.id));
  if (snapshot?.collab_enabled !== true || remote || !member.agent_id) return null;
  if (member.state?.harness === 'shell' || member.agent_id === snapshot.steward_active) return null;
  const label = member.title || member.conv_id;
  return html`<${MenuButton} member=${member} act="make-steward" regular="✦ make Steward…" wizard="✦ crown Steward…"
    title=${`Make ${label} the Steward. It keeps its conversation and working directory, and you choose what happens to the current one.`} />`;
}

function MenuSeparator() {
  return html`<div class="menu-sep" role="separator"></div>`;
}

function MemberMenu({ member, group, snapshot, actions, ungrouped }) {
  const interactions = useGroupsInteractions();
  const label = member.title || member.conv_id;
  const canRemote = harnessCanRemoteControl(snapshot, member.state?.harness);
  const prefill = JSON.stringify({ targetMode: 'solo', target: member.agent_id || member.conv_id, owner: member.agent_id || member.conv_id });
  // No pane, no launch and no sudo: these could only fail for an external steward.
  const runsHere = !member.external_steward;
  return html`
    <${MenuButton} member=${member} act="view-agent-messages" regular="view messages" wizard="view missives" title=${wizardCopy("Open this agent's messages in the Messages tab", "Open this hero's missives in the Messages tab")} />
    ${runsHere && html`<${MenuButton} member=${member} act="terminal-chooser" attrs=${{ 'data-cwd': member.state?.cwd || member.cwd || '' }} regular="terminals & windows…" wizard="🔮 scrying portals…" title=${wizardCopy("Open a terminal: attach to this agent's live session, or a separate shell in its working directory — as a native window or in the browser (term / web term / open window / web window)", "Open a scrying portal: gaze into this hero's live session, or open a shell of your own in its lair — as a mortal window or a browser crystal (term / web term / open window / web window)")} />`}
    <${MenuButton} member=${member} act="export-summary" regular="summary…" wizard="inscribe scroll…" disabled=${!member.online} title=${member.online ? wizardCopy('Ask this agent to produce a shareable export of the conversation (a summary / report) and download it here. Multiple files are zipped automatically.', 'Ask this hero to inscribe a shareable account of its conversation and bring it here. Multiple scrolls are bundled automatically.') : wizardCopy('Export needs a running agent — it produces the file in its own session. Unavailable while the agent is offline.', 'The hero must be channeling to inscribe an export. Unavailable while it slumbers.')} />
    <${MenuButton} member=${member} act="debug-export" regular="export debug info" wizard="inscribe ward diagnostics" title=${wizardCopy('Download the requested, resolved, and running launch configurations, including exact sandbox implementation and composed profiles. Task text and authorization tokens are redacted.', 'Download this hero\'s requested, resolved, and running summoning configurations, including exact ward implementation and composed profiles. Quest text and authorization runes are redacted.')} />
    <${MenuSeparator} />
    ${!ungrouped ? html`<${MenuButton} member=${member} group=${group} act="edit-member" attrs=${{ onClick: (event) => {
      event.preventDefault();
      event.stopPropagation();
      interactions.closeMenu(true);
      actions.openMemberEditor(member, group, 'title');
    } }} regular="edit" wizard="enchant" title=${wizardCopy('Edit this agent — title, role, description, ownership, permissions', 'Enchant this hero — title, class, description, party ownership, and grimoire')} />
      <${MenuButton} member=${member} group=${group} act=${member.owner ? 'revoke-owner' : 'grant-owner'} className=${member.owner ? 'warn' : undefined} regular=${member.owner ? 'revoke owner' : 'make owner'} wizard=${member.owner ? 'revoke party owner' : 'make party owner'} title=${wizardCopy(member.owner ? 'Revoke group owner status' : 'Make this agent an owner of the group', member.owner ? 'Revoke party owner status' : 'Make this hero an owner of the party')} />` : null}
    <${MenuButton} member=${member} selector="conv" act="perm-edit" regular="permissions" wizard="grimoire" title=${wizardCopy("Edit this agent's permanent permissions (grant / deny / inherit-default)", "Open this hero's grimoire of permanent boons and bindings")} />
    ${runsHere && html`<${MenuButton} member=${member} act="sudo-grant" regular="+ sudo" wizard="+ sudo" title=${wizardCopy('Grant a time-bounded sudo elevation to this agent', 'Grant this hero a time-bounded sudo boon')} />`}
    ${runsHere && html`<${MakeStewardMenuItem} member=${member} group=${group} snapshot=${snapshot} />`}
    <${NotifyMenuItem} member=${member} />
    <${RemoteMenuItem} member=${member} canRemote=${canRemote} />
    <${FastModeMenuItem} member=${member} />
    <${MenuButton} member=${member} group=${group} selector="label" act="cron-new" attrs=${{ 'data-prefill': prefill }} regular="⏰ schedule…" wizard="⏳ bind ritual…" title=${wizardCopy(`Schedule a recurring nudge for ${label}`, `Bind a recurring ritual for hero ${label}`)} />
    ${member.agent_id && !snapshot?.peer_view && !(group?.fleet_node && !group.fleet_node.local) && !(member.fleet_node?.id && !isPageNode(member.fleet_node.id)) ? html`<${MenuButton} member=${member} group=${group} act="usage-budget" attrs=${{ onClick: (event) => {
      event.preventDefault();
      event.stopPropagation();
      interactions.closeMenu(true);
      openUsageRulesManager({ tab: 'agents', agent: member.agent_id });
    } }} regular="💰 usage budget…" wizard="💰 tithe…" title=${wizardCopy(`See ${label}'s usage budgets; set its own usage profiles, extend a budget, snooze or exempt it`, `Weigh ${label}'s tithes; set its own, extend, snooze or exempt it`)} />` : null}
    <${MenuSeparator} />
    ${snapshot?.peer_view || (group?.fleet_node && !group.fleet_node.local) ? html`<${MenuButton} member=${member} act="clone" regular="clone" wizard="mirror hero" title=${wizardCopy("Ask the peer to fork a sibling of this agent that inherits its groups and permissions. The original keeps running.", "Ask the peer to mirror this hero into a sibling that inherits its parties and boons. The original keeps channeling.")} />` : null}
    <${CompactMenuItem} member=${member} snapshot=${snapshot} />
    ${runsHere && html`<${MenuButton} member=${member} act="reincarnate" regular="reincarnate" wizard="reincarnate hero" title=${wizardCopy('Reincarnate this agent — by default ask it to do so itself (it writes its own handoff); or force an immediate daemon-driven reincarnation.', 'Reincarnate this hero — by default ask it to write its own handoff; or force its immediate return in a fresh vessel.')} />`}
    ${runsHere && html`<${RestartMenuItem} member=${member} />`}
    ${runsHere && html`<${SubscriptionMenuItems} member=${member} />`}
    ${runsHere && html`<${SandboxRestartMenuItem} member=${member} />`}
    ${runsHere && html`<${SandboxImplMenuItem} member=${member} />`}
    <${MenuSeparator} />
    <${SendElsewhereItems} member=${member} group=${ungrouped ? null : group} snapshot=${snapshot} />
    <${MenuSeparator} />
    ${ungrouped
      ? html`<${MenuButton} member=${member} selector="conv" act="retire-agent" className="warn" regular="retire" wizard="banish" title=${wizardCopy('Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored).', 'Banish this hero — return it to a plain conversation, revoking its party memberships and boons. Reversible via reinstate.')} /><${MenuButton} member=${member} act="delete-agent" className="danger" regular="delete" wizard="erase hero" title=${wizardCopy('Permanently delete this agent and conversation', 'Permanently erase this hero and its conversation scroll')} />`
      : html`<${MenuButton} member=${member} group=${group} act="remove-member" className="danger" regular="remove" wizard="dismiss from party" title=${wizardCopy('Remove this agent from the group', 'Remove this hero from the party')} /><${MenuButton} member=${member} selector="conv" act="retire-agent" className="warn" regular="retire" wizard="banish" title=${wizardCopy('Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored).', 'Banish this hero — return it to a plain conversation, revoking its party memberships and boons. Reversible via reinstate.')} />`}
  `;
}

function MemberActions({ member, group, snapshot, actions, ungrouped, menuKey }) {
  const offlineWhy = member.online ? '' : ' — unavailable while the agent is offline';
  return html`<div class="row-actions">
    <button class="icon-btn" data-act="jump" ...${memberAttrs(member)} disabled=${!member.online} title=${`Focus this agent's terminal window; when using web terminals, Ctrl/Cmd-click keeps this tab open${offlineWhy}`} aria-label="Focus window"><${EyeIcon} /></button>
    <button class="icon-btn" data-act="hide" ...${memberAttrs(member)} disabled=${!member.online} title=${`Hide this agent's terminal window — detaches its tmux client. The agent keeps running.${offlineWhy}`} aria-label="Hide window"><${EyeIcon} hidden=${true} /></button>
    <button class="icon-btn warn" data-act="retire-agent" data-conv=${member.conv_id} data-stable-agent=${member.agent_id || ''} data-label=${member.title || member.conv_id} title="Retire this agent — demote it back to a plain conversation, revoking its group memberships and permission grants. Reversible via reinstate (stripped grants are not restored)." aria-label="Retire agent"><${TrashIcon} /></button>
    <${ActionMenu} menuKey=${menuKey} kind="row-menu"><${MemberMenu} member=${member} group=${group} snapshot=${snapshot} actions=${actions} ungrouped=${ungrouped} /><//>
  </div>`;
}

// AgentGearMenu is the agent's ⚙ menu outside a Groups row: on a Terminals
// tab (tcl-xy5wkp). An agent on this node gets the row menu at agent level
// (no group, as for an ungrouped agent). One on another node, which this
// node's roster does not carry, gets what a fused Groups view offers its
// rows: moving it here. It needs a GroupsInteractionProvider above it.
export function AgentGearMenu({ member, node = null, snapshot, menuKey }) {
  if (node && !node.local) {
    return html`<${ActionMenu} menuKey=${menuKey} kind="row-menu"><${SendElsewhereItems} member=${member} group=${{ fleet_node: node }} snapshot=${snapshot} /><//>`;
  }
  return html`<${ActionMenu} menuKey=${menuKey} kind="row-menu"><${MemberMenu} member=${member} group=${null} snapshot=${snapshot} actions=${{}} ungrouped=${true} /><//>`;
}

function SudoBadge({ grants, conv }) {
  if (!grants?.length) return null;
  const fmt = (seconds) => seconds <= 0 ? 'expired' : seconds < 60 ? `${seconds}s` : seconds < 3600 ? `${Math.floor(seconds / 60)}m${seconds % 60 ? `${seconds % 60}s` : ''}` : `${Math.floor(seconds / 3600)}h${Math.floor((seconds % 3600) / 60) ? `${Math.floor((seconds % 3600) / 60)}m` : ''}`;
  const title = `${grants.length} active sudo grant${grants.length === 1 ? '' : 's'} — click to manage:\n${grants.map((grant) => `${grant.slug} (expires in ${fmt(grant.remaining_seconds)})`).join('\n')}`;
  return html`<span class="sudo-badge" data-act="sudo-manage" data-conv=${grants[0].conv_id || conv || ''} title=${title}>🔓</span>`;
}

// openMemberNotification raises the quick reader on one of member's
// notifications — the row glyph and the group header's hint share it.
export function openMemberNotification(member, message, launcher, returnFocus = null) {
  return openHumanNotificationReader({
    sender: {
      agent: member.agent_id || '',
      conv: member.conv_id || '',
      label: member.title || member.conv_id,
      // Another node's agent (the fused view): its notification lives there.
      ...(member.fleet_node?.id && !isPageNode(member.fleet_node.id)
        ? { node: member.fleet_node.id, nodeName: member.fleet_node.name || '' } : {}),
    },
    messageID: message?.id || null,
    launcher,
    returnFocus: returnFocus || null,
  });
}

function HumanNotificationAttention({ member, snapshot }) {
  const unread = memberUnreadHumanCount(member, snapshot);
  if (!unread) return null;
  const label = member.title || member.conv_id;
  const message = memberHumanMessages(member, snapshot, true)[0];
  const title = `${unread} unread notification${unread === 1 ? '' : 's'} from ${label} — open quick reader`;
  const attachment = message?.attachment;
  const previewID = message ? `human-notification-preview-${message.id}` : undefined;
  const openReader = (event) => {
    event.preventDefault();
    event.stopPropagation();
    openMemberNotification(member, message, event.currentTarget,
      event.currentTarget.closest('.rowname')?.querySelector('.rowname-text'));
  };
  return html`<button type="button" class="human-notification-attention"
    ...${memberAttrs(member)}
    data-sender=${humanNotificationSenderQuery(member)}
    aria-label=${title} aria-describedby=${previewID}
    onClick=${openReader}><span class="human-notification-attention-glyph" aria-hidden="true">!</span>
    ${message && html`<span class="human-notification-preview" id=${previewID} role="tooltip">
      <span class="human-notification-preview-head">
        <strong>${message.subject || '(no subject)'}</strong>
        <span>${relTime(message.created_at)}</span>
      </span>
      <span class="human-notification-preview-body">${message.body
        || bodilessNotice(message) || '(empty notification)'}</span>
      <span class="human-notification-preview-foot">
        ${attachment
          ? html`<span class="human-notification-preview-attachment">📎 ${attachment.filename || 'attachment'}</span>`
          : html`<span>No attachment</span>`}
        <span>${unread > 1 ? `${unread} unread · ` : ''}preview only · click to open</span>
      </span>
    </span>`}
  </button>`;
}

function MemberName({ member, snapshot, actions, grants, editorKey }) {
  const state = member.state || {};
  const canRename = harnessCanRename(snapshot, state.harness);
  const idPrefix = memberColHidden('id') ? `${idTooltip(member.agent_id, member.conv_id)} — ` : '';
  if (!canRename) return html`<div class="rowname"><${HumanNotificationAttention} member=${member} snapshot=${snapshot} /><span class="rowname-text rowname-fixed" title=${`${idPrefix}This agent's harness does not support renaming`}>${member.title || '(unnamed)'}</span><${PresenceBadge} member=${member} /><${SubscriptionBadge} member=${member} /><${SudoBadge} grants=${grants} conv=${member.conv_id} /></div>`;
  return html`<div class="rowname"><${HumanNotificationAttention} member=${member} snapshot=${snapshot} /><${InlineEditor}
    editorKey=${editorKey} value=${member.title || ''} className="rowname-input"
    placeholder="1-64 chars: A-Za-z0-9 _ - [ ] { } ( ) — Enter saves, Esc cancels"
    onCommit=${(value) => actions.renameAgent(member, value)}
    hint=${(draft) => agentTitleHint(draft, snapshot?.spawn_name_normalize !== false)}
    triggerProps=${{
      class: 'rowname-text', role: 'button', tabindex: '0', 'data-act': 'rename-name',
      ...memberAttrs(member), 'data-current': member.title || '', 'data-editor-key': editorKey,
      title: `${idPrefix}Click to rename this agent — Enter saves, Esc cancels`,
    }}
  >${member.title || '(unnamed)'}<//><${PresenceBadge} member=${member} /><${SubscriptionBadge} member=${member} /><${SudoBadge} grants=${grants} conv=${member.conv_id} /></div>`;
}

// SubscriptionMenuItems: the owner of a lost subscription token sends it
// again; anyone may switch an agent on a subscription to the node's own login.
function SubscriptionMenuItems({ member }) {
  const sub = member.claude_subscription;
  if (!sub?.peer) return null;
  const mine = sub.peer === fleetState.fleet.value?.self?.id;
  return html`${!sub.held && mine ? html`<${MenuButton} member=${member} act="claude-subscription" attrs=${{ 'data-online': member.online ? '1' : '0' }} regular="◆ send my subscription again…" title="This node forgot your Claude subscription token (its tclaude restarted). Send it again in an encrypted request, so the agent can start." />` : null}
    <${MenuButton} member=${member} act="claude-login-local" regular="◆ use the node's own login…" title="Stop running this agent on the Claude subscription it was spawned on: from its next start it uses the Claude login of the node it runs on." />`;
}

// SubscriptionBadge marks an agent that runs Claude Code on a peer operator's
// own Claude subscription, handed over in memory only: once the node no
// longer holds the token (its tclaude restarted), a relaunch refuses.
function SubscriptionBadge({ member }) {
  const sub = member.claude_subscription;
  if (!sub?.peer) return null;
  const who = nodeRef(sub.peer, fleetState.fleet.value);
  const owner = who.local ? 'your' : `${who.name || sub.label || 'a peer'}'s`;
  const title = sub.held
    ? `Runs on ${owner} own Claude subscription. The token is held in memory on the node running it, never on its disk or in the agent's environment; it ends with the agent.`
    : `Runs on ${owner} own Claude subscription, but the node running it no longer holds the token (its tclaude restarted), so it cannot start until ${who.local ? 'you send it' : 'its owner sends it'} again or it switches to the node's own login.`;
  const text = `◆ ${owner} subscription${sub.held ? '' : ' (token gone)'}`;
  if (sub.held) return html`<span class="presence-badge subscription-badge" style=${who.color ? `--nc:${who.color}` : undefined} title=${title} data-subscription="held">${text}</span>`;
  // A lost token is actionable: its owner sends it again, anyone else may
  // switch the agent to the node's own login (subscription-relaunch.js).
  return html`<button type="button" class="presence-badge subscription-badge lost" style=${who.color ? `--nc:${who.color}` : undefined} title=${`${title} Click to ${who.local ? 'send it again' : 'switch it to the node\'s own login'}.`}
    data-subscription="lost" data-act="claude-subscription" ...${menuMemberAttrs(member, 'agent')} data-online=${member.online ? '1' : '0'}>${text}</button>`;
}

// PresenceBadge marks a visiting agent with its home, and a returned one with
// a short-lived "back home" tag, both in the home node's colour (tcl-hocp2h).
function PresenceBadge({ member }) {
  const view = presenceView(member, fleetState.fleet.value);
  if (!view || view.kind === 'away') return null;
  return html`<span class=${`presence-badge presence-${view.kind}`} style=${view.home.color ? `--nc:${view.home.color}` : undefined} title=${view.title} data-presence=${view.kind}>⌂ ${view.label}</span>`;
}

// showVisitingRow scrolls to the agent's live row elsewhere in a fused view
// and flashes it; without one it opens the node where the agent runs.
function showVisitingRow(member, at) {
  const root = document.getElementById('fleetview-fused-groups-root') || document;
  const rows = [...root.querySelectorAll('tr[data-dnd-agent]')]
    .filter((row) => row.dataset.dndAgent === member.agent_id && !row.classList.contains('away-placeholder') && row.getClientRects().length);
  if (rows.length) {
    rows[0].scrollIntoView({ block: 'center', behavior: 'smooth' });
    rows[0].classList.remove('presence-flash');
    void rows[0].offsetWidth;
    rows[0].classList.add('presence-flash');
    return;
  }
  if (at.id) void import('./fleet-island.js').then((m) => m.openNodeView(at.local ? '' : at.id)).catch(() => {});
}

// AwayRow is the dimmed placeholder an away agent leaves in its home groups:
// no status, terminal or menu, just where it is now and a way to get there.
function AwayRow({ member, view, columns, group }) {
  // Only a fused view can hold the visiting row; one node's view links out.
  const visiting = !!group?.fleet_node && !!member.agent_id;
  const cell = (column) => {
    switch (column.key) {
      case 'ctl': return html`<td><span class="away-dot" aria-hidden="true">○</span></td>`;
      case 'id': return html`<td class="id" title=${idTooltip(member.agent_id, member.conv_id)}>${shortAgentId(member.agent_id, member.conv_id)}</td>`;
      case 'title': return html`<td class="name-cell"><div class="rowname"><span class="rowname-text rowname-fixed">${member.title || '(unnamed)'}</span>
        <span class="presence-badge presence-away" style=${view.at.color ? `--nc:${view.at.color}` : undefined} title=${view.title} data-presence="away">${view.label}</span>
        <button type="button" class="fa-link away-goto" data-away-goto=${member.agent_id || ''}
          title=${visiting ? `Scroll to ${member.title || 'it'} on ${view.at.name}` : `Open ${view.at.name}'s dashboard`}
          onClick=${(event) => { event.preventDefault(); event.stopPropagation(); showVisitingRow(member, view.at); }}
        >${visiting ? '↓ show' : view.at.local ? 'open on this node ↗' : `open on ${view.at.name} ↗`}</button>
        ${TELEPORT_ROUTES ? html`<button type="button" class="away-recall" data-act="send-elsewhere" data-mode="home" data-presence="away"
          data-agent=${member.agent_id} data-conv=${member.conv_id} data-label=${member.title || member.conv_id} data-group=${group?.name || ''}
          data-home-node=${view.home.id} data-home-name=${view.home.name}
          title=${`Recall ${member.title || 'it'} straight home from ${view.at.name}`}>⌂ recall home…</button>` : null}</div></td>`;
      default: return html`<td></td>`;
    }
  };
  return html`<tr class="away-placeholder" data-key=${member.conv_id} data-dnd-agent=${member.agent_id || member.conv_id}>${columns.map((column) => html`<${Fragment} key=${column.key}>${cell(column)}<//>`)}</tr>`;
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

function StackedLocation({ start, current, differ, wrap = false }) {
  if (!differ) return current || start;
  const row = wrap ? 'loc-row loc-wrap' : 'loc-row';
  return html`<div class="loc-pair"><span class=${row}><span class="loc-tag">init</span>${start}</span><span class=${row}><span class="loc-tag">now</span>${current}</span></div>`;
}

function CwdCell({ member, group }) {
  const startup = member.startup_dir || member.state?.cwd || '';
  const current = member.current_dir || '';
  const path = (value, which) => value
    ? html`<span class="cwd cwd-link" data-act="term-dir" ...${memberAttrs(member)} data-group=${group?.name} data-which=${which} title=${`Open a terminal here — ${value}`}>${shortCwd(value)}</span>`
    : html`<span class="cwd">—</span>`;
  return html`<${StackedLocation} start=${path(startup, 'start')} current=${path(current, 'worktree')} differ=${!!current && !!startup && current !== startup} />`;
}

function BranchCell({ member }) {
  const branch = (name, url, pr) => {
    if (!name) return html`<span class="muted">—</span>`;
    const branchNode = url
      ? html`<a class="branch branch-link" href=${url} target="_blank" rel="noopener noreferrer" draggable=${false} title=${`Open branch on GitHub — ${name}`}>⎇ ${name}</a>`
      : html`<span class="branch" title=${`git branch: ${name}`}>⎇ ${name}</span>`;
    // .pr-chain keeps the link and its badges one inline run: inside an
    // init/now .loc-row (a flex row with a gap) they would otherwise each
    // become a flex item and sit wider apart than on a single-branch row.
    return html`${branchNode}${pr.number && pr.url ? html` <${PRChain} pr=${pr} />` : null}`;
  };
  const start = branch(member.startup_branch || '', member.startup_branch_url || '', {
    number: member.startup_pr_number || 0, url: member.startup_pr_url || '', state: member.startup_pr_state || '',
    checks: member.startup_checks, reviews: member.startup_reviews, title: member.startup_pr_title || '',
  });
  const current = branch(member.branch || '', member.branch_url || '', {
    number: member.branch_pr_number || 0, url: member.branch_pr_url || '', state: member.branch_pr_state || '',
    checks: member.branch_checks, reviews: member.branch_reviews, title: member.branch_pr_title || '',
  });
  const seen = new Set([member.startup_pr_url, member.branch_pr_url].filter(Boolean));
  const presented = (member.presented_prs || []).filter((pr) => {
    const url = String(pr.url || '').trim();
    if (!url || seen.has(url) || !/^https?:\/\//i.test(url)) return false;
    seen.add(url); return true;
  });
  return html`<${StackedLocation} start=${start} current=${current} differ=${(member.startup_branch || '') !== (member.branch || '')} wrap />${presented.length ? html` <span class="presented-prs">${presented.map((pr) => html`<${PRChain} key=${pr.url} pr=${pr} className="presented-pr"
    describe=${pr.summary ? `${pr.summary} — ${pr.url}` : `Presented pull request — ${pr.url}`} />`)}</span>` : null}`;
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
    case 'ctl': return html`<td><div class="agent-ctl"><${AgentStatusDot} member=${member} /><${MemberActions} member=${member} group=${group} snapshot=${snapshot} actions=${actions} ungrouped=${ungrouped} menuKey=${menuKey} /></div><${HarnessLine} member=${member} snapshot=${snapshot} local=${!group?.fleet_node || !!group.fleet_node.local} /></td>`;
    case 'id': return html`<td class="id" title=${idTooltip(member.agent_id, member.conv_id)}>${shortAgentId(member.agent_id, member.conv_id)}</td>`;
    case 'title': return html`<td class="name-cell"><${MemberName} member=${member} snapshot=${snapshot} actions=${actions} grants=${grants} editorKey=${editorKey} /></td>`;
    case 'state': return html`<${StateCell} member=${member} />`;
    case 'last': return html`<td><span class="last-hook">${relTime(state.last_hook)}</span></td>`;
    case 'age': return html`<td><span class="last-hook" title=${member.created_at || ''}>${relTime(member.created_at)}</span></td>`;
    case 'cwd': return html`<td><${CwdCell} member=${member} group=${group} /></td>`;
    case 'branch': return html`<td><${BranchCell} member=${member} /></td>`;
    case 'role': return html`<td><${RoleCell} member=${member} group=${group} actions=${actions} /></td>`;
    case 'task': return html`<td class="task-cell"><${TaskCell} member=${member} /></td>`;
    case 'process': return html`<td class="process-cell"><${MemberProcessCell} member=${member} group=${group} snapshot=${snapshot} /></td>`;
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
  const presence = presenceView(member, fleetState.fleet.value);
  if (presence?.kind === 'away') return html`<${AwayRow} member=${member} view=${presence} columns=${columns} group=${group} />`;
  return html`<tr
    class=${`dnd-draggable${presence ? ` presence-row-${presence.kind}` : ''}`} draggable=${interactions.editorKey !== editorKey} data-key=${member.conv_id}
    data-dnd-source-ungrouped=${ungrouped ? '1' : undefined}
    data-dnd-source-group=${ungrouped ? undefined : group.name}
    data-dnd-conv=${member.conv_id} data-dnd-agent=${member.agent_id || member.conv_id}
    data-dnd-label=${member.title || member.conv_id}
  >${columns.map((column) => html`<${MemberCell} key=${column.key} column=${column} member=${member} group=${group} ungrouped=${ungrouped} snapshot=${snapshot} actions=${actions} grants=${grants} menuKey=${menuKey} editorKey=${editorKey} />`)}</tr>`;
}

// QueuedMemberRow is a queued spawn (queued-spawns.js) drawn as a member row:
// ▶ where the status light sits, "queued" for the state, and the draft's
// launch shape in the other columns. Not an agent yet, so there is no
// terminal to focus; the trash discards, the cog reopens the spawn dialog,
// and dragging it onto the bin discards too (dnd.js, data-dnd-queued).
function QueuedMemberRow({ entry, columns }) {
  const draft = entry.draft || {};
  const label = queuedSpawnLabel(entry);
  const act = (fn) => (event) => { event.preventDefault(); event.stopPropagation(); void fn(entry); };
  const edit = act(editQueuedSpawn);
  const muted = (value) => (value ? html`<span class="muted">${value}</span>` : html`<span class="muted">—</span>`);
  const harness = String(draft.harness || '');
  const labels = harnessLabels(harness);
  const roles = Array.isArray(draft.roleRefs) ? draft.roleRefs.filter(Boolean) : [];
  const brief = String(draft.initialMessage || '').trim().replace(/\s+/g, ' ');
  const descr = String(draft.descr || '').trim();
  const branch = String(draft.worktreeBranch || draft.worktree || '').trim();
  const cell = (column) => {
    switch (column.key) {
      case 'ctl': return html`<td><div class="agent-ctl">
        <button type="button" class="status-dot queued-play" data-act="queued-spawn-launch" data-group=${entry.group}
          title=${`queued — click to spawn ${label} now (opens the dialog and launches it; worktree, fetch and spawn checks run now)`}
          aria-label=${`Spawn ${label} now`} onClick=${act(launchQueuedSpawn)}>▶</button>
        <div class="row-actions">
          <button class="icon-btn" disabled title="Focus this agent's terminal window — unavailable until the queued agent is spawned" aria-label="Focus window"><${EyeIcon} /></button>
          <button class="icon-btn warn" data-act="queued-spawn-discard" data-group=${entry.group} title="Discard this queued agent — remove it from the queue without launching" aria-label=${`Discard queued ${label}`} onClick=${act(discardQueuedSpawn)}><${TrashIcon} /></button>
          <button type="button" class="cog-btn" data-act="queued-spawn-edit" data-group=${entry.group} title="Review or edit the queued spawn in the spawn dialog" aria-label=${`Edit queued ${label}`} onClick=${edit}><span class="cog-glyph">⚙︎</span></button>
        </div></div>
        ${draft.profile || harness ? html`<div class="agent-harness" title=${draft.profile ? `Queued with profile ${draft.profile}` : `Queued for ${labels.long}${draft.model ? ` · ${draft.model}` : ''}`}><span class="runtime-meta runtime-meta-offline">${draft.profile
          ? String(draft.profile)
          : html`<${HarnessMark} name=${harness} shortLabel=${labels.short} longLabel=${labels.long} tooltip=${labels.long} />${draft.model ? html`<span class="harness-sep">·</span><span class="harness-model">${displayModel(String(draft.model), harness)}</span>` : null}${draft.effort ? html`<span class="harness-effort" title=${draft.effort}>${shortEffort(draft.effort)}</span>` : null}`}</span></div>` : null}</td>`;
      case 'title': return html`<td class="name-cell"><div class="rowname"><span class="rowname-text queued-name" role="button" tabindex="0"
        title="Click to review or edit the queued spawn" onClick=${edit}
        onKeyDown=${(event) => { if (event.key === 'Enter' || event.key === ' ') edit(event); }}>${label}</span></div></td>`;
      case 'state': return html`<td class="state-cell"><span class="state-pill-wrap" title=${`queued${entry.created_at ? ` ${relTime(entry.created_at)}` : ''} — prepared but not started; ▶ spawns it`}><span class="state-pill state-queued">queued</span></span></td>`;
      case 'age': return html`<td><span class="last-hook" title=${entry.created_at || ''}>${entry.created_at ? relTime(entry.created_at) : ''}</span></td>`;
      case 'cwd': return html`<td>${draft.cwd ? html`<span class="cwd" title=${draft.cwd}>${shortCwd(draft.cwd)}</span>` : muted('')}</td>`;
      case 'branch': return html`<td>${branch ? html`<span class="branch muted" title=${`worktree branch at launch: ${branch}`}>⎇ ${branch}</span>` : muted('')}</td>`;
      case 'role': return html`<td>${muted(roles.join(', ') || String(draft.role || ''))}</td>`;
      case 'task': return html`<td class="task-cell">${muted(String(draft.task || ''))}</td>`;
      case 'descr': return html`<td class="descr-cell">${descr ? html`<span class="descr-text">${descr}</span>` : brief ? html`<span class="muted" title=${brief}>“${brief.length > 60 ? `${brief.slice(0, 59)}…` : brief}”</span>` : muted('')}</td>`;
      default: return html`<td></td>`;
    }
  };
  return html`<tr class="dnd-draggable queued-row" draggable=${true} data-key=${`queued:${entry.id}`}
    data-dnd-pending="1" data-dnd-queued=${entry.id} data-dnd-queued-group=${entry.group || ''}
    data-dnd-conv=${`queued:${entry.id}`} data-dnd-label=${label}
  >${columns.map((column) => html`<${Fragment} key=${column.key}>${cell(column)}<//>`)}</tr>`;
}

// MemberTable lists a group's members, its queued spawns (if any) heading the
// rows outside the sort — they are not agents yet, so no column orders them.
export function MemberTable({ members, group, tableKey, ungrouped = false, snapshot, actions, SortHead, lead = null, queued = [] }) {
  const columns = visibleMemberCols();
  const activeSort = tableSortState('members', columns);
  return html`<table><${SortHead} table="members" columns=${columns} lead=${lead} /><tbody>${queued.map((entry) => html`<${QueuedMemberRow} key=${`queued:${entry.id}`} entry=${entry} columns=${columns} />`)}${applySortState(members, MEMBER_ACCESSORS, activeSort).map((member) => html`<${MemberRow} key=${member.conv_id} member=${member} group=${group} tableKey=${tableKey} ungrouped=${ungrouped} snapshot=${snapshot} actions=${actions} columns=${columns} />`)}</tbody></table>`;
}
// dashboard-imperative-boundary: media-effects
