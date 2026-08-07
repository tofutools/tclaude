// terminal-interactions.js — shared native-terminal affordances for every
// dashboard xterm surface: selection/copy, safe links, and clipboard images.

import {
  isCommandPaletteShortcut,
  requestCommandPalette,
} from './command-registry.js';

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

// A TUI may own its selection and use the ordinary platform copy chord to
// publish it through OSC 52. Copilot CLI does exactly that: the selection is
// invisible to xterm, Ctrl/Cmd+C stays application input, and the resulting
// OSC arrives asynchronously over the PTY. Recognize only the exact copy
// gesture so the caller can arm browser clipboard access without consuming the
// key -- the running application must still receive it and decide what it
// means. An application that emits no OSC leaves only a quiet, bounded token.
export function isTerminalClipboardRequestShortcut(event) {
  if (!event || event.type !== 'keydown' || event.altKey || event.shiftKey ||
      event.isComposing || event.keyCode === 229 ||
      (event.ctrlKey && event.metaKey)) return false;
  const copyKey = event.code === 'KeyC' ||
    (typeof event.key === 'string' && event.key.toLowerCase() === 'c');
  return copyKey && Boolean(event.ctrlKey || event.metaKey);
}

export function isComposeMessageShortcut(event) {
  if (!event || event.type !== 'keydown' || event.altKey || event.shiftKey ||
      event.isComposing || event.keyCode === 229) return false;
  const m = event.code === 'KeyM' ||
    (typeof event.key === 'string' && event.key.toLowerCase() === 'm');
  return m && Boolean(event.ctrlKey || event.metaKey);
}

export function claimCommandPaletteShortcut(
  event,
  documentRef,
  requestPalette = requestCommandPalette,
) {
  if (!isCommandPaletteShortcut(event) ||
      !requestPalette(documentRef, { source: 'terminal' })) return false;
  event.preventDefault();
  // The palette opens synchronously. Do not let this same keydown bubble to
  // its global toggle handler and immediately close it again.
  event.stopPropagation();
  return true;
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

// Embedded credentials are rejected, not merely hidden. `https://<anything>@evil
// .example/` renders as the victim host right up to the '@', which is the whole
// point of putting it there; a terminal link carrying userinfo is either a
// spoof or a credential leak, and neither is worth a click. Doing it here
// covers both opening a link and describing one.
function safeHTTPURL(raw) {
  try {
    const url = new URL(raw);
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
    if (url.username || url.password) return null;
    return url.href;
  } catch (_) {
    return null;
  }
}

// OSC 8 file hyperlinks name files on the agentd host, not on the computer
// running the browser. Parse only ordinary absolute local file URLs here; UNC
// hosts and every other non-HTTP scheme stay blocked.
export function safeTerminalLink(raw) {
  const http = safeHTTPURL(raw);
  if (http) return { kind: 'http', target: http };
  try {
    // Some harnesses put the absolute path itself in OSC 8's URI field while
    // others use a file:// URI. A leading // would be a network-path
    // reference, so do not reinterpret it as a local filesystem target.
    if (typeof raw === 'string' && raw.startsWith('/') && !raw.startsWith('//')) {
      const url = new URL(raw, 'file://localhost');
      const path = decodeURIComponent(url.pathname);
      if (!path.includes('\0')) return { kind: 'file', target: path };
      return null;
    }
    if (typeof raw !== 'string' || !/^file:\/\//i.test(raw)) return null;
    const url = new URL(raw);
    if (url.protocol !== 'file:' ||
        (url.hostname !== '' && url.hostname !== 'localhost') ||
        url.username || url.password) return null;
    const path = decodeURIComponent(url.pathname);
    if (!path.startsWith('/') || path.includes('\0')) return null;
    return { kind: 'file', target: path };
  } catch (_) {
    return null;
  }
}

// Some harness renderers colour a local path but do not attach OSC 8 metadata.
// xterm therefore sees ordinary cells, not a hyperlink. Recognize the two
// unambiguous visible forms we can safely recover: a local file:// URI and an
// absolute POSIX path. Relative labels such as "report.png" deliberately stay
// plain because the browser cannot know which terminal directory they name.
const VISIBLE_LOCAL_FILE_RE = /file:\/\/(?:localhost)?\/[^\s"'<>]+|\/[^\s"'<>]+/g;

function trimPathPunctuation(raw) {
  let end = raw.length;
  while (end > 1 && /[.,;:!?]/.test(raw[end - 1])) end--;
  for (const [open, close] of [['(', ')'], ['[', ']'], ['{', '}']]) {
    while (end > 1 && raw[end - 1] === close) {
      const value = raw.slice(0, end);
      const opens = value.split(open).length - 1;
      const closes = value.split(close).length - 1;
      if (closes <= opens) break;
      end--;
    }
  }
  return raw.slice(0, end);
}

export function visibleLocalFileLinks(text) {
  if (typeof text !== 'string' || !text) return [];
  const links = [];
  for (const match of text.matchAll(VISIBLE_LOCAL_FILE_RE)) {
    const matched = match[0];
    const start = match.index;
    // A raw absolute path must start at a token boundary. This prevents the
    // suffixes of dates, bare-domain URLs, and relative paths becoming links.
    const before = start > 0 ? text[start - 1] : '';
    if (matched.startsWith('/') && before && !/[\s([{"'<>=]/.test(before)) continue;
    const raw = trimPathPunctuation(matched);
    if (raw === '/') continue;
    // A space can be part of a host filename but is indistinguishable from a
    // prose boundary in plain terminal cells. Only recover a raw path when the
    // rest of its logical line is blank; explicit file:// URLs remain
    // unambiguous and may appear inline.
    if (raw.startsWith('/') && /\S/.test(text.slice(start + matched.length))) continue;
    const parsed = safeTerminalLink(raw);
    if (!parsed || parsed.kind !== 'file') continue;
    links.push({ text: raw, start, end: start + raw.length });
  }
  return links;
}

function stringBoundaryToCell(line, index) {
  let seen = 0;
  for (let col = 0; col < line.length; col++) {
    const cell = line.getCell(col);
    if (!cell || cell.getWidth() === 0) continue;
    if (index <= seen) return col;
    const next = seen + (cell.getChars().length || 1);
    if (index < next) return col;
    if (index === next) return col + cell.getWidth();
    seen = next;
  }
  return line.length;
}

function wrappedLineWindow(term, row) {
  const buffer = term.buffer.active;
  let first = row;
  let line = buffer.getLine(first);
  if (!line) return null;
  while (line.isWrapped && first > 0) {
    first--;
    line = buffer.getLine(first);
    if (!line) return null;
  }
  const lines = [line];
  while (true) {
    const next = buffer.getLine(first + lines.length);
    if (!next || !next.isWrapped) break;
    lines.push(next);
  }
  return { first, lines, texts: lines.map((item) => item.translateToString(true)) };
}

function logicalBoundaryToBuffer(window, index, endBoundary) {
  let seen = 0;
  for (let offset = 0; offset < window.lines.length; offset++) {
    const length = window.texts[offset].length;
    const atLineEnd = index === seen + length;
    if (index < seen + length || (atLineEnd && (endBoundary || offset === window.lines.length - 1))) {
      return {
        x: stringBoundaryToCell(window.lines[offset], index - seen),
        y: window.first + offset + 1,
      };
    }
    seen += length;
  }
  const last = window.lines.length - 1;
  return {
    x: stringBoundaryToCell(window.lines[last], window.texts[last].length),
    y: window.first + last + 1,
  };
}

export function visibleLocalFileLinkProvider(term, handlers) {
  return {
    provideLinks(y, callback) {
      const window = wrappedLineWindow(term, y - 1);
      if (!window) {
        callback([]);
        return;
      }
      const text = window.texts.join('');
      const links = visibleLocalFileLinks(text).map((match) => {
        const start = logicalBoundaryToBuffer(window, match.start, false);
        const end = logicalBoundaryToBuffer(window, match.end, true);
        return {
          text: match.text,
          range: {
            start: { x: start.x + 1, y: start.y },
            end: { x: end.x, y: end.y },
          },
          activate: handlers.activate,
          hover: handlers.hover,
          leave: handlers.leave,
        };
      });
      callback(links);
    },
  };
}

// The status line is one row of chrome under the terminal, so a long target
// would push out the hint that precedes it. Shorten the PATH only: the origin
// answers "who am I about to contact", so eliding any part of it would defeat
// the reveal it exists for. `new URL` has already punycoded the host and
// percent-encoded anything in the path that could reorder the display.
function shortenForStatus(rawURL, max = 120) {
  let url;
  try {
    url = new URL(rawURL);
  } catch (_) {
    return rawURL.slice(0, max);
  }
  const rest = url.href.slice(url.origin.length);
  const room = max - url.origin.length;
  if (rest.length <= room) return url.href;
  return `${url.origin}${rest.slice(0, Math.max(room - 1, 0))}…`;
}

function shortenPathForStatus(path, max = 100) {
  if (path.length <= max) return path;
  const tailLength = Math.min(48, Math.floor((max - 1) / 2));
  return `${path.slice(0, max - tailLength - 1)}…${path.slice(-tailLength)}`;
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

async function uploadImages(files, signal, terminalPath) {
  const fd = new FormData();
  const stamp = Date.now();
  files.forEach((file, i) => {
    const ext = IMAGE_TYPES.get(file.type);
    fd.append('file', file, `pasted-image-${stamp}-${i + 1}.${ext}`);
  });
  const endpoint = `/api/terminal-attachments?terminal=${encodeURIComponent(terminalPath)}`;
  const res = await fetch(endpoint, {
    method: 'POST', credentials: 'same-origin', body: fd, signal,
  });
  if (!res.ok) throw new Error((await res.text().catch(() => '')) || `HTTP ${res.status}`);
  const payload = await res.json();
  return (payload.files || []).map(f => f.path).filter(Boolean);
}

// attachTerminalInteractions must be called after term.open(host). It returns a
// disposer for DOM listeners; xterm-owned handlers/addons die with term.dispose.
export function attachTerminalInteractions({
  term, host, copyButton, setStatus, baseStatus = () => '',
  terminalPath,
  applicationClipboardShortcuts = false,
  onComposeMessage = null, onSelectionChange = () => {},
  requestPalette = requestCommandPalette,
  fetchImpl = globalThis.fetch,
  downloadFile = null,
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

  async function downloadHostFile(path) {
    if (downloadFile) {
      downloadFile(path);
      return;
    }
    const query = new URLSearchParams({ terminal: terminalPath, path });
    const href = `/api/terminal-file?${query}`;
    // Keep the eventual GET streaming through the browser rather than
    // buffering a potentially large file into a Blob, but make errors and an
    // expired auth session observable first. auth-session.js wraps this fetch
    // and owns the top-level sign-in redirect.
    const response = await fetchImpl(href, {
      method: 'HEAD', credentials: 'same-origin', cache: 'no-store',
    });
    if (!response?.ok) {
      throw new Error(`download unavailable (${response?.status || 'network error'})`);
    }
    const anchor = ownerDocument.createElement('a');
    anchor.href = href;
    anchor.download = '';
    anchor.style.display = 'none';
    ownerDocument.body.append(anchor);
    anchor.click();
    anchor.remove();
  }

  const activateLink = (event, raw) => {
    const link = safeTerminalLink(raw);
    if (!link) { flash('blocked unsafe link'); return; }
    if (!event || (!event.ctrlKey && !event.metaKey)) {
      // Keep the destination in the hint. This flash replaces whatever the
      // hover put there and no further hover fires until the pointer moves, so
      // dropping the target here would blank it precisely when the human has
      // just been told to click again.
      const action = link.kind === 'file' ? 'download' : 'open';
      const target = link.kind === 'file'
        ? shortenPathForStatus(link.target)
        : shortenForStatus(link.target);
      flash(`Ctrl/Cmd-click to ${action} ${target}`);
      return;
    }
    if (link.kind === 'file') {
      void downloadHostFile(link.target).then(
        () => flash(`downloading ${link.target.split('/').pop() || 'file'}…`),
        (error) => flash(String(error?.message || error).slice(0, 120), 5000),
      );
      return;
    }
    window.open(link.target, '_blank', 'noopener,noreferrer');
  };
  // An OSC 8 hyperlink chooses its label text independently of its target, so
  // "see the docs" — or a string that reads like some other URL — can point
  // anywhere. Show the real destination while the pointer rests on it, so
  // Ctrl/Cmd-click is never a blind gesture. The same linkHandler is handed to
  // the web-links addon below, so plain URL matches get the reveal too; there
  // the text already IS the target, and it costs nothing to be consistent.
  const showLinkTarget = (raw) => {
    if (!setStatus) return;
    if (statusTimer) { clearTimeout(statusTimer); statusTimer = null; }
    const link = safeTerminalLink(raw);
    if (!link) {
      setStatus('blocked unsafe link');
      return;
    }
    const action = link.kind === 'file' ? 'download →' : '→';
    const target = link.kind === 'file'
      ? shortenPathForStatus(link.target)
      : shortenForStatus(link.target);
    setStatus(`Ctrl/Cmd-click ${action} ${target}`);
  };
  const clearLinkTarget = () => {
    if (!setStatus) return;
    if (statusTimer) { clearTimeout(statusTimer); statusTimer = null; }
    setStatus(baseStatus());
  };
  const linkHandler = {
    activate: (event, text) => activateLink(event, text),
    hover: (event, text) => showLinkTarget(text),
    leave: () => clearLinkTarget(),
    // xterm otherwise recognizes OSC 8 file:// links visually but declines to
    // hand them to this guarded handler.
    allowNonHttpProtocols: true,
  };
  term.options.linkHandler = linkHandler; // explicit OSC 8 hyperlinks
  if (globalThis.WebLinksAddon && globalThis.WebLinksAddon.WebLinksAddon) {
    term.loadAddon(new globalThis.WebLinksAddon.WebLinksAddon(
      (event, uri) => activateLink(event, uri), linkHandler,
    ));
  }
  disposables.push(term.registerLinkProvider(
    visibleLocalFileLinkProvider(term, linkHandler),
  ));

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
    // xterm owns a hidden textarea, so the dashboard's global launcher
    // deliberately treats it like ordinary text input. Ask the surrounding
    // document synchronously instead. The integrated dashboard claims the
    // request only when its experimental terminal shortcut is enabled; the
    // standalone terminal and default dashboard config both keep Ctrl-K.
    if (claimCommandPaletteShortcut(event, ownerDocument, requestPalette)) return false;
    if (onComposeMessage && isComposeMessageShortcut(event)) {
      event.preventDefault();
      onComposeMessage();
      return false;
    }
    // Do not call preventDefault: Chrome still needs to dispatch the paste
    // event to xterm's textarea (and our capture listener above it).
    if (isBrowserPasteShortcut(event)) return false;
    if (applicationClipboardShortcuts && isTerminalClipboardRequestShortcut(event)) {
      // Start the permission-sensitive browser write inside the trusted
      // keydown, then leave the chord entirely to xterm/the TUI. Copilot's OSC
      // 52 response resolves it; Ctrl+C applications that merely cancel work
      // emit nothing and the pending token expires without changing clipboard.
      armTmuxClipboardFromGesture();
      if (event.metaKey && !event.ctrlKey) {
        event.preventDefault();
        // xterm maps Meta+C to an Escape-prefixed character rather than the
        // Ctrl+C byte Copilot's terminal UI binds. Inject ETX through
        // Terminal.input so it follows the ordinary onData/WebSocket path once.
        term.input('\x03');
        return false;
      }
      return true;
    }
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
      const paths = await uploadImages(files, controller.signal, terminalPath);
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
// dashboard-imperative-boundary: browser-io
