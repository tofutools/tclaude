import { h } from 'preact';
import htm from 'htm';
import { ImageAttachmentPreview } from './image-preview-overlay.js';

const html = htm.bind(h);

// Both media types use one dialog so navigation preserves its display mode.
export function VideoAttachmentPreview(props) {
  return html`<${ImageAttachmentPreview} ...${props} videoTrigger=${true} />`;
}
