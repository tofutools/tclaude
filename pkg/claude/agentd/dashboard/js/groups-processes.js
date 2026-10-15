// groups-processes.js -- the Groups tab's Processes perspective (mocks G3,
// P2/P3): the very same groups tree as the Agents perspective (groups-list.js
// renders it), with each group's member details swapped for its runs as graph
// cards, agents shown as avatars on the nodes they perform and a busy/idle
// avatar strip. GroupsProcessesFocus frames the tree (filter toolbar, load
// states, the single-run focus) and hands the tree what it needs through
// ProcessesPerspective; the tree asks for it with useProcessesPerspective and
// renders GroupProcessBody / UngroupedProcessBody / GroupRunCounts. Data comes
// from the process context read model (process-context.js) and renders
// through the shared process UI kit (process-kit.js).

import { createContext, h } from 'preact';
import { useContext, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ProcessMiniGraph, ProcessRunBadge, ProcessScopeChip, ProcessStateChip, runName, runState, stateLabel,
} from './process-kit.js';
import {
  RUN_FILTERS, busyPerformers, formatRunDuration, groupContextEntry, groupRunsFor,
  normalizeRunFilter, processContext, processRunApi, runNeedsYou, unauthorizedProfiles, withPerformerNames,
} from './process-context.js';
import { GroupsProcessesPlaceholder } from './groups-focus.js';
import { GroupsRunFocus } from './groups-run-focus.js';
import { agentRunIndex, groupProcessCounts } from './groups-process-indicators.js';
import { GetStarted, StepListEditor, defaultNotify, markQuickStart, openInGraphEditor, quickStartSeen } from './groups-quickstart.js';
import { groupRoster } from './process-quickstart.js';
import { handOffProcessDraft } from './process-draft-handoff.js';

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

// StartRunForm starts a group-scoped run: pick a template, fill its params,
// and explicitly authorize the program profiles it runs under (never
// pre-ticked: authorizing host program execution is the human's call), then
// POST it with the group pre-filled. Refusals show inline; a refusal naming
// unauthorized profiles (a referenced template's) adds them to the list. The
// created run appears on the next context refresh.
function StartRunForm({ group, api, source, onClose, wizard, initialTemplateId = '' }) {
  const [templates, setTemplates] = useState(null);
  const [templateId, setTemplateId] = useState('');
  const [needs, setNeeds] = useState({ params: [], programProfiles: [] });
  const [values, setValues] = useState({});
  const [authorized, setAuthorized] = useState(() => new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    api.listTemplates().then((rows) => {
      if (!live) return;
      setTemplates(rows);
      setTemplateId(rows.some((row) => row.id === initialTemplateId) ? initialTemplateId : rows[0]?.id || '');
    }).catch((cause) => { if (live) { setTemplates([]); setError(cause.message); } });
    return () => { live = false; };
  }, [api]);
  useEffect(() => {
    let live = true;
    setNeeds({ params: [], programProfiles: [] });
    setValues({});
    setAuthorized(new Set());
    setError('');
    if (!templateId || typeof api.templateRequirements !== 'function') return () => { live = false; };
    api.templateRequirements(templateId).then((found) => {
      if (!live) return;
      const params = found.params || [];
      // The server binds only what is sent (root params get no default
      // fill), so a default starts out as the field's value.
      setValues((current) => ({
        ...Object.fromEntries(params.filter((param) => param.hasDefault).map((param) => [param.name, param.default])),
        ...current,
      }));
      // Merge, so profiles a refusal already named survive a slow fetch.
      setNeeds((current) => ({
        params,
        programProfiles: [...new Set([...current.programProfiles, ...(found.programProfiles || [])])].sort(),
      }));
    }).catch(() => { /* the start refusal will say what is missing */ });
    return () => { live = false; };
  }, [api, templateId]);
  const missing = needs.params.filter((param) => param.required && !String(values[param.name] ?? '').trim());
  const start = async (event) => {
    event.preventDefault();
    if (!templateId || busy || missing.length) return;
    setBusy(true);
    setError('');
    try {
      await api.startGroupRun({ templateId, group: group.name, params: values, authorizeProgramProfiles: [...authorized] });
      markQuickStart();
      onClose();
      void source.refresh();
    } catch (cause) {
      const named = unauthorizedProfiles(cause.message);
      if (named.length) {
        setNeeds((current) => ({ ...current, programProfiles: [...new Set([...current.programProfiles, ...named])].sort() }));
      }
      setError(cause.message);
    } finally {
      setBusy(false);
    }
  };
  const toggleProfile = (profile, on) => setAuthorized((current) => {
    const next = new Set(current);
    if (on) next.add(profile);
    else next.delete(profile);
    return next;
  });
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
    <button type="submit" class="primary" disabled=${busy || !templateId || missing.length > 0}
      title=${missing.length ? `Fill ${missing.map((param) => param.name).join(', ')} first` : ''}>${busy ? 'starting…' : 'start'}</button>
    <button type="button" onClick=${onClose}>cancel</button>
    ${needs.params.length > 0 && html`<div class="gp-start-params" role="group" aria-label="Parameters">
      ${needs.params.map((param) => html`<label key=${param.name} class="gp-start-param" title=${param.description}>
        <span>${param.name}${param.required ? html`<b aria-label="required"> *</b>` : ''}</span>
        <input type=${param.type === 'number' ? 'number' : 'text'} data-start-param=${param.name} disabled=${busy}
          placeholder=${param.default || param.type} value=${values[param.name] ?? ''}
          onInput=${(event) => { const value = event.currentTarget.value; setValues((current) => ({ ...current, [param.name]: value })); }} />
      </label>`)}
    </div>`}
    ${needs.programProfiles.length > 0 && html`<div class="gp-start-profiles" role="group" aria-label="Program authorization">
      <span class="gp-start-note">${wizard ? 'This rite runs host programs as:' : 'This process runs host programs under:'}</span>
      ${needs.programProfiles.map((profile) => html`<label key=${profile || '(default)'} class="gp-start-profile">
        <input type="checkbox" data-start-profile=${profile} disabled=${busy} checked=${authorized.has(profile)}
          onChange=${(event) => toggleProfile(profile, event.currentTarget.checked)} />
        authorize <code>${profile || '(default profile)'}</code>
      </label>`)}
    </div>`}
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

function RunCard({ run, entry, nameOf, wizard, onOpen }) {
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
      <button type="button" class="gp-open" data-process-open=${named.runId} onClick=${() => onOpen(named.runId)}
        title=${wizard ? `Gaze into ${runName(named)}` : `Open ${runName(named)}: live graph, node inspector and performers`}>${wizard ? 'gaze ›' : 'open ›'}</button>
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

// memberRunEntries lists the agent-scoped runs of the given members, one
// entry per run (the first member it lists wins), skipping runs already shown
// as cards. The context indexes only live agent-scoped runs, so only the
// filters that show live runs list them.
function memberRunEntries(context, members, filter, shown = new Set()) {
  if (filter !== 'running' && filter !== 'all') return [];
  const index = agentRunIndex(context);
  const seen = new Set(shown);
  const out = [];
  for (const member of members) {
    for (const entry of index.get(member?.agent_id) || []) {
      if (seen.has(entry.runId)) continue;
      seen.add(entry.runId);
      out.push({ ...entry, member });
    }
  }
  return out;
}

// MemberRuns is the slim line of agent-scoped runs under a group's cards
// (and the whole body of Ungrouped): the performer's avatar and a run badge
// that opens the single-run focus.
function MemberRuns({ entries, label, onOpen }) {
  if (!entries.length) return null;
  return html`<div class="gp-member-runs" data-process-member-runs>
    <span class="gp-member-runs-label">${label}</span>
    ${entries.map((entry) => html`<span key=${entry.runId} class="gp-member-run" data-process-member-run=${entry.runId}>
      <span class="pk-avatar" title=${memberName(entry.member)}>${memberInitial(entry.member)}</span>
      <${ProcessRunBadge} run=${entry.run} nodeId=${entry.nodeId} onOpen=${() => onOpen(entry.runId)} />
    </span>`)}
  </div>`;
}

function GroupRuns({ group, roster, context, filter, wizard, api, source, onFocusRun, notify }) {
  const [starting, setStarting] = useState(false);
  // quick is the quick start: null, 'pick' (the Get started panel opened on a
  // group that has processes) or {list, origin} (the step-list editor).
  const [quick, setQuick] = useState(null);
  // dismissed only re-renders this group at once; the pref folds the rest.
  const [, setDismissed] = useState(false);
  const entry = groupContextEntry(context, group);
  const { runs, planned } = groupRunsFor(context, entry, filter);
  // The roster is the group's full membership: the Groups search narrows
  // which groups show, never who is busy or how performers are named.
  const members = ((roster || group).members || []).filter((member) => memberKey(member));
  const names = new Map(members.map((member) => [memberKey(member), memberName(member)]));
  const nameOf = (ref) => names.get(ref) || '';
  const busy = busyPerformers(groupRunsFor(context, entry, 'running').runs);
  const memberRuns = memberRunEntries(context, members, filter, new Set(runs.map((run) => run.runId)));
  const label = FILTER_LABELS[filter][wizard ? 1 : 0].toLowerCase();
  const open = (runId) => onFocusRun({ group: group.name, runId });
  // A group with no runs at all, in any state, gets the Get started panel.
  const everything = groupRunsFor(context, entry, 'all');
  const fresh = !everything.runs.length && !everything.planned.length && !memberRunEntries(context, members, 'all').length;
  // Past the first run (a process exists anywhere, or the operator used or
  // dismissed the panel) an empty group folds Get started to one line.
  const folded = fresh && (quickStartSeen() || Object.keys(context?.runs || {}).length > 0);
  const editing = quick && typeof quick === 'object';
  const picking = !editing && !starting && (quick === 'pick' || (fresh && !folded));
  const people = groupRoster({ members }, busy);
  const quickStart = editing
    ? html`<${StepListEditor} key=${quick.key} group=${group} roster=${people} list=${quick.list} origin=${quick.origin}
        api=${api} source=${source} wizard=${wizard} notify=${notify} onClose=${() => setQuick(null)} />`
    : picking ? html`<${GetStarted} group=${group} roster=${people} wizard=${wizard} api=${api}
        onEditList=${(list, origin) => setQuick({ list, origin, key: Date.now() })}
        onOpenGraph=${(view, name) => (view
          ? openInGraphEditor(api, view, name).catch((cause) => (notify || defaultNotify)(`Could not open the graph editor: ${cause.message}`, true))
          : handOffProcessDraft({ create: true }))}
        onReuse=${(templateId) => { setQuick(null); setStarting({ templateId }); }}
        onDismiss=${() => { markQuickStart('dismissed'); setQuick(null); setDismissed(true); }} />`
      : fresh && folded && !starting && html`<div class="gp-empty gq-folded" data-quickstart-folded=${group.name}>
        ${wizard ? 'No rites yet' : 'No processes yet'} · <button type="button" class="gq-link" data-quickstart-expand onClick=${() => setQuick('pick')}>Get started ▸</button></div>`;
  return html`<div class="gp-group" data-process-group=${group.name}>
    <div class="gp-group-head">
      <${GroupAvatarStrip} members=${members} busy=${busy} wizard=${wizard} />
      <span class="gp-spacer"></span>
      ${!fresh && !quick && !starting && html`<button type="button" class="gp-start" data-process-new=${group.name} onClick=${() => setQuick('pick')}
        title=${wizard ? `Shape a new rite for ${group.name}` : `Build a new process for ${group.name} from steps or a starter`}>${wizard ? '+ new rite' : '+ new process'}</button>`}
      ${!starting && html`<button type="button" class="gp-start" data-process-start=${group.name} onClick=${() => { setQuick(null); setStarting(true); }}
        title=${wizard ? `Begin a rite bound to ${group.name}` : `Start a process run scoped to ${group.name}`}>${wizard ? '+ begin a rite' : '+ start process'}</button>`}
      ${quick === 'pick' && html`<button type="button" class="gp-start" onClick=${() => setQuick(null)}>close</button>`}
    </div>
    ${starting && html`<${StartRunForm} group=${group} api=${api} source=${source} wizard=${wizard}
      initialTemplateId=${starting?.templateId || ''} onClose=${() => setStarting(false)} />`}
    ${quickStart}
    ${runs.map((run) => html`<${RunCard} key=${run.runId} run=${run} entry=${entry} nameOf=${nameOf} wizard=${wizard} onOpen=${open} />`)}
    <${MemberRuns} entries=${memberRuns} label=${wizard ? 'Familiar rites' : 'Member runs'} onOpen=${open} />
    ${planned.length > 0 && html`<div class="gp-planned-list">${planned.map((item) => html`<${PlannedRow} key=${item.id} item=${item} wizard=${wizard} />`)}</div>`}
    ${!runs.length && !planned.length && !memberRuns.length && !fresh && html`<div class="gp-empty">${filter === 'all'
      ? (wizard ? 'No rites in this party yet.' : 'No processes in this group yet.')
      : `No ${label} ${wizard ? 'rites' : 'processes'}.`}</div>`}
  </div>`;
}

const ProcessesPerspective = createContext(null);

// useProcessesPerspective is what the groups tree reads to tell the two
// perspectives apart: null in Agents, the run context and handlers in
// Processes.
export function useProcessesPerspective() {
  return useContext(ProcessesPerspective);
}

// GroupProcessBody replaces a real group's member details in the Processes
// perspective: its own scope's runs only (subgroups render theirs nested).
export function GroupProcessBody({ group }) {
  const perspective = useProcessesPerspective();
  if (!perspective) return null;
  return html`<${GroupRuns} group=${group} roster=${perspective.rosters.get(group.name)} context=${perspective.context}
    filter=${perspective.filter} wizard=${perspective.wizard} api=${perspective.api} source=${perspective.source}
    onFocusRun=${perspective.onFocusRun} notify=${perspective.notify} />`;
}

// ungroupedRuns are the agent-scoped runs of Ungrouped's agents, the only
// runs the virtual group has.
function ungroupedRuns(perspective, group) {
  return memberRunEntries(perspective.context, group.members || [], perspective.filter);
}

export function UngroupedProcessBody({ group }) {
  const perspective = useProcessesPerspective();
  if (!perspective) return null;
  const { wizard, filter } = perspective;
  const entries = ungroupedRuns(perspective, group);
  const label = FILTER_LABELS[filter][wizard ? 1 : 0].toLowerCase();
  return html`<div class="gp-group" data-process-group=${group.key || group.name}>
    <${MemberRuns} entries=${entries} label=${wizard ? 'Familiar rites' : 'Agent runs'}
      onOpen=${(runId) => perspective.onFocusRun({ group: group.key || group.name, runId })} />
    ${!entries.length && html`<div class="gp-empty">${filter === 'running' || filter === 'all'
      ? (wizard ? 'No unbound familiar walks a rite.' : 'No ungrouped agent runs a process.')
      : `No ${label} ${wizard ? 'rites' : 'processes'} outside groups.`}</div>`}
  </div>`;
}

function countText(counts, wizard) {
  const parts = [];
  if (counts.running) parts.push(`${counts.running} ${wizard ? 'unfolding' : 'running'}`);
  if (counts.needsYou) parts.push(`${counts.needsYou} ${wizard ? 'awaits you' : 'needs you'}`);
  if (counts.planned) parts.push(`${counts.planned} ${wizard ? 'foretold' : 'planned'}`);
  return parts.join(' · ');
}

// SubgroupRunTotal is the dim "↳ in subgroups" line on a summary row: the
// runs of every nested group, so collapsing a parent never hides work. It
// is not a roll-up: the group's own counts stay its own, and the line hides
// while the group is open (its subgroups then show their own rows).
export function SubgroupRunTotal({ node }) {
  const perspective = useProcessesPerspective();
  if (!perspective || !node.children?.length) return null;
  const total = { running: 0, needsYou: 0, planned: 0 };
  const add = (child) => {
    const counts = groupProcessCounts(groupContextEntry(perspective.context, child.group));
    total.running += counts.running;
    total.needsYou += counts.needsYou;
    total.planned += counts.planned;
    for (const grandchild of child.children || []) add(grandchild);
  };
  for (const child of node.children) add(child);
  const text = countText(total, perspective.wizard);
  if (!text) return null;
  return html`<span class="gp-subtree-total" title="Runs in this group's subgroups, shown while it is collapsed">↳ ${perspective.wizard ? 'in sub-parties' : 'in subgroups'}: ${text}</span>`;
}

// UngroupedRunCount is Ungrouped's summary-row count in the Processes
// perspective.
export function UngroupedRunCount({ group }) {
  const perspective = useProcessesPerspective();
  if (!perspective) return null;
  const entries = memberRunEntries(perspective.context, group.members || [], 'running');
  const needsYou = entries.filter((entry) => entry.run.needsYou === true).length;
  const counts = { running: entries.length - needsYou, needsYou, planned: 0 };
  const { wizard } = perspective;
  if (!entries.length) return html`<span class="gpi-none">${wizard ? 'no rites' : 'no processes'}</span>`;
  return html`<span class="gpi-chips">
    ${counts.running > 0 && html`<span class="pk-chip pk-state-running gpi-chip"><span aria-hidden="true">●</span> ${counts.running} ${wizard ? 'unfolding' : 'running'}</span>`}
    ${counts.needsYou > 0 && html`<span class="pk-chip pk-state-waiting gpi-chip"><span aria-hidden="true">●</span> ${counts.needsYou} ${wizard ? 'awaits you' : 'needs you'}</span>`}
  </span>`;
}

// findFocusGroup resolves the open run's group: a real group from the full
// roster (so a Groups search cannot strand it), else a shown group by key,
// which is how Ungrouped's agent runs open.
function findFocusGroup(focusedRun, rosters, groups) {
  if (!focusedRun) return null;
  return rosters.get(focusedRun.group)
    || (groups || []).find((group) => (group.virtual ? group.key || group.name : group.name) === focusedRun.group)
    || null;
}

// GroupsProcessesFocus frames the Processes perspective: the run filter, the
// load states, and either the single-run focus or `children` -- the shared
// groups tree -- under the ProcessesPerspective it reads.
export function GroupsProcessesFocus({
  groups, allGroups = null, filter = 'running', onFilter = () => {}, wizard = false, source = processContext, api = processRunApi,
  focusedRun = null, onFocusRun = () => {}, runApi = undefined, children = null, notify = undefined,
}) {
  const rosters = new Map((allGroups || []).map((group) => [group.name, group]));
  const current = normalizeRunFilter(filter);
  useEffect(() => {
    void source.refresh();
    const poll = () => { void source.refresh(); };
    document.addEventListener('tclaude:snapshot', poll);
    return () => document.removeEventListener('tclaude:snapshot', poll);
  }, [source]);
  const status = source.status.value;
  const context = source.context.value;
  const focusGroup = findFocusGroup(focusedRun, rosters, groups);
  // A run whose group is gone closes rather than hiding and popping back.
  const strandedRun = status === 'ready' && focusedRun && !focusGroup;
  useEffect(() => { if (strandedRun) onFocusRun(null); }, [strandedRun]);
  // Closing the focus hands keyboard focus back to the card that opened it.
  const lastRun = useRef(null);
  useEffect(() => {
    const previous = lastRun.current;
    lastRun.current = focusedRun?.runId || null;
    if (previous && !focusedRun) {
      const opener = [...document.querySelectorAll('[data-process-open], [data-process-member-run] [data-process-run]')]
        .find((el) => (el.dataset.processOpen || el.dataset.processRun) === previous);
      opener?.focus?.();
    }
  }, [focusedRun?.runId]);
  if (status === 'ready' && focusGroup) {
    return html`<div class="gp-focus" data-groups-processes>
      ${source.error.value && html`<span class="gp-stale" role="status">Showing the last update: ${source.error.value}</span>`}
      <${GroupsRunFocus} group=${focusGroup} roster=${rosters.get(focusGroup.name)} context=${context} runId=${focusedRun.runId}
        wizard=${wizard} source=${source} api=${runApi} onClose=${() => onFocusRun(null)}
        onSelectRun=${(runId) => onFocusRun({ group: focusedRun.group, runId })} />
    </div>`;
  }
  const perspective = { context, filter: current, wizard, api, source, onFocusRun, rosters, notify };
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
    ${status === 'ready' && html`<${ProcessesPerspective.Provider} value=${perspective}><div class="gp-tree">${children}</div><//>`}
    ${status === 'ready' && context?.truncated && html`<div class="gp-card-note">Some runs are not shown: the run context was truncated.</div>`}
  </div>`;
}
