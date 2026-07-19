// process-connection-feedback.js -- pure semantic feedback for process-editor
// connector gestures. The graph widget owns presentation and timing; this
// module owns which actions the edit model can actually perform.

import {
  processEdgePortAvailability, processNodePortAvailable, processPortUnavailableMessage,
} from './process-port-availability.js';
import { PASS_OUTCOMES, UNNAMED_OUTCOME } from './process-outcome-vocabulary.js';

function nodeLabel(model, id) {
  const node = model?.node?.(id);
  return String(node?.name || id || 'node');
}

function sourceInstruction(model, source) {
  const label = nodeLabel(model, source.nodeId);
  return source.port === 'in'
    ? `Drag from ${label}'s input to connect a predecessor.`
    : `Drag from ${label}'s output to connect a successor.`;
}

function disabled(message) {
  return { state: 'disabled', enabled: false, message };
}

function invalid(message) {
  return { state: 'invalid', enabled: false, message };
}

function valid(message, connection) {
  return { state: 'valid', enabled: true, message, ...(connection || {}) };
}

// Build the free-outcome lookup once for a rendered connection gesture. The
// edit model's freeOutcome method intentionally scans outgoing edges; doing
// that independently for every node/body/port target would turn the supported
// 2,048-node / 4,096-edge authoring bound into quadratic UI work. This is the
// same first-free-pass rule, indexed in one O(E + N) pass and kept detached
// from both the model and the resolver request.
export function prepareProcessConnectionFeedback(model) {
  const takenByFrom = new Map();
  for (const edge of model?.edges || []) {
    if (!takenByFrom.has(edge.from)) takenByFrom.set(edge.from, new Set());
    takenByFrom.get(edge.from).add(edge.outcome);
  }
  const freeOutcomeByFrom = new Map();
  const nodes = model?.template?.nodes || {};
  for (const from of Object.keys(nodes)) {
    const taken = takenByFrom.get(from) || new Set();
    // Mirrors ProcessEditModel.newEdgeOutcome — the drag preview and the
    // read-only edgeEditable gate must evaluate the same edge the drop will
    // actually create. See that method for why a lone pass edge pairs with
    // 'fail' rather than a second pass-vocabulary name.
    let base = 'pass';
    const type = nodes[from]?.type;
    if (type !== 'decision') {
      if (!taken.size) {
        freeOutcomeByFrom.set(from, UNNAMED_OUTCOME);
        continue;
      }
      if (type === 'task' && taken.size === 1 && PASS_OUTCOMES.includes([...taken][0])) base = 'fail';
    }
    let outcome = base;
    for (let n = 2; taken.has(outcome); n += 1) outcome = `${base}-${n}`;
    freeOutcomeByFrom.set(from, outcome);
  }
  return Object.freeze({
    freeOutcome(from) { return freeOutcomeByFrom.get(from); },
  });
}

export function resolveProcessConnectionFeedback(model, request = {}, prepared = null) {
  const source = request.source || {};
  const sourceNode = model?.node?.(source.nodeId);
  if (!sourceNode || (source.port !== 'in' && source.port !== 'out')) {
    return disabled('This connector is not available.');
  }
  if (!processNodePortAvailable(sourceNode, source.port)) {
    return disabled(processPortUnavailableMessage(sourceNode, source.port));
  }
  if (request.phase === 'source') {
    return { state: 'available', enabled: true, message: sourceInstruction(model, source) };
  }

  const candidate = request.candidate || {};
  if (candidate.emptyCanvas) {
    if (model?.config?.canInsert === false) {
      return invalid('Adding connected nodes is not allowed in this view.');
    }
    return valid(source.port === 'in'
      ? 'Drop here to choose a new predecessor node.'
      : 'Drop here to choose a new successor node.');
  }
  if (!candidate.nodeId || !model?.node?.(candidate.nodeId)) {
    return { state: 'neutral', enabled: false, message: '' };
  }
  if (candidate.nodeId === source.nodeId) {
    if (candidate.port === source.port) {
      return { state: 'source', enabled: true, message: sourceInstruction(model, source) };
    }
    if (!candidate.port) {
      return invalid('Choose a different node; dropping on the source node does not create a connection.');
    }
    return invalid('Self-loop connections are not supported because v1 processes are acyclic.');
  }
  if (source.port === 'in' && candidate.port === 'in') {
    return invalid('Connect this input to an output port or another node body.');
  }

  const from = source.port === 'in' ? candidate.nodeId : source.nodeId;
  const to = source.port === 'in' ? source.nodeId : candidate.nodeId;
  const endpointAvailability = processEdgePortAvailability(model.node(from), model.node(to));
  if (!endpointAvailability.enabled) {
    return invalid(endpointAvailability.message);
  }
  const outcome = prepared?.freeOutcome?.(from) || model.freeOutcome?.(from, 'pass') || 'pass';
  const prospective = { from, outcome, to };
  if (model?.config?.edgeEditable && !model.config.edgeEditable(prospective)) {
    return invalid('This connection is read-only in this view.');
  }
  return valid(`Drop to connect ${nodeLabel(model, from)} to ${nodeLabel(model, to)}.`, { from, to });
}
