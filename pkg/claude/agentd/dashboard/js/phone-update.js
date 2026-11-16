// phone-update.js -- the phone's Update tclaude sheet (☰ More › Update
// tclaude): the node's configured self-update, run from a phone, as
// Config › Installation › Updates runs it on the desktop (node-update.js,
// whose status read, job polling, confirmation text and job view it uses).
//
// The sheet shows the version, the saved update method and whether an update
// is known to be available. With no method chosen, or one that only notifies
// (manual), it says so and links to Config › Updates on the desktop site
// instead of offering a run. Otherwise Update asks "Update and restart
// agentd?" in the sheet, with what the method will do, then follows the job:
// its phase while it runs, "restarting" while agentd is away (the reads are
// retried, so the sheet picks up again when it is back), then the new version,
// or the error with the job's output. While a run is in progress the sheet
// holds Back, a swipe and Escape, and says why.
//
// On a peer's page the same routes reach the peer (remote-node.js), which
// needs node.update there: canUpdateHere tells from its peer_view.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { useSheetGuard } from './phone-sheet.js';
import { featureState } from './peer-view-limits.js';
import { shellToast } from './shell-state.js';
import { setLayoutOverride } from './layout.js';
import {
  UpdateJobView, accessText, applyConfirm, createNodeUpdateActions, savedMethod, updateJobActive, useNodeUpdate, versionView,
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

let sharedActions = null;
const defaultActions = () => (sharedActions ||= createNodeUpdateActions());

// UpdateSheet is the sheet's body. actions are node-update.js's
// (createNodeUpdateActions), navigate the shell's; openDesktop opens
// Config › Updates on the desktop site.
export function UpdateSheet({ close, navigate, actions = defaultActions(), timers = globalThis, toast = shellToast, node = thisNode(),
  openDesktop = () => openDesktopUpdates(navigate) }) {
  const { status, error, busy, job, active, unanswered, start } = useNodeUpdate({ node, actions, toast, timers });
  const [confirming, setConfirming] = useState(false);
  // since: the id of the job before this sheet started one (null until it
  // does). A job with another id is this sheet's; the one before is history,
  // also when a start is refused and no new job comes.
  const [since, setSince] = useState(null);
  const running = busy || active;
  useSheetGuard(() => {
    if (running) { toast(RUN_HOLD_TEXT); return true; }
    if (confirming) { setConfirming(false); return true; }
    return false;
  });
  const v = versionView(status);
  const method = savedMethod(status);
  const unset = !method.id || method.id === 'manual';
  const head = html`<h2 class="phone-update-title">Update tclaude</h2>`;
  if (error) return html`<div class="phone-update" data-update="error-read">${head}<p class="ui-error" role="alert">${accessText(error, node)}</p>
    <${Button} class="phone-btn" onClick=${close}>Close<//></div>`;
  if (!status) return html`<div class="phone-update" data-update="loading">${head}<p class="phone-update-note">Reading the version…</p></div>`;

  const facts = html`<dl class="phone-update-facts">
    <dt>Version</dt><dd>${v.current || 'unknown'}</dd>
    <dt>Method</dt><dd>${method.id ? method.label : 'none chosen'}</dd>
    <dt>Latest</dt><dd>${v.latest || 'not checked'}${v.update ? html` <span class="phone-update-new">update available</span>` : status.update_available === false ? ' · up to date' : ''}</dd>
  </dl>`;

  // An install on screen: running, or this sheet's result. A check is no
  // install: it only refreshes Latest.
  const install = job && job.action !== 'check' ? job : null;
  if (install && (active || (since !== null && install.id !== since))) {
    const failed = install.state === 'failed' || install.state === 'rolled_back' || !!install.rolled_back;
    const done = !active;
    const phase = unanswered ? 'agentd is restarting… the page reconnects when it is back'
      : (install.phase || install.state || '').replaceAll('_', ' ');
    return html`<div class="phone-update" data-update=${!done ? 'running' : failed ? 'failed' : 'done'} aria-live="polite">
      ${head}
      ${!done && html`<p class="phone-update-state"><span class="phone-update-spin" aria-hidden="true"></span>Updating…</p>
        <p class="phone-update-phase">${phase}</p>
        <p class="phone-update-note">Agents keep running. Closing waits until agentd is back.</p>`}
      ${done && !failed && html`<p class="phone-update-ok">✓ Updated${install.version ? ` to ${install.version}` : ''}. agentd is back.</p>`}
      ${done && failed && html`<p class="ui-error phone-update-fail" role="alert">✗ The update failed${install.rolled_back || install.state === 'rolled_back' ? ' and was rolled back' : ''}.</p>`}
      <div class="phone-update-job"><${UpdateJobView} job=${install} unanswered=${unanswered} standing=${status.warnings || []} idp="phone-" /></div>
      ${done && html`<${Button} class="phone-btn" onClick=${close}>Close<//>`}
    </div>`;
  }

  if (unset) {
    return html`<div class="phone-update" data-update="unset">${head}${facts}
      <p class="phone-update-note">${method.id === 'manual'
        ? 'The update method here only notifies: tclaude tells you about a new release and leaves installing it to you.'
        : 'No update method is chosen here, so there is nothing to run from a phone.'} Choose one in Config › Updates on the desktop.</p>
      <${Button} class="phone-btn" onClick=${() => { close(); openDesktop(); }}>Config › Updates on the desktop site ›<//>
    </div>`;
  }

  const plan = applyConfirm({ node, status, method: method.id, methodLabel: method.label, version: method.id === 'custom' ? '' : v.latest });
  if (confirming) {
    return html`<div class="phone-update" data-update="confirm">
      <h2 class="phone-update-title">Update and restart agentd?</h2>
      <p class="phone-update-body">${plan.body.charAt(0).toUpperCase() + plan.body.slice(1)}</p>
      <div class="phone-btns">
        <${Button} class="phone-btn phone-btn-grow" onClick=${() => setConfirming(false)}>Cancel<//>
        <${Button} class="phone-btn phone-btn-pri phone-btn-grow" data-sheet-focus disabled=${running}
          onClick=${() => { setConfirming(false); setSince(job?.id || ''); start('apply', method.id === 'custom' ? undefined : v.latest || undefined); }}>${plan.okLabel}<//>
      </div>
    </div>`;
  }
  return html`<div class="phone-update" data-update="ready">${head}${facts}
    ${(status.warnings || []).map((w) => html`<p key=${w} class="phone-update-warn">${w}</p>`)}
    <p class="phone-update-note">Runs ${method.label.toLowerCase()} on ${node.label}, then restarts agentd. Agents keep running.</p>
    <div class="phone-btns">
      <${Button} class="phone-btn phone-btn-grow" disabled=${running} onClick=${() => start('check')}>${active ? 'Checking…' : 'Check now'}<//>
      <${Button} class="phone-btn phone-btn-pri phone-btn-grow" disabled=${running} onClick=${() => setConfirming(true)}>Update…<//>
    </div>
  </div>`;
}
