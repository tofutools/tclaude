// row-menu.js -- a table row's ⋯ (or ⚙) menu, shared by a group's
// Resources (groups-resources.js), its saved processes and its Task board
// list (groups-taskboard.js).

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Button, Menu, MenuItem } from './ui-components.js';

const html = htm.bind(h);

// RowMenu is a row's ⋯: what is rarely wanted, and what cannot be undone
// behind a confirm. An item with children opens them in its place (Move to
// group lists the groups), with a way back; children may be a function,
// read only when the item is picked. A disabled item is a note.
export function RowMenu({ label, items, glyph = '⋯', title = '', buttonClass = 'gr-more', keepSlot = false }) {
  const [open, setOpen] = useState(false);
  const [sub, setSub] = useState(null);
  const ref = useRef(null);
  const close = () => { setOpen(false); setSub(null); };
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') close(); });
  if (!items.length) {
    return keepSlot ? html`<${Button} size="sm" icon=${true} class=${buttonClass} disabled aria-label=${`Nothing more for ${label}`}>${glyph}<//>` : null;
  }
  const shown = sub ? [{ label: '‹ Back', attrs: { 'data-menu-back': '' }, back: true }, ...sub.children] : items;
  const pick = (item) => {
    if (item.back) return setSub(null);
    if (item.children) return setSub({ ...item, children: typeof item.children === 'function' ? item.children() : item.children });
    close();
    void item.run();
  };
  return html`<span class="gr-add" ref=${ref}>
    <${Button} size="sm" variant=${keepSlot ? undefined : 'link'} icon=${keepSlot} class=${keepSlot ? buttonClass : `gq-link ${buttonClass}`} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
      title=${title || `More for ${label}`} aria-label=${title || `More for ${label}`} onClick=${() => (open ? close() : setOpen(true))}>${glyph}<//>
    ${open && html`<${Menu} size="sm" class="gr-add-menu">
      ${shown.map((item) => html`<${MenuItem} key=${item.label} danger=${item.danger} disabled=${item.disabled} ...${item.attrs || {}}
        onClick=${() => pick(item)}>${item.children ? `${item.label} ›` : item.label}<//>`)}
    <//>`}
  </span>`;
}

