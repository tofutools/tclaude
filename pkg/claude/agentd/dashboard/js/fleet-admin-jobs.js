import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { shortID } from './fleet-admin-model.js';

const html = htm.bind(h);

const POLL_MS = 5000;
// FOLLOW_MS paces live output reads; OUTPUT_KEEP caps what a stream keeps.
export const FOLLOW_MS = 2000;
export const OUTPUT_KEEP = 512 * 1024;
const TERMINAL = new Set(['completed', 'failed', 'canceled', 'timeout', 'refused', 'interrupted', 'output_unavailable']);
const BAD = new Set(['failed', 'canceled', 'timeout', 'refused', 'interrupted', 'output_unavailable', 'unknown']);
const REPO_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

function errText(error) { return error?.message || String(error); }

// expired pending jobs can no longer start (approve is a silent no-op).
function expired(j) { const t = Date.parse(j.expires || ''); return Number.isFinite(t) && t > 0 && t <= Date.now(); }

function when(iso) {
  const t = new Date(iso);
  return Number.isFinite(t.getTime()) && t.getFullYear() > 1 ? t.toLocaleString() : '—';
}

function parse(raw) {
  if (raw && typeof raw === 'object') return raw;
  try { return JSON.parse(raw || 'null') || {}; } catch (_) { return {}; }
}

// jobView flattens a durable job row: the immutable request and, once known,
// the result.
export function jobView(j) {
  const q = parse(j.request);
  const r = parse(j.result);
  return {
    id: j.id, incoming: j.direction === 'in', peer: j.peer, state: j.state,
    repo: q.repo || '', ref: q.ref || '', group: q.group || '', harness: q.harness || '', command: q.command || '',
    timeout: q.timeout_seconds || 0, require: q.require || '',
    code: r.code || '', exit: r.state ? r.exit_code : null, commit: r.commit || '',
    created: j.created_at, terminal: TERMINAL.has(j.state), caller: j.caller_agent || '',
    // Output exists only when the result carries a log artifact.
    hasLogs: !!r.logs, expires: j.expires_at,
  };
}

// groupText names a repo's receiving groups: by name where the daemon sends
// them, else by stable ID.
export function groupText(repo) {
  const names = repo.group_names;
  if (Array.isArray(names) && names.length) return names.join(', ');
  const ids = repo.definition?.groups || [];
  return ids.length ? ids.map((id) => `group #${id}`).join(', ') : 'no group';
}

// runBody is the submit request: one peer, several (fan-out), or automatic
// placement (auto or group:<pool>) with a preference.
export function runBody(form) {
  const body = { repo: form.repo.trim(), ref: form.ref.trim(), group: form.group.trim(), command: form.command, timeout_seconds: Number(form.timeout) || 3600 };
  if (form.harness) body.harness = form.harness;
  if (form.require.trim()) body.require = form.require.trim();
  if (form.target === 'pick') {
    if (form.nodes.length === 1) body.peer = form.nodes[0];
    else body.nodes = [...form.nodes];
  } else {
    body.peer = form.target;
    if (form.prefer) body.prefer = form.prefer;
  }
  return body;
}

function RunJobDialog({ peers, pools, actions, confirm, onClose, onDone }) {
  const [form, setForm] = useState({ target: 'pick', nodes: [], repo: '', ref: '', group: '', harness: '', command: '', timeout: '3600', require: '', prefer: 'least-loaded' });
  const [error, setError] = useState('');
  const set = (k) => (e) => setForm({ ...form, [k]: e.currentTarget.value });
  const toggle = (id) => (e) => setForm({ ...form, nodes: e.currentTarget.checked ? [...form.nodes, id] : form.nodes.filter((x) => x !== id) });
  const label = (id) => peers.find((p) => p.id === id)?.label || shortID(id);
  const submit = () => {
    if (form.target === 'pick' && !form.nodes.length) { setError('Pick at least one node.'); return; }
    if (!REPO_RE.test(form.repo.trim())) { setError('Name the repository alias the peer allowed (tclaude federation repos ls on that node).'); return; }
    if (!form.ref.trim() || !form.group.trim() || !form.command.trim()) { setError('Ref, receiving group and command are required.'); return; }
    const t = Number(form.timeout);
    if (!Number.isInteger(t) || t < 1) { setError('The timeout is a whole number of seconds.'); return; }
    setError('');
    const body = runBody(form);
    const where = form.target === 'pick' ? form.nodes.map(label).join(', ') : form.target === 'auto' ? 'one trusted node chosen automatically' : `one node of pool ${form.target.slice(6)}`;
    confirm({
      title: `Run a job on ${form.target === 'pick' && form.nodes.length === 1 ? label(form.nodes[0]) : form.target === 'pick' ? `${form.nodes.length} nodes` : 'an automatically placed node'}?`,
      body: `Sends this ${form.harness ? `${form.harness} task` : 'shell command'} to ${where}: a one-shot worker runs it in a checkout of ${body.repo} at ${body.ref} in group ${body.group}, for up to ${t} s. It runs as soon as a node admits it — unless that node's jobs.run grant to you asks for manual approval, then it waits for its operator — and it runs under that node's credentials. The output comes back here when it finishes.`,
      okLabel: 'Send job',
      busyLabel: 'Sending…',
      action: () => actions.runJob(body),
    }).then((r) => {
      if (!r) return;
      const results = r.results || [r];
      const failed = results.filter((x) => x.status && x.status >= 400);
      const undelivered = results.filter((x) => x.delivered === false);
      onDone(`${results.length - failed.length} job${results.length - failed.length === 1 ? '' : 's'} sent${failed.length ? `; ${failed.length} refused (${failed.map((x) => `${label(x.peer)}: ${x.error || x.status}`).join('; ')})` : ''}${undelivered.length ? `; ${undelivered.length} not delivered yet — Retry resends the same request` : ''}`, failed.length > 0);
    }).catch((e) => setError(errText(e)));
  };
  return html`<${Overlay} id="fleet-run-job" labelledby="fleet-run-job-title" onClose=${onClose}>
    <h3 id="fleet-run-job-title">Run a job in a peer's repository</h3>
    <label class="fa-jb-row"><span class="fa-k">where</span><select id="fleet-job-target" value=${form.target} onChange=${set('target')}>
      <option value="pick">nodes I pick</option><option value="auto">automatic: any trusted node</option>
      ${pools.map((p) => html`<option key=${p} value=${`group:${p}`}>automatic: a node of pool ${p}</option>`)}</select></label>
    ${form.target === 'pick'
      ? html`<div class="fa-jb-nodes">${peers.map((p) => html`<label key=${p.id} class=${p.online ? '' : 'muted'}><input type="checkbox" data-node=${p.id} checked=${form.nodes.includes(p.id)} onChange=${toggle(p.id)} /> ${p.label}${p.online ? '' : ' (offline)'}</label>`)}</div>`
      : html`<label class="fa-jb-row"><span class="fa-k">prefer</span><select id="fleet-job-prefer" value=${form.prefer} onChange=${set('prefer')}><option value="least-loaded">least loaded</option><option value="most-free-ram">most free RAM</option></select></label>`}
    <label class="fa-jb-row"><span class="fa-k">repo</span><input id="fleet-job-repo" value=${form.repo} placeholder="the peer's allowed repository alias" autocomplete="off" spellcheck="false" onInput=${set('repo')} /></label>
    <label class="fa-jb-row"><span class="fa-k">ref</span><input id="fleet-job-ref" value=${form.ref} placeholder="branch, full ref or exact commit" autocomplete="off" spellcheck="false" onInput=${set('ref')} /></label>
    <label class="fa-jb-row"><span class="fa-k">group</span><input id="fleet-job-group" value=${form.group} placeholder="receiving group on the peer" autocomplete="off" onInput=${set('group')} /></label>
    <label class="fa-jb-row"><span class="fa-k">harness</span><select id="fleet-job-harness" value=${form.harness} onChange=${set('harness')}>
      <option value="">shell command</option>${['claude', 'codex', 'opencode', 'copilot', 'gemini'].map((x) => html`<option key=${x} value=${x}>${x} task</option>`)}</select></label>
    <label class="fa-jb-row"><span class="fa-k">command</span><textarea id="fleet-job-command" rows="3" spellcheck="false" placeholder=${form.harness ? 'the task for the agent' : 'shell command'} onInput=${set('command')}>${form.command}</textarea></label>
    <label class="fa-jb-row"><span class="fa-k">timeout</span><input id="fleet-job-timeout" value=${form.timeout} inputmode="numeric" onInput=${set('timeout')} /></label>
    <label class="fa-jb-row"><span class="fa-k">require</span><input id="fleet-job-require" value=${form.require} placeholder="os=darwin,harness=codex,label=gpu (optional)" autocomplete="off" spellcheck="false" onInput=${set('require')} /></label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation job run --node … --repo … --ref … --group … --command … [--follow]</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button id="fleet-job-send" type="button" class="primary" onClick=${submit}>Send…</button>
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

// appendOutput adds live chunks to the per-stream text, keeping the newest
// OUTPUT_KEEP characters of each.
export function appendOutput(out, chunks) {
  const next = { ...out };
  for (const c of chunks || []) {
    const k = c?.stream === 'stderr' ? 'stderr' : 'stdout';
    let text = next[k] + String(c?.data ?? '');
    if (text.length > OUTPUT_KEEP) { text = text.slice(-OUTPUT_KEEP); next.trimmed = true; }
    next[k] = text;
  }
  return next;
}

// canFollow is a job whose output can be read live: one this node sent that
// has not finished.
export function canFollow(j) {
  return !j.incoming && !j.terminal && j.state !== 'unknown';
}

// OutputDialog shows a job's output: live while it runs (polled only while
// the dialog is open, Fleet is shown and the browser tab is visible), then the
// stored logs once it ends.
function OutputDialog({ job, label, actions, timers, active, onClose }) {
  const [live, setLive] = useState(!job.terminal);
  const [out, setOut] = useState({ stdout: '', stderr: '', trimmed: false, skipped: false });
  const [note, setNote] = useState('');
  const [failed, setFailed] = useState('');
  const [logs, setLogs] = useState(null);
  const [visible, setVisible] = useState(() => !globalThis.document?.hidden);
  const [retry, setRetry] = useState(0);
  const cursor = useRef('');
  const pre = useRef(null);
  useEffect(() => {
    const doc = globalThis.document;
    if (!doc?.addEventListener) return undefined;
    const onVis = () => setVisible(!doc.hidden);
    doc.addEventListener('visibilitychange', onVis);
    return () => doc.removeEventListener('visibilitychange', onVis);
  }, []);
  useEffect(() => {
    if (live) return undefined;
    actions.jobLogs(job.id).then(setLogs).catch((e) => setLogs({ error: errText(e) }));
    return undefined;
  }, [live]);
  useEffect(() => {
    if (!live || !active || !visible || failed) return undefined;
    let off = false; let t = null;
    const loop = () => {
      actions.jobOutput(job.id, cursor.current).then((r) => {
        if (off) return;
        setNote('');
        cursor.current = r?.cursor ?? cursor.current;
        if ((r?.chunks || []).length || r?.truncated) setOut((o) => ({ ...appendOutput(o, r.chunks), skipped: o.skipped || !!r.truncated }));
        if (r?.done) { setLive(false); return; }
        t = timers.setTimeout(loop, FOLLOW_MS);
      }).catch((e) => {
        if (off) return;
        // 409: not running yet (or no longer followable); keep checking.
        if (e?.status === 409) { setNote(`Waiting for output: ${errText(e)}`); t = timers.setTimeout(loop, FOLLOW_MS); return; }
        setFailed(errText(e));
      });
    };
    loop();
    return () => { off = true; timers.clearTimeout(t); };
  }, [live, active, visible, failed, retry]);
  useEffect(() => {
    const el = pre.current;
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 40) el.scrollTop = el.scrollHeight;
  }, [out]);
  const shown = live ? out : logs;
  return html`<${Overlay} id="fleet-job-logs" labelledby="fleet-job-logs-title" onClose=${onClose}>
    <h3 id="fleet-job-logs-title">Job ${job.id} ${job.incoming ? 'from' : 'on'} ${label(job.peer)}</h3>
    <div class="muted">${job.repo}@${job.ref}${job.commit ? ` (${job.commit.slice(0, 12)})` : ''} · ${live ? html`<span class="fa-jb-live">● live</span>${!visible || !active ? ' (paused while hidden)' : ''}` : `${job.terminal ? job.state : 'finished'}${job.exit != null ? ` · exit ${job.exit}` : ''}${job.code ? ` · ${job.code}` : ''}`}</div>
    ${live && (out.trimmed || out.skipped) && html`<div class="muted">${out.skipped ? 'Some earlier output was skipped by the node. ' : ''}${out.trimmed ? 'Only the most recent output is kept here; the full output is in the logs once the job ends.' : ''}</div>`}
    ${live && note && html`<div class="muted" id="fleet-job-wait">${note}</div>`}
    ${live && failed && html`<div class="fa-danger" role="alert">Live output stopped: ${failed} <button type="button" class="fa-link" onClick=${() => { setFailed(''); setRetry((n) => n + 1); }}>retry</button></div>`}
    ${shown == null ? html`<div class="empty">Loading…</div>` : shown.error ? html`<div class="fa-danger" role="alert">${shown.error}</div>` : html`
      <div class="fa-k">stdout</div><pre class="fa-run-out" ref=${pre}>${shown.stdout || ''}</pre>
      ${shown.stderr ? html`<div class="fa-k">stderr</div><pre class="fa-run-out err">${shown.stderr}</pre>` : ''}`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation job status ${job.id}</code>${live ? html`; live output: <code>job run … --follow</code>` : ''}</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

function RepoDialog({ repo, groups, actions, confirm, onClose, onDone }) {
  const [form, setForm] = useState({ name: repo?.name || '', url: repo?.definition?.url || '', clone: repo?.definition?.clone || '', groups: repo?.group_names || [] });
  const [error, setError] = useState('');
  const set = (k) => (e) => setForm({ ...form, [k]: e.currentTarget.value });
  const toggle = (g) => (e) => setForm({ ...form, groups: e.currentTarget.checked ? [...form.groups, g] : form.groups.filter((x) => x !== g) });
  const save = () => {
    if (!REPO_RE.test(form.name.trim())) { setError('The alias is 1-64 letters, digits, . _ - and starts with a letter or digit.'); return; }
    if (!form.url.trim() || !form.clone.trim().startsWith('/')) { setError('Give the remote URL and the absolute path of the local clone.'); return; }
    if (!form.groups.length) { setError('Pick at least one receiving group.'); return; }
    setError('');
    const body = { name: form.name.trim(), url: form.url.trim(), clone: form.clone.trim(), groups: form.groups };
    if (repo) body.revision = repo.revision;
    confirm({
      title: repo ? `${repo.enabled ? 'Save' : 'Re-enable'} repository ${body.name}?` : `Allow repository ${body.name} for remote jobs?`,
      body: `Peers granted jobs.run in ${body.groups.join(', ')} can ask to run commands in a checkout of ${body.clone} (${body.url}) on this node, at any ref they name. Jobs run as soon as they arrive, as the user tclaude runs as — unless the peer's jobs.run grant sets manual job approval (job_approval=manual), then each waits for you under Jobs.${repo ? ' Jobs from it still waiting for approval can no longer run (approving one fails it); peers send them again.' : ''}`,
      okLabel: repo ? 'Save' : 'Allow',
      busyLabel: 'Saving…',
      action: () => (repo ? actions.updateRepo(repo.name, body) : actions.addRepo(body)),
    }).then((r) => { if (r) onDone(`Repository ${body.name} saved`); })
      .catch((e) => setError(errText(e)));
  };
  return html`<${Overlay} id="fleet-repo" labelledby="fleet-repo-title" onClose=${onClose}>
    <h3 id="fleet-repo-title">${repo ? `${repo.enabled ? 'Edit' : 'Re-enable'} repository ${repo.name}` : 'Allow a repository for remote jobs'}</h3>
    <label class="fa-jb-row"><span class="fa-k">alias</span><input id="fleet-repo-name" value=${form.name} disabled=${!!repo} placeholder="name peers use (--repo)" autocomplete="off" spellcheck="false" onInput=${set('name')} /></label>
    <label class="fa-jb-row"><span class="fa-k">URL</span><input id="fleet-repo-url" value=${form.url} placeholder="git remote, e.g. git@github.com:org/repo.git" autocomplete="off" spellcheck="false" onInput=${set('url')} /></label>
    <label class="fa-jb-row"><span class="fa-k">clone</span><input id="fleet-repo-clone" value=${form.clone} placeholder="/abs/path of the local clone on this node" autocomplete="off" spellcheck="false" onInput=${set('clone')} /></label>
    <div class="fa-jb-row"><span class="fa-k">groups</span><div class="fa-jb-nodes">${groups.map((g) => html`<label key=${g}><input type="checkbox" data-group=${g} checked=${form.groups.includes(g)} onChange=${toggle(g)} /> ${g}</label>`)}</div></div>
    ${repo && !(repo.group_names || []).length && (repo.definition?.groups || []).length > 0 && html`<div class="muted">Currently ${groupText(repo)}; tick the groups to keep.</div>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation repos ${repo ? 'update' : 'add'} …</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button id="fleet-repo-save" type="button" class="primary" onClick=${save}>${repo ? 'Save…' : 'Allow…'}</button>
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

// JobsPage lists remote jobs (sent by this node and received from peers) with
// approve / cancel / retry / acknowledge-stopped and their logs, can send new
// ones, and manages the repositories peers may run jobs in here.
export function JobsPage({ view, pools, groups, actions, confirm, toast, timers, active }) {
  const [jobs, setJobs] = useState(null);
  const [repos, setRepos] = useState(null);
  const [error, setError] = useState('');
  const [dialog, setDialog] = useState(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!active) return undefined;
    let off = false; let t = null;
    const loop = () => {
      actions.jobs().then((r) => { if (!off) { setJobs(r.map(jobView)); setError(''); } })
        .catch((e) => { if (!off) setError(errText(e)); })
        .finally(() => { if (!off) t = timers.setTimeout(loop, POLL_MS); });
    };
    loop();
    return () => { off = true; timers.clearTimeout(t); };
  }, [tick, active]);
  useEffect(() => {
    actions.repos().then(setRepos).catch((e) => setRepos({ error: errText(e) }));
  }, [tick]);
  const label = (id) => view.trusted.find((r) => r.id === id)?.label || shortID(id);
  const reload = () => setTick((n) => n + 1);
  const done = (msg, bad = false) => { setDialog(null); toast(msg, bad); reload(); };
  const act = (opts, okMsg) => confirm(opts).then((r) => { if (r) { toast(okMsg, false); reload(); } }).catch((e) => toast(`${opts.okLabel} failed: ${errText(e)}`, true));
  const what = (j) => `${j.harness ? `${j.harness} task` : 'shell command'} in ${j.repo}@${j.ref} (group ${j.group})`;

  const approve = (j) => act({
    title: `Run ${label(j.peer)}'s job here?`,
    body: `A one-shot worker runs this ${what(j)} on this node, as the user tclaude runs as, for up to ${j.timeout} s: it can read and change that checkout and use this node's logins and network. Output goes back to ${label(j.peer)}. Command: ${j.command}`,
    okLabel: 'Run job', busyLabel: 'Starting…',
    action: () => actions.approveJob(j.id),
  }, 'Job approved');
  const cancel = (j) => act({
    title: j.incoming ? `Stop ${label(j.peer)}'s job?` : `Cancel the job on ${label(j.peer)}?`,
    body: j.incoming
      ? (j.state === 'pending' ? `The job is refused without running and ${label(j.peer)} is told it was canceled.` : `The worker is stopped mid-run (exit 130); changes it already made to the checkout stay. ${label(j.peer)} is told it was canceled.`)
      : `${label(j.peer)} is asked to stop it. If it is already running there, whatever it changed so far stays; it may also finish before the request arrives.`,
    okLabel: 'Cancel job', busyLabel: 'Canceling…',
    action: () => actions.cancelJob(j.id),
  }, 'Cancel requested');
  const retry = (j) => act({
    title: `Resend the job to ${label(j.peer)}?`,
    body: `Resends the same request (same job ID) — safe if ${label(j.peer)} already received it: it will not run twice. Use this when delivery was uncertain.`,
    okLabel: 'Resend', busyLabel: 'Resending…',
    action: () => actions.retryJob(j.id),
  }, 'Job resent');
  const ack = (j) => act({
    title: 'Acknowledge the job has stopped?',
    body: `This node lost track of job ${j.id} (from ${label(j.peer)}) while it ran. Only confirm after checking that its worker and anything it started have stopped: the job is marked interrupted, its worker retired and its slot freed for new jobs, and ${label(j.peer)} is told.`,
    okLabel: 'It has stopped', busyLabel: 'Saving…',
    action: () => actions.acknowledgeJobStopped(j.id),
  }, 'Job marked interrupted');
  const disableRepo = (r) => act({
    title: `Disable repository ${r.name}?`,
    body: `Peers can no longer run jobs in ${r.definition?.clone || r.name}. Jobs from it still waiting for approval can no longer run: approving one fails it. A job already running finishes. Re-enable… allows it again.`,
    okLabel: 'Disable', busyLabel: 'Disabling…',
    action: () => actions.disableRepo(r.name),
  }, `Repository ${r.name} disabled`);

  const list = jobs || [];
  const pending = list.filter((j) => j.incoming && j.state === 'pending' && !expired(j)).length;
  const repoRows = Array.isArray(repos) ? repos : [];
  return html`<div class="fa-jobs">
    <div class="fa-grant-form">
      <button id="fleet-job-open" type="button" disabled=${!view.trusted.length} onClick=${() => setDialog({ kind: 'run' })}>Run a job…</button>
      <button id="fleet-jobs-refresh" type="button" onClick=${reload}>Refresh</button>
      <span class="muted">${pending ? html`<span class="fa-warn">${pending} waiting for your approval. </span>` : ''}CLI: <code>tclaude federation job ls</code></span>
    </div>
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    ${jobs == null ? (error ? '' : html`<div class="empty">Loading…</div>`) : !list.length ? html`<div class="empty">No remote jobs.</div>` : html`
    <table class="fa-table" id="fleet-jobs">
      <thead><tr><th></th><th>Peer</th><th>Repo</th><th>Group</th><th>Command</th><th>Created</th><th>State</th><th></th></tr></thead>
      <tbody>${list.map((j) => html`<tr key=${j.id} data-job=${j.id}>
        <td title=${j.incoming ? 'received: runs on this node' : 'sent: runs on the peer'}>${j.incoming ? '⇠' : '⇢'}</td>
        <td>${label(j.peer)}${j.caller ? html` <span class="muted">(${j.caller})</span>` : ''}</td>
        <td><code>${j.repo}@${j.ref}</code></td>
        <td>${j.group}</td>
        <td class="fa-wrap" title=${j.command}><code>${j.command.length > 80 ? `${j.command.slice(0, 80)}…` : j.command}</code>${j.harness ? html` <span class="muted">(${j.harness})</span>` : ''}</td>
        <td class="fa-nowrap">${when(j.created)}</td>
        <td class=${BAD.has(j.state) ? 'fa-danger' : j.state === 'pending' && j.incoming ? 'fa-warn' : ''}>${j.state}${j.exit != null && j.terminal ? ` (${j.exit})` : ''}${j.code ? html` <span class="muted">${j.code}</span>` : ''}</td>
        <td class="fa-acts">
          ${j.incoming && j.state === 'pending' && !expired(j) && html`<button type="button" data-fa="approve" onClick=${() => approve(j)}>Approve…</button>`}
          ${!j.terminal && j.state !== 'unknown' && html`<button type="button" data-fa="cancel" onClick=${() => cancel(j)}>Cancel…</button>`}
          ${!j.incoming && j.state === 'submitted' && html`<button type="button" data-fa="retry" onClick=${() => retry(j)}>Resend…</button>`}
          ${j.incoming && j.state === 'unknown' && html`<button type="button" data-fa="ack" onClick=${() => ack(j)}>Acknowledge stopped…</button>`}
          ${j.terminal && j.hasLogs && html`<button type="button" data-fa="logs" onClick=${() => setDialog({ kind: 'logs', job: j })}>Output</button>`}
          ${canFollow(j) && html`<button type="button" data-fa="follow" onClick=${() => setDialog({ kind: 'logs', job: j })}>Live output</button>`}
        </td>
      </tr>`)}</tbody>
    </table>`}
    <h4>Repositories peers may run jobs in here</h4>
    <div class="fa-grant-form"><button id="fleet-repo-add" type="button" onClick=${() => setDialog({ kind: 'repo' })}>Allow a repository…</button>
      <span class="muted">CLI: <code>tclaude federation repos ls</code></span></div>
    ${repos?.error ? html`<div class="fa-danger">${repos.error}</div>` : repos == null ? '' : !repoRows.length ? html`<div class="empty">No repositories allowed: peers cannot run jobs here.</div>` : html`
    <table class="fa-table" id="fleet-repos">
      <thead><tr><th>Alias</th><th>Clone</th><th>URL</th><th>Groups</th><th>Rev</th><th></th></tr></thead>
      <tbody>${repoRows.map((r) => html`<tr key=${r.name} data-repo=${r.name} class=${r.enabled ? '' : 'muted'}>
        <td>${r.name}</td><td class="fa-wrap"><code>${r.definition?.clone || ''}</code></td><td class="fa-wrap"><code>${r.definition?.url || ''}</code></td>
        <td>${groupText(r)}</td><td>${r.revision}</td>
        <td class="fa-acts">${r.enabled ? html`<button type="button" data-fa="edit" onClick=${() => setDialog({ kind: 'repo', repo: r })}>Edit…</button><button type="button" data-fa="disable" onClick=${() => disableRepo(r)}>Disable…</button>` : html`<span class="muted">disabled </span><button type="button" data-fa="enable" onClick=${() => setDialog({ kind: 'repo', repo: r })}>Re-enable…</button>`}</td>
      </tr>`)}</tbody>
    </table>`}
    ${dialog?.kind === 'run' && html`<${RunJobDialog} peers=${view.trusted} pools=${pools} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${dialog?.kind === 'logs' && html`<${OutputDialog} job=${dialog.job} label=${label} actions=${actions} timers=${timers} active=${active} onClose=${() => setDialog(null)} />`}
    ${dialog?.kind === 'repo' && html`<${RepoDialog} repo=${dialog.repo} groups=${groups} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
  </div>`;
}
