import test from 'node:test';
import assert from 'node:assert/strict';
import {
	attachTerminalInteractions, beginGestureClipboardWrite, decodeOSC52,
	isBrowserPasteShortcut, isComposeMessageShortcut, safeTerminalLink,
	isTerminalClipboardRequestShortcut, shouldArmTmuxClipboard, terminalKeyInput,
	visibleLocalFileLinkProvider,
	visibleLocalFileLinks,
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
    focus() {},
  };
  return {
    host, term,
    key: (event) => keyHandler(event),
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
  const firstInteractions = attachTerminalInteractions({ term: first.term, host: first.host });
  const secondInteractions = attachTerminalInteractions({ term: second.term, host: second.host });
  try {
    // An OSC sequence with no preceding mouse copy is consumed but cannot
    // start a browser clipboard write.
    first.osc52(`;${Buffer.from('poison').toString('base64')}`);
    assert.equal(writes.length, 0);

    const copyEvent = key({ key: 'c', code: 'KeyC', ctrlKey: true });
    assert.equal(first.key(copyEvent), true, 'the application must still receive Ctrl+C');
    assert.equal(writes.length, 1, 'keyboard gesture starts the deferred browser write');
    first.osc52(`;${Buffer.from('copilot selection').toString('base64')}`);
    assert.deepEqual(await writes[0], { type: 'text/plain', text: 'copilot selection' });

    drag(first, doc);
    assert.equal(writes.length, 2);
    drag(second, doc);
    assert.equal(writes.length, 3, 'new pane supersedes the first page-global write');
    await assert.rejects(writes[1], /canceled/);

    // The canceled pane no longer owns the active token, so its later OSC is
    // ignored rather than resolving the second pane's clipboard item.
    first.osc52(`;${Buffer.from('stale').toString('base64')}`);
    second.osc52(`;${Buffer.from('latest 🧇').toString('base64')}`);
    assert.deepEqual(await writes[2], { type: 'text/plain', text: 'latest 🧇' });

    drag(first, doc);
    assert.equal(writes.length, 4);
    firstInteractions.invalidate();
    await assert.rejects(writes[3], /canceled/);
    first.osc52(`;${Buffer.from('after invalidate').toString('base64')}`);
    assert.equal(writes.length, 4);
  } finally {
    firstInteractions.dispose();
    secondInteractions.dispose();
    if (oldNavigator) Object.defineProperty(globalThis, 'navigator', oldNavigator);
    else delete globalThis.navigator;
    if (oldClipboardItem) Object.defineProperty(globalThis, 'ClipboardItem', oldClipboardItem);
    else delete globalThis.ClipboardItem;
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
