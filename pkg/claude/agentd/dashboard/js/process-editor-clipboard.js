// process-editor-clipboard.js -- pure, bounded clipboard envelope for process
// graph selections. The browser controller owns ClipboardEvent I/O; this file
// only creates, serializes, parses, and validates plain data so Node tests can
// exercise the exact code shipped to the dashboard.

import { selectionItems } from './process-selection.js';

export const PROCESS_CLIPBOARD_SENTINEL = 'tclaude-process-selection:';
export const PROCESS_CLIPBOARD_PREFIX = `${PROCESS_CLIPBOARD_SENTINEL}v1\n`;
export const PROCESS_CLIPBOARD_KIND = 'tclaude/process-selection';
export const PROCESS_CLIPBOARD_VERSION = 1;
export const PROCESS_CLIPBOARD_MAX_BYTES = 256 * 1024;
export const PROCESS_CLIPBOARD_MAX_NODES = 2048;
export const PROCESS_CLIPBOARD_MAX_EDGES = 4096;
export const PROCESS_CLIPBOARD_MAX_ID = 128;
export const PROCESS_CLIPBOARD_MAX_OUTCOME = 512;
export const PROCESS_CLIPBOARD_MAX_COORDINATE = 1_000_000;

const PROCESS_NODE_TYPES = new Set(['task', 'decision', 'parallel', 'wait', 'start', 'end']);
const NODE_ID = /^[a-z0-9][a-z0-9._-]*$/;
const MAX_JSON_DEPTH = 32;
const MAX_JSON_ITEMS = 32_768;

export class ProcessClipboardError extends Error {
  constructor(code, message) {
    super(message);
    this.name = 'ProcessClipboardError';
    this.code = code;
  }
}

function reject(code, message) {
  throw new ProcessClipboardError(code, message);
}

function utf8Bytes(value) {
  return new TextEncoder().encode(String(value)).length;
}

function isRecord(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function hasOnlyKeys(value, keys) {
  const allowed = new Set(keys);
  return Object.keys(value).every((key) => allowed.has(key));
}

function validateJSONValue(value) {
  let seen = 0;
  const walk = (candidate, depth) => {
    seen += 1;
    if (seen > MAX_JSON_ITEMS || depth > MAX_JSON_DEPTH) {
      reject('node_shape', 'Clipboard node data exceeds the supported structure limits.');
    }
    if (candidate == null || typeof candidate === 'string' || typeof candidate === 'boolean') return;
    if (typeof candidate === 'number') {
      if (!Number.isFinite(candidate)) reject('node_shape', 'Clipboard node data contains an invalid number.');
      return;
    }
    if (Array.isArray(candidate)) {
      for (const item of candidate) walk(item, depth + 1);
      return;
    }
    if (!isRecord(candidate)) reject('node_shape', 'Clipboard node data has an unsupported value.');
    for (const [key, item] of Object.entries(candidate)) {
      if (/\p{Cc}/u.test(key)) reject('node_shape', 'Clipboard node data has an invalid field name.');
      walk(item, depth + 1);
    }
  };
  walk(value, 0);
}

function validateNodeID(value) {
  return typeof value === 'string' && value.length > 0
    && value.length <= PROCESS_CLIPBOARD_MAX_ID && NODE_ID.test(value);
}

function validateOutcome(value) {
  return typeof value === 'string' && value.length > 0
    && value.length <= PROCESS_CLIPBOARD_MAX_OUTCOME && !/[\u0000-\u001f\u007f]/.test(value);
}

function canonicalPayloadSize(payload) {
  let encoded;
  try {
    encoded = JSON.stringify(payload);
  } catch {
    reject('format', 'Clipboard selection is not valid JSON data.');
  }
  if (utf8Bytes(PROCESS_CLIPBOARD_PREFIX + encoded) > PROCESS_CLIPBOARD_MAX_BYTES) {
    reject('limit', 'Clipboard selection exceeds the 256 KiB editor limit.');
  }
  return encoded;
}

// validateProcessSelectionPayload is the single canonical envelope validator.
// Both parse preflight and the model mutation call it; it returns a detached,
// stable-order value so later insertion cannot observe caller-owned mutation.
export function validateProcessSelectionPayload(payload) {
  if (!isRecord(payload) || !hasOnlyKeys(payload, ['kind', 'version', 'nodes', 'edges'])) {
    reject('format', 'Clipboard selection has an invalid envelope.');
  }
  if (payload.kind !== PROCESS_CLIPBOARD_KIND || payload.version !== PROCESS_CLIPBOARD_VERSION) {
    reject('version', 'Clipboard selection uses an unsupported format version.');
  }
  if (!Array.isArray(payload.nodes) || !Array.isArray(payload.edges)) {
    reject('format', 'Clipboard selection has invalid node or edge lists.');
  }
  if (!payload.nodes.length) reject('empty', 'Clipboard selection does not contain any nodes.');
  if (payload.nodes.length > PROCESS_CLIPBOARD_MAX_NODES || payload.edges.length > PROCESS_CLIPBOARD_MAX_EDGES) {
    reject('limit', 'Clipboard selection exceeds the process graph limits.');
  }
  // Bound direct callers before detailed recursive validation. Parser callers
  // already received the equivalent raw-text gate before JSON.parse.
  canonicalPayloadSize(payload);

  const nodeIDs = new Set();
  const nodes = payload.nodes.map((entry) => {
    if (!isRecord(entry) || !hasOnlyKeys(entry, ['id', 'node', 'position'])
        || !validateNodeID(entry.id) || !isRecord(entry.node)
        || !isRecord(entry.position) || !hasOnlyKeys(entry.position, ['x', 'y'])) {
      reject('node', 'Clipboard selection contains an invalid node record.');
    }
    if (nodeIDs.has(entry.id)) reject('duplicate_node', 'Clipboard selection contains duplicate node IDs.');
    nodeIDs.add(entry.id);
    if (!PROCESS_NODE_TYPES.has(entry.node.type)) {
      reject('node_type', 'Clipboard selection contains an unsupported node type.');
    }
    // Topology is carried only by the bounded edge list. A nested `next`
    // would be a second, potentially stale source of node references.
    if (Object.hasOwn(entry.node, 'next')) {
      reject('topology', 'Clipboard selection contains unsupported nested topology data.');
    }
    validateJSONValue(entry.node);
    const { x, y } = entry.position;
    if (!Number.isFinite(x) || !Number.isFinite(y)
        || Math.abs(x) > PROCESS_CLIPBOARD_MAX_COORDINATE
        || Math.abs(y) > PROCESS_CLIPBOARD_MAX_COORDINATE) {
      reject('position', 'Clipboard selection contains an invalid node position.');
    }
    return { id: entry.id, node: structuredClone(entry.node), position: { x, y } };
  }).sort((left, right) => left.id.localeCompare(right.id, 'en'));

  const edgeKeys = new Set();
  const edges = payload.edges.map((edge) => {
    if (!isRecord(edge) || !hasOnlyKeys(edge, ['from', 'outcome', 'to'])
        || !validateNodeID(edge.from) || !validateNodeID(edge.to) || !validateOutcome(edge.outcome)) {
      reject('edge', 'Clipboard selection contains an invalid edge record.');
    }
    if (!nodeIDs.has(edge.from) || !nodeIDs.has(edge.to)) {
      reject('reference', 'Clipboard selection contains an edge with a missing endpoint.');
    }
    const key = `${edge.from}\u0000${edge.outcome}`;
    if (edgeKeys.has(key)) {
      reject('duplicate_edge', 'Clipboard selection contains duplicate edge outcomes.');
    }
    edgeKeys.add(key);
    return { from: edge.from, outcome: edge.outcome, to: edge.to };
  }).sort((left, right) => JSON.stringify([left.from, left.outcome, left.to])
    .localeCompare(JSON.stringify([right.from, right.outcome, right.to]), 'en'));

  const canonical = { kind: PROCESS_CLIPBOARD_KIND, version: PROCESS_CLIPBOARD_VERSION, nodes, edges };
  canonicalPayloadSize(canonical);
  return canonical;
}

export function createProcessSelectionPayload(model, selection, positions = []) {
  const selectedIDs = [...new Set(selectionItems(selection)
    .filter((item) => item?.type === 'node' && model.node(item.id))
    .map((item) => item.id))].sort((left, right) => left.localeCompare(right, 'en'));
  if (!selectedIDs.length) return null;
  if (selectedIDs.length > PROCESS_CLIPBOARD_MAX_NODES) {
    reject('limit', 'Clipboard selection exceeds the process graph limits.');
  }
  const selected = new Set(selectedIDs);
  const positionByID = new Map((positions || []).map((position) => [position.id, position]));
  const nodes = selectedIDs.map((id) => {
    const node = structuredClone(model.node(id));
    // Edges are the normalized editor truth; never export a stale alias.
    delete node.next;
    const laid = positionByID.get(id) || model.layout?.nodes?.[id];
    return { id, node, position: { x: laid?.x, y: laid?.y } };
  });
  const edges = (model.edges || []).filter((edge) => edge.from && selected.has(edge.from) && selected.has(edge.to))
    .map(({ from, outcome, to }) => ({ from, outcome, to }));
  return validateProcessSelectionPayload({
    kind: PROCESS_CLIPBOARD_KIND,
    version: PROCESS_CLIPBOARD_VERSION,
    nodes,
    edges,
  });
}

export function serializeProcessSelection(payload) {
  const canonical = validateProcessSelectionPayload(payload);
  return PROCESS_CLIPBOARD_PREFIX + canonicalPayloadSize(canonical);
}

export function isProcessSelectionClipboardText(text) {
  return typeof text === 'string' && text.startsWith(PROCESS_CLIPBOARD_SENTINEL);
}

export function parseProcessSelection(text) {
  if (!isProcessSelectionClipboardText(text)) return null;
  if (utf8Bytes(text) > PROCESS_CLIPBOARD_MAX_BYTES) {
    reject('limit', 'Clipboard selection exceeds the 256 KiB editor limit.');
  }
  if (!text.startsWith(PROCESS_CLIPBOARD_PREFIX)) {
    reject('version', 'Clipboard selection uses an unsupported format version.');
  }
  let payload;
  try {
    payload = JSON.parse(text.slice(PROCESS_CLIPBOARD_PREFIX.length));
  } catch {
    reject('format', 'Clipboard selection is not valid JSON data.');
  }
  return validateProcessSelectionPayload(payload);
}

// Placement repeat state stores only this bounded digest, never raw clipboard
// bytes. A collision can at worst change the visual cascade offset.
export function processSelectionFingerprint(text) {
  let hash = 0x811c9dc5;
  for (let index = 0; index < text.length; index += 1) {
    hash ^= text.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193);
  }
  return `${text.length}:${(hash >>> 0).toString(16).padStart(8, '0')}`;
}
