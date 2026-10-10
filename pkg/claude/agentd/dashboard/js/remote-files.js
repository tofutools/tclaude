// remote-files.js — files on a peer's agent terminal: the refusal wording,
// one-level directory listing, a small text preview and the download, all
// through this node's daemon and the live terminal view's pinned viewer.
// DOM-free apart from the download anchor; the browser UI is
// remote-files-panel.js.

// REMOTE_FILE_ERRORS explain the stable refusals of a remote terminal file
// download or listing (the peer's or this node's), in the operator's terms.
export const REMOTE_FILE_ERRORS = Object.freeze({
  not_shared: 'the peer does not share files from this terminal (needs a sessions.files.read grant)',
  unsafe_path: 'not downloadable: outside the agent\'s working directory, a symlink, or a protected file (credentials, .env, .git/config)',
  root_too_broad: 'the agent runs in a home or system directory; the peer shares files only from a project directory',
  file_too_large: 'the file is over the 32 MiB download limit',
  viewer_closed: 'this terminal view is no longer live; reopen it to download',
  peer_offline: 'the peer is offline; try again when it reconnects',
  not_found: 'no such file under the agent\'s working directory',
  limit: 'another download from this peer is still running; try again shortly',
  timeout: 'the download took too long; try again',
});

// Statuses that name their refusal on their own; only an ambiguous 403 needs
// its body read (HEAD has none).
const REMOTE_FILE_STATUS = Object.freeze({ 404: 'not_found', 413: 'file_too_large', 429: 'limit', 503: 'peer_offline', 504: 'timeout' });

export function remoteFileError(code, status) {
  return REMOTE_FILE_ERRORS[code] || `download unavailable (${status || 'network error'})`;
}

// PREVIEW_MAX_BYTES caps what a preview transfers: past it, download instead.
export const PREVIEW_MAX_BYTES = 256 << 10;

export function remoteFileHref({ terminal, viewer, path, list = false }) {
  return `/api/federation/terminal-file?${new URLSearchParams({ terminal, viewer, path, ...(list ? { list: 'true' } : {}) })}`;
}

async function refusal(res) {
  const code = await res.json().then((b) => b?.code || '', () => '');
  return new Error(remoteFileError(code || REMOTE_FILE_STATUS[res.status], res.status));
}

// listRemoteDir reads one directory level: {entries:[{path,kind,size}],
// truncated}. The peer omits secrets, symlinks, special and oversized files.
export async function listRemoteDir({ terminal, viewer, path = '.', fetchImpl = globalThis.fetch }) {
  if (!viewer) throw new Error(REMOTE_FILE_ERRORS.not_shared);
  const res = await fetchImpl(remoteFileHref({ terminal, viewer, path, list: true }), { credentials: 'same-origin', cache: 'no-store' }).catch(() => null);
  if (!res) throw new Error(remoteFileError('', 0));
  if (!res.ok) throw await refusal(res);
  const body = await res.json();
  return { entries: Array.isArray(body?.entries) ? body.entries : [], truncated: body?.truncated === true };
}

// previewRemoteFile fetches a small file as text: {text} or {binary: true}.
export async function previewRemoteFile({ terminal, viewer, path, fetchImpl = globalThis.fetch }) {
  if (!viewer) throw new Error(REMOTE_FILE_ERRORS.not_shared);
  const res = await fetchImpl(remoteFileHref({ terminal, viewer, path }), { credentials: 'same-origin', cache: 'no-store' }).catch(() => null);
  if (!res) throw new Error(remoteFileError('', 0));
  if (!res.ok) throw await refusal(res);
  const bytes = new Uint8Array(await res.arrayBuffer());
  if (bytes.length > PREVIEW_MAX_BYTES) return { tooLarge: true };
  if (bytes.subarray(0, 8192).includes(0)) return { binary: true };
  return { text: new TextDecoder('utf-8', { fatal: false }).decode(bytes) };
}

// downloadRemoteFile saves a path from the peer through the pinned viewer.
// HEAD carries no error body, so a 403 preflight (several refusals share it)
// is read once more with an aborted GET for its stable code; other statuses
// name their refusal and are never retried as a GET, which would transfer the
// file in full if it had become available.
// dashboard-imperative-boundary: browser-io
export async function downloadRemoteFile({ terminal, viewer, path, fetchImpl = globalThis.fetch, documentRef = globalThis.document }) {
  if (!viewer) throw new Error(REMOTE_FILE_ERRORS.not_shared);
  const href = remoteFileHref({ terminal, viewer, path });
  const head = await fetchImpl(href, { method: 'HEAD', credentials: 'same-origin', cache: 'no-store' }).catch(() => null);
  if (!head?.ok) {
    if (head?.status !== 403) throw new Error(remoteFileError(REMOTE_FILE_STATUS[head?.status], head?.status));
    const ctl = new AbortController();
    const res = await fetchImpl(href, { credentials: 'same-origin', cache: 'no-store', signal: ctl.signal }).catch(() => null);
    let code = '';
    if (res && !res.ok) code = await res.json().then((b) => b?.code || '', () => '');
    ctl.abort();
    throw new Error(remoteFileError(code, res?.status || head?.status));
  }
  const anchor = documentRef.createElement('a');
  anchor.href = href;
  anchor.download = '';
  anchor.style.display = 'none';
  documentRef.body.append(anchor);
  anchor.click();
  anchor.remove();
}
