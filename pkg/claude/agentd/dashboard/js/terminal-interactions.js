// terminal-interactions.js — shared native-terminal affordances for every
// dashboard xterm surface: selection/copy, safe links, and clipboard images.

const IMAGE_TYPES = new Map([
  ['image/png', 'png'],
  ['image/jpeg', 'jpg'],
  ['image/webp', 'webp'],
]);
const PASTE_REPEAT_MS = 1000;
const TMUX_COPY_TIMEOUT_MS = 30000;
const TMUX_DRAG_MIN_DISTANCE_SQ = 9;
const SELECT_HINT = 'Option-drag to select on macOS; Shift-drag on Linux/Windows';
// Keep terminal-originated clipboard writes useful for large selections without
// allowing an unbounded OSC 52 payload to turn into a second large allocation
// during base64 decode. This is deliberately separate from attachment limits.
const MAX_OSC52_BYTES = 1024 * 1024;
// Clipboard writes are a page-global OS side effect. Keep at most one armed
// tmux copy across every terminal pane; a newer gesture supersedes the older
// one, matching Clipboard.write's own ordering contract.
let activeTmuxClipboardCopy = null;

// Browsers expose Shift+Enter distinctly, but xterm's default legacy keyboard
// encoding sends the same carriage return as plain Enter. Translate the
// browser gesture to Ctrl+J's line-feed byte, which both Claude Code and Codex
// CLI treat as "insert newline" in every terminal. Returning null leaves every
// other key (including modified Enter chords) to xterm.
export function terminalKeyInput(event) {
  if (event && event.type === 'keydown' && event.key === 'Enter' && event.shiftKey &&
      !event.altKey && !event.ctrlKey && !event.metaKey &&
      !event.isComposing && event.keyCode !== 229) {
    return '\n';
  }
  return null;
}

// Keep paste shortcuts in the browser. On Windows/Linux xterm otherwise turns
// Ctrl+V into the literal SYN byte before Chrome dispatches its paste event.
// Codex interprets that byte as "read an image from the OS clipboard", which
// means a remote web terminal tries agentd's X11 clipboard instead of the
// browser clipboard. Returning false from xterm's custom key handler skips its
// terminal input path without canceling the browser default; the subsequent
// paste event then carries either text to xterm or image bytes to onPaste.
//
// Shift is allowed for browsers/platforms that use Ctrl/Cmd+Shift+V for plain-
// text paste. Alt is deliberately excluded so AltGr and terminal chords remain
// available to the application.
export function isBrowserPasteShortcut(event) {
  if (!event || event.type !== 'keydown' || event.altKey) return false;
  const pasteKey = event.code === 'KeyV' ||
    (typeof event.key === 'string' && event.key.toLowerCase() === 'v');
  return pasteKey && Boolean(event.ctrlKey || event.metaKey);
}

// OSC 52 payloads have the form "selection;base64-data". tmux emits one when
// copy-mode creates a paste buffer while set-clipboard is external/on (external
// is the default). xterm exposes the payload without the OSC identifier.
//
// Return null for queries, malformed data, or oversized clipboard writes. The
// caller still consumes those terminal control sequences so they never render.
export function decodeOSC52(payload) {
  if (typeof payload !== 'string') return null;
  const separator = payload.indexOf(';');
  if (separator < 0) return null;
  // Check the unsliced string first. xterm has already accumulated the OSC
  // payload by this point, but an oversized sequence should not cause another
  // large string allocation here before we reject it.
  const encodedLength = payload.length - separator - 1;
  if (encodedLength > Math.ceil(MAX_OSC52_BYTES / 3) * 4) return null;
  const encoded = payload.slice(separator + 1);
  if (encoded === '?') return null;
  // OSC 52 uses ordinary RFC 4648 base64. Reject whitespace and URL-safe
  // variants rather than letting browser-specific atob leniency diverge.
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(encoded)) return null;
  try {
    const binary = atob(encoded);
    if (binary.length > MAX_OSC52_BYTES) return null;
    const bytes = Uint8Array.from(binary, c => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch (_) {
    return null;
  }
}

// Start a ClipboardItem write while the browser is still handling the mouseup
// gesture, but defer the actual text until tmux's OSC 52 response arrives over
// the PTY/WebSocket round trip. WebKit in particular requires the write call to
// happen inside the user gesture; a later writeText call may be denied even
// though the terminal output was caused by that gesture.
//
// Dependencies are injectable so the gesture/async split is covered by the
// Node suite without pretending Node has a system clipboard.
export function beginGestureClipboardWrite({
  clipboard = globalThis.navigator && globalThis.navigator.clipboard,
  ClipboardItemCtor = globalThis.ClipboardItem,
  BlobCtor = globalThis.Blob,
} = {}) {
  if (!clipboard || typeof clipboard.write !== 'function' ||
      typeof ClipboardItemCtor !== 'function' || typeof BlobCtor !== 'function') return null;

  let resolveContent;
  let rejectContent;
  let contentSettled = false;
  const content = new Promise((resolve, reject) => {
    resolveContent = resolve;
    rejectContent = reject;
  });
  let writeResult;
  try {
    const item = new ClipboardItemCtor({ 'text/plain': content });
    // This invocation, not eventual resolution of content, is the permission-
    // sensitive operation and therefore must remain synchronous with mouseup.
    writeResult = clipboard.write([item]);
  } catch (_) {
    // Do not strand a representation promise if construction/write is absent
    // or throws synchronously. The OSC handler will use writeText/legacy copy.
    contentSettled = true;
    resolveContent(new BlobCtor([], { type: 'text/plain' }));
    return null;
  }

  return {
    result: Promise.resolve(writeResult).then(() => true, () => false),
    resolve(text) {
      if (contentSettled) return;
      contentSettled = true;
      resolveContent(new BlobCtor([text], { type: 'text/plain' }));
    },
    cancel() {
      if (contentSettled) return;
      contentSettled = true;
      rejectContent(new Error('tmux clipboard response canceled'));
    },
  };
}

export function shouldArmTmuxClipboard(drag, event, mouseTrackingMode) {
  if (!drag || !event || mouseTrackingMode === 'none') return false;
  const multiClickCopy = Number(event.detail) >= 2;
  return (drag.moved || multiClickCopy) && event.button === 0 &&
    !event.altKey && !event.shiftKey && !event.ctrlKey && !event.metaKey;
}

function safeHTTPURL(raw) {
  try {
    const url = new URL(raw);
    return (url.protocol === 'http:' || url.protocol === 'https:') ? url.href : null;
  } catch (_) {
    return null;
  }
}

function legacyCopy(text) {
  const area = document.createElement('textarea');
  area.value = text;
  area.setAttribute('readonly', '');
  area.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0';
  document.body.append(area);
  area.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  area.remove();
  return ok;
}

async function writeClipboard(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (_) { /* insecure context or denied permission: use legacy copy */ }
  }
  return legacyCopy(text);
}

function clipboardImages(e) {
  const dt = e.clipboardData;
  if (!dt) return { files: [], unsupported: false };
  const files = [];
  const seen = new Set();
  let unsupported = false;
  const add = (file) => {
    if (!file || !String(file.type || '').startsWith('image/')) return;
    if (!IMAGE_TYPES.has(file.type)) { unsupported = true; return; }
    const key = `${file.name || ''}|${file.size}|${file.type}`;
    if (seen.has(key)) return;
    seen.add(key);
    files.push(file);
  };
  if (dt.files) {
    for (let i = 0; i < dt.files.length; i++) add(dt.files[i]);
  }
  if (dt.items) {
    for (let i = 0; i < dt.items.length; i++) {
      const item = dt.items[i];
      if (item.kind === 'file' && String(item.type || '').startsWith('image/')) add(item.getAsFile());
    }
  }
  return { files, unsupported };
}

async function uploadImages(files, signal) {
  const fd = new FormData();
  const stamp = Date.now();
  files.forEach((file, i) => {
    const ext = IMAGE_TYPES.get(file.type);
    fd.append('file', file, `pasted-image-${stamp}-${i + 1}.${ext}`);
  });
  const res = await fetch('/api/terminal-attachments', {
    method: 'POST', credentials: 'same-origin', body: fd, signal,
  });
  if (!res.ok) throw new Error((await res.text().catch(() => '')) || `HTTP ${res.status}`);
  const payload = await res.json();
  return (payload.files || []).map(f => f.path).filter(Boolean);
}

// attachTerminalInteractions must be called after term.open(host). It returns a
// disposer for DOM listeners; xterm-owned handlers/addons die with term.dispose.
export function attachTerminalInteractions({
  term, host, copyButton, setStatus, baseStatus = () => '', onSelectionChange = () => {},
}) {
  let statusTimer = null;
  let uploadPending = false;
  let uploadController = null;
  let generation = 0;
  let lastPasteAt = 0;
  let lastPasteKey = '';
  let tmuxDrag = null;
  let pendingTmuxCopy = null;
  const disposables = [];
  const ownerDocument = host.ownerDocument || document;

  function flash(message, delay = 2200) {
    if (!setStatus) return;
    if (statusTimer) clearTimeout(statusTimer);
    setStatus(message);
    statusTimer = setTimeout(() => setStatus(baseStatus()), delay);
  }

  function updateCopyButton() {
    const selected = term.hasSelection();
    onSelectionChange(selected);
    if (!copyButton) return;
    // Keep the control clickable even before a selection exists: clicking it
    // is the discoverable path to the platform-specific force-selection hint.
    copyButton.disabled = false;
    copyButton.dataset.hasSelection = selected ? 'true' : 'false';
    copyButton.setAttribute('aria-label', selected
      ? 'Copy selected terminal text (Ctrl/Cmd+Shift+C)'
      : `Copy terminal text. ${SELECT_HINT}`);
  }

  function cancelPendingTmuxCopy() {
    if (!pendingTmuxCopy) return;
    pendingTmuxCopy.cancel();
  }

  function finishPendingTmuxCopy(token) {
    if (pendingTmuxCopy !== token || !token.oscReceived || token.result === null) return;
    if (token.timer) clearTimeout(token.timer);
    pendingTmuxCopy = null;
    if (activeTmuxClipboardCopy === token) activeTmuxClipboardCopy = null;
    flash(token.result ? 'copied' : 'tmux copied; browser clipboard permission denied');
  }

  function armTmuxClipboardFromGesture() {
    if (activeTmuxClipboardCopy) activeTmuxClipboardCopy.cancel();
    const deferred = beginGestureClipboardWrite();
    const token = { deferred, timer: null, oscReceived: false, result: null };
    token.cancel = () => {
      if (token.timer) clearTimeout(token.timer);
      if (token.deferred) token.deferred.cancel();
      if (pendingTmuxCopy === token) pendingTmuxCopy = null;
      if (activeTmuxClipboardCopy === token) activeTmuxClipboardCopy = null;
    };
    token.timer = setTimeout(() => {
      if (activeTmuxClipboardCopy !== token) return;
      token.cancel();
      // A drag can belong to the running TUI rather than tmux copy-mode. A
      // missing OSC 52 is therefore a quiet no-op, not an error to flash.
    }, TMUX_COPY_TIMEOUT_MS);
    pendingTmuxCopy = token;
    activeTmuxClipboardCopy = token;
    if (deferred) {
      void deferred.result.then((ok) => {
        token.result = ok;
        // A browser may reject clipboard permission before tmux answers. Do not
        // claim a tmux copy failed for a drag the running TUI consumed instead;
        // only surface the result once a matching OSC 52 actually arrived.
        finishPendingTmuxCopy(token);
      });
    }
  }

  const onTmuxMouseDown = (event) => {
    if (event.button !== 0 || event.altKey || event.shiftKey || event.ctrlKey || event.metaKey) {
      tmuxDrag = null;
      return;
    }
    tmuxDrag = { x: event.clientX, y: event.clientY, moved: false };
  };
  const onTmuxMouseMove = (event) => {
    if (!tmuxDrag || tmuxDrag.moved) return;
    const dx = event.clientX - tmuxDrag.x;
    const dy = event.clientY - tmuxDrag.y;
    if (dx * dx + dy * dy >= TMUX_DRAG_MIN_DISTANCE_SQ) tmuxDrag.moved = true;
  };
  const onTmuxMouseUp = (event) => {
    const drag = tmuxDrag;
    tmuxDrag = null;
    if (!shouldArmTmuxClipboard(drag, event, term.modes.mouseTrackingMode)) return;
    // This document-capture listener runs before xterm forwards mouseup to
    // tmux. Arm the permission-sensitive write now; OSC 52 resolves it later.
    armTmuxClipboardFromGesture();
  };

  host.addEventListener('mousedown', onTmuxMouseDown, true);
  ownerDocument.addEventListener('mousemove', onTmuxMouseMove, true);
  ownerDocument.addEventListener('mouseup', onTmuxMouseUp, true);

  async function copySelection() {
    const selected = term.getSelection();
    if (!selected) {
      flash(SELECT_HINT);
      term.focus();
      return;
    }
    if (await writeClipboard(selected)) flash('copied');
    else flash('copy failed — clipboard permission denied');
    term.focus();
  }

  const activateLink = (event, raw) => {
    const url = safeHTTPURL(raw);
    if (!url) { flash('blocked unsafe link'); return; }
    if (!event || (!event.ctrlKey && !event.metaKey)) {
      flash('Ctrl/Cmd-click to open link');
      return;
    }
    window.open(url, '_blank', 'noopener,noreferrer');
  };
  const linkHandler = {
    activate: (event, text) => activateLink(event, text),
    hover: () => {},
    leave: () => {},
    allowNonHttpProtocols: false,
  };
  term.options.linkHandler = linkHandler; // explicit OSC 8 hyperlinks
  if (globalThis.WebLinksAddon && globalThis.WebLinksAddon.WebLinksAddon) {
    term.loadAddon(new globalThis.WebLinksAddon.WebLinksAddon(
      (event, uri) => activateLink(event, uri), linkHandler,
    ));
  }

  disposables.push(term.onSelectionChange(updateCopyButton));
  // tmux's normal mouse/copy-mode path stores the text in a tmux buffer and
  // emits OSC 52 to the attached terminal. Turning that standard sequence into
  // a browser clipboard write gives unmodified drag the same end result as a
  // native terminal, without polling tmux or adding a second server protocol.
  disposables.push(term.parser.registerOscHandler(52, (payload) => {
    const text = decodeOSC52(payload);
    // Ignore unsolicited OSC 52 completely. With tmux's default
    // set-clipboard=external, pane applications are filtered by tmux already;
    // this armed-only gate adds defense in depth (including when a user has
    // opted into set-clipboard=on) and prevents background clipboard poisoning.
    if (text !== null && pendingTmuxCopy && activeTmuxClipboardCopy === pendingTmuxCopy) {
      const token = pendingTmuxCopy;
      if (token.timer) {
        clearTimeout(token.timer);
        token.timer = null;
      }
      token.oscReceived = true;
      if (token.deferred) {
        token.deferred.resolve(text);
        finishPendingTmuxCopy(token);
      } else {
        // Older browsers cannot hold a promise-backed ClipboardItem open from
        // mouseup, but still get a best-effort write while the armed gesture's
        // transient activation may remain live.
        void writeClipboard(text).then((ok) => {
          token.result = ok;
          finishPendingTmuxCopy(token);
        });
      }
    }
    return true;
  }));
  updateCopyButton();
  if (copyButton) copyButton.addEventListener('click', copySelection);

  term.attachCustomKeyEventHandler((event) => {
    if (event.type !== 'keydown') return true;
    // Do not call preventDefault: Chrome still needs to dispatch the paste
    // event to xterm's textarea (and our capture listener above it).
    if (isBrowserPasteShortcut(event)) return false;
    const input = terminalKeyInput(event);
    if (input !== null) {
      event.preventDefault();
      // Terminal.input follows the normal user-input path and fires onData, so
      // the existing binary WebSocket forwarding remains the single PTY sink.
      term.input(input);
      return false;
    }
    const copyChord = (event.ctrlKey || event.metaKey) && event.shiftKey && event.code === 'KeyC';
    if (!copyChord) return true;
    event.preventDefault();
    copySelection();
    return false;
  });

  const onPaste = async (event) => {
    const { files, unsupported } = clipboardImages(event);
    if (!files.length) {
      if (unsupported) { event.preventDefault(); flash('unsupported image type — use PNG, JPEG, or WebP'); }
      return; // ordinary text paste remains xterm's responsibility
    }
    event.preventDefault();
    event.stopPropagation();
    if (uploadPending) return;
    const key = files.map(f => `${f.size}|${f.type}`).join(',');
    const now = performance.now();
    if (key === lastPasteKey && now - lastPasteAt < PASTE_REPEAT_MS) return;
    lastPasteKey = key;
    lastPasteAt = now;
    uploadPending = true;
    const myGeneration = generation;
    const controller = new AbortController();
    uploadController = controller;
    flash(files.length === 1 ? 'uploading image…' : `uploading ${files.length} images…`, 30000);
    try {
      const paths = await uploadImages(files, controller.signal);
      // The fallback modal reuses one xterm across sessions. close/reopen calls
      // invalidate(), so a slow upload from the old session can never paste its
      // path through the replacement session's WebSocket.
      if (controller.signal.aborted || generation !== myGeneration) return;
      if (!paths.length) throw new Error('upload returned no file path');
      term.paste(paths.join(' ') + ' ');
      flash(paths.length === 1 ? 'image pasted' : `${paths.length} images pasted`);
    } catch (error) {
      if (controller.signal.aborted || error && error.name === 'AbortError') return;
      const detail = String(error && error.message || error).replace(/\s+/g, ' ').slice(0, 120);
      flash(`image paste failed: ${detail}`, 5000);
    } finally {
      if (uploadController === controller) {
        uploadController = null;
        uploadPending = false;
        if (generation === myGeneration) term.focus();
      }
    }
  };
  // Capture on the host so image data is claimed before xterm's textarea paste
  // listener; text-only events are untouched and continue into xterm normally.
  host.addEventListener('paste', onPaste, true);

  function invalidate() {
    generation++;
    tmuxDrag = null;
    cancelPendingTmuxCopy();
    if (uploadController) uploadController.abort();
    uploadController = null;
    uploadPending = false;
    lastPasteAt = 0;
    lastPasteKey = '';
  }

  let disposed = false;
  return {
    invalidate,
    copySelection,
    dispose() {
      if (disposed) return;
      disposed = true;
      invalidate();
      if (statusTimer) clearTimeout(statusTimer);
      statusTimer = null;
      host.removeEventListener('mousedown', onTmuxMouseDown, true);
      ownerDocument.removeEventListener('mousemove', onTmuxMouseMove, true);
      ownerDocument.removeEventListener('mouseup', onTmuxMouseUp, true);
      host.removeEventListener('paste', onPaste, true);
      if (copyButton) copyButton.removeEventListener('click', copySelection);
      for (const d of disposables) { try { d.dispose(); } catch (_) { /* already disposed */ } }
    },
  };
}
