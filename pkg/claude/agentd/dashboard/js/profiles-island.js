import { Fragment, h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDialogFocus } from './dialog-focus.js';
import { relTime } from './helpers.js';

const html = htm.bind(h);
export const PROFILES_IDLE_POLL_MS = 15_000;
export const PROFILES_RUNNING_POLL_MS = 2_000;

// Viewer kinds, in the order the row offers them. Each opens pprof's own web
// UI for that profile in a new tab (served by the daemon behind dashboard
// auth, so it works over remote access too).
const VIEWERS = [
  { kind: 'cpu', label: 'CPU', title: 'CPU profile — where agentd spent CPU time during the capture window (opens pprof\'s flame graph)' },
  { kind: 'heap', label: 'heap', title: 'Live heap at the end of the capture' },
  { kind: 'allocs', label: 'allocs', title: 'Allocations since daemon start — high allocation rates drive GC CPU' },
  { kind: 'goroutine', label: 'goroutines', title: 'Goroutines at the end of the capture' },
  { kind: 'block', label: 'block', title: 'Where goroutines blocked (channel/select/sleep waits) during the capture' },
  { kind: 'mutex', label: 'mutex', title: 'Mutex contention during the capture' },
];

const DOWNLOADS = [
  { name: 'trace.out', label: 'trace', title: 'Execution trace — open with `go tool trace trace.out` to see wakeup patterns' },
  { name: 'goroutines.txt', label: 'stacks', title: 'Full goroutine stack dump (text)' },
  { name: 'stats-after.json', label: 'stats', title: 'tclaude counter snapshot at the end of the capture (JSON)' },
];

function fmtPct(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  return value >= 10 ? `${Math.round(value)}%` : `${value.toFixed(1)}%`;
}

function fmtRate(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  return value >= 10 ? `${Math.round(value)}/s` : `${value.toFixed(2)}/s`;
}

function profileBase(capture) {
  return `/api/profiles/${encodeURIComponent(capture.id)}`;
}

function hasFile(capture, name) {
  return Array.isArray(capture.files) && capture.files.some((file) => file.name === name && file.bytes > 0);
}

function hasKind(capture, kind) {
  return Array.isArray(capture.files) && capture.files.some((file) => file.kind === kind && file.bytes > 0);
}

function statusPill(capture, now) {
  switch (capture.status) {
    case 'running': {
      const end = Date.parse(capture.started_at) + capture.seconds * 1000;
      const left = Math.max(0, Math.ceil((end - now) / 1000));
      return html`<span class="state-pill state-working" title="Capture in progress">running · ${left}s left</span>`;
    }
    case 'done':
      return html`<span class="state-pill profile-done">done</span>`;
    case 'interrupted':
      return html`<span class="state-pill state-offline" title="The daemon or CLI stopped before this capture finished">interrupted</span>`;
    default:
      return html`<span class="state-pill state-awaiting" title=${capture.error || ''}>failed</span>`;
  }
}

function CaptureRow({ capture, actions, deleting, now }) {
  const base = profileBase(capture);
  const headline = capture.headline;
  const done = capture.status === 'done';
  const started = new Date(capture.started_at);
  return html`<tr data-key=${`profile-${capture.id}`}>
    <td title=${Number.isNaN(started.getTime()) ? '' : started.toLocaleString()}>
      <div>${relTime(capture.started_at)}</div>
      <div class="muted profile-id">${capture.id}</div>
    </td>
    <td>${capture.label || html`<span class="muted">—</span>`}<div class="muted">${capture.source} · ${capture.seconds}s${capture.trace_seconds ? ` + ${capture.trace_seconds}s trace` : ''}</div></td>
    <td>${statusPill(capture, now)}${capture.error ? html`<div class="muted profile-error">${capture.error}</div>` : null}</td>
    <td title="agentd's own CPU over the window, as top shows it (100% = one core)">${headline?.cpu_available ? fmtPct(headline.self_cpu_percent) : '—'}</td>
    <td title="CPU of child processes (tmux, git, …) that exited during the window — top shows these under their own names, not agentd">${headline?.cpu_available ? fmtPct(headline.child_cpu_percent) : '—'}</td>
    <td title="Instrumented subprocess starts (tmux) per second — see summary for the per-command breakdown">${headline ? fmtRate(headline.subprocesses_per_sec) : '—'}</td>
    <td class="profile-links">
      ${hasFile(capture, 'summary.txt') && html`<a href=${`${base}/files/summary.txt`} target="_blank" rel="noopener"
        title="Counters over the capture window: CPU split, GC rate, subprocess table">summary</a>`}
      ${VIEWERS.filter((viewer) => hasKind(capture, viewer.kind)).map((viewer) => html`<a key=${viewer.kind}
        href=${`${base}/ui/${viewer.kind}/`} target="_blank" rel="noopener" title=${viewer.title}>${viewer.label}</a>`)}
      ${DOWNLOADS.filter((file) => hasFile(capture, file.name)).map((file) => html`<a key=${file.name} class="muted"
        href=${`${base}/files/${file.name}?download=1`} title=${`${file.title} — download`}>${file.label} ⤓</a>`)}
    </td>
    <td>
      <button type="button" class="tool" disabled=${!done && capture.status === 'running' || deleting === capture.id}
        title="Delete this capture's files" aria-label=${`Delete capture ${capture.id}`}
        onClick=${() => actions.remove(capture)}>${deleting === capture.id ? '…' : '✕'}</button>
    </td>
  </tr>`;
}

function ProfileModal({ state, actions }) {
  const draft = state.view.value.modal;
  const secondsRef = useRef(null);
  const close = () => state.closeModal();
  const { dialogRef } = useDialogFocus({ open: !!draft, initialFocusRef: secondsRef, onEscape: close });
  if (!draft) return null;
  const max = state.view.value.defaults.max_seconds;
  const field = (key) => (event) => state.updateModal({ [key]: event.currentTarget.value, error: '' });
  return html`<div ref=${dialogRef} class="modal-overlay show" id="profile-modal"
    onClick=${(event) => { if (event.target === event.currentTarget) close(); }}>
    <form class="cron-create-modal" role="dialog" aria-modal="true" aria-labelledby="profile-modal-title"
      onSubmit=${(event) => { event.preventDefault(); void actions.start(); }}>
      <h3 id="profile-modal-title">Profile the daemon</h3>
      <p class="muted">Captures a CPU profile of this agentd process while it keeps running normally, plus heap,
        goroutine, block and mutex profiles and an execution trace. Run it while the CPU usage you want to explain is happening.
        Press Enter to start with these settings.</p>
      <label class="cron-create-row"><span class="cron-create-label">Duration</span>
        <input ref=${secondsRef} id="profile-modal-seconds" type="number" min="1" max=${max} step="1" inputmode="numeric"
          value=${draft.seconds} onInput=${field('seconds')} /> <span class="muted">seconds of CPU profile (max ${max})</span></label>
      <label class="cron-create-row"><span class="cron-create-label">Trace</span>
        <input id="profile-modal-trace" type="number" min="0" step="1" inputmode="numeric"
          value=${draft.trace_seconds} onInput=${field('trace_seconds')} /> <span class="muted">seconds of execution trace at the start (0 skips it)</span></label>
      <label class="cron-create-row"><span class="cron-create-label">Label</span>
        <input id="profile-modal-label" type="text" maxlength="80" placeholder="optional — e.g. 12 agents idle" autocomplete="off"
          value=${draft.label} onInput=${field('label')} /></label>
      <div class="cron-create-error" id="profile-modal-error" role=${draft.error ? 'alert' : undefined}>${draft.error}</div>
      <div class="modal-buttons">
        <button id="profile-modal-cancel" type="button" onClick=${close}>Cancel</button><span class="spacer"></span>
        <button id="profile-modal-submit" class="primary" type="submit" disabled=${draft.submitting}>
          ${draft.submitting ? 'Starting…' : 'Start profiling'}</button>
      </div>
    </form>
  </div>`;
}

function CaptureList({ current, actions, now }) {
  if (!current.loaded) {
    if (current.request.phase === 'error') {
      return html`<div class="island-error" role="alert">Failed to load profile captures: ${current.request.error}</div>`;
    }
    return html`<div class="empty">Loading profile captures…</div>`;
  }
  if (!current.captures.length) {
    return html`<div class="empty">No captures yet. Start one while agentd is busy to see where its CPU goes; captures from <code>tclaude agentd profile</code> appear here too.</div>`;
  }
  return html`<table class="debug-table profile-table">
    <thead><tr>
      <th scope="col">Started</th><th scope="col">Label</th><th scope="col">Status</th>
      <th scope="col">agentd CPU</th><th scope="col">child CPU</th><th scope="col">subproc</th>
      <th scope="col">Open</th><th scope="col" aria-label="Delete"></th>
    </tr></thead>
    <tbody>
      ${current.captures.map((capture) => html`<${CaptureRow} key=${capture.id} capture=${capture}
        actions=${actions} deleting=${current.deleting} now=${now} />`)}
    </tbody>
  </table>`;
}

export function ProfilesApp({
  state,
  actions,
  setTimeoutImpl = globalThis.setTimeout,
  clearTimeoutImpl = globalThis.clearTimeout,
  idlePollMs = PROFILES_IDLE_POLL_MS,
  runningPollMs = PROFILES_RUNNING_POLL_MS,
}) {
  const current = state.view.value;
  const [now, setNow] = useState(() => Date.now());
  const running = !!current.running;
  useEffect(() => {
    if (!current.active) {
      actions.cancel();
      return undefined;
    }
    let timer = null;
    let stopped = false;
    const tick = async () => {
      await actions.load();
      if (stopped) return;
      setNow(Date.now());
      timer = setTimeoutImpl(tick, state.view.value.running ? runningPollMs : idlePollMs);
    };
    void tick();
    return () => {
      stopped = true;
      if (timer !== null) clearTimeoutImpl(timer);
      actions.cancel();
    };
  }, [current.active, running, actions, state, setTimeoutImpl, clearTimeoutImpl, idlePollMs, runningPollMs]);

  return html`<${Fragment}>
    <div class="profile-section">
      <div class="filter-bar">
        <strong>Daemon profiling</strong>
        <span class="muted">CPU/heap/goroutine profiles of this agentd process, viewable in pprof's web UI.</span>
        <span class="spacer"></span>
        <button class="tool" id="profile-start" type="button" disabled=${running}
          title=${running ? 'A capture is already running' : 'Capture a CPU profile and friends of this daemon'}
          onClick=${() => state.openModal()}>${running ? '⏱ profiling…' : '⏱ Profile daemon…'}</button>
      </div>
      ${current.notice && html`<div class="muted profile-notice" aria-live="polite">${current.notice}</div>`}
      ${current.loaded && current.request.phase === 'error' && html`<div class="island-error" role="alert">Refresh failed: ${current.request.error}</div>`}
      <div id="profile-list"><${CaptureList} current=${current} actions=${actions} now=${now} /></div>
    </div>
    <${ProfileModal} state=${state} actions=${actions} />
  </${Fragment}>`;
}

export function mountProfilesIsland({ host, state, actions, registerCleanup, setTimeoutImpl, clearTimeoutImpl }) {
  render(html`<${ProfilesApp} state=${state} actions=${actions}
    setTimeoutImpl=${setTimeoutImpl} clearTimeoutImpl=${clearTimeoutImpl} />`, host);
  registerCleanup(() => {
    actions.dispose();
    render(null, host);
  });
}
