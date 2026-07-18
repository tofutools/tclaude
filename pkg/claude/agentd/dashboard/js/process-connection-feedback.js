// process-connection-feedback.js -- pure semantic feedback for process-editor
// connector gestures. The graph widget owns presentation and timing; this
// module owns which actions the edit model can actually perform.

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

export function resolveProcessConnectionFeedback(model, request = {}) {
  const source = request.source || {};
  const sourceNode = model?.node?.(source.nodeId);
  if (!sourceNode || (source.port !== 'in' && source.port !== 'out')) {
    return disabled('This connector is not available.');
  }
  if (source.port === 'out' && sourceNode.type === 'end') {
    return disabled('End nodes cannot have outgoing connections.');
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
  if (model.node(from)?.type === 'end') {
    return invalid('End nodes cannot have outgoing connections.');
  }
  const prospective = { from, outcome: model.freeOutcome?.(from, 'pass') || 'pass', to };
  if (model?.config?.edgeEditable && !model.config.edgeEditable(prospective)) {
    return invalid('This connection is read-only in this view.');
  }
  return valid(`Drop to connect ${nodeLabel(model, from)} to ${nodeLabel(model, to)}.`, { from, to });
}
