// board-quick-menu.js -- the quick menu of a connector dropped on blank board
// (tcl-29ocuy): what shape goes at its end. The field holds the keys while it is
// open, so the board's own number keys stay out of it. Letters search, Enter
// takes the marked row and a digit takes the row showing it. Shape names hold no
// digits, so a digit never searches.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { quickShapes } from './board-model.js';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);

// The most room the menu takes, to keep it on the canvas: board.css's width, and
// the field with every row.
export const QUICK_MENU_SIZE = Object.freeze({ width: 180, height: 134 });

export function BoardQuickMenu({ x, y, kind, onPreview, onPick, onCancel }) {
  const [query, setQuery] = useState('');
  const [marked, setMarked] = useState(kind);
  const root = useRef(null);
  const field = useRef(null);
  useEffect(() => { field.current?.focus(); }, []);
  // Clicking elsewhere cancels, and the click then does what it would have done.
  // The pointer wandering off does not: the menu is the keyboard's.
  useDismissablePopover(root, true, (why) => { if (why !== 'away') onCancel(); });
  const shown = quickShapes(query);
  const current = shown.find(({ id }) => id === marked) || shown[0];
  useEffect(() => { if (current) onPreview(current.id); }, [current?.id]);

  const step = (by) => {
    const at = shown.indexOf(current);
    if (shown.length) setMarked(shown[(at + by + shown.length) % shown.length].id);
  };
  const onKeyDown = (event) => {
    const plain = !event.ctrlKey && !event.metaKey && !event.altKey;
    let handled = true;
    if (event.key === 'Escape') onCancel();
    else if (event.key === 'Enter') { if (current) onPick(current.id); }
    else if (event.key === 'ArrowDown' || (event.key === 'Tab' && !event.shiftKey)) step(1);
    else if (event.key === 'ArrowUp' || event.key === 'Tab') step(-1);
    else if (plain && /^[0-9]$/.test(event.key)) {
      const row = shown.find(({ digit }) => digit === event.key);
      if (row) onPick(row.id);
    } else handled = false;
    if (handled) { event.preventDefault(); event.stopPropagation(); }
  };

  return html`<div ref=${root} class="board-quick" style=${`left:${x}px;top:${y}px`} role="dialog" aria-label="Next shape">
    <input ref=${field} type="text" aria-label="Find a shape" placeholder="Shape…" autocomplete="off" spellcheck="false" value=${query}
      onInput=${(event) => { setQuery(event.currentTarget.value); setMarked(''); }} onKeyDown=${onKeyDown} />
    ${shown.map((shape) => html`<button key=${shape.id} type="button" class=${shape === current ? 'on' : ''} tabindex="-1"
      onPointerDown=${(event) => event.preventDefault()} onPointerEnter=${() => setMarked(shape.id)} onClick=${() => onPick(shape.id)}>
      <kbd>${shape.digit}</kbd>${shape.label}
    </button>`)}
    ${!shown.length && html`<div class="board-quick-none">No such shape</div>`}
  </div>`;
}
