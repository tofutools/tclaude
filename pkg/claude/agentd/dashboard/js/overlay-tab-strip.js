// overlay-tab-strip.js -- a window's tabs, in its title bar. Even a window of one
// tab shows it, so there is one layout. That tab drags the window, as a browser's
// only tab does, and a window of several drags by the bar around them. A tab's ×
// minimizes that tab to the dock and stops nothing.

import { h } from 'preact';
import htm from 'htm';
import { activateOverlayTab, minimizeOverlayTab } from './overlay-instances.js';
import { startTabDrag, tabDropTarget } from './overlay-tab-drag.js';

const html = htm.bind(h);

export function OverlayTabStrip({ window, overlay, win, viewOf }) {
  const target = tabDropTarget.value?.windowId === window.id ? tabDropTarget.value : null;
  const onPointerDown = (event, tab) => {
    if (event.button !== 0) return;
    if (window.tabs.length > 1) startTabDrag(event, tab.id, win);
    else overlay.dragWindow(event);
  };
  // The tab in flight may be this strip's, and target.index counts without it.
  const landsBefore = target && window.tabs.filter((tab) => tab.id !== target.tabId)[target.index]?.id;
  return html`<div class=${`companion-tabs overlay-tab-strip${target ? ' joining' : ''}`} role="tablist" aria-label="Tabs" data-tab-strip=${window.id}>
    ${window.tabs.map((tab) => {
      const { label, lead, title } = viewOf(tab);
      const selected = tab.id === window.active;
      return html`<div key=${tab.id} class=${`companion-tab${selected ? ' selected' : ''}${landsBefore === tab.id ? ' drop-before' : ''}`} data-tab-id=${tab.id}>
        <button type="button" role="tab" aria-selected=${selected ? 'true' : 'false'} title=${title}
          onPointerDown=${(event) => onPointerDown(event, tab)} onClick=${() => activateOverlayTab(tab.id)}>${lead}<span>${label}</span></button>
        <button type="button" class="companion-tab-close" aria-label=${`Minimize ${label}`} title="Minimize this tab (it keeps running)"
          onClick=${() => minimizeOverlayTab(tab.id)}>×</button>
      </div>`;
    })}
    ${target && !landsBefore && html`<span class="overlay-tab-drop-end" aria-hidden="true"></span>`}
  </div>`;
}
