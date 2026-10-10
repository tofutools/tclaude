import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { recoverCommands } from './skynet-model.js';

const html = htm.bind(h);

function when(iso) {
  const t = new Date(iso);
  return Number.isFinite(t.getTime()) && t.getFullYear() > 1 ? t.toLocaleString() : '—';
}

// keyBadge is the Peers-row marker for a peer's active key transition.
export function keyBadge(t) {
  if (!t) return null;
  return t.state === 'conflict'
    ? { cls: 'fa-danger', text: '⚠ competing keys', title: 'Competing new signing keys: needs explicit recovery' }
    : { cls: 'fa-warn', text: 'new key pending', title: 'A new signing key waits out the detection window' };
}

// KeyTransitionDialog explains a peer's pending or conflicted signing-key
// transition, read-only. Rotation and recovery stay in the CLI; a conflict
// shows the recover-peer commands to copy.
export function KeyTransitionDialog({ row, resolved = false, copy, toast, onClose }) {
  const t = row.keyTransition;
  if (resolved) {
    return html`<${Overlay} id="fleet-key-transition" labelledby="fleet-key-transition-title" onClose=${onClose}>
      <h3 id="fleet-key-transition-title">${row.label}: key transition resolved</h3>
      <p>This node no longer reports a pending or conflicting signing key for ${row.label}. See <code>tclaude federation identity rotations</code> for what happened.</p>
      <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Close</button></div>
    </${Overlay}>`;
  }
  const cmds = t.state === 'conflict' ? recoverCommands(t) : null;
  const onCopy = (text) => copy(text).then(() => toast('Command copied', false)).catch(() => toast(text, false));
  return html`<${Overlay} id="fleet-key-transition" labelledby="fleet-key-transition-title" onClose=${onClose}>
    <h3 id="fleet-key-transition-title">${t.state === 'conflict' ? `${row.label}: competing new signing keys` : `${row.label} announced a new signing key`}</h3>
    <table class="fa-table fa-kv"><tbody>
      <tr><th>current</th><td><code>${t.oldID || row.id}</code><br /><code>${row.fingerprint}</code></td></tr>
      <tr><th>${t.state === 'conflict' ? 'one candidate' : 'announced'}</th><td><code>${t.newID}</code><br /><code>${t.newFingerprint}</code>${t.state === 'conflict' ? html`<br /><span class="muted">others: <code>tclaude federation identity rotations</code></span>` : ''}</td></tr>
      <tr><th>received</th><td>${when(t.receivedAt)}</td></tr>
      ${t.state === 'pending' && html`<tr><th>earliest acceptance</th><td>${when(t.acceptAfter)}</td></tr>`}
      ${t.reason && html`<tr><th>reason</th><td>${t.reason}</td></tr>`}
    </tbody></table>
    ${t.state === 'pending'
      ? html`<p>This node is waiting out its detection window before it accepts the successor key; nothing needs doing now. Acceptance is not guaranteed: if a competing successor key appears, the transition stops and needs your explicit recovery. To be sure the new key is legitimate, compare its fingerprint with ${row.label}'s operator.</p>`
      : html`<p class="fa-danger">More than one signed successor key was announced, so this node accepts none of them on its own. Ask ${row.label}'s operator, out of band, which instance and fingerprint are theirs. Then, in a terminal on this node:</p>
        ${cmds ? html`<div class="fa-kt-cmd"><span class="muted">Preview (changes nothing):</span> <code id="fleet-key-preview">${cmds.preview}</code>
            <button type="button" class="fa-link" data-fa="copy-preview" onClick=${() => onCopy(cmds.preview)}>copy</button></div>
          ${cmds.apply && html`<div class="fa-kt-cmd"><span class="muted">Only if the operator confirmed this exact candidate's fingerprint:</span> <code id="fleet-key-apply">${cmds.apply}</code>
            <button type="button" class="fa-link" data-fa="copy-apply" onClick=${() => onCopy(cmds.apply)}>copy</button></div>`}
          <div class="muted">If the operator names a different instance or fingerprint, do not apply this one: list the candidates with <code>tclaude federation identity rotations</code>.</div>`
        : html`<div class="muted">List the candidates with <code>tclaude federation identity rotations</code>.</div>`}`}
    <div class="muted fa-cli-note">Key rotation and recovery are CLI-only: <code>tclaude federation identity …</code></div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

// fmtSpan renders a duration as "2 d 3 h" / "3 h 12 min" / "12 min".
export function fmtSpan(ms) {
  const m = Math.max(0, Math.ceil(ms / 60000));
  const d = Math.floor(m / 1440); const h = Math.floor((m % 1440) / 60); const min = m % 60;
  if (d) return h ? `${d} d ${h} h` : `${d} d`;
  if (h) return min ? `${h} h ${min} min` : `${h} h`;
  return `${min} min`;
}

// localRotation reads this node's pending rotation from the rotations read:
// the latest chain row names the successor and its earliest activation.
export function localRotation(rotations) {
  const local = rotations?.local;
  if (!local?.pending) return null;
  const row = (local.chain || []).at(-1) || {};
  const at = new Date(row.activate_at);
  return { newID: row.new_id || '', newFingerprint: row.new_fingerprint || '', activateAt: Number.isFinite(at.getTime()) ? at : null };
}

// rotateConsequence spells out what a rotation does, from the preview's
// consequence flags (a missing flag is still stated: never drop a warning).
export function rotateConsequence(p) {
  const fx = p?.effects || {};
  const on = (k) => fx[k] !== false;
  const id = p?.instance_id || 'this node';
  const parts = [`Confirming generates a new signing key and successor instance ID (this preview generated nothing). The current identity ${id}${p?.fingerprint ? ` (${p.fingerprint})` : ''} signs the successor${on('successor_linked') ? ', linking it to this identity so trusted peers can follow it' : ''}.`];
  if (p?.window_seconds > 0) parts.push(`Peers wait out a ${fmtSpan(p.window_seconds * 1000)} detection window before accepting the new key; until then the rotation shows as pending here and on their side.`);
  if (on('streams_reconnect')) parts.push('Federation streams to the hub and peers reconnect.');
  if (on('pending_sealed_mail_requires_resend')) parts.push('Sealed mail still pending delivery to peers must be resent.');
  const revoked = [on('issued_model_credentials_revoked') && 'Model credentials this node issued to peers', on('requester_paid_leases_revoked') && 'requester-paid model leases'].filter(Boolean);
  if (revoked.length) parts.push(`${revoked.join(' and ')} are revoked.`);
  if (p?.hop_limit > 0) {
    const n = (p.hop_count || 0) + 1;
    parts.push(n >= p.hop_limit
      ? `This is the last linked rotation (${n} of ${p.hop_limit}): rotating again means re-pairing every peer.`
      : `This is linked rotation ${n} of at most ${p.hop_limit}; rotation ${p.hop_limit + 1} needs re-pairing every peer.`);
  }
  parts.push('Recovering or revoking the old key stays in the CLI.');
  return parts.join(' ');
}

// rotateBlock says why a rotation cannot start now (the daemon refuses both
// with 409 rotation), or '' when it can.
export function rotateBlock(p) {
  if (!p) return '';
  if (p.pending) return 'A rotation is already pending.';
  if (p.hop_limit > 0 && (p.hop_count || 0) >= p.hop_limit) return `This identity has reached ${p.hop_limit} linked rotations. Re-pair offline peers before resetting the public chain (CLI); no further rotation is possible until then.`;
  return '';
}

// IdentityRotationSection shows this node's rotation state in node settings
// and starts a rotation (tclaude federation identity rotate [--apply]) after
// a confirmation that spells out every consequence.
export function IdentityRotationSection({ actions, confirm, toast, timers = globalThis }) {
  const [rot, setRot] = useState(undefined);
  const [preview, setPreview] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const load = () => {
    setNow(Date.now());
    actions.identityRotations().then((r) => setRot(localRotation(r))).catch((e) => setRot({ error: e?.message || String(e) }));
    actions.previewRotateIdentity().then(setPreview).catch(() => setPreview(null));
  };
  useEffect(() => { load(); }, []);
  const pending = rot && !rot.error ? rot : null;
  useEffect(() => {
    if (!pending) return undefined;
    const id = timers.setInterval(() => setNow(Date.now()), 30000);
    return () => timers.clearInterval(id);
  }, [pending]);
  const block = pending ? '' : rotateBlock(preview);
  const rotate = () => {
    setError('');
    setBusy(true);
    actions.previewRotateIdentity().then((p) => {
      setPreview(p);
      setBusy(false);
      const why = rotateBlock(p);
      if (why) { setError(why); return false; }
      return confirm({
        title: 'Rotate this node\'s identity?',
        body: rotateConsequence(p),
        okLabel: 'Rotate identity',
        busyLabel: 'Rotating…',
        action: () => actions.rotateIdentity(),
      });
    }).then((r) => {
      if (!r) return;
      toast(`Identity rotated; ${r.new_id || 'the successor'} is pending`, false);
      load();
    }).catch((e) => { setBusy(false); setError(e?.message || String(e)); load(); });
  };
  const left = pending?.activateAt ? pending.activateAt.getTime() - now : null;
  return html`<div id="fleet-identity">
    <h4 class="fa-ns-h">Identity</h4>
    ${rot?.error ? html`<div class="fa-danger">${rot.error}</div>`
      : rot === undefined ? html`<div class="muted">Loading…</div>`
      : pending ? html`<div id="fleet-rotation-pending" class="fa-warn">Rotation pending: successor <code>${pending.newID}</code>${pending.newFingerprint && html` <code>${pending.newFingerprint}</code>`}.
          ${left == null ? '' : left > 0 ? ` Peers accept it no earlier than ${pending.activateAt.toLocaleString()} (in ${fmtSpan(left)}).` : ' The detection window has passed.'}</div>`
      : html`<div class="muted">No rotation pending.</div>`}
    ${block && html`<div id="fleet-rotation-blocked" class="fa-warn">${block}</div>`}
    <div class="fa-ns-actions"><button id="fleet-rotate-open" type="button" disabled=${busy || !!pending || !!block} onClick=${rotate}>Rotate identity…</button>
      <span class="muted">CLI: <code>tclaude federation identity rotate [--apply]</code>; recover-local, recover-peer and revoke-old stay CLI-only</span></div>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
  </div>`;
}
