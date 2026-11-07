// groups-taskboard-files.js -- a task's files in the dashboard (docs/taskboard.md
// "Files on a task"): the Files field of the New task form, Ask an agent and
// the edit form. Files come in through attachment-intake.js, as in the spawn
// dialog: 📎 choose files, a drop anywhere on the dialog, or Ctrl/Cmd+V of
// copied files or a screenshot. A task's stored files are tiles; an image
// shows its thumbnail, read through the board's api (fileData), so a
// linked node's board shows its files too.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { AttachmentList } from './attachment-intake.js';
import { formatAttachmentSize } from './attachment-files.js';
import { TASK_FILE_MAX, TASK_FILE_MAX_BYTES } from './taskboard-actions.js';
import { shortcutText } from './keybindings.js';

const html = htm.bind(h);

// FILE_LIMIT_TEXT says the limits where files are picked.
export const FILE_LIMIT_TEXT = `up to ${TASK_FILE_MAX} files, ${TASK_FILE_MAX_BYTES >> 20} MiB each`;

// thumbs caches a stored image's object URL by its content, so a re-render
// (every snapshot poll) never reads it again.
const thumbs = new Map();

function base64Blob(data, type) {
  const bytes = Uint8Array.from(globalThis.atob(String(data || '')), (c) => c.charCodeAt(0));
  return new Blob([bytes], { type: type || 'application/octet-stream' });
}

// SHOWN_TYPES are the types a file opens as in a tab, by its extension. The
// type is never taken from the reply: a linked node is not trusted, and a
// blob: URL has this dashboard's origin, so a file claimed as an image but
// sent as text/html would run here. Anything else is a download.
const SHOWN_TYPES = { png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp', pdf: 'application/pdf' };

function shownType(name) {
  const dot = String(name || '').lastIndexOf('.');
  return dot > 0 ? SHOWN_TYPES[String(name).slice(dot + 1).toLowerCase()] || '' : '';
}

// readFile reads a stored file into a Blob through the board's api: an image
// or a PDF (by its name) as that type, anything else as plain bytes.
export async function readFile(api, taskId, attachment) {
  const out = await api.fileData(taskId, attachment.id);
  return base64Blob(out?.data, shownType(attachment.name) || 'application/octet-stream');
}

// useThumb is a stored image's thumbnail URL, read once per content.
function useThumb(api, taskId, attachment) {
  const key = attachment.image && attachment.sha256 ? attachment.sha256 : '';
  const [url, setUrl] = useState(() => (key ? thumbs.get(key) || '' : ''));
  useEffect(() => {
    if (!key || url || !api?.fileData) return undefined;
    let live = true;
    readFile(api, taskId, attachment).then((blob) => {
      const made = thumbs.get(key) || URL.createObjectURL(blob);
      thumbs.set(key, made);
      if (live) setUrl(made);
    }).catch(() => {});
    return () => { live = false; };
  }, [key]);
  return url;
}

// openFile shows a stored file: an image or PDF in a new tab, anything else
// as a download.
async function openFile(api, taskId, attachment) {
  const blob = await readFile(api, taskId, attachment);
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.rel = 'noopener';
  if (blob.type !== 'application/octet-stream') a.target = '_blank';
  else a.download = attachment.name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

function extLabel(name) {
  const dot = String(name || '').lastIndexOf('.');
  return dot > 0 ? String(name).slice(dot + 1, dot + 5).toUpperCase() : 'FILE';
}

function StoredTile({ api, taskId, attachment, removed, onToggle, busy, nameOf }) {
  const thumb = useThumb(api, taskId, attachment);
  const by = nameOf ? nameOf(attachment.added_by) : '';
  return html`<div class=${`tb-file${removed ? ' tb-file-removed' : ''}`} data-file=${attachment.id}>
    <button data-ui-exempt type="button" class="tb-file-open" title=${`Open ${attachment.name}`} disabled=${removed}
      onClick=${() => { void openFile(api, taskId, attachment).catch(() => {}); }}>
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
export function FilePicker({ add, busy, full = false }) {
  const input = useRef(null);
  return html`<span class="tb-files-pick">
    <${Button} size="sm" class="tb-files-choose" disabled=${busy || full} onClick=${() => input.current?.click()}>📎 Choose files…<//>
    <input ref=${input} type="file" class="tb-files-input" multiple hidden disabled=${busy}
      onChange=${(event) => { add(event.currentTarget.files); event.currentTarget.value = ''; }} />
    <span class="ui-hint">${full ? `the task holds ${TASK_FILE_MAX} files` : `…or drop files on the dialog, or paste (${shortcutText('terminal-paste')}) · ${FILE_LIMIT_TEXT}`}</span>
  </span>`;
}

// FilesField is the files of a request that has no task yet (New task, Ask):
// the picked files as a list, and the picker.
export function FilesField({ intake, attachments, busy }) {
  return html`<span class="tb-files-field">
    <${FilePicker} add=${intake.add} busy=${busy} full=${attachments.length >= TASK_FILE_MAX} />
    ${attachments.length > 0 && html`<${AttachmentList} class="tb-files-list" attachments=${attachments} remove=${intake.remove} busy=${busy} />`}
  </span>`;
}

// TaskFiles is the edit form's Files: the task's stored files as tiles (✕
// marks one to remove on Save, ↺ keeps it), the files picked to add, and the
// picker.
export function TaskFiles({ api, task, removed, onToggle, intake, pending, busy, nameOf = null, canWrite = true }) {
  const stored = task.attachments || [];
  const held = stored.length - removed.length + pending.length;
  return html`<span class="tb-files-field">
    ${(stored.length > 0 || pending.length > 0) && html`<div class="tb-files">
      ${stored.map((attachment) => html`<${StoredTile} key=${attachment.id} api=${api} taskId=${task.id} attachment=${attachment}
        removed=${removed.includes(attachment.id)} onToggle=${canWrite ? onToggle : null} busy=${busy} nameOf=${nameOf} />`)}
      ${pending.map((attachment) => html`<${PendingTile} key=${`new-${attachment.id}`} attachment=${attachment} remove=${intake.remove} busy=${busy} />`)}
    </div>`}
    ${canWrite && html`<${FilePicker} add=${intake.add} busy=${busy} full=${held >= TASK_FILE_MAX} />`}
  </span>`;
}
