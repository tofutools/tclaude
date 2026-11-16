// A foldable section whose header carries its own controls. A native
// <details>/<summary> cannot hold them: interactive content inside <summary>
// is invalid (Chrome's Issues panel flags each control), and a closed
// <details> hides everything but its <summary>, so the controls cannot sit
// beside it either. These sections are a plain wrapper with an `open` class,
// a header row, and a real toggle button (aria-expanded) around the title,
// the shape FusedSection (groups-processes.js) already has. The header row
// still folds on a click anywhere a control is not, as the summary did.

import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

const CONTROL = 'button, a[href], input, select, textarea, label, [role="menu"], [role="menuitem"]';

// DisclosureToggle is the header's fold button: keyboard Enter / Space and
// the screen reader's expanded state come from it.
export function DisclosureToggle({ open, class: extra, onToggle, title, children }) {
  return html`<button data-ui-exempt type="button" class=${['disclosure-toggle', extra].filter(Boolean).join(' ')}
    aria-expanded=${open ? 'true' : 'false'} title=${title} onClick=${() => onToggle()}>${children}</button>`;
}

// DisclosureTri is the ▶ / ▼ marker a <details> summary drew from
// dashboard.css's generic rule (a CSS chevron in the phone layout), for a
// section that kept that look.
export function DisclosureTri({ open }) {
  return html`<span class=${`disclosure-tri${open ? ' open' : ''}`} aria-hidden="true"></span>`;
}

// headToggleClick makes the rest of a header row a fold target too: a click
// that lands on no control (the toggle button folds by itself, and a control
// that called preventDefault keeps its click).
export function headToggleClick(onToggle) {
  return (event) => {
    if (event.defaultPrevented || event.target.closest?.(CONTROL)) return;
    onToggle();
  };
}
