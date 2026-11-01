// board-manage.js -- what is done to a board as a whole rather than to its
// contents: getting it out as a file, bringing one in, and deleting it (with
// the dialog that says who else is on it). The list and the tab menu ask for a
// delete here; one BoardDeleteHost in the terminal shell answers, so the dialog
// outlives a list that closes around it.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { signal } from '@preact/signals';
import htm from 'htm';
import { boardIdFromTitle, cleanText, drawnElements, fromExcalidraw, toExcalidraw } from './board-model.js';
import { companionAgents } from './companion-model.js';
import { useDialogFocus } from './dialog-focus.js';

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
export const boardIsGone = (error) => error?.status === 404 && error.code === 'not_found';

const readBoard = async (id, fetchImpl) => bodyOf(await fetchImpl(`/api/boards/${encodeURIComponent(id)}`, { credentials: 'same-origin' }));

function deleteRequest(id, fetchImpl) {
  return fetchImpl(`/api/boards/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' });
}

// exportBoard downloads a board as an .excalidraw file.
export async function exportBoard(id, fetchImpl) {
  const board = await readBoard(id, fetchImpl);
  const { downloadText } = await import('./board-export.js');
  downloadText(`${id}.excalidraw`, toExcalidraw(drawnElements(new Map((board.elements || []).map((el) => [el.id, el])))));
}

// freeBoardId finds an id for a title that is not taken; '' leaves it to the daemon.
export function freeBoardId(title, taken) {
  const base = boardIdFromTitle(title);
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

// importBoardFile makes a new board of an .excalidraw file, under the file's
// name. A file that fails half way leaves no half a board behind.
export async function importBoardFile(file, { fetchImpl, taken = [] }) {
  if (file.size > MAX_IMPORT_BYTES) throw new Error(`the file is larger than ${MAX_IMPORT_BYTES >> 20} MiB`);
  const parsed = fromExcalidraw(await file.text());
  if (!parsed) throw new Error('this is not an .excalidraw file');
  const title = cleanText(file.name.replace(/\.(excalidraw|json)$/i, ''), 120).replace(/\s+/g, ' ').trim() || 'Imported board';
  const board = await bodyOf(await fetchImpl('/api/boards', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title, id: freeBoardId(title, taken) || undefined }),
  }));
  try {
    for (const elements of batches(parsed.elements)) {
      await bodyOf(await fetchImpl(`/api/boards/${encodeURIComponent(board.id)}`, {
        method: 'PATCH', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ elements }),
      }));
    }
  } catch (error) {
    await deleteRequest(board.id, fetchImpl).catch(() => {});
    throw error;
  }
  return { id: board.id, title: board.title || title, skipped: parsed.skipped };
}

// -- deleting ---------------------------------------------------------------

// { id, title, onDeleted } while the dialog is up.
export const boardDeleteRequest = signal(null);

export function askDeleteBoard(request) {
  boardDeleteRequest.value = request;
}

const deletedListeners = new Set();

// onBoardDeleted hears of every board found gone in this page, deleted here or
// elsewhere: what shows the board closes.
export function onBoardDeleted(listener) {
  deletedListeners.add(listener);
  return () => deletedListeners.delete(listener);
}

export function announceBoardDeleted(id) {
  for (const listener of [...deletedListeners]) listener(id);
}

function BoardDeleteDialog({ request, snapshot, fetchImpl, notify, close }) {
  const [board, setBoard] = useState(null);
  const [exported, setExported] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState('');
  const cancelRef = useRef(null);
  const { dialogRef } = useDialogFocus({ open: true, initialFocusRef: cancelRef, onEscape: close });
  useEffect(() => {
    let current = true;
    readBoard(request.id, fetchImpl).then((read) => {
      if (current) setBoard(read);
    }).catch((error) => {
      if (!current) return;
      if (boardIsGone(error)) {
        notify('That board is already gone.');
        announceBoardDeleted(request.id);
        request.onDeleted?.();
        close();
      } else setProblem(error.message);
    });
    return () => { current = false; };
  }, [request]);
  const title = board?.title || request.title || request.id;
  const agents = companionAgents(snapshot?.value);
  const participants = board?.participants || [];
  const people = [
    ...participants.map((id) => ({ id, via: false })),
    ...(board?.group_members || []).filter((id) => !participants.includes(id)).map((id) => ({ id, via: true })),
  ];
  const count = board?.elements?.length ?? 0;
  async function exportFirst() {
    try {
      await exportBoard(request.id, fetchImpl);
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
      if (!boardIsGone(error)) {
        setBusy(false);
        setProblem(`Could not delete it: ${error.message}`);
        return;
      }
    }
    notify(`Deleted “${title}”.`);
    announceBoardDeleted(request.id);
    request.onDeleted?.();
    close();
  }
  return html`<div ref=${dialogRef} class="modal-overlay show board-delete-overlay">
    <div class="modal board-delete" role="alertdialog" aria-modal="true" aria-label=${`Delete ${title}`}>
      <h3>Delete “${title}”?</h3>
      ${board ? html`<p>
        ${count} element${count === 1 ? '' : 's'}${board.group ? `, in group ${board.group}` : ''}.
        This cannot be undone.${people.length ? ' Agents on it are told.' : ''}
      </p>
      ${people.length ? html`<ul class="board-delete-people" aria-label="Agents on this board">
        ${people.map(({ id, via }) => html`<li key=${id}>
          ${agents.get(id)?.name || id.slice(0, 12)}${via ? html` <small>through the group</small>` : ''}
        </li>`)}
      </ul>` : html`<p class="board-muted">No agents are on it.</p>`}` : !problem && html`<p class="board-muted">Loading…</p>`}
      ${problem && html`<p class="board-error" role="alert">${problem}</p>`}
      <div class="modal-buttons">
        <button type="button" class="board-delete-export" disabled=${!board} onClick=${exportFirst}>${exported ? '✓ Exported' : '⤓ Export first'}</button>
        <button type="button" ref=${cancelRef} onClick=${close}>Cancel</button>
        <button type="button" class="confirm-danger board-delete-confirm" disabled=${!board || busy} onClick=${confirm}>Delete board</button>
      </div>
    </div>
  </div>`;
}

// BoardDeleteHost shows the dialog a list or a tab asked for.
export function BoardDeleteHost({ snapshot, fetchImpl, notify }) {
  const request = boardDeleteRequest.value;
  if (!request) return null;
  return html`<${BoardDeleteDialog} key=${request.id} request=${request} snapshot=${snapshot} fetchImpl=${fetchImpl}
    notify=${notify} close=${() => { boardDeleteRequest.value = null; }} />`;
}
