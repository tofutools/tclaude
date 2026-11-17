// typed-field.js: a text field that keeps what the operator typed across
// re-renders until it commits. Shared by the inline editor and the full
// editor's rule rows.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

// TypedField is a text input or textarea that commits on change. Until then
// what the operator typed lives only in the field, and the editor re-renders
// meanwhile (the dashboard's snapshot poll does, every couple of seconds): a
// plain value= field would be reset to the committed text each time. So what
// they type is kept here, against the value it was typed over, and shows
// until a new value arrives (their own commit, an undo, a merge).
export function TypedField({ as = 'input', value, onChange, ...rest }) {
  const [typed, setTyped] = useState(null);
  const shown = typed && typed.over === value ? typed.text : value;
  // A new value drops what was typed over the old one for good, so it cannot
  // come back should the value later return to the old one.
  useEffect(() => { if (typed && typed.over !== value) setTyped(null); }, [value]);
  return html`<${as} ...${rest} value=${shown}
    onInput=${(event) => setTyped({ over: value, text: event.currentTarget.value })}
    onChange=${(event) => { setTyped(null); onChange(event); }} />`;
}
