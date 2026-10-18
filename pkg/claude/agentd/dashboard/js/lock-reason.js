// lock-reason.js — how a dialog shows what another node withholds: the
// control stays where it is, disabled, with a small 🔒 whose hover or focus
// says why (the missing grant, by name). NodeChip marks a dialog acting on
// another node; it is the only remote marker a dialog carries.
import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

export function LockReason({ why, id = undefined }) {
  if (!why) return null;
  return html`<span class="lock-why" id=${id} tabindex="0" role="img" aria-label=${`Locked: ${why}`} data-why=${why}>🔒</span>`;
}

export function NodeChip({ name }) {
  if (!name) return null;
  return html`<span class="dialog-node-chip" title=${`This acts on ${name}`}>on ${name}</span>`;
}
