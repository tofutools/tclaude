// tab-resource-rename.js -- renaming the drawing board or live page a tab shows,
// from the tab itself: a double-click or F2 on a Collab strip tab or on an
// overlay window's title-bar tab turns its label into the Groups tab's name
// field (RenameField: Enter saves, Escape or leaving it keeps the name), saved
// as the resource's title (renameResource).
//
// Only a drawing board's or a page's tab has a name to change here. A terminal
// or a shell is not named by the operator, an agent is renamed the agent way,
// and a process tab holds an editor whose own name field is the process's
// name: renaming the template under it would save a new version the open
// editor does not know of, so its next save would conflict.

import { h } from 'preact';
import htm from 'htm';
import { RenameField, renameResource } from './groups-resources.js';
import { processContext, processRunApi } from './process-context.js';
import { shellToast } from './shell-state.js';

const html = htm.bind(h);

// tabRenameTarget is { kind, id } of the drawing board or page a tab shows, or
// null for a tab with nothing to rename here. A Collab pane passes its seed
// ({ drawingBoard, page, remote }), an overlay tab itself ({ kind,
// drawingBoard, page }). A peer's resource is that node's to rename.
export function tabRenameTarget(of) {
  if (!of || of.remote) return null;
  if (of.drawingBoard && (!of.kind || of.kind === 'drawing_board')) return { kind: 'drawing_board', id: String(of.drawingBoard) };
  if (of.page && (!of.kind || of.kind === 'page')) return { kind: 'page', id: String(of.page) };
  return null;
}

// tabRenameKey reports whether a keydown on a tab asks to rename it (F2).
export const tabRenameKey = (event) => event.key === 'F2' && !event.altKey && !event.ctrlKey && !event.metaKey && !event.shiftKey;

// The tab's own handlers (activate, drag, its menu, a pill's collapse) must
// not see what is typed or clicked in the field.
const contain = (event) => event.stopPropagation();

// TabResourceRename is a tab's name as the field to retype. onRenamed(title)
// relabels the tab once the rename took; onDone ends the edit either way.
export function TabResourceRename({ target, label, onRenamed, onDone, api = processRunApi, source = processContext, notify = shellToast }) {
  const save = renameResource({ ...target, label }, { api, source, notify, onRenamed });
  return html`<span class="mux-tab-rename-slot" onKeyDown=${contain} onClick=${contain} onDblClick=${contain}
    onPointerDown=${contain} onMouseDown=${contain} onContextMenu=${contain}>
    <${RenameField} class="mux-tab-rename" value=${label} label=${label} onSave=${save} onDone=${onDone} />
  </span>`;
}
