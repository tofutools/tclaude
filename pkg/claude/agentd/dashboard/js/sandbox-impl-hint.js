import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

/* The note under a sandbox-implementation row, rendered identically wherever
   sandboxImplHintFor's answer is shown.

   It exists as one component because the hint can carry a documentation link
   and the two call sites — the spawn dialog and the profile editor — would
   otherwise each decide for themselves whether to render it. A site that
   interpolated only `.text` would drop the link silently: the copy would still
   read as if it pointed somewhere, and nothing would say it did not. */
export function SandboxImplHint({ hint, id }) {
  if (!hint) return null;
  return html`<div id=${id} class=${`spawn-field-hint${hint.warn ? ' warn' : ''}`}
    >${hint.text}${hint.doc && html` <a href=${hint.doc.href} target="_blank"
      rel="noopener">${hint.doc.label}</a>`}</div>`;
}
