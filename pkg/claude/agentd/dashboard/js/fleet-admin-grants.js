import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { PEER_SLUGS, extraPolicy, grantConsequence, grantRows, slugInfo } from './fleet-admin-model.js';

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
  if (row.group) return html`group <b>${row.group}</b>`;
  if (row.allGroups) return html`<span class="fa-warn" title="Covers every current group and every group created later">all groups, incl. future</span>`;
  return html`<span class="muted">node-wide</span>`;
}

// GrantsPage shows and edits one target's peer grants. Every grant goes
// through a confirm naming what it allows and where; an unscoped group grant
// says it covers future groups too.
export function GrantsPage({ view, pools, groups, actions, confirm, toast, target, setTarget }) {
  const targets = grantTargets(view, pools);
  const current = targets.find((t) => t.id === target) || targets[0] || null;
  const [grants, setGrants] = useState(null);
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  const [slug, setSlug] = useState('message.direct');
  const [group, setGroup] = useState('');
  const [maxLive, setMaxLive] = useState(2);
  const info = slugInfo(slug);

  useEffect(() => {
    if (!current) return undefined;
    let off = false;
    setGrants(null); setError('');
    actions.grants(current.id).then((g) => { if (!off) setGrants(g); }).catch((e) => { if (!off) setError(errText(e)); });
    return () => { off = true; };
  }, [current?.id, tick]);

  if (!current) return html`<div class="empty">Trust a peer first; grants say what it may do on this node.</div>`;
  const rows = grants ? grantRows(grants, { ownPool: current.pool ? current.id.slice(6) : '', groups }) : [];
  const scopeGroupName = info.kind === 'node' ? '' : group;
  const canGrant = info.kind !== 'scoped' || !!group;

  const grant = () => {
    const body = { peer: current.id, slug, scope: scopeGroupName ? `group=${scopeGroupName}` : '' };
    // The daemon replaces a grant with the same permission and scope, launch
    // settings included: keep the existing ones and change only the cap.
    // A deleted group's grant has no name, so an unscoped grant matches only
    // an empty scope, never a nameless group row.
    const existing = rows.find((r) => !r.pool && !r.deletedGroup && r.slug === slug && (scopeGroupName ? r.group === scopeGroupName : !r.scope));
    if (existing && !info.policy) { toast(`${current.label} already has ${slug} there`, false); return Promise.resolve(); }
    if (info.policy) body.spawn_policy = { ...(existing?.policy || {}), max_live: Math.max(1, Number(maxLive) || 2) };
    const kept = existing ? extraPolicy(existing.policy) : [];
    const consequence = grantConsequence({ target: current.label, slug, group: scopeGroupName, maxLive: body.spawn_policy?.max_live });
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
    }).catch((e) => toast(`Grant failed: ${errText(e)}`, true));
  };
  const revoke = (row) => confirm({
    title: `Revoke ${row.slug} from ${current.label}?`,
    body: `${current.label} loses ${row.slug} (${row.what || 'this permission'}) ${row.deletedGroup ? "in a deleted group (it applies nowhere)" : row.group ? `in group ${row.group}` : row.allGroups ? 'in every group' : 'node-wide'}. Anything it is doing that relies on it stops being allowed.`,
    okLabel: 'Revoke',
    busyLabel: 'Revoking…',
    action: () => actions.revoke({ peer: current.id, slug: row.slug, scope: row.scope }),
  }).then((ok) => { if (ok) { toast(`Revoked ${row.slug} from ${current.label}`, false); setTick((n) => n + 1); } })
    .catch((e) => toast(`Revoke failed: ${errText(e)}`, true));

  return html`<div class="fa-grants">
    <div class="fa-grants-head">
      <label><span class="fa-k">Grants of</span>
        <select id="fleet-grant-target" value=${current.id} onChange=${(e) => setTarget(e.currentTarget.value)}>
          ${targets.map((t) => html`<option key=${t.id} value=${t.id}>${t.label}${t.level === 'unrestricted' ? ' (unrestricted)' : ''}</option>`)}
        </select></label>
      ${current.level === 'unrestricted' && html`<span class="fa-warn">Unrestricted: ${current.label} already holds every group permission on all groups; the grants below matter once it is restricted.</span>`}
      ${current.pool && html`<span class="muted">A pool's grants apply to every member node.</span>`}
    </div>
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    ${!grants && !error ? html`<div class="empty">Loading…</div>` : rows.length === 0 ? html`<div class="empty">No grants: ${current.label} can do nothing on this node beyond being trusted.</div>` : html`
    <table class="fa-table" id="fleet-grants">
      <thead><tr><th>Permission</th><th>Where</th><th>Allows</th><th></th></tr></thead>
      <tbody>${rows.map((r) => html`<tr key=${r.key} data-slug=${r.slug}>
        <td><code class=${r.sensitive ? 'fa-sensitive' : ''}>${r.slug}</code>${r.maxLive ? html` <span class="muted">cap ${r.maxLive}</span>` : ''}${extraPolicy(r.policy).length ? html` <span class="muted" title=${extraPolicy(r.policy).map((k) => `${k}: ${JSON.stringify(r.policy[k])}`).join('\n')}>+ settings</span>` : ''}</td>
        <td>${where(r)}</td>
        <td class="muted">${r.what}</td>
        <td class="fa-acts">${r.pool
          ? html`<span class="muted" title="Revoke it on the pool">via pool ${r.pool}</span>`
          : !r.revocable ? html`<span class="muted" title="Its group was deleted; the grant no longer applies to any group">group deleted</span>`
          : html`<button type="button" class="fa-danger" data-fa="revoke" onClick=${() => revoke(r)}>Revoke…</button>`}</td>
      </tr>`)}</tbody>
    </table>`}
    <div class="fa-grant-form">
      <span class="fa-k">Add</span>
      <select id="fleet-grant-slug" value=${slug} onChange=${(e) => setSlug(e.currentTarget.value)}>
        ${PEER_SLUGS.map((s) => html`<option key=${s.slug} value=${s.slug}>${s.slug}</option>`)}
      </select>
      ${info.kind !== 'node' && html`<select id="fleet-grant-group" value=${group} onChange=${(e) => setGroup(e.currentTarget.value)}>
        ${info.kind === 'group' ? html`<option value="">all groups (incl. future)</option>` : html`<option value="">pick a group…</option>`}
        ${groups.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}
      </select>`}
      ${info.policy && html`<label><span class="fa-k">cap</span><input id="fleet-grant-cap" type="number" min="1" value=${maxLive} style="width:4em" onInput=${(e) => setMaxLive(e.currentTarget.value)} /></label>`}
      <button id="fleet-grant-submit" type="button" class="primary" disabled=${!canGrant} onClick=${grant}>Grant…</button>
      <span class="muted">${info.what}</span>
    </div>
    <div class="muted fa-cli-note">Receiver launch settings (profile, harness, model, directory, requester pays, job approval) and gateway scopes: <code>tclaude federation grant --help</code></div>
  </div>`;
}
