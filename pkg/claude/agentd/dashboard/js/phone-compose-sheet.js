// phone-compose-sheet.js -- the phone's message sheet: a short note to one
// agent, in a bottom sheet over whatever is open (its terminal, Now, Agents,
// Mail). The desktop's "Send message to agent" dialog stays the desktop's;
// on a phone every ✉ Message opens this instead.
//
// It sends exactly as that dialog does (message-access-dialog-actions.js
// sendOperatorMessage): through the agent's mailbox, so it is queued while the
// agent is busy, with attachments uploaded first. Saved prompts insert at the
// caret, as on the desktop. Another node's agent takes text only.
//
// A draft is not lost by accident: Escape, the scrim, a swipe down on the
// grab handle and Cancel ask first while anything is written or attached.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { createMessageAccessDialogActions } from './message-access-dialog-actions.js';
import { loadSavedPrompts } from './saved-prompts-actions.js';
import { nodeFetch } from './node-api.js';
import { openPhoneSheet, requestClosePhoneSheet, useSheetGuard } from './phone-sheet.js';
import { Button } from './ui-components.js';
import { ButtonRow } from './phone-ui.js';

const html = htm.bind(h);

// The daemon's limits on an operator message (as the desktop dialog's).
export const MESSAGE_MAX_BYTES = 16384;
export const MAX_ATTACHMENTS = 8;

const fileName = (file, index) => file?.name || `pasted-image-${index + 1}.png`;

// insertAt puts text into value at the selection [start, end), and says
// where the caret goes after it; null when the result would be over the
// message limit.
export function insertAt(value, text, start = value.length, end = start) {
  const next = value.slice(0, start) + text + value.slice(end);
  if (new TextEncoder().encode(next).length > MESSAGE_MAX_BYTES) return null;
  return { value: next, caret: start + text.length };
}

// The saved prompts, loaded when the sheet first lists them.
function usePrompts(open, node) {
  const [state, setState] = useState({ prompts: null, error: '' });
  useEffect(() => {
    if (!open || state.prompts) return undefined;
    let live = true;
    loadSavedPrompts(node ? nodeFetch(node) : undefined).then(
      (prompts) => { if (live) setState({ prompts: prompts || [], error: '' }); },
      (error) => { if (live) setState({ prompts: null, error: error?.message || String(error) }); },
    );
    return () => { live = false; };
  }, [open]);
  return state;
}

// ComposeSheet is the sheet's body. send(draft) sends it (sendOperatorMessage's
// draft); close() closes the sheet without asking.
export function ComposeSheet({ agent, label = '', node = '', send, close }) {
  const [body, setBody] = useState('');
  const [files, setFiles] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [listing, setListing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [caret, setCaret] = useState(null);
  const field = useRef(null);
  const picker = useRef(null);
  const textOnly = !!node;
  const dirty = !!body || files.length > 0;
  const prompts = usePrompts(listing, node);
  const name = label || 'the agent';

  // A close the operator asked for waits for a yes while there is a draft.
  useSheetGuard(() => {
    if (!dirty && !busy) return false;
    if (!busy) setConfirming(true);
    return true;
  });

  useLayoutEffect(() => {
    if (caret === null) return;
    field.current?.focus();
    field.current?.setSelectionRange?.(caret, caret);
    setCaret(null);
  }, [caret]);

  const addFiles = (incoming) => {
    if (busy || textOnly) return;
    setFiles((now) => [...now, ...Array.from(incoming || []).filter(Boolean)].slice(0, MAX_ATTACHMENTS));
  };
  const insert = (prompt) => {
    const at = field.current;
    const done = insertAt(body, prompt.body || '', at?.selectionStart ?? body.length, at?.selectionEnd ?? undefined);
    if (!done) { setError(`That prompt would take the message over ${MESSAGE_MAX_BYTES} bytes.`); return; }
    setError('');
    setListing(false);
    setBody(done.value);
    setCaret(done.caret);
  };
  const submit = async () => {
    if (busy) return;
    if (!body.trim() && (textOnly || !files.length)) {
      setError(textOnly ? 'Write a message.' : 'Write a message or attach a file.');
      field.current?.focus();
      return;
    }
    setError('');
    setBusy(true);
    try {
      await send({ to: agent, subject: '', body, files: Object.freeze([...files]), ...(node ? { node } : {}) });
      close();
    } catch (cause) {
      setError(cause?.message || String(cause));
      setBusy(false);
    }
  };
  const onKeyDown = (event) => {
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey) && !event.isComposing) {
      event.preventDefault();
      void submit();
    }
  };

  return html`<div class="phone-compose" onPaste=${(event) => {
      const pasted = Array.from(event.clipboardData?.files || []);
      if (!pasted.length || textOnly) return;
      event.preventDefault();
      addFiles(pasted);
    }}>
    <div class="phone-compose-head">
      <h2 class="phone-compose-title">Message ${name}</h2>
      ${!textOnly && html`<button data-ui-exempt type="button" class="phone-compose-chip" disabled=${busy || files.length >= MAX_ATTACHMENTS}
        aria-label="Attach files" title="Attach files" onClick=${() => picker.current?.click()}>📎</button>
        <input ref=${picker} type="file" multiple hidden class="phone-compose-files" disabled=${busy}
          onChange=${(event) => { addFiles(event.currentTarget.files); event.currentTarget.value = ''; }} />`}
      <button data-ui-exempt type="button" class="phone-compose-chip" disabled=${busy}
        aria-expanded=${listing ? 'true' : 'false'} onClick=${() => setListing(!listing)}>Prompts ${listing ? '▴' : '▾'}</button>
    </div>
    ${listing && html`<div class="phone-compose-prompts">
      ${prompts.error ? html`<p class="ui-error-box" role="alert">${prompts.error}</p>`
        : !prompts.prompts ? html`<p class="ui-hint phone-compose-note">Loading…</p>`
          : !prompts.prompts.length ? html`<p class="ui-hint phone-compose-note">No saved prompts yet.</p>`
            : html`<ul class="phone-menu" aria-label="Saved prompts">${prompts.prompts.map((prompt) => html`<li key=${prompt.name}>
              <button data-ui-exempt type="button" class="phone-menu-item phone-compose-prompt" title=${prompt.body}
                onClick=${() => insert(prompt)}>${prompt.label || prompt.name}</button></li>`)}</ul>`}
    </div>`}
    <textarea ref=${field} data-sheet-focus class="phone-compose-body" rows="5" maxlength=${MESSAGE_MAX_BYTES}
      aria-label=${`Message to ${name}`} placeholder="Write a message…" enterkeyhint="enter" spellcheck=${true}
      value=${body} readOnly=${busy} onInput=${(event) => setBody(event.currentTarget.value)} onKeyDown=${onKeyDown}></textarea>
    ${files.length > 0 && html`<ul class="phone-compose-attachments" aria-label="Attachments">
      ${files.map((file, index) => html`<li key=${`${index}:${fileName(file, index)}`} class="phone-compose-att">
        <span class="phone-compose-file">${fileName(file, index)}</span>
        <button data-ui-exempt type="button" class="phone-compose-unattach" disabled=${busy} aria-label=${`Remove ${fileName(file, index)}`}
          onClick=${() => setFiles((now) => now.filter((_, i) => i !== index))}>✕</button>
      </li>`)}
    </ul>`}
    ${error && html`<p class="ui-error-box phone-compose-error" role="alert">${error}</p>`}
    ${confirming
      ? html`<div class="phone-compose-foot" role="alertdialog" aria-label="Discard this message?">
          <span class="phone-compose-ask">Discard this message?</span>
          <${ButtonRow} buttons=${[
            { key: 'keep', label: 'Keep', onClick: () => { setConfirming(false); field.current?.focus(); } },
            { key: 'discard', label: 'Discard', tone: 'no', onClick: close },
          ]} />
        </div>`
      : html`<div class="phone-compose-foot">
          <${Button} class="phone-btn phone-compose-cancel" disabled=${busy} onClick=${requestClosePhoneSheet}>Cancel<//>
          <span class="ui-hint phone-compose-note">${node ? 'text only on another node' : "queued if it's busy"}</span>
          <${Button} class="phone-btn phone-btn-pri phone-compose-send" disabled=${busy} onClick=${() => void submit()}>${busy ? 'Sending…' : 'Send ➤'}<//>
        </div>`}
  </div>`;
}

// openPhoneCompose opens the sheet for seed ({ agent, label, node }), the
// seed every ✉ Message hands onComposeMessage. notify reports a failure to
// set up; send failures show in the sheet.
let actions = null;
export function openPhoneCompose(seed = {}, { notify = () => {}, send = null } = {}) {
  if (!seed.agent) return;
  actions ||= createMessageAccessDialogActions({ notify });
  openPhoneSheet({
    label: `Message ${seed.label || seed.agent}`,
    Body: ComposeSheet,
    props: {
      agent: seed.agent, label: seed.label || '', node: seed.node || '',
      send: send || ((draft) => actions.sendOperatorMessage(draft)),
    },
  });
}
