// process-node-blocks.js -- pure block vocabulary for the docked node
// inspector (mock E3, tcl-oypy1p). No DOM, no fetch: Node's test runner
// exercises the exact file shipped to the browser
// (jstest/process-node-blocks.test.mjs).
//
// A node is edited as a stack of blocks. Each block owns a slice of the node
// model and is either REQUIRED for the node kind (always shown, cannot be
// removed) or OPTIONAL (shown when its slice carries data or when the author
// added it this session; removing it clears its slice). Block order is a
// per-kind UI preference, never part of the template.
//
// Blocks map onto the existing model only. The mock's richer surfaces
// (group role resolution, scope chips, budget/permission bounds) wait on the
// scope model (tcl-fboivp) and execution grants (tcl-09b60a); until those
// land, Bounds edits what the model has today: the performer timeout and its
// contact schedule.

const TASK_PROMPT_KINDS = ['agent', 'human'];

function performerHasPrompt(node) {
  return !!(node.performer?.prompt || node.performer?.ask);
}

function clearPerformerKeys(node, keys) {
  if (!node.performer) return;
  for (const key of keys) delete node.performer[key];
}

export const NODE_BLOCKS = [
  {
    key: 'performer', label: 'Performer', icon: '👥',
    kinds: ['task', 'decision'], required: true,
    hint: 'Who performs this node: kind, profile and kind-specific settings',
  },
  {
    key: 'prompt', label: 'Prompt', icon: '📜',
    kinds: ['task', 'decision'],
    hint: 'Instruction text for an agent, or the question put to a human',
    // A program performer has no prompt: its command lives in Performer.
    available: (node) => TASK_PROMPT_KINDS.includes(node.performer?.kind || (node.type === 'decision' ? 'human' : 'agent')),
    present: performerHasPrompt,
    clear: (node) => clearPerformerKeys(node, ['prompt', 'ask']),
  },
  {
    key: 'stages', label: 'Work stages', icon: '⟴',
    kinds: ['task'],
    hint: 'Optional plan before work, checks after it, and a review gate',
    present: (node) => !!(node.plan || node.review || node.checks?.length),
    clear: (node) => { delete node.plan; delete node.review; delete node.checks; },
  },
  {
    key: 'inputs', label: 'Inputs', icon: '⇥',
    kinds: ['task', 'decision'],
    hint: 'What this node receives: template parameters and upstream captures',
    // Informational: derived from the graph, so it has nothing to clear.
    present: (node, context) => (context?.inputs?.captures?.length || 0) > 0,
  },
  {
    key: 'retry', label: 'Retry', icon: '↻',
    kinds: ['task'],
    hint: 'How many attempts the work gets and how a failed attempt restarts',
    present: (node) => !!node.retry,
    clear: (node) => { delete node.retry; },
  },
  {
    key: 'captures', label: 'Outputs', icon: '⇤',
    kinds: ['task'],
    hint: 'Named outputs this node publishes for downstream nodes',
    present: (node) => !!node.captures?.length,
    clear: (node) => { delete node.captures; },
  },
  {
    key: 'wait', label: 'Wait', icon: '⏱',
    kinds: ['wait'], required: true,
    hint: 'Pause for a duration, until a time, or for a signal',
  },
  {
    key: 'subprocess', label: 'Subgraph', icon: '▣',
    kinds: ['subprocess'], required: true,
    hint: 'The nested graph (inline, or a template reference) and its in/out ports',
  },
  {
    key: 'outcomes', label: 'Outcomes', icon: '⇉',
    kinds: ['task', 'decision', 'parallel', 'wait', 'start', 'subprocess'], required: true,
    hint: 'Outgoing edges: which outcome leads where',
  },
  {
    key: 'bounds', label: 'Bounds', icon: '🛡',
    kinds: ['task', 'decision'],
    hint: 'Performer timeout and contact schedule (nudge cadence, budget, escalation)',
    present: (node) => !!(node.performer?.timeout || node.performer?.contact),
    clear: (node) => clearPerformerKeys(node, ['timeout', 'contact']),
  },
  {
    key: 'about', label: 'Notes', icon: '✎',
    kinds: ['task', 'decision', 'parallel', 'wait', 'start', 'end', 'subprocess'],
    hint: 'Description and longer documentation for this node',
    present: (node) => !!(node.description || node.doc),
    clear: (node) => { delete node.description; delete node.doc; },
  },
];

const BLOCKS_BY_KEY = new Map(NODE_BLOCKS.map((block) => [block.key, block]));

export function nodeBlock(key) {
  return BLOCKS_BY_KEY.get(key) || null;
}

function nodeKind(node) {
  return node?.type || 'task';
}

// blocksForKind lists the blocks valid for a node kind in default order.
export function blocksForKind(kind) {
  return NODE_BLOCKS.filter((block) => block.kinds.includes(kind));
}

function blockAvailable(block, node) {
  return !block.available || block.available(node);
}

function blockPresent(block, node, context) {
  return !!block.required || !!block.present?.(node, context);
}

// orderKeys sorts block keys by a stored preference. Unknown or stale entries
// in the preference are ignored; keys missing from it keep their default
// relative order after the preferred ones, so a block added in a later
// release still appears rather than vanishing from a saved order.
export function orderKeys(keys, preference = []) {
  const rank = new Map();
  for (const key of Array.isArray(preference) ? preference : []) {
    if (typeof key === 'string' && !rank.has(key)) rank.set(key, rank.size);
  }
  const defaults = new Map(keys.map((key, index) => [key, index]));
  return [...keys].sort((a, b) => {
    const ra = rank.has(a) ? rank.get(a) : rank.size + defaults.get(a);
    const rb = rank.has(b) ? rank.get(b) : rank.size + defaults.get(b);
    return ra - rb;
  });
}

// visibleBlocks resolves the stack the inspector renders for one node:
// required blocks, optional blocks whose slice carries data, and blocks the
// author added this session (`opened`), filtered to what the kind and the
// current performer allow, in the preferred order.
export function visibleBlocks(node, { opened = [], order = [], context = {} } = {}) {
  const kind = nodeKind(node);
  const openSet = new Set(opened);
  const keys = blocksForKind(kind)
    .filter((block) => blockAvailable(block, node))
    .filter((block) => blockPresent(block, node, context) || openSet.has(block.key))
    .map((block) => block.key);
  return orderKeys(keys, order);
}

// addableBlocks is what "+ add block" offers: blocks valid for this kind and
// performer that are not already in the stack.
export function addableBlocks(node, visible) {
  const shown = new Set(visible);
  return blocksForKind(nodeKind(node))
    .filter((block) => !block.required && blockAvailable(block, node) && !shown.has(block.key));
}

// removeBlock clears an optional block's slice of a DRAFT node (inside
// ProcessEditModel.updateNode). Required blocks cannot be removed.
export function removeBlock(node, key) {
  const block = nodeBlock(key);
  if (!block) throw new Error(`unknown block ${key}`);
  if (block.required) throw new Error(`${block.label} is required for this node`);
  block.clear?.(node);
}

// moveKey returns the visible order with `key` moved by delta (±1) among the
// visible keys. Out-of-range moves return the order unchanged.
export function moveKey(visible, key, delta) {
  const index = visible.indexOf(key);
  const target = index + delta;
  if (index < 0 || target < 0 || target >= visible.length) return [...visible];
  const next = [...visible];
  next.splice(index, 1);
  next.splice(target, 0, key);
  return next;
}

// moveKeyBefore drops `key` immediately before `before` (drag and drop). A
// null/unknown `before` moves it to the end.
export function moveKeyBefore(visible, key, before) {
  if (!visible.includes(key) || key === before) return [...visible];
  const next = visible.filter((candidate) => candidate !== key);
  const index = next.indexOf(before);
  if (index < 0) next.push(key);
  else next.splice(index, 0, key);
  return next;
}

// mergeOrder folds a reordered visible subset back into the full stored
// preference for the kind, so hidden blocks keep their remembered slots.
export function mergeOrder(kind, preference, visibleOrder) {
  const all = orderKeys(blocksForKind(kind).map((block) => block.key), preference);
  const visibleSet = new Set(visibleOrder);
  const queue = [...visibleOrder];
  return all.map((key) => (visibleSet.has(key) ? queue.shift() : key));
}

// upstreamInputs summarizes what a node receives: the captures published by
// its direct predecessors plus the template's declared parameters. Captures
// are names only in v1 (model/types.go), so this is a read-only listing.
export function upstreamInputs(model, nodeId) {
  const captures = [];
  const seen = new Set();
  for (const edge of model.incomingEdges(nodeId)) {
    if (!edge.from) continue;
    const source = model.node(edge.from);
    for (const name of source?.captures || []) {
      const key = `${edge.from}\x00${name}`;
      if (seen.has(key)) continue;
      seen.add(key);
      captures.push({ from: edge.from, fromName: source.name || edge.from, name });
    }
  }
  const params = Object.keys(model.template?.params || {}).sort();
  return { captures, params };
}

// stageSummary renders a node's work pipeline as ordered chips for the Work
// stages block header: plan → work → checks → review.
export function stageSummary(node) {
  const stages = [];
  if (node.plan) stages.push({ key: 'plan', label: 'plan', detail: node.plan.approval === 'human' ? 'human approval' : '' });
  stages.push({ key: 'work', label: 'work', detail: node.performer?.kind || '' });
  for (const check of node.checks || []) stages.push({ key: `check:${check.id}`, label: check.id, detail: 'check' });
  if (node.review) stages.push({ key: 'review', label: 'review', detail: node.review.performer?.kind || '' });
  return stages;
}

// setSubprocessPort binds (or, with an empty value, unbinds) one in-port
// (field 'inputs': body param ← parent expression) or out-port (field
// 'outputs': body end node → parent outcome) on a ▣ node draft.
export function setSubprocessPort(node, field, key, raw) {
  if (field !== 'inputs' && field !== 'outputs') throw new Error(`unknown port field ${field}`);
  const sub = node.subprocess || (node.subprocess = {});
  const value = String(raw ?? '').trim();
  const next = { ...(sub[field] || {}) };
  if (value) next[key] = value;
  else delete next[key];
  if (Object.keys(next).length) sub[field] = next;
  else delete sub[field];
}

// setSubprocessRef edits a referenced body's templateRef / version.
export function setSubprocessRef(node, field, raw) {
  if (field !== 'templateRef' && field !== 'version') throw new Error(`unknown reference field ${field}`);
  const sub = node.subprocess || (node.subprocess = {});
  const value = String(raw ?? '').trim();
  if (field === 'templateRef' && !value) throw new Error('a referenced subprocess needs a template id');
  if (value) sub[field] = value;
  else delete sub[field];
}
