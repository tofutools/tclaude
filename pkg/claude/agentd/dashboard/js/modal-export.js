// modal-export.js — the per-agent "📋 summary…" export modal (JOH-265).
//
// Asks a LIVE agent to consolidate a shareable artifact (a summary / report,
// one or more files auto-zipped) and downloads it here. The async, curated
// twin of the group's mechanical "⤓ export". Flow:
//
//   1. The modal collects a format preset, an optional title, and free-text
//      instructions, then POSTs /api/agents/{conv}/export → an export job id.
//   2. The form is replaced by a spinner; the modal polls
//      /api/export-jobs/{id} every 2s.
//   3. On `ready` it triggers a browser download of the artifact, swaps the
//      spinner for a success line, and offers a "Download again" button.
//      On `failed` it shows the reason. The user can Close at any point — the
//      job keeps running server-side and the artifact persists (TTL) for a
//      later re-download via the row's menu.

import { $, shortId, bindModalSubmitHotkey } from './helpers.js';
import { toast, bindBackdropDiscard } from './refresh.js';

const POLL_INTERVAL_MS = 2000;
// After this long with no result, nudge the human that the agent may be busy —
// the poll keeps going (the daemon eventually times the job out server-side).
const SLOW_NOTE_AFTER_MS = 90000;

// Preset → seed text for the instructions box. Selecting a preset fills the
// textarea (until the human edits it by hand, after which we stop clobbering).
const PRESETS = {
  summary:
    'Produce a concise, shareable summary of this conversation as a single ' +
    'Markdown file. Lead with the key findings / outcomes, then the supporting ' +
    'detail. Write for someone who was not here: spell out the context and avoid ' +
    'internal shorthand. Keep it self-contained.',
  detailed:
    'Produce a thorough, well-structured report of this conversation as Markdown ' +
    '(split into several files if that reads better — they will be zipped). ' +
    'Cover background, what was done, findings, supporting evidence / links, and ' +
    'next steps. Write for an outside reader who needs the full picture.',
  custom: '',
};

// Poll handle + whether the human has hand-edited the instructions, so a
// preset change does not overwrite their text.
let pollTimer = null;
let instructionsSeeded = false;

function openExportModal(conv, label) {
  clearPoll();
  const modal = $('#export-agent-modal');
  modal.dataset.conv = conv;
  modal.dataset.label = label || '';
  modal.dataset.jobId = '';

  $('#export-agent-meta').textContent = label ? `target: ${label}` : `target: ${shortId(conv)}`;
  $('#export-agent-preset').value = 'summary';
  $('#export-agent-title-input').value = '';
  $('#export-agent-instructions').value = PRESETS.summary;
  instructionsSeeded = true;
  $('#export-agent-error').textContent = '';
  $('#export-agent-status-note').textContent = '';

  // Reset to the form phase.
  $('#export-agent-form').hidden = false;
  $('#export-agent-status').hidden = true;
  const submitBtn = $('#export-agent-submit');
  submitBtn.hidden = false;
  submitBtn.disabled = false;
  submitBtn.textContent = 'Export';
  $('#export-agent-download').hidden = true;
  $('#export-agent-cancel').textContent = 'Cancel';

  modal.classList.add('show');
  setTimeout(() => $('#export-agent-instructions').focus(), 0);
}

function closeExportModal() {
  clearPoll();
  $('#export-agent-modal').classList.remove('show');
}

function clearPoll() {
  if (pollTimer) {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
}

async function readError(r) {
  const t = await r.text();
  try {
    const j = JSON.parse(t);
    if (j && j.error) return j.error;
  } catch (_) { /* not JSON */ }
  return t || `HTTP ${r.status}`;
}

async function submitExport() {
  const modal = $('#export-agent-modal');
  // Ignore a stray submit (e.g. the Ctrl/Cmd+Enter hotkey) once we've left
  // the form for the working phase — the request is already in flight.
  if ($('#export-agent-form').hidden) return;
  const conv = modal.dataset.conv;
  const errEl = $('#export-agent-error');
  errEl.textContent = '';

  const body = {
    preset: $('#export-agent-preset').value,
    title: $('#export-agent-title-input').value.trim(),
    instructions: $('#export-agent-instructions').value.trim(),
  };

  const submitBtn = $('#export-agent-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = 'Requesting…';
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/export`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      errEl.textContent = await readError(r);
      submitBtn.disabled = false;
      submitBtn.textContent = 'Export';
      return;
    }
    const job = await r.json();
    modal.dataset.jobId = String(job.id);
    enterWorkingPhase();
    startPoll(job.id);
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
    submitBtn.disabled = false;
    submitBtn.textContent = 'Export';
  }
}

// enterWorkingPhase swaps the form for the spinner + status line.
function enterWorkingPhase() {
  $('#export-agent-form').hidden = true;
  const submitBtn = $('#export-agent-submit');
  submitBtn.hidden = true;
  submitBtn.disabled = true;
  $('#export-agent-status').hidden = false;
  $('#export-agent-cancel').textContent = 'Close';
  $('#export-agent-spinner').style.display = '';
  const row = $('#export-agent-status').querySelector('.export-status-row');
  if (row) row.classList.remove('done');
  setStatus('Waiting for the agent to pick up the request…');
  $('#export-agent-status-note').textContent = '';
}

function setStatus(text) {
  $('#export-agent-status-text').textContent = text;
}

function startPoll(jobId) {
  clearPoll();
  const startedAt = Date.now();
  const tick = async () => {
    const modal = $('#export-agent-modal');
    // Bail if the modal was closed or reused for another job.
    if (!modal.classList.contains('show') || modal.dataset.jobId !== String(jobId)) {
      return;
    }
    try {
      const r = await fetch(`/api/export-jobs/${encodeURIComponent(jobId)}`, { credentials: 'same-origin' });
      if (r.ok) {
        const job = await r.json();
        if (job.status === 'ready') {
          onExportReady(jobId, job);
          return;
        }
        if (job.status === 'failed') {
          onExportFailed(job);
          return;
        }
        setStatus(job.status === 'running'
          ? 'The agent is producing your export…'
          : 'Waiting for the agent to pick up the request…');
        if (Date.now() - startedAt > SLOW_NOTE_AFTER_MS) {
          $('#export-agent-status-note').textContent =
            'Still working — the agent may be busy with another task. You can close this; the file will be ready to download from the agent’s menu when it lands.';
        }
      } else if (r.status === 404) {
        onExportFailed({ error: 'the export job is no longer available' });
        return;
      }
      // Other transient errors: keep polling.
    } catch (_) {
      // Network hiccup — keep polling.
    }
    pollTimer = setTimeout(tick, POLL_INTERVAL_MS);
  };
  pollTimer = setTimeout(tick, POLL_INTERVAL_MS);
}

function onExportReady(jobId, job) {
  clearPoll();
  const row = $('#export-agent-status').querySelector('.export-status-row');
  if (row) row.classList.add('done');
  $('#export-agent-spinner').style.display = 'none';
  const name = job.artifact_name || 'export';
  setStatus(`✅ Export ready: ${name}`);
  $('#export-agent-status-note').textContent = 'Downloaded. Use “Download again” if your browser blocked it.';
  triggerDownload(jobId);
  const dl = $('#export-agent-download');
  dl.hidden = false;
  dl.textContent = 'Download again';
  dl.onclick = () => triggerDownload(jobId);
  const label = $('#export-agent-modal').dataset.label || shortId($('#export-agent-modal').dataset.conv);
  toast(`Export ready for ${label}`);
}

function onExportFailed(job) {
  clearPoll();
  const row = $('#export-agent-status').querySelector('.export-status-row');
  if (row) row.classList.add('done');
  $('#export-agent-spinner').style.display = 'none';
  setStatus('⚠️ Export failed');
  $('#export-agent-status-note').textContent = job.error || 'the agent did not deliver an export';
}

function triggerDownload(jobId) {
  const a = document.createElement('a');
  a.href = `/api/export-jobs/${encodeURIComponent(jobId)}/artifact`;
  a.download = '';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

function bindExportModal() {
  $('#export-agent-cancel').addEventListener('click', closeExportModal);
  $('#export-agent-submit').addEventListener('click', submitExport);

  // Preset change seeds the instructions until the human edits them by hand.
  $('#export-agent-preset').addEventListener('change', (e) => {
    const ta = $('#export-agent-instructions');
    if (!instructionsSeeded && ta.value.trim() !== '') return;
    ta.value = PRESETS[e.target.value] || '';
    instructionsSeeded = true;
  });
  $('#export-agent-instructions').addEventListener('input', () => { instructionsSeeded = false; });

  bindModalSubmitHotkey($('#export-agent-modal'), $('#export-agent-submit'));
  bindBackdropDiscard('export-agent-modal', closeExportModal);
}

export { openExportModal, bindExportModal };
