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
const PROCESS_PERFORMER_KINDS = new Set(['human', 'agent', 'program']);
const NODE_ID = /^[a-z0-9][a-z0-9._-]*$/;
const MAX_JSON_DEPTH = 32;
const MAX_JSON_ITEMS = 32_768;
const PROTOTYPE_KEYS = new Set(['__proto__', 'prototype', 'constructor']);
const NODE_FIELDS = new Set([
  'type', 'join', 'name', 'description', 'doc', 'performer', 'plan', 'checks',
  'review', 'retry', 'wait', 'next', 'result', 'captures', 'metadata',
]);
const STEP_FIELDS = new Set([
  'id', 'name', 'description', 'doc', 'performer', 'approval', 'approvalRetry', 'retry',
]);
const PERFORMER_FIELDS = new Set([
  'kind', 'profile', 'prompt', 'ask', 'choices', 'choiceOutcomes', 'assignee',
  'model', 'effort', 'run', 'args', 'timeout', 'contact',
]);
const CONTACT_FIELDS = new Set(['cadence', 'budget', 'escalationTarget']);
const RETRY_FIELDS = new Set(['maxAttempts', 'backoff', 'onFail']);
const WAIT_FIELDS = new Set(['duration', 'until', 'signal']);
const FAIL_OUTCOMES = ['fail', 'failed', 'failure', 'error'];

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

function wireShapeReject() {
  reject('node_shape', 'Clipboard selection contains incompatible process node data.');
}

function validateWireRecord(value, fields) {
  if (!isRecord(value)) wireShapeReject();
  for (const key of Object.keys(value)) {
    if (PROTOTYPE_KEYS.has(key) || !fields.has(key)) wireShapeReject();
  }
}

function validateOptionalStrings(value, fields) {
  for (const field of fields) {
    if (Object.hasOwn(value, field) && typeof value[field] !== 'string') wireShapeReject();
  }
}

function validateStringList(value) {
  if (!Array.isArray(value) || value.some((item) => typeof item !== 'string')) wireShapeReject();
}

function validateStringMap(value) {
  if (!isRecord(value)) wireShapeReject();
  for (const [key, item] of Object.entries(value)) {
    if (PROTOTYPE_KEYS.has(key) || typeof item !== 'string') wireShapeReject();
  }
}

function validateRetry(value) {
  validateWireRecord(value, RETRY_FIELDS);
  if (Object.hasOwn(value, 'maxAttempts') && !Number.isSafeInteger(value.maxAttempts)) wireShapeReject();
  validateOptionalStrings(value, ['backoff', 'onFail']);
}

function validateContact(value) {
  validateWireRecord(value, CONTACT_FIELDS);
  if (Object.hasOwn(value, 'budget') && !Number.isSafeInteger(value.budget)) wireShapeReject();
  validateOptionalStrings(value, ['cadence', 'escalationTarget']);
}

function validatePerformer(value) {
  validateWireRecord(value, PERFORMER_FIELDS);
  if (!PROCESS_PERFORMER_KINDS.has(value.kind)) wireShapeReject();
  validateOptionalStrings(value, [
    'profile', 'prompt', 'ask', 'assignee', 'model', 'effort', 'run', 'timeout',
  ]);
  if (Object.hasOwn(value, 'choices')) validateStringList(value.choices);
  if (Object.hasOwn(value, 'args')) validateStringList(value.args);
  if (Object.hasOwn(value, 'choiceOutcomes')) validateStringMap(value.choiceOutcomes);
  if (Object.hasOwn(value, 'contact')) validateContact(value.contact);
}

function validateStep(value) {
  validateWireRecord(value, STEP_FIELDS);
  validateOptionalStrings(value, ['id', 'name', 'description', 'doc', 'approval']);
  if (!Object.hasOwn(value, 'performer')) wireShapeReject();
  validatePerformer(value.performer);
  if (Object.hasOwn(value, 'approvalRetry')) validateRetry(value.approvalRetry);
  if (Object.hasOwn(value, 'retry')) validateRetry(value.retry);
}

// validateProcessEditNode is the clipboard boundary's authoritative mirror of
// model.Node's strict JSON edit wire. Keep the field sets above in lockstep
// with process/model/types.go + schema.go: unlike semantic validation, shape
// incompatibility must be refused before untrusted data reaches editor state.
export function validateProcessEditNode(value) {
  validateWireRecord(value, NODE_FIELDS);
  if (!PROCESS_NODE_TYPES.has(value.type)) wireShapeReject();
  validateOptionalStrings(value, ['join', 'name', 'description', 'doc', 'result']);
  if (Object.hasOwn(value, 'performer')) validatePerformer(value.performer);
  if (Object.hasOwn(value, 'plan')) validateStep(value.plan);
  if (Object.hasOwn(value, 'checks')) {
    if (!Array.isArray(value.checks)) wireShapeReject();
    for (const check of value.checks) validateStep(check);
  }
  if (Object.hasOwn(value, 'review')) validateStep(value.review);
  if (Object.hasOwn(value, 'retry')) validateRetry(value.retry);
  if (Object.hasOwn(value, 'wait')) {
    validateWireRecord(value.wait, WAIT_FIELDS);
    validateOptionalStrings(value.wait, WAIT_FIELDS);
  }
  if (Object.hasOwn(value, 'next')) validateStringMap(value.next);
  if (Object.hasOwn(value, 'captures')) validateStringList(value.captures);
  if (Object.hasOwn(value, 'metadata')) {
    if (!isRecord(value.metadata)) wireShapeReject();
    validateJSONValue(value.metadata);
  }
  return value;
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

function isCompoundTask(node) {
  return node?.type === 'task'
    && (isRecord(node.plan) || (Array.isArray(node.checks) && node.checks.length > 0) || isRecord(node.review));
}

function firstFailTarget(edgesBySource, sourceID) {
  const outcomes = edgesBySource.get(sourceID);
  if (!outcomes) return '';
  for (const outcome of FAIL_OUTCOMES) {
    if (outcomes.has(outcome)) return outcomes.get(outcome);
  }
  return '';
}

function isSanctionedRetryEdge(edge, nodesByID, edgesBySource) {
  const decision = nodesByID.get(edge.from);
  const target = nodesByID.get(edge.to);
  return edge.outcome === 'retry' && decision?.type === 'decision'
    && decision.performer?.kind === 'human' && isCompoundTask(target)
    && firstFailTarget(edgesBySource, edge.to) === edge.from;
}

// v1 permits one engine-intercepted retry edge: a human poison-escalation
// decision may retry the compound task whose fail edge entered it. Every other
// directed cycle is incompatible with the process topology authority.
function validateProcessSelectionTopology(nodes, edges) {
  const nodesByID = new Map(nodes.map((entry) => [entry.id, entry.node]));
  const edgesBySource = new Map();
  for (const edge of edges) {
    let outcomes = edgesBySource.get(edge.from);
    if (!outcomes) {
      outcomes = new Map();
      edgesBySource.set(edge.from, outcomes);
    }
    outcomes.set(edge.outcome, edge.to);
  }
  const adjacency = new Map(nodes.map((entry) => [entry.id, []]));
  const indegree = new Map(nodes.map((entry) => [entry.id, 0]));
  for (const edge of edges) {
    if (isSanctionedRetryEdge(edge, nodesByID, edgesBySource)) continue;
    adjacency.get(edge.from).push(edge.to);
    indegree.set(edge.to, indegree.get(edge.to) + 1);
  }
  const ready = nodes.map((entry) => entry.id).filter((id) => indegree.get(id) === 0);
  let visited = 0;
  while (ready.length) {
    const id = ready.pop();
    visited += 1;
    for (const target of adjacency.get(id)) {
      const remaining = indegree.get(target) - 1;
      indegree.set(target, remaining);
      if (remaining === 0) ready.push(target);
    }
  }
  if (visited !== nodes.length) {
    reject('topology', 'Clipboard selection contains an unsupported process graph cycle.');
  }
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
    validateProcessEditNode(entry.node);
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

  validateProcessSelectionTopology(nodes, edges);

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
