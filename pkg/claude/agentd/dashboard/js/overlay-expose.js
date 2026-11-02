// overlay-expose.js -- every overlay window at once (tcl-vg45q5), as macOS
// Mission Control shows them (tcl-ukrr3o): the open windows themselves move and
// scale into a spread without overlap, live, terminals and boards still drawing,
// over a dimmed page. Minimized windows and tabs on the dock are not mounted, so
// they show as a row of cards along the bottom. Picking one raises it and
// leaves; Escape or a click outside leaves with nothing changed. Entering and
// leaving never moves or retiles a window: the spread is a CSS transform on the
// window elements, from a stylesheet this view owns, and nothing else changes.
// No window re-renders or reflows for it; only transforms animate.

import { h } from 'preact';
import htm from 'htm';
import { computed, signal } from '@preact/signals';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { focusOverlayWindow, minimizedOverlayTabs, openOverlayWindow, overlayHostUp, overlayWindows } from './overlay-instances.js';
import { overlayWindowsByStack, reducedMotion } from './overlay-window.js';
import { hasShownOverlay } from './overlay-stack.js';
import { matchesShortcut } from './keybindings.js';

const html = htm.bind(h);

export const OVERLAY_EXPOSE_LABEL = 'Show all windows';

const KIND_NAMES = { chat: 'Chat', board: 'Board', terminal: 'Terminal', start: 'Steward', decide: 'Decision' };
const KIND_GLYPHS = { chat: '💬', board: '✎', terminal: '⌨', start: '✦', decide: '⚖' };

export const overlayExposeOpen = signal(false);

// exposeEntriesOf is what the view shows, front first: the windows as they
// stack, minimized ones after them, then tabs that wait on the dock on their own.
export function exposeEntriesOf(windows, minimizedTabs, stack) {
  const depth = (window) => stack.indexOf(window.id);
  const shown = windows.filter((window) => !window.minimized).sort((a, b) => depth(b) - depth(a));
  const away = windows.filter((window) => window.minimized);
  const entries = [...shown, ...away].map((window) => {
    const tab = window.tabs.find((candidate) => candidate.id === window.active) || window.tabs[0];
    return { key: `w:${window.id}`, windowId: window.id, tab, tabs: window.tabs, minimized: !!window.minimized };
  });
  for (const { tab } of minimizedTabs) entries.push({ key: `t:${tab.id}`, windowId: '', tab, tabs: [tab], minimized: true, docked: true });
  return entries.filter((entry) => entry.tab);
}

// The view has something to show only with two or more, and only while Collab
// is there to hold windows at all.
export const overlayExposeAvailable = computed(() => overlayHostUp.value && overlayWindows.value.length + minimizedOverlayTabs.value.length >= 2);

export function showAllOverlayWindows(open = true) {
  overlayExposeOpen.value = open && overlayExposeAvailable.peek();
  return overlayExposeOpen.peek();
}

const matches = (entry, query) => !query || entry.tabs.some(({ label }) => String(label || '').toLowerCase().includes(query));

// Whether the view closed on a window chosen, which takes focus itself. Any
// other way out puts focus back where it was.
let picked = false;

function pick(entry) {
  picked = true;
  overlayExposeOpen.value = false;
  openOverlayWindow(entry.tab);
  focusOverlayWindow(entry.tab.id);
}

// The spread's gap between windows, and how long they take to get there and back.
const SPREAD_GAP = 28;
const SPREAD_MS = 220;

// spreadLayout lays windows of sizes [{ w, h }] out in area { x, y, width,
// height } without overlap, in reading order: the grid of columns that shows the
// most of them, each scaled down to its cell (never up) and centred in it, a
// short last row centred too. It returns each one's centre and scale.
export function spreadLayout(sizes, area, gap = SPREAD_GAP) {
  const count = sizes.length;
  let best = null;
  for (let cols = 1; cols <= count; cols++) {
    const rows = Math.ceil(count / cols);
    const cellW = (area.width - gap * (cols - 1)) / cols;
    const cellH = (area.height - gap * (rows - 1)) / rows;
    if (cellW <= 0 || cellH <= 0) continue;
    const scales = sizes.map(({ w, h }) => Math.min(1, cellW / w, cellH / h));
    const shown = scales.reduce((sum, scale, index) => sum + scale * scale * sizes[index].w * sizes[index].h, 0);
    if (!best || shown > best.shown) best = { cols, rows, cellW, cellH, scales, shown };
  }
  if (!best) return sizes.map(() => ({ x: area.x + area.width / 2, y: area.y + area.height / 2, scale: 0.1 }));
  const { cols, rows, cellW, cellH, scales } = best;
  const top = area.y + (area.height - (rows * cellH + (rows - 1) * gap)) / 2;
  return sizes.map((_, index) => {
    const row = Math.floor(index / cols);
    const inRow = Math.min(cols, count - row * cols);
    const left = area.x + (area.width - (inRow * cellW + (inRow - 1) * gap)) / 2;
    return { x: left + (index % cols) * (cellW + gap) + cellW / 2, y: top + row * (cellH + gap) + cellH / 2, scale: scales[index] };
  });
}

function windowNode(win, windowId) {
  return [...(win?.document?.querySelectorAll?.('[data-overlay-window]') || [])].find((candidate) => candidate.dataset.overlayWindow === windowId) || null;
}

// Where a shown window sits, from its layout box, which a transform leaves alone.
function boxOf(win, windowId) {
  const node = windowNode(win, windowId);
  if (!node?.offsetWidth || !node.offsetHeight) return null;
  return { x: node.offsetLeft, y: node.offsetTop, w: node.offsetWidth, h: node.offsetHeight };
}

const selectorFor = (windowId) => `[data-overlay-window="${String(windowId).replace(/["\\]/g, '\\$&')}"]`;

// The stylesheet the spread is: while it shows, the windows rise over the dim
// page, take no input and lose their resize frames and split lines; each moves
// to its place, and one that does not match the filter dims.
function spreadSheet(placed, dimmed, motion) {
  const moving = motion ? `transition: transform ${SPREAD_MS}ms ease, opacity ${SPREAD_MS}ms ease;` : '';
  // A window that came or went while the view shows has no place in the spread,
  // and stays out of sight rather than land on it.
  const rules = [
    `[data-overlay-window] { pointer-events: none !important; transform-origin: center center; ${moving} }`,
    ':is(.window-resize-frame, .overlay-split-line, .overlay-split-cross, .overlay-split-detach, .overlay-split-gap, .overlay-snap-guide) { visibility: hidden !important; }',
  ];
  if (placed.length) rules.push(`[data-overlay-window]:not(${placed.map(({ windowId }) => selectorFor(windowId)).join(', ')}) { visibility: hidden !important; }`);
  for (const { windowId, box, spot } of placed) {
    const dx = spot.x - (box.x + box.w / 2);
    const dy = spot.y - (box.y + box.h / 2);
    rules.push(`${selectorFor(windowId)} { z-index: calc(var(--overlay-level, var(--z-window)) - var(--z-window) + var(--z-dialog) + 1) !important; transform: translate(${dx.toFixed(1)}px, ${dy.toFixed(1)}px) scale(${spot.scale.toFixed(4)}) !important;${dimmed.has(windowId) ? ' opacity: .25;' : ''} }`);
  }
  return rules.join('\n');
}

// The stylesheet the spread is, rendered by OverlayExpose, which outlives the
// view: when the view goes, the windows glide back, as the transition outlives
// the transforms for as long as the glide takes.
const spreadCss = signal('');
let settling = null;

// useSpreadSheet keeps that stylesheet in the page while the view shows.
function useSpreadSheet(win, css, motion) {
  useLayoutEffect(() => {
    win?.clearTimeout?.(settling);
    spreadCss.value = css;
  }, [css]);
  useLayoutEffect(() => () => {
    if (!motion) { spreadCss.value = ''; return; }
    spreadCss.value = `[data-overlay-window] { transition: transform ${SPREAD_MS}ms ease, opacity ${SPREAD_MS}ms ease; }`;
    settling = win?.setTimeout?.(() => { spreadCss.value = ''; }, SPREAD_MS + 40);
  }, []);
}

export function OverlayExpose({ win = globalThis.window }) {
  const css = spreadCss.value;
  const sheet = css && html`<style data-overlay-expose="">${css}</style>`;
  if (!overlayExposeOpen.value || !overlayExposeAvailable.value) return sheet || null;
  return html`${sheet}<${ExposeView} win=${win} />`;
}

const optionId = (index) => `overlay-expose-option-${index}`;
const titleOf = (tab) => tab.label || KIND_NAMES[tab.kind] || 'Window';

function ExposeView({ win }) {
  const [query, setQuery] = useState('');
  const [chosen, setChosen] = useState(0);
  const input = useRef(null);
  const stage = useRef(null);
  const [motion] = useState(() => !reducedMotion(win));
  // The stack is read once on entry, so the cards keep their order while it shows.
  const [stack] = useState(() => overlayWindowsByStack());
  const [returnTo] = useState(() => win?.document?.activeElement || null);
  const entries = exposeEntriesOf(overlayWindows.value, minimizedOverlayTabs.value, stack);
  // Where each open window stands, from its layout box, which the spread's
  // transform leaves alone: read on entry, and again when the page resizes and
  // the windows keep themselves on it. One that can't be measured is a card.
  const readBoxes = () => Object.fromEntries(exposeEntriesOf(overlayWindows.peek(), [], stack).filter((entry) => !entry.minimized)
    .map((entry) => [entry.key, boxOf(win, entry.windowId)]).filter(([, box]) => box));
  const [boxes, setBoxes] = useState(readBoxes);
  const [area, setArea] = useState(null);
  const needle = query.trim().toLowerCase();
  // The live windows in reading order of where they stand, so each moves a short way.
  const live = entries.filter((entry) => !entry.minimized && boxes[entry.key])
    .sort((a, b) => (boxes[a.key].y + boxes[a.key].h / 2) - (boxes[b.key].y + boxes[b.key].h / 2) || boxes[a.key].x - boxes[b.key].x);
  const spots = area ? spreadLayout(live.map((entry) => boxes[entry.key]), area) : [];
  const placed = live.map((entry, index) => ({ entry, windowId: entry.windowId, box: boxes[entry.key], spot: spots[index] })).filter(({ spot }) => spot);
  const cards = entries.filter((entry) => !live.includes(entry));
  // With no room measured for the spread, every window is a card.
  const spreading = live.length > 0;
  const shown = (spreading ? [...(area ? live : []), ...cards] : entries).filter((entry) => matches(entry, needle));
  const at = Math.min(chosen, Math.max(0, shown.length - 1));
  const dimmed = new Set(live.filter((entry) => !matches(entry, needle)).map((entry) => entry.windowId));
  const leave = () => { overlayExposeOpen.value = false; };

  useSpreadSheet(win, spreadSheet(placed, dimmed, motion), motion);
  // The room the spread has: the stage between the filter and the cards, which
  // changes with the page and with the row of cards.
  useLayoutEffect(() => {
    if (!spreading) return undefined;
    const measure = () => {
      const rect = stage.current?.getBoundingClientRect?.();
      setArea((now) => {
        const next = rect?.width && rect.height ? { x: rect.left, y: rect.top, width: rect.width, height: rect.height } : null;
        return JSON.stringify(now) === JSON.stringify(next) ? now : next;
      });
    };
    measure();
    const observer = typeof win?.ResizeObserver === 'function' ? new win.ResizeObserver(measure) : null;
    if (stage.current) observer?.observe(stage.current);
    // The windows keep themselves on the page in their own resize handlers: read
    // them after those.
    const resized = () => win.requestAnimationFrame?.(() => { setBoxes(readBoxes()); measure(); });
    win?.addEventListener?.('resize', resized);
    return () => {
      observer?.disconnect();
      win?.removeEventListener?.('resize', resized);
    };
  }, [spreading]);

  // However the view goes, the flag goes with it, so it never comes back unasked.
  // Leaving with nothing chosen puts focus back, as into a terminal it came from:
  // after the pointer's own default, which would blur to the page.
  useEffect(() => {
    picked = false;
    input.current?.focus?.();
    return () => {
      overlayExposeOpen.value = false;
      if (!picked && returnTo?.isConnected !== false) win?.setTimeout?.(() => returnTo?.focus?.(), 0);
    };
  }, []);

  function onKey(event) {
    const forward = event.key === 'ArrowRight' || event.key === 'ArrowDown' || (event.key === 'Tab' && !event.shiftKey);
    const back = event.key === 'ArrowLeft' || event.key === 'ArrowUp' || (event.key === 'Tab' && event.shiftKey);
    if (event.key !== 'Escape' && event.key !== 'Enter' && !forward && !back) return;
    // The view keeps its keys: no focus trap or shortcut beneath it sees them.
    event.preventDefault();
    event.stopPropagation();
    if (event.key === 'Escape') leave();
    else if (event.key === 'Enter') { if (shown[at]) pick(shown[at]); } else if (forward) setChosen((at + 1) % Math.max(1, shown.length));
    else setChosen((at - 1 + shown.length) % Math.max(1, shown.length));
  }

  const away = (event) => { if (event.target === event.currentTarget) leave(); };
  const option = (entry, index, extra = {}) => ({
    key: entry.key, id: optionId(index), type: 'button', role: 'option', 'aria-selected': index === at ? 'true' : 'false',
    title: `Raise ${titleOf(entry.tab)}`, onMouseEnter: () => setChosen(index), onClick: () => pick(entry), ...extra,
  });
  const cardList = spreading ? shown.filter((entry) => cards.includes(entry)) : shown;
  const spread = placed.length > 0;
  // The windows themselves can't take a click while spread: a target over each
  // one does, above the windows and so outside the dimmed view, whose level they
  // pass. It names the window and its tabs where the pointer or the arrows are.
  const targets = spread && html`<div class="overlay-expose-targets" role="listbox" aria-label="Open windows">
    ${placed.map(({ entry, box, spot }) => {
      const index = shown.indexOf(entry);
      if (index < 0) return null;
      const w = box.w * spot.scale;
      const h = box.h * spot.scale;
      const { tab, tabs } = entry;
      // The filter keeps the keyboard: a press here that never becomes a click
      // must not leave focus outside the view's keys.
      return html`<button ...${option(entry, index)} class=${`overlay-expose-live${index === at ? ' chosen' : ''}`} data-ui-exempt
        onPointerDown=${(event) => event.preventDefault()}
        style=${`left:${(spot.x - w / 2).toFixed(1)}px;top:${(spot.y - h / 2).toFixed(1)}px;width:${w.toFixed(1)}px;height:${h.toFixed(1)}px`}>
        <span class="overlay-expose-live-label">${KIND_GLYPHS[tab.kind] || '▣'} ${titleOf(tab)}${tabs.length > 1 && html` <span class="overlay-expose-count" aria-label=${`${tabs.length} tabs`}>${tabs.length}</span>`}</span>
      </button>`;
    })}
  </div>`;

  return html`<div class=${`overlay-expose${spread ? ' spread' : ''}`} role="dialog" aria-modal="true" aria-label="All windows" onKeyDown=${onKey}
    onPointerDown=${away}>
    <div class="overlay-expose-bar">
      <input ref=${input} class="overlay-expose-filter" type="text" placeholder="Type to filter windows" aria-label="Filter windows by title"
        role="combobox" aria-expanded="true" aria-controls="overlay-expose-list" aria-activedescendant=${shown[at] ? optionId(at) : undefined}
        value=${query} onInput=${(event) => { setQuery(event.currentTarget.value); setChosen(0); }} />
      <span class="overlay-expose-hint">Enter or a click raises · Esc leaves</span>
    </div>
    ${spreading && html`<div ref=${stage} class="overlay-expose-stage" onPointerDown=${away}></div>`}
    <div class=${`overlay-expose-grid${spreading ? ' row' : ''}`} id="overlay-expose-list" role="listbox" aria-label=${spreading ? 'Minimized windows' : 'Windows'}
      onPointerDown=${away}>
      ${shown.length === 0 && html`<p class="overlay-expose-empty ui-empty-block">No window matches “${query.trim()}”.</p>`}
      ${cardList.map((entry) => {
        const index = shown.indexOf(entry);
        const { tab, tabs } = entry;
        const others = tabs.filter((other) => other.id !== tab.id);
        return html`<button ...${option(entry, index)} class=${`overlay-expose-card${index === at ? ' chosen' : ''}${entry.minimized ? ' minimized' : ''}`} data-ui-exempt>
          <span class="overlay-expose-head">
            <span class="overlay-expose-glyph" aria-hidden="true">${KIND_GLYPHS[tab.kind] || '▣'}</span>
            <span class="overlay-expose-title">${titleOf(tab)}</span>
            ${tabs.length > 1 && html`<span class="overlay-expose-count" aria-label=${`${tabs.length} tabs`}>${tabs.length}</span>`}
          </span>
          <span class="overlay-expose-kind">${KIND_NAMES[tab.kind] || 'Window'}${entry.docked ? ' · on the dock' : entry.minimized ? ' · minimized' : ''}</span>
          ${others.length > 0 && html`<span class="overlay-expose-tabs">${others.map((other) => html`<span key=${other.id} class="overlay-expose-tab">${other.label || KIND_NAMES[other.kind] || 'Tab'}</span>`)}</span>`}
        </button>`;
      })}
    </div>
  </div>
  ${targets}`;
}

// installOverlayExposeKey lets its shortcut (Ctrl ,) open and close the view from anywhere,
// a focused terminal included: capture phase, before xterm sees the key.
export function installOverlayExposeKey(win) {
  const onKey = (event) => {
    if (!matchesShortcut(event, 'expose')) return;
    // Not over a dialog: its focus trap would pull typing into the form beneath.
    if (!overlayExposeOpen.peek() && (!overlayExposeAvailable.peek() || hasShownOverlay())) return;
    event.preventDefault();
    event.stopPropagation();
    showAllOverlayWindows(!overlayExposeOpen.peek());
  };
  win?.addEventListener?.('keydown', onKey, true);
  return () => win?.removeEventListener?.('keydown', onKey, true);
}
