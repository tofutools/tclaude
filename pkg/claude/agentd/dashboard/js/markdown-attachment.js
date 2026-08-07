// MarkdownAttachment is how a published Markdown document appears in a human
// notification, on both surfaces that show attachments: the quick reader drawer
// and Messages.
//
// The document renders IN the message, not behind a control. A report an agent
// wrote to be read is the content of that notification, so hiding it behind a
// button put the actual message one click away and — since the attachment card
// sits below the body — well down the pane.
//
// The two controls beside it open the same modal viewer the image attachments
// get, for a long document the message column is too narrow to read: "Open"
// lands on the rendered document, "View source" on the original Markdown. Both
// modes stay reachable from a toggle in the viewer's header, so arriving in one
// is never a dead end.

import { Fragment, h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';
import { attachmentHrefByID, attachmentSize } from './human-attachments.js';
import { MarkdownDocument } from './markdown-document.js';
import { DialogResizer, useDialogResize } from './dialog-resize.js';

const html = htm.bind(h);

function safeIDPart(value) {
  return String(value ?? '').replace(/[^a-zA-Z0-9_-]/g, '-');
}

function LoadState({ state }) {
  if (state === 'loading') {
    return html`<div class="markdown-attachment-state" role="status">Loading document…</div>`;
  }
  if (state === 'gone') {
    return html`<div class="markdown-attachment-state" role="alert">
      <strong>This attachment is no longer available.</strong>
      <span>The stored file was removed before it could be read.</span>
    </div>`;
  }
  if (state === 'error') {
    return html`<div class="markdown-attachment-state" role="alert">
      <strong>The document could not be read.</strong>
      <span>The file may be damaged or no longer available. Download the original to inspect it.</span>
    </div>`;
  }
  return null;
}

export function MarkdownAttachment({ messageID, attachment, surface = 'attachment' }) {
  const [loadState, setLoadState] = useState('idle');
  const [source, setSource] = useState('');
  // null while closed; otherwise the mode the viewer is showing.
  const [viewMode, setViewMode] = useState(null);
  const overlayRef = useRef(null);
  const closeRef = useRef(null);
  const renderable = Boolean(attachment?.markdown);
  const href = attachment ? attachmentHrefByID(messageID, attachment) : '';
  const filename = attachment?.filename || 'document.md';
  // Several documents can share a message, so the attachment id keeps the
  // dialog's labelling ids unique.
  const titleID = `markdown-source-title-${surface}-${safeIDPart(messageID)}-${safeIDPart(attachment?.id ?? 0)}`;
  const viewerOpen = viewMode !== null;

  const close = useCallback(() => setViewMode(null), []);
  const { dialogRef } = useDialogFocus({
    open: viewerOpen,
    initialFocusRef: closeRef,
    onEscape: close,
    shouldHandle: () => isTopmostOverlay(overlayRef.current),
  });
  const { dialogStyle, resizerProps } = useDialogResize({
    dialogRef,
    prefKey: 'tclaude.dash.attachmentViewer.markdown.size',
  });

  // Fetched when the message is shown rather than on demand: the document is
  // the thing the operator came to read, so it must already be there. One GET
  // both fetches it and distinguishes a file the cleanup already removed.
  useEffect(() => {
    if (!renderable) return undefined;
    let active = true;
    setLoadState('loading');
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
  }, [href, renderable]);

  if (!renderable) return null;

  return html`<${Fragment}>
    <button type="button" class="human-attachment-markdown-trigger"
      disabled=${loadState !== 'ready'}
      onClick=${() => setViewMode('rendered')} aria-label=${`Open ${filename} in the document viewer`}>
      <span class="human-attachment-markdown-icon" aria-hidden="true">⤢</span>
      <span class="human-attachment-markdown-label">Open</span>
    </button>
    <button type="button" class="human-attachment-markdown-trigger"
      disabled=${loadState !== 'ready'}
      onClick=${() => setViewMode('source')} aria-label=${`View the Markdown source of ${filename}`}>
      <span class="human-attachment-markdown-icon" aria-hidden="true">¶</span>
      <span class="human-attachment-markdown-label">View source</span>
    </button>
    <div class="markdown-attachment-document">
      <${LoadState} state=${loadState} />
      ${loadState === 'ready' && html`<${MarkdownDocument} source=${source} />`}
    </div>
    ${viewerOpen && html`<div ref=${overlayRef} class="modal-overlay show markdown-preview-overlay"
      onMouseDown=${(event) => {
        if (event.target === event.currentTarget && isTopmostOverlay(overlayRef.current)) {
          event.preventDefault();
          close();
        }
      }}>
      <div ref=${dialogRef} class="markdown-preview-dialog" role="dialog" aria-modal="true"
        aria-labelledby=${titleID} style=${dialogStyle}>
        <header class="markdown-preview-header">
          <div class="markdown-preview-heading">
            <h2 id=${titleID}>${filename}</h2>
            <span>${viewMode === 'source' ? 'Markdown source' : 'Document'} · ${attachmentSize(attachment.size_bytes)}</span>
          </div>
          <div class="markdown-preview-header-actions">
            <div class="markdown-preview-modes" role="group" aria-label="Document view mode">
              <button type="button" class="markdown-preview-mode"
                aria-pressed=${viewMode === 'rendered'}
                onClick=${() => setViewMode('rendered')}>Rendered</button>
              <button type="button" class="markdown-preview-mode"
                aria-pressed=${viewMode === 'source'}
                onClick=${() => setViewMode('source')}>Source</button>
            </div>
            <a class="markdown-preview-download" href=${href} download=${filename}
              title="Download this agent-published file">Download</a>
            <button ref=${closeRef} type="button" class="markdown-preview-close"
              onClick=${close} aria-label="Close the document viewer" title="Close (Esc)">×</button>
          </div>
        </header>
        <div class="markdown-preview-stage" tabIndex="0" role="region"
          aria-label=${viewMode === 'source' ? `${filename} source` : filename}>
          ${viewMode === 'source'
            ? html`<pre class="markdown-preview-source">${source}</pre>`
            : html`<${MarkdownDocument} source=${source} className="markdown-document markdown-preview-document" />`}
        </div>
        <${DialogResizer} ...${resizerProps} label="Resize the document viewer" />
      </div>
    </div>`}
  </${Fragment}>`;
}
