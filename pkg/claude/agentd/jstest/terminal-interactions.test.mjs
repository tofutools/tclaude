import test from 'node:test';
import assert from 'node:assert/strict';
import { decodeOSC52, terminalKeyInput } from '../dashboard/js/terminal-interactions.js';

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

test('OSC 52 decodes tmux clipboard text as UTF-8', () => {
  const text = 'first line\nsmörgåsbord 🧇';
  const encoded = Buffer.from(text, 'utf8').toString('base64');
  assert.equal(decodeOSC52(`c;${encoded}`), text);
});

test('OSC 52 accepts an empty clipboard but rejects queries and malformed data', () => {
  assert.equal(decodeOSC52('c;'), '');
  assert.equal(decodeOSC52('c;?'), null);
  assert.equal(decodeOSC52('missing-separator'), null);
  assert.equal(decodeOSC52('c;not base64'), null);
  assert.equal(decodeOSC52('c;abcd='), null);
});

test('OSC 52 rejects decoded clipboard text over the one MiB bound', () => {
  const oversized = Buffer.alloc(1024 * 1024 + 1, 0x61).toString('base64');
  assert.equal(decodeOSC52(`c;${oversized}`), null);
});
