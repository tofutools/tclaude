import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { Dialog, DialogClose } from './ui-dialog.js';
import { RequestError, requestJSON } from './request-json.js';
import { errText } from './cell-format.js';
import { NodeUpdateSettings } from './node-update-settings.js';
import { SELF_UPDATE_METHODS } from './config-self-update.js';
import { ServicePanel } from './config-service.js';
import { LauncherPanel } from './config-launcher.js';

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
    // A dry run of Update now under the node's saved settings; nothing is
    // installed. A peer's needs node.update. The hub has none.
    preflight: (node) => call('GET', `${updateBase(node)}/preflight`),
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

// useNodeUpdate reads a node's update status and follows its job: while a
// job runs the status is re-read every second, and failed reads (the daemon
// restarting) are retried rather than shown. start(action, version) starts a
// check, apply or rollback and toasts a refusal.
export function useNodeUpdate({ node, actions, toast, timers = globalThis }) {
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
  return { status, error, busy, job, active, unanswered, start };
}

// usePreflight runs a node's update dry run on demand (run()), keeping the
// last result.
export function usePreflight({ node, actions }) {
  const [plan, setPlan] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => { setPlan(null); setError(null); }, [node.id]);
  const run = () => {
    setBusy(true);
    return actions.preflight(node)
      .then((p) => { setPlan(p); setError(null); })
      .catch((e) => { setPlan(null); setError(e); })
      .finally(() => setBusy(false));
  };
  return { plan, error, busy, run };
}

// UpdateJobView is an update job's progress or result, then its launch
// report, its own warnings (not the node's standing ones in `standing`) and
// the custom command's or package manager's output. idp prefixes the ids.
export function UpdateJobView({ job, unanswered = false, standing = [], idp = 'fleet-' }) {
  if (!job) return null;
  const launch = launchView(job.launch);
  const warnings = updateJobActive(job) ? [] : (job.warnings || []).filter((w) => !standing.includes(w));
  return html`<div id=${`${idp}node-update-job`} class=${job.state === 'failed' || job.state === 'rolled_back' || job.rolled_back ? 'fa-danger' : ''}>
      ${job.action} ${job.from_version ? `${job.from_version} → ` : ''}${job.version || ''}: <b>${unanswered ? `${job.state === 'rolling_back' || job.phase === 'rolling_back' ? 'rolling back' : 'restarting'}… (not answering yet)` : job.state.replaceAll('_', ' ')}</b>${job.phase && job.phase !== job.state && !(job.state === 'completed' && job.phase === 'complete') ? ` (${job.phase.replaceAll('_', ' ')})` : ''}${job.rolled_back && job.state !== 'rolled_back' ? ' — rolled back' : ''}${job.error ? ` — ${job.error}` : ''}
      ${job.deadline && updateJobActive(job) && html`<span class="muted"> · healthy by ${new Date(job.deadline).toLocaleTimeString()} or it rolls back</span>`}
    </div>
    ${launch && html`<div id=${`${idp}node-update-launch`} class=${launch.ok ? 'muted' : 'fa-warn'}>${launch.text}${launch.details && html`<ul>${launch.details.map((d) => html`<li key=${d}>${d}</li>`)}</ul>`}</div>`}
    ${warnings.length > 0 && html`<ul class="fa-warn update-job-warnings" id=${`${idp}node-update-job-warnings`}>${warnings.map((w) => html`<li key=${w}>${w}</li>`)}</ul>`}
    ${job.output && !updateJobActive(job) && html`<details class="update-job-output"><summary>Output</summary><pre id=${`${idp}node-update-job-output`}>${job.output}</pre></details>`}`;
}

const PLAN_MARKS = { ok: '✓', warn: '⚠', fail: '✗' };

// UpdatePlanView is a dry run's result: ready or not, each check, what an
// update would run, and what only a real update finds out.
export function UpdatePlanView({ plan, error = null, node, idp = 'fleet-' }) {
  if (error) {
    const text = error?.status === 404 ? `${node?.label || 'This node'} cannot test its update method yet (an older tclaude).`
      : error?.status === 403 ? `${node?.label || 'This node'} does not let you test its update method (needs node.update).`
        : `Could not test the update method: ${errText(error)}`;
    return html`<div class="update-plan fa-danger" id=${`${idp}update-plan`} role="alert">${text}</div>`;
  }
  if (!plan) return null;
  return html`<div class="update-plan" id=${`${idp}update-plan`} data-ready=${plan.ready ? 'true' : 'false'}>
    <div class="update-plan-head"><b class=${plan.ready ? 'update-plan-ok' : 'fa-danger'}>${plan.ready ? 'Ready ✓' : 'Would fail ✗'}</b>
      <span class="muted"> Test run of the saved method; nothing was installed.</span></div>
    <ul class="update-plan-checks">${(plan.checks || []).map((c, i) => html`<li key=${i} data-state=${c.state}>
      <span class=${`update-plan-mark update-plan-${c.state}`} aria-label=${c.state}>${PLAN_MARKS[c.state] || '·'}</span> <b>${c.name}</b>: ${c.detail}</li>`)}</ul>
    ${(plan.steps || []).length > 0 && html`<div class="update-plan-sub">Update now would:</div>
      <ol class="update-plan-steps">${plan.steps.map((step, i) => html`<li key=${i}>${step}</li>`)}</ol>`}
    ${(plan.not_checked || []).length > 0 && html`<details class="update-plan-unchecked"><summary>Not checked by a test run</summary>
      <ul>${plan.not_checked.map((n, i) => html`<li key=${i}>${n}</li>`)}</ul></details>`}
  </div>`;
}

// applyConfirm is the confirmation for an apply: what the node will do, by
// method. method is the node's saved updates.self.method ('' when none is
// chosen, which keeps each binary's own behaviour); methodLabel names it.
export function applyConfirm({ node, status, method = '', methodLabel = '', version = '' }) {
  const v = versionView(status);
  const target = version || v.latest;
  const warnings = (status?.warnings || []).length ? ` Warnings: ${status.warnings.join('; ')}.` : '';
  const restart = 'Its agent sessions keep running; dashboards and peer links reconnect after the restart.';
  if (method === 'custom') {
    return {
      title: `Run the custom update command on ${node.label}?`,
      body: `${node.label} runs its custom update command with /bin/sh -c, even when it is already on the latest release. Afterwards tclaude checks each binary still runs as tclaude and speaks the same federation protocol. If the command installed a new binary, the current ones are kept for rollback and its daemon restarts; if it changed nothing, that counts as already up to date. ${restart}${warnings}`,
      okLabel: 'Run it',
    };
  }
  if (method && method !== 'release' && method !== 'go_install') {
    return {
      title: `Update tclaude on ${node.label} now?`,
      body: `${node.label} updates tclaude with ${methodLabel || method}${target ? ` (the latest release is ${target})` : ''}, checks the new binaries, and restarts its daemon if they changed. ${restart}${warnings}`,
      okLabel: 'Update',
    };
  }
  const built = method === 'go_install' || (!method && (status?.binaries || []).some((b) => b.install_method && b.install_method !== 'release'));
  const what = target || 'the latest release';
  return {
    title: target ? `Update tclaude on ${node.label} to ${target}?` : `Update tclaude on ${node.label} now?`,
    body: `${node.label} ${built ? `builds tclaude ${what} with go install (it is not a release install; local modifications are replaced)` : `downloads and verifies the official tclaude ${what} release`}, replaces its tclaude binaries (the current ones are kept for rollback) and restarts its daemon. ${restart}${warnings}`,
    okLabel: 'Update',
  };
}

// savedMethod is the node's saved update method from its status read, with
// a label; fallback names it before the read (or on an older node).
function savedMethod(status, fallback = '') {
  const id = status && 'chosen_method' in status ? status.chosen_method || '' : fallback;
  return { id, label: SELF_UPDATE_METHODS.find((m) => m.id === id)?.label || id };
}

// NodeUpdateDialog is a node's Installation view, as Config › Installation
// is this node's: its tclaude version, where the operator checks for, applies
// or rolls back an update or tests the update method, then the Start
// automatically, App launcher and Updates parts (the hub has only the
// version). The parts read their own routes under their own grants, so they
// show whether or not the version read works. Apply and rollback confirm with
// the consequence (binaries replaced, daemon restarts); a source build warns
// that the official release replaces it.
export function NodeUpdateDialog({ node, confirm, toast, onClose, actions = defaultActions(), timers = globalThis }) {
  const { status, error, busy, job, active, unanswered, start } = useNodeUpdate({ node, actions, toast, timers });
  const canTest = !node.hub && !!actions.preflight;
  const test = usePreflight({ node, actions: canTest ? actions : { preflight: async () => null } });
  const v = versionView(status);
  // The hub restarts under its service supervisor and rolls itself back if
  // the new binary is not healthy in time; while it restarts every node's hub
  // connection drops.
  const blocked = node.hub && status?.blocked;
  const supervisor = status?.supervisor;
  // A custom command may track a branch: Update now runs it with or without
  // a newer release, and never pins one.
  const chosen = savedMethod(status);
  const custom = !node.hub && chosen.id === 'custom';
  const apply = () => confirm(node.hub ? {
    title: `Update the hub to ${v.latest}?`,
    body: `The hub downloads the official tclaude-hub ${v.latest} release, verifies its checksum, stages it next to ${v.current || 'the current binary'} and restarts under ${supervisor || 'its supervisor'}. Every node's hub connection drops and reconnects while it restarts. If the new hub is not healthy by the deadline it rolls back to ${v.current || 'the current version'} automatically.`
      + ((status?.warnings || []).length ? ` Warnings: ${status.warnings.join('; ')}.` : ''),
    okLabel: 'Update hub',
  } : applyConfirm({ node, status, method: chosen.id, methodLabel: chosen.label, version: custom ? '' : v.latest })).then((ok) => ok && start('apply', custom ? undefined : v.latest));
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
      ${canTest && html`<${Button} id="fleet-node-update-test" disabled=${busy || active || test.busy} title="Dry-run the saved update method: what it would run and the local checks; installs nothing" onClick=${test.run}>${test.busy ? 'Testing…' : 'Test method'}</${Button}>`}
      ${(v.update || custom) && html`<${Button} variant="primary" id="fleet-node-update-apply" disabled=${busy || active || !!blocked} onClick=${apply}>${custom ? 'Update now…' : `Update to ${v.latest}…`}</${Button}>`}
      ${status.rollback_available && html`<${Button} variant="danger" id="fleet-node-update-rollback" disabled=${busy || active || !!blocked} onClick=${rollback}>Roll back…</${Button}>`}`}
    footer=${html`<${DialogClose} disabled=${busy}>Close<//>`}
    after=${html`<div class="muted fa-cli-note">CLI: <code>${node.hub ? 'tclaude federation hub update' : `tclaude update${node.local ? '' : ` --node ${node.id}`}`} --check|--dry-run|--apply|--rollback</code></div>`}>
    ${error ? html`<div class="muted" role="alert">${accessText(error, node)}</div>` : !status ? html`<div class="muted">Loading…</div>` : html`
      <div class="fa-dl">
        <span class="fa-k">Version</span><span>${v.current || 'unknown'}${status.install_method ? html` <span class="muted">(${status.install_method.replaceAll('_', ' ')})</span>` : ''}</span>
        <span class="fa-k">Latest</span><span>${v.latest || html`<span class="muted">not checked</span>`}${v.update ? html` <span class="fa-warn">update available</span>` : status.update_available === false ? html` <span class="muted">up to date</span>` : ''}${status.checked_at ? html` <span class="muted">· checked ${new Date(status.checked_at).toLocaleString()}</span>` : ''}</span>
        ${node.hub && html`<span class="fa-k">Supervisor</span><span id="fleet-hub-supervisor">${supervisor || html`<span class="fa-danger">none</span>`}</span>`}
        ${(status.binaries || []).map((b) => html`<span key=${b.name} class="fa-k">${b.name}</span><span><code>${b.version}</code> <span class="muted">${b.path}</span></span>`)}
      </div>
      ${(status.warnings || []).map((w) => html`<div key=${w} class="fa-warn">${w}</div>`)}
      ${blocked && html`<div class="fa-danger" role="alert" id="fleet-hub-update-blocked">${blocked.code === 'not_supervised' ? 'The hub is not running under systemd or launchd, so it cannot restart itself or roll back safely; update refused' : 'Update is not possible'}${blocked.message ? `: ${blocked.message}` : '.'}</div>`}
      <${UpdateJobView} job=${job} unanswered=${unanswered} standing=${status.warnings || []} idp="fleet-" />
`}
    ${canTest && html`<${UpdatePlanView} plan=${test.plan} error=${test.error} node=${node} idp="fleet-" />`}
    ${!node.hub && html`<div class="fa-installation">
      ${actions.fetchImpl && html`<${ServicePanel} idp="fleet-" base=${serviceBase(node)} fetchImpl=${actions.fetchImpl} confirm=${confirm} toast=${toast} timers=${timers} />`}
      ${actions.fetchImpl && html`<${LauncherPanel} idp="fleet-" base=${serviceBase(node)} fetchImpl=${actions.fetchImpl} confirm=${confirm} toast=${toast} />`}
      ${actions.settings && html`<${NodeUpdateSettings} node=${node} actions=${actions} confirm=${confirm} toast=${toast} />`}
    </div>`}
  </${Dialog}>`;
}

// UpdateNowPanel is Config › Installation › Updates' try-it row: Check now,
// Test method and Update now, all under the node's SAVED update settings.
// With unsaved method edits in the form it says so and holds back Test and
// Update until they are saved. method/methodLabel are the saved method;
// left undefined, the status read's chosen_method is used.
export function UpdateNowPanel({ node, method, methodLabel = '', notifyOnly: notifyOnlyProp = false, unsaved = false, confirm, toast, actions = defaultActions(), timers = globalThis, idp = 'cfg-' }) {
  const { status, error, busy, job, active, unanswered, start } = useNodeUpdate({ node, actions, toast, timers });
  const test = usePreflight({ node, actions });
  const v = versionView(status);
  const held = busy || active || !status;
  // The caller's saved method (Config re-reads it after each save) wins
  // over the status read's, which is taken once.
  const chosen = method !== undefined ? { id: method, label: methodLabel || savedMethod(null, method).label } : savedMethod(status);
  const notifyOnly = notifyOnlyProp || chosen.id === 'manual';
  const update = () => confirm(applyConfirm({ node, status, method: chosen.id, methodLabel: chosen.label })).then((ok) => ok && start('apply'));
  const updateTitle = notifyOnly ? 'The saved method only notifies; choose a method that installs'
    : unsaved ? 'Save changes first: Update now uses the saved method' : '';
  return html`<div class="update-now" id=${`${idp}update-now`}>
    <div class="update-now-actions">
      <${Button} size="sm" id=${`${idp}update-now-check`} disabled=${held} title="Ask GitHub whether a newer release exists" onClick=${() => start('check')}>Check now<//>
      <${Button} size="sm" id=${`${idp}update-now-test`} disabled=${held || test.busy || unsaved} title=${unsaved ? 'Save changes first: the test uses the saved method' : 'Dry-run the saved method: what it would run and the local checks; installs nothing'} onClick=${test.run}>${test.busy ? 'Testing…' : 'Test method'}<//>
      <${Button} size="sm" variant="primary" id=${`${idp}update-now-apply`} disabled=${held || unsaved || notifyOnly} title=${updateTitle} onClick=${update}>Update now…<//>
      <span class="update-now-version muted" id=${`${idp}update-now-version`}>${error ? accessText(error, node) : !status ? 'Reading the version…'
        : html`${v.current || 'unknown version'}${v.latest ? ` · latest ${v.latest}` : ' · latest not checked'}${v.update ? html` <span class="fa-warn">update available</span>` : status.update_available === false ? ' · up to date' : ''}`}</span>
    </div>
    <div class=${unsaved ? 'cfg-su-warn' : 'cfg-su-note'} id=${`${idp}update-now-note`}>${unsaved
      ? 'You changed the method settings above. These buttons use the saved settings: save changes first.'
      : `These use the saved method${chosen.id ? ` (${chosen.label})` : ' (none chosen: each binary updates the way it was installed)'}. Test method runs nothing.`}</div>
    ${status && html`<${UpdateJobView} job=${job} unanswered=${unanswered} standing=${status.warnings || []} idp=${idp} />`}
    <${UpdatePlanView} plan=${test.plan} error=${test.error} node=${node} idp=${idp} />
  </div>`;
}
