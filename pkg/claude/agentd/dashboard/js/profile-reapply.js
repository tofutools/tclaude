// profile-reapply.js — the profile editor's "Running agents" row: which
// running agents keep a copy of this spawn profile, and what a re-apply would
// change for each (tcl-sqs8sr).
//
// An agent copies its spawn profile at first spawn, so an edit reaches new
// spawns only. Review lists the agents holding a copy
// (GET /api/spawn-profiles/{name}/running-agents) with a per-agent diff:
// fixed and explicit values are kept, spawn-only ones are not re-applied.
// Apply re-applies to the selected agents one at a time
// (POST /api/spawn-profiles/{name}/reapply): a stopped agent takes the copy on
// its next launch, an idle one is stopped, updated and resumed, a busy one is
// skipped (tcl-hxuwnc). Each grant change shows what the re-apply does to the
// agent's row (tcl-fg2kqk).
import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { BatchPreview, selectedRows, selectionOf } from './batch-preview.js';

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
    <code>${g.slug}</code>: ${shown(g.from)} → ${shown(g.to)}${g.status ? html` <span class="ui-hint">(${g.status})</span>` : ''}
  </li>`)}</ul>`;
}

export const reapplyColumns = Object.freeze([
  { key: 'agent', label: 'Agent', render: (r) => html`<span title=${r.agent_id}>${r.title || r.agent_id}</span>` },
  { key: 'tier', label: 'Copied as', render: (r) => (r.copied_at ? `${r.tier}, ${r.copied_at}` : r.tier) },
  { key: 'online', label: 'Running', render: (r) => (r.online ? 'yes' : 'no') },
  { key: 'fields', label: 'Re-apply would', render: (r) => html`<${FieldChanges} fields=${r.fields} />` },
  { key: 'grants', label: 'Grants', render: (r) => (r.grants_error ? html`<span class="ui-hint">${r.grants_error}</span>` : html`<${GrantChanges} grants=${r.grants} />`) },
]);

const rowKey = (r) => r.agent_id;

// reapplyOutcomeText is the per-agent result line after Apply.
export function reapplyOutcomeText(result) {
  switch (result?.outcome) {
    case 'applied': return 'applied; takes effect on its next launch';
    case 'applied_restarted': return 'applied; restarted';
    case 'busy_skipped': return result.detail || 'busy, skipped';
    case 'restarted_unapplied': return result.detail || 'refused; resumed with its old settings';
    default: return result?.error ? `failed: ${result.error}` : 'failed';
  }
}

async function reapplyOne(fetchImpl, profileName, agentID) {
  const response = await fetchImpl(`/api/spawn-profiles/${encodeURIComponent(profileName)}/reapply`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ agent_id: agentID }),
  });
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = null; }
  if (!response.ok) return { outcome: body?.outcome || 'failed', detail: body?.outcome ? body.error : '', error: body?.error || text || `HTTP ${response.status}` };
  return body || { outcome: 'failed', error: 'empty response' };
}

export function ProfileReapplyControl({ profileName, fetchImpl = globalThis.fetch }) {
  const [phase, setPhase] = useState('idle'); // idle | loading | ready | confirm | applying
  const [body, setBody] = useState(null);
  const [error, setError] = useState('');
  const [selected, setSelected] = useState(new Set());
  const [results, setResults] = useState({});

  async function review() {
    setPhase('loading'); setError('');
    try {
      const got = await readJSON(await fetchImpl(`/api/spawn-profiles/${encodeURIComponent(profileName)}/running-agents`, { credentials: 'same-origin' }));
      setBody(got); setResults({});
      setSelected(selectionOf(Array.isArray(got?.agents) ? got.agents : [], rowKey));
      setPhase('ready');
    } catch (e) { setError(e.message || String(e)); setPhase('idle'); }
  }

  const rows = Array.isArray(body?.agents) ? body.agents : [];
  const apply = body?.apply || {};
  const chosen = selectedRows(rows, rowKey, selected);
  const busy = phase === 'applying';

  async function applySelected() {
    setPhase('applying');
    for (const row of chosen) {
      let result;
      try { result = await reapplyOne(fetchImpl, profileName, row.agent_id); } catch (e) { result = { outcome: 'failed', error: e.message || String(e) }; }
      setResults((prev) => ({ ...prev, [row.agent_id]: result }));
    }
    setPhase('ready');
  }
  const columns = Object.keys(results).length === 0 ? reapplyColumns : [...reapplyColumns,
    { key: 'result', label: 'Result', render: (r) => (results[r.agent_id] ? html`<span data-outcome=${results[r.agent_id].outcome}>${reapplyOutcomeText(results[r.agent_id])}</span>` : '') }];
  const notes = [
    'Saving this profile changes new spawns only; each running agent keeps the copy it was spawned with.',
    ...(body?.grants_note ? [body.grants_note] : []),
    ...(Array.isArray(body?.notes) ? body.notes : []),
  ];
  return html`<div class="profile-reapply" id="profile-editor-reapply">
    ${error && html`<div class="ui-error-box">${error}</div>`}
    ${phase === 'idle' || phase === 'loading' ? html`<${Button} size="sm" disabled=${phase === 'loading'} onClick=${review}>Review running agents…<//>` : html`
      <${BatchPreview} rows=${rows} rowKey=${rowKey} columns=${columns} selected=${selected}
        onChange=${setSelected} disabled=${!apply.enabled || busy || phase === 'confirm'} label="running agents from this profile" notes=${notes}
        empty="No running agent keeps a copy of this profile." />
      ${rows.length > 0 && html`<div class="profile-reapply-acts">
        ${phase === 'confirm' ? html`
          <span>Re-apply to ${chosen.length} agent${chosen.length === 1 ? '' : 's'}? Idle running agents are restarted.</span>
          <${Button} size="sm" variant="success" onClick=${applySelected}>Apply to ${chosen.length}<//>
          <${Button} size="sm" onClick=${() => setPhase('ready')}>Cancel<//>` : html`
          <${Button} size="sm" variant="success" disabled=${!apply.enabled || busy || chosen.length === 0} title=${apply.reason || ''}
            onClick=${() => setPhase('confirm')}>${busy ? 'Applying…' : 'Apply to selected'}<//>
          ${Object.keys(results).length > 0 && !busy && html`<${Button} size="sm" onClick=${review}>Refresh<//>`}
          ${apply.reason && html`<span class="ui-hint">${apply.reason}</span>`}`}
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
