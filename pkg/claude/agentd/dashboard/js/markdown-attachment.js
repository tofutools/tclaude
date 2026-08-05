// MarkdownAttachment is how a published Markdown document appears in a human
// notification, on both surfaces that show attachments: the quick reader drawer
// and Messages.
//
// The document renders IN the message, not behind a control. A report an agent
// wrote to be read is the content of that notification, so hiding it behind a
// button put the actual message one click away and — since the attachment card
// sits below the body — well down the pane. The button that remains does the
// opposite job: it opens the original Markdown source, for when the operator
// wants to see what the agent actually wrote rather than how it renders.

import { Fragment, h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';
import { attachmentHrefByID, attachmentSize } from './human-attachments.js';
import { MarkdownDocument } from './markdown-document.js';

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

// `siblings` is every file published with the same notification, so a document
// can show `![map](map.png)` as the image attached beside it. The document's
// own entry is in the list and harmless: only files the daemon confirmed are
// raster images can be referenced as one.
export function MarkdownAttachment({ messageID, attachment, siblings, surface = 'attachment' }) {
  const [loadState, setLoadState] = useState('idle');
  const [source, setSource] = useState('');
  const [sourceOpen, setSourceOpen] = useState(false);
  const overlayRef = useRef(null);
  const closeRef = useRef(null);
  const triggerRef = useRef(null);
  const renderable = Boolean(attachment?.markdown);
  const href = attachment ? attachmentHrefByID(messageID, attachment) : '';
  const filename = attachment?.filename || 'document.md';
  // Several documents can share a message, so the attachment id keeps the
  // dialog's labelling ids unique.
  const titleID = `markdown-source-title-${surface}-${safeIDPart(messageID)}-${safeIDPart(attachment?.id ?? 0)}`;

  const close = useCallback(() => setSourceOpen(false), []);
  const { dialogRef } = useDialogFocus({
    open: sourceOpen,
    initialFocusRef: closeRef,
    onEscape: close,
    shouldHandle: () => isTopmostOverlay(overlayRef.current),
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
    <button ref=${triggerRef} type="button" class="human-attachment-markdown-trigger"
      disabled=${loadState !== 'ready'}
      onClick=${() => setSourceOpen(true)} aria-label=${`View the Markdown source of ${filename}`}>
      <span class="human-attachment-markdown-icon" aria-hidden="true">¶</span>
      <span class="human-attachment-markdown-label">View source</span>
    </button>
    <div class="markdown-attachment-document">
      <${LoadState} state=${loadState} />
      ${loadState === 'ready' && html`<${MarkdownDocument} source=${source} attachments=${siblings} />`}
    </div>
    ${sourceOpen && html`<div ref=${overlayRef} class="modal-overlay show markdown-preview-overlay"
      onMouseDown=${(event) => {
        if (event.target === event.currentTarget && isTopmostOverlay(overlayRef.current)) {
          event.preventDefault();
          close();
        }
      }}>
      <div ref=${dialogRef} class="markdown-preview-dialog" role="dialog" aria-modal="true"
        aria-labelledby=${titleID}>
        <header class="markdown-preview-header">
          <div class="markdown-preview-heading">
            <h2 id=${titleID}>${filename}</h2>
            <span>Markdown source · ${attachmentSize(attachment.size_bytes)}</span>
          </div>
          <div class="markdown-preview-header-actions">
            <a class="markdown-preview-download" href=${href} download=${filename}
              title="Download this agent-published file">Download</a>
            <button ref=${closeRef} type="button" class="markdown-preview-close"
              onClick=${close} aria-label="Close the source view" title="Close (Esc)">×</button>
          </div>
        </header>
        <div class="markdown-preview-stage" tabIndex="0" role="region"
          aria-label=${`${filename} source`}>
          <pre class="markdown-preview-source">${source}</pre>
        </div>
      </div>
    </div>`}
  </${Fragment}>`;
}
