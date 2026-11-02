// The standard dashboard controls as Preact components (tcl-32qwip). They
// render the ui-* classes from ui.css, so Preact code gets the standard look,
// states and skins by using them; innerHTML and createElement paths use the
// classes directly, through uiButtonClass where a variant is computed.
//
// Every component passes unknown props through to its element, takes extra
// classes as `class`, and hands its DOM element to `domRef` (a function
// component cannot take `ref` in Preact 10 without compat).

import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

const BUTTON_VARIANTS = new Set(['primary', 'success', 'danger', 'ghost', 'link']);

function join(...parts) {
  return parts.filter(Boolean).join(' ');
}

// uiButtonClass returns the class list for a standard button: variant is
// 'primary' | 'success' | 'danger' | 'ghost' | 'link' (or a space-separated
// pair such as 'ghost danger'), size 'sm' for rows and toolbars, icon for a
// square icon-only button.
export function uiButtonClass({ variant = '', size = '', icon = false } = {}) {
  return join('ui-btn', buttonModifiers({ variant, size, icon }));
}

function buttonModifiers({ variant, size, icon }) {
  const variants = String(variant || '').split(/\s+/).filter((name) => BUTTON_VARIANTS.has(name));
  return join(...variants.map((name) => `ui-btn-${name}`), size === 'sm' && 'ui-sm', icon && 'ui-btn-icon');
}

// Button: busy marks aria-busy (pair it with disabled while a request runs);
// pressed, when given, makes it a toggle with aria-pressed.
export function Button({ variant, size, icon, busy, pressed, type = 'button', class: extra, className, domRef, children, ...rest }) {
  return html`<button
    ...${rest}
    ref=${domRef}
    type=${type}
    class=${join('ui-btn', buttonModifiers({ variant, size, icon }), extra, className)}
    aria-busy=${busy ? 'true' : undefined}
    aria-pressed=${pressed === undefined ? undefined : pressed ? 'true' : 'false'}
  >${children}</button>`;
}

export function Select({ size, invalid, class: extra, className, domRef, children, ...rest }) {
  return html`<select
    ...${rest}
    ref=${domRef}
    class=${join('ui-select', size === 'sm' && 'ui-sm', extra, className)}
    aria-invalid=${invalid ? 'true' : undefined}
  >${children}</select>`;
}

export function Input({ size, invalid, type = 'text', class: extra, className, domRef, ...rest }) {
  return html`<input
    ...${rest}
    ref=${domRef}
    type=${type}
    class=${join('ui-input', size === 'sm' && 'ui-sm', extra, className)}
    aria-invalid=${invalid ? 'true' : undefined}
  />`;
}

export function Textarea({ size, invalid, class: extra, className, domRef, ...rest }) {
  return html`<textarea
    ...${rest}
    ref=${domRef}
    class=${join('ui-textarea', size === 'sm' && 'ui-sm', extra, className)}
    aria-invalid=${invalid ? 'true' : undefined}
  />`;
}

// Check: a checkbox (or, with type="radio", a radio) inside its label, so the
// text toggles it. Props go to the input; labelClass styles the label.
export function Check({ type = 'checkbox', labelClass, title, domRef, children, ...rest }) {
  return html`<label class=${join('ui-check', labelClass)} title=${title}>
    <input type=${type} ...${rest} ref=${domRef} />${children}
  </label>`;
}

// Switch: an on/off toggle. Give it an aria-label, or wrap it in a label.
export function Switch({ class: extra, className, domRef, ...rest }) {
  return html`<input type="checkbox" role="switch" ...${rest} ref=${domRef} class=${join('ui-switch', extra, className)} />`;
}

// Seg: a segmented group of Buttons; mark the chosen one pressed.
export function Seg({ class: extra, className, domRef, children, ...rest }) {
  return html`<div role="group" ...${rest} ref=${domRef} class=${join('ui-seg', extra, className)}>${children}</div>`;
}

// Menu: the panel only; its owner positions it and opens and closes it.
export function Menu({ class: extra, className, domRef, children, ...rest }) {
  return html`<div role="menu" ...${rest} ref=${domRef} class=${join('ui-menu', extra, className)}>${children}</div>`;
}

export function MenuItem({ danger, type = 'button', class: extra, className, domRef, children, ...rest }) {
  return html`<button
    role="menuitem"
    ...${rest}
    ref=${domRef}
    type=${type}
    class=${join('ui-menu-item', danger && 'ui-btn-danger', extra, className)}
  >${children}</button>`;
}

export function MenuSep() {
  return html`<hr class="ui-menu-sep" role="separator" />`;
}

export function MenuHeading({ children }) {
  return html`<div class="ui-menu-heading" role="presentation">${children}</div>`;
}
