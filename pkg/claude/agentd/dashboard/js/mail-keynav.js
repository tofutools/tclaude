// mail-keynav.js — ↑/↓ row navigation for the Messages tab's list panes.
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
//   Movement clamps at the ends of the rendered page; it never pages. Paging
//   is an explicit act with its own controls, and an arrow key that quietly
//   loaded the next page would let a held-down key walk the operator through a
//   whole mailbox — and, in the sidebar, through one folder fetch per row.

const STEPS = { ArrowUp: -1, ArrowDown: 1 };

// arrowStep reads a keydown as a one-row move, or 0 for anything else. Any
// modifier disqualifies the key: those combinations belong to the browser and
// the OS, and a pane with no equivalent of its own must not swallow them.
export function arrowStep(event) {
  if (!event || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return 0;
  return STEPS[event.key] || 0;
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

// moveRowSelection is a pane's whole keydown contract: read the key, find the
// neighbouring row, take the focus there, and select it. Returns true when the
// pane consumed the key.
export function moveRowSelection({ event, rowSelector, wrapSelector, select }) {
  const step = arrowStep(event);
  if (!step) return false;
  const rows = paneRows(event.currentTarget, rowSelector);
  if (!rows.length) return false;
  event.preventDefault();
  const target = stepRow(rows, anchorRow(rows, event.target, rowSelector, wrapSelector), step);
  if (!target) return true;
  focusRow(target);
  // Selection moves with the keyboard, so the pane has to follow it; 'nearest'
  // keeps the list from jumping when the row is already on screen.
  if (typeof target.scrollIntoView === 'function') target.scrollIntoView({ block: 'nearest' });
  select(target);
  return true;
}
