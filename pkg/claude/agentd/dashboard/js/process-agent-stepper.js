// process-agent-stepper.js -- the Terminals view of an agent-scoped run (mock T3).
//
// A run bound to one agent (◉) executes inside that agent's context, so in
// the agent's own terminal it reads better as a vertical stepper than as a
// graph: agentic steps are turns injected into this conversation, program
// steps run on the host, decisions wait for a human. A bounds strip makes the
// agent's envelope explicit (working dir, sandbox, grants, spend, identity),
// and a step that will leave that envelope is flagged before the run gets
// there. The stepper is pure presentation: it renders the read model's
// projection plus the dashboard snapshot, and every action is injected.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import {
  ProcessNodeGlyph, ProcessPauseControl, ProcessPausedChip, ProcessScopeChip, ProcessStateChip, runIconView, runName, runSteps,
} from './process-kit.js';

const html = htm.bind(h);

// ── Pure helpers ─────────────────────────────────────────────────────────

// namedScope labels a ◉ scope bound to this pane's agent with the agent's
// display name rather than its stable id.
export function namedScope(scope, agentId, name) {
  if (!scope || String(scope.kind || '').toLowerCase() !== 'agent' || !name) return scope;
  return scope.agentId === agentId && !scope.name && !scope.agentName ? { ...scope, agentName: name } : scope;
}

// isAgentScopedTo reports whether run is bound to agentId (◉ scope).
export function isAgentScopedTo(run, agentId) {
  const scope = run?.scope || {};
  return String(scope.kind || '') === 'agent' && !!agentId && String(scope.agentId || '') === String(agentId);
}

function baseName(path) {
  const clean = String(path || '').replace(/\/+$/, '');
  return clean.slice(clean.lastIndexOf('/') + 1);
}

// agentRunBounds derives the envelope an agent-scoped run executes in from
// the dashboard snapshot. Every field is what the snapshot already shows for
// the agent elsewhere; nothing here is a new policy.
export function agentRunBounds(snapshotValue, agentId) {
  if (!snapshotValue || !agentId) return null;
  const member = (snapshotValue.agents || []).find((entry) => entry && (entry.agent_id === agentId || entry.conv_id === agentId));
  if (!member) return null;
  const state = member.state || {};
  const profiles = (state.sandbox_profiles || []).map((profile) => profile?.name).filter(Boolean);
  const sandbox = profiles.length ? profiles.join(', ') : String(state.sandbox_mode || '');
  const grants = [...(snapshotValue.permissions?.grants?.[member.conv_id] || [])].sort();
  const groupIDs = new Map((snapshotValue.groups || []).map((group) => [group?.name, group?.id]));
  const groups = (member.groups || []).map((name) => ({ id: groupIDs.get(name) ?? name, name }));
  return {
    name: String(member.title || member.name || ''),
    cwd: String(state.cwd || ''),
    dir: baseName(state.cwd),
    sandbox,
    grants,
    costUSD: Number(state.cost_usd || 0),
    groups,
  };
}

function clock(iso) {
  const at = Date.parse(iso || '');
  if (!Number.isFinite(at)) return '';
  const date = new Date(at);
  return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}

function ago(iso, now) {
  const at = Date.parse(iso || '');
  if (!Number.isFinite(at)) return '';
  const minutes = Math.max(0, Math.floor((now - at) / 60000));
  return minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h`;
}

function outcomesOf(run, nodeId) {
  return [...new Set((run?.edges || []).filter((edge) => String(edge.from) === nodeId)
    .map((edge) => String(edge.label || '')).filter((label) => label && label !== 'next'))];
}

// stepperRows turns the run's work steps into stepper rows: what the step is,
// where it executes relative to this agent, what has happened, and any
// warning that the step leaves the agent's bounds or needs a human.
export function stepperRows(run, agentId, now = Date.now()) {
  return runSteps(run).map((node) => {
    const dispatch = node.dispatch || null;
    const settled = node.state === 'done' || node.state === 'skipped';
    const row = {
      id: node.id, label: node.label, kind: node.kind, state: node.state,
      here: false, detail: [], warning: '', time: '',
    };
    switch (node.kind) {
      case 'agent': {
        const performer = String(dispatch?.agentId || (node.performers || [])[0]?.id || '');
        row.here = !performer || performer === String(agentId);
        if (dispatch?.deliveredAt) {
          row.detail.push(`turn injected at ${clock(dispatch.deliveredAt)}`);
          row.time = ago(dispatch.deliveredAt, now);
        } else if (!settled) {
          row.detail.push(node.state === 'planned' ? 'will be injected as a turn' : 'waiting to be injected');
        }
        if (Number(dispatch?.attempt) > 1) row.detail.push(`loop ×${dispatch.attempt}`);
        if (!row.here && performer) row.detail.push(`performed by ${dispatch?.name || performer}`);
        if (dispatch?.state === 'needs_performer' && dispatch.waitReason) row.warning = dispatch.waitReason;
        break;
      }
      case 'command': {
        const profile = String(node.performer?.ref || '');
        if (settled) row.detail.push(`ran on the host${profile ? ` · profile ${profile}` : ''}`);
        else row.warning = `runs on the host, outside this agent's sandbox (program profile ${profile || 'none'})`;
        break;
      }
      case 'decision': {
        const outcomes = outcomesOf(run, node.id);
        if (outcomes.length) row.detail.push(outcomes.join(' · '));
        if (!settled) row.warning = 'waits for a human decision';
        break;
      }
      case 'human':
        row.detail.push('performed by a human');
        if (!settled) row.warning = 'waits for a human';
        break;
      default:
        if (node.description) row.detail.push(node.description);
    }
    return row;
  });
}

// ── Components ───────────────────────────────────────────────────────────

function BoundsStrip({ bounds }) {
  if (!bounds) {
    return html`<div class="pk-bounds" data-process-bounds><span class="pk-bounds-chip pk-bounds-unknown">bounds unavailable: the agent is not in the snapshot</span></div>`;
  }
  const grants = bounds.grants.length
    ? `${bounds.grants.length} grant${bounds.grants.length === 1 ? '' : 's'}`
    : 'defaults only';
  return html`<div class="pk-bounds" data-process-bounds>
    <span class="pk-bounds-chip" title=${bounds.cwd || 'working directory unknown'}>📁 ${bounds.dir || 'unknown dir'}</span>
    <span class="pk-bounds-chip" title="Sandbox the agent was launched with">🛡 sandbox: ${bounds.sandbox || 'none'}</span>
    <span class="pk-bounds-chip" title=${bounds.grants.length ? bounds.grants.join('\n') : 'No per-agent grants beyond the defaults'}>🔑 perms: ${grants}</span>
    <span class="pk-bounds-chip" title="Spend so far; runs have no budget cap">$ ${bounds.costUSD.toFixed(2)} spent · no cap</span>
    <span class="pk-bounds-chip" title="The run is bound to the agent's stable id, so it follows the agent across new conversations">⟲ survives reincarnate</span>
  </div>`;
}

function StepRow({ row }) {
  return html`<div class=${`pk-stepper-step pk-state-${row.state}${row.here ? ' pk-stepper-step-here' : ''}`} data-process-step=${row.id}>
    <span class="pk-stepper-step-dot" aria-hidden="true"><${ProcessNodeGlyph} kind=${row.kind} /></span>
    <div class="pk-stepper-step-body">
      <div class="pk-stepper-step-title">
        <b>${row.label}</b>
        ${row.kind === 'agent' && row.here ? html`<span class="pk-stepper-step-here-label">← this conversation</span>` : null}
        ${row.time ? html`<span class="pk-stepper-step-time">${row.time}</span>` : null}
      </div>
      ${row.detail.length ? html`<div class="pk-stepper-step-detail">${row.detail.join(' · ')}</div>` : null}
      ${row.warning ? html`<div class="pk-stepper-step-warning" role="note">⚠ ${row.warning}</div>` : null}
    </div>
  </div>`;
}

// ProcessAgentStepper is mock T3. actions: { promote(run, groupName),
// pause(run), resume(run) } — each optional; onGraph switches the popover to its graph view.
export function ProcessAgentStepper({ run, agentId, bounds = null, actions = {}, onGraph = null, onClose = null }) {
  const view = runIconView(run, agentId);
  const rows = stepperRows(run, agentId);
  const groups = bounds?.groups || [];
  const [target, setTarget] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const group = target || groups[0]?.name || '';
  const promote = async () => {
    setBusy(true);
    setError('');
    try {
      await actions.promote(run, group);
    } catch (err) {
      setError(String(err?.message || err || 'promotion failed'));
    } finally {
      setBusy(false);
    }
  };
  const promoteTitle = !actions.promote ? 'Promotion is not available here'
    : groups.length ? `Re-scope this run to group ${group}` : 'This agent is in no group to promote the run to';
  return html`
    <div class="pk-run-popover pk-agent-stepper" role="dialog" aria-label=${`${runName(run)} in this agent`} data-process-popover=${run.runId} data-process-stepper>
      <div class="pk-run-popover-head">
        <b class="pk-run-popover-name">${runName(run)}</b>
        <${ProcessScopeChip} scope=${namedScope(run.scope, agentId, bounds?.name)} />
        <${ProcessStateChip} state=${view.state} label=${String(run.status || view.state)} />
        <${ProcessPausedChip} run=${run} />
        ${onClose ? html`<button type="button" class="pk-run-popover-close" title="Close" aria-label="Close" onClick=${onClose}>×</button>` : null}
      </div>
      <${BoundsStrip} bounds=${bounds} />
      <div class="pk-stepper-list" role="list">${rows.map((row) => html`<${StepRow} key=${row.id} row=${row} />`)}</div>
      <div class="pk-run-popover-actions">
        <${ProcessPauseControl} run=${run} actions=${actions} />
        <button type="button" class="mux-btn" disabled=${!onGraph} onClick=${() => onGraph?.()}>⧉ graph view</button>
        ${groups.length > 1 ? html`<select class="pk-stepper-group" aria-label="Group to promote the run to"
          value=${group} onChange=${(event) => setTarget(event.currentTarget.value)}>
          ${groups.map((entry) => html`<option key=${entry.id} value=${entry.name}>${entry.name}</option>`)}</select>` : null}
        <button type="button" class="mux-btn" disabled=${busy || !actions.promote || !groups.length}
          title=${promoteTitle} onClick=${() => void promote()}>⇪ promote to group run</button>
        ${error ? html`<span class="pk-run-popover-error" role="alert">${error}</span>` : null}
      </div>
    </div>
  `;
}
