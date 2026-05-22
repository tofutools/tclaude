// Slop-mode visual feedback: a Vegas-themed coin spray on every
// button click and a louder "JACKPOT" celebration when a new agent
// spawns. Purely cosmetic and gated on body.slop — when the human
// flips slop off the dashboard goes back to its default quiet self.
//
// Companion to slop.js (which handles the theme toggle + chrome
// re-skin). This module is loaded unconditionally so the click
// listener can attach early; the body-class check inside the
// handler is what actually gates the effect.

import { isSlopActive } from './slop.js';

const COIN_EMOJIS = ['🪙', '💰', '⭐', '🍒', '🔔', '7️⃣'];
const JACKPOT_EMOJIS = ['🪙', '💰', '⭐', '🍒', '🔔', '7️⃣', '🎰', '💎'];

// reducedMotion gates every effect — both the per-click spray and the
// jackpot banner — so a user with `prefers-reduced-motion: reduce` sees
// nothing extra. Read at call time (not module init) because the OS
// preference can change at runtime.
function reducedMotion() {
  return window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

// spawnCoin drops a single emoji at (x,y), then animates it via the
// CSS keyframes `slop-coin-arc` (see dashboard.css). Random arc/spin
// per coin so a burst looks unstructured. animationend self-cleans.
//
// `dy` is signed: negative = the coin arcs upward (click bursts),
// positive = the coin falls down (jackpot rain). `spread` is the
// half-width of the horizontal scatter.
function spawnCoin(x, y, fromEmojis, spread, dy) {
  const el = document.createElement('span');
  el.className = 'slop-coin';
  el.textContent = fromEmojis[Math.floor(Math.random() * fromEmojis.length)];
  el.style.left = x + 'px';
  el.style.top = y + 'px';
  // --dx / --dy feed into the keyframes; randomised so each coin
  // takes its own arc instead of marching in formation. The 0.6
  // random factor keeps the same sign as `dy` so an upward arc
  // doesn't accidentally bias downward (or vice versa).
  el.style.setProperty('--dx', ((Math.random() - 0.5) * 2 * spread) + 'px');
  el.style.setProperty('--dy', (dy + dy * 0.6 * Math.random()) + 'px');
  el.style.setProperty('--rot', (Math.random() * 720 - 360) + 'deg');
  el.style.animationDuration = (0.7 + Math.random() * 0.5) + 's';
  document.body.appendChild(el);
  el.addEventListener('animationend', () => el.remove(), { once: true });
}

// shouldEmitFor decides whether a click should trigger a coin spray.
// We want the spray on actual interactive controls (real <button>s,
// data-act handles, the spawn chip) and NOT on modal backdrops, raw
// text, anchors that just toggle <details>, or the slop icon itself
// (it has its own jiggle and toggles the theme — adding a spray on
// the very click that turns slop off looks broken).
function shouldEmitFor(target) {
  if (!target || target.nodeType !== 1) return false;
  if (target.closest('.slop-icon')) return false;
  // Inert chrome — clicks on modal overlays/backdrops are dismissals,
  // not real button presses. Don't decorate them.
  if (target.classList && target.classList.contains('modal-overlay')) return false;
  return !!target.closest('button, [data-act], .spawn-btn, summary');
}

// bindSlopClickFx attaches a single delegated click listener at the
// document root. Capture phase so a button's own handler calling
// stopPropagation/preventDefault doesn't suppress our spray.
//
// The check happens on every click rather than at bind time so the
// listener can stay registered for the lifetime of the page and
// merely no-op while slop is off.
export function bindSlopClickFx() {
  document.addEventListener('click', (e) => {
    if (!isSlopActive()) return;
    if (reducedMotion()) return;
    if (!shouldEmitFor(e.target)) return;
    // Use the actual pointer location when available; fall back to
    // the target's center for keyboard-triggered clicks (Enter on a
    // focused button fires "click" with clientX/Y == 0).
    let x = e.clientX;
    let y = e.clientY;
    if (!x && !y) {
      const r = e.target.getBoundingClientRect();
      x = r.left + r.width / 2;
      y = r.top + r.height / 2;
    }
    const n = 4 + Math.floor(Math.random() * 3);
    // Negative dy → coins arc upward out of the click point.
    for (let i = 0; i < n; i++) spawnCoin(x, y, COIN_EMOJIS, 80, -70);
  }, true);
}

// slopJackpot is the bigger celebration: a centered "JACKPOT" banner
// plus a wider coin shower from the top of the viewport. Used by the
// spawn modal on a successful POST. Silently no-ops when slop is off
// so callers don't need their own conditional.
export function slopJackpot() {
  if (!isSlopActive()) return;
  if (reducedMotion()) return;
  const banner = document.createElement('div');
  banner.className = 'slop-jackpot';
  banner.textContent = '🎰 JACKPOT! 🎰';
  banner.setAttribute('aria-hidden', 'true');
  document.body.appendChild(banner);
  banner.addEventListener('animationend', () => banner.remove(), { once: true });
  // Shower coins from random points along the top edge so the burst
  // feels like it rained down on the page rather than erupting from
  // the banner itself.
  const vw = window.innerWidth || document.documentElement.clientWidth;
  for (let i = 0; i < 28; i++) {
    const x = Math.random() * vw;
    const y = -20;
    setTimeout(() => spawnCoin(x, y, JACKPOT_EMOJIS, 40, vw * 0.6),
      Math.random() * 600);
  }
}
