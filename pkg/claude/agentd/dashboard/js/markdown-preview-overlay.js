// MarkdownAttachmentPreview owns the Markdown document viewer used by both the
// quick notification drawer and Messages, the way ImageAttachmentPreview owns
// the one-image viewer. The attachment card keeps its download link; only this
// overlay is shared between the two surfaces.

import { Fragment, h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';
import { MarkdownDocument } from './markdown-document.js';

const html = htm.bind(h);

// A notification can publish several files, each with its own download URL.
// Fall back to the legacy single-artifact route for a snapshot that predates
// the per-file one.
function attachmentURL(messageID, attachment) {
  return attachment?.url || `/api/human-messages/${encodeURIComponent(messageID)}/attachment`;
}

function safeIDPart(value) {
  return String(value ?? '').replace(/[^a-zA-Z0-9_-]/g, '-');
}

function attachmentSize(bytes) {
  const size = Number(bytes || 0);
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(size < 10 * 1024 ? 1 : 0)} KiB`;
  return `${(size / (1024 * 1024)).toFixed(size < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
}

function LoadState({ state }) {
  if (state === 'loading') {
    return html`<div class="markdown-preview-state" role="status">Loading document…</div>`;
  }
  if (state === 'gone') {
    return html`<div class="markdown-preview-state" role="alert">
      <strong>This attachment is no longer available.</strong>
      <span>The stored file was removed before it could be opened.</span>
    </div>`;
  }
  if (state === 'error') {
    return html`<div class="markdown-preview-state" role="alert">
      <strong>The document could not be read.</strong>
      <span>The file may be damaged or no longer available. Download the original to inspect it.</span>
    </div>`;
  }
  return null;
}

export function MarkdownAttachmentPreview({ messageID, attachment, surface = 'attachment' }) {
  const [open, setOpen] = useState(false);
  const [loadState, setLoadState] = useState('idle');
  const [source, setSource] = useState('');
  const [showSource, setShowSource] = useState(false);
  const overlayRef = useRef(null);
  const closeRef = useRef(null);
  const href = attachment ? attachmentURL(messageID, attachment) : '';
  const filename = attachment?.filename || 'document.md';
  // Several viewers can share a message, so the attachment id keeps the
  // dialog's labelling ids unique.
  const titleID = `markdown-preview-title-${surface}-${safeIDPart(messageID)}-${safeIDPart(attachment?.id ?? 0)}`;
  const detailsID = `${titleID}-details`;

  const close = useCallback(() => setOpen(false), []);
  const { dialogRef } = useDialogFocus({
    open,
    initialFocusRef: closeRef,
    onEscape: close,
    shouldHandle: () => isTopmostOverlay(overlayRef.current),
  });

  useEffect(() => {
    if (!open) return undefined;
    let active = true;
    setLoadState('loading');
    setShowSource(false);
    // Unlike the image viewer there is no HEAD preflight: the document has to
    // be read anyway, so one GET both fetches it and distinguishes a file the
    // cleanup already removed.
    fetch(href, { credentials: 'same-origin' })
      .then(async (response) => {
        if (!active) return;
        if (response.status === 410) { setLoadState('gone'); return; }
        if (!response.ok) { setLoadState('error'); return; }
        const text = await response.text();
        if (!active) return;
        setSource(text);
        setLoadState('ready');
      })
      .catch(() => {
        if (active) setLoadState('error');
      });
    return () => { active = false; };
  }, [href, open]);

  if (!attachment?.markdown) return null;

  return html`<${Fragment}>
    <button type="button" class="human-attachment-markdown-trigger"
      onClick=${() => setOpen(true)} aria-label=${`Read ${filename}`}>
      <span class="human-attachment-markdown-icon" aria-hidden="true">¶</span>
      <span class="human-attachment-markdown-label">Read</span>
    </button>
    ${open && html`<div ref=${overlayRef} class="modal-overlay show markdown-preview-overlay"
      onMouseDown=${(event) => {
        if (event.target === event.currentTarget && isTopmostOverlay(overlayRef.current)) {
          event.preventDefault();
          close();
        }
      }}>
      <div ref=${dialogRef} class="markdown-preview-dialog" role="dialog" aria-modal="true"
        aria-labelledby=${titleID} aria-describedby=${detailsID}>
        <header class="markdown-preview-header">
          <div class="markdown-preview-heading">
            <h2 id=${titleID}>${filename}</h2>
            <span>${attachment.content_type || 'text/markdown'} · ${attachmentSize(attachment.size_bytes)}</span>
          </div>
          <div class="markdown-preview-header-actions">
            ${loadState !== 'gone' && html`<a class="markdown-preview-download" href=${href} download=${filename}
              title="Download this agent-published file">Download</a>`}
            <button ref=${closeRef} type="button" class="markdown-preview-close"
              onClick=${close} aria-label="Close document viewer" title="Close (Esc)">×</button>
          </div>
        </header>
        <div class="markdown-preview-stage" tabIndex="0" role="region"
          aria-label=${`${filename} contents`}>
          <${LoadState} state=${loadState} />
          ${loadState === 'ready' && (showSource
            ? html`<pre class="markdown-preview-source">${source}</pre>`
            : html`<${MarkdownDocument} source=${source} />`)}
        </div>
        <footer class="markdown-preview-footer" id=${detailsID}>
          <span class="markdown-preview-footer-file">${filename}</span>
          <span class="markdown-preview-footer-size">${attachmentSize(attachment.size_bytes)}</span>
          <span class="grow"></span>
          <button type="button" class="markdown-preview-toggle"
            disabled=${loadState !== 'ready'} aria-pressed=${showSource}
            onClick=${() => setShowSource((value) => !value)}>
            ${showSource ? 'Show rendered' : 'Show source'}
          </button>
        </footer>
      </div>
    </div>`}
  </${Fragment}>`;
}
