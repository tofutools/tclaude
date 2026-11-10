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
// pressed, when given, makes it a toggle with aria-pressed. A raw aria-busy or
// aria-pressed still applies when the prop is not given, as does a raw
// aria-invalid on the fields below.
export function Button({ variant, size, icon, busy, pressed, type = 'button', class: extra, className, domRef, children, ...rest }) {
  return html`<button
    ...${rest}
    ref=${domRef}
    type=${type}
    class=${join('ui-btn', buttonModifiers({ variant, size, icon }), extra, className)}
    aria-busy=${busy ? 'true' : rest['aria-busy']}
    aria-pressed=${pressed === undefined ? rest['aria-pressed'] : pressed ? 'true' : 'false'}
  >${children}</button>`;
}

export function Select({ size, invalid, class: extra, className, domRef, children, ...rest }) {
  return html`<select
    ...${rest}
    ref=${domRef}
    class=${join('ui-select', size === 'sm' && 'ui-sm', extra, className)}
    aria-invalid=${invalid ? 'true' : rest['aria-invalid']}
  >${children}</select>`;
}

export function Input({ size, invalid, type = 'text', class: extra, className, domRef, ...rest }) {
  return html`<input
    ...${rest}
    ref=${domRef}
    type=${type}
    class=${join('ui-input', size === 'sm' && 'ui-sm', extra, className)}
    aria-invalid=${invalid ? 'true' : rest['aria-invalid']}
  />`;
}

export function Textarea({ size, invalid, class: extra, className, domRef, ...rest }) {
  return html`<textarea
    ...${rest}
    ref=${domRef}
    class=${join('ui-textarea', size === 'sm' && 'ui-sm', extra, className)}
    aria-invalid=${invalid ? 'true' : rest['aria-invalid']}
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
// size="sm" packs the rows of a long action menu.
export function Menu({ size, class: extra, className, domRef, children, ...rest }) {
  return html`<div role="menu" ...${rest} ref=${domRef} class=${join('ui-menu', size === 'sm' && 'ui-sm', extra, className)}>${children}</div>`;
}

// MenuItem: danger for what cannot be undone, warn for what is reversible but
// disruptive.
export function MenuItem({ danger, warn, type = 'button', class: extra, className, domRef, children, ...rest }) {
  return html`<button
    role="menuitem"
    ...${rest}
    ref=${domRef}
    type=${type}
    class=${join('ui-menu-item', danger && 'ui-btn-danger', warn && !danger && 'ui-menu-warn', extra, className)}
  >${children}</button>`;
}

export function MenuSep() {
  return html`<hr class="ui-menu-sep" role="separator" />`;
}

export function MenuHeading({ children }) {
  return html`<div class="ui-menu-heading" role="presentation">${children}</div>`;
}

// SortTh: a sortable column header in the house style (sortable /
// sort-active / sort-arrow in dashboard.css). active marks the sorted column
// and dir ('asc' | 'desc') its direction; onSort runs on click and on Enter or
// Space. Without onSort a key press clicks the header, for tables whose
// clicks are delegated through data-sort-* attributes. sortLabel supplies the
// default "Sort by …" title.
export function SortTh({ active = false, dir = 'asc', onSort, sortLabel, title, class: extra, className, domRef, children, ...rest }) {
  const activate = (event) => {
    if (event.type === 'keydown') {
      if (event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
      event.preventDefault();
      if (!onSort) {
        event.currentTarget.click();
        return;
      }
    }
    onSort(event);
  };
  return html`<th
    ...${rest}
    ref=${domRef}
    class=${join('sortable', active && 'sort-active', extra, className)}
    tabindex="0"
    aria-sort=${active ? (dir === 'asc' ? 'ascending' : 'descending') : 'none'}
    title=${title ?? (sortLabel ? `Sort by ${sortLabel}` : undefined)}
    onClick=${onSort ? activate : undefined}
    onKeyDown=${activate}
  >${children}<span class="sort-arrow" aria-hidden="true">${active ? (dir === 'asc' ? '▲' : '▼') : '▾'}</span></th>`;
}

// OptionalSelect: the control for an optional setting, one that is either
// left unset (inherit the next tier: a profile, the group, the harness's own
// default) or set. The first option is the unset one; its value is '' unless
// unsetValue names the wire value a form already uses for it (such as
// 'inherit'). Without options it is the tri-state inherit / on / off on the
// values '' / '1' / '0', the shape management-model's triValue and readTri
// convert to and from a nullable boolean; on and off relabel those two.
// options ([value, label] pairs) make it inherit / value instead.
//
// value is the option value as a string; onChange gets the new one (not the
// event). Every other prop goes to the Select, so ids, disabled, title and
// the ui-select look stay the caller's. jstest/ui-optional-select.test.mjs
// keeps forms from hand-writing the unset/on/off options again.
export const OPTIONAL_ON = '1';
export const OPTIONAL_OFF = '0';

export function OptionalSelect({ value, onChange, unset = 'Default', unsetValue = '', on = 'on', off = 'off', options, ...rest }) {
  const set = options || [[OPTIONAL_ON, on], [OPTIONAL_OFF, off]];
  return html`<${Select}
    ...${rest}
    data-optional-select
    value=${value ?? unsetValue}
    onChange=${(event) => onChange?.(event.currentTarget.value)}
  ><option key=${`unset:${unsetValue}`} value=${unsetValue}>${unset}</option>${set.map(([key, label]) => html`<option key=${key} value=${key}>${label}</option>`)}<//>`;
}
