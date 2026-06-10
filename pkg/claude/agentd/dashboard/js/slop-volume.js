// slop-volume.js — persistent volume sliders for slop ("vegas") mode.
//
// The header's 🔇/🔊 button (slop-audio.js) is the master ON/OFF for all
// slop sound; this module adds the mixer next to it: a 🎚️ button that
// opens a small popover with two sliders — 🎵 music (the Vegas lounge
// radio, vegas.js) and 🔔 FX (the synthesized casino effects,
// slop-audio.js). It lives in the header HUD rather than the Vegas tab
// or the Config tab because volume is something you reach for the
// moment the casino gets loud, from whatever tab you're on — same
// reason the mute button lives there.
//
// The values persist server-side in ~/.tclaude/config.json (the "slop"
// block) via the dashboard's /api/slop/volumes endpoint — GET on first
// slop activation, debounced POST on slider moves. config.json rather
// than localStorage so the setting survives browser profiles and is
// visible/editable like any other tclaude config. The master mute stays
// in localStorage (slop-audio.js) — on/off is a per-browser whim,
// volume is calibration.
//
// The native <audio controls> volume in the Vegas tab sets the same
// value: vegas.js emits `tclaude:slopmusicvol` when the user drags it,
// and we mirror that onto the slider and persist it too.

import { isSlopActive } from './slop.js';
import { setEffectsVolume } from './slop-audio.js';
import { setMusicVolume } from './vegas.js';
import { toast } from './refresh.js';

const PERSIST_DEBOUNCE_MS = 500;

let music = 100;   // percent, 0–100
let effects = 100; // percent, 0–100
let lastPersisted = null; // "music/effects" key of the last server write
let persistTimer = null;
let loaded = false; // GET /api/slop/volumes done (or in flight)

// apply pushes the current values into the two audio owners and the
// slider widgets (which may have been the source — setting .value to
// the same value is a no-op and fires no event).
function apply() {
  setMusicVolume(music);
  setEffectsVolume(effects);
  const m = document.getElementById('slop-vol-music');
  const f = document.getElementById('slop-vol-fx');
  if (m) { m.value = music; m.title = 'Music volume: ' + music + '%'; }
  if (f) { f.value = effects; f.title = 'FX volume: ' + effects + '%'; }
  const mv = document.getElementById('slop-vol-music-val');
  const fv = document.getElementById('slop-vol-fx-val');
  if (mv) mv.textContent = music + '%';
  if (fv) fv.textContent = effects + '%';
}

// schedulePersist debounces the server write so dragging a slider is
// one POST, not fifty. Identical values are skipped — the volumechange
// echo of our own writes lands here too.
function schedulePersist() {
  clearTimeout(persistTimer);
  persistTimer = setTimeout(async () => {
    const key = music + '/' + effects;
    if (key === lastPersisted) return;
    try {
      const r = await fetch('/api/slop/volumes', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ music_volume: music, effects_volume: effects }),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      lastPersisted = key;
    } catch (e) {
      // The volume still applied locally — only the persistence failed.
      toast('Could not save volume: ' + (e.message || e), true);
    }
  }, PERSIST_DEBOUNCE_MS);
}

// loadVolumes fetches the persisted values once, lazily on the first
// slop activation — the plain dashboard never pays for the request.
async function loadVolumes() {
  if (loaded) return;
  loaded = true;
  try {
    const r = await fetch('/api/slop/volumes', { credentials: 'same-origin' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const d = await r.json();
    if (Number.isFinite(d.music_volume)) music = d.music_volume;
    if (Number.isFinite(d.effects_volume)) effects = d.effects_volume;
    lastPersisted = music + '/' + effects;
    apply();
  } catch {
    // Defaults (100/100) already applied — the mixer still works for
    // this session, it just won't have loaded a saved calibration.
  }
}

function setPopoverOpen(open) {
  const pop = document.getElementById('slop-volume-pop');
  const btn = document.getElementById('slop-volume-btn');
  if (!pop || !btn) return;
  pop.classList.toggle('open', open);
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

export function bindSlopVolume() {
  const btn = document.getElementById('slop-volume-btn');
  const pop = document.getElementById('slop-volume-pop');
  if (!btn || !pop) return;

  btn.addEventListener('click', () => {
    setPopoverOpen(!pop.classList.contains('open'));
  });
  // Close on a click outside the popover/button, or on Escape — the
  // same dismissal manners as the filter-bar view menus.
  document.addEventListener('pointerdown', (e) => {
    if (!pop.classList.contains('open')) return;
    if (pop.contains(e.target) || btn.contains(e.target)) return;
    setPopoverOpen(false);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') setPopoverOpen(false);
  });

  const m = document.getElementById('slop-vol-music');
  const f = document.getElementById('slop-vol-fx');
  if (m) m.addEventListener('input', () => {
    music = Math.min(100, Math.max(0, parseInt(m.value, 10) || 0));
    apply();
    schedulePersist();
  });
  if (f) f.addEventListener('input', () => {
    effects = Math.min(100, Math.max(0, parseInt(f.value, 10) || 0));
    apply();
    schedulePersist();
  });

  // The Vegas tab's native <audio controls> volume drag — mirror it
  // onto the music slider and persist it like our own.
  document.addEventListener('tclaude:slopmusicvol', (e) => {
    const v = e.detail && e.detail.volume;
    if (!Number.isFinite(v)) return;
    music = Math.min(100, Math.max(0, v));
    apply();
    schedulePersist();
  });

  // Load the persisted values when slop turns on (and close the mixer
  // when it turns off — the HUD it anchors to is CSS-hidden then).
  document.addEventListener('tclaude:slop', (e) => {
    if (e.detail && e.detail.active) loadVolumes();
    else setPopoverOpen(false);
  });
  if (isSlopActive()) loadVolumes();

  apply();
}
