// drawing-board-manage.js -- what is done to a drawing board as a whole rather than to its
// contents: getting it out as a file, bringing one in, and deleting it (with
// the dialog that says who else is on it). The list and the tab menu ask for a
// delete here; one DrawingBoardDeleteHost in the terminal shell answers, so the dialog
// outlives a list that closes around it.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { Button } from './ui-components.js';
import { drawingBoardIdFromTitle, cleanText, drawnElements, fromExcalidraw, toExcalidraw } from './drawing-board-model.js';
import { companionAgents } from './companion-model.js';
import { Dialog, DialogClose } from './ui-dialog.js';
import { downloadText } from './download.js';

const html = htm.bind(h);

const MAX_IMPORT_BYTES = 64 << 20;
// In characters, at most three bytes each: the daemon takes 4 MiB a request.
const MAX_BATCH_CHARS = 1 << 20;

async function bodyOf(response) {
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(body.error || `${response.status} ${response.statusText || ''}`.trim());
    error.status = response.status;
    error.code = body.code;
    throw error;
  }
  return body;
}

// A daemon with collab off answers 404 too, and has deleted nothing.
export const drawingBoardIsGone = (error) => error?.status === 404 && error.code === 'not_found';

const readDrawingBoard = async (id, fetchImpl) => bodyOf(await fetchImpl(`/api/drawing-boards/${encodeURIComponent(id)}`, { credentials: 'same-origin' }));

function deleteRequest(id, fetchImpl) {
  return fetchImpl(`/api/drawing-boards/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' });
}

// exportDrawingBoard downloads a drawing board as an .excalidraw file.
export async function exportDrawingBoard(id, fetchImpl) {
  const drawingBoard = await readDrawingBoard(id, fetchImpl);
  downloadText(`${id}.excalidraw`, toExcalidraw(drawnElements(new Map((drawingBoard.elements || []).map((el) => [el.id, el])))));
}

// freeDrawingBoardId finds an id for a title that is not taken; '' leaves it to the daemon.
export function freeDrawingBoardId(title, taken) {
  const base = drawingBoardIdFromTitle(title);
  if (!base || !taken.includes(base)) return base;
  for (let n = 2; ; n++) {
    const id = `${base.slice(0, 60)}-${n}`;
    if (!taken.includes(id)) return id;
  }
}

function batches(elements) {
  const out = [];
  let batch = []; let chars = 0;
  for (const el of elements) {
    const size = JSON.stringify(el).length;
    if (batch.length && chars + size > MAX_BATCH_CHARS) {
      out.push(batch);
      batch = []; chars = 0;
    }
    batch.push(el);
    chars += size;
  }
  if (batch.length) out.push(batch);
  return out;
}

// importDrawingBoardFile makes a new drawing board of an .excalidraw file, under the file's
// name. A file that fails half way leaves no half a drawing board behind.
export async function importDrawingBoardFile(file, { fetchImpl, taken = [] }) {
  if (file.size > MAX_IMPORT_BYTES) throw new Error(`the file is larger than ${MAX_IMPORT_BYTES >> 20} MiB`);
  const parsed = fromExcalidraw(await file.text());
  if (!parsed) throw new Error('this is not an .excalidraw file');
  const title = cleanText(file.name.replace(/\.(excalidraw|json)$/i, ''), 120).replace(/\s+/g, ' ').trim() || 'Imported drawing board';
  const drawingBoard = await bodyOf(await fetchImpl('/api/drawing-boards', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title, id: freeDrawingBoardId(title, taken) || undefined }),
  }));
  try {
    for (const elements of batches(parsed.elements)) {
      await bodyOf(await fetchImpl(`/api/drawing-boards/${encodeURIComponent(drawingBoard.id)}`, {
        method: 'PATCH', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ elements }),
      }));
    }
  } catch (error) {
    await deleteRequest(drawingBoard.id, fetchImpl).catch(() => {});
    throw error;
  }
  return { id: drawingBoard.id, title: drawingBoard.title || title, skipped: parsed.skipped };
}

// -- deleting ---------------------------------------------------------------

// { id, title, onDeleted } while the dialog is up.
export const drawingBoardDeleteRequest = signal(null);

export function askDeleteDrawingBoard(request) {
  drawingBoardDeleteRequest.value = request;
}

const deletedListeners = new Set();

// onDrawingBoardDeleted hears of every drawing board found gone in this page, deleted here or
// elsewhere: what shows the drawing board closes.
export function onDrawingBoardDeleted(listener) {
  deletedListeners.add(listener);
  return () => deletedListeners.delete(listener);
}

export function announceDrawingBoardDeleted(id) {
  for (const listener of [...deletedListeners]) listener(id);
}

function DrawingBoardDeleteDialog({ request, snapshot, fetchImpl, notify, close }) {
  const [drawingBoard, setDrawingBoard] = useState(null);
  const [exported, setExported] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState('');
  const cancelRef = useRef(null);
  useEffect(() => {
    let current = true;
    readDrawingBoard(request.id, fetchImpl).then((read) => {
      if (current) setDrawingBoard(read);
    }).catch((error) => {
      if (!current) return;
      if (drawingBoardIsGone(error)) {
        notify('That drawing board is already gone.');
        announceDrawingBoardDeleted(request.id);
        request.onDeleted?.();
        close();
      } else setProblem(error.message);
    });
    return () => { current = false; };
  }, [request]);
  const title = drawingBoard?.title || request.title || request.id;
  const agents = companionAgents(snapshot?.value);
  const participants = drawingBoard?.participants || [];
  const people = [
    ...participants.map((id) => ({ id, via: false })),
    ...(drawingBoard?.group_members || []).filter((id) => !participants.includes(id)).map((id) => ({ id, via: true })),
  ];
  const count = drawingBoard?.elements?.length ?? 0;
  async function exportFirst() {
    try {
      await exportDrawingBoard(request.id, fetchImpl);
      setExported(true);
    } catch (error) {
      setProblem(`Could not export it: ${error.message}`);
    }
  }
  async function confirm() {
    setBusy(true);
    try {
      await bodyOf(await deleteRequest(request.id, fetchImpl));
    } catch (error) {
      // Someone else got there first: what was asked for has happened.
      if (!drawingBoardIsGone(error)) {
        setBusy(false);
        setProblem(`Could not delete it: ${error.message}`);
        return;
      }
    }
    notify(`Deleted “${title}”.`);
    announceDrawingBoardDeleted(request.id);
    request.onDeleted?.();
    close();
  }
  return html`<${Dialog} id="drawing-board-delete-modal" overlayClass="drawing-board-delete-overlay" dialogClass="modal drawing-board-delete"
    title=${`Delete “${title}”?`} initialFocusRef=${cancelRef} onClose=${close} busy=${busy} error=${problem}
    footerStart=${html`<${Button} class="drawing-board-delete-export" disabled=${!drawingBoard} onClick=${exportFirst}>${exported ? '✓ Exported' : '⤓ Export first'}<//>`}
    footer=${html`<${DialogClose} domRef=${cancelRef} disabled=${busy}>Cancel<//>
      <${Button} variant="danger" class="drawing-board-delete-confirm" disabled=${!drawingBoard || busy} onClick=${confirm}>Delete drawing board<//>`}>
    ${drawingBoard ? html`<p>
      ${count} element${count === 1 ? '' : 's'}${drawingBoard.group ? `, in group ${drawingBoard.group}` : ''}.
      This cannot be undone.${people.length ? ' Agents on it are told.' : ''}
    </p>
    ${people.length ? html`<ul class="drawing-board-delete-people" aria-label="Agents on this drawing board">
      ${people.map(({ id, via }) => html`<li key=${id}>
        ${agents.get(id)?.name || id.slice(0, 12)}${via ? html` <small>through the group</small>` : ''}
      </li>`)}
    </ul>` : html`<p class="drawing-board-muted">No agents are on it.</p>`}` : !problem && html`<p class="drawing-board-muted">Loading…</p>`}
  <//>`;
}

// DrawingBoardDeleteHost shows the dialog a list or a tab asked for.
export function DrawingBoardDeleteHost({ snapshot, fetchImpl, notify }) {
  const request = drawingBoardDeleteRequest.value;
  if (!request) return null;
  return html`<${DrawingBoardDeleteDialog} key=${request.id} request=${request} snapshot=${snapshot} fetchImpl=${fetchImpl}
    notify=${notify} close=${() => { drawingBoardDeleteRequest.value = null; }} />`;
}
