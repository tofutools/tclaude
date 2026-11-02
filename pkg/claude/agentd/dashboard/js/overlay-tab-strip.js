// overlay-tab-strip.js -- a window's tabs, in its title bar. Even a window of one
// tab shows it, so there is one layout. That tab drags the window, as a browser's
// only tab does, and a window of several drags by the bar around them. A tab's ×
// minimizes that tab to the dock and stops nothing.

import { h } from 'preact';
import { useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { activateOverlayTab, minimizeOverlayTab } from './overlay-instances.js';
import { startTabDrag, tabDropTarget } from './overlay-tab-drag.js';

const html = htm.bind(h);

export function OverlayTabStrip({ window, overlay, win, viewOf }) {
  const target = tabDropTarget.value?.windowId === window.id ? tabDropTarget.value : null;
  const stripRef = useRef(null);
  // A strip too full for its bar still shows the tab that is showing.
  useLayoutEffect(() => {
    const strip = stripRef.current;
    const box = strip?.getBoundingClientRect?.();
    const tab = strip?.querySelector?.('.companion-tab.selected')?.getBoundingClientRect?.();
    if (!box || !tab) return;
    if (tab.left < box.left) strip.scrollLeft += tab.left - box.left;
    else if (tab.right > box.right) strip.scrollLeft += tab.right - box.right;
  }, [window.active, window.tabs.length]);
  const onPointerDown = (event, tab) => {
    if (event.button !== 0) return;
    if (window.tabs.length > 1) startTabDrag(event, tab.id, win);
    else overlay.dragWindow(event);
  };
  // The tab in flight may be this strip's, and target.index counts without it.
  const landsBefore = target && window.tabs.filter((tab) => tab.id !== target.tabId)[target.index]?.id;
  // Ctrl+wheel over the title bar zooms a board.
  const onWheel = (event) => { if (!event.ctrlKey) event.currentTarget.scrollLeft += event.deltaY; };
  return html`<div ref=${stripRef} class=${`companion-tabs overlay-tab-strip${target ? ' joining' : ''}`} role="tablist" aria-label="Tabs" data-tab-strip=${window.id} onWheel=${onWheel}>
    ${window.tabs.map((tab) => {
      const { label, lead, title } = viewOf(tab);
      const selected = tab.id === window.active;
      return html`<div key=${tab.id} class=${`companion-tab${selected ? ' selected' : ''}${landsBefore === tab.id ? ' drop-before' : ''}`} data-tab-id=${tab.id}>
        <button type="button" role="tab" aria-selected=${selected ? 'true' : 'false'} title=${title}
          onPointerDown=${(event) => onPointerDown(event, tab)} onClick=${(event) => (selected ? overlay.releaseFocus(event) : activateOverlayTab(tab.id))}>${lead}<span>${label}</span></button>
        <button type="button" class="companion-tab-close" aria-label=${`Minimize ${label}`} title="Minimize this tab (it keeps running)"
          onClick=${() => minimizeOverlayTab(tab.id)}>×</button>
      </div>`;
    })}
    ${target && !landsBefore && html`<span class="overlay-tab-drop-end" aria-hidden="true"></span>`}
  </div>`;
}
