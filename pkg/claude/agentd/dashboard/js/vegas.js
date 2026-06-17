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
//
// The radio is also governed by the master sound switch slop-audio.js
// owns (the 🔇/🔊 button in the header): we only auto-start when sound
// is enabled, and we start/stop in response to its `tclaude:slopsound`
// event so one button mutes both the FX and the music. The custom
// play/pause button here is a finer per-track control when sound is on.
//
// "Now playing" song name: browsers don't expose the stream's ICY
// metadata to script, so we can't read the title off the <audio>. Instead
// agentd proxies SomaFM's tiny recent-songs feed at /api/slop/nowplaying
// (see dashboard_slop_nowplaying.go); we poll it while music plays and
// show "♪ Artist — Title" with the title linking to a YouTube search for
// the track. Best-effort: a blip just leaves the last song shown, and an
// empty feed hides the song line — the music is unaffected either way.

import { isSlopActive } from './slop.js';
import { isSlopSoundEnabled } from './slop-audio.js';

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
// and unmute on the first interaction (see playWithSound). The custom
// play button also gives the user a manual way to start it.
const VEGAS_STREAM = {
  src: 'https://ice1.somafm.com/illstreet-128-mp3',
  label: 'SomaFM — Illinois Street Lounge',
  home: 'https://somafm.com/illstreet/',
};

// musicVolume is the persisted radio volume in percent (0–100), owned
// by slop-volume.js (loaded from /api/slop/volumes). Applied to the
// <audio> element when it is built and on every setter call.
let musicVolume = 100;

// setMusicVolume applies the persisted music volume (percent) to a live
// player and remembers it for the next startMusic. Called by
// slop-volume.js on load and on every slider move.
export function setMusicVolume(pct) {
  musicVolume = Math.min(100, Math.max(0, Number(pct) || 0));
  const audio = document.querySelector('#vegas-player audio');
  if (audio && Math.round(audio.volume * 100) !== musicVolume) {
    audio.volume = musicVolume / 100;
  }
}

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

  // Now-playing "stereo display": a dynamic song readout (artist / title /
  // time-on-air, filled by the poller below — empty until the first
  // successful fetch) above the static station line. It sits above the
  // play/pause transport so the player reads as a filled card rather than
  // an empty box, now the (meaningless live-stream) seek bar is gone.
  const label = document.createElement('div');
  label.className = 'vegas-nowplaying';

  const songLine = document.createElement('div');
  songLine.className = 'vegas-song';
  songLine.id = 'vegas-song';

  const stationLine = document.createElement('div');
  stationLine.className = 'vegas-station';
  stationLine.textContent = '📻 ' + VEGAS_STREAM.label;

  label.appendChild(songLine);
  label.appendChild(stationLine);

  // The <audio> is a controls-less engine — we draw our own chrome below.
  // The native <audio controls> bar can't be themed to the casino card,
  // and with the (meaningless live-stream) seek bar hidden it rendered as
  // a big empty white pill.
  const audio = document.createElement('audio');
  audio.src = VEGAS_STREAM.src;
  audio.autoplay = true;
  audio.preload = 'auto';
  audio.setAttribute('aria-label', VEGAS_STREAM.label);
  audio.volume = musicVolume / 100;
  // A dead/unreachable stream should explain itself rather than sit as a
  // silent broken control.
  audio.addEventListener('error', () => showStreamError(host), { once: true });

  // Custom play/pause transport. Volume lives in the header mixer
  // (slop-volume.js's 🎚️ popover), so play/pause is the only control the
  // player itself needs.
  const transport = document.createElement('div');
  transport.className = 'vegas-transport';
  const playBtn = document.createElement('button');
  playBtn.type = 'button';
  playBtn.className = 'vegas-play';
  transport.appendChild(playBtn);

  // "Active" = audibly playing (not paused, not muted). The armed-muted
  // autoplay of a gestureless ?slop=1 load reads as not-yet-active, so the
  // button shows ▶ to invite the click that brings the sound up.
  const syncPlayBtn = () => {
    const active = !audio.paused && !audio.muted;
    playBtn.textContent = active ? '⏸' : '▶';
    playBtn.setAttribute('aria-label', active ? 'Pause' : 'Play');
  };
  playBtn.addEventListener('click', () => {
    // Decide from the button's shown intent, not the post-event audio
    // state: the document-level unmute arm (armMutedUntilGesture) fires on
    // this same gesture and would otherwise flip a "play" into a pause.
    if (playBtn.getAttribute('aria-label') === 'Play') {
      audio.muted = false;
      audio.play().catch(() => {});
    } else {
      audio.pause();
    }
  });
  audio.addEventListener('play', syncPlayBtn);
  audio.addEventListener('pause', syncPlayBtn);
  audio.addEventListener('playing', syncPlayBtn);
  // Mute flips (the arm muting on a gestureless load, then unmuting on the
  // first interaction) fire volumechange but not play/pause — keep the icon
  // honest about whether sound is actually coming out.
  audio.addEventListener('volumechange', syncPlayBtn);
  syncPlayBtn();

  host.appendChild(label);
  host.appendChild(audio);
  host.appendChild(transport);
  playWithSound(audio);
  startNowPlayingPoll();
}

// ─── Now-playing poller ────────────────────────────────────────────────
// Polls /api/slop/nowplaying (agentd's SomaFM proxy) while music plays and
// paints the song readout into #vegas-song — an artist line, the title (a
// link to a YouTube search for the track), and a time-on-air line.
// Independent of playback state, so the song still shows while the stream
// is armed-muted before the first gesture.
//
// The "· 1:23" after the title is REAL time-on-air, counted up from the
// track's actual start (the feed's start timestamp) by a 1s ticker. It is
// NOT a progress bar: SomaFM exposes no song duration, so there's no honest
// percent-complete to fill. The native <audio> seek bar is even less
// meaningful for a live stream (no fixed length, and its time readout is
// the whole listening session, not the song) — dashboard.css hides that
// bar so this counter is the one position indicator.
const NOWPLAYING_POLL_MS = 30000;
let nowPlayingTimer = null;
let elapsedTimer = null;
let lastNowPlayingKey = null;
let songStartedAt = null; // unix seconds the current track went on air, or null

function startNowPlayingPoll() {
  stopNowPlayingPoll();
  lastNowPlayingKey = null;
  songStartedAt = null;
  refreshNowPlaying();
  nowPlayingTimer = setInterval(refreshNowPlaying, NOWPLAYING_POLL_MS);
  // A separate 1s tick advances the elapsed counter between the 30s polls.
  elapsedTimer = setInterval(tickElapsed, 1000);
}

function stopNowPlayingPoll() {
  if (nowPlayingTimer) { clearInterval(nowPlayingTimer); nowPlayingTimer = null; }
  if (elapsedTimer) { clearInterval(elapsedTimer); elapsedTimer = null; }
  lastNowPlayingKey = null;
  songStartedAt = null;
}

async function refreshNowPlaying() {
  const el = document.getElementById('vegas-song');
  if (!el) return; // player torn down between polls
  try {
    const r = await fetch('/api/slop/nowplaying', { credentials: 'same-origin' });
    if (!r.ok) return; // transient — keep the last song shown
    renderNowPlaying(el, await r.json());
  } catch { /* offline / blip — keep the last song shown */ }
}

function renderNowPlaying(el, data) {
  const title = ((data && data.title) || '').trim();
  const artist = ((data && data.artist) || '').trim();
  if (!title && !artist) {
    // Empty feed — hide the line rather than show a stale track.
    el.replaceChildren();
    lastNowPlayingKey = null;
    songStartedAt = null;
    return;
  }
  const key = artist + '' + title;
  if (key === lastNowPlayingKey) return; // unchanged — no DOM churn
  lastNowPlayingKey = key;
  const started = Number(data && data.started_at) || 0;
  songStartedAt = started > 0 ? started : null;

  el.replaceChildren();

  // Artist line (♪ Artist), above the title.
  if (artist) {
    const artistLine = document.createElement('div');
    artistLine.className = 'vegas-artist';
    artistLine.textContent = '♪ ' + artist;
    el.appendChild(artistLine);
  }

  // Title line — the focal point. With a prebuilt search URL it links to a
  // YouTube search for the track; otherwise it's plain text.
  const url = (data && data.search_url) || '';
  if (title && url) {
    const a = document.createElement('a');
    a.className = 'vegas-title';
    a.href = url;
    a.target = '_blank';
    a.rel = 'noopener';
    a.textContent = title;
    if (data && data.album) a.title = 'Album: ' + data.album + ' — search YouTube ↗';
    else a.title = 'Search YouTube for this track ↗';
    el.appendChild(a);
  } else if (title) {
    const t = document.createElement('div');
    t.className = 'vegas-title';
    t.textContent = title;
    el.appendChild(t);
  }

  // Time-on-air line ("· 1:23 on air") — only when we know the start time.
  if (songStartedAt != null) {
    const elapsedLine = document.createElement('div');
    elapsedLine.className = 'vegas-elapsed-line';
    elapsedLine.append('· ');
    const elapsed = document.createElement('span');
    elapsed.className = 'vegas-elapsed';
    elapsed.id = 'vegas-elapsed';
    elapsed.title = 'Time on air (track started ' +
      new Date(songStartedAt * 1000).toLocaleTimeString() + ')';
    elapsedLine.appendChild(elapsed);
    elapsedLine.append(' on air');
    el.appendChild(elapsedLine);
    tickElapsed(); // paint immediately, don't wait a second
  }
}

// tickElapsed advances the elapsed counter from songStartedAt. Clamped at
// 0 so listener buffering / clock skew can't show a negative time.
function tickElapsed() {
  const el = document.getElementById('vegas-elapsed');
  if (!el || songStartedAt == null) return;
  let sec = Math.floor(Date.now() / 1000) - songStartedAt;
  if (sec < 0) sec = 0;
  el.textContent = formatElapsed(sec);
}

// formatElapsed renders seconds as m:ss (or h:mm:ss past an hour).
function formatElapsed(sec) {
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const ss = String(sec % 60).padStart(2, '0');
  if (h) return h + ':' + String(m).padStart(2, '0') + ':' + ss;
  return m + ':' + ss;
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
  stopNowPlayingPoll();
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
      // stack, so autoplay-with-sound is granted. Only start when the
      // master sound switch is on — a muted user entering slop shouldn't
      // get the radio.
      if (isSlopSoundEnabled()) startMusic();
    } else {
      stopMusic();
      leaveVegasTabIfActive();
    }
  });
  // The master sound switch (slop-audio.js's header button) toggled. Mute
  // → stop the stream; unmute → start it if we're in slop mode. This is
  // what makes the one header button govern the music as well as the FX.
  document.addEventListener('tclaude:slopsound', (e) => {
    const on = !!(e.detail && e.detail.enabled);
    if (on) {
      if (isSlopActive()) startMusic();
    } else {
      stopMusic();
    }
  });
  // Page loaded already in slop mode (e.g. `--slop` → ?slop=1). The
  // initial tclaude:slop fired from applySlopThemeIfRequested() before
  // this binder existed, so kick the player off here — unless sound is
  // muted. startMusic handles the gestureless autoplay block by starting
  // muted and unmuting on the first interaction.
  if (isSlopActive() && isSlopSoundEnabled()) startMusic();
}
