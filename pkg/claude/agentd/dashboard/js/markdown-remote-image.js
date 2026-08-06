// The click-to-load half of the Markdown viewer's image support.
//
// An image published with the notification renders straight away — its bytes
// already sit on the operator's own daemon. An image at an http(s) URL does
// not: fetching it is a request to a third party made on the operator's
// behalf, from a browser that is not behind the egress boundary the agent
// writing the document may be confined by (the reasoning is in
// markdown-model.js, at REMOTE_IMAGE). So the document renders a placeholder
// naming the host, and the request happens only when the operator asks for it.
//
// Load state lives in MarkdownDocument rather than here, because "load all the
// images in this document" is a decision about the document — and so is "which
// of these is still waiting to be seen", which a failure puts an image back
// into. These components are told what has been loaded and what has failed, and
// report what the operator clicked and what the browser refused to fetch.

import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

// The host is the one fact the operator's decision turns on, so it is what the
// placeholder leads with. A src that will not parse cannot reach this far —
// markdown-model.js only produces a remote node for a URL it parsed — but the
// fallback keeps a malformed one from rendering the word "undefined".
function hostOf(src) {
  try {
    return new URL(src).host;
  } catch {
    return '';
  }
}

// RemoteImage is one held-back image: a placeholder until `loaded`, the real
// picture after. Everything inline-level, because an image lives inside a
// paragraph.
export function RemoteImage({ src, alt, title, loaded, failed, onLoad, onError }) {
  const host = hostOf(src);
  const description = alt || 'Untitled image';

  if (loaded && !failed) {
    return html`<img class="markdown-remote-image-loaded" src=${src} alt=${alt || ''}
      title=${title} loading="lazy" decoding="async" referrerpolicy="no-referrer"
      onError=${() => onError?.(src)} />`;
  }

  // `failed` implies the operator already asked for this one, so the placeholder
  // it goes back to is a retry rather than a first offer.
  const retry = Boolean(failed);
  return html`<span class="markdown-remote-image" data-failed=${retry ? 'true' : 'false'}>
    <span class="markdown-remote-image-icon" aria-hidden="true">▤</span>
    <span class="markdown-remote-image-text">
      <strong>${description}</strong>
      <span>${retry
        ? `Could not be loaded from ${host || 'an external site'}`
        : `External image${host ? ` from ${host}` : ''} · not loaded`}</span>
    </span>
    <button type="button" class="markdown-remote-image-load"
      title=${src}
      aria-label=${`${retry ? 'Retry loading' : 'Load'} the external image "${description}"${host ? ` from ${host}` : ''}`}
      onClick=${(event) => {
        // A document may wrap its image in a link — `[![alt](img)](target)` —
        // which puts this button inside an anchor pointing wherever the author
        // chose. Loading the image is this button's whole job, so the click
        // stops here rather than also opening the author's link.
        event.preventDefault();
        event.stopPropagation();
        onLoad?.(src);
      }}>${retry ? 'Try again' : 'Load image'}</button>
  </span>`;
}

// RemoteImageNotice offers to load a document's remote images in one go. It
// appears only when there is more than one still held back — for a single
// image the placeholder's own button is already the shortest path, and a
// banner above every illustrated document would be noise.
export function RemoteImageNotice({ count, onLoadAll }) {
  if (count < 2) return null;
  return html`<div class="markdown-remote-image-notice" role="status">
    <span class="markdown-remote-image-notice-text">
      This document shows ${count} images hosted elsewhere. None are fetched unless you ask.
    </span>
    <button type="button" class="markdown-remote-image-notice-load" onClick=${onLoadAll}>
      Load all ${count}
    </button>
  </div>`;
}
