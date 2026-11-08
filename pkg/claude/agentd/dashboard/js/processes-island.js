import { h, render } from 'preact';
import { effect } from '@preact/signals';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { TrashIcon } from './row-icons.js';
import { Dialog } from './ui-dialog.js';
import { AutomationsSubnav, PROCESSES_SUBTAB, automationsSubnavEntries, openAutomationsEntry } from './automations-subnav.js';
import { registerCommandProvider } from './command-registry.js';
import { buildProcessEditorCommands } from './process-command-registry.js';
import { bindProcessTemplateDnd, setProcessTemplateDeleteHandler } from './process-template-dnd.js';
import { takeProcessDraft } from './process-draft-handoff.js';
import { WorkflowExportButton, WorkflowImportButton } from './process-workflow-share.js';
import { fleetState } from './fleet-state.js';
import { NodeSection, fusedPeers, openOnNode, peerRequestMutation, usePeerSections } from './fused-sections.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { overlayHostUp } from './overlay-instances.js';
import {
  editorDirty, floatHandoff, floatingProcessEditor, floatingProcessTab, paletteEditor, processDocked, processEditorMount, processShortcutLive,
  registerProcessEditorHost, returningEditor, showProcessEditorError, stripProcesses, takeReturningEditor, trackTouchedEditor,
} from './process-editor-place.js';
import {
  TO_DASHBOARD_LABEL, announceSolo, editorBack, heldForWindow, installProcessWindows, isProcessSolo, openerFloats, openerTakesBack,
  soloHandoff,
} from './process-editor-window.js';
import { TO_WINDOW_LABEL } from './overlay-tab-moves.js';
import { withShortcut } from './keybindings.js';
import { liveHelpers, sendToHelper } from './get-help.js';
import { dashboardState } from './snapshot-store.js';

const html = htm.bind(h);

function RequestBody({ request, label, retry, children }) {
  if (request.phase === 'loading' && !request.hasPayload) return html`<div class="process-placeholder">Loading ${label}…</div>`;
  if (request.phase === 'error' && !request.hasPayload) return html`<div class="process-placeholder island-error" role="alert">Could not load ${label}: ${request.error} <${Button} class="process-action" onClick=${retry}>retry<//></div>`;
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
  return html`<button data-ui-exempt class=${`${className}-edit${value ? '' : ' process-unnamed'}`} type="button" ...${{ [editAttr]: id }} disabled=${busy} title=${editTitle} aria-label=${editLabel} onClick=${open}>${value || emptyLabel}</button>`;
}

function EditableName({ template, actions, busy }) {
  return html`<${InlineTemplateField} id=${template.id} value=${template.name || ''} emptyLabel=${template.id} placeholder="display name" className="process-name" editAttr="data-process-name-edit" inputAttr="data-process-name-input" editTitle="Click to rename" editLabel=${`Rename ${template.name || template.id}`} busy=${busy} commit=${(next) => actions.renameTemplate({ id: template.id, name: template.name || '', sourceHash: template.latestVersion?.sourceHash || '' }, next)} />`;
}

function EditableDescription({ template, actions, busy }) {
  return html`<${InlineTemplateField} id=${template.id} value=${template.description || ''} emptyLabel="add a description" placeholder="short description" className="process-description" editAttr="data-process-description-edit" inputAttr="data-process-description-input" editTitle="Click to edit the description" editLabel=${`Description for ${template.name || template.id}`} busy=${busy} commit=${(next) => actions.describeTemplate({ id: template.id, description: template.description || '', sourceHash: template.latestVersion?.sourceHash || '' }, next)} />`;
}

function Templates({ current, actions }) {
  return html`<div id="process-panel-templates" class="process-panel active" role="tabpanel" aria-label="Process templates">
    <div class="filter-bar process-toolbar"><strong>Reusable process graphs</strong><span class="spacer"></span><${Button} id="process-scribe-library" class="process-action" title="Open a scoped agent that can safely author process templates" onClick=${() => actions.summonScribe({ kind: 'library' })}><span class="process-scribe-plain">Edit with agent</span><span class="process-scribe-wizard">Consult a process scribe</span><//><${WorkflowImportButton} onImported=${() => actions.load('templates')} /><${Button} variant="primary" id="process-template-new" class="process-action" onClick=${actions.openCreate}>+ new template<//></div>
    <div id="process-templates-list" class="process-list" aria-busy=${current.requests.templates.phase === 'loading'}><${RequestBody} request=${current.requests.templates} label="templates" retry=${() => actions.load('templates')}>
      ${current.templates.length === 0 ? html`<div class="process-placeholder"><h3>No process templates yet</h3><p>Create a blank template to start shaping a repeatable graph.</p></div>` : html`<table><thead><tr><th>Template</th><th>Description</th><th>Latest</th><th>Versions</th><th></th></tr></thead><tbody>${current.templates.map((template) => { const latest = template.latestVersion || {}; const actor = actions.describeActor(latest.actor); return html`<tr key=${template.id} data-process-template=${template.id} draggable=${true} data-process-template-drag=${template.id} data-process-template-name=${template.name || ''} data-process-template-versions=${template.versionCount || 0}><td><${EditableName} template=${template} actions=${actions} busy=${current.mutation.busy} /><div class="process-secondary" title="Template id (permanent)">${template.id}</div></td><td class="process-description"><${EditableDescription} template=${template} actions=${actions} busy=${current.mutation.busy} /></td><td><span class="process-hash" title=${latest.semanticHash || ''}>${(latest.semanticHash || '').slice(0, 10) || '—'}</span>${actor && html`<div class="process-secondary process-version-actor">by ${actor.label}</div>`}</td><td>${template.versionCount || 0}</td><td class="process-actions"><${Button} size="sm" class="process-action" data-process-action="edit" data-id=${template.id} onClick=${() => actions.openEditor(template.id)}>open<//><${Button} size="sm" class="process-action" data-process-action="rename" data-id=${template.id} title="Change the display name; the id stays fixed" onClick=${() => actions.openRename({ id: template.id, name: template.name || '', sourceHash: latest.sourceHash || '' })}>rename<//><${WorkflowExportButton} id=${template.id} size="sm" /><${Button} size="sm" icon=${true} variant="ghost danger" class="process-action process-delete-btn" data-process-action="delete" data-id=${template.id} disabled=${current.mutation.busy} title="Delete this template and all its versions" aria-label=${`Delete ${template.name || template.id}`} onClick=${() => actions.deleteTemplate({ id: template.id, name: template.name || '', versionCount: template.versionCount || 0 })}><${TrashIcon} /><//></td></tr>`; })}</tbody></table>`}
    </${RequestBody}></div>
  </div>`;
}

function ScribeStatus({ scribes, actions }) {
  if (!scribes.length) return null;
  return html`<div class="process-scribe-status" role="status" aria-label="Process scribe sessions"><strong>Process ${scribes.length === 1 ? 'scribe' : 'scribes'}</strong>${scribes.map((scribe) => html`<div key=${scribe.agentId} class="process-scribe-session" data-process-scribe=${scribe.agentId}><span class=${`process-scribe-state ui-eyebrow ${scribe.online ? 'online' : 'stopped'}`}>${scribe.online ? 'active' : 'stopped'}</span><button data-ui-exempt class="process-scribe-open" type="button" disabled=${!scribe.online} onClick=${() => actions.openScribe(scribe)}>${scribe.name}</button><span class="process-secondary">${scribe.scopeLabel}</span>${/^https?:\/\//i.test(scribe.taskURL) && html`<a class="process-scribe-task" href=${scribe.taskURL} title=${scribe.taskURL}>${scribe.taskLabel || 'task'}</a>`}${scribe.online && html`<${Button} size="sm" class="process-action" data-process-scribe-action="stop" onClick=${() => actions.stopScribe(scribe)}>stop<//>`}<${Button} size="sm" variant="ghost danger" class="process-action" data-process-scribe-action="retire" onClick=${() => actions.retireScribe(scribe)}>retire<//></div>`)}</div>`;
}

// processEditorConfig is what an editor opened from this tab is given, wherever
// it shows: docked here, or floating in an overlay window (process-overlay.js).
export function processEditorConfig({ mount, readable = false, state, actions, confirmDiscard }) {
  return {
    readableOpen: readable, confirmDiscard,
    isShortcutActive: () => processShortcutLive(mount, () => state.view.value.active),
    // Ctrl/Cmd+O leaves for the template list, which only the docked editor has.
    onOpenTemplates: () => (isProcessSolo() || !processDocked(mount) ? false : actions?.activateSubtab?.('templates')),
    onOpenTemplate: actions?.openReferencedTemplate,
    templateRefs: () => state.view.value.templates, onScribe: actions?.summonScribe,
    onAskAgent: actions?.askAgent,
    onMessageHelper: (helper, message) => sendToHelper(helper, message),
    // An editor's helper counts while the snapshot lists it running.
    helperLive: (helper) => liveHelpers(dashboardState.snapshot.value).some((agent) => agent.id === helper.id),
    describeActor: actions?.describeActor, onOpenActor: actions?.openActor,
  };
}

const loadTemplateEditor = async (mount, value) => (await import('./process-editor.js')).openTemplateEditor(mount, value);

export function ProcessEditorBoundary({ spec, state, actions, confirmDiscard, openEditor = null }) {
  const slotRef = useRef(null);
  useEffect(() => {
    let disposed = false;
    // An editor back from an overlay window comes as it was, unsaved edits and all.
    const live = spec.live || null;
    const mount = live?.mount || processEditorMount(slotRef.current.ownerDocument);
    let editor = live?.editor || null;
    slotRef.current.appendChild(mount);
    if (editor) state.setEditor(editor);
    else {
      (openEditor || loadTemplateEditor)(mount, { id: spec.id, blank: spec.blank, name: spec.name, view: spec.view, handoff: spec.handoff?.draft || null, config: processEditorConfig({ mount, readable: !!spec.readable, state, actions, confirmDiscard }) })
        .then((value) => {
          editor = value;
          if (disposed) { editor?.destroy?.(); return; }
          state.setEditor(editor);
          // A draft from another window is here now, and that window lets it go.
          spec.handoff?.taken?.();
        })
        .catch((cause) => {
          spec.handoff?.failed?.();
          if (!disposed) { showProcessEditorError(mount, cause.message); state.setNotice(`Could not open editor: ${cause.message}`); }
        });
    }
    return () => {
      disposed = true;
      state.setEditor(null);
      // One that went to an overlay window lives on there, and one on its way to
      // a browser window waits for it.
      if (editor && (floatingProcessEditor(spec.id)?.editor === editor || heldForWindow(editor))) return;
      editor?.destroy?.();
      mount.remove();
    };
  }, [spec.key]);
  return html`<div ref=${slotRef} class="process-editor-slot"></div>`;
}

function RenameDialog({ spec, busy, actions }) {
  const nameRef = useRef(null); const [name, setName] = useState(spec.name || '');
  const submit = (event) => { event?.preventDefault(); if (!busy) void actions.submitRename(name); };
  return html`<${Dialog} id="process-rename-modal" overlayClass="process-editor-modal process-rename-modal" dialogClass="modal process-rename-dialog"
    title="Rename template" initialFocusRef=${nameRef} onClose=${() => actions.closeRename()} busy=${busy} onSubmitHotkey=${submit}
    error=${spec.error || ''} cancel="Cancel"
    footer=${html`<${Button} variant="primary" type="submit" form="process-rename-form" disabled=${busy}>${busy ? 'Saving…' : 'Save name'}<//>`}>
    <form id="process-rename-form" onSubmit=${submit}>
      <div class="field process-editor-field process-rename-field"><label for="process-rename-input">Display name</label><input ref=${nameRef} id="process-rename-input" data-process-rename-input type="text" autocomplete="off" spellcheck="false" placeholder="display name" value=${name} data-select-on-focus onInput=${(event) => setName(event.currentTarget.value)} /></div>
      <p class="muted">Shown wherever this template is listed. The id <code>${spec.id}</code> is permanent.</p>
    </form>
  <//>`;
}

function CreateDialog({ spec, busy, actions }) {
  const nameRef = useRef(null); const [name, setName] = useState(spec.name || '');
  const outcomeUnknown = !!spec.attempt?.blocked && name.trim() === spec.attempt.name;
  const submit = (event) => { event?.preventDefault(); if (!busy && !outcomeUnknown) void actions.submitCreate(name); };
  return html`<${Dialog} id="process-create-modal" overlayClass="process-editor-modal process-rename-modal" dialogClass="modal process-rename-dialog"
    title="New process template" initialFocusRef=${nameRef} onClose=${() => actions.closeCreate()} busy=${busy} onSubmitHotkey=${submit}
    error=${spec.error || ''} cancel="Cancel"
    footer=${html`<${Button} variant="primary" type="submit" form="process-create-form" disabled=${busy || !name.trim() || outcomeUnknown}>${busy ? 'Creating…' : 'Create'}<//>`}>
    <form id="process-create-form" onSubmit=${submit}>
      <div class="field process-editor-field process-rename-field"><label for="process-create-input">Display name</label><input ref=${nameRef} id="process-create-input" data-process-create-input type="text" autocomplete="off" spellcheck="false" placeholder="e.g. Release train" value=${name} disabled=${busy} onInput=${(event) => setName(event.currentTarget.value)} /></div>
      <p class="muted">Creating the template assigns its permanent id automatically.</p>
    </form>
  <//>`;
}

// FusedPeerProcesses is a section per ticked peer after this node's
// templates: its library, opened on its own view; delete acts on that node.
function FusedPeerProcesses({ peers, active, describeActor, confirm, notify, fetchFor }) {
  const { byNode, reload } = usePeerSections({
    peers, path: '/v1/process/templates', what: 'its processes', active, ...(fetchFor ? { fetchFor } : {}),
    read: (body) => body?.templates || [],
  });
  const remove = async (node, template) => {
    const label = template.name || template.id;
    const yes = await confirm({ title: 'Delete process template?', body: `Delete ${label} and all its versions on ${node.name}.`, okLabel: 'Delete' });
    if (!yes) return;
    try {
      await peerRequestMutation(node, { reload: () => reload(node.id), ...(fetchFor ? { fetchFor } : {}) })(`/v1/process/templates/${encodeURIComponent(template.id)}`, { method: 'DELETE' });
      notify(`deleted process template ${label} on ${node.name}`);
    } catch (error) {
      notify(`Delete failed: ${error.body?.message || error.body?.error || error.message}`, true);
    }
  };
  return peers.map((node) => {
    const entry = byNode[node.id];
    const templates = entry?.data || [];
    return html`<${NodeSection} key=${node.id} node=${node} entry=${entry} summary=${`${templates.length} template${templates.length === 1 ? '' : 's'}`}
      onOpen=${() => openOnNode(node, { tab: 'processes', subtab: 'templates' })}>
      ${templates.length === 0 ? html`<div class="process-placeholder"><p>No process templates on ${node.name}.</p></div>`
        : html`<table><thead><tr><th>Template</th><th>Description</th><th>Latest</th><th>Versions</th><th></th></tr></thead><tbody>${templates.map((template) => {
          const latest = template.latestVersion || {};
          const actor = describeActor(latest.actor);
          return html`<tr key=${template.id} data-process-peer-template=${template.id}><td>${template.name || template.id}<div class="process-secondary" title="Template id (permanent)">${template.id}</div></td>
            <td class="process-description">${template.description || ''}</td>
            <td><span class="process-hash" title=${latest.semanticHash || ''}>${(latest.semanticHash || '').slice(0, 10) || '—'}</span>${actor && html`<div class="process-secondary process-version-actor">by ${actor.label}</div>`}</td>
            <td>${template.versionCount || 0}</td>
            <td class="process-actions"><${Button} size="sm" class="process-action" title=${`Open in the editor on ${node.name}'s own view`} onClick=${() => openOnNode(node, { tab: 'processes', subtab: 'templates', selection: template.id })}>open ↗<//><${Button} size="sm" icon=${true} variant="ghost danger" class="process-action process-delete-btn" title=${`Delete this template and all its versions on ${node.name}`} aria-label=${`Delete ${template.name || template.id} on ${node.name}`} onClick=${() => remove(node, template)}><${TrashIcon} /><//></td></tr>`;
        })}</tbody></table>`}
    </${NodeSection}>`;
  });
}

// EditorMoves are where the editor can go from here: an overlay window or a
// browser window of its own, or, in such a window, back to the dashboard.
function EditorMoves({ actions, state, spec }) {
  if (isProcessSolo()) {
    if (!openerTakesBack()) return null;
    const back = async (overlay) => {
      const editor = state.currentEditor();
      if (!editor) return;
      const label = editor.model?.template?.name || spec.name || spec.id;
      if (!(await editorBack({ id: spec.id, label, editor, overlay }))) {
        state.setNotice('The dashboard did not take the editor back; it stays here.');
        return;
      }
      state.setEditor(null);
      editor.destroy?.();
      globalThis.window?.close?.();
    };
    return html`<${Button} class="process-action" data-process-back title="Move this editor back to the dashboard's Processes tab, unsaved edits and all" onClick=${() => void back(false)}>${TO_DASHBOARD_LABEL}<//>${openerFloats() && html`<${Button} class="process-action" data-process-back-overlay title="Float this editor in a window over the dashboard, unsaved edits and all" onClick=${() => void back(true)}>Open in overlay<//>`}`;
  }
  return html`${overlayHostUp.value && html`<${Button} class="process-action" data-process-float title="Float this editor in a window over the dashboard, unsaved edits and all" onClick=${() => void actions.floatEditor?.()}>Open in overlay<//>`}<${Button} class="process-action" data-process-window title="Move this editor to a browser window of its own, unsaved edits and all" onClick=${(event) => void actions.editorToSeparateWindow?.({ rect: event.currentTarget.closest('#process-editor-view')?.getBoundingClientRect?.() })}>${TO_WINDOW_LABEL}<//>`;
}

export function ProcessesApp({ state, actions, confirmDiscard, fleet = fleetState, confirm = shellConfirm, notify = shellToast, fetchFor = undefined }) {
  const current = { ...state.view.value, state };
  // A fused view adds a section per ticked peer (fused-sections.js).
  const peers = fusedPeers(fleet);
  useEffect(() => { if (current.active) void actions.refreshActive(); }, [current.active]);
  useEffect(() => { const poll = () => { const view = state.view.value; if (view.active && view.canvas?.kind === 'editor') void actions.observeTemplateHeads(); }; document.addEventListener('tclaude:snapshot', poll); return () => document.removeEventListener('tclaude:snapshot', poll); }, []);
  // The dock's process library deletes and pins too; keep this list current.
  useEffect(() => { const changed = (event) => { if (event.detail?.source !== 'processes' && state.view.value.active) void actions.refreshActive(); }; document.addEventListener('tclaude:process-library-changed', changed); return () => document.removeEventListener('tclaude:process-library-changed', changed); }, []);
  useEffect(() => { const reselected = (event) => { if (event.detail?.tab === 'processes' && state.view.value.active) void actions.refreshActive(); }; document.addEventListener('tclaude:tab-reselected', reselected); return () => document.removeEventListener('tclaude:tab-reselected', reselected); }, []);
  const spec = current.canvas;
  // Processes is an Automations subtab: render the shared Automations row with
  // Processes active. Re-picking Processes returns to the template list.
  const selectEntry = (key) => { if (key === PROCESSES_SUBTAB) void actions.activateSubtab('templates'); else openAutomationsEntry(key); };
  return html`<div class="processes-island"><div class="process-subnav"><${AutomationsSubnav} entries=${automationsSubnavEntries(current.snapshot)} active=${PROCESSES_SUBTAB} onSelect=${selectEntry} /><span class="spacer"></span><span id="process-notice" class="process-notice" role="status">${current.notice}</span></div><${ScribeStatus} scribes=${current.scribes || []} actions=${actions} />${spec ? html`<div id="process-editor-view" class="process-canvas-view"><div class="process-canvas-tools"><${Button} class="process-action" data-process-close-view title=${withShortcut('Return to templates', 'process-open')} onClick=${actions.closeCanvas}>← templates<//>${spec.id && !spec.blank && html`<${EditorMoves} actions=${actions} state=${state} spec=${spec} />`}</div><${ProcessEditorBoundary} spec=${spec} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} /></div>` : html`<${Templates} current=${current} actions=${actions} />${peers.length ? html`<${FusedPeerProcesses} peers=${peers} active=${current.active} describeActor=${actions.describeActor} confirm=${confirm} notify=${notify} fetchFor=${fetchFor} />` : null}`}${current.create && html`<${CreateDialog} key=${current.create.key} spec=${current.create} busy=${current.mutation.busy} actions=${actions} />`}${current.rename && html`<${RenameDialog} key=${current.rename.key} spec=${current.rename} busy=${current.mutation.busy} actions=${actions} />`}</div>`;
}

export function mountProcessesIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  const unregisterCommands = registerCommandProvider('process-editor', () => {
    const view = state.view.value;
    const docked = view.active && view.canvas?.kind === 'editor' ? state.currentEditor() : null;
    return buildProcessEditorCommands({ editor: paletteEditor(docked), actions });
  });
  registerCleanup(unregisterCommands);
  registerCleanup(trackTouchedEditor(document));
  const restore = (event) => {
    const loc = event.detail?.location;
    if (loc?.tab === 'processes') void Promise.resolve(actions.applyLocation(loc)).catch(() => false).then((ok) => { if (!ok) actions.correctLocation(); });
  };
  document.addEventListener('tclaude:restore-location', restore);
  registerCleanup(() => document.removeEventListener('tclaude:restore-location', restore));
  // A process handed over from Groups › Processes (the quick start) opens in
  // the full editor: a template it just created, or the new-template dialog.
  const takeDraft = () => {
    const draft = takeProcessDraft();
    if (draft?.create) actions.openCreate();
    else if (draft?.id) {
      // The editor may hold unsaved edits of another template; the draft is
      // already saved, so declining only leaves it in Templates.
      void Promise.resolve(actions.canLeaveEditor ? actions.canLeaveEditor() : true).then((ok) => {
        if (ok) return actions.openEditor(draft.id, false, draft.name || '', { view: draft.view, readable: true });
        state.setNotice?.(`${draft.name || draft.id} is saved in Templates.`);
        return undefined;
      });
    }
  };
  document.addEventListener('tclaude:process-draft', takeDraft);
  registerCleanup(() => document.removeEventListener('tclaude:process-draft', takeDraft));
  takeDraft();
  setProcessTemplateDeleteHandler((spec) => actions.deleteTemplate(spec));
  const unbindTemplateDnd = bindProcessTemplateDnd();
  registerCleanup(() => { unbindTemplateDnd(); setProcessTemplateDeleteHandler(null); });
  // An editor a separate window never took waits for this tab's own editor to
  // close, and then comes back here.
  registerCleanup(effect(() => {
    const waiting = returningEditor.value;
    if (!waiting) return;
    if (state.canvas.value) {
      state.setNotice(`${waiting.label}'s unsaved edits are waiting: close this editor to get them back.`);
      return;
    }
    const back = takeReturningEditor();
    void actions.openEditor(back.id, false, back.label, { live: back.live });
  }));
  // An overlay window's process tab opens and docks its editor through here.
  const canLeave = () => (actions.canLeaveEditor ? actions.canLeaveEditor() : true);
  registerCleanup(registerProcessEditorHost({
    open: (mount, id, name, handoff = null, { view = null, readable = false } = {}) => loadTemplateEditor(mount, { id, name, handoff, view, config: processEditorConfig({ mount, readable, state, actions, confirmDiscard }) }),
    dock: (id, live, name) => {
      void actions.openEditor(id, false, name, { live });
      document.querySelector('nav [data-tab="processes"]')?.click();
    },
    release: (id) => actions.releaseEditor?.(id) || null,
    canLeave,
    confirmDiscard,
  }));
  // A browser window of the editor's own hands its draft back here, and this
  // page, when it is such a window, takes up the draft it opened for.
  if (isProcessSolo()) {
    document.body.classList.add('process-solo');
    const solo = soloHandoff();
    // Closing the window drops what it has not saved, so it asks first.
    const leaving = (event) => {
      if (!editorDirty(state.currentEditor())) return;
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', leaving);
    registerCleanup(() => window.removeEventListener('beforeunload', leaving));
    if (solo) {
      document.title = `${solo.label} — tclaude process`;
      registerCleanup(announceSolo(solo.id));
      void actions.openEditor(solo.id, false, solo.label, { handoff: { draft: solo.handoff, taken: solo.taken } });
    } else {
      // Too late: the dashboard took its editor back, so this window has nothing
      // of its own to show.
      window.close();
    }
  } else {
    registerCleanup(installProcessWindows(window, {
      // It answers once an editor here has taken the draft up, so the window
      // keeps it until then, and takes none for a template already open here.
      open: async (id, name, handoff, { overlay }) => {
        if (floatingProcessTab(id) || stripProcesses.peek().includes(id) || state.canvas.value?.id === id) return false;
        if (overlay && overlayHostUp.peek()) return floatHandoff(id, name, handoff);
        if (!(await canLeave())) return false;
        return new Promise((resolve) => {
          void actions.openEditor(id, false, name, { handoff: { draft: handoff, taken: () => resolve(true), failed: () => resolve(false) } });
          document.querySelector('nav [data-tab="processes"]')?.click();
        });
      },
      floats: () => overlayHostUp.peek(),
    }));
  }
  render(html`<${ProcessesApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  registerCleanup(() => render(null, host));
}
