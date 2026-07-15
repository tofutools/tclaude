// Race-free human-to-agent composer for integrated terminal panes. Message
// bodies never enter xterm; submit uploads files, then enqueues one durable
// senderless inbox row through /api/operator-message.

import { makeModalResizable } from './helpers.js';
import { snapshotOperatorMessageDraft } from './operator-message-model.js';
import { bindBackdropDiscard } from './refresh.js';

let bound = false;
let target = null;
let files = [];
let pending = false;
let dismissGuard = null;

const el = (id) => document.getElementById(id);

function renderFiles() {
  const list = el('operator-message-attachments-list');
  if (!list) return;
  list.innerHTML = '';
  files.forEach((file, index) => {
    const li = document.createElement('li');
    const name = document.createElement('span');
    name.className = 'att-name';
    name.textContent = file.name || `pasted-image-${index + 1}.png`;
    const size = document.createElement('span');
    size.className = 'att-size';
    size.textContent = `${file.size} B`;
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'att-remove';
    remove.textContent = '✕';
    remove.disabled = pending;
    remove.setAttribute('aria-label', `Remove ${name.textContent}`);
    remove.addEventListener('click', () => {
      if (pending) return;
      files.splice(index, 1);
      dismissGuard?.markDirty();
      renderFiles();
    });
    li.append(name, size, remove);
    list.append(li);
  });
}

function addFiles(incoming) {
  if (pending) return;
  const before = files.length;
  for (const file of incoming || []) {
    if (file && files.length < 8) files.push(file);
  }
  if (files.length !== before) dismissGuard?.markDirty();
  renderFiles();
}

function close() {
  if (pending) return;
  el('operator-message-modal')?.classList.remove('show');
  files = [];
  target = null;
  renderFiles();
}

async function upload(batch) {
  if (!batch.length) return '';
  const form = new FormData();
  batch.forEach((file, index) => form.append('file', file, file.name || `pasted-image-${index + 1}.png`));
  const res = await fetch('/api/spawn-attachments', {
    method: 'POST', credentials: 'same-origin', body: form,
  });
  const payload = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(payload.error || `attachment upload failed (HTTP ${res.status})`);
  return payload.token || '';
}

function setPending(next) {
  pending = next;
  // Keep the focused editor in the tab/focus chain so a failed send restores
  // the draft at the same caret. disabled would blur it in most browsers.
  el('operator-message-subject').readOnly = next;
  el('operator-message-body').readOnly = next;
  for (const id of [
    'operator-message-attach-btn', 'operator-message-attach-input',
    'operator-message-cancel', 'operator-message-submit',
  ]) {
    el(id).disabled = next;
  }
  el('operator-message-attachments-list').querySelectorAll('.att-remove')
    .forEach((button) => { button.disabled = next; });
  const button = el('operator-message-submit');
  button.querySelector('.theme-copy-regular').textContent = next ? 'Queueing…' : 'Send';
  button.querySelector('.theme-copy-wizard').textContent = next ? '✒ Sending missive…' : '✒ Send missive';
}

async function submit() {
  if (pending || !target) return;
  const draft = snapshotOperatorMessageDraft({
    target,
    subject: el('operator-message-subject').value,
    body: el('operator-message-body').value,
    files,
  });
  if (!draft.body.trim() && !draft.files.length) {
    el('operator-message-error').textContent = 'Write a message or attach a file.';
    return;
  }
  setPending(true);
  el('operator-message-error').textContent = '';
  try {
    const attachmentToken = await upload(draft.files);
    const res = await fetch('/api/operator-message', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        to: draft.to,
        subject: draft.subject,
        body: draft.body,
        attachment_token: attachmentToken,
      }),
    });
    const payload = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(payload.error || `send failed (HTTP ${res.status})`);
    setPending(false);
    close();
  } catch (error) {
    el('operator-message-error').textContent = error.message || String(error);
  } finally {
    setPending(false);
  }
}

export function openOperatorMessageModal(nextTarget) {
  if (pending || !nextTarget || !nextTarget.agent) return;
  target = nextTarget;
  files = [];
  pending = false;
  el('operator-message-to').textContent = nextTarget.label || nextTarget.agent;
  el('operator-message-to').title = nextTarget.agent;
  el('operator-message-subject').value = '';
  el('operator-message-body').value = '';
  el('operator-message-error').textContent = '';
  el('operator-message-submit').querySelector('.theme-copy-regular').textContent = 'Send';
  el('operator-message-submit').querySelector('.theme-copy-wizard').textContent = '✒ Send missive';
  renderFiles();
  el('operator-message-modal').classList.add('show');
  setTimeout(() => el('operator-message-body').focus(), 0);
}

export function bindOperatorMessageModal() {
  if (bound || !el('operator-message-modal')) return;
  bound = true;
  dismissGuard = bindBackdropDiscard('operator-message-modal', close, () => !pending);
  el('operator-message-cancel').addEventListener('click', () => { void dismissGuard.tryDismiss(); });
  el('operator-message-submit').addEventListener('click', submit);
  el('operator-message-attach-btn').addEventListener('click', () => el('operator-message-attach-input').click());
  el('operator-message-attach-input').addEventListener('change', (event) => {
    addFiles(event.target.files);
    event.target.value = '';
  });
  const modal = el('operator-message-modal');
  modal.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void submit();
    }
  });
  modal.addEventListener('paste', (event) => {
    const pasted = [...(event.clipboardData?.files || [])];
    if (!pasted.length) return;
    event.preventDefault();
    if (pending) return;
    addFiles(pasted);
  });
  modal.addEventListener('dragover', (event) => {
    if (event.dataTransfer?.types?.includes('Files')) event.preventDefault();
  });
  modal.addEventListener('drop', (event) => {
    const dropped = [...(event.dataTransfer?.files || [])];
    if (!dropped.length) return;
    event.preventDefault();
    if (pending) return;
    addFiles(dropped);
  });
  makeModalResizable(
    modal.querySelector('.cron-create-modal'),
    'tclaude.dash.modalSize.operator-message',
  );
}
