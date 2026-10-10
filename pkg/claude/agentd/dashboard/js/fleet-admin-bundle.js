import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

// ENTRY_BYTES is how much of one bundle entry a read asks for; the daemon caps
// it and pages larger entries (transcripts) with byte offsets.
export const ENTRY_BYTES = 256 * 1024;

function errText(error) { return error?.message || String(error); }

function size(n) {
  if (!(n >= 0)) return '—';
  return n < 1024 ? `${n} B` : n < 1 << 20 ? `${(n / 1024).toFixed(1)} KiB` : `${(n / (1 << 20)).toFixed(1)} MiB`;
}

// entryText renders one read for display, always as plain text: a complete
// JSON entry is pretty-printed, JSON lines one pretty value per line, anything
// else (or anything that does not parse) as it came. Bundle content is a
// peer's data and is never interpreted as markup.
export function entryText(kind, text, complete) {
  if (kind === 'json' && complete) {
    try { return JSON.stringify(JSON.parse(text), null, 2); } catch (_) { return text; }
  }
  if (kind === 'jsonl') {
    return text.split('\n').map((line) => {
      if (!line.trim()) return line;
      try { return JSON.stringify(JSON.parse(line), null, 2); } catch (_) { return line; }
    }).join('\n');
  }
  return text;
}

// BundleInspectDialog fetches and verifies an incoming offer's payload on
// this node, lists its entries and shows any of them as text, so the operator
// can read what a peer sent before downloading or importing it.
export function BundleInspectDialog({ offer, label, actions, toast, onImport, onDecline, onClose }) {
  const peer = label(offer.peer);
  const [entries, setEntries] = useState(null);
  const [open, setOpen] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let off = false;
    // A ready offer is already fetched and verified on this node.
    (offer.state === 'ready' ? Promise.resolve() : actions.fetchOffer(offer))
      .then(() => actions.offerContents(offer))
      .then((c) => { if (!off) setEntries(Array.isArray(c?.entries) ? c.entries : []); })
      .catch((e) => { if (!off) setEntries({ error: errText(e) }); });
    return () => { off = true; };
  }, []);
  // seq numbers reads: only the latest one may change the view, so a slow
  // earlier read (another entry, or a page of one no longer shown) is dropped.
  const seq = useRef(0);
  const read = (entry, offset = 0) => {
    const mine = ++seq.current;
    setBusy(true); setError('');
    actions.offerEntry(offer, entry.path, offset, ENTRY_BYTES)
      .then((r) => {
        if (mine !== seq.current) return;
        setOpen((prev) => {
          const text = String(r?.text || '');
          // A page continues the entry only exactly where the last one ended.
          const more = offset > 0 && prev?.path === entry.path && prev.next === offset;
          if (offset > 0 && !more) return prev;
          return { path: entry.path, kind: r?.kind || entry.kind, size: r?.size ?? entry.size, raw: more ? prev.raw + text : text, truncated: !!r?.truncated, next: r?.next_offset ?? null };
        });
      })
      .catch((e) => { if (mine === seq.current) setError(errText(e)); })
      .finally(() => { if (mine === seq.current) setBusy(false); });
  };
  const download = () => {
    setError('');
    actions.downloadOffer(offer)
      .then(() => toast(`Downloading ${peer}'s ${offer.offer?.type || 'bundle'} offer`, false))
      .catch((e) => setError(errText(e)));
  };
  const list = Array.isArray(entries) ? entries : [];
  const shown = useMemo(() => (open && open.kind !== 'binary' ? entryText(open.kind, open.raw, !open.truncated) : ''), [open]);
  return html`<${Overlay} id="fleet-bundle-inspect" labelledby="fleet-bundle-inspect-title" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-bundle-inspect-title">Inspect ${peer}'s ${offer.offer?.type || 'bundle'} offer</h3>
    <div class="muted">${offer.offer?.summary || ''} · ${size(offer.offer?.bytes)} · fetched and verified on this node; nothing is imported until you apply it.</div>
    ${entries?.error ? html`<div class="fa-danger" role="alert">${entries.error}</div>`
      : entries == null ? html`<div class="muted">Fetching and verifying…</div>`
      : !list.length ? html`<div class="muted">The bundle lists no entries.</div>`
      : html`<div class="fa-bi">
        <ul class="fa-bi-list" id="fleet-bundle-entries">${list.map((e) => html`<li key=${e.path}>
          <button type="button" class=${`fa-link${open?.path === e.path ? ' active' : ''}`} data-path=${e.path} onClick=${() => read(e)}>${e.path}</button>
          <span class="muted">${e.kind || ''} · ${size(e.size)}</span></li>`)}</ul>
        <div class="fa-bi-view">
          ${!open ? html`<div class="muted">Pick an entry to read it here.</div>`
            : open.kind === 'binary' ? html`<div class="muted" id="fleet-bundle-binary">${open.path} is binary (${size(open.size)}); it is not shown. Download the bundle to inspect it.</div>`
            : html`<pre class="fa-bi-text" id="fleet-bundle-text">${shown}</pre>
              ${open.truncated && html`<div class="fa-bi-more muted">Showing the first ${size(open.next ?? 0)} of ${size(open.size)}.
                ${open.next != null && html` <button type="button" class="fa-link" id="fleet-bundle-more" disabled=${busy} onClick=${() => read({ path: open.path, kind: open.kind, size: open.size }, open.next)}>Load more</button>`}</div>`}`}
        </div>
      </div>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons">
      <button type="button" id="fleet-bundle-download" disabled=${!Array.isArray(entries)} onClick=${download}>Download</button>
      <span class="spacer"></span>
      <button type="button" onClick=${onDecline}>Decline…</button>
      <button type="button" class="primary" id="fleet-bundle-import" onClick=${onImport}>Import…</button>
      <button type="button" onClick=${onClose}>Close</button>
    </div>
  </${Overlay}>`;
}
