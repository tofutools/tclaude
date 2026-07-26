// mail-keynav.js — keyboard navigation for the Messages tab's list panes.
//
// The Messages tab reads like a desktop mail client, so it should be driven
// like one: open a row, then walk the pane with the arrow keys instead of
// going back to the mouse for every step. Both list panes behave the same way
// — the sidebar's folders and the message list's rows (stored mail and access
// requests alike) — so the traversal lives here once and each pane supplies
// only its row selector and what "select this row" means for it.
//
// Two deliberate boundaries:
//
//   Order comes from the DOM, not the model. What a pane shows is already the
//   product of its filter, its collapsed groups, and (in the access folder)
//   the pending/handled split. Rebuilding that order from the model would be a
//   second source of truth, free to drift from the one the operator sees.
//
//   Every row-navigation key clamps within the rendered page. Even PageDown is
//   a viewport-sized row move, not a request for another server page.

import { visiblePageSize } from './list-viewport.js';

const STEPS = { ArrowUp: -1, ArrowDown: 1 };

// arrowStep reads a keydown as a one-row move, or 0 for anything else. Any
// modifier disqualifies the key: those combinations belong to the browser and
// the OS, and a pane with no equivalent of its own must not swallow them.
export function arrowStep(event) {
  if (!plainKey(event)) return 0;
  return STEPS[event.key] || 0;
}

function plainKey(event) {
  return !!event && !event.isComposing && event.keyCode !== 229
    && !event.altKey && !event.ctrlKey && !event.metaKey && !event.shiftKey;
}

// rowEnabled keeps arrow keys away from what a click cannot reach either: a
// running bulk op disables every row until it finishes.
function rowEnabled(row) {
  return !row.disabled && row.getAttribute?.('disabled') == null;
}

// paneRows lists a pane's selectable rows in the order they are painted.
export function paneRows(container, rowSelector) {
  if (!container?.querySelectorAll) return [];
  return [...container.querySelectorAll(rowSelector)].filter(rowEnabled);
}

// anchorRow decides which row a move starts from: the row holding focus, or
// the row owning the focused control (a message row's checkbox and 🗑 sit
// beside its button, not inside it), else the selected row, else nothing.
export function anchorRow(rows, target, rowSelector, wrapSelector) {
  const focused = target?.closest?.(rowSelector);
  if (focused && rows.includes(focused)) return focused;
  const owner = wrapSelector ? target?.closest?.(wrapSelector)?.querySelector(rowSelector) : null;
  if (owner && rows.includes(owner)) return owner;
  return rows.find((row) => row.classList.contains('active')) || null;
}

// stepRow resolves the destination row. With no anchor the move enters the
// pane from the end it came from. At either end it returns null — the caller
// still reports the key as handled, so the pane holds still instead of
// scrolling away from the row the operator is reading.
export function stepRow(rows, anchor, step) {
  if (!rows.length || !step) return null;
  if (!anchor) return step > 0 ? rows[0] : rows[rows.length - 1];
  const next = rows.indexOf(anchor) + step;
  return next >= 0 && next < rows.length ? rows[next] : null;
}

// focusRow gives a row keyboard focus. Panes call this from their click
// handlers too, because a click does not focus a <button> on every platform
// (macOS is the notable exception) — without it "click a message, then arrow
// to the next one" would work on Linux and be dead on a Mac.
export function focusRow(row) {
  if (typeof row?.focus === 'function') row.focus();
}

function revealRow(row) {
  if (typeof row?.scrollIntoView === 'function') row.scrollIntoView({ block: 'nearest' });
}

function activateRow(row, select) {
  focusRow(row);
  revealRow(row);
  select?.(row);
}

function navigationTarget(event, rows, anchor, rowSelector) {
  switch (event.key) {
    case 'ArrowUp':
    case 'ArrowDown':
      return stepRow(rows, anchor, STEPS[event.key]);
    case 'Home':
      return rows[0];
    case 'End':
      return rows[rows.length - 1];
    case 'PageUp':
    case 'PageDown': {
      if (!anchor) return event.key === 'PageDown' ? rows[0] : rows[rows.length - 1];
      const direction = event.key === 'PageDown' ? 1 : -1;
      const next = rows.indexOf(anchor)
        + direction * visiblePageSize(event.currentTarget, rowSelector);
      return rows[Math.max(0, Math.min(rows.length - 1, next))];
    }
    default:
      return undefined;
  }
}

// moveRowSelection handles ↑/↓, Home/End, and viewport-sized PageUp/PageDown.
// All of them remain page-local. Returns true when the pane consumed the key.
export function moveRowSelection({ event, rowSelector, wrapSelector, select }) {
  if (!plainKey(event)) return false;
  const rows = paneRows(event.currentTarget, rowSelector);
  if (!rows.length) return false;
  const anchor = anchorRow(rows, event.target, rowSelector, wrapSelector);
  const target = navigationTarget(event, rows, anchor, rowSelector);
  if (target === undefined) return false;
  event.preventDefault();
  if (!target) return true;
  activateRow(target, target === anchor ? undefined : select);
  return true;
}

// enterFirstRow connects a filter to the rendered results below it.
export function enterFirstRow({ event, container, rowSelector, select }) {
  if (!plainKey(event) || event.key !== 'ArrowDown') return false;
  const first = paneRows(container, rowSelector)[0];
  if (!first) return false;
  event.preventDefault();
  activateRow(first, select);
  return true;
}

// leaveFirstRow returns from the first rendered result to the pane's filter.
// Escape is intentionally scoped to the first row too: deeper in the list it
// remains available to future message-level interactions.
export function leaveFirstRow({ event, rowSelector, wrapSelector, filter }) {
  if (!plainKey(event) || !['ArrowUp', 'Escape'].includes(event.key)) return false;
  const rows = paneRows(event.currentTarget, rowSelector);
  if (!rows.length) return false;
  if (anchorRow(rows, event.target, rowSelector, wrapSelector) !== rows[0]) return false;
  event.preventDefault();
  filter?.focus?.();
  return true;
}

// focusPaneRows enters another pane on its selected row, falling back to the
// first painted row when the pane has no visible selection.
export function focusPaneRows(container, rowSelector) {
  const rows = paneRows(container, rowSelector);
  const target = rows.find((row) => row.classList.contains('active')) || rows[0];
  if (!target) return false;
  focusRow(target);
  revealRow(target);
  return true;
}
