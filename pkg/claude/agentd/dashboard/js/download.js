// The dashboard's one way to hand the browser a download. Every save goes
// through a short-lived <a download> clicked once: an authenticated
// same-origin URL streams to disk through the browser's own download path
// (the session cookie and Referer ride along for the auth check), and data
// already in memory becomes an object URL that is revoked shortly after.
// dashboard-imperative-boundary: browser-io

// downloadURL saves href. With no filename the endpoint's
// Content-Disposition names the file.
export function downloadURL(href, { filename = '', documentRef = globalThis.document } = {}) {
  const anchor = documentRef.createElement('a');
  anchor.href = href;
  anchor.download = filename;
  anchor.rel = 'noopener';
  anchor.style.display = 'none';
  documentRef.body.append(anchor);
  anchor.click();
  anchor.remove();
}

// downloadBlob saves data already in memory as filename. The object URL
// outlives the click by a second, so a browser that starts the save
// asynchronously still reads it.
export function downloadBlob(blob, filename, { documentRef = globalThis.document, windowRef = globalThis } = {}) {
  const url = windowRef.URL.createObjectURL(blob);
  downloadURL(url, { filename, documentRef });
  setTimeout(() => windowRef.URL.revokeObjectURL(url), 1000);
}

// downloadText saves text as filename (JSON by default).
export function downloadText(filename, text, { type = 'application/json', documentRef = globalThis.document, windowRef = globalThis } = {}) {
  downloadBlob(new windowRef.Blob([text], { type }), filename, { documentRef, windowRef });
}

// downloadJSON saves value as pretty-printed JSON with a trailing newline.
export function downloadJSON(filename, value, options = {}) {
  downloadText(filename, JSON.stringify(value, null, 2) + '\n', options);
}
