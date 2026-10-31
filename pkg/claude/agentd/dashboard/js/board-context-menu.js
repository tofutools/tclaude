// board-context-menu.js -- the right-click menu of a board (tcl-99j1u9). The
// editor says what it offers: rows, separators and rows of swatches. This keeps
// it on screen and takes the keys, wearing the tab strip's menu styling.

import { h } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';

const html = htm.bind(h);

const EDGE_PX = 8;

export function BoardContextMenu({ x, y, label, items, win, onClose }) {
  const root = useRef(null);
  const [place, setPlace] = useState({ x, y });
  // The pointer wandering off leaves it open: a menu asks for a choice.
  useDismissablePopover(root, true, (why) => { if (why !== 'away') onClose(); });
  useLayoutEffect(() => {
    const menu = root.current;
    const box = menu?.getBoundingClientRect?.();
    if (box?.width) {
      setPlace({
        x: Math.max(EDGE_PX, Math.min(x, (win?.innerWidth ?? Infinity) - box.width - EDGE_PX)),
        y: Math.max(EDGE_PX, Math.min(y, (win?.innerHeight ?? Infinity) - box.height - EDGE_PX)),
      });
    }
    menu?.querySelector?.('[role="menuitem"]:not(:disabled), [role="menuitemradio"]:not(:disabled)')?.focus();
  }, [x, y]);

  const run = (action) => { action(); onClose(); };
  const onKeyDown = (event) => {
    // A key held by the menu is not the board's.
    event.stopPropagation();
    if (event.key === 'Escape') { event.preventDefault(); onClose(); return; }
    const rows = [...event.currentTarget.querySelectorAll('button:not(:disabled)')];
    const index = rows.indexOf(event.target);
    const next = { ArrowDown: rows[(index + 1) % rows.length], ArrowUp: rows[(index - 1 + rows.length) % rows.length], Home: rows[0], End: rows.at(-1) }[event.key];
    if (!next) return;
    event.preventDefault();
    next.focus();
  };

  return html`<div ref=${root} class="mux-tab-menu board-context" role="menu" aria-label=${label} style=${`left:${place.x}px;top:${place.y}px`}
    onKeyDown=${onKeyDown} onContextMenu=${(event) => event.preventDefault()}>
    ${items.map((item, i) => {
      if (item === 'separator') return html`<div key=${i} class="mux-tab-menu-separator" role="separator"></div>`;
      if (item.swatches) {
        return html`<div key=${i} class="board-context-swatches" role="group">
          ${item.swatches.map((s) => html`<button key=${s.color} type="button" role="menuitem" tabIndex="-1" aria-label=${s.label} title=${s.label}
            class=${`board-swatch${s.color === 'transparent' ? ' fill' : ''}${s.on ? ' on' : ''}`} style=${s.color === 'transparent' ? '' : `background:${s.color}`}
            onClick=${() => run(s.run)}>${s.color === 'transparent' ? '∅' : ''}</button>`)}
        </div>`;
      }
      return html`<button key=${i} type="button" tabIndex="-1" disabled=${!!item.disabled} role=${item.checked === undefined ? 'menuitem' : 'menuitemradio'}
        aria-checked=${item.checked === undefined ? null : String(item.checked)} class=${`mux-tab-menu-item${item.danger ? ' danger' : ''}`} onClick=${() => run(item.run)}>
        <span>${item.checked ? '✓ ' : ''}${item.label}</span>${item.keys && html`<kbd>${item.keys}</kbd>`}
      </button>`;
    })}
  </div>`;
}
