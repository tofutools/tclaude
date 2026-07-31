import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('group clone order inserts beside the chosen root or subgroup anchor', async (t) => {
  const harness = await createPreactHarness(t);
  const { insertGroupBeside } = await harness.importDashboardModule('js/group-order.js');

  assert.deepEqual(
    insertGroupBeside(['root-a', 'root-b'], 'root-copy', 'root-a'),
    ['root-a', 'root-copy', 'root-b'],
  );
  assert.deepEqual(
    insertGroupBeside(['parent', 'child-a', 'child-b'], 'child-copy', 'child-b', true),
    ['parent', 'child-a', 'child-copy', 'child-b'],
  );
  assert.deepEqual(
    insertGroupBeside(['root-a'], 'copy', 'missing'),
    ['root-a', 'copy'],
  );
});
