// groups-saved-process-dnd.js — drag a group's saved-process chip
// (groups-saved-processes.js) onto the shared #dnd-trash bin to remove the
// process from that group: the drag twin of the chip's × button. The
// template itself stays; only the group's pin goes.
//
// Same isolation discipline as process-template-dnd.js: the drag sets ONLY a
// custom MIME (never text/plain), every handler self-gates on the
// module-local active flag, and dnd.js's drop handler bails on the MIME.
//
// Chips render once per group, so binding is document-level delegation keyed
// on data-saved-process-drag, refcounted across the mounted chip rows. Each
// group's row registers the remover the drop calls for its chips.

import { $, $$ } from './helpers.js';
import { isWizardActive } from './slop.js';

export const SAVED_PROCESS_DRAG_MIME = 'application/x-tclaude-saved-process';

const removers = new Map();
let active = false;
let spec = null;
let source = null;
let unbind = null;
let bound = 0;

// registerSavedProcessRemover routes drops of `group`'s chips to remove(id).
export function registerSavedProcessRemover(group, remove) {
  removers.set(group, remove);
  return () => { if (removers.get(group) === remove) removers.delete(group); };
}

function dragPill(e, text) {
  const pill = $('#dnd-pill');
  if (!pill) return;
  if (!text) {
    pill.classList.remove('show', 'clone');
    return;
  }
  if (pill.textContent !== text) pill.textContent = text;
  pill.classList.remove('clone');
  pill.classList.add('show');
  pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
}

function endDrag() {
  active = false;
  spec = null;
  source?.removeEventListener('dragend', endDrag);
  source = null;
  $$('.gq-saved-drag-source').forEach((chip) => chip.classList.remove('gq-saved-drag-source'));
  const trash = $('#dnd-trash');
  if (trash) trash.classList.remove('show', 'dnd-drop-over', 'dnd-trash-unpin-mode');
  dragPill(null, null);
}

function bind() {
  const off = [];
  const listen = (type, listener) => {
    document.addEventListener(type, listener);
    off.push(() => document.removeEventListener(type, listener));
  };
  // A press on the chip's buttons must stay a click, not start a drag.
  let suppressed = null;
  const restore = () => {
    if (!suppressed) return;
    suppressed.draggable = true;
    suppressed = null;
  };
  listen('pointerdown', (e) => {
    const chip = e.target.closest?.('[data-saved-process-drag]');
    if (!chip || !e.target.closest('button')) return;
    chip.draggable = false;
    suppressed = chip;
  });
  listen('pointerup', restore);
  listen('pointercancel', restore);
  const trashTarget = (e) => e.target.closest?.('#dnd-trash');
  listen('dragstart', (e) => {
    const chip = e.target.closest?.('[data-saved-process-drag]');
    if (!chip) return;
    active = true;
    spec = {
      group: chip.getAttribute('data-saved-process-group') || '',
      templateId: chip.getAttribute('data-saved-process-drag'),
      name: chip.getAttribute('data-saved-process-name') || '',
    };
    source = chip;
    chip.addEventListener('dragend', endDrag, { once: true });
    e.dataTransfer.setData(SAVED_PROCESS_DRAG_MIME, spec.templateId);
    e.dataTransfer.effectAllowed = 'move';
    chip.classList.add('gq-saved-drag-source');
    $('#dnd-trash')?.classList.add('show', 'dnd-trash-unpin-mode');
  });
  listen('dragend', () => { if (active) endDrag(); });
  listen('dragover', (e) => {
    if (!active) return;
    const trash = trashTarget(e);
    if (!trash) {
      dragPill(e, null);
      return;
    }
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    trash.classList.add('dnd-drop-over');
    const label = spec?.name || spec?.templateId || '';
    dragPill(e, isWizardActive() ? `↓ unbind rite ${label} from ${spec?.group}` : `↓ remove ${label} from ${spec?.group}`);
  });
  listen('dragleave', (e) => {
    if (!active) return;
    const trash = trashTarget(e);
    if (trash && !trash.contains(e.relatedTarget)) trash.classList.remove('dnd-drop-over');
  });
  listen('drop', (e) => {
    if (!active) return;
    const trash = trashTarget(e);
    if (!trash) return;
    e.preventDefault();
    const dropped = spec;
    endDrag();
    const remove = dropped && removers.get(dropped.group);
    if (remove) void remove(dropped.templateId);
  });
  return () => {
    if (active) endDrag();
    off.forEach((fn) => fn());
  };
}

// bindSavedProcessDnd binds the document listeners while any chip row is
// mounted; the returned function releases this row's hold.
export function bindSavedProcessDnd() {
  if (bound++ === 0) unbind = bind();
  let released = false;
  return () => {
    if (released) return;
    released = true;
    if (--bound === 0) {
      unbind?.();
      unbind = null;
    }
  };
}
