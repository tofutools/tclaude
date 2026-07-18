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
