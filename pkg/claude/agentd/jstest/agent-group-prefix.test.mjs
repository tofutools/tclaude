// Unit tests for agent-group-prefix.js: an agent's group where its terminal
// names it, in full ("tclaude/lead") or, in a tab beside another of the same
// name, as a prefix ("t/lead").

import test from 'node:test';
import assert from 'node:assert/strict';
import { agentGroupMark, clashPrefixes, distinctStarts, fitGroup, groupName, markTitle, seedGroupMark } from '../dashboard/js/agent-group-prefix.js';

test('tabs get a prefix only on a name clash, told apart among their own groups', () => {
  const prefixes = clashPrefixes([
    { key: 'a', name: 'lead', group: 'tclaude' },
    { key: 'b', name: 'Lead', group: 'counterslop' },
    { key: 'c', name: 'reviewer', group: 'tclaude' },
    { key: 'd', name: 'scratch', group: '' },
  ]);
  assert.equal(prefixes.get('a'), 't');
  assert.equal(prefixes.get('b'), 'c', 'names compared without case');
  assert.equal(prefixes.has('c'), false, 'a unique name has no prefix');
  assert.equal(prefixes.has('d'), false);
});

test('a clash within one group needs no prefix; one with an ungrouped agent does', () => {
  assert.equal(clashPrefixes([{ key: 'a', name: 'lead', group: 'tclaude' }, { key: 'b', name: 'lead', group: 'tclaude' }]).size, 0);
  const mixed = clashPrefixes([{ key: 'a', name: 'lead', group: 'tclaude' }, { key: 'b', name: 'lead', group: '' }]);
  assert.equal(mixed.get('a'), 't');
  assert.equal(mixed.has('b'), false);
});

test('a group that starts another is given whole, and a start never ends on a separator', () => {
  const starts = distinctStarts(['tclaude', 'tclaude-config']);
  assert.equal(starts.get('tclaude'), 'tclaude');
  assert.equal(starts.get('tclaude-config'), 'tclaude-c');
  assert.equal(distinctStarts(['webshop', 'workers']).get('workers'), 'wo');
});

const snapshot = {
  agents: [
    { agent_id: 'agt_a', conv_id: 'conv-a', title: 'lead', groups: ['tclaude'] },
    { agent_id: 'agt_c', title: 'reviewer', groups: ['tclaude', 'payments'], spawn_group: 'payments' },
    { agent_id: 'agt_d', title: 'loner', groups: [] },
  ],
};

test('an agent is named after its full group, the one its terminal was opened from', () => {
  const a = agentGroupMark(snapshot, 'agt_a');
  assert.equal(groupName('lead', a), 'tclaude/lead');
  assert.equal(markTitle('lead', a), 'tclaude/lead');
  assert.deepEqual(agentGroupMark(snapshot, 'conv-a'), a, 'found by conversation too');
  assert.equal(agentGroupMark(snapshot, 'agt_c').group, 'payments', 'else the group it was spawned into');
  const opened = agentGroupMark(snapshot, 'agt_c', 'tclaude');
  assert.equal(markTitle('reviewer', opened), 'tclaude/reviewer (also in payments)');
  assert.equal(agentGroupMark(snapshot, 'agt_d'), null);
  assert.equal(agentGroupMark(null, 'agt_a'), null);
  assert.equal(groupName('loner', null), 'loner');
});

test('a full group short of room keeps the letters that fit before an ellipsis', () => {
  const width = (text) => text.length * 10;
  assert.equal(fitGroup('webshop', 70, width), 'webshop');
  assert.equal(fitGroup('webshop', 60, width), 'websh…');
  assert.equal(fitGroup('webshop', -40, width), 'w…', 'at least one letter');
  assert.equal(fitGroup('webshop', Number.NaN, width), 'webshop', 'not measured: whole');
});

test('a popped-out window takes a well-formed mark from its seed, and drops any other', () => {
  const mark = { group: 'tclaude', groups: ['tclaude', 'webshop'] };
  assert.deepEqual(seedGroupMark(mark), mark);
  assert.deepEqual(seedGroupMark({ ...mark, prefix: 't' }), mark, 'an older seed\'s prefix is ignored');
  assert.equal(seedGroupMark({ ...mark, group: 'other' }), null);
  assert.equal(seedGroupMark({ ...mark, groups: 'tclaude' }), null);
  assert.equal(seedGroupMark(undefined), null);
});
