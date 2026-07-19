// process-template-dnd.js — drag a process template row onto the overlay bin
// to delete it (the drag twin of the row's trash button in processes-island.js).
//
// The Processes tab's template rows carry data-process-template-drag +
// draggable (see processes-island.js). Dragging one shows the shared #dnd-trash
// overlay; dropping on it runs the SAME actions.deleteTemplate() commit the row
// button uses, so the confirm copy, the in-use handling and the list refresh
// cannot drift between the two affordances. There is no reorder gesture here —
// the bin is the only target, which is why this module is much smaller than
// group-reorder.js.
//
// Isolation from the other DnD modules (dnd.js member rows, group-reorder.js,
// dock-dnd.js) follows the same discipline they use on each other: this drag
// sets ONLY a custom MIME ('application/x-tclaude-process-template') and never
// text/plain, every handler self-gates on the module-local templateDragActive
// flag, and dnd.js's drop handler bails on this MIME up front rather than
// relying on its JSON.parse happening to fail.
//
// The island re-renders rows on every templates refresh, so binding is document
// -level delegation keyed on the attribute — a reconciled row stays draggable
// without rebinding.

import { $, $$ } from './helpers.js';
import { isWizardActive } from './slop.js';

// Custom drag payload type. Intentionally NOT 'text/plain' — see the module
// header: withholding text/plain is what keeps dnd.js's member-row drop handler
// out of this gesture.
const TEMPLATE_DRAG_MIME = 'application/x-tclaude-process-template';

// Live gesture state. dragover cannot read the DataTransfer payload (browsers
// gate getData to the drop event), so the dragged row is stashed here for the
// pill text and the drop commit.
let templateDragActive = false;
let templateDragRow = null;
let templateDragSpec = null;

// deleteHandler is supplied by the Processes island, which owns the actions
// object. Kept as a module-level slot rather than an import so this module has
// no dependency on island construction order; a drop before the island
// registers simply does nothing.
let deleteHandler = null;

export function setProcessTemplateDeleteHandler(handler) {
  deleteHandler = typeof handler === 'function' ? handler : null;
}

function trashTarget(e) {
  return e.target.closest('#dnd-trash');
}

// dragPill drives the shared #dnd-pill cursor label, matching group-reorder.js's
// handling so the three drag modules present one consistent cursor affordance.
function dragPill(e, text) {
  const pill = $('#dnd-pill');
  if (!pill) return;
  if (!text) {
    pill.classList.remove('show', 'clone');
    return;
  }
  pill.textContent = text;
  pill.classList.remove('clone');
  pill.classList.add('show');
  pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
}

// endTemplateDrag is the single teardown. Idempotent, so calling it from both
// the drop handler and dragend is safe.
function endTemplateDrag() {
  // Clear the flag FIRST (mirrors dnd.js / group-reorder.js) so later document
  // events cannot be misrouted even if a DOM call below were to throw.
  templateDragActive = false;
  templateDragSpec = null;
  templateDragRow?.removeEventListener('dragend', endTemplateDrag);
  templateDragRow = null;
  $$('.process-template-drag-source').forEach((row) => row.classList.remove('process-template-drag-source'));
  const trash = $('#dnd-trash');
  if (trash) trash.classList.remove('show', 'dnd-drop-over', 'dnd-trash-template-mode');
  dragPill(null, null);
}

function bindProcessTemplateDnd() {
  const removers = [];
  const listen = (target, type, listener, options) => {
    target.addEventListener(type, listener, options);
    removers.push(() => target.removeEventListener(type, listener, options));
  };

  // Gesture-scoped draggable suppression (mirrors group-reorder.js). The whole
  // row is draggable, but a press landing on the inline rename button, the
  // action buttons or the trash button must produce that control's CLICK, not a
  // drag — once a native drag starts the click is gone for good. pointerdown
  // fires before the drag is initiated, so it is the right place to decide.
  let suppressedRow = null;
  const restoreRowDraggable = () => {
    if (!suppressedRow) return;
    suppressedRow.draggable = true;
    suppressedRow = null;
  };
  listen(document, 'pointerdown', (e) => {
    const row = e.target.closest('[data-process-template-drag]');
    if (!row) return;
    const ctl = e.target.closest('button, a, input, select, textarea, label, [contenteditable]');
    if (ctl && row.contains(ctl)) {
      row.draggable = false;
      suppressedRow = row;
    }
  });
  listen(document, 'pointerup', restoreRowDraggable);
  listen(document, 'pointercancel', restoreRowDraggable);

  listen(document, 'dragstart', (e) => {
    const row = e.target.closest('[data-process-template-drag]');
    if (!row) return;
    const id = row.getAttribute('data-process-template-drag');
    if (!id) return;
    templateDragActive = true;
    templateDragSpec = {
      id,
      name: row.getAttribute('data-process-template-name') || '',
      versionCount: Number(row.getAttribute('data-process-template-versions')) || 0,
    };
    templateDragRow?.removeEventListener('dragend', endTemplateDrag);
    templateDragRow = row;
    row.addEventListener('dragend', endTemplateDrag, { once: true });
    // Custom MIME ONLY — see the module header. The gesture deletes, so
    // effectAllowed/dropEffect stay 'move' rather than 'copy'.
    e.dataTransfer.setData(TEMPLATE_DRAG_MIME, id);
    e.dataTransfer.effectAllowed = 'move';
    row.classList.add('process-template-drag-source');
    const trash = $('#dnd-trash');
    // template-mode swaps the bin's label from the agent voice (Retire/Banish)
    // to the template voice (Delete/Unmake) for this gesture only.
    if (trash) trash.classList.add('show', 'dnd-trash-template-mode');
  });

  // dragend is the guaranteed reset for a CANCELLED or no-target drag (Escape,
  // or a release over nothing). A successful drop tears down in the drop
  // handler before the island re-renders the list.
  //
  // Self-gate like every other handler here: dragend BUBBLES, so without this
  // check another module's drag ending (a member row, a group header, a dock
  // card) would run this teardown and clear the shared #dnd-trash / #dnd-pill
  // out from under it.
  listen(document, 'dragend', () => { if (templateDragActive) endTemplateDrag(); });

  listen(document, 'dragover', (e) => {
    if (!templateDragActive) return;
    const trash = trashTarget(e);
    if (!trash) {
      dragPill(e, null);
      return;
    }
    e.preventDefault(); // required for `drop` to fire on this element
    e.dataTransfer.dropEffect = 'move';
    trash.classList.add('dnd-drop-over');
    const label = templateDragSpec?.name || templateDragSpec?.id || '';
    dragPill(e, isWizardActive() ? `↓ unmake rite ${label}` : `↓ delete template ${label}`);
  });

  listen(document, 'dragleave', (e) => {
    if (!templateDragActive) return;
    const trash = trashTarget(e);
    if (!trash) return;
    if (trash.contains(e.relatedTarget)) return;
    trash.classList.remove('dnd-drop-over');
  });

  listen(document, 'drop', (e) => {
    if (!templateDragActive) return;
    const trash = trashTarget(e);
    if (!trash) return;
    e.preventDefault();
    // Snapshot the target BEFORE teardown: endTemplateDrag clears the state,
    // and the confirm dialog resolves long after this handler returns.
    const spec = templateDragSpec;
    endTemplateDrag();
    if (spec && deleteHandler) void deleteHandler(spec);
  });

  // Tear down any drag still in flight BEFORE dropping the listeners: unbinding
  // mid-gesture would otherwise strand the shared bin with .show +
  // .dnd-trash-template-mode (so the next agent retire-drag would read
  // "Delete"/"Unmake") and leak a reference to the detached row.
  return () => {
    if (templateDragActive) endTemplateDrag();
    removers.forEach((remove) => remove());
  };
}

export { bindProcessTemplateDnd, TEMPLATE_DRAG_MIME };
