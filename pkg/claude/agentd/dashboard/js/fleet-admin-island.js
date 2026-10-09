import { h, render } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { GrantsPage } from './fleet-admin-grants.js';
import { InvitesPage } from './fleet-admin-invites.js';
import { ProfilesPage } from './fleet-admin-profiles.js';
import { AuditPage } from './fleet-admin-audit.js';
import { HarnessesPage } from './fleet-admin-harnesses.js';
import { createHarnessActions } from './fleet-harness-actions.js';
import { dashboardState } from './snapshot-store.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { fmtAge, nodeHref, pollDelay, remoteNodeID } from './skynet-model.js';
import { LABEL_RE, UNRESTRICTED_CONSEQUENCE, adminView, shortFingerprint, shortID } from './fleet-admin-model.js';

const html = htm.bind(h);

// ADMIN_POLL_MS paces the status re-read while the admin view is on screen,
// so a peer coming online or a CLI-side change shows without a reload.
const ADMIN_POLL_MS = 10000;

// SUB_PAGES are the admin sections.
const SUB_PAGES = Object.freeze([
  { id: 'peers', label: 'Peers' },
  { id: 'harnesses', label: 'Harnesses' },
  { id: 'invites', label: 'Invites & joining' },
  { id: 'grants', label: 'Peer grants' },
  { id: 'profiles', label: 'Profiles & pools' },
  { id: 'audit', label: 'Audit' },
]);

function defaultSwitchHome() {
  globalThis.location.assign(nodeHref('', { pathname: '/fleet-admin', search: globalThis.location.search }));
}

function defaultCopy(text) {
  const clipboard = globalThis.navigator?.clipboard;
  if (!clipboard?.writeText) return Promise.reject(new Error('clipboard unavailable'));
  return clipboard.writeText(text);
}

function errText(error) { return error?.message || String(error); }

let harnessActionsSingleton = null;
function defaultHarnessActions() {
  harnessActionsSingleton ||= createHarnessActions();
  return harnessActionsSingleton;
}

// localGroups names this node's active groups, the scopes a grant can take.
function localGroups(snap) {
  return (snap?.groups || []).filter((g) => g?.name && !g.archived).map((g) => g.name).sort();
}

// setLevel changes a trusted peer's level. peers/trust by instance ID would
// also re-trust a peer untrusted elsewhere since the last poll, so the level
// change re-reads the status first and refuses for a peer no longer trusted.
async function setLevel(actions, opts) {
  const st = await actions.status();
  if (!(st?.peers || []).some((p) => p?.trusted && p.instance_id === opts.instance)) {
    throw new Error('this peer is no longer trusted (untrusted elsewhere) — trust it again from the waiting list');
  }
  return actions.trust(opts);
}

// suggestLabel turns a peer's self-reported name into a valid local label.
export function suggestLabel(row) {
  const s = String(row?.name || '').toLowerCase().replace(/[^a-z0-9_.-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 32);
  return LABEL_RE.test(s) ? s : '';
}

function ago(iso, now) {
  const t = Date.parse(iso || '');
  return Number.isFinite(t) ? `${fmtAge(Math.max(0, now - t))} ago` : '—';
}

function Fingerprint({ value, copy, toast }) {
  if (!value) return html`<span class="muted">—</span>`;
  const onCopy = () => copy(value).then(() => toast('Fingerprint copied', false)).catch(() => toast(`Fingerprint: ${value}`, false));
  return html`<span class="fa-fp"><code>${value}</code> <button type="button" class="fa-link" onClick=${onCopy} title="Copy the fingerprint">copy</button></span>`;
}

function Identity({ self, actions, confirm, toast, copy, reload }) {
  const disconnect = () => confirm({
    title: 'Disconnect from the hub?',
    body: `${self.name} stops talking to the hub at ${self.hubURL || 'its configured URL'}. Until you reconnect, `
      + 'no peer can reach this node and this node cannot reach any peer: remote views, mail, attach and '
      + 'remote jobs stop in both directions. Trusted peers and their grants are kept.',
    okLabel: 'Disconnect',
    busyLabel: 'Disconnecting…',
    action: () => actions.setHubEnabled(false),
  }).then((ok) => { if (ok) { toast('Disconnected from the hub', false); reload(); } })
    .catch((error) => toast(`Disconnect failed: ${errText(error)}`, true));
  const reconnect = () => actions.setHubEnabled(true)
    .then(() => { toast('Reconnecting to the hub…', false); reload(); })
    .catch((error) => toast(`Reconnect failed: ${errText(error)}`, true));
  const hubClass = self.hubState === 'connected' ? 'ok' : self.enabled ? 'warn' : 'off';
  return html`<div class="fa-identity">
    <span><span class="fa-k">This node</span> <b>${self.name}</b></span>
    <span><span class="fa-k">Instance</span> <code>${self.id}</code></span>
    <span><span class="fa-k">Fingerprint</span> <${Fingerprint} value=${self.fingerprint} copy=${copy} toast=${toast} /></span>
    <span><span class="fa-k">Hub</span> ${self.hubURL ? html`<code>${self.hubURL}</code> ` : ''}<span class=${`fa-hub ${hubClass}`} title=${self.hubError || ''}>${self.hubState}</span></span>
    ${self.enabled
      ? html`<button type="button" class="fa-danger" onClick=${disconnect}>Disconnect</button>`
      : self.hubURL ? html`<button type="button" onClick=${reconnect}>Reconnect</button>` : ''}
  </div>`;
}

// ConsequenceNote spells out what unrestricted trust implies; every dialog
// that grants it repeats it.
function ConsequenceNote() {
  return html`<div class="fa-consequence" role="note"><b>Unrestricted.</b> ${UNRESTRICTED_CONSEQUENCE}</div>`;
}

// TrustDialog trusts a hub-visible instance. It previews first, so the
// operator sees the exact fingerprint the daemon will pin and any default
// profile that would apply, and must confirm the fingerprint was compared out
// of band. Unrestricted trust also sends that fingerprint as the daemon's
// confirmation.
export function TrustDialog({ row, actions, onClose, onDone }) {
  const [label, setLabel] = useState(suggestLabel(row));
  const [noDefault, setNoDefault] = useState(false);
  const [level, setLevel] = useState('restricted');
  const [preview, setPreview] = useState(null);
  const [checked, setChecked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  // seq drops a slower, superseded preview (the profile opt-out toggled).
  const seq = useRef(0);
  const loadPreview = useCallback(() => {
    const n = ++seq.current;
    setPreview(null); setError('');
    return actions.previewTrust({ instance: row.id, noDefaultProfile: noDefault })
      .then((p) => { if (n === seq.current) setPreview(p); })
      .catch((e) => { if (n === seq.current) setError(errText(e)); });
  }, [row.id, noDefault]);
  useEffect(() => { loadPreview(); }, [loadPreview]);
  const profile = preview?.profile || null;
  const effective = profile ? preview.level : level;
  const labelOK = !label || LABEL_RE.test(label);
  const submit = async () => {
    if (!preview || !checked || !labelOK || busy) return;
    setBusy(true); setError('');
    try {
      await actions.trust({
        instance: row.id, label,
        level: profile ? '' : level,
        noDefaultProfile: noDefault,
        previewToken: preview.plan?.preview_token || '',
        confirmFingerprint: effective === 'unrestricted' ? preview.fingerprint : '',
      });
      onDone(`Trusted ${label || row.label} (${effective})`);
    } catch (e) {
      setError(errText(e));
      if (['stale_preview', 'profile', 'preview_required'].includes(e?.code)) loadPreview();
    } finally { setBusy(false); }
  };
  const plan = preview?.plan;
  return html`<${Overlay} id="fleet-trust-modal" labelledby="fleet-trust-title" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-trust-title">Trust ${row.label}?</h3>
    <div class="fa-dl">
      <span class="fa-k">Instance</span><code class="fa-wrap">${row.id}</code>
      <span class="fa-k">Fingerprint</span><code class="fa-wrap fa-fp-full">${preview?.fingerprint || row.fingerprint || '…'}</code>
      ${row.name && html`<span class="fa-k">Reports name</span><span>${row.name}</span>`}
    </div>
    <label class="cron-create-row"><span class="cron-create-label">Local label</span>
      <input id="fleet-trust-label" type="text" value=${label} placeholder="optional, a-z 0-9 - _ ." autocomplete="off" spellcheck="false"
        onInput=${(e) => setLabel(e.currentTarget.value)} />
    </label>
    ${!labelOK && html`<div class="cron-create-error">Labels are 1–32 characters of a-z, 0-9, - _ .</div>`}
    ${profile
      ? html`<div class="fa-profile">Default profile <b>${profile.name}</b> applies: level <b>${preview.level}</b>${plan ? `, ${plan.changes?.length || 0} change(s), ${plan.security_changes || 0} security-relevant` : ''}.
          <label><input type="checkbox" checked=${noDefault} onChange=${(e) => setNoDefault(e.currentTarget.checked)} /> Don't apply the default profile</label></div>`
      : html`<div class="fa-levels" role="radiogroup" aria-label="Trust level">
          ${noDefault && html`<label><input type="checkbox" checked=${noDefault} onChange=${(e) => setNoDefault(e.currentTarget.checked)} /> Don't apply the default profile</label>`}
          <label><input type="radio" name="fa-level" checked=${level === 'restricted'} onChange=${() => setLevel('restricted')} /> <b>Restricted</b> — the peer gets only the grants you give it</label>
          <label><input type="radio" name="fa-level" checked=${level === 'unrestricted'} onChange=${() => setLevel('unrestricted')} /> <b>Unrestricted</b></label>
        </div>`}
    ${plan?.conflicts?.length > 0 && html`<div class="cron-create-error">Profile conflicts: ${plan.conflicts.join('; ')}</div>`}
    ${effective === 'unrestricted' && html`<${ConsequenceNote} />`}
    <label class="fa-ack"><input id="fleet-trust-ack" type="checkbox" checked=${checked} onChange=${(e) => setChecked(e.currentTarget.checked)} />
      <span>I compared this fingerprint with ${row.label}'s operator over another channel (e.g. <code>tclaude federation identity</code> on their node)</span></label>
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      <span class="spacer"></span>
      <button id="fleet-trust-submit" type="button" class=${effective === 'unrestricted' ? 'danger' : 'primary'} disabled=${busy || !preview || !checked || !labelOK} onClick=${submit}>
        ${busy ? 'Trusting…' : effective === 'unrestricted' ? 'Trust unrestricted' : 'Trust'}
      </button>
    </div>
  </${Overlay}>`;
}

// UnrestrictDialog widens a trusted peer to unrestricted: the consequence and
// the full fingerprint are shown, and the fingerprint is sent as confirmation.
export function UnrestrictDialog({ row, actions, onClose, onDone }) {
  const [checked, setChecked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const submit = async () => {
    if (!checked || busy) return;
    setBusy(true); setError('');
    try {
      await setLevel(actions, { instance: row.id, level: 'unrestricted', confirmFingerprint: row.fingerprint });
      onDone(`${row.label} is now unrestricted`);
    } catch (e) { setError(errText(e)); } finally { setBusy(false); }
  };
  return html`<${Overlay} id="fleet-level-modal" labelledby="fleet-level-title" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-level-title">Make ${row.label} unrestricted?</h3>
    <div class="fa-dl">
      <span class="fa-k">Instance</span><code class="fa-wrap">${row.id}</code>
      <span class="fa-k">Fingerprint</span><code class="fa-wrap fa-fp-full">${row.fingerprint || '—'}</code>
    </div>
    <${ConsequenceNote} />
    <label class="fa-ack"><input id="fleet-level-ack" type="checkbox" checked=${checked} onChange=${(e) => setChecked(e.currentTarget.checked)} />
      <span>I understand, and this is ${row.label}'s fingerprint</span></label>
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      <span class="spacer"></span>
      <button id="fleet-level-submit" type="button" class="danger" disabled=${busy || !checked || !row.fingerprint} onClick=${submit}>${busy ? 'Saving…' : 'Make unrestricted'}</button>
    </div>
  </${Overlay}>`;
}

function grantsCell(g) {
  if (!g.total) return html`<span class="muted">none</span>`;
  const parts = [g.nodeWide && `${g.nodeWide} node-wide`, g.scoped && `${g.scoped} group`].filter(Boolean).join(' · ');
  return html`<span title=${parts}>${g.total} <span class="muted">(${parts})</span></span>`;
}

function PeersPage({ view, now, onTrust, onUnrestrict, onRestrict, onUntrust, onGrants }) {
  return html`<div class="fa-peers">
    <h4>Trusted peers <span class="muted">${view.trusted.length}</span></h4>
    ${view.trusted.length === 0
      ? html`<div class="empty">No trusted peers yet. Trust an instance below once its operator has read you its fingerprint.</div>`
      : html`<table class="fa-table" id="fleet-trusted">
        <thead><tr><th>Peer</th><th>Instance</th><th>Fingerprint</th><th>Level</th><th>Seen</th><th>Grants</th><th>Pools</th><th></th></tr></thead>
        <tbody>${view.trusted.map((r) => html`<tr key=${r.id} data-peer=${r.id}>
          <td><span class=${`fa-dot${r.online ? ' on' : ''}`} title=${r.online ? 'online' : 'offline'}></span> <b>${r.label}</b>${r.name && r.name !== r.label ? html` <span class="muted">${r.name}</span>` : ''}</td>
          <td><code title=${r.id}>${shortID(r.id)}</code></td>
          <td><code title=${r.fingerprint}>${shortFingerprint(r.fingerprint)}</code></td>
          <td><span class=${`fa-level ${r.level}`}>${r.level}</span></td>
          <td>${r.online ? 'online' : ago(r.lastSeen, now)}</td>
          <td>${grantsCell(r.grants)}</td>
          <td>${r.pools.length ? r.pools.join(', ') : html`<span class="muted">—</span>`}</td>
          <td class="fa-acts">
            <button type="button" data-fa="grants" onClick=${() => onGrants(r)}>Grants…</button>
            ${r.level === 'unrestricted'
              ? html`<button type="button" data-fa="restrict" onClick=${() => onRestrict(r)}>Restrict…</button>`
              : html`<button type="button" data-fa="unrestrict" onClick=${() => onUnrestrict(r)}>Make unrestricted…</button>`}
            <button type="button" class="fa-danger" data-fa="untrust" onClick=${() => onUntrust(r)}>Untrust…</button>
          </td>
        </tr>`)}</tbody>
      </table>`}
    <h4>Waiting to be trusted <span class="muted">${view.waiting.length}</span></h4>
    ${view.waiting.length === 0
      ? html`<div class="empty">No other instances visible on the hub.</div>`
      : html`<table class="fa-table" id="fleet-waiting">
        <thead><tr><th>Reports name</th><th>Instance</th><th>Fingerprint</th><th>Seen</th><th></th></tr></thead>
        <tbody>${view.waiting.map((r) => html`<tr key=${r.id} data-peer=${r.id}>
          <td><span class=${`fa-dot${r.online ? ' on' : ''}`}></span> ${r.name || html`<span class="muted">unnamed</span>`}</td>
          <td><code title=${r.id}>${shortID(r.id)}</code></td>
          <td><code title=${r.fingerprint}>${shortFingerprint(r.fingerprint)}</code></td>
          <td>${r.online ? 'online' : ago(r.lastSeen, now)}</td>
          <td class="fa-acts"><button type="button" class="primary" data-fa="trust" onClick=${() => onTrust(r)}>Trust…</button></td>
        </tr>`)}</tbody>
      </table>`}
  </div>`;
}

// FleetAdmin is the top-level "Fleet" view: this node's federation identity,
// hub connection and the peers it trusts. Its data is this node's own (never
// a peer's), so a peer view hands the page back to this node.
export function FleetAdmin({
  state, actions, harnessActions = defaultHarnessActions(), confirm = shellConfirm, toast = shellToast, copy = defaultCopy, snapshot = dashboardState.snapshot,
  timers = globalThis, now = () => Date.now(), remote = remoteNodeID(), switchHome = defaultSwitchHome,
}) {
  const active = state.view.value.adminActive;
  const [status, setStatus] = useState(null);
  const [pools, setPools] = useState([]);
  const [failure, setFailure] = useState('');
  const [page, setPage] = useState('peers');
  const [dialog, setDialog] = useState(null);
  const [grantTarget, setGrantTarget] = useState('');
  const [tick, setTick] = useState(0);
  const reload = () => setTick((n) => n + 1);

  useEffect(() => { if (active && remote) switchHome(); }, [active, remote]);

  useEffect(() => {
    if (!active || remote) return undefined;
    let disposed = false; let timer = null;
    const loop = async () => {
      if (disposed) return;
      if (!globalThis.document?.hidden) {
        try {
          const [st, pl] = await Promise.all([actions.status(), actions.pools().catch(() => [])]);
          if (!disposed) { setStatus(st); setPools(pl); setFailure(''); }
        } catch (error) {
          if (!disposed) setFailure(error?.status === 404 || error?.status === 403 ? 'unavailable' : errText(error));
        }
      }
      if (!disposed) timer = timers.setTimeout(loop, pollDelay({ base: ADMIN_POLL_MS }));
    };
    loop();
    return () => { disposed = true; timers.clearTimeout(timer); };
  }, [active, remote, tick]);

  if (remote) return html`<div class="empty">Opening fleet administration on this node…</div>`;
  if (failure === 'unavailable') return html`<div class="empty">Federation is not available on this daemon. Link nodes with <code>tclaude federation connect</code> (see docs/federation.md).</div>`;
  const view = adminView(status, { pools });
  if (!view) return html`<div class="empty">${failure ? `Could not read federation status: ${failure}` : 'Loading…'}</div>`;

  const done = (msg) => { setDialog(null); toast(msg, false); reload(); };
  const untrust = (r) => confirm({
    title: `Untrust ${r.label}?`,
    body: `${r.label} (${r.id}) stops being a trusted peer. Every peer grant you gave it (${r.grants.total}) and its cached catalog are deleted, `
      + 'its node pools\' grants stop applying to it, and it can no longer read, message, attach to or spawn on this node. '
      + 'Trusting it again starts from scratch, with a fresh fingerprint check.',
    okLabel: 'Untrust',
    busyLabel: 'Untrusting…',
    action: () => actions.untrust(r.id),
  }).then((ok) => { if (ok) done(`Untrusted ${r.label}`); }).catch((e) => toast(`Untrust failed: ${errText(e)}`, true));
  const restrict = (r) => confirm({
    title: `Restrict ${r.label}?`,
    body: `${r.label} loses the implicit access unrestricted trust gave it: from now on it can do only what its explicit peer grants (${r.grants.total}) allow.`,
    okLabel: 'Restrict',
    busyLabel: 'Saving…',
    action: () => setLevel(actions, { instance: r.id, level: 'restricted' }),
  }).then((ok) => { if (ok) done(`${r.label} is now restricted`); }).catch((e) => toast(`Restrict failed: ${errText(e)}`, true));

  const sub = SUB_PAGES.find((p) => p.id === page) || SUB_PAGES[0];
  return html`<div class="fleet-admin">
    <${Identity} self=${view.self} actions=${actions} confirm=${confirm} toast=${toast} copy=${copy} reload=${reload} />
    <div class="fa-subtabs" role="tablist">${SUB_PAGES.map((p) => html`<button type="button" role="tab" key=${p.id} aria-selected=${p.id === sub.id ? 'true' : 'false'}
      class=${`fa-subtab${p.id === sub.id ? ' on' : ''}`} onClick=${() => setPage(p.id)}>${p.label}</button>`)}</div>
    ${sub.id === 'peers'
      ? html`<${PeersPage} view=${view} now=${now()} onTrust=${(r) => setDialog({ kind: 'trust', row: r })}
          onUnrestrict=${(r) => setDialog({ kind: 'unrestrict', row: r })} onRestrict=${restrict} onUntrust=${untrust}
          onGrants=${(r) => { setGrantTarget(r.id); setPage('grants'); }} />`
      : sub.id === 'harnesses'
      ? html`<${HarnessesPage} view=${view} actions=${harnessActions} confirm=${confirm} toast=${toast} copy=${copy} timers=${timers} />`
      : sub.id === 'invites'
      ? html`<${InvitesPage} view=${view} actions=${actions} confirm=${confirm} toast=${toast} copy=${copy} now=${now()} />`
      : sub.id === 'profiles'
      ? html`<${ProfilesPage} view=${view} pools=${pools} actions=${actions} confirm=${confirm} toast=${toast} reload=${reload}
          onOpenGrants=${(target) => { setGrantTarget(target); setPage('grants'); }} />`
      : sub.id === 'grants'
      ? html`<${GrantsPage} view=${view} pools=${pools} groups=${localGroups(snapshot.value)} actions=${actions} confirm=${confirm} toast=${toast}
          target=${grantTarget} setTarget=${setGrantTarget} />`
      : html`<${AuditPage} view=${view} actions=${actions} now=${now()} />`}
    ${dialog?.kind === 'trust' && html`<${TrustDialog} row=${dialog.row} actions=${actions} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${dialog?.kind === 'unrestrict' && html`<${UnrestrictDialog} row=${dialog.row} actions=${actions} onClose=${() => setDialog(null)} onDone=${done} />`}
  </div>`;
}

export function mountFleetAdminIsland({ host, state, actions, registerCleanup }) {
  render(html`<${FleetAdmin} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
