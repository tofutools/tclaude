import { Fragment, h } from 'preact';
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { isTopmostOverlay } from './overlay-stack.js';
import { DialogResizer, useDialogResize } from './dialog-resize.js';
import {
  ViewerDisplayButtons, ZOOM_STEP, clamp, useViewerDisplay, useZoomGestures,
} from './viewer-controls.js';

import { VideoPoster, PlayerState, formatDuration } from './video-preview-content.js';
import { attachmentNavigationStep, previewAttachments } from './attachment-navigation.js';

const html = htm.bind(h);

// Zoom is a scale against the image's own pixels (1 = one image pixel per CSS
// pixel), so the percentage the footer shows is the one that matters when
// inspecting a screenshot. `null` means "fit": the largest scale that shows the
// whole image without upscaling it, recomputed as the stage resizes.
const MIN_SCALE = 0.05;
const MAX_SCALE = 16;

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


// useStageBox reports the stage's content box, which is what "fit" fits into.
// It follows the dialog through a resize-grip drag, maximize and fullscreen.
function useStageBox(stageRef, open) {
  const [box, setBox] = useState(null);
  useEffect(() => {
    const stage = stageRef.current;
    if (!open || !stage) return undefined;
    const measure = () => {
      const style = typeof getComputedStyle === 'function' ? getComputedStyle(stage) : {};
      const pad = (side) => parseFloat(style[`padding${side}`]) || 0;
      const w = (stage.clientWidth || 0) - pad('Left') - pad('Right');
      const hgt = (stage.clientHeight || 0) - pad('Top') - pad('Bottom');
      setBox((prev) => (prev && prev.w === w && prev.h === hgt ? prev : { w, h: hgt }));
    };
    measure();
    if (typeof ResizeObserver !== 'function') return undefined;
    const observer = new ResizeObserver(measure);
    observer.observe(stage);
    return () => observer.disconnect();
  }, [open, stageRef]);
  return box;
}

// ImageAttachmentPreview owns the media viewer used by both the quick
// notification drawer and Messages. The thumbnail remains in the attachment
// card; only this overlay is shared between the two surfaces.
//
// Zooming sizes the image in layout (width/height), not with a transform, so
// the stage's scroll range is exactly the zoomed image and centring it with
// auto margins stays safe when it overflows. Every zoom keeps one point still:
// the pointer for a pinch / Ctrl+wheel / double-click, the middle otherwise.
export function ImageAttachmentPreview({ messageID, attachment: launcherAttachment, siblings = [], surface = 'attachment', videoTrigger = false }) {
  const [selected, setSelected] = useState(null);
  const items = previewAttachments(siblings.length ? siblings : [launcherAttachment]);
  const attachment = items[selected] || launcherAttachment;
  const index = items.indexOf(attachment);
  const video = Boolean(attachment?.video);
  const [meta, setMeta] = useState(null);
  const [open, setOpen] = useState(false);
  const [loadState, setLoadState] = useState('idle');
  const [natural, setNatural] = useState(null);
  const [scale, setScale] = useState(null);
  const [panning, setPanning] = useState(false);
  const overlayRef = useRef(null);
  const closeRef = useRef(null);
  const stageRef = useRef(null);
  const imageRef = useRef(null);
  // The anchor a pending zoom must restore once the new size is laid out.
  const anchorRef = useRef(null);
  const href = attachment ? attachmentURL(messageID, attachment) : '';
  const filename = attachment?.filename || 'attachment';
  // Several previews can share a message, so the attachment id keeps the
  // dialog's labelling ids unique.
  const titleID = `image-preview-title-${surface}-${safeIDPart(messageID)}-${safeIDPart(attachment?.id ?? 0)}`;
  const detailsID = `${titleID}-details`;

  const close = useCallback(() => { setOpen(false); setSelected(null); }, []);
  const { dialogRef } = useDialogFocus({
    open,
    initialFocusRef: closeRef,
    onEscape: close,
    shouldHandle: () => isTopmostOverlay(overlayRef.current),
  });
  const { dialogStyle, resizerProps } = useDialogResize({
    dialogRef,
    // One media dialog keeps one size regardless of which thumbnail opens it.
    // Reuse the image key to retain existing saved image-viewer sizes.
    prefKey: 'tclaude.dash.attachmentViewer.image.size',
  });
  const topmost = () => isTopmostOverlay(overlayRef.current);
  const view = useViewerDisplay({ dialogRef, open, shouldHandle: topmost });
  const stageBox = useStageBox(stageRef, open);

  const fitScale = natural && stageBox && stageBox.w > 0 && stageBox.h > 0
    ? Math.min(1, stageBox.w / natural.w, stageBox.h / natural.h)
    : 1;
  const effective = scale ?? fitScale;
  // Read by the gesture handlers, which can fire several times between renders:
  // each must compound on the last request, not on the last render.
  const effectiveRef = useRef(effective);
  effectiveRef.current = effective;
  const fitRef = useRef(fitScale);
  fitRef.current = fitScale;

  const zoomTo = useCallback((compute, point = null) => {
    if (!natural) return;
    const from = effectiveRef.current;
    const to = compute === 'fit' ? null : clamp(compute(from), MIN_SCALE, MAX_SCALE);
    const target = to ?? fitRef.current;
    if (Math.abs(target - from) < 1e-4) {
      // Nothing moves, so there is no anchor to restore — but "fit" still
      // switches the mode, so it keeps tracking later resizes.
      if (to === null) setScale(null);
      return;
    }
    const stage = stageRef.current;
    const image = imageRef.current;
    if (stage && image && !anchorRef.current && image.offsetWidth > 0) {
      const rect = stage.getBoundingClientRect();
      const px = point ? clamp(point.x - rect.left, 0, stage.clientWidth) : stage.clientWidth / 2;
      const py = point ? clamp(point.y - rect.top, 0, stage.clientHeight) : stage.clientHeight / 2;
      anchorRef.current = {
        px, py,
        ux: (stage.scrollLeft + px - image.offsetLeft) / image.offsetWidth,
        uy: (stage.scrollTop + py - image.offsetTop) / image.offsetHeight,
      };
    }
    effectiveRef.current = target;
    setScale(to);
  }, [natural]);

  useLayoutEffect(() => {
    const anchor = anchorRef.current;
    anchorRef.current = null;
    const stage = stageRef.current;
    const image = imageRef.current;
    if (!anchor || !stage || !image) return;
    stage.scrollLeft = image.offsetLeft + anchor.ux * image.offsetWidth - anchor.px;
    stage.scrollTop = image.offsetTop + anchor.uy * image.offsetHeight - anchor.py;
  }, [effective]);

  useZoomGestures({
    targetRef: dialogRef,
    enabled: open && !video && loadState === 'ready',
    shouldHandle: topmost,
    onZoom: (factor, point) => zoomTo((value) => value * factor, point),
    onKey: (action) => {
      if (action === 'in') zoomTo((value) => value * ZOOM_STEP);
      else if (action === 'out') zoomTo((value) => value / ZOOM_STEP);
      else zoomTo('fit');
    },
  });

  const navigate = (step) => {
    if (items.length < 2) return;
    setSelected((Math.max(0, index) + step + items.length) % items.length);
  };
  useEffect(() => {
    if (!open || items.length < 2) return undefined;
    const onKeyDown = (event) => {
      if (!topmost()) return;
      const step = attachmentNavigationStep(event, video);
      if (!step) return;
      event.preventDefault();
      navigate(step);
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [open, video, index, siblings]);

  useEffect(() => {
    if (!open) return undefined;
    let active = true;
    setLoadState('checking');
    setScale(null);
    setNatural(null);
    setMeta(null);
    anchorRef.current = null;
    if (stageRef.current) { stageRef.current.scrollLeft = 0; stageRef.current.scrollTop = 0; }
    fetch(href, { method: 'HEAD', credentials: 'same-origin' })
      .then((response) => {
        if (!active) return;
        if (response.status === 410) setLoadState('gone');
        else if (!response.ok) setLoadState('error');
        else setLoadState(video ? 'ready' : 'loading');
      })
      .catch(() => {
        if (active) setLoadState('error');
      });
    return () => { active = false; };
  }, [href, open, video]);

  // Double-click toggles between fit and a closer look at the clicked point:
  // actual pixels for an image that fit had to shrink, double size otherwise.
  const onImageDoubleClick = (event) => {
    const point = { x: event.clientX, y: event.clientY };
    if (scale !== null) zoomTo('fit', point);
    else zoomTo(() => (fitScale < 1 ? 1 : fitScale * 2), point);
  };

  // Drag to pan with the mouse once the image overflows the stage. Touch and
  // pen keep the stage's native scrolling.
  const onStagePointerDown = (event) => {
    const stage = stageRef.current;
    if (event.button !== 0 || event.pointerType !== 'mouse' || !stage) return;
    if (stage.scrollWidth <= stage.clientWidth && stage.scrollHeight <= stage.clientHeight) return;
    // A press on the stage's own scrollbar belongs to the scrollbar: panning
    // from it would drag the content against the thumb.
    const rect = stage.getBoundingClientRect();
    if (event.clientX - rect.left >= stage.clientWidth || event.clientY - rect.top >= stage.clientHeight) return;
    event.preventDefault();
    stage.focus?.({ preventScroll: true });
    const startX = event.clientX, startY = event.clientY;
    const startLeft = stage.scrollLeft, startTop = stage.scrollTop;
    stage.setPointerCapture?.(event.pointerId);
    setPanning(true);
    const onMove = (moveEvent) => {
      stage.scrollLeft = startLeft - (moveEvent.clientX - startX);
      stage.scrollTop = startTop - (moveEvent.clientY - startY);
    };
    const onUp = () => {
      stage.removeEventListener('pointermove', onMove);
      stage.removeEventListener('pointerup', onUp);
      stage.removeEventListener('pointercancel', onUp);
      if (stage.hasPointerCapture?.(event.pointerId)) stage.releasePointerCapture?.(event.pointerId);
      setPanning(false);
    };
    stage.addEventListener('pointermove', onMove);
    stage.addEventListener('pointerup', onUp);
    stage.addEventListener('pointercancel', onUp);
  };

  if (videoTrigger ? !launcherAttachment?.video : !launcherAttachment?.previewable) return null;

  const pannable = Boolean(natural && stageBox
    && (natural.w * effective > stageBox.w + 0.5 || natural.h * effective > stageBox.h + 0.5));
  const imageStyle = natural
    ? { width: `${natural.w * effective}px`, height: `${natural.h * effective}px` }
    : undefined;
  const ready = loadState === 'ready';

  return html`<${Fragment}>
    <button type="button" class=${`human-attachment-preview-trigger${videoTrigger ? ' video-preview-trigger' : ''}`}
      onClick=${() => { setSelected(null); setOpen(true); }} aria-label=${`${videoTrigger ? 'Play' : 'Preview'} ${launcherAttachment.filename || 'attachment'}`}>
      ${videoTrigger ? html`<${VideoPoster} href=${attachmentURL(messageID, launcherAttachment)} />
        <span class="video-preview-play" aria-hidden="true">▶</span>`
        : html`<img class="human-attachment-preview-thumb" src=${attachmentURL(messageID, launcherAttachment)} alt="" loading="lazy" decoding="async" />`}
      <span class="human-attachment-preview-label">${videoTrigger ? 'Play' : 'Preview'}</span>
    </button>
    ${open && html`<div ref=${overlayRef} class=${`modal-overlay show image-preview-overlay${video ? ' video-preview-overlay' : ''}`}
      onMouseDown=${(event) => {
        if (event.target === event.currentTarget && isTopmostOverlay(overlayRef.current)) {
          event.preventDefault();
          close();
        }
      }}>
      <div ref=${dialogRef} class=${`image-preview-dialog${video ? ' video-preview-dialog' : ''}`} role="dialog" aria-modal="true"
        aria-labelledby=${titleID} aria-describedby=${detailsID} style=${dialogStyle}
        data-display=${view.display}>
        <header class="image-preview-header">
          <div class="image-preview-heading">
            <h2 id=${titleID}>${filename}</h2>
            <span id=${detailsID}>${attachment.content_type || 'image'} · ${attachmentSize(attachment.size_bytes)}${natural ? ` · ${natural.w}×${natural.h}` : ''}${meta ? ` · ${meta.w}×${meta.h} · ${formatDuration(meta.duration)}` : ''}</span>
          </div>
          <div class="image-preview-header-actions">
            ${items.length > 1 && html`<div role="group" class="attachment-preview-nav" aria-label="Message attachments">
              <button type="button" class="viewer-display-button attachment-preview-step"
                onClick=${() => navigate(-1)} aria-label="Previous attachment"
                title=${video ? 'Previous attachment (Shift+Left)' : 'Previous attachment (Left or Shift+Left)'}>‹</button>
              <span class="attachment-preview-position" role="status" aria-live="polite" aria-atomic="true">${index + 1} of ${items.length}</span>
              <button type="button" class="viewer-display-button attachment-preview-step"
                onClick=${() => navigate(1)} aria-label="Next attachment"
                title=${video ? 'Next attachment (Shift+Right)' : 'Next attachment (Right or Shift+Right)'}>›</button>
            </div>`}
            ${loadState !== 'gone' && html`<a class="image-preview-download" href=${href} download=${filename}
              title="Download this agent-published file">Download</a>`}
            <${ViewerDisplayButtons} ...${view} />
            <button ref=${closeRef} type="button" class="image-preview-close"
              onClick=${close} aria-label=${video ? 'Close video player' : 'Close image preview'} title="Close (Esc)">×</button>
          </div>
        </header>
        <div ref=${stageRef} class=${`image-preview-stage${video ? ' video-preview-stage' : ''}`} data-pannable=${!video && pannable} data-panning=${panning}
          tabIndex="0" role="region" onPointerDown=${video ? undefined : onStagePointerDown}
          aria-label=${video ? 'Video player' : 'Image canvas; pinch, Ctrl+scroll or + and − to zoom; drag or scroll to pan'}>
          ${video ? html`<${PlayerState} state=${loadState} />` : html`<${PreviewState} state=${loadState} />`}
          ${video && ready && html`<video key=${href} class="video-preview-video" src=${href}
            controls autoplay playsInline preload="auto" aria-label=${filename}
            onLoadedMetadata=${(event) => {
              const { videoWidth: w, videoHeight: h, duration } = event.currentTarget;
              setMeta({ w, h, duration });
            }} onError=${() => setLoadState('error')}></video>`}
          ${!video && (loadState === 'loading' || ready) && html`<img key=${href} ref=${imageRef}
            class="image-preview-image" src=${href} alt=${filename} draggable="false"
            data-sized=${Boolean(natural)} style=${imageStyle}
            onDblClick=${onImageDoubleClick}
            onLoad=${(event) => {
              const { naturalWidth: w, naturalHeight: hgt } = event.currentTarget;
              if (w > 0 && hgt > 0) setNatural({ w, h: hgt });
              setLoadState('ready');
            }}
            onError=${() => setLoadState('error')} />`}
        </div>
        ${!video && html`<footer class="image-preview-footer">
          <span class="image-preview-footer-file">${filename}</span>
          <span class="image-preview-footer-size">${attachmentSize(attachment.size_bytes)}</span>
          <span class="grow"></span>
          <span class="image-preview-zoom-label">Zoom</span>
          <button type="button" onClick=${() => zoomTo((value) => value / ZOOM_STEP)}
            disabled=${!ready || effective <= MIN_SCALE} aria-label="Zoom out" title="Zoom out (−)">−</button>
          <button type="button" class="image-preview-zoom-value" onClick=${() => zoomTo('fit')}
            disabled=${!ready} aria-label="Zoom level; fit to the viewer"
            title="Fit to the viewer (0)">${Math.round(effective * 100)}%</button>
          <button type="button" onClick=${() => zoomTo((value) => value * ZOOM_STEP)}
            disabled=${!ready || effective >= MAX_SCALE} aria-label="Zoom in" title="Zoom in (+)">+</button>
          <button type="button" onClick=${() => zoomTo('fit')} disabled=${!ready}
            aria-pressed=${scale === null} title="Fit the whole image in the viewer">Fit</button>
          <button type="button" onClick=${() => zoomTo(() => 1)} disabled=${!ready}
            aria-pressed=${scale !== null && Math.abs(scale - 1) < 1e-4}
            aria-label="Actual size" title="Actual size (100%)">1:1</button>
        </footer>`}
        <${DialogResizer} ...${resizerProps} label=${video ? 'Resize the video player' : 'Resize the image viewer'} />
      </div>
    </div>`}
  </${Fragment}>`;
}
