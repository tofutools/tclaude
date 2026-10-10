import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { viewersFocus } from './remote-viewers.js';

const html = htm.bind(h);

const POLL_MS = 5000;

function errText(error) { return error?.message || String(error); }

function since(t, now) {
  const s = Math.max(0, Math.round((now - Date.parse(t)) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${Math.round(s / 3600)}h`;
}

// ViewersPanel lists peers watching or driving this node's agent terminals
// right now (incoming remote terminal views) and lets the operator kick one.
// It renders nothing while nobody is watching.
export function ViewersPanel({ view, actions, confirm, toast, timers = globalThis, now = () => Date.now() }) {
  const [rows, setRows] = useState([]);
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let off = false; let t = null;
    const loop = () => {
      actions.viewers().then((r) => { if (!off) { setRows(Array.isArray(r) ? r : []); setError(''); } })
        .catch((e) => { if (!off) setError(errText(e)); })
        .finally(() => { if (!off) t = timers.setTimeout(loop, POLL_MS); });
    };
    loop();
    return () => { off = true; timers.clearTimeout(t); };
  }, [tick]);
  const label = (id) => view.trusted.find((r) => r.id === id)?.label || id;
  const unrestricted = (id) => view.trusted.find((r) => r.id === id)?.level === 'unrestricted';
  const kick = (v) => confirm({
    title: `Disconnect ${label(v.peer)} from ${v.session || v.agent}?`,
    body: `${label(v.peer)}'s ${v.read_only ? 'read-only view' : 'interactive session (it can type, including answering harness prompts)'} of ${v.session || v.agent} closes now. The agent keeps running. ${unrestricted(v.peer)
      ? `${label(v.peer)} is unrestricted, so it can open it again at once: restrict or untrust it to keep it out.`
      : `${label(v.peer)} can open it again while any sessions.watch (read-only) or sessions.attach (typing) grant covers ${v.group ? `group ${v.group}` : 'that agent\'s group'} — direct, all-groups or through a pool. Revoke those (or remove it from the pool) to keep it out.`}`,
    okLabel: 'Disconnect',
    busyLabel: 'Disconnecting…',
    action: () => actions.kickViewer(v.id),
  }).then((r) => { if (r) { toast(`Disconnected ${label(v.peer)}`, false); setTick((n) => n + 1); } })
    .catch((e) => toast(`Disconnect failed: ${errText(e)}`, true));
  // A viewer badge on an agent row or terminal header opens this panel
  // filtered to that agent until the operator shows them all again.
  const focus = viewersFocus.value;
  const shown = focus ? rows.filter((v) => v.agent === focus || v.session === focus) : rows;
  if (!rows.length && !error && !focus) return null;
  return html`<div class="fa-viewers" id="fleet-viewers">
    <h4>Watching this node's terminals now <span class="muted">${rows.length}</span></h4>
    ${focus && html`<div class="muted" id="fleet-viewers-focus">${shown.length ? 'Showing who views' : 'No one is viewing'} <code title=${focus}>${shown[0]?.session || focus}</code> now · <button type="button" class="fa-link" onClick=${() => { viewersFocus.value = ''; }}>show all</button></div>`}
    ${error && html`<div class="fa-danger">${error}</div>`}
    ${shown.length > 0 && html`<table class="fa-table"><thead><tr><th>Peer</th><th>Agent</th><th>Group</th><th>Mode</th><th>For</th><th></th></tr></thead>
      <tbody>${shown.map((v) => html`<tr key=${v.id} data-viewer=${v.id}>
        <td>${label(v.peer)}</td>
        <td><code title=${v.agent}>${v.session || v.agent}</code></td>
        <td>${v.group || ''}</td>
        <td class=${v.read_only ? 'muted' : 'fa-warn'}>${v.read_only ? 'watching' : 'interactive'}</td>
        <td class="muted">${v.started ? since(v.started, now()) : ''}</td>
        <td class="fa-acts"><button type="button" class="fa-danger" data-fa="kick" onClick=${() => kick(v)}>Disconnect…</button></td>
      </tr>`)}</tbody></table>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation viewers [--session …]</code>, <code>tclaude federation kick ID</code></div>
  </div>`;
}
