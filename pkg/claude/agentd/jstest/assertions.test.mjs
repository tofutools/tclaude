import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';

test('assertAbsent reports a bounded element label without requiring a message', () => {
  const found = {
    localName: 'button',
    getAttribute(name) {
      assert.equal(name, 'class');
      return 'primary active';
    },
  };

  assert.throws(() => assertAbsent(found), (error) => {
    assert.equal(error.code, 'ERR_ASSERTION');
    assert.equal(error.actual, 'button.primary active');
    assert.equal(error.expected, null);
    return true;
  });
});

test('assertAbsent preserves a caller-provided failure message', () => {
  const found = {
    localName: 'dialog',
    getAttribute: () => 'dirty',
  };

  assert.throws(() => assertAbsent(found, 'expected the dialog to close'), (error) => {
    assert.match(error.message, /^expected the dialog to close\n/);
    assert.equal(error.actual, 'dialog.dirty');
    assert.equal(error.expected, null);
    return true;
  });
});
