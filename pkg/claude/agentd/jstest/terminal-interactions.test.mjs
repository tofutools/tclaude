import test from 'node:test';
import assert from 'node:assert/strict';
import {
  beginGestureClipboardWrite, decodeOSC52, shouldArmTmuxClipboard, terminalKeyInput,
} from '../dashboard/js/terminal-interactions.js';

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

test('gesture clipboard write starts synchronously and resolves its text later', async () => {
  let writtenItems = null;
  let writtenBlob = null;
  class FakeClipboardItem {
    constructor(data) { this.data = data; }
  }
  const clipboard = {
    write(items) {
      writtenItems = items;
      return items[0].data['text/plain'].then((blob) => { writtenBlob = blob; });
    },
  };

  const deferred = beginGestureClipboardWrite({
    clipboard, ClipboardItemCtor: FakeClipboardItem, BlobCtor: Blob,
  });
  assert.ok(deferred);
  assert.equal(writtenItems.length, 1, 'clipboard.write ran before OSC text existed');
  assert.equal(writtenBlob, null);

  deferred.resolve('tmux selection 🧇');
  assert.equal(await deferred.result, true);
  assert.equal(writtenBlob.type, 'text/plain');
  assert.equal(await writtenBlob.text(), 'tmux selection 🧇');
});

test('gesture clipboard cancellation rejects the pending representation quietly', async () => {
  class FakeClipboardItem {
    constructor(data) { this.data = data; }
  }
  const clipboard = {
    write(items) { return items[0].data['text/plain']; },
  };
  const deferred = beginGestureClipboardWrite({
    clipboard, ClipboardItemCtor: FakeClipboardItem, BlobCtor: Blob,
  });
  deferred.cancel();
  assert.equal(await deferred.result, false);
});

test('gesture clipboard write degrades when ClipboardItem is unavailable', () => {
  let called = false;
  const deferred = beginGestureClipboardWrite({
    clipboard: { write() { called = true; } }, ClipboardItemCtor: undefined, BlobCtor: Blob,
  });
  assert.equal(deferred, null);
  assert.equal(called, false);
});

test('tmux clipboard arming requires a tracked unmodified copy gesture', () => {
  const event = {
    button: 0, detail: 1, altKey: false, shiftKey: false, ctrlKey: false, metaKey: false,
  };
  assert.equal(shouldArmTmuxClipboard({ moved: true }, event, 'drag'), true);
  assert.equal(shouldArmTmuxClipboard({ moved: false }, { ...event, detail: 2 }, 'drag'), true,
    'tmux double-click copy does not move');
  assert.equal(shouldArmTmuxClipboard({ moved: false }, event, 'drag'), false,
    'plain clicks do not arm');
  assert.equal(shouldArmTmuxClipboard({ moved: true }, event, 'none'), false,
    'browser-owned selection does not request clipboard permission');
  assert.equal(shouldArmTmuxClipboard({ moved: true }, { ...event, shiftKey: true }, 'drag'), false,
    'modifier-forced browser selection does not arm');
});
