// lock-reason.js — how a dialog shows what another node withholds: the
// control stays where it is, disabled, with a small 🔒 whose hover or focus
// says why (the missing grant, by name). NodeChip marks a dialog acting on
// another node; it is the only remote marker a dialog carries.
import { h } from 'preact';
import htm from 'htm';
import { isPageNode } from './node-api.js';
import { unrestrictedOn } from './node-trust.js';

const html = htm.bind(h);

export function LockReason({ why, id = undefined }) {
  if (!why) return null;
  return html`<span class="lock-why" id=${id} tabindex="0" role="img" aria-label=${`Locked: ${why}`} data-why=${why}>🔒</span>`;
}

export function NodeChip({ name }) {
  if (!name) return null;
  return html`<span class="node-chip" title=${`This acts on ${name}`}>on ${name}</span>`;
}

// trustLock is the 🔒 reason of a control that another node keeps to its own
// operator: blank where this operator may use it (this node, or a peer that
// trusts this one unrestricted). "Here" is a blank node off a peer's page.
export function trustLock(node, what) {
  const remotePage = !!globalThis.__tclaudeRemoteNode?.id;
  if (!node && !remotePage) return '';
  if (unrestrictedOn(node)) return '';
  const name = (!node || isPageNode(node)) && remotePage
    ? globalThis.document?.documentElement?.dataset?.remoteNodeName || 'This peer' : 'That node';
  return `${name} keeps ${what} to its own operator: it needs unrestricted trust from ${name === 'That node' ? 'it' : name}.`;
}
