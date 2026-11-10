import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { extraPolicy, grantExpiryText, grantRows } from './fleet-admin-model.js';
import { AddGrantDialog } from './fleet-admin-grant-dialog.js';
import { ALL_TARGETS } from './fleet-admin-page.js';
import { Button } from './ui-components.js';
import { PeerCombobox, namedItems } from './registry-picker.js';
import { errText } from './cell-format.js';
import { FaTable } from './fa-table.js';

const html = htm.bind(h);

// grantTargets lists who can hold grants: trusted peers, then node pools
// (group:<name>, whose grants apply to every member node).
export function grantTargets(view, pools) {
  return [
    ...view.trusted.map((r) => ({ id: r.id, label: r.label, level: r.level })),
    ...(Array.isArray(pools) ? pools : []).filter((p) => p?.name).map((p) => ({ id: `group:${p.name}`, label: `pool ${p.name}`, pool: true })),
  ];
}

function where(row) {
  if (row.deletedGroup) return html`<span class="muted">deleted group${row.groupID != null ? ` #${row.groupID}` : ''}</span>`;
  if (row.inert) return html`<span class="fa-warn" title=${`Authorizes nothing: ${row.inert}`}>inert: needs a group</span>`;
  if (row.gateway) return html`gateway <b>${row.gateway}</b>`;
  if (row.group) return html`group <b>${row.group}</b>`;
  if (row.allGroups) return html`<span class="fa-warn" title="Covers every current group and every group created later">all groups, incl. future</span>`;
  return html`<span class="muted">node-wide</span>`;
}

function whereText(row) {
  return row.deletedGroup ? 'in a deleted group (it applies nowhere)' : row.inert ? 'with no group (it applies nowhere)' : row.gateway ? `on gateway ${row.gateway}` : row.group ? `in group ${row.group}` : row.allGroups ? 'in every group' : 'node-wide';
}

export { ALL_TARGETS };

// directRows are the grants a target's own page can revoke: not inherited
// from a pool, and not stuck on a deleted group.
function directRows(rows) { return rows.filter((r) => !r.pool && r.revocable); }

function matches(t, r, q) {
  return !q || [t.label, t.id, r.slug, r.group, r.gateway, r.what].some((x) => String(x || '').toLowerCase().includes(q));
}

// grantCells renders one grant row's permission, where, allows and action
// cells.
function grantCells(r, onRevoke) {
  return html`<td><code class=${r.sensitive ? 'fa-sensitive' : ''}>${r.slug}</code>${r.maxLive ? html` <span class="muted">cap ${r.maxLive}</span>` : ''}${extraPolicy(r.policy).length ? html` <span class="muted" title=${extraPolicy(r.policy).map((k) => `${k}: ${JSON.stringify(r.policy[k])}`).join('\n')}>+ settings</span>` : ''}${r.expiresAt ? html` <span class="muted fa-expiry" data-expires=${r.expiresAt} title=${`Until ${new Date(r.expiresAt).toLocaleString()}`}>${grantExpiryText(r.expiresAt)}</span>` : ''}</td>
    <td>${where(r)}</td>
    <td class="muted">${r.what}</td>
    <td class="fa-acts">${r.pool
      ? html`<span class="muted" title="Revoke it on the pool">via pool ${r.pool}</span>`
      : !r.revocable ? html`<span class="muted" title="Its group was deleted; the grant no longer applies to any group">group deleted</span>`
      : html`<${Button} size="sm" variant="ghost danger" data-fa="revoke" onClick=${onRevoke}>Revoke…<//>`}</td>`;
}

// GrantsPage lists peer grants: every peer's and pool's together
// (ALL_TARGETS, the default) or one target's. One filter narrows either view.
// New grants are added in AddGrantDialog; every grant and revoke goes through
// a confirm naming what it allows and where.
export function GrantsPage({ view, pools, groups, actions, confirm, toast, target, setTarget }) {
  const targets = grantTargets(view, pools);
  const picked = targets.find((t) => t.id === target) || null;
  const all = !picked && targets.length > 0;
  const current = picked;
  const [grants, setGrants] = useState(null);
  // allGrants holds, per target, its grants or the error reading them.
  const [allGrants, setAllGrants] = useState(null);
  const [filter, setFilter] = useState('');
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  // adding is the add dialog's preselected target ('' = none), or null.
  const [adding, setAdding] = useState(null);

  const targetIDs = targets.map((t) => t.id).join(' ');
  useEffect(() => {
    if (!all) return undefined;
    let off = false;
    setAllGrants(null); setError('');
    Promise.all(targets.map((t) => actions.grants(t.id).then((g) => ({ t, grants: g }), (e) => ({ t, error: errText(e) }))))
      .then((list) => { if (!off) setAllGrants(list); });
    return () => { off = true; };
  }, [all, targetIDs, tick]);
  useEffect(() => {
    if (!current) return undefined;
    let off = false;
    setGrants(null); setError('');
    actions.grants(current.id).then((g) => { if (!off) setGrants(g); }).catch((e) => { if (!off) setError(errText(e)); });
    return () => { off = true; };
  }, [current?.id, tick]);

  if (!targets.length) return html`<div class="empty">Trust a peer first; grants say what it may do on this node.</div>`;
  const q = filter.trim().toLowerCase();
  const targetRows = (t, g) => grantRows(g, { ownPool: t.pool ? t.id.slice(6) : '', groups });
  const revoke = (t, row) => confirm({
    title: `Revoke ${row.slug} from ${t.label}?`,
    body: `${t.label} loses ${row.slug} (${row.what || 'this permission'}) ${whereText(row)}. Anything it is doing that relies on it stops being allowed.`,
    okLabel: 'Revoke',
    busyLabel: 'Revoking…',
    action: () => actions.revoke({ peer: t.id, slug: row.slug, scope: row.scope }),
  }).then((ok) => { if (ok) { toast(`Revoked ${row.slug} from ${t.label}`, false); setTick((n) => n + 1); } })
    .catch((e) => toast(`Revoke failed: ${errText(e)}`, true));
  // revokeAll removes every grant listed on t's own page, one revoke each;
  // pool-inherited grants and the trust level are left alone.
  const revokeAll = (t, rows) => {
    const direct = directRows(rows);
    const kept = rows.filter((r) => r.pool).map((r) => `${r.slug} through pool ${r.pool}`);
    const n = direct.length;
    return confirm({
      title: `Revoke all ${n} grant${n === 1 ? '' : 's'} from ${t.label}?`,
      body: `${t.label} loses everything it was granted on this node: ${direct.map((r) => `${r.slug} ${whereText(r)}`).join('; ')}. Anything it is doing that relies on these stops being allowed.${t.pool ? ' This applies to every node in the pool.' : ` It stays trusted (${t.level || 'restricted'})${kept.length ? ` and keeps ${kept.join(', ')}` : ''}.`}${t.level === 'unrestricted' ? ` ${t.label} is unrestricted, so this changes nothing until it is restricted.` : ''}`,
      okLabel: 'Revoke all',
      busyLabel: 'Revoking…',
      action: async () => {
        for (const [i, r] of direct.entries()) {
          try {
            await actions.revoke({ peer: t.id, slug: r.slug, scope: r.scope });
          } catch (e) {
            throw new Error(`revoked ${i} of ${n}; ${r.slug} ${whereText(r)} and the rest remain: ${errText(e)}`);
          }
        }
        return n;
      },
    }).then((ok) => { if (ok) toast(`Revoked ${n} grant${n === 1 ? '' : 's'} from ${t.label}`, false); })
      .catch((e) => toast(`Revoke all failed: ${errText(e)}`, true))
      .finally(() => setTick((k) => k + 1));
  };

  let body; let count = '';
  if (all) {
    const sections = (allGrants || []).map(({ t, grants: g, error: err }) => {
      // full keeps pool-inherited rows so revoke-all can say what survives.
      const full = err ? [] : targetRows(t, g || []);
      const rows = full.filter((r) => !r.pool);
      return { t, err, full, rows, shown: rows.filter((r) => matches(t, r, q)) };
    });
    const total = sections.reduce((n, x) => n + x.rows.length, 0);
    const shownN = sections.reduce((n, x) => n + x.shown.length, 0);
    const visible = sections.filter((x) => !q || x.shown.length);
    if (allGrants) count = q ? `${shownN} of ${total} grants` : `${total} grant${total === 1 ? '' : 's'} across ${allGrants.length} peer${allGrants.length === 1 ? '' : 's'} and pool${allGrants.length === 1 ? '' : 's'}`;
    body = !allGrants ? html`<div class="empty">Loading…</div>` : visible.length === 0 ? html`<div class="empty">No grants match.</div>` : html`
      <${FaTable} id="fleet-grants" head=${['Peer / pool', 'Permission', 'Where', 'Allows', '']}>${visible.map(({ t, err, full, rows, shown }) => html`
          <tr key=${`h|${t.id}`} class="fa-grant-peer" data-peer=${t.id}>
            <td colspan="4"><${Button} size="sm" variant="link" class="fa-link" data-fa="open-target" title="Show only these grants" onClick=${() => setTarget(t.id)}><b>${t.label}</b><//>
              ${!t.pool && html` <code class="muted" title=${t.id}>${t.id.slice(0, 12)}${t.id.length > 12 ? '…' : ''}</code>`}
              <span class="muted"> · </span>${t.level === 'unrestricted' ? html`<span class="fa-level unrestricted">unrestricted</span>` : html`<span class="muted">${t.pool ? 'pool' : 'restricted'}</span>`}
              <span class="muted"> · ${err ? '' : rows.length ? `${rows.length} grant${rows.length === 1 ? '' : 's'}` : 'no grants'}</span>${err && html`<span class="fa-danger">${err}</span>`}</td>
            <td class="fa-acts"><${Button} size="sm" data-fa="add-grant" onClick=${() => setAdding(t.id)}>+ Add<//>${directRows(rows).length > 0 && html`<${Button} size="sm" variant="ghost danger" data-fa="revoke-all" onClick=${() => revokeAll(t, full)}>Revoke all…<//>`}</td>
          </tr>
          ${shown.map((r) => html`<tr key=${`${t.id}|${r.key}`} data-peer=${t.id} data-slug=${r.slug}>
            <td class="muted">${t.label}</td>${grantCells(r, () => revoke(t, r))}</tr>`)}`)}<//>`;
  } else {
    const rows = grants ? targetRows(current, grants) : [];
    const shown = rows.filter((r) => matches(current, r, q));
    if (grants && q) count = `${shown.length} of ${rows.length} grants`;
    body = html`
      ${error && html`<div class="cron-create-error ui-error-box" role="alert">${error}</div>`}
      ${!grants && !error ? html`<div class="empty">Loading…</div>` : rows.length === 0 ? html`<div class="empty">No grants: ${current.label} can do nothing on this node beyond being trusted.</div>` : shown.length === 0 ? html`<div class="empty">No grants match.</div>` : html`
      <${FaTable} id="fleet-grants" head=${['Permission', 'Where', 'Allows', '']}>${shown.map((r) => html`<tr key=${r.key} data-slug=${r.slug}>${grantCells(r, () => revoke(current, r))}</tr>`)}<//>`}
      <div class="fa-grants-foot">
        ${current.level === 'unrestricted' && html`<span class="fa-warn">Unrestricted: ${current.label} already holds every group permission on all groups; the grants above matter once it is restricted.</span>`}
        ${current.pool && html`<span class="muted">A pool's grants apply to every member node.</span>`}
        ${grants && directRows(rows).length > 0 && html`<${Button} size="sm" variant="danger" class="fa-grants-revoke-all" data-fa="revoke-all" onClick=${() => revokeAll(current, rows)}>Revoke all ${directRows(rows).length} from ${current.label}…<//>`}
      </div>`;
  }

  return html`<div class="fa-grants">
    <div class="fa-grants-head">
      <label><span class="fa-k">Grants of</span>
        <${PeerCombobox} id="fleet-grant-target" value=${all ? ALL_TARGETS : current.id} ariaLabel="Grants of" placeholder="Type to find a peer or pool…"
          items=${namedItems([{ name: ALL_TARGETS, label: 'All peers & pools' },
            ...targets.map((t) => ({ name: t.id, label: t.label, descr: t.level === 'unrestricted' ? 'unrestricted' : '' }))])}
          onChange=${setTarget} /></label>
      <input id="fleet-grant-filter" class="fa-grants-filter" type="search" placeholder="Filter: peer, permission, group, description…" value=${filter} autocomplete="off" spellcheck="false" onInput=${(e) => setFilter(e.currentTarget.value)} />
      ${count && html`<span class="muted">${count}</span>`}
      <${Button} size="sm" variant="primary" id="fleet-grant-add" class="fa-grants-add" onClick=${() => setAdding(current ? current.id : '')}>+ Add grant…<//>
    </div>
    ${body}
    ${adding !== null && html`<${AddGrantDialog} targets=${targets} initialTarget=${adding} groups=${groups} actions=${actions} confirm=${confirm} toast=${toast}
      onClose=${() => setAdding(null)} onDone=${(ok = true) => { setTick((n) => n + 1); if (ok) setAdding(null); }} />`}
  </div>`;
}
