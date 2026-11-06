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
//   inputAttrs  more attributes on the box itself (aria-invalid, aria-describedby)
//
// An agent's form fill (form-fill.js) sets a choice as it sets a <select>
// (tcl-alhauf). The wrapper is the field: data-value is its value,
// choiceItems what it offers, and CHOICE_FILL_EVENT on it chooses a key (or ''
// for none) as a pick would. A pick of the operator's (a row, or ×) first
// sends CHOICE_PICK_EVENT, which the fill watch takes as a <select> change:
// the field, and whatever the pick moves, are the operator's. An action box (a
// menu) holds no value, so it is no field.
import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { FilterCombobox } from './filter-combobox.js';
import { CHOICE_FILL_EVENT, CHOICE_PICK_EVENT } from './choice-fill.js';

const html = htm.bind(h);

// CHOICE_CAP is how many rows a choice lists at once; past it, nothing typed
// shows the groups to narrow by, and a query its best matches.
export const CHOICE_CAP = 30;


export function ChoiceCombobox({
  id, value = '', onChange, items = [], noneLabel = '', clearable = false, className = '', action = false, missing = 'missing', floating = true,
  placeholder = 'Type to find…', emptyHint = 'No matches.', ariaLabel = 'Find', disabled = false, cap = CHOICE_CAP, attrs = {}, inputAttrs = {},
}) {
  const chosen = !action && value ? items.find((item) => item.key === value) : null;
  // What the box shows while it is not being typed in, as a <select> shows
  // its option: the chosen value, or what none means here.
  const selectedLabel = action ? '' : value ? (chosen ? chosen.label || chosen.key : `${value} (${missing})`) : noneLabel;
  const wrapper = useRef(null);
  // This render's, for the fill listener bound once.
  const latest = useRef(null);
  latest.current = { items, onChange, value, disabled, clearable: !!(noneLabel || clearable) };
  const fillable = !action && !!id;
  useEffect(() => {
    const el = wrapper.current;
    if (!el) return;
    el.choiceItems = fillable ? items : undefined;
    el.choiceClearable = fillable && !!(noneLabel || clearable);
  });
  useEffect(() => {
    const el = wrapper.current;
    if (!el || !fillable) return undefined;
    // A fill names a key the list offers, or '' (form-fill.js checks an
    // agent's value first; an undo can put back the none it found).
    const onFill = (event) => {
      const now = latest.current;
      const key = String(event.detail ?? '');
      if (now.disabled || key === now.value) return;
      if (key && !now.items.some((item) => item.key === key && !item.disabled)) return;
      now.onChange(key);
    };
    el.addEventListener(CHOICE_FILL_EVENT, onFill);
    return () => el.removeEventListener(CHOICE_FILL_EVENT, onFill);
  }, [fillable]);
  // picked is the operator choosing key: the fill watch hears of it first.
  const picked = (key) => {
    const el = wrapper.current;
    const View = el?.ownerDocument?.defaultView;
    if (fillable && View?.CustomEvent) el.dispatchEvent(new View.CustomEvent(CHOICE_PICK_EVENT, { bubbles: true }));
    onChange(key);
  };
  return html`<div ...${attrs} ref=${wrapper} data-fill-choice=${fillable ? '' : undefined}
    class=${`choice-combobox${className ? ` ${className}` : ''}${disabled ? ' disabled' : ''}${value && !chosen && !action ? ' missing' : ''}`}
    id=${id} data-value=${action ? '' : value || ''}
    onClick=${(event) => event.preventDefault()}
    onKeyDown=${(event) => { if (event.key === 'Enter') event.preventDefault(); }}>
    <${FilterCombobox} id=${id ? `${id}-list` : undefined} inputId=${id ? `${id}-input` : undefined} items=${items}
      value=${action ? '' : value || ''} multi=${false} cap=${cap} ariaLabel=${ariaLabel} disabled=${disabled}
      selectedLabel=${selectedLabel} overlay=${floating} placeholder=${placeholder} emptyHint=${emptyHint} inputAttrs=${inputAttrs}
      onChange=${(key) => {
        // Picking the chosen row again keeps it, as a <select> does: only ×
        // empties a choice, and only where none is one.
        if (action) { if (key) onChange(key); } else if (key && key !== value) picked(key);
      }} />
    ${!action && value && (noneLabel || clearable) && !disabled ? html`<${Button} size="sm" variant="ghost" class="choice-combobox-clear"
      title=${noneLabel ? `Clear: ${noneLabel}` : 'Clear'} aria-label=${noneLabel ? `Clear: ${noneLabel}` : 'Clear'} onClick=${() => picked('')}>×<//>` : null}
  </div>`;
}
