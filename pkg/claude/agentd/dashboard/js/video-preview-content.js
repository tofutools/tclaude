import { Fragment, h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

export function formatDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return '';
  const total = Math.round(seconds);
  const hrs = Math.floor(total / 3600);
  const min = Math.floor((total % 3600) / 60);
  const sec = String(total % 60).padStart(2, '0');
  return hrs ? `${hrs}:${String(min).padStart(2, '0')}:${sec}` : `${min}:${sec}`;
}

export function PlayerState({ state }) {
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
// video messages would starve the snapshot poll. So the frame is painted onto
// the canvas beside it as soon as it decodes and the <video> is dropped, which
// aborts its request. The #t fragment asks for the frame just past zero, since
// some encoders open on a black frame.
export function VideoPoster({ href }) {
  const [captured, setCaptured] = useState(false);
  const canvasRef = useRef(null);
  return html`<${Fragment}>
    <canvas ref=${canvasRef} class="human-attachment-preview-thumb video-preview-poster"
      hidden=${!captured} aria-hidden="true"></canvas>
    ${!captured && html`<video class="human-attachment-preview-thumb" src=${`${href}#t=0.1`}
      preload="metadata" muted playsInline tabIndex="-1" aria-hidden="true"
      onLoadedData=${(event) => {
        const video = event.currentTarget;
        const canvas = canvasRef.current;
        try {
          canvas.width = video.videoWidth;
          canvas.height = video.videoHeight;
          canvas.getContext('2d').drawImage(video, 0, 0);
        } catch {
          // An undrawable frame leaves a blank (black) poster; the play badge
          // still says what the card is.
        }
        setCaptured(true);
      }}
      onError=${() => setCaptured(true)}></video>`}
  </${Fragment}>`;
}
