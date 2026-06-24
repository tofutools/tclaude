// slop-audio.js — synthesized casino sound effects + the master sound
// switch for slop mode.
//
// The dashboard's slop mode was all silent flair: coins spray, reels
// spin, jackpots flash — without a peep — while the Vegas tab's lounge
// radio played on a completely separate control. This module gives the
// effects a voice AND becomes the single master toggle for ALL slop
// sound: the synthesized FX here plus the Vegas radio in vegas.js.
//
// The FX are synthesized live with the Web Audio API (oscillators +
// filtered noise), so there are NO asset files to embed, nothing to
// 404, and it works offline. They subscribe to the same `tclaude:slopfx`
// event bus slop-fx.js emits and turn each effect into a sound.
//
// The master switch lives on the 🔇/🔊 button in the header (slop-only;
// CSS-hidden otherwise). It is ON by default — entering slop mode is
// opting in to the full casino, so you get music + FX — and is
// remembered in localStorage. Flipping it broadcasts a `tclaude:slopsound`
// event that vegas.js listens for to pause/resume the radio, so the one
// button governs both. The Vegas tab still has its own play/volume slider
// for fine control. Browsers block audio without a user gesture; the
// toggle click (or, on a gestureless ?slop=1 load, the first click
// anywhere) is the gesture that brings the AudioContext to life.
//
// Note on reduced motion: slop-fx.js short-circuits every effect — and
// therefore every `tclaude:slopfx` dispatch — when the OS asks for
// reduced motion, so a reduced-motion user gets a still, quiet dashboard
// (the radio, which isn't motion, still honours the master switch).

import { isSlopActive } from './slop.js';

const STORAGE_KEY = 'tclaude.slop.sound';

let ctx = null;      // AudioContext, built lazily inside a user gesture
let master = null;   // master gain node — volume + an instant kill switch
let lastCoinAt = 0;  // rate-limit guard for the dense click-coin tick

// FX_HEADROOM is the historical full level of the master gain —
// individual sounds stay well under 1.0 on top of it. The persisted
// effects volume (slop-volume.js) scales it: 100% = this value.
const FX_HEADROOM = 0.5;
let effectsVolume = 100; // percent, 0–100; applied to master below

// setEffectsVolume applies the persisted FX volume (percent). Owned by
// slop-volume.js, which loads it from /api/slop/volumes and calls this
// on load and on every slider move. Takes effect immediately on a live
// ctx and is baked into ensureCtx for one built later.
export function setEffectsVolume(pct) {
  effectsVolume = Math.min(100, Math.max(0, Number(pct) || 0));
  if (master) master.gain.value = FX_HEADROOM * (effectsVolume / 100);
}

// prefersEnabled defaults to ON when the key was never set — slop sound
// is part of the casino, opt-out rather than opt-in. An explicit '0'
// (the user muted) is honoured. (Function declaration → hoisted, so the
// `enabled` initializer below can call it.)
function prefersEnabled() {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    return v === null ? true : v === '1';
  } catch { return true; }
}

// Master on/off, mirroring the toggle + localStorage. Initialized at
// module load (not just in bindSlopAudio) so isSlopSoundEnabled() is
// correct regardless of bootstrap call order — vegas.js reads it to gate
// the radio and must not depend on bindSlopAudio() running first.
let enabled = prefersEnabled();
function persist(on) {
  try { localStorage.setItem(STORAGE_KEY, on ? '1' : '0'); } catch { /* private mode / blocked — fine */ }
}

// isSlopSoundEnabled is the master state vegas.js reads before starting
// the radio (and on the tclaude:slopsound event below).
export function isSlopSoundEnabled() {
  return enabled;
}

// ensureCtx lazily builds the AudioContext + master gain. The first call
// must happen inside a user gesture (browsers refuse to start audio
// otherwise) — every path here runs from a click handler or only after
// the user flipped the toggle, itself a click. A ctx that the browser
// parked as "suspended" is resumed on the spot. Returns null when the
// browser has no Web Audio at all, so callers no-op gracefully.
function ensureCtx() {
  if (ctx) {
    if (ctx.state === 'suspended') ctx.resume().catch(() => {});
    return ctx;
  }
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) return null;
  ctx = new AC();
  master = ctx.createGain();
  master.gain.value = FX_HEADROOM * (effectsVolume / 100);
  master.connect(ctx.destination);
  return ctx;
}

// tone schedules one oscillator note with a fast attack and exponential
// decay. `start` is an offset (seconds) from "now" so a caller can lay
// several notes into a short melody. exponentialRamp can't touch 0, so
// the envelope floors at 0.0001 (effectively silent).
function tone(freq, start, dur, type, peak) {
  const osc = ctx.createOscillator();
  const g = ctx.createGain();
  osc.type = type || 'sine';
  osc.frequency.value = freq;
  const t0 = ctx.currentTime + start;
  g.gain.setValueAtTime(0.0001, t0);
  g.gain.exponentialRampToValueAtTime(peak || 0.3, t0 + 0.012);
  g.gain.exponentialRampToValueAtTime(0.0001, t0 + dur);
  osc.connect(g);
  g.connect(master);
  osc.start(t0);
  osc.stop(t0 + dur + 0.02);
}

// noiseBurst is a short band-passed white-noise hit — the body of a reel
// whir or a mechanical clunk. The buffer is generated per call (cheap at
// these durations) so there's nothing to preload.
function noiseBurst(start, dur, freq, q, peak) {
  const len = Math.max(1, Math.floor(ctx.sampleRate * dur));
  const buf = ctx.createBuffer(1, len, ctx.sampleRate);
  const data = buf.getChannelData(0);
  for (let i = 0; i < len; i++) data[i] = Math.random() * 2 - 1;
  const src = ctx.createBufferSource();
  src.buffer = buf;
  const filt = ctx.createBiquadFilter();
  filt.type = 'bandpass';
  filt.frequency.value = freq;
  filt.Q.value = q || 1;
  const g = ctx.createGain();
  const t0 = ctx.currentTime + start;
  g.gain.setValueAtTime(peak || 0.25, t0);
  g.gain.exponentialRampToValueAtTime(0.0001, t0 + dur);
  src.connect(filt);
  filt.connect(g);
  g.connect(master);
  src.start(t0);
  src.stop(t0 + dur + 0.02);
}

// ─── The sound palette ─────────────────────────────────────────────
// Each maps to one `fx` from the bus. Kept short and bright so a busy
// dashboard doesn't turn into a wall of noise.

function coinClink() {
  // A quick "ting-ting" — two high sine blips a third apart.
  tone(1318, 0, 0.08, 'sine', 0.22);    // E6
  tone(1760, 0.05, 0.10, 'sine', 0.18); // A6
}
function clunk() {
  // The reels settling: a low thunk + a damped noise body.
  noiseBurst(0, 0.10, 220, 0.7, 0.16);
  tone(140, 0, 0.10, 'square', 0.10);
}
function reelWhir() {
  // The half-second blur of a manual pull — filtered noise, no pitch.
  noiseBurst(0, 0.5, 900, 0.8, 0.10);
}
function chaChing() {
  // The register opening on a real working→idle win: a bright two-note
  // rise capped with a sparkle.
  tone(880, 0, 0.10, 'triangle', 0.24);     // A5
  tone(1318, 0.10, 0.24, 'triangle', 0.28); // E6
  noiseBurst(0.10, 0.16, 2200, 2, 0.06);
}
function fanfare(big) {
  // A jackpot: an ascending C-major arpeggio. `big` (the Konami mega)
  // stacks an octave-up flourish and a longer sparkle on top.
  [523, 659, 784, 1046].forEach((f, i) => tone(f, i * 0.09, 0.18, 'triangle', 0.26));
  if (big) {
    [1318, 1568, 2093].forEach((f, i) => tone(f, 0.36 + i * 0.08, 0.22, 'triangle', 0.24));
    noiseBurst(0.36, 0.5, 3000, 1.5, 0.05);
  }
}
function leverPull() {
  // The big side lever being yanked: a meaty mechanical ka-CHUNK (low
  // thunk + noise body) followed by the reels taking off (a longer
  // whir). Louder/longer than a single machine's 'spin' so a deliberate
  // lever pull feels like a bigger event.
  noiseBurst(0, 0.09, 180, 0.6, 0.24);
  tone(110, 0, 0.12, 'square', 0.16);
  noiseBurst(0.11, 0.55, 850, 0.8, 0.12);
}

// play turns one `fx` name into a sound. Silently no-ops when muted or
// when Web Audio is unavailable.
function play(fx) {
  if (!enabled) return;
  if (!ensureCtx()) return;
  switch (fx) {
    case 'coin': {
      // Clicks can come in fast bursts; throttle so they don't smear
      // into a continuous jingle.
      const now = performance.now();
      if (now - lastCoinAt < 60) return;
      lastCoinAt = now;
      coinClink();
      break;
    }
    case 'spin':      reelWhir(); break;
    case 'stop':      clunk(); break;
    case 'lever':     leverPull(); break;
    case 'win-pull':
    case 'win-spawn': fanfare(false); break;
    case 'win-idle':  chaChing(); break;
    case 'win-mega':  fanfare(true); break;
  }
}

// renderToggle paints the header button to match `enabled`.
function renderToggle() {
  const btn = document.getElementById('slop-sound-btn');
  if (!btn) return;
  btn.textContent = enabled ? '🔊' : '🔇';
  btn.setAttribute('aria-pressed', enabled ? 'true' : 'false');
  btn.title = enabled
    ? 'Slop sound (music + FX): on — click to mute'
    : 'Slop sound (music + FX): off — click to unmute';
}

function setEnabled(on) {
  enabled = on;
  persist(on);
  renderToggle();
  if (on) {
    // Build/resume the ctx inside this click gesture so the first real
    // effect is instant, and give a tiny confirmation blip — but only in
    // slop ("casino") mode. In the regular-mode Vegas view (body.vegas
    // without body.slop) there are no FX, so a casino coin blip would
    // contradict the "music + volume, no FX" intent; the same button there
    // just (un)mutes the radio. The ctx is still warmed for a later slop.
    const ctxReady = ensureCtx();
    if (ctxReady && isSlopActive()) coinClink();
  } else if (ctx && ctx.state === 'running') {
    ctx.suspend().catch(() => {});
  }
  // Tell vegas.js to start/stop the radio so the one button governs both
  // the FX (above) and the music. One-way, like the other slop events.
  document.dispatchEvent(new CustomEvent('tclaude:slopsound', { detail: { enabled: on } }));
}

export function bindSlopAudio() {
  enabled = prefersEnabled();
  renderToggle();
  const btn = document.getElementById('slop-sound-btn');
  if (btn) btn.addEventListener('click', () => setEnabled(!enabled));

  // The sound side of the shared bus. The body-class check keeps a stray
  // timer-driven fx (the working→idle scan) from sounding off the moment
  // after slop is turned off but before the suspend below lands.
  document.addEventListener('tclaude:slopfx', (e) => {
    if (!isSlopActive()) return;
    play(e.detail && e.detail.fx);
  });

  // Leaving slop suspends the ctx so nothing can make noise on the plain
  // dashboard; re-entering (always via a click on the header icon, so a
  // valid gesture) resumes it lazily when sound is enabled.
  document.addEventListener('tclaude:slop', (e) => {
    const active = !!(e.detail && e.detail.active);
    if (!active) {
      if (ctx && ctx.state === 'running') ctx.suspend().catch(() => {});
    } else if (enabled) {
      ensureCtx();
    }
  });
}
