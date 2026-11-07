// attachment-intake.js -- taking files into a dialog: the 📎 picker, a drag
// of files from the desktop, and Ctrl/Cmd+V of copied files or a screenshot.
// One behaviour for every dialog that takes files (the spawn dialog, the task
// board's forms and Ask an agent), so a paste or a drop works the same
// everywhere. The dialog keeps the list; this hook adds to and removes from
// it, and hands out the handlers to put on the dialog's root.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { attachKey, checkAttachment, formatAttachmentSize, pastedName } from './attachment-files.js';

const html = htm.bind(h);

// PASTE_REPEAT_MS: a paste of the same files again within this time is the
// browser firing twice (or a held key), not a second copy.
export const PASTE_REPEAT_MS = 1000;

// hasDraggedFiles reports whether a drag carries files (not text, not a row).
export function hasDraggedFiles(event) {
  const types = event.dataTransfer?.types;
  return !!types && Array.prototype.indexOf.call(types, 'Files') !== -1;
}

// clipboardFiles are the files a paste carries: its files and its items of
// kind file, each once.
export function clipboardFiles(transfer) {
  if (!transfer) return [];
  const seen = new Set();
  const collected = [];
  const collect = (file) => {
    if (!file) return;
    const key = attachKey(file);
    if (seen.has(key)) return;
    seen.add(key);
    collected.push(file);
  };
  for (let index = 0; index < (transfer.files?.length || 0); index += 1) collect(transfer.files[index]);
  for (let index = 0; index < (transfer.items?.length || 0); index += 1) {
    if (transfer.items[index].kind === 'file') collect(transfer.items[index].getAsFile());
  }
  return collected;
}

// useAttachmentIntake adds files to and removes them from a dialog's list.
//
//   setAttachments  the list's state setter; entries are { id, file, name, size, url }
//   attachments     the list as rendered, for the limits' count
//   busy()          true while the dialog sends: nothing is taken then
//   onChange()      runs on every add or remove (the spawn dialog drops its
//                   uploaded copies)
//   limits          { maxFiles, maxBytes, accept(file, name) -> '' | why }, or null
//   onReject(text)  says which files were left out, and why
//
// It returns add(files), remove(id), dragOver (a drag of files is over the
// dialog) and handlers ({ onPaste, onDragEnter, onDragOver, onDragLeave,
// onDrop }) for the dialog's root.
export function useAttachmentIntake({ setAttachments, attachments = [], busy = () => false, onChange = () => {}, limits = null, onReject = () => {} }) {
  const sequence = useRef(0);
  const pasteState = useRef({ at: 0, keys: new Set() });
  const dragDepth = useRef(0);
  const [dragOver, setDragOver] = useState(false);

  const add = (files) => {
    if (busy()) return;
    const additions = [];
    const refused = [];
    let count = attachments.length;
    for (const file of files || []) {
      if (!file) continue;
      const name = file.name || pastedName(file, ++sequence.current);
      const why = checkAttachment(file, name, count, limits);
      if (why) { refused.push(`${name} (${why})`); continue; }
      count += 1;
      additions.push({
        id: ++sequence.current, file, name, size: file.size,
        url: (file.type || '').startsWith('image/') ? URL.createObjectURL(file) : '',
      });
    }
    if (refused.length) onReject(`Not attached: ${refused.join(', ')}`);
    if (!additions.length) return;
    onChange();
    setAttachments((before) => [...before, ...additions]);
  };

  const remove = (id) => {
    if (busy()) return;
    onChange();
    setAttachments((before) => {
      const removed = before.find((attachment) => attachment.id === id);
      if (removed?.url) URL.revokeObjectURL(removed.url);
      return before.filter((attachment) => attachment.id !== id);
    });
  };

  const onPaste = (event) => {
    if (busy()) return;
    const collected = clipboardFiles(event.clipboardData);
    if (!collected.length) return;
    event.preventDefault();
    const now = globalThis.performance?.now?.() || 0;
    const repeating = now - pasteState.current.at < PASTE_REPEAT_MS;
    const fresh = repeating ? collected.filter((file) => !pasteState.current.keys.has(attachKey(file))) : collected;
    pasteState.current = { at: now, keys: new Set(collected.map(attachKey)) };
    add(fresh);
  };
  const onDragEnter = (event) => {
    if (!hasDraggedFiles(event)) return;
    event.preventDefault();
    if (busy()) return;
    dragDepth.current += 1;
    setDragOver(true);
  };
  const onDragOver = (event) => {
    if (!hasDraggedFiles(event)) return;
    event.preventDefault();
    if (busy()) return;
    event.dataTransfer.dropEffect = 'copy';
  };
  const onDragLeave = (event) => {
    if (!hasDraggedFiles(event)) return;
    if (busy()) return;
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (!dragDepth.current) setDragOver(false);
  };
  const onDrop = (event) => {
    if (!hasDraggedFiles(event)) return;
    event.preventDefault();
    if (busy()) return;
    dragDepth.current = 0;
    setDragOver(false);
    add(event.dataTransfer.files);
  };

  return { add, remove, dragOver, handlers: { onPaste, onDragEnter, onDragOver, onDragLeave, onDrop } };
}

// AttachmentList lists a dialog's attachments: a thumbnail (an image) or 📄,
// the name, the size and ✕.
export function AttachmentList({ attachments, remove, busy, id = undefined, class: extra = '' }) {
  return html`<ul class=${`spawn-attachments-list${extra ? ` ${extra}` : ''}`} id=${id}>
    ${attachments.map((attachment) => html`<li key=${attachment.id}>
      ${attachment.url
        ? html`<img class="att-thumb" src=${attachment.url} alt="" />`
        : html`<span class="att-icon">📄</span>`}
      <span class="att-name" title=${attachment.name}>${attachment.name}</span>
      <span class="att-size">${formatAttachmentSize(attachment.size)}</span>
      <button data-ui-exempt type="button" class="att-remove" title="Remove" disabled=${busy}
        aria-label=${`Remove ${attachment.name}`} onClick=${() => remove(attachment.id)}>✕</button>
    </li>`)}
  </ul>`;
}
