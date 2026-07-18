import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  PROCESS_SNIPPET_UNAVAILABLE, normalizeProcessSnippetLibrary, validateProcessSnippetName,
} from '../dashboard/js/process-snippet-library.js';
import { validateProcessSelectionPayload } from '../dashboard/js/process-editor-clipboard.js';

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

test('snippet names share rune, UTF-8 byte, and control validation', () => {
  assert.deepEqual(validateProcessSnippetName('  🚀 Review  '), { name: '🚀 Review', error: '' });
  assert.equal(validateProcessSnippetName('🚀'.repeat(40)).error, '', '40 emoji are exactly 160 UTF-8 bytes');
  assert.match(validateProcessSnippetName('🚀'.repeat(41)).error, /160 UTF-8 bytes/);
  assert.match(validateProcessSnippetName(`bad\u0000name`).error, /control/);
});

test('shared node-wire fixtures match the shipped browser authority', () => {
  const fixtures = JSON.parse(readFileSync(new URL('./process-snippet-wire-fixtures.json', import.meta.url), 'utf8'));
  for (const fixture of fixtures.cases) {
    if (fixture.accepted) {
      assert.doesNotThrow(() => validateProcessSelectionPayload(fixture.envelope), fixture.name);
    } else {
      assert.throws(() => validateProcessSelectionPayload(fixture.envelope), undefined, fixture.name);
    }
  }
});

test('JSON.stringify keeps markup and JavaScript separators within the shared byte boundary', () => {
  const payload = envelope();
  payload.nodes[0].node.doc = '<>&'.repeat(20_000) + '\u2028\u2029' + String.raw`\u2028`;
  assert.doesNotThrow(() => validateProcessSelectionPayload(payload));
  assert.ok(new TextEncoder().encode(JSON.stringify(payload)).length < 256 * 1024);
});
