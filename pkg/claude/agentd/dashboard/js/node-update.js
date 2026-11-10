import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { Dialog, DialogClose } from './ui-dialog.js';
import { RequestError, requestJSON } from './request-json.js';
import { errText } from './cell-format.js';
import { NodeUpdateSettings } from './node-update-settings.js';
import { ServicePanel } from './config-service.js';

const html = htm.bind(h);

// UPDATE_POLL_MS paces the status re-read while an update job runs; the node's
// daemon restarts mid-job, so failed reads are retried rather than shown.
const UPDATE_POLL_MS = 1000;
// The hub is read every 2 s: its admin calls share a per-connection budget.
const HUB_UPDATE_POLL_MS = 2000;

// updateBase addresses a node's self-update routes: this node directly, a peer
// through the peer proxy (which needs the node.update grant there).
export function updateBase(node) {
  if (node?.hub) return '/api/federation/hub/update';
  return node?.local ? '/api/node/update' : `/api/peer/${encodeURIComponent(node.id)}/node/update`;
}

// serviceBase is the node API base the Start automatically part reads
// <base>/service under: this node's, or a peer's through the peer proxy
// (which needs node.read there, and node.update.settings to act).
export function serviceBase(node) {
  return node?.local ? '/api/node' : `/api/peer/${encodeURIComponent(node.id)}/node`;
}

// updateJobActive reports whether a self-update job is still going.
export function updateJobActive(job) {
  return job?.state === 'running' || job?.state === 'restarting' || job?.state === 'rolling_back';
}

// versionView reads the version fields a node summary or update status
// carries: current, latest and whether an update is known to be available.
export function versionView(s) {
  const current = s?.current_version || s?.version || '';
  const latest = s?.latest_version || '';
  return { current, latest, update: s?.update_available === true && !!latest };
}

// launchView reads an update job's launch report: whether the restart kept
// the daemon's flags and environment. null when the job has none. Values
// never reach the dashboard; differences name the flag or variable.
export function launchView(launch) {
  if (!launch) return null;
  const diffs = launch.differences || [];
  const describe = (d) => d.kind === 'env' ? `environment ${d.name} ${d.change}`
    : d.change === 'changed' ? `${d.kind === 'flag' ? d.name : d.kind} changed: ${d.before} → ${d.after}`
      : d.change === 'added' ? `${d.kind === 'flag' ? d.name : d.kind} added` : `${d.kind === 'flag' ? d.name : d.kind} removed`;
  // expected: the restarted daemon against its launch profile, or a unit
  // tclaude did not write.
  const exp = launch.expected?.state === 'differs' ? launch.expected : null;
  const expDetails = exp ? (exp.differences || []).map((d) => `${describe(d)} (vs the ${exp.baseline === 'unit' ? 'service unit' : 'launch profile'})`) : [];
  if (launch.state === 'identical') return exp
    ? { ok: false, text: `Kept flags and environment ✓, but agentd does not run as its ${exp.baseline === 'unit' ? 'service unit' : 'launch profile'} says`, details: expDetails }
    : { ok: true, text: 'Kept flags and environment ✓' };
  if (launch.state === 'differs') return { ok: false, text: `${diffs.length} difference${diffs.length === 1 ? '' : 's'} in flags and environment after the restart`, details: [...diffs.map(describe), ...expDetails] };
  if (launch.args_check === 'failed') return { ok: false, text: `The new binary rejects the daemon's flags: ${launch.args_check_detail || 'unknown reason'}` };
  if (launch.state === 'unavailable' || launch.state === 'not_restarted') return null;
  return { ok: true, text: 'Flags and environment recorded; compared after the restart' };
}

export class NodeUpdateError extends RequestError {}

// UPDATE_REFUSALS lead the daemon's own message for an apply its update
// settings refuse (see the auto-update settings below the version).
const UPDATE_REFUSALS = {
  update_method_package_managed: 'a package manager owns tclaude there.',
  update_package_manager_ack_required: 'the package-manager acknowledgement is missing.',
  update_method_notify_only: 'its update method only notifies.',
};

// createNodeUpdateActions wraps GET/POST /api/node/update (and the peer-proxied
// form). Jobs are followed through the status read, which survives the
// daemon's restart.
export function createNodeUpdateActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  const call = (method, url, body) => requestJSON(url, { method, body, fetchImpl, ErrorClass: NodeUpdateError });
  return Object.freeze({
    // fetchImpl serves the Start automatically part, which reads and acts
    // through the node's service routes itself (serviceBase).
    fetchImpl,
    status: (node) => call('GET', updateBase(node)),
    start: (node, action, version) => call('POST', updateBase(node), version ? { action, version } : { action }),
    // The node's auto-update settings (updates.self); a peer's needs
    // node.update to read and node.update.settings to change.
    settings: (node) => call('GET', `${updateBase(node)}/settings`),
    saveSettings: (node, body) => call('PUT', `${updateBase(node)}/settings`, body),
  });
}

let sharedActions = null;
function defaultActions() { return (sharedActions ||= createNodeUpdateActions()); }

// hubMayBeRestarting is a failed hub read that a restart explains: no answer
// through the relay. The dialog keeps retrying those instead of giving up.
function hubMayBeRestarting(error) {
  return !error?.status || error.status === 502 || error.status === 503 || error.status === 504;
}

function accessText(error, node) {
  if (node.hub && error?.status === 403) return 'This node does not hold the hub capability for updates (hub.update).';
  // A daemon without the route answers 404; an older hub refuses the
  // operation (400 "operation"). Either way the feature is simply absent.
  if (node.hub && (error?.status === 404 || (error?.status === 400 && error?.code === 'operation'))) return 'Hub update is not available on this build of tclaude or the hub.';
  if (node.hub && hubMayBeRestarting(error)) return 'The hub is not answering right now (it may be restarting); retrying…';
  if (error?.status === 403) return `${node.label} does not let you manage its updates (needs node.update).`;
  if (error?.status === 503) return `Self-update is unavailable on ${node.label}: ${errText(error)}`;
  if (error?.status === 404 && !node.local) return `${node.label} does not offer self-update yet.`;
  return `Could not read ${node.label}'s update status: ${errText(error)}`;
}

// NodeUpdateDialog is a node's Installation view, as Config › Installation
// is this node's: its tclaude version, where the operator checks for, applies
// or rolls back an update, then the Start automatically and Updates parts
// (the hub has only the version). Apply and rollback confirm with the
// consequence (binaries replaced, daemon restarts); a source build warns that
// the official release replaces it.
export function NodeUpdateDialog({ node, confirm, toast, onClose, actions = defaultActions(), timers = globalThis }) {
  const [status, setStatus] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [tick, setTick] = useState(0);
  // unanswered is set while a running job's status read fails: the daemon or
  // hub is restarting.
  const [unanswered, setUnanswered] = useState(false);
  const job = status?.job || null;
  const active = updateJobActive(job);

  useEffect(() => {
    let off = false;
    actions.status(node).then((s) => { if (!off) { setStatus(s); setError(null); setUnanswered(false); } })
      .catch((e) => { if (!off) { if (active) setUnanswered(true); else setError(e); } });
    return () => { off = true; };
  }, [node.id, tick]);

  // While a job runs, re-read the status every second. Reads fail while the
  // daemon restarts; keep trying until the job settles.
  const retrying = !active && node.hub && error && hubMayBeRestarting(error);
  useEffect(() => {
    if (!active && !retrying) return undefined;
    const t = timers.setTimeout(() => setTick((n) => n + 1), node.hub ? HUB_UPDATE_POLL_MS : UPDATE_POLL_MS);
    return () => timers.clearTimeout(t);
  }, [active, retrying, tick]);

  const start = (action, version) => {
    setBusy(true);
    return actions.start(node, action, version)
      .then((j) => { setStatus((s) => ({ ...(s || {}), job: j })); setTick((n) => n + 1); })
      .catch((e) => toast(e?.code === 'update_busy' ? `${node.label} is already updating`
        : UPDATE_REFUSALS[e?.code] ? `${node.label}: ${UPDATE_REFUSALS[e.code]} ${errText(e)}` : `Update ${action} failed: ${errText(e)}`, true))
      .finally(() => setBusy(false));
  };
  const v = versionView(status);
  // Release binaries are swapped for the verified release; any other install
  // (source, go install, unmarked) is rebuilt with go install at that tag.
  const built = (status?.binaries || []).some((b) => b.install_method && b.install_method !== 'release');
  // The hub restarts under its service supervisor and rolls itself back if
  // the new binary is not healthy in time; while it restarts every node's hub
  // connection drops.
  const blocked = node.hub && status?.blocked;
  const supervisor = status?.supervisor;
  const apply = () => confirm(node.hub ? {
    title: `Update the hub to ${v.latest}?`,
    body: `The hub downloads the official tclaude-hub ${v.latest} release, verifies its checksum, stages it next to ${v.current || 'the current binary'} and restarts under ${supervisor || 'its supervisor'}. Every node's hub connection drops and reconnects while it restarts. If the new hub is not healthy by the deadline it rolls back to ${v.current || 'the current version'} automatically.`
      + ((status?.warnings || []).length ? ` Warnings: ${status.warnings.join('; ')}.` : ''),
    okLabel: 'Update hub',
  } : {
    title: `Update tclaude on ${node.label} to ${v.latest}?`,
    body: `${node.label} ${built ? `builds tclaude ${v.latest} with go install (it is not a release install; local modifications are replaced)` : `downloads and verifies the official tclaude ${v.latest} release`}, replaces its tclaude binaries (the current ones are kept for rollback) and restarts its daemon. Its agent sessions keep running; dashboards and peer links reconnect after the restart.`
      + ((status?.warnings || []).length ? ` Warnings: ${status.warnings.join('; ')}.` : ''),
    okLabel: 'Update',
  }).then((ok) => ok && start('apply', v.latest));
  const rollback = () => confirm(node.hub ? {
    title: 'Roll back the hub?',
    body: `The hub restores the binary it ran before its last update and restarts under ${supervisor || 'its supervisor'}. Every node's hub connection drops and reconnects while it restarts.`,
    okLabel: 'Roll back hub',
  } : {
    title: `Roll back tclaude on ${node.label}?`,
    body: `${node.label} restores the tclaude binaries it had before its last update and restarts its daemon. Its agent sessions keep running.`,
    okLabel: 'Roll back',
  }).then((ok) => ok && start('rollback'));

  return html`<${Dialog} id="fleet-node-update-modal" title=${node.hub ? 'tclaude-hub' : `Installation on ${node.label}`}
    dialogClass=${node.hub ? undefined : 'cron-create-modal fa-installation-modal'} onClose=${onClose} busy=${busy}
    footerStart=${status && html`<${Button} id="fleet-node-update-check" disabled=${busy || active} onClick=${() => start('check')}>Check now</${Button}>
      ${v.update && html`<${Button} variant="primary" id="fleet-node-update-apply" disabled=${busy || active || !!blocked} onClick=${apply}>Update to ${v.latest}…</${Button}>`}
      ${status.rollback_available && html`<${Button} variant="danger" id="fleet-node-update-rollback" disabled=${busy || active || !!blocked} onClick=${rollback}>Roll back…</${Button}>`}`}
    footer=${html`<${DialogClose} disabled=${busy}>Close<//>`}
    after=${html`<div class="muted fa-cli-note">CLI: <code>${node.hub ? 'tclaude federation hub update' : `tclaude update${node.local ? '' : ` --node ${node.id}`}`} --check|--apply|--rollback</code></div>`}>
    ${error ? html`<div class="muted" role="alert">${accessText(error, node)}</div>` : !status ? html`<div class="muted">Loading…</div>` : html`
      <div class="fa-dl">
        <span class="fa-k">Version</span><span>${v.current || 'unknown'}${status.install_method ? html` <span class="muted">(${status.install_method.replaceAll('_', ' ')})</span>` : ''}</span>
        <span class="fa-k">Latest</span><span>${v.latest || html`<span class="muted">not checked</span>`}${v.update ? html` <span class="fa-warn">update available</span>` : status.update_available === false ? html` <span class="muted">up to date</span>` : ''}${status.checked_at ? html` <span class="muted">· checked ${new Date(status.checked_at).toLocaleString()}</span>` : ''}</span>
        ${node.hub && html`<span class="fa-k">Supervisor</span><span id="fleet-hub-supervisor">${supervisor || html`<span class="fa-danger">none</span>`}</span>`}
        ${(status.binaries || []).map((b) => html`<span key=${b.name} class="fa-k">${b.name}</span><span><code>${b.version}</code> <span class="muted">${b.path}</span></span>`)}
      </div>
      ${(status.warnings || []).map((w) => html`<div key=${w} class="fa-warn">${w}</div>`)}
      ${blocked && html`<div class="fa-danger" role="alert" id="fleet-hub-update-blocked">${blocked.code === 'not_supervised' ? 'The hub is not running under systemd or launchd, so it cannot restart itself or roll back safely; update refused' : 'Update is not possible'}${blocked.message ? `: ${blocked.message}` : '.'}</div>`}
      ${job && html`<div id="fleet-node-update-job" class=${job.state === 'failed' || job.state === 'rolled_back' || job.rolled_back ? 'fa-danger' : ''}>
        ${job.action} ${job.from_version ? `${job.from_version} → ` : ''}${job.version || ''}: <b>${unanswered ? `${job.state === 'rolling_back' || job.phase === 'rolling_back' ? 'rolling back' : 'restarting'}… (not answering yet)` : job.state.replaceAll('_', ' ')}</b>${job.phase && job.phase !== job.state && !(job.state === 'completed' && job.phase === 'complete') ? ` (${job.phase.replaceAll('_', ' ')})` : ''}${job.rolled_back && job.state !== 'rolled_back' ? ' — rolled back' : ''}${job.error ? ` — ${job.error}` : ''}
        ${job.deadline && updateJobActive(job) && html`<span class="muted"> · healthy by ${new Date(job.deadline).toLocaleTimeString()} or it rolls back</span>`}
      </div>`}
      ${job && launchView(job.launch) && ((l) => html`<div id="fleet-node-update-launch" class=${l.ok ? 'muted' : 'fa-warn'}>${l.text}${l.details && html`<ul>${l.details.map((d) => html`<li key=${d}>${d}</li>`)}</ul>`}</div>`)(launchView(job.launch))}
      ${!node.hub && html`<div class="fa-installation">
        ${actions.fetchImpl && html`<${ServicePanel} idp="fleet-" base=${serviceBase(node)} fetchImpl=${actions.fetchImpl} confirm=${confirm} toast=${toast} timers=${timers} />`}
        ${actions.settings && html`<${NodeUpdateSettings} node=${node} actions=${actions} confirm=${confirm} toast=${toast} />`}
      </div>`}`}
  </${Dialog}>`;
}
