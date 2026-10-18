// transfers.js — byte progress for the dashboard's file uploads: files pasted
// or dropped into a terminal, spawn attachments, and message attachments.
//
// fetch cannot report upload progress, so a tracked upload goes through
// XMLHttpRequest and resolves to an ordinary Response. Each upload is listed
// here while it runs; transfer-card.js renders the list (a terminal's own over
// its pane, the rest in a page-level tray). An upload that finishes within
// SHOW_AFTER_MS never shows, so a pasted screenshot does not flash a card.

const SHOW_AFTER_MS = 400;
const DONE_LINGER_MS = 1200;

let nextID = 0;
let items = [];
const listeners = new Set();

function emit() {
  for (const listener of [...listeners]) {
    try { listener(items); } catch (_) { /* one view's failure is not the upload's */ }
  }
}

function update(id, patch) {
  if (!items.some((t) => t.id === id)) return;
  items = items.map((t) => (t.id === id ? { ...t, ...patch } : t));
  emit();
}

function remove(id) {
  const before = items.length;
  items = items.filter((t) => t.id !== id);
  if (items.length !== before) emit();
}

export function subscribeTransfers(listener) {
  listeners.add(listener);
  listener(items);
  return () => { listeners.delete(listener); };
}

// setTransferMinimized collapses a card to its progress pill, or expands it.
export function setTransferMinimized(id, minimized) {
  update(id, { minimized });
}

// fileProgress spreads the bytes sent over the files in body order (0..1 per
// file). FormData sends its parts one after another, so the first file
// finishes before the second starts.
export function fileProgress(files, sent) {
  let before = 0;
  return files.map((file) => {
    const size = file.size || 0;
    const done = size ? Math.min(Math.max(sent - before, 0), size) / size : (sent >= before ? 1 : 0);
    before += size;
    return done;
  });
}

function abortError() {
  return new DOMException('upload cancelled', 'AbortError');
}

function responseHeaders(raw) {
  const headers = new Headers();
  for (const line of String(raw || '').trim().split(/[\r\n]+/)) {
    const at = line.indexOf(':');
    if (at > 0) {
      try { headers.append(line.slice(0, at).trim(), line.slice(at + 1).trim()); } catch (_) { /* skip a malformed header */ }
    }
  }
  return headers;
}

// xhrUpload POSTs body and resolves to a Response, reporting upload bytes to
// onProgress(loaded, total). An aborted signal rejects with an AbortError, as
// fetch does.
export function xhrUpload(url, { body, signal, onProgress, XHR = globalThis.XMLHttpRequest } = {}) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(abortError()); return; }
    const xhr = new XHR();
    const onAbort = () => xhr.abort();
    const settle = () => signal?.removeEventListener('abort', onAbort);
    xhr.open('POST', url);
    xhr.withCredentials = true;
    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress?.(event.loaded, event.total);
    };
    xhr.onload = () => {
      settle();
      const empty = [101, 204, 205, 304].includes(xhr.status);
      resolve(new Response(empty ? null : xhr.responseText, {
        status: xhr.status, headers: responseHeaders(xhr.getAllResponseHeaders()),
      }));
    };
    xhr.onerror = () => { settle(); reject(new TypeError('upload failed: network error')); };
    xhr.onabort = () => { settle(); reject(abortError()); };
    signal?.addEventListener('abort', onAbort, { once: true });
    xhr.send(body);
  });
}

// routedURL is where the page's own fetch would send url. A peer's per-node
// page reroutes its API calls (remote-node.js); XMLHttpRequest bypasses that
// wrapper, so a tracked upload asks for the same route itself.
export function routedURL(url) {
  return globalThis.__tclaudeRemoteNode?.route?.(url) || url;
}

// trackedUpload uploads body like xhrUpload while listing it for the progress
// cards. files are the File objects in body order; scope says where the card
// shows ('dialog' for the page tray, else a terminal path); finishing names
// the wait after the last byte (a remote terminal's hop to its node).
export function trackedUpload(url, {
  body, files = [], scope = 'dialog', title = '', finishing = 'Finishing…', signal,
  XHR, setTimeoutImpl = globalThis.setTimeout, clearTimeoutImpl = globalThis.clearTimeout,
} = {}) {
  const id = ++nextID;
  const controller = new AbortController();
  const cancel = () => controller.abort();
  if (signal?.aborted) cancel();
  signal?.addEventListener('abort', cancel, { once: true });
  const list = files.map((file) => ({ name: file.name || 'file', size: file.size || 0 }));
  const total = list.reduce((sum, file) => sum + file.size, 0);
  items = [...items, {
    id, scope, title, files: list, total, sent: 0, finishing,
    phase: 'uploading', visible: false, minimized: false, cancel,
  }];
  emit();
  const showTimer = setTimeoutImpl(() => update(id, { visible: true }), SHOW_AFTER_MS);
  const done = () => {
    clearTimeoutImpl(showTimer);
    signal?.removeEventListener('abort', cancel);
  };
  return xhrUpload(url, {
    body, signal: controller.signal, XHR,
    onProgress: (loaded, length) => update(id, {
      sent: length ? Math.min(loaded / length, 1) * total : 0,
      phase: length && loaded >= length ? 'finishing' : 'uploading',
    }),
  }).then((response) => {
    done();
    const shown = items.find((t) => t.id === id)?.visible;
    // A finished upload lingers briefly as done; a failed one leaves at once,
    // since its caller reports the error where the upload was started.
    if (response.ok && shown) {
      update(id, { phase: 'done', sent: total, cancel: null });
      setTimeoutImpl(() => remove(id), DONE_LINGER_MS);
    } else {
      remove(id);
    }
    return response;
  }, (error) => {
    done();
    remove(id);
    throw error;
  });
}
