// attachment-viewer.js -- the attachment viewer, as one component for any
// surface that shows files: the Messages tab's viewer (the media viewer of
// image-preview-overlay.js for images and videos, the document viewer of
// markdown-attachment.js for Markdown), opened on one file of a list.
//
// The Messages tab opens it from its attachment card's own controls (a
// thumbnail, ⤢ View). The task board opens it from a task's file tiles, a
// comment's, and the List's 📎 count, reading the files through the board's
// api (attachment-source.js taskFileSource).
//
// Only files the daemon gave a viewer verdict to open in it (`previewable`,
// `video`, `markdown`, as on a notification's files). Anything else has no
// viewer here, as in Messages: viewerKind says so, and the caller offers the
// download (or its own fallback) instead.

import { h } from 'preact';
import htm from 'htm';
import { MediaViewer } from './image-preview-overlay.js';
import { MarkdownViewer } from './markdown-attachment.js';

const html = htm.bind(h);

// viewerKind is which viewer opens a file: 'media', 'markdown', or '' for
// none.
export function viewerKind(attachment) {
  if (attachment?.previewable || attachment?.video) return 'media';
  if (attachment?.markdown) return 'markdown';
  return '';
}

// firstViewable is the first file of a list that opens in the viewer, or
// null.
export function firstViewable(attachments) {
  return (attachments || []).find((attachment) => viewerKind(attachment)) || null;
}

// AttachmentViewer is the open viewer on `attachment`; `attachments` are the
// files beside it (the media viewer steps through those it can show, and a
// document can reference an image among them). `source` reads them; onClose
// closes it. Renders nothing for a file without a viewer.
export function AttachmentViewer({ source, attachment, attachments = [], surface = 'attachment', downloadTitle = 'Download this file', onClose }) {
  const kind = viewerKind(attachment);
  if (kind === 'media') {
    return html`<${MediaViewer} source=${source} attachment=${attachment} siblings=${attachments}
      surface=${surface} downloadTitle=${downloadTitle} onClose=${onClose} />`;
  }
  if (kind === 'markdown') {
    return html`<${MarkdownViewer} files=${source} attachment=${attachment} siblings=${attachments}
      surface=${surface} downloadTitle=${downloadTitle} onClose=${onClose} />`;
  }
  return null;
}
