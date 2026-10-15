import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { loadSavedPrompts, saveSavedPrompt, deleteSavedPrompt } from './saved-prompts-actions.js';

const html = htm.bind(h);
const blank = () => ({ name: '', body: '' });

function SavedPromptManager({ prompts, onClose, onChanged, confirmDiscard }) {
  const [original, setOriginal] = useState(null);
  const [draft, setDraft] = useState(blank);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const [deleting, setDeleting] = useState(false);
  const dirty = draft.name !== (original?.name || '') || draft.body !== (original?.body || '');
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const nameRef = useRef(null);
  const choose = async (name) => {
    if (dirty && !await confirmDiscard()) return;
    const prompt = prompts.find((item) => item.name === name) || null;
    setOriginal(prompt); setDraft(prompt ? { ...prompt } : blank());
    setError(''); setDeleting(false);
  };
  const mutate = async (remove = false) => {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setError('');
    try {
      if (remove) await deleteSavedPrompt(original.name);
      else await saveSavedPrompt(original?.name, draft);
      // Clear the saved draft before refreshing so a failed refresh never
      // leaves a successful create looking like an unsaved, retryable write.
      setOriginal(null); setDraft(blank()); setDeleting(false);
      await onChanged();
    } catch (cause) { setError(cause.message); }
    finally { busyRef.current = false; setBusy(false); }
  };
  return html`<div onKeyDown=${(event) => {
    // Keep the parent composer's submit hotkey from sending while editing.
    // Escape and Tab must reach the shared document-level dialog controller.
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) event.stopPropagation();
  }}>
    <${Overlay} id="saved-prompts-manage-modal" labelledby="saved-prompts-title"
      onClose=${onClose} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}
      initialFocusRef=${nameRef} registerClose=${registerClose}>
      <h3 id="saved-prompts-title">Saved prompts</h3>
      <p class="modal-hint">Reusable text shared across all groups. Insert a prompt into a message, then edit before sending.</p>
      <label class="cron-create-row"><span class="cron-create-label">Edit prompt</span>
        <select aria-label="Edit saved prompt" disabled=${busy} value=${original?.name || ''}
          onChange=${(event) => {
            const name = event.currentTarget.value;
            event.currentTarget.value = original?.name || '';
            void choose(name);
          }}>
          <option value="">New prompt</option>
          ${prompts.map((prompt) => html`<option key=${prompt.name} value=${prompt.name}>${prompt.name}</option>`)}
        </select></label>
      <label class="cron-create-row"><span class="cron-create-label">Name</span>
        <input ref=${nameRef} aria-label="Prompt name" value=${draft.name} disabled=${busy}
          placeholder="e.g. cold-review" autocomplete="off"
          onInput=${(event) => setDraft({ ...draft, name: event.currentTarget.value })}/></label>
      <label class="cron-create-row"><span class="cron-create-label">Body</span>
        <textarea aria-label="Prompt body" rows="8" maxlength="16384" value=${draft.body} disabled=${busy}
          onInput=${(event) => setDraft({ ...draft, body: event.currentTarget.value })}/></label>
      <div role="alert" class="cron-create-error">${error}</div>
      <div class="modal-buttons">
        <button type="button" disabled=${busy} onClick=${() => void requestClose()}>Close</button>
        ${original ? html`<button type="button" disabled=${busy} onClick=${() => {
          if (deleting) void mutate(true); else setDeleting(true);
        }}>${deleting ? 'Confirm delete' : 'Delete prompt'}</button>` : null}
        <span class="spacer"></span>
        <button type="button" class="primary" disabled=${busy || !draft.name.trim() || !draft.body.trim()}
          onClick=${() => void mutate()}>${busy ? 'Saving…' : 'Save prompt'}</button>
      </div>
    </${Overlay}>
  </div>`;
}

export function SavedPromptPicker({ value, onChange, textareaRef, disabled = false, confirmDiscard }) {
  const [prompts, setPrompts] = useState([]);
  const [error, setError] = useState('');
  const [managing, setManaging] = useState(false);
  const [caret, setCaret] = useState(null);
  const live = useRef(true);
  const reload = async () => {
    const list = await loadSavedPrompts();
    if (live.current) { setPrompts(list); setError(''); }
  };
  useEffect(() => {
    live.current = true;
    void reload().catch((cause) => { if (live.current) setError(cause.message); });
    return () => { live.current = false; };
  }, []);
  useLayoutEffect(() => {
    if (caret === null) return;
    textareaRef.current?.focus();
    textareaRef.current?.setSelectionRange?.(caret, caret);
    setCaret(null);
  }, [caret]);
  const insert = (event) => {
    const prompt = prompts.find((item) => item.name === event.currentTarget.value);
    event.currentTarget.value = '';
    if (!prompt) return;
    const field = textareaRef.current;
    const start = field?.selectionStart ?? value.length;
    const end = field?.selectionEnd ?? start;
    const next = value.slice(0, start) + prompt.body + value.slice(end);
    if (new TextEncoder().encode(next).length > 16384) { setError('This prompt would exceed the 16384-byte message limit.'); return; }
    setError(''); onChange(next); setCaret(start + prompt.body.length);
  };
  return html`<div class="cron-create-row saved-prompts-row">
    <span class="cron-create-label">Saved prompts</span>
    <div class="cron-create-target">
      <select aria-label="Insert saved prompt" disabled=${disabled || !prompts.length} value="" onChange=${insert}>
        <option value="">${prompts.length ? 'Insert prompt…' : 'No saved prompts'}</option>
        ${prompts.map((prompt) => html`<option key=${prompt.name} value=${prompt.name}>${prompt.name}</option>`)}
      </select>
      <button type="button" disabled=${disabled} onClick=${() => setManaging(true)}>Manage prompts…</button>
      ${error ? html`<div class="cron-create-error" role="alert">${error} <button type="button" onClick=${() => void reload().catch((cause) => setError(cause.message))}>Retry</button></div>` : null}
    </div>
    ${managing ? html`<${SavedPromptManager} prompts=${prompts} onClose=${() => setManaging(false)}
      onChanged=${reload} confirmDiscard=${confirmDiscard}/>` : null}
  </div>`;
}
