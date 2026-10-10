import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

// UPDATE_POLL_MS paces the status re-read while an update job runs; the node's
// daemon restarts mid-job, so failed reads are retried rather than shown.
const UPDATE_POLL_MS = 1000;

function errText(error) { return error?.message || String(error); }

// updateBase addresses a node's self-update routes: this node directly, a peer
// through the peer proxy (which needs the node.update grant there).
export function updateBase(node) {
  return node?.local ? '/api/node/update' : `/api/peer/${encodeURIComponent(node.id)}/node/update`;
}

// updateJobActive reports whether a self-update job is still going.
export function updateJobActive(job) {
  return job?.state === 'running' || job?.state === 'restarting';
}

// versionView reads the version fields a node summary or update status
// carries: current, latest and whether an update is known to be available.
export function versionView(s) {
  const current = s?.current_version || s?.version || '';
  const latest = s?.latest_version || '';
  return { current, latest, update: s?.update_available === true && !!latest };
}

export class NodeUpdateError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || '';
  }
}

// createNodeUpdateActions wraps GET/POST /api/node/update (and the peer-proxied
// form). Jobs are followed through the status read, which survives the
// daemon's restart.
export function createNodeUpdateActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(method, url, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(url, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) throw new NodeUpdateError(res.status, data);
    return data;
  }
  return Object.freeze({
    status: (node) => call('GET', updateBase(node)),
    start: (node, action, version) => call('POST', updateBase(node), version ? { action, version } : { action }),
  });
}

let sharedActions = null;
function defaultActions() { return (sharedActions ||= createNodeUpdateActions()); }

function accessText(error, node) {
  if (error?.status === 403) return `${node.label} does not let you manage its updates (needs node.update).`;
  if (error?.status === 503) return `Self-update is unavailable on ${node.label}: ${errText(error)}`;
  if (error?.status === 404 && !node.local) return `${node.label} does not offer self-update yet.`;
  return `Could not read ${node.label}'s update status: ${errText(error)}`;
}

// NodeUpdateDialog shows one node's tclaude version and lets the operator
// check for, apply or roll back an update. Apply and rollback confirm with the
// consequence (binaries replaced, daemon restarts); a source build warns that
// the official release replaces it.
export function NodeUpdateDialog({ node, confirm, toast, onClose, actions = defaultActions(), timers = globalThis }) {
  const [status, setStatus] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [tick, setTick] = useState(0);
  const job = status?.job || null;
  const active = updateJobActive(job);

  useEffect(() => {
    let off = false;
    actions.status(node).then((s) => { if (!off) { setStatus(s); setError(null); } })
      .catch((e) => { if (!off && !active) setError(e); });
    return () => { off = true; };
  }, [node.id, tick]);

  // While a job runs, re-read the status every second. Reads fail while the
  // daemon restarts; keep trying until the job settles.
  useEffect(() => {
    if (!active) return undefined;
    const t = timers.setTimeout(() => setTick((n) => n + 1), UPDATE_POLL_MS);
    return () => timers.clearTimeout(t);
  }, [active, tick]);

  const start = (action, version) => {
    setBusy(true);
    return actions.start(node, action, version)
      .then((j) => { setStatus((s) => ({ ...(s || {}), job: j })); setTick((n) => n + 1); })
      .catch((e) => toast(e?.code === 'update_busy' ? `${node.label} is already updating` : `Update ${action} failed: ${errText(e)}`, true))
      .finally(() => setBusy(false));
  };
  const v = versionView(status);
  // Release binaries are swapped for the verified release; any other install
  // (source, go install, unmarked) is rebuilt with go install at that tag.
  const built = (status?.binaries || []).some((b) => b.install_method && b.install_method !== 'release');
  const apply = () => confirm({
    title: `Update tclaude on ${node.label} to ${v.latest}?`,
    body: `${node.label} ${built ? `builds tclaude ${v.latest} with go install (it is not a release install; local modifications are replaced)` : `downloads and verifies the official tclaude ${v.latest} release`}, replaces its tclaude binaries (the current ones are kept for rollback) and restarts its daemon. Its agent sessions keep running; dashboards and peer links reconnect after the restart.`
      + ((status?.warnings || []).length ? ` Warnings: ${status.warnings.join('; ')}.` : ''),
    okLabel: 'Update',
  }).then((ok) => ok && start('apply', v.latest));
  const rollback = () => confirm({
    title: `Roll back tclaude on ${node.label}?`,
    body: `${node.label} restores the tclaude binaries it had before its last update and restarts its daemon. Its agent sessions keep running.`,
    okLabel: 'Roll back',
  }).then((ok) => ok && start('rollback'));

  return html`<${Overlay} id="fleet-node-update-modal" labelledby="fleet-node-update-title" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-node-update-title">tclaude on ${node.label}</h3>
    ${error ? html`<div class="muted" role="alert">${accessText(error, node)}</div>` : !status ? html`<div class="muted">Loading…</div>` : html`
      <div class="fa-dl">
        <span class="fa-k">Version</span><span>${v.current || 'unknown'}${status.install_method ? html` <span class="muted">(${status.install_method.replaceAll('_', ' ')})</span>` : ''}</span>
        <span class="fa-k">Latest</span><span>${v.latest || html`<span class="muted">not checked</span>`}${v.update ? html` <span class="fa-warn">update available</span>` : status.update_available === false ? html` <span class="muted">up to date</span>` : ''}${status.checked_at ? html` <span class="muted">· checked ${new Date(status.checked_at).toLocaleString()}</span>` : ''}</span>
        ${(status.binaries || []).map((b) => html`<span key=${b.name} class="fa-k">${b.name}</span><span><code>${b.version}</code> <span class="muted">${b.path}</span></span>`)}
      </div>
      ${(status.warnings || []).map((w) => html`<div key=${w} class="fa-warn">${w}</div>`)}
      ${job && html`<div id="fleet-node-update-job" class=${job.state === 'failed' ? 'fa-danger' : ''}>
        ${job.action} ${job.version || ''}: <b>${job.state}</b>${job.phase ? ` (${job.phase})` : ''}${job.error ? ` — ${job.error}` : ''}
      </div>`}`}
    <div class="modal-buttons">
      ${status && html`<button id="fleet-node-update-check" type="button" disabled=${busy || active} onClick=${() => start('check')}>Check now</button>
        ${v.update && html`<button id="fleet-node-update-apply" type="button" class="primary" disabled=${busy || active} onClick=${apply}>Update to ${v.latest}…</button>`}
        ${status.rollback_available && html`<button id="fleet-node-update-rollback" type="button" class="danger" disabled=${busy || active} onClick=${rollback}>Roll back…</button>`}`}
      <span class="spacer"></span>
      <button type="button" disabled=${busy} onClick=${onClose}>Close</button>
    </div>
    <div class="muted fa-cli-note">CLI: <code>tclaude update${node.local ? '' : ` --node ${node.id}`} --check|--apply|--rollback</code></div>
  </${Overlay}>`;
}
