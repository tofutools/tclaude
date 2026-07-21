import { h, render, Fragment } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isModifiedClick, relTime } from './helpers.js';
import { WORKLIST_VIEWS, actorLabel, dueBucket, fmtAge, fmtDue, groupWaitingOn, isActionable, kindMeta, nudgeLine } from './process-worklist-core.js';
import { registerCommandProvider } from './command-registry.js';
import { buildProcessEditorCommands } from './process-command-registry.js';
import { ProcessViewerBoundary } from './process-viewer-island.js';
import { bindProcessTemplateDnd, setProcessTemplateDeleteHandler } from './process-template-dnd.js';

const html = htm.bind(h);
const WORKLIST_TITLES = {
  'my-work': 'Pending items assigned to you (the operator)',
  'waiting-on': 'Everything pending, grouped by whom it waits on',
  due: 'Pending items with a deadline inside 24h or already past it',
  blocked: 'Nodes that exhausted their budget and wait on a retry / skip / cancel decision',
  decision: 'Decision nodes waiting on a human verdict',
  review: 'Gate-stage items waiting on a human review',
  recent: 'Items created or resolved within the last 24h',
};

function RequestBody({ request, label, retry, children }) {
  if (request.phase === 'loading' || request.phase === 'idle') return html`<p class="muted">Loading ${label}…</p>`;
  if (request.phase === 'error' && !children) return html`<div role="alert" class="island-error">Could not load ${label}: ${request.error} <button onClick=${retry}>retry</button></div>`;
  return html`<${Fragment}>${request.phase === 'error' && html`<div role="alert" class="island-error">Refresh failed: ${request.error} <button onClick=${retry}>retry</button></div>`}${children}</${Fragment}>`;
}

// Click-to-edit name in the list. Unlike the editor header, there is no draft
// to fold this into, so committing is an immediate CAS save through
// submitRename -- which is also why the row still offers the dialog, where the
// id and the consequences are spelled out.
function EditableName({ template, actions, busy }) {
  const [editing, setEditing] = useState(false);
  const inputRef = useRef(null);
  useLayoutEffect(() => {
    if (!editing) return;
    const input = inputRef.current;
    if (!input) return;
    // Seed explicitly; see EditableTitle.
    input.value = template.name || '';
    input.focus();
    // Not every DOM implementation provides select() (the linkedom test DOM
    // does not); selecting the existing name is a convenience, not a contract.
    input.select?.();
  }, [editing]);
  // One edit session resolves exactly once -- see EditableTitle for why: Enter
  // unmounts the input and the trailing blur would otherwise commit an empty
  // name read from a dead ref, here as a real CAS save.
  const settled = useRef(false);
  useLayoutEffect(() => { if (editing) settled.current = false; }, [editing]);
  // Read through the ref, not the event: see EditableTitle.
  const commit = async () => {
    if (settled.current) return;
    const input = inputRef.current;
    if (!input) return;
    settled.current = true;
    const next = String(input.value ?? '').trim();
    setEditing(false);
    if (next === (template.name || '')) return;
    // Commit directly. Routing through openRename would flash the dialog open
    // and immediately shut it again.
    try {
      await actions.renameTemplate({
        id: template.id, name: template.name || '', sourceHash: template.latestVersion?.sourceHash || '',
      }, next);
    } catch {
      // renameTemplate already surfaced the failure through the notice line.
    }
  };
  if (editing) {
    return html`<input
      ref=${inputRef} class="process-name-input" type="text" spellcheck="false"
      placeholder="display name" aria-label=${`Display name for ${template.id}`}
      data-process-name-input=${template.id} defaultValue=${template.name || ''}
      onBlur=${() => commit()}
      onKeyDown=${(event) => {
        if (event.isComposing || event.keyCode === 229) return;
        if (event.key === 'Enter') { event.preventDefault(); commit(); }
        if (event.key === 'Escape') { event.preventDefault(); settled.current = true; setEditing(false); }
      }}
    />`;
  }
  return html`<button
    class=${`process-name-edit${template.name ? '' : ' process-unnamed'}`}
    type="button" data-process-name-edit=${template.id} disabled=${busy}
    title="Click to rename" aria-label=${`Rename ${template.name || template.id}`}
    onClick=${() => setEditing(true)}
  >${template.name || template.id}</button>`;
}

// Same 24x24 outline trash used by the group member rows, so the two tabs read
// as one delete vocabulary. Wizard mode restyles it via body.wizard rules.
function TrashIcon() {
  return html`<svg class="trash-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path><line x1="10" y1="11" x2="10" y2="17"></line><line x1="14" y1="11" x2="14" y2="17"></line></svg>`;
}

function Templates({ current, actions }) {
  const rows = current.templates;
  return html`<div id="process-panel-templates" class="process-panel active" role="tabpanel" aria-label="Process templates">
    <div class="filter-bar process-toolbar"><strong>Reusable process graphs</strong><span class="spacer"></span><button id="process-scribe-library" class="process-action" type="button" title="Open a scoped agent that can safely author process templates" onClick=${() => actions.summonScribe({ kind: 'library' })}><span class="process-scribe-plain">Edit with agent</span><span class="process-scribe-wizard">Consult a process scribe</span></button><button id="process-template-new" class="process-action primary" type="button" onClick=${() => actions.openCreate()}>+ new template</button></div>
    <div id="process-templates-list" class="process-list" aria-busy=${current.requests.templates.phase === 'loading'}>
      <${RequestBody} request=${current.requests.templates} label="templates" retry=${() => actions.load('templates')}>
        ${rows.length === 0 ? html`<div class="process-placeholder"><h3>No process templates yet</h3><p>Create a blank template to start shaping a repeatable graph.</p></div>` : html`<table><thead><tr><th>Template</th><th>Description</th><th>Latest</th><th>Versions</th><th></th></tr></thead><tbody>
          ${rows.map((template) => { const latest = template.latestVersion || {}; const hash = (latest.semanticHash || '').slice(0, 10); const actor = actions.describeActor(latest.actor); return html`<tr key=${template.id} data-process-template=${template.id} draggable=${true} data-process-template-drag=${template.id} data-process-template-name=${template.name || ''} data-process-template-versions=${template.versionCount || 0}><td><${EditableName} template=${template} actions=${actions} busy=${current.mutation.busy} /><div class="process-secondary" title="Template id (permanent)">${template.id}</div></td><td class="process-description">${template.description || '—'}</td><td><span class="process-hash" title=${latest.semanticHash || ''}>${hash || '—'}</span>${actor && html`<div class="process-secondary process-version-actor">by ${actor.label}</div>`}</td><td>${template.versionCount || 0}</td><td class="process-actions"><button class="process-action" data-process-action="edit" data-id=${template.id} type="button" onClick=${() => actions.openEditor(template.id)}>open</button><button class="process-action" data-process-action="rename" data-id=${template.id} type="button" title="Change the display name; the id stays fixed" onClick=${() => actions.openRename({ id: template.id, name: template.name || '', sourceHash: latest.sourceHash || '' })}>rename</button><button class="process-action" data-process-action="instantiate" data-id=${template.id} type="button" onClick=${() => actions.openInstantiation({ id: template.id, ref: latest.ref })}>instantiate</button><button class="process-action process-action-danger process-delete-btn" data-process-action="delete" data-id=${template.id} type="button" disabled=${current.mutation.busy} title="Delete this template and all its versions — refused while an unfinished run still needs it" aria-label=${`Delete ${template.name || template.id}`} onClick=${() => actions.deleteTemplate({ id: template.id, name: template.name || '', versionCount: template.versionCount || 0 })}><${TrashIcon} /></button></td></tr>`; })}
        </tbody></table>`}
      </${RequestBody}>
    </div>
  </div>`;
}

function ScribeStatus({ scribes, actions }) {
  if (!scribes.length) return null;
  return html`<div class="process-scribe-status" role="status" aria-label="Process scribe sessions">
    <strong>Process ${scribes.length === 1 ? 'scribe' : 'scribes'}</strong>
    ${scribes.map((scribe) => html`<div key=${scribe.agentId} class="process-scribe-session" data-process-scribe=${scribe.agentId}>
      <span class=${`process-scribe-state ${scribe.online ? 'online' : 'stopped'}`}>${scribe.online ? 'active' : 'stopped'}</span>
      <button class="wl-link" type="button" disabled=${!scribe.online} title=${scribe.online ? 'Open the responsible conversation' : 'This session is stopped'} onClick=${() => actions.openScribe(scribe)}>${scribe.name}</button>
      <span class="process-secondary">${scribe.scopeLabel}</span>
      ${/^https?:\/\//i.test(scribe.taskURL) && html`<a class="process-scribe-task" href=${scribe.taskURL} title=${scribe.taskURL}>${scribe.taskLabel || 'task'}</a>`}
      ${scribe.online && html`<button class="process-action" type="button" data-process-scribe-action="stop" onClick=${() => actions.stopScribe(scribe)}>stop</button>`}
      <button class="process-action process-action-danger" type="button" data-process-scribe-action="retire" onClick=${() => actions.retireScribe(scribe)}>retire</button>
    </div>`)}
  </div>`;
}

function Runs({ current, actions }) {
  const highlighted = current.highlightedRun;
  useEffect(() => {
    if (!highlighted) return;
    const row = document.querySelector(`[data-process-run="${CSS.escape(highlighted)}"]`);
    if (!row) { current.state.setNotice(`Run ${highlighted} is not in the runs list.`); return; }
    row.scrollIntoView({ block: 'center', behavior: 'smooth' }); row.classList.add('wl-run-flash');
    current.state.setHighlightedRun(null);
  }, [highlighted, current.runs]);
  return html`<div id="process-panel-runs" class="process-panel active" role="tabpanel" aria-label="Process runs">
    <div class="filter-bar process-toolbar"><strong>Instantiated runs</strong><span class="spacer"></span><button id="process-runs-refresh" class="process-action" type="button" onClick=${() => actions.load('runs')}>↻ refresh</button></div>
    <div id="process-runs-list" class="process-list" aria-busy=${current.requests.runs.phase === 'loading'}><${RequestBody} request=${current.requests.runs} label="runs" retry=${() => actions.load('runs')}>
      ${current.runs.length === 0 ? html`<div class="process-placeholder"><h3>No process runs yet</h3><p>Instantiate a template to create a durable run.</p></div>` : html`<table><thead><tr><th>Run</th><th>Template</th><th>Status</th><th>Started</th><th>Current activity</th><th></th></tr></thead><tbody>${current.runs.map((run) => html`<tr key=${run.id} data-process-run=${run.id}><td><strong>${run.id}</strong></td><td><span class="process-hash" title=${run.templateRef || ''}>${shortProcessRef(run.templateRef) || '—'}</span></td><td><span class="process-status">${run.status || 'unknown'}</span>${run.adapted && html` <span class="process-adapted-badge">⟳ adapted</span>`}</td><td>${run.started ? relTime(run.started) : '—'}</td><td>${run.currentActivity || '—'}</td><td class="process-actions"><button class="process-action" data-process-action="view" data-id=${run.id} type="button" onClick=${() => actions.openViewer(run.id)}>open</button></td></tr>`)}</tbody></table>`}
    </${RequestBody}></div>
  </div>`;
}

function shortProcessRef(ref) { const marker = '@sha256:'; const at = ref?.indexOf(marker) ?? -1; return at < 0 ? (ref || '') : `${ref.slice(0, at)}${marker}${ref.slice(at + marker.length, at + marker.length + 10)}`; }

function ItemActions({ item, current, actions }) {
  if (item.kind === 'agent-obligation') return html`<span class="process-secondary" title="Agent obligations are reported by the working agent through the run/node report route with a durable evidence ref — they cannot be resolved from this list.">agent reports via evidence</span>`;
  if (!isActionable(item)) return '—';
  const missing = current.missingComments.has(item.id);
  const submit = async (event, action) => { const ok = await actions.submitWorklistAction(item.id, action); if (!ok && current.state.missingComments.value.has(item.id)) event.currentTarget.closest('td')?.querySelector('input')?.focus(); };
  return html`<${Fragment}><input class=${`wl-comment${missing ? ' wl-comment-missing' : ''}`} type="text" data-worklist-comment=${item.id} placeholder="Comment (required)" value=${current.drafts[item.id] || ''} aria-label=${`Comment for ${item.summary || item.id}`} onInput=${(event) => current.state.setDraft(item.id, event.currentTarget.value)} /><div class="wl-action-row">${(item.availableActions || []).map((action) => html`<button key=${action} disabled=${current.mutation.busy} class="process-action wl-action" data-worklist-action=${action} data-worklist-item=${item.id} type="button" onClick=${(event) => submit(event, action)}>${action}</button>`)}</div></${Fragment}>`;
}

function WorkItemRow({ item, current, actions, now }) {
  const meta = kindMeta(item.kind); const bucket = dueBucket(item, now); const nudge = nudgeLine(item.nudge);
  const cls = ['wl-row', bucket ? `wl-${bucket}` : '', item.status !== 'pending' ? 'wl-resolved' : '', item.detached ? 'wl-detached' : ''].filter(Boolean).join(' ');
  return html`<tr key=${item.id} class=${cls} data-key=${item.id}><td class="wl-kind"><span class="wl-glyph">${meta.glyph}</span> ${meta.label}${item.detached && html` <span class="wl-detached-badge" title=${`${item.detachmentCount || 1} reservation-relative detachment${(item.detachmentCount || 1) === 1 ? '' : 's'}`}>⑂ detached</span>`}${item.ownerEpoch && html` <span class="wl-epoch-badge">◈ epoch ${item.ownerEpoch.ordinal}</span>`}${item.status !== 'pending' && html` <span class="process-status">${item.status}</span>`}</td><td class="wl-main"><div class="wl-summary">${item.summary || '—'}</div>${nudge && html`<div class=${`wl-nudge process-secondary${item.nudge?.paused ? ' wl-paused' : ''}`}>${nudge}</div>`}</td><td class="wl-where"><button class="wl-link" data-worklist-run=${item.run} type="button" onClick=${() => actions.openRunInList(item.run)}>${item.run}</button><div class="process-secondary"><button class="wl-link wl-link-node" data-worklist-run=${item.run} type="button" onClick=${() => actions.openRunInList(item.run)}>${item.node}</button>${item.attempt > 1 ? ` · attempt ${item.attempt}` : ''}</div></td><td class="wl-assignee">${actorLabel(item.assignee)}</td><td class="wl-age" title=${item.createdAt || 'not recorded'}>${fmtAge(item.createdAt, now)}</td><td class="wl-due" title=${item.dueAt || 'no deadline recorded'}>${fmtDue(item.dueAt, now)}</td><td class="wl-actions"><${ItemActions} item=${item} current=${current} actions=${actions} /></td></tr>`;
}

function Worklist({ current, actions }) {
  const now = Date.now(); const work = current.worklist; const rows = current.worklistRows;
  let tableRows = rows.map((item) => html`<${WorkItemRow} key=${item.id} item=${item} current=${current} actions=${actions} now=${now} />`);
  if (current.worklistView === 'waiting-on') tableRows = groupWaitingOn(rows).flatMap((group) => [html`<tr key=${`who-${group.assignee || 'unassigned'}`} class="wl-group-head"><td colspan="7">Waiting on ${group.label} · ${group.items.length}</td></tr>`, ...group.items.map((item) => html`<${WorkItemRow} key=${item.id} item=${item} current=${current} actions=${actions} now=${now} />`)]);
  const pending = (work.items || []).filter((item) => item.status === 'pending').length;
  const emptyTitle = !work.items?.length && current.runs.length === 0 ? 'No process runs yet' : rows.length ? '' : `Nothing in “${WORKLIST_VIEWS.find((view) => view.key === current.worklistView)?.label || current.worklistView}”`;
  return html`<div id="process-panel-worklist" class="process-panel active" role="tabpanel" aria-label="Process worklist"><div class="filter-bar process-toolbar"><div class="process-worklist-views" role="group" aria-label="Worklist views">${WORKLIST_VIEWS.map((view) => html`<button key=${view.key} class=${`process-view-chip${current.worklistView === view.key ? ' active' : ''}`} data-worklist-view=${view.key} type="button" aria-pressed=${current.worklistView === view.key} title=${WORKLIST_TITLES[view.key]} onClick=${() => current.state.setWorklistView(view.key)}>${view.label}<span class="wl-view-count">${current.worklistCounts[view.key] || ''}</span></button>`)}</div><span class="spacer"></span><button id="process-worklist-refresh" class="process-action" type="button" onClick=${() => actions.load('worklist')}>↻ refresh</button></div>
    <div id="process-worklist-degraded" class="wl-degraded" role="alert" hidden=${!work.degradedRuns?.length}>${work.degradedRuns?.length ? html`<${Fragment}><span class="wl-degraded-glyph">⚠</span> ${work.degradedRuns.length} run${work.degradedRuns.length === 1 ? '' : 's'} could not be read (their work items are missing from this list): ${work.degradedRuns.map((run) => html`<span key=${run.run} class="wl-degraded-run" title=${run.error || ''}>${run.run}</span>`)}</${Fragment}>` : null}</div>
    <div id="process-worklist-list" class="process-list" aria-busy=${current.requests.worklist.phase === 'loading'}><${RequestBody} request=${current.requests.worklist} label="worklist" retry=${() => actions.load('worklist')}>
      ${rows.length ? html`<table><thead><tr><th>Kind</th><th>Work item</th><th>Run / node</th><th>Assignee</th><th>Age</th><th>Due</th><th>Actions</th></tr></thead><tbody>${tableRows}</tbody></table>` : html`<div class="process-placeholder"><h3>${emptyTitle}</h3><p>${pending ? `${pending} pending item${pending === 1 ? '' : 's'} in other views.` : current.runs.length ? 'No process run is waiting on anyone.' : 'The worklist fills as instantiated runs wait on people or hit blocks.'}</p></div>`}
    </${RequestBody}></div></div>`;
}

export function ProcessEditorBoundary({ spec, state, actions, confirmDiscard, openEditor = null }) {
  const mountRef = useRef(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let disposed = false; let editor = null;
    setError('');
    const loadEditor = openEditor || (async (mount, value) => {
      const { openTemplateEditor } = await import('./process-editor.js');
      return openTemplateEditor(mount, value);
    });
    loadEditor(mountRef.current, {
      id: spec.id, blank: spec.blank, name: spec.name, view: spec.view,
      config: {
        confirmDiscard,
        onInstantiate: actions?.openInstantiation ? (value) => actions.openInstantiation(value) : undefined,
        onScribe: actions?.summonScribe ? (value, options) => actions.summonScribe(value, options) : undefined,
        describeActor: actions?.describeActor ? (value) => actions.describeActor(value) : undefined,
        onOpenActor: actions?.openActor ? (value) => actions.openActor(value) : undefined,
      },
    }).then((value) => { editor = value; if (disposed) editor?.destroy?.(); else state.setEditor(editor); }).catch((error) => { if (!disposed) { setError(error.message); state.setNotice(`Could not open editor: ${error.message}`); } });
    return () => { disposed = true; state.setEditor(null); editor?.destroy?.(); };
  }, [spec.key]);
  return html`<div id="process-editor-canvas" ref=${mountRef} class="process-canvas-mount" data-process-mount="editor">${error && html`<div class="process-placeholder" role="alert">Could not open editor: ${error}</div>`}</div>`;
}

// Ctrl/Cmd+Enter is the dashboard-wide confirm hotkey (helpers.js
// bindModalSubmitHotkey for imperative modals, fieldSubmit in the Preact
// dialogs). Both modifiers are accepted on every platform so no OS sniffing is
// needed, and IME composition is excluded so committing a candidate never
// submits the form. Plain Enter is left to the browser's native form handling.
//
// Bound per field rather than on the form, matching fieldSubmit: a keydown
// handler on an ancestor makes Preact register its shared event proxy for a
// type the descendants never registered, which the linkedom test DOM then
// dispatches against the wrong element.
export function fieldSubmitHotkey(submit) {
  return (event) => {
    if (event.key !== 'Enter' || (!event.ctrlKey && !event.metaKey) || event.isComposing || event.keyCode === 229) return;
    event.preventDefault();
    submit();
  };
}

function paramDefaultText(value) {
  if (value === undefined) return '';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  try { return JSON.stringify(value); } catch { return String(value); }
}

function initialParamValues(params) {
  return Object.fromEntries(params.map(([name, param]) => [name,
    param.default !== undefined ? paramDefaultText(param.default) : '',
  ]));
}

function InstantiateDialog({ spec, busy, actions }) {
  const params = Object.entries(spec.template?.params || {}).sort(([a], [b]) => a.localeCompare(b, 'en'));
  const firstParamRef = useRef(null);
  const createRef = useRef(null);
  const initializedRef = useRef(spec.phase === 'ready' && spec.template ? spec.ref : '');
  const [values, setValues] = useState(() => initialParamValues(params));
  const close = () => { if (!busy) actions.closeInstantiation(); };
  const { dialogRef } = useDialogFocus({
    open: true, initialFocusRef: params.length ? firstParamRef : createRef, onEscape: close,
  });
  // List-row instantiation opens in loading state, then fills the same keyed
  // component with an exact template. Initialize once at that transition;
  // later refreshes of the same ref must not overwrite user edits.
  useLayoutEffect(() => {
    if (spec.phase !== 'ready' || !spec.template || initializedRef.current === spec.ref) return;
    initializedRef.current = spec.ref;
    setValues(initialParamValues(params));
    queueMicrotask(() => (params.length ? firstParamRef.current : createRef.current)?.focus());
  }, [spec.phase, spec.ref, spec.template]);
  const change = (name, value) => setValues((current) => ({ ...current, [name]: value }));
  const submit = (event) => {
    event?.preventDefault();
    const resolved = {};
    for (const [name, param] of params) {
      const value = values[name] ?? '';
      if (param.type === 'number' && value !== '') resolved[name] = value;
      else if (param.type === 'boolean' && (value === 'true' || value === 'false')) resolved[name] = value;
      else if (value !== '' || param.required === true || param.default !== undefined) resolved[name] = value;
    }
    void actions.submitInstantiation(resolved);
  };
  return html`<div class="modal-overlay show process-instantiate-modal" onClick=${(event) => { if (event.target === event.currentTarget) close(); }}><form ref=${dialogRef} class="modal process-instantiate-dialog" role="dialog" aria-modal="true" aria-labelledby="process-instantiate-title" onSubmit=${submit}>
    <h3 id="process-instantiate-title">Instantiate ${spec.template?.name || spec.id}</h3>
    <p class="muted">This run pins <span class="process-hash" title=${spec.ref}>${shortProcessRef(spec.ref)}</span>.</p>
    ${spec.phase === 'loading' ? html`<p class="muted">Loading exact template version…</p>` : spec.phase === 'error' ? html`<div class="island-error" role="alert">Could not load template: ${spec.error}</div>` : params.length ? html`<div class="process-instantiate-fields">${params.map(([name, param], index) => {
      const label = param.name || name; const required = param.required === true; const type = param.type || 'string';
      const input = type === 'boolean'
        ? html`<label><span>${label}${required ? ' *' : ''}</span><select ref=${index === 0 ? firstParamRef : undefined} data-process-param-input=${name} required=${required && param.default === undefined} value=${values[name] ?? ''} onChange=${(event) => change(name, event.currentTarget.value ?? [...event.currentTarget.options].find((option) => option.selected)?.value ?? '')}><option value="">Not set</option><option value="true">true</option><option value="false">false</option></select></label>`
        : html`<label><span>${label}${required ? ' *' : ''}</span><input ref=${index === 0 ? firstParamRef : undefined} data-process-param-input=${name} type=${type === 'number' ? 'number' : 'text'} step=${type === 'number' ? 'any' : undefined} required=${required && param.default === undefined} value=${values[name] ?? ''} onInput=${(event) => change(name, event.currentTarget.value)} /></label>`;
      return html`<div key=${name} class="process-instantiate-field" data-process-param=${name}>${input}<div class="process-secondary">${param.description || name}${type !== 'string' ? ` · ${type}` : ''}</div></div>`;
    })}</div>` : html`<p class="process-placeholder-inline">This template declares no parameters.</p>`}
    <div class="modal-buttons"><button type="button" disabled=${busy} onClick=${close}>Cancel</button><button ref=${createRef} class="primary" type="submit" disabled=${busy || spec.phase !== 'ready'}>${busy ? 'Creating…' : 'Create run'}</button></div>
  </form></div>`;
}

// Renames the display name only. The id below the field is the immutable store
// key every pinned ref and live run resolves through, so it is shown for
// orientation rather than offered as an editable field.
function RenameDialog({ spec, busy, actions }) {
  const nameRef = useRef(null);
  const [name, setName] = useState(spec.name || '');
  const close = () => { if (!busy) actions.closeRename(); };
  const { dialogRef } = useDialogFocus({ open: true, initialFocusRef: nameRef, onEscape: close });
  const submit = (event) => { event?.preventDefault(); void actions.submitRename(name); };
  // process-editor-modal is the editor's shared modal skin contract: it carries
  // the --process-control-* variables and the wizard button/field treatment, so
  // this dialog matches its siblings instead of needing its own theme rules.
  return html`<div class="modal-overlay show process-editor-modal process-rename-modal" onClick=${(event) => { if (event.target === event.currentTarget) close(); }}><form ref=${dialogRef} class="modal process-rename-dialog" role="dialog" aria-modal="true" aria-labelledby="process-rename-title" onSubmit=${submit}>
    <h3 id="process-rename-title">Rename template</h3>
    <div class="field process-editor-field process-rename-field">
      <label for="process-rename-input">Display name</label>
      <input ref=${nameRef} id="process-rename-input" data-process-rename-input type="text" autocomplete="off" spellcheck="false" placeholder="display name" value=${name} data-select-on-focus onInput=${(event) => setName(event.currentTarget.value)} onKeyDown=${fieldSubmitHotkey(() => { if (!busy) submit(); })} />
    </div>
    <p class="muted">Shown wherever this template is listed. Leave it empty to fall back to the id. The id <code>${spec.id}</code> is permanent and keeps existing runs and pinned versions working.</p>
    ${spec.error && html`<div class="island-error" role="alert">${spec.error}</div>`}
    <div class="modal-buttons"><button type="button" disabled=${busy} onClick=${close}>Cancel</button><button class="primary" type="submit" disabled=${busy}>${busy ? 'Saving…' : 'Save name'}</button></div>
  </form></div>`;
}

// Creation asks for a name only. Submitting persists the scaffold and the store
// returns its permanent id before the editor opens, so there is nothing here
// for an operator to get permanently wrong.
function CreateDialog({ spec, busy, actions }) {
  const nameRef = useRef(null);
  const [name, setName] = useState(spec.name || '');
  const outcomeUnknown = !!spec.attempt?.blocked && name.trim() === spec.attempt.name;
  const close = () => { if (!busy) actions.closeCreate(); };
  const { dialogRef } = useDialogFocus({ open: true, initialFocusRef: nameRef, onEscape: close });
  const submit = (event) => { event?.preventDefault(); if (!busy && !outcomeUnknown) void actions.submitCreate(name); };
  return html`<div class="modal-overlay show process-editor-modal process-rename-modal" onClick=${(event) => { if (event.target === event.currentTarget) close(); }}><form ref=${dialogRef} class="modal process-rename-dialog" role="dialog" aria-modal="true" aria-labelledby="process-create-title" onSubmit=${submit}>
    <h3 id="process-create-title">New process template</h3>
    <div class="field process-editor-field process-rename-field">
      <label for="process-create-input">Display name</label>
      <input ref=${nameRef} id="process-create-input" data-process-create-input type="text" autocomplete="off" spellcheck="false" placeholder="e.g. Release train" value=${name} disabled=${busy} onInput=${(event) => setName(event.currentTarget.value)} onKeyDown=${fieldSubmitHotkey(() => submit())} />
    </div>
    <p class="muted">You can rename this at any time. Creating the template assigns its permanent id automatically.</p>
    ${spec.error && html`<div class="island-error" role="alert">${spec.error}</div>`}
    <div class="modal-buttons"><button type="button" disabled=${busy} onClick=${close}>Cancel</button><button class="primary" type="submit" disabled=${busy || !name.trim() || outcomeUnknown}>${busy ? 'Creating…' : 'Create'}</button></div>
  </form></div>`;
}

export function ProcessesApp({ state, actions, confirmDiscard }) {
  const current = { ...state.view.value, state };
  useEffect(() => { if (current.active) void actions.refreshActive(); }, [current.active]);
  useEffect(() => { const poll = () => {
    const view = state.view.value;
    if (!view.active) return;
    void actions.load('worklist', { quiet: true });
    if (view.subtab === 'templates' || view.canvas?.kind === 'editor') void actions.observeTemplateHeads();
  }; document.addEventListener('tclaude:snapshot', poll); return () => document.removeEventListener('tclaude:snapshot', poll); }, []);
  useEffect(() => { const reselected = (event) => { if (event.detail?.tab === 'processes' && state.view.value.active) void actions.refreshActive(); }; document.addEventListener('tclaude:tab-reselected', reselected); return () => document.removeEventListener('tclaude:tab-reselected', reselected); }, []);
  const navigate = async (event, name) => { if (isModifiedClick(event)) return; event.preventDefault(); await actions.activateSubtab(name); };
  const subtabKey = (event) => { if (event.key === ' ' || event.key === 'Spacebar') { event.preventDefault(); event.currentTarget.click(); } };
  const spec = current.canvas;
  const viewerBackRef = useRef(null);
  useLayoutEffect(() => {
    if (spec?.kind === 'viewer') viewerBackRef.current?.focus({ preventScroll: true });
  }, [spec?.key]);
  return html`<div class="processes-island"><div class="process-subnav" role="tablist" aria-label="Process views">${['templates', 'runs', 'worklist'].map((name) => html`<a key=${name} class=${`process-subtab${current.subtab === name ? ' active' : ''}`} data-process-subtab=${name} href=${`/processes/${name}`} role="tab" aria-selected=${current.subtab === name} onClick=${(event) => navigate(event, name)} onKeyDown=${subtabKey}>${name[0].toUpperCase() + name.slice(1)}${name === 'worklist' && html`<span id="process-worklist-badge" class="tab-badge warn" hidden=${current.actionable === 0}>${current.actionable}</span>`}</a>`)}<span class="spacer"></span><span id="process-notice" class="process-notice" role="status">${current.notice}</span></div>
    <${ScribeStatus} scribes=${current.scribes || []} actions=${actions} />
    ${spec ? html`<div id=${spec.kind === 'editor' ? 'process-editor-view' : 'process-viewer-view'} class=${`process-canvas-view${spec.kind === 'editor' ? ' process-scroll-surface' : ''}`}><button ref=${spec.kind === 'viewer' ? viewerBackRef : undefined} class="process-action" data-process-close-view type="button" onClick=${actions.closeCanvas}>← ${current.subtab}</button>${spec.kind === 'editor' ? html`<${ProcessEditorBoundary} spec=${spec} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />` : html`<${ProcessViewerBoundary} spec=${spec} actions=${actions} active=${current.active} />`}</div>` : current.subtab === 'templates' ? html`<${Templates} current=${current} actions=${actions} />` : current.subtab === 'runs' ? html`<${Runs} current=${current} actions=${actions} />` : html`<${Worklist} current=${current} actions=${actions} />`}
    ${current.instantiation && html`<${InstantiateDialog} key=${current.instantiation.key} spec=${current.instantiation} busy=${current.mutation.busy} actions=${actions} />`}
    ${current.create && html`<${CreateDialog} key=${current.create.key} spec=${current.create} busy=${current.mutation.busy} actions=${actions} />`}
    ${current.rename && html`<${RenameDialog} key=${current.rename.key} spec=${current.rename} busy=${current.mutation.busy} actions=${actions} />`}
  </div>`;
}

export function mountProcessesIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  const unregisterCommands = registerCommandProvider('process-editor', () => {
    const view = state.view.value;
    if (!view.active || view.canvas?.kind !== 'editor') return [];
    return buildProcessEditorCommands({ editor: state.currentEditor(), actions });
  });
  registerCleanup(unregisterCommands);

  // The history router (js/nav-history.js) resolved a Processes location from
  // the URL — a deep link, a reload, or a browser Back/Forward — and wants this
  // tab to show it, editor included.
  //
  // Registered HERE, synchronously during mount, rather than in a component
  // effect: dashboard.js awaits this island before calling initNavHistory(), so
  // attaching the listener on the mount path is what guarantees the router's
  // one-shot restore event cannot be dispatched into the void. A Preact effect
  // would only run on the next flush, and a deep link that lost the race would
  // show the list with the editor's URL in the address bar, permanently.
  //
  // applyLocation resolves false when it did not land where it was asked
  // (a refused discard, or a location this tab cannot show) — correct the URL
  // so it never describes a view that is not on screen. A rejection is treated
  // the same way rather than left as an unhandled rejection.
  const restore = (event) => {
    const loc = event.detail?.location;
    if (loc?.tab !== 'processes') return;
    void Promise.resolve(actions.applyLocation(loc))
      .catch(() => false)
      .then((ok) => { if (!ok) actions.correctLocation(); });
  };
  document.addEventListener('tclaude:restore-location', restore);
  registerCleanup(() => document.removeEventListener('tclaude:restore-location', restore));
  // The drag-to-bin delete lives in a document-level module (the overlay bin is
  // outside this island), so hand it the same commit the row button calls and
  // bind it for as long as the island is mounted.
  setProcessTemplateDeleteHandler((spec) => actions.deleteTemplate(spec));
  const unbindTemplateDnd = bindProcessTemplateDnd();
  registerCleanup(() => { unbindTemplateDnd(); setProcessTemplateDeleteHandler(null); });
  render(html`<${ProcessesApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  // Rendering null unmounts ProcessEditorBoundary, the sole owner of editor /
  // graph disposal. Do not destroy through state here as well.
  registerCleanup(() => render(null, host));
}
