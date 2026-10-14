// groups-run-focus.js -- the Groups › Processes single-run focus (mock G5):
// the group's runs on the left, the selected run's live graph in the middle,
// the selected node in a docked inspector on the right (approve / request
// changes on a human gate, history), and the group's agents as a performer
// dock along the bottom.
//
// The graph is the shared kit graph the G3 cards draw (process-kit.js); the
// run's durable detail (awaited verdicts, blocked branches, evidence events)
// comes from the run API beside the context read model, refreshed on each
// snapshot poll while the focus is open.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ProcessMiniGraph, ProcessNodeGlyph, ProcessScopeChip, ProcessStateChip, ProcessStepStrip,
  kindLabel, performerColor, runGraphNodes, runName, runState, stateLabel,
} from './process-kit.js';
import {
  formatRunDuration, groupContextEntry, groupRunsFor, runDetailApi, runNeedsYou, withPerformerNames,
} from './process-context.js';

const html = htm.bind(h);

// Events kept per open run: the inspector shows a node's latest few, and a
// run that has been open for hours must not grow without bound.
const MAX_EVENTS = 1000;
const HISTORY_ROWS = 8;

function memberName(member) {
  return member?.title || member?.agent_id || member?.conv_id || '';
}

function memberKey(member) {
  return member?.agent_id || member?.conv_id || '';
}

// useRunDetail reads the run's durable view and its events, then follows
// both on every snapshot poll: the view in full, the events from the last
// sequence seen. One read is in flight at a time: a poll that lands mid-read
// queues exactly one follow-up instead of discarding the read, so a slow
// daemon still converges. A long run's history catches up read after read
// (`catchingUp`) without waiting for polls.
function useRunDetail(api, runId) {
  const empty = (id) => ({ runId: id, view: null, events: [], error: '', catchingUp: false });
  const [detail, setDetail] = useState(() => empty(runId));
  const [revision, setRevision] = useState(0);
  const latest = useRef(detail);
  latest.current = detail;
  const inflight = useRef(null);
  const again = useRef(false);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    const poll = () => setRevision((value) => value + 1);
    document.addEventListener('tclaude:snapshot', poll);
    return () => {
      mounted.current = false;
      document.removeEventListener('tclaude:snapshot', poll);
    };
  }, []);
  useEffect(() => {
    if (inflight.current?.runId === runId) {
      again.current = true;
      return;
    }
    const token = { runId };
    inflight.current = token;
    again.current = false;
    const prior = latest.current.runId === runId ? latest.current : empty(runId);
    if (prior !== latest.current) setDetail(prior);
    const after = prior.events.length ? prior.events[prior.events.length - 1].sequence : 0;
    const settle = (next) => {
      if (inflight.current !== token || !mounted.current) return;
      inflight.current = null;
      setDetail(next);
      if (again.current || next.catchingUp) {
        again.current = false;
        setRevision((value) => value + 1);
      }
    };
    Promise.all([
      api.getRun(runId),
      api.listEvents(runId, after).catch(() => ({ events: [], more: false })),
    ]).then(([view, page]) => {
      const events = [...prior.events, ...(page.events || [])].slice(-MAX_EVENTS);
      settle({ runId, view, events, error: '', catchingUp: page.more === true });
    }).catch((cause) => {
      settle({ ...prior, error: cause?.message || String(cause), catchingUp: false });
    });
  }, [api, runId, revision]);
  const current = detail.runId === runId ? detail : empty(runId);
  return [current, () => setRevision((value) => value + 1)];
}

// defaultNode picks what the inspector opens on: the gate waiting on you,
// else the run's current node.
function defaultNode(run, view) {
  const ids = new Set(runGraphNodes(run).map((node) => node.id));
  const awaited = (view?.awaitingDecisions || []).find((decision) => ids.has(String(decision.nodeId)));
  if (awaited) return String(awaited.nodeId);
  return [].concat(run?.current || []).map(String).find((id) => ids.has(id)) || '';
}

const VERDICT_PRESENTATION = [
  [/^(approved?|accept(ed)?|yes|ok|pass(ed)?|lgtm)$/i, '✓ approve', 'primary'],
  [/^(changes?|changes?[_-]requested|request[_-]changes|reject(ed)?|rework|no|fail(ed)?)$/i, '↺ request changes', ''],
];

export function verdictPresentation(verdict) {
  for (const [pattern, label, cls] of VERDICT_PRESENTATION) {
    if (pattern.test(String(verdict))) return { label, cls };
  }
  return { label: String(verdict), cls: '' };
}

function performerText(node, nameOf, wizard) {
  const performer = node.performer || {};
  const kind = String(performer.kind || '').toLowerCase();
  if (kind === 'human') return wizard ? 'any keeper' : 'any operator';
  if (kind === 'program' || kind === 'command') return wizard ? 'an incantation' : 'program';
  const ref = performer.ref && performer.ref !== 'unassigned' ? performer.ref : '';
  if (!ref) return kind === 'agent' ? 'unassigned' : '—';
  return nameOf(ref) || performer.name || performer.label || ref;
}

function eventTime(iso) {
  const when = new Date(iso || '');
  return Number.isNaN(when.getTime()) ? '' : when.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function eventText(event) {
  const kind = String(event.kind || '').replace(/[_.]+/g, ' ');
  return event.actor ? `${kind} · ${event.actor}` : kind;
}

function DecisionForm({ decision, node, onDecide, wizard }) {
  const [busy, setBusy] = useState('');
  const [decided, setDecided] = useState('');
  const [note, setNote] = useState('');
  const [error, setError] = useState('');
  const decide = async (verdict) => {
    if (busy || decided) return;
    setBusy(verdict);
    setError('');
    try {
      await onDecide({ nodeId: node.id, verdict, evidence: note.trim(), attempt: decision.attempt || 0 });
      // The gate stays shut until the refreshed view drops this decision
      // (the form is keyed by node and attempt), so a second click cannot
      // race the first into a refused duplicate.
      setDecided(verdict);
      setNote('');
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      setBusy('');
    }
  };
  const verdicts = decision.verdicts || [];
  return html`<div class="grf-decide" data-run-decision=${node.id}>
    <textarea aria-label=${wizard ? 'Words for the record (optional)' : 'Note for the record (optional)'} rows="2" maxlength="4096"
      placeholder=${wizard ? 'words for the record (optional)' : 'note (optional)'} value=${note} disabled=${!!busy || !!decided}
      onInput=${(event) => setNote(event.currentTarget.value)}></textarea>
    <div class="grf-verdicts">${verdicts.map((verdict) => {
      const { label, cls } = verdictPresentation(verdict);
      return html`<button key=${verdict} type="button" class=${cls} data-run-verdict=${verdict} disabled=${!!busy || !!decided}
        onClick=${() => decide(verdict)}>${busy === verdict ? '…' : label}</button>`;
    })}</div>
    ${!verdicts.length && html`<div class="grf-muted">This gate offers no verdicts.</div>`}
    ${decided && html`<div class="grf-muted" role="status">${wizard ? 'Your word is sealed:' : 'Recorded:'} ${verdictPresentation(decided).label}</div>`}
    ${error && html`<div class="grf-error" role="alert">${error}</div>`}
  </div>`;
}

export function RunNodeInspector({ run, node, detail, nameOf, wizard = false, onDecide }) {
  if (!node) {
    return html`<aside class="grf-inspector" aria-label=${wizard ? 'Rite step' : 'Node inspector'}>
      <div class="grf-muted">${wizard ? 'Choose a step to read it.' : 'Select a node to inspect it.'}</div>
    </aside>`;
  }
  const nodes = runGraphNodes(run);
  const byId = new Map(nodes.map((item) => [item.id, item]));
  const edges = (run.edges || []).filter((edge) => !edge.back);
  const upstream = edges.filter((edge) => String(edge.to) === node.id).map((edge) => byId.get(String(edge.from))).filter(Boolean);
  const downstream = edges.filter((edge) => String(edge.from) === node.id).map((edge) => byId.get(String(edge.to))).filter(Boolean);
  const view = detail.view;
  const decision = (view?.awaitingDecisions || []).find((item) => String(item.nodeId) === node.id);
  const blocked = (view?.blocked || []).find((item) => String(item.nodeId) === node.id);
  const history = detail.events.filter((event) => String(event.nodeId || '') === node.id).slice(-HISTORY_ROWS);
  const neighbours = (list) => (list.length
    ? list.map((item, i) => html`${i > 0 && ', '}<span class=${`grf-neighbour pk-state-${item.state}`}>${item.label}${item.state === 'done' ? ' ✓' : ''}</span>`)
    : '—');
  return html`<aside class="grf-inspector" aria-label=${wizard ? 'Rite step' : 'Node inspector'} data-run-inspector=${node.id}>
    <div class="grf-kicker">${wizard ? 'STEP' : 'NODE'} · ${kindLabel(node.kind, wizard).toLowerCase()}</div>
    <h3 class="grf-node-title"><${ProcessNodeGlyph} kind=${node.kind} wizard=${wizard} /> ${node.label}</h3>
    <${ProcessStateChip} state=${node.state} label=${decision ? (wizard ? 'awaits your word' : 'waiting on you') : stateLabel(node.state)} />
    <dl class="grf-facts">
      <dt>${wizard ? 'bearer' : 'performer'}</dt><dd>${performerText(node, nameOf, wizard)}</dd>
      <dt>upstream</dt><dd>${neighbours(upstream)}</dd>
      <dt>downstream</dt><dd>${neighbours(downstream)}</dd>
      ${blocked && html`<dt>blocked</dt><dd class="grf-blocked">${blocked.reason || 'parked on an operator'}</dd>`}
    </dl>
    ${decision && html`<${DecisionForm} key=${`${node.id}:${decision.attempt || 0}`} decision=${decision} node=${node} onDecide=${onDecide} wizard=${wizard} />`}
    <div class="grf-section">${wizard ? 'CHRONICLE' : 'HISTORY'}</div>
    ${history.length
    ? html`<ol class="grf-history">${history.map((event) => html`<li key=${event.sequence}><time>${eventTime(event.occurredAt)}</time> ${eventText(event)}</li>`)}</ol>`
    : html`<div class="grf-muted">${view ? 'No events for this node yet.' : 'Loading…'}</div>`}
    ${detail.catchingUp && html`<div class="grf-muted" role="status">Reading the rest of this run's history…</div>`}
  </aside>`;
}

function RunListItem({ run, entry, selected, onSelect, nameOf, wizard }) {
  const state = runState(run);
  const terminal = state === 'done' || state === 'failed';
  const duration = formatRunDuration(run.startedAt, terminal && run.updatedAt ? Date.parse(run.updatedAt) : Date.now());
  const agentScope = run.scope?.kind === 'agent' ? nameOf(run.scope.agentId) || run.scope.agentId : '';
  return html`<li><button type="button" class=${`grf-run${selected ? ' selected' : ''}`} data-run-list-item=${run.runId}
      aria-current=${selected ? 'true' : undefined} onClick=${() => onSelect(run.runId)}>
    <span class="grf-run-name">${runName(run)}${agentScope && html` <${ProcessScopeChip} scope=${{ kind: 'agent', name: agentScope }} wizard=${wizard} />`}</span>
    <span class="grf-run-meta"><span class=${`pk-dot-inline pk-state-${state}`} aria-hidden="true">●</span> ${runNeedsYou(run, entry) ? (wizard ? 'awaits you' : 'needs you') : stateLabel(state).toLowerCase()}${duration && ` · ${duration}`} <${ProcessStepStrip} run=${run} /></span>
  </button></li>`;
}

function RunList({ group, entry, runs, planned, selectedRunId, onSelect, nameOf, wizard }) {
  const live = runs.filter((run) => !['done', 'failed'].includes(runState(run)));
  const recent = runs.filter((run) => ['done', 'failed'].includes(runState(run)));
  const item = (run) => html`<${RunListItem} key=${run.runId} run=${run} entry=${entry} selected=${run.runId === selectedRunId}
    onSelect=${onSelect} nameOf=${nameOf} wizard=${wizard} />`;
  return html`<nav class="grf-runs" aria-label=${`${group.name} ${wizard ? 'rites' : 'runs'}`}>
    <div class="grf-section">${group.name} · ${wizard ? 'RITES' : 'RUNS'}</div>
    ${live.length ? html`<ul>${live.map(item)}</ul>` : html`<div class="grf-muted">${wizard ? 'No rites unfolding.' : 'Nothing running.'}</div>`}
    ${planned.length > 0 && html`<div class="grf-section">${wizard ? 'FORETOLD' : 'PLANNED'}</div>
      <ul>${planned.map((p) => html`<li key=${p.id} class="grf-planned" data-run-planned=${p.id}>
        <span class="grf-run-name">${p.name || p.templateId || p.id}</span>
        <span class="grf-run-meta">◷ ${p.nextAt ? new Date(p.nextAt).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' }) : ''} · ${p.source || 'scheduled'}</span>
      </li>`)}</ul>`}
    ${recent.length > 0 && html`<div class="grf-section">${wizard ? 'FULFILLED' : 'RECENT'}</div><ul>${recent.map(item)}</ul>`}
  </nav>`;
}

// PerformerDock lists the group's agents along the canvas foot. Drag-assign
// needs a run-time reassign API, which the daemon does not have yet: the
// dock ships read-only and says so.
// TODO(tcl-amd5lb): wire drag onto a node once runs can reassign a node's
// performer, and "+ spawn for node" once a run can spawn into its group.
function PerformerDock({ members, busy, wizard }) {
  return html`<div class="grf-dock" role="group" aria-label=${wizard ? 'Familiars' : 'Performers'} data-run-dock>
    <span class="grf-dock-label">${wizard ? 'familiars' : 'performers'}</span>
    ${members.map((member) => html`<span key=${memberKey(member)} class=${`grf-dock-agent${busy.has(memberKey(member)) ? ' busy' : ''}`}
      draggable="false" aria-disabled="true" data-run-dock-agent=${memberKey(member)}
      title=${`${memberName(member)}: drag-assign needs run reassignment, which is not available yet`}>
      <span class=${`pk-avatar ${performerColor(memberKey(member))}`} aria-hidden="true">${[...String(memberName(member) || '?')][0].toUpperCase()}</span>${memberName(member)}
    </span>`)}
    <span class="grf-dock-note">${wizard ? 'binding a familiar to a step is not yet possible' : 'drag-assign arrives with run reassignment'}</span>
  </div>`;
}

export function GroupsRunFocus({
  group, roster = null, context, runId, wizard = false, source, api = runDetailApi, onSelectRun = () => {}, onClose = () => {},
}) {
  const entry = groupContextEntry(context, group);
  const { runs, planned } = groupRunsFor(context, entry, 'all');
  const members = ((roster || group).members || []).filter((member) => memberKey(member));
  const names = new Map(members.map((member) => [memberKey(member), memberName(member)]));
  const nameOf = (ref) => names.get(ref) || '';
  const raw = context?.runs?.[runId] ? { ...context.runs[runId], runId: context.runs[runId].runId ?? runId } : null;
  const run = raw ? withPerformerNames(raw, nameOf) : null;
  const [detail, refresh] = useRunDetail(api, runId);
  const [picked, setPicked] = useState({ runId, nodeId: '' });
  const nodeId = picked.runId === runId && picked.nodeId ? picked.nodeId : defaultNode(run, detail.view);
  const node = run ? runGraphNodes(run).find((item) => item.id === nodeId) || null : null;
  // A wide graph scrolls; bring the selected node into view when a run opens
  // or the selection moves, without scrolling the page.
  const graphRef = useRef(null);
  useEffect(() => {
    const pane = graphRef.current;
    const target = node && [...(pane?.querySelectorAll('[data-process-node]') || [])].find((el) => el.dataset.processNode === node.id);
    if (!pane || !target?.getBoundingClientRect) return;
    const paneBox = pane.getBoundingClientRect();
    const box = target.getBoundingClientRect();
    if (box.left < paneBox.left || box.right > paneBox.right) {
      pane.scrollLeft += box.left - paneBox.left - (paneBox.width - box.width) / 2;
    }
    if (box.top < paneBox.top || box.bottom > paneBox.bottom) {
      pane.scrollTop += box.top - paneBox.top - (paneBox.height - box.height) / 2;
    }
  }, [runId, node?.id, !!run]);
  const busy = new Set();
  for (const item of runs) {
    for (const n of item.nodes || []) {
      if (['running', 'awaiting', 'blocked', 'ready'].includes(n.state) && n.performer?.kind === 'agent' && n.performer.ref) busy.add(n.performer.ref);
    }
  }
  const onDecide = async (request) => {
    await api.decide(runId, request);
    refresh();
    void source?.refresh?.();
  };
  // Opening a run moves focus onto its back button (the clicked card button
  // is gone), and Esc closes the focus from anywhere but a text field.
  const backRef = useRef(null);
  useEffect(() => { backRef.current?.focus?.(); }, []);
  const close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    const onKeyDown = (event) => {
      if (event.key !== 'Escape' || event.defaultPrevented) return;
      if (event.target?.closest?.('textarea, input, select, [role="dialog"]')) return;
      event.preventDefault();
      close.current();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, []);
  const template = run?.templateRef || run?.templateId || '';
  return html`<div class="grf" data-run-focus=${runId}>
    <${RunList} group=${group} entry=${entry} runs=${runs} planned=${planned} selectedRunId=${runId}
      onSelect=${onSelectRun} nameOf=${nameOf} wizard=${wizard} />
    <section class="grf-canvas" aria-label=${run ? `${runName(run)} ${wizard ? 'rite' : 'run'}` : 'run'}>
      <div class="grf-crumbs">
        <button type="button" class="grf-back" ref=${backRef} data-run-focus-close onClick=${onClose}
          title=${wizard ? 'Back to every rite' : 'Back to every run (Esc)'}>← ${wizard ? 'all rites' : 'all runs'}</button>
        <span>${group.name} › <b>${run ? runName(run) : runId}</b>${template && html` · ${wizard ? 'grimoire' : 'template'} <code>${template}</code>`}</span>
        ${run && runNeedsYou(run, entry) && html`<span class="pk-chip pk-state-waiting">👤 ${wizard ? 'awaits your word' : 'waiting on you'}</span>`}
        ${detail.error && html`<span class="grf-error" role="status">${detail.error}</span>`}
      </div>
      ${run
    ? html`<div class="grf-graph" ref=${graphRef}><${ProcessMiniGraph} run=${run} wizard=${wizard} selectedId=${node?.id || ''}
        onNodeClick=${(clicked) => setPicked({ runId, nodeId: clicked.id })} /></div>`
    : html`<div class="grf-muted grf-gone">${wizard ? 'This rite is no longer in view.' : 'This run is no longer in the process context.'}</div>`}
      <${PerformerDock} members=${members} busy=${busy} wizard=${wizard} />
    </section>
    ${run
    ? html`<${RunNodeInspector} run=${run} node=${node} detail=${detail} nameOf=${nameOf} wizard=${wizard} onDecide=${onDecide} />`
    : html`<aside class="grf-inspector"></aside>`}
  </div>`;
}
