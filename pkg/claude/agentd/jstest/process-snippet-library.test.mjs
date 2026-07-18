import test from 'node:test';
import assert from 'node:assert/strict';
import {
  PROCESS_SNIPPET_UNAVAILABLE, normalizeProcessSnippetLibrary,
} from '../dashboard/js/process-snippet-library.js';

const envelope = (id = 'task') => ({
  kind: 'tclaude/process-selection', version: 1,
  nodes: [{ id, node: { type: 'task', name: 'Reusable' }, position: { x: 12, y: 34 } }],
  edges: [],
});

test('custom snippet library revalidates, isolates corruption, and sorts stable identities', () => {
  const library = normalizeProcessSnippetLibrary({
    generation: 7,
    snippets: [
      { id: `psn_${'b'.repeat(32)}`, name: 'zeta', revision: 1, available: true, envelope: envelope('zeta') },
      { id: `psn_${'c'.repeat(32)}`, name: 'Broken', revision: 3, available: true, envelope: { kind: 'wrong' } },
      { id: `psn_${'a'.repeat(32)}`, name: 'Alpha', revision: 2, available: true, envelope: envelope('alpha') },
      { id: '../unsafe', name: 'Ignored', revision: 1, available: false },
    ],
  });
  assert.equal(library.generation, 7);
  assert.deepEqual(library.snippets.map((item) => item.name), ['Alpha', 'Broken', 'zeta']);
  assert.equal(library.snippets[0].available, true);
  assert.equal(library.snippets[0].payload.nodes[0].id, 'alpha');
  assert.equal(library.snippets[1].available, false);
  assert.equal(library.snippets[1].payload, null);
  assert.equal(library.snippets[1].unavailableReason, PROCESS_SNIPPET_UNAVAILABLE);
  assert.equal(Object.hasOwn(library.snippets[1], 'envelope'), false,
    'rejected raw payload is never retained in client state');
});

test('unavailable server rows remain manageable without accepting a payload', () => {
  const library = normalizeProcessSnippetLibrary({ snippets: [{
    id: `psn_${'d'.repeat(32)}`, name: 'Legacy', revision: 9,
    available: false, unavailableReason: 'raw server detail', envelope: envelope(),
  }] });
  assert.equal(library.snippets[0].id, `psn_${'d'.repeat(32)}`);
  assert.equal(library.snippets[0].revision, 9);
  assert.equal(library.snippets[0].available, false);
  assert.equal(library.snippets[0].unavailableReason, PROCESS_SNIPPET_UNAVAILABLE,
    'client uses one bounded public reason rather than echoing server/corrupt bytes');
});
