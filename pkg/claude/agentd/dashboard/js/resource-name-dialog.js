// resource-name-dialog.js -- the small "name it first" dialog a drawing board
// or a live page opens before it is made. Every way of making one (the Groups
// tab's ＋, the Collab panel's ＋ New, the command palette) asks through
// askResourceName; the shell island hosts the one dialog (ResourceNameHost).
//
// The dialog says what is made and in which group, and holds one Name field,
// prefilled with the old automatic name and selected, so typing replaces it.
// Enter in the field or Ctrl/Cmd+Enter creates, Escape or Cancel closes
// without making anything. An empty name blocks Create; a refusal from the
// daemon shows in the dialog's error slot and the dialog stays.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { signal } from '@preact/signals';
import { Dialog } from './ui-dialog.js';
import { Button, Input } from './ui-components.js';
import { wizWord } from './slop.js';

const html = htm.bind(h);

// The words per kind, regular and 🧙 (the palette's own wizard words,
// palette-resources.js).
const KIND_COPY = Object.freeze({
  drawing_board: {
    title: (group) => [`New drawing board in ${group}`, `Unroll a scrying slate in ${group}`],
    line: (group) => [`A drawing board shared with ${group}'s agents, opened as a tab.`, `A slate the heroes of ${group} draw on with you.`],
    create: ['Create', 'Unroll the slate'],
    busy: ['Creating…', 'Unrolling…'],
  },
  page: {
    title: (group) => [`New live page in ${group}`, `Raise a looking glass in ${group}`],
    line: (group) => [`A web page a member of ${group} serves, live here for you to view and mark up. It starts blank.`,
      `A glass a hero of ${group} fills with its vision, live for you to scry and mark. It starts clear.`],
    create: ['Create', 'Raise the glass'],
    busy: ['Creating…', 'Raising…'],
  },
});

const pick = ([regular, wizard]) => wizWord(regular, wizard);

// { kind, group, value, create, resolve } while the dialog is up.
export const resourceNameRequest = signal(null);

// askResourceName opens the dialog for kind ('drawing_board' or 'page') in
// group (a name), with value as the Name. create(name) makes the resource and
// throws to refuse; the promise resolves to what it returned, or null when the
// operator cancelled. A request already up is cancelled first.
let requests = 0;

export function askResourceName({ kind, group, value = '', create }) {
  resourceNameRequest.peek()?.resolve(null);
  return new Promise((resolve) => {
    const request = {
      kind, group, value, create, seq: ++requests,
      resolve: (result) => {
        if (resourceNameRequest.peek() === request) resourceNameRequest.value = null;
        resolve(result);
      },
    };
    resourceNameRequest.value = request;
  });
}

export function ResourceNameDialog({ request }) {
  const copy = KIND_COPY[request.kind] || KIND_COPY.drawing_board;
  const [name, setName] = useState(request.value);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const inputRef = useRef(null);
  const submit = async () => {
    if (busy) return;
    const typed = name.trim();
    if (!typed) {
      inputRef.current?.focus();
      return;
    }
    setBusy(true);
    setError('');
    try {
      const result = await request.create(typed);
      request.resolve(result ?? true);
    } catch (cause) {
      setBusy(false);
      setError(cause?.message || String(cause));
    }
  };
  const onKeyDown = (event) => {
    if (event.key !== 'Enter' || event.isComposing || event.keyCode === 229 || event.ctrlKey || event.metaKey) return;
    event.preventDefault();
    void submit();
  };
  // The prefilled name is never empty, so this shows once the field is cleared.
  const invalid = !name.trim();
  const what = request.kind === 'page' ? 'live page' : 'drawing board';
  return html`<${Dialog} id="resource-name-modal" dialogClass="cron-create-modal resource-name-modal"
    title=${pick(copy.title(request.group))} onClose=${() => request.resolve(null)} onSubmitHotkey=${submit}
    busy=${busy} initialFocusRef=${inputRef} error=${error} cancel="Cancel"
    footer=${html`<${Button} variant="primary" id="resource-name-submit" disabled=${busy || invalid} busy=${busy}
      onClick=${() => { void submit(); }}>${pick(busy ? copy.busy : copy.create)}<//>`}>
    <p class="modal-hint ui-hint">${pick(copy.line(request.group))}</p>
    <label class="cron-create-row">
      <span class="cron-create-label">Name</span>
      <${Input} domRef=${inputRef} id="resource-name-input" value=${name} invalid=${invalid} disabled=${busy}
        aria-label=${`Name of the new ${what}`} aria-describedby=${invalid ? 'resource-name-invalid' : undefined}
        data-select-on-focus="" autocomplete="off" spellcheck="false"
        onInput=${(event) => { setName(event.currentTarget.value); setError(''); }} onKeyDown=${onKeyDown} />
    </label>
    ${invalid && html`<div class="ui-error resource-name-invalid" id="resource-name-invalid" role="alert">${pick(['Give it a name.', 'It needs a name.'])}</div>`}
  <//>`;
}

// ResourceNameHost shows the dialog a creation asked for.
export function ResourceNameHost() {
  const request = resourceNameRequest.value;
  if (!request) return null;
  return html`<${ResourceNameDialog} key=${request.seq} request=${request} />`;
}
