// side-panel-predict.js -- the ballistic projection behind the side panel's
// predictive peek (side-panel-reveal.js, tcl-o4nims).
//
// A pointer flung at the right edge decelerates into it. From the last few
// moves this estimates the pointer's rightward velocity, projects where it
// would stop under a typical deceleration, and says whether that is past the
// open panel's left edge. Only the horizontal component counts (tcl-bg5qm1):
// speed, steadiness, braking and the landing are all x. Every rule is
// continuous: small or slow moves near the edge are refused by their speed
// and the distance they travelled, never by a band where nothing runs. A
// snap too short to sample is arrivalReveals' business. The constants are in
// one place to be tuned on main.
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
  // flick; an ordinary move across the page is well under it), for a motion
  // that started RUNWAY_FULL_PX or more from the panel.
  MIN_VX: 1.2,
  // The runway (v2): a motion that starts close to the panel has little room
  // to show its speed, so the speed it needs falls with the distance from
  // where it started to the full panel's left edge: linearly from MIN_VX at
  // RUNWAY_FULL_PX down to MIN_VX_FLOOR at no distance. The floor is well
  // above a slow drift.
  RUNWAY_FULL_PX: 600,
  MIN_VX_FLOOR: 0.6,
  // MIN_TRAVEL_PX: the motion has moved at least this far right since it
  // started, so a twitch near the edge is not a fling, wherever it is.
  MIN_TRAVEL_PX: 80,
  // ARRIVAL_MIN_TRAVEL_PX: an arrival in the edge zone reveals at once,
  // without the dwell, when the motion that brought it there travelled at
  // least this far at the fling speed its start needs (arrivalReveals): a snap
  // from the left is over in a few frames, too few for the prediction.
  ARRIVAL_MIN_TRAVEL_PX: 200,
  // FRAME_MS: one frame, which a motion starting from rest is assumed to
  // have taken to make its first move.
  FRAME_MS: 16,
  // MIN_STEP_SHARE: every step in the window holds this share of the speed
  // required, so the speed has been sustained rather than averaged from one
  // burst.
  MIN_STEP_SHARE: 0.5,
  // BRAKE_RATIO: the recent half's speed is at least this share of the
  // window's. A pointer flung at a screen edge does not brake (the edge stops
  // it); one aimed at something on the page brakes on the way in.
  BRAKE_RATIO: 0.7,
  // CROSS_BRAKE_RATIO: the stricter share at the move that crosses into the
  // full panel's area, which reveals the panel at once: content under the
  // would-be panel is exactly what a braking move there is aimed at.
  CROSS_BRAKE_RATIO: 0.9,
  // DECEL: the deceleration the projection assumes. Lower projects further
  // (more eager); this is on the firm side, so a fling has to be aimed.
  DECEL: 0.008,
  // PEEK_RETRACT_MS: a peek whose prediction stopped holding retracts after
  // this, unless the pointer reached the edge zone or the panel first. It
  // shrinks back to PEEK_MIN_PX first, so a cancel is not a snap.
  PEEK_RETRACT_MS: 500,
  // PEEK_MIN_PX: the peek's width when it starts; it grows towards the
  // panel's full width as the pointer closes in (peekWidthFor).
  PEEK_MIN_PX: 48,
  // STOP_MS: no move for this long while the prediction holds is a stop.
  // STOP_MARGIN_PX: a stop inside the full panel's area, or this close left
  // of the peek's edge, reveals the panel rather than letting the peek go.
  STOP_MS: 90,
  STOP_MARGIN_PX: 32,
});

// recentSamples keeps the samples inside the window ending at now.
export function recentSamples(samples, now, constants = PREDICT) {
  return samples.filter((sample) => now - sample.t <= constants.WINDOW_MS);
}

// minSpeedFor is the rightward speed (px/ms) a motion needs, from the
// runway it started with (px from its start to the full panel's left edge):
// MIN_VX from RUNWAY_FULL_PX out, falling linearly to MIN_VX_FLOOR at none
// (or a start already past that edge).
export function minSpeedFor(runway, constants = PREDICT) {
  const scale = Math.max(0, Math.min(1, runway / constants.RUNWAY_FULL_PX));
  return constants.MIN_VX_FLOOR + (constants.MIN_VX - constants.MIN_VX_FLOOR) * scale;
}

// predictEdgeLanding judges samples ([{ t, x, y }], oldest first) against the
// edge: edgeX is where the edge zone starts, band the panel's vertical extent
// ({ top, bottom }, or null for the whole height), origin where the motion
// started and the full panel's left edge ({ x, fullLeft }, or null: the
// window's first move and the edge zone). Only x is judged, but the pointer
// has to be within the panel's height now. It answers { peek, reason, vx, px,
// minVx, runway, travel, landingX, y }; reason names the rule that refused (or
// 'ok'), for the debug overlay and the tests.
export function predictEdgeLanding(samples, edgeX, band = null, constants = PREDICT, origin = null) {
  const out = { peek: false, reason: 'few', vx: 0, px: 0, minVx: NaN, runway: NaN, travel: 0, landingX: NaN, y: NaN };
  if (!samples.length) return out;
  const first = samples[0];
  const last = samples[samples.length - 1];
  out.y = last.y; // where the overlay draws the landing
  // The runway is from where the motion started (origin.x, or the window's
  // first move) to the full panel's left edge (origin.fullLeft, or the edge
  // zone); the travel is how far right it has come since.
  const fullLeft = origin?.fullLeft ?? edgeX;
  const startX = origin?.x ?? first.x;
  const runway = fullLeft - startX;
  const travel = last.x - startX;
  const minVx = minSpeedFor(runway, constants);
  const base = { ...out, runway, travel, minVx };
  if (samples.length < constants.MIN_SAMPLES) return base;
  const span = last.t - first.t;
  if (span < constants.MIN_SPAN_MS) return { ...base, reason: 'short' };
  const vx = (last.x - first.x) / span;
  // The projection starts from the recent half of the window: a move that is
  // already braking to stop short shows it there, where the whole window's
  // average would still carry its early speed.
  const tail = samples.filter((sample) => last.t - sample.t <= constants.WINDOW_MS / 2);
  const from = tail.length >= 2 ? tail[0] : samples[samples.length - 2];
  const px = (last.x - from.x) / (last.t - from.t || 1);
  // Where it would stop: v² / 2a, along x.
  const landingX = px > 0 ? last.x + (px * px) / (2 * constants.DECEL) : last.x;
  const judged = { ...base, vx, px, landingX };
  if (vx < minVx) return { ...judged, reason: 'slow' };
  if (travel < constants.MIN_TRAVEL_PX) return { ...judged, reason: 'short-travel' };
  for (let i = 1; i < samples.length; i += 1) {
    const dt = samples[i].t - samples[i - 1].t;
    if (dt > 0 && (samples[i].x - samples[i - 1].x) / dt < minVx * constants.MIN_STEP_SHARE) return { ...judged, reason: 'unsteady' };
  }
  if (px < vx * constants.BRAKE_RATIO) return { ...judged, reason: 'braking' };
  if (band && (last.y < band.top || last.y > band.bottom)) return { ...judged, reason: 'off-panel' };
  // It has to land where the open panel would be: past its left edge (v2;
  // the edge zone when that is not known). A braking move into content there
  // was refused above.
  if (landingX < fullLeft) return { ...judged, reason: 'short-of-edge' };
  return { ...judged, peek: true, reason: 'ok' };
}

// arrivalReveals is whether an arrival in the edge zone (the pointer is there,
// within the panel's height) reveals the panel at once rather than after the
// dwell: the motion that brought it there travelled ARRIVAL_MIN_TRAVEL_PX or
// more, at no less than the speed its start needs (minSpeedFor), on average
// from its start. A snap from the left qualifies in its few frames; a slow
// approach, or a short one from near the edge, is left to the dwell.
export function arrivalReveals({ startX, startT, x, t, fullLeft }, constants = PREDICT) {
  const travel = x - startX;
  if (!(travel >= constants.ARRIVAL_MIN_TRAVEL_PX)) return false;
  return travel / Math.max(1, t - startT) >= minSpeedFor(fullLeft - startX, constants);
}

// crossingHolds is whether a prediction that holds may reveal the panel at the
// move crossing into the full panel's area: it must not be braking at all
// (CROSS_BRAKE_RATIO), since a braking move there is aimed at content under
// the would-be panel.
export function crossingHolds(prediction, constants = PREDICT) {
  return !!prediction?.peek && prediction.px >= prediction.vx * constants.CROSS_BRAKE_RATIO;
}

// peekWidthFor is the growing peek's width (v2): PEEK_MIN_PX when the peek
// starts (at startX), growing with the pointer's progress towards the full
// panel's left edge (fullLeft), or towards the edge zone (edgeX) for a
// motion that started inside the full panel's area, up to panelW. It never
// shrinks while the prediction holds (prev).
export function peekWidthFor({ startX, x, fullLeft, edgeX, panelW, prev = 0 }, constants = PREDICT) {
  const min = constants.PEEK_MIN_PX;
  if (!(panelW > min)) return Math.max(prev, min);
  const goal = startX < fullLeft ? fullLeft : edgeX;
  const progress = goal > startX ? Math.max(0, Math.min(1, (x - startX) / (goal - startX))) : 1;
  return Math.max(prev, Math.round(min + (panelW - min) * progress));
}

// stopReveals is whether a stop at x, while the prediction holds, reveals the
// panel: inside the full panel's area, or within STOP_MARGIN_PX left of the
// peek's edge (peekLeft).
export function stopReveals(x, fullLeft, peekLeft, constants = PREDICT) {
  return x >= fullLeft || x >= peekLeft - constants.STOP_MARGIN_PX;
}
