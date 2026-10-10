import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { SCRIPT_MAX_BYTES, TIMEOUT_DEFAULT_S, TIMEOUT_MAX_S, runActive, runOK, scriptBytes } from './fleet-run-actions.js';

const html = htm.bind(h);

const RUN_POLL_MS = 1000;
// HUB_POLL_EVERY reads a hub job on every second tick (2 s): hub admin calls
// share the hub's per-connection control budget with everything else.
const HUB_POLL_EVERY = 2;
const RUN_POLL_GIVE_UP = 30;

function errText(error) { return error?.message || String(error); }

// ACCEPT_CONSEQUENCE is the remote-code-execution warning the accept switch
// and the node.exec grant both spell out.
export const ACCEPT_CONSEQUENCE = 'Any peer granted node.exec — and every unrestricted peer — can then run any shell command on this node as the user tclaude runs as: read and change your files, use your logins and keys, and reach whatever this machine can reach.';

// HUB_CONSEQUENCE is what running code on the hub can and cannot do; the run
// confirm and the Hub page both say it.
export const HUB_CONSEQUENCE = 'Code on the hub can disrupt every node\'s connectivity through it. Pinned identity keys mean it still cannot read or forge end-to-end node content.';

// hubTarget is the hub as a Run target, present while this node has one.
export function hubTarget(self) {
  if (!self?.hubURL) return null;
  let host = self.hubURL;
  try { host = new URL(self.hubURL).host || host; } catch (_) { /* keep the URL */ }
  return { id: 'hub', label: `hub ${host}`, hub: true, online: self.hubState === 'connected' };
}

// readiness says whether a node can take a script from this operator now:
// this node always can; a peer needs node.exec and its accept switch, and
// must be online; the hub needs hub.exec and its host-set switch.
export function readiness(row, probe) {
  if (row.local) return { ok: true, text: 'this node' };
  if (!row.online) return { ok: false, text: row.hub ? 'not connected' : 'offline' };
  if (row.hub) {
    if (!probe) return { ok: false, text: 'checking…' };
    if (probe.error?.status === 403 || probe.data?.can_exec === false) return { ok: false, text: 'needs hub.exec' };
    if (probe.error?.status === 404 || (probe.error?.status === 400 && probe.error?.code === 'operation')) return { ok: false, text: 'hub scripts not available on this build' };
    if (probe.error) return { ok: false, text: errText(probe.error) };
    if (!probe.data?.accept_remote_scripts) return { ok: false, text: 'does not accept remote scripts' };
    return { ok: true, text: 'ready' };
  }
  if (!probe) return { ok: false, text: 'checking…' };
  if (probe.error) {
    if (probe.error.status === 403) return { ok: false, text: 'not granted (needs node.exec)' };
    if (probe.error.status === 502 || probe.error.status === 504) return { ok: false, text: 'unreachable' };
    return { ok: false, text: errText(probe.error) };
  }
  if (!probe.data?.accept_remote_scripts) return { ok: false, text: 'does not accept remote scripts' };
  return { ok: true, text: 'ready' };
}

// START_ERRORS explain a refused start in the operator's terms.
const START_ERRORS = {
  remote_scripts_disabled: (n) => `${n.label} no longer accepts remote scripts`,
  hub_exec_required: () => 'This node no longer holds hub.exec on the hub',
  hub_root_refused: () => 'The hub refuses to run scripts because its service user is root',
  run_busy: (n) => `${n.label} is already running as many scripts as it allows; try again when one finishes`,
};

function startError(n, e) {
  return START_ERRORS[e?.code]?.(n) || errText(e);
}

function fmtDuration(ms) {
  if (ms == null || ms < 0) return '';
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  return s < 60 ? `${s.toFixed(1)} s` : `${Math.floor(s / 60)} min ${Math.round(s % 60)} s`;
}

function limitsText(l) {
  if (!l) return 'none';
  const parts = [];
  if (l.memory) parts.push(`memory ${l.memory}`);
  if (l.cpu != null) parts.push(`cpu ${l.cpu}`);
  if (l.pids != null) parts.push(`${l.pids} processes`);
  return parts.length ? parts.join(', ') : 'none';
}

// Settings is this node's receiving side: whether peers may run scripts here
// and the limits each script runs under. Local-only; peers can never change it.
function Settings({ actions, confirm, toast }) {
  const [settings, setSettings] = useState(null);
  const [error, setError] = useState('');
  const [memory, setMemory] = useState('');
  const [cpu, setCpu] = useState('');
  const [pids, setPids] = useState('');
  const fill = (s) => {
    setSettings(s);
    setMemory(s?.resource_limits?.memory || '');
    setCpu(s?.resource_limits?.cpu != null ? String(s.resource_limits.cpu) : '');
    setPids(s?.resource_limits?.pids != null ? String(s.resource_limits.pids) : '');
  };
  useEffect(() => { actions.settings().then(fill).catch((e) => setError(errText(e))); }, []);
  if (error) return html`<div class="cron-create-error" role="alert">Script settings: ${error}</div>`;
  if (!settings) return html`<div class="muted">Loading script settings…</div>`;
  const on = !!settings.accept_remote_scripts;
  const toggle = () => confirm({
    title: on ? 'Stop accepting remote scripts?' : 'Accept remote scripts on this node?',
    body: on
      ? 'Peers can no longer run scripts here. Scripts they are running now are canceled. Your own runs from this dashboard are not affected.'
      : `Full remote code execution. ${ACCEPT_CONSEQUENCE} Scripts run under the limits below (${limitsText(settings.resource_limits)}). Turn it off again to cancel their running scripts.`,
    okLabel: on ? 'Stop accepting' : 'Accept remote scripts',
    busyLabel: 'Saving…',
    action: () => actions.saveSettings({ accept_remote_scripts: !on }),
  }).then((s) => { if (s) { fill(s); toast(on ? 'Remote scripts are off' : 'Remote scripts are on', false); } })
    .catch((e) => toast(`Saving failed: ${errText(e)}`, true));
  // Limits replace the saved set as a whole: an empty field removes that
  // limit (the platform defaults do not come back). Removing or raising one
  // confirms, since remote scripts may then use more of this machine.
  const saveLimits = () => {
    const limits = {};
    const cur = settings.resource_limits || {};
    if (memory.trim()) limits.memory = memory.trim();
    if (cpu.trim()) {
      limits.cpu = Number(cpu);
      if (!(limits.cpu > 0)) { toast('cpu must be a positive number of cores', true); return Promise.resolve(); }
    }
    if (pids.trim()) {
      limits.pids = Number(pids);
      if (!Number.isInteger(limits.pids) || limits.pids < 1) { toast('processes must be a positive whole number', true); return Promise.resolve(); }
    }
    const removed = [cur.memory && !limits.memory && 'memory', cur.cpu != null && limits.cpu == null && 'cpu', cur.pids != null && limits.pids == null && 'processes'].filter(Boolean);
    const raised = [cur.cpu != null && limits.cpu > cur.cpu && 'cpu', cur.pids != null && limits.pids > cur.pids && 'processes', cur.memory && limits.memory && limits.memory !== cur.memory && 'memory (changed)'].filter(Boolean);
    const save = () => actions.saveSettings({ resource_limits: limits });
    const done = (s) => { if (s) { fill(s); toast('Script limits saved', false); } };
    const fail = (e) => toast(`Saving limits failed: ${errText(e)}`, true);
    if (!removed.length && !raised.length) return save().then(done).catch(fail);
    return confirm({
      title: 'Loosen the script limits?',
      body: `Scripts on this node will run with ${limitsText(limits)}${removed.length ? ` — no ${removed.join(', ')} limit any more; the defaults do not come back unless you set them again` : ''}${raised.length ? `${removed.length ? ';' : ' —'} changed: ${raised.join(', ')}` : ''}.${on ? ' This node accepts remote scripts, so peers\' scripts may use that much of this machine.' : ''}`,
      okLabel: 'Save limits',
      busyLabel: 'Saving…',
      action: save,
    }).then(done).catch(fail);
  };
  return html`<div class="fa-run-settings" id="fleet-run-settings">
    <span class="fa-k">This node</span>
    <span class=${on ? 'fa-warn' : 'muted'}>${on ? '⚠ accepts remote scripts from peers with node.exec' : 'does not accept remote scripts'}</span>
    <button id="fleet-run-accept" type="button" class=${on ? '' : 'fa-danger'} onClick=${toggle}>${on ? 'Stop accepting…' : 'Accept remote scripts…'}</button>
    <span class="fa-k">Limits</span>
    <label>memory <input id="fleet-run-memory" size="6" value=${memory} placeholder="none" onInput=${(e) => setMemory(e.currentTarget.value)} /></label>
    <label>cpu <input id="fleet-run-cpu" size="4" value=${cpu} placeholder="none" onInput=${(e) => setCpu(e.currentTarget.value)} /></label>
    <label>processes <input id="fleet-run-pids" size="5" value=${pids} placeholder="none" onInput=${(e) => setPids(e.currentTarget.value)} /></label>
    <button id="fleet-run-limits" type="button" onClick=${saveLimits}>Save limits</button>
  </div>`;
}

function LogView({ node, job, actions }) {
  const [log, setLog] = useState(null);
  const load = (stream) => {
    setLog({ stream, text: null });
    actions.fullLog(node, job.id, stream).then((text) => setLog({ stream, text }))
      .catch((e) => setLog({ stream, text: '', error: errText(e) }));
  };
  return html`<span class="fa-run-logs">
    <button type="button" class="fa-link" data-fa="log-stdout" onClick=${() => load('stdout')}>full stdout</button>
    <button type="button" class="fa-link" data-fa="log-stderr" onClick=${() => load('stderr')}>full stderr</button>
    ${log && html`<div class="fa-run-full">
      <div class="muted">${log.stream}${log.error ? ` — ${log.error}` : log.text == null ? ' — loading…' : ''} <button type="button" class="fa-link" onClick=${() => setLog(null)}>hide</button></div>
      ${log.text != null && !log.error && html`<pre class="fa-run-out">${log.text || '(empty)'}</pre>`}
    </div>`}
  </span>`;
}

function ResultPane({ node, entry, actions }) {
  const job = entry.job;
  const state = entry.error ? 'not started' : job?.state || 'starting';
  const bad = entry.error || (job && !runActive(job) && !runOK(job));
  return html`<div class=${`fa-run-pane${bad ? ' bad' : ''}`} data-node=${node.id}>
    <div class="fa-run-head"><b>${node.local ? '⌂ ' : node.hub ? '⬡ ' : ''}${node.label}</b>
      <span class=${bad ? 'fa-danger' : runActive(job) ? 'fa-warn' : ''}>${state}</span>
      ${job && !runActive(job) && html`<span>exit ${job.exit_code}</span><span class="muted">${fmtDuration(job.duration_ms)}</span>`}
      ${job && !runActive(job) && html`<${LogView} node=${node} job=${job} actions=${actions} />`}
    </div>
    ${entry.error && html`<div class="fa-danger">${entry.error}</div>`}
    ${job?.output_truncated && html`<div class="muted">Output reached the size limit and was cut off.</div>`}
    ${job?.error && html`<div class="fa-danger">${job.error}</div>`}
    ${job?.stdout_tail && html`<pre class="fa-run-out">${job.stdout_tail}</pre>`}
    ${job?.stderr_tail && html`<pre class="fa-run-out err">${job.stderr_tail}</pre>`}
  </div>`;
}

// RunPage runs one script on chosen nodes: this node and any trusted peer
// that granted node.exec and accepts remote scripts. Each node gets its own
// POST and its own polling; offline nodes are skipped, never queued.
export function RunPage({ view, actions, confirm, toast, timers = globalThis, preselect = '' }) {
  const hub = hubTarget(view.self);
  const nodes = [{ id: view.self.id, label: view.self.name, local: true, online: true }, ...view.trusted.map((r) => ({ id: r.id, label: r.label, online: r.online })), ...(hub ? [hub] : [])];
  const nodesKey = nodes.map((n) => `${n.id}:${n.online}`).join(',');
  const [probes, setProbes] = useState({});
  const [picked, setPicked] = useState(() => new Set(preselect ? [preselect] : []));
  const [script, setScript] = useState('');
  const [timeout, setTimeoutS] = useState(String(TIMEOUT_DEFAULT_S));
  const [runs, setRuns] = useState({});
  const runsRef = useRef(runs);
  runsRef.current = runs;

  useEffect(() => {
    let off = false;
    for (const n of nodes) {
      if (n.local || !n.online) continue;
      actions.status(n).then((data) => { if (!off) setProbes((p) => ({ ...p, [n.id]: { data } })); })
        .catch((error) => { if (!off) setProbes((p) => ({ ...p, [n.id]: { error } })); });
    }
    return () => { off = true; };
  }, [nodesKey]);

  const ready = (n) => readiness(n, probes[n.id]).ok;
  const anyActive = Object.values(runs).some((r) => runActive(r.job));
  // Poll every running job once a second from one loop that lives while any
  // job runs. A job that is gone or no longer readable (404/403: grant or
  // switch removed) stops; one whose node stays unreachable for
  // RUN_POLL_GIVE_UP reads in a row is marked unreachable so it never blocks
  // the page.
  const mounted = useRef(true);
  useEffect(() => () => { mounted.current = false; }, []);
  useEffect(() => {
    if (!anyActive) return undefined;
    let stopped = false; let t = null;
    const update = (id, fn) => { if (mounted.current) setRuns((cur) => (cur[id] ? { ...cur, [id]: fn(cur[id]) } : cur)); };
    let ticks = 0;
    const tick = async () => {
      ticks++;
      const active = Object.values(runsRef.current).filter((r) => runActive(r.job) && (!r.node.hub || ticks % HUB_POLL_EVERY === 0));
      await Promise.all(active.map((r) => actions.job(r.node, r.job.id)
        .then((job) => update(r.node.id, (e) => (e.job?.id === job.id ? { ...e, job, misses: 0 } : e)))
        .catch((err) => update(r.node.id, (e) => {
          if (e.job?.id !== r.job.id) return e;
          if (err?.status === 404 || err?.status === 403) return { ...e, job: { ...e.job, state: 'lost', error: errText(err) } };
          const misses = (e.misses || 0) + 1;
          return misses >= RUN_POLL_GIVE_UP ? { ...e, misses, job: { ...e.job, state: 'unreachable', error: `no answer for ${misses} s: ${errText(err)}` } } : { ...e, misses };
        }))));
      if (!stopped) t = timers.setTimeout(tick, RUN_POLL_MS);
    };
    t = timers.setTimeout(tick, RUN_POLL_MS);
    return () => { stopped = true; timers.clearTimeout(t); };
  }, [anyActive]);

  const chosen = nodes.filter((n) => picked.has(n.id) && ready(n));
  // The hub sets its own, usually tighter, limits; with the hub picked the
  // script and timeout must fit them so the confirmed timeout is the one used.
  const hubLimits = chosen.some((n) => n.hub) ? probes.hub?.data?.limits || {} : {};
  const maxBytes = Math.min(SCRIPT_MAX_BYTES, hubLimits.max_script_bytes || SCRIPT_MAX_BYTES);
  const maxTimeout = Math.min(TIMEOUT_MAX_S, hubLimits.max_timeout_seconds || TIMEOUT_MAX_S);
  const bytes = scriptBytes(script);
  const timeoutS = Math.trunc(Number(timeout));
  const timeoutOK = timeoutS >= 1 && timeoutS <= maxTimeout;
  const canRun = chosen.length > 0 && script.trim() && bytes <= maxBytes && timeoutOK && !anyActive;

  const toggle = (id) => setPicked((cur) => { const next = new Set(cur); if (next.has(id)) next.delete(id); else next.add(id); return next; });
  const allOnline = () => setPicked(new Set(nodes.filter(ready).map((n) => n.id)));

  // A new run replaces the previous results; a re-run replaces only the
  // nodes it targets. Each entry keeps the script it ran.
  const launch = (targets, text, secs, again) => {
    setRuns((cur) => {
      const next = again ? { ...cur } : {};
      for (const n of targets) next[n.id] = { node: n, job: null, text, secs };
      return next;
    });
    return Promise.all(targets.map((n) => actions.start(n, text, secs)
      .then((job) => { if (mounted.current) setRuns((cur) => ({ ...cur, [n.id]: { node: n, job, text, secs } })); })
      .catch((e) => { if (mounted.current) setRuns((cur) => ({ ...cur, [n.id]: { node: n, job: null, text, secs, error: startError(n, e) } })); })));
  };
  // The hub runs it as its own service user, and code there reaches the
  // whole fleet's connectivity, so a hub target is spelled out separately.
  const confirmRun = (targets, text, secs, again) => {
    const onHub = targets.find((n) => n.hub);
    const nodesOnly = targets.filter((n) => !n.hub);
    const user = probes.hub?.data?.service_user;
    return confirm({
    title: `${again ? 'Re-run' : 'Run'} the script on ${targets.length} ${onHub && !nodesOnly.length ? 'hub' : `node${targets.length === 1 ? '' : 's'}`}${onHub && nodesOnly.length ? ' including the hub' : ''}?`,
    body: `${nodesOnly.length ? `Runs it with /bin/sh as the tclaude user on ${nodesOnly.map((n) => n.label).join(', ')}` : 'Runs it with /bin/sh'}${onHub ? `${nodesOnly.length ? ', and' : ''} on the hub (${onHub.label.replace(/^hub /, '')}) as its service user${user ? ` ${user}` : ''}` : ''}, with a ${secs} s timeout. It can do anything that user can on those machines.${onHub ? ` ${HUB_CONSEQUENCE} The hub records the full script in its audit.` : ''} Each target runs it independently; nothing is queued for targets that are offline.`,
    okLabel: again ? 'Re-run' : 'Run',
    }).then((ok) => ok && launch(targets, text, secs, again));
  };
  const run = () => confirmRun(chosen, script, timeoutS, false);
  // Every pane holds the same script (a new run replaces them all), so the
  // re-run sends that script to the failed nodes that are still ready.
  const failedRuns = Object.values(runs).filter((r) => r.error || (r.job && !runActive(r.job) && !runOK(r.job)));
  const failed = failedRuns.map((r) => r.node).filter((n) => ready(n));
  const rerun = () => confirmRun(failed, failedRuns[0].text, failedRuns[0].secs, true);

  return html`<div class="fa-run">
    <${Settings} actions=${actions} confirm=${confirm} toast=${toast} />
    <h4>Run a script</h4>
    <div class="fa-run-nodes" id="fleet-run-nodes">
      ${nodes.map((n) => { const r = readiness(n, probes[n.id]); return html`<label key=${n.id} class=${r.ok ? '' : 'muted'} data-node=${n.id}>
        <input type="checkbox" disabled=${!r.ok} checked=${picked.has(n.id) && r.ok} onChange=${() => toggle(n.id)} />
        ${n.local ? '⌂ ' : n.hub ? '⬡ ' : ''}${n.label} <span class="muted">${r.text}</span></label>`; })}
      <button id="fleet-run-all" type="button" class="fa-link" onClick=${allOnline}>all online</button>
    </div>
    <textarea id="fleet-run-script" class="fa-run-script" rows="8" spellcheck="false" placeholder="#!/bin/sh — runs with /bin/sh in the tclaude user's home directory" value=${script} onInput=${(e) => setScript(e.currentTarget.value)}></textarea>
    <div class="fa-run-bar">
      <span class=${bytes > maxBytes ? 'fa-danger' : 'muted'}>${bytes} / ${maxBytes} bytes</span>
      <label><span class="fa-k">Timeout</span> <input id="fleet-run-timeout" size="6" value=${timeout} onInput=${(e) => setTimeoutS(e.currentTarget.value)} /> s</label>
      ${!timeoutOK && html`<span class="fa-danger">1–${maxTimeout} s${maxTimeout < TIMEOUT_MAX_S ? ' (hub limit)' : ''}</span>`}
      <button id="fleet-run-submit" type="button" class="primary" disabled=${!canRun} onClick=${run}>Run on ${chosen.length} node${chosen.length === 1 ? '' : 's'}…</button>
      ${failed.length > 0 && !anyActive && html`<button id="fleet-run-rerun" type="button" onClick=${rerun}>Re-run on ${failed.length} failed…</button>`}
    </div>
    ${Object.keys(runs).length > 0 && html`<div class="fa-run-results" id="fleet-run-results">
      ${nodes.filter((n) => runs[n.id]).map((n) => html`<${ResultPane} key=${n.id} node=${n} entry=${runs[n.id]} actions=${actions} />`)}
    </div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation run --node … | --all --file script.sh</code>; receiving settings: <code>tclaude federation scripts</code>. Peers need the <code>node.exec</code> grant (Peer grants) and this switch on their own node.</div>
  </div>`;
}
