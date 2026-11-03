// vegas.js — background music for the Vegas ("slop") soundtrack.
//
// A "Vegas" tab appears after Config holding a lounge-music player. It is
// live whenever the Vegas features are active — that's slop ("casino")
// mode OR the regular-mode opt-in (config slop.vegas_in_regular_mode,
// body.vegas applied from the snapshot by refresh.js). The player is
// created when they turn on and torn down when they turn off, so the
// soundtrack stops the moment the feature leaves.
//
// The audio engine stays imperative (one detached <audio>); the player card
// is a Preact island (VegasPlayer) rendered from the engine's signals.
//
// We stream an internet radio station via an <audio> element rather than
// embedding YouTube: monetised YouTube content serves ads, and many of
// those videos disable embedding outright (the original ?v=… link did —
// it rendered "Video unavailable / Watch on YouTube"). A plain ICE/MP3
// stream has neither problem, plays forever, and pauses cleanly.
//
// We react to that combined state via the `tclaude:vegas` CustomEvent
// slop.js dispatches whenever slop OR the regular-mode opt-in flips,
// reading the live isVegasActive() rather than the event's detail —
// rather than reaching into slop.js's internals. Companion to slop.js
// (the theme toggle) and slop-fx.js (the casino-only visual flair).
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

import { h, render } from 'preact';
import { signal } from '@preact/signals';
import htm from 'htm';
import { isVegasActive } from './slop.js';
import { isSlopSoundEnabled } from './slop-audio.js';
import { dashPrefs } from './prefs.js';
import { dashboardState } from './snapshot-store.js';

const html = htm.bind(h);

// The station catalog. SomaFM is listener-supported and genuinely
// commercial-free; each entry is one of its channels. The first
// (illstreet — "Illinois Street Lounge", the vintage cocktail / Rat-Pack
// channel) is the default and the original Vegas-lounge soundtrack, so a
// fresh dashboard keeps playing what it always did.
//
// The radio is shared by both cosmetic themes: the 🎰 slop/Vegas soundtrack
// and the 🧙 wizard soundtrack. `group` sorts each station into one of the
// two — 'vegas' (lounge/jazz) or 'wizard' (fantasy ambient/celtic) — which
// the two-level picker (a group <select> filtering the channel <select>)
// reads. The wizard entries carry flavor labels ("The Tavern") over the
// real SomaFM channels; `desc` names the actual station so it's never a
// mystery what's playing.
//
// Only {id, label, desc, group} is stored here: every URL (the ICE/MP3
// stream, the station home, the now-playing feed) derives from the id by
// SomaFM's fixed URL shape (streamFor/homeFor below + the server's
// somaSongsURL), so adding a channel is a one-line entry. The id set MUST
// match the server's allowlist (config.SlopChannels) —
// TestSlopNowPlaying_ChannelMatchesVegasJS pins them so a channel added on
// one side but not the other fails CI. (The pin test scrapes `id: '…'`, so
// keep the group catalog below on a different key — `key:` — to stay out of
// its way.)
const CHANNELS = [
  { id: 'illstreet',    label: 'Illinois Street Lounge', desc: 'Vintage cocktail & exotica',      group: 'vegas'  },
  { id: 'secretagent',  label: 'Secret Agent',           desc: 'Spy-jazz & surf for your espionage', group: 'vegas' },
  { id: 'groovesalad',  label: 'Groove Salad',           desc: 'Ambient & downtempo chill',       group: 'vegas'  },
  { id: 'lush',         label: 'Lush',                   desc: 'Mostly vocal, mostly chilled',    group: 'vegas'  },
  { id: 'bootliquor',   label: 'Boot Liquor',            desc: 'Americana roots for cowboys',     group: 'vegas'  },
  { id: 'u80s',         label: 'Underground 80s',        desc: 'Early alternative & new wave',    group: 'vegas'  },
  { id: 'defcon',       label: 'DEF CON Radio',          desc: 'Music for hacking',               group: 'vegas'  },
  { id: 'thistle',      label: '🍺 The Tavern',          desc: 'ThistleRadio — Celtic roots',     group: 'wizard' },
  { id: 'folkfwd',      label: "🪕 The Bard's Rest",     desc: 'Folk Forward — indie & alt-folk', group: 'wizard' },
  { id: 'dronezone',    label: '🔮 The Astral Plane',    desc: 'Drone Zone — atmospheric ambient', group: 'wizard' },
  { id: 'darkzone',     label: '🕯️ The Dungeon',         desc: 'The Dark Zone — dark ambient',     group: 'wizard' },
  { id: 'doomed',       label: '💀 The Crypt',           desc: 'Doomed — dark industrial ambient', group: 'wizard' },
  { id: 'deepspaceone', label: '✨ The Cosmos',          desc: 'Deep Space One — deep-space ambient', group: 'wizard' },
];

const DEFAULT_CHANNEL = 'illstreet';        // Vegas group default (config.DefaultSlopChannel)
const WIZARD_DEFAULT_CHANNEL = 'thistle';   // Wizard group default (config.DefaultWizardChannel)
const PLAY_INTENT_KEY = 'tclaude.dash.music.playIntent';

// The channel groups the top-level picker offers. `key` matches each
// channel's `group`; deliberately NOT keyed `id:` so the pin test's channel
// scrape (id: '…') doesn't pick these up. label is what the group <select>
// shows.
const GROUPS = [
  { key: 'vegas',  label: '🎰 Vegas Lounge' },
  { key: 'wizard', label: "🧙 Wizard's Realm" },
];

// URL builders for a SomaFM channel id (the fixed shape SomaFM uses).
const streamFor = (id) => 'https://ice1.somafm.com/' + id + '-128-mp3';
const homeFor   = (id) => 'https://somafm.com/' + id + '/';

// channelById resolves an id to its catalog entry, falling back to the
// first (default) channel for an unknown id so callers always get a valid
// {id,label,desc,group}.
function channelById(id) {
  return CHANNELS.find((c) => c.id === id) || CHANNELS[0];
}

// groupDefaultChannel returns the id of the first catalog channel in a
// group — the station picking that group jumps to. Falls back to the global
// default if a group somehow has no channels.
function groupDefaultChannel(group) {
  const c = CHANNELS.find((ch) => ch.group === group);
  return c ? c.id : DEFAULT_CHANNEL;
}

// themeDefaultChannel is the station a FRESH listener (no persisted choice)
// hears, chosen by the active cosmetic theme: the wizard soundtrack opens on
// the Tavern, everything else on the Vegas lounge. An explicit saved channel
// (loadChannel) always overrides this — the radio remembers your station.
function themeDefaultChannel() {
  return document.body.classList.contains('wizard')
    ? WIZARD_DEFAULT_CHANNEL : DEFAULT_CHANNEL;
}

// activeChannelId is the channel the live player is (or will be) built on.
// It starts at the default and is corrected to the persisted choice by
// loadChannel() on the first slop activation; the picker updates it on a
// user change. startMusic and the now-playing poll both read it, so a
// channel switch is "set this + rebuild".
let activeChannelId = DEFAULT_CHANNEL;
let channelLoaded = false;        // GET /api/slop/channel done (or in flight)
let userPicked = false;           // a user pick supersedes a still-in-flight load
let hasExplicitChannel = false;   // a real saved choice exists (server-persisted or picked)

// loadChannel fetches the persisted channel once, lazily on the first radio
// activation — like slop-volume.js's loadVolumes, the plain dashboard never
// pays for it. If a channel was EXPLICITLY saved and differs from what's
// playing, it switches the live player to it (best-effort: a failed GET just
// leaves the theme default playing). A fresh listener with no saved choice
// keeps the theme default (Tavern in wizard mode, lounge otherwise) that
// syncVegas already applied — so the server's `persisted` flag, not the bare
// resolved id (which is the global default when nothing is saved), is what
// gates the override. The catalog the server returns is ignored here — the
// client renders from CHANNELS above; the pin test keeps the two in step.
async function loadChannel() {
  if (channelLoaded) return;
  channelLoaded = true;
  try {
    const r = await fetch('/api/slop/channel', { credentials: 'same-origin' });
    if (!r.ok) return;
    const d = await r.json();
    // The user picked a channel while this GET was in flight — their choice
    // (already persisted + playing) wins; applying the stale saved value
    // here would switch the player back under them. Mirrors slop-volume.js's
    // `touched` guard against a late volumes GET.
    if (userPicked) return;
    const id = (d && d.channel) || '';
    if (d && d.persisted && id && CHANNELS.some((c) => c.id === id)) {
      hasExplicitChannel = true;
      applyChannel(id);
    }
    // else: no explicit choice — leave the theme default in place so a fresh
    // wizard listener stays on the Tavern instead of snapping to the lounge.
  } catch {
    // Offline / blip — the theme default channel is already in place.
  }
}

// applyChannel points the player at a channel WITHOUT persisting (used by
// loadChannel for the saved value). Rebuilds a live player so the switch is
// audible; a no-op when the channel is unchanged or no player exists.
//
// A channel switch changes the station, not whether the operator wants sound,
// so the rebuild PRESERVES the current transport state. Re-tuning a paused
// player (a saved playIntent=pause, or a manual pause) must stay paused —
// otherwise the theme-default re-tune (syncVegas, on a slop→wizard flip) or a
// late loadChannel would call startMusic() with its default autoplay=true and
// start the very stream the operator explicitly stopped.
function applyChannel(id) {
  if (id === activeChannelId) return;
  activeChannelId = id;
  if (audio) {
    const wasPlaying = !audio.paused;
    stopMusic();
    startMusic({ autoplay: wasPlaying });
  }
}

// switchChannel is the user-driven change from the picker: persist the
// choice to the backend (config.json's slop block) then apply it. Runs
// inside the <select> change gesture, so the rebuilt player's
// autoplay-with-sound is granted.
function switchChannel(id) {
  if (!CHANNELS.some((c) => c.id === id)) return;
  userPicked = true;          // beat a still-in-flight loadChannel (see its guard)
  hasExplicitChannel = true;  // a deliberate pick — no longer a theme default
  persistChannel(id);
  applyChannel(id);
}

// switchGroup is the user-driven change from the top-level group picker: jump
// to that group's default station (its first catalog channel). Treated as an
// explicit pick — choosing "Wizard's Realm" is a deliberate act — so it
// persists and survives a theme flip like any other channel choice.
function switchGroup(group) {
  const id = groupDefaultChannel(group);
  // If the active channel is already in the chosen group, keep it — jumping
  // to the group's first station would be a surprising demotion.
  if (channelById(activeChannelId).group === group) return;
  switchChannel(id);
}

// persistChannel POSTs the chosen channel to the backend. Best-effort: the
// channel still plays locally if the save fails (mirrors slop-volume.js's
// fire-and-forget persistence).
function persistChannel(id) {
  fetch('/api/slop/channel', {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ channel: id }),
  }).catch(() => { /* the channel already applied locally */ });
}

// musicVolume is the persisted radio volume in percent (0–100), owned
// by slop-volume.js (loaded from /api/slop/volumes). Applied to the
// <audio> element when it is built and on every setter call. The initial
// value matches the server default (config.DefaultMusicVolume, 50%) so the
// radio never plays at full blast if it is built before slop-volume.js's
// first apply() lands — the startle bug this default guards against.
let musicVolume = 50;

// setMusicVolume applies the persisted music volume (percent) to a live
// player and remembers it for the next startMusic. Called by
// slop-volume.js on load and on every slider move.
export function setMusicVolume(pct) {
  musicVolume = Math.min(100, Math.max(0, Number(pct) || 0));
  if (audio && Math.round(audio.volume * 100) !== musicVolume) {
    audio.volume = musicVolume / 100;
  }
}

// cancelUnmuteArm tears down a pending "unmute on first gesture" arming
// (see playWithSound). Held at module scope so stopMusic / a rebuild can
// cancel a still-waiting arm instead of leaking listeners that reference
// a stopped <audio>.
let cancelUnmuteArm = null;

// connectionLost is raised by connection.js when the dashboard loses contact
// with agentd (the 2s poll can't reach it). While it's set we force the radio
// off and refuse to (re)start it — a disconnected dashboard shouldn't keep
// streaming music as if nothing were wrong. connection.js clears it on
// reconnect, which re-runs syncVegas() to restore the radio to the live state.
let connectionLost = false;

// setConnectionLost is called by connection.js on the disconnect/reconnect
// edges. Raising it stops the music now (and syncVegas() below keeps it
// stopped); clearing it reconciles the radio back to whatever the current
// theme + master sound switch want.
export function setConnectionLost(lost) {
  lost = !!lost;
  if (lost === connectionLost) return;
  connectionLost = lost;
  if (lost) stopMusic();
  else syncVegas();
}

function shouldAutoPlayMusic() {
  return dashPrefs.getItem(PLAY_INTENT_KEY) !== 'pause';
}

function rememberPlayIntent(intent) {
  if (intent === 'pause') dashPrefs.setItem(PLAY_INTENT_KEY, 'pause');
  else dashPrefs.setItem(PLAY_INTENT_KEY, 'play');
}

// ─── The audio engine ──────────────────────────────────────────────────
// The engine is imperative: one controls-less <audio> element, never in the
// DOM (a detached media element plays the same), built by startMusic and
// torn down by stopMusic. Its state reaches the HUD only through the signals
// below, which VegasPlayer renders into #vegas-player.
let audio = null;
let playerMounted = false;
// hud is null with no player, else the station it is tuned to and whether
// the stream failed (the HUD then shows the error card instead).
const hud = signal(null);
// audible is "playing with sound" (not paused, not muted). The armed-muted
// autoplay of a gestureless ?slop=1 load reads as not-yet-audible, so the
// button shows ▶ to invite the click that brings the sound up.
const audible = signal(false);

function startMusic({ autoplay = true } = {}) {
  if (!playerMounted) return;
  // Already playing — don't rebuild, which would restart the stream on a
  // redundant event (e.g. a duplicate tclaude:vegas).
  if (audio) return;
  // Drop any stale arm from a previous stop.
  if (cancelUnmuteArm) cancelUnmuteArm();
  const chan = channelById(activeChannelId);
  audio = document.createElement('audio');
  const el = audio;
  audio.src = streamFor(chan.id);
  audio.autoplay = autoplay;
  audio.preload = 'auto';
  audio.setAttribute('aria-label', 'SomaFM — ' + chan.label);
  audio.volume = musicVolume / 100;
  // A dead/unreachable stream should explain itself rather than sit as a
  // silent broken control.
  audio.addEventListener('error', () => { if (audio === el) showStreamError(); }, { once: true });
  // Mute flips (the arm muting on a gestureless load, then unmuting on the
  // first interaction) fire volumechange but not play/pause — keep the icon
  // honest about whether sound is actually coming out.
  const sync = () => { if (audio === el) audible.value = !el.paused && !el.muted; };
  for (const type of ['play', 'pause', 'playing', 'volumechange']) audio.addEventListener(type, sync);
  sync();
  hud.value = { channelId: chan.id, error: false };
  if (autoplay) playWithSound(audio);
  startNowPlayingPoll();
}

// togglePlay is the HUD's play/pause button. It decides from the button's
// shown intent, not the post-event audio state: the document-level unmute
// arm (armMutedUntilGesture) fires on this same gesture and would otherwise
// flip a "play" into a pause.
function togglePlay(event) {
  if (!audio) return;
  if (event.currentTarget.getAttribute('aria-label') === 'Play') {
    rememberPlayIntent('play');
    audio.muted = false;
    audio.play().catch(() => {});
  } else {
    rememberPlayIntent('pause');
    audio.pause();
  }
}

// ─── Now-playing poller ────────────────────────────────────────────────
// Polls /api/slop/nowplaying (agentd's SomaFM proxy) while music plays and
// publishes the song readout — an artist line, the title (a link to a
// YouTube search for the track), and a time-on-air line. Independent of
// playback state, so the song still shows while the stream is armed-muted
// before the first gesture.
//
// The "· 1:23" after the title is REAL time-on-air, counted up from the
// track's actual start (the feed's start timestamp) by a 1s ticker. It is
// NOT a progress bar: SomaFM exposes no song duration, so there's no honest
// percent-complete to fill, and a live stream has no meaningful seek bar.
const NOWPLAYING_POLL_MS = 30000;
let nowPlayingTimer = null;
let elapsedTimer = null;
// song is the on-air track ({ artist, title, url, album, startedAt }) or
// null; nowSec is the clock the elapsed counter reads, advanced each second.
const song = signal(null);
const nowSec = signal(Math.floor(Date.now() / 1000));

function startNowPlayingPoll() {
  stopNowPlayingPoll();
  refreshNowPlaying();
  nowPlayingTimer = setInterval(refreshNowPlaying, NOWPLAYING_POLL_MS);
  // A separate 1s tick advances the elapsed counter between the 30s polls.
  elapsedTimer = setInterval(tickElapsed, 1000);
}

function stopNowPlayingPoll() {
  if (nowPlayingTimer) { clearInterval(nowPlayingTimer); nowPlayingTimer = null; }
  if (elapsedTimer) { clearInterval(elapsedTimer); elapsedTimer = null; }
  song.value = null;
}

async function refreshNowPlaying() {
  const polled = audio;
  if (!polled) return; // player torn down between polls
  try {
    // Tag the poll with the active channel so the proxy reads the matching
    // feed (the URL is validated server-side against the allowlist).
    const r = await fetch('/api/slop/nowplaying?channel=' + encodeURIComponent(activeChannelId),
      { credentials: 'same-origin' });
    if (!r.ok) return; // transient — keep the last song shown
    const data = await r.json();
    if (audio === polled) song.value = songFrom(data);
  } catch { /* offline / blip — keep the last song shown */ }
}

// songFrom reads the proxy's answer; an empty feed hides the line rather
// than show a stale track.
function songFrom(data) {
  const title = ((data && data.title) || '').trim();
  const artist = ((data && data.artist) || '').trim();
  if (!title && !artist) return null;
  const started = Number(data && data.started_at) || 0;
  tickElapsed(); // paint the counter now, don't wait a second
  return { artist, title, url: (data && data.search_url) || '', album: (data && data.album) || '', startedAt: started > 0 ? started : null };
}

function tickElapsed() {
  nowSec.value = Math.floor(Date.now() / 1000);
}

// formatElapsed renders seconds as m:ss (or h:mm:ss past an hour). Clamped
// at 0 so listener buffering / clock skew can't show a negative time.
function formatElapsed(sec) {
  sec = Math.max(0, sec);
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
function playWithSound(el) {
  el.muted = false;
  const p = el.play();
  if (p && typeof p.catch === 'function') {
    // A rejection can also come from releasing the element mid-buffer (a
    // fast channel switch aborts its play()); only the live element arms.
    p.catch(() => { if (audio === el) armMutedUntilGesture(el); });
  }
}

function armMutedUntilGesture(el) {
  if (cancelUnmuteArm) cancelUnmuteArm();
  el.muted = true;
  // Muted autoplay is allowed without a gesture; this connects the
  // stream so unmuting later is instant.
  el.play().catch(() => {});
  const unmute = () => {
    cancel();
    el.muted = false;
    // Re-issue play() inside the gesture in case the muted attempt was
    // itself suspended.
    el.play().catch(() => {});
  };
  // Each arm cancels only itself, so a stale arm can never take down the
  // live element's.
  const cancel = () => {
    document.removeEventListener('pointerdown', unmute, true);
    document.removeEventListener('keydown', unmute, true);
    if (cancelUnmuteArm === cancel) cancelUnmuteArm = null;
  };
  cancelUnmuteArm = cancel;
  document.addEventListener('pointerdown', unmute, true);
  document.addEventListener('keydown', unmute, true);
}

// releaseAudio stops the engine element for good. Pause + drop the src +
// load() so the ongoing stream fetch is actually aborted, not just
// detached — otherwise the connection (and audio) can linger. This is the
// whole point of the module: music must stop when you leave the mode.
function releaseAudio() {
  stopNowPlayingPoll();
  if (cancelUnmuteArm) cancelUnmuteArm();
  if (!audio) return;
  const el = audio;
  audio = null;
  el.pause();
  el.removeAttribute('src');
  el.load();
  audible.value = false;
}

function stopMusic() {
  releaseAudio();
  hud.value = null;
}

// showStreamError swaps the HUD for the error card. The engine is released,
// so the next sync (a theme or sound toggle) rebuilds a fresh player.
function showStreamError() {
  const channelId = activeChannelId;
  releaseAudio();
  hud.value = { channelId, error: true };
}

// ─── The HUD ───────────────────────────────────────────────────────────
// VegasPlayer renders the player card from the engine's signals: the
// now-playing "stereo display" (song readout above the station line), the
// station line's two-level picker (a group <select> filtering a channel
// <select>), and the play/pause transport. Volume lives in the header mixer
// (slop-volume.js's 🎚️ popover). The pickers and the round transport button
// are the casino card's own themed chrome (dashboard.css .vegas-*), so they
// stay native controls under data-ui-exempt.
function SongReadout({ track }) {
  const { artist, title, url, album, startedAt } = track;
  return html`
    ${artist && html`<div class="vegas-artist">♪ ${artist}</div>`}
    ${title && (url
      ? html`<a class="vegas-title" href=${url} target="_blank" rel="noopener"
          title=${album ? 'Album: ' + album + ' — search YouTube ↗' : 'Search YouTube for this track ↗'}>${title}</a>`
      : html`<div class="vegas-title">${title}</div>`)}
    ${startedAt != null && html`<div class="vegas-elapsed-line">· <span class="vegas-elapsed" id="vegas-elapsed"
        title=${'Time on air (track started ' + new Date(startedAt * 1000).toLocaleTimeString() + ')'}>${formatElapsed(nowSec.value - startedAt)}</span> on air</div>`}`;
}

export function VegasPlayer() {
  const state = hud.value;
  if (!state) return null;
  if (state.error) {
    return html`<div class="vegas-error">🎲 Couldn’t reach the stream — <a href=${homeFor(state.channelId)} target="_blank" rel="noopener">open it on SomaFM ↗</a></div>`;
  }
  const chan = channelById(state.channelId);
  const track = song.value;
  const on = audible.value;
  return html`
    <div class="vegas-nowplaying">
      <div class="vegas-song" id="vegas-song">${track && html`<${SongReadout} track=${track} />`}</div>
      <div class="vegas-station">📻 <select data-ui-exempt class="vegas-group" id="vegas-group" aria-label="Radio group"
          value=${chan.group} onChange=${(e) => switchGroup(e.currentTarget.value)}>
          ${GROUPS.map((g) => html`<option key=${g.key} value=${g.key}>${g.label}</option>`)}
        </select> <select data-ui-exempt class="vegas-channel" id="vegas-channel" aria-label="Radio channel"
          value=${chan.id} title=${chan.desc || ('SomaFM — ' + chan.label)} onChange=${(e) => switchChannel(e.currentTarget.value)}>
          ${CHANNELS.filter((c) => c.group === chan.group).map((c) => html`<option key=${c.id} value=${c.id} title=${c.desc || null}>${c.label}</option>`)}
        </select></div>
    </div>
    <div class="vegas-transport">
      <button data-ui-exempt type="button" class="vegas-play" aria-label=${on ? 'Pause' : 'Play'} onClick=${togglePlay}>${on ? '⏸' : '▶'}</button>
    </div>`;
}

// When slop turns off while the Vegas tab is the active one, that tab's
// nav button has just been CSS-hidden, leaving a stranded visible
// section. Fall back to Groups so the user isn't staring at a dead
// player. Mirrors bindTabs()'s active-class toggling in refresh.js (kept
// local to avoid a circular import).
function leaveVegasTabIfActive() {
  const sec = document.getElementById('tab-vegas');
  if (!sec || !sec.classList.contains('active')) return;
  document.querySelectorAll('nav [data-tab]').forEach(b =>
    b.classList.toggle('active', b.dataset.tab === 'groups'));
  document.querySelectorAll('main section').forEach(s =>
    s.classList.toggle('active', s.id === 'tab-groups'));
  dashboardState.setActiveTab('groups');
}

// syncVegas brings the radio in line with the live state. It plays when
// the Vegas features are active (slop OR the regular-mode opt-in) AND the
// master sound switch is on, unless the operator's last explicit
// player-button intent was pause. In that saved-paused case it still builds
// the player chrome so the Play button is available, but it does not start
// the stream. Idempotent — startMusic no-ops if a player already exists,
// stopMusic if none does — so it's safe to call on every tclaude:vegas /
// tclaude:slopsound event and on boot.
//
// When reached from a toggle click (or the master-switch click) we're
// inside that gesture's call stack, so autoplay-with-sound is granted; on
// a gestureless activation (the snapshot turning body.vegas on, or a
// ?slop=1 load) startMusic falls back to muted playback and unmutes on the
// first interaction. leaveVegasTabIfActive only fires when the features go
// fully inactive, so merely muting the sound never kicks you off the tab.
function syncVegas() {
  // Disconnected from agentd — keep the radio off no matter what any theme /
  // sound event asks for. connection.js re-runs us via setConnectionLost(false)
  // when the link is back, at which point the checks below resume normally.
  if (connectionLost) { stopMusic(); return; }
  if (isVegasActive()) {
    // For a fresh listener (no explicit saved/picked channel), keep the
    // station in step with the active theme — the wizard soundtrack opens on
    // the Tavern, the lounge otherwise. Done before startMusic so the first
    // build tunes to the right station (no lounge→tavern restart blip), and
    // re-checked on every sync so a mid-session casino↔wizard flip re-tunes
    // the auto default. An explicit pick (hasExplicitChannel) is untouched —
    // the radio remembers your station across themes. applyChannel no-ops
    // when the station is unchanged.
    if (!hasExplicitChannel && !userPicked) applyChannel(themeDefaultChannel());
    // Load the persisted channel once (lazily, on first activation) so a
    // later unmute starts on the saved station.
    loadChannel();
    if (isSlopSoundEnabled()) startMusic({ autoplay: shouldAutoPlayMusic() });
    else stopMusic(); // master mute → no stream, but keep the tab/HUD
  } else {
    stopMusic();
    leaveVegasTabIfActive();
  }
}

export function bindVegasMusic() {
  // The HUD is a Preact island on the tab's #vegas-player host; it renders
  // nothing until the engine builds a player.
  const host = document.getElementById('vegas-player');
  if (host && !playerMounted) {
    render(html`<${VegasPlayer} />`, host);
    playerMounted = true;
  }
  // The Vegas features' availability changed — slop toggled, or the
  // regular-mode opt-in flipped (refresh.js → setVegasRegularMode).
  document.addEventListener('tclaude:vegas', syncVegas);
  // The master sound switch (slop-audio.js's header button) toggled. This
  // is what makes the one header button govern the music as well as the FX.
  document.addEventListener('tclaude:slopsound', syncVegas);
  // Page may have loaded already active (e.g. `--slop` → ?slop=1; the
  // regular-mode opt-in arrives with the first snapshot, after this binds,
  // via its own tclaude:vegas). The initial tclaude:vegas from
  // applySlopThemeIfRequested() fired before this binder existed, so sync
  // the initial state here.
  syncVegas();
}
// dashboard-imperative-boundary: media-effects
