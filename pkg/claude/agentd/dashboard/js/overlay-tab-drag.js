// overlay-tab-drag.js -- dragging a tab of a window with several (tcl-e4i44z):
// along its strip to reorder it, onto another window to join that window, or off
// the strip for a window of its own. It runs on pointer events, unlike the
// Terminals strip's HTML5 drag, which touch screens do not reliably start.

import { signal } from '@preact/signals';
import { dropIndex, pulledOff } from './overlay-tab-model.js';
import { moveOverlayTab, tearOverlayTabOut } from './overlay-instances.js';
import { droppedAt } from './overlay-window-model.js';

const DRAG_START_PX = 5;
const TEAR_OFF_PX = 40;

// Where the tab in flight would land: { tabId, windowId, index }, index counting
// the tabs there without it. Otherwise null.
export const tabDropTarget = signal(null);

// The tab following the pointer: { label, x, y }. Otherwise null.
export const tabGhost = signal(null);

const windowOf = (element) => element.closest?.('section')?.querySelector?.('[data-tab-strip]')?.dataset?.tabStrip || '';

// The strip of the title bar under a point, if the window on top there has one.
// A window being dragged is under the pointer itself, so it looks through that,
// and through the resize frames, which sit beside the windows and over their edges.
function stripAt(doc, { x, y }, through) {
  const top = [...(doc?.elementsFromPoint?.(x, y) || [])].find((element) => !element.closest?.('.window-resize-frame') && (!through || windowOf(element) !== through));
  return top?.closest?.('header')?.querySelector?.('[data-tab-strip]') || null;
}

function targetAt(doc, point, tabId, through = '') {
  const strip = stripAt(doc, point, through);
  if (!strip) return null;
  const spans = [...strip.querySelectorAll('[data-tab-id]')].filter((node) => node.dataset.tabId !== tabId)
    .map((node) => node.getBoundingClientRect());
  return { tabId, windowId: strip.dataset.tabStrip, index: dropIndex(spans, point.x) };
}

const sameTarget = (a, b) => a?.windowId === b?.windowId && a?.index === b?.index;

function aim(next) {
  if (!sameTarget(tabDropTarget.peek(), next)) tabDropTarget.value = next;
}

// tabJoin lets a window of one tab, dragged by its title bar or tab, drop into
// another window's strip. over and drop take the pointer, and say whether it is
// over a strip.
export function tabJoin(win, tabId, windowId) {
  const over = (point) => {
    const target = targetAt(win?.document, point, tabId, windowId);
    aim(target);
    return !!target;
  };
  const drop = () => {
    const target = tabDropTarget.peek();
    aim(null);
    if (target) moveOverlayTab(tabId, target.windowId, target.index);
    return !!target;
  };
  return { over, drop, cancel: () => aim(null) };
}

// startTabDrag follows a press on a tab. Anything shorter than a drag stays a
// click, and Escape or a cancelled pointer leaves everything where it was. A
// finger swiping along a strip too full to show every tab scrolls it instead,
// since a tab keeps the browser from doing that.
export function startTabDrag(event, tabId, win) {
  const doc = win?.document;
  const node = event.currentTarget;
  const strip = node.closest?.('[data-tab-strip]');
  const { pointerId, clientX: sx, clientY: sy } = event;
  const scrolledFrom = strip?.scrollLeft;
  let dragging = false;
  let swiping = false;
  const point = (m) => ({ x: m.clientX, y: m.clientY });
  const stop = () => {
    win?.removeEventListener?.('pointermove', move);
    win?.removeEventListener?.('pointerup', up);
    win?.removeEventListener?.('pointercancel', cancel);
    win?.removeEventListener?.('keydown', onKey, true);
    tabGhost.value = null;
    aim(null);
  };
  const move = (m) => {
    if (m.pointerId !== pointerId) return;
    const dx = m.clientX - sx;
    const dy = m.clientY - sy;
    if (!dragging && !swiping) {
      if (Math.hypot(dx, dy) < DRAG_START_PX) return;
      swiping = m.pointerType === 'touch' && Math.abs(dx) > Math.abs(dy) && strip?.scrollWidth > strip?.clientWidth;
    }
    if (swiping) {
      strip.scrollLeft = scrolledFrom - dx;
      return;
    }
    if (!dragging) {
      dragging = true;
      node.closest?.('[data-tab-id]')?.classList?.add?.('dragging');
    }
    tabGhost.value = { label: node.textContent.trim(), x: m.clientX + 8, y: m.clientY + 8 };
    aim(targetAt(doc, point(m), tabId));
  };
  const up = (u) => {
    if (u.pointerId !== pointerId) return;
    const dragged = dragging;
    const target = dragged ? targetAt(doc, point(u), tabId) : null;
    const pressHeld = !dragged && !swiping;
    const rect = strip?.getBoundingClientRect?.();
    node.closest?.('[data-tab-id]')?.classList?.remove?.('dragging');
    stop();
    if (pressHeld) return;
    // A release back on the tab clicks it, which would choose it again.
    const swallow = (click) => { click.preventDefault(); click.stopPropagation(); };
    node.addEventListener('click', swallow, { capture: true, once: true });
    setTimeout(() => node.removeEventListener('click', swallow, { capture: true }), 0);
    if (target) moveOverlayTab(tabId, target.windowId, target.index);
    else if (dragged && rect && pulledOff(rect, point(u), TEAR_OFF_PX)) tearOverlayTabOut(tabId, droppedAt(u));
  };
  const cancel = (c) => {
    if (c.pointerId !== pointerId) return;
    node.closest?.('[data-tab-id]')?.classList?.remove?.('dragging');
    stop();
  };
  const onKey = (key) => {
    if (key.key !== 'Escape' || !dragging) return;
    key.preventDefault();
    key.stopImmediatePropagation();
    node.closest?.('[data-tab-id]')?.classList?.remove?.('dragging');
    stop();
  };
  win?.addEventListener?.('pointermove', move);
  win?.addEventListener?.('pointerup', up);
  win?.addEventListener?.('pointercancel', cancel);
  win?.addEventListener?.('keydown', onKey, true);
}
