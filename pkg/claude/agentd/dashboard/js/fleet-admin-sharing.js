// fleet-admin-sharing.js — Fleet → Usage sharing: the model gateways peers
// share with this node, and the ones this node shares with peers, each with
// what is spent of its cap and when it ends (model_shares.go). The shares
// this node gives can be extended, raised or revoked here; a received one
// can be asked to grow, through an operator access request to its peer.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { RequestAccessDialog, USAGE_PERM, createPeerAccessActions, parseUSD } from './peer-access.js';
import { openUsageRulesManager } from './modal-usage-rules.js';

const html = htm.bind(h);
const DAY_MS = 86400000;

function errText(error) { return error?.message || String(error); }

export function dollars(n) {
  return `$${(Number(n) || 0).toFixed(2)}`;
}

// shareBar is how much of a capped share is spent, 0-100 (null: no cap).
export function shareBar(share) {
  if (!(share?.max_usd > 0)) return null;
  return Math.min(100, Math.round(((share.spent_usd || 0) / share.max_usd) * 100));
}

// endsText says when a share ends, relative to now.
export function endsText(share, now) {
  const at = Date.parse(share?.expires_at || '');
  if (!Number.isFinite(at)) return 'no end';
  if (at <= now) return 'ended';
  const days = Math.round((at - now) / DAY_MS);
  if (days >= 2) return `ends in ${days} days`;
  return `ends in ${Math.max(1, Math.round((at - now) / 3600000))} h`;
}

// extendedExpiry is the shown grant's end pushed back by days, counted from
// now when it already passed.
export function extendedExpiry(share, now, days = 7) {
  const at = Date.parse(share?.grant_expires_at || '');
  return new Date(Math.max(Number.isFinite(at) ? at : now, now) + days * DAY_MS).toISOString();
}

function ShareRow({ share, now, children }) {
  const bar = shareBar(share);
  const cls = share.used_up ? 'fa-danger' : bar !== null && bar >= 80 ? 'fa-warn' : '';
  return html`<tr data-share=${`${share.peer}/${share.gateway}`}>
    <td>${share.peer_name || share.peer}</td>
    <td><code>${share.gateway}</code> <span class="muted">${share.dialect}</span></td>
    <td class=${cls}>${bar === null ? html`${dollars(share.spent_usd)} <span class="muted">(${share.unrestricted && !share.scope ? 'unrestricted trust' : 'no cap'})</span>`
      : html`${dollars(share.spent_usd)} / ${dollars(share.max_usd)}${share.used_up ? ' · used up' : ''}
        <div class="fa-share-bar" role="meter" aria-valuemin="0" aria-valuemax="100" aria-valuenow=${bar}><i style=${`width:${bar}%`}></i></div>`}</td>
    <td class="muted">${endsText(share, now)}</td>
    <td class="fa-acts">${children}</td>
  </tr>`;
}

// SharingPage lists both directions. Given rows act on the shown grant
// (its scope), received rows ask that peer for more.
export function SharingPage({ actions, confirm, toast, now = Date.now(), peerAccess = (id) => createPeerAccessActions({ base: `/api/peer/${encodeURIComponent(id)}` }) }) {
  const [given, setGiven] = useState(null);
  const [received, setReceived] = useState(null);
  const [raise, setRaise] = useState({});
  const [asking, setAsking] = useState(null);
  const [tick, setTick] = useState(0);
  const reload = () => setTick((n) => n + 1);
  useEffect(() => {
    let off = false;
    actions.modelSharesGiven().then((v) => { if (!off) setGiven(v); }).catch((e) => { if (!off) setGiven({ error: errText(e) }); });
    actions.modelSharesReceived().then((v) => { if (!off) setReceived(v); }).catch((e) => { if (!off) setReceived({ error: errText(e) }); });
    return () => { off = true; };
  }, [tick]);
  const act = (o) => confirm(o).then((r) => { if (r) { toast(o.done, false); reload(); } })
    .catch((e) => toast(`${o.okLabel} failed: ${errText(e)}`, true));
  const grantOf = (s) => ({ peer: s.peer, slug: USAGE_PERM, scope: s.scope || '' });
  const where = (s) => (s.scope ? `gateway ${s.gateway}` : 'every gateway');
  const extend = (s) => act({
    title: `Extend ${s.peer_name || s.peer}'s usage share by 7 days?`,
    body: `This grant keeps its cap and what it has spent, and ends ${new Date(extendedExpiry(s, now)).toLocaleString()}.`,
    okLabel: 'Extend', busyLabel: 'Saving…', done: 'Usage share extended',
    action: () => actions.extend({ ...grantOf(s), expires_at: extendedExpiry(s, now) }),
  });
  const raiseBy = (s) => {
    const add = parseUSD(raise[s.peer + s.gateway]);
    if (!(add > 0)) { toast('Enter the dollars to add', true); return; }
    const cap = s.max_usd + add;
    act({
      title: `Raise ${s.peer_name || s.peer}'s cap to ${dollars(cap)}?`,
      body: `${s.peer_name || s.peer} may then spend ${dollars(cap - (s.spent_usd || 0))} more through ${where(s)}, paid with this node's credentials.`,
      okLabel: 'Raise', busyLabel: 'Saving…', done: `Cap raised to ${dollars(cap)}`,
      action: async () => {
        await actions.grant({ ...grantOf(s), spawn_policy: { max_usd: cap }, ...(s.grant_expires_at ? { expires_at: s.grant_expires_at } : {}) });
        setRaise((r) => ({ ...r, [s.peer + s.gateway]: '' }));
      },
    });
  };
  const revoke = (s) => act({
    title: `Stop sharing usage with ${s.peer_name || s.peer}?`,
    body: `Its agents lose model access through ${where(s)} at once. What it spent stays recorded.`,
    okLabel: 'Revoke', busyLabel: 'Revoking…', done: 'Usage share revoked',
    action: () => actions.revoke(grantOf(s)),
  });
  const receivedRows = received?.shares || [];
  const receivedErrors = Object.entries(received?.errors || {});
  const givenRows = given?.shares || [];
  return html`<div class="fa-sharing">
    ${globalThis.__tclaudeRemoteNode?.id ? '' : html`<p class="fa-sharing-fleet modal-hint">Budgets across nodes — fleet and node-pool spend caps, and what each node shares with you — live in <button type="button" class="fa-link" id="fa-open-fleet-usage" onClick=${() => openUsageRulesManager({ tab: 'fleet' })}>Fleet usage limits…</button></p>`}
    <h4 class="fa-h">Shared with you <span class="muted">— peers paying for your agents' model calls</span> <button type="button" class="fa-link" onClick=${reload}>refresh</button></h4>
    ${received?.error ? html`<div class="fa-danger">${received.error}</div>` : !received ? html`<div class="empty">Asking peers…</div>`
      : receivedRows.length === 0 ? html`<div class="empty">${received.offline ? 'Federation is off.' : 'No peer shares usage with you. Ask one from its peer view (ask for usage…).'}</div>` : html`
    <table class="fa-table" id="fleet-shares-received">
      <thead><tr><th>Peer</th><th>Gateway</th><th>Spent</th><th>Ends</th><th></th></tr></thead>
      <tbody>${receivedRows.map((s) => html`<${ShareRow} key=${`${s.peer}/${s.gateway}`} share=${s} now=${now}>
        <button type="button" data-fa="ask-more" onClick=${() => setAsking(s)}>Ask for more…</button></${ShareRow}>`)}</tbody>
    </table>`}
    ${receivedErrors.length > 0 && html`<div class="muted">Could not ask: ${receivedErrors.map(([p, e]) => `${p} (${e})`).join(', ')}</div>`}
    <h4 class="fa-h">You share <span class="muted">— peers whose agents bill your model gateways</span></h4>
    ${given?.error ? html`<div class="fa-danger">${given.error}</div>` : !given ? html`<div class="empty">Loading…</div>`
      : givenRows.length === 0 ? html`<div class="empty">You share no model gateway. Grant <code>models.proxy</code> on Peer grants, with a dollar cap and a time, or approve a peer's usage request.</div>` : html`
    <table class="fa-table" id="fleet-shares-given">
      <thead><tr><th>Peer</th><th>Gateway</th><th>Spent</th><th>Ends</th><th></th></tr></thead>
      <tbody>${givenRows.map((s) => html`<${ShareRow} key=${`${s.peer}/${s.gateway}`} share=${s} now=${now}>
        ${s.since && s.pool && html`<span class="muted" title="Change it on Peer grants, as that node group's grant">from node group ${s.pool}</span>`}
        ${s.since && !s.pool && html`${s.grant_expires_at && html`<button type="button" data-fa="extend-share" onClick=${() => extend(s)}>+7 days…</button>`}
        ${s.max_usd > 0 && html`<input class="fa-share-add" aria-label=${`Dollars to add for ${s.peer_name || s.peer}`} placeholder="+$" size="5" inputmode="decimal"
          value=${raise[s.peer + s.gateway] || ''} onInput=${(e) => setRaise({ ...raise, [s.peer + s.gateway]: e.currentTarget.value })} />
          <button type="button" data-fa="raise-share" onClick=${() => raiseBy(s)}>Raise…</button>`}
        <button type="button" class="fa-danger" data-fa="revoke-share" onClick=${() => revoke(s)}>Revoke…</button>`}</${ShareRow}>`)}</tbody>
    </table>`}
    <div class="muted fa-cli-note">You are notified when a share reaches 80% and 100% of its cap and when it ends, on both sides. Dollars are what the gateway charges from the provider's token counts; use an API key for a shared gateway, not a subscription login. CLI: <code>tclaude federation shares [--given]</code>.</div>
    ${asking && html`<${RequestAccessDialog} node=${asking.peer_name || asking.peer} perm=${USAGE_PERM} gateway=${String(asking.scope || '').startsWith('http_proxy=') ? asking.gateway : ''} onClose=${() => setAsking(null)} actions=${peerAccess(asking.peer)} />`}
  </div>`;
}
