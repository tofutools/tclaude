// drawing-board-attach-dialog.js -- File › Export › Attach to a task… in the
// drawing board editor (tk_413bdf5d; the operator's pick A on Q33): one
// dialog that finds a task, draws the part chosen (the selection, or the
// whole board) with a live preview, and attaches it as a PNG through
// POST /v1/taskboard/tasks/{id}/drawings, optionally as a new comment.
//
// The daemon updates a drawing already on the task in place when it shows
// the same part of the same board; the dialog says so before you click
// ("already on it: attaching updates it"), by the same selection digest the
// daemon keeps (taskboard_drawings.go selectionDigest).
//
// askAttachDrawing opens it; the shell island hosts the one dialog
// (DrawingBoardAttachHost).

import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { signal } from '@preact/signals';
import { Dialog } from './ui-dialog.js';
import { Button, Check, Input, Textarea } from './ui-components.js';

const html = htm.bind(h);

// The tasks the list shows at once; the search narrows the rest.
const SHOWN = 8;
const OPEN = new Set(['open', 'blocked']);

// { drawingBoardId, title, group, selection, fetchImpl, notify, seq } while up.
export const attachDrawingRequest = signal(null);
let requests = 0;

// askAttachDrawing opens the dialog for the drawing board: selection is the
// ids selected now (empty: the whole board), group its group, if any, whose
// tasks the list puts first.
export function askAttachDrawing({ drawingBoardId, title = '', group = '', selection = [], fetchImpl = (...a) => globalThis.fetch(...a), notify = () => {} }) {
  attachDrawingRequest.value = { drawingBoardId, title, group, selection: [...selection], fetchImpl, notify, seq: ++requests };
}

async function json(response) {
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error(body?.message || body?.error || `${response.status} ${response.statusText || ''}`.trim());
  return body;
}

// selectionDigest is the daemon's digest of a pick of element ids, in any
// order: the first 16 bytes of the SHA-256 of the sorted, distinct ids, one
// per line, in hex. "" for no ids (the whole board).
export async function selectionDigest(ids, subtle = globalThis.crypto?.subtle) {
  const sorted = [...new Set(ids)].sort();
  if (!sorted.length || !subtle) return '';
  const sum = new Uint8Array(await subtle.digest('SHA-256', new TextEncoder().encode(sorted.join('\n'))));
  return [...sum.slice(0, 16)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

// existingDrawing is the task's own file that shows this part of this
// board, which attaching updates: the same board, the same pick, no region.
export function existingDrawing(task, board, digest) {
  return (task?.attachments || []).find((a) => {
    const d = a?.drawing;
    return d && d.board === board && !d.region && (d.selection || '') === digest && !a.comment_id;
  }) || null;
}

// taskChoices are the open tasks across the boards this viewer reads, the
// drawing board's group first, matching query in title or id.
export function taskChoices(context, group, query) {
  const q = query.trim().toLowerCase();
  const rows = [];
  for (const g of context?.groups || []) {
    for (const t of g.tasks || []) {
      if (!OPEN.has(t.status)) continue;
      if (q && !t.title.toLowerCase().includes(q) && !t.id.toLowerCase().includes(q)) continue;
      rows.push({ ...t, group: t.group || g.group });
    }
  }
  const lane = (t) => (t.assignee || t.column ? 0 : t.stage === 'next' ? 1 : 2);
  return rows.sort((a, b) => (b.group === group) - (a.group === group) || lane(a) - lane(b) || a.title.localeCompare(b.title));
}

function laneOf(t) {
  if (t.column) return t.column;
  if (t.assignee) return 'now';
  return t.stage === 'next' ? 'next' : 'idea';
}

export function DrawingBoardAttachDialog({ request, close }) {
  const { drawingBoardId, title, group, selection, fetchImpl, notify } = request;
  const [context, setContext] = useState(null);
  const [query, setQuery] = useState('');
  const [chosen, setChosen] = useState(null);
  const [part, setPart] = useState(selection.length ? 'selection' : 'whole');
  const [preview, setPreview] = useState({ src: '', error: '' });
  const [digest, setDigest] = useState('');
  const [asComment, setAsComment] = useState(false);
  const [comment, setComment] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const searchRef = useRef(null);
  const elements = part === 'selection' ? selection : [];

  useEffect(() => {
    let live = true;
    fetchImpl('/v1/taskboard/context', { credentials: 'same-origin', cache: 'no-store' })
      .then(json).then((body) => { if (live) setContext(body); })
      .catch((cause) => { if (live) setError(`Could not read the task boards: ${cause?.message || cause}`); });
    return () => { live = false; };
  }, []);

  // The preview is the export itself, at one pixel per unit.
  useEffect(() => {
    let live = true;
    setPreview({ src: '', error: '' });
    fetchImpl(`/api/drawing-boards/${encodeURIComponent(drawingBoardId)}/export?format=json`, {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ format: 'png', scale: 1, ...(elements.length ? { elements } : {}) }),
    }).then(json).then((out) => { if (live) setPreview({ src: `data:${out.content_type};base64,${out.data}`, error: '' }); })
      .catch((cause) => { if (live) setPreview({ src: '', error: cause?.message || String(cause) }); });
    selectionDigest(elements).then((d) => { if (live) setDigest(d); });
    return () => { live = false; };
  }, [part]);

  const choices = useMemo(() => taskChoices(context, group, query), [context, group, query]);
  const task = chosen && choices.find((t) => t.id === chosen.id) || chosen;
  const existing = task && !asComment ? existingDrawing(task, drawingBoardId, digest) : null;

  const submit = async () => {
    if (busy || !task) return;
    setBusy(true);
    setError('');
    try {
      const body = { board: drawingBoardId, ...(elements.length ? { elements } : {}), ...(asComment ? { comment: comment.trim() || `${title || drawingBoardId}` } : {}) };
      const out = await json(await fetchImpl(`/v1/taskboard/tasks/${encodeURIComponent(task.id)}/drawings`, {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      }));
      const name = out?.attachment?.name || 'the drawing';
      notify(out?.comment ? `Commented on ${task.id} with ${name}.` : out?.replaced ? `Updated ${name} on ${task.id}.` : `Attached ${name} to ${task.id}.`);
      close();
    } catch (cause) {
      setBusy(false);
      setError(cause?.message || String(cause));
    }
  };

  const verb = asComment ? `Comment on ${task?.id || 'the task'}` : existing ? `Update on ${task.id}` : `Attach to ${task?.id || 'a task'}`;
  return html`<${Dialog} id="drawing-board-attach-modal" dialogClass="cron-create-modal drawing-board-attach-modal"
    title=${`Attach to a task · ${title || drawingBoardId}`} onClose=${close} onSubmitHotkey=${submit}
    busy=${busy} initialFocusRef=${searchRef} error=${error} cancel="Cancel"
    footer=${html`<${Button} variant="primary" id="drawing-board-attach-submit" disabled=${busy || !task || !!preview.error} busy=${busy}
      onClick=${() => { void submit(); }}>${busy ? 'Attaching…' : verb}<//>`}>
    <label class="cron-create-row">
      <span class="cron-create-label">Task</span>
      <${Input} domRef=${searchRef} id="drawing-board-attach-search" value=${query} placeholder="Find a task by title or id"
        autocomplete="off" spellcheck="false" aria-controls="drawing-board-attach-tasks"
        onInput=${(event) => setQuery(event.currentTarget.value)} />
    </label>
    <div class="drawing-board-attach-tasks" id="drawing-board-attach-tasks" role="listbox" aria-label="Tasks">
      ${!context && !error && html`<div class="ui-hint">Reading the task boards…</div>`}
      ${context && !choices.length && html`<div class="ui-hint">No open task matches.</div>`}
      ${choices.slice(0, SHOWN).map((t) => html`<button type="button" role="option" key=${t.id} data-task=${t.id}
        aria-selected=${task?.id === t.id ? 'true' : 'false'} class=${`drawing-board-attach-task${task?.id === t.id ? ' on' : ''}`}
        onClick=${() => setChosen(t)}>
        <span class="drawing-board-attach-title">${t.title}</span>
        <span class="drawing-board-attach-id">${t.id}${t.group !== group ? ` · ${t.group}` : ''}</span>
        <span class=${`drawing-board-attach-lane lane-${laneOf(t)}`}>${laneOf(t)}</span>
      </button>`)}
    </div>
    <div class="cron-create-row">
      <span class="cron-create-label">Draw</span>
      <div class="ui-seg" role="group" aria-label="What to draw">
        ${selection.length > 0 && html`<${Button} size="sm" data-part="selection" pressed=${part === 'selection'} class=${part === 'selection' ? 'active' : ''}
          onClick=${() => setPart('selection')}>Selection (${selection.length})<//>`}
        <${Button} size="sm" data-part="whole" pressed=${part === 'whole'} class=${part === 'whole' ? 'active' : ''}
          onClick=${() => setPart('whole')}>Whole board<//>
      </div>
    </div>
    <div class="drawing-board-attach-preview" aria-live="polite">
      ${preview.src ? html`<img src=${preview.src} alt="What is attached" />`
        : html`<span class="ui-hint">${preview.error ? `Cannot draw it: ${preview.error}` : 'Drawing…'}</span>`}
    </div>
    ${existing && html`<p class="ui-hint drawing-board-attach-existing">Already on ${task.id} as <b>${existing.name}</b>${existing.drawing?.version ? ` (version ${existing.drawing.version})` : ''}: attaching updates it.</p>`}
    <${Check} id="drawing-board-attach-as-comment" checked=${asComment} onChange=${(event) => setAsComment(event.currentTarget.checked)}>Post it as a new comment<//>
    ${asComment && html`<${Textarea} id="drawing-board-attach-comment" rows="2" value=${comment} placeholder="What the drawing shows (Markdown)"
      aria-label="Comment" onInput=${(event) => setComment(event.currentTarget.value)} />`}
  <//>`;
}

// DrawingBoardAttachHost shows the dialog a File menu asked for.
export function DrawingBoardAttachHost() {
  const request = attachDrawingRequest.value;
  if (!request) return null;
  const close = () => { if (attachDrawingRequest.peek() === request) attachDrawingRequest.value = null; };
  return html`<${DrawingBoardAttachDialog} key=${request.seq} request=${request} close=${close} />`;
}
