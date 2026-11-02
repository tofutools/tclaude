// overlay-tab-drag.js -- dragging a tab of a window with several (tcl-e4i44z):
// along its strip to reorder it, onto another window to join that window, or off
// the strip for a window of its own. A group's pill drags the whole group the same
// way. It runs on pointer events, unlike the Terminals strip's HTML5 drag, which
// touch screens do not reliably start.
//
// The Collab strip is one more place to drop on (tcl-0mqn7r): a tab lands there
// beside the tab under the pointer, and the main nav's Collab entry takes it at
// the end, switching to that view when the pointer rests on it. A tab of that
// strip, dragged the other way, shows where it would land on a window's strip.

import { signal } from '@preact/signals';
import { dropIndex, pulledOff } from './overlay-tab-model.js';
import { moveOverlayTab, overlayWindows, tearOverlayTabOut } from './overlay-instances.js';
import { droppedAt } from './overlay-window-model.js';
import { moveTabsToCollab } from './overlay-tab-moves.js';

const DRAG_START_PX = 5;
const TEAR_OFF_PX = 40;
const HOLD_MS = 500;
// How long the pointer rests on the nav's Collab entry before the view switches.
export const NAV_SWITCH_MS = 600;
const COLLAB_NAV = 'nav [data-tab="terminals"]';

// Where the tabs in flight would land: { ids, windowId, index, groupId }, index
// counting the tabs there without them, and groupId the group they were dropped
// inside. Otherwise null.
export const tabDropTarget = signal(null);

// The tab following the pointer: { label, x, y }. Otherwise null.
export const tabGhost = signal(null);

// Where overlay tabs in flight would land in the Collab view: { beside, after }
// on its strip, beside the pane keyed beside or at the end with none, or { nav }
// on the main nav's entry. Otherwise null.
export const collabDropTarget = signal(null);

// The Collab tab in flight toward the windows: { key, floats }, floats saying
// whether it can be in one. Otherwise null.
export const collabDrag = signal(null);

const windowOf = (element) => element.closest?.('section')?.querySelector?.('[data-tab-strip]')?.dataset?.tabStrip || '';

// The strip of the title bar under a point, if the window on top there has one.
// A window being dragged is under the pointer itself, so it looks through that,
// and through the resize frames, which sit beside the windows and over their edges.
function stripAt(doc, { x, y }, through) {
  const top = [...(doc?.elementsFromPoint?.(x, y) || [])].find((element) => !element.closest?.('.window-resize-frame') && (!through || windowOf(element) !== through));
  const strip = top?.closest?.('header')?.querySelector?.('[data-tab-strip]') || null;
  return strip && { strip, groupId: top.closest?.('[data-tab-group]')?.dataset?.tabGroup || '' };
}

function targetAt(doc, point, ids, through = '') {
  const found = stripAt(doc, point, through);
  if (!found) return null;
  const { strip, groupId } = found;
  const nodes = [...strip.querySelectorAll('[data-tab-id]')].filter((node) => !ids.includes(node.dataset.tabId));
  const beside = nodes[dropIndex(nodes.map((node) => node.getBoundingClientRect()), point.x)];
  // A collapsed group's tabs are not drawn, though they count.
  const windowId = strip.dataset.tabStrip;
  const tabs = (overlayWindows.peek().find((window) => window.id === windowId)?.tabs || []).filter((tab) => !ids.includes(tab.id));
  return { ids, windowId, index: beside ? tabs.findIndex((tab) => tab.id === beside.dataset.tabId) : tabs.length, groupId };
}

// Where on the Collab view a point is: its strip, beside the nearest tab, or the
// nav's entry for it. The strip only counts while its view shows.
function collabAt(doc, { x, y }, through) {
  const top = [...(doc?.elementsFromPoint?.(x, y) || [])].find((element) => !element.closest?.('.window-resize-frame') && (!through || windowOf(element) !== through));
  if (top?.closest?.(COLLAB_NAV)) return { nav: true };
  const strip = top?.closest?.('[data-collab-strip]');
  if (!strip) return null;
  const nodes = [...strip.querySelectorAll('.mux-tab[data-pane-key]')];
  if (!nodes.length) return { beside: '', after: false };
  const at = dropIndex(nodes.map((node) => node.getBoundingClientRect()), x);
  return at < nodes.length
    ? { beside: nodes[at].dataset.paneKey, after: false }
    : { beside: nodes[nodes.length - 1].dataset.paneKey, after: true };
}

const sameTarget = (a, b) => a?.windowId === b?.windowId && a?.index === b?.index && a?.groupId === b?.groupId;
const sameCollab = (a, b) => a?.nav === b?.nav && a?.beside === b?.beside && a?.after === b?.after;

// Resting on the nav's entry switches to the Collab view, as a browser's tab bar
// does under a dragged link.
let navTimer = null;
function restOnNav(doc, on) {
  const nav = doc?.querySelector?.(COLLAB_NAV);
  nav?.classList?.toggle?.('overlay-drop', on);
  if (!on) {
    clearTimeout(navTimer);
    navTimer = null;
  } else if (!navTimer) navTimer = setTimeout(() => { navTimer = null; nav?.click?.(); }, NAV_SWITCH_MS);
}

function aimCollab(next, doc) {
  if (!sameCollab(collabDropTarget.peek(), next)) collabDropTarget.value = next;
  restOnNav(doc, !!next?.nav);
}

function aim(next, doc = null, collab = null) {
  if (!sameTarget(tabDropTarget.peek(), next)) tabDropTarget.value = next;
  aimCollab(next ? null : collab, doc);
}

// stripTargetAt is where tabs land on an overlay strip, given an HTML5 drag from
// the Collab strip: ids is empty, as none of them are there yet.
export function stripTargetAt(doc, point) {
  return targetAt(doc, point, []);
}

// aimFromCollab shows where a Collab tab would land, or nothing.
export function aimFromCollab(target) {
  if (!sameTarget(tabDropTarget.peek(), target)) tabDropTarget.value = target;
}

// tabJoin lets a window of one tab, dragged by its title bar or tab, drop into
// another window's strip. over and drop take the pointer, and say whether it is
// over a strip.
export function tabJoin(win, tabId, windowId) {
  const doc = win?.document;
  const over = (point) => {
    const target = targetAt(doc, point, [tabId], windowId);
    const collab = target ? null : collabAt(doc, point, windowId);
    aim(target, doc, collab);
    return !!(target || collab);
  };
  const drop = () => {
    const target = tabDropTarget.peek();
    const collab = collabDropTarget.peek();
    aim(null, doc);
    if (target) moveOverlayTab(tabId, target.windowId, target.index, target.groupId);
    else if (collab) void moveTabsToCollab([tabId], collab);
    return !!(target || collab);
  };
  return { over, drop, cancel: () => aim(null, doc) };
}

// A release back on what was pressed clicks it, which would act on it again.
function swallowClick(node) {
  const swallow = (click) => { click.preventDefault(); click.stopPropagation(); };
  node.addEventListener('click', swallow, { capture: true, once: true });
  setTimeout(() => node.removeEventListener('click', swallow, { capture: true }), 0);
}

// quietLift stops the mouse press and click a browser makes of a finger lifting,
// which would take focus from the menu that hold opened, and so close it.
function quietLift(node, win) {
  const quiet = (touch) => touch.preventDefault();
  node.addEventListener('touchend', quiet, { once: true, passive: false });
  const done = () => {
    win?.removeEventListener?.('pointerup', done);
    win?.removeEventListener?.('pointercancel', done);
    setTimeout(() => node.removeEventListener('touchend', quiet), 0);
  };
  win?.addEventListener?.('pointerup', done);
  win?.addEventListener?.('pointercancel', done);
}

// holdForMenu opens a menu when a finger rests on a control, where there is no
// right click. onHold takes the point, and cancel ends the drag the press began.
export function holdForMenu(event, win, onHold, cancel) {
  if (event.pointerType !== 'touch') return;
  const node = event.currentTarget;
  const { pointerId, clientX: sx, clientY: sy } = event;
  const stop = () => {
    clearTimeout(timer);
    win?.removeEventListener?.('pointermove', move);
    win?.removeEventListener?.('pointerup', stop);
    win?.removeEventListener?.('pointercancel', stop);
  };
  const move = (m) => {
    if (m.pointerId === pointerId && Math.hypot(m.clientX - sx, m.clientY - sy) >= DRAG_START_PX) stop();
  };
  const timer = setTimeout(() => {
    stop();
    cancel?.();
    quietLift(node, win);
    onHold({ x: sx, y: sy });
  }, HOLD_MS);
  win?.addEventListener?.('pointermove', move);
  win?.addEventListener?.('pointerup', stop);
  win?.addEventListener?.('pointercancel', stop);
}

// startTabDrag follows a press on a tab or a group's pill: item is { ids, label,
// land, tear }, the tabs that go and what to do with them. Dropped on the Collab
// view, they go there. Anything shorter than a drag
// stays a click, and Escape, a cancelled pointer or what it returns leaves
// everything where it was. A finger swiping along a strip too full to show every
// tab scrolls it instead, since a tab keeps the browser from doing that.
export function startTabDrag(event, { ids, label, land, tear }, win) {
  const doc = win?.document;
  const node = event.currentTarget;
  const strip = node.closest?.('[data-tab-strip]');
  const held = node.closest?.('[data-tab-id], [data-tab-group]');
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
    aim(null, doc);
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
      held?.classList?.add?.('dragging');
    }
    tabGhost.value = { label, x: m.clientX + 8, y: m.clientY + 8 };
    const target = targetAt(doc, point(m), ids);
    aim(target, doc, target ? null : collabAt(doc, point(m)));
  };
  const up = (u) => {
    if (u.pointerId !== pointerId) return;
    const dragged = dragging;
    const target = dragged ? targetAt(doc, point(u), ids) : null;
    const collab = dragged && !target ? collabAt(doc, point(u)) : null;
    const pressHeld = !dragged && !swiping;
    const rect = strip?.getBoundingClientRect?.();
    end();
    if (pressHeld) return;
    swallowClick(node);
    if (target) land(target);
    else if (collab) void moveTabsToCollab(ids, collab);
    else if (dragged && rect && pulledOff(rect, point(u), TEAR_OFF_PX)) tear(droppedAt(u));
  };
  const end = () => {
    held?.classList?.remove?.('dragging');
    stop();
  };
  const cancel = (c) => {
    if (c.pointerId === pointerId) end();
  };
  const onKey = (key) => {
    if (key.key !== 'Escape' || !dragging) return;
    key.preventDefault();
    key.stopImmediatePropagation();
    end();
  };
  win?.addEventListener?.('pointermove', move);
  win?.addEventListener?.('pointerup', up);
  win?.addEventListener?.('pointercancel', cancel);
  win?.addEventListener?.('keydown', onKey, true);
  return end;
}
