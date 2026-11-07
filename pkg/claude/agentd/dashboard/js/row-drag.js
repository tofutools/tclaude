// row-drag.js -- how a table row in the Groups view drags. An agent row
// (dnd.js), a resource row (resource-dnd.js) and a task row in the task
// board's List (groups-taskboard.js) are each draggable="true" as a whole,
// show the grab cursor (dashboard.css), and keep their controls clickable
// through this one guard.
//
// The whole <tr> is draggable, so a press on an in-row control (a button,
// the ⚙ cog and its menu items, a click-to-edit cell, a field) plus even a
// 1px cursor wobble makes the browser start dragging the ROW instead of
// firing that control's click. That cannot be fixed in dragstart: native
// DnD fires dragstart at the drag SOURCE NODE (the <tr>), so e.target there
// is always the row, never the control that was pressed. pointerdown, by
// contrast, targets the element under the cursor, and fires BEFORE the drag
// is initiated. So when the press lands on an interactive descendant, the
// row's draggable goes OFF for the gesture and the click lands; a press on
// a plain cell leaves it ON, so the row still drags.
//
// The suppression is strictly gesture-scoped: pointerup / pointercancel
// restore draggability at once, so a row is never left un-draggable between
// gestures. That is safe: the drag-vs-click decision is made during the move
// BEFORE pointerup. Only the row this guard turned off is turned back on.

// ROW_DRAG_CONTROLS are the elements a press on is a click, not a drag.
export const ROW_DRAG_CONTROLS = 'button, a, input, select, textarea, label, [data-act], [contenteditable], [role="button"]';

// bindRowDragGuard guards the rows matching rowSelector on doc. It returns
// { unbind, restore }: restore turns a row it suppressed back on (a drag's
// end does it too).
export function bindRowDragGuard(doc, rowSelector) {
  let suppressed = null;
  const restore = () => {
    if (!suppressed) return;
    suppressed.draggable = true;
    suppressed = null;
  };
  const down = (e) => {
    const row = e.target?.closest?.(rowSelector);
    if (!row) return;
    const control = e.target.closest(ROW_DRAG_CONTROLS);
    if (!control || !row.contains(control)) return;
    restore();
    row.draggable = false;
    suppressed = row;
  };
  doc.addEventListener('pointerdown', down);
  doc.addEventListener('pointerup', restore);
  doc.addEventListener('pointercancel', restore);
  const unbind = () => {
    doc.removeEventListener('pointerdown', down);
    doc.removeEventListener('pointerup', restore);
    doc.removeEventListener('pointercancel', restore);
  };
  return { unbind, restore };
}

// bindTaskRowDrag guards the task board List's rows (groups-taskboard.js
// marks a draggable one data-dnd-task); dashboard.js binds it with the agent
// and resource rows' drags. It returns the unbind.
export function bindTaskRowDrag(doc = globalThis.document) {
  return bindRowDragGuard(doc, 'tr[data-dnd-task]').unbind;
}
