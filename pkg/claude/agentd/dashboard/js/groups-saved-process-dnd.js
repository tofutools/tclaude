// groups-saved-process-dnd.js — drag a group's saved-process chip
// (groups-saved-processes.js) onto the shared #dnd-trash bin to remove the
// process from that group: the drag twin of the chip's × button. The
// template itself stays; only the group's pin goes. Dropped on the dock's
// process library (dock-processes.js, .dock-processes) instead, the chip
// publishes the process there, the drag twin of the strip's Publish, so other
// groups can start it from the library.
//
// Same isolation discipline as process-template-dnd.js: the drag sets ONLY a
// custom MIME (never text/plain), every handler self-gates on the
// module-local active flag, and dnd.js's drop handler bails on the MIME.
//
// Chips render once per group, so binding is document-level delegation keyed
// on data-saved-process-drag, refcounted across the mounted chip rows. Each
// group's row registers the remover and the publisher the drops call for its
// chips.

import { $, $$ } from './helpers.js';
import { isWizardActive } from './slop.js';
import { bindRowDragGuard } from './row-drag.js';

export const SAVED_PROCESS_DRAG_MIME = 'application/x-tclaude-saved-process';

// LIBRARY_TARGET is the dock's process library section, folded or not.
const LIBRARY_TARGET = '.dock-processes';
const LIBRARY_OVER = 'dock-publish-over';

const removers = new Map();
const publishers = new Map();
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

// registerSavedProcessPublisher routes drops of `group`'s chips on the dock's
// process library to publish(id).
export function registerSavedProcessPublisher(group, publish) {
  publishers.set(group, publish);
  return () => { if (publishers.get(group) === publish) publishers.delete(group); };
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
  // The library sits at the window's right edge: flip the pill left of the
  // pointer when it would run off it.
  const width = pill.offsetWidth || 0;
  const x = e.clientX + 12 + width > (window.innerWidth || Infinity) - 8 ? e.clientX - 12 - width : e.clientX + 12;
  pill.style.transform = `translate(${x}px, ${e.clientY + 12}px)`;
}

function endDrag() {
  active = false;
  spec = null;
  source?.removeEventListener('dragend', endDrag);
  source = null;
  $$('.gq-saved-drag-source').forEach((chip) => chip.classList.remove('gq-saved-drag-source'));
  const trash = $('#dnd-trash');
  if (trash) trash.classList.remove('show', 'dnd-drop-over', 'dnd-trash-unpin-mode');
  $$(`.${LIBRARY_OVER}`).forEach((el) => el.classList.remove(LIBRARY_OVER));
  dragPill(null, null);
}

function libraryPill(dragged) {
  const label = dragged?.name || dragged?.templateId || '';
  if (dragged?.listed) return `${label} is already in the library`;
  return isWizardActive() ? `↑ inscribe rite ${label} in the grimoire` : `↑ publish ${label} to the library`;
}

function bind() {
  const off = [];
  const listen = (type, listener) => {
    document.addEventListener(type, listener);
    off.push(() => document.removeEventListener(type, listener));
  };
  // A press on the chip's buttons must stay a click, not start a drag.
  off.push(bindRowDragGuard(document, '[data-saved-process-drag]', 'button').unbind);
  const trashTarget = (e) => e.target.closest?.('#dnd-trash');
  const libraryTarget = (e) => e.target.closest?.(LIBRARY_TARGET);
  listen('dragstart', (e) => {
    const chip = e.target.closest?.('[data-saved-process-drag]');
    if (!chip) return;
    active = true;
    spec = {
      group: chip.getAttribute('data-saved-process-group') || '',
      templateId: chip.getAttribute('data-saved-process-drag'),
      name: chip.getAttribute('data-saved-process-name') || '',
      listed: chip.getAttribute('data-saved-process-listed') === '1',
    };
    source = chip;
    chip.addEventListener('dragend', endDrag, { once: true });
    e.dataTransfer.setData(SAVED_PROCESS_DRAG_MIME, spec.templateId);
    // copyMove: the chip is also a resource row (resource-dnd.js), whose
    // Ctrl/Cmd-drop onto another group copies it there; 'move' alone would
    // make that drop refused.
    e.dataTransfer.effectAllowed = 'copyMove';
    chip.classList.add('gq-saved-drag-source');
    $('#dnd-trash')?.classList.add('show', 'dnd-trash-unpin-mode');
  });
  listen('dragend', () => { if (active) endDrag(); });
  listen('dragover', (e) => {
    if (!active) return;
    const library = libraryTarget(e);
    if (library) {
      e.preventDefault();
      // Publishing copies: the group keeps its pin.
      e.dataTransfer.dropEffect = 'copy';
      library.classList.add(LIBRARY_OVER);
      dragPill(e, libraryPill(spec));
      return;
    }
    // Off the library: drop its light even if a dragleave was missed.
    $$(`.${LIBRARY_OVER}`).forEach((el) => el.classList.remove(LIBRARY_OVER));
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
    const library = libraryTarget(e);
    if (library && !library.contains(e.relatedTarget)) library.classList.remove(LIBRARY_OVER);
    const trash = trashTarget(e);
    if (trash && !trash.contains(e.relatedTarget)) trash.classList.remove('dnd-drop-over');
  });
  listen('drop', (e) => {
    if (!active) return;
    if (libraryTarget(e)) {
      e.preventDefault();
      const dropped = spec;
      endDrag();
      const publish = dropped && publishers.get(dropped.group);
      if (publish) void publish(dropped.templateId);
      return;
    }
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
