import test from 'node:test';
import assert from 'node:assert/strict';
import {
  attachTerminalInteractions, beginGestureClipboardWrite, decodeOSC52,
  isBrowserPasteShortcut, shouldArmTmuxClipboard, terminalKeyInput,
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

test('both Ctrl and Meta paste shortcuts stay browser-owned on every platform', () => {
  for (const shortcut of [
    key({ key: 'v', code: 'KeyV', ctrlKey: true }),
    key({ key: 'V', code: 'KeyV', ctrlKey: true, shiftKey: true }),
    key({ key: 'v', code: 'KeyV', metaKey: true }),
    key({ key: 'V', code: 'KeyV', metaKey: true, shiftKey: true }),
  ]) {
    assert.equal(isBrowserPasteShortcut(shortcut), true);
  }
});

test('unrelated and Alt-modified V chords remain terminal-owned', () => {
  assert.equal(isBrowserPasteShortcut(key({ key: 'v', code: 'KeyV' })), false);
  assert.equal(isBrowserPasteShortcut(
    key({ key: 'v', code: 'KeyV', ctrlKey: true, altKey: true })), false);
  assert.equal(isBrowserPasteShortcut(
    key({ type: 'keyup', key: 'v', code: 'KeyV', ctrlKey: true })), false);
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

class FakeEventTarget {
  constructor() { this.listeners = new Map(); }
  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type).add(fn);
  }
  removeEventListener(type, fn) { this.listeners.get(type)?.delete(fn); }
  dispatch(type, event) {
    for (const fn of this.listeners.get(type) || []) fn(event);
  }
}

function terminalHarness(ownerDocument) {
  const host = new FakeEventTarget();
  host.ownerDocument = ownerDocument;
  host.title = '';
  let osc52 = null;
  let keyHandler = null;
  const term = {
    options: {},
    modes: { mouseTrackingMode: 'drag' },
    parser: {
      registerOscHandler(id, handler) {
        assert.equal(id, 52);
        osc52 = handler;
        return { dispose() {} };
      },
    },
    onSelectionChange() { return { dispose() {} }; },
    attachCustomKeyEventHandler(handler) { keyHandler = handler; },
    hasSelection() { return false; },
    getSelection() { return ''; },
    focus() {},
  };
  return {
    host, term,
    key: (event) => keyHandler(event),
    osc52: (payload) => osc52(payload),
  };
}

test('terminal wiring reserves Ctrl+V without canceling the browser paste event', () => {
  const doc = new FakeEventTarget();
  const harness = terminalHarness(doc);
  const interactions = attachTerminalInteractions({ term: harness.term, host: harness.host });
  try {
    let prevented = false;
    const event = key({
      key: 'v', code: 'KeyV', ctrlKey: true,
      preventDefault() { prevented = true; },
    });
    assert.equal(harness.key(event), false, 'xterm must not forward Ctrl+V to the PTY');
    assert.equal(prevented, false, 'the browser must still dispatch its paste event');
  } finally {
    interactions.dispose();
  }
});

function drag(harness, ownerDocument) {
  const plain = { button: 0, detail: 1, altKey: false, shiftKey: false, ctrlKey: false, metaKey: false };
  harness.host.dispatch('mousedown', { ...plain, clientX: 1, clientY: 1 });
  ownerDocument.dispatch('mousemove', { ...plain, clientX: 10, clientY: 1 });
  ownerDocument.dispatch('mouseup', { ...plain, clientX: 10, clientY: 1 });
}

test('terminal lifecycle accepts only the latest armed pane OSC 52', async () => {
  const oldNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const oldClipboardItem = Object.getOwnPropertyDescriptor(globalThis, 'ClipboardItem');
  const writes = [];
  class FakeClipboardItem {
    constructor(data) { this.data = data; }
  }
  const clipboard = {
    write(items) {
      const representation = items[0].data['text/plain'];
      const result = representation.then(async blob => ({ type: blob.type, text: await blob.text() }));
      writes.push(result);
      return result.then(() => undefined);
    },
  };
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { clipboard } });
  Object.defineProperty(globalThis, 'ClipboardItem', { configurable: true, value: FakeClipboardItem });

  const doc = new FakeEventTarget();
  const first = terminalHarness(doc);
  const second = terminalHarness(doc);
  const firstInteractions = attachTerminalInteractions({ term: first.term, host: first.host });
  const secondInteractions = attachTerminalInteractions({ term: second.term, host: second.host });
  try {
    // An OSC sequence with no preceding mouse copy is consumed but cannot
    // start a browser clipboard write.
    first.osc52(`;${Buffer.from('poison').toString('base64')}`);
    assert.equal(writes.length, 0);

    drag(first, doc);
    assert.equal(writes.length, 1);
    drag(second, doc);
    assert.equal(writes.length, 2, 'new pane supersedes the first page-global write');
    await assert.rejects(writes[0], /canceled/);

    // The canceled pane no longer owns the active token, so its later OSC is
    // ignored rather than resolving the second pane's clipboard item.
    first.osc52(`;${Buffer.from('stale').toString('base64')}`);
    second.osc52(`;${Buffer.from('latest 🧇').toString('base64')}`);
    assert.deepEqual(await writes[1], { type: 'text/plain', text: 'latest 🧇' });

    drag(first, doc);
    assert.equal(writes.length, 3);
    firstInteractions.invalidate();
    await assert.rejects(writes[2], /canceled/);
    first.osc52(`;${Buffer.from('after invalidate').toString('base64')}`);
    assert.equal(writes.length, 3);
  } finally {
    firstInteractions.dispose();
    secondInteractions.dispose();
    if (oldNavigator) Object.defineProperty(globalThis, 'navigator', oldNavigator);
    else delete globalThis.navigator;
    if (oldClipboardItem) Object.defineProperty(globalThis, 'ClipboardItem', oldClipboardItem);
    else delete globalThis.ClipboardItem;
  }
});
