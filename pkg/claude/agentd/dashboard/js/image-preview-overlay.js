import { Fragment, h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';

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

function PreviewState({ state }) {
  if (state === 'checking' || state === 'loading') {
    return html`<div class="image-preview-state" role="status">Loading preview…</div>`;
  }
  if (state === 'gone') {
    return html`<div class="image-preview-state" role="alert">
      <strong>This attachment is no longer available.</strong>
      <span>The stored file was removed before it could be previewed.</span>
    </div>`;
  }
  if (state === 'error') {
    return html`<div class="image-preview-state" role="alert">
      <strong>Preview unavailable.</strong>
      <span>The file may be damaged or no longer available. Download the original to inspect it.</span>
    </div>`;
  }
  return null;
}

// ImageAttachmentPreview owns the one-image viewer used by both the quick
// notification drawer and Messages. The thumbnail remains in the attachment
// card; only this overlay is shared between the two surfaces.
export function ImageAttachmentPreview({ messageID, attachment, surface = 'attachment' }) {
  const [open, setOpen] = useState(false);
  const [loadState, setLoadState] = useState('idle');
  const [zoom, setZoom] = useState(1);
  const overlayRef = useRef(null);
  const closeRef = useRef(null);
  const href = attachment ? attachmentURL(messageID, attachment) : '';
  const filename = attachment?.filename || 'attachment';
  // Several previews can share a message, so the attachment id keeps the
  // dialog's labelling ids unique.
  const titleID = `image-preview-title-${surface}-${safeIDPart(messageID)}-${safeIDPart(attachment?.id ?? 0)}`;
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
    setLoadState('checking');
    setZoom(1);
    fetch(href, { method: 'HEAD', credentials: 'same-origin' })
      .then((response) => {
        if (!active) return;
        if (response.status === 410) setLoadState('gone');
        else if (!response.ok) setLoadState('error');
        else setLoadState('loading');
      })
      .catch(() => {
        if (active) setLoadState('error');
      });
    return () => { active = false; };
  }, [href, open]);

  if (!attachment?.previewable) return null;

  return html`<${Fragment}>
    <button type="button" class="human-attachment-preview-trigger"
      onClick=${() => setOpen(true)} aria-label=${`Preview ${filename}`}>
      <img class="human-attachment-preview-thumb" src=${href} alt="" loading="lazy" decoding="async" />
      <span class="human-attachment-preview-label">Preview</span>
    </button>
    ${open && html`<div ref=${overlayRef} class="modal-overlay show image-preview-overlay"
      onMouseDown=${(event) => {
        if (event.target === event.currentTarget && isTopmostOverlay(overlayRef.current)) {
          event.preventDefault();
          close();
        }
      }}>
      <div ref=${dialogRef} class="image-preview-dialog" role="dialog" aria-modal="true"
        aria-labelledby=${titleID} aria-describedby=${detailsID}>
        <header class="image-preview-header">
          <div class="image-preview-heading">
            <h2 id=${titleID}>${filename}</h2>
            <span>${attachment.content_type || 'image'} · ${attachmentSize(attachment.size_bytes)}</span>
          </div>
          <div class="image-preview-header-actions">
            ${loadState !== 'gone' && html`<a class="image-preview-download" href=${href} download=${filename}
              title="Download this agent-published file">Download</a>`}
            <button ref=${closeRef} type="button" class="image-preview-close"
              onClick=${close} aria-label="Close image preview" title="Close (Esc)">×</button>
          </div>
        </header>
        <div class="image-preview-stage" data-zoomed=${zoom > 1} tabIndex="0" role="region"
          aria-label="Image canvas; scroll to pan when zoomed">
          <${PreviewState} state=${loadState} />
          ${(loadState === 'loading' || loadState === 'ready') && html`<img
            class="image-preview-image" src=${href} alt=${filename}
            style=${{ transform: `scale(${zoom})` }}
            onLoad=${() => setLoadState('ready')}
            onError=${() => setLoadState('error')} />`}
        </div>
        <footer class="image-preview-footer" id=${detailsID}>
          <span class="image-preview-footer-file">${filename}</span>
          <span class="image-preview-footer-size">${attachmentSize(attachment.size_bytes)}</span>
          <span class="grow"></span>
          <span class="image-preview-zoom-label">Zoom</span>
          <button type="button" onClick=${() => setZoom((value) => Math.max(.5, value - .5))}
            disabled=${zoom <= .5} aria-label="Zoom out">−</button>
          <button type="button" onClick=${() => setZoom(1)}
            aria-label="Reset zoom">${Math.round(zoom * 100)}%</button>
          <button type="button" onClick=${() => setZoom((value) => Math.min(3, value + .5))}
            disabled=${zoom >= 3} aria-label="Zoom in">+</button>
        </footer>
      </div>
    </div>`}
  </${Fragment}>`;
}
