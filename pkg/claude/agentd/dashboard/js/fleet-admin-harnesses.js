import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { CREDENTIAL_FILE_HARNESSES, SHARE_WARNING, accessHint, cellView, harnessColumns, jobActive } from './fleet-harness-model.js';

const html = htm.bind(h);

const JOB_POLL_MS = 1000;

function errText(error) { return error?.message || String(error); }

function when(iso) {
  const t = new Date(iso);
  return Number.isFinite(t.getTime()) ? t.toLocaleString() : '—';
}

// HarnessDialog manages one harness on one node: install or update it (with
// the busy-workers choice the daemon asks for), copy this operator's login
// along with a remote install, and the node's credential backups — back up,
// restore, and for a peer push this operator's login.
export function HarnessDialog({ node, row, actions, confirm, toast, onJob, onClose, onChanged }) {
  const name = row?.name;
  const label = row?.display_name || name;
  const fileLogin = CREDENTIAL_FILE_HARNESSES.includes(name);
  const action = row?.installed ? 'update' : 'install';
  const [ops, setOps] = useState(null);
  const [opsError, setOpsError] = useState(null);
  const [backups, setBackups] = useState(null);
  const [credError, setCredError] = useState(null);
  const [copy, setCopy] = useState(false);
  const [overwrite, setOverwrite] = useState(false);
  const [busyChoice, setBusyChoice] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let off = false;
    actions.operations(node).then((o) => { if (!off) setOps(o); }).catch((e) => { if (!off) setOpsError(e); });
    return () => { off = true; };
  }, []);
  useEffect(() => {
    if (!fileLogin) return undefined;
    let off = false;
    actions.backups(node, name).then((b) => { if (!off) { setBackups(b); setCredError(null); } }).catch((e) => { if (!off) setCredError(e); });
    return () => { off = true; };
  }, [tick]);

  const recipe = (ops?.recipes || []).find((r) => r.harness === name);
  const command = recipe ? (action === 'install' ? recipe.install_command : recipe.update_command) : '';
  const copyable = !node.local && action === 'install' && fileLogin;

  const start = async (mode) => {
    if (busy) return;
    setBusy(true); setError('');
    const req = { action, harness: name };
    if (mode) req.mode = mode;
    if (copyable && copy) { req.copy_credentials = true; if (overwrite) req.overwrite_credentials = true; }
    try {
      const job = await actions.start(node, req);
      onJob(node, job);
      toast(`${action === 'install' ? 'Installing' : 'Updating'} ${label} on ${node.label}…`, false);
      onClose();
    } catch (e) {
      if (e?.code === 'harness_workers_busy') setBusyChoice(true);
      else setError(errText(e));
    } finally { setBusy(false); }
  };
  const run = () => {
    const copying = copyable && copy;
    confirm({
      title: `${action === 'install' ? 'Install' : 'Update'} ${label} on ${node.label}?`,
      body: `Runs ${command || `the ${label} ${action}`} on ${node.label} as its daemon user.`
        + (copying ? ` Your ${label} login files are copied there too. ${SHARE_WARNING}${overwrite ? ' An existing login there is backed up, then replaced.' : ' An existing login there is kept.'}` : ''),
      okLabel: action === 'install' ? 'Install' : 'Update',
    }).then((ok) => { if (ok) start(''); });
  };
  const backupNow = () => actions.backup(node, name)
    .then((r) => { toast(`Backed up ${label} login on ${node.label}`, false); setTick((n) => n + 1); onChanged?.(r?.availability); })
    .catch((e) => toast(`Backup failed: ${errText(e)}`, true));
  const restore = (b) => confirm({
    title: `Restore ${label} login on ${node.label}?`,
    body: `Replaces the current ${label} login files on ${node.label} with the backup from ${when(b.created_at)}. The current files are backed up first, so this can be undone.`,
    okLabel: 'Restore', busyLabel: 'Restoring…',
    action: () => actions.restore(node, name, b.id),
  }).then((r) => { if (r) { toast(`Restored ${label} login on ${node.label}`, false); setTick((n) => n + 1); onChanged?.(r?.availability); } })
    .catch((e) => toast(`Restore failed: ${errText(e)}`, true));
  const push = () => confirm({
    title: `Push your ${label} login to ${node.label}?`,
    body: `${SHARE_WARNING} Your ${label} login files are copied to ${node.label}; its current ${label} login is backed up first, then replaced.`,
    okLabel: 'Push my login', busyLabel: 'Pushing…',
    action: () => actions.push(node, name),
  }).then((r) => { if (r) { toast(`Pushed your ${label} login to ${node.label}`, false); setTick((n) => n + 1); onChanged?.(r?.availability); } })
    .catch((e) => toast(`Push failed: ${errText(e)}`, true));

  const cell = cellView(row);
  return html`<${Overlay} id="fleet-harness-modal" labelledby="fleet-harness-title" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-harness-title">${label} on ${node.label}</h3>
    <div class="fa-dl">
      <span class="fa-k">Installed</span><span>${row?.installed ? `${row.version || 'version unknown'}${row.path ? ` · ${row.path}` : ''}` : 'no'}</span>
      ${row?.latest_version && html`<span class="fa-k">Latest</span><span>${row.latest_version}${row.update_available === true ? ' — update available' : ''}</span>`}
      <span class="fa-k">State</span><span>${cell.title}</span>
    </div>
    <h4>${action === 'install' ? 'Install' : 'Update'}</h4>
    ${opsError
      ? html`<div class="muted">${node.local ? errText(opsError) : `Managing harnesses on ${node.label}: ${accessHint(opsError, 'node.harnesses.install')}.`}</div>`
      : html`
        ${command && html`<div class="muted">Runs <code>${command}</code></div>`}
        ${copyable && html`<label class="fa-ack"><input id="fleet-harness-copy" type="checkbox" checked=${copy} onChange=${(e) => setCopy(e.currentTarget.checked)} />
          <span>Copy my ${label} login there too (needs node.credentials.receive)</span></label>
          ${copy && html`<div class="fa-consequence" role="note">${SHARE_WARNING}</div>
            <label class="fa-ack"><input id="fleet-harness-overwrite" type="checkbox" checked=${overwrite} onChange=${(e) => setOverwrite(e.currentTarget.checked)} />
              <span>Replace an existing login there (it is backed up first)</span></label>`}`}
        ${busyChoice
          ? html`<div class="fa-consequence" role="alert">Agents using ${label} on ${node.label} are busy.
              <div class="modal-buttons"><button id="fleet-harness-now" type="button" class="danger" disabled=${busy} onClick=${() => start('now')}>Run now (may interrupt them)</button>
              <button id="fleet-harness-idle" type="button" disabled=${busy} onClick=${() => start('when_idle')}>When they are idle</button></div></div>`
          : html`<button id="fleet-harness-run" type="button" class="primary" disabled=${busy || !ops} onClick=${run}>${action === 'install' ? 'Install…' : 'Update…'}</button>`}`}
    ${fileLogin
      ? html`<h4>Login files</h4>
        ${credError
          ? html`<div class="muted">${node.local ? errText(credError) : `${node.label}: ${accessHint(credError, 'node.credentials.receive')}.`}</div>`
          : html`<div class="fa-grant-form">
              ${!node.local && html`<button id="fleet-harness-push" type="button" class="danger" onClick=${push}>Push my login…</button>`}
              <button id="fleet-harness-backup" type="button" onClick=${backupNow}>Back up now</button>
            </div>
            ${backups && (backups.length
              ? html`<table class="fa-table" id="fleet-harness-backups"><thead><tr><th>Backup</th><th>Taken</th><th></th></tr></thead>
                <tbody>${backups.map((b) => html`<tr key=${b.id}><td><code>${b.id.slice(0, 12)}</code></td><td>${when(b.created_at)}</td>
                  <td class="fa-acts"><button type="button" data-fa="restore" onClick=${() => restore(b)}>Restore…</button></td></tr>`)}</tbody></table>`
              : html`<div class="muted">No backups yet.</div>`)}`}`
      : html`<div class="muted">${label} keeps its login outside copyable files (keychain or environment): log in on ${node.label} itself.</div>`}
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" disabled=${busy} onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

function JobRow({ entry, copy, toast, onDismiss }) {
  const { node, job } = entry;
  const last = (job.log || []).at(-1);
  return html`<tr data-job=${job.id}>
    <td>${node.label}</td>
    <td>${job.action} ${(job.harnesses || []).join(', ')}${job.mode ? html` <span class="muted">(${job.mode === 'when_idle' ? 'when idle' : 'now'})</span>` : ''}</td>
    <td class=${job.state === 'failed' ? 'fa-danger' : ''}>${job.state === 'waiting_idle' ? 'waiting for idle agents' : job.state}${job.phase ? html` <span class="muted">· ${job.phase}</span>` : ''}</td>
    <td>${(job.results || []).map((r) => html`<div key=${r.harness}>${r.harness}: <span class=${r.state === 'failed' ? 'fa-danger' : r.state === 'manual_required' ? 'fa-warn' : ''}>${r.state}</span>${r.error ? html` <span class="muted">${r.error}</span>` : ''}
        ${r.manual_command && html` <code>${r.manual_command}</code> <button type="button" class="fa-link" onClick=${() => copy(r.manual_command).then(() => toast('Command copied', false)).catch(() => toast(r.manual_command, false))}>copy</button>`}
        ${r.credentials && html` <span class="muted">login ${r.credentials.copied ? 'copied' : 'kept'}${r.credentials.backup_id ? `, backup ${r.credentials.backup_id.slice(0, 12)}` : ''}</span>`}</div>`)}
      ${!(job.results || []).length && last ? html`<span class="muted">${last.message}</span>` : ''}
      ${job.error && html`<div class="fa-danger">${job.error}</div>`}
      ${(job.warnings || []).map((w, i) => html`<div key=${i} class="fa-warn">${w}</div>`)}</td>
    <td class="fa-acts">${!jobActive(job) && html`<button type="button" class="fa-link" onClick=${onDismiss}>dismiss</button>`}</td>
  </tr>`;
}

// HarnessesPage is the fleet-wide harness matrix: one row per node (this node
// and every trusted peer that shares node.harnesses.read), one column per
// harness. A cell opens that harness on that node; a row updates them all.
export function HarnessesPage({ view, actions, confirm, toast, copy, timers = globalThis }) {
  const nodes = [{ id: view.self.id, label: view.self.name, local: true }, ...view.trusted.map((r) => ({ id: r.id, label: r.label, local: false, online: r.online }))];
  const [avail, setAvail] = useState({});
  const [jobs, setJobs] = useState({});
  const [open, setOpen] = useState(null);
  const jobsRef = useRef(jobs);
  jobsRef.current = jobs;
  const nodesKey = nodes.map((n) => n.id).join(',');

  const load = (refresh = false) => {
    for (const node of nodes) {
      setAvail((cur) => ({ ...cur, [node.id]: { ...cur[node.id], loading: true } }));
      actions.availability(node, { refresh })
        .then((a) => setAvail((cur) => ({ ...cur, [node.id]: { data: a } })))
        .catch((e) => setAvail((cur) => ({ ...cur, [node.id]: { error: e } })));
    }
  };
  useEffect(() => { load(false); }, [nodesKey]);

  // Poll active jobs about once a second; a finished job's availability
  // replaces that node's row.
  const activeKey = Object.values(jobs).filter((j) => jobActive(j.job)).map((j) => j.job.id).join(',');
  useEffect(() => {
    if (!activeKey) return undefined;
    let disposed = false; let timer = null;
    const tick = async () => {
      for (const entry of Object.values(jobsRef.current)) {
        if (!jobActive(entry.job)) continue;
        try {
          const job = await actions.job(entry.node, entry.job.id);
          if (disposed) return;
          setJobs((cur) => ({ ...cur, [job.id]: { ...entry, job } }));
          if (!jobActive(job) && job.availability) setAvail((cur) => ({ ...cur, [entry.node.id]: { data: job.availability } }));
        } catch (_) { /* a restarting or busy peer: try again next tick */ }
      }
      if (!disposed) timer = timers.setTimeout(tick, JOB_POLL_MS);
    };
    timer = timers.setTimeout(tick, JOB_POLL_MS);
    return () => { disposed = true; timers.clearTimeout(timer); };
  }, [activeKey]);

  const onJob = (node, job) => setJobs((cur) => ({ ...cur, [job.id]: { node, job } }));
  const updateAll = (node) => confirm({
    title: `Update every installed harness on ${node.label}?`,
    body: `Runs each installed harness's update on ${node.label}. If agents are busy, the update waits until they are idle.`,
    okLabel: 'Update all', busyLabel: 'Starting…',
    action: () => actions.start(node, { action: 'update', all: true, mode: 'when_idle' }),
  }).then((job) => { if (job?.id) onJob(node, job); }).catch((e) => toast(`Update failed on ${node.label}: ${accessHint(e, 'node.harnesses.install')}`, true));

  const columns = harnessColumns(Object.values(avail).map((a) => a.data).filter(Boolean));
  const jobList = Object.values(jobs).sort((a, b) => String(b.job.started_at).localeCompare(String(a.job.started_at)));
  return html`<div class="fa-harnesses">
    <div class="fa-grant-form">
      <button id="fleet-harness-refresh" type="button" onClick=${() => load(true)}>Re-check</button>
      <span class="muted">Versions are cached for a few minutes; Re-check probes again. A peer shows here when it shares node.harnesses.read. CLI: <code>tclaude harness ls|install|update|credentials</code>.</span>
    </div>
    <table class="fa-table" id="fleet-harnesses">
      <thead><tr><th>Node</th>${columns.map((c) => html`<th key=${c}>${c}</th>`)}<th></th></tr></thead>
      <tbody>${nodes.map((node) => {
        const a = avail[node.id] || {};
        const byName = new Map((a.data?.harnesses || []).map((x) => [x.name, x]));
        return html`<tr key=${node.id} data-node=${node.id}>
          <td>${node.local ? '⌂ ' : ''}<b>${node.label}</b></td>
          ${a.error
            ? html`<td colspan=${columns.length} class="muted">${accessHint(a.error)}${a.error?.status === 403 ? ' (needs node.harnesses.read)' : ''}</td>`
            : !a.data
              ? html`<td colspan=${columns.length} class="muted">loading…</td>`
              : columns.map((c) => {
                const row = byName.get(c);
                const v = cellView(row);
                return html`<td key=${c}>${row && v.state !== 'na'
                  ? html`<button type="button" class=${`fa-cell ${v.state}`} data-cell=${c} title=${v.title} onClick=${() => setOpen({ node, row })}>
                      ${v.text}${v.update ? html` <span class="fa-up" aria-label="update available">↑</span>` : ''}${v.credential ? html` <span aria-label="login present" title="login files present">🔑</span>` : ''}</button>`
                  : html`<span class="muted" title=${v.title}>${v.text}</span>`}</td>`;
              })}
          <td class="fa-acts">${a.data && (a.data.harnesses || []).some((x) => x.update_available === true) && html`<button type="button" data-fa="update-all" onClick=${() => updateAll(node)}>Update all…</button>`}</td>
        </tr>`;
      })}</tbody>
    </table>
    ${jobList.length > 0 && html`<h4>Jobs</h4><table class="fa-table" id="fleet-harness-jobs">
      <thead><tr><th>Node</th><th>Job</th><th>State</th><th>Result</th><th></th></tr></thead>
      <tbody>${jobList.map((entry) => html`<${JobRow} key=${entry.job.id} entry=${entry} copy=${copy} toast=${toast}
        onDismiss=${() => setJobs((cur) => { const next = { ...cur }; delete next[entry.job.id]; return next; })} />`)}</tbody>
    </table>`}
    ${open && html`<${HarnessDialog} node=${open.node} row=${open.row} actions=${actions} confirm=${confirm} toast=${toast} onJob=${onJob}
      onClose=${() => setOpen(null)} onChanged=${(availability) => { if (availability) setAvail((cur) => ({ ...cur, [open.node.id]: { data: availability } })); }} />`}
  </div>`;
}
