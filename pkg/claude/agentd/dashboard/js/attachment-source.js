// attachment-source.js -- where the attachment viewer (attachment-viewer.js)
// reads a file from. The viewer is the Messages tab's: the media viewer
// (image-preview-overlay.js) and the document viewer (markdown-attachment.js).
// A source tells them three things about one file:
//
//   href(a)   a URL the browser can show now, or '' until check() made one
//   check(a)  'ready' | 'gone' | 'error': whether the file is there, and,
//             for a source whose bytes must be fetched first, fetching them
//   text(a)   { state, text }: the file as text, for the document viewer
//   key       names the files' owner (a message, a task) in dialog ids
//
// A notification's files have authenticated download routes, so its source
// is plain URLs. A task's files are read through the board's api as base64
// (fileData), the form a linked node's board carries too, so its source
// turns them into blob: URLs.

import { useEffect, useState } from 'preact/hooks';
import { attachmentHrefByID } from './human-attachments.js';

// messageFileSource is a notification's (or agent mail's) files.
export function messageFileSource(messageID) {
  const href = (attachment) => attachmentHrefByID(messageID, attachment);
  return {
    key: messageID,
    href,
    // HEAD tells a file the cleanup already removed (410) from one that
    // will not load, before the viewer tries to draw it.
    async check(attachment) {
      try {
        const response = await fetch(href(attachment), { method: 'HEAD', credentials: 'same-origin' });
        if (response.status === 410) return 'gone';
        return response.ok ? 'ready' : 'error';
      } catch {
        return 'error';
      }
    },
    // One GET both fetches a document and tells a removed one.
    async text(attachment) {
      try {
        const response = await fetch(href(attachment), { credentials: 'same-origin' });
        if (response.status === 410) return { state: 'gone', text: '' };
        if (!response.ok) return { state: 'error', text: '' };
        return { state: 'ready', text: await response.text() };
      } catch {
        return { state: 'error', text: '' };
      }
    },
  };
}

// SAFE_TYPES are the types a task's blob may carry, by the file's
// extension. The type is never taken from the reply: a linked node is not
// trusted, and a blob: URL has this dashboard's origin, so a file claimed as
// an image but typed text/html would run here if opened in a tab. Anything
// else is plain bytes.
const SAFE_TYPES = {
  png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp', avif: 'image/avif',
  mp4: 'video/mp4', m4v: 'video/mp4', webm: 'video/webm', ogv: 'video/ogg', mov: 'video/quicktime',
  pdf: 'application/pdf',
};

export function safeFileType(name) {
  const dot = String(name || '').lastIndexOf('.');
  return dot > 0 ? SAFE_TYPES[String(name).slice(dot + 1).toLowerCase()] || '' : '';
}

function base64Blob(data, type) {
  const bytes = Uint8Array.from(globalThis.atob(String(data || '')), (c) => c.charCodeAt(0));
  return new Blob([bytes], { type: type || 'application/octet-stream' });
}

// readTaskFile reads a task's stored file into a Blob through the board's
// api, typed by its name (SAFE_TYPES) or as plain bytes.
export async function readTaskFile(api, taskId, attachment) {
  const out = await api.fileData(taskId, attachment.id);
  return base64Blob(out?.data, safeFileType(attachment.filename || attachment.name) || 'application/octet-stream');
}

// taskURLs caches a task file's object URL by its content (or, without a
// hash, by task and id), so a re-render (every snapshot poll) or a second
// open never reads it again. Blobs are small (a few MiB at most, capped by
// the board) and few, so they stay for the page's life.
const taskURLs = new Map();
// ready is the URLs already made, for href's synchronous read.
const ready = new Map();
const taskFileKey = (taskId, attachment) => attachment.sha256 || `${taskId}#${attachment.id}`;

// taskBlob is { blob, url } for a task's file, read once.
async function taskBlob(api, taskId, attachment) {
  const key = taskFileKey(taskId, attachment);
  if (!taskURLs.has(key)) {
    const pending = readTaskFile(api, taskId, attachment).then((blob) => ({ blob, url: URL.createObjectURL(blob) }));
    taskURLs.set(key, pending);
    pending.catch(() => { if (taskURLs.get(key) === pending) taskURLs.delete(key); });
  }
  const made = await taskURLs.get(key);
  ready.set(key, made.url);
  return made;
}

function stateOf(err) {
  return err?.status === 410 || err?.status === 404 ? 'gone' : 'error';
}

// taskFileSource is a task's files (or a task comment's), through the
// board's api.
export function taskFileSource(api, taskId) {
  return {
    key: taskId,
    href: (attachment) => ready.get(taskFileKey(taskId, attachment)) || '',
    async check(attachment) {
      try {
        await taskBlob(api, taskId, attachment);
        return 'ready';
      } catch (err) {
        return stateOf(err);
      }
    },
    async text(attachment) {
      try {
        const { blob } = await taskBlob(api, taskId, attachment);
        return { state: 'ready', text: await blob.text() };
      } catch (err) {
        return { state: stateOf(err), text: '' };
      }
    },
  };
}

// taskViewerFile is a task's file in the viewer's shape: a notification
// file's fields (`filename`, `size_bytes`, the daemon's `previewable` /
// `markdown` / `video` verdicts), and the board's own beside them.
export function taskViewerFile(attachment) {
  return {
    ...attachment,
    filename: attachment.name,
    size_bytes: attachment.size,
  };
}

// useTaskThumb is a stored image's thumbnail URL, read once per content
// through the same cache the viewer reads from.
export function useTaskThumb(api, taskId, attachment) {
  const key = attachment.image && api?.fileData ? taskFileKey(taskId, attachment) : '';
  const [url, setUrl] = useState(() => (key ? ready.get(key) || '' : ''));
  useEffect(() => {
    if (!key || url) return undefined;
    let live = true;
    taskBlob(api, taskId, attachment).then((made) => {
      if (live) setUrl(made.url);
    }).catch(() => {});
    return () => { live = false; };
  }, [key]);
  return url;
}
