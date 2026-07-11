import test from 'node:test';
import assert from 'node:assert/strict';
import { terminalKeyInput } from '../dashboard/js/terminal-interactions.js';

function key(overrides = {}) {
  return {
    type: 'keydown', key: 'Enter', shiftKey: false,
    altKey: false, ctrlKey: false, metaKey: false,
    ...overrides,
  };
}

test('Shift+Enter becomes the universal Ctrl+J newline byte', () => {
  assert.equal(terminalKeyInput(key({ shiftKey: true })), '\n');
});

test('plain Enter and unrelated keys remain xterm-owned', () => {
  assert.equal(terminalKeyInput(key()), null);
  assert.equal(terminalKeyInput(key({ key: 'a', shiftKey: true })), null);
  assert.equal(terminalKeyInput(key({ type: 'keyup', shiftKey: true })), null);
});

test('additional modifiers on Shift+Enter remain xterm-owned', () => {
  for (const modifier of ['altKey', 'ctrlKey', 'metaKey']) {
    assert.equal(terminalKeyInput(key({ shiftKey: true, [modifier]: true })), null, modifier);
  }
});

test('Shift+Enter remains xterm-owned while an IME composition is active', () => {
  assert.equal(terminalKeyInput(key({ shiftKey: true, isComposing: true })), null);
  assert.equal(terminalKeyInput(key({ shiftKey: true, keyCode: 229 })), null);
});
