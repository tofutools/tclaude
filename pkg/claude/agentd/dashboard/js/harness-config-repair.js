// harness-config-repair.js — the Diagnostics › Doctor control for agents whose
// explicit --harness-config a relaunch lost before #3817 (tcl-pa5589).
//
// Review lists them (GET /api/harness-config-repairs) in a batch preview,
// every agent selected; Restore re-pins the ones left selected (POST). A
// re-pin changes the recorded launch only, so a running agent is offered a
// restart afterwards (POST /api/agents/{conv}/restart, refused unless idle).
import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { BatchPreview, selectedRows, selectionOf } from './batch-preview.js';

const html = htm.bind(h);

const CHANGE_LABEL = {
  widened: 'widened: runs without the read-only floor it asked for',
  narrowed: 'narrowed: runs under the floor it opted out of',
  unpinned: 'unpinned: same posture today, but a profile edit could change it',
};

async function readJSON(response) {
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!response.ok) throw new Error(body?.error || body?.message || (typeof body === 'string' ? body : '') || `HTTP ${response.status}`);
  return body;
}

export const repairColumns = Object.freeze([
  { key: 'agent', label: 'Agent', render: (r) => html`<span title=${r.agent_id}>${r.title || r.agent_id}</span>` },
  { key: 'requested', label: 'Spawned with' },
  { key: 'current', label: 'Runs with', render: (r) => (r.source ? `${r.current} (profile ${r.source})` : r.current) },
  { key: 'change', label: 'Change', render: (r) => html`<span class=${`ui-tag hc-repair-${r.change}`} title=${CHANGE_LABEL[r.change] || ''}>${r.change}</span>` },
  { key: 'online', label: 'Running', render: (r) => (r.online ? 'yes' : 'no') },
]);

const rowKey = (r) => r.agent_id;

export function HarnessConfigRepairControl({ notify, onDone, fetchImpl = globalThis.fetch }) {
  const [phase, setPhase] = useState('idle'); // idle | loading | ready | applying | applied
  const [rows, setRows] = useState([]);
  const [limits, setLimits] = useState([]);
  const [selected, setSelected] = useState(new Set());
  const [restartable, setRestartable] = useState([]);
  const [error, setError] = useState('');

  async function review() {
    setPhase('loading'); setError('');
    try {
      const body = await readJSON(await fetchImpl('/api/harness-config-repairs', { credentials: 'same-origin' }));
      const agents = Array.isArray(body?.agents) ? body.agents : [];
      setRows(agents); setLimits(Array.isArray(body?.limits) ? body.limits : []);
      setSelected(selectionOf(agents, rowKey));
      setPhase('ready');
    } catch (e) { setError(e.message || String(e)); setPhase('idle'); }
  }

  async function restore() {
    const chosen = selectedRows(rows, rowKey, selected);
    if (!chosen.length) return;
    setPhase('applying'); setError('');
    try {
      const body = await readJSON(await fetchImpl('/api/harness-config-repairs', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agent_ids: chosen.map(rowKey) }),
      }));
      const results = Array.isArray(body?.results) ? body.results : [];
      const done = results.filter((r) => r.repinned);
      const failed = results.filter((r) => !r.repinned);
      const byID = new Map(rows.map((r) => [r.agent_id, r]));
      setRestartable(done.filter((r) => r.online).map((r) => byID.get(r.agent_id)).filter(Boolean));
      notify?.(`Restored ${done.length} agent${done.length === 1 ? '' : 's'}${failed.length ? `; ${failed.length} not changed: ${failed.map((f) => f.error).join('; ')}` : ''}`, failed.length > 0);
      setPhase('applied');
    } catch (e) { setError(e.message || String(e)); setPhase('ready'); }
  }

  async function restartNow() {
    const failures = [];
    for (const row of restartable) {
      try {
        await readJSON(await fetchImpl(`/api/agents/${encodeURIComponent(row.conv_id)}/restart`, { method: 'POST', credentials: 'same-origin' }));
      } catch (e) { failures.push(`${row.title || row.agent_id}: ${e.message || e}`); }
    }
    notify?.(failures.length ? `Restart failed for ${failures.join('; ')}` : 'Restarted', failures.length > 0);
    setRestartable([]);
  }

  const busy = phase === 'loading' || phase === 'applying';
  return html`<div class="hc-repair">
    ${error && html`<div class="doctor-error ui-error-box">${error}</div>`}
    ${(phase === 'idle' || phase === 'loading') && html`<div class="doctor-acts">
      <${Button} size="sm" disabled=${busy} onClick=${review}>Review agents<//>
    </div>`}
    ${(phase === 'ready' || phase === 'applying') && html`
      <${BatchPreview} rows=${rows} rowKey=${rowKey} columns=${repairColumns} selected=${selected}
        onChange=${setSelected} disabled=${busy} label="agents" notes=${limits}
        empty="No agent needs this repair any more." />
      <div class="doctor-acts">
        <${Button} size="sm" variant="success" disabled=${busy || selected.size === 0} onClick=${restore}>Restore selected<//>
      </div>`}
    ${phase === 'applied' && html`<div class="doctor-acts">
      ${restartable.length > 0 && html`<span class="hc-repair-note">${restartable.length} restored agent${restartable.length === 1 ? ' is' : 's are'} running and take${restartable.length === 1 ? 's' : ''} the posture on the next resume.</span>
        <${Button} size="sm" onClick=${restartNow}>Restart now<//>`}
      <${Button} size="sm" onClick=${() => onDone?.()}>Done<//>
    </div>`}
  </div>`;
}
