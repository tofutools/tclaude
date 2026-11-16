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

// The operator's saved prompts (this node's, as the desktop dialog lists them
// for any agent), loaded when the sheet first lists them.
function usePrompts(open) {
  const [state, setState] = useState({ prompts: null, error: '' });
  useEffect(() => {
    if (!open || state.prompts) return undefined;
    let live = true;
    loadSavedPrompts().then(
      (prompts) => { if (live) setState({ prompts: prompts || [], error: '' }); },
      (error) => { if (live) setState({ prompts: null, error: error?.message || String(error) }); },
    );
    return () => { live = false; };
  }, [open]);
  return state;
}

// To is Write's recipient: a field that finds an agent as the operator
// types, or the one picked, with ✕ to pick again.
function To({ to, query, busy, recipients, onQuery, onPick, onClear }) {
  if (to) {
    return html`<div class="phone-compose-to"><span class="phone-compose-to-label">To</span>
      <span class="phone-compose-att phone-compose-to-picked"><span class="phone-compose-file">${to.label || to.agent}</span>
        <button data-ui-exempt type="button" class="phone-compose-unattach" disabled=${busy}
          aria-label=${`Choose someone other than ${to.label || to.agent}`} onClick=${onClear}>✕</button></span></div>`;
  }
  const hits = matchRecipients(recipients(), query);
  return html`<div class="phone-compose-to">
    <label class="phone-compose-to-field"><span class="phone-compose-to-label">To</span>
      <input name="phone-compose-sheet-input" class="ui-input phone-compose-to-input" data-sheet-focus type="search" autocomplete="off" enterkeyhint="search"
        placeholder="An agent's name or group" aria-label="To" value=${query} onInput=${(event) => onQuery(event.currentTarget.value)} /></label>
    ${hits.length ? html`<ul class="phone-menu phone-compose-recipients" aria-label="Agents">${hits.map((r) => html`<li key=${r.agent}>
      <button data-ui-exempt type="button" class="phone-menu-item phone-compose-recipient" onClick=${() => onPick({ agent: r.agent, label: r.label })}>
        <span class="phone-compose-file">${r.label}</span>${r.group && html`<small>${r.group}</small>`}${!r.online && html`<small>offline</small>`}
      </button></li>`)}</ul>`
      : html`<p class="ui-hint phone-compose-note">No agent matches “${query.trim()}”.</p>`}
  </div>`;
}

// RECIPIENTS_SHOWN is how many agents the To field lists for a query.
export const RECIPIENTS_SHOWN = 6;

// matchRecipients is the agents a To query finds, by name or group, live
// ones first, at most RECIPIENTS_SHOWN.
export function matchRecipients(all, query) {
  const q = String(query || '').trim().toLowerCase();
  const hits = (all || []).filter((r) => !q || `${r.label} ${r.group || ''}`.toLowerCase().includes(q));
  return hits.sort((a, b) => Number(!!b.online) - Number(!!a.online)).slice(0, RECIPIENTS_SHOWN);
}

// ComposeSheet is the sheet's body. send(draft) sends it (sendOperatorMessage's
// draft); close() closes the sheet without asking. title replaces "Message
// <name>" (a reply's "Reply to <name>"), hint the line beside Send, and
// offline, when set, says why nothing can be sent (Send stays off).
// recipients() lists the agents a To field picks from ({ agent, label,
// group, online }): Write, which starts with no one.
export function ComposeSheet({ agent = '', label = '', node = '', title = '', hint = '', offline = '', recipients = null, send, close }) {
  const [to, setTo] = useState(agent ? { agent, label } : null);
  const [query, setQuery] = useState('');
  const [body, setBody] = useState('');
  const [files, setFiles] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [listing, setListing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [caret, setCaret] = useState(null);
  const root = useRef(null);
  const field = useRef(null);
  const picker = useRef(null);
  const textOnly = !!node;
  const dirty = !!body || files.length > 0;
  const prompts = usePrompts(listing);
  const name = to?.label || to?.agent || 'the agent';
  const picking = !!recipients && !to;

  // A close the operator asked for waits for a yes while there is a draft.
  // Asked again while the question is up (Escape, Back), it means Keep.
  useSheetGuard(() => {
    if (!dirty && !busy) return false;
    if (busy) return true;
    if (confirming) keep();
    else setConfirming(true);
    return true;
  });
  const keep = () => { setConfirming(false); setCaret(field.current?.selectionStart ?? body.length); };
  // The question takes focus: its row replaces the Cancel just pressed.
  useLayoutEffect(() => {
    if (confirming) root.current?.querySelector('.phone-compose-foot button')?.focus();
  }, [confirming]);

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
    if (busy || offline) return;
    if (!to) {
      setError('Choose who to send it to.');
      root.current?.querySelector('.phone-compose-to input')?.focus();
      return;
    }
    if (!body.trim() && (textOnly || !files.length)) {
      setError(textOnly ? 'Write a message.' : 'Write a message or attach a file.');
      field.current?.focus();
      return;
    }
    setError('');
    setBusy(true);
    try {
      await send({ to: to.agent, subject: '', body, files: Object.freeze([...files]), ...(node ? { node } : {}) });
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

  return html`<div class="phone-compose" ref=${root} onPaste=${(event) => {
      const pasted = Array.from(event.clipboardData?.files || []);
      if (!pasted.length || textOnly) return;
      event.preventDefault();
      addFiles(pasted);
    }}>
    <div class="phone-compose-head">
      <h2 class="phone-compose-title">${title || (recipients ? 'New message' : `Message ${name}`)}</h2>
      ${!textOnly && html`<button data-ui-exempt type="button" class="phone-compose-chip" disabled=${busy || files.length >= MAX_ATTACHMENTS}
        aria-label="Attach files" title="Attach files" onClick=${() => picker.current?.click()}>📎</button>
        <input name="phone-compose-files" ref=${picker} type="file" multiple hidden class="phone-compose-files" disabled=${busy}
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
    ${recipients && html`<${To} to=${to} query=${query} busy=${busy} recipients=${recipients}
      onQuery=${setQuery} onPick=${(next) => { setTo(next); setQuery(''); setError(''); setCaret(body.length); }}
      onClear=${() => setTo(null)} />`}
    <textarea name="write-a-message" ref=${field} data-sheet-focus=${picking ? undefined : ''} class="phone-compose-body" rows="5" maxlength=${MESSAGE_MAX_BYTES}
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
            { key: 'keep', label: 'Keep', onClick: keep },
            { key: 'discard', label: 'Discard', tone: 'no', onClick: close },
          ]} />
        </div>`
      : html`<div class="phone-compose-foot">
          <${Button} class="phone-btn phone-compose-cancel" disabled=${busy} onClick=${requestClosePhoneSheet}>Cancel<//>
          <span class="ui-hint phone-compose-note">${offline || hint || (node ? 'text only on another node' : "queued if it's busy")}</span>
          <${Button} class="phone-btn phone-btn-pri phone-compose-send" disabled=${busy || !!offline} onClick=${() => void submit()}>${busy ? 'Sending…' : 'Send ➤'}<//>
        </div>`}
  </div>`;
}

// openPhoneCompose opens the sheet for seed ({ agent, label, node }), the
// seed every ✉ Message hands onComposeMessage. notify reports a failure to
// set up; send failures show in the sheet. The rest are ComposeSheet's:
// send replaces the mailbox send (a reply), and recipients makes it Write's
// sheet, which may start with no agent.
let actions = null;
export function openPhoneCompose(seed = {}, { notify = () => {}, send = null, title = '', hint = '', offline = '', recipients = null } = {}) {
  if (!seed.agent && !recipients) return;
  actions ||= createMessageAccessDialogActions({ notify });
  const name = seed.label || seed.agent;
  openPhoneSheet({
    label: title || (name ? `Message ${name}` : 'New message'),
    Body: ComposeSheet,
    props: {
      agent: seed.agent || '', label: seed.label || '', node: seed.node || '', title, hint, offline, recipients,
      send: send || ((draft) => actions.sendOperatorMessage(draft)),
    },
  });
}
