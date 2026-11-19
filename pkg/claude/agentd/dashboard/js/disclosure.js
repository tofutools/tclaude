// A foldable section whose header carries its own controls. A native
// <details>/<summary> cannot hold them: interactive content inside <summary>
// is invalid (Chrome's Issues panel flags each control), and a closed
// <details> hides everything but its <summary>, so the controls cannot sit
// beside it either. These sections are a plain wrapper with an `open` class,
// a header row, and a real toggle button (aria-expanded) around the title,
// the shape FusedSection (groups-processes.js) already has. The header row
// still folds on a click anywhere a control is not, as the summary did.

import { h } from 'preact';
import { useId } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

// A focusable element (the 🔒 lock reason) is a control too: tapping it shows
// why, and must not fold the section under it.
const CONTROL = 'button, a[href], input, select, textarea, label, [role="menu"], [role="menuitem"], [tabindex]:not([tabindex="-1"])';

// DisclosureToggle is the header's fold button: keyboard Enter / Space and
// the screen reader's expanded state come from it.
// `controls` names the body it folds (aria-controls), `label` replaces the
// text as its accessible name, and `disabled` holds the section as it is.
export function DisclosureToggle({ open, class: extra, onToggle, title, controls, label, disabled, children }) {
  return html`<button data-ui-exempt type="button" class=${['disclosure-toggle', extra].filter(Boolean).join(' ')}
    aria-expanded=${open ? 'true' : 'false'} aria-controls=${controls} aria-label=${label} title=${title}
    disabled=${disabled} onClick=${(event) => onToggle(event)}>${children}</button>`;
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
    onToggle(event);
  };
}

// Disclosure is the shared foldable box: a wrapper (`open` class while
// open), a header row, and a body. The header holds `before`, the fold
// button around `toggle`, then `actions` -- controls that sit beside the
// toggle, never inside it, which is what a <summary> could not do. It is
// controlled: the caller owns `open` and persists it in `onToggle(event)`.
//
// - `foldOnHead` folds on a click anywhere in the header a control is not
//   (headToggleClick); off, only the toggle folds, as a group's wide header
//   (a drag handle and drop zone) wants.
// - `fill` (true, or the span's class) ends the header with an empty span
//   that folds on a click, the divider line FusedSection draws.
// - `keepBody` keeps a folded body in the document, hidden, as a closed
//   <details> did (drop targets and queries still find it); otherwise a
//   folded body is not drawn at all.
// - `headAttrs` / `attrs` / `bodyAttrs` land on the header, wrapper, body.
export function Disclosure({
  open, onToggle, class: extra = '', attrs = {}, headClass = '', headAttrs = {},
  toggle, toggleClass = '', title, label, disabled = false, before = null, actions = null,
  foldOnHead = false, fill = false, bodyClass = '', bodyAttrs = {}, keepBody = false, children,
}) {
  const bodyId = `${useId()}-disclosure`;
  const drawn = open || keepBody;
  const fold = (event) => { if (!disabled) onToggle(event); };
  return html`<div class=${['disclosure', extra, open ? 'open' : ''].filter(Boolean).join(' ')} ...${attrs}>
    <div class=${['disclosure-head', headClass].filter(Boolean).join(' ')} ...${headAttrs}
      onClick=${foldOnHead ? headToggleClick(fold) : undefined}>
      ${before}<${DisclosureToggle} class=${toggleClass} open=${open} onToggle=${fold} title=${title} label=${label}
        disabled=${disabled} controls=${drawn ? bodyId : undefined}>${toggle}<//>${actions}
      ${fill && html`<span class=${['disclosure-fill', typeof fill === 'string' ? fill : ''].filter(Boolean).join(' ')} aria-hidden="true" title=${title} onClick=${fold}></span>`}
    </div>
    ${drawn && html`<div id=${bodyId} class=${bodyClass || undefined} hidden=${!open} ...${bodyAttrs}>${children}</div>`}
  </div>`;
}
