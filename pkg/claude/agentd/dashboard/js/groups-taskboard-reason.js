// groups-taskboard-reason.js -- the dialog the task board asks a reason
// through: Block… and Drop… on a List row's ⋯, and a Lanes drag onto a
// Blocked column (tcl-xjl81y). It is the dashboard's standard Dialog
// (ui-dialog.js), as the New task form and the name-it-first dialog are, so
// the board asks the way the rest of the Groups tab does instead of opening
// a field under the row.
//
// One Reason field, a resizable textarea (a reason can be nuanced, so it
// may take several lines), focused on open. Ctrl/Cmd+Enter submits through
// the dialog's submit hotkey, Enter adds a line, Escape or Cancel closes
// without a write. A required reason (a block) keeps the submit disabled
// while it is empty or only whitespace. onSubmit(reason) runs the
// write and resolves true when it landed: the dialog closes; on false (the
// write was refused and said so) it stays, with the reason kept, for a retry.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Dialog } from './ui-dialog.js';
import { Button, Textarea } from './ui-components.js';

const html = htm.bind(h);

// REASON_COPY is each kind's words: block needs a reason, drop takes one.
const REASON_COPY = Object.freeze({
  block: {
    title: 'Block task', ok: 'Block', busy: 'Blocking…', required: true, danger: false,
    label: 'Block reason', placeholder: 'why is it blocked? (required)',
    line: 'The reason shows on the card, and the task waits in Blocked.',
  },
  drop: {
    title: 'Drop task', ok: 'Drop', busy: 'Dropping…', required: false, danger: true,
    label: 'Drop reason', placeholder: 'why drop it? (optional, e.g. "parked in awb")',
    line: 'It comes off the board; park it in the tracker first if it is still wanted.',
  },
});

// TaskReasonDialog asks the reason for `kind` ('block' or 'drop') on task.
// `action` overrides the words of what runs (a Lanes drag that assigns
// first: "assign to builder · block"). busy is the board's own busy flag
// for this write.
export function TaskReasonDialog({ kind, task, action = '', busy = false, onSubmit, onClose }) {
  const copy = REASON_COPY[kind] || REASON_COPY.block;
  const [reason, setReason] = useState('');
  const inputRef = useRef(null);
  const sending = useRef(false);
  const invalid = copy.required && !reason.trim();
  const submit = async () => {
    if (invalid || busy || sending.current) {
      inputRef.current?.focus();
      return;
    }
    sending.current = true;
    try {
      if (await onSubmit(reason.trim())) onClose();
      else inputRef.current?.focus();
    } finally {
      sending.current = false;
    }
  };
  return html`<${Dialog} id="taskboard-reason-modal" dialogClass="cron-create-modal taskboard-reason-modal"
    title=${copy.title} onClose=${onClose} onSubmitHotkey=${() => void submit()}
    busy=${busy} initialFocusRef=${inputRef} cancel="Cancel"
    footer=${html`<${Button} variant=${copy.danger ? 'danger' : 'primary'} id="taskboard-reason-submit" busy=${busy}
      disabled=${busy || invalid} onClick=${() => { void submit(); }}>${busy ? copy.busy : copy.ok}<//>`}>
    <div class="modal-meta tb-reason-task" data-task-form=${kind}>${action ? `${action} ` : ''}“${task.title || task.id}”</div>
    <p class="modal-hint ui-hint">${copy.line}</p>
    <div class="cron-create-row tb-new-row"><span class="cron-create-label">Reason</span>
      <span class="tb-new-field">
        <${Textarea} domRef=${inputRef} class="tb-form-reason" rows=${4} aria-label=${copy.label} placeholder=${copy.placeholder}
          aria-describedby="taskboard-reason-hint" value=${reason} maxLength=${400} disabled=${busy} required=${copy.required}
          onInput=${(event) => setReason(event.currentTarget.value)} />
        <span class="ui-hint tb-new-hint" id="taskboard-reason-hint">${copy.ok} with Ctrl/Cmd+Enter · Enter adds a line · Esc cancels</span>
      </span>
    </div>
  <//>`;
}
