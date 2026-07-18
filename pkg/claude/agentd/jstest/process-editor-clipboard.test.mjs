import test from 'node:test';
import assert from 'node:assert/strict';
import {
  PROCESS_CLIPBOARD_KIND, PROCESS_CLIPBOARD_MAX_BYTES, PROCESS_CLIPBOARD_MAX_NODES,
  PROCESS_CLIPBOARD_PREFIX, PROCESS_CLIPBOARD_VERSION,
  createProcessSelectionPayload, isProcessSelectionClipboardText,
  parseProcessSelection, processSelectionFingerprint, serializeProcessSelection,
  validateProcessSelectionPayload,
} from '../dashboard/js/process-editor-clipboard.js';

function envelope(overrides = {}) {
  return {
    kind: PROCESS_CLIPBOARD_KIND,
    version: PROCESS_CLIPBOARD_VERSION,
    nodes: [
      { id: 'build', node: { type: 'task', performer: { kind: 'agent', profile: 'implementer' }, metadata: { owner: 'release' } }, position: { x: 10, y: 20 } },
      { id: 'review', node: { type: 'decision', performer: { kind: 'human', ask: 'Ship?' } }, position: { x: 110, y: 220 } },
    ],
    edges: [{ from: 'build', outcome: 'pass', to: 'review' }],
    ...overrides,
  };
}

test('selection payload preserves node semantics and internal topology only', () => {
  const model = {
    template: { nodes: {
      build: { type: 'task', performer: { kind: 'agent', profile: 'implementer' }, retry: { maxAttempts: 3 }, next: { pass: 'review' } },
      review: { type: 'decision', performer: { kind: 'human', ask: 'Ship?' } },
      ship: { type: 'end', result: 'success' },
    } },
    layout: { nodes: {} },
    edges: [
      { from: '', outcome: 'start', to: 'build' },
      { from: 'build', outcome: 'pass', to: 'review' },
      { from: 'review', outcome: 'ship', to: 'ship' },
    ],
    node(id) { return this.template.nodes[id]; },
  };
  const payload = createProcessSelectionPayload(model, {
    type: 'multi', items: [
      { type: 'node', id: 'review' }, { type: 'edge', from: 'review', outcome: 'ship' },
      { type: 'node', id: 'build' },
    ],
  }, [{ id: 'build', x: 10, y: 20 }, { id: 'review', x: 110, y: 220 }]);

  assert.deepEqual(payload.nodes.map((entry) => entry.id), ['build', 'review']);
  assert.deepEqual(payload.nodes[0].node.retry, { maxAttempts: 3 });
  assert.equal(payload.nodes[0].node.next, undefined, 'normalized edges are the only copied topology');
  assert.deepEqual(payload.edges, [{ from: 'build', outcome: 'pass', to: 'review' }]);
  assert.equal(JSON.stringify(payload).includes('ship'), false, 'crossing targets and unselected nodes stay out');
});

test('versioned sentinel text round-trips canonically across editor instances', () => {
  const text = serializeProcessSelection(envelope());
  assert.ok(text.startsWith(PROCESS_CLIPBOARD_PREFIX));
  assert.equal(isProcessSelectionClipboardText(text), true);
  assert.deepEqual(parseProcessSelection(text), validateProcessSelectionPayload(envelope()));
  assert.equal(parseProcessSelection('ordinary clipboard text'), null);
  assert.equal(isProcessSelectionClipboardText('ordinary clipboard text'), false);
  assert.equal(isProcessSelectionClipboardText('tclaude-process-selection:v2\n{}'), true);
  assert.throws(() => parseProcessSelection('tclaude-process-selection:v2\n{}'), /unsupported format version/);
  assert.equal(processSelectionFingerprint(text), processSelectionFingerprint(text));
  assert.notEqual(processSelectionFingerprint(text), processSelectionFingerprint(`${text} `));
});

test('validator rejects duplicate identities, outcome pairs, and missing references without echoing data', () => {
  const secret = 'raw clipboard SECRET';
  const cases = [
    [envelope({ nodes: [envelope().nodes[0], envelope().nodes[0]] }), /duplicate node IDs/],
    [envelope({ edges: [
      { from: 'build', outcome: 'pass', to: 'review' },
      { from: 'build', outcome: 'pass', to: 'build' },
    ] }), /duplicate edge outcomes/],
    [envelope({ edges: [{ from: 'build', outcome: 'pass', to: 'missing' }] }), /missing endpoint/],
    [envelope({ nodes: [{ id: secret, node: { type: 'task' }, position: { x: 0, y: 0 } }], edges: [] }), /invalid node record/],
  ];
  for (const [payload, expected] of cases) {
    assert.throws(() => validateProcessSelectionPayload(payload), (error) => {
      assert.match(error.message, expected);
      assert.equal(error.message.includes(secret), false, 'diagnostic never surfaces raw clipboard content');
      return true;
    });
  }
});

test('validator rejects stale topology/version, hostile geometry, depth, and public resource overflow', () => {
  assert.throws(() => validateProcessSelectionPayload(envelope({ version: 2 })), /unsupported format version/);
  const nestedTopology = envelope();
  nestedTopology.nodes[0].node.next = { pass: 'review' };
  assert.throws(() => validateProcessSelectionPayload(nestedTopology), /nested topology/);
  const hostilePosition = envelope();
  hostilePosition.nodes[0].position.x = Number.MAX_VALUE;
  assert.throws(() => validateProcessSelectionPayload(hostilePosition), /invalid node position/);

  let deep = 'leaf';
  for (let index = 0; index < 40; index += 1) deep = { child: deep };
  const hostileDepth = envelope();
  hostileDepth.nodes[0].node.metadata = deep;
  assert.throws(() => validateProcessSelectionPayload(hostileDepth), /structure limits/);

  const tooMany = envelope({
    nodes: Array.from({ length: PROCESS_CLIPBOARD_MAX_NODES + 1 }, (_, index) => ({
      id: `node-${index}`, node: { type: 'task' }, position: { x: index, y: 0 },
    })),
    edges: [],
  });
  assert.throws(() => validateProcessSelectionPayload(tooMany), /graph limits/);

  const tooLarge = serializeProcessSelection(envelope());
  const oversized = `${PROCESS_CLIPBOARD_PREFIX}${JSON.stringify({
    ...envelope(), nodes: [{ id: 'build', node: { type: 'task', doc: 'x'.repeat(PROCESS_CLIPBOARD_MAX_BYTES) }, position: { x: 0, y: 0 } }], edges: [],
  })}`;
  assert.ok(tooLarge.length < PROCESS_CLIPBOARD_MAX_BYTES);
  assert.throws(() => parseProcessSelection(oversized), /256 KiB/);
});
