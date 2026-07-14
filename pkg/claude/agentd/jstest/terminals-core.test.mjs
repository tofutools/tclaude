import test from 'node:test';
import assert from 'node:assert/strict';
import {
  createAgentRosterReconciler, departedAgentSelectors,
} from '../dashboard/js/terminals-core.js';

test('departed agent selectors include stable and conversation identities', () => {
  const before = [
    { agent_id: 'agt_keep', conv_id: 'conv-old' },
    { agent_id: 'agt_retired', conv_id: 'conv-retired' },
  ];
  const after = [
    // The actor survived a reincarnation: its stable selector stays while the
    // obsolete generation selector departs.
    { agent_id: 'agt_keep', conv_id: 'conv-new' },
  ];

  assert.deepEqual(
    new Set(departedAgentSelectors(before, after)),
    new Set(['conv-old', 'agt_retired', 'conv-retired']),
  );
});

test('first or malformed roster is only a baseline', () => {
  assert.deepEqual(departedAgentSelectors(undefined, []), []);
  assert.deepEqual(departedAgentSelectors([], [{ agent_id: 'agt_new', conv_id: 'conv-new' }]), []);
});

test('selector extraction ignores empty, duplicate, and malformed identities', () => {
  const before = [
    null,
    { agent_id: 'agt_gone', conv_id: '' },
    { agent_id: 'agt_gone', conv_id: 42 },
  ];
  assert.deepEqual(departedAgentSelectors(before, []), ['agt_gone']);
});

test('degraded rosters neither close panes nor replace the authoritative baseline', () => {
  const reconcile = createAgentRosterReconciler();
  const original = [{ agent_id: 'agt_retired', conv_id: 'conv-retired' }];

  assert.deepEqual(reconcile(original, true), [], 'first authoritative roster is the baseline');
  assert.deepEqual(reconcile([], false), [], 'a degraded empty roster is ignored');
  assert.deepEqual(
    new Set(reconcile([], true)),
    new Set(['agt_retired', 'conv-retired']),
    'the later authoritative poll still observes the retirement',
  );
});

test('malformed authoritative input does not replace the roster baseline', () => {
  const reconcile = createAgentRosterReconciler();
  const original = [{ agent_id: 'agt_retired', conv_id: 'conv-retired' }];
  reconcile(original, true);
  assert.deepEqual(reconcile(null, true), []);
  assert.deepEqual(
    new Set(reconcile([], true)),
    new Set(['agt_retired', 'conv-retired']),
  );
});
