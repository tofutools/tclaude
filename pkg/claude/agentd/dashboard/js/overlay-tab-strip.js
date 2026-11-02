// overlay-tab-strip.js -- a window's tabs, in its title bar. Even a window of one
// tab shows it, so there is one layout. That tab drags the window, as a browser's
// only tab does, and a window of several drags by the bar around them. A tab's ×
// minimizes that tab to the dock and stops nothing. Tabs group and take a
// right-click menu as a Terminals strip's do, and a menu rests under a held finger.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { signal } from '@preact/signals';
import { TabContextMenu, TabGroupRename, useArmedCollapse } from './tab-controls.js';
import { sanitizeGroupName, segmentsFor } from './tab-group-model.js';
import {
  activateOverlayTab, changeOverlayGroup, groupOverlayTab, minimizeOverlayTab, minimizeOverlayTabs, moveOverlayGroup, moveOverlayTab,
  overlayWindows, startOverlayGroup, tearOverlayGroupOut, tearOverlayTabOut, ungroupOverlayTabs,
} from './overlay-instances.js';
import { holdForMenu, startTabDrag, tabDropTarget, tabGhost } from './overlay-tab-drag.js';
import { groupsOf, windowHolding } from './overlay-tab-model.js';

const html = htm.bind(h);

// The open menu: { x, y, label, trigger } with the tabId or the windowId and
// groupId it is for. A strip goes when its window shows another tab, so these
// wait outside it.
const tabMenu = signal(null);
// The group being renamed: { windowId, groupId }.
const renaming = signal(null);

const groupMembers = (window, groupId) => window.tabs.filter((tab) => tab.groupId === groupId);

// The tab in flight. It stands apart from the strips, as it outlives leaving one.
export function OverlayTabGhost() {
  const ghost = tabGhost.value;
  return ghost && html`<div class="overlay-tab-ghost" style=${{ left: `${ghost.x}px`, top: `${ghost.y}px` }}>${ghost.label}</div>`;
}

function tabItems(menu, window, { item, separator }) {
  const { tabId } = menu;
  const tab = window.tabs.find((candidate) => candidate.id === tabId);
  const others = window.tabs.length > 1;
  return [
    item('New group from this tab', () => {
      const group = startOverlayGroup(tabId);
      if (group) renaming.value = { windowId: window.id, groupId: group.id };
    }, { refocus: false }),
    ...groupsOf(window).filter((group) => group.id !== tab.groupId).map((group) => item(`Add to “${group.name}”`, () => groupOverlayTab(tabId, group))),
    ...(tab.groupId ? [item('Remove from group', () => groupOverlayTab(tabId))] : []),
    separator,
    ...(others ? [item('Move to a new window', () => tearOverlayTabOut(tabId, null)), separator] : []),
    item('Minimize tab', () => minimizeOverlayTab(tabId)),
    ...(others ? [
      item('Minimize other tabs', () => minimizeOverlayTabs(window.id, (candidate) => candidate.id !== tabId)),
      item('Minimize all tabs', () => minimizeOverlayTabs(window.id, () => true)),
    ] : []),
  ];
}

function groupItems(menu, window, { item, separator }) {
  const { groupId } = menu;
  const group = window.groups?.find((candidate) => candidate.id === groupId);
  return [
    item(group.collapsed ? 'Expand group' : 'Collapse group', () => changeOverlayGroup(window.id, groupId, { collapsed: !group.collapsed })),
    item('Rename group…', () => { renaming.value = { windowId: window.id, groupId }; }, { refocus: false }),
    item('Ungroup tabs', () => ungroupOverlayTabs(window.id, groupId)),
    separator,
    item('Minimize tabs in group', () => minimizeOverlayTabs(window.id, (tab) => tab.groupId === groupId)),
  ];
}

const menuWindow = (menu) => {
  if (!menu) return null;
  const windows = overlayWindows.value;
  if (menu.tabId) return windowHolding(windows, menu.tabId);
  const window = windows.find((candidate) => candidate.id === menu.windowId);
  return window?.groups?.some((group) => group.id === menu.groupId) ? window : null;
};

// The strip's menu, shown while what it is for is there.
export function OverlayTabMenu() {
  const menu = tabMenu.value;
  const window = menuWindow(menu);
  useEffect(() => { if (menu && !window) tabMenu.value = null; }, [menu, window]);
  if (!window) return null;
  const close = (restoreFocus) => {
    tabMenu.value = null;
    if (restoreFocus) menu.trigger?.focus?.();
  };
  const focusBack = () => menu.trigger?.focus?.();
  return html`<${TabContextMenu} menu=${menu} closeMenu=${close} focusAfterAction=${() => {}} focusAfterDismiss=${focusBack}
    items=${(parts) => (menu.tabId ? tabItems(menu, window, parts) : groupItems(menu, window, parts))} />`;
}

const openMenu = (what, trigger, { x, y }) => { tabMenu.value = { ...what, trigger, x, y }; };

// A press from the keyboard has no point of its own, so it opens under the control.
const openFromKeys = (what, trigger) => {
  const rect = trigger.getBoundingClientRect();
  openMenu(what, trigger, { x: rect.left, y: rect.bottom });
};

const menuHandlers = (what) => ({
  onContextMenu: (event) => {
    event.preventDefault();
    openMenu(what, event.currentTarget, { x: event.clientX, y: event.clientY });
  },
  onKeyDown: (event) => {
    if (!(event.shiftKey && event.key === 'F10')) return;
    event.preventDefault();
    openFromKeys(what, event.currentTarget);
  },
});

function GroupStack({ window, group, overlay, win, renderTab }) {
  const members = groupMembers(window, group.id);
  const toggle = () => changeOverlayGroup(window.id, group.id, { collapsed: !group.collapsed });
  const { onPillClick, clearCollapseTimer } = useArmedCollapse(toggle);
  const where = { windowId: window.id, groupId: group.id, label: group.name };
  const startRename = () => {
    clearCollapseTimer();
    renaming.value = { windowId: window.id, groupId: group.id };
  };
  const stopRename = () => { renaming.value = null; };
  const onPointerDown = (event) => {
    if (event.button !== 0) return;
    const trigger = event.currentTarget;
    const cancel = members.length === window.tabs.length ? (overlay.dragWindow(event), overlay.cancelGesture) : startTabDrag(event, {
      ids: members.map((tab) => tab.id),
      label: group.name,
      land: (target) => moveOverlayGroup(window.id, group.id, target.windowId, target.index),
      tear: (at) => tearOverlayGroupOut(window.id, group.id, at),
    }, win);
    holdForMenu(event, win, (point) => openMenu(where, trigger, point), cancel);
  };
  const menu = menuHandlers(where);
  const onKeyDown = (event) => {
    if (event.key === 'F2') {
      event.preventDefault();
      startRename();
    } else menu.onKeyDown(event);
  };
  const isRenaming = renaming.value?.windowId === window.id && renaming.value?.groupId === group.id;
  const visible = group.collapsed ? members.filter((tab) => tab.id === window.active) : members;
  return html`<div class=${`mux-tab-group mux-group-${group.color} overlay-tab-group${group.collapsed ? ' collapsed' : ''}`} role="group"
    aria-label=${`${group.name}, ${members.length} tab${members.length === 1 ? '' : 's'}`} data-tab-group=${group.id}>
    ${isRenaming ? html`<${TabGroupRename} group=${group} label=${`Rename tab group ${group.name}`} onCancel=${stopRename}
      onCommit=${(id, value) => { stopRename(); changeOverlayGroup(window.id, id, { name: sanitizeGroupName(value, group.name) }); }} />` : html`
      <button type="button" class="mux-group-pill" aria-expanded=${group.collapsed ? 'false' : 'true'} aria-haspopup="menu"
        aria-label=${`${group.name}, ${members.length} tab${members.length === 1 ? '' : 's'}`}
        title=${`Drag to move the whole ${group.name} group · click to ${group.collapsed ? 'expand' : 'collapse'} · double-click or F2 to rename · right-click or press and hold for more`}
        onPointerDown=${onPointerDown} onClick=${onPillClick} onDblClick=${(event) => { event.preventDefault(); event.stopPropagation(); startRename(); }}
        onContextMenu=${menu.onContextMenu} onKeyDown=${onKeyDown}>
        <span class="mux-group-caret" aria-hidden="true">${group.collapsed ? '▸' : '▾'}</span>
        <span class="mux-group-name">${group.name}</span>
        <span class="mux-group-count" aria-hidden="true">${members.length}</span>
      </button>`}
    ${visible.map(renderTab)}
  </div>`;
}

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
    const trigger = event.currentTarget;
    const cancel = window.tabs.length > 1 ? startTabDrag(event, {
      ids: [tab.id],
      label: viewOf(tab).label,
      land: (where) => moveOverlayTab(tab.id, where.windowId, where.index, where.groupId),
      tear: (at) => tearOverlayTabOut(tab.id, at),
    }, win) : (overlay.dragWindow(event), overlay.cancelGesture);
    holdForMenu(event, win, (point) => openMenu({ tabId: tab.id, label: viewOf(tab).label }, trigger, point), cancel);
  };
  // The tabs in flight may be this strip's, and target.index counts without them.
  const landsBefore = target && window.tabs.filter((tab) => !target.ids.includes(tab.id))[target.index]?.id;
  // Ctrl+wheel over the title bar zooms a board.
  const onWheel = (event) => { if (!event.ctrlKey) event.currentTarget.scrollLeft += event.deltaY; };
  const renderTab = (tab) => {
    const { label, lead, title } = viewOf(tab);
    const selected = tab.id === window.active;
    const menu = menuHandlers({ tabId: tab.id, label });
    return html`<div key=${tab.id} class=${`companion-tab${selected ? ' selected' : ''}${landsBefore === tab.id ? ' drop-before' : ''}`} data-tab-id=${tab.id}>
      <button type="button" role="tab" aria-selected=${selected ? 'true' : 'false'} aria-haspopup="menu" title=${title}
        onPointerDown=${(event) => onPointerDown(event, tab)} onClick=${(event) => (selected ? overlay.releaseFocus(event) : activateOverlayTab(tab.id))}
        ...${menu}>${lead}<span>${label}</span></button>
      <button type="button" class="companion-tab-close" aria-label=${`Minimize ${label}`} title="Minimize this tab (it keeps running)"
        onClick=${() => minimizeOverlayTab(tab.id)}>×</button>
    </div>`;
  };
  const groups = new Map((window.groups || []).map((group) => [group.id, group]));
  const groupOf = new Map(window.tabs.map((tab) => [tab.id, tab.groupId]));
  const segments = segmentsFor(window.tabs.map((tab) => ({ ...tab, key: tab.id })), (key) => groupOf.get(key), groups);
  return html`<div ref=${stripRef} class=${`companion-tabs overlay-tab-strip${target ? ' joining' : ''}`} role="tablist" aria-label="Tabs" data-tab-strip=${window.id} onWheel=${onWheel}>
    ${segments.map((segment) => (segment.type === 'group'
    ? html`<${GroupStack} key=${segment.key} window=${window} group=${segment.group} overlay=${overlay} win=${win} renderTab=${renderTab} />`
    : renderTab(segment.pane)))}
    ${target && !landsBefore && html`<span class="overlay-tab-drop-end" aria-hidden="true"></span>`}
  </div>`;
}
