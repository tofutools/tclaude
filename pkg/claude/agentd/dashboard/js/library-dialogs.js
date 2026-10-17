// library-dialogs.js -- the right panel library's two dialogs, hosted in the
// management island's dialog slot:
//
// - LibraryNameDialog asks for one name (rename, or a process clone) and
//   runs the descriptor's submit with it. A refusal (the name is taken, or
//   invalid) stays in the dialog so the operator can pick another.
// - LibraryBundleImportDialog reviews a config bundle before anything is
//   written: every item with what applying it would do (create, replace,
//   unchanged), a per-item skip, the replace switch for existing items, and a
//   value for each path placeholder the bundle could not resolve here.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { previewBundleImport } from './dock-library.js';

const html = htm.bind(h);

const message = (error) => error?.message || String(error);

export function LibraryNameDialog({ descriptor, onClose, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const initial = descriptor.initial || '';
  const [name, setName] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const submit = async () => {
    const next = name.trim();
    if (busy) return;
    if (!next) {
      setError('enter a name');
      return;
    }
    setBusy(true);
    setError('');
    try {
      await descriptor.submit(next);
      onClose();
    } catch (cause) {
      setError(message(cause));
    } finally {
      setBusy(false);
    }
  };
  return html`<${Overlay} id="library-name-modal" labelledby="library-name-title" onClose=${onClose}
      dirty=${name !== initial} blocked=${busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
    <h3 id="library-name-title">${descriptor.title}</h3>
    ${descriptor.blurb && html`<div class="modal-meta">${descriptor.blurb}</div>`}
    <label class="cron-create-row">
      <span class="cron-create-label">${descriptor.label || 'New name'}</span>
      <input id="library-name-input" type="text" value=${name} autocomplete="off" spellcheck="false" data-select-on-focus
        onInput=${(event) => setName(event.currentTarget.value)}
        onKeyDown=${(event) => {
          if (event.key !== 'Enter' || event.isComposing) return;
          event.preventDefault();
          void submit();
        }} />
    </label>
    <div class="cron-create-error" id="library-name-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="library-name-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>${busy ? 'Saving…' : descriptor.okLabel || 'Save'}</button>
    </div>
  </${Overlay}>`;
}

const ACTION_LABELS = Object.freeze({ create: 'New', replace: 'Replaces existing', unchanged: 'Unchanged' });

// bundleItemLabel turns "sandbox-profiles/base" into a readable kind + name.
function bundleItemLabel(item) {
  const [section, ...rest] = String(item).split('/');
  return { section: section.replace(/-/g, ' '), name: rest.join('/') };
}

export function LibraryBundleImportDialog({ descriptor, onClose, confirmDiscard, fetchImpl }) {
  const { notify, onImported } = descriptor;
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [preview, setPreview] = useState(null);
  const [skip, setSkip] = useState([]);
  const [replace, setReplace] = useState(false);
  const [keepPaths, setKeepPaths] = useState(false);
  const [values, setValues] = useState({});
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  const request = useRef(0);
  const ask = (apply) => previewBundleImport({ bundle: descriptor.bundle, skip, replace, values, keepPaths, apply, fetchImpl });
  // Re-preview whenever a choice changes, so the list always shows what
  // applying would do now. Placeholder values debounce while typed.
  useEffect(() => {
    const token = ++request.current;
    const timer = setTimeout(() => {
      setBusy((value) => value || 'preview');
      ask(false)
        .then((next) => { if (token === request.current) { setPreview(next); setError(''); } })
        .catch((cause) => { if (token === request.current) setError(message(cause)); })
        .finally(() => { if (token === request.current) setBusy((value) => (value === 'preview' ? '' : value)); });
    }, preview ? 250 : 0);
    return () => clearTimeout(timer);
  }, [skip.join('\n'), replace, keepPaths, JSON.stringify(values)]);
  const changes = preview?.changes || [];
  // Skipped items drop out of the preview; keep them listed so they can come back.
  const rows = [...changes.map((change) => ({ ...change, skipped: false })),
    ...skip.filter((item) => !changes.some((change) => change.item === item)).map((item) => ({ item, action: '', skipped: true }))]
    .sort((a, b) => a.item.localeCompare(b.item));
  const unresolved = preview?.unresolved || [];
  const placeholders = [...new Map(unresolved.map((entry) => [entry.name, entry])).values()];
  const pending = changes.filter((change) => change.action !== 'unchanged');
  const conflicts = changes.filter((change) => change.action === 'replace');
  const blocked = !preview || !!busy || !pending.length || placeholders.some((entry) => !values[entry.name]) || (conflicts.length > 0 && !replace);
  const toggleSkip = (item) => setSkip((value) => (value.includes(item) ? value.filter((entry) => entry !== item) : [...value, item]));
  const apply = async () => {
    if (blocked) return;
    setBusy('apply');
    setError('');
    try {
      const result = await ask(true);
      const applied = result.applied || [];
      notify?.(`imported ${applied.length} item${applied.length === 1 ? '' : 's'}${result.warnings?.length ? ` — ${result.warnings.join('; ')}` : ''}`);
      onImported?.(result);
      onClose();
    } catch (cause) {
      setError(message(cause));
    } finally {
      setBusy('');
    }
  };
  return html`<${Overlay} id="library-bundle-import-modal" labelledby="library-bundle-import-title" onClose=${onClose}
      dirty=${false} blocked=${busy === 'apply'} confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
    <h3 id="library-bundle-import-title">Import ${descriptor.fileName || 'config bundle'}</h3>
    <div class="modal-meta">Nothing is written until you import. Untick an item to leave it out.</div>
    <div class="library-import-list" id="library-bundle-import-list">
      ${!preview && !error && html`<div class="dock-empty">Reading the bundle…</div>`}
      ${rows.map((row) => {
        const label = bundleItemLabel(row.item);
        return html`<label key=${row.item} class=${`library-import-row${row.skipped ? ' is-skipped' : ''}`} data-library-import-item=${row.item}>
          <input type="checkbox" checked=${!row.skipped} disabled=${busy === 'apply'} onChange=${() => toggleSkip(row.item)} />
          <span class="library-import-kind">${label.section}</span>
          <span class="library-import-name">${label.name}</span>
          <span class=${`library-import-action is-${row.skipped ? 'skipped' : row.action}`}>${row.skipped ? 'Skipped' : ACTION_LABELS[row.action] || row.action}</span>
        </label>`;
      })}
    </div>
    ${conflicts.length > 0 && html`<label class="library-import-option"><input id="library-bundle-import-replace" type="checkbox" checked=${replace}
        onChange=${(event) => setReplace(event.currentTarget.checked)} /> Replace the ${conflicts.length} existing item${conflicts.length === 1 ? '' : 's'} with the imported version</label>`}
    ${(placeholders.length > 0 || keepPaths) && html`<div class="library-import-values">
      <div class="modal-meta">The bundle names paths relative to its home or workspace. Say where they are here, or keep the paths exactly as written.</div>
      <label class="library-import-option"><input type="checkbox" checked=${keepPaths} onChange=${(event) => setKeepPaths(event.currentTarget.checked)} /> Keep paths as written</label>
      ${placeholders.map((entry) => html`<label key=${entry.name} class="cron-create-row">
        <span class="cron-create-label" title=${`${entry.item} ${entry.field}${entry.original ? ` (was ${entry.original})` : ''}`}>${entry.name}</span>
        <input type="text" value=${values[entry.name] || ''} placeholder=${entry.original || ''} data-library-import-value=${entry.name}
          onInput=${(event) => { const value = event.currentTarget.value; setValues((current) => ({ ...current, [entry.name]: value })); }} />
      </label>`)}
    </div>`}
    ${preview?.security_changes > 0 && html`<div class="modal-meta library-import-security">${preview.security_changes} item${preview.security_changes === 1 ? '' : 's'} can change what agents may do. Check them before importing.</div>`}
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy === 'apply'} onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="library-bundle-import-submit" class="primary" type="button" disabled=${blocked} onClick=${apply}>
        ${busy === 'apply' ? 'Importing…' : `Import ${pending.length} item${pending.length === 1 ? '' : 's'}`}
      </button>
    </div>
  </${Overlay}>`;
}
