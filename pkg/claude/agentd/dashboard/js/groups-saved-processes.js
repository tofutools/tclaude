// groups-saved-processes.js -- a group's saved processes in the Groups ›
// Processes perspective (mock Q3, tcl-7p6bgt): chips at the top of the
// group's body, one per process saved to the group (context
// groups[].processes, in pin order). ▶ opens a start strip that asks only for
// the process's start inputs and command consent, then starts it in the
// group (a new one is built from the group's process buttons). The strip
// also edits the steps, publishes the process to the
// template library, shares it as a .pwf, or removes it from the group; the chip's × opens the
// strip straight at that removal confirmation, and dragging a chip onto the
// bin removes it too (groups-saved-process-dnd.js).

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { unauthorizedProfiles } from './process-context.js';
import { viewToStepList } from './process-step-list.js';
import { defaultNotify, markQuickStart } from './groups-quickstart.js';
import { DraftStartWarning, noteDraftOpened, reopenScribeDraft, scribeDraftsOf, scribeStartWarning } from './groups-scribe-draft.js';
import { bindSavedProcessDnd, registerSavedProcessRemover } from './groups-saved-process-dnd.js';
import { WorkflowExportButton } from './process-workflow-share.js';
import { shellConfirm } from './shell-state.js';

const html = htm.bind(h);

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
}

// initialValues prefills each start input with its default: the server binds
// only what is sent.
function initialValues(process) {
  return Object.fromEntries((process.params || [])
    .filter((param) => param.default !== undefined && param.default !== null)
    .map((param) => [param.name, String(param.default)]));
}

// StartStrip is the start form of one saved process.
function StartStrip({ group, process, api, source, wizard, notify, editing = '', removing = false, scribeDraft = null, scribes = [], onRemoving, onEdit, onClose }) {
  const [values, setValues] = useState(() => initialValues(process));
  const [authorized, setAuthorized] = useState(() => new Set());
  const [extraProfiles, setExtraProfiles] = useState([]);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  // Set while Start asks about a scribe's unreviewed work; see scribeStartWarning.
  const [draftWarning, setDraftWarning] = useState(null);
  const params = process.params || [];
  const profiles = [...new Set([...(process.programProfiles || []), ...extraProfiles])].sort();
  const missing = params.filter((param) => param.required && !String(values[param.name] ?? '').trim());
  const unconsented = profiles.filter((profile) => !authorized.has(profile));
  const blockedWhy = missing.length ? `Fill ${missing.map((param) => param.name).join(', ')} first`
    : unconsented.length ? 'Allow its commands to run first' : '';
  const act = async (kind, work) => {
    if (busy) return;
    setBusy(kind);
    setError('');
    try {
      await work();
    } catch (cause) {
      const named = unauthorizedProfiles(cause.message);
      if (named.length) setExtraProfiles((current) => [...new Set([...current, ...named])]);
      setError(cause.message);
    } finally {
      setBusy('');
    }
  };
  const begin = async (expectedRef = '') => {
    await api.startGroupRun({
      templateId: process.templateId, group: group.name, params: values,
      authorizeProgramProfiles: profiles.filter((profile) => authorized.has(profile)),
      ...(expectedRef ? { expectedRef } : {}),
    });
    markQuickStart();
    notify(`Started ${process.name} in ${group.name}`);
    void source?.refresh?.();
    onClose();
  };
  const start = (event) => {
    event.preventDefault();
    if (blockedWhy || draftWarning) return;
    void act('start', async () => {
      const warning = await scribeStartWarning(api, scribes, process.templateId);
      if (warning) setDraftWarning(warning);
      else await begin();
    });
  };
  // Pinned to the version the question was about: a save since then asks again
  // instead of running what nobody was asked about.
  const startAnyway = () => act('start', async () => {
    try {
      await begin(draftWarning.ref);
    } catch (cause) {
      if (cause.body?.code !== 'process_run_template_moved') throw cause;
      const warning = await scribeStartWarning(api, scribes, process.templateId);
      setDraftWarning(warning);
      if (!warning) setError(`${process.name} changed since you were asked. Press Start to run the current version.`);
    }
  });
  // A process the step list cannot show opens as the inline graph.
  const edit = () => act('edit', async () => {
    const view = await api.getTemplate(process.templateId);
    noteDraftOpened(scribes, process.templateId, view);
    const converted = viewToStepList(view);
    if (!converted.ok) {
      onEdit({ mode: 'graph', view, process });
      return;
    }
    onEdit({
      list: converted.list, process, stored: view,
      saved: { templateId: process.templateId, sourceHash: String(view.sourceHash || ''), ref: String(view.currentRef || ''), listed: view.listed ?? process.listed },
    });
  });
  const publish = () => act('publish', async () => {
    await api.publishTemplate(process.templateId);
    notify(`Published ${process.name} to the template library`);
    void source?.refresh?.();
  });
  const remove = () => act('remove', async () => {
    await api.unpinProcess(group.name, process.templateId);
    notify(`Removed ${process.name} from ${group.name}`);
    void source?.refresh?.();
    onClose();
  });
  const toggle = (profile, on) => setAuthorized((current) => {
    const next = new Set(current);
    if (on) next.add(profile);
    else next.delete(profile);
    return next;
  });
  return html`<form class="gq-strip" aria-label=${`Start ${process.name} in ${group.name}`} data-saved-strip=${process.templateId} onSubmit=${start}
    onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose(); } }}>
    <span class="gq-strip-title">▶ ${wizard ? 'Begin' : 'Run'} <b>${process.name}</b></span>
    ${params.map((param) => html`<label key=${param.name} class="gq-param" title=${param.description || ''}>
      <span>${param.name}${param.required ? html`<b aria-label="required"> *</b>` : ''}</span>
      <input type=${param.type === 'number' ? 'number' : 'text'} data-start-param=${param.name} disabled=${!!busy}
        value=${values[param.name] ?? ''}
        onInput=${(event) => { const value = event.currentTarget.value; setValues((current) => ({ ...current, [param.name]: value })); }} />
    </label>`)}
    ${profiles.map((profile) => html`<label key=${profile || '(default)'} class="gq-profile" data-consent-profile=${profile}>
      <input type="checkbox" data-start-profile=${profile} disabled=${!!busy} checked=${authorized.has(profile)}
        onChange=${(event) => toggle(profile, event.currentTarget.checked)} />
      <span>Allow its commands to run on this host <span class="gq-dim">(program profile: ${profile || 'default'})</span></span>
    </label>`)}
    <span class="gq-strip-acts">
    <button type="submit" class="gq-btn primary" data-saved-start disabled=${!!busy || !!blockedWhy || !!draftWarning} title=${blockedWhy}>${busy === 'start' ? 'starting…' : 'Start'}</button>
    ${blockedWhy && html`<span class="gq-why" data-start-blocked>${blockedWhy}</span>`}
    ${draftWarning && html`<${DraftStartWarning} warning=${draftWarning} busy=${!!busy} openDisabled=${!!editing} onOpen=${edit} onStartAnyway=${startAnyway} />`}
    <button type="button" class="gq-btn" data-saved-edit disabled=${!!busy || !!editing} onClick=${edit}
      title=${editing ? 'Save or cancel the open editor first' : 'Edit the steps; saving writes a new version'}>${busy === 'edit' ? 'opening…' : '✎ Edit steps'}</button>
    ${scribeDraft && html`<button type="button" class="gq-btn" data-saved-scribe disabled=${!!busy || !!editing}
      title=${editing ? 'Save or cancel the open editor first' : 'Show the scribe drafting this process'} onClick=${() => { reopenScribeDraft(process.templateId); onEdit({ mode: 'scribe', draft: scribeDraft }); }}>✎ Scribe</button>`}
    ${!process.listed && html`<button type="button" class="gq-link" data-saved-publish disabled=${!!busy} onClick=${publish}
      title="List it in the template library, for every group to reuse">publish as template</button>`}
    <${WorkflowExportButton} id=${process.templateId} className="gq-link" />
    ${removing
    ? html`<span class="gq-confirm" role="group" aria-label="Confirm removal">
        ${process.listed ? `Remove from ${group.name}? It stays in the template library.` : `Remove from ${group.name}? It is not in the template library, so publish it first to keep it.`}
        <button type="button" class="gq-btn" data-saved-remove-confirm disabled=${!!busy} onClick=${remove}>remove</button>
        <button type="button" class="gq-link" onClick=${() => onRemoving(false)}>keep</button></span>`
    : html`<button type="button" class="gq-link" data-saved-remove disabled=${!!busy} onClick=${() => onRemoving(true)}
      title=${`Remove ${process.name} from ${group.name}'s processes`}>remove</button>`}
    </span>
    <span class="gq-spacer"></span>
    <button type="button" class="gq-link" aria-label="Close" onClick=${onClose}>✕</button>
    ${error && html`<div class="gq-error" role="alert">${error}</div>`}
  </form>`;
}

// SavedProcesses is the group's saved-process chips and the open strip.
// `editing` is the template id open in the inline editor below; ✎ opens one
// there as a graph.
export function SavedProcesses({ group, processes, api, source, wizard = false, notify = defaultNotify, editing = '', scribes = [], onEdit, confirm = shellConfirm }) {
  const [open, setOpen] = useState('');
  const [removing, setRemoving] = useState(false);
  const [loading, setLoading] = useState('');
  const editGraph = async (process) => {
    if (loading) return;
    setLoading(process.templateId);
    try {
      const view = await api.getTemplate(process.templateId);
      noteDraftOpened(scribes, process.templateId, view);
      setOpen('');
      onEdit({ mode: 'graph', view, process });
    } catch (cause) {
      notify(`Could not open ${process.name}: ${cause.message}`, true);
    } finally {
      setLoading('');
    }
  };
  // A chip dropped on the bin: a listed process unpins at once (the library
  // keeps it); a group-only one asks first, as the strip's remove does.
  const removeDropped = async (templateId) => {
    const process = processes.find((candidate) => candidate.templateId === templateId);
    if (!process) return;
    if (!process.listed && !(await confirm({
      title: `Remove ${process.name} from ${group.name}?`,
      body: 'It is not in the template library, so publish it first to keep it.',
      okLabel: 'Remove',
    }))) return;
    try {
      await api.unpinProcess(group.name, templateId);
      if (open === templateId) setOpen('');
      notify(`Removed ${process.name} from ${group.name}`);
      void source?.refresh?.();
    } catch (cause) {
      notify(`Could not remove ${process.name}: ${cause.message}`, true);
    }
  };
  useEffect(() => bindSavedProcessDnd(), []);
  useEffect(() => registerSavedProcessRemover(group.name, removeDropped));
  const current = processes.find((process) => process.templateId === open) || null;
  return html`<div class="gq-saved" data-saved-processes=${group.name}>
    <div class="gq-saved-row">
      <span class="gq-saved-label">${wizard ? 'Rites' : 'Processes'}</span>
      ${processes.map((process) => html`<span key=${process.templateId} class=${`gq-saved-chip${open === process.templateId ? ' open' : ''}`}
        data-saved-process=${process.templateId} draggable="true" data-saved-process-drag=${process.templateId}
        data-saved-process-group=${group.name} data-saved-process-name=${process.name}
        title=${`Drag onto the bin to remove ${process.name} from ${group.name}`}>
        <span class="gq-saved-name" title=${process.listed ? 'Also in the template library' : `Saved to ${group.name} only`}>${process.name}</span>
        <span class="gq-dim">${plural(process.steps || 0, 'step')}</span>
        ${editing === process.templateId
    ? html`<span class="gq-saved-editing" data-saved-editing=${process.templateId}>✎ editing</span>`
    : html`<button type="button" class="gq-saved-edit" data-saved-edit-graph=${process.templateId} disabled=${!!loading || !!editing}
        aria-label=${`Edit ${process.name}`} title=${editing ? 'Save or cancel the open editor first' : `Edit ${process.name} here as a graph`} onClick=${() => editGraph(process)}>✎</button>`}
        <button type="button" class="gq-saved-run" data-saved-run=${process.templateId} aria-expanded=${open === process.templateId}
          aria-label=${`Start ${process.name}`} title=${`Start ${process.name} in ${group.name}`}
          onClick=${() => { setOpen(open === process.templateId && !removing ? '' : process.templateId); setRemoving(false); }}>▶</button>
        <button type="button" class="gq-saved-remove" data-saved-remove-chip=${process.templateId}
          aria-label=${`Remove ${process.name} from ${group.name}`} title=${`Remove ${process.name} from ${group.name}'s processes`}
          onClick=${() => { setRemoving(true); setOpen(process.templateId); }}>×</button>
      </span>`)}
    </div>
    ${current && html`<${StartStrip} key=${current.templateId} removing=${removing} onRemoving=${setRemoving} editing=${editing}
      scribeDraft=${scribeDraftsOf(scribes, [current])[0] || null} scribes=${scribes} group=${group} process=${current} api=${api} source=${source}
      wizard=${wizard} notify=${notify} onClose=${() => setOpen('')}
      onEdit=${(edit) => { setOpen(''); onEdit({ ...edit, origin: '' }); }} />`}
  </div>`;
}
