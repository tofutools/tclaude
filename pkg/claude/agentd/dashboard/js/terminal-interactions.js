// terminal-interactions.js — shared native-terminal affordances for every
// dashboard xterm surface: selection/copy, safe links, and clipboard images.

const IMAGE_TYPES = new Map([
  ['image/png', 'png'],
  ['image/jpeg', 'jpg'],
  ['image/webp', 'webp'],
]);
const PASTE_REPEAT_MS = 1000;
const SELECT_HINT = 'Option-drag to select on macOS; Shift-drag on Linux/Windows';

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
export function attachTerminalInteractions({ term, host, copyButton, setStatus, baseStatus = () => '' }) {
  let statusTimer = null;
  let uploadPending = false;
  let uploadController = null;
  let generation = 0;
  let lastPasteAt = 0;
  let lastPasteKey = '';
  const disposables = [];
  const oldTitle = host.title;
  host.title = `${SELECT_HINT}. Ctrl/Cmd+Shift+C copies.`;

  function flash(message, delay = 2200) {
    if (!setStatus) return;
    if (statusTimer) clearTimeout(statusTimer);
    setStatus(message);
    statusTimer = setTimeout(() => setStatus(baseStatus()), delay);
  }

  function updateCopyButton() {
    if (!copyButton) return;
    const selected = term.hasSelection();
    // Keep the control clickable even before a selection exists: clicking it
    // is the discoverable path to the platform-specific force-selection hint.
    copyButton.disabled = false;
    copyButton.dataset.hasSelection = selected ? 'true' : 'false';
    copyButton.title = selected
      ? 'Copy selected terminal text (Ctrl/Cmd+Shift+C)'
      : SELECT_HINT;
  }

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
    hover: () => { host.title = 'Ctrl/Cmd-click to open link'; },
    leave: () => { host.title = oldTitle || `${SELECT_HINT}. Ctrl/Cmd+Shift+C copies.`; },
    allowNonHttpProtocols: false,
  };
  term.options.linkHandler = linkHandler; // explicit OSC 8 hyperlinks
  if (globalThis.WebLinksAddon && globalThis.WebLinksAddon.WebLinksAddon) {
    term.loadAddon(new globalThis.WebLinksAddon.WebLinksAddon(
      (event, uri) => activateLink(event, uri), linkHandler,
    ));
  }

  disposables.push(term.onSelectionChange(updateCopyButton));
  updateCopyButton();
  if (copyButton) copyButton.addEventListener('click', copySelection);

  term.attachCustomKeyEventHandler((event) => {
    if (event.type !== 'keydown') return true;
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
    if (uploadController) uploadController.abort();
    uploadController = null;
    uploadPending = false;
    lastPasteAt = 0;
    lastPasteKey = '';
  }

  return {
    invalidate,
    dispose() {
      invalidate();
      if (statusTimer) clearTimeout(statusTimer);
      host.removeEventListener('paste', onPaste, true);
      if (copyButton) copyButton.removeEventListener('click', copySelection);
      for (const d of disposables) { try { d.dispose(); } catch (_) { /* already disposed */ } }
      host.title = oldTitle;
    },
  };
}
