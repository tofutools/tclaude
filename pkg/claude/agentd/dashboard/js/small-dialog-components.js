import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { exportChecklistSteps, fmtBytes } from './export-progress.js';
import { relTime, shortId } from './helpers.js';

const html = htm.bind(h);

const EXPORT_PRESETS = Object.freeze({
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
});

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function errorMessage(error) { return error?.message || String(error); }

export function PresetCloneDialog({ descriptor, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const suggested = `${descriptor.source.name}-copy`;
  const [name, setName] = useState(suggested);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const submitGuard = useRef(false);
  const submit = async () => {
    if (submitGuard.current) return;
    submitGuard.current = true;
    setError('');
    setBusy(true);
    try {
      await actions.clonePreset({
        source: descriptor.source,
        create: descriptor.create,
        name,
      }, descriptor);
    } catch (cause) { setError(errorMessage(cause)); }
    finally {
      submitGuard.current = false;
      setBusy(false);
    }
  };
  return html`
    <${Overlay}
      id="clone-modal"
      labelledby="clone-modal-title"
      onClose=${() => actions.close(descriptor)}
      dirty=${name !== suggested}
      blocked=${busy}
      confirmDiscard=${confirmDiscard}
      registerClose=${registerClose}
    >
      <h3 id="clone-modal-title"><${Words}
        plain=${`Clone ${descriptor.presetKind}: ${descriptor.source.name}`}
        wizard=${`Mirror ${descriptor.kindWizard}: ${descriptor.source.name}`}
      /></h3>
      <div class="modal-meta" id="clone-modal-blurb"><${Words}
        plain="A full copy under a new name — every setting is carried over; unique aliases stay with the original."
        wizard="An identical twin under a new name — every rune is carried over; unique true names stay with the original."
      /></div>
      <label class="cron-create-row">
        <span class="cron-create-label">New name</span>
        <input
          id="clone-modal-name"
          type="text"
          value=${name}
          placeholder="kebab-or-snake-case label for the copy"
          autocomplete="off"
          spellcheck="false"
          data-select-on-focus
          onInput=${(event) => setName(event.currentTarget.value)}
          onKeyDown=${(event) => {
            if (event.key !== 'Enter' || event.isComposing) return;
            event.preventDefault();
            void submit();
          }}
        />
      </label>
      <div class="cron-create-error" id="clone-modal-error" role="alert">${error}</div>
      <div class="modal-buttons">
        <button id="clone-modal-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
        <span class="spacer"></span>
        <button id="clone-modal-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>
          <${Words} plain=${busy ? 'Creating…' : 'Create copy'} wizard=${busy ? 'Mirroring…' : 'Mirror it'}/>
        </button>
      </div>
    </${Overlay}>
  `;
}

function ExportChecklist({ status, failedAt }) {
  return html`<div class="export-checklist">
    ${exportChecklistSteps(status, failedAt).map((step) => html`
      <div key=${step.key} class=${`export-step ${step.state}`}>
        ${step.state === 'active'
          ? html`<span class="export-spinner" style=${`animation-delay:-${Date.now() % 800}ms`} aria-hidden="true"></span>`
          : html`<span class="export-step-icon" aria-hidden="true">${step.icon}</span>`}
        <span class="export-step-label">${step.label}</span>
      </div>
    `)}
  </div>`;
}

function ExportHistory({ jobs, actions, conv, refreshHistory }) {
  if (!jobs.length) return null;
  const mutate = async (operation) => {
    try { await operation(); } catch (_) { /* history controls remain best-effort */ }
    refreshHistory();
  };
  return html`
    <div id="export-agent-history" class="export-history">
      <div class="export-history-head">
        <span class="export-history-title">Previous exports</span>
        <button
          id="export-agent-clear"
          type="button"
          class="export-history-clear"
          title="Delete every saved export for this agent"
          onClick=${() => mutate(() => actions.clearExports(conv))}
        >Clear all</button>
      </div>
      <div id="export-agent-history-list" class="export-history-list">
        ${jobs.map((job) => {
          const name = job.title || job.artifact_name;
          const when = job.created_at ? relTime(job.created_at) : '';
          const size = job.artifact_size ? ` · ${fmtBytes(job.artifact_size)}` : '';
          return html`
            <div key=${job.id} class="export-history-item">
              <div class="ehi-main">
                <div class="ehi-title">${name || html`<span class="ehi-sub">(${job.preset || 'untitled'})</span>`}</div>
                <div class="ehi-sub">${when}${size}</div>
              </div>
              <span class=${`ehi-status ${job.status || ''}`}>${job.status || ''}</span>
              ${job.ready && html`<button data-export-act="download" title="Download this export" aria-label="Download this export" onClick=${() => actions.downloadExport(job.id)}>⤓</button>`}
              <button class="ehi-del" data-export-act="delete" title="Delete this export" aria-label="Delete this export" onClick=${() => mutate(() => actions.deleteExport(job.id))}>🗑</button>
            </div>
          `;
        })}
      </div>
    </div>
  `;
}

export function AgentExportDialog({ descriptor, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [preset, setPreset] = useState('summary');
  const [title, setTitle] = useState('');
  const [instructions, setInstructions] = useState(EXPORT_PRESETS.summary);
  const [sameGroup, setSameGroup] = useState(false);
  const [phase, setPhase] = useState('form');
  const [jobID, setJobID] = useState(null);
  const [status, setStatus] = useState('cloning');
  const [lastActive, setLastActive] = useState('cloning');
  const [note, setNote] = useState({ plain: '', wizard: '' });
  const [error, setError] = useState('');
  const [history, setHistory] = useState([]);
  const [historyVersion, setHistoryVersion] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const submitGuard = useRef(false);
  const instructionsEdited = useRef(false);
  const requestController = useRef(null);
  const textareaRef = useRef(null);

  useEffect(() => {
    const timer = setTimeout(() => textareaRef.current?.focus(), 0);
    return () => clearTimeout(timer);
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    actions.loadExportHistory(descriptor.conv, { signal: controller.signal }).then(
      setHistory,
      (cause) => { if (cause?.name !== 'AbortError') setHistory([]); },
    );
    return () => controller.abort();
  }, [descriptor.conv, historyVersion]);
  useEffect(() => {
    if (!jobID || phase !== 'working') return undefined;
    return actions.watchExport(jobID, {
      onStatus(next) {
        setStatus(next);
        if (next !== 'ready' && next !== 'failed') setLastActive(next);
      },
      onReady(job) {
        setStatus('ready');
        setPhase('ready');
        const name = job.artifact_name || 'export';
        const readyNote = `Downloaded ${name}. Use “Download again” if your browser blocked it.`;
        setNote({ plain: readyNote, wizard: readyNote });
        actions.downloadExport(jobID);
        actions.exportReady(descriptor.label || shortId(descriptor.conv));
        setHistoryVersion((value) => value + 1);
      },
      onFailed(job) {
        setStatus('failed');
        setPhase('failed');
        setNote(job.error
          ? { plain: `⚠️ ${job.error}`, wizard: `⚠️ ${job.error}` }
          : {
              plain: '⚠️ the agent did not deliver an export',
              wizard: '⚠️ the familiar did not deliver its scroll',
            });
      },
      onSlow() {
        setNote({
          plain: 'Still working — the agent may be busy with another task. Keep this open to download automatically when it lands.',
          wizard: 'Still inscribing — the familiar may be occupied with another quest. Keep this open and the scroll will appear when ready.',
        });
      },
    });
  }, [jobID, phase]);
  useEffect(() => () => requestController.current?.abort(), []);

  const dirty = phase === 'form' && (
    preset !== 'summary' || title !== '' || instructions !== EXPORT_PRESETS.summary || sameGroup
  );
  const close = () => {
    requestController.current?.abort();
    if (phase === 'working') actions.exportStillRunning();
    actions.close(descriptor);
  };
  const submit = async () => {
    if (submitGuard.current || phase !== 'form') return;
    submitGuard.current = true;
    setError('');
    setSubmitting(true);
    const controller = new AbortController();
    requestController.current = controller;
    try {
      const job = await actions.startExport({
        conv: descriptor.conv, preset, title, instructions, sameGroup,
        signal: controller.signal,
      });
      setJobID(job.id);
      setStatus('cloning');
      setLastActive('cloning');
      setNote({ plain: '', wizard: '' });
      setPhase('working');
    } catch (cause) {
      if (cause?.name !== 'AbortError') setError(errorMessage(cause));
    } finally {
      submitGuard.current = false;
      requestController.current = null;
      setSubmitting(false);
    }
  };
  const retry = () => {
    setJobID(null);
    setStatus('cloning');
    setLastActive('cloning');
    setNote({ plain: '', wizard: '' });
    setError('');
    setPhase('form');
  };

  return html`
    <${Overlay}
      id="export-agent-modal"
      labelledby="export-agent-title"
      onClose=${close}
      onSubmitHotkey=${submit}
      dirty=${dirty}
      blocked=${submitting}
      confirmDiscard=${confirmDiscard}
      registerClose=${registerClose}
    >
      <h3 id="export-agent-title"><${Words} plain="Export conversation" wizard="Inscribe conversation scroll"/></h3>
      <div class="modal-meta" id="export-agent-meta">target: ${descriptor.label || shortId(descriptor.conv)}</div>
      ${phase === 'form' ? html`
        <div id="export-agent-form">
          <p class="export-intro"><${Words}
            plain="Ask this agent to consolidate a shareable export of the conversation — a summary or report for others to read. It runs on an isolated clone of the conversation, so the live agent is never disturbed; the file downloads here when it's ready. Multiple files are zipped automatically."
            wizard="Ask this familiar to inscribe a shareable account of its conversation. The work runs through an isolated mirror, so the live familiar is never disturbed; the finished scroll appears here. Multiple scrolls are bundled automatically."
          /></p>
          <label class="cron-create-row">
            <span class="cron-create-label">Format</span>
            <select id="export-agent-preset" value=${preset} onChange=${(event) => {
              const next = event.currentTarget.value;
              setPreset(next);
              if (!instructionsEdited.current || !instructions.trim()) {
                setInstructions(EXPORT_PRESETS[next] || '');
                instructionsEdited.current = false;
              }
            }}>
              <option value="summary">Summary report (Markdown)</option>
              <option value="detailed">Full detailed report</option>
              <option value="custom">Custom — see instructions</option>
            </select>
          </label>
          <label class="cron-create-row">
            <span class="cron-create-label">Title</span>
            <input id="export-agent-title-input" type="text" maxlength="200" value=${title} autocomplete="off" spellcheck="false" placeholder="optional — names the download, e.g. 'Auth research summary'" onInput=${(event) => setTitle(event.currentTarget.value)} />
          </label>
          <label class="cron-create-row">
            <span class="cron-create-label">Instructions</span>
            <textarea ref=${textareaRef} id="export-agent-instructions" rows="6" value=${instructions} spellcheck="false" placeholder="How should the agent produce the export? Audience, focus, what to include or leave out, format. Leave blank to let the agent decide." onInput=${(event) => { instructionsEdited.current = true; setInstructions(event.currentTarget.value); }}></textarea>
          </label>
          <label class="export-same-group" title="The export runs on an isolated clone of this conversation so the live agent is never disturbed. By default the clone is standalone. Check this to put the clone in the same group(s) as the original, so the summary writer can message peers if it needs to.">
            <input id="export-agent-same-group" type="checkbox" checked=${sameGroup} onChange=${(event) => setSameGroup(event.currentTarget.checked)} />
            <span><${Words}
              plain=${html`Clone into the same group <span class="export-same-group-hint">— lets the summary writer message peers (default: isolated)</span>`}
              wizard=${html`Mirror into the same party <span class="export-same-group-hint">— lets the scribe send missives to peers (default: isolated)</span>`}
            /></span>
          </label>
        </div>
      ` : html`
        <div id="export-agent-status" role="status" aria-live="polite" aria-atomic="true">
          <div id="export-agent-checklist"><${ExportChecklist} status=${status} failedAt=${lastActive}/></div>
          <div class="export-status-note" id="export-agent-status-note"><${Words} plain=${note.plain} wizard=${note.wizard}/></div>
        </div>
      `}
      <${ExportHistory} jobs=${history} actions=${actions} conv=${descriptor.conv} refreshHistory=${() => setHistoryVersion((value) => value + 1)} />
      <div class="cron-create-error" id="export-agent-error" role="alert">${error}</div>
      <div class="modal-buttons">
        <button id="export-agent-cancel" type="button" disabled=${submitting} onClick=${() => { void requestClose(); }}>${phase === 'form' ? 'Cancel' : 'Close'}</button>
        <span class="spacer"></span>
        ${phase === 'ready' && html`<button id="export-agent-download" class="primary" type="button" onClick=${() => actions.downloadExport(jobID)}>Download again</button>`}
        ${phase === 'failed' && html`<button id="export-agent-retry" class="primary" type="button" onClick=${retry}>Retry</button>`}
        ${phase === 'form' && html`<button id="export-agent-submit" class="primary" type="button" disabled=${submitting} onClick=${submit}>${submitting ? 'Requesting…' : 'Export'}</button>`}
      </div>
    </${Overlay}>
  `;
}

export function TerminalDirectoryDialog({ descriptor, actions, confirmDiscard }) {
  const choose = (value) => actions.finishChoice(descriptor, value);
  return html`
    <${Overlay}
      id="term-modal"
      dialogClass="modal"
      labelledby="term-title"
      onClose=${() => choose(null)}
      confirmDiscard=${confirmDiscard}
    >
      <h3 id="term-title"><span class="term-title-regular">Open a terminal</span><span class="term-title-wizard">🔮 Open a scrying portal</span></h3>
      <p><${Words}
        plain=${html`Spawn a terminal window for this agent. <strong>Current</strong> is where it has most recently been editing files; <strong>Worktree</strong> is the git worktree/repo root around that; <strong>Launch</strong> is where Claude Code was started.`}
        wizard=${html`Open a scrying portal for this familiar. <strong>Current</strong> is where it most recently worked its craft; <strong>Worktree</strong> is the surrounding git worktree/repo root; <strong>Launch</strong> is where it was summoned.`}
      /></p>
      ${descriptor.label && html`<div class="modal-meta" id="term-meta">${descriptor.label}</div>`}
      <div class="modal-buttons">
        <button id="term-cancel" type="button" onClick=${() => choose(null)}>Cancel</button>
        <button id="term-start" type="button" onClick=${() => choose('start')}>Launch dir</button>
        <button id="term-worktree" type="button" onClick=${() => choose('worktree')}>Worktree dir</button>
        <button id="term-current" type="button" autofocus onClick=${() => choose('current')}>Current dir</button>
      </div>
    </${Overlay}>
  `;
}

// SANDBOX_IMPL_MODE_INHERIT is the "don't pin anything" option in the mode
// select. It is not a mode the server accepts — it is the absence of one, which
// makes the server carry the agent's recorded mode forward or, when the
// implementation being replaced forced that mode, fall back to the harness
// default. Sending "" is what asks for that.
const SANDBOX_IMPL_MODE_INHERIT = '';

// The one implementation that KEEPS the harness-builtin mode a launch is given.
// Every other one derives its own, which is why the pin is offered only here.
const SANDBOX_IMPL_HARNESS_BUILTIN = 'harness-builtin';

// SandboxImplDialog assigns the sandbox IMPLEMENTATION an existing agent will
// relaunch under — the operator counterpart to picking one at spawn.
//
// It loads the durable posture itself instead of reading the row it was opened
// from: the row's sandbox fields describe the agent's last LAUNCH, while this
// dialog edits relaunch intent, and the two diverge the moment an assignment is
// recorded. Loading also means the dialog cannot offer to "keep" a value the
// server has since resolved differently.
export function SandboxImplDialog({ descriptor, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [request, setRequest] = useState({ phase: 'loading', data: null, error: '' });
  const [choice, setChoice] = useState('');
  const [mode, setMode] = useState(SANDBOX_IMPL_MODE_INHERIT);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const submitGuard = useRef(false);
  const firstOptionRef = useRef(null);

  useEffect(() => {
    const controller = new AbortController();
    setRequest({ phase: 'loading', data: null, error: '' });
    actions.loadSandboxImpl(descriptor.conv, { signal: controller.signal }).then(
      (data) => {
        setRequest({ phase: 'ready', data, error: '' });
        setChoice(String(data?.sandbox_implementation || ''));
      },
      (cause) => {
        if (cause?.name !== 'AbortError') {
          setRequest({ phase: 'error', data: null, error: errorMessage(cause) });
        }
      },
    );
    return () => controller.abort();
  }, [descriptor.conv]);

  const posture = request.data || {};
  const current = String(posture.sandbox_implementation || '');
  const harnessName = posture.harness || descriptor.harness;
  const options = actions.sandboxImplOptions(harnessName);
  // The pin is offered only where the launch would actually keep it. Every other
  // implementation DERIVES its harness mode (harness.ResolveSandboxImplementationMode),
  // so a select rendered beside them would be at its most prominent exactly where
  // the server discards what it collects.
  const modes = choice === SANDBOX_IMPL_HARNESS_BUILTIN
    ? actions.sandboxModes(harnessName)
    : [];
  const availability = actions.sandboxImplAvailability(harnessName, choice);
  const pinned = modes.length > 0 ? mode : SANDBOX_IMPL_MODE_INHERIT;
  // Dirty only once the operator has actually moved off the recorded posture, so
  // closing a dialog they merely looked at does not raise a discard prompt.
  const dirty = request.phase === 'ready'
    && (choice !== current || pinned !== SANDBOX_IMPL_MODE_INHERIT);

  const submit = async () => {
    if (submitGuard.current || !choice || request.phase !== 'ready' || !dirty) return;
    submitGuard.current = true;
    setError('');
    setBusy(true);
    try {
      await actions.assignSandboxImpl({
        conv: descriptor.conv,
        label: descriptor.label,
        implementation: choice,
        sandbox: pinned,
      }, descriptor);
    } catch (cause) { setError(errorMessage(cause)); }
    finally {
      submitGuard.current = false;
      setBusy(false);
    }
  };

  return html`
    <${Overlay}
      id="sandbox-impl-modal"
      dialogClass="modal"
      labelledby="sandbox-impl-title"
      initialFocusRef=${firstOptionRef}
      onClose=${() => actions.close(descriptor)}
      onSubmitHotkey=${submit}
      dirty=${dirty}
      blocked=${busy}
      confirmDiscard=${confirmDiscard}
      registerClose=${registerClose}
    >
      <h3 id="sandbox-impl-title"><${Words}
        plain="Sandbox implementation"
        wizard="Ward implementation"
      /></h3>
      <div class="modal-meta" id="sandbox-impl-meta">${descriptor.label || shortId(descriptor.conv)}</div>
      <p><${Words}
        plain="Which layer owns OS-level confinement for this agent's launches. Recorded now and applied by the next launch — wake the agent to run under it."
        wizard="Which layer holds the ward around this familiar. Bound now, taking effect the next time it is summoned."
      /></p>
      ${request.phase === 'loading' && html`<p class="sandbox-impl-loading" id="sandbox-impl-loading">loading the recorded posture…</p>`}
      ${request.phase === 'error' && html`<div class="cron-create-error" id="sandbox-impl-load-error" role="alert">${request.error}</div>`}
      ${request.phase === 'ready' && html`
        <div id="sandbox-impl-options" role="radiogroup" aria-labelledby="sandbox-impl-title">
          ${options.map((option, index) => {
            const value = String(option.value || '');
            const selected = value === choice;
            return html`
              <label class=${`sandbox-impl-option${selected ? ' selected' : ''}`} key=${value}
                title=${option.descr || ''}>
                <input type="radio" name="sandbox-impl-choice" value=${value} checked=${selected}
                  ref=${index === 0 ? firstOptionRef : null}
                  disabled=${busy} onChange=${() => setChoice(value)} />
                <span class="sandbox-impl-option-body">
                  <span class="sandbox-impl-option-name">${value}
                    ${value === current && html`<span class="sandbox-impl-tag">current</span>`}
                    ${option.experimental && html`<span class="sandbox-impl-tag warn">experimental</span>`}
                  </span>
                  ${/* The catalog label names the harness ("Claude Code built-in"); the
                        value above is the enum the CLI and the API take. Both are shown
                        because the operator may be reading either. */
                    option.label && html`<span class="sandbox-impl-option-label">${option.label}</span>`}
                  <span class="sandbox-impl-option-descr">${option.descr}</span>
                </span>
              </label>
            `;
          })}
        </div>
        ${availability && html`
          <p class="sandbox-impl-hint sandbox-impl-warn" id="sandbox-impl-availability" role="status">
            ⚠ ${availability}
          </p>
        `}
        ${modes.length > 0 && html`
          <label class="cron-create-row" id="sandbox-impl-mode-row">
            <span class="cron-create-label">Harness sandbox mode</span>
            <select id="sandbox-impl-mode" value=${mode} disabled=${busy}
              onChange=${(event) => setMode(event.currentTarget.value)}>
              <option value=${SANDBOX_IMPL_MODE_INHERIT}>leave to the server</option>
              ${modes.map((value) => html`<option key=${value} value=${value}>${value}</option>`)}
            </select>
          </label>
          <p class="sandbox-impl-hint" id="sandbox-impl-hint"><${Words}
            plain="Leave this alone unless you mean to pin it. Left alone, the server carries the agent's recorded mode forward, or uses the harness default when the implementation being replaced had forced one."
            wizard="Leave this be unless you mean to bind it. Left alone, the recorded mode carries forward."
          /></p>
        `}
        ${posture.temporary_sandbox_active && html`
          <p class="sandbox-impl-hint" id="sandbox-impl-unlocked">⚠ the temporary sandbox unlock is active on this agent; restore its normal sandbox before assigning.</p>
        `}
      `}
      <div class="cron-create-error" id="sandbox-impl-error" role="alert">${error}</div>
      <div class="modal-buttons">
        <button id="sandbox-impl-cancel" type="button" disabled=${busy}
          onClick=${() => { void requestClose(); }}>Cancel</button>
        <button id="sandbox-impl-assign" class="primary" type="button"
          disabled=${busy || !dirty}
          onClick=${submit}>${busy ? 'Assigning…' : 'Assign'}</button>
      </div>
    </${Overlay}>
  `;
}
