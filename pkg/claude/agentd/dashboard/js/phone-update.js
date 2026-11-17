// phone-update.js -- the phone's Update tclaude sheet (☰ More › Update
// tclaude): the node's configured self-update, run from a phone, as
// Config › Installation › Updates runs it on the desktop (node-update.js,
// whose status read, job polling, confirmation text and job view it uses).
//
// The sheet shows the version, what Update runs and whether an update is
// known to be available. Update is offered whenever Config › Updates'
// Update now is (node-update.js updateNowRefusal): with a saved method, that
// method; with none saved, each binary the way it was installed ("as
// installed: release download", methodText), with a note that settings and
// a schedule are set up in Config › Updates on the desktop. Only a
// notify-only method (manual) holds it back, or with none saved a binary the
// node would refuse to update as installed (package-managed, unidentified),
// linking there instead. Update
// asks "Update and restart agentd?" in the sheet, with what the method will
// do, then follows the job: its phase while it runs, "restarting" while
// agentd is away (the reads are retried, so the sheet picks up again when it
// is back), then the new version, or the error. A finished run's output
// (JobOutput) is monospace, scrollable and opened at its end, and a failure
// puts the run's error and the log's failing lines above it, so a long log
// (a custom command's ansible run) shows where it failed without hunting.
// "Already up to date" when nothing changed.
//
// Update sends no version, as Config › Updates' Update now: the
// saved method decides. While an install runs and agentd answers, the sheet
// holds Back, a swipe and Escape, and says why; once agentd stops answering
// it offers Close, since the run goes on at the node either way.
//
// On a peer's page the same routes reach the peer (remote-node.js), which
// needs node.update there: canUpdateHere tells from its peer_view.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { SheetHead } from './phone-ui.js';
import { Button } from './ui-components.js';
import { useSheetGuard } from './phone-sheet.js';
import { featureState } from './peer-view-limits.js';
import { shellToast } from './shell-state.js';
import { writeClipboard } from './insecure-origin.js';
import { setLayoutOverride } from './layout.js';
import { ErrorNote } from './phone-ui.js';
import {
  UpdateJobView, accessText, applyConfirm, createNodeUpdateActions, installedText, methodText, savedMethod, updateNowRefusal, useNodeUpdate, versionView,
} from './node-update.js';

const html = htm.bind(h);

// canUpdateHere reports whether this page may run the node's self-update:
// always on this node's own page; on a peer's only when it shares
// node.update (unrestricted trust does).
export function canUpdateHere(snapshot) {
  const view = snapshot?.peer_view;
  if (!view) return true;
  return view.level === 'unrestricted' || featureState('node.update', view) === 'shared';
}

// thisNode is the node the page shows, as node-update.js addresses it: its
// local routes, which a peer's page carries to the peer.
export function thisNode(remote = globalThis.__tclaudeRemoteNode) {
  return { id: remote?.id || 'self', label: remote?.id || 'this node', local: true };
}

// RUN_HOLD_TEXT is what a close says while a run is in progress.
export const RUN_HOLD_TEXT = 'The update is running: this closes once agentd is back.';

// openDesktopUpdates opens Config › Updates on the desktop site.
function openDesktopUpdates(navigate) {
  navigate?.('/config/updates');
  setLayoutOverride('desktop');
}

// FAILING matches a log line that says what went wrong: an ansible task's
// "fatal:" or "FAILED!", an error, a non-zero exit. failingLines is the last
// few of them, so a long log's failure shows without scrolling through it.
const FAILING = /\b(fatal|failed|failure|error|errors|exception|traceback|panic|exit (status|code) [1-9]\d*)\b|FAILED!|^\s*E\s/i;
// CLEAN is a line that names a failure only to say there was none, or one
// that was ignored: a recap's failed=0, "0 errors", ansible's -v JSON
// "failed": false, an ignored task's "...ignoring".
const CLEAN = /\b(failed|errors?)=0\b|(?:^|[\s(])0 (errors?|failures?|failed)\b|"failed":\s*false|\.\.\.ignoring\s*$/i;
export function failingLines(output, max = 6) {
  const lines = String(output || '').split('\n').map((l) => l.trimEnd()).filter((l) => FAILING.test(l) && !CLEAN.test(l));
  return lines.slice(-max);
}

// JobOutput is a finished run's output on a phone: on a failure, the run's
// error and the log's failing lines first, then the whole output, monospace,
// scrollable and opened at its end (where a script stops), with Copy.
export function JobOutput({ job, failed, copy = writeClipboard, toast = shellToast }) {
  const pre = useRef(null);
  const text = String(job.output || '');
  useLayoutEffect(() => { if (pre.current) pre.current.scrollTop = pre.current.scrollHeight; }, [text]);
  const lines = failed ? failingLines(text) : [];
  if (!text && !(failed && job.error)) return null;
  return html`<div class="phone-update-out">
    ${failed && (job.error || lines.length > 0) && html`<div class="phone-update-why" data-update-why>
      <h3 class="ui-eyebrow">Where it failed</h3>
      ${job.error && html`<pre class="phone-update-err">${job.error}</pre>`}
      ${lines.length > 0 && html`<pre class="phone-update-lines">${lines.join('\n')}</pre>`}
    </div>`}
    ${text && html`<div class="phone-update-log-head"><${SheetHead}>Output · ${text.split('\n').length} lines, the end shown<//>
      <${Button} class="phone-btn phone-update-copy" onClick=${async () => toast(await copy(text) ? 'Copied the output' : 'Could not copy the output', false)}>Copy<//></div>
      <pre class="phone-update-log" ref=${pre} tabindex="0" aria-label="The update's output">${text}</pre>`}
  </div>`;
}

let sharedActions = null;
const defaultActions = () => (sharedActions ||= createNodeUpdateActions());

// UpdateSheet is the sheet's body. actions are node-update.js's
// (createNodeUpdateActions), navigate the shell's; openDesktop opens
// Config › Updates on the desktop site.
export function UpdateSheet({ close, navigate, actions = defaultActions(), timers = globalThis, toast = shellToast, node = thisNode(),
  openDesktop = () => openDesktopUpdates(navigate) }) {
  const { status, error, busy, job, active, unanswered, start } = useNodeUpdate({ node, actions, toast, timers });
  const [confirming, setConfirming] = useState(false);
  // since: the id of the job before this sheet's (null until there is one).
  // A job with another id is this sheet's, the one before history, also when
  // a start is refused and no new job comes. An install already running
  // when the sheet opens (a scheduled one, one from the desktop) is the
  // sheet's too, so its result stays on screen.
  const [since, setSince] = useState(null);
  // An install, not a check: a check only refreshes Latest.
  const install = job && job.action !== 'check' ? job : null;
  useEffect(() => { if (install && active && since === null) setSince(''); }, [install?.id, active]);
  // Close is held while an install runs and agentd answers. Once it stops
  // answering (restarting), Close is offered: if it never comes back, the
  // sheet must not hold the page; the run goes on at the node either way.
  const installing = busy || (active && !!install);
  const holding = installing && !unanswered;
  useSheetGuard(() => {
    if (holding) { toast(RUN_HOLD_TEXT); return true; }
    if (confirming) { setConfirming(false); return true; }
    return false;
  });
  const v = versionView(status);
  const method = savedMethod(status);
  // As Config › Updates' Update now: only a notify-only method holds it
  // back. With none saved each binary updates the way it was installed.
  const refusal = updateNowRefusal(method, status);
  const running = methodText(method, status);
  const head = html`<h2 class="phone-update-title">Update tclaude</h2>`;
  if (error) return html`<div class="phone-update" data-update="error-read">${head}<${ErrorNote}>${accessText(error, node)}<//>
    <${Button} class="phone-btn" onClick=${close}>Close<//></div>`;
  if (!status) return html`<div class="phone-update" data-update="loading">${head}<p class="phone-update-note">Reading the version…</p></div>`;

  const facts = html`<dl class="phone-update-facts">
    <dt>Version</dt><dd>${v.current || 'unknown'}</dd>
    <dt>Method</dt><dd data-update-method>${running}</dd>
    <dt>Latest</dt><dd>${v.latest || 'not checked'}${v.update ? html` <span class="phone-update-new">update available</span>` : status.update_available === false ? ' · up to date' : ''}</dd>
  </dl>`;

  // An install on screen: running, or this sheet's result.
  if (install && (active || (since !== null && install.id !== since))) {
    const failed = install.state === 'failed' || install.state === 'rolled_back' || !!install.rolled_back;
    const done = !active;
    const rollback = install.action === 'rollback';
    // A run that installed nothing new (already on the target) succeeds
    // without restarting: say so rather than "updated".
    const changed = install.restart_required || install.phase === 'restarted' || (install.from_version && install.version && install.from_version !== install.version);
    const phase = unanswered ? 'agentd is restarting… the page reconnects when it is back'
      : (install.phase || install.state || '').replaceAll('_', ' ');
    return html`<div class="phone-update" data-update=${!done ? 'running' : failed ? 'failed' : 'done'} aria-live="polite">
      ${head}
      ${!done && html`<p class="phone-update-state"><span class="phone-update-spin" aria-hidden="true"></span>${rollback ? 'Rolling back…' : 'Updating…'}</p>
        <p class="phone-update-phase">${phase}</p>
        <p class="phone-update-note">${unanswered ? 'You can close this: the update goes on at the node.' : 'Agents keep running. Closing waits until agentd is back.'}</p>`}
      ${done && !failed && html`<p class="phone-update-ok">${rollback ? `✓ Rolled back to ${v.current || 'the previous version'}.`
        : changed ? `✓ Updated to ${v.current || install.version || 'the new version'}. agentd is back.` : `✓ Already up to date${v.current ? ` (${v.current})` : ''}: nothing changed.`}</p>`}
      ${done && failed && html`<p class="ui-error phone-update-fail" role="alert">✗ The update failed${install.rolled_back || install.state === 'rolled_back' ? ' and was rolled back' : ''}.</p>`}
      <div class="phone-update-job"><${UpdateJobView} job=${install} unanswered=${unanswered} standing=${status.warnings || []} idp="phone-" output=${false} error=${!done} /></div>
      ${done && html`<${JobOutput} job=${install} failed=${failed} toast=${toast} />`}
      ${(done || unanswered) && html`<${Button} class="phone-btn" onClick=${close}>Close<//>`}
    </div>`;
  }

  const toDesktop = html`<${Button} class="phone-btn" data-update-desktop onClick=${() => { close(); openDesktop(); }}>Config › Updates on the desktop site ›<//>`;
  if (refusal) {
    return html`<div class="phone-update" data-update="unset">${head}${facts}
      <p class="phone-update-note" data-update-refusal>${method.id === 'manual'
        ? 'The update method here only notifies: tclaude tells you about a new release and leaves installing it to you. Choose one that installs in Config › Updates on the desktop.'
        : `${refusal}.`}</p>
      ${toDesktop}
    </div>`;
  }

  // As Config › Updates' Update now: the saved method, no version pinned
  // (a checkout following a branch builds its head, not the release tag).
  const plan = applyConfirm({ node, status, method: method.id, methodLabel: method.label });
  if (confirming) {
    return html`<div class="phone-update" data-update="confirm">
      <h2 class="phone-update-title">Update and restart agentd?</h2>
      <p class="phone-update-body">${plan.body.charAt(0).toUpperCase() + plan.body.slice(1)}</p>
      <div class="phone-btns">
        <${Button} class="phone-btn phone-btn-grow" onClick=${() => setConfirming(false)}>Cancel<//>
        <${Button} class="phone-btn phone-btn-pri phone-btn-grow" data-sheet-focus disabled=${busy || active}
          onClick=${() => { setConfirming(false); setSince(job?.id || ''); start('apply'); }}>${plan.okLabel}<//>
      </div>
    </div>`;
  }
  return html`<div class="phone-update" data-update="ready">${head}${facts}
    ${(status.warnings || []).map((w) => html`<p key=${w} class="phone-update-warn">${w}</p>`)}
    <p class="phone-update-note">${method.id
      ? `Runs ${method.label.toLowerCase()} on ${node.label}, then restarts agentd.`
      : `No method is saved, so each binary on ${node.label} updates the way it was installed${installedText(status) ? ` (${installedText(status)})` : ''}, then agentd restarts.`} Agents keep running.</p>
    <div class="phone-btns">
      <${Button} class="phone-btn phone-btn-grow" disabled=${busy || active} onClick=${() => start('check')}>${active ? 'Checking…' : 'Check now'}<//>
      <${Button} class="phone-btn phone-btn-pri phone-btn-grow" disabled=${busy || active} onClick=${() => setConfirming(true)}>Update…<//>
    </div>
    ${!method.id && html`<p class="phone-update-note" data-update-unsaved>Nothing updates on its own until a method is saved: set one up, and a schedule, in Config › Updates on the desktop.</p>${toDesktop}`}
  </div>`;
}
