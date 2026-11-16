// phone-terminal-type.js -- the phone's "type into the terminal" sheet, a
// shell's counterpart of the agent's message sheet (phone-compose-sheet.js).
// A shell takes command text, not a message: what is written here goes to the
// terminal as keystrokes, by the path the terminal's own keys take (the
// widget's paste: the terminal's socket, bracketed when the shell asked for
// it), with no message header. Type leaves the line for more; Type + ⏎
// presses Enter after it.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { openPhoneSheet } from './phone-sheet.js';
import { Button } from './ui-components.js';

const html = htm.bind(h);

// TYPE_MAX_BYTES caps one send, as an operator message is capped
// (phone-compose-sheet.js MESSAGE_MAX_BYTES).
export const TYPE_MAX_BYTES = 16384;

// typeProblem says why text cannot be typed, or '' when it can. Enter alone
// (nothing written) is a send: it runs the line already in the terminal.
export function typeProblem(text, enter) {
  if (!text && !enter) return 'Write something to type.';
  if (new TextEncoder().encode(text).length > TYPE_MAX_BYTES) return `That is over ${TYPE_MAX_BYTES} bytes.`;
  return '';
}

// A command is short and the terminal keeps what was run, so unlike the
// message sheet a dismissed draft is not asked about.
//
// TypeSheet is the sheet's body. type(text, enter) types it and says whether
// the terminal took it; close() closes the sheet.
export function TypeSheet({ name = 'the terminal', type, close }) {
  const [text, setText] = useState('');
  const [error, setError] = useState('');
  const send = (enter) => {
    const problem = typeProblem(text, enter);
    if (problem) { setError(problem); return; }
    if (!type(text, enter)) { setError('The terminal took no input (not connected, or view only), so nothing was typed.'); return; }
    close();
  };
  return html`<div class="phone-compose phone-type">
    <div class="phone-compose-head"><h2 class="phone-compose-title">Type into ${name}</h2></div>
    <textarea name="terminal-input" data-sheet-focus class="phone-compose-body" rows="3" maxlength=${TYPE_MAX_BYTES}
      aria-label=${`Text to type into ${name}`} placeholder="A command, typed as on a keyboard…" autocapitalize="off"
      autocomplete="off" autocorrect="off" spellcheck=${false} enterkeyhint="enter"
      value=${text} onInput=${(event) => { setText(event.currentTarget.value); setError(''); }}></textarea>
    ${error && html`<p class="ui-error-box phone-compose-error" role="alert">${error}</p>`}
    <div class="phone-compose-foot">
      <${Button} class="phone-btn phone-compose-cancel" onClick=${close}>Cancel<//>
      <span class="phone-type-gap"></span>
      <${Button} class="phone-btn phone-type-plain" onClick=${() => send(false)}>Type<//>
      <${Button} class="phone-btn phone-btn-pri phone-type-enter" onClick=${() => send(true)}>Type + ⏎<//>
    </div>
  </div>`;
}

// openTypeSheet opens the sheet over the terminal named name; type(text,
// enter) is the terminal's.
export function openTypeSheet({ name, type }) {
  openPhoneSheet({ label: `Type into ${name}`, Body: TypeSheet, props: { name, type } });
}
