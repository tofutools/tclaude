// side-panel-predict.js -- the ballistic projection behind the side panel's
// predictive peek (side-panel-reveal.js, tcl-o4nims).
//
// A pointer flung at the right edge decelerates into it. From the last few
// moves this estimates the pointer's velocity, projects where it would stop
// under a typical deceleration, and says whether that is at or past the edge
// zone. It is deliberately conservative: a wrong peek covers a strip of the
// page's right-hand content, so every rule below is a reason NOT to peek, and
// the constants are in one place to be tuned on main.
//
// Pure: no DOM, no timers. side-panel-reveal.js feeds it samples and acts on
// the answer; the debug overlay draws the answer.

// PREDICT holds the tunable constants. Velocities are px/ms, deceleration
// px/ms².
export const PREDICT = Object.freeze({
  // WINDOW_MS: only moves this recent count.
  WINDOW_MS: 80,
  // MIN_SAMPLES: this many moves in the window, so one jump is not a fling.
  MIN_SAMPLES: 4,
  // MIN_SPAN_MS: and they span at least this long.
  MIN_SPAN_MS: 30,
  // MIN_VX: the window's rightward speed (1.2 px/ms = 1200 px/s, a fast
  // flick; an ordinary move across the page is well under it).
  MIN_VX: 1.2,
  // MIN_STEP_VX: every step in the window holds this rightward speed, so the
  // speed has been sustained rather than averaged from one burst.
  MIN_STEP_VX: 0.6,
  // MAX_SLOPE: |vy| / vx at most this (about 17°): mostly horizontal. A
  // diagonal flick is aimed at something on the page, not the edge.
  MAX_SLOPE: 0.3,
  // BRAKE_RATIO: the recent half's speed is at least this share of the
  // window's. A pointer flung at a screen edge does not brake (the edge stops
  // it); one aimed at something on the page brakes on the way in.
  BRAKE_RATIO: 0.7,
  // DECEL: the deceleration the projection assumes. Lower projects further
  // (more eager); this is on the firm side, so a fling has to be aimed.
  DECEL: 0.008,
  // MIN_DISTANCE_PX: a pointer already this close to the edge is the edge
  // zone's business (its dwell), not a prediction's.
  MIN_DISTANCE_PX: 48,
  // HANDLE_MARGIN_PX: a landing this close above or below the folded handle
  // is aiming at the handle (to pin the panel open), which a peek would move
  // out from under the click.
  HANDLE_MARGIN_PX: 40,
  // PEEK_RETRACT_MS: a peek whose prediction stopped holding retracts after
  // this, unless the pointer reached the edge zone or the panel first.
  PEEK_RETRACT_MS: 500,
});

// recentSamples keeps the samples inside the window ending at now.
export function recentSamples(samples, now, constants = PREDICT) {
  return samples.filter((sample) => now - sample.t <= constants.WINDOW_MS);
}

// predictEdgeLanding judges samples ([{ t, x, y }], oldest first) against the
// edge: edgeX is where the edge zone starts, band the panel's vertical extent
// ({ top, bottom }, or null for the whole height), handle the folded handle's
// ({ top, bottom }, or null). It answers
// { peek, reason, vx, vy, landingX, landingY }; reason names the rule that
// refused (or 'ok'), for the debug overlay and the tests.
export function predictEdgeLanding(samples, edgeX, band = null, constants = PREDICT, handle = null) {
  const out = { peek: false, reason: 'few', vx: 0, vy: 0, landingX: NaN, landingY: NaN };
  if (samples.length < constants.MIN_SAMPLES) return out;
  const first = samples[0];
  const last = samples[samples.length - 1];
  const span = last.t - first.t;
  if (span < constants.MIN_SPAN_MS) return { ...out, reason: 'short' };
  const vx = (last.x - first.x) / span;
  const vy = (last.y - first.y) / span;
  // The projection starts from the recent half of the window: a move that is
  // already braking to stop short shows it there, where the whole window's
  // average would still carry its early speed.
  const tail = samples.filter((sample) => last.t - sample.t <= constants.WINDOW_MS / 2);
  const from = tail.length >= 2 ? tail[0] : samples[samples.length - 2];
  const tailSpan = last.t - from.t || 1;
  const px = (last.x - from.x) / tailSpan;
  const py = (last.y - from.y) / tailSpan;
  // Where it would stop: v² / 2a along the motion, split onto the axes.
  const speed = Math.hypot(px, py);
  const travel = speed > 0 ? (speed * speed) / (2 * constants.DECEL) : 0;
  const landingX = speed > 0 ? last.x + (px / speed) * travel : last.x;
  const landingY = speed > 0 ? last.y + (py / speed) * travel : last.y;
  const judged = { ...out, vx, vy, landingX, landingY };
  if (vx < constants.MIN_VX) return { ...judged, reason: 'slow' };
  for (let i = 1; i < samples.length; i += 1) {
    const dt = samples[i].t - samples[i - 1].t;
    if (dt > 0 && (samples[i].x - samples[i - 1].x) / dt < constants.MIN_STEP_VX) return { ...judged, reason: 'unsteady' };
  }
  if (Math.abs(vy) > vx * constants.MAX_SLOPE) return { ...judged, reason: 'diagonal' };
  if (px < vx * constants.BRAKE_RATIO) return { ...judged, reason: 'braking' };
  if (edgeX - last.x < constants.MIN_DISTANCE_PX) return { ...judged, reason: 'near' };
  if (landingX < edgeX) return { ...judged, reason: 'short-of-edge' };
  if (band && (landingY < band.top || landingY > band.bottom)) return { ...judged, reason: 'off-panel' };
  if (handle && landingY >= handle.top - constants.HANDLE_MARGIN_PX && landingY <= handle.bottom + constants.HANDLE_MARGIN_PX) {
    return { ...judged, reason: 'handle' };
  }
  return { ...judged, peek: true, reason: 'ok' };
}
