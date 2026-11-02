// overlay-expose.js -- every overlay window at once (tcl-vg45q5). A dimmed view
// over the page lays all windows out side by side, minimized ones too, each with
// its kind, active tab and how many tabs it holds. Picking one raises it and
// leaves; Escape or a click outside leaves with nothing changed. Entering and
// leaving never moves or retiles a window: the view only reads them.

import { h } from 'preact';
import htm from 'htm';
import { computed, signal } from '@preact/signals';
import { useEffect, useRef, useState } from 'preact/hooks';
import { focusOverlayWindow, minimizedOverlayTabs, openOverlayWindow, overlayHostUp, overlayWindows } from './overlay-instances.js';
import { overlayWindowsByStack } from './overlay-window.js';

const html = htm.bind(h);

export const OVERLAY_EXPOSE_LABEL = 'Show all windows';
export const OVERLAY_EXPOSE_SHORTCUT = 'Ctrl+,';

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

function pick(entry) {
  overlayExposeOpen.value = false;
  openOverlayWindow(entry.tab);
  focusOverlayWindow(entry.tab.id);
}

// Where a shown window sits in the page, as shares of it, for the card's map.
function shareOf(win, windowId) {
  const node = [...(win?.document?.querySelectorAll?.('[data-overlay-window]') || [])].find((candidate) => candidate.dataset.overlayWindow === windowId);
  const rect = node?.getBoundingClientRect?.();
  const width = win?.innerWidth || 0;
  const height = win?.innerHeight || 0;
  if (!rect || !width || !height || !rect.width) return null;
  const clamp = (value) => Math.max(0, Math.min(100, value));
  return { left: clamp((rect.left / width) * 100), top: clamp((rect.top / height) * 100), width: clamp((rect.width / width) * 100), height: clamp((rect.height / height) * 100) };
}

export function OverlayExpose({ win = globalThis.window }) {
  if (!overlayExposeOpen.value) return null;
  return html`<${ExposeView} win=${win} />`;
}

function ExposeView({ win }) {
  const [query, setQuery] = useState('');
  const [chosen, setChosen] = useState(0);
  const input = useRef(null);
  // The stack is read once on entry, so the cards keep their order while it shows.
  const [stack] = useState(() => overlayWindowsByStack());
  const [returnTo] = useState(() => win?.document?.activeElement || null);
  const entries = exposeEntriesOf(overlayWindows.value, minimizedOverlayTabs.value, stack);
  const needle = query.trim().toLowerCase();
  const shown = entries.filter((entry) => matches(entry, needle));
  const at = Math.min(chosen, Math.max(0, shown.length - 1));
  // Read once on entry: the view changes nothing, so nothing moves under it.
  const [maps] = useState(() => Object.fromEntries(entries.filter((entry) => entry.windowId && !entry.minimized).map((entry) => [entry.key, shareOf(win, entry.windowId)])));
  // Leaving with nothing chosen puts focus back, as into a terminal it came from.
  const leave = () => {
    overlayExposeOpen.value = false;
    returnTo?.focus?.();
  };

  useEffect(() => { input.current?.focus?.(); }, []);
  // A window closing to fewer than two leaves nothing to choose between.
  useEffect(() => { if (entries.length < 2) leave(); }, [entries.length]);

  function onKey(event) {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      leave();
    } else if (event.key === 'Enter') {
      event.preventDefault();
      if (shown[at]) pick(shown[at]);
    } else if (event.key === 'ArrowRight' || event.key === 'ArrowDown' || (event.key === 'Tab' && !event.shiftKey)) {
      event.preventDefault();
      setChosen((at + 1) % Math.max(1, shown.length));
    } else if (event.key === 'ArrowLeft' || event.key === 'ArrowUp' || (event.key === 'Tab' && event.shiftKey)) {
      event.preventDefault();
      setChosen((at - 1 + shown.length) % Math.max(1, shown.length));
    }
  }

  return html`<div class="overlay-expose" role="dialog" aria-modal="true" aria-label="All windows" onKeyDown=${onKey}
    onPointerDown=${(event) => { if (event.target === event.currentTarget) leave(); }}>
    <div class="overlay-expose-bar">
      <input ref=${input} class="overlay-expose-filter" type="text" placeholder="Type to filter windows" aria-label="Filter windows by title"
        value=${query} onInput=${(event) => { setQuery(event.currentTarget.value); setChosen(0); }} />
      <span class="overlay-expose-hint">Enter or a click raises · Esc leaves</span>
    </div>
    <div class="overlay-expose-grid" role="listbox" aria-label="Windows"
      onPointerDown=${(event) => { if (event.target === event.currentTarget) leave(); }}>
      ${shown.length === 0 && html`<p class="overlay-expose-empty">No window matches “${query.trim()}”.</p>`}
      ${shown.map((entry, index) => {
        const { tab, tabs } = entry;
        const map = maps[entry.key];
        const others = tabs.filter((other) => other.id !== tab.id);
        return html`<button key=${entry.key} type="button" role="option" aria-selected=${index === at ? 'true' : 'false'}
          class=${`overlay-expose-card${index === at ? ' chosen' : ''}${entry.minimized ? ' minimized' : ''}`}
          title=${`Raise ${tab.label || KIND_NAMES[tab.kind] || 'window'}`}
          onMouseEnter=${() => setChosen(index)} onClick=${() => pick(entry)}>
          <span class="overlay-expose-head">
            <span class="overlay-expose-glyph" aria-hidden="true">${KIND_GLYPHS[tab.kind] || '▣'}</span>
            <span class="overlay-expose-title">${tab.label || KIND_NAMES[tab.kind] || 'Window'}</span>
            ${tabs.length > 1 && html`<span class="overlay-expose-count" aria-label=${`${tabs.length} tabs`}>${tabs.length}</span>`}
          </span>
          <span class="overlay-expose-kind">${KIND_NAMES[tab.kind] || 'Window'}${entry.docked ? ' · on the dock' : entry.minimized ? ' · minimized' : ''}</span>
          ${others.length > 0 && html`<span class="overlay-expose-tabs">${others.map((other) => html`<span key=${other.id} class="overlay-expose-tab">${other.label || KIND_NAMES[other.kind] || 'Tab'}</span>`)}</span>`}
          <span class="overlay-expose-map" aria-hidden="true">
            ${map && html`<span class="overlay-expose-map-window" style=${`left:${map.left}%;top:${map.top}%;width:${map.width}%;height:${map.height}%`}></span>`}
          </span>
        </button>`;
      })}
    </div>
  </div>`;
}

// installOverlayExposeKey lets Ctrl+, open and close the view from anywhere,
// a focused terminal included: capture phase, before xterm sees the key.
export function installOverlayExposeKey(win) {
  const onKey = (event) => {
    if (!event.ctrlKey || event.altKey || event.metaKey || event.shiftKey || event.key !== ',') return;
    if (!overlayExposeOpen.peek() && !overlayExposeAvailable.peek()) return;
    event.preventDefault();
    event.stopPropagation();
    showAllOverlayWindows(!overlayExposeOpen.peek());
  };
  win?.addEventListener?.('keydown', onKey, true);
  return () => win?.removeEventListener?.('keydown', onKey, true);
}
