import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

// PEER_MAIL_EVENT opens the peer mail dialog from anywhere in the Messages
// tab (detail: { peer?, subject?, to? }).
export const PEER_MAIL_EVENT = 'tclaude:peer-mail';
const PEER_GROUP = 'federation:';
// A forwarded away request carries its one-shot ticket in this line.
const TICKET_RE = /One-shot answer: tclaude federation answer ([0-9a-f]{16,64}\.[0-9a-f]{16,64}@[A-Za-z0-9_.-]{1,128}) --decision/;

function errText(error) { return error?.message || String(error); }

// peerOfMessage is the peer instance an inbound operator message came from
// (peer operator mail lands in the human inbox under federation:<instance>).
export function peerOfMessage(m) {
  if (!m || m.from_conv || !String(m.group || '').startsWith(PEER_GROUP)) return '';
  return String(m.group).slice(PEER_GROUP.length);
}

// awayTicket extracts the one-shot answer ticket from a cover request a peer
// operator forwarded while away.
export function awayTicket(m) {
  if (!peerOfMessage(m)) return '';
  return TICKET_RE.exec(String(m.body || ''))?.[1] || '';
}

export function replySubject(s) {
  const t = String(s || '').trim();
  return /^re:/i.test(t) ? t : `Re: ${t || '(no subject)'}`;
}

// createPeerMailActions wraps the local-only /api/federation mail, outbox
// and away routes (the cookie mirrors of tclaude federation send / notify /
// outbox / away / return / answer).
export function createPeerMailActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(method, path, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
    const res = await fetchImpl(`/api/federation/${path}`, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) { const e = new Error(data?.error || `HTTP ${res.status}`); e.status = res.status; throw e; }
    return data;
  }
  return Object.freeze({
    peers: async () => ((await call('GET', 'status'))?.peers || []).filter((p) => p.trusted)
      .map((p) => ({ id: p.instance_id, label: p.label || p.name || p.instance_id, online: !!p.online })),
    notify: ({ peer, subject = '', body }) => call('POST', 'notify', { peer, ...(subject ? { subject } : {}), body }),
    send: ({ to, role = '', subject = '', body }) => call('POST', 'send', { to, ...(role ? { role } : {}), ...(subject ? { subject } : {}), body }),
    outbox: async (limit = 30) => (await call('GET', `outbox?limit=${limit}`)) || [],
    away: () => call('GET', 'away'),
    setAway: ({ cover, until = '' }) => call('POST', 'away', { cover, ...(until ? { until } : {}) }),
    returnHome: () => call('POST', 'return', {}),
    answer: (ticket, decision) => call('POST', 'answer', { ticket, decision }),
  });
}

let shared = null;
function defaultActions() { return (shared ||= createPeerMailActions()); }

// PeerMailDialog writes to a peer's operator (notify) or to agents on a peer
// (send: agent@peer or group:<group>@peer, optionally by role), and shows
// what this node sent recently with its delivery state.
export function PeerMailDialog({ initial = {}, onClose, toast, actions = defaultActions() }) {
  const [peers, setPeers] = useState(null);
  const [kind, setKind] = useState(initial.to ? 'agents' : 'operator');
  const [peer, setPeer] = useState(initial.peer || '');
  const [to, setTo] = useState(initial.to || '');
  const [role, setRole] = useState('');
  const [subject, setSubject] = useState(initial.subject || '');
  const [body, setBody] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [outbox, setOutbox] = useState(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    actions.peers().then((p) => { setPeers(p); if (!peer && p[0]) setPeer(p[0].id); }).catch((e) => { setPeers([]); setError(errText(e)); });
  }, []);
  useEffect(() => { actions.outbox().then(setOutbox).catch((e) => setOutbox({ error: errText(e) })); }, [tick]);
  const label = (id) => peers?.find((p) => p.id === id)?.label || id;
  const ready = body.trim() && (kind === 'operator' ? peer : to.trim());
  const send = () => {
    setBusy(true); setError('');
    const p = kind === 'operator'
      ? actions.notify({ peer, subject: subject.trim(), body })
      : actions.send({ to: to.trim(), role: role.trim(), subject: subject.trim(), body });
    p.then((r) => { toast?.(`Sent to ${r?.to || (kind === 'operator' ? `${label(peer)}'s operator` : to)}${r?.hub_connected === false ? ' (queued until the hub reconnects)' : ''}`, false); setBody(''); setTick((n) => n + 1); })
      .catch((e) => setError(errText(e)))
      .finally(() => setBusy(false));
  };
  return html`<${Overlay} id="peer-mail-modal" labelledby="peer-mail-title" onClose=${onClose} blocked=${busy}>
    <h3 id="peer-mail-title">Message a peer</h3>
    <div class="peer-mail-kind" role="radiogroup">
      <label><input type="radio" name="peer-mail-kind" value="operator" checked=${kind === 'operator'} onChange=${() => setKind('operator')} /> its operator</label>
      <label><input type="radio" name="peer-mail-kind" value="agents" checked=${kind === 'agents'} onChange=${() => setKind('agents')} /> agents on it</label>
    </div>
    ${kind === 'operator'
      ? html`<label class="peer-mail-opt">Peer <select id="peer-mail-peer" value=${peer} onChange=${(e) => setPeer(e.currentTarget.value)}>
          ${(peers || []).map((p) => html`<option key=${p.id} value=${p.id}>${p.label}${p.online ? '' : ' (offline: queued)'}</option>`)}</select></label>`
      : html`<label class="peer-mail-opt">To <input id="peer-mail-to" value=${to} onInput=${(e) => setTo(e.currentTarget.value)} placeholder="agent@peer or group:<group>@peer" /></label>
        <label class="peer-mail-opt">Role <input id="peer-mail-role" value=${role} onInput=${(e) => setRole(e.currentTarget.value)} placeholder="only with group:…, e.g. reviewer" /></label>`}
    <label class="peer-mail-opt">Subject <input id="peer-mail-subject" value=${subject} onInput=${(e) => setSubject(e.currentTarget.value)} /></label>
    <label class="peer-mail-opt peer-mail-top">Body <textarea id="peer-mail-body" rows="6" value=${body} onInput=${(e) => setBody(e.currentTarget.value)}></textarea></label>
    <div class="muted">${kind === 'operator'
      ? 'Lands in that operator\'s Messages, from you as this node\'s operator.'
      : 'Delivered as mail from you, this node\'s operator, if the peer lets you message that agent or group.'} It waits in the outbox while the peer is offline.</div>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${onClose}>Close</button>
      <button id="peer-mail-send" type="button" class="primary" disabled=${busy || !ready} onClick=${send}>${busy ? 'Sending…' : 'Send'}</button>
    </div>
    <h4 class="peer-mail-h">Outbox <button type="button" class="fa-link" onClick=${() => setTick((n) => n + 1)}>refresh</button></h4>
    ${outbox?.error ? html`<div class="fa-danger">${outbox.error}</div>` : !outbox ? html`<div class="muted">Loading…</div>` : outbox.length === 0 ? html`<div class="muted">Nothing sent yet.</div>` : html`
      <table class="fa-table" id="peer-mail-outbox"><tbody>${outbox.slice(0, 10).map((o) => html`<tr key=${o.envelope_id}>
        <td>${o.to}</td><td title=${o.preview || ''}>${o.subject || o.preview}</td>
        <td class=${o.last_error ? 'fa-danger' : ''} title=${o.last_error || ''}>${o.state}${o.attempts > 1 ? ` · ${o.attempts} tries` : ''}</td>
        <td class="muted">${o.updated_at ? new Date(o.updated_at).toLocaleString() : ''}</td></tr>`)}</tbody></table>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation notify --peer … | send ${'<to>'}</code>, <code>tclaude federation outbox</code></div>
  </${Overlay}>`;
}

// PeerMailHost mounts the dialog when PEER_MAIL_EVENT fires.
export function PeerMailHost({ doc = globalThis.document, toast, actions }) {
  const [open, setOpen] = useState(null);
  useEffect(() => {
    const on = (e) => setOpen({ ...(e.detail || {}) });
    doc.addEventListener(PEER_MAIL_EVENT, on);
    return () => doc.removeEventListener(PEER_MAIL_EVENT, on);
  }, [doc]);
  if (!open) return null;
  return html`<${PeerMailDialog} initial=${open} toast=${toast} actions=${actions} onClose=${() => setOpen(null)} />`;
}

// AwayAnswer answers a cover request a peer operator forwarded while away:
// once, approve or deny. Always/extend are not available to a cover.
export function AwayAnswer({ ticket, peerLabel, confirm, toast, actions = defaultActions() }) {
  const go = (decision) => confirm({
    title: `${decision === 'approve' ? 'Approve' : 'Deny'} this request for ${peerLabel}'s operator?`,
    body: decision === 'approve'
      ? `You answer once, on ${peerLabel}'s operator's behalf: the waiting agent on ${peerLabel} is allowed this one action. It grants nothing lasting; ${peerLabel} applies it only if you are still its cover and the request is still pending.`
      : `You answer once, on ${peerLabel}'s operator's behalf: the waiting agent on ${peerLabel} is refused this action.`,
    okLabel: decision === 'approve' ? 'Approve once' : 'Deny',
    busyLabel: 'Sending…',
    action: () => actions.answer(ticket, decision),
  }).then((r) => { if (r) toast?.(`Answer sent to ${peerLabel} (${r.state || 'queued'})`, false); })
    .catch((e) => toast?.(`Answer failed: ${errText(e)}`, true));
  return html`<span class="peer-away-answer">
    <button type="button" class="access-btn deny" data-act="away-deny" onClick=${() => go('deny')}>Deny</button>
    <button type="button" class="access-btn approve" data-act="away-approve" onClick=${() => go('approve')}>Approve once</button>
  </span>`;
}

function localInput(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

// AwayControl shows and sets away cover: while away, access requests your
// agents raise that you do not answer are forwarded to one trusted peer's
// operator, who may answer each once. Peer access requests are never
// forwarded.
export function AwayControl({ confirm, toast, actions = defaultActions(), now = () => Date.now() }) {
  const [away, setAway] = useState(undefined);
  const [peers, setPeers] = useState([]);
  const [editing, setEditing] = useState(false);
  const [cover, setCover] = useState('');
  const [until, setUntil] = useState('');
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let off = false;
    actions.away().then((r) => { if (!off) setAway(r?.away || null); }).catch((e) => { if (!off) setAway({ error: errText(e) }); });
    actions.peers().then((p) => { if (!off) { setPeers(p); setCover((c) => c || p[0]?.id || ''); } }).catch(() => {});
    return () => { off = true; };
  }, [tick]);
  const label = (id) => peers.find((p) => p.id === id)?.label || id;
  const set = () => {
    const untilISO = until ? new Date(until).toISOString() : '';
    confirm({
      title: `Hand your approvals to ${label(cover)} while away?`,
      body: `Until you return${until ? ` or ${new Date(until).toLocaleString()}` : ''}, access requests your agents raise that you do not answer are forwarded to ${label(cover)}'s operator, who may approve or deny each one once (it needs your approvals.answer grant; to answer harness prompts it also needs sessions.read and sessions.attach). Peer operators' access requests are never forwarded; only you decide those.`,
      okLabel: 'Go away',
      busyLabel: 'Saving…',
      action: () => actions.setAway({ cover, until: untilISO }),
    }).then((r) => {
      if (!r) return;
      setEditing(false); setTick((n) => n + 1);
      toast?.(`Away: ${label(cover)} covers${r.warnings?.length ? ` — ${r.warnings.join('; ')}` : ''}`, !!r.warnings?.length);
    }).catch((e) => toast?.(`Away failed: ${errText(e)}`, true));
  };
  const back = () => confirm({
    title: 'Return?',
    body: `Requests stop forwarding to ${label(away.cover)}, and answers it has not sent yet are no longer accepted.`,
    okLabel: 'Return',
    busyLabel: 'Saving…',
    action: () => actions.returnHome(),
  }).then((r) => { if (r) { setTick((n) => n + 1); toast?.('Returned: you answer your own requests again', false); } })
    .catch((e) => toast?.(`Return failed: ${errText(e)}`, true));
  if (away === undefined) return html`<span class="muted">…</span>`;
  if (away?.error) return html`<span class="muted" title=${away.error}>unavailable</span>`;
  if (away) {
    const until = away.until && !String(away.until).startsWith('0001-') ? new Date(away.until) : null;
    const short = until && (until.toDateString() === new Date(now()).toDateString() ? until.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : until.toLocaleDateString());
    return html`<span id="fleet-away" title=${`${label(away.cover)}'s operator answers your agents' access requests${until ? ` until ${until.toLocaleString()}` : ''}`}><span class="fa-warn">away</span>: ${label(away.cover)}${short ? ` → ${short}` : ''} <button id="fleet-away-return" type="button" class="fa-link" onClick=${back}>return…</button></span>`;
  }
  if (!editing) {
    return html`<span id="fleet-away"><button id="fleet-away-open" type="button" class="fa-link" disabled=${!peers.length}
      title=${peers.length ? 'Have a trusted peer\'s operator answer your agents\' access requests while you are away' : 'Trust a peer first'} onClick=${() => setEditing(true)}>away…</button></span>`;
  }
  return html`<span id="fleet-away" class="fa-away-edit">
    <select id="fleet-away-cover" aria-label="Covering peer" value=${cover} onChange=${(e) => setCover(e.currentTarget.value)}>
      ${peers.map((p) => html`<option key=${p.id} value=${p.id}>${p.label}</option>`)}</select>
    <input id="fleet-away-until" type="datetime-local" aria-label="Until (optional)" min=${localInput(new Date(now()))} value=${until} onInput=${(e) => setUntil(e.currentTarget.value)} />
    <button id="fleet-away-go" type="button" disabled=${!cover} onClick=${set}>Go away…</button>
    <button type="button" class="fa-link" onClick=${() => setEditing(false)}>cancel</button>
  </span>`;
}
