// overlay-snap-assist.js -- Snap Assist (tcl-aahter): once the operator snaps a
// window, the largest cell left empty shows the windows outside the tiling, each
// as a card with its title and kind. A click on one places it there, and the next
// empty cell asks, until none are left. A press anywhere else, Escape, or any
// drag ends it. Arrows move between the cards and Enter places one.

import { h } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { focusOverlayWindow, minimizeOverlayWindow, overlayWindows } from './overlay-instances.js';
import { activeTabOf } from './overlay-tab-model.js';
import { endSnapAssist, placeSnapAssisted, snapAssist, tiledOverlayIds } from './overlay-window.js';

const html = htm.bind(h);

// snapAssistChoices is every window that could fill the cell: open or minimized,
// and in no tiling.
export function snapAssistChoices(windows, tiled) {
  return windows.filter((window) => !tiled.has(window.id) && window.tabs.length);
}

// focusSnapped gives the keyboard back to the window the operator snapped, as
// Escape leaves the question: the window itself, then whatever its tab holds,
// such as a terminal.
function focusSnapped(doc, windowId) {
  const window = overlayWindows.peek().find((candidate) => candidate.id === windowId);
  if (!window) return;
  [...(doc?.querySelectorAll?.('[data-overlay-window]') || [])].find((node) => node.dataset.overlayWindow === windowId)?.focus?.();
  focusOverlayWindow(window.active);
}

export function SnapAssist({ win, viewOf }) {
  const assist = snapAssist.value;
  // The window just snapped may not have joined the tiling yet.
  const choices = assist ? snapAssistChoices(overlayWindows.value, tiledOverlayIds()).filter((window) => window.id !== assist.from) : [];
  const panel = useRef(null);
  const asking = !!assist && choices.length > 0;
  // Nothing to offer is no question.
  useEffect(() => { if (assist && !choices.length) endSnapAssist(); }, [assist, choices.length]);
  useEffect(() => {
    if (!asking) return undefined;
    // After the release that snapped, which hands focus to its window.
    const focus = setTimeout(() => panel.current?.querySelector?.('.snap-assist-card')?.focus?.(), 0);
    const doc = win?.document;
    const press = (event) => { if (!panel.current?.contains(event.target)) endSnapAssist(); };
    // While it asks, the keyboard is its: arrows and Enter from wherever focus
    // went, such as a terminal the snapped window handed it to, reach the cards.
    const key = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopPropagation();
        const from = snapAssist.peek()?.from;
        endSnapAssist();
        focusSnapped(doc, from);
        return;
      }
      if (!['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Enter'].includes(event.key) || panel.current?.contains(doc.activeElement)) return;
      event.preventDefault();
      event.stopPropagation();
      panel.current?.querySelector?.('.snap-assist-card')?.focus?.();
    };
    doc?.addEventListener?.('pointerdown', press, true);
    doc?.addEventListener?.('keydown', key, true);
    return () => {
      clearTimeout(focus);
      doc?.removeEventListener?.('pointerdown', press, true);
      doc?.removeEventListener?.('keydown', key, true);
    };
  }, [asking, assist?.gap]);
  if (!asking) return null;
  const pick = (window) => {
    if (window.minimized) minimizeOverlayWindow(window.active, false);
    placeSnapAssisted(window.id);
  };
  const onKeyDown = (event) => {
    const cards = [...panel.current.querySelectorAll('.snap-assist-card')];
    const at = cards.indexOf(win?.document?.activeElement);
    const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[event.key];
    if (!step) return;
    event.preventDefault();
    cards[(at + step + cards.length) % cards.length]?.focus?.();
  };
  const { rect } = assist.gap;
  return html`<div ref=${panel} class="snap-assist" role="listbox" aria-label="Pick a window for this space"
    style=${{ left: `${rect.x}px`, top: `${rect.y}px`, width: `${rect.w}px`, height: `${rect.h}px` }} onKeyDown=${onKeyDown}>
    ${choices.map((window) => {
      const tab = activeTabOf(window);
      const { label, lead } = viewOf(tab);
      const more = window.tabs.length - 1;
      return html`<button type="button" key=${window.id} class="snap-assist-card" role="option" aria-selected="false"
        title=${`Put ${label} here`} onClick=${() => pick(window)}>
        <span class="snap-assist-kind" aria-hidden="true">${lead}</span>
        <span class="snap-assist-title">${label}</span>
        ${more > 0 && html`<span class="snap-assist-more">+${more}</span>`}
        ${window.minimized && html`<span class="snap-assist-note">minimized</span>`}
      </button>`;
    })}
  </div>`;
}
