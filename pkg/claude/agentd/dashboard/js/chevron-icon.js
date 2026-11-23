// chevron-icon.js -- the dashboard's one chevron: drawn as a line icon, not
// the ‹ › « » glyphs, whose position in a button shifts with the font and
// the system. Previous/next steps, pagers and back buttons all draw it.

import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

const PATHS = {
  left: 'M10 3L5 8l5 5',
  right: 'M6 3l5 5-5 5',
  'double-left': 'M8 3L3 8l5 5M13 3L8 8l5 5',
  'double-right': 'M3 3l5 5-5 5M8 3l5 5-5 5',
};

// Chevron points left or right, doubled for a first/last step; size is its
// square in px. It is decorative: its button carries the label.
export function Chevron({ dir = 'right', double = false, size = 14 }) {
  const d = PATHS[double ? `double-${dir}` : dir] || PATHS.right;
  return html`<svg class="chevron-ico" viewBox="0 0 16 16" width=${size} height=${size} aria-hidden="true" focusable="false"
    fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d=${d} /></svg>`;
}
