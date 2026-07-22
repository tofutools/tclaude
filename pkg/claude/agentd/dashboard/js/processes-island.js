import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isModifiedClick } from './helpers.js';
import { registerCommandProvider } from './command-registry.js';
import { buildProcessEditorCommands } from './process-command-registry.js';
import { bindProcessTemplateDnd, setProcessTemplateDeleteHandler } from './process-template-dnd.js';

const html = htm.bind(h);

function RequestBody({ request, label, retry, children }) {
  if (request.phase === 'loading' && !request.hasPayload) return html`<div class="process-placeholder">Loading ${label}…</div>`;
  if (request.phase === 'error' && !request.hasPayload) return html`<div class="process-placeholder island-error" role="alert">Could not load ${label}: ${request.error} <button class="process-action" type="button" onClick=${retry}>retry</button></div>`;
  return html`<div class="process-request-body">${request.phase === 'error' && html`<div class="island-error" role="alert">Refresh failed: ${request.error}</div>`}${children}</div>`;
}

function InlineTemplateField({ id, value, emptyLabel, placeholder, className, editAttr, inputAttr, editTitle, editLabel, busy, commit }) {
  const [editing, setEditing] = useState(false);
  const inputRef = useRef(null);
  const session = useRef(null);
  const settled = useRef(false);
  const open = () => {
    session.current = { value, save: commit };
    settled.current = false;
    setEditing(true);
  };
  useLayoutEffect(() => {
    if (!editing) return;
    const input = inputRef.current;
    if (!input) return;
    input.value = session.current?.value || '';
    input.focus();
    input.select?.();
  }, [editing]);
  const finish = async () => {
    if (settled.current || !inputRef.current || !session.current) return;
    settled.current = true;
    const started = session.current;
    const next = String(inputRef.current.value ?? '').trim();
    setEditing(false);
    if (next !== started.value) await started.save(next).catch(() => {});
  };
  if (editing) return html`<input ref=${inputRef} class=${`${className}-input`} type="text" spellcheck="false" placeholder=${placeholder} aria-label=${editLabel} ...${{ [inputAttr]: id }} defaultValue=${session.current?.value || ''} onBlur=${finish} onKeyDown=${(event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === 'Enter') { event.preventDefault(); finish(); }
    if (event.key === 'Escape') { event.preventDefault(); settled.current = true; setEditing(false); }
  }} />`;
  return html`<button class=${`${className}-edit${value ? '' : ' process-unnamed'}`} type="button" ...${{ [editAttr]: id }} disabled=${busy} title=${editTitle} aria-label=${editLabel} onClick=${open}>${value || emptyLabel}</button>`;
}

function EditableName({ template, actions, busy }) {
  return html`<${InlineTemplateField} id=${template.id} value=${template.name || ''} emptyLabel=${template.id} placeholder="display name" className="process-name" editAttr="data-process-name-edit" inputAttr="data-process-name-input" editTitle="Click to rename" editLabel=${`Rename ${template.name || template.id}`} busy=${busy} commit=${(next) => actions.renameTemplate({ id: template.id, name: template.name || '', sourceHash: template.latestVersion?.sourceHash || '' }, next)} />`;
}

function EditableDescription({ template, actions, busy }) {
  return html`<${InlineTemplateField} id=${template.id} value=${template.description || ''} emptyLabel="add a description" placeholder="short description" className="process-description" editAttr="data-process-description-edit" inputAttr="data-process-description-input" editTitle="Click to edit the description" editLabel=${`Description for ${template.name || template.id}`} busy=${busy} commit=${(next) => actions.describeTemplate({ id: template.id, description: template.description || '', sourceHash: template.latestVersion?.sourceHash || '' }, next)} />`;
}

function TrashIcon() {
  return html`<svg class="trash-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path><line x1="10" y1="11" x2="10" y2="17"></line><line x1="14" y1="11" x2="14" y2="17"></line></svg>`;
}

function Templates({ current, actions }) {
  return html`<div id="process-panel-templates" class="process-panel active" role="tabpanel" aria-label="Process templates">
    <div class="filter-bar process-toolbar"><strong>Reusable process graphs</strong><span class="spacer"></span><button id="process-scribe-library" class="process-action" type="button" title="Open a scoped agent that can safely author process templates" onClick=${() => actions.summonScribe({ kind: 'library' })}><span class="process-scribe-plain">Edit with agent</span><span class="process-scribe-wizard">Consult a process scribe</span></button><button id="process-template-new" class="process-action primary" type="button" onClick=${actions.openCreate}>+ new template</button></div>
    <div id="process-templates-list" class="process-list" aria-busy=${current.requests.templates.phase === 'loading'}><${RequestBody} request=${current.requests.templates} label="templates" retry=${() => actions.load('templates')}>
      ${current.templates.length === 0 ? html`<div class="process-placeholder"><h3>No process templates yet</h3><p>Create a blank template to start shaping a repeatable graph.</p></div>` : html`<table><thead><tr><th>Template</th><th>Description</th><th>Latest</th><th>Versions</th><th></th></tr></thead><tbody>${current.templates.map((template) => { const latest = template.latestVersion || {}; const actor = actions.describeActor(latest.actor); return html`<tr key=${template.id} data-process-template=${template.id} draggable=${true} data-process-template-drag=${template.id} data-process-template-name=${template.name || ''} data-process-template-versions=${template.versionCount || 0}><td><${EditableName} template=${template} actions=${actions} busy=${current.mutation.busy} /><div class="process-secondary" title="Template id (permanent)">${template.id}</div></td><td class="process-description"><${EditableDescription} template=${template} actions=${actions} busy=${current.mutation.busy} /></td><td><span class="process-hash" title=${latest.semanticHash || ''}>${(latest.semanticHash || '').slice(0, 10) || '—'}</span>${actor && html`<div class="process-secondary process-version-actor">by ${actor.label}</div>`}</td><td>${template.versionCount || 0}</td><td class="process-actions"><button class="process-action" data-process-action="edit" data-id=${template.id} type="button" onClick=${() => actions.openEditor(template.id)}>open</button><button class="process-action" data-process-action="rename" data-id=${template.id} type="button" title="Change the display name; the id stays fixed" onClick=${() => actions.openRename({ id: template.id, name: template.name || '', sourceHash: latest.sourceHash || '' })}>rename</button><button class="process-action process-action-danger process-delete-btn" data-process-action="delete" data-id=${template.id} type="button" disabled=${current.mutation.busy} title="Delete this template and all its versions" aria-label=${`Delete ${template.name || template.id}`} onClick=${() => actions.deleteTemplate({ id: template.id, name: template.name || '', versionCount: template.versionCount || 0 })}><${TrashIcon} /></button></td></tr>`; })}</tbody></table>`}
    </${RequestBody}></div>
  </div>`;
}

function ScribeStatus({ scribes, actions }) {
  if (!scribes.length) return null;
  return html`<div class="process-scribe-status" role="status" aria-label="Process scribe sessions"><strong>Process ${scribes.length === 1 ? 'scribe' : 'scribes'}</strong>${scribes.map((scribe) => html`<div key=${scribe.agentId} class="process-scribe-session" data-process-scribe=${scribe.agentId}><span class=${`process-scribe-state ${scribe.online ? 'online' : 'stopped'}`}>${scribe.online ? 'active' : 'stopped'}</span><button class="process-scribe-open" type="button" disabled=${!scribe.online} onClick=${() => actions.openScribe(scribe)}>${scribe.name}</button><span class="process-secondary">${scribe.scopeLabel}</span>${/^https?:\/\//i.test(scribe.taskURL) && html`<a class="process-scribe-task" href=${scribe.taskURL} title=${scribe.taskURL}>${scribe.taskLabel || 'task'}</a>`}${scribe.online && html`<button class="process-action" type="button" data-process-scribe-action="stop" onClick=${() => actions.stopScribe(scribe)}>stop</button>`}<button class="process-action process-action-danger" type="button" data-process-scribe-action="retire" onClick=${() => actions.retireScribe(scribe)}>retire</button></div>`)}</div>`;
}

export function ProcessEditorBoundary({ spec, state, actions, confirmDiscard, openEditor = null }) {
  const mountRef = useRef(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let disposed = false; let editor = null;
    const loadEditor = openEditor || (async (mount, value) => (await import('./process-editor.js')).openTemplateEditor(mount, value));
    loadEditor(mountRef.current, { id: spec.id, blank: spec.blank, name: spec.name, view: spec.view, config: { confirmDiscard, onScribe: actions?.summonScribe, describeActor: actions?.describeActor, onOpenActor: actions?.openActor } })
      .then((value) => { editor = value; if (disposed) editor?.destroy?.(); else state.setEditor(editor); })
      .catch((cause) => { if (!disposed) { setError(cause.message); state.setNotice(`Could not open editor: ${cause.message}`); } });
    return () => { disposed = true; state.setEditor(null); editor?.destroy?.(); };
  }, [spec.key]);
  return html`<div id="process-editor-canvas" ref=${mountRef} class="process-canvas-mount" data-process-mount="editor">${error && html`<div class="process-placeholder" role="alert">Could not open editor: ${error}</div>`}</div>`;
}

export function fieldSubmitHotkey(submit) {
  return (event) => { if (event.key === 'Enter' && (event.ctrlKey || event.metaKey) && !event.isComposing && event.keyCode !== 229) { event.preventDefault(); submit(); } };
}

function RenameDialog({ spec, busy, actions }) {
  const nameRef = useRef(null); const [name, setName] = useState(spec.name || '');
  const close = () => { if (!busy) actions.closeRename(); };
  const { dialogRef } = useDialogFocus({ open: true, initialFocusRef: nameRef, onEscape: close });
  const submit = (event) => { event?.preventDefault(); void actions.submitRename(name); };
  return html`<div class="modal-overlay show process-editor-modal process-rename-modal" onClick=${(event) => { if (event.target === event.currentTarget) close(); }}><form ref=${dialogRef} class="modal process-rename-dialog" role="dialog" aria-modal="true" aria-labelledby="process-rename-title" onSubmit=${submit}><h3 id="process-rename-title">Rename template</h3><div class="field process-editor-field process-rename-field"><label for="process-rename-input">Display name</label><input ref=${nameRef} id="process-rename-input" data-process-rename-input type="text" autocomplete="off" spellcheck="false" placeholder="display name" value=${name} data-select-on-focus onInput=${(event) => setName(event.currentTarget.value)} onKeyDown=${fieldSubmitHotkey(() => { if (!busy) submit(); })} /></div><p class="muted">Shown wherever this template is listed. The id <code>${spec.id}</code> is permanent.</p>${spec.error && html`<div class="island-error" role="alert">${spec.error}</div>`}<div class="modal-buttons"><button type="button" disabled=${busy} onClick=${close}>Cancel</button><button class="primary" type="submit" disabled=${busy}>${busy ? 'Saving…' : 'Save name'}</button></div></form></div>`;
}

function CreateDialog({ spec, busy, actions }) {
  const nameRef = useRef(null); const [name, setName] = useState(spec.name || '');
  const outcomeUnknown = !!spec.attempt?.blocked && name.trim() === spec.attempt.name;
  const close = () => { if (!busy) actions.closeCreate(); };
  const { dialogRef } = useDialogFocus({ open: true, initialFocusRef: nameRef, onEscape: close });
  const submit = (event) => { event?.preventDefault(); if (!busy && !outcomeUnknown) void actions.submitCreate(name); };
  return html`<div class="modal-overlay show process-editor-modal process-rename-modal" onClick=${(event) => { if (event.target === event.currentTarget) close(); }}><form ref=${dialogRef} class="modal process-rename-dialog" role="dialog" aria-modal="true" aria-labelledby="process-create-title" onSubmit=${submit}><h3 id="process-create-title">New process template</h3><div class="field process-editor-field process-rename-field"><label for="process-create-input">Display name</label><input ref=${nameRef} id="process-create-input" data-process-create-input type="text" autocomplete="off" spellcheck="false" placeholder="e.g. Release train" value=${name} disabled=${busy} onInput=${(event) => setName(event.currentTarget.value)} onKeyDown=${fieldSubmitHotkey(submit)} /></div><p class="muted">Creating the template assigns its permanent id automatically.</p>${spec.error && html`<div class="island-error" role="alert">${spec.error}</div>`}<div class="modal-buttons"><button type="button" disabled=${busy} onClick=${close}>Cancel</button><button class="primary" type="submit" disabled=${busy || !name.trim() || outcomeUnknown}>${busy ? 'Creating…' : 'Create'}</button></div></form></div>`;
}

export function ProcessesApp({ state, actions, confirmDiscard }) {
  const current = { ...state.view.value, state };
  useEffect(() => { if (current.active) void actions.refreshActive(); }, [current.active]);
  useEffect(() => { const poll = () => { const view = state.view.value; if (view.active && view.canvas?.kind === 'editor') void actions.observeTemplateHeads(); }; document.addEventListener('tclaude:snapshot', poll); return () => document.removeEventListener('tclaude:snapshot', poll); }, []);
  useEffect(() => { const reselected = (event) => { if (event.detail?.tab === 'processes' && state.view.value.active) void actions.refreshActive(); }; document.addEventListener('tclaude:tab-reselected', reselected); return () => document.removeEventListener('tclaude:tab-reselected', reselected); }, []);
  const navigate = async (event) => { if (isModifiedClick(event)) return; event.preventDefault(); await actions.activateSubtab('templates'); };
  const spec = current.canvas;
  return html`<div class="processes-island"><div class="process-subnav" role="tablist" aria-label="Process views"><a class="process-subtab active" data-process-subtab="templates" href="/processes/templates" role="tab" aria-selected="true" onClick=${navigate}>Templates</a><span class="spacer"></span><span id="process-notice" class="process-notice" role="status">${current.notice}</span></div><${ScribeStatus} scribes=${current.scribes || []} actions=${actions} />${spec ? html`<div id="process-editor-view" class="process-canvas-view process-scroll-surface"><button class="process-action" data-process-close-view type="button" onClick=${actions.closeCanvas}>← templates</button><${ProcessEditorBoundary} spec=${spec} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} /></div>` : html`<${Templates} current=${current} actions=${actions} />`}${current.create && html`<${CreateDialog} key=${current.create.key} spec=${current.create} busy=${current.mutation.busy} actions=${actions} />`}${current.rename && html`<${RenameDialog} key=${current.rename.key} spec=${current.rename} busy=${current.mutation.busy} actions=${actions} />`}</div>`;
}

export function mountProcessesIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  const unregisterCommands = registerCommandProvider('process-editor', () => {
    const view = state.view.value;
    return !view.active || view.canvas?.kind !== 'editor' ? [] : buildProcessEditorCommands({ editor: state.currentEditor(), actions });
  });
  registerCleanup(unregisterCommands);
  const restore = (event) => {
    const loc = event.detail?.location;
    if (loc?.tab === 'processes') void Promise.resolve(actions.applyLocation(loc)).catch(() => false).then((ok) => { if (!ok) actions.correctLocation(); });
  };
  document.addEventListener('tclaude:restore-location', restore);
  registerCleanup(() => document.removeEventListener('tclaude:restore-location', restore));
  setProcessTemplateDeleteHandler((spec) => actions.deleteTemplate(spec));
  const unbindTemplateDnd = bindProcessTemplateDnd();
  registerCleanup(() => { unbindTemplateDnd(); setProcessTemplateDeleteHandler(null); });
  render(html`<${ProcessesApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  registerCleanup(() => render(null, host));
}
