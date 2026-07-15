// Race-free human-to-agent composer for integrated terminal panes. Message
// bodies never enter xterm; submit uploads files, then enqueues one durable
// senderless inbox row through /api/operator-message.

let bound = false;
let target = null;
let files = [];
let pending = false;

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
    remove.setAttribute('aria-label', `Remove ${name.textContent}`);
    remove.addEventListener('click', () => { files.splice(index, 1); renderFiles(); });
    li.append(name, size, remove);
    list.append(li);
  });
}

function addFiles(incoming) {
  for (const file of incoming || []) {
    if (file && files.length < 8) files.push(file);
  }
  renderFiles();
}

function close() {
  if (pending) return;
  el('operator-message-modal')?.classList.remove('show');
  files = [];
  target = null;
  renderFiles();
}

async function upload() {
  if (!files.length) return '';
  const form = new FormData();
  files.forEach((file, index) => form.append('file', file, file.name || `pasted-image-${index + 1}.png`));
  const res = await fetch('/api/spawn-attachments', {
    method: 'POST', credentials: 'same-origin', body: form,
  });
  const payload = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(payload.error || `attachment upload failed (HTTP ${res.status})`);
  return payload.token || '';
}

async function submit() {
  if (pending || !target) return;
  const body = el('operator-message-body').value;
  if (!body.trim() && !files.length) {
    el('operator-message-error').textContent = 'Write a message or attach a file.';
    return;
  }
  pending = true;
  const button = el('operator-message-submit');
  button.disabled = true;
  button.textContent = 'Queueing…';
  el('operator-message-error').textContent = '';
  try {
    const attachmentToken = await upload();
    const res = await fetch('/api/operator-message', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        to: target.agent,
        subject: el('operator-message-subject').value,
        body,
        attachment_token: attachmentToken,
      }),
    });
    const payload = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(payload.error || `send failed (HTTP ${res.status})`);
    pending = false;
    close();
  } catch (error) {
    el('operator-message-error').textContent = error.message || String(error);
    pending = false;
  } finally {
    button.disabled = false;
    button.textContent = 'Send';
  }
}

export function openOperatorMessageModal(nextTarget) {
  if (!nextTarget || !nextTarget.agent) return;
  target = nextTarget;
  files = [];
  pending = false;
  el('operator-message-to').textContent = nextTarget.label || nextTarget.agent;
  el('operator-message-to').title = nextTarget.agent;
  el('operator-message-subject').value = '';
  el('operator-message-body').value = '';
  el('operator-message-error').textContent = '';
  renderFiles();
  el('operator-message-modal').classList.add('show');
  setTimeout(() => el('operator-message-body').focus(), 0);
}

export function bindOperatorMessageModal() {
  if (bound || !el('operator-message-modal')) return;
  bound = true;
  el('operator-message-cancel').addEventListener('click', close);
  el('operator-message-submit').addEventListener('click', submit);
  el('operator-message-attach-btn').addEventListener('click', () => el('operator-message-attach-input').click());
  el('operator-message-attach-input').addEventListener('change', (event) => {
    addFiles(event.target.files);
    event.target.value = '';
  });
  const modal = el('operator-message-modal');
  modal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') { event.preventDefault(); close(); return; }
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void submit();
    }
  });
  modal.addEventListener('paste', (event) => {
    const pasted = [...(event.clipboardData?.files || [])];
    if (!pasted.length) return;
    event.preventDefault();
    addFiles(pasted);
  });
  modal.addEventListener('dragover', (event) => {
    if (event.dataTransfer?.types?.includes('Files')) event.preventDefault();
  });
  modal.addEventListener('drop', (event) => {
    const dropped = [...(event.dataTransfer?.files || [])];
    if (!dropped.length) return;
    event.preventDefault();
    addFiles(dropped);
  });
  modal.addEventListener('click', (event) => { if (event.target === modal) close(); });
}
