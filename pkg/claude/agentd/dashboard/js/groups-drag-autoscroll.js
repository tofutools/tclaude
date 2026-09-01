// groups-drag-autoscroll.js — keep long Groups rosters reachable during the
// two native drags owned by the tab (member rows and group headers).
//
// Native HTML DnD does not reliably autoscroll the document in every browser.
// Track the latest drag position and scroll on animation frames instead of on
// dragover itself: dragover cadence is irregular and may pause while the page
// moves beneath a stationary pointer.

const EDGE_MIN = 56;
const EDGE_MAX = 120;
const EDGE_VIEWPORT_RATIO = 0.14;
const MIN_SPEED = 80;
const MAX_SPEED = 800;

// edgeScrollVelocity returns pixels/second. The quadratic ramp keeps the outer
// part of the hot zone precise while still making a pointer held at the very
// edge traverse a long roster quickly.
function edgeScrollVelocity(clientY, viewportHeight) {
  if (!Number.isFinite(clientY) || !Number.isFinite(viewportHeight) || viewportHeight <= 0) return 0;
  // Pixel-align the boundary so floating-point residue cannot turn the exact
  // edge into MIN_SPEED instead of the intended zero-speed boundary.
  const edge = Math.round(Math.min(EDGE_MAX, Math.max(EDGE_MIN, viewportHeight * EDGE_VIEWPORT_RATIO)));
  let direction = 0;
  let pressure = 0;
  if (clientY < edge) {
    direction = -1;
    pressure = (edge - clientY) / edge;
  } else if (clientY > viewportHeight - edge) {
    direction = 1;
    pressure = (clientY - (viewportHeight - edge)) / edge;
  }
  if (!direction) return 0;
  pressure = Math.min(1, Math.max(0, pressure));
  return direction * (MIN_SPEED + (MAX_SPEED - MIN_SPEED) * pressure * pressure);
}

function bindGroupsDragAutoScroll() {
  let active = false;
  let velocity = 0;
  let frameID = 0;
  let lastFrame = 0;

  const stopFrame = () => {
    if (frameID) window.cancelAnimationFrame(frameID);
    frameID = 0;
    lastFrame = 0;
  };
  const stop = () => {
    active = false;
    velocity = 0;
    stopFrame();
  };
  const frame = (now) => {
    frameID = 0;
    if (!active || !velocity) return;
    if (lastFrame) {
      // Bound a delayed/background frame so returning to the dashboard never
      // produces one enormous jump.
      const elapsed = Math.min(50, now - lastFrame);
      window.scrollBy(0, velocity * elapsed / 1000);
    }
    lastFrame = now;
    frameID = window.requestAnimationFrame(frame);
  };
  const update = (e) => {
    if (!active) return;
    // The fixed retire/delete target is an intentional destination, not a cue
    // to move the roster underneath it.
    velocity = e.target.closest?.('#dnd-trash')
      ? 0
      : edgeScrollVelocity(e.clientY, window.innerHeight);
    if (!velocity) {
      stopFrame();
      return;
    }
    if (!frameID) frameID = window.requestAnimationFrame(frame);
  };
  const start = (e) => {
    const source = e.target.closest?.('.dnd-draggable, [data-group-reorder]');
    if (!source) return;
    active = true;
    velocity = 0;
    lastFrame = 0;
  };

  document.addEventListener('dragstart', start);
  document.addEventListener('dragover', update);
  document.addEventListener('dragend', stop);
  document.addEventListener('drop', stop);

  return () => {
    stop();
    document.removeEventListener('dragstart', start);
    document.removeEventListener('dragover', update);
    document.removeEventListener('dragend', stop);
    document.removeEventListener('drop', stop);
  };
}

export { bindGroupsDragAutoScroll, edgeScrollVelocity };
