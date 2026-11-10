// groups-taskboard-files.js -- a task's files in the dashboard (docs/taskboard.md
// "Files on a task"): the Files field of the New task form, Ask an agent and
// the edit form. Files come in through attachment-intake.js, as in the spawn
// dialog: 📎 choose files, a drop anywhere on the dialog, or Ctrl/Cmd+V of
// copied files or a screenshot. A task's stored files are tiles; an image
// shows its thumbnail, read through the board's api (fileData), so a
// linked node's board shows its files too. A click on a tile opens the
// Messages tab's attachment viewer (attachment-viewer.js) on it: an image
// with zoom and ‹ › through the task's other images, a Markdown file as
// the rendered document. A file with no viewer (text, a PDF) opens as
// before: a PDF in a new tab, anything else as a download.

import { Fragment, h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { AttachmentList } from './attachment-intake.js';
import { formatAttachmentSize } from './attachment-files.js';
import { TASK_FILE_MAX, TASK_FILE_MAX_BYTES } from './taskboard-actions.js';
import { shortcutText } from './keybindings.js';
import { downloadBlob } from './download.js';
import { cachedTaskFileURL, readTaskFile, taskFileSource, taskFileURL, taskViewerFile, useTaskThumb } from './attachment-source.js';
import { AttachmentViewer, firstViewable, viewerKind } from './attachment-viewer.js';
import { drawingBoardFocusParams } from './drawing-board-model.js';

const html = htm.bind(h);

// FILE_LIMIT_TEXT says the limits where files are picked.
export const FILE_LIMIT_TEXT = `up to ${TASK_FILE_MAX} files, ${TASK_FILE_MAX_BYTES >> 20} MiB each`;

// readFile reads a stored file into a Blob through the board's api.
export function readFile(api, taskId, attachment) {
  return readTaskFile(api, taskId, attachment);
}

// useThumb is a stored image's thumbnail URL, read once per content.
export function useThumb(api, taskId, attachment) {
  return useTaskThumb(api, taskId, attachment);
}

// openFallback shows a stored file the viewer has no view for: a PDF in a
// new tab, anything else as a download.
async function openFallback(api, taskId, attachment) {
  const blob = await readTaskFile(api, taskId, attachment);
  if (blob.type !== 'application/pdf') { downloadBlob(blob, attachment.name); return; }
  const url = URL.createObjectURL(blob);
  globalThis.open(url, '_blank', 'noopener');
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

// useTaskViewer is a task's (or a comment's) files in the attachment
// viewer: open(attachment) opens it on one of them, or falls back for a
// file without a viewer; `viewer` is the open viewer to render.
//
// The open file is held by id and looked up in the current list: the board
// re-reads its tasks on every snapshot poll, so the objects change under an
// open viewer while the files stay the same.
//
// The viewer renders where the caller puts it, which in the edit form is
// inside the form's dialog. Its wrapper keeps a paste, a drop or Ctrl/Cmd+
// Enter in the viewer from reaching the form (which would take the pasted
// files, or Save); the viewer itself uses none of them, and its own keys
// (Escape, the arrows, zoom) listen on the document, which a stop at the
// wrapper would cut off, so other keys pass.
export function useTaskViewer(api, taskId, attachments, surface = 'taskboard') {
  const [shownId, setShownId] = useState(null);
  const source = useMemo(() => taskFileSource(api, taskId), [api, taskId]);
  const files = useMemo(() => (attachments || []).map(taskViewerFile), [attachments]);
  const open = (attachment) => {
    const file = files.find((f) => f.id === attachment.id) || taskViewerFile(attachment);
    if (viewerKind(file) && api?.fileData) { setShownId(file.id); return; }
    void openFallback(api, taskId, attachment).catch(() => {});
  };
  const shown = shownId === null ? null : files.find((f) => f.id === shownId) || null;
  const stop = (event) => event.stopPropagation();
  const stopSubmit = (event) => { if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) event.stopPropagation(); };
  const viewer = shown && html`<div class="tb-file-viewer" onPaste=${stop} onDrop=${stop} onDragOver=${stop} onDragEnter=${stop} onKeyDown=${stopSubmit}>
    <${AttachmentViewer} source=${source} attachment=${shown} attachments=${files}
      surface=${`${surface}-${taskId}`} onClose=${() => setShownId(null)} />
  </div>`;
  return { open, viewer, files, first: firstViewable(files) };
}

function extLabel(name) {
  const dot = String(name || '').lastIndexOf('.');
  return dot > 0 ? String(name).slice(dot + 1, dot + 5).toUpperCase() : 'FILE';
}

const DRAWING_BOARD_ID = /^[a-z0-9][a-z0-9-]{0,63}$/;
const ELEMENT_ID = /^[A-Za-z0-9_-]{1,64}$/;
const NODE_ID = /^inst_[A-Za-z0-9]{4,64}$/;

// drawingLink is where a drawing-board export (a file with `drawing`,
// tk_413bdf5d) opens its board: { board, version, elements, box, node }, or
// null for any other file. node is the linked node the board is on (export
// --node --attach-to), '' for this node's own. The reference comes from the
// board's node, so only well-formed ids and a finite box pass.
export function drawingLink(attachment) {
  const d = attachment?.drawing;
  if (!d || typeof d !== 'object' || !DRAWING_BOARD_ID.test(String(d.board || ''))) return null;
  const node = d.node === undefined || d.node === '' ? '' : String(d.node);
  if (node && !NODE_ID.test(node)) return null;
  const elements = Array.isArray(d.elements) ? d.elements.filter((id) => typeof id === 'string' && ELEMENT_ID.test(id)).slice(0, 5000) : [];
  const c = d.crop || {};
  const box = [c.x, c.y, c.width, c.height].every(Number.isFinite) && c.width > 0 && c.height > 0
    ? { x: c.x, y: c.y, width: c.width, height: c.height } : null;
  return { board: d.board, version: Number.isFinite(d.version) ? d.version : 0, elements, box, node };
}

// drawingLinkHref is where a linked node's board opens: that node's page
// (?node=), on the board and the part the image shows (crop, elements), in
// a window of its own.
export function drawingLinkHref(link) {
  const params = new URLSearchParams({ node: link.node, ...drawingBoardFocusParams({ elements: link.elements, box: link.box }) });
  return `/drawing-boards/${encodeURIComponent(link.board)}?${params}`;
}

// openDrawingLink opens a drawing's board on the part it shows: this node's
// own in its Collab tab (the opener is loaded on use: it lives with the
// terminal shell), a linked node's on that node's page.
export function openDrawingLink(link, win = globalThis.window) {
  if (link.node) {
    win?.open?.(drawingLinkHref(link), '_blank', 'noopener');
    return Promise.resolve(null);
  }
  return import('./drawing-board-overlay-state.js').then((m) => m.openDrawingBoardAt(link.board, link));
}

// StoredTile is one stored file; onOpen (useTaskViewer's open) shows it. A
// drawing on this node's board offers its drawing board.
export function StoredTile({ api, taskId, attachment, removed, onToggle, busy, nameOf, onOpen }) {
  const thumb = useThumb(api, taskId, attachment);
  const by = nameOf ? nameOf(attachment.added_by) : '';
  const drawing = api?.local ? drawingLink(attachment) : null;
  return html`<div class=${`tb-file${removed ? ' tb-file-removed' : ''}`} data-file=${attachment.id}>
    <button data-ui-exempt type="button" class="tb-file-open" title=${`Open ${attachment.name}`} disabled=${removed}
      onClick=${() => onOpen(attachment)}>
      ${thumb ? html`<img class="tb-file-thumb" src=${thumb} alt="" />` : html`<span class="tb-file-thumb tb-file-ext">${extLabel(attachment.name)}</span>`}
    </button>
    <span class="tb-file-name" title=${attachment.name}>${attachment.name}</span>
    <span class="tb-file-size">${formatAttachmentSize(attachment.size)}${by ? ` · ${by}` : ''}${removed ? ' · removed on Save' : ''}</span>
    ${drawing && html`<${Button} size="sm" class="tb-file-drawing" disabled=${removed}
      title=${drawing.node
        ? `Open drawing board ${drawing.board} on the linked node ${drawing.node}, on its page (it was drawn at version ${drawing.version})`
        : `Open drawing board ${drawing.board} on the part this image shows (it was drawn at version ${drawing.version})`}
      onClick=${() => { void openDrawingLink(drawing).catch(() => {}); }}>${drawing.node ? '✎ Open on its node' : '✎ Edit in drawing board'}<//>`}
    ${onToggle && html`<button data-ui-exempt type="button" class="tb-file-rm" disabled=${busy}
      title=${removed ? 'Keep this file' : 'Remove this file (on Save)'} aria-label=${`${removed ? 'Keep' : 'Remove'} ${attachment.name}`}
      onClick=${() => onToggle(attachment.id)}>${removed ? '↺' : '✕'}</button>`}
  </div>`;
}

function PendingTile({ attachment, remove, busy }) {
  return html`<div class="tb-file tb-file-new" data-file-new=${attachment.id}>
    ${attachment.url ? html`<img class="tb-file-thumb" src=${attachment.url} alt="" />` : html`<span class="tb-file-thumb tb-file-ext">${extLabel(attachment.name)}</span>`}
    <span class="tb-file-name" title=${attachment.name}>${attachment.name}</span>
    <span class="tb-file-size">${formatAttachmentSize(attachment.size)} · added on Save</span>
    <button data-ui-exempt type="button" class="tb-file-rm" disabled=${busy} title="Leave this file out"
      aria-label=${`Remove ${attachment.name}`} onClick=${() => remove(attachment.id)}>✕</button>
  </div>`;
}

// FilePicker is the 📎 button and the hint that a drop or a paste works too.
export function FilePicker({ add, busy, full = false, hint = '' }) {
  const input = useRef(null);
  return html`<span class="tb-files-pick">
    <${Button} size="sm" class="tb-files-choose" disabled=${busy || full} onClick=${() => input.current?.click()}>📎 Choose files…<//>
    <input ref=${input} type="file" class="tb-files-input" multiple hidden disabled=${busy}
      onChange=${(event) => { add(event.currentTarget.files); event.currentTarget.value = ''; }} />
    <span class="ui-hint">${full ? `that is ${TASK_FILE_MAX} files` : `${hint || `…or drop files on the dialog, or paste (${shortcutText('terminal-paste')})`} · ${FILE_LIMIT_TEXT}`}</span>
  </span>`;
}

// FilesField is the files of a request that has no task yet (New task, Ask):
// the picked files as a list, and the picker.
export function FilesField({ intake, attachments, busy, hint = '' }) {
  return html`<span class="tb-files-field">
    <${FilePicker} add=${intake.add} busy=${busy} full=${attachments.length >= TASK_FILE_MAX} hint=${hint} />
    ${attachments.length > 0 && html`<${AttachmentList} class="tb-files-list" attachments=${attachments} remove=${intake.remove} busy=${busy} />`}
  </span>`;
}

// StoredFiles is a list of stored files as tiles, opening in the
// attachment viewer: a task's (TaskFiles) or a comment's.
export function StoredFiles({ api, taskId, attachments, class: extra = '', surface = 'taskboard' }) {
  const viewer = useTaskViewer(api, taskId, attachments, surface);
  return html`<${Fragment}><div class=${`tb-files${extra ? ` ${extra}` : ''}`}>
    ${attachments.map((attachment) => html`<${StoredTile} key=${attachment.id} api=${api} taskId=${taskId} attachment=${attachment} onOpen=${viewer.open} />`)}
  </div>${viewer.viewer}</${Fragment}>`;
}

// TaskFiles is the edit form's Files: the task's stored files as tiles (✕
// marks one to remove on Save, ↺ keeps it), the files picked to add, and the
// picker.
export function TaskFiles({ api, task, removed, onToggle, intake, pending, busy, nameOf = null, canWrite = true }) {
  const stored = task.attachments || [];
  const held = stored.length - removed.length + pending.length;
  const viewer = useTaskViewer(api, task.id, stored, 'taskfiles');
  return html`<span class="tb-files-field">
    ${viewer.viewer}
    ${(stored.length > 0 || pending.length > 0) && html`<div class="tb-files">
      ${stored.map((attachment) => html`<${StoredTile} key=${attachment.id} api=${api} taskId=${task.id} attachment=${attachment}
        removed=${removed.includes(attachment.id)} onToggle=${canWrite ? onToggle : null} busy=${busy} nameOf=${nameOf} onOpen=${viewer.open} />`)}
      ${pending.map((attachment) => html`<${PendingTile} key=${`new-${attachment.id}`} attachment=${attachment} remove=${intake.remove} busy=${busy} />`)}
    </div>`}
    ${canWrite && html`<${FilePicker} add=${intake.add} busy=${busy} full=${held >= TASK_FILE_MAX} />`}
  </span>`;
}

// FilesCount is the List's Files cell: 📎 and the count. With a file the
// attachment viewer can show, it is a button that opens the viewer on the
// first such file, stepping through the rest.
export function FilesCount({ api, task }) {
  const files = task.attachments || [];
  const viewer = useTaskViewer(api, task.id, files, 'tblist');
  const title = `${files.length} file${files.length === 1 ? '' : 's'} on this task`;
  if (!viewer.first || !api?.fileData) return html`<span class="tb-files" title=${title}>📎 ${files.length}</span>`;
  return html`<${Fragment}><button data-ui-exempt type="button" class="tb-files tb-files-count"
      title=${`${title}: view ${viewer.first.filename}`} aria-label=${`${title}; view ${viewer.first.filename}`}
      onClick=${(event) => { event.stopPropagation(); viewer.open(viewer.first); }}>📎 ${files.length}</button>${viewer.viewer}</${Fragment}>`;
}

// taskPastedName names a pasted file (a screenshot has no name) by the time
// it was pasted, so a second paste into the same task's brief, in a later
// edit, does not reuse the first one's name: the brief links files by name.
export function taskPastedName(file, n, now = new Date()) {
  const extension = (file?.type && file.type.split('/')[1]) || 'png';
  const pad = (v) => String(v).padStart(2, '0');
  const stamp = `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
  return `pasted-${stamp}-${n}.${extension}`;
}

// useMarkdownFiles is a task's files as a Markdown brief or comment refers
// to them (MarkdownDocument's attachments: { filename, url, previewable }):
// the stored images, read once each through the board's api (the thumbnails'
// cache), and the files picked but not saved yet. Only images are listed: a
// reference to anything else stays a link or its text. With `source`, only
// the images that text names are read, so a hover or a comment does not
// fetch every picture on the task.
export function namesFile(source, name) {
  if (source == null) return true;
  const text = String(source);
  if (!name || !text) return false;
  if (text.includes(name)) return true;
  try { return decodeURIComponent(text).includes(name); } catch { return text.includes(encodeURI(name)); }
}

export function useMarkdownFiles(api, task, pending = [], source = undefined) {
  const stored = (task?.attachments || []).filter((attachment) => attachment.image && attachment.sha256 && namesFile(source, attachment.name));
  const cached = (a) => (api?.fileData && task?.id ? cachedTaskFileURL(api, task.id, a) : '');
  const [urls, setUrls] = useState(() => new Map(stored.map((a) => [a.sha256, cached(a)])));
  const key = stored.map((a) => a.sha256).join(',');
  useEffect(() => {
    if (!api?.fileData || !task?.id) return undefined;
    let live = true;
    for (const attachment of stored) {
      const ready = cached(attachment);
      if (ready) {
        setUrls((before) => (before.get(attachment.sha256) ? before : new Map(before).set(attachment.sha256, ready)));
        continue;
      }
      taskFileURL(api, task.id, attachment).then((made) => {
        if (live) setUrls((before) => new Map(before).set(attachment.sha256, made));
      }).catch(() => {});
    }
    return () => { live = false; };
  }, [task?.id, key]);
  return [
    ...stored.filter((a) => urls.get(a.sha256)).map((a) => ({ filename: a.name, url: urls.get(a.sha256), previewable: true })),
    ...pending.filter((p) => p.url).map((p) => ({ filename: p.name, url: p.url, previewable: true })),
  ];
}
