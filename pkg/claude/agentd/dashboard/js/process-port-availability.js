// process-port-availability.js -- the process editor's semantic authority for
// connector presence and newly authored edge endpoints. The shared graph core
// consumes the editor's projected availability metadata, while the viewer
// deliberately omits that metadata and retains its existing default DOM.

export const PROCESS_PORT_IN = 'in';
export const PROCESS_PORT_OUT = 'out';

export function processNodePortAvailable(node, port) {
  if (!node || (port !== PROCESS_PORT_IN && port !== PROCESS_PORT_OUT)) return false;
  if (node.type === 'start') return port === PROCESS_PORT_OUT;
  if (node.type === 'end') return port === PROCESS_PORT_IN;
  return true;
}

export function processNodePortAvailability(node) {
  return Object.freeze({
    in: processNodePortAvailable(node, PROCESS_PORT_IN),
    out: processNodePortAvailable(node, PROCESS_PORT_OUT),
  });
}

export function processPortUnavailableMessage(node, port) {
  if (node?.type === 'start' && port === PROCESS_PORT_IN) {
    return 'Start nodes cannot have incoming connections.';
  }
  if (node?.type === 'end' && port === PROCESS_PORT_OUT) {
    return 'End nodes cannot have outgoing connections.';
  }
  return 'This connector is not available.';
}

// Mutations that copy or synthesize an edge need more than the bare port
// sentence. Templates authored before Start/End became single-sided still load
// with edges like `ordinary -> Start`; those render, save, and delete fine, but
// no mutation may re-create one. Naming the offending edge and the way out
// keeps the (correct) wholesale rejection from reading as a dead end.
const PROCESS_EDGE_MUTATION_GUIDANCE = new Map([
  ['duplicate', Object.freeze({
    subject: 'Duplicate cannot copy the edge',
    cause: 'That edge predates the current Start/End port rules, so it can be kept but not re-created.',
    recovery: 'Deselect or delete that edge, then duplicate the remaining nodes.',
  })],
  ['paste', Object.freeze({
    subject: 'Paste cannot re-create the edge',
    cause: 'That edge predates the current Start/End port rules, so it can be kept but not re-created.',
    recovery: 'Copy the selection again without that edge, or delete the edge first.',
  })],
  ['delete-rewire', Object.freeze({
    subject: 'Delete with rewire cannot re-create the edge',
    cause: 'Rewiring has to build that connection anew, which the current Start/End port rules forbid.',
    recovery: 'Delete without rewiring instead, then reconnect the remaining nodes by hand.',
  })],
]);

export function describeProcessEdge(edge) {
  const label = `${edge?.from || '?'} -> ${edge?.to || '?'}`;
  return edge?.outcome ? `${label} (outcome "${edge.outcome}")` : label;
}

// processEdgeMutationMessage wraps a port-availability rejection in
// operation-specific recovery guidance. Unknown operations fall back to the
// bare sentence, so callers that have nothing better to say stay unchanged.
export function processEdgeMutationMessage(operation, edge, message) {
  const guidance = PROCESS_EDGE_MUTATION_GUIDANCE.get(operation);
  if (!guidance) return message;
  return `${guidance.subject} ${describeProcessEdge(edge)}: ${message} ${guidance.cause} ${guidance.recovery}`;
}

export function processEdgePortAvailability(fromNode, toNode) {
  if (!processNodePortAvailable(fromNode, PROCESS_PORT_OUT)) {
    return Object.freeze({
      enabled: false,
      port: PROCESS_PORT_OUT,
      message: processPortUnavailableMessage(fromNode, PROCESS_PORT_OUT),
    });
  }
  if (!processNodePortAvailable(toNode, PROCESS_PORT_IN)) {
    return Object.freeze({
      enabled: false,
      port: PROCESS_PORT_IN,
      message: processPortUnavailableMessage(toNode, PROCESS_PORT_IN),
    });
  }
  return Object.freeze({ enabled: true, port: '', message: '' });
}
