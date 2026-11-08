// overlay-window-motion.js -- the one minimize and restore animation every
// overlay window plays (tcl-2e15pj), whatever sent it to the dock or brought it
// back: its own −, the dock's pill, Ctrl ; and the bulk minimize and restore,
// the Steward's Ctrl . and chip.
//
// It needs no caller to ask. It watches the windows' minimized flags, so every
// path that flips one is animated, and the state stays the truth from the first
// moment: a window minimizing is minimized already, and a second press during
// the flight brings it back like any other press would.
//
// The animation is FLIP on the live window, so the operator sees the real
// content (terminal, drawing board, page) move, nothing cloned. Minimizing
// keeps the window shown, as if open, while it flies from where it was into its
// dock pill (or the Steward's chip, or the dock's "+N" and ☰ when the pill is
// folded away) with a fade near the end; then it is put away. Restoring starts
// the window at that rectangle and grows it to its own. A press mid-flight
// starts the opposite flight from where the window is on screen, taking as long
// as it had flown. Several windows at once stagger, the whole kept short.
// Reduced motion plays nothing: the windows go and come at once, as before.
//
// No window flies black, and none waits to fly. Content that takes a moment to
// come up (a terminal attaching, which starts the moment its window renders)
// registers here (registerLiveContent), and every flight asks ensureLiveContent
// as it starts: content not live yet is covered by the last frame it showed, or
// a neutral placeholder, and the cover comes off the moment the live content is
// up, mid-flight or after.

import { effect, signal, untracked } from '@preact/signals';
import { h } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

export const MOTION_MS = 220;
export const MOTION_STAGGER_MS = 25;
// No bulk minimize or restore runs longer than this, the stagger shrinking to fit.
export const MOTION_TOTAL_MS = 400;
const EASING = 'cubic-bezier(.4,0,.2,1)';
// How faint a window is at the dock end of its flight.
const DOCKED_OPACITY = 0.25;

export function reducedMotion(win) {
  try {
    return !!win?.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches;
  } catch {
    return false;
  }
}

// The window ids in flight: 'minimize', 'restore', or 'waiting' for a restore
// rendered but not yet started, which keeps it out of sight for that frame.
export const overlayWindowMotion = signal(new Map());

// overlayWindowLeaving reports whether a window, minimized already, is still
// flying to the dock, and so still shown as if open.
export const overlayWindowLeaving = (id) => overlayWindowMotion.value.get(id) === 'minimize';
// overlayWindowWaiting reports whether a window being restored is out of sight
// until its flight starts, on the next frame.
export const overlayWindowWaiting = (id) => overlayWindowMotion.value.get(id) === 'waiting';

function setMotion(id, state) {
  const next = new Map(overlayWindowMotion.peek());
  if (state) next.set(id, state);
  else next.delete(id);
  overlayWindowMotion.value = next;
}

// Where a window goes besides its dock pill, for windows with none: each is
// asked (window) and may name an element or a rectangle.
const motionTargets = new Set();

export function addOverlayMotionTarget(resolve) {
  motionTargets.add(resolve);
  return () => motionTargets.delete(resolve);
}

// ---- live content ----------------------------------------------------------

// Content that may not be up at once, by window id: { tabId, live, ready,
// capture, host }. live() says whether it is up now; ready() resolves true once
// it is, false when it gives up; capture() is a copy of what it shows now, or
// null when that is nothing worth keeping; host() is the element it draws in,
// which a cover lies over.
const liveContent = new Map();
// The last good frame of each window: { tabId, node }.
const lastFrames = new Map();

function keepFrame(windowId, provider) {
  let node = null;
  try { node = provider.capture?.() || null; } catch { node = null; }
  if (node) lastFrames.set(windowId, { tabId: provider.tabId, node });
}

// registerLiveContent says the content of windowId may need a moment. It
// returns the way to take that back, which keeps the content's last frame.
export function registerLiveContent(windowId, provider) {
  liveContent.set(windowId, provider);
  return () => {
    if (liveContent.get(windowId) !== provider) return;
    keepFrame(windowId, provider);
    liveContent.delete(windowId);
  };
}

export function resetOverlayMotionForTest() {
  liveContent.clear();
  lastFrames.clear();
  motionTargets.clear();
  overlayLiveCovers.value = new Map();
  overlayWindowMotion.value = new Map();
}

const sleep = (win, ms) => new Promise((resolve) => {
  const timer = win?.setTimeout || globalThis.setTimeout;
  timer(resolve, ms);
});

// What covers each window's content while it comes up, by window id:
// { frame, rect, token }. frame is the kept copy of its last screen, or null
// for the placeholder; rect is where the content sits in the window. The window
// draws it (LiveCover); the copy is handed to that host, never built here.
export const overlayLiveCovers = signal(new Map());
let coverTokens = 0;

function setCover(id, cover) {
  const next = new Map(overlayLiveCovers.peek());
  if (cover) next.set(id, cover);
  else next.delete(id);
  overlayLiveCovers.value = next;
}

// coverContent covers the content of window id at host with frame (or, with
// none, a placeholder) until the returned function takes it off. One cover at
// a time: a flight turned round replaces the one before.
function coverContent(id, host, frame) {
  const section = host?.closest?.('[data-overlay-window]');
  const at = host?.getBoundingClientRect?.();
  const outer = section?.getBoundingClientRect?.();
  if (!at || !outer) return () => {};
  const token = ++coverTokens;
  setCover(id, {
    frame,
    token,
    rect: { left: at.left - outer.left - (section.clientLeft || 0), top: at.top - outer.top - (section.clientTop || 0), width: at.width, height: at.height },
  });
  return () => {
    if (overlayLiveCovers.peek().get(id)?.token === token) setCover(id, null);
  };
}

// LiveCover draws a window's cover: its last screen, or "Connecting…".
export function LiveCover({ cover }) {
  const holder = useRef(null);
  useLayoutEffect(() => {
    const node = holder.current;
    if (!node || !cover?.frame) return undefined;
    node.replaceChildren(cover.frame);
    return () => node.replaceChildren();
  }, [cover?.frame]);
  if (!cover) return null;
  const { left, top, width, height } = cover.rect;
  return html`<div class=${`overlay-live-frame${cover.frame ? '' : ' placeholder'}`} aria-hidden="true" inert
    data-overlay-live-frame=${cover.frame ? 'last' : 'placeholder'} style=${`left:${left}px;top:${top}px;width:${width}px;height:${height}px`}>
    ${cover.frame ? html`<div ref=${holder} class="overlay-live-frame-holder"></div>` : 'Connecting…'}
  </div>`;
}

// ensureLiveContent is what every flight asks as it starts. Content that is
// live is left alone. Otherwise it is covered at once with its last good frame,
// or a placeholder, until it is up. The promise resolves true when the window
// shows live content, false when its content gave up; it never holds a flight.
export function ensureLiveContent(window) {
  const provider = liveContent.get(window.id);
  if (!provider) return Promise.resolve(true);
  let live = false;
  try { live = !!provider.live?.(); } catch { live = false; }
  if (live) {
    keepFrame(window.id, provider);
    return Promise.resolve(true);
  }
  const kept = lastFrames.get(window.id);
  const frame = kept && kept.tabId === provider.tabId ? kept.node.cloneNode(true) : null;
  let host = null;
  try { host = provider.host?.() || null; } catch { host = null; }
  const uncover = coverContent(window.id, host, frame);
  return Promise.resolve().then(() => provider.ready()).then((ok) => !!ok, () => false).then((ok) => {
    if (ok && liveContent.get(window.id) === provider) keepFrame(window.id, provider);
    uncover();
    return ok;
  });
}

// terminalReady resolves true once the terminal widget in widgetRef is
// connected (statusRef) and has drawn its first output, false when it gives up.
export async function terminalReady(widgetRef, statusRef, { win, pollMs = 30, timeoutMs = 10000 } = {}) {
  const started = Date.now();
  while (!(widgetRef.current && statusRef.current === 'connected')) {
    if (Date.now() - started >= timeoutMs) return false;
    await sleep(win, pollMs);
  }
  const widget = widgetRef.current;
  // Connected, its first output comes quickly or not at all: a stale frame
  // should not stand over a live terminal for long.
  return typeof widget.settled === 'function' ? widget.settled({ quietMs: 30, maxAfterOutputMs: 150, timeoutMs: 1500 }) : true;
}

// captureTerminal copies the screen of the terminal in host, drawn by xterm's
// DOM renderer, or null while it shows nothing.
export function captureTerminal(host) {
  const screen = host?.querySelector?.('.xterm');
  if (!screen || !screen.querySelector?.('.xterm-rows')?.textContent?.trim()) return null;
  const copy = screen.cloneNode(true);
  copy.classList?.add('overlay-live-frame-copy');
  return copy;
}

// useLiveTerminal registers the terminal of a window's tab as live content,
// while windowId names its window (empty while it shows no terminal). host() is
// the element the terminal draws in. It returns the terminal's onStatus, which
// passes the status on to setStatus and keeps the last frame the moment the
// terminal stops being live.
export function useLiveTerminal({ windowId, tabId, widgetRef, host, win, setStatus }) {
  const statusRef = useRef('');
  useLayoutEffect(() => {
    if (!windowId) return undefined;
    return registerLiveContent(windowId, {
      tabId,
      host,
      live: () => statusRef.current === 'connected' && !!host()?.querySelector?.('.xterm-rows')?.textContent?.trim(),
      ready: () => terminalReady(widgetRef, statusRef, { win }),
      capture: () => (statusRef.current === 'connected' ? captureTerminal(host()) : null),
    });
  }, [windowId, tabId]);
  return (next) => {
    const provider = windowId && liveContent.get(windowId);
    if (provider && statusRef.current === 'connected' && next !== 'connected') keepFrame(windowId, provider);
    statusRef.current = next;
    setStatus(next);
  };
}

// ---- the flight --------------------------------------------------------------

// staggerDelays is when each of count windows starts, step apart, closer when
// that would take the whole past total.
export function staggerDelays(count, { duration = MOTION_MS, step = MOTION_STAGGER_MS, total = MOTION_TOTAL_MS } = {}) {
  const each = count > 1 ? Math.max(0, Math.min(step, (total - duration) / (count - 1))) : 0;
  return Array.from({ length: count }, (_, index) => Math.round(index * each));
}

// motionOrder is the order windows moving together start in: the front one
// leaves first, and the back one returns first, so the front lands on top.
// stack lists window ids, the front last.
export function motionOrder(moves, stack) {
  const depth = (id) => {
    const at = stack.indexOf(id);
    return at < 0 ? stack.length : at;
  };
  return [...moves].sort((a, b) => (a.dir === b.dir ? 0 : a.dir === 'minimize' ? -1 : 1)
    || (a.dir === 'minimize' ? depth(b.id) - depth(a.id) : depth(a.id) - depth(b.id)));
}

// flipTransform makes a box laid out at box look as if at rect.
export function flipTransform(box, rect) {
  const sx = Math.max(rect.width, 1) / Math.max(box.width, 1);
  const sy = Math.max(rect.height, 1) / Math.max(box.height, 1);
  return `translate(${(rect.left - box.left).toFixed(1)}px, ${(rect.top - box.top).toFixed(1)}px) scale(${sx.toFixed(4)}, ${sy.toFixed(4)})`;
}

// motionKeyframes is the flight between a window's own box and rect: down into
// rect when minimizing, fading near the end, or up out of it when restoring.
// from, for a flight turned round, is where the window was on screen then.
export function motionKeyframes(dir, box, rect, { from = null, fromOpacity = 1 } = {}) {
  const start = { transformOrigin: '0 0', transform: from ? flipTransform(box, from) : dir === 'minimize' ? 'none' : flipTransform(box, rect) };
  if (dir === 'minimize') {
    return [
      { ...start, opacity: fromOpacity, offset: 0 },
      { opacity: Math.max(fromOpacity, DOCKED_OPACITY), offset: 0.6 },
      { transformOrigin: '0 0', transform: flipTransform(box, rect), opacity: DOCKED_OPACITY, offset: 1 },
    ];
  }
  return [
    { ...start, opacity: from ? fromOpacity : DOCKED_OPACITY, offset: 0 },
    { opacity: 1, offset: 0.4 },
    { transformOrigin: '0 0', transform: 'none', opacity: 1, offset: 1 },
  ];
}

const rectOf = (node) => {
  const rect = node?.getBoundingClientRect?.();
  return rect && rect.width > 0 && rect.height > 0 ? { left: rect.left, top: rect.top, width: rect.width, height: rect.height } : null;
};

function windowNode(doc, id) {
  return [...(doc?.querySelectorAll?.('[data-overlay-window]') || [])].find((node) => node.dataset?.overlayWindow === id) || null;
}

const shownIn = (node) => !node.closest?.('.overlay-dock-measure');

// dockRectOf is the rectangle a window flies into or out of: its pill, or what
// stands for it (a chip), or the dock's "+N" or ☰ it is folded behind, or failing
// all, a spot at the foot of the page.
export function dockRectOf(win, window) {
  const doc = win?.document;
  const pill = [...(doc?.querySelectorAll?.('[data-dock-window]') || [])].find((node) => node.dataset?.dockWindow === window.id && shownIn(node));
  const pillRect = rectOf(pill);
  if (pillRect) return pillRect;
  for (const resolve of motionTargets) {
    let found = null;
    try { found = resolve(window); } catch { found = null; }
    const rect = found && (typeof found.getBoundingClientRect === 'function' ? rectOf(found) : found);
    if (rect?.width && rect.height) return rect;
  }
  for (const selector of ['.overlay-dock-more-button', '.overlay-dock-menu-button']) {
    const rect = rectOf([...(doc?.querySelectorAll?.(selector) || [])].find(shownIn));
    if (rect) return rect;
  }
  const width = win?.innerWidth || 0;
  const height = win?.innerHeight || 0;
  return width && height ? { left: width / 2 - 60, top: height - 28, width: 120, height: 24 } : null;
}

// installOverlayWindowMotion animates the windows of windows (a signal of the
// overlay windows) in win from now on. stack() lists window ids, the front
// last. It returns the way to stop.
export function installOverlayWindowMotion(win, { windows, stack = () => [] }) {
  let known = new Map(windows.peek().map((window) => [window.id, !!window.minimized]));
  // id -> { dir, animation, delay }
  const flights = new Map();
  // A hidden browser tab runs no animation frames: there, the next tick.
  const frame = (run) => (typeof win?.requestAnimationFrame === 'function' && !win.document?.hidden
    ? win.requestAnimationFrame(run) : (win?.setTimeout || globalThis.setTimeout)(run, 0));

  function land(id, flight) {
    if (flights.get(id) !== flight) return;
    flights.delete(id);
    flight.animation?.cancel?.();
    setMotion(id, '');
  }

  // launch starts a flight's animation, its window laid out where it ends up.
  function launch(window, flight, { from, fromOpacity, duration, turned }) {
    const id = window.id;
    const shown = windowNode(win?.document, id);
    const box = rectOf(shown);
    const rect = flight.dir === 'minimize' ? dockRectOf(win, window) : box;
    if (!box || !rect || typeof shown.animate !== 'function') {
      land(id, flight);
      return;
    }
    const keyframes = motionKeyframes(flight.dir, box, flight.dir === 'minimize' ? rect : from, { from: turned ? from : null, fromOpacity });
    flight.animation = shown.animate(keyframes, { duration, delay: flight.delay, easing: EASING, fill: 'both' });
    flight.animation.onfinish = () => land(id, flight);
    if (flight.dir === 'restore') setMotion(id, 'restore');
  }

  function fly(window, dir, delay) {
    const doc = win?.document;
    const id = window.id;
    const prior = flights.get(id);
    // A restore not yet started never showed: going back is instant.
    if (prior && !prior.animation) {
      land(id, prior);
      return;
    }
    const node = windowNode(doc, id);
    // Where the flight starts on screen: the window as it shows now, or for a
    // restore from rest, its pill.
    const turned = !!prior;
    const from = turned || dir === 'minimize' ? rectOf(node) : dockRectOf(win, window);
    let fromOpacity = 1;
    let duration = MOTION_MS;
    if (prior) {
      try { fromOpacity = Number(win.getComputedStyle?.(node)?.opacity ?? 1); } catch { fromOpacity = 1; }
      if (!Number.isFinite(fromOpacity)) fromOpacity = 1;
      const flown = Number(prior.animation.currentTime ?? MOTION_MS) - prior.delay;
      duration = Math.max(60, Math.min(MOTION_MS, flown));
      prior.animation.cancel?.();
    }
    if (!from) {
      if (prior) land(id, prior);
      return;
    }
    if (dir === 'minimize' && node?.contains?.(doc?.activeElement)) doc.activeElement.blur?.();
    const flight = { dir, animation: null, delay: turned ? 0 : delay };
    flights.set(id, flight);
    // A window turned round is on screen already, and flies on at once.
    setMotion(id, dir === 'minimize' ? 'minimize' : turned ? 'restore' : 'waiting');
    // On the next frame, before it paints, the window has rendered as it now
    // is (its content mounting and attaching, its pill on the dock): it flies
    // with the best content it has.
    frame(() => {
      if (flights.get(id) !== flight) return;
      void ensureLiveContent(window);
      launch(window, flight, { from, fromOpacity, duration, turned });
    });
  }

  const stop = effect(() => {
    const now = windows.value;
    untracked(() => {
      const moves = [];
      for (const window of now) {
        const was = known.get(window.id);
        if (was !== undefined && was !== !!window.minimized) moves.push({ id: window.id, dir: window.minimized ? 'minimize' : 'restore', window });
      }
      known = new Map(now.map((window) => [window.id, !!window.minimized]));
      // A window gone mid-flight just stops, and its last frame goes with it.
      for (const [id, flight] of flights) if (!known.has(id)) land(id, flight);
      for (const id of lastFrames.keys()) if (!known.has(id)) lastFrames.delete(id);
      for (const id of overlayLiveCovers.peek().keys()) if (!known.has(id)) setCover(id, null);
      if (!moves.length) return;
      if (reducedMotion(win)) {
        // Instant, as before; a flight under way stops where it would end.
        for (const { id } of moves) if (flights.has(id)) land(id, flights.get(id));
        return;
      }
      const ordered = motionOrder(moves, stack());
      const delays = staggerDelays(ordered.length);
      ordered.forEach((move, index) => fly(move.window, move.dir, delays[index]));
    });
  });

  return () => {
    stop();
    for (const [id, flight] of flights) land(id, flight);
  };
}
