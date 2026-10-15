// process-workflow-share.js -- share processes as .pwf workflow files
// (docs/pwf.md) from the dashboard: export a template with every template it
// references, and import one after a review of what it would run. The review
// mirrors `tclaude agent process-templates import`: an import applies exactly
// the reviewed preview (its reviewDigest) and never starts a run.
import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { downloadBlob } from './paf-model.js';

const html = htm.bind(h);
const PWF_TYPE = 'application/vnd.tclaude.pwf+zip';
const MAX_FILE = 32 << 20;

async function errorText(response) {
  const body = await response.json().catch(() => ({}));
  return body.message || body.error || `${response.status} ${response.statusText}`;
}

// importError keeps the response body: a stale-review conflict carries the
// fresh preview to review again.
async function importError(response) {
  const body = await response.json().catch(() => ({}));
  const err = new Error(body.message || body.error || `${response.status} ${response.statusText}`);
  err.status = response.status;
  err.body = body;
  return err;
}

// Programs run as argv without a shell, so the review shows each argument as
// its own token: quoted when it holds spaces or quotes, with control and
// bidi characters escaped so the text shown is the text that runs.
const INVISIBLE = /[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u200b-\u200f\u2028-\u202e\u2060-\u2069\ufeff]/gu;
export function displayArg(arg) {
  const text = String(arg ?? '').replace(/\\/g, '\\\\').replace(INVISIBLE, (c) => `\\u{${c.codePointAt(0).toString(16)}}`);
  return text === '' || /[\s'"]/.test(text) ? `'${text.replace(/'/g, "'\\''")}'` : text;
}

// exportWorkflow downloads <id>.pwf. It resolves to '' or an error message.
export async function exportWorkflow(id, { fetchImpl = fetch, download = downloadBlob } = {}) {
  const response = await fetchImpl(`/v1/process/templates/${encodeURIComponent(id)}/export`, { credentials: 'same-origin' });
  if (!response.ok) return errorText(response);
  download(await response.blob(), `${String(id).replace(/[^A-Za-z0-9._-]+/g, '-')}.pwf`);
  return '';
}

// previewImport posts the file without apply: the daemon verifies it and
// says what importing would change here, writing nothing.
export async function previewImport(bytes, { fetchImpl = fetch } = {}) {
  const response = await fetchImpl('/v1/process/import', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': PWF_TYPE }, body: bytes,
  });
  if (!response.ok) throw new Error(await errorText(response));
  return response.json();
}

// applyImport imports exactly the reviewed preview.
export async function applyImport(bytes, preview, { replace = false, fetchImpl = fetch } = {}) {
  const query = `apply=1&review=${encodeURIComponent(preview.reviewDigest || '')}${replace ? '&replace=1' : ''}`;
  const response = await fetchImpl(`/v1/process/import?${query}`, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': PWF_TYPE }, body: bytes,
  });
  if (!response.ok) throw await importError(response);
  return response.json();
}

const STATUS_TEXT = {
  new: 'new here',
  identical: 'already here, unchanged',
  update: 'replaces your current version',
  version: 'older pinned version, stored alongside',
};

// importReview shapes a preview for the dialog: what the reviewer must look
// at, in the order they should look at it.
export function importReview(preview) {
  const signatures = preview.signatures || [];
  const valid = signatures.filter((s) => s.status === 'valid');
  let signer = 'Unsigned: nothing proves where this file came from.';
  if (valid.length) {
    const trust = valid.map((s) => s.trust || 'unknown key');
    signer = trust.includes('this node') ? 'Signed by this node.'
      : trust.find((t) => t.startsWith('known peer')) ? `Signed by a ${trust.find((t) => t.startsWith('known peer'))}.`
        : 'Signed by an unknown key: the file is unchanged since signing, but the signer is not known here.';
  } else if (signatures.length) {
    signer = 'The signature does not verify: treat this file as unsigned.';
  }
  const templates = (preview.templates || []).map((t) => ({ ...t, label: STATUS_TEXT[t.status] || t.status }));
  return {
    title: preview.title || preview.rootId || 'Workflow',
    summary: preview.summary || '',
    signer,
    trusted: valid.some((s) => s.trust === 'this node' || (s.trust || '').startsWith('known peer')),
    params: preview.params || [],
    roles: preview.roles || [],
    humans: preview.humans || [],
    programs: (preview.programs || []).map((p) => ({ ...p, command: [p.run, ...(p.args || [])].map(displayArg).join(' ') })),
    assignees: preview.assignees || [],
    templates,
    updates: preview.updates || 0,
    nothingToDo: templates.length > 0 && templates.every((t) => t.status === 'identical'),
  };
}

function ImportDialog({ file, bytes, preview: initialPreview, onClose, onImported }) {
  const [preview, setPreview] = useState(initialPreview);
  const review = importReview(preview);
  const [replace, setReplace] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const blocked = review.updates > 0 && !replace;
  const submit = async () => {
    if (busy || blocked || review.nothingToDo) return;
    setBusy(true);
    setError('');
    try {
      const result = await applyImport(bytes, preview, { replace });
      onImported?.(result);
      onClose();
    } catch (err) {
      // A failed apply may have stored some templates, and a stale review
      // returns the current preview: refresh the list and review again.
      onImported?.(null);
      if (err?.body?.preview) {
        setPreview(err.body.preview);
        setReplace(false);
        setError(`${err.message} The review below is updated; check it again.`);
      } else {
        setError(err?.message || String(err));
      }
      setBusy(false);
    }
  };
  return html`<${Overlay} id="process-workflow-import" dialogClass="modal pwf-import-dialog" overlayClass="pwf-import-modal"
      labelledby="pwf-import-title" onClose=${onClose} blocked=${busy}>
    <h3 id="pwf-import-title">Import workflow: ${review.title}</h3>
    <p class="muted pwf-file">${file}</p>
    ${review.summary && html`<p class="pwf-summary">${review.summary}</p>`}
    <p class=${`pwf-signer ${review.trusted ? 'is-trusted' : 'is-untrusted'}`} data-pwf-signer>${review.signer}</p>
    <div class="pwf-section"><h4>Commands it would run</h4>
      ${review.programs.length === 0 ? html`<p class="muted">None. Every step is an agent or you.</p>` : html`<ul class="pwf-commands">${review.programs.map((p) => html`<li key=${p.step} data-pwf-command=${p.step}><code>${p.command}</code> <span class="muted">· ${p.step} · program profile <b>${p.profile || 'default'}</b></span></li>`)}</ul>
      <p class="muted">Nothing runs on import. Starting a run asks you to allow each program profile first.</p>`}
    </div>
    ${review.roles.length > 0 && html`<div class="pwf-section"><h4>Agent roles it asks for</h4><ul>${review.roles.map((r) => html`<li key=${r.name}><b>${r.name}</b> <span class="muted">· ${r.steps.join(', ')}</span></li>`)}</ul>
      <p class="muted">A step waits until a member of the run's group has the role, or its fallback spawns one.</p></div>`}
    ${review.humans.length > 0 && html`<div class="pwf-section"><h4>Asks you</h4><p class="muted">${review.humans.join(', ')}</p></div>`}
    ${review.params.length > 0 && html`<div class="pwf-section"><h4>Start inputs</h4><p>${review.params.map((p) => html`<span key=${p.name} class="pwf-param">${p.name}${p.required ? ' *' : ''}</span>`)}</p></div>`}
    ${review.assignees.length > 0 && html`<div class="pwf-section pwf-warn" data-pwf-assignees><h4>Not portable</h4><ul>${review.assignees.map((a) => html`<li key=${a.name}>${a.steps.join(', ')} name the agent <b>${a.name}</b>, which may not exist here. Those steps wait until reassigned.</li>`)}</ul></div>`}
    <div class="pwf-section"><h4>What changes here</h4><ul class="pwf-templates">${review.templates.map((t) => html`<li key=${t.ref} data-pwf-template=${t.id} data-pwf-status=${t.status}><b>${t.name || t.id}</b> <span class=${`pwf-status is-${t.status}`}>${t.label}</span></li>`)}</ul>
      ${review.updates > 0 && html`<label class="pwf-replace"><input type="checkbox" data-pwf-replace checked=${replace} onChange=${(e) => setReplace(e.currentTarget.checked)} /> Replace my current version of ${review.updates === 1 ? 'that template' : `those ${review.updates} templates`}</label>`}
    </div>
    ${error && html`<div class="island-error" role="alert">${error}</div>`}
    <div class="modal-buttons"><button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      <button class="primary" type="button" data-pwf-import disabled=${busy || blocked || review.nothingToDo} onClick=${submit}>${review.nothingToDo ? 'Already imported' : busy ? 'Importing…' : 'Import'}</button></div>
  </${Overlay}>`;
}

// WorkflowImportButton picks a .pwf file, previews it and opens the review.
export function WorkflowImportButton({ onImported }) {
  const input = useRef(null);
  const [state, setState] = useState(null);
  const [error, setError] = useState('');
  const choose = async (event) => {
    const picked = event.currentTarget.files?.[0];
    event.currentTarget.value = '';
    if (!picked) return;
    setError('');
    if (picked.size > MAX_FILE) {
      setError('That file is too large to be a workflow.');
      return;
    }
    try {
      const bytes = new Uint8Array(await picked.arrayBuffer());
      setState({ file: picked.name, bytes, preview: await previewImport(bytes) });
    } catch (err) {
      setError(`Cannot import ${picked.name}: ${err?.message || err}`);
    }
  };
  return html`<span class="pwf-import-entry">
    <input ref=${input} type="file" accept=".pwf,${PWF_TYPE}" hidden data-pwf-file onChange=${choose} />
    <button id="process-workflow-import-btn" class="process-action" type="button" title="Import a shared .pwf workflow file: you review what it runs before anything is stored" onClick=${() => input.current?.click()}>⤒ import workflow</button>
    ${error && html`<span class="island-error pwf-inline-error" role="alert">${error}</span>`}
    ${state && html`<${ImportDialog} ...${state} onClose=${() => setState(null)} onImported=${onImported} />`}
  </span>`;
}

// WorkflowExportButton downloads one template as a .pwf.
export function WorkflowExportButton({ id }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const run = async () => {
    setBusy(true);
    setError('');
    try {
      setError(await exportWorkflow(id));
    } catch (err) {
      setError(err?.message || String(err));
    } finally {
      setBusy(false);
    }
  };
  return html`<button class="process-action" data-process-action="export" data-id=${id} type="button" disabled=${busy}
    title=${error || 'Download as a .pwf workflow file to share; it carries the templates this one uses'} aria-invalid=${error ? 'true' : undefined}
    onClick=${run}>${busy ? 'exporting…' : error ? 'export failed' : 'share'}</button>`;
}
