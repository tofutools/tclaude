// terminal-interactions.js — shared native-terminal affordances for every
// dashboard xterm surface: selection/copy, safe links, and pasted/dropped files.

import { downloadRemoteFile as downloadRemoteFileVia } from './remote-files.js';
import { trackedUpload } from './transfers.js';
import {
  isCommandPaletteShortcut,
  requestCommandPalette,
} from './command-registry.js';
import { clipboardFailure, writeClipboard } from './insecure-origin.js';

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
let clipboardDebugPaneID = 0;

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

// cellAt maps a pointer position to a buffer cell {row, col} of term, whose
// screen element is in host; null off-screen or before layout.
export function cellAt(term, host, clientX, clientY) {
  const screen = host?.querySelector?.('.xterm-screen');
  const rect = screen?.getBoundingClientRect?.();
  if (!rect || !rect.width || !rect.height || !term.cols || !term.rows) return null;
  const clamp = (v, max) => Math.max(0, Math.min(max - 1, v));
  const col = clamp(Math.floor((clientX - rect.left) / (rect.width / term.cols)), term.cols);
  const row = clamp(Math.floor((clientY - rect.top) / (rect.height / term.rows)), term.rows);
  return { row: row + (term.buffer?.active?.viewportY || 0), col };
}

// hiddenCell reports whether a cell is drawn so it cannot be read: concealed
// (SGR 8), or a foreground set to exactly its background colour.
function hiddenCell(cell) {
  if (cell.isInvisible()) return true;
  const mode = cell.getFgColorMode?.();
  return !!mode && mode === cell.getBgColorMode?.() && cell.getFgColor() === cell.getBgColor();
}

// rowCells is a buffer row's cells from start to end (exclusive), one string
// per column as drawn: a hidden cell reads as a space, a wide character's
// spacer column as nothing.
function rowCells(line, start, end) {
  const out = [];
  for (let x = start; x < end; x++) {
    const cell = line.getCell(x);
    if (!cell) break;
    out.push(cell.getWidth() === 0 ? '' : hiddenCell(cell) ? ' ' : (cell.getChars() || ' '));
  }
  return out;
}

// selectedText is the text of the cells a pointer gesture covered on term:
// rows break with a newline unless the buffer marks the next one as a soft
// wrap, and each row drops its trailing padding. clicks 2 takes the word
// under the pointer and 3 its row, as terminal multi-clicks do. null when
// nothing readable was covered.
export function selectedText(term, from, to, clicks = 1) {
  const buffer = term?.buffer?.active;
  if (!buffer || !from || !to || typeof buffer.getLine !== 'function') return null;
  const [a, b] = (from.row < to.row || (from.row === to.row && from.col <= to.col)) ? [from, to] : [to, from];
  const width = term.cols;
  if (clicks >= 2) {
    const line = buffer.getLine(b.row);
    if (!line) return null;
    const cols = rowCells(line, 0, width);
    if (clicks >= 3) return cols.join('').replace(/ +$/, '') || null;
    // Words span columns, not string offsets: a cell may hold several code
    // points, and a wide character's spacer column holds none.
    const blank = (c) => c !== '' && /^\s+$/.test(c);
    let at = b.col;
    while (at > 0 && cols[at] === '') at--;
    if (at >= cols.length || blank(cols[at])) return null;
    let l = at; let r = at;
    while (l > 0 && !blank(cols[l - 1])) l--;
    while (r < cols.length - 1 && !blank(cols[r + 1])) r++;
    return cols.slice(l, r + 1).join('');
  }
  let out = '';
  for (let y = a.row; y <= b.row; y++) {
    const line = buffer.getLine(y);
    if (!line) return null;
    const row = rowCells(line, y === a.row ? a.col : 0, y === b.row ? b.col + 1 : width).join('');
    const next = y < b.row ? buffer.getLine(y + 1) : null;
    out += next?.isWrapped ? row : row.replace(/ +$/, '') + (y < b.row ? '\n' : '');
  }
  return out || null;
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

// Arbitrary file paste and drop are for agent terminals only: the agent reads
// the pasted path as an attachment. A plain shell keeps image paste but takes
// no other files (agentd enforces the same split).
export function isAgentTerminalPath(path) {
  if (typeof path !== 'string') return false;
  if (path.startsWith('/api/open-window-ws/') || path.startsWith('/api/spawn-focus-ws/')) return true;
  if (!path.startsWith('/api/federation/terminal?')) return false;
  const query = new URLSearchParams(path.slice(path.indexOf('?') + 1));
  return query.has('agent') && !query.has('shell');
}

// terminalPathToken quotes an uploaded path the way a desktop terminal quotes
// a dropped file, so a name with spaces or quotes stays one argument.
export function terminalPathToken(path) {
  return /^[\w@%+=:,./-]+$/.test(path) ? path : `'${path.replaceAll("'", "'\\''")}'`;
}

// transferFiles collects the files a paste or drop carries. Without anyFile
// only PNG/JPEG/WebP images count; other image types report unsupported.
// Folders cannot be uploaded and are reported, not read.
export function transferFiles(dt, anyFile) {
  const files = [];
  let unsupported = false;
  let folders = false;
  if (!dt) return { files, unsupported, folders };
  const seen = new Set();
  const add = (file, entry) => {
    if (!file) return;
    const key = `${file.name || ''}|${file.size}|${file.type}`;
    if (seen.has(key)) return;
    seen.add(key);
    if (entry && entry.isDirectory) { folders = true; return; }
    if (!anyFile) {
      if (!String(file.type || '').startsWith('image/')) return;
      if (!IMAGE_TYPES.has(file.type)) { unsupported = true; return; }
    }
    files.push(file);
  };
  // Items first: only an item reveals that a dropped entry is a folder.
  if (dt.items) {
    for (let i = 0; i < dt.items.length; i++) {
      const item = dt.items[i];
      if (item.kind === 'file') add(item.getAsFile(), item.webkitGetAsEntry?.());
    }
  }
  if (dt.files) {
    for (let i = 0; i < dt.files.length; i++) add(dt.files[i]);
  }
  return { files, unsupported, folders };
}

// A clipboard screenshot arrives as a generic "image.png"; give it a unique
// name. A named file keeps its own name on an agent terminal, while a shell
// terminal's image-only endpoint wants the extension to match the MIME type.
function uploadName(file, i, stamp, anyFile) {
  const ext = IMAGE_TYPES.get(file.type);
  if (ext && (!anyFile || !file.name || /^image\.\w+$/i.test(file.name))) return `pasted-image-${stamp}-${i + 1}.${ext}`;
  return file.name || `pasted-file-${stamp}-${i + 1}`;
}

async function uploadFiles(files, signal, terminalPath, anyFile, uploadImpl) {
  const fd = new FormData();
  const stamp = Date.now();
  files.forEach((file, i) => fd.append('file', file, uploadName(file, i, stamp, anyFile)));
  // A remote terminal's upload is proxied to the peer under /api/federation/,
  // which a peer-view page keeps local (remote-node.js rewrites other /api/).
  const remote = terminalPath.startsWith('/api/federation/terminal?');
  const base = remote ? '/api/federation/terminal-attachments' : '/api/terminal-attachments';
  const endpoint = `${base}?terminal=${encodeURIComponent(terminalPath)}`;
  // Terminal upload routes stay local on a peer's page, so no routedURL.
  const res = await uploadImpl(endpoint, {
    body: fd, files, signal, scope: terminalPath,
    finishing: remote ? 'Sending to the remote node…' : 'Finishing…',
  });
  if (!res.ok) {
    const text = await res.text().catch(() => '');
    let error = null;
    try { error = JSON.parse(text); } catch (_) { /* plain-text error */ }
    // A node from before arbitrary file paste refuses non-images this way.
    if (remote && res.status === 415 && error?.code === 'image') {
      throw new Error('remote node accepts images only (it may need a newer tclaude)');
    }
    throw new Error(error?.error || text || `HTTP ${res.status}`);
  }
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
  // uploadImpl posts pasted/dropped files; the default lists the upload for
  // the pane's progress card (transfer-card.js).
  uploadImpl = trackedUpload,
  downloadFile = null,
  // A watch-only remote terminal takes no input, so no file upload either;
  // file downloads from a remote node are not offered (a separate grant).
  canInput = () => true,
  fileDownloads = true,
  // remoteFileViewer, on a remote terminal, returns the live viewer ID while
  // the peer shares this terminal's files (sessions.files.read), else ''.
  // Only paths visible in the output download: an OSC 8 link's label need not
  // match its target, and the peer controls the whole byte stream.
  remoteFileViewer = null,
  // OSC 52 clipboard writes are trusted from the local tmux, which filters
  // pane applications. A peer controls a remote terminal's whole byte stream
  // and sees the pointer, so there ('selection') an OSC 52 is only the
  // signal that the remote tmux copied after a pointer selection: the
  // clipboard gets the cells the operator selected, read here at mouseup,
  // never the peer's text. false never writes from the stream.
  oscClipboard = true,
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
  const debugPane = ++clipboardDebugPaneID;
  // Opt in from DevTools with window.__tclaudeClipboardDebug = true.
  // Log only routing/state metadata, never clipboard text or terminal paths.
  const debugCopy = (event, details = {}) => {
    if (globalThis.__tclaudeClipboardDebug !== true) return;
    console.info('[tclaude-copy]', JSON.stringify({
      pane: debugPane, event,
      activation: globalThis.navigator?.userActivation?.isActive,
      focused: ownerDocument.hasFocus?.(),
      ...details,
    }));
  };

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
    debugCopy('finished', { success: token.result });
    if (token.timer) clearTimeout(token.timer);
    pendingTmuxCopy = null;
    if (activeTmuxClipboardCopy === token) activeTmuxClipboardCopy = null;
    flash(token.result ? 'copied' : `tmux copied; ${clipboardFailure('browser clipboard permission denied')}`);
  }

  function armTmuxClipboardFromGesture(selection = null) {
    if (!oscClipboard) return;
    // A remote copy needs the cells a pointer selected; a key chord has none.
    if (oscClipboard === 'selection' && !selection) return;
    if (activeTmuxClipboardCopy) activeTmuxClipboardCopy.cancel();
    const deferred = beginGestureClipboardWrite();
    debugCopy('armed', {
      deferred: !!deferred,
      secureContext: globalThis.isSecureContext,
      clipboardAPI: !!globalThis.navigator?.clipboard,
    });
    const token = { deferred, selection, timer: null, oscReceived: false, result: null };
    token.cancel = () => {
      debugCopy('canceled', { oscReceived: token.oscReceived });
      if (token.timer) clearTimeout(token.timer);
      if (token.deferred) token.deferred.cancel();
      if (pendingTmuxCopy === token) pendingTmuxCopy = null;
      if (activeTmuxClipboardCopy === token) activeTmuxClipboardCopy = null;
    };
    token.timer = setTimeout(() => {
      if (activeTmuxClipboardCopy !== token) return;
      debugCopy('timeout');
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
    debugCopy('mousedown', {
      button: event.button, shift: event.shiftKey, alt: event.altKey,
      ctrl: event.ctrlKey, meta: event.metaKey, trusted: event.isTrusted,
    });
    if (event.button !== 0 || event.altKey || event.shiftKey || event.ctrlKey || event.metaKey) {
      tmuxDrag = null;
      return;
    }
    tmuxDrag = { x: event.clientX, y: event.clientY, moved: false,
      cell: oscClipboard === 'selection' ? cellAt(term, host, event.clientX, event.clientY) : null };
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
    const mouseTrackingMode = term.modes.mouseTrackingMode;
    const arm = shouldArmTmuxClipboard(drag, event, mouseTrackingMode);
    if (drag) debugCopy('mouseup', {
      moved: drag.moved, clicks: event.detail, mouseTrackingMode, arm,
      button: event.button, shift: event.shiftKey, alt: event.altKey,
      ctrl: event.ctrlKey, meta: event.metaKey, trusted: event.isTrusted,
    });
    if (!arm) return;
    // This document-capture listener runs before xterm forwards mouseup to
    // tmux. Arm the permission-sensitive write now; OSC 52 resolves it later.
    // A remote selection is read now, as the operator sees it, before the
    // peer can redraw.
    if (oscClipboard === 'selection') {
      const selected = selectedText(term, drag.cell, cellAt(term, host, event.clientX, event.clientY), Number(event.detail) || 1);
      if (!selected) return;
      armTmuxClipboardFromGesture(selected);
      return;
    }
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
    else flash(`copy failed — ${clipboardFailure('clipboard permission denied')}`);
    term.focus();
  }

  async function downloadHostFile(path) {
    // A remote terminal's file comes through /api/federation/, which a peer's
    // view keeps on this node (remote-node.js): it downloads there too.
    if (remoteFileViewer) { await downloadRemoteFile(path); return; }
    // remote-node.js routes fetch only; this anchor download would read this
    // node's agentd under a peer's marker.
    if (globalThis.__tclaudeRemoteNode?.id) { flash('download not available in a peer view yet'); return; }
    if (!fileDownloads) { flash('downloading files from a remote node is not available yet'); return; }
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

  // downloadRemoteFile fetches a path from the peer through this node's
  // pinned viewer (remote-files.js owns the preflight and refusal wording).
  function downloadRemoteFile(path) {
    return downloadRemoteFileVia({ terminal: terminalPath, viewer: remoteFileViewer(), path, fetchImpl, documentRef: ownerDocument });
  }

  // A file link names a path on the terminal's host. On a remote terminal that
  // host is the peer: only a visible path, and only while the live view holds
  // the peer's file grant, is offered (no download hint otherwise).
  const linkFor = (raw, visible = false) => {
    const link = safeTerminalLink(raw);
    if (!link || link.kind !== 'file') return link;
    // A remote path downloads exactly as shown: no percent-decoding, which
    // could make the target differ from the visible text.
    if (remoteFileViewer) return visible && remoteFileViewer() && !String(raw).includes('%') ? link : null;
    return fileDownloads ? link : null;
  };
  const activateLink = (event, raw, visible = false) => {
    const link = linkFor(raw, visible);
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
  const showLinkTarget = (raw, visible = false) => {
    if (!setStatus) return;
    if (statusTimer) { clearTimeout(statusTimer); statusTimer = null; }
    const link = linkFor(raw, visible);
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
  if (fileDownloads) {
    disposables.push(term.registerLinkProvider(
      visibleLocalFileLinkProvider(term, linkHandler),
    ));
  } else if (remoteFileViewer) {
    // Visible paths link only while the peer shares files with this view.
    const visibleHandler = {
      activate: (event, text) => activateLink(event, text, true),
      hover: (event, text) => showLinkTarget(text, true),
      leave: () => clearLinkTarget(),
    };
    const provider = visibleLocalFileLinkProvider(term, visibleHandler);
    disposables.push(term.registerLinkProvider({
      provideLinks(y, callback) {
        if (!remoteFileViewer()) { callback([]); return; }
        provider.provideLinks(y, callback);
      },
    }));
  }

  disposables.push(term.onSelectionChange(updateCopyButton));
  // tmux's normal mouse/copy-mode path stores the text in a tmux buffer and
  // emits OSC 52 to the attached terminal. Turning that standard sequence into
  // a browser clipboard write gives unmodified drag the same end result as a
  // native terminal, without polling tmux or adding a second server protocol.
  disposables.push(term.parser.registerOscHandler(52, (payload) => {
    if (!oscClipboard) return true;
    const text = decodeOSC52(payload);
    debugCopy('osc52', {
      valid: text !== null,
      textLength: text?.length,
      pending: !!pendingTmuxCopy,
      ownsCopy: !!pendingTmuxCopy && activeTmuxClipboardCopy === pendingTmuxCopy,
    });
    // Ignore unsolicited OSC 52 completely. With tmux's default
    // set-clipboard=external, pane applications are filtered by tmux already;
    // this armed-only gate adds defense in depth (including when a user has
    // opted into set-clipboard=on) and prevents background clipboard poisoning.
    if (text !== null && pendingTmuxCopy && activeTmuxClipboardCopy === pendingTmuxCopy) {
      const token = pendingTmuxCopy;
      // A remote copy writes the cells the operator selected, never the
      // peer's text: its OSC 52 only says the copy happened.
      const copied = token.selection ?? text;
      if (token.timer) {
        clearTimeout(token.timer);
        token.timer = null;
      }
      token.oscReceived = true;
      if (token.deferred) {
        token.deferred.resolve(copied);
        finishPendingTmuxCopy(token);
      } else {
        debugCopy('fallback-write');
        // Older browsers cannot hold a promise-backed ClipboardItem open from
        // mouseup, but still get a best-effort write while the armed gesture's
        // transient activation may remain live.
        void writeClipboard(copied).then((ok) => {
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

  const acceptsFiles = isAgentTerminalPath(terminalPath);

  // uploadAndPaste stages files next to the session and types their paths into
  // the prompt, exactly as a desktop terminal does for a dropped file.
  const uploadAndPaste = async (files) => {
    if (!canInput()) { flash('watch-only: this terminal takes no input'); return; }
    if (uploadPending) { flash('an upload is still in progress'); return; }
    const key = files.map(f => `${f.name || ''}|${f.size}|${f.type}`).join(',');
    const now = performance.now();
    if (key === lastPasteKey && now - lastPasteAt < PASTE_REPEAT_MS) return;
    lastPasteKey = key;
    lastPasteAt = now;
    uploadPending = true;
    const myGeneration = generation;
    const controller = new AbortController();
    uploadController = controller;
    const noun = files.every(f => IMAGE_TYPES.has(f.type)) ? 'image' : 'file';
    flash(files.length === 1 ? `uploading ${noun}…` : `uploading ${files.length} ${noun}s…`, 30000);
    try {
      const paths = await uploadFiles(files, controller.signal, terminalPath, acceptsFiles, uploadImpl);
      // Session changes invalidate pending uploads, so an old upload cannot
      // paste its path through a replacement session's WebSocket.
      if (controller.signal.aborted || generation !== myGeneration) return;
      if (!paths.length) throw new Error('upload returned no file path');
      term.paste(paths.map(terminalPathToken).join(' ') + ' ');
      flash(paths.length === 1 ? `${noun} pasted` : `${paths.length} ${noun}s pasted`);
    } catch (error) {
      if (controller.signal.aborted || error && error.name === 'AbortError') {
        // Cancelled from the progress card: replace the 30s "uploading" status.
        if (generation === myGeneration) flash('upload cancelled');
        return;
      }
      const detail = String(error && error.message || error).replace(/\s+/g, ' ').slice(0, 120);
      flash(`${noun} paste failed: ${detail}`, 5000);
    } finally {
      if (uploadController === controller) {
        uploadController = null;
        uploadPending = false;
        if (generation === myGeneration) term.focus();
      }
    }
  };

  const onPaste = (event) => {
    const { files, unsupported } = transferFiles(event.clipboardData, acceptsFiles);
    if (!files.length) {
      if (unsupported) { event.preventDefault(); flash('unsupported image type — use PNG, JPEG, or WebP'); }
      return; // ordinary text paste remains xterm's responsibility
    }
    event.preventDefault();
    event.stopPropagation();
    uploadAndPaste(files);
  };
  // Capture on the host so file data is claimed before xterm's textarea paste
  // listener; text-only events are untouched and continue into xterm normally.
  host.addEventListener('paste', onPaste, true);

  // Dropping files from a file manager. Every file drag over the terminal is
  // claimed, so a refused drop never falls through to the browser opening the
  // file in place of the dashboard. Tab drags carry no Files and pass through.
  const draggingFiles = (event) => Array.from(event.dataTransfer?.types || []).includes('Files');
  const dropAllowed = () => acceptsFiles && canInput();
  const setDropActive = (on) => host.classList?.toggle('terminal-file-drop', on);
  const onDragEnter = (event) => {
    if (!draggingFiles(event)) return;
    if (!acceptsFiles) flash('file drop works in agent terminals only');
    else if (!canInput()) flash('watch-only: this terminal takes no input');
  };
  const onDragOver = (event) => {
    if (!draggingFiles(event)) return;
    event.preventDefault();
    const allowed = dropAllowed();
    event.dataTransfer.dropEffect = allowed ? 'copy' : 'none';
    setDropActive(allowed);
  };
  const onDragLeave = (event) => {
    if (!host.contains(event.relatedTarget)) setDropActive(false);
  };
  const onDrop = (event) => {
    if (!draggingFiles(event)) return;
    event.preventDefault();
    event.stopPropagation();
    setDropActive(false);
    if (!dropAllowed()) return;
    const { files, folders } = transferFiles(event.dataTransfer, true);
    if (!files.length) { flash(folders ? 'folders cannot be pasted — drop files instead' : 'no files to paste'); return; }
    uploadAndPaste(files);
  };
  host.addEventListener('dragenter', onDragEnter);
  host.addEventListener('dragover', onDragOver);
  host.addEventListener('dragleave', onDragLeave);
  host.addEventListener('drop', onDrop);

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
      host.removeEventListener('dragenter', onDragEnter);
      host.removeEventListener('dragover', onDragOver);
      host.removeEventListener('dragleave', onDragLeave);
      host.removeEventListener('drop', onDrop);
      setDropActive(false);
      if (copyButton) copyButton.removeEventListener('click', copySelection);
      for (const d of disposables) { try { d.dispose(); } catch (_) { /* already disposed */ } }
    },
  };
}
// dashboard-imperative-boundary: browser-io
