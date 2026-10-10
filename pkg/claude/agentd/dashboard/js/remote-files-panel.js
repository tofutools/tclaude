// remote-files-panel.js — the remote terminal's file browser (see
// remote-files.js for the transport and refusal wording).
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { PREVIEW_MAX_BYTES, downloadRemoteFile, listRemoteDir, previewRemoteFile } from './remote-files.js';

const html = htm.bind(h);

const baseName = (p) => String(p).split('/').filter(Boolean).pop() || p;
const parentDir = (p) => { const parts = String(p).split('/').filter((x) => x && x !== '.'); parts.pop(); return parts.length ? parts.join('/') : '.'; };

export function fileSize(n) {
  if (!(n >= 0)) return '';
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1 << 20)).toFixed(1)} MiB`;
}

// RemoteFilesPanel browses the agent's project directory on the peer one
// level at a time: open a folder, preview a small text file, or download.
export function RemoteFilesPanel({ terminal, viewer, peer = 'the peer', fetchImpl = globalThis.fetch, documentRef = globalThis.document, onClose }) {
  const [dir, setDir] = useState('.');
  const [listing, setListing] = useState(null);
  const [tick, setTick] = useState(0);
  const [preview, setPreview] = useState(null);
  const [note, setNote] = useState('');
  useEffect(() => {
    let off = false;
    setListing(null);
    listRemoteDir({ terminal, viewer, path: dir, fetchImpl })
      .then((v) => { if (!off) setListing(v); })
      .catch((e) => { if (!off) setListing({ error: e?.message || String(e) }); });
    return () => { off = true; };
  }, [terminal, viewer, dir, tick]);
  const open = (path) => { setPreview(null); setNote(''); setDir(path); };
  const download = (path) => {
    setNote('');
    downloadRemoteFile({ terminal, viewer, path, fetchImpl, documentRef }).catch((e) => setNote(`${baseName(path)}: ${e?.message || e}`));
  };
  const show = (entry) => {
    setNote('');
    setPreview({ path: entry.path, loading: true });
    previewRemoteFile({ terminal, viewer, path: entry.path, fetchImpl })
      .then((v) => setPreview((p) => (p?.path === entry.path ? { path: entry.path, ...v } : p)))
      .catch((e) => setPreview((p) => (p?.path === entry.path ? { path: entry.path, error: e?.message || String(e) } : p)));
  };
  const entries = listing?.entries || [];
  const dirs = entries.filter((e) => e.kind === 'directory');
  const files = entries.filter((e) => e.kind !== 'directory');
  return html`<div class="term-files" id="term-files" role="region" aria-label=${`Files on ${peer}`}>
    <div class="term-files-head">
      <b>Files on ${peer}</b> <code id="term-files-dir">${dir}</code>
      ${dir !== '.' && html`<button type="button" class="term-files-link" id="term-files-up" onClick=${() => open(parentDir(dir))}>↑ up</button>`}
      <button type="button" class="term-files-link" id="term-files-refresh" onClick=${() => setTick((n) => n + 1)}>refresh</button>
      <span class="term-files-spacer"></span>
      <span class="term-files-muted">Secret files, symlinks and files over 32 MiB are not listed.</span>
      <button type="button" class="term-files-link" id="term-files-close" aria-label="Close files" onClick=${onClose}>×</button>
    </div>
    ${note && html`<div class="term-files-error" role="alert">${note}</div>`}
    ${preview ? html`<div class="term-files-preview" id="term-files-preview">
        <div class="term-files-head"><code>${preview.path}</code>
          <button type="button" class="term-files-link" id="term-files-preview-download" onClick=${() => download(preview.path)}>download</button>
          <button type="button" class="term-files-link" id="term-files-preview-back" onClick=${() => setPreview(null)}>← back to ${dir}</button></div>
        ${preview.loading ? html`<div class="term-files-muted">Loading…</div>`
          : preview.error ? html`<div class="term-files-error" role="alert">${preview.error}</div>`
          : preview.binary ? html`<div class="term-files-muted">This looks like a binary file; download it to open it.</div>`
          : preview.tooLarge ? html`<div class="term-files-muted">Too large to preview here; download it instead.</div>`
          : html`<pre class="term-files-text">${preview.text}</pre>`}
      </div>`
    : listing?.error ? html`<div class="term-files-error" role="alert" id="term-files-error">${listing.error}</div>`
    : !listing ? html`<div class="term-files-muted">Loading…</div>`
    : !entries.length ? html`<div class="term-files-muted" id="term-files-empty">Nothing to show in this directory.</div>`
    : html`<table class="term-files-table" id="term-files-list"><tbody>
        ${dirs.map((e) => html`<tr key=${e.path} data-path=${e.path}><td><button type="button" class="term-files-link" data-files="open" onClick=${() => open(e.path)}>📁 ${baseName(e.path)}/</button></td><td></td><td></td></tr>`)}
        ${files.map((e) => html`<tr key=${e.path} data-path=${e.path}><td>${baseName(e.path)}</td><td class="term-files-muted">${fileSize(e.size)}</td>
          <td class="term-files-acts">${e.size <= PREVIEW_MAX_BYTES && html`<button type="button" class="term-files-link" data-files="preview" onClick=${() => show(e)}>preview</button>`}
            <button type="button" class="term-files-link" data-files="download" onClick=${() => download(e.path)}>download</button></td></tr>`)}
      </tbody></table>
      ${listing.truncated && html`<div class="term-files-muted" id="term-files-truncated">Only the first 100 entries were checked; open a narrower directory to see more.</div>`}`}
  </div>`;
}
