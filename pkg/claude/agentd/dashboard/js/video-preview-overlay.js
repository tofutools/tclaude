import { Fragment, h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';
import { DialogResizer, useDialogResize } from './dialog-resize.js';
import { ViewerDisplayButtons, useViewerDisplay } from './viewer-controls.js';

const html = htm.bind(h);

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

function formatDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return '';
  const total = Math.round(seconds);
  const hrs = Math.floor(total / 3600);
  const min = Math.floor((total % 3600) / 60);
  const sec = String(total % 60).padStart(2, '0');
  return hrs ? `${hrs}:${String(min).padStart(2, '0')}:${sec}` : `${min}:${sec}`;
}

function PlayerState({ state }) {
  if (state === 'checking') {
    return html`<div class="image-preview-state" role="status">Loading video…</div>`;
  }
  if (state === 'gone') {
    return html`<div class="image-preview-state" role="alert">
      <strong>This attachment is no longer available.</strong>
      <span>The stored file was removed before it could be played.</span>
    </div>`;
  }
  if (state === 'error') {
    return html`<div class="image-preview-state" role="alert">
      <strong>This browser can't play this video.</strong>
      <span>The codec may be unsupported or the file damaged. Download the original to watch it locally.</span>
    </div>`;
  }
  return null;
}

// VideoPoster is the thumbnail frame. A <video preload="metadata"> left
// mounted keeps its ranged request open for as long as the page lives, and the
// dashboard is plain HTTP/1.1 with a handful of connections per host: a list of
// video messages would starve the snapshot poll. So the frame is copied onto a
// canvas as soon as it decodes and the <video> is dropped, which aborts its
// request. The #t fragment asks for the frame just past zero, since some
// encoders open on a black frame.
function VideoPoster({ href }) {
  const [poster, setPoster] = useState(null);
  const [failed, setFailed] = useState(false);
  if (poster) return html`<img class="human-attachment-preview-thumb video-preview-poster" src=${poster} alt="" />`;
  if (failed) return html`<span class="human-attachment-preview-thumb video-preview-poster" aria-hidden="true"></span>`;
  return html`<video class="human-attachment-preview-thumb" src=${`${href}#t=0.1`}
    preload="metadata" muted playsInline tabIndex="-1" aria-hidden="true"
    onLoadedData=${(event) => {
      const video = event.currentTarget;
      try {
        const canvas = document.createElement('canvas');
        canvas.width = video.videoWidth;
        canvas.height = video.videoHeight;
        canvas.getContext('2d').drawImage(video, 0, 0);
        setPoster(canvas.toDataURL('image/jpeg', 0.8));
      } catch {
        setFailed(true);
      }
    }}
    onError=${() => setFailed(true)}></video>`;
}

// VideoAttachmentPreview is the video counterpart of ImageAttachmentPreview:
// a poster-frame thumbnail in the attachment card that opens a modal player.
// Playback is the browser's native <video> element, so the formats it plays
// are the browser's (MP4/H.264, WebM/VP9/AV1, Ogg/Theora everywhere current);
// the daemon only vouches for the container (`attachment.video`), and the
// player falls back to a download hint when the codec inside is unsupported.
export function VideoAttachmentPreview({ messageID, attachment, surface = 'attachment' }) {
  const [open, setOpen] = useState(false);
  const [loadState, setLoadState] = useState('idle');
  const [meta, setMeta] = useState(null);
  const overlayRef = useRef(null);
  const closeRef = useRef(null);
  const href = attachment ? attachmentURL(messageID, attachment) : '';
  const filename = attachment?.filename || 'attachment';
  const titleID = `video-preview-title-${surface}-${safeIDPart(messageID)}-${safeIDPart(attachment?.id ?? 0)}`;
  const detailsID = `${titleID}-details`;

  const close = useCallback(() => setOpen(false), []);
  const { dialogRef } = useDialogFocus({
    open,
    initialFocusRef: closeRef,
    onEscape: close,
    shouldHandle: () => isTopmostOverlay(overlayRef.current),
  });
  const { dialogStyle, resizerProps } = useDialogResize({
    dialogRef,
    prefKey: 'tclaude.dash.attachmentViewer.video.size',
  });
  const view = useViewerDisplay({
    dialogRef, open, shouldHandle: () => isTopmostOverlay(overlayRef.current),
  });

  // Preflight like the image viewer, so a cleaned-up file shows "gone" rather
  // than the generic can't-play state the <video> error event would give.
  useEffect(() => {
    if (!open) return undefined;
    let active = true;
    setLoadState('checking');
    setMeta(null);
    fetch(href, { method: 'HEAD', credentials: 'same-origin' })
      .then((response) => {
        if (!active) return;
        if (response.status === 410) setLoadState('gone');
        else if (!response.ok) setLoadState('error');
        else setLoadState('ready');
      })
      .catch(() => {
        if (active) setLoadState('error');
      });
    return () => { active = false; };
  }, [href, open]);

  if (!attachment?.video) return null;

  const details = [
    attachment.content_type || 'video',
    attachmentSize(attachment.size_bytes),
    meta?.w && meta?.h ? `${meta.w}×${meta.h}` : '',
    formatDuration(meta?.duration),
  ].filter(Boolean).join(' · ');

  return html`<${Fragment}>
    <button type="button" class="human-attachment-preview-trigger video-preview-trigger"
      onClick=${() => setOpen(true)} aria-label=${`Play ${filename}`}>
      <${VideoPoster} href=${href} />
      <span class="video-preview-play" aria-hidden="true">▶</span>
      <span class="human-attachment-preview-label">Play</span>
    </button>
    ${open && html`<div ref=${overlayRef} class="modal-overlay show image-preview-overlay video-preview-overlay"
      onMouseDown=${(event) => {
        if (event.target === event.currentTarget && isTopmostOverlay(overlayRef.current)) {
          event.preventDefault();
          close();
        }
      }}>
      <div ref=${dialogRef} class="image-preview-dialog video-preview-dialog" role="dialog" aria-modal="true"
        aria-labelledby=${titleID} aria-describedby=${detailsID} style=${dialogStyle}
        data-display=${view.display}>
        <header class="image-preview-header">
          <div class="image-preview-heading">
            <h2 id=${titleID}>${filename}</h2>
            <span id=${detailsID}>${details}</span>
          </div>
          <div class="image-preview-header-actions">
            ${loadState !== 'gone' && html`<a class="image-preview-download" href=${href} download=${filename}
              title="Download this agent-published file">Download</a>`}
            <${ViewerDisplayButtons} ...${view} />
            <button ref=${closeRef} type="button" class="image-preview-close"
              onClick=${close} aria-label="Close video player" title="Close (Esc)">×</button>
          </div>
        </header>
        <div class="image-preview-stage video-preview-stage">
          <${PlayerState} state=${loadState} />
          ${loadState === 'ready' && html`<video class="video-preview-video" src=${href}
            controls autoplay playsInline preload="auto" aria-label=${filename}
            onLoadedMetadata=${(event) => {
              const { videoWidth: w, videoHeight: hgt, duration } = event.currentTarget;
              setMeta({ w, h: hgt, duration });
            }}
            onError=${() => setLoadState('error')}></video>`}
        </div>
        <${DialogResizer} ...${resizerProps} label="Resize the video player" />
      </div>
    </div>`}
  </${Fragment}>`;
}
