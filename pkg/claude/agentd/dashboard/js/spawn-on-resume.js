// spawn-on-resume.js — the spawn dialog's read-only "On resume" row (launch
// slice 10, operator-approved mock spawn-on-resume.png states 1 and 2).
//
// Collapsed by default to a one-line summary of what a resume will do; Show
// expands a read-only table generated from the launch registry
// (GET /api/relaunch-policy): the configurable rows with the global relaunch
// policy's current mode, then every other input grouped by its layer. The
// policy is changed in Config › Launch, not here.
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { MODE_LABELS, READONLY_SECTIONS } from './relaunch-policy-table.js';

const html = htm.bind(h);

const LIST_PREVIEW = 4;

// onResumeSummary is the collapsed line: how many configurable rows there
// are, and which differ from their default.
export function onResumeSummary(rows) {
  const changed = rows.filter((r) => r.mode !== r.default);
  const count = `${rows.length} configurable row${rows.length === 1 ? '' : 's'}`;
  if (!changed.length) return `${count}, all at their defaults`;
  return `${count}; changed: ${changed.map((r) => `${r.label} → ${MODE_LABELS[r.mode] || r.mode}`).join(', ')}`;
}

function sectionList(members) {
  const labels = members.map((f) => f.label);
  const shown = labels.slice(0, LIST_PREVIEW).join(' · ');
  return labels.length > LIST_PREVIEW ? `${shown} · and ${labels.length - LIST_PREVIEW} more` : shown;
}

export function SpawnOnResumeRow({ fetchImpl = globalThis.fetch, busy = false }) {
  const [data, setData] = useState(null);
  const [failed, setFailed] = useState(false);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const response = await fetchImpl('/api/relaunch-policy', { credentials: 'same-origin' });
        const body = await response.json().catch(() => null);
        if (!response.ok || !body) throw new Error(`HTTP ${response.status}`);
        if (live) setData(body);
      } catch { if (live) setFailed(true); }
    })();
    return () => { live = false; };
  }, [fetchImpl]);

  // The row is information only: when the policy cannot be read it says
  // nothing rather than guessing.
  if (failed || !data) return null;
  const rows = Array.isArray(data.rows) ? data.rows : [];
  const fields = Array.isArray(data.fields) ? data.fields : [];

  return html`<div class="cron-create-row" id="agent-spawn-on-resume-row">
    <span class="cron-create-label">On resume</span>
    <div class="cron-create-target spawn-on-resume">
      <div class="spawn-on-resume-head">
        <div>
          <div><strong>Global relaunch policy</strong>: what a resume or reincarnation of this agent re-reads, and what it keeps.</div>
          <div class="ui-hint" data-on-resume-summary>${onResumeSummary(rows)}</div>
        </div>
        <${Button} size="sm" disabled=${busy} aria-expanded=${open ? 'true' : 'false'} aria-controls="agent-spawn-on-resume-table"
          onClick=${() => setOpen((v) => !v)}>${open ? 'Hide' : 'Show'}<//>
      </div>
      ${open && html`<table id="agent-spawn-on-resume-table" class="spawn-on-resume-table ui-compact">
        <thead><tr><th scope="col">Field</th><th scope="col">On resume</th></tr></thead>
        <tbody>
          <tr class="rlp-section"><th colspan="2" scope="colgroup">Configurable</th></tr>
          ${rows.map((row) => html`<tr key=${row.field} data-on-resume-field=${row.field}>
            <td>${row.label}</td>
            <td>${row.settable === false ? html`<span class="ui-hint">No relaunch reads this row yet</span>`
              : html`<span class=${`ui-tag${row.mode !== row.default ? ' ui-tag-accent' : ''}`} title=${row.gap || undefined}>${MODE_LABELS[row.mode] || row.mode}</span> <span class="ui-hint">global</span>`}</td>
          </tr>`)}
          <tr class="rlp-section"><th colspan="2" scope="colgroup">Follows its layer</th></tr>
          ${READONLY_SECTIONS.filter((s) => s.kind !== 'once').map((section) => {
            const members = fields.filter((f) => !f.configurable && section.layers.includes(f.layer));
            if (!members.length) return null;
            return html`<tr key=${section.kind} data-on-resume-layer=${section.kind}>
              <td>${sectionList(members)}</td>
              <td><span class=${`ui-tag rlp-pill rlp-pill-${section.kind}`}>${section.pill}</span></td>
            </tr>`;
          })}
        </tbody>
      </table>
      <div class="ui-hint">Change the policy, and see every input with its reason, in Config › Launch.</div>`}
    </div>
  </div>`;
}
