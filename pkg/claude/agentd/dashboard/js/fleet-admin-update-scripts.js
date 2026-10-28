import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { accessHint } from './fleet-harness-model.js';

const html = htm.bind(h);

// SCRIPT_POLL_MS re-reads a node while one of its status checks or updates
// is still running.
const SCRIPT_POLL_MS = 2000;

function when(iso) {
  const t = new Date(iso);
  return Number.isFinite(t.getTime()) ? t.toLocaleString() : '';
}

// overviewBusy says whether a node's update-scripts overview is still moving.
export function overviewBusy(o) {
  return (o?.scripts || []).some((s) => s.state === 'checking') || (o?.jobs || []).some((j) => j.state === 'running');
}

// scriptView summarises one update script's last status.
export function scriptView(s) {
  if (s.state === 'checking') return { cls: 'muted', text: 'checking…' };
  if (s.state === 'failed') return { cls: 'fa-danger', text: s.error || 'status check failed' };
  if (!s.has_status) return { cls: 'muted', text: 'no status script' };
  if (s.state === 'unchecked') return { cls: 'muted', text: 'not checked' };
  const pending = (s.items || []).filter((i) => i.update_available === true).length;
  const summary = s.summary || (s.update_available === true ? (pending ? `${pending} pending` : 'updates pending') : s.update_available === false ? 'up to date' : 'checked');
  return { cls: s.update_available === true ? 'fa-warn' : '', text: summary, pending: s.update_available === true };
}

// ScriptRow is one script's row; the first row of a node carries the node
// cell (head), which spans the node's rows.
function ScriptRow({ head, node, script, job, onCheck, onUpdate }) {
  const v = scriptView(script);
  const running = job?.state === 'running';
  const items = script.items || [];
  const more = items.length > 0 || script.details || (job && job.output);
  return html`<tr data-node=${head ? node.id : undefined} data-script=${script.name}>
    ${head}
    <td>${script.label}${script.label !== script.name ? html` <span class="muted">${script.name}</span>` : ''}</td>
    <td>
      <span class=${v.cls}>${v.pending ? html`<span class="fa-up" aria-label="update available">↑</span> ` : ''}${v.text}</span>
      ${script.checked_at && html` <span class="muted">· ${when(script.checked_at)}</span>`}
      ${job && html`<div class=${job.state === 'failed' ? 'fa-danger' : 'muted'}>update ${job.state}${job.error ? ` — ${job.error}` : ''}${job.finished_at ? ` · ${when(job.finished_at)}` : ''}</div>`}
      ${more && html`<details><summary class="muted">details</summary>
        ${items.length > 0 && html`<table class="fa-table"><thead><tr><th>Item</th><th>Current</th><th>Latest</th><th></th></tr></thead>
          <tbody>${items.map((it, i) => html`<tr key=${i}><td>${it.name}</td><td>${it.current || ''}</td><td>${it.latest || ''}</td>
            <td>${it.update_available === true ? html`<span class="fa-warn">pending</span>` : it.update_available === false ? html`<span class="muted">up to date</span>` : ''}${it.detail ? html` <span class="muted">${it.detail}</span>` : ''}</td></tr>`)}</tbody></table>`}
        ${script.details && html`<pre class="fa-script-out">${script.details}</pre>`}
        ${job?.output && html`<div class="muted">Last update output</div><pre class="fa-script-out">${job.output}</pre>`}
      </details>`}
    </td>
    <td class="fa-acts">
      ${script.has_status && html`<button type="button" data-fa="script-check" disabled=${script.state === 'checking'} onClick=${() => onCheck(node, script)}>Check</button>`}
      ${script.has_update && html`<button type="button" data-fa="script-update" class=${v.pending ? 'primary' : ''} disabled=${running} onClick=${() => onUpdate(node, script)}>${running ? 'Updating…' : 'Update…'}</button>`}
    </td>
  </tr>`;
}

// UpdateScriptsSection lists each node's own update scripts — the status and
// update commands its operator configured under updates.scripts — with what
// they report as pending, and runs them. Peers show when they share
// node.update.
export function UpdateScriptsSection({ nodes, actions, confirm, toast, timers = globalThis }) {
  const [data, setData] = useState({});
  const dataRef = useRef(data);
  dataRef.current = data;
  const nodesKey = nodes.map((n) => n.id).join(',');

  const load = (node) => actions.updateScripts(node)
    .then((o) => setData((cur) => ({ ...cur, [node.id]: { data: o } })))
    .catch((e) => setData((cur) => ({ ...cur, [node.id]: { error: e } })));
  useEffect(() => { for (const node of nodes) load(node); }, [nodesKey]);

  const busyKey = nodes.filter((n) => overviewBusy(data[n.id]?.data)).map((n) => n.id).join(',');
  useEffect(() => {
    if (!busyKey) return undefined;
    const t = timers.setTimeout(() => { for (const node of nodes) if (overviewBusy(dataRef.current[node.id]?.data)) load(node); }, SCRIPT_POLL_MS);
    return () => timers.clearTimeout(t);
  }, [busyKey, data]);

  const onCheck = (node, script) => actions.checkUpdateScripts(node, script?.name || '')
    .then((o) => setData((cur) => ({ ...cur, [node.id]: { data: o } })))
    .catch((e) => toast(`Check failed on ${node.label}: ${accessHint(e, 'node.update')}`, true));
  const onUpdate = (node, script) => confirm({
    title: `Run ${script.label} update on ${node.label}?`,
    body: `Runs ${node.label}'s own update command for ${script.label} as its daemon user${script.update_command ? `: ${script.update_command}` : ''}. Its status is checked again when it finishes.`,
    okLabel: 'Update', busyLabel: 'Starting…',
    action: () => actions.runUpdateScript(node, script.name),
  }).then((job) => { if (job?.id) { toast(`Updating ${script.label} on ${node.label}…`, false); load(node); } })
    .catch((e) => toast(e?.code === 'update_busy' ? `${node.label} is already running an update script` : `Update failed on ${node.label}: ${accessHint(e, 'node.update')}`, true));

  return html`<div id="fleet-update-scripts">
    <h4>Custom updates</h4>
    <div class="muted">Each node's own status and update scripts, from <code>updates.scripts</code> in its config. A peer shows here when it shares node.update.</div>
    <table class="fa-table">
      <thead><tr><th>Node</th><th>Target</th><th>Status</th><th></th></tr></thead>
      <tbody>${nodes.map((node) => {
        const d = data[node.id] || {};
        const scripts = d.data?.scripts || [];
        const latest = new Map();
        for (const j of d.data?.jobs || []) if (!latest.has(j.script)) latest.set(j.script, j);
        const head = html`<td rowspan=${Math.max(scripts.length, 1)}>${node.local ? '⌂ ' : ''}<b>${node.label}</b>
          ${scripts.some((s) => s.has_status) && html` <button type="button" class="fa-link" data-fa="script-check-all" onClick=${() => onCheck(node, null)}>check all</button>`}</td>`;
        if (d.error) return html`<tr key=${node.id} data-node=${node.id}>${head}<td colspan="3" class="muted">${accessHint(d.error, 'node.update')}${d.error?.status === 404 ? ' (no update scripts on this version)' : ''}</td></tr>`;
        if (!d.data) return html`<tr key=${node.id} data-node=${node.id}>${head}<td colspan="3" class="muted">loading…</td></tr>`;
        if (d.data.error) return html`<tr key=${node.id} data-node=${node.id}>${head}<td colspan="3" class="fa-danger">${d.data.error}</td></tr>`;
        if (!scripts.length) return html`<tr key=${node.id} data-node=${node.id}>${head}<td colspan="3" class="muted">no update scripts configured</td></tr>`;
        return scripts.map((s, i) => html`<${ScriptRow} key=${`${node.id}/${s.name}`} head=${i === 0 ? head : null} node=${node} script=${s} job=${latest.get(s.name)} onCheck=${onCheck} onUpdate=${onUpdate} />`);
      })}</tbody>
    </table>
  </div>`;
}
