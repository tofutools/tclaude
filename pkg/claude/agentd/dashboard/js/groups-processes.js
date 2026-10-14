// groups-processes.js -- the Groups tab's Processes focus (mock G3): the same
// group tree, but each group renders its runs as graph cards, with agents as
// avatars on the nodes they perform and a busy/idle avatar strip on the group
// header. Data comes from the process context read model (process-context.js)
// and renders through the shared process UI kit (process-kit.js).

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ProcessMiniGraph, ProcessScopeChip, ProcessStateChip, runName, runState, stateLabel,
} from './process-kit.js';
import {
  RUN_FILTERS, busyPerformers, formatRunDuration, groupContextEntry, groupRunsFor,
  normalizeRunFilter, processContext, processRunApi, runNeedsYou, withPerformerNames,
} from './process-context.js';
import { GroupsProcessesPlaceholder } from './groups-focus.js';

const html = htm.bind(h);

const FILTER_LABELS = Object.freeze({
  running: ['Running', 'Unfolding'],
  planned: ['Planned', 'Foretold'],
  done: ['Done', 'Fulfilled'],
  all: ['All', 'All'],
});

function memberName(member) {
  return member?.title || member?.agent_id || member?.conv_id || '';
}

function memberKey(member) {
  return member?.agent_id || member?.conv_id || '';
}

function memberInitial(member) {
  return [...String(memberName(member) || '?')][0].toUpperCase();
}

// StartRunForm starts a group-scoped run: pick a template, POST it with the
// group pre-filled. Errors (a template needing params, a missing grant) show
// inline; the created run appears on the next context refresh.
function StartRunForm({ group, api, source, onClose, wizard }) {
  const [templates, setTemplates] = useState(null);
  const [templateId, setTemplateId] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    api.listTemplates().then((rows) => {
      if (!live) return;
      setTemplates(rows);
      setTemplateId(rows[0]?.id || '');
    }).catch((cause) => { if (live) { setTemplates([]); setError(cause.message); } });
    return () => { live = false; };
  }, [api]);
  const start = async (event) => {
    event.preventDefault();
    if (!templateId || busy) return;
    setBusy(true);
    setError('');
    try {
      await api.startGroupRun({ templateId, group: group.name });
      onClose();
      void source.refresh();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setBusy(false);
    }
  };
  const label = wizard ? `Begin a rite for ${group.name}` : `Start a process in ${group.name}`;
  return html`<form class="gp-start-form" aria-label=${label} onSubmit=${start}
    onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose(); } }}>
    <span class="pk-chip pk-scope pk-scope-group">⌂ ${wizard ? 'circle-bound' : 'group scope'} · ${group.name}</span>
    ${templates === null
    ? html`<span class="gp-start-note">Loading templates…</span>`
    : templates.length === 0 && !error
      ? html`<span class="gp-start-note">No process templates yet: create one in Automations › Processes.</span>`
      : html`<select aria-label="Template" value=${templateId} disabled=${busy} onChange=${(event) => setTemplateId(event.currentTarget.value)}>
        ${templates.map((template) => html`<option key=${template.id} value=${template.id}>${template.name}</option>`)}
      </select>`}
    <button type="submit" class="primary" disabled=${busy || !templateId}>${busy ? 'starting…' : 'start'}</button>
    <button type="button" onClick=${onClose}>cancel</button>
    ${error && html`<span class="gp-start-error" role="alert">${error}</span>`}
  </form>`;
}

function GroupAvatarStrip({ members, busy, wizard }) {
  if (!members.length) return null;
  const busyCount = members.filter((member) => busy.has(memberKey(member))).length;
  return html`<span class="gp-avatar-strip">
    <span class="pk-avatars">${members.map((member) => {
      const isBusy = busy.has(memberKey(member));
      return html`<span key=${memberKey(member)} class=${`pk-avatar gp-member ${isBusy ? 'busy' : 'idle'}`}
        title=${`${memberName(member)} · ${isBusy ? 'performing a process step' : 'free'}`}>${memberInitial(member)}</span>`;
    })}</span>
    <span class="gp-strip-text">${busyCount} ${wizard ? 'bound to rites' : 'busy in processes'} · ${members.length - busyCount} free</span>
  </span>`;
}

function RunCard({ run, entry, nameOf, wizard }) {
  const named = withPerformerNames(run, nameOf);
  const state = runState(named);
  const terminal = state === 'done' || state === 'failed';
  const duration = formatRunDuration(named.startedAt, terminal && named.updatedAt ? Date.parse(named.updatedAt) : Date.now());
  const template = named.templateRef || named.templateId || '';
  const owner = named.scope?.owner ? nameOf(named.scope.owner) || named.scope.owner : '';
  return html`<article class="gp-card" data-process-run-card=${named.runId}>
    <div class="gp-card-head">
      <span class="gp-card-name">${runName(named)}</span>
      <${ProcessStateChip} state=${state} label=${`${stateLabel(state)}${duration ? ` · ${duration}` : ''}`} />
      <${ProcessScopeChip} scope=${named.scope?.kind === 'agent' && nameOf(named.scope.agentId) ? { kind: 'agent', name: nameOf(named.scope.agentId) } : named.scope} wizard=${wizard} />
      ${(template || owner) && html`<span class="gp-card-meta">${template && html`${wizard ? 'grimoire' : 'template'} <code>${template}</code>`}${template && owner && ' · '}${owner && html`owner <b>${owner}</b>`}</span>`}
      <span class="gp-spacer"></span>
      ${runNeedsYou(named, entry) && html`<span class="pk-chip pk-state-waiting gp-needs-you">👤 ${wizard ? 'awaits your word' : 'waiting on you'}</span>`}
    </div>
    <div class="gp-card-body"><${ProcessMiniGraph} run=${named} wizard=${wizard} /></div>
    ${named.truncated && html`<div class="gp-card-note">Graph truncated: open the run for every node.</div>`}
  </article>`;
}

function PlannedRow({ item, wizard }) {
  const when = item.nextAt ? new Date(item.nextAt) : null;
  const at = when && !Number.isNaN(when.getTime())
    ? when.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '';
  return html`<span class="pk-chip gp-planned" data-process-planned=${item.id}>
    ${item.source === 'trigger' ? '⚡' : '⏲'} ${item.name || item.templateRef || item.templateId || item.id}${at && ` · ${at}`}
    <span class="gp-planned-source">${wizard ? (item.source === 'trigger' ? 'omen' : 'drumbeat') : (item.source || 'scheduled')}</span>
  </span>`;
}

function GroupRuns({ group, context, filter, wizard, api, source }) {
  const [starting, setStarting] = useState(false);
  const entry = groupContextEntry(context, group);
  const { runs, planned } = groupRunsFor(context, entry, filter);
  const members = (group.members || []).filter((member) => memberKey(member));
  const names = new Map(members.map((member) => [memberKey(member), memberName(member)]));
  const nameOf = (ref) => names.get(ref) || '';
  const busy = busyPerformers(groupRunsFor(context, entry, 'running').runs);
  const label = FILTER_LABELS[filter][wizard ? 1 : 0].toLowerCase();
  return html`<div class="gp-group" data-process-group=${group.name}>
    <div class="gp-group-head">
      <span class="gp-group-name">${group.name}</span>
      <${GroupAvatarStrip} members=${members} busy=${busy} wizard=${wizard} />
      <span class="gp-spacer"></span>
      ${!starting && html`<button type="button" class="gp-start" data-process-start=${group.name} onClick=${() => setStarting(true)}
        title=${wizard ? `Begin a rite bound to ${group.name}` : `Start a process run scoped to ${group.name}`}>${wizard ? '+ begin a rite' : '+ start process'}</button>`}
    </div>
    ${starting && html`<${StartRunForm} group=${group} api=${api} source=${source} wizard=${wizard} onClose=${() => setStarting(false)} />`}
    ${runs.map((run) => html`<${RunCard} key=${run.runId} run=${run} entry=${entry} nameOf=${nameOf} wizard=${wizard} />`)}
    ${planned.length > 0 && html`<div class="gp-planned-list">${planned.map((item) => html`<${PlannedRow} key=${item.id} item=${item} wizard=${wizard} />`)}</div>`}
    ${!runs.length && !planned.length && html`<div class="gp-empty">${filter === 'all'
      ? (wizard ? 'No rites in this party yet.' : 'No processes in this group yet.')
      : `No ${label} ${wizard ? 'rites' : 'processes'}.`}</div>`}
  </div>`;
}

export function GroupsProcessesFocus({
  groups, filter = 'running', onFilter = () => {}, wizard = false, source = processContext, api = processRunApi,
}) {
  const current = normalizeRunFilter(filter);
  useEffect(() => {
    void source.refresh();
    const poll = () => { void source.refresh(); };
    document.addEventListener('tclaude:snapshot', poll);
    return () => document.removeEventListener('tclaude:snapshot', poll);
  }, [source]);
  const status = source.status.value;
  const context = source.context.value;
  const realGroups = (groups || []).filter((group) => !group.virtual);
  return html`<div class="gp-focus" data-groups-processes>
    <div class="gp-toolbar">
      <div class="gp-filter" role="group" aria-label=${wizard ? 'Rite filter' : 'Process filter'}>
        ${RUN_FILTERS.map((value) => html`<button key=${value} type="button" data-process-filter=${value}
          class=${current === value ? 'active' : ''} aria-pressed=${current === value ? 'true' : 'false'}
          onClick=${() => onFilter(value)}>${FILTER_LABELS[value][wizard ? 1 : 0]}</button>`)}
      </div>
      ${source.error.value && status === 'ready' && html`<span class="gp-stale" role="status">Showing the last update: ${source.error.value}</span>`}
    </div>
    ${status === 'idle' && html`<div class="gp-empty" role="status">Loading process runs…</div>`}
    ${status === 'error' && html`<div class="gp-empty gp-error" role="alert">Could not load process runs: ${source.error.value}</div>`}
    ${status === 'unavailable' && html`<${GroupsProcessesPlaceholder} wizard=${wizard} />`}
    ${status === 'ready' && realGroups.map((group) => html`<${GroupRuns} key=${group.name} group=${group} context=${context} filter=${current} wizard=${wizard} api=${api} source=${source} />`)}
    ${status === 'ready' && context?.truncated && html`<div class="gp-card-note">Some runs are not shown: the run context was truncated.</div>`}
  </div>`;
}
