// process-kit.js -- the shared process UI kit (mock S1). Every surface that
// shows a process run in context (Groups, Terminals, the editor) renders it
// through these components so node kinds, run/node states and run scope read
// the same everywhere. Styles live in process-kit.css under the `pk-` prefix.
//
// The kit consumes a small read-only RUN PROJECTION, never the editor model
// or an engine checkpoint:
//
//   run = {
//     id, name,                       // run id and display name (runId / templateRef / templateId also accepted)
//     status,                         // engine run status (running, completed, failed, canceled) or a kit state
//     scope: { kind, name },          // kind: 'group' (⌂) | 'agent' (◉) | '' (unscoped)
//     current,                        // optional node id (or ids) the run is "at"
//     nodes: [{ id, label, kind, state, sub, performers: [{ id, label, initial, kind }] }],
//                                     // or the read model's single performer: { kind, ref }
//     edges: [{ from, to, label, back }],
//   }
//
// Node `kind` and `state` accept the kit vocabulary below plus the engine and
// editor spellings (task, pending, blocked, ...); normalizeNodeKind and
// normalizeNodeState fold them. Pure helpers are exported next to the
// components so callers and tests can reason about a projection without a DOM.

import { h } from 'preact';
import { useId, useState } from 'preact/hooks';
import htm from 'htm';
import { layoutProcessGraph } from './process-layout.js';

const html = htm.bind(h);

export const PROCESS_NODE_KINDS = Object.freeze({
  agent: { glyph: '🤖', label: 'Agent task', wizardLabel: 'Familiar rune', hint: 'prompt, performer slot, stages' },
  human: { glyph: '👤', label: 'Human step', wizardLabel: 'Mortal rune', hint: 'approve / input / do' },
  command: { glyph: '⌘', label: 'Command', wizardLabel: 'Incantation rune', hint: 'shell, in a sandbox' },
  decision: { glyph: '◆', label: 'Decision', wizardLabel: 'Forking rune', hint: 'on outcome or expression' },
  parallel: { glyph: '⫴', label: 'Parallel', wizardLabel: 'Splitting rune', hint: 'fan-out ×N, join all/any' },
  wait: { glyph: '⏳', label: 'Wait', wizardLabel: 'Waiting rune', hint: 'time, event, PR merged…' },
  subprocess: { glyph: '▣', label: 'Subprocess', wizardLabel: 'Nested circle', hint: 'inline or a template ref' },
  start: { glyph: '●', label: 'Start', wizardLabel: 'Opening rune', hint: 'entry marker' },
  end: { glyph: '◉', label: 'End', wizardLabel: 'Closing rune', hint: 'terminal node' },
});

const KIND_ALIASES = Object.freeze({
  task: 'agent', agentic: 'agent', cmd: 'command', shell: 'command', program: 'command',
  approval: 'human', gate: 'human', dec: 'decision', par: 'parallel', join: 'parallel', timer: 'wait',
  comp: 'subprocess', compound: 'subprocess', subgraph: 'subprocess',
});

// Kit states, in the order a run icon prefers them (most urgent first).
export const PROCESS_STATES = Object.freeze({
  failed: { label: 'failed', step: 'f' },
  waiting: { label: 'blocked on human', step: 'w' },
  running: { label: 'running', step: 'r' },
  done: { label: 'done', step: 'd' },
  planned: { label: 'planned', step: '' },
  skipped: { label: 'skipped', step: 's' },
});

const STATE_ALIASES = Object.freeze({
  completed: 'done', succeeded: 'done', success: 'done',
  active: 'running', dispatched: 'running',
  blocked: 'waiting', awaiting: 'waiting', 'needs-human': 'waiting', parked: 'waiting',
  canceled: 'failed', cancelled: 'failed', error: 'failed',
  pending: 'planned', ready: 'planned', queued: 'planned', '': 'planned',
});

export const PROCESS_SCOPES = Object.freeze({
  group: { glyph: '⌂', label: 'group scope', wizardLabel: 'circle-bound' },
  agent: { glyph: '◉', label: 'agent scope', wizardLabel: 'familiar-bound' },
});

export function normalizeNodeKind(kind) {
  const key = String(kind || '').toLowerCase();
  if (PROCESS_NODE_KINDS[key]) return key;
  return KIND_ALIASES[key] || 'agent';
}

export function normalizeNodeState(state) {
  const key = String(state || '').toLowerCase();
  if (PROCESS_STATES[key]) return key;
  return STATE_ALIASES[key] || 'planned';
}

export function normalizeScope(scope) {
  const kind = String(scope?.kind || '').toLowerCase();
  const name = scope?.name || (kind === 'agent' ? scope?.agentName || scope?.agentId : scope?.group || scope?.groupId) || '';
  return { kind: PROCESS_SCOPES[kind] ? kind : '', name: String(name) };
}

export function runID(run) {
  return String(run?.id ?? run?.runId ?? '');
}

export function runName(run) {
  return String(run?.name || run?.templateRef || run?.templateId || runID(run) || 'run');
}

// A task's kit kind follows its performer: the read model keeps the template
// type ('task') and says who performs it.
const PERFORMER_KINDS = Object.freeze({ human: 'human', program: 'command', command: 'command', agent: 'agent' });

function nodeKind(node) {
  const raw = String(node.kind || '').toLowerCase();
  const performer = PERFORMER_KINDS[String(node.performer?.kind || '').toLowerCase()];
  if ((raw === '' || raw === 'task') && performer) return performer;
  return normalizeNodeKind(raw);
}

function nodePerformers(node) {
  if (Array.isArray(node.performers)) return node.performers;
  const ref = String(node.performer?.ref || '');
  if (!ref || ref === 'unassigned') return [];
  return [{ id: ref, label: node.performer.label || ref, kind: node.performer.kind }];
}

function isTerminalKind(kind) {
  return kind === 'start' || kind === 'end';
}

function nodesOf(run) {
  return (run?.nodes || []).filter((node) => node && node.id != null).map((node) => ({
    ...node,
    id: String(node.id),
    kind: nodeKind(node),
    state: normalizeNodeState(node.state),
    label: String(node.label || node.id),
    performers: nodePerformers(node),
  }));
}

// REASSIGNABLE_DISPATCH is the set of dispatch states whose attempt is still
// outstanding and unreported, which is what the assign endpoint accepts.
const REASSIGNABLE_DISPATCH = new Set(['pending', 'needs_performer', 'delivered']);

// reassignableNode reports whether a node's current agent attempt can be
// handed to someone else, and if not, why (for the button's title).
export function reassignableNode(run, node) {
  if (!node || node.kind !== 'agent') return { ok: false, why: 'Only an agent node can be reassigned' };
  if (String(run?.scope?.kind || '') === 'agent') {
    return { ok: false, why: 'An agent-scoped run is performed only by its bound agent' };
  }
  if (!node.dispatch || !REASSIGNABLE_DISPATCH.has(String(node.dispatch.state || ''))) {
    return { ok: false, why: 'The node has no outstanding attempt to reassign' };
  }
  return { ok: true, why: '' };
}

// runPauseState reports whether a run is paused and whether the Pause /
// Resume control applies to it, and if not, why (for the button's title).
export function runPauseState(run) {
  const paused = !!run?.paused;
  const status = String(run?.status || '').toLowerCase();
  if (!paused && status && status !== 'running') return { paused, ok: false, why: 'A finished run cannot be paused' };
  return {
    paused,
    ok: true,
    why: paused ? 'Resume the run: dispatch new turns and commands again'
      : 'Pause the run: work in flight finishes, nothing new is dispatched',
  };
}

// runGraphNodes is the normalized node list (kit kind, state, performers)
// the graph draws, for surfaces that describe one node (the run inspector).
export function runGraphNodes(run) {
  return nodesOf(run);
}

function edgesOf(run, known) {
  return (run?.edges || [])
    .filter((edge) => edge && known.has(String(edge.from)) && known.has(String(edge.to)))
    .map((edge) => ({ ...edge, from: String(edge.from), to: String(edge.to), back: edge.back === true }));
}

// runSteps orders the run's work nodes (start/end excluded) for the step
// strip: a topological walk that ignores back edges, ties broken by
// projection order, with any unreachable leftovers appended in that order.
export function runSteps(run) {
  const nodes = nodesOf(run);
  const index = new Map(nodes.map((node, i) => [node.id, i]));
  const edges = edgesOf(run, index).filter((edge) => !edge.back && edge.from !== edge.to);
  const indegree = new Map(nodes.map((node) => [node.id, 0]));
  const out = new Map(nodes.map((node) => [node.id, []]));
  for (const edge of edges) {
    indegree.set(edge.to, indegree.get(edge.to) + 1);
    out.get(edge.from).push(edge.to);
  }
  const ready = nodes.filter((node) => indegree.get(node.id) === 0).map((node) => node.id);
  const ordered = [];
  const seen = new Set();
  while (ready.length) {
    ready.sort((a, b) => index.get(a) - index.get(b));
    const id = ready.shift();
    seen.add(id);
    ordered.push(id);
    for (const next of out.get(id)) {
      indegree.set(next, indegree.get(next) - 1);
      if (indegree.get(next) === 0) ready.push(next);
    }
  }
  for (const node of nodes) if (!seen.has(node.id)) ordered.push(node.id);
  const byID = new Map(nodes.map((node) => [node.id, node]));
  return ordered.map((id) => byID.get(id)).filter((node) => !isTerminalKind(node.kind));
}

function performs(node, performerID) {
  return (node.performers || []).some((p) => String(p?.id) === String(performerID));
}

// currentRunNode picks the node a badge or icon names. For a performer it is
// the node that performer is on (active first, then its next planned node);
// otherwise the explicit `current`, else the first waiting, running, failed or
// planned step, else the last step.
export function currentRunNode(run, performerID = '') {
  const steps = runSteps(run);
  if (!steps.length) return null;
  const pick = (list) => list.find((n) => n.state === 'waiting')
    || list.find((n) => n.state === 'running')
    || list.find((n) => n.state === 'failed')
    || list.find((n) => n.state === 'planned');
  if (performerID) {
    const mine = steps.filter((node) => performs(node, performerID));
    const found = pick(mine) || mine[mine.length - 1];
    if (found) return found;
  }
  if (run?.current != null) {
    const ids = new Set([].concat(run.current).map(String));
    const explicit = steps.filter((node) => ids.has(node.id));
    const found = pick(explicit) || explicit[0];
    if (found) return found;
  }
  return pick(steps) || steps[steps.length - 1];
}

// runState folds a run into one kit state: a terminal run status wins, then
// the most urgent node state among its steps.
export function runState(run) {
  const status = String(run?.status || '').toLowerCase();
  const normalized = normalizeNodeState(status);
  if (status && (normalized === 'failed' || normalized === 'done')) return normalized;
  const states = new Set(runSteps(run).map((node) => node.state));
  if (states.has('failed')) return 'failed';
  if (states.has('waiting')) return 'waiting';
  if (states.has('running') || normalized === 'running') return 'running';
  if (states.size && [...states].every((state) => state === 'done' || state === 'skipped')) return 'done';
  return 'planned';
}

export function stateLabel(state) {
  return PROCESS_STATES[normalizeNodeState(state)].label;
}

export function kindLabel(kind, wizard = false) {
  const entry = PROCESS_NODE_KINDS[normalizeNodeKind(kind)];
  return wizard ? entry.wizardLabel : entry.label;
}

// runIconView is the data behind a terminal-overlay icon and the run badge.
// `nodeID` pins the named node when the caller already knows it (the read
// model's per-agent index); it then drives the ring like a performer's node.
export function runIconView(run, performerID = '', nodeID = '') {
  const steps = runSteps(run);
  const pinned = nodeID ? steps.find((step) => step.id === String(nodeID)) : null;
  const node = pinned || currentRunNode(run, performerID);
  const scope = normalizeScope(run?.scope);
  // From a performer's point of view the ring shows that performer's node;
  // a terminal run status still wins so a dead run never reads as live.
  const overall = runState(run);
  const mine = pinned ? pinned.state : performerID && node && performs(node, performerID) ? node.state : '';
  const state = overall === 'failed' || overall === 'done' || !mine || mine === 'skipped' ? overall : mine;
  const position = node ? steps.findIndex((step) => step.id === node.id) + 1 : 0;
  const name = runName(run);
  const parts = [name, stateLabel(overall)];
  if (scope.kind) parts.push(`${PROCESS_SCOPES[scope.kind].label}${scope.name ? ` · ${scope.name}` : ''}`);
  if (node) parts.push(`at ${node.label} (${position}/${steps.length})`);
  return {
    state,
    scope: scope.kind,
    glyph: scope.kind === 'agent' ? '◉' : '◈',
    node,
    position,
    total: steps.length,
    badge: node && steps.length ? `${position}/${steps.length}` : '',
    title: parts.join(' · '),
  };
}

const AVATAR_COLORS = 7;

export function performerColor(id) {
  let hash = 0;
  for (const ch of String(id || '')) hash = (hash * 31 + ch.codePointAt(0)) >>> 0;
  return `c${(hash % AVATAR_COLORS) + 1}`;
}

function performerInitial(performer) {
  if (performer.initial) return String(performer.initial).slice(0, 2);
  const label = String(performer.label || performer.id || '?');
  return [...label][0].toUpperCase();
}

// ── Mini-graph layout ────────────────────────────────────────────────────

const MINI = Object.freeze({
  normal: { height: 40, minWidth: 104, maxWidth: 196, charWidth: 6.6, pad: 46, rankSep: 46, nodeSep: 18, margin: 18, subHeight: 0 },
  compact: { height: 28, minWidth: 74, maxWidth: 156, charWidth: 5.9, pad: 36, rankSep: 34, nodeSep: 12, margin: 14, subHeight: 0 },
});

function miniNodeSize(node, scale) {
  if (isTerminalKind(node.kind)) return { width: 30, height: 30 };
  if (node.kind === 'decision') return { width: scale.height + 18, height: scale.height + 18 };
  const text = Math.max(node.label.length, node.sub && scale === MINI.normal ? String(node.sub).length * 0.85 : 0);
  const width = Math.round(Math.min(scale.maxWidth, Math.max(scale.minWidth, text * scale.charWidth + scale.pad)));
  return { width, height: scale.height };
}

function edgeVisualState(from, to) {
  if (from.state === 'skipped' || to.state === 'skipped') return 'skipped';
  if (from.state === 'done' && (to.state === 'running' || to.state === 'waiting' || to.state === 'failed')) return 'running';
  if (from.state === 'done' && to.state === 'done') return 'done';
  return 'planned';
}

// miniGraphLayout lays a run projection out left to right for the read-only
// mini-graph. It reuses the editor's deterministic layered layout (ranks and
// in-rank order) on a transposed graph, then draws its own simple curves so
// the mini-graph never depends on the editor's routing or DOM.
export function miniGraphLayout(run, { compact = false } = {}) {
  const scale = compact ? MINI.compact : MINI.normal;
  const nodes = nodesOf(run);
  const known = new Map(nodes.map((node) => [node.id, node]));
  const edges = edgesOf(run, known);
  if (!nodes.length) return { width: 0, height: 0, nodes: [], edges: [] };
  const sizes = new Map(nodes.map((node) => [node.id, miniNodeSize(node, scale)]));
  let laid;
  try {
    laid = layoutProcessGraph({
      nodes: nodes.map((node) => ({ id: node.id, type: node.kind })),
      edges: edges.map((edge) => ({ from: edge.from, to: edge.to, back: edge.back })),
    }, {
      marginX: scale.margin,
      marginY: scale.margin,
      nodeSep: scale.nodeSep,
      rankSep: scale.rankSep,
      sweeps: 4,
      // Transposed: the layout's x axis is our y axis and vice versa.
      nodeSize: (raw) => {
        const size = sizes.get(raw.id);
        return { width: size.height, height: size.width };
      },
    });
  } catch {
    laid = null;
  }
  const position = new Map();
  const routed = new Map();
  if (laid) {
    const layer = new Map(laid.nodes.map((node) => [node.id, node.layer]));
    // Edges that skip ranks keep the layout's obstacle-avoiding orthogonal
    // route (transposed); adjacent-rank edges get the mini-graph's own curve.
    for (const edge of laid.edges) {
      if (edge.back || !(layer.get(edge.to) - layer.get(edge.from) > 1) || !edge.points?.length) continue;
      routed.set(edge.inputIndex, edge.points.map((point) => ({ x: point.y, y: point.x })));
    }
    for (const node of laid.nodes) position.set(node.id, { x: node.y, y: node.x });
  } else {
    // Degenerate projection (the layout rejected it): one straight row.
    let cursor = scale.margin;
    for (const node of nodes) {
      const size = sizes.get(node.id);
      position.set(node.id, { x: cursor + size.width / 2, y: scale.margin + scale.height / 2 });
      cursor += size.width + scale.rankSep;
    }
  }
  // Orthogonal escapes may leave the node band (above the first row); shift
  // everything so the graph starts inside its margin.
  const routePoints = [...routed.values()].flat();
  const top = Math.min(...nodes.map((node) => position.get(node.id).y - sizes.get(node.id).height / 2), ...routePoints.map((p) => p.y - 4));
  const left = Math.min(...nodes.map((node) => position.get(node.id).x - sizes.get(node.id).width / 2), ...routePoints.map((p) => p.x));
  const dy = scale.margin - top;
  const dx = scale.margin - left;
  for (const pos of position.values()) { pos.x += dx; pos.y += dy; }
  for (const p of routePoints) { p.x += dx; p.y += dy; }
  const placed = nodes.map((node) => ({ ...node, ...sizes.get(node.id), ...position.get(node.id) }));
  const byID = new Map(placed.map((node) => [node.id, node]));
  const maxX = Math.max(...placed.map((n) => n.x + n.width / 2), ...routePoints.map((p) => p.x)) + scale.margin;
  let maxY = Math.max(...placed.map((n) => n.y + n.height / 2), ...routePoints.map((p) => p.y + 4)) + scale.margin;
  const backLaneY = maxY - scale.margin / 2 + 4;
  let backLanes = 0;
  const laidEdges = edges.map((edge, index) => {
    const a = byID.get(edge.from);
    const b = byID.get(edge.to);
    const state = edgeVisualState(a, b);
    const label = String(edge.label || edge.outcome || '');
    if (edge.back || b.x <= a.x) {
      const lane = backLaneY + backLanes * 10;
      backLanes += 1;
      const y1 = a.y + a.height / 2;
      const y2 = b.y + b.height / 2 + 3;
      return {
        key: `${edge.from}->${edge.to}#${index}`, from: edge.from, to: edge.to, back: true, state, label,
        path: `M${a.x},${y1} C${a.x},${lane} ${b.x},${lane} ${b.x},${y2}`,
        labelX: (a.x + b.x) / 2, labelY: lane - 4,
      };
    }
    const points = routed.get(index);
    if (points) {
      const middle = points[Math.floor((points.length - 1) / 2)];
      const next = points[Math.floor((points.length - 1) / 2) + 1] || middle;
      return {
        key: `${edge.from}->${edge.to}#${index}`, from: edge.from, to: edge.to, back: false, state, label,
        path: points.map((point, i) => `${i ? 'L' : 'M'}${point.x},${point.y}`).join(' '),
        labelX: (middle.x + next.x) / 2, labelY: (middle.y + next.y) / 2,
      };
    }
    const x1 = a.x + a.width / 2;
    const x2 = b.x - b.width / 2 - 3;
    const mid = (x1 + x2) / 2;
    return {
      key: `${edge.from}->${edge.to}#${index}`, from: edge.from, to: edge.to, back: false, state, label,
      path: `M${x1},${a.y} C${mid},${a.y} ${mid},${b.y} ${x2},${b.y}`,
      labelX: mid, labelY: (a.y + b.y) / 2,
    };
  });
  if (backLanes) maxY = backLaneY + backLanes * 10 + 6;
  return { width: Math.ceil(maxX), height: Math.ceil(maxY), nodes: placed, edges: laidEdges };
}

// ── Components ───────────────────────────────────────────────────────────

export function ProcessNodeGlyph({ kind, wizard = false }) {
  const key = normalizeNodeKind(kind);
  const entry = PROCESS_NODE_KINDS[key];
  return html`<span class=${`pk-glyph pk-kind-${key}`} title=${wizard ? entry.wizardLabel : entry.label} aria-hidden="true">${entry.glyph}</span>`;
}

export function ProcessPerformers({ performers = [], max = 3 }) {
  const list = (performers || []).filter(Boolean);
  if (!list.length) return null;
  const shown = list.slice(0, max);
  const extra = list.length - shown.length;
  return html`<span class="pk-avatars">${shown.map((performer) => {
    const raw = performer.kind === 'program' ? 'command' : performer.kind;
    const kind = raw === 'human' || raw === 'command' ? raw : 'agent';
    const color = kind === 'agent' ? performerColor(performer.id ?? performer.label) : kind;
    return html`<span key=${performer.id ?? performer.label} class=${`pk-avatar ${color}`} title=${performer.label || performer.id}>${performerInitial(performer)}</span>`;
  })}${extra > 0 && html`<span class="pk-avatar more" title=${`${extra} more`}>+${extra}</span>`}</span>`;
}

export function ProcessNodeChip({ node, compact = false, wizard = false }) {
  const kind = normalizeNodeKind(node?.kind);
  const state = normalizeNodeState(node?.state);
  const label = String(node?.label ?? node?.id ?? '');
  return html`<span class=${`pk-node-chip pk-state-${state}${compact ? ' compact' : ''}`} title=${`${kindLabel(kind, wizard)} · ${stateLabel(state)}`}><${ProcessNodeGlyph} kind=${kind} wizard=${wizard} /><span class="pk-node-label">${label}</span></span>`;
}

export function ProcessStateChip({ state, label = '' }) {
  const key = normalizeNodeState(state);
  return html`<span class=${`pk-chip pk-state-${key}`}><span class="pk-dot" aria-hidden="true"></span>${label || stateLabel(key)}</span>`;
}

export function ProcessScopeChip({ scope, wizard = false }) {
  const { kind, name } = normalizeScope(scope);
  if (!kind) return null;
  const entry = PROCESS_SCOPES[kind];
  const text = `${entry.glyph} ${wizard ? entry.wizardLabel : entry.label}${name ? ` · ${name}` : ''}`;
  return html`<span class=${`pk-chip pk-scope pk-scope-${kind}`} data-process-scope=${kind}>${text}</span>`;
}

// ProcessPausedChip marks a run an operator paused.
export function ProcessPausedChip({ run }) {
  if (!run?.paused) return null;
  const by = run.paused.by ? ` by ${run.paused.by}` : '';
  return html`<span class="pk-chip pk-paused" data-process-paused title=${`Paused${by}: nothing new is dispatched until it is resumed`}>⏸ paused</span>`;
}

// ProcessPauseControl is the run-level Pause / Resume button. actions:
// { pause(run), resume(run) } — each optional and may throw the server's
// refusal, which is shown beside the button.
export function ProcessPauseControl({ run, actions = {} }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const state = runPauseState(run);
  const act = state.paused ? actions.resume : actions.pause;
  const title = !state.ok ? state.why : !act ? `${state.paused ? 'Resuming' : 'Pausing'} a run is not available here` : state.why;
  const click = async () => {
    setBusy(true);
    setError('');
    try {
      await act(run);
    } catch (err) {
      setError(String(err?.message || err || (state.paused ? 'resume failed' : 'pause failed')));
    } finally {
      setBusy(false);
    }
  };
  return html`<span class="pk-pause" data-process-pause=${state.paused ? 'paused' : 'live'}>
    <button type="button" class="mux-btn" disabled=${busy || !state.ok || !act} title=${title}
      onClick=${() => void click()}>${state.paused ? '▶ resume run' : '⏸ pause run'}</button>
    ${error ? html`<span class="pk-pause-error" role="alert">${error}</span>` : null}
  </span>`;
}

export function ProcessStepStrip({ run }) {
  const steps = runSteps(run);
  if (!steps.length) return null;
  return html`<span class="pk-steps" role="img" aria-label=${steps.map((s) => `${s.label}: ${stateLabel(s.state)}`).join(', ')}>${steps.map((step) => html`<i key=${step.id} class=${`pk-step ${PROCESS_STATES[step.state].step}`} title=${`${step.label} · ${stateLabel(step.state)}`}></i>`)}</span>`;
}

// ProcessRunBadge is the agent-row badge `◈ run › Node ▪▪▫`. With onOpen it
// is a button that opens the run from this performer's point of view.
export function ProcessRunBadge({ run, performerId = '', nodeId = '', onOpen = null }) {
  const view = runIconView(run, performerId, nodeId);
  const cls = `pk-run-badge pk-state-${view.state}${view.scope ? ` pk-scope-${view.scope}` : ''}`;
  const body = html`<span class="pk-run-glyph" aria-hidden="true">${view.glyph}</span><span class="pk-run-name">${runName(run)}</span>${view.node && html`<span class="pk-sep" aria-hidden="true">›</span><b class="pk-run-node">${view.node.label}</b>`}<${ProcessStepStrip} run=${run} />`;
  if (typeof onOpen === 'function') {
    return html`<button type="button" class=${cls} title=${view.title} aria-label=${view.title} data-process-run=${runID(run)} onClick=${(event) => { event.stopPropagation(); onOpen(run, view.node); }}>${body}</button>`;
  }
  return html`<span class=${cls} title=${view.title} data-process-run=${runID(run)}>${body}</span>`;
}

// ProcessRunIcon is the terminal-overlay icon: a circle whose ring carries
// the run state (blue arc running, amber blocked on human, dashed planned,
// purple agent-scoped) plus this performer's `i/n` step badge.
export function ProcessRunIcon({ run, performerId = '', onOpen = null, active = false, small = false }) {
  const view = runIconView(run, performerId);
  const cls = `pk-run-icon pk-state-${view.state}${view.scope ? ` pk-scope-${view.scope}` : ''}${active ? ' on' : ''}${small ? ' small' : ''}`;
  const body = html`<span class="pk-run-icon-glyph" aria-hidden="true">${view.glyph}</span>${!small && view.badge && html`<span class="pk-run-icon-badge">${view.badge}</span>`}`;
  if (typeof onOpen === 'function') {
    return html`<button type="button" class=${cls} title=${view.title} aria-label=${view.title} aria-pressed=${active ? 'true' : 'false'} data-process-run=${runID(run)} onClick=${(event) => { event.stopPropagation(); onOpen(run, view.node); }}>${body}</button>`;
  }
  return html`<span class=${cls} role="img" title=${view.title} aria-label=${view.title} data-process-run=${runID(run)}>${body}</span>`;
}

const ARROW_IDS = { done: 'done', running: 'running', planned: 'planned', skipped: 'planned' };

// ProcessMiniGraph renders a projected run read-only. `performerId` marks the
// "you are here" node(s); `onNodeClick` turns nodes into buttons and
// `selectedId` marks the one an inspector beside the graph describes.
export function ProcessMiniGraph({ run, compact = false, performerId = '', onNodeClick = null, selectedId = '', wizard = false }) {
  // Marker ids are per instance: the same run can be drawn on two surfaces
  // in one document, and a hidden copy must not own the visible arrowheads.
  const prefix = `pk-ah-${useId().replace(/[^A-Za-z0-9_-]/g, '_')}`;
  const layout = miniGraphLayout(run, { compact });
  if (!layout.nodes.length) return html`<div class="pk-mini-graph empty">no nodes</div>`;
  const nodes = layout.nodes.map((node) => {
    const here = performerId && performs(node, performerId);
    const selected = selectedId !== '' && node.id === String(selectedId);
    const cls = `pk-gnode pk-state-${node.state} pk-kind-${node.kind}${isTerminalKind(node.kind) ? ' terminal' : ''}${here ? ' here' : ''}${selected ? ' selected' : ''}`;
    const style = `left:${node.x}px;top:${node.y}px;width:${node.width}px;height:${node.height}px`;
    const title = `${node.label} · ${kindLabel(node.kind, wizard)} · ${stateLabel(node.state)}`;
    const inner = isTerminalKind(node.kind)
      ? html`<span class="pk-gnode-label">${node.kind}</span>`
      : node.kind === 'decision'
        ? html`<span class="pk-gnode-label">${node.label}</span>`
        : html`<${ProcessNodeGlyph} kind=${node.kind} wizard=${wizard} /><span class="pk-gnode-text"><span class="pk-gnode-label">${node.label}</span>${!compact && node.sub && html`<span class="pk-gnode-sub">${node.sub}</span>`}</span><${ProcessPerformers} performers=${node.performers} />`;
    if (typeof onNodeClick === 'function') {
      return html`<button type="button" key=${node.id} class=${cls} style=${style} title=${title} aria-label=${title} data-process-node=${node.id} aria-pressed=${selected ? 'true' : 'false'} onClick=${() => onNodeClick(node)}>${inner}</button>`;
    }
    return html`<div key=${node.id} class=${cls} style=${style} title=${title} data-process-node=${node.id}>${inner}</div>`;
  });
  return html`<div class=${`pk-mini-graph${compact ? ' compact' : ''}`} role="group" aria-label=${`${runName(run)} graph`} style=${`width:${layout.width}px;height:${layout.height}px`}>
    <svg class="pk-edges" width=${layout.width} height=${layout.height} aria-hidden="true">
      <defs>${Object.keys(ARROW_IDS).filter((k) => k !== 'skipped').map((state) => html`<marker key=${state} id=${`${prefix}-${state}`} class=${`pk-arrow ${state}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto"><path d="M0,0 L10,5 L0,10 z" /></marker>`)}</defs>
      ${layout.edges.map((edge) => html`<path key=${edge.key} class=${`pk-edge ${edge.state}${edge.back ? ' back' : ''}`} d=${edge.path} marker-end=${`url(#${prefix}-${ARROW_IDS[edge.state]})`} />`)}
    </svg>
    ${layout.edges.filter((edge) => edge.label).map((edge) => html`<span key=${`l:${edge.key}`} class="pk-elabel" style=${`left:${edge.labelX}px;top:${edge.labelY}px`}>${edge.label}</span>`)}
    ${nodes}
  </div>`;
}
