// groups-saved-processes.js -- a group's saved processes in the Groups ›
// Processes perspective (mock Q3, tcl-7p6bgt): chips at the top of the
// group's body, one per process saved to the group (context
// groups[].processes, in pin order). ▶ opens a start strip that asks only for
// the process's start inputs and command consent, then starts it in the
// group. The strip also edits the steps, publishes the process to the
// template library, or removes it from the group.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { unauthorizedProfiles } from './process-context.js';
import { viewToStepList } from './process-step-list.js';
import { handOffProcessDraft } from './process-draft-handoff.js';
import { defaultNotify, markQuickStart } from './groups-quickstart.js';

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
function StartStrip({ group, process, api, source, wizard, notify, onEdit, onClose }) {
  const [values, setValues] = useState(() => initialValues(process));
  const [authorized, setAuthorized] = useState(() => new Set());
  const [extraProfiles, setExtraProfiles] = useState([]);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [confirmRemove, setConfirmRemove] = useState(false);
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
  const start = (event) => {
    event.preventDefault();
    if (blockedWhy) return;
    void act('start', async () => {
      await api.startGroupRun({
        templateId: process.templateId, group: group.name, params: values,
        authorizeProgramProfiles: profiles.filter((profile) => authorized.has(profile)),
      });
      markQuickStart();
      notify(`Started ${process.name} in ${group.name}`);
      void source?.refresh?.();
      onClose();
    });
  };
  // A process the step list cannot show opens in the full graph editor.
  const edit = () => act('edit', async () => {
    const view = await api.getTemplate(process.templateId);
    const converted = viewToStepList(view);
    if (!converted.ok) {
      notify(`${process.name} opens in the graph editor: ${converted.reason}`);
      handOffProcessDraft({ id: process.templateId, name: process.name });
      return;
    }
    onEdit({
      list: converted.list,
      saved: { templateId: process.templateId, sourceHash: String(view.sourceHash || ''), listed: view.listed ?? process.listed },
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
    <button type="submit" class="gq-btn primary" data-saved-start disabled=${!!busy || !!blockedWhy} title=${blockedWhy}>${busy === 'start' ? 'starting…' : 'Start'}</button>
    <button type="button" class="gq-btn" data-saved-edit disabled=${!!busy} onClick=${edit}
      title="Edit the steps; saving writes a new version">${busy === 'edit' ? 'opening…' : '✎ Edit steps'}</button>
    ${!process.listed && html`<button type="button" class="gq-link" data-saved-publish disabled=${!!busy} onClick=${publish}
      title="List it in the template library, for every group to reuse">publish as template</button>`}
    ${confirmRemove
    ? html`<span class="gq-confirm" role="group" aria-label="Confirm removal">
        ${process.listed ? `Remove from ${group.name}? It stays in the template library.` : `Remove from ${group.name}? It is not in the template library, so publish it first to keep it.`}
        <button type="button" class="gq-btn" data-saved-remove-confirm disabled=${!!busy} onClick=${remove}>remove</button>
        <button type="button" class="gq-link" onClick=${() => setConfirmRemove(false)}>keep</button></span>`
    : html`<button type="button" class="gq-link" data-saved-remove disabled=${!!busy} onClick=${() => setConfirmRemove(true)}
      title=${`Remove ${process.name} from ${group.name}'s processes`}>remove</button>`}
    <span class="gq-spacer"></span>
    <button type="button" class="gq-link" aria-label="Close" onClick=${onClose}>✕</button>
    ${blockedWhy && html`<div class="gq-why" data-start-blocked>▶ ${blockedWhy}</div>`}
    ${error && html`<div class="gq-error" role="alert">${error}</div>`}
  </form>`;
}

// SavedProcesses is the group's saved-process chips and the open strip.
export function SavedProcesses({ group, processes, api, source, wizard = false, notify = defaultNotify, onNew, onEdit }) {
  const [open, setOpen] = useState('');
  const current = processes.find((process) => process.templateId === open) || null;
  return html`<div class="gq-saved" data-saved-processes=${group.name}>
    <div class="gq-saved-row">
      <span class="gq-saved-label">${wizard ? 'Rites' : 'Processes'}</span>
      ${processes.map((process) => html`<span key=${process.templateId} class=${`gq-saved-chip${open === process.templateId ? ' open' : ''}`}
        data-saved-process=${process.templateId}>
        <span class="gq-saved-name" title=${process.listed ? 'Also in the template library' : `Saved to ${group.name} only`}>${process.name}</span>
        <span class="gq-dim">${plural(process.steps || 0, 'step')}</span>
        <button type="button" class="gq-saved-run" data-saved-run=${process.templateId} aria-expanded=${open === process.templateId}
          aria-label=${`Start ${process.name}`} title=${`Start ${process.name} in ${group.name}`}
          onClick=${() => setOpen(open === process.templateId ? '' : process.templateId)}>▶</button>
      </span>`)}
      <button type="button" class="gq-link" data-saved-new=${group.name} onClick=${onNew}
        title=${wizard ? `Shape a new rite for ${group.name}` : `Build a new process for ${group.name}`}>＋ new</button>
    </div>
    ${current && html`<${StartStrip} key=${current.templateId} group=${group} process=${current} api=${api} source=${source}
      wizard=${wizard} notify=${notify} onClose=${() => setOpen('')}
      onEdit=${(edit) => { setOpen(''); onEdit({ ...edit, origin: '' }); }} />`}
  </div>`;
}
