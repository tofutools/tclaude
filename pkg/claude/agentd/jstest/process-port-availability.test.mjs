import test from 'node:test';
import assert from 'node:assert/strict';
import {
  PROCESS_PORT_IN, PROCESS_PORT_OUT, describeProcessEdge, processEdgeMutationMessage,
  processEdgePortAvailability, processNodePortAvailability, processNodePortAvailable,
  processPortUnavailableMessage,
} from '../dashboard/js/process-port-availability.js';

test('editor port availability is one closed semantic table for every node kind', () => {
  const expected = {
    start: { in: false, out: true },
    end: { in: true, out: false },
    task: { in: true, out: true },
    decision: { in: true, out: true },
    parallel: { in: true, out: true },
    wait: { in: true, out: true },
  };
  for (const [type, ports] of Object.entries(expected)) {
    assert.deepEqual(processNodePortAvailability({ type }), ports, type);
    assert.equal(processNodePortAvailable({ type }, PROCESS_PORT_IN), ports.in, `${type}/in`);
    assert.equal(processNodePortAvailable({ type }, PROCESS_PORT_OUT), ports.out, `${type}/out`);
  }
  assert.equal(processNodePortAvailable(null, 'in'), false);
  assert.equal(processNodePortAvailable({ type: 'task' }, 'sideways'), false);
});

test('edge endpoint availability names the exact unavailable semantic side', () => {
  assert.deepEqual(processEdgePortAvailability({ type: 'task' }, { type: 'end' }), {
    enabled: true, port: '', message: '',
  });
  assert.deepEqual(processEdgePortAvailability({ type: 'end' }, { type: 'task' }), {
    enabled: false, port: 'out', message: 'End nodes cannot have outgoing connections.',
  });
  assert.deepEqual(processEdgePortAvailability({ type: 'task' }, { type: 'start' }), {
    enabled: false, port: 'in', message: 'Start nodes cannot have incoming connections.',
  });
  assert.equal(processPortUnavailableMessage({ type: 'start' }, 'in'),
    'Start nodes cannot have incoming connections.');
});

test('edge descriptions name endpoints and outcome without inventing missing parts', () => {
  assert.equal(describeProcessEdge({ from: 'end', outcome: 'legacy-out', to: 'ordinary' }),
    'end -> ordinary (outcome "legacy-out")');
  assert.equal(describeProcessEdge({ from: 'end', to: 'ordinary' }), 'end -> ordinary');
  assert.equal(describeProcessEdge({ from: '', to: 'start', outcome: 'start' }),
    '? -> start (outcome "start")');
  assert.equal(describeProcessEdge(null), '? -> ?');
});

test('mutation rejections keep the base sentence and add operation-specific recovery', () => {
  const base = 'End nodes cannot have outgoing connections.';
  const edge = { from: 'end', outcome: 'legacy-out', to: 'ordinary' };

  for (const operation of ['duplicate', 'paste', 'snippet', 'delete-rewire']) {
    const message = processEdgeMutationMessage(operation, edge, base);
    assert.ok(message.includes(base), `${operation} preserves the base authority sentence`);
    assert.ok(message.includes('end -> ordinary (outcome "legacy-out")'),
      `${operation} names the offending edge`);
    assert.notEqual(message, base);
  }

  // Only duplicate may claim the edge is preserved legacy topology: it
  // demonstrably exists in the loaded template. A pasted or snippet payload has
  // unknown provenance and a rewire bridge is brand new, so neither may say so.
  assert.match(processEdgeMutationMessage('duplicate', edge, base),
    /predates the current Start\/End port rules/);
  for (const operation of ['paste', 'snippet', 'delete-rewire']) {
    assert.doesNotMatch(processEdgeMutationMessage(operation, edge, base), /predates/,
      `${operation} must not assert provenance it cannot know`);
  }
  const rewire = processEdgeMutationMessage('delete-rewire', edge, base);
  assert.match(rewire, /Rewiring has to build that connection anew/);
  // The recovery names the delete dialog's own choice label verbatim.
  assert.match(rewire, /Choose "Delete \+ drop edges" instead/);
  // Duplicate refuses selections containing edges, so the guidance must point
  // at the endpoint nodes rather than at deselecting the edge itself.
  const duplicate = processEdgeMutationMessage('duplicate', edge, base);
  assert.match(duplicate, /Deselect one of its endpoint nodes, or delete the edge first\./);
  assert.doesNotMatch(duplicate, /Deselect (or delete )?that edge/);
  assert.match(processEdgeMutationMessage('paste', edge, base), /Copy the selection again without that edge/);
  // Snippet insertion is not a paste: never tell that operator to re-copy.
  const snippet = processEdgeMutationMessage('snippet', edge, base);
  assert.match(snippet, /This snippet cannot be inserted because of the edge/);
  assert.match(snippet, /Re-save the snippet from a selection that omits that edge\./);
  assert.doesNotMatch(snippet, /Paste|Copy the selection again/);
  // Guidance composes cleanly with an absent cause: no double colon, no double
  // space, no dangling separator where the omitted clause used to sit.
  for (const operation of ['duplicate', 'paste', 'snippet', 'delete-rewire']) {
    const message = processEdgeMutationMessage(operation, edge, base);
    assert.doesNotMatch(message, / {2}/, `${operation} has no doubled spaces`);
    assert.equal(message.split(':').length, 2, `${operation} reads as one colon-introduced clause`);
    assert.equal(message.trim(), message);
  }

  // Unknown and absent operations fall back to the bare authority sentence, so
  // addEdge/setEdgeTarget/chooser surfaces stay exactly as they were.
  assert.equal(processEdgeMutationMessage('add-edge', edge, base), base);
  assert.equal(processEdgeMutationMessage(undefined, edge, base), base);
  // Inherited Object.prototype keys must not resolve to guidance.
  assert.equal(processEdgeMutationMessage('toString', edge, base), base);
  assert.equal(processEdgeMutationMessage('constructor', edge, base), base);
});
