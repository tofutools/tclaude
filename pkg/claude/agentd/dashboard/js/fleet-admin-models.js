import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

function num(n) {
  if (!n) return '—';
  if (n >= 1e6) return `${(n / 1e6).toFixed(n % 1e6 ? 1 : 0)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(n % 1e3 ? 1 : 0)}k`;
  return String(n);
}

function utcToday(now) { return new Date(now).toISOString().slice(0, 10); }

// limitText renders a daily requests/tokens pair. There is no "unlimited":
// a limit that is not set (0) makes the gateway refuse every request.
function limitText(requests, tokens) {
  return `${requests ? `${num(requests)} req` : 'req not set'} / ${tokens ? `${num(tokens)} tok` : 'tok not set'}`;
}

// gatewayGaps lists what a gateway's policy is missing. The daemon refuses
// every request through a gateway without a model allowlist, positive daily
// budgets (all, per peer, per session), per-request token caps, a concurrency
// cap and a rate.
export function gatewayGaps(p) {
  const gaps = [];
  if (!(p.models || []).length) gaps.push('model allowlist');
  if (!(p.daily_requests > 0 && p.daily_tokens > 0)) gaps.push('daily budget');
  if (!(p.peer_daily_requests > 0 && p.peer_daily_tokens > 0)) gaps.push('per-peer budget');
  if (!(p.session_daily_requests > 0 && p.session_daily_tokens > 0)) gaps.push('per-session budget');
  if (!(p.max_input_tokens > 0 && p.max_output_tokens > 0)) gaps.push('per-request token caps');
  if (!(p.max_concurrent >= 1)) gaps.push('concurrency cap');
  if (!(p.requests_per_minute >= 1)) gaps.push('rate limit');
  return gaps;
}

// usageRows sums the day's request rows per gateway × peer × model.
export function usageRows(rows) {
  const by = new Map();
  for (const r of rows || []) {
    const k = `${r.proxy}\u0000${r.peer}\u0000${r.model}`;
    const a = by.get(k) || { proxy: r.proxy, peer: r.peer, model: r.model, requests: 0, failed: 0, running: 0, charged: 0, input: 0, output: 0 };
    a.requests += 1;
    // A row is written when a request is reserved (status 0, incomplete, the
    // reservation charged) and settled when it ends.
    if (!r.status && !r.complete) a.running += 1;
    else if (r.status >= 400 || !r.complete) a.failed += 1;
    a.charged += r.charged_tokens || 0;
    a.input += r.input_tokens || 0;
    a.output += r.output_tokens || 0;
    by.set(k, a);
  }
  return [...by.values()].sort((a, b) => b.charged - a.charged || a.proxy.localeCompare(b.proxy));
}

// ModelsPage administers this node's model gateways: peers with a
// models.proxy grant send model requests through them, charged against the
// gateway's limits (models.proxy, or models.proxy.leased for requester-paid
// workers). Every switch here only narrows or restores access; the
// gateway itself (provider, credentials, limits) is configured in config.
export function ModelsPage({ view, actions, confirm, toast, now = Date.now() }) {
  const [control, setControl] = useState(null);
  const [leases, setLeases] = useState(null);
  const [usage, setUsage] = useState(null);
  const [day, setDay] = useState(utcToday(now));
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  const [blocking, setBlocking] = useState({});
  const label = (id) => view.trusted.find((r) => r.id === id)?.label || id;
  const reload = () => setTick((n) => n + 1);

  useEffect(() => {
    let off = false;
    actions.models().then((c) => { if (!off) { setControl(c); setError(''); } }).catch((e) => { if (!off) setError(errText(e)); });
    actions.modelLeases().then((l) => { if (!off) setLeases(l); }).catch((e) => { if (!off) setLeases({ error: errText(e) }); });
    return () => { off = true; };
  }, [tick]);
  useEffect(() => {
    let off = false;
    setUsage(null);
    actions.modelUsage(day).then((u) => { if (!off) setUsage(u); }).catch((e) => { if (!off) setUsage({ error: errText(e) }); });
    return () => { off = true; };
  }, [day, tick]);

  const flip = (o) => confirm(o).then((r) => { if (r) { toast(o.done, false); reload(); } })
    .catch((e) => toast(`${o.okLabel} failed: ${errText(e)}`, true));
  const master = () => flip(control.disabled ? {
    title: 'Turn model gateways back on?',
    body: 'Gateways that are enabled serve peers with a models.proxy grant again, within their limits. Revoked leases stay revoked; requester-paid workers need a new lease.',
    okLabel: 'Turn on', busyLabel: 'Saving…', done: 'Model gateways are on',
    action: () => actions.setModelSwitch({ disabled: false }),
  } : {
    title: 'Turn off every model gateway on this node?',
    body: 'No peer can send model requests through this node until you turn them back on, and every active lease is revoked at once: requester-paid workers using them lose model access mid-task. Gateway settings and peer grants are kept.',
    okLabel: 'Turn off all', busyLabel: 'Saving…', done: 'Model gateways are off',
    action: () => actions.setModelSwitch({ disabled: true }),
  });
  const caveat = (p) => {
    const gaps = gatewayGaps(p);
    return [control.disabled ? ' All gateways are currently off, so nothing is served until you turn them back on.' : '',
      gaps.length ? ` Its policy is missing ${gaps.join(', ')}, so it still refuses every request until that is configured.` : ''].join('');
  };
  const gateway = (name, p) => flip(p.enabled ? {
    title: `Disable gateway ${name}?`,
    body: `${name} stops serving peers and its active leases are revoked at once (workers using them lose model access). Its settings and peer grants are kept.`,
    okLabel: 'Disable', busyLabel: 'Saving…', done: `${name} disabled`,
    action: () => actions.setModelSwitch({ name, disabled: true }),
  } : {
    title: `Enable gateway ${name}?`,
    body: `Peers with a models.proxy (or, for requester-paid workers, models.proxy.leased) grant for ${name} can send model requests through it again, charged to this node's provider account within its limits (${limitText(p.daily_requests, p.daily_tokens)} a day).${caveat(p)}`,
    okLabel: 'Enable', busyLabel: 'Saving…', done: `${name} enabled`,
    action: () => actions.setModelSwitch({ name, disabled: false }),
  });
  const block = (name, peer) => flip({
    title: `Block ${label(peer)} on ${name}?`,
    body: `${label(peer)} can no longer use ${name}, even with a models.proxy grant, and its leases on ${name} are revoked at once. Its grants stay; unblock to restore access.`,
    okLabel: 'Block', busyLabel: 'Saving…', done: `${label(peer)} blocked on ${name}`,
    action: () => actions.setModelSwitch({ name, peer, disabled: true }),
  });
  const unblock = (name, peer) => flip({
    title: `Unblock ${label(peer)} on ${name}?`,
    body: `${label(peer)} can use ${name} again wherever its models.proxy or models.proxy.leased grants allow.${caveat(control.gateways?.[name] || {})}`,
    okLabel: 'Unblock', busyLabel: 'Saving…', done: `${label(peer)} unblocked on ${name}`,
    action: () => actions.setModelSwitch({ name, peer, disabled: false }),
  });
  const revoke = (l) => flip({
    title: `Revoke lease ${l.id.slice(0, 12)}…?`,
    body: `The ${label(l.peer)} worker${l.worker ? ` ${l.worker}` : ''} using this lease loses model access through ${l.proxy} at once. It cannot be restored; the peer has to ask for a new lease.`,
    okLabel: 'Revoke', busyLabel: 'Revoking…', done: 'Lease revoked',
    action: () => actions.revokeModelLease(l.id),
  });

  const gws = control?.gateways ? Object.entries(control.gateways).sort(([a], [b]) => a.localeCompare(b)) : [];
  const peers = view.trusted || [];
  const leaseList = Array.isArray(leases) ? [...leases].sort((a, b) => Number(a.revoked) - Number(b.revoked) || String(b.touched_at).localeCompare(String(a.touched_at))) : [];
  const used = Array.isArray(usage) ? usageRows(usage) : [];
  return html`<div class="fa-models">
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    ${!control && !error && html`<div class="empty">Loading…</div>`}
    ${control && html`<div class="fa-run-settings" id="fleet-models-master">
      <span class="fa-k">Model gateways</span>
      <span class=${control.disabled ? 'fa-warn' : 'muted'}>${control.disabled ? 'all off: no peer model requests are served' : `on (${gws.filter(([, p]) => p.enabled).length} of ${gws.length} enabled)`}</span>
      <button id="fleet-models-master-toggle" type="button" class=${control.disabled ? '' : 'fa-danger'} onClick=${master}>${control.disabled ? 'Turn on…' : 'Turn off all…'}</button>
      <button type="button" class="fa-link" onClick=${reload}>refresh</button>
    </div>
    ${gws.length === 0 ? html`<div class="empty">No model gateways are configured. A gateway is an HTTP proxy with a model policy in config.</div>` : html`
    <table class="fa-table" id="fleet-model-gateways">
      <thead><tr><th>Gateway</th><th>State</th><th>Models</th><th>Daily: all · per peer · per session</th><th>Per request</th><th>Blocked peers</th><th></th></tr></thead>
      <tbody>${gws.map(([name, p]) => html`<tr key=${name} data-gateway=${name}>
        <td><code>${name}</code>${p.dialect ? html` <span class="muted">${p.dialect}</span>` : ''}</td>
        <td class=${p.enabled && !control.disabled && !gatewayGaps(p).length ? '' : 'fa-warn'} title=${gatewayGaps(p).length ? `Missing: ${gatewayGaps(p).join(', ')}` : ''}>${!p.enabled ? 'disabled' : control.disabled ? 'enabled (all off)' : gatewayGaps(p).length ? 'refuses requests: policy incomplete' : 'enabled'}</td>
        <td>${(p.models || []).length ? p.models.join(', ') : html`<span class="fa-warn">none</span>`}</td>
        <td>${limitText(p.daily_requests, p.daily_tokens)} · ${limitText(p.peer_daily_requests, p.peer_daily_tokens)} · ${limitText(p.session_daily_requests, p.session_daily_tokens)}</td>
        <td class="muted">${[p.max_input_tokens ? `≤${num(p.max_input_tokens)} in` : '', p.max_output_tokens ? `≤${num(p.max_output_tokens)} out` : '', p.max_concurrent ? `${p.max_concurrent} at once` : '', p.requests_per_minute ? `${p.requests_per_minute}/min` : '', `leases idle out after ${p.lease_idle_hours || 24}h`].filter(Boolean).join(' · ')}</td>
        <td>${(p.blocked_peers || []).map((b) => html`<span class="fa-chip" key=${b}>${label(b)} <button type="button" class="fa-link" title=${`Unblock ${label(b)}`} data-fa="unblock" onClick=${() => unblock(name, b)}>×</button></span> `)}
          ${peers.filter((r) => !(p.blocked_peers || []).includes(r.id)).length > 0 && html`<select class="fa-model-block" aria-label=${`Block a peer on ${name}`} value=${blocking[name] || ''} onChange=${(e) => setBlocking({ ...blocking, [name]: e.currentTarget.value })}>
            <option value="">block a peer…</option>
            ${peers.filter((r) => !(p.blocked_peers || []).includes(r.id)).map((r) => html`<option key=${r.id} value=${r.id}>${r.label}</option>`)}
          </select>
          ${blocking[name] && html`<button type="button" class="fa-danger" data-fa="block" onClick=${() => { const b = blocking[name]; setBlocking({ ...blocking, [name]: '' }); block(name, b); }}>Block…</button>`}`}</td>
        <td class="fa-acts"><button type="button" class=${p.enabled ? 'fa-danger' : ''} data-fa="gateway" onClick=${() => gateway(name, p)}>${p.enabled ? 'Disable…' : 'Enable…'}</button></td>
      </tr>`)}</tbody>
    </table>`}`}
    <h4 class="fa-h">Leases <span class="muted">— requester-paid workers using a gateway</span></h4>
    ${leases?.error ? html`<div class="fa-danger">${leases.error}</div>` : !leases ? html`<div class="empty">Loading…</div>` : leaseList.length === 0 ? html`<div class="empty">No leases.</div>` : html`
    <table class="fa-table" id="fleet-model-leases">
      <thead><tr><th>Lease</th><th>Peer</th><th>Gateway</th><th>Worker</th><th>Last used</th><th></th></tr></thead>
      <tbody>${leaseList.map((l) => html`<tr key=${l.id} data-lease=${l.id} class=${l.revoked ? 'muted' : ''}>
        <td><code title=${l.id}>${l.id.slice(0, 12)}</code>${l.kind ? html` <span class="muted">${l.kind}</span>` : ''}</td>
        <td>${label(l.peer)}</td>
        <td><code>${l.proxy}</code></td>
        <td>${l.worker ? html`<code title=${l.worker}>${l.worker.slice(0, 14)}</code>` : '—'}</td>
        <td class="muted">${l.touched_at && !String(l.touched_at).startsWith('0001-') ? new Date(l.touched_at).toLocaleString() : ''}${l.idle_seconds ? ` · idles out after ${Math.round(l.idle_seconds / 3600)}h` : ''}</td>
        <td class="fa-acts">${l.revoked ? 'revoked' : html`<button type="button" class="fa-danger" data-fa="revoke-lease" onClick=${() => revoke(l)}>Revoke…</button>`}</td>
      </tr>`)}</tbody>
    </table>`}
    <h4 class="fa-h">Usage <input type="date" id="fleet-model-day" value=${day} max=${utcToday(now)} onChange=${(e) => e.currentTarget.value && setDay(e.currentTarget.value)} /> <span class="muted">UTC day</span></h4>
    ${usage?.error ? html`<div class="fa-danger">${usage.error}</div>` : !usage ? html`<div class="empty">Loading…</div>` : used.length === 0 ? html`<div class="empty">No model requests on ${day}.</div>` : html`
    <table class="fa-table" id="fleet-model-usage">
      <thead><tr><th>Gateway</th><th>Peer</th><th>Model</th><th>Requests</th><th>Charged tokens</th><th>In · out</th></tr></thead>
      <tbody>${used.map((u) => html`<tr key=${`${u.proxy}/${u.peer}/${u.model}`}>
        <td><code>${u.proxy}</code></td><td>${label(u.peer)}</td><td>${u.model || '—'}</td>
        <td>${u.requests}${u.running ? html` <span class="muted">(${u.running} in progress)</span>` : ''}${u.failed ? html` <span class="fa-danger">(${u.failed} failed)</span>` : ''}</td>
        <td>${num(u.charged)}</td><td class="muted">${num(u.input)} · ${num(u.output)}</td>
      </tr>`)}</tbody>
    </table>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation models status|usage --day …|leases [--revoke ID]|disable|enable [--name GW] [--peer PEER]</code>. Grant peers access with <code>models.proxy</code> (or <code>models.proxy.leased</code>) on Peer grants. In-progress requests count their reservation until they finish.</div>
  </div>`;
}
