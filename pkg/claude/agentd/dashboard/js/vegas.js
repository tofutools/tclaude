// vegas.js — background music for slop ("vegas") mode.
//
// In slop mode a "Vegas" tab appears after Config holding a lounge-music
// player. The player is created when slop turns on and torn down when it
// turns off, so the soundtrack stops the moment you leave the mode.
//
// We stream an internet radio station via an <audio> element rather than
// embedding YouTube: monetised YouTube content serves ads, and many of
// those videos disable embedding outright (the original ?v=… link did —
// it rendered "Video unavailable / Watch on YouTube"). A plain ICE/MP3
// stream has neither problem, plays forever, and pauses cleanly.
//
// We react to slop state via the `tclaude:slop` CustomEvent that slop.js
// dispatches on every apply/toggle ({ detail: { active } }), rather than
// reaching into slop.js's internals. Companion to slop.js (the theme
// toggle) and slop-fx.js (the visual flair).

import { isSlopActive } from './slop.js';

// The station. SomaFM is listener-supported and genuinely commercial-
// free; "Illinois Street Lounge" is its vintage cocktail / Rat-Pack
// channel — the closest thing to a Vegas-lounge soundtrack. Swapping the
// vibe is a one-object edit here: e.g. secretagent (spy-jazz) or
// groovesalad (downtempo) on the same host, or any other ICE/MP3 stream.
//
// Browsers gate autoplay-WITH-SOUND on a recent user gesture: the
// header-icon click that turns slop on satisfies that (startMusic runs
// inside that click's call stack). A bare ?slop=1 page load — e.g.
// `tclaude agentd serve --slop` — has no gesture yet, so we start muted
// and unmute on the first interaction (see playWithSound). The <audio
// controls> also gives the user a manual play/volume fallback.
const VEGAS_STREAM = {
  src: 'https://ice1.somafm.com/illstreet-128-mp3',
  label: 'SomaFM — Illinois Street Lounge',
  home: 'https://somafm.com/illstreet/',
};

// cancelUnmuteArm tears down a pending "unmute on first gesture" arming
// (see playWithSound). Held at module scope so stopMusic / a rebuild can
// cancel a still-waiting arm instead of leaking listeners that reference
// a removed <audio>.
let cancelUnmuteArm = null;

function startMusic() {
  const host = document.getElementById('vegas-player');
  if (!host) return;
  // Already playing — don't rebuild, which would restart the stream on a
  // redundant event (e.g. a duplicate tclaude:slop).
  if (host.querySelector('audio')) return;
  // Drop any stale arm from a previous stop, and clear a leftover error
  // card from a previous failed attempt.
  if (cancelUnmuteArm) cancelUnmuteArm();
  host.replaceChildren();

  const label = document.createElement('div');
  label.className = 'vegas-nowplaying';
  label.textContent = '♪ Now playing — ' + VEGAS_STREAM.label;

  const audio = document.createElement('audio');
  audio.src = VEGAS_STREAM.src;
  audio.autoplay = true;
  audio.controls = true;
  audio.preload = 'auto';
  audio.setAttribute('aria-label', VEGAS_STREAM.label);
  // A dead/unreachable stream should explain itself rather than sit as a
  // silent broken control.
  audio.addEventListener('error', () => showStreamError(host), { once: true });

  host.appendChild(label);
  host.appendChild(audio);
  playWithSound(audio);
}

// playWithSound starts the stream audibly. From a toggle click we hold a
// fresh user gesture, so play() resolves and music plays at once. On a
// gestureless ?slop=1 load the browser rejects autoplay-with-sound — so
// we fall back to muted playback (always permitted), which gets the
// stream live, and unmute on the very first interaction anywhere on the
// page. The result: music is audible the instant the user touches the
// page, whatever they click, with no buffering gap.
function playWithSound(audio) {
  audio.muted = false;
  const p = audio.play();
  if (p && typeof p.catch === 'function') {
    p.catch(() => armMutedUntilGesture(audio));
  }
}

function armMutedUntilGesture(audio) {
  audio.muted = true;
  // Muted autoplay is allowed without a gesture; this connects the
  // stream so unmuting later is instant.
  audio.play().catch(() => {});
  const unmute = () => {
    cancelUnmuteArm();
    audio.muted = false;
    // Re-issue play() inside the gesture in case the muted attempt was
    // itself suspended.
    audio.play().catch(() => {});
  };
  cancelUnmuteArm = () => {
    document.removeEventListener('pointerdown', unmute, true);
    document.removeEventListener('keydown', unmute, true);
    cancelUnmuteArm = null;
  };
  document.addEventListener('pointerdown', unmute, true);
  document.addEventListener('keydown', unmute, true);
}

function stopMusic() {
  if (cancelUnmuteArm) cancelUnmuteArm();
  const host = document.getElementById('vegas-player');
  if (!host) return;
  // Pause + drop the src + load() before removing so the ongoing stream
  // fetch is actually aborted, not just detached — otherwise the
  // connection (and audio) can linger. This is the whole point of the
  // module: music must stop when you leave the mode.
  const audio = host.querySelector('audio');
  if (audio) {
    audio.pause();
    audio.removeAttribute('src');
    audio.load();
  }
  host.replaceChildren();
}

function showStreamError(host) {
  host.replaceChildren();
  const msg = document.createElement('div');
  msg.className = 'vegas-error';
  msg.append('🎲 Couldn’t reach the stream — ');
  const a = document.createElement('a');
  a.href = VEGAS_STREAM.home;
  a.target = '_blank';
  a.rel = 'noopener';
  a.textContent = 'open it on SomaFM ↗';
  msg.appendChild(a);
  host.appendChild(msg);
}

// When slop turns off while the Vegas tab is the active one, that tab's
// nav button has just been CSS-hidden, leaving a stranded visible
// section. Fall back to Groups so the user isn't staring at a dead
// player. Mirrors bindTabs()'s active-class toggling in refresh.js (kept
// local to avoid a circular import).
function leaveVegasTabIfActive() {
  const sec = document.getElementById('tab-vegas');
  if (!sec || !sec.classList.contains('active')) return;
  document.querySelectorAll('nav button').forEach(b =>
    b.classList.toggle('active', b.dataset.tab === 'groups'));
  document.querySelectorAll('main section').forEach(s =>
    s.classList.toggle('active', s.id === 'tab-groups'));
}

export function bindVegasMusic() {
  document.addEventListener('tclaude:slop', (e) => {
    const active = !!(e.detail && e.detail.active);
    if (active) {
      // Reached from a toggle click → we're inside that gesture's call
      // stack, so autoplay-with-sound is granted.
      startMusic();
    } else {
      stopMusic();
      leaveVegasTabIfActive();
    }
  });
  // Page loaded already in slop mode (e.g. `--slop` → ?slop=1). The
  // initial tclaude:slop fired from applySlopThemeIfRequested() before
  // this binder existed, so kick the player off here. startMusic handles
  // the gestureless autoplay block by starting muted and unmuting on the
  // first interaction.
  if (isSlopActive()) startMusic();
}
