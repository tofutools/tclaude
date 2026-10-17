import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import {
  loadSavedPrompts, saveSavedPrompt, deleteSavedPrompt,
  savedPromptsExportBlob, parseSavedPromptsImport,
} from './saved-prompts-actions.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { downloadBlob } from './paf-model.js';

const html = htm.bind(h);
const blank = () => ({ name: '', body: '' });

// cloneName picks the first free "<name>-copy[-n]" handle for a clone draft.
function cloneName(name, prompts) {
  const taken = new Set(prompts.map((prompt) => prompt.name));
  let candidate = `${name}-copy`;
  for (let n = 2; taken.has(candidate); n += 1) candidate = `${name}-copy-${n}`;
  return candidate;
}

// SavedPromptImport previews a parsed import file: which names are new, which
// already exist, and whether existing ones are overwritten or skipped.
function SavedPromptImport({ incoming, prompts, busy, onImport, onCancel }) {
  const [overwrite, setOverwrite] = useState(false);
  const existing = new Map(prompts.map((prompt) => [prompt.name, prompt]));
  const fresh = incoming.filter((prompt) => !existing.has(prompt.name));
  const changed = incoming.filter((prompt) => existing.has(prompt.name) && existing.get(prompt.name).body !== prompt.body);
  const same = incoming.length - fresh.length - changed.length;
  const writes = fresh.length + (overwrite ? changed.length : 0);
  return html`<div class="saved-prompts-import" id="saved-prompts-import">
    <div class="saved-prompts-import-summary" role="status">
      ${incoming.length} prompt${incoming.length === 1 ? '' : 's'} in file: ${fresh.length} new${changed.length ? `, ${changed.length} differ from existing` : ''}${same ? `, ${same} unchanged` : ''}.
    </div>
    <ul class="saved-prompts-import-list">${incoming.map((prompt) => html`<li key=${prompt.name}>
      <code>${prompt.name}</code> <span class="muted">${!existing.has(prompt.name) ? 'new' : existing.get(prompt.name).body === prompt.body ? 'unchanged' : overwrite ? 'overwrite' : 'skip (exists)'}</span>
    </li>`)}</ul>
    ${changed.length ? html`<label class="saved-prompts-import-overwrite"><input type="checkbox" checked=${overwrite} disabled=${busy}
      onChange=${(event) => setOverwrite(event.currentTarget.checked)}/> Overwrite the ${changed.length} existing prompt${changed.length === 1 ? '' : 's'} that differ</label>` : null}
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onCancel}>Cancel import</button>
      <span class="spacer"></span>
      <button type="button" class="primary" id="saved-prompts-import-submit" disabled=${busy || !writes}
        onClick=${() => onImport([...fresh.map((prompt) => ['', prompt]), ...(overwrite ? changed.map((prompt) => [prompt.name, prompt]) : [])])}>
        ${busy ? 'Importing…' : `Import ${writes}`}</button>
    </div>
  </div>`;
}

function SavedPromptManager({ prompts, onClose, onChanged, confirmDiscard, initial = '', clone = false }) {
  const seed = prompts.find((item) => item.name === initial) || null;
  const [original, setOriginal] = useState(clone ? null : seed);
  const [draft, setDraft] = useState(() => (seed
    ? { name: clone ? cloneName(seed.name, prompts) : seed.name, body: seed.body } : blank()));
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const [deleting, setDeleting] = useState(false);
  const [incoming, setIncoming] = useState(null);
  const fileRef = useRef(null);
  const baseline = clone ? blank() : (original || blank());
  const dirty = draft.name !== baseline.name || draft.body !== baseline.body;
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const nameRef = useRef(null);
  const choose = async (name) => {
    if (dirty && !await confirmDiscard()) return;
    const prompt = prompts.find((item) => item.name === name) || null;
    setOriginal(prompt); setDraft(prompt ? { ...prompt } : blank());
    setError(''); setDeleting(false);
  };
  const startClone = async () => {
    if (dirty && !await confirmDiscard()) return;
    const source = original;
    setOriginal(null); setDraft({ name: cloneName(source.name, prompts), body: source.body });
    setError(''); setDeleting(false);
    nameRef.current?.focus();
  };
  const run = async (work) => {
    if (busyRef.current) return;
    busyRef.current = true; setBusy(true); setError('');
    try { await work(); } catch (cause) { setError(cause.message); } finally { busyRef.current = false; setBusy(false); }
  };
  const mutate = (remove = false) => run(async () => {
    if (remove) await deleteSavedPrompt(original.name);
    else await saveSavedPrompt(original?.name, draft);
    // Clear the saved draft before refreshing so a failed refresh never
    // leaves a successful create looking like an unsaved, retryable write.
    setOriginal(null); setDraft(blank()); setDeleting(false);
    await onChanged();
  });
  const importPrompts = (writes) => run(async () => {
    let done = 0;
    try {
      for (const [previous, prompt] of writes) {
        await saveSavedPrompt(previous, prompt);
        done += 1;
      }
    } finally {
      if (done) await onChanged();
    }
    setIncoming(null);
    shellToast(`imported ${done} saved prompt${done === 1 ? '' : 's'}`);
  });
  const readImport = async (file) => {
    if (!file) return;
    setError('');
    try { setIncoming(parseSavedPromptsImport(await file.text())); } catch (cause) { setError(`Import failed: ${cause.message}`); }
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
      <p class="modal-hint">Reusable text shared across all groups. Add prompts to a spawn, profile or group so their text opens the agent's task brief, or insert one into a message and edit before sending.</p>
      <div class="filter-bar saved-prompts-transfer">
        <span class="spacer"></span>
        <button type="button" class="tool" id="saved-prompts-export" disabled=${busy || !prompts.length}
          title="Download every saved prompt as a JSON file" onClick=${() => downloadBlob(savedPromptsExportBlob(prompts), 'tclaude-saved-prompts.json')}>⇪ export</button>
        <button type="button" class="tool" id="saved-prompts-import-open" disabled=${busy}
          title="Load saved prompts from an exported JSON file; you confirm before anything is written" onClick=${() => fileRef.current?.click()}>⤒ import</button>
        <input ref=${fileRef} type="file" accept="application/json,.json" hidden
          onChange=${(event) => { void readImport(event.currentTarget.files?.[0]); event.currentTarget.value = ''; }}/>
      </div>
      ${incoming ? html`<${SavedPromptImport} incoming=${incoming} prompts=${prompts} busy=${busy}
        onImport=${(writes) => void importPrompts(writes)} onCancel=${() => setIncoming(null)}/>` : null}
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
        }}>${deleting ? 'Confirm delete' : 'Delete prompt'}</button>
        <button type="button" id="saved-prompts-clone" disabled=${busy} onClick=${() => void startClone()}>Clone</button>` : null}
        <span class="spacer"></span>
        <button type="button" class="primary" disabled=${busy || !draft.name.trim() || !draft.body.trim()}
          onClick=${() => void mutate()}>${busy ? 'Saving…' : 'Save prompt'}</button>
      </div>
    </${Overlay}>
  </div>`;
}

function useSavedPrompts() {
  const [prompts, setPrompts] = useState([]);
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  const live = useRef(true);
  const reload = async () => {
    const list = await loadSavedPrompts();
    if (live.current) { setPrompts(list); setError(''); }
  };
  useEffect(() => {
    live.current = true;
    void reload().catch((cause) => { if (live.current) setError(cause.message); })
      .finally(() => { if (live.current) setLoaded(true); });
    return () => { live.current = false; };
  }, []);
  return { prompts, error, setError, reload, loaded };
}

// SavedPromptsDialog is the manager opened on its own — from the dock's
// Saved prompts section — rather than from a composer. initial preselects a
// prompt; clone starts a copy of it. The dock refreshes off the snapshot poll.
export function SavedPromptsDialog({ descriptor, onClose, confirmDiscard }) {
  const { prompts, error, reload, loaded } = useSavedPrompts();
  useEffect(() => { if (error) shellToast(error, true); }, [error]);
  if (!loaded) return null;
  return html`<${SavedPromptManager} prompts=${prompts} initial=${descriptor?.initial || ''} clone=${!!descriptor?.clone}
    onClose=${onClose} onChanged=${reload} confirmDiscard=${confirmDiscard}/>`;
}

// SavedPromptChips selects an ordered set of saved prompts with the same chip
// + "Add…" control the role pickers use. value/onChange carry prompt names.
export function SavedPromptChips({ id, value = [], onChange, disabled = false, confirmDiscard = shellConfirmDiscard, hint = '' }) {
  const { prompts, error, setError, reload } = useSavedPrompts();
  const [inspected, setInspected] = useState('');
  const [managing, setManaging] = useState(false);
  const selected = value.map((name) => prompts.find((prompt) => prompt.name === name) || { name, missing: prompts.length > 0 });
  const shown = selected.find((prompt) => prompt.name === inspected);
  const available = prompts.filter((prompt) => !value.includes(prompt.name));
  return html`<div class="agent-spawn-role-picker saved-prompt-picker">
    <div class="agent-spawn-role-chips" id=${id}>
      ${selected.map((prompt) => html`<span key=${prompt.name} class=${`agent-spawn-role-chip${prompt.missing ? ' missing' : ''}`}>
        <button type="button" class="role-chip-inspect" disabled=${disabled}
          title=${prompt.missing ? 'This prompt is missing from the library' : prompt.body}
          aria-expanded=${inspected === prompt.name ? 'true' : 'false'}
          onClick=${() => setInspected(inspected === prompt.name ? '' : prompt.name)}>${prompt.name}</button>
        <button type="button" class="role-chip-remove" disabled=${disabled} title=${`Remove ${prompt.name}`}
          aria-label=${`Remove prompt ${prompt.name}`} onClick=${() => {
            setInspected((current) => current === prompt.name ? '' : current);
            onChange(value.filter((name) => name !== prompt.name));
          }}>×</button>
      </span>`)}
      <select class="saved-prompt-chip-add" value="" disabled=${disabled || !available.length}
        aria-label="Add saved prompt" onChange=${(event) => {
          const next = event.currentTarget.value;
          event.currentTarget.value = '';
          if (next) onChange([...value, next]);
        }}>
        <option value="">${prompts.length ? '＋ Add prompt…' : 'No saved prompts'}</option>
        ${available.map((prompt) => html`<option key=${prompt.name} value=${prompt.name}>${prompt.name}</option>`)}
      </select>
      <button type="button" class="tool saved-prompt-chip-manage" disabled=${disabled}
        title="Create, edit, clone, import or export saved prompts" onClick=${() => setManaging(true)}>Manage…</button>
    </div>
    ${hint ? html`<div class="spawn-field-hint">${hint}</div>` : null}
    ${shown && html`<div class=${`agent-spawn-role-inspect${shown.missing ? ' missing' : ''}`}>
      ${shown.missing ? '⚠ This prompt is no longer in the library.' : html`<div class="saved-prompt-inspect-body">${shown.body}</div>`}
    </div>`}
    ${error ? html`<div class="cron-create-error" role="alert">${error} <button type="button" onClick=${() => void reload().catch((cause) => setError(cause.message))}>Retry</button></div>` : null}
    ${managing ? html`<${SavedPromptManager} prompts=${prompts} onClose=${() => setManaging(false)}
      onChanged=${reload} confirmDiscard=${confirmDiscard}/>` : null}
  </div>`;
}

export function SavedPromptPicker({ value, onChange, textareaRef, disabled = false, confirmDiscard }) {
  const { prompts, error, setError, reload } = useSavedPrompts();
  const [managing, setManaging] = useState(false);
  const [caret, setCaret] = useState(null);
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
      <div class="cron-target-input-row saved-prompts-controls">
        <select aria-label="Insert saved prompt" disabled=${disabled || !prompts.length} value="" onChange=${insert}>
          <option value="">${prompts.length ? 'Insert prompt…' : 'No saved prompts'}</option>
          ${prompts.map((prompt) => html`<option key=${prompt.name} value=${prompt.name}>${prompt.name}</option>`)}
        </select>
        <button type="button" disabled=${disabled} onClick=${() => setManaging(true)}>Manage prompts…</button>
      </div>
      ${error ? html`<div class="cron-create-error" role="alert">${error} <button type="button" onClick=${() => void reload().catch((cause) => setError(cause.message))}>Retry</button></div>` : null}
    </div>
    ${managing ? html`<${SavedPromptManager} prompts=${prompts} onClose=${() => setManaging(false)}
      onChanged=${reload} confirmDiscard=${confirmDiscard}/>` : null}
  </div>`;
}
