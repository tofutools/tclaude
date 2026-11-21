// Unit tests for agent-group-prefix.js: the short group prefix on an agent's
// name in its terminal ("t/lead").

import test from 'node:test';
import assert from 'node:assert/strict';
import { agentGroupMark, groupPrefixes, markTitle, markedName } from '../dashboard/js/agent-group-prefix.js';

test('a group gets the shortest start no other group shares', () => {
  const prefixes = groupPrefixes(['tclaude', 'counterslop', 'webshop', 'workers', 'web', 'Wiki']);
  assert.equal(prefixes.get('tclaude'), 't');
  assert.equal(prefixes.get('counterslop'), 'c');
  assert.equal(prefixes.get('workers'), 'wo');
  assert.equal(prefixes.get('Wiki'), 'Wi', 'compared without case: W and w are not told apart');
  // web is the start of webshop: it is given in full; webshop needs one more.
  assert.equal(prefixes.get('web'), 'web');
  assert.equal(prefixes.get('webshop'), 'webs');
  assert.equal(groupPrefixes(['solo']).get('solo'), 's');
  assert.equal(groupPrefixes([]).size, 0);
});

const snapshot = {
  groups: [{ name: 'tclaude' }, { name: 'counterslop' }, { name: 'payments-platform-migration-2027' }],
  agents: [
    { agent_id: 'agt_a', conv_id: 'conv-a', title: 'lead', groups: ['tclaude'] },
    { agent_id: 'agt_b', title: 'lead', groups: ['counterslop'] },
    { agent_id: 'agt_c', title: 'reviewer', groups: ['tclaude', 'payments-platform-migration-2027'], spawn_group: 'payments-platform-migration-2027' },
    { agent_id: 'agt_d', title: 'loner', groups: [] },
  ],
};

test('two leads of different groups are told apart', () => {
  const a = agentGroupMark(snapshot, 'agt_a');
  const b = agentGroupMark(snapshot, 'agt_b');
  assert.equal(markedName('lead', a), 't/lead');
  assert.equal(markedName('lead', b), 'c/lead');
  assert.equal(markTitle('lead', a), 'tclaude/lead');
  assert.deepEqual(agentGroupMark(snapshot, 'conv-a'), a, 'found by conversation too');
});

test('an agent in several groups shows the one its terminal was opened from', () => {
  assert.equal(agentGroupMark(snapshot, 'agt_c').group, 'payments-platform-migration-2027', 'else the group it was spawned into');
  const opened = agentGroupMark(snapshot, 'agt_c', 'tclaude');
  assert.equal(markedName('reviewer', opened), 't/reviewer');
  assert.equal(markTitle('reviewer', opened), 'tclaude/reviewer (also in payments-platform-migration-2027)');
  assert.equal(agentGroupMark(snapshot, 'agt_c', 'counterslop').group, 'payments-platform-migration-2027', 'not a group it is in');
});

test('an agent in no group, or not on this node, has no prefix', () => {
  assert.equal(agentGroupMark(snapshot, 'agt_d'), null);
  assert.equal(agentGroupMark(snapshot, 'agt_remote'), null);
  assert.equal(agentGroupMark(null, 'agt_a'), null);
  assert.equal(markedName('loner', null), 'loner');
  assert.equal(markTitle('loner', null), 'loner');
});
