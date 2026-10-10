import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

const BRIEF_MAX = 4096;

function errText(error) { return error?.message || String(error); }

function when(t) {
  return t && !String(t).startsWith('0001-') ? new Date(t).toLocaleString() : '';
}

// paidBy says whose model account a requested worker uses. Only a request
// that carries a model lease is requester-paid; a proxy selector naming some
// other node passes through to that node's gateway without a lease.
function paidBy(r) {
  if (r.model_lease) return `${r.from}'s model gateway (requester-paid: its usage is charged to them)`;
  if (String(r.credentials || '').startsWith('proxy:')) return `the model gateway ${String(r.credentials).slice(6)} (a third-party gateway, not this node's logins)`;
  return 'this node\'s harness logins (your provider account)';
}

function payTag(r) {
  if (r.model_lease) return 'requester-paid';
  if (String(r.credentials || '').startsWith('proxy:')) return `via ${String(r.credentials).slice(6)}`;
  return '';
}

// PlacementList renders why each candidate node was or was not chosen, like
// the CLI's placement readout.
function PlacementList({ placement }) {
  if (!placement?.candidates?.length) return null;
  return html`<table class="fa-table" id="fleet-spawn-placement"><tbody>${placement.candidates.map((c) => html`<tr key=${c.instance}>
    <td>${c.peer || c.instance}</td><td>${c.group || ''}</td>
    <td class=${c.instance === placement.selected ? '' : c.eligible ? 'muted' : 'fa-danger'}>${c.instance === placement.selected ? 'selected' : c.eligible ? 'eligible' : 'skipped'}${c.reason ? ` — ${c.reason}` : ''}${c.attempt ? ` (${c.attempt})` : ''}</td>
  </tr>`)}</tbody></table>`;
}

// ApproveDialog launches a peer's requested worker here, optionally
// overriding what the peer asked for. The override fields mirror
// `tclaude federation requests approve`.
function ApproveDialog({ req, actions, confirm, toast, onClose, onDone }) {
  const [o, setO] = useState({ name: req.name || '', profile: req.profile || '', cwd: '', harness: '', model: '' });
  const set = (k) => (e) => setO({ ...o, [k]: e.currentTarget.value });
  const go = () => {
    const overrides = Object.fromEntries(Object.entries(o).map(([k, v]) => [k, v.trim()]).filter(([k, v]) => v && v !== (req[k] || '')));
    confirm({
      title: `Start ${o.name.trim() || 'a worker'} for ${req.from}?`,
      body: `A new agent starts in your group ${req.group} on this node, as you, under that group's spawn rules, and works on ${req.from}'s brief. It runs with ${paidBy(req)}, can use what members of ${req.group} can plus any worker permissions in the node profile assigned to ${req.from}, and stays until you retire it. ${req.from} is told it was approved and can reach it like any member of ${req.group}.`,
      okLabel: 'Approve and start',
      busyLabel: 'Starting…',
      action: () => actions.approveSpawn(req.id, overrides),
    }).then((r) => { if (r) { toast(`Started ${r.label || r.agent_id} in ${r.group}`, false); onDone(); } })
      .catch((e) => toast(`Approve failed: ${errText(e)}`, true));
  };
  return html`<${Overlay} id="fleet-spawn-approve" labelledby="fleet-spawn-approve-title" onClose=${onClose}>
    <h3 id="fleet-spawn-approve-title">Approve spawn request #${req.id}</h3>
    <div class="fa-spawn-brief">${req.brief}</div>
    <div class="muted">From ${req.from} into group ${req.group}${req.role ? `, role ${req.role}` : ''}. Leave a field empty to keep what was requested or the group's default.</div>
    ${['name', 'profile', 'cwd', 'harness', 'model'].map((k) => html`<label class="fa-spawn-opt" key=${k}>${k}
      <input id=${`fleet-spawn-${k}`} value=${o[k]} onInput=${set(k)} placeholder=${k === 'cwd' ? 'group default' : k === 'name' || k === 'profile' ? (req[k] || 'default') : 'profile default'} /></label>`)}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${onClose}>Cancel</button>
      <button id="fleet-spawn-approve-go" type="button" class="primary" onClick=${go}>Approve…</button>
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation requests approve ${req.id} [--name …] [--profile …] [--cwd …] [--harness …] [--model …]</code></div>
  </${Overlay}>`;
}

// DenyDialog declines a request with an optional reason sent back.
function DenyDialog({ req, actions, confirm, toast, onClose, onDone }) {
  const [reason, setReason] = useState('');
  const go = () => confirm({
    title: `Deny spawn request #${req.id} from ${req.from}?`,
    body: `No worker starts. ${req.from} is told it was denied${reason.trim() ? ', with your reason' : ''}.`,
    okLabel: 'Deny', busyLabel: 'Denying…',
    action: () => actions.denySpawn(req.id, reason.trim()),
  }).then((x) => { if (x) { toast(`Denied #${req.id}`, false); onDone(); } }).catch((e) => toast(`Deny failed: ${errText(e)}`, true));
  return html`<${Overlay} id="fleet-spawn-deny" labelledby="fleet-spawn-deny-title" onClose=${onClose}>
    <h3 id="fleet-spawn-deny-title">Deny spawn request #${req.id}</h3>
    <div class="fa-spawn-brief">${req.brief}</div>
    <label class="fa-spawn-opt fa-spawn-top">Reason <textarea id="fleet-spawn-deny-reason" rows="2" maxlength="300" value=${reason} onInput=${(e) => setReason(e.currentTarget.value)} placeholder="optional, sent to the requester"></textarea></label>
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${onClose}>Cancel</button>
      <button id="fleet-spawn-deny-go" type="button" class="fa-danger" onClick=${go}>Deny…</button>
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation requests deny ${req.id} [--reason …]</code></div>
  </${Overlay}>`;
}

// SendDialog asks a peer (or automatic placement) for a worker in one of its
// exported groups; the peer's operator decides. Mirrors
// `tclaude federation spawn-request`.
function SendDialog({ view, pools, actions, confirm, toast, onClose, onDone }) {
  const [result, setResult] = useState(null);
  const peers = view.trusted || [];
  const [target, setTarget] = useState(peers[0] ? `peer:${peers[0].id}` : 'auto');
  const [f, setF] = useState({ group: '', brief: '', name: '', role: '', profile: '', credentials: '', require: '', prefer: '' });
  const set = (k) => (e) => setF({ ...f, [k]: e.currentTarget.value });
  const auto = !target.startsWith('peer:');
  const where = auto ? (target === 'auto' ? 'the best-placed trusted node' : `the best-placed node in pool ${target.slice(6)}`) : (peers.find((p) => `peer:${p.id}` === target)?.label || target.slice(5));
  const go = () => {
    const body = { brief: f.brief.trim(), ...(f.group.trim() ? { group: f.group.trim() } : {}) };
    for (const k of ['name', 'role', 'profile', 'credentials']) if (f[k].trim()) body[k] = f[k].trim();
    if (auto) { body.node = target; for (const k of ['require', 'prefer']) if (f[k].trim()) body[k] = f[k].trim(); } else body.peer = target.slice(5);
    confirm({
      title: `Ask ${where} for a worker?`,
      body: `${where}'s operator gets your brief${body.group ? ` for its group ${body.group}` : ''} and decides whether to start a worker there — unless that node granted you groups.members.spawn on the group, in which case the worker starts right away without its operator deciding. The worker runs on that node under its rules${/^proxy:.*@self$/.test(body.credentials || '') ? ', using your model gateway (requester-paid: its model usage is charged to you)' : body.credentials?.startsWith('proxy:') ? `, through the model gateway ${body.credentials.slice(6)}` : ''}. Requests wait up to 72 hours.`,
      okLabel: 'Send request',
      busyLabel: 'Sending…',
      action: () => actions.sendSpawnRequest(body),
    }).then((r) => {
      if (!r) return;
      const refused = r.state === 'refused';
      toast(refused ? `${r.to || where} refused the spawn request` : `Spawn request sent to ${r.to || where}${r.hub_connected === false ? ' (queued until the hub reconnects)' : ''}`, refused);
      if (r.placement || refused) setResult(r); else onDone();
    }).catch((e) => { toast(`Spawn request failed: ${errText(e)}`, true); if (e?.body?.placement) setResult({ error: errText(e), placement: e.body.placement }); });
  };
  return html`<${Overlay} id="fleet-spawn-send" labelledby="fleet-spawn-send-title" onClose=${onClose}>
    <h3 id="fleet-spawn-send-title">Request a worker on a peer</h3>
    <label class="fa-spawn-opt">Node
      <select id="fleet-spawn-target" value=${target} onChange=${(e) => setTarget(e.currentTarget.value)}>
        ${peers.map((p) => html`<option key=${p.id} value=${`peer:${p.id}`}>${p.label}</option>`)}
        <option value="auto">auto: best-placed trusted node</option>
        ${(pools || []).map((p) => html`<option key=${p.name} value=${`group:${p.name}`}>auto within pool ${p.name}</option>`)}
      </select></label>
    <label class="fa-spawn-opt">Group <input id="fleet-spawn-group" value=${f.group} onInput=${set('group')} placeholder=${auto ? 'optional with one authorized group' : 'a group the peer exports'} /></label>
    <label class="fa-spawn-opt fa-spawn-top">Brief <textarea id="fleet-spawn-brief" rows="4" maxlength=${BRIEF_MAX} value=${f.brief} onInput=${set('brief')}></textarea></label>
    <label class="fa-spawn-opt">Name <input value=${f.name} onInput=${set('name')} /></label>
    <label class="fa-spawn-opt">Role <input value=${f.role} onInput=${set('role')} /></label>
    <label class="fa-spawn-opt">Profile <input value=${f.profile} onInput=${set('profile')} placeholder="its default" /></label>
    <label class="fa-spawn-opt">Credentials <input value=${f.credentials} onInput=${set('credentials')} placeholder="local, or proxy:<gateway>@self (requester-paid)" /></label>
    ${auto && html`<label class="fa-spawn-opt">Require <input value=${f.require} onInput=${set('require')} placeholder="os=linux,harness=claude,label=gpu" /></label>
      <label class="fa-spawn-opt">Prefer <select value=${f.prefer} onChange=${set('prefer')}><option value="">least-loaded</option><option value="most-free-ram">most-free-ram</option></select></label>`}
    ${result && html`<div id="fleet-spawn-result">
      ${result.error ? html`<div class="fa-danger" role="alert">${result.error}</div>`
        : html`<div class=${result.state === 'refused' ? 'fa-danger' : ''}>Sent to ${result.to}: <b>${result.state}</b>${result.state === 'refused' ? ' — placement never retries elsewhere after a refusal' : result.state === 'sent' ? ' — not yet acknowledged; check the outbox' : ''}</div>`}
      <${PlacementList} placement=${result.placement} /></div>`}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${result && !result.error ? onDone : onClose}>${result ? 'Close' : 'Cancel'}</button>
      <button id="fleet-spawn-send-go" type="button" class="primary" disabled=${!f.brief.trim() || (!auto && !f.group.trim())} onClick=${go}>Send…</button>
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude federation spawn-request &lt;group&gt;@&lt;peer&gt; --brief …</code> or <code>--node auto|group:&lt;pool&gt;</code></div>
  </${Overlay}>`;
}

// SpawnRequestsPage lists the spawn requests peers sent to this node's
// groups (approve, deny, or abandon an unconfirmed launch), asks a peer for
// a worker, and shows the delivery state of what this node sent.
export function SpawnRequestsPage({ view, pools, actions, confirm, toast, now = Date.now() }) {
  const [rows, setRows] = useState(null);
  const [outbox, setOutbox] = useState(null);
  const [error, setError] = useState('');
  const [all, setAll] = useState(false);
  const [tick, setTick] = useState(0);
  const [dialog, setDialog] = useState(null);
  const reload = () => setTick((n) => n + 1);
  useEffect(() => {
    let off = false;
    actions.spawnRequests().then((r) => { if (!off) { setRows(r); setError(''); } }).catch((e) => { if (!off) setError(errText(e)); });
    actions.outbox().then((r) => { if (!off) setOutbox(r); }).catch((e) => { if (!off) setOutbox({ error: errText(e) }); });
    return () => { off = true; };
  }, [tick]);

  const abandon = (r) => confirm({
    title: `Abandon the unconfirmed launch of #${r.id}?`,
    body: `The launch never confirmed its worker started. Abandoning returns the request to pending so it can be approved or denied again — but the original worker may still appear late${r.result_agent ? ` (${r.result_agent})` : ''}. Check for it before approving again, or you may end up with two.`,
    okLabel: 'Abandon launch', busyLabel: 'Abandoning…',
    action: () => actions.abandonSpawn(r.id),
  }).then((x) => { if (x) { toast(x.warning || `#${r.id} is pending again`, false); reload(); } }).catch((e) => toast(`Abandon failed: ${errText(e)}`, true));

  const list = Array.isArray(rows) ? rows.filter((r) => all || r.status === 'pending' || r.status === 'launching') : [];
  const hidden = Array.isArray(rows) ? rows.length - list.length : 0;
  return html`<div class="fa-spawns">
    <div class="fa-run-settings">
      <span class="fa-k">Incoming</span>
      <span class="muted">${Array.isArray(rows) ? `${rows.filter((r) => r.status === 'pending').length} pending` : ''}</span>
      <label class="muted"><input type="checkbox" checked=${all} onChange=${(e) => setAll(e.currentTarget.checked)} /> show decided${hidden && !all ? ` (${hidden})` : ''}</label>
      <button type="button" class="fa-link" onClick=${reload}>refresh</button>
      <span class="grow"></span>
      <button id="fleet-spawn-new" type="button" onClick=${() => setDialog({ kind: 'send' })}>Request a worker on a peer…</button>
    </div>
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    ${!rows && !error ? html`<div class="empty">Loading…</div>` : list.length === 0 && !error ? html`<div class="empty">No ${all ? '' : 'open '}spawn requests from peers.</div>` : html`
    <table class="fa-table" id="fleet-spawn-requests">
      <thead><tr><th>#</th><th>From</th><th>Group</th><th>Worker</th><th>Brief</th><th>State</th><th>Expires</th><th></th></tr></thead>
      <tbody>${list.map((r) => html`<tr key=${r.id} data-spawn=${r.id}>
        <td>${r.id}</td>
        <td title=${r.instance}>${r.from}</td>
        <td>${r.group}</td>
        <td>${[r.name, r.role && `role ${r.role}`, r.profile && `profile ${r.profile}`].filter(Boolean).join(' · ') || html`<span class="muted">defaults</span>`}
          ${payTag(r) && html`<div class="muted">${payTag(r)}</div>`}</td>
        <td class="fa-spawn-brief-cell" title=${r.brief}>${r.brief}</td>
        <td class=${r.status === 'denied' || r.status === 'expired' ? 'fa-danger' : r.status === 'launching' ? 'fa-warn' : ''} title=${r.reason || ''}>${r.status}${r.result_agent ? html` <code title=${r.result_agent}>${r.result_agent.slice(0, 14)}</code>` : ''}${r.reason ? html` <span class="muted">— ${r.reason}</span>` : ''}</td>
        <td class="muted">${r.status === 'pending' ? (Date.parse(r.expires_at) < now ? 'expired' : when(r.expires_at)) : ''}</td>
        <td class="fa-acts">${r.status === 'pending' ? html`<button type="button" data-fa="approve" onClick=${() => setDialog({ kind: 'approve', req: r })}>Approve…</button> <button type="button" class="fa-danger" data-fa="deny" onClick=${() => setDialog({ kind: 'deny', req: r })}>Deny…</button>`
          : r.status === 'launching' ? html`<button type="button" class="fa-danger" data-fa="abandon" onClick=${() => abandon(r)}>Abandon launch…</button>` : ''}</td>
      </tr>`)}</tbody>
    </table>`}
    <h4 class="fa-h">Outbox <span class="muted">— delivery of spawn requests and operator mail this node sent; decisions arrive in Messages</span></h4>
    ${outbox?.error ? html`<div class="fa-danger">${outbox.error}</div>` : !outbox ? html`<div class="empty">Loading…</div>` : outbox.length === 0 ? html`<div class="empty">Nothing sent yet.</div>` : html`
    <table class="fa-table" id="fleet-outbox">
      <thead><tr><th>To</th><th>From</th><th>Subject</th><th>State</th><th>Attempts</th><th>Updated</th></tr></thead>
      <tbody>${outbox.map((o) => html`<tr key=${o.envelope_id} data-envelope=${o.envelope_id}>
        <td>${o.to}</td><td>${o.from}</td><td title=${o.preview || ''}>${o.subject || o.preview}</td>
        <td class=${o.last_error ? 'fa-danger' : ''} title=${o.last_error || ''}>${o.state}${o.last_error ? html` <span class="muted">— ${o.last_error}</span>` : ''}</td>
        <td>${o.attempts}</td><td class="muted">${when(o.updated_at)}</td>
      </tr>`)}</tbody>
    </table>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation requests [--all]</code>, <code>requests approve|deny|abandon ID</code>, <code>tclaude federation outbox</code>.</div>
    ${dialog?.kind === 'approve' && html`<${ApproveDialog} req=${dialog.req} actions=${actions} confirm=${confirm} toast=${toast} onClose=${() => setDialog(null)} onDone=${() => { setDialog(null); reload(); }} />`}
    ${dialog?.kind === 'deny' && html`<${DenyDialog} req=${dialog.req} actions=${actions} confirm=${confirm} toast=${toast} onClose=${() => setDialog(null)} onDone=${() => { setDialog(null); reload(); }} />`}
    ${dialog?.kind === 'send' && html`<${SendDialog} view=${view} pools=${pools} actions=${actions} confirm=${confirm} toast=${toast} onClose=${() => setDialog(null)} onDone=${() => { setDialog(null); reload(); }} />`}
  </div>`;
}
