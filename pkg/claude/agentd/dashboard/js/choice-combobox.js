// choice-combobox.js — an in-form choice of one value on FilterCombobox: what
// a flat <select> over a list that can grow to hundreds becomes. Like the
// <select>, the box shows the chosen value (or what none means here) until
// it is typed in; typing filters, the list opens over what follows, × after
// the box clears it where none is allowed, and a value the list no longer
// has stays, marked missing.
//
// It stands in a form the way a <select> did:
//   - the box comes first, so a <label> around the field hands its clicks to
//     the box, never to the ×;
//   - no click in it reaches a <label> around it (which would hand the click
//     to that row's first control, a radio say);
//   - Enter in it never submits a form around it.
//
// Props:
//   id          the wrapper's id; the box is `${id}-input`, its list `${id}-list`
//   value       the chosen key ('' for none)
//   onChange    (key) → void ('' when cleared)
//   items       FilterCombobox items ({ key, label, hint?, group?, lead?, … })
//   noneLabel   what '' means here ("No group", "(any group)"): shown when
//               nothing is chosen, and makes × available; without it a value
//               cannot be cleared (the <select> had no empty option)
//   clearable   × clears the value even with no none label (an agent box,
//               whose empty state needs no words)
//   className   another class on the wrapper (agent-combobox, say)
//   floating    the list opens over what follows (default); false keeps it in
//               the flow, for a box inside a scroll box that would clip it
//   action      the list is a menu: choosing calls onChange and the box stays
//               empty (a "Start in group…" picker)
//   missing     the word after a value the list lacks (default "missing")
//   placeholder, emptyHint, ariaLabel, disabled, cap
//   attrs       more attributes on the wrapper (a data-* hook)
import { h } from 'preact';
import htm from 'htm';
import { Button } from './ui-components.js';
import { FilterCombobox } from './filter-combobox.js';

const html = htm.bind(h);

// CHOICE_CAP is how many rows a choice lists at once; past it, nothing typed
// shows the groups to narrow by, and a query its best matches.
export const CHOICE_CAP = 30;

export function ChoiceCombobox({
  id, value = '', onChange, items = [], noneLabel = '', clearable = false, className = '', action = false, missing = 'missing', floating = true,
  placeholder = 'Type to find…', emptyHint = 'No matches.', ariaLabel = 'Find', disabled = false, cap = CHOICE_CAP, attrs = {},
}) {
  const chosen = !action && value ? items.find((item) => item.key === value) : null;
  // What the box shows while it is not being typed in, as a <select> shows
  // its option: the chosen value, or what none means here.
  const selectedLabel = action ? '' : value ? (chosen ? chosen.label || chosen.key : `${value} (${missing})`) : noneLabel;
  return html`<div ...${attrs} class=${`choice-combobox${className ? ` ${className}` : ''}${disabled ? ' disabled' : ''}${value && !chosen && !action ? ' missing' : ''}`}
    id=${id} data-value=${action ? '' : value || ''}
    onClick=${(event) => event.preventDefault()}
    onKeyDown=${(event) => { if (event.key === 'Enter') event.preventDefault(); }}>
    <${FilterCombobox} id=${id ? `${id}-list` : undefined} inputId=${id ? `${id}-input` : undefined} items=${items}
      value=${action ? '' : value || ''} multi=${false} cap=${cap} ariaLabel=${ariaLabel} disabled=${disabled}
      selectedLabel=${selectedLabel} overlay=${floating} placeholder=${placeholder} emptyHint=${emptyHint}
      onChange=${(key) => {
        // Picking the chosen row again keeps it, as a <select> does: only ×
        // empties a choice, and only where none is one.
        if (action ? key : key && key !== value) onChange(key);
      }} />
    ${!action && value && (noneLabel || clearable) && !disabled ? html`<${Button} size="sm" variant="ghost" class="choice-combobox-clear"
      title=${noneLabel ? `Clear: ${noneLabel}` : 'Clear'} aria-label=${noneLabel ? `Clear: ${noneLabel}` : 'Clear'} onClick=${() => onChange('')}>×<//>` : null}
  </div>`;
}
