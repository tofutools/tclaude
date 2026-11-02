// tab-controls.js -- what the Terminals strip and an overlay window's strip share:
// a tab's right-click menu, and the box that renames a group of tabs.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { MAX_TERMINAL_GROUP_NAME_LENGTH } from './tab-group-model.js';

const html = htm.bind(h);

export const GROUP_COLLAPSE_CLICK_DELAY_MS = 200;

// useArmedCollapse lets a group's pill share a click and a double click. A click
// collapses and a double click renames, and collapsing moves the showing tab, so
// it can't fire on a double click's first click and be undone. A pointer click
// arms it on a short timer that a second click cancels. A keyboard click (detail
// 0) has no double click to wait for.
export function useArmedCollapse(toggle) {
  const collapseTimer = useRef(null);
  const clearCollapseTimer = () => {
    if (collapseTimer.current) {
      clearTimeout(collapseTimer.current);
      collapseTimer.current = null;
    }
  };
  useEffect(() => clearCollapseTimer, []);
  const onPillClick = (event) => {
    if (event.detail === 0) {
      toggle();
      return;
    }
    clearCollapseTimer();
    collapseTimer.current = setTimeout(() => {
      collapseTimer.current = null;
      toggle();
    }, GROUP_COLLAPSE_CLICK_DELAY_MS);
  };
  return { onPillClick, clearCollapseTimer };
}

// TabContextMenu is a menu at menu.x, menu.y, of what items({ item, separator })
// returns, named for menu.label.
export function TabContextMenu({ menu, items, closeMenu, focusAfterAction, focusAfterDismiss }) {
  const menuRef = useRef(null);

  useLayoutEffect(() => {
    const node = menuRef.current;
    if (!node) return;
    const rect = node.getBoundingClientRect();
    const viewportWidth = Number.isFinite(window.innerWidth) ? window.innerWidth : rect.right;
    const viewportHeight = Number.isFinite(window.innerHeight) ? window.innerHeight : rect.bottom;
    const left = Math.max(4, Math.min(menu.x, viewportWidth - rect.width - 4));
    const top = Math.max(4, Math.min(menu.y, viewportHeight - rect.height - 4));
    node.style.left = `${left}px`;
    node.style.top = `${top}px`;
    node.querySelector('[role="menuitem"]')?.focus();
  }, [menu]);

  useDismissablePopover(menuRef, true, (why) => closeMenu(why !== 'outside'));
  useEffect(() => {
    const onFocusIn = (event) => {
      if (!menuRef.current?.contains(event.target)) closeMenu(false);
    };
    document.addEventListener('focusin', onFocusIn);
    return () => document.removeEventListener('focusin', onFocusIn);
  }, [closeMenu]);

  const onMenuKeyDown = (event) => {
    if (event.key === 'Tab') {
      event.preventDefault();
      closeMenu(false);
      focusAfterDismiss(event.shiftKey);
      return;
    }
    const items = [...event.currentTarget.querySelectorAll('[role="menuitem"]')];
    const index = items.indexOf(document.activeElement);
    let next = null;
    if (event.key === 'ArrowDown') next = items[(index + 1) % items.length];
    else if (event.key === 'ArrowUp') next = items[(index - 1 + items.length) % items.length];
    else if (event.key === 'Home') next = items[0];
    else if (event.key === 'End') next = items.at(-1);
    if (!next) return;
    event.preventDefault();
    next.focus();
  };
  // Most commands hand focus back to the strip. The rename commands must not:
  // they hand focus to the rename input the stack is about to render, and the
  // parent's refocus would blur it — committing the edit before it is typed.
  const run = (action, { refocus = true } = {}) => {
    closeMenu(false);
    void action();
    if (refocus) focusAfterAction();
  };

  const item = (label, action, { className = 'mux-tab-menu-item', refocus = true } = {}) => html`
    <button type="button" role="menuitem" tabIndex="-1" class=${className} onClick=${() => run(action, { refocus })}>${label}</button>
  `;
  const separator = html`<div class="mux-tab-menu-separator" role="separator"></div>`;

  return html`
    <div
      ref=${menuRef}
      class="mux-tab-menu"
      role="menu"
      aria-label=${`Actions for ${menu.label}`}
      style=${{ left: `${menu.x}px`, top: `${menu.y}px` }}
      onKeyDown=${onMenuKeyDown}
    >${items({ item, separator })}</div>
  `;
}

// TabGroupRename is the group's name as a box to retype.
export function TabGroupRename({ group, label, onCommit, onCancel }) {
  const inputRef = useRef(null);
  // onBlur commits, so a cancel must first mark itself: browsers do not fire
  // blur when a focused node is removed today, but any future close that moves
  // focus before unmount would otherwise turn Escape into a commit. The ref is
  // the guard that keeps Escape meaning "discard" regardless of blur timing.
  const cancelledRef = useRef(false);
  useLayoutEffect(() => {
    inputRef.current?.focus();
    // Selecting the existing name makes retyping it the default gesture.
    // Guarded because the test DOM implements focus() but not select().
    if (typeof inputRef.current?.select === 'function') inputRef.current.select();
  }, []);
  const cancel = () => {
    cancelledRef.current = true;
    onCancel();
  };
  const commit = (value) => {
    if (!cancelledRef.current) onCommit(group.id, value);
  };
  return html`
    <input
      ref=${inputRef}
      class="mux-group-rename"
      type="text"
      maxLength=${MAX_TERMINAL_GROUP_NAME_LENGTH}
      aria-label=${label}
      value=${group.name}
      onKeyDown=${(event) => {
        event.stopPropagation();
        if (event.key === 'Enter') commit(event.currentTarget.value);
        else if (event.key === 'Escape') cancel();
      }}
      onBlur=${(event) => commit(event.currentTarget.value)}
    />
  `;
}
