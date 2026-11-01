// Browser APIs that exist only in a secure context, with what still works
// without one. A dashboard opened over plain http on a LAN or tailnet address
// is a supported setup and has neither crypto.randomUUID nor
// navigator.clipboard, so call these rather than the APIs themselves.
// dashboard-imperative-boundary: browser-io

// randomUUID is crypto.randomUUID, built from getRandomValues (available in
// every context) when that is missing.
export function randomUUID(source = globalThis.crypto) {
  if (typeof source?.randomUUID === 'function') return source.randomUUID();
  const bytes = new Uint8Array(16);
  source.getRandomValues(bytes);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
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

// clipboardFailure is the operator's explanation of a failed writeClipboard.
export function clipboardFailure(secureReason = 'the clipboard is not available here') {
  return globalThis.isSecureContext === false
    ? 'the browser refused it; the clipboard is limited over plain HTTP except on localhost'
    : secureReason;
}

// clipboardReadNote explains a menu that cannot read the clipboard: the paste
// key still can, because only it receives the text from the browser.
export function clipboardReadNote(keys, win = globalThis) {
  return win.isSecureContext === true
    ? `This browser can’t read the clipboard from a menu. Use ${keys}.`
    : `The menu can’t read the clipboard over plain HTTP except on localhost. Use ${keys}.`;
}

// dictationBlock says why dictation can't be used here, or null when it can:
// `note` for a line on the page and `why` for the longer tooltip. A browser without
// speech recognition comes first, since HTTPS would not help it. Otherwise a
// non-secure page gets its own answer, because the browser refuses the
// microphone there whatever the site permission says.
export function dictationBlock(supported, win = globalThis) {
  if (!supported) {
    return {
      note: 'Dictation isn’t available in this browser',
      why: 'This browser has no speech recognition. Dictation works in Chrome, Edge and Safari.',
    };
  }
  if (win.isSecureContext === false) {
    return {
      note: 'Dictation needs HTTPS or localhost',
      why: 'Browsers refuse the microphone on plain HTTP pages, and no site permission changes that. Open the dashboard on localhost, or over HTTPS such as tailscale serve.',
    };
  }
  return null;
}

// writeClipboard puts text on the clipboard and says whether it got there. Call
// it from the click: the legacy copy needs the user's gesture.
export async function writeClipboard(text) {
  if (globalThis.navigator?.clipboard?.writeText) {
    try {
      await globalThis.navigator.clipboard.writeText(text);
      return true;
    } catch (_) { /* denied permission: use legacy copy */ }
  }
  try { return legacyCopy(text); } catch (_) { return false; }
}
