import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { AUDIT_WINDOWS, auditSince, shortID } from './fleet-admin-model.js';

const html = htm.bind(h);

const PAGE = 200;
const MAX = 1000;

function errText(error) { return error?.message || String(error); }

function when(iso) {
  const t = new Date(iso);
  return Number.isFinite(t.getTime()) ? t.toLocaleString() : '—';
}

// AuditPage reads this node's federation activity log (metadata only: who,
// which peer, what kind, outcome), newest first, filtered by peer and time.
export function AuditPage({ view, actions, now }) {
  const [peer, setPeer] = useState('');
  const [windowMs, setWindowMs] = useState(86400e3);
  const [limit, setLimit] = useState(PAGE);
  const [rows, setRows] = useState(null);
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let off = false;
    setError('');
    actions.audit({ peer, since: auditSince(windowMs, now), limit })
      .then((r) => { if (!off) setRows(r); })
      .catch((e) => { if (!off) { setRows([]); setError(errText(e)); } });
    return () => { off = true; };
  }, [peer, windowMs, limit, tick]);

  const peers = [...view.trusted, ...view.waiting];
  const label = (id) => peers.find((r) => r.id === id)?.label || (id ? shortID(id) : '—');
  const list = rows || [];
  return html`<div class="fa-audit">
    <div class="fa-grant-form">
      <label><span class="fa-k">Peer</span><select id="fleet-audit-peer" value=${peer} onChange=${(e) => { setPeer(e.currentTarget.value); setLimit(PAGE); }}>
        <option value="">all peers</option>
        ${view.trusted.map((r) => html`<option key=${r.id} value=${r.id}>${r.label}</option>`)}
      </select></label>
      <label><span class="fa-k">When</span><select id="fleet-audit-window" value=${String(windowMs)} onChange=${(e) => { setWindowMs(Number(e.currentTarget.value)); setLimit(PAGE); }}>
        ${AUDIT_WINDOWS.map((w) => html`<option key=${w.ms} value=${String(w.ms)}>${w.label}</option>`)}
      </select></label>
      <button id="fleet-audit-refresh" type="button" onClick=${() => setTick((n) => n + 1)}>Refresh</button>
      <span class="muted">Metadata of remote activity only. CLI: <code>tclaude federation audit</code></span>
    </div>
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    ${rows == null ? html`<div class="empty">Loading…</div>` : list.length === 0 ? html`<div class="empty">No federation activity in this window.</div>` : html`
    <table class="fa-table" id="fleet-audit">
      <thead><tr><th>When</th><th></th><th>Peer</th><th>Kind</th><th>Actor</th><th>Target</th><th>Group</th><th>Outcome</th></tr></thead>
      <tbody>${list.map((r) => html`<tr key=${r.id} data-kind=${r.kind}>
        <td class="fa-nowrap">${when(r.at)}</td>
        <td title=${r.direction === 'in' ? 'incoming: the peer acted on this node' : r.direction === 'out' ? 'outgoing: this node acted on the peer' : r.direction}>${r.direction === 'in' ? '⇠' : r.direction === 'out' ? '⇢' : '·'}</td>
        <td>${label(r.peer)}</td>
        <td><code>${r.kind}</code></td>
        <td>${r.actor || html`<span class="muted">—</span>`}</td>
        <td class="fa-wrap">${r.target || ''}</td>
        <td>${r.group || ''}</td>
        <td class=${r.status >= 400 ? 'fa-danger' : ''}>${[r.state, r.status || ''].filter(Boolean).join(' ') || '—'}</td>
      </tr>`)}</tbody>
    </table>
    ${list.length >= limit && limit < MAX && html`<div class="fa-grant-form"><button id="fleet-audit-more" type="button" onClick=${() => setLimit(Math.min(MAX, limit + PAGE))}>Show more</button></div>`}
    ${list.length >= MAX && html`<div class="muted">Showing the newest ${MAX}; narrow by peer or time for older rows.</div>`}`}
  </div>`;
}
