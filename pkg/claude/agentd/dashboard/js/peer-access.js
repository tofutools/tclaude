import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { slugInfo } from './fleet-admin-model.js';

const html = htm.bind(h);

const STATUS_POLL_MS = 3000;
const REASON_MAX = 4096;

// TTL_CHOICES are the grant lifetimes an access request can ask for (or an
// approval can set); 0 is permanent. The daemon accepts 0..30 days.
export const TTL_CHOICES = Object.freeze([
  { s: 3600, label: '1 hour' },
  { s: 8 * 3600, label: '8 hours' },
  { s: 86400, label: '1 day' },
  { s: 7 * 86400, label: '7 days' },
  { s: 30 * 86400, label: '30 days' },
  { s: 0, label: 'permanent' },
]);

export function ttlText(s) {
  if (s == null) return '';
  const c = TTL_CHOICES.find((x) => x.s === Number(s));
  if (c) return c.label;
  if (s % 86400 === 0) return `${s / 86400} days`;
  if (s % 3600 === 0) return `${s / 3600} hours`;
  return `${Math.round(s / 60)} min`;
}

// requestScope says whether a permission is granted per group ('group': a
// group may be named or left to every shared group; 'scoped': a group is
// required) or node-wide ('node').
export function requestScope(perm) {
  return slugInfo(perm)?.kind || 'node';
}

function errText(error) { return error?.message || String(error); }

// createPeerAccessActions sends the requester side. On a peer view
// remote-node.js forwards /api/* to /api/peer/{id}/*, so these reach the
// peer's /api/peer-access-requests routes.
export function createPeerAccessActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(method, url, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(url, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) { const e = new Error(data?.error || `HTTP ${res.status}`); e.status = res.status; e.code = data?.code || ''; throw e; }
    return data;
  }
  return Object.freeze({
    request: ({ permission, groupID = 0, reason = '', ttl = 3600 }) => call('POST', '/api/peer-access-requests', {
      permission, ...(groupID ? { group_id: groupID } : {}), ...(reason ? { reason } : {}), grant_ttl_seconds: ttl,
    }),
    status: (id) => call('GET', `/api/peer-access-requests/${encodeURIComponent(id)}`),
  });
}

let sharedActions = null;
function defaultActions() { return (sharedActions ||= createPeerAccessActions()); }

const ACTIVE = new Set(['pending']);

// RequestAccessDialog asks the peer's operator for one permission this
// operator lacks on that node. The peer's operator decides (only them, never
// an away cover); on approval the grant applies and the feature appears on
// the next refresh.
export function RequestAccessDialog({ node, perm, groups = [], onClose, actions = defaultActions(), timers = globalThis }) {
  const scope = requestScope(perm);
  const [groupID, setGroupID] = useState(scope === 'scoped' && groups[0] ? String(groups[0].id) : '');
  const [reason, setReason] = useState('');
  const [ttl, setTtl] = useState('3600');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [request, setRequest] = useState(null);
  useEffect(() => {
    if (!request || !ACTIVE.has(request.status)) return undefined;
    let off = false;
    const t = timers.setTimeout(() => {
      actions.status(request.id).then((r) => { if (!off) setRequest(r); }).catch(() => { if (!off) setRequest({ ...request }); });
    }, STATUS_POLL_MS);
    return () => { off = true; timers.clearTimeout(t); };
  }, [request]);
  const send = () => {
    setBusy(true); setError('');
    actions.request({ permission: perm, groupID: Number(groupID) || 0, reason: reason.trim(), ttl: Number(ttl) })
      .then(setRequest)
      .catch((e) => setError(e?.status === 403 ? `${node} only takes requests from peers it already shares something with (${errText(e)})` : errText(e)))
      .finally(() => setBusy(false));
  };
  const group = groups.find((g) => String(g.id) === String(groupID));
  return html`<${Overlay} id="peer-access-modal" labelledby="peer-access-title" onClose=${onClose} blocked=${busy}>
    <h3 id="peer-access-title">Request <code>${perm}</code> from ${node}</h3>
    ${request ? html`<div id="peer-access-status" class=${request.status === 'approved' ? '' : request.status === 'pending' ? 'muted' : 'fa-danger'}>
        Request ${request.id}: <b>${request.status}</b>
        ${request.status === 'pending' && html`<div class="muted">${node}'s operator decides; this updates on its own.</div>`}
        ${request.status === 'approved' && html`<div>Granted${request.grant_ttl_seconds ? ` for ${ttlText(request.grant_ttl_seconds)}` : ' permanently'}${request.grant_group_id ? ` in group #${request.grant_group_id}` : ''}. Retry the action; the view picks it up on its next refresh.</div>`}
      </div>` : html`
      <div class="muted">${slugInfo(perm)?.what || ''}</div>
      ${scope !== 'node' && html`<label class="peer-access-opt">Group
        <select id="peer-access-group" value=${groupID} onChange=${(e) => setGroupID(e.currentTarget.value)}>
          ${scope === 'group' && html`<option value="">any group ${node} shares with me (it may narrow)</option>`}
          ${groups.map((g) => html`<option key=${g.id} value=${String(g.id)}>${g.name}</option>`)}
        </select></label>`}
      <label class="peer-access-opt">For
        <select id="peer-access-ttl" value=${ttl} onChange=${(e) => setTtl(e.currentTarget.value)}>
          ${TTL_CHOICES.map((c) => html`<option key=${c.s} value=${String(c.s)}>${c.label}</option>`)}
        </select></label>
      <label class="peer-access-opt peer-access-reason">Reason <textarea id="peer-access-reason" rows="3" maxlength=${REASON_MAX} value=${reason} onInput=${(e) => setReason(e.currentTarget.value)}></textarea></label>
      <div class="muted">${node}'s operator sees the permission${group ? `, group ${group.name}` : ''}, how long, and your reason, and may approve it for less. Requests time out after 5 minutes without a decision.</div>
      ${error && html`<div class="fa-danger" role="alert">${error}</div>`}`}
    <div class="modal-buttons">
      <span class="spacer"></span>
      <button type="button" onClick=${onClose}>${request ? 'Close' : 'Cancel'}</button>
      ${!request && html`<button id="peer-access-submit" type="button" class="primary" disabled=${busy || (scope === 'scoped' && !groupID)} onClick=${send}>Send request</button>`}
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation access request --node … --permission ${perm}</code></div>
  </${Overlay}>`;
}
