// slop-spectacle.js — the loud end of slop mode.
//
// Three pieces of pure showmanship, all gated on body.slop and
// prefers-reduced-motion:
//
//   1. Konami mega-jackpot — type ↑↑↓↓←→←→ B A and the whole page
//      erupts: a "MEGA JACKPOT" banner, a screen shake, a confetti
//      storm, and a 'win-mega' on the bus (so slop-audio plays the big
//      fanfare and slop-credits pays out 777).
//   2. Side pull-lever — a casino lever pinned to the right edge. Yank
//      it (click) to spin every live slot machine on the page at once
//      via slop-fx's pullAllMachines(); one machine may land 7-7-7.
//   3. Confetti on big wins — a colourful burst layered over the coin
//      shower whenever a 'win-pull' or 'win-mega' crosses the bus.
//
// Companion to slop-fx.js: we reuse its showJackpotBanner() and
// pullAllMachines() rather than re-implementing them, and route the
// mega through its emitSlopFx() so audio + credits react through the one
// bus. No new emit kinds are invented here.

import { isSlopActive } from './slop.js';
import { showJackpotBanner, pullAllMachines, emitSlopFx } from './slop-fx.js';

// reducedMotion mirrors slop-fx.js's guard (kept local to avoid a
// cross-module import of a private helper). Read at call time — the OS
// preference can flip mid-session.
function reducedMotion() {
  return window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

// ─── Confetti ──────────────────────────────────────────────────────
// Short-lived coloured pieces raining from the top edge. Same self-
// cleaning lifecycle as slop-fx's coins: spawn, animate once, remove on
// animationend. Colours are a festive casino-ish spread.
const CONFETTI_COLORS = ['#ffd700', '#ff4d6d', '#4dd2ff', '#7CFC00', '#ff9f1c', '#c77dff'];

function confetti(count) {
  if (!isSlopActive() || reducedMotion()) return;
  const vw = window.innerWidth || document.documentElement.clientWidth;
  const vh = window.innerHeight || document.documentElement.clientHeight;
  for (let i = 0; i < count; i++) {
    const el = document.createElement('span');
    el.className = 'slop-confetti';
    el.style.left = (Math.random() * vw) + 'px';
    el.style.top = '-16px';
    el.style.background = CONFETTI_COLORS[Math.floor(Math.random() * CONFETTI_COLORS.length)];
    // Fall most of the viewport height with a little horizontal drift
    // and a tumble; stagger the start so it rains rather than drops as
    // one sheet.
    el.style.setProperty('--dx', ((Math.random() - 0.5) * 180) + 'px');
    el.style.setProperty('--dy', (vh * (0.7 + Math.random() * 0.4)) + 'px');
    el.style.setProperty('--rot', (Math.random() * 1080 - 540) + 'deg');
    el.style.animationDelay = (Math.random() * 0.4) + 's';
    el.style.animationDuration = (1.4 + Math.random() * 1.0) + 's';
    document.body.appendChild(el);
    el.addEventListener('animationend', () => el.remove(), { once: true });
  }
}

// ─── Konami mega-jackpot ───────────────────────────────────────────
const KONAMI = [
  'ArrowUp', 'ArrowUp', 'ArrowDown', 'ArrowDown',
  'ArrowLeft', 'ArrowRight', 'ArrowLeft', 'ArrowRight', 'b', 'a',
];
let konamiProgress = 0;

function megaJackpot() {
  if (!isSlopActive() || reducedMotion()) return;
  showJackpotBanner('💥 MEGA JACKPOT 💥');
  confetti(80);
  // Screen shake — a class on <body> drives the keyframes; remove it
  // after the animation so a later mega can re-trigger it.
  document.body.classList.add('slop-shake');
  setTimeout(() => document.body.classList.remove('slop-shake'), 800);
  // Route through the bus so audio (big fanfare) and credits (+777)
  // react; the confetti above is our own since it's spectacle-local.
  emitSlopFx('win-mega');
}

export function bindKonami() {
  document.addEventListener('keydown', (e) => {
    if (!isSlopActive()) return;
    // Match case-insensitively on the B/A keys; arrows compare as-is.
    const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
    konamiProgress = key === KONAMI[konamiProgress] ? konamiProgress + 1 : (key === KONAMI[0] ? 1 : 0);
    if (konamiProgress === KONAMI.length) {
      konamiProgress = 0;
      megaJackpot();
    }
  });
}

// ─── Side pull-lever ───────────────────────────────────────────────
// A casino lever fixed to the right edge (slop-only via CSS). Clicking
// it animates a yank and spins every live machine. The element lives in
// dashboard.html so it's part of the static page; we just wire the
// click. Pulls are debounced for the yank's duration so a mashing user
// doesn't stack animations.
const LEVER_YANK_MS = 600;
let leverBusy = false;

export function bindSlopLever() {
  const lever = document.getElementById('slop-lever');
  if (!lever) return;
  lever.addEventListener('click', () => {
    if (!isSlopActive() || reducedMotion()) return;
    if (leverBusy) return;
    leverBusy = true;
    lever.classList.add('slop-lever-pulled');
    setTimeout(() => {
      lever.classList.remove('slop-lever-pulled');
      leverBusy = false;
    }, LEVER_YANK_MS);
    pullAllMachines();
  });
}

// ─── Confetti on big wins ──────────────────────────────────────────
export function bindSlopConfetti() {
  document.addEventListener('tclaude:slopfx', (e) => {
    if (!isSlopActive()) return;
    const fx = e.detail && e.detail.fx;
    // win-mega already rains its own (bigger) confetti from megaJackpot;
    // here we add a burst for the hand-pulled 7-7-7 so a manual win
    // feels as good as the spawn banner.
    if (fx === 'win-pull') confetti(40);
  });
}

// bindSlopSpectacle installs all three. One entry point keeps dashboard.js
// tidy.
export function bindSlopSpectacle() {
  bindKonami();
  bindSlopLever();
  bindSlopConfetti();
}
