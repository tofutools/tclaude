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
  DEFAULT_RUN_FILTER, RUN_KINDS, RUN_KIND_LABELS, busyPerformers, formatRunDuration, groupContextEntry, groupRunsFor,
  normalizeRunFilter, processContext, processRunApi, runDetailApi, runFilterHas, runFilterKinds, runNeedsYou,
  unauthorizedProfiles, withPerformerNames,
} from './process-context.js';
import { GroupsProcessesPlaceholder } from './groups-focus.js';
import { GroupsRunFocus } from './groups-run-focus.js';
import { WorkflowImportButton } from './process-workflow-share.js';
import {
  ProcessGraphRow, agentRunIndex, enabled, groupProcessCounts,
} from './groups-process-indicators.js';
import {
  GetStarted, StepListEditor, defaultNotify, markQuickStart, openInGraphEditor, quickStartSeen,
} from './groups-quickstart.js';
import { DraftStartWarning, ScribeDraft, draftWithScribe, leaveScribeDraft, noteDraftOpened, restorableScribeDraft, scribeStartWarning } from './groups-scribe-draft.js';
import { processScribeSessions } from './process-scribe.js';
import { viewToStepList } from './process-step-list.js';
import { shellConfirm } from './shell-state.js';
import { SavedProcesses } from './groups-saved-processes.js';
import { MiniGraphEditor } from './groups-mini-editor.js';
import { groupRoster } from './process-quickstart.js';
import { handOffProcessDraft } from './process-draft-handoff.js';
import { takeProcessStart } from './process-start-handoff.js';
import { fusedSectionOpen, setFusedSectionOpen } from './groups-fused-sections.js';
import { FusedLinks } from './groups-fused-links.js';
import { useProcessScope } from './groups-process-scope.js';

const html = htm.bind(h);

// filterLabel names the ticked kinds for the empty notes ("running or
// planned"); '' when every kind is ticked.
function filterLabel(filter, wizard) {
  const kinds = runFilterKinds(filter);
  if (kinds.length === RUN_KINDS.length) return '';
  return kinds.map((kind) => RUN_KIND_LABELS[kind][wizard ? 1 : 0].toLowerCase()).join(' or ');
}

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
// created run appears on the next context refresh. While a scribe is at work on
// the template it asks first, as the ▶ strip does.
function StartRunForm({ group, api, source, onClose, wizard, initialTemplateId = '', members = null, onBuild = null, scribes = [], onOpenTemplate = null }) {
  // members set means an agent-scoped run: the operator picks which member
  // the run is bound to, and its steps run as that agent's work.
  const agentScoped = Array.isArray(members);
  const [agent, setAgent] = useState(() => (agentScoped ? memberKey(members.find((member) => member.agent_id)) : ''));
  const [templates, setTemplates] = useState(null);
  const [templateId, setTemplateId] = useState('');
  const [needs, setNeeds] = useState({ params: [], programProfiles: [] });
  const [values, setValues] = useState({});
  const [authorized, setAuthorized] = useState(() => new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  // Set while Start asks about a scribe's unreviewed work; see scribeStartWarning.
  const [draftWarning, setDraftWarning] = useState(null);
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
    setDraftWarning(null);
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
  const act = async (work) => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      await work();
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
  const begin = async (expectedRef = '') => {
    const scope = agentScoped ? { agent } : { group: group.name };
    await api.startGroupRun({
      templateId, ...scope, params: values, authorizeProgramProfiles: [...authorized], ...(expectedRef ? { expectedRef } : {}),
    });
    markQuickStart();
    onClose();
    void source.refresh();
  };
  const start = (event) => {
    event.preventDefault();
    if (!templateId || busy || draftWarning || missing.length || (agentScoped && !agent)) return;
    void act(async () => {
      const warning = await scribeStartWarning(api, scribes, templateId);
      if (warning) setDraftWarning(warning);
      else await begin();
    });
  };
  // Pinned to the version the question was about: a save since then asks again
  // instead of running what nobody was asked about.
  const startAnyway = () => act(async () => {
    try {
      await begin(draftWarning.ref);
    } catch (cause) {
      if (cause.body?.code !== 'process_run_template_moved') throw cause;
      const warning = await scribeStartWarning(api, scribes, templateId);
      setDraftWarning(warning);
      if (!warning) setError('It changed since you were asked. Press start to run the current version.');
    }
  });
  const openDraft = () => act(async () => {
    const chosen = templates.find((template) => template.id === templateId);
    await onOpenTemplate({ templateId, name: chosen?.name || templateId });
  });
  const toggleProfile = (profile, on) => setAuthorized((current) => {
    const next = new Set(current);
    if (on) next.add(profile);
    else next.delete(profile);
    return next;
  });
  const label = agentScoped
    ? (wizard ? `Begin a rite for one hero of ${group.name}` : `Start a process for one agent in ${group.name}`)
    : (wizard ? `Begin a rite for ${group.name}` : `Start a process in ${group.name}`);
  const agents = agentScoped ? members.filter((member) => member.agent_id) : [];
  return html`<form class="gp-start-form" aria-label=${label} data-start-scope=${agentScoped ? 'agent' : 'group'} onSubmit=${start}
    onKeyDown=${(event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose(); } }}>
    ${agentScoped
    ? html`<span class="pk-chip pk-scope pk-scope-agent">◉ ${wizard ? 'hero-bound' : 'agent scope'}</span>
      ${agents.length
        ? html`<select aria-label="Agent" data-start-agent value=${agent} disabled=${busy} onChange=${(event) => setAgent(event.currentTarget.value)}>
          ${agents.map((member) => html`<option key=${member.agent_id} value=${member.agent_id}>${memberName(member)}</option>`)}
        </select>`
        : html`<span class="gp-start-note">${wizard ? 'No hero here can bear a rite.' : 'No agent in this group to run it.'}</span>`}`
    : html`<span class="pk-chip pk-scope pk-scope-group">⌂ ${wizard ? 'circle-bound' : 'group scope'} · ${group.name}</span>`}
    ${templates === null
    ? html`<span class="gp-start-note">Loading templates…</span>`
    : templates.length === 0 && !error
      ? html`<span class="gp-start-note">${wizard ? 'No rites written yet.' : 'No saved processes yet.'}</span>`
      : html`<select aria-label="Template" value=${templateId} disabled=${busy} onChange=${(event) => setTemplateId(event.currentTarget.value)}>
        ${templates.map((template) => html`<option key=${template.id} value=${template.id}>${template.name}</option>`)}
      </select>`}
    <button type="submit" class="primary" disabled=${busy || !!draftWarning || !templateId || missing.length > 0 || (agentScoped && !agent)}
      title=${missing.length ? `Fill ${missing.map((param) => param.name).join(', ')} first` : ''}>${busy ? 'starting…' : 'start'}</button>
    <button type="button" onClick=${onClose}>cancel</button>
    ${draftWarning && html`<${DraftStartWarning} warning=${draftWarning} busy=${busy} openDisabled=${!onOpenTemplate} onOpen=${openDraft} onStartAnyway=${startAnyway} />`}
    ${onBuild && html`<button type="button" class="gq-link" data-process-new=${group.name} onClick=${onBuild}
      title=${wizard ? `Shape a new rite for ${group.name}` : `Build a new process for ${group.name} from steps or a starter`}>${wizard ? 'or shape a new rite ▸' : 'or build a new one ▸'}</button>`}
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

// memberAvailability says what a member could do with a process step right
// now: performing one already ('process'), busy on other work ('working'),
// waiting on the human ('waiting'), free to start at once ('idle'), or
// powered off ('offline'). Process work comes first: it is what this
// perspective is about. Other states read the same agent status the Agents
// perspective's state pill shows.
export function memberAvailability(member, busy) {
  if (busy.has(memberKey(member))) return 'process';
  if (!member?.online) return 'offline';
  const state = member.state || {};
  if (state.recovery_status && state.recovery_status !== 'recovered') return 'working';
  const status = state.status || '';
  if (status === 'awaiting_input' || status === 'awaiting_permission') return 'waiting';
  return status === '' || status === 'idle' ? 'idle' : 'working';
}

const AVAILABILITY = [
  // [state, strip text, wizard strip text, avatar tooltip]
  ['process', 'in processes', 'bound to rites', 'performing a process step'],
  ['working', 'working', 'at other labours', 'working on other tasks (not in a process)'],
  ['waiting', 'waiting on you', 'awaiting your word', 'waiting on you: a question or permission prompt'],
  ['idle', 'idle', 'at rest', 'idle: can take a step now'],
  ['offline', 'offline', 'slumbering', 'offline'],
];

function GroupAvatarStrip({ members, busy, wizard }) {
  if (!members.length) return null;
  const states = members.map((member) => memberAvailability(member, busy));
  const count = (state) => states.filter((value) => value === state).length;
  // Idle is the number that matters (who can start a step now), so it always
  // shows; the other states only when some member is in them.
  const parts = AVAILABILITY.filter(([state]) => state === 'idle' || count(state) > 0)
    .map(([state, plain, rite]) => `${count(state)} ${wizard ? rite : plain}`);
  // One icon per state in use, not one per member: a big group's row of
  // overlapping ringed avatars was unreadable. The count sits bottom-right
  // like the UI's other counters; the tooltip names the members.
  return html`<span class="gp-avatar-strip">
    <span class="gp-avatar-states">${AVAILABILITY.filter(([state]) => count(state) > 0).map(([state, , , tip]) => {
      const names = members.filter((_, i) => states[i] === state).map(memberName);
      return html`<span key=${state} class=${`gp-member is-${state}${state === 'process' ? ' busy' : ''}`}
        data-availability=${state} title=${`${names.length} ${tip} — ${names.join(', ')}`}>
        <svg class="gp-member-glyph" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="5.2" r="3" /><path d="M2.4 14.6c0-3.4 2.5-5.6 5.6-5.6s5.6 2.2 5.6 5.6z" /></svg>
        ${names.length > 1 && html`<span class="gp-member-count">${names.length}</span>`}
      </span>`;
    })}</span>
    <span class="gp-strip-text">${parts.join(' · ')}</span>
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
  if (!runFilterHas(filter, 'running')) return [];
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

// The run-control icons are small process graphs in the place of the Agents
// row's person glyphs: a single lane (start, then one step) for a process
// run by one agent, and a lane that forks into parallel steps for a process
// the whole group runs.
function RunIcon({ group = false }) {
  return group
    ? html`<svg class="subgroup-ico" viewBox="0 0 28 24" width="17" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="4" cy="12" r="3"></circle><path d="M7 12h4M11 12l3-6h2M11 12l3 6h2"></path><rect x="17" y="3" width="9" height="6" rx="1.5"></rect><rect x="17" y="15" width="9" height="6" rx="1.5"></rect></svg>`
    : html`<svg class="spawn-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="4" cy="12" r="3"></circle><path d="M7 12h5"></path><path d="M10 9l3 3-3 3"></path><rect x="14" y="7" width="9" height="10" rx="1.5"></rect></svg>`;
}

// liveGroupRuns are the runs pause all / resume all act on: the group's own
// live runs and its members' agent-scoped runs, whatever the run filter
// shows, so the result never depends on what happens to be visible.
export function liveGroupRuns(context, entry, members) {
  const own = groupRunsFor(context, entry, 'running').runs;
  const seen = new Set(own.map((run) => run.runId));
  const agentRuns = memberRunEntries(context, members, 'running', seen).map((item) => item.run);
  return [...own, ...agentRuns].filter((run) => !['done', 'failed'].includes(runState(run)));
}

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 'es'}`;
}

// RunControls is the Processes perspective's version of the Agents row
// (spawn agent, create subgroup, power on, shutdown), in the same places:
// start a process for one agent, start one for the group, resume every
// paused run, pause every running one. Pause stands in for shutdown because
// it is reversible: work in flight finishes, nothing new is dispatched until
// resume. Ending runs (cancel) is deliberately not one click away. Pause and
// resume wear ⏸/⏵ rather than the agent row's 🛑/🟢, so in Both, where the
// two rows sit side by side, "pause every run" never reads as "shut down
// every agent".
// RunGlyph draws pause and resume as SVG: the ⏸/⏵ characters render at
// very different sizes across fonts.
function RunGlyph({ kind }) {
  return html`<svg class="gp-run-glyph" viewBox="0 0 12 12" aria-hidden="true" data-run-glyph=${kind}>${kind === 'pause'
    ? html`<rect x="2.5" y="2" width="2.6" height="8" rx=".6" /><rect x="6.9" y="2" width="2.6" height="8" rx=".6" />`
    : html`<path d="M3.2 1.8v8.4c0 .5.5.8.9.5l6.2-4.2c.4-.3.4-.8 0-1L4.1 1.3c-.4-.3-.9 0-.9.5z" />`}</svg>`;
}

function RunControls({ group, runs, agentIds, wizard, runApi, source, notify, onStart, inline = false }) {
  const [busy, setBusy] = useState('');
  const pausable = runs.filter((run) => !run.paused);
  const resumable = runs.filter((run) => run.paused);
  const bulk = async (kind, shown) => {
    if (busy || !shown.length) return;
    setBusy(kind);
    try {
      // The cards show a capped list; act on every live run the group and
      // its members have, read fresh.
      let targets = shown;
      if (typeof runApi.listLiveRuns === 'function') {
        const all = await runApi.listLiveRuns([{ group: group.name }, ...agentIds.map((agent) => ({ agent }))]);
        targets = all.filter((run) => (kind === 'pause' ? !run.paused : !!run.paused));
      }
      const results = await Promise.allSettled(targets.map((run) => Promise.resolve().then(() => runApi[kind](run.runId))));
      const failed = results.map((result, i) => [result, targets[i]]).filter(([result]) => result.status === 'rejected');
      if (failed.length) {
        const [result, run] = failed[0];
        (notify || defaultNotify)(`Could not ${kind} ${failed.length} of ${plural(targets.length, 'process')}: ${runName(run) || run.runId}: ${result.reason?.message || result.reason}`, true);
      }
    } catch (cause) {
      (notify || defaultNotify)(`Could not ${kind} the group's processes: ${cause?.message || cause}`, true);
    } finally {
      setBusy('');
      void source.refresh();
    }
  };
  const shared = { 'data-group': group.name };
  // Inline is Both's half-row beside the agent controls; otherwise the row.
  return html`<${inline ? 'span' : 'div'} class=${inline ? 'gp-run-controls gf-run-controls' : 'group-header-actions gp-run-controls'}>
    <button type="button" class="spawn-btn" ...${shared} data-process-start-agent=${group.name} onClick=${() => onStart('agent')}
      title=${wizard ? `Begin a rite for one hero of ${group.name}` : `Agent process: run a saved process, or build a new one, for one agent in ${group.name}`}
      aria-label="Start a process for one agent in this group"><${RunIcon} /><span class="spawn-btn-label-wizard" aria-hidden="true">📜</span></button>
    <button type="button" class="spawn-btn subgroup-btn" ...${shared} data-process-start=${group.name} onClick=${() => onStart('group')}
      title=${wizard ? `Begin a rite bound to ${group.name}` : `Group process: run a saved process, or build a new one, for the whole group`}
      aria-label="Start a process for this group"><${RunIcon} group=${true} /><span class="subgroup-icon-wizard" aria-hidden="true">⚔📜</span></button>
    <span class="group-actions">
      <button type="button" ...${shared} data-process-resume-all=${group.name} disabled=${!!busy || !resumable.length}
        onClick=${() => bulk('resume', resumable)} aria-label="Resume every paused process in this group"
        title=${resumable.length ? `Resume ${plural(resumable.length, 'paused process')}: what waited is planned again` : 'No paused processes in this group'}><${RunGlyph} kind="resume" /></button>
      <button type="button" class="warn" ...${shared} data-process-pause-all=${group.name} disabled=${!!busy || !pausable.length}
        onClick=${() => bulk('pause', pausable)} aria-label="Pause every running process in this group"
        title=${pausable.length ? `Pause ${plural(pausable.length, 'running process')}: steps in flight finish, nothing new starts until you resume. Nothing is cancelled.` : 'No running processes to pause in this group'}><${RunGlyph} kind="pause" /></button>
    </span>
  <//>`;
}

// GroupRuns is a group's runs. In Both (`fused`) it also lays out the rest of
// the group body around them, since its start and edit state drive both the
// process controls and the Processes section: the Agents and Processes
// sections, each with its own controls on its header line, then
// `fused.after` (the group's triggers).
function GroupRuns({ group, roster, context, filter, wizard, api, runApi = runDetailApi, source, onFocusRun, notify, fused = null, confirm = shellConfirm, scribes = [] }) {
  const [starting, setStarting] = useState(false);
  // quick is the quick start: null, 'pick' (the Get started panel opened on a
  // group that has processes), {list, origin, saved} (the step-list editor;
  // saved when it edits one of the group's saved processes) or {mode:
  // 'graph', view, process} (the inline mini editor of a saved process).
  // Both editors of a saved process carry `process` and `stored` (the view
  // it was opened at), so ☰ Steps / ◈ Graph switch over the same draft.
  const [quick, setQuick] = useState(null);
  // dismissed only re-renders this group at once; the pref folds the rest.
  const [, setDismissed] = useState(false);
  // "Start in <group>" from the dock's library opens this group's start form
  // on that template.
  // The importer sits here rather than in Get started: importing pins a process
  // to the group, which takes the panel away, and a stale review stays open.
  const importerRef = useRef(null);
  const [droppedPwf, setDroppedPwf] = useState(null);
  const [pwfOver, setPwfOver] = useState(false);
  const pickPwf = () => importerRef.current?.querySelector('[data-pwf-file]')?.click();
  const fileDrag = (event) => [...(event.dataTransfer?.types || [])].includes('Files');
  // Only a .pwf is taken: any other file falls through to the page's own drop
  // (a .paf), and one dropped on the importer's own dialog changes nothing.
  const pwfDrop = {
    onDragOver: (event) => { if (!fileDrag(event)) return; event.preventDefault(); setPwfOver(true); },
    onDragLeave: (event) => { if (!event.currentTarget.contains(event.relatedTarget)) setPwfOver(false); },
    onDrop: (event) => {
      setPwfOver(false);
      if (!fileDrag(event) || event.target.closest?.('.modal-overlay, [role="dialog"]')) return;
      const file = [...(event.dataTransfer.files || [])].find((f) => /\.pwf$/i.test(f.name));
      if (!file) return;
      event.preventDefault();
      setDroppedPwf(file);
    },
  };
  useEffect(() => {
    const take = () => {
      const request = takeProcessStart(group.name);
      if (!request) return;
      setQuick(null);
      setStarting({ templateId: request.templateId, scope: 'group' });
    };
    take();
    document.addEventListener('tclaude:process-start', take);
    return () => document.removeEventListener('tclaude:process-start', take);
  }, [group.name]);
  const entry = groupContextEntry(context, group);
  const { runs, planned } = groupRunsFor(context, entry, filter);
  // The roster is the group's full membership: the Groups search narrows
  // which groups show, never who is busy or how performers are named.
  const members = ((roster || group).members || []).filter((member) => memberKey(member));
  const names = new Map(members.map((member) => [memberKey(member), memberName(member)]));
  const nameOf = (ref) => names.get(ref) || '';
  const busy = busyPerformers(groupRunsFor(context, entry, 'running').runs);
  const memberRuns = memberRunEntries(context, members, filter, new Set(runs.map((run) => run.runId)));
  const label = filterLabel(filter, wizard);
  const open = (runId) => onFocusRun({ group: group.name, runId });
  const savedProcesses = entry?.processes || [];
  // A group with no runs at all, in any state, and no saved processes gets
  // the Get started panel.
  const everything = groupRunsFor(context, entry, 'all');
  const fresh = !everything.runs.length && !everything.planned.length && !savedProcesses.length
    && !memberRunEntries(context, members, 'all').length;
  // Past the first run (a process exists anywhere, or the operator used or
  // dismissed the panel) an empty group folds Get started to one line.
  // Both always folds it: a full panel in every quiet group would bury the
  // member tables.
  const folded = fresh && (!!fused || quickStartSeen() || Object.keys(context?.runs || {}).length > 0);
  const editing = quick && typeof quick === 'object';
  const picking = !editing && !starting && (quick === 'pick' || (fresh && !folded));
  const people = groupRoster({ members }, busy);
  // The step list may have saved a version (a refused start keeps it open):
  // the graph continues from that one, so its CAS base is the newest.
  const toGraph = ({ view, changed, sourceHash, ref }) => {
    const resaved = !!sourceHash && sourceHash !== quick.saved?.sourceHash;
    setQuick({
      mode: 'graph', key: Date.now(), process: quick.process, changed,
      stored: resaved && !changed ? view : quick.stored,
      view: { ...view, sourceHash: sourceHash || quick.saved?.sourceHash || '', currentRef: ref || quick.saved?.ref || '' },
    });
  };
  const toSteps = ({ list, changed, sourceHash, ref }) => setQuick({
    list, origin: '', key: Date.now(), process: quick.process, stored: quick.stored,
    saved: { templateId: quick.process.templateId, sourceHash: sourceHash || quick.view.sourceHash || '', ref: ref || quick.view.currentRef || '', listed: !!quick.process.listed, changed },
  });
  // A stored version opens as any saved process does: the step list, or the
  // inline graph when the list cannot show it.
  const openStored = (process, view) => {
    noteDraftOpened(scribes, process.templateId, view);
    const converted = viewToStepList(view);
    if (!converted.ok) {
      setQuick({ mode: 'graph', key: Date.now(), process, view, stored: view });
      return;
    }
    setQuick({
      list: converted.list, origin: '', key: Date.now(), process, stored: view,
      saved: { templateId: process.templateId, sourceHash: String(view.sourceHash || ''), ref: String(view.currentRef || ''), listed: !!process.listed },
    });
  };
  const reviewDraft = ({ draft }, view) => {
    leaveScribeDraft(draft.templateId);
    openStored(savedProcesses.find((saved) => saved.templateId === draft.templateId)
      || { templateId: draft.templateId, name: draft.name, listed: false }, view);
  };
  const closeDraft = ({ draft }) => {
    leaveScribeDraft(draft.templateId);
    setQuick(null);
  };
  // A scribe outlives its panel: after a reload or a tab change, its draft
  // comes back on its own unless the operator already left it. Only with
  // nothing else open, so it never replaces what they are doing.
  const restoring = !quick && !starting ? restorableScribeDraft(scribes, savedProcesses) : null;
  useEffect(() => {
    if (restoring) setQuick({ mode: 'scribe', key: Date.now(), draft: restoring });
  }, [restoring?.templateId]);
  const quickStart = editing && quick.mode === 'scribe'
    ? html`<${ScribeDraft} key=${quick.key} group=${group} draft=${quick.draft} api=${api} wizard=${wizard} notify=${notify || defaultNotify}
        onReview=${(view) => reviewDraft(quick, view)} onClose=${() => closeDraft(quick)} />`
    : editing && quick.mode === 'graph'
    ? html`<${MiniGraphEditor} key=${quick.key} group=${group} roster=${people} groupInfo=${entry} process=${quick.process}
        view=${quick.view} stored=${quick.stored || quick.view} changed=${!!quick.changed} api=${api} source=${source} wizard=${wizard}
        notify=${notify || defaultNotify} onClose=${() => setQuick(null)} onSteps=${toSteps} />`
    : editing
    ? html`<${StepListEditor} key=${quick.key} group=${group} roster=${people} list=${quick.list} origin=${quick.origin} saved=${quick.saved || null} groupInfo=${entry}
        api=${api} source=${source} wizard=${wizard} notify=${notify} onClose=${() => setQuick(null)}
        onGraph=${quick.saved && quick.process ? toGraph : null}
        onNewer=${quick.saved && quick.process ? (view) => openStored(quick.process, view) : null} />`
    : picking ? html`<${GetStarted} group=${group} roster=${people} wizard=${wizard} api=${api}
        onEditList=${(list, origin) => setQuick({ list, origin, key: Date.now() })}
        onOpenGraph=${(view, name) => (view
          ? openInGraphEditor(api, view, name).catch((cause) => (notify || defaultNotify)(`Could not open the graph editor: ${cause.message}`, true))
          : handOffProcessDraft({ create: true }))}
        onDraft=${(text) => draftWithScribe(api, { group, text, roster: people, confirm })
          .then((draft) => { if (draft) { setQuick({ mode: 'scribe', key: Date.now(), draft }); void source?.refresh?.(); } })
          .catch((cause) => (notify || defaultNotify)(`Could not draft the process: ${cause.message}`, true))}
        onImport=${pickPwf}
        onReuse=${(templateId) => { setQuick(null); setStarting({ templateId }); }}
        onDismiss=${() => { markQuickStart('dismissed'); setQuick(null); setDismissed(true); }} />`
      : fresh && folded && !starting && html`<div class="gp-empty gq-folded" data-quickstart-folded=${group.name}>
        ${wizard ? 'No rites yet' : 'No processes yet'} · <button type="button" class="gq-link" data-quickstart-expand onClick=${() => setQuick('pick')}>Get started ▸</button></div>`;
  const startScope = (scope) => { setQuick(null); setStarting({ scope }); };
  const controls = (inline) => html`<${RunControls} group=${group} runs=${liveGroupRuns(context, entry, members)}
    agentIds=${members.map((member) => member.agent_id).filter(Boolean)} wizard=${wizard} runApi=${runApi}
    source=${source} notify=${notify} onStart=${startScope} inline=${inline} />`;
  // Beside the body, not in it: Both's section unmounts its body when folded,
  // which would lose an open review and replay a dropped file on unfolding.
  const importer = html`<span ref=${importerRef}><${WorkflowImportButton} group=${group.name} id="" className="dock-process-import-hidden"
    dropped=${droppedPwf} onImported=${() => void source.refresh()} /></span>`;
  const body = html`${savedProcesses.length > 0 && html`<${SavedProcesses} group=${group} processes=${savedProcesses} api=${api} source=${source}
      wizard=${wizard} notify=${notify || defaultNotify}
      editing=${editing ? quick.process?.templateId || quick.saved?.templateId || quick.draft?.templateId || '' : ''} scribes=${scribes}
      onEdit=${(edit) => {
        setStarting(false);
        setQuick(edit.mode === 'graph' ? { ...edit, stored: edit.view, key: Date.now() } : { ...edit, key: Date.now() });
      }} />`}
    ${starting && html`<${StartRunForm} key=${starting.scope || 'group'} group=${group} api=${api} source=${source} wizard=${wizard}
      members=${starting.scope === 'agent' ? members : null} onBuild=${() => { setStarting(false); setQuick('pick'); }}
      initialTemplateId=${starting?.templateId || ''} scribes=${scribes} onClose=${() => setStarting(false)}
      onOpenTemplate=${async (process) => {
        const view = await api.getTemplate(process.templateId);
        setStarting(false);
        openStored({ ...process, listed: view.listed }, view);
      }} />`}
    ${quickStart}
    ${runs.map((run) => html`<${RunCard} key=${run.runId} run=${run} entry=${entry} nameOf=${nameOf} wizard=${wizard} onOpen=${open} />`)}
    <${MemberRuns} entries=${memberRuns} label=${wizard ? 'Hero rites' : 'Member runs'} onOpen=${open} />
    ${planned.length > 0 && html`<div class="gp-planned-list">${planned.map((item) => html`<${PlannedRow} key=${item.id} item=${item} wizard=${wizard} />`)}</div>`}
    ${!runs.length && !planned.length && !memberRuns.length && !fresh && html`<div class="gp-empty">${!label
      ? (wizard ? 'No rites in this party yet.' : 'No processes in this group yet.')
      : `No ${label} ${wizard ? 'rites' : 'processes'}.`}</div>`}`;
  if (fused) {
    const shown = runs.length > 0 || planned.length > 0 || memberRuns.length > 0;
    // The run controls head the open section's content on their own title
    // row, where the Agents half has its table header; folded, they ride
    // the section line.
    return html`<${FusedSection} group=${group.name} section="agents" label=${wizard ? 'Heroes' : 'Agents'} count=${fused.count}
        actions=${fused.actions} closedActions=${fused.closedActions}>${fused.agents}<//>
      <${FusedSection} group=${group.name} section="processes" label=${wizard ? 'Rites' : 'Processes'}
        count=${runs.length + planned.length + memberRuns.length}
        actions=${quick === 'pick' && html`<button type="button" class="gp-start" onClick=${() => setQuick(null)}>close</button>`}
        closedActions=${controls(true)}
        auto=${shown} force=${!!starting || !!quick} attrs=${{ 'data-fused-processes': group.name }}>
        <div class=${`gp-group${pwfOver ? ' gp-pwf-over' : ''}`} data-process-group=${group.name} ...${pwfDrop}><div class="group-header-actions gf-title-row">${controls(true)}</div>${body}</div>
      <//>
      ${fused.after}
      ${importer}`;
  }
  return html`<div class=${`gp-group${pwfOver ? ' gp-pwf-over' : ''}`} data-process-group=${group.name} ...${pwfDrop}>
    ${importer}
    ${controls(false)}
    <div class="gp-group-head">
      <${GroupAvatarStrip} members=${members} busy=${busy} wizard=${wizard} />
      <span class="gp-spacer"></span>
      ${quick === 'pick' && html`<button type="button" class="gp-start" onClick=${() => setQuick(null)}>close</button>`}
    </div>
    ${body}
  </div>`;
}

// FusedSection is one of Both's two sections in a group: a thin labelled
// divider row, not a box, so the content keeps the exact left edge it has
// in Agents and Processes -- Both adds a line, never a nesting level. The
// line carries the section's own controls (`actions`, plus `closedActions`
// while folded, which the open content otherwise heads), so they stay in
// reach while the section is folded. A dim `count` beside the label says
// whether a folded section has anything in it. The label, and the empty
// rest of the line, fold the section; the choice is remembered per group
// (see groups-fused-sections.js), and `force` holds it open while a start
// or edit form inside needs it. `onToggle(el)` runs after a fold changes,
// with the clicked element, for a section whose state matters outside it
// (Subgroups moves activity bots).
// A button on a folded line unfolds the section too, so what it spawns or
// starts shows up rather than landing out of sight. That re-render waits for
// a task, not a microtask: a microtask runs between listeners, and opening
// drops `closedActions`, which would detach the button before the
// document's row-action handler sees the click.
export function FusedSection({ group, section, label, count = null, auto = true, force = false, attrs = {}, actions = null, closedActions = null, onToggle = null, children }) {
  const [, bump] = useState(0);
  const open = force || fusedSectionOpen(group, section, auto);
  const toggle = (event) => {
    if (force) return;
    setFusedSectionOpen(group, section, !open);
    bump((n) => n + 1);
    onToggle?.(event.currentTarget);
  };
  const unfold = (event) => {
    if (open || !event.target.closest?.('button')) return;
    setFusedSectionOpen(group, section, true);
    const el = event.currentTarget;
    setTimeout(() => { bump((n) => n + 1); onToggle?.(el); }, 0);
  };
  const title = force ? 'Open while a form inside is in use' : open ? `Fold ${label}` : `Show ${label}`;
  return html`<div class=${`gf-section gf-section-${section}${open ? ' open' : ''}${force ? ' forced' : ''}`} data-fused-section=${section} ...${attrs}>
    <div class="gf-section-head">
      <button type="button" class="gf-section-toggle" aria-expanded=${open ? 'true' : 'false'} disabled=${force} title=${title} onClick=${toggle}>
        <span class="gf-tri" aria-hidden="true">${open ? '▾' : '▸'}</span>
        <span class="gf-label">${label}</span>
        ${count !== null && html`<span class=${`gf-count${count ? '' : ' zero'}`}>${count}</span>`}
      </button>
      ${(actions || (!open && closedActions)) && html`<span class="group-header-actions gf-section-actions" onClickCapture=${unfold}>${!open && closedActions}${actions}</span>`}
      <span class="gf-section-fill" aria-hidden="true" title=${title} onClick=${toggle}></span>
    </div>
    ${open && children}
  </div>`;
}

const ProcessesPerspective = createContext(null);

// useProcessesPerspective is what the groups tree reads to tell the lenses
// apart: null in Agents, the run context and handlers in Processes, and the
// same with `fused` set in Both.
export function useProcessesPerspective() {
  return useContext(ProcessesPerspective);
}

// GroupProcessBody replaces a real group's member details in the Processes
// perspective: its own scope's runs only (subgroups render theirs nested).
// In Both it is the whole group body instead; `fused` carries the agent half
// (see GroupRuns).
// A group the run context does not cover (see groups-process-scope.js) gets
// UnsharedRuns instead; the runs body otherwise sees the group as the context
// names it, so its starts, edits and pauses reach that group.
export function GroupProcessBody({ group, fused = null }) {
  const perspective = useProcessesPerspective();
  const own = useProcessScope()(group);
  if (!perspective) return null;
  if (!own) return html`<${UnsharedRuns} group=${group} wizard=${perspective.wizard} fused=${fused} />`;
  return html`<${GroupRuns} group=${own} roster=${perspective.rosters.get(own.name)} context=${perspective.context}
    filter=${perspective.filter} wizard=${perspective.wizard} api=${perspective.api} source=${perspective.source}
    onFocusRun=${perspective.onFocusRun} notify=${perspective.notify} runApi=${perspective.runApi} fused=${fused} confirm=${perspective.confirm} scribes=${perspective.scribes} />`;
}

// UnsharedRuns stands in for the runs of a group on a node whose run context
// is not read here (a peer's group in the fused view). In Both the Agents
// section stays as it is.
function UnsharedRuns({ group, wizard, fused }) {
  const node = group.fleet_node?.name || 'this node';
  // This node's own groups on a peer's page: their runs show on its own page.
  const text = group.fleet_node?.local
    ? (wizard ? 'The rites of this realm show on its own page.' : "This node's process runs show on its own page.")
    : (wizard ? `The rites of ${node} are not shared across realms.` : `Process runs on ${node} are not shared across nodes.`);
  const note = html`<div class="gp-empty gp-unshared" data-process-unshared=${group.name}>${text}</div>`;
  if (!fused) return html`<div class="gp-group" data-process-group=${group.name}>${note}</div>`;
  return html`<${FusedSection} group=${group.name} section="agents" label=${wizard ? 'Heroes' : 'Agents'} count=${fused.count}
      actions=${fused.actions} closedActions=${fused.closedActions}>${fused.agents}<//>
    ${note}
    ${fused.after}`;
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
  const label = filterLabel(filter, wizard);
  // In Both, Ungrouped's runs follow its member table only when there are
  // any: it has no controls, so an empty section would be pure noise.
  if (perspective.fused) {
    if (!entries.length) return null;
    return html`<${FusedSection} group=${group.key || group.name} section="processes" label=${wizard ? 'Rites' : 'Processes'}
      attrs=${{ 'data-fused-processes': group.key || group.name }}>
      <div class="gp-group" data-process-group=${group.key || group.name}>
        <${MemberRuns} entries=${entries} label=${wizard ? 'Hero rites' : 'Agent runs'}
          onOpen=${(runId) => perspective.onFocusRun({ group: group.key || group.name, runId })} />
      </div>
    <//>`;
  }
  return html`<div class="gp-group" data-process-group=${group.key || group.name}>
    <${MemberRuns} entries=${entries} label=${wizard ? 'Hero rites' : 'Agent runs'}
      onOpen=${(runId) => perspective.onFocusRun({ group: group.key || group.name, runId })} />
    ${!entries.length && html`<div class="gp-empty">${runFilterHas(filter, 'running')
      ? (wizard ? 'No unbound hero walks a rite.' : 'No ungrouped agent runs a process.')
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
  const scope = useProcessScope();
  if (!perspective || !node.children?.length) return null;
  const total = { running: 0, needsYou: 0, planned: 0 };
  const add = (child) => {
    const own = scope(child.group);
    const counts = groupProcessCounts(own ? groupContextEntry(perspective.context, own) : null);
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

// UngroupedRunCount is Ungrouped's summary-row process graphs, beside its
// member bots in both perspectives; nothing when no agent runs a process.
export function UngroupedRunCount({ group, snapshot, source = processContext }) {
  const perspective = useProcessesPerspective();
  const context = perspective ? perspective.context : (enabled(snapshot, source) ? source.context.value : null);
  if (!context) return null;
  const entries = memberRunEntries(context, group.members || [], 'running');
  const needsYou = entries.filter((entry) => entry.run.needsYou === true).length;
  const counts = { running: entries.length - needsYou, needsYou, planned: 0 };
  const wizard = perspective ? perspective.wizard : document.body.classList.contains('wizard');
  return html`<${ProcessGraphRow} counts=${counts} wizard=${wizard} />`;
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

// GroupsProcessesFocus frames the Processes perspective: the load states, and
// either the single-run focus or `children` -- the shared groups tree --
// under the ProcessesPerspective it reads. The run filter itself is the
// checklist on the toolbar's Processes button (groups-focus.js).
//
// `fused` frames Both the same way, except that the tree never waits on the
// run context: until it is ready the tree shows as in Agents.
export function GroupsProcessesFocus({
  groups, allGroups = null, filter = DEFAULT_RUN_FILTER, wizard = false, source = processContext, api = processRunApi,
  focusedRun = null, onFocusRun = () => {}, runApi = undefined, children = null, notify = undefined, fused = false, confirm = undefined,
}) {
  const rosters = new Map((allGroups || []).map((group) => [group.name, group]));
  const current = normalizeRunFilter(filter);
  const fusedFrame = useRef(null);
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
  useEffect(() => { if (strandedRun) onFocusRun(null, { correction: true }); }, [strandedRun]);
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
    return html`<div class="gp-focus" data-groups-processes=${fused ? undefined : ''} data-groups-fused=${fused ? '' : undefined}>
      ${source.error.value && html`<span class="gp-stale" role="status">Showing the last update: ${source.error.value}</span>`}
      <${GroupsRunFocus} group=${focusGroup} roster=${rosters.get(focusGroup.name)} context=${context} runId=${focusedRun.runId}
        wizard=${wizard} source=${source} api=${runApi} onClose=${() => onFocusRun(null)}
        onSelectRun=${(runId) => onFocusRun({ group: focusedRun.group, runId })} />
    </div>`;
  }
  const scribes = processScribeSessions({ groups: allGroups || [] });
  const perspective = { context, filter: current, wizard, api, runApi: runApi || runDetailApi, source, onFocusRun, rosters, notify, fused, confirm, scribes };
  if (fused) {
    return html`<div class="gp-focus gf-focus" data-groups-fused ref=${fusedFrame}>
      ${source.error.value && status === 'ready' && html`<div class="gp-toolbar"><span class="gp-stale" role="status">Showing the last update: ${source.error.value}</span></div>`}
      ${status === 'error' && html`<div class="gp-toolbar"><span class="gp-stale gp-error" role="alert">Could not load process runs: ${source.error.value}</span></div>`}
      ${status === 'ready'
        ? html`<${ProcessesPerspective.Provider} value=${perspective}><div class="gp-tree">${children}</div><//>`
        : html`<div class="gp-tree">${children}</div>`}
      ${status === 'ready' && context?.truncated && html`<div class="gp-card-note">Some runs are not shown: the run context was truncated.</div>`}
      ${status === 'ready' && html`<${FusedLinks} frame=${fusedFrame} />`}
    </div>`;
  }
  return html`<div class="gp-focus" data-groups-processes>
    ${source.error.value && status === 'ready' && html`<div class="gp-toolbar"><span class="gp-stale" role="status">Showing the last update: ${source.error.value}</span></div>`}
    ${status === 'idle' && html`<div class="gp-empty" role="status">Loading process runs…</div>`}
    ${status === 'error' && html`<div class="gp-empty gp-error" role="alert">Could not load process runs: ${source.error.value}</div>`}
    ${status === 'unavailable' && html`<${GroupsProcessesPlaceholder} wizard=${wizard} />`}
    ${status === 'ready' && html`<${ProcessesPerspective.Provider} value=${perspective}><div class="gp-tree">${children}</div><//>`}
    ${status === 'ready' && context?.truncated && html`<div class="gp-card-note">Some runs are not shown: the run context was truncated.</div>`}
  </div>`;
}
