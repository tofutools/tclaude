import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

/* The note under a sandbox-implementation row, rendered identically wherever
   sandboxImplHintFor's answer is shown — the spawn dialog and the profile
   editor — so the two call sites cannot drift in how they present it. */
export function SandboxImplHint({ hint, id }) {
  if (!hint) return null;
  return html`<div id=${id} class=${`spawn-field-hint${hint.warn ? ' warn' : ''}`}
    >${hint.text}</div>`;
}
