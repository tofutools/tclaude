import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { LAUNCH_FIELDS, PEER_SLUGS, REQUESTER_PAYS, extraPolicy, grantConsequence, grantRows, slugInfo } from './fleet-admin-model.js';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

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
  if (row.gateway) return html`gateway <b>${row.gateway}</b>`;
  if (row.group) return html`group <b>${row.group}</b>`;
  if (row.allGroups) return html`<span class="fa-warn" title="Covers every current group and every group created later">all groups, incl. future</span>`;
  return html`<span class="muted">node-wide</span>`;
}

function whereText(row) {
  return row.deletedGroup ? 'in a deleted group (it applies nowhere)' : row.gateway ? `on gateway ${row.gateway}` : row.group ? `in group ${row.group}` : row.allGroups ? 'in every group' : 'node-wide';
}

// ALL_TARGETS is the "Grants of" choice listing every peer's and pool's
// grants together.
export const ALL_TARGETS = '*all';

// directRows are the grants a target's own page can revoke: not inherited
// from a pool, and not stuck on a deleted group.
function directRows(rows) { return rows.filter((r) => !r.pool && r.revocable); }

function matches(t, r, q) {
  return !q || [t.label, t.id, r.slug, r.group, r.gateway, r.what].some((x) => String(x || '').toLowerCase().includes(q));
}

// grantCells renders one grant row's permission, where, allows and action
// cells.
function grantCells(r, onRevoke) {
  return html`<td><code class=${r.sensitive ? 'fa-sensitive' : ''}>${r.slug}</code>${r.maxLive ? html` <span class="muted">cap ${r.maxLive}</span>` : ''}${extraPolicy(r.policy).length ? html` <span class="muted" title=${extraPolicy(r.policy).map((k) => `${k}: ${JSON.stringify(r.policy[k])}`).join('\n')}>+ settings</span>` : ''}</td>
    <td>${where(r)}</td>
    <td class="muted">${r.what}</td>
    <td class="fa-acts">${r.pool
      ? html`<span class="muted" title="Revoke it on the pool">via pool ${r.pool}</span>`
      : !r.revocable ? html`<span class="muted" title="Its group was deleted; the grant no longer applies to any group">group deleted</span>`
      : html`<button type="button" class="fa-danger" data-fa="revoke" onClick=${onRevoke}>Revoke…</button>`}</td>`;
}

// GrantsPage shows and edits one target's peer grants, or lists every
// target's together (ALL_TARGETS). Every grant goes through a confirm naming
// what it allows and where; an unscoped group grant says it covers future
// groups too.
export function GrantsPage({ view, pools, groups, actions, confirm, toast, target, setTarget }) {
  const targets = grantTargets(view, pools);
  const all = target === ALL_TARGETS && targets.length > 0;
  const current = all ? null : targets.find((t) => t.id === target) || targets[0] || null;
  const [grants, setGrants] = useState(null);
  // allGrants holds, per target, its grants or the error reading them.
  const [allGrants, setAllGrants] = useState(null);
  const [filter, setFilter] = useState('');
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  const [slug, setSlug] = useState('message.direct');
  const [group, setGroup] = useState('');
  const [maxLive, setMaxLive] = useState(2);
  const [gateway, setGateway] = useState('');
  const [launch, setLaunch] = useState({});
  const [showLaunch, setShowLaunch] = useState(false);
  const info = slugInfo(slug);
  const fields = LAUNCH_FIELDS.filter((f) => !f.slugs || f.slugs.includes(slug));

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
  const picker = html`<label><span class="fa-k">Grants of</span>
        <select id="fleet-grant-target" value=${all ? ALL_TARGETS : current?.id} onChange=${(e) => setTarget(e.currentTarget.value)}>
          ${targets.map((t) => html`<option key=${t.id} value=${t.id}>${t.label}${t.level === 'unrestricted' ? ' (unrestricted)' : ''}</option>`)}
          <option value=${ALL_TARGETS}>All peers & pools</option>
        </select></label>`;

  if (all) {
    const q = filter.trim().toLowerCase();
    const sections = (allGrants || []).map(({ t, grants: g, error: err }) => {
      // full keeps pool-inherited rows so revoke-all can say what survives.
      const full = err ? [] : targetRows(t, g || []);
      const rows = full.filter((r) => !r.pool);
      return { t, err, full, rows, shown: rows.filter((r) => matches(t, r, q)) };
    }).filter((x) => !q || x.shown.length);
    const total = sections.reduce((n, x) => n + x.rows.length, 0);
    return html`<div class="fa-grants">
      <div class="fa-grants-head">
        ${picker}
        <label><span class="fa-k">Filter</span><input id="fleet-grant-filter" type="text" placeholder="peer, permission or group…" value=${filter} autocomplete="off" spellcheck="false" style="width:14em" onInput=${(e) => setFilter(e.currentTarget.value)} /></label>
        ${allGrants && html`<span class="muted">${total} grant${total === 1 ? '' : 's'} across ${allGrants.length} peer${allGrants.length === 1 ? '' : 's'} and pool${allGrants.length === 1 ? '' : 's'}. Pick one to add grants.</span>`}
      </div>
      ${!allGrants ? html`<div class="empty">Loading…</div>` : sections.length === 0 ? html`<div class="empty">No grants match.</div>` : html`
      <table class="fa-table" id="fleet-grants">
        <thead><tr><th>Peer / pool</th><th>Permission</th><th>Where</th><th>Allows</th><th></th></tr></thead>
        <tbody>${sections.map(({ t, err, full, rows, shown }) => html`
          <tr key=${`h|${t.id}`} class="fa-grant-peer" data-peer=${t.id}>
            <td colspan="4"><button type="button" class="fa-link" data-fa="open-target" title="Show only these grants" onClick=${() => setTarget(t.id)}><b>${t.label}</b></button>
              ${!t.pool && html` <code class="muted">${t.id.slice(0, 12)}${t.id.length > 12 ? '…' : ''}</code>`}
              <span class="muted"> · </span>${t.level === 'unrestricted' ? html`<span class="fa-level unrestricted">unrestricted</span>` : html`<span class="muted">${t.pool ? 'pool' : 'restricted'}</span>`}
              <span class="muted"> · ${err ? '' : rows.length ? `${rows.length} grant${rows.length === 1 ? '' : 's'}` : 'no grants'}</span>${err && html`<span class="fa-danger">${err}</span>`}</td>
            <td class="fa-acts">${directRows(rows).length > 0 && html`<button type="button" class="fa-danger" data-fa="revoke-all" onClick=${() => revokeAll(t, full)}>Revoke all…</button>`}</td>
          </tr>
          ${shown.map((r) => html`<tr key=${`${t.id}|${r.key}`} data-peer=${t.id} data-slug=${r.slug}>
            <td class="muted">${t.label}</td>${grantCells(r, () => revoke(t, r))}</tr>`)}`)}</tbody>
      </table>`}
    </div>`;
  }

  if (!current) return html`<div class="empty">Trust a peer first; grants say what it may do on this node.</div>`;
  const rows = grants ? targetRows(current, grants) : [];
  const direct = directRows(rows);
  const scopeGroupName = info.kind === 'node' ? '' : group;
  const canGrant = info.kind !== 'scoped' || !!group;

  const grant = () => {
    const gw = info.gateway ? gateway.trim() : '';
    if (gw && !/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(gw)) { toast('A gateway name is letters, digits, . _ - (max 64)', true); return Promise.resolve(); }
    const body = { peer: current.id, slug, scope: gw ? `http_proxy=${gw}` : scopeGroupName ? `group=${scopeGroupName}` : '' };
    // The daemon replaces a grant with the same permission and scope, launch
    // settings included: keep the existing ones and change only the cap.
    // A deleted group's grant has no name, so an unscoped grant matches only
    // an empty scope, never a nameless group row.
    const existing = rows.find((r) => !r.pool && !r.deletedGroup && r.slug === slug && (gw ? r.scope === body.scope : scopeGroupName ? r.group === scopeGroupName : !r.scope));
    if (existing && !info.policy) { toast(`${current.label} already has ${slug} there`, false); return Promise.resolve(); }
    if (info.policy) {
      // Launch settings left untouched keep the existing grant's values; the
      // daemon replaces a same-scope grant wholesale.
      // Only settings this permission shows are sent; others typed before
      // switching permission are dropped.
      const visible = new Set([...fields.map((f) => f.key), ...(slug === 'groups.members.spawn' ? ['requester_pays'] : []), ...(slug === 'jobs.run' ? ['job_approval'] : [])]);
      const set = {};
      for (const [k, v] of Object.entries(launch)) {
        if (v === undefined || !visible.has(k)) continue;
        set[k] = k === 'allowed_profiles' ? String(v).split(',').map((x) => x.trim()).filter(Boolean) : String(v).trim();
      }
      body.spawn_policy = { ...(existing?.policy || {}), ...set, max_live: Math.max(1, Number(maxLive) || 2) };
      for (const k of Object.keys(body.spawn_policy)) { const v = body.spawn_policy[k]; if (v === '' || (Array.isArray(v) && !v.length)) delete body.spawn_policy[k]; }
    }
    const kept = existing ? extraPolicy(existing.policy).filter((k) => !(k in (body.spawn_policy || {})) || existing.policy[k] === body.spawn_policy[k]) : [];
    const consequence = grantConsequence({ target: current.label, slug, group: scopeGroupName, maxLive: body.spawn_policy?.max_live, gateway: gw, policy: body.spawn_policy });
    const replaces = existing ? ` This updates the existing grant (cap ${existing.maxLive || 2} → ${body.spawn_policy.max_live})${kept.length ? `; its other launch settings (${kept.join(', ')}) are kept` : ''}.` : '';
    return confirm({
      title: `${existing ? 'Update' : 'Grant'} ${slug} ${existing ? 'for' : 'to'} ${current.label}?`,
      body: `${consequence}${replaces}${info.sensitive ? ' This lets it act on this node, not only read.' : ''}${info.warning ? ` ${info.warning}` : ''}${current.pool ? ' The grant applies to every node in the pool, including nodes added later.' : ''}`,
      okLabel: 'Grant',
      busyLabel: 'Granting…',
      action: () => actions.grant(body),
    }).then((res) => {
      if (!res) return;
      for (const w of res.warnings || []) toast(w, true);
      toast(`Granted ${slug} to ${current.label}`, false);
      setTick((n) => n + 1);
      setLaunch({});
    }).catch((e) => toast(`Grant failed: ${errText(e)}`, true));
  };
  return html`<div class="fa-grants">
    <div class="fa-grants-head">
      ${picker}
      ${current.level === 'unrestricted' && html`<span class="fa-warn">Unrestricted: ${current.label} already holds every group permission on all groups; the grants below matter once it is restricted.</span>`}
      ${current.pool && html`<span class="muted">A pool's grants apply to every member node.</span>`}
      ${direct.length > 0 && html`<button type="button" class="fa-danger fa-grants-revoke-all" data-fa="revoke-all" onClick=${() => revokeAll(current, rows)}>Revoke all ${direct.length} from ${current.label}…</button>`}
    </div>
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    ${!grants && !error ? html`<div class="empty">Loading…</div>` : rows.length === 0 ? html`<div class="empty">No grants: ${current.label} can do nothing on this node beyond being trusted.</div>` : html`
    <table class="fa-table" id="fleet-grants">
      <thead><tr><th>Permission</th><th>Where</th><th>Allows</th><th></th></tr></thead>
      <tbody>${rows.map((r) => html`<tr key=${r.key} data-slug=${r.slug}>${grantCells(r, () => revoke(current, r))}</tr>`)}</tbody>
    </table>`}
    <div class="fa-grant-form">
      <span class="fa-k">Add</span>
      <select id="fleet-grant-slug" value=${slug} onChange=${(e) => { setSlug(e.currentTarget.value); setLaunch({}); }}>
        ${PEER_SLUGS.map((s) => html`<option key=${s.slug} value=${s.slug}>${s.slug}</option>`)}
      </select>
      ${info.kind !== 'node' && html`<select id="fleet-grant-group" value=${group} onChange=${(e) => setGroup(e.currentTarget.value)}>
        ${info.kind === 'group' ? html`<option value="">all groups (incl. future)</option>` : html`<option value="">pick a group…</option>`}
        ${groups.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}
      </select>`}
      ${info.gateway && html`<input id="fleet-grant-gateway" type="text" placeholder="all gateways (or a name)" value=${gateway} autocomplete="off" spellcheck="false" style="width:13em" onInput=${(e) => setGateway(e.currentTarget.value)} />`}
      ${info.policy && html`<label><span class="fa-k">cap</span><input id="fleet-grant-cap" type="number" min="1" value=${maxLive} style="width:4em" onInput=${(e) => setMaxLive(e.currentTarget.value)} /></label>
        <button id="fleet-grant-launch-toggle" type="button" class="fa-link" aria-expanded=${showLaunch ? 'true' : 'false'} onClick=${() => setShowLaunch(!showLaunch)}>launch settings${Object.keys(launch).length ? ' •' : ''}</button>`}
      <button id="fleet-grant-submit" type="button" class="primary" disabled=${!canGrant} onClick=${grant}>Grant…</button>
      <span class="muted">${info.what}</span>
    </div>
    ${info.policy && showLaunch && html`<div class="fa-grant-launch" id="fleet-grant-launch">
      <span class="muted">How this node starts workers for the peer. Empty uses the group's or profile's default; on an existing grant, untouched settings are kept.</span>
      ${fields.map((f) => html`<label key=${f.key}><span class="fa-k">${f.label}</span><input data-launch=${f.key} type="text" placeholder=${f.hint} value=${launch[f.key] ?? ''} autocomplete="off" spellcheck="false" onInput=${(e) => setLaunch({ ...launch, [f.key]: e.currentTarget.value })} /></label>`)}
      ${slug === 'groups.members.spawn' && html`<label><span class="fa-k">requester pays</span><select data-launch="requester_pays" value=${launch.requester_pays ?? ''} onChange=${(e) => setLaunch({ ...launch, requester_pays: e.currentTarget.value })}>
        ${REQUESTER_PAYS.map((o) => html`<option key=${o.value} value=${o.value}>${o.label}</option>`)}</select></label>`}
      ${slug === 'jobs.run' && html`<label><span class="fa-k">job approval</span><select data-launch="job_approval" value=${launch.job_approval ?? ''} onChange=${(e) => setLaunch({ ...launch, job_approval: e.currentTarget.value })}>
        <option value="">auto (default)</option><option value="manual">manual: each job waits for you</option></select></label>`}
    </div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation grant PEER SLUG [--scope …] [--max-live N] [--profile …] [--harness …] [--model …] [--cwd …] [--allow-profile …] [--requester-pays …] [--job-approval …]</code></div>
  </div>`;
}
