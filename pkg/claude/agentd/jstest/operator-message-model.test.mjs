import test from 'node:test';
import assert from 'node:assert/strict';
import { snapshotOperatorMessageDraft } from '../dashboard/js/operator-message-model.js';

test('operator message submit snapshots target, fields, and attachment membership atomically', () => {
  const target = { agent: 'agt_original' };
  const first = { name: 'first.txt' };
  const files = [first];
  const draft = snapshotOperatorMessageDraft({
    target, subject: 'before', body: 'original body', files,
  });

  target.agent = 'agt_later';
  files.splice(0, 1, { name: 'later.txt' });

  assert.deepEqual(draft, {
    to: 'agt_original',
    subject: 'before',
    body: 'original body',
    files: [first],
  });
  assert.equal(Object.isFrozen(draft), true);
  assert.equal(Object.isFrozen(draft.files), true);
});
