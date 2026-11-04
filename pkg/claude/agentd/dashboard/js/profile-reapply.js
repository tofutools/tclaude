// profile-reapply.js — the profile editor's "Running agents" row: which
// running agents keep a copy of this spawn profile, and what a re-apply would
// change for each (tcl-sqs8sr).
//
// An agent copies its spawn profile at first spawn, so an edit reaches new
// spawns only. Review lists the agents holding a copy
// (GET /api/spawn-profiles/{name}/running-agents) with a per-agent diff:
// fixed and explicit values are kept, spawn-only ones are not re-applied.
// Apply is shown but disabled until relaunch reads the spawn copy
// (tcl-an26ed); grant changes are shown only (tcl-kdpz4l).
import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { BatchPreview, selectionOf } from './batch-preview.js';

const html = htm.bind(h);

async function readJSON(response) {
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!response.ok) throw new Error(body?.error || body?.message || (typeof body === 'string' ? body : '') || `HTTP ${response.status}`);
  return body;
}

const shown = (value) => (value === '' || value == null ? 'unset' : value);

function FieldChanges({ fields }) {
  if (!fields?.length) return html`<span class="ui-hint">no launch change</span>`;
  return html`<ul class="profile-reapply-changes">${fields.map((f) => html`<li key=${f.key} data-status=${f.status}>
    <span title=${f.key}>${f.label}</span>: <code>${shown(f.from)}</code> → <code>${shown(f.to)}</code>
    <span class=${f.status === 'changes' ? 'ui-tag' : 'ui-hint'}>${f.status}</span>
  </li>`)}</ul>`;
}

function GrantChanges({ grants }) {
  if (!grants?.length) return html`<span class="ui-hint">none</span>`;
  return html`<ul class="profile-reapply-changes">${grants.map((g) => html`<li key=${g.slug}>
    <code>${g.slug}</code>: ${shown(g.from)} → ${shown(g.to)}
  </li>`)}</ul>`;
}

export const reapplyColumns = Object.freeze([
  { key: 'agent', label: 'Agent', render: (r) => html`<span title=${r.agent_id}>${r.title || r.agent_id}</span>` },
  { key: 'tier', label: 'Copied as', render: (r) => (r.copied_at ? `${r.tier}, ${r.copied_at}` : r.tier) },
  { key: 'online', label: 'Running', render: (r) => (r.online ? 'yes' : 'no') },
  { key: 'fields', label: 'Re-apply would', render: (r) => html`<${FieldChanges} fields=${r.fields} />` },
  { key: 'grants', label: 'Grants (shown only)', render: (r) => html`<${GrantChanges} grants=${r.grants} />` },
]);

const rowKey = (r) => r.agent_id;

export function ProfileReapplyControl({ profileName, fetchImpl = globalThis.fetch }) {
  const [phase, setPhase] = useState('idle'); // idle | loading | ready
  const [body, setBody] = useState(null);
  const [error, setError] = useState('');

  async function review() {
    setPhase('loading'); setError('');
    try {
      const got = await readJSON(await fetchImpl(`/api/spawn-profiles/${encodeURIComponent(profileName)}/running-agents`, { credentials: 'same-origin' }));
      setBody(got); setPhase('ready');
    } catch (e) { setError(e.message || String(e)); setPhase('idle'); }
  }

  const rows = Array.isArray(body?.agents) ? body.agents : [];
  const apply = body?.apply || {};
  const notes = [
    'Saving this profile changes new spawns only; each running agent keeps the copy it was spawned with.',
    ...(body?.grants_note ? [body.grants_note] : []),
    ...(Array.isArray(body?.notes) ? body.notes : []),
  ];
  return html`<div class="profile-reapply" id="profile-editor-reapply">
    ${error && html`<div class="ui-error-box">${error}</div>`}
    ${phase !== 'ready' ? html`<${Button} size="sm" disabled=${phase === 'loading'} onClick=${review}>Review running agents…<//>` : html`
      <${BatchPreview} rows=${rows} rowKey=${rowKey} columns=${reapplyColumns} selected=${selectionOf(rows, rowKey)}
        onChange=${() => {}} disabled=${true} label="running agents from this profile" notes=${notes}
        empty="No running agent keeps a copy of this profile." />
      ${rows.length > 0 && html`<div class="profile-reapply-acts">
        <${Button} size="sm" variant="success" disabled=${!apply.enabled} title=${apply.reason || ''}>Apply to selected<//>
        ${!apply.enabled && apply.reason && html`<span class="ui-hint">${apply.reason}</span>`}
      </div>`}`}
  </div>`;
}

// savedProfileNotice is the toast after an edit: the edit reaches new spawns
// only, so it says how many running agents keep their copy.
export function savedProfileNotice(name, response) {
  const n = Number(response?.running_agents_with_copy);
  if (!Number.isFinite(n) || n <= 0) return `profile updated: ${name}`;
  return `profile updated: ${name}. Applies to new spawns; ${n} running agent${n === 1 ? ' keeps its' : 's keep their'} copy.`;
}
