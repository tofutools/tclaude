// MarkdownAttachment is how a published Markdown document appears in a human
// notification, on both surfaces that show attachments: the quick reader drawer
// and Messages.
//
// The document renders IN the message, not behind a control. A report an agent
// wrote to be read is the content of that notification, so hiding it behind a
// button put the actual message one click away and — since the attachment card
// sits below the body — well down the pane.
//
// The single control beside it opens the same modal viewer the image
// attachments get, for a long document the message column is too narrow to
// read. It lands on the rendered document; the original Markdown is one click
// further, on the viewer's own Rendered/Source toggle. That toggle is why there
// is one control here and not two: a second card button for the source spelled
// out a mode the viewer already switches between, and the card — a filename, a
// media type, a size, a download link — has no width to spare for it.

import { Fragment, h } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';
import { attachmentHrefByID, attachmentSize } from './human-attachments.js';
import { MarkdownDocument } from './markdown-document.js';
import { DialogResizer, useDialogResize } from './dialog-resize.js';
import {
  ViewerDisplayButtons, ZOOM_STEP, clamp, useViewerDisplay, useZoomGestures,
} from './viewer-controls.js';

const html = htm.bind(h);

// Text zoom in the viewer: pinch, Ctrl+wheel and the zoom keys scale the
// document with CSS `zoom`, which reflows it to the dialog width (a transform
// would only magnify it off the side). The document's type is set in px, so
// this is the one knob that scales all of it together.
const MIN_TEXT_ZOOM = 0.5;
const MAX_TEXT_ZOOM = 3;

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
  const [textZoom, setTextZoom] = useState(1);
  const textZoomRef = useRef(1);
  textZoomRef.current = textZoom;
  const stageRef = useRef(null);
  // The stage point a pending zoom must keep still, restored after layout.
  const anchorRef = useRef(null);

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
  const view = useViewerDisplay({ dialogRef, open: viewerOpen });

  const zoomText = useCallback((compute, point = null) => {
    const from = textZoomRef.current;
    const to = clamp(compute(from), MIN_TEXT_ZOOM, MAX_TEXT_ZOOM);
    if (Math.abs(to - from) < 1e-3) return;
    const stage = stageRef.current;
    if (stage && !anchorRef.current) {
      const rect = stage.getBoundingClientRect();
      const px = point ? clamp(point.x - rect.left, 0, stage.clientWidth) : 0;
      const py = point ? clamp(point.y - rect.top, 0, stage.clientHeight) : 0;
      anchorRef.current = { px, py, x: stage.scrollLeft + px, y: stage.scrollTop + py, from };
    }
    textZoomRef.current = to;
    setTextZoom(to);
  }, []);

  // Keep the anchored point of the document under the pointer (or the top of
  // the view, for keys and buttons) as the text reflows.
  useLayoutEffect(() => {
    const anchor = anchorRef.current;
    anchorRef.current = null;
    const stage = stageRef.current;
    if (!anchor || !stage) return;
    const ratio = textZoom / anchor.from;
    stage.scrollLeft = anchor.x * ratio - anchor.px;
    stage.scrollTop = anchor.y * ratio - anchor.py;
  }, [textZoom]);

  useZoomGestures({
    targetRef: dialogRef,
    enabled: viewerOpen,
    onZoom: (factor, point) => zoomText((value) => value * factor, point),
    onKey: (action) => {
      if (action === 'in') zoomText((value) => value * ZOOM_STEP);
      else if (action === 'out') zoomText((value) => value / ZOOM_STEP);
      else zoomText(() => 1);
    },
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
      onClick=${() => setViewMode('rendered')} aria-label=${`View ${filename} in the document viewer`}>
      <span class="human-attachment-markdown-icon" aria-hidden="true">⤢</span>
      <span class="human-attachment-markdown-label">View</span>
    </button>
    <div class="markdown-attachment-document">
      <${LoadState} state=${loadState} />
      ${loadState === 'ready' && html`<${MarkdownDocument} source=${source} attachments=${siblings} />`}
    </div>
    ${viewerOpen && html`<div ref=${overlayRef} class="modal-overlay show markdown-preview-overlay"
      onMouseDown=${(event) => {
        if (event.target === event.currentTarget && isTopmostOverlay(overlayRef.current)) {
          event.preventDefault();
          close();
        }
      }}>
      <div ref=${dialogRef} class="markdown-preview-dialog" role="dialog" aria-modal="true"
        aria-labelledby=${titleID} style=${dialogStyle} data-display=${view.display}>
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
            ${textZoom !== 1 && html`<button type="button" class="markdown-preview-zoom-reset"
              onClick=${() => zoomText(() => 1)} aria-label=${`Text size ${Math.round(textZoom * 100)}%; reset to 100%`}
              title="Reset text size (0)">${Math.round(textZoom * 100)}%</button>`}
            <a class="markdown-preview-download" href=${href} download=${filename}
              title="Download this agent-published file">Download</a>
            <${ViewerDisplayButtons} ...${view} />
            <button ref=${closeRef} type="button" class="markdown-preview-close"
              onClick=${close} aria-label="Close the document viewer" title="Close (Esc)">×</button>
          </div>
        </header>
        <div ref=${stageRef} class="markdown-preview-stage" tabIndex="0" role="region"
          aria-label=${viewMode === 'source' ? `${filename} source` : filename}>
          <div class="markdown-preview-zoom" style=${textZoom !== 1 ? { zoom: textZoom } : undefined}>
            ${viewMode === 'source'
              ? html`<pre class="markdown-preview-source">${source}</pre>`
              : html`<${MarkdownDocument} source=${source} attachments=${siblings}
                  className="markdown-document markdown-preview-document" />`}
          </div>
        </div>
        <${DialogResizer} ...${resizerProps} label="Resize the document viewer" />
      </div>
    </div>`}
  </${Fragment}>`;
}
