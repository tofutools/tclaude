import { h } from 'preact';
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
