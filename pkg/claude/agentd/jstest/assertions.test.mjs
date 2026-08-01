import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertDifferentNode, assertSameNode } from './assertions.mjs';

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

function node(localName, attributes = {}) {
  const value = {
    localName,
    getAttribute: (name) => attributes[name] ?? null,
  };
  // The diagnostic boundary must never walk this graph.
  value.parentNode = value;
  value.childNodes = [value];
  return value;
}

test('assertSameNode preserves identity when distinct nodes have the same description', () => {
  const actual = node('button', { id: 'save', class: 'primary' });
  const expected = node('button', { id: 'save', class: 'primary' });

  assert.throws(() => assertSameNode(actual, expected, 'focus moved'), (error) => {
    assert.match(error.message, /^focus moved\nexpected the same DOM node\n/);
    assert.match(error.message, /actual: button#save\.primary/);
    assert.match(error.message, /expected: button#save\.primary/);
    assert.doesNotMatch(error.message, /parentNode|childNodes/);
    return true;
  });
  assert.doesNotThrow(() => assertSameNode(actual, actual));
});

test('assertDifferentNode preserves identity and bounds its shared-node diagnostic', () => {
  const shared = node('a', { 'data-key': 'agent-7' });
  const other = node('a', { 'data-key': 'agent-7' });

  assert.throws(() => assertDifferentNode(shared, shared, 'node was reused'), (error) => {
    assert.match(error.message, /^node was reused\nexpected different DOM nodes\n/);
    assert.match(error.message, /both: a\[data-key="agent-7"\]/);
    assert.doesNotMatch(error.message, /parentNode|childNodes/);
    return true;
  });
  assert.doesNotThrow(() => assertDifferentNode(shared, other));
});
