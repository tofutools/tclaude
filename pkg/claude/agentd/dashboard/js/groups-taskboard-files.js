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
import { useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { AttachmentList } from './attachment-intake.js';
import { formatAttachmentSize } from './attachment-files.js';
import { TASK_FILE_MAX, TASK_FILE_MAX_BYTES } from './taskboard-actions.js';
import { shortcutText } from './keybindings.js';
import { downloadBlob } from './download.js';
import { readTaskFile, taskFileSource, taskViewerFile, useTaskThumb } from './attachment-source.js';
import { AttachmentViewer, firstViewable, viewerKind } from './attachment-viewer.js';

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
export function useTaskViewer(api, taskId, attachments, surface = 'taskboard') {
  const [shown, setShown] = useState(null);
  const source = useMemo(() => taskFileSource(api, taskId), [api, taskId]);
  const files = useMemo(() => (attachments || []).map(taskViewerFile), [attachments]);
  const open = (attachment) => {
    const file = files.find((f) => f.id === attachment.id) || taskViewerFile(attachment);
    if (viewerKind(file) && api?.fileData) { setShown(file); return; }
    void openFallback(api, taskId, attachment).catch(() => {});
  };
  const viewer = shown && html`<${AttachmentViewer} source=${source} attachment=${shown} attachments=${files}
    surface=${`${surface}-${taskId}`} onClose=${() => setShown(null)} />`;
  return { open, viewer, files, first: firstViewable(files) };
}

function extLabel(name) {
  const dot = String(name || '').lastIndexOf('.');
  return dot > 0 ? String(name).slice(dot + 1, dot + 5).toUpperCase() : 'FILE';
}

// StoredTile is one stored file; onOpen (useTaskViewer's open) shows it.
export function StoredTile({ api, taskId, attachment, removed, onToggle, busy, nameOf, onOpen }) {
  const thumb = useThumb(api, taskId, attachment);
  const by = nameOf ? nameOf(attachment.added_by) : '';
  return html`<div class=${`tb-file${removed ? ' tb-file-removed' : ''}`} data-file=${attachment.id}>
    <button data-ui-exempt type="button" class="tb-file-open" title=${`Open ${attachment.name}`} disabled=${removed}
      onClick=${() => onOpen(attachment)}>
      ${thumb ? html`<img class="tb-file-thumb" src=${thumb} alt="" />` : html`<span class="tb-file-thumb tb-file-ext">${extLabel(attachment.name)}</span>`}
    </button>
    <span class="tb-file-name" title=${attachment.name}>${attachment.name}</span>
    <span class="tb-file-size">${formatAttachmentSize(attachment.size)}${by ? ` · ${by}` : ''}${removed ? ' · removed on Save' : ''}</span>
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
