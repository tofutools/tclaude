// export-progress.js — the shared progress model for per-agent export jobs
// (the "📋 summary…" export, JOH-265/JOH-266): one ordered step list, rendered
// two ways — a vertical checklist in the export modal's working phase, and a
// compact horizontal stepper on the Jobs tab's "Agent exports" rows.
//
// The job's status ladder (cloning → requested → running → ready | failed) is
// MONOTONIC, so each step's state (done / active / pending) is derived
// ORDINALLY from the latest polled status instead of accumulated from events —
// a job that races through 'requested' between two 2s polls still renders the
// earlier steps checked. 'failed' carries no phase information of its own; the
// modal remembers the last non-terminal status it saw so the ✗ lands on the
// step that was actually in flight (see renderExportChecklist's failedAt).

import { esc } from './helpers.js';

// The steps, in ladder order. `label` is the modal checklist line; `short` is
// the Jobs-tab stepper chip. The first three are activities keyed by the job
// status while they run; 'ready' is the terminal landing step.
const EXPORT_STEPS = [
  { key: 'cloning', label: 'Cloning the conversation (isolated summary writer)', short: 'clone' },
  { key: 'requested', label: 'Handing the brief to the summary writer', short: 'brief' },
  { key: 'running', label: 'Writing the export', short: 'write' },
  { key: 'ready', label: 'Export ready', short: 'ready' },
];

// activeExportStepIndex maps a job status to the index of the step currently
// in flight. 'ready' maps past the end (every step done); an unknown status —
// including 'failed' — maps to -1 and the caller decides how to render it.
function activeExportStepIndex(status) {
  if (status === 'ready') return EXPORT_STEPS.length;
  return EXPORT_STEPS.findIndex(s => s.key === status);
}

// exportSpinnerHTML is the shared spinning-circle markup. The inline negative
// animation-delay re-phases the spin to wall-clock so the Jobs tab's 2s
// wholesale innerHTML re-render doesn't visibly restart the animation from 0°
// every poll (the CSS animation is 0.8s — see .export-spinner + the
// dashboard-poll-restarts-CSS-animations note in helpers.syncBotAnimations).
function exportSpinnerHTML() {
  return `<span class="export-spinner" style="animation-delay:-${Date.now() % 800}ms" aria-hidden="true"></span>`;
}

// renderExportChecklist renders the modal's vertical step list for `status`.
// `failedAt` applies only when status === 'failed': the last non-terminal
// status the caller saw, so the ✗ marks the step that was in flight. A failed
// job with no failedAt (nothing remembered) marks the first step — the
// earliest the job can have died.
function renderExportChecklist(status, failedAt) {
  const failed = status === 'failed';
  const active = failed
    ? Math.max(activeExportStepIndex(failedAt), 0)
    : activeExportStepIndex(status);
  return `<div class="export-checklist">` + EXPORT_STEPS.map((s, i) => {
    let cls = 'pending';
    let icon = '<span class="export-step-icon" aria-hidden="true">·</span>';
    if (i < active) {
      cls = 'done';
      icon = '<span class="export-step-icon" aria-hidden="true">✓</span>';
    } else if (i === active) {
      if (failed) {
        cls = 'failed';
        icon = '<span class="export-step-icon" aria-hidden="true">✗</span>';
      } else {
        cls = 'active';
        icon = exportSpinnerHTML();
      }
    }
    return `<div class="export-step ${cls}">${icon}<span class="export-step-label">${esc(s.label)}</span></div>`;
  }).join('') + `</div>`;
}

// triggerExportDownload starts a browser download of a ready job's artifact.
// Shared by the export modal (auto-download + "Download again" + history) and
// the Jobs tab's per-row download button.
function triggerExportDownload(jobId) {
  const a = document.createElement('a');
  a.href = `/api/export-jobs/${encodeURIComponent(jobId)}/artifact`;
  a.download = '';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

// fmtBytes renders an artifact size for humans. Shared by the export modal's
// history panel and the Jobs tab's export rows.
function fmtBytes(n) {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KiB`;
  return `${n} B`;
}

export {
  EXPORT_STEPS, activeExportStepIndex,
  renderExportChecklist, triggerExportDownload, fmtBytes,
};
