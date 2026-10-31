// Browser APIs that exist only in a secure context, with what still works
// without one. A dashboard opened over plain http on a LAN or tailnet address
// is a supported setup and has neither crypto.randomUUID nor
// navigator.clipboard, so call these rather than the APIs themselves.

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
export function clipboardFailure() {
  return globalThis.isSecureContext === false
    ? 'the browser refused it; the clipboard is limited over plain HTTP except on localhost'
    : 'the clipboard is not available here';
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
