// cell-parts.js -- small cell components the dashboard tables share: a
// node's scope pill and a timestamp shown as an absolute time plus how long
// ago it was.

import { h } from 'preact';
import htm from 'htm';
import { relTime } from './helpers.js';

const html = htm.bind(h);

// NodePill is a node's coloured scope pill; this node's own name leads with ⌂.
export function NodePill({ node, class: extra = '' }) {
  return html`<span class=${extra ? `scope-pill ${extra}` : 'scope-pill'} style=${`--nc:${node.color}`}>${node.local ? '⌂ ' : ''}${node.name}</span>`;
}

// StampAgo is a log or audit time: the absolute stamp (text, with the raw
// value as its title), then "(5m ago)" when the time is known.
export function StampAgo({ iso, text, title = iso }) {
  const ago = relTime(iso);
  return html`<span class="last-hook" title=${title}>${text}</span>${ago && html` <span class="muted">(${ago})</span>`}`;
}
