// claude-wheel.js -- how far Claude Code's fullscreen view moves for each
// wheel notch, so a phone swipe can send the notches that move the content
// as far as the finger went. No DOM.
//
// Measured on Claude Code 2.1.323 in a tclaude pane (tmux, mouse off, so the
// SGR wheel reports reach Claude itself), and read from its wheel handler,
// which this mirrors. In a terminal that is not xterm.js, which tmux is to
// Claude, the base is 1 line a notch, and its wheel acceleration (on by
// default) works per notch on the gap since the one before:
//   - a notch more than ACCEL_GAP_MS after the last scrolls the base, 1 line;
//   - a closer one adds ACCEL_STEP to the multiplier, up to ACCEL_MAX lines;
//   - the first notch against the last direction scrolls nothing (Claude
//     takes it for a wheel's stray tick), and the one after it starts over;
//   - a stray tick followed within FLIP_MS by a notch the old way puts it in
//     a wheel mode that climbs faster, left after WHEEL_IDLE_MS, or after a
//     burst of notches under BURST_MS apart, each of which scrolls one line.
// Measured: 1 notch, 1 line; 4 notches 80ms apart, 4 lines; 4 at once, 4
// (1+1+1+1); 8 notches 10ms apart, 13 (1,1,1,1,2,2,2,3); a reversal's first
// notch, 0. CLAUDE_CODE_SCROLL_SPEED changes the base; tclaude leaves it.

const BASE = 1;
const ACCEL_GAP_MS = 40;
const ACCEL_STEP = 0.3;
const ACCEL_MAX = Math.max(6 * Math.min(BASE, 1), BASE * 2);
const FLIP_MS = 200;
const WHEEL_IDLE_MS = 1500;
const BURST_MS = 5;
const BURST_EXIT = 5;
const WHEEL_HALF_LIFE_MS = 150;
const WHEEL_BOOST = 15;
const WHEEL_STEP_MAX = 3;
const WHEEL_MAX = Math.max(15 * Math.min(BASE, 1), BASE * 2);

// claudeWheelState is the handler's state before any notch.
export function claudeWheelState() {
  return { time: -Infinity, mult: BASE, dir: 0, pendingFlip: false, wheelMode: false, burst: 0 };
}

// claudeWheelStep is one notch: dir is its direction (any two distinct
// values), at the time t in ms. It returns the lines it scrolls and the state
// after it, leaving the state passed in as it was, so a caller can ask what a
// notch would do before sending it.
export function claudeWheelStep(state, dir, t) {
  const s = { ...state };
  const lines = (mult) => Math.max(1, Math.floor(mult));
  if (s.wheelMode && t - s.time > WHEEL_IDLE_MS) {
    s.wheelMode = false;
    s.burst = 0;
    s.mult = BASE;
  }
  if (s.pendingFlip) {
    s.pendingFlip = false;
    if (dir !== s.dir || t - s.time > FLIP_MS) {
      s.dir = dir;
      s.time = t;
      s.mult = BASE;
      return { lines: lines(s.mult), state: s };
    }
    s.wheelMode = true;
  }
  const gap = t - s.time;
  if (dir !== s.dir && s.dir !== 0) {
    // The direction is kept: a notch back the old way within FLIP_MS reads
    // the reversal as a stray tick.
    s.pendingFlip = true;
    s.time = t;
    return { lines: 0, state: s };
  }
  s.dir = dir;
  s.time = t;
  if (s.wheelMode) {
    if (gap < BURST_MS) {
      s.burst += 1;
      if (s.burst < BURST_EXIT) return { lines: 1, state: s };
      s.wheelMode = false;
      s.burst = 0;
      s.mult = BASE;
    } else {
      s.burst = 0;
    }
  }
  if (s.wheelMode) {
    const decay = 0.5 ** (gap / WHEEL_HALF_LIFE_MS);
    s.mult = Math.min(WHEEL_MAX, 1 + (s.mult - 1) * decay + WHEEL_BOOST * decay, s.mult + WHEEL_STEP_MAX);
    return { lines: lines(s.mult), state: s };
  }
  s.mult = gap > ACCEL_GAP_MS ? BASE : Math.min(ACCEL_MAX, s.mult + ACCEL_STEP);
  return { lines: lines(s.mult), state: s };
}
