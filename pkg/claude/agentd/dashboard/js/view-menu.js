// view-menu.js -- the dashboard's view filter dropdown (tcl-t1onur): a button
// with a count of what differs from the default, over a menu of checkbox
// rows, headings and separators. The Groups tab's "▾ view" and the Collab
// panel's "Filter" share it, so the two look and behave the same. Escape
// closes the menu and returns focus to the button; a press outside closes it.
// A closed menu is hidden and drops its menu role.

import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// ViewMenu is the button and its menu. open/onOpenChange hold whether the menu
// is open; badge is the count shown on the button (hidden at 0). ids names the
// button, menu and badge elements ({ button, menu, badge }); variant and
// buttonClass style the button; children are the menu's rows.
export function ViewMenu({ open, onOpenChange, label, badge = 0, ids = {}, variant, buttonClass = '', title, children }) {
  const buttonRef = useRef(null);
  const menuRef = useRef(null);
  const latest = useRef(null);
  latest.current = { open, onOpenChange };

  useEffect(() => {
    const onKeyDown = (event) => {
      if (event.key !== 'Escape' || !latest.current.open) return;
      event.preventDefault();
      latest.current.onOpenChange(false);
      buttonRef.current?.focus();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, []);

  useDismissablePopover(() => menuRef.current?.parentElement, open, () => onOpenChange(false), { escape: false });

  return html`<div class="view-popover-wrap">
    <${Button}
      size="sm"
      variant=${variant}
      domRef=${buttonRef}
      id=${ids.button}
      class=${`view-btn ${buttonClass}`.trim()}
      aria-haspopup="menu"
      aria-expanded=${open ? 'true' : 'false'}
      aria-controls=${ids.menu}
      title=${title}
      onClick=${() => onOpenChange(!open)}
    >
      ${label}
      <span id=${ids.badge} class="view-badge ui-badge" hidden=${!badge}>${badge || ''}</span>
    <//>
    <div ref=${menuRef} id=${ids.menu} class=${`view-menu${open ? ' open' : ''}`} role=${open ? 'menu' : undefined} hidden=${!open}>${children}</div>
  </div>`;
}

// ViewToggle is one checkbox row of a ViewMenu.
export function ViewToggle({ id, checked, onChange, title, children }) {
  return html`<label class="filter-toggle" title=${title}>
    <input id=${id} type="checkbox" checked=${checked} onChange=${(event) => onChange(event.currentTarget.checked)} />
    <span>${children}</span>
  </label>`;
}

// ViewMenuSep separates two sets of rows; ViewMenuHeading names the set below.
export const ViewMenuSep = () => html`<div class="view-menu-sep" role="separator"></div>`;
export const ViewMenuHeading = ({ title, children }) => html`<div class="ui-eyebrow view-menu-heading" title=${title}>${children}</div>`;
