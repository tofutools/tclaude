import test from 'node:test';
import assert from 'node:assert/strict';
import {
  disclosurePreference,
  groupActivityAtDisclosure,
  groupActivityPlacement,
  mergeActivityMembers,
} from '../dashboard/js/group-tree-activity.js';

const member = (conv_id) => ({ conv_id });

test('disclosure preference distinguishes explicit folds from reconciliation noise', () => {
  assert.equal(disclosurePreference(true, true), '1');
  assert.equal(disclosurePreference(false, true), '0',
    'an operator fold persists across pending-group default-open rendering');
  assert.equal(disclosurePreference(false, false), null,
    'a recreated default-closed details does not become explicitly closed');
  assert.equal(disclosurePreference(false, false, '0'), '0',
    'a recreated folded pending group keeps its explicit-close sentinel');
  assert.equal(disclosurePreference(true, false, '1'), '1',
    'a recreated open group keeps its explicit-open sentinel');
});

test('folded group rolls every hidden descendant into its header', () => {
  const grandchild = groupActivityAtDisclosure([member('grandchild')], [], false);
  const child = groupActivityAtDisclosure(
    [member('child')],
    [grandchild.subtreeMembers],
    true,
  );
  const root = groupActivityAtDisclosure(
    [member('root')],
    [child.subtreeMembers],
    false,
  );

  assert.deepEqual(child.headerMembers.map((row) => row.conv_id), ['child'],
    'an open child leaves its visible grandchild activity at the grandchild header');
  assert.deepEqual(root.headerMembers.map((row) => row.conv_id),
    ['root', 'child', 'grandchild'],
    'a folded root absorbs the complete subtree even when hidden descendants are open');
});

test('open group keeps descendant activity on the visible child headers', () => {
  const child = groupActivityAtDisclosure([member('child')], [], false);
  const root = groupActivityAtDisclosure([member('root')], [child.subtreeMembers], true);

  assert.deepEqual(root.headerMembers.map((row) => row.conv_id), ['root']);
  assert.deepEqual(child.headerMembers.map((row) => row.conv_id), ['child']);
  assert.deepEqual(root.subtreeMembers.map((row) => row.conv_id), ['root', 'child'],
    'the complete subtree still propagates for a higher folded ancestor');
});

test('rollups count an agent only once across overlapping nested memberships', () => {
  const shared = member('shared');
  const child = groupActivityAtDisclosure([shared, member('child-only')], [], false);
  const root = groupActivityAtDisclosure(
    [shared, member('root-only')],
    [child.subtreeMembers],
    false,
  );

  assert.deepEqual(root.headerMembers.map((row) => row.conv_id),
    ['shared', 'root-only', 'child-only']);
  assert.equal(mergeActivityMembers([[{}], [{}]]).length, 2,
    'legacy rows without conv_id remain distinct');
});

function idsByGroup(placement) {
  return Object.fromEntries([...placement].map(([name, members]) => [
    name, members.map((row) => row.conv_id),
  ]));
}

const tree = () => [
  { name: 'root', members: [member('root')] },
  { name: 'child-a', parent: 'root', members: [member('child-a')] },
  { name: 'grandchild', parent: 'child-a', members: [member('grandchild')] },
  { name: 'child-b', parent: 'root', members: [member('child-b')] },
];

test('placement puts a fully folded tree on its top-level visible header', () => {
  const placement = groupActivityPlacement(tree(), () => false);

  assert.deepEqual(idsByGroup(placement), {
    root: ['root', 'child-a', 'grandchild', 'child-b'],
    'child-a': ['child-a', 'grandchild'],
    grandchild: ['grandchild'],
    'child-b': ['child-b'],
  });
});

test('placement follows partial visibility to the leaf-most visible headers', () => {
  const open = new Set(['root']);
  const placement = groupActivityPlacement(tree(), (group) => open.has(group.name));

  assert.deepEqual(idsByGroup(placement), {
    root: ['root'],
    'child-a': ['child-a', 'grandchild'],
    grandchild: ['grandchild'],
    'child-b': ['child-b'],
  }, 'the open root keeps direct activity while each visible folded child owns its subtree');
});

test('placement separates every level when the whole tree is unfolded', () => {
  const placement = groupActivityPlacement(tree(), () => true);

  assert.deepEqual(idsByGroup(placement), {
    root: ['root'],
    'child-a': ['child-a'],
    grandchild: ['grandchild'],
    'child-b': ['child-b'],
  });
});

test('placement treats filtered or dangling children as visible roots', () => {
  const groups = [
    { name: 'visible-child', parent: 'filtered-parent', members: [member('child')] },
    { name: 'virtual', virtual: true, members: [member('virtual')] },
  ];
  const placement = groupActivityPlacement(groups, () => false);

  assert.deepEqual(idsByGroup(placement), { 'visible-child': ['child'] });
  assert.equal(placement.has('virtual'), false);
});
