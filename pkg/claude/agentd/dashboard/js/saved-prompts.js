import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { PromptCombobox, namedItems } from './registry-picker.js';
import { useGuardedOverlayClose } from './management-overlay.js';
import { Dialog } from './ui-dialog.js';
import {
  loadSavedPrompts, saveSavedPrompt, deleteSavedPrompt,
  savedPromptsExportBlob, parseSavedPromptsImport,
} from './saved-prompts-actions.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { downloadBlob } from './download.js';
import { announceNodeChanged, nodeFetch } from './node-api.js';

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
      <${Button} disabled=${busy} onClick=${onCancel}>Cancel import</${Button}>
      <span class="spacer"></span>
      <${Button} variant="primary" id="saved-prompts-import-submit" disabled=${busy || !writes}
        onClick=${() => onImport([...fresh.map((prompt) => ['', prompt]), ...(overwrite ? changed.map((prompt) => [prompt.name, prompt]) : [])])}>
        ${busy ? 'Importing…' : `Import ${writes}`}</${Button}>
    </div>
  </div>`;
}

function SavedPromptManager({ prompts, onClose, onChanged, confirmDiscard, initial = '', clone = false, node = '' }) {
  const api = node ? nodeFetch(node) : undefined;
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
    if (remove) await deleteSavedPrompt(original.name, api);
    else await saveSavedPrompt(original?.name, draft, api);
    // Clear the saved draft before refreshing so a failed refresh never
    // leaves a successful create looking like an unsaved, retryable write.
    setOriginal(null); setDraft(blank()); setDeleting(false);
    await onChanged();
  });
  const importPrompts = (writes) => run(async () => {
    let done = 0;
    try {
      for (const [previous, prompt] of writes) {
        await saveSavedPrompt(previous, prompt, api);
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
    <${Dialog} id="saved-prompts-manage-modal"
      onClose=${onClose} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}
      initialFocusRef=${nameRef} guard=${{ requestClose, registerClose }} titleId="saved-prompts-title" title="Saved prompts">
      <p class="modal-hint ui-hint">Reusable text shared across all groups. Add prompts to a spawn, profile or group so their text opens the agent's task brief, or insert one into a message and edit before sending.</p>
      <div class="filter-bar saved-prompts-transfer">
        <span class="spacer"></span>
        <${Button} id="saved-prompts-export" disabled=${busy || !prompts.length}
          title="Download every saved prompt as a JSON file" onClick=${() => downloadBlob(savedPromptsExportBlob(prompts), 'tclaude-saved-prompts.json')}>⇪ export<//>
        <${Button} id="saved-prompts-import-open" disabled=${busy}
          title="Load saved prompts from an exported JSON file; you confirm before anything is written" onClick=${() => fileRef.current?.click()}>⤒ import<//>
        <input ref=${fileRef} type="file" accept="application/json,.json" hidden
          onChange=${(event) => { void readImport(event.currentTarget.files?.[0]); event.currentTarget.value = ''; }}/>
      </div>
      ${incoming ? html`<${SavedPromptImport} incoming=${incoming} prompts=${prompts} busy=${busy}
        onImport=${(writes) => void importPrompts(writes)} onCancel=${() => setIncoming(null)}/>` : null}
      <label class="cron-create-row"><span class="cron-create-label">Edit prompt</span>
        <${PromptCombobox} id="saved-prompts-edit" ariaLabel="Edit saved prompt" disabled=${busy} value=${original?.name || ''}
          noneLabel="New prompt" entries=${prompts} onChange=${(name) => void choose(name)} /></label>
      <label class="cron-create-row"><span class="cron-create-label">Name</span>
        <input ref=${nameRef} aria-label="Prompt name" value=${draft.name} disabled=${busy}
          placeholder="e.g. cold-review" autocomplete="off"
          onInput=${(event) => setDraft({ ...draft, name: event.currentTarget.value })}/></label>
      <label class="cron-create-row"><span class="cron-create-label">Body</span>
        <textarea aria-label="Prompt body" rows="8" maxlength="16384" value=${draft.body} disabled=${busy}
          onInput=${(event) => setDraft({ ...draft, body: event.currentTarget.value })}/></label>
      <div role="alert" class="cron-create-error ui-error-box">${error}</div>
      <div class="modal-buttons">
        <${Button} disabled=${busy} onClick=${() => void requestClose()}>Close</${Button}>
        ${original ? html`<${Button} disabled=${busy} onClick=${() => {
          if (deleting) void mutate(true); else setDeleting(true);
        }}>${deleting ? 'Confirm delete' : 'Delete prompt'}</${Button}>
        <${Button} id="saved-prompts-clone" disabled=${busy} onClick=${() => void startClone()}>Clone</${Button}>` : null}
        <span class="spacer"></span>
        <${Button} variant="primary" disabled=${busy || !draft.name.trim() || !draft.body.trim()}
          onClick=${() => void mutate()}>${busy ? 'Saving…' : 'Save prompt'}</${Button}>
      </div>
    </${Dialog}>
  </div>`;
}

// useSavedPrompts reads node's library (blank: the page's node).
function useSavedPrompts(enabled = true, node = '') {
  const [prompts, setPrompts] = useState([]);
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  const live = useRef(true);
  const reload = async () => {
    const list = await loadSavedPrompts(node ? nodeFetch(node) : undefined);
    if (live.current) { setPrompts(list); setError(''); }
  };
  useEffect(() => {
    live.current = true;
    if (!enabled) return () => { live.current = false; };
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
  // descriptor.node: another node's library, edited in place (blank: the
  // page's); its dock card follows once that node's snapshot is re-read.
  const node = descriptor?.node || '';
  const { prompts, error, reload, loaded } = useSavedPrompts(true, node);
  const changed = () => { if (node) announceNodeChanged(node); return reload(); };
  useEffect(() => { if (error) shellToast(error, true); }, [error]);
  if (!loaded) return null;
  return html`<${SavedPromptManager} prompts=${prompts} initial=${descriptor?.initial || ''} clone=${!!descriptor?.clone}
    node=${node} onClose=${onClose} onChanged=${changed} confirmDiscard=${confirmDiscard}/>`;
}

// SavedPromptChips selects an ordered set of saved prompts with the same chip
// + "Add…" control the role pickers use (that chip-styled select stays
// data-ui-exempt, as theirs does). value/onChange carry prompt names.
//
// node names the node whose library the chips pick from and Manage… edits
// (node-api.js; blank: the page's). library replaces it with a given list (a remote
// spawn's two libraries, agent-spawn-remote.js): each prompt may carry a
// label and a source, sources groups the Add list ({source, label}, in order),
// and visible limits it to some of them. locked names chips that cannot be
// removed, with lockedReason as their hover text.
export function SavedPromptChips({
  id, value = [], onChange, disabled = false, confirmDiscard = shellConfirmDiscard, hint = '',
  library = null, sources = [], visible = null, locked = [], lockedReason = '', node = '',
}) {
  const own = useSavedPrompts(!library, node);
  const { error, setError, reload } = own;
  const prompts = library || own.prompts;
  const [inspected, setInspected] = useState('');
  const [managing, setManaging] = useState(false);
  const fixed = new Set(locked);
  const selected = value.map((name) => prompts.find((prompt) => prompt.name === name)
    || { name, missing: !fixed.has(name) && prompts.length > 0, unknown: fixed.has(name) });
  const shown = selected.find((prompt) => prompt.name === inspected);
  const available = prompts.filter((prompt) => !value.includes(prompt.name)
    && (!visible || visible.includes(prompt.source)));
  const groups = library && sources.length
    ? sources.map((group) => ({ ...group, prompts: available.filter((prompt) => prompt.source === group.source) })).filter((group) => group.prompts.length)
    : null;
  const chipLabel = (prompt) => {
    const source = sources.find((group) => group.source === prompt.source);
    return library && source?.chip ? `${prompt.label || prompt.name} · ${source.chip}` : prompt.label || prompt.name;
  };
  return html`<div class="agent-spawn-role-picker saved-prompt-picker">
    <div class="agent-spawn-role-chips" id=${id}>
      ${selected.map((prompt) => html`<span key=${prompt.name} class=${`agent-spawn-role-chip${prompt.missing ? ' missing' : ''}${fixed.has(prompt.name) ? ' locked' : ''}`}>
        <button data-ui-exempt type="button" class="role-chip-inspect" disabled=${disabled} data-remote-spawn-free=""
          title=${prompt.missing ? 'This prompt is missing from the library' : prompt.unknown ? lockedReason : prompt.body}
          aria-expanded=${inspected === prompt.name ? 'true' : 'false'}
          onClick=${() => setInspected(inspected === prompt.name ? '' : prompt.name)}>${chipLabel(prompt)}</button>
        <button data-ui-exempt type="button" class="role-chip-remove" disabled=${disabled || fixed.has(prompt.name)} data-remote-spawn-field="prompts"
          title=${fixed.has(prompt.name) ? lockedReason : `Remove ${prompt.name}`}
          aria-label=${`Remove prompt ${prompt.name}`} onClick=${() => {
            setInspected((current) => current === prompt.name ? '' : current);
            onChange(value.filter((name) => name !== prompt.name));
          }}>×</button>
      </span>`)}
      <${PromptCombobox} id=${id ? `${id}-add` : undefined} className="saved-prompt-chip-add" action=${true} value=""
        disabled=${disabled || !available.length} attrs=${{ 'data-remote-spawn-field': 'prompts' }}
        ariaLabel="Add saved prompt" placeholder=${prompts.length ? '＋ Add prompt…' : 'No saved prompts'}
        items=${namedItems(available.map((prompt) => ({
          name: prompt.name, label: prompt.label || prompt.name, body: prompt.body || '',
          group: groups ? sources.find((group) => group.source === prompt.source)?.label || '' : '',
        })))}
        onChange=${(next) => { if (next) onChange([...value, next]); }} />
      ${library ? null : html`<${Button} size="sm" class="saved-prompt-chip-manage" disabled=${disabled}
        title="Create, edit, clone, import or export saved prompts" onClick=${() => setManaging(true)}>Manage…<//>`}
    </div>
    ${hint ? html`<div class="spawn-field-hint ui-hint">${hint}</div>` : null}
    ${shown && html`<div class=${`agent-spawn-role-inspect${shown.missing ? ' missing' : ''}`}>
      ${shown.missing ? '⚠ This prompt is no longer in the library.' : shown.unknown ? lockedReason : html`<div class="saved-prompt-inspect-body">${shown.body}</div>`}
    </div>`}
    ${error ? html`<div class="cron-create-error ui-error-box" role="alert">${error} <${Button} size="sm" onClick=${() => void reload().catch((cause) => setError(cause.message))}>Retry<//></div>` : null}
    ${managing ? html`<${SavedPromptManager} prompts=${prompts} node=${node} onClose=${() => setManaging(false)}
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
  const insert = (name) => {
    const prompt = prompts.find((item) => item.name === name);
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
        <${PromptCombobox} className="saved-prompt-insert" action=${true} value="" ariaLabel="Insert saved prompt"
          disabled=${disabled || !prompts.length} placeholder=${prompts.length ? 'Insert prompt…' : 'No saved prompts'}
          entries=${prompts} onChange=${insert} />
        <${Button} size="sm" disabled=${disabled} onClick=${() => setManaging(true)}>Manage prompts…<//>
      </div>
      ${error ? html`<div class="cron-create-error ui-error-box" role="alert">${error} <${Button} size="sm" onClick=${() => void reload().catch((cause) => setError(cause.message))}>Retry<//></div>` : null}
    </div>
    ${managing ? html`<${SavedPromptManager} prompts=${prompts} onClose=${() => setManaging(false)}
      onChanged=${reload} confirmDiscard=${confirmDiscard}/>` : null}
  </div>`;
}
