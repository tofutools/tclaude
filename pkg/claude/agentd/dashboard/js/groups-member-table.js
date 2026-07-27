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

export function HarnessLine({ member }) {
  const state = member.state || {};
  const offline = !member.online;
  const metadataClass = `runtime-meta${offline ? ' runtime-meta-offline' : ''}`;
  const harness = state.harness || '';
  const labels = harnessLabels(harness);
  const model = state.model || '';
  // Both indicators trail the metadata text as bare glyphs, tightly packed:
  // "CC · O4.8 1M high 🔒 📱". The sandbox one used to own a framed chip on its
  // own line below, which cost a whole row of height to say what a padlock says.
  const sandbox = html`<${SandboxBadge} member=${member} />`;
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

// osSandboxBadge describes an agent whose row carries a recorded launch-time
// OS-sandbox verdict (os_sandbox_state — today, Claude Code). The launch mode
// alone cannot describe those agents: `inherit`, the default and recommended
// mode, means "whatever settings.json says", so a mode-driven badge showed
// nothing for a confined agent and nothing for an unconfined one either, and
// the operator could not tell them apart.
//
// Returns null when there is nothing to say — an `inherit` launch that no
// settings file confines is the unremarkable, unchanged case.
//
// Every title opens with the RESOLVED posture ("Sandbox: on", "Sandbox: off",
// "Sandbox: on (unverified)"), because the badge no longer prints that word on
// screen: the tooltip is now the only place it can be read, and one that opened
// with the launch's REQUEST would tell an operator the opposite of the truth in
// exactly the case — a request that lost — where it matters most.
function osSandboxBadge(mode, state, source, prefix, unverified, implementation) {
  const via = source ? ` (${source})` : '';
  // On harness-builtin, a settings file outranking the one that decided could
  // not be read, so a policy tclaude never saw may say the opposite. The layer
  // verdicts reuse `unverified` for their explicit partial-fidelity copy; their
  // separate outer OS wall still earns the lock below.
  const partialDarwinTclaudeLayer = unverified && source.includes('Seatbelt/sandbox-exec');
  const partialDarwinIsolated = partialDarwinTclaudeLayer && source.includes('isolated network');
  const openCodeExecutorLayer = unverified
    && source.includes('OpenCode tool-executing server confined');
  const partialLinuxTclaudeLayer = unverified
    && source.includes('ambient host Unix sockets reachable');
  const caveat = openCodeExecutorLayer
    ? ` ⚠ Partial fidelity: filesystem mounts confine OpenCode's tool-executing server, but the attach pane stays outside the boundary, the authenticated loopback control plane remains reachable, and host networking plus ambient host Unix sockets remain available.`
    : partialDarwinIsolated
    ? ` ⚠ Partial fidelity: Seatbelt enforces filesystem and network operations, but there is no PID isolation or constructed root, and hidden paths remain enumerable.`
    : partialDarwinTclaudeLayer
    ? ` ⚠ Partial fidelity: Seatbelt enforces filesystem operations, but hidden paths remain enumerable and the host network plus ambient Unix sockets remain reachable.`
    : partialLinuxTclaudeLayer
      ? ` ⚠ Partial fidelity: filesystem mounts are enforced, but ambient host Unix sockets remain connectable.`
      : unverified
        ? ` ⚠ Unverified: tclaude could not read a settings file that outranks this, so the real posture may differ.`
        : '';
  if (state === 'on') {
    // `source` for a launch-decided verdict names the tier that CHOSE the mode
    // in place of the anonymous actor: `global default profile "agents"
    // (sandbox \`on\`)` rather than `this launch (sandbox \`on\`)`.
    // "forced ON for this launch" alone read as the operator's own doing, which
    // is wrong for the common case where a group or global default profile
    // carries the sandbox and they never picked one.
    const why = mode === 'on'
      ? `forced ON by ${source || 'this launch'}`
      // Managed policy outranks the launch's own `--settings` block, so it can
      // turn the sandbox ON over an explicit `off`. Calling that "not chosen at
      // launch" would be true but useless, and calling an enterprise policy file
      // "your Claude Code settings" is simply wrong.
      : mode === 'off'
        ? `forced ON by ${source || 'a higher-precedence settings file'}, overriding this launch's \`off\``
        : `not chosen at launch — inherited from your Claude Code settings${via}`;
    // "working dir writable, $HOME read-only" described Claude Code's DEFAULT
    // sandbox shape, which any profile `allowWrite` under $HOME can falsify —
    // in every mode, not just some. The shape is whatever the operator's
    // settings and the applied profile resolved to, and the profile clause
    // below names that; asserting a fixed one here was the same over-claim as
    // naming a single settings file and calling it the configuration.
    const confined = unverified ? '' : ' Bash is confined.';
    // The hedge rides in the opening posture rather than only in the caveat
    // below: "on" alone, read first, is the claim this case cannot make.
    const posture = unverified ? 'on (unverified)' : 'on';
    return {
      // The exact implementation value is load-bearing: tclaude-layer has
      // earned the real lock because the OS wall is established even when its
      // fidelity caveats remain. Future/unknown implementations must earn
      // their own badge rather than inheriting this exception by name shape.
      danger: implementation === 'tclaude-layer'
        ? false
        : implementation && implementation !== 'harness-builtin'
          ? true
          : unverified,
      // A mode of `off` means tclaude emitted `{"sandbox":{"enabled":false}}`
      // and, with it, NONE of the profile's filesystem rules (claudeSettingsJSON
      // skips every filesystem key for an `off` launch). Managed policy can
      // still force the sandbox on over that, but what it enforces is the
      // operator's own settings — the profile's rules were never handed to the
      // HARNESS. sandboxProfileClause separately accounts for tclaude-layer,
      // which renders those same profile rules into its outer OS wall.
      rulesWithheldBecause: mode === 'off'
        ? 'this launch requested sandbox `off`, so none of its filesystem rules were emitted'
        : '',
      title: `${prefix}: ${posture} — ${why}.${confined}${caveat}`,
    };
  }
  if (mode === 'on') {
    // The launch asked for `on` and lost. Only enterprise managed policy
    // outranks a `--settings` block, and an operator who believes this agent is
    // confined is precisely who needs telling that it is not — so the title
    // leads with the posture that won, then names the request it overrode.
    return {
      danger: true, rulesWithheldBecause: 'the sandbox is off',
      title: `${prefix}: off — this launch asked for the OS sandbox to be ON, but ${source || 'a higher-precedence settings file'} turned it off. The agent's Bash runs unconfined.${caveat}`,
    };
  }
  if (mode === 'off') {
    // `source` is attributed the same way the `on` branch's is, so an `off`
    // that a group or global default profile chose says so. The old wording
    // ("forced OFF for this launch … Explicit opt-in") credited a human with
    // opting this agent out of containment — the mirror image of the
    // misattribution this tooltip exists to remove, and in the direction that
    // matters more, since it is the claim an operator is least likely to doubt.
    return {
      danger: true,
      rulesWithheldBecause: 'this launch requested sandbox `off`, so none of its filesystem rules were emitted',
      title: `${prefix}: off — the OS sandbox is forced OFF by ${source || 'this launch'}. The agent's Bash runs unconfined.${caveat}`,
    };
  }
  return null;
}

const SANDBOX_SCOPE_LABELS = {
  global: 'global default',
  group: 'group default',
  explicit: 'chosen for this agent',
};

// sandboxProfileClause names the tclaude sandbox profiles applied to a launch
// and, when their filesystem rules are NOT in force, says why.
//
// A profile is orthogonal to the sandbox STATE: it never decides whether the
// agent is sandboxed, it supplies the rules. For Claude Code those filesystem
// grants are compiled into the harness's own `sandbox.filesystem.*` through
// `--settings`, so they bite only while the sandbox is enabled, while a
// profile's environment entries are plain env vars that apply either way. A
// tooltip that named only the settings file which enabled the sandbox read as
// the whole configuration, when a profile is what actually shaped it.
//
// "Customized by" is deliberately weaker than "rules from": the snapshot the
// browser gets carries profile NAMES only, so this cannot know whether a given
// profile contributes filesystem rules, environment entries, or both. It names
// the profile and lets the withheld-reason carry the one thing that IS known.
//
// Returns "" for a launch that recorded no policy at all — a row older than the
// snapshot. Saying "no profile" there would invent a fact rather than report
// one; "none applied" is only claimed where a resolved policy exists to say it.
function sandboxProfileClause(member, withheldBecause) {
  const applied = member.state?.sandbox_profiles || [];
  if (!applied.length) {
    // Four different facts, and flattening them would be its own small lie: the
    // launch MODE discarded every profile tier; the operator chose "none"; the
    // launch resolved to no profile; or nothing was ever recorded (a row older
    // than the snapshot), in which case an absence tclaude never observed is
    // not reported as one.
    if (member.state?.sandbox_profiles_omitted) {
      return sandboxProfilesUnsupported(member)
        ? ' tclaude sandbox profiles do not apply under this launch mode.'
        : ' No tclaude sandbox profile — this launch omitted them.';
    }
    return member.state?.sandbox_profiles_recorded ? ' No tclaude sandbox profile applied.' : '';
  }
  const names = applied
    .map((p) => `“${p.name}” (${SANDBOX_SCOPE_LABELS[p.scope] || p.scope})`)
    .join(' + ');
  const clause = ` Customized by tclaude sandbox profile ${names}.`;
  const harness = (member.state?.harness || 'claude').trim();
  const rulesOwnedByTclaudeLayer =
    member.state?.sandbox_implementation === 'tclaude-layer'
    && (harness === 'claude' || harness === 'codex');
  if (rulesOwnedByTclaudeLayer && member.state?.os_sandbox_state === 'on') {
    const their = applied.length > 1 ? 'Their' : 'Its';
    const they = applied.length > 1 ? 'they define' : 'it defines';
    return clause + ` ${their} filesystem rules are enforced as OS mounts by the tclaude layer`
      + ` (the inner harness sandbox is off by design);`
      + ` any environment entries ${they} also apply.`;
  }
  const reason = rulesOwnedByTclaudeLayer
    ? 'the tclaude layer is not active'
    : withheldBecause;
  if (!reason) return clause;
  const their = applied.length > 1 ? 'Their' : 'Its';
  const they = applied.length > 1 ? 'they define' : 'it defines';
  return clause + ` ${their} filesystem rules are not in force (${reason});`
    + ` any environment entries ${they} still apply.`;
}

// sandboxProfilesUnsupported reports whether the launch's own MODE is what
// discarded the profile tiers, as opposed to the operator omitting them.
//
// The daemon sets ProfilesOmitted for both, so the flag alone cannot tell them
// apart, and asserting the mode did it turns an operator who deliberately
// picked sandbox profile "none" into someone whose launch mode overrode them.
// This mirrors sandboxProfilesDisabled (spawn_sandbox_guard.go) — Codex's
// `danger-full-access` is a raw no-sandbox launch that cannot carry the managed
// permission profile tclaude policy compiles into. Keep the two in step.
function sandboxProfilesUnsupported(member) {
  const harness = (member.state?.harness || 'claude').trim();
  const mode = (member.state?.sandbox_mode || '').trim();
  return harness === 'codex' && mode === 'danger-full-access';
}

// sandboxIndicator resolves an agent's sandbox posture to the glyph that trails
// its harness line, or null when there is nothing to say. The mode, the
// deciding settings file and the applied sandbox profiles live in the tooltip
// rather than on screen: a padlock per row is enough to scan a group for the
// unconfined agent, and the framed "🔒 workspace-write" chip this replaces cost
// every row a second line to say less.
function sandboxIndicator(member) {
  const mode = member.state?.sandbox_mode || '';
  const offline = !member.online;
  const prefix = offline ? 'Last used sandbox' : 'Sandbox';
  // A recorded verdict wins: it is the resolved outcome, where the mode is only
  // the request. Absent one (a pre-column row, or Codex — whose --sandbox mode
  // IS its posture) the mode-driven branch below is unchanged.
  const state = member.state?.os_sandbox_state || '';
  if (state) {
    const badge = osSandboxBadge(mode, state, member.state?.os_sandbox_source || '', prefix,
      !!member.state?.os_sandbox_unverified, member.state?.sandbox_implementation || '');
    if (!badge) return null;
    // The label the chip used to print ("on", "on overridden", "on?") is not
    // dropped, only moved: every osSandboxBadge title opens with the resolved
    // posture, so the tooltip stays a complete account on its own.
    return {
      danger: badge.danger, offline,
      // On the harness-builtin path, a verdict tclaude could not prove says
      // nothing about the profile's rules either way, so the clause makes no
      // fresh enforcement claim. The tclaude-layer branch is different: the
      // outer renderer owns those rules and sandboxProfileClause says so.
      title: badge.title + sandboxProfileClause(member, badge.rulesWithheldBecause),
    };
  }
  if (!mode || mode === 'inherit') return null;
  // `off` is Claude-only (no other harness offers it) and means the OS sandbox
  // is disabled outright, so it is a danger glyph on a pre-verdict row too —
  // otherwise every legacy `off` agent keeps a padlock it has not earned.
  const danger = mode === 'danger-full-access' || mode === 'off';
  // A harness whose MODE is its posture (Codex) records no verdict, so it has
  // no os_sandbox_source to fold the chooser into — sandbox_mode_source is
  // where its attribution lives. The old wording said "Explicit opt-in" for
  // every such row, which is wrong whenever a group or global default profile
  // carried the mode and the operator never picked one.
  const chosenBy = member.state?.sandbox_mode_source || '';
  const by = chosenBy ? ` Chosen by ${chosenBy}.` : '';
  const title = danger
    ? mode === 'off'
      // Claude's `off` disables the OS sandbox; it has no "full access" mode,
      // so borrowing Codex's vocabulary here would name a concept it lacks.
      ? `${prefix}: off — the OS sandbox is disabled for this launch. The agent's Bash runs unconfined.${by}`
      : `${prefix}: ${mode} — the OS sandbox is OFF (full access).${by}`
    : `${prefix}: ${mode} — launch-time OS sandbox confining the agent's writes.${by}`;
  // A mode-driven row (Codex, or a legacy Claude row) states its own posture,
  // so the mode alone decides whether the profile's rules are in force.
  const withheld = danger ? 'the sandbox is off' : '';
  return { danger, title: title + sandboxProfileClause(member, withheld), offline };
}

export function SandboxBadge({ member }) {
  const badge = sandboxIndicator(member);
  if (!badge) return null;
  const unlocked = !!member.state?.temporary_sandbox_mode;
  // A warning can also mean a normally unconfined launch or an unverified
  // verdict. Only the warning produced by this temporary override is a
  // restore shortcut; otherwise clicking a warning would misleadingly offer
  // to "unlock" an agent that is already unconfined.
  const actionable = !!member.online && (unlocked || !badge.danger);
  const action = unlocked ? 'restore' : 'unlock';
  const actionHint = unlocked
    ? ' Click to stop and restart this agent with its preserved normal sandbox configuration.'
    : ' Click to stop and restart this agent with its sandbox temporarily disabled.';
  const title = badge.title + (actionable ? actionHint : '');
  const className = `sandbox-badge${badge.danger ? ' sandbox-danger' : ''}${badge.offline ? ' runtime-meta-offline' : ''}${actionable ? ' sandbox-action' : ''}`;
  // aria-label carries the same full text as the tooltip: a glyph-only
  // indicator whose whole meaning is the hover would otherwise be pointer-only.
  return html`<span class=${className} role=${actionable ? 'button' : 'note'}
    tabindex=${actionable ? '0' : null}
    data-act=${actionable ? 'sandbox-restart' : null}
    data-action=${actionable ? action : null}
    ...${actionable ? memberAttrs(member) : {}}
    aria-label=${title} title=${title}>${badge.danger ? '⚠' : '🔒'}</span>`;
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

function StatePill({ state, online }) {
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
  return html`<span class=${`state-pill ${className}`} title=${info.title} aria-label=${ariaLabel}>${label}</span>`;
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
  const detail = state?.status_detail || '';
  const title = detail ? `${status}: ${detail}` : status;
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
  return html`<span ref=${hostRef} class="slop-machine" data-opaque-host="slop-reels" data-status=${status} data-conv=${conv || ''} title=${title} aria-label=${title}></span>`;
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
  const detail = state?.status_detail || '';
  const [glyph, label] = WIZARD_STATE[status] || ['✨', status];
  const title = detail ? `${status}: ${detail}` : status;
  return html`<span class="wizard-pill" data-status=${status} data-conv=${conv || ''} title=${title} aria-label=${title}><span class="wizard-pill-glyph">${glyph}</span> ${label}</span>`;
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
  // The window here is already the EFFECTIVE one — min(model window, pinned
  // auto-compaction window) — and context_pct is measured against it, so the
  // meter shows how close the agent is to its next compaction rather than to a
  // model limit it may never reach. Name the pin when there is one, otherwise
  // the smaller number looks like a bug.
  const win = Number(state?.context_window_size || 0);
  const pinned = Number(state?.auto_compact_window || 0);
  const pinNote = pinned > 0 && pinned <= win ? ` (auto-compacts at ${fmtTokens(pinned)})` : '';
  const regularTitle = !known ? 'context window: usage not reported yet'
    : win > 0 && total > 0 ? `context: ${fmtTokens(total)} / ${fmtTokens(win)} tokens — ${Math.round(pct)}%${pinNote}`
      : `context: ${Math.round(pct)}% full${pinNote}`;
  const wizardTitle = !known ? '🔮 Mana reserves: not yet divined'
    : win > 0 && total > 0 ? `🔮 Mana: ${fmtTokens(total)} / ${fmtTokens(win)} channeled — ${Math.round(pct)}%${pinNote}`
      : `🔮 Mana: ${Math.round(pct)}% channeled${pinNote}`;
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
  return html`<td class="state-cell"><${ContextMeter} state=${state} /><${StatePill} state=${state} online=${member.online} /><${SlopMachine} state=${state} online=${member.online} conv=${member.conv_id} /><${WizardPill} state=${state} online=${member.online} conv=${member.conv_id} />${member.online ? html`<${ActivityBadges} state=${state} />` : null}</td>`;
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
    <${MenuButton} member=${member} selector="label" act="cron-new" attrs=${{ 'data-prefill': prefill }} regular="schedule…" wizard="bind ritual…" title=${wizardCopy(`Schedule a recurring nudge for ${label}`, `Bind a recurring ritual for familiar ${label}`)} />
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

function MemberName({ member, snapshot, actions, grants, editorKey }) {
  const state = member.state || {};
  const canRename = harnessCanRename(snapshot, state.harness);
  const idPrefix = memberColHidden('id') ? `${idTooltip(member.agent_id, member.conv_id)} — ` : '';
  if (!canRename) return html`<div class="rowname"><span class="rowname-text rowname-fixed" title=${`${idPrefix}This agent's harness does not support renaming`}>${member.title || '(unnamed)'}</span><${SudoBadge} grants=${grants} conv=${member.conv_id} /></div>`;
  return html`<div class="rowname"><${InlineEditor}
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
    case 'ctl': return html`<td><div class="agent-ctl"><${AgentStatusDot} member=${member} /><${MemberActions} member=${member} group=${group} snapshot=${snapshot} actions=${actions} ungrouped=${ungrouped} menuKey=${menuKey} /></div><${HarnessLine} member=${member} /></td>`;
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
