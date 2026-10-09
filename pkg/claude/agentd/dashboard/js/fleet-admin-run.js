import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { SCRIPT_MAX_BYTES, TIMEOUT_DEFAULT_S, TIMEOUT_MAX_S, runActive, runOK, scriptBytes } from './fleet-run-actions.js';

const html = htm.bind(h);

const RUN_POLL_MS = 1000;

function errText(error) { return error?.message || String(error); }

// ACCEPT_CONSEQUENCE is the remote-code-execution warning the accept switch
// and the node.exec grant both spell out.
export const ACCEPT_CONSEQUENCE = 'Any peer granted node.exec — and every unrestricted peer — can then run any shell command on this node as the user tclaude runs as: read and change your files, use your logins and keys, and reach whatever this machine can reach.';

// readiness says whether a node can take a script from this operator now:
// this node always can; a peer needs node.exec and its accept switch, and
// must be online.
export function readiness(row, probe) {
  if (row.local) return { ok: true, text: 'this node' };
  if (!row.online) return { ok: false, text: 'offline' };
  if (!probe) return { ok: false, text: 'checking…' };
  if (probe.error) {
    if (probe.error.status === 403) return { ok: false, text: 'not granted (needs node.exec)' };
    if (probe.error.status === 502 || probe.error.status === 504) return { ok: false, text: 'unreachable' };
    return { ok: false, text: errText(probe.error) };
  }
  if (!probe.data?.accept_remote_scripts) return { ok: false, text: 'does not accept remote scripts' };
  return { ok: true, text: 'ready' };
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
  const saveLimits = () => {
    const limits = {};
    if (memory.trim()) limits.memory = memory.trim();
    if (cpu.trim()) limits.cpu = Number(cpu);
    if (pids.trim()) limits.pids = Math.trunc(Number(pids));
    return actions.saveSettings({ resource_limits: limits })
      .then((s) => { fill(s); toast('Script limits saved', false); })
      .catch((e) => toast(`Saving limits failed: ${errText(e)}`, true));
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
    <div class="fa-run-head"><b>${node.local ? '⌂ ' : ''}${node.label}</b>
      <span class=${bad ? 'fa-danger' : runActive(job) ? 'fa-warn' : ''}>${state}</span>
      ${job && !runActive(job) && html`<span>exit ${job.exit_code}</span><span class="muted">${fmtDuration(job.duration_ms)}</span>`}
      ${job && !runActive(job) && html`<${LogView} node=${node} job=${job} actions=${actions} />`}
    </div>
    ${entry.error && html`<div class="fa-danger">${entry.error}</div>`}
    ${job?.error && html`<div class="fa-danger">${job.error}</div>`}
    ${job?.stdout_tail && html`<pre class="fa-run-out">${job.stdout_tail}</pre>`}
    ${job?.stderr_tail && html`<pre class="fa-run-out err">${job.stderr_tail}</pre>`}
  </div>`;
}

// RunPage runs one script on chosen nodes: this node and any trusted peer
// that granted node.exec and accepts remote scripts. Each node gets its own
// POST and its own polling; offline nodes are skipped, never queued.
export function RunPage({ view, actions, confirm, toast, timers = globalThis }) {
  const nodes = [{ id: view.self.id, label: view.self.name, local: true, online: true }, ...view.trusted.map((r) => ({ id: r.id, label: r.label, online: r.online }))];
  const nodesKey = nodes.map((n) => `${n.id}:${n.online}`).join(',');
  const [probes, setProbes] = useState({});
  const [picked, setPicked] = useState(() => new Set());
  const [script, setScript] = useState('');
  const [timeout, setTimeoutS] = useState(String(TIMEOUT_DEFAULT_S));
  const [runs, setRuns] = useState({});
  const [lastScript, setLastScript] = useState(null);
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
  useEffect(() => {
    if (!anyActive) return undefined;
    let disposed = false;
    const t = timers.setTimeout(async () => {
      await Promise.all(Object.values(runsRef.current).filter((r) => runActive(r.job)).map((r) => actions.job(r.node, r.job.id)
        .then((job) => { if (!disposed) setRuns((cur) => ({ ...cur, [r.node.id]: { ...cur[r.node.id], job } })); })
        .catch((e) => {
          // Gone or no longer readable (grant or switch removed): stop polling.
          if (!disposed && (e?.status === 404 || e?.status === 403)) setRuns((cur) => ({ ...cur, [r.node.id]: { ...cur[r.node.id], job: { ...r.job, state: 'lost', error: errText(e) } } }));
        })));
      if (!disposed) setRuns((cur) => ({ ...cur }));
    }, RUN_POLL_MS);
    return () => { disposed = true; timers.clearTimeout(t); };
  }, [anyActive, runs]);

  const bytes = scriptBytes(script);
  const timeoutS = Math.trunc(Number(timeout));
  const timeoutOK = timeoutS >= 1 && timeoutS <= TIMEOUT_MAX_S;
  const chosen = nodes.filter((n) => picked.has(n.id) && ready(n));
  const canRun = chosen.length > 0 && script.trim() && bytes <= SCRIPT_MAX_BYTES && timeoutOK && !anyActive;

  const toggle = (id) => setPicked((cur) => { const next = new Set(cur); if (next.has(id)) next.delete(id); else next.add(id); return next; });
  const allOnline = () => setPicked(new Set(nodes.filter(ready).map((n) => n.id)));

  const launch = (targets, text, secs) => {
    setLastScript({ text, secs });
    setRuns((cur) => {
      const next = { ...cur };
      for (const n of targets) next[n.id] = { node: n, job: null };
      return next;
    });
    return Promise.all(targets.map((n) => actions.start(n, text, secs)
      .then((job) => setRuns((cur) => ({ ...cur, [n.id]: { node: n, job } })))
      .catch((e) => setRuns((cur) => ({ ...cur, [n.id]: { node: n, job: null, error: e?.code === 'remote_scripts_disabled' ? `${n.label} no longer accepts remote scripts` : errText(e) } })))));
  };
  const confirmRun = (targets, text, secs, again) => confirm({
    title: `${again ? 'Re-run' : 'Run'} the script on ${targets.length} node${targets.length === 1 ? '' : 's'}?`,
    body: `Runs it with /bin/sh as the tclaude user on ${targets.map((n) => n.label).join(', ')}, with a ${secs} s timeout. It can do anything that user can on those machines. Each node runs it independently; nothing is queued for nodes that are offline.`,
    okLabel: again ? 'Re-run' : 'Run',
  }).then((ok) => ok && launch(targets, text, secs));
  const run = () => confirmRun(chosen, script, timeoutS, false);
  const failed = Object.values(runs).filter((r) => r.error || (r.job && !runActive(r.job) && !runOK(r.job))).map((r) => r.node);
  const rerun = () => confirmRun(failed, lastScript.text, lastScript.secs, true);

  return html`<div class="fa-run">
    <${Settings} actions=${actions} confirm=${confirm} toast=${toast} />
    <h4>Run a script</h4>
    <div class="fa-run-nodes" id="fleet-run-nodes">
      ${nodes.map((n) => { const r = readiness(n, probes[n.id]); return html`<label key=${n.id} class=${r.ok ? '' : 'muted'} data-node=${n.id}>
        <input type="checkbox" disabled=${!r.ok} checked=${picked.has(n.id) && r.ok} onChange=${() => toggle(n.id)} />
        ${n.local ? '⌂ ' : ''}${n.label} <span class="muted">${r.text}</span></label>`; })}
      <button id="fleet-run-all" type="button" class="fa-link" onClick=${allOnline}>all online</button>
    </div>
    <textarea id="fleet-run-script" class="fa-run-script" rows="8" spellcheck="false" placeholder="#!/bin/sh — runs with /bin/sh in the tclaude user's home directory" value=${script} onInput=${(e) => setScript(e.currentTarget.value)}></textarea>
    <div class="fa-run-bar">
      <span class=${bytes > SCRIPT_MAX_BYTES ? 'fa-danger' : 'muted'}>${bytes} / ${SCRIPT_MAX_BYTES} bytes</span>
      <label><span class="fa-k">Timeout</span> <input id="fleet-run-timeout" size="6" value=${timeout} onInput=${(e) => setTimeoutS(e.currentTarget.value)} /> s</label>
      ${!timeoutOK && html`<span class="fa-danger">1–${TIMEOUT_MAX_S} s</span>`}
      <button id="fleet-run-submit" type="button" class="primary" disabled=${!canRun} onClick=${run}>Run on ${chosen.length} node${chosen.length === 1 ? '' : 's'}…</button>
      ${failed.length > 0 && !anyActive && lastScript && html`<button id="fleet-run-rerun" type="button" onClick=${rerun}>Re-run on ${failed.length} failed…</button>`}
    </div>
    ${Object.keys(runs).length > 0 && html`<div class="fa-run-results" id="fleet-run-results">
      ${nodes.filter((n) => runs[n.id]).map((n) => html`<${ResultPane} key=${n.id} node=${n} entry=${runs[n.id]} actions=${actions} />`)}
    </div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation run --node … | --all --file script.sh</code>; receiving settings: <code>tclaude federation scripts</code>. Peers need the <code>node.exec</code> grant (Peer grants) and this switch on their own node.</div>
  </div>`;
}
