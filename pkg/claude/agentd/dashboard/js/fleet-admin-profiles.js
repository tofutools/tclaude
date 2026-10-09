import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { POOL_NAME_RE, UNRESTRICTED_CONSEQUENCE, changeText, grantText, profileSummary } from './fleet-admin-model.js';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

// ApplyDialog applies a node profile to one trusted peer: it previews the
// plan (every change, how many are security-relevant, conflicts) and commits
// exactly that plan. Making a restricted peer unrestricted repeats the
// consequence and confirms its fingerprint.
export function ApplyDialog({ profile, peers, actions, poolNames = new Map(), onClose, onDone }) {
  const [peer, setPeer] = useState(peers[0]?.id || '');
  const [plan, setPlan] = useState(null);
  const [checked, setChecked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const row = peers.find((p) => p.id === peer) || null;
  const widens = profile.level === 'unrestricted' && row?.level !== 'unrestricted';
  useEffect(() => {
    if (!peer) return undefined;
    let off = false;
    setPlan(null); setChecked(false); setError('');
    actions.applyProfile(profile.name, { peer })
      .then((p) => { if (!off) setPlan(p); })
      .catch((e) => { if (off) return; setError(errText(e)); if (e?.body?.plan) setPlan(e.body.plan); });
    return () => { off = true; };
  }, [peer]);
  const changes = plan?.changes || [];
  const conflicts = plan?.conflicts || [];
  const ready = plan?.preview_token && !conflicts.length && (!widens || checked);
  const apply = async () => {
    if (!ready || busy) return;
    setBusy(true); setError('');
    try {
      await actions.applyProfile(profile.name, { peer, apply: true, previewToken: plan.preview_token, confirmFingerprint: widens ? row.fingerprint : '' });
      onDone(`Applied ${profile.name} to ${row.label}`);
    } catch (e) { setError(errText(e)); } finally { setBusy(false); }
  };
  return html`<${Overlay} id="fleet-apply-modal" labelledby="fleet-apply-title" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-apply-title">Apply profile ${profile.name}</h3>
    <label class="cron-create-row"><span class="cron-create-label">To peer</span>
      <select id="fleet-apply-peer" value=${peer} onChange=${(e) => setPeer(e.currentTarget.value)}>
        ${peers.map((p) => html`<option key=${p.id} value=${p.id}>${p.label} (${p.level})</option>`)}
      </select></label>
    ${!plan && !error ? html`<div class="empty">Previewing…</div>` : plan && html`<div class="fa-plan">
      <div>${changes.length ? `${changes.length} change(s), ${plan.security_changes || 0} security-relevant${plan.future_workers_only ? ' (worker defaults apply to future workers only)' : ''}:` : 'No changes: the peer already matches this profile.'}</div>
      ${changes.length > 0 && html`<ul id="fleet-apply-changes">${changes.map((c, i) => html`<li key=${i} class=${c.security ? 'fa-sensitive' : ''}>${changeText(c, poolNames)}</li>`)}</ul>`}
      ${(plan.pools || []).filter((p) => (p.live_grants || []).length).map((p) => html`<div key=${p.id} class="fa-pool-grants">Via pool <b>${p.name}</b> it gets: ${p.live_grants.map((g, i) => html`${i ? ', ' : ''}<span class=${!g.scope && !g.group_name ? 'fa-warn' : ''}>${grantText(g)}</span>`)}</div>`)}
      ${conflicts.length > 0 && html`<div class="cron-create-error">Conflicts — resolve before applying: ${conflicts.join('; ')}</div>`}
    </div>`}
    ${widens && html`<div class="fa-consequence" role="note"><b>${row.label} becomes unrestricted.</b> ${UNRESTRICTED_CONSEQUENCE}</div>
      <div class="fa-dl"><span class="fa-k">Fingerprint</span><code class="fa-wrap fa-fp-full">${row.fingerprint || '—'}</code></div>
      <label class="fa-ack"><input id="fleet-apply-ack" type="checkbox" checked=${checked} onChange=${(e) => setChecked(e.currentTarget.checked)} />
        <span>I understand, and this is ${row.label}'s fingerprint</span></label>`}
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      <span class="spacer"></span>
      <button id="fleet-apply-submit" type="button" class=${widens ? 'danger' : 'primary'} disabled=${busy || !ready || !changes.length} onClick=${apply}>${busy ? 'Applying…' : 'Apply'}</button>
    </div>
  </${Overlay}>`;
}

// ProfilesPage manages node pools (membership carries the pool's grants) and
// node profiles (the default for newly trusted peers, applying one to a peer,
// deleting). Profile definitions are edited with the CLI.
export function ProfilesPage({ view, pools, actions, confirm, toast, onOpenGrants, reload }) {
  const [profiles, setProfiles] = useState(null);
  const [defaultID, setDefaultID] = useState('');
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  const [newPool, setNewPool] = useState('');
  const [adding, setAdding] = useState({});
  const [applying, setApplying] = useState(null);
  const refresh = () => { setTick((n) => n + 1); reload(); };

  useEffect(() => {
    let off = false;
    actions.profiles().then((r) => {
      if (off) return;
      setProfiles(r?.profiles || []);
      setDefaultID(r?.default?.id || '');
      setError('');
    }).catch((e) => { if (!off) setError(errText(e)); });
    return () => { off = true; };
  }, [tick]);

  const done = (msg) => { toast(msg, false); refresh(); };
  const summaries = profiles ? profiles.map((p) => profileSummary(p, new Map(pools.map((x) => [x.id, x.name])))) : null;
  const fail = (what) => (e) => toast(`${what} failed: ${errText(e)}`, true);
  const label = (id) => view.trusted.find((r) => r.id === id)?.label || id;
  const poolGrants = async (name) => {
    try { return await actions.grants(`group:${name}`); } catch (_) { return null; }
  };
  const grantList = (gs) => (gs == null ? 'its grants' : gs.length ? `its ${gs.length} grant(s): ${gs.map(grantText).join(', ')}` : 'no grants yet');
  const poolNames = new Map(pools.map((p) => [p.id, p.name]));

  const createPool = () => {
    const name = newPool.trim();
    if (!POOL_NAME_RE.test(name)) { toast('Pool names are lowercase letters, digits, . _ - (start with a letter or digit, max 64)', true); return; }
    actions.createPool(name).then(() => { setNewPool(''); done(`Created pool ${name}`); }).catch(fail('Create pool'));
  };
  const deletePool = async (pool) => {
    const gs = await poolGrants(pool.name);
    const users = (summaries || []).filter((p) => p.poolIDs.includes(pool.id));
    confirm({
      title: `Delete pool ${pool.name}?`,
      body: `Its ${pool.members.length} member(s) lose ${grantList(gs)}, and the pool's grants are deleted with it. The members stay trusted with their own direct grants.`
        + (users.length ? ` Profiles ${users.map((p) => p.name).join(', ')} include this pool: applying them${users.some((p) => p.id === defaultID) ? ', trusting new peers with the default profile' : ''} and enrolling with their invites fail until their definitions drop it.` : ''),
      okLabel: 'Delete pool', busyLabel: 'Deleting…',
      action: () => actions.deletePool(pool.name),
    }).then((ok) => { if (ok) done(`Deleted pool ${pool.name}`); }).catch(fail('Delete pool'));
  };
  const addMember = async (pool) => {
    const peer = adding[pool.name] || '';
    if (!peer) return;
    const gs = await poolGrants(pool.name);
    confirm({
      title: `Add ${label(peer)} to pool ${pool.name}?`,
      body: `${label(peer)} gains ${grantList(gs)} — and any grant added to the pool later.`,
      okLabel: 'Add to pool', busyLabel: 'Adding…',
      action: () => actions.addPoolMember(pool.name, peer),
    }).then((ok) => { if (ok) { setAdding({ ...adding, [pool.name]: '' }); done(`Added ${label(peer)} to ${pool.name}`); } }).catch(fail('Add member'));
  };
  const removeMember = (pool, peer) => confirm({
    title: `Remove ${label(peer)} from pool ${pool.name}?`,
    body: `${label(peer)} loses the pool's grants (its own direct grants stay).`,
    okLabel: 'Remove', busyLabel: 'Removing…',
    action: () => actions.removePoolMember(pool.name, peer),
  }).then((ok) => { if (ok) done(`Removed ${label(peer)} from ${pool.name}`); }).catch(fail('Remove member'));

  const setDefault = async (p) => {
    const via = [];
    for (const name of p ? p.pools : []) {
      const gs = await poolGrants(name);
      via.push(`pool ${name} (${grantList(gs)})`);
    }
    return confirm({
    title: p ? `Make ${p.name} the default peer profile?` : 'Clear the default peer profile?',
    body: p
      ? `Every peer trusted from now on (unless the operator opts out) gets ${p.name}: ${p.level} trust, ${p.grants} direct peer grant(s)${via.length ? `, and membership of ${via.join('; ')}` : ''}.${p.level === 'unrestricted' ? ` ${UNRESTRICTED_CONSEQUENCE}` : ''} Peers already trusted are not changed.`
      : 'Peers trusted from now on start restricted with no grants unless a profile is chosen. Peers already trusted are not changed.',
    okLabel: p ? 'Make default' : 'Clear default', busyLabel: 'Saving…',
    action: () => actions.setDefaultProfile(p ? p.name : ''),
  }).then((ok) => { if (ok) done(p ? `${p.name} is the default peer profile` : 'Cleared the default peer profile'); }).catch(fail('Default profile'));
  };
  const deleteProfile = (p) => confirm({
    title: `Delete profile ${p.name}?`,
    body: 'Only an unused profile can be deleted: one applied to a peer or set as the default is refused. Unused invite tokens for it stop working.',
    okLabel: 'Delete', busyLabel: 'Deleting…',
    action: () => actions.deleteProfile(p.name),
  }).then((ok) => { if (ok) done(`Deleted profile ${p.name}`); }).catch(fail('Delete profile'));

  return html`<div class="fa-profiles">
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    <h4>Node pools <span class="muted">${pools.length}</span></h4>
    <div class="muted">A pool's grants apply to every member node — peer grants for a pool are edited on the Peer grants page.</div>
    ${pools.length > 0 && html`<table class="fa-table" id="fleet-pools">
      <thead><tr><th>Pool</th><th>Members</th><th>Add member</th><th></th></tr></thead>
      <tbody>${pools.map((pool) => {
        const ids = (pool.members || []).map((m) => m.instance_id);
        const candidates = view.trusted.filter((r) => !ids.includes(r.id));
        return html`<tr key=${pool.name} data-pool=${pool.name}>
          <td><b>${pool.name}</b></td>
          <td>${ids.length ? ids.map((id) => html`<span key=${id} class="fa-member">${label(id)} <button type="button" class="fa-link" data-fa="remove-member" title="Remove from the pool" onClick=${() => removeMember(pool, id)}>×</button></span>`) : html`<span class="muted">none</span>`}</td>
          <td>${candidates.length ? html`<select data-fa="member-pick" value=${adding[pool.name] || ''} onChange=${(e) => setAdding({ ...adding, [pool.name]: e.currentTarget.value })}>
              <option value="">trusted peer…</option>${candidates.map((r) => html`<option key=${r.id} value=${r.id}>${r.label}</option>`)}
            </select> <button type="button" data-fa="add-member" disabled=${!adding[pool.name]} onClick=${() => addMember(pool)}>Add…</button>` : html`<span class="muted">—</span>`}</td>
          <td class="fa-acts"><button type="button" data-fa="pool-grants" onClick=${() => onOpenGrants(`group:${pool.name}`)}>Grants…</button>
            <button type="button" class="fa-danger" data-fa="delete-pool" onClick=${() => deletePool(pool)}>Delete…</button></td>
        </tr>`;
      })}</tbody>
    </table>`}
    <div class="fa-grant-form">
      <input id="fleet-pool-name" type="text" placeholder="new pool name" value=${newPool} autocomplete="off" spellcheck="false" onInput=${(e) => setNewPool(e.currentTarget.value)} />
      <button id="fleet-pool-create" type="button" disabled=${!newPool.trim()} onClick=${createPool}>Create pool</button>
    </div>
    <h4>Node profiles <span class="muted">${summaries ? summaries.length : ''}</span></h4>
    <div class="muted">A profile bundles a trust level, pools, peer grants and worker defaults for a peer. Create and edit definitions with <code>tclaude federation profile</code>.</div>
    ${summaries && summaries.length === 0 && html`<div class="empty">No profiles yet.</div>`}
    ${summaries && summaries.length > 0 && html`<table class="fa-table" id="fleet-profiles">
      <thead><tr><th>Profile</th><th>Level</th><th>Pools</th><th>Grants</th><th>Labels</th><th></th></tr></thead>
      <tbody>${summaries.map((p) => html`<tr key=${p.id} data-profile=${p.name}>
        <td><b>${p.name}</b> <span class="muted">rev ${p.revision}</span>${p.id === defaultID ? html` <span class="fa-badge">default</span>` : ''}</td>
        <td><span class=${`fa-level ${p.level}`}>${p.level}</span></td>
        <td>${p.pools.join(', ') || html`<span class="muted">—</span>`}</td>
        <td>${p.grants}</td>
        <td>${p.labels.join(', ') || html`<span class="muted">—</span>`}</td>
        <td class="fa-acts">
          <button type="button" data-fa="apply-profile" disabled=${!view.trusted.length} onClick=${() => setApplying(p)}>Apply to peer…</button>
          ${p.id === defaultID
            ? html`<button type="button" data-fa="clear-default" onClick=${() => setDefault(null)}>Clear default…</button>`
            : html`<button type="button" data-fa="make-default" onClick=${() => setDefault(p)}>Make default…</button>`}
          <button type="button" class="fa-danger" data-fa="delete-profile" onClick=${() => deleteProfile(p)}>Delete…</button>
        </td>
      </tr>`)}</tbody>
    </table>`}
    ${applying && html`<${ApplyDialog} profile=${applying} peers=${view.trusted} actions=${actions} poolNames=${poolNames} onClose=${() => setApplying(null)}
      onDone=${(msg) => { setApplying(null); done(msg); }} />`}
  </div>`;
}
