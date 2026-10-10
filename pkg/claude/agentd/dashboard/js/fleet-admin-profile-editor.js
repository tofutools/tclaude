import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { PEER_SLUGS, REQUESTER_PAYS, UNRESTRICTED_CONSEQUENCE, slugInfo } from './fleet-admin-model.js';

const html = htm.bind(h);

const NAME_RE = /^[a-z0-9][a-z0-9._-]{0,63}$/;
const LABEL_RE = /^[A-Za-z0-9._-]{1,64}$/;
// ADVANCED are the definition fields edited as JSON (worker permission
// overrides, teleport landing, the config bundle); they pass through as is.
const ADVANCED = ['worker_permissions', 'teleport_landing', 'config_bundle'];

function errText(error) { return error?.message || String(error); }

function scopeText(scope, groupNames) {
  const s = String(scope || '');
  if (!s) return '';
  if (s.startsWith('http_proxy=')) return `gateway ${s.slice(11)}`;
  // The daemon stores a profile grant's group by ID (group=<id>).
  const g = s.startsWith('group_id=') ? s.slice(9) : s.startsWith('group=') ? s.slice(6) : null;
  if (g != null) return /^\d+$/.test(g) ? `group ${groupNames.get(g) || `#${g}`}` : `group ${g}`;
  return s;
}

// definitionFrom builds the definition sent to the daemon from the form;
// ADVANCED fields come from the JSON box and are kept verbatim.
export function definitionFrom({ level, pools, labels, requesterPays, grants, advanced }) {
  const def = { trust_level: level, pools: [...pools], labels: labels.split(',').map((x) => x.trim()).filter(Boolean), peer_grants: grants.map((g) => ({ slug: g.slug, scope: g.scope || '', ...(g.spawn_policy && Object.keys(g.spawn_policy).length ? { spawn_policy: g.spawn_policy } : {}) })) };
  if (requesterPays) def.requester_pays = requesterPays;
  const extra = advanced.trim() ? JSON.parse(advanced) : {};
  if (!extra || typeof extra !== 'object' || Array.isArray(extra)) throw new Error('Advanced must be a JSON object');
  for (const k of Object.keys(extra)) {
    if (!ADVANCED.includes(k)) throw new Error(`Advanced only takes ${ADVANCED.join(', ')} (not ${k})`);
    if (extra[k] != null) def[k] = extra[k];
  }
  return def;
}

// ProfileEditor creates a node profile or edits one (a new revision). A
// profile bundles what a peer gets when it is applied: trust level, pool
// memberships, peer grants, worker defaults. Saving changes no peer: peers
// it was applied to keep their settings until it is applied again.
export function ProfileEditor({ profile = null, pools = [], groups = [], actions, confirm, toast, onClose, onDone }) {
  const d = profile?.definition || {};
  const [name, setName] = useState(profile?.name || '');
  const [level, setLevel] = useState(d.trust_level === 'unrestricted' ? 'unrestricted' : 'restricted');
  const [poolIDs, setPoolIDs] = useState(new Set(d.pools || []));
  const [labels, setLabels] = useState((d.labels || []).join(', '));
  const [requesterPays, setRequesterPays] = useState(d.requester_pays || '');
  const [grants, setGrants] = useState((d.peer_grants || []).map((g, i) => ({ ...g, key: `g${i}` })));
  const [advanced, setAdvanced] = useState(() => {
    const extra = Object.fromEntries(ADVANCED.filter((k) => d[k] != null).map((k) => [k, d[k]]));
    return Object.keys(extra).length ? JSON.stringify(extra, null, 2) : '';
  });
  const [applied, setApplied] = useState(null);
  const [slug, setSlug] = useState('message.direct');
  const [scope, setScope] = useState('');
  const [error, setError] = useState('');
  useEffect(() => {
    if (!profile) return;
    actions.profile(profile.name).then((r) => setApplied(r?.applied_peers || [])).catch(() => setApplied(null));
  }, [profile?.name]);
  const groupNames = new Map();
  const info = slugInfo(slug);
  const addGrant = () => {
    const sc = info.gateway ? (scope.trim() ? `http_proxy=${scope.trim()}` : '') : info.kind === 'node' ? '' : scope ? `group=${scope}` : '';
    if (info.kind === 'scoped' && !sc) { setError(`${slug} needs a group`); return; }
    if (grants.some((g) => g.slug === slug && (g.scope || '') === sc)) { setError(`${slug} is already in the profile there`); return; }
    setGrants([...grants, { key: `n${Date.now()}`, slug, scope: sc, ...(info.policy ? { spawn_policy: { max_live: 2 } } : {}) }]);
    setScope(''); setError('');
  };
  const save = () => {
    let def;
    try { def = definitionFrom({ level, pools: poolIDs, labels, requesterPays, grants, advanced }); } catch (e) { setError(errText(e)); return; }
    if (!profile && !NAME_RE.test(name.trim())) { setError('Profile names are 1-64 lowercase letters, digits, . _ - (start with a letter or digit)'); return; }
    const bad = def.labels.find((l) => !LABEL_RE.test(l));
    if (bad) { setError(`Label ${bad}: letters, digits, . _ - (max 64)`); return; }
    const appliedText = applied == null ? '' : applied.length ? ` It is applied to ${applied.length} peer(s); they keep their current settings until you apply it again (Apply to peer… shows the difference).` : ' It is not applied to any peer yet.';
    const sensitive = def.peer_grants.filter((g) => slugInfo(g.slug).sensitive).map((g) => g.slug);
    confirm({
      title: profile ? `Save profile ${profile.name} (revision ${profile.revision} → ${profile.revision + 1})?` : `Create profile ${name.trim()}?`,
      body: `A peer this profile is applied to gets ${level} trust, ${def.peer_grants.length} peer grant(s)${sensitive.length ? ` (including ${sensitive.join(', ')}, which let it act on this node)` : ''} and ${def.pools.length} pool membership(s).${level === 'unrestricted' ? ` ${UNRESTRICTED_CONSEQUENCE}` : ''}${profile ? `${appliedText} Invites already issued for ${profile.name} pin revision ${profile.revision} and stop working: issue new ones. Peers trusted from now on with it as the default get this version.` : ' Nothing changes for any peer until you apply it, make it the default, or issue invites with it.'}`,
      okLabel: profile ? 'Save' : 'Create',
      busyLabel: 'Saving…',
      action: () => (profile ? actions.saveProfile({ id: profile.id, name: profile.name, revision: profile.revision, definition: def }) : actions.createProfile({ name: name.trim(), definition: def })),
    }).then((r) => { if (r) onDone(profile ? `Saved ${profile.name} (revision ${r.revision ?? profile.revision + 1})` : `Created profile ${name.trim()}`); })
      .catch((e) => setError(e?.status === 409 && e?.code === 'stale_profile' ? 'Someone changed this profile meanwhile: close and reopen it to edit the current revision.' : errText(e)));
  };
  return html`<${Overlay} id="fleet-profile-editor" labelledby="fleet-profile-editor-title" onClose=${onClose}>
    <h3 id="fleet-profile-editor-title">${profile ? `Edit profile ${profile.name}` : 'New node profile'}${profile ? html` <span class="muted">rev ${profile.revision}</span>` : ''}</h3>
    ${!profile && html`<label class="fa-pe-row"><span class="fa-k">name</span><input id="fleet-profile-name" value=${name} autocomplete="off" spellcheck="false" onInput=${(e) => setName(e.currentTarget.value)} /></label>`}
    <label class="fa-pe-row"><span class="fa-k">trust</span><select id="fleet-profile-level" value=${level} onChange=${(e) => setLevel(e.currentTarget.value)}>
      <option value="restricted">restricted: only its grants</option><option value="unrestricted">unrestricted</option></select></label>
    ${level === 'unrestricted' && html`<div class="fa-consequence" role="note"><b>Unrestricted.</b> ${UNRESTRICTED_CONSEQUENCE}</div>`}
    <div class="fa-pe-row"><span class="fa-k">pools</span><span>${pools.length ? pools.map((p) => html`<label key=${p.id} class="fa-pe-check"><input type="checkbox" data-pool=${p.id} checked=${poolIDs.has(p.id)}
      onChange=${(e) => { const s = new Set(poolIDs); if (e.currentTarget.checked) s.add(p.id); else s.delete(p.id); setPoolIDs(s); }} /> ${p.name}</label>`) : html`<span class="muted">no pools</span>`}
      ${[...poolIDs].filter((id) => !pools.some((p) => p.id === id)).map((id) => html`<span key=${id} class="fa-danger" title="This pool no longer exists; saving fails until it is unticked">missing pool ${id} <button type="button" class="fa-link" onClick=${() => { const s = new Set(poolIDs); s.delete(id); setPoolIDs(s); }}>drop</button></span>`)}</span></div>
    <label class="fa-pe-row"><span class="fa-k">labels</span><input id="fleet-profile-labels" value=${labels} placeholder="gpu, ci" onInput=${(e) => setLabels(e.currentTarget.value)} /></label>
    <label class="fa-pe-row"><span class="fa-k">requester pays</span><select id="fleet-profile-pays" value=${requesterPays} onChange=${(e) => setRequesterPays(e.currentTarget.value)}>
      ${REQUESTER_PAYS.map((o) => html`<option key=${o.value} value=${o.value}>${o.label}</option>`)}</select></label>
    <div class="fa-pe-row fa-pe-top"><span class="fa-k">peer grants</span><div>
      ${grants.length === 0 ? html`<div class="muted">none</div>` : html`<table class="fa-table" id="fleet-profile-grants"><tbody>${grants.map((g) => html`<tr key=${g.key} data-slug=${g.slug}>
        <td><code class=${slugInfo(g.slug).sensitive ? 'fa-sensitive' : ''}>${g.slug}</code></td>
        <td>${scopeText(g.scope, groupNames) || (slugInfo(g.slug).kind === 'group' ? html`<span class="fa-warn">all groups, incl. future</span>` : slugInfo(g.slug).gateway ? 'all gateways' : html`<span class="muted">node-wide</span>`)}</td>
        <td>${slugInfo(g.slug).policy ? html`cap <input type="number" min="1" style="width:4em" value=${g.spawn_policy?.max_live || 2}
          onInput=${(e) => setGrants(grants.map((x) => (x.key === g.key ? { ...x, spawn_policy: { ...(x.spawn_policy || {}), max_live: Math.max(1, Number(e.currentTarget.value) || 2) } } : x)))} />` : ''}</td>
        <td class="fa-acts"><button type="button" class="fa-link" data-fa="drop-grant" onClick=${() => setGrants(grants.filter((x) => x.key !== g.key))}>remove</button></td>
      </tr>`)}</tbody></table>`}
      <div class="fa-grant-form">
        <select id="fleet-profile-slug" value=${slug} onChange=${(e) => { setSlug(e.currentTarget.value); setScope(''); }}>
          ${PEER_SLUGS.map((s) => html`<option key=${s.slug} value=${s.slug}>${s.slug}</option>`)}</select>
        ${info.gateway ? html`<input id="fleet-profile-scope" value=${scope} placeholder="all gateways (or a name)" onInput=${(e) => setScope(e.currentTarget.value)} />`
          : info.kind !== 'node' && html`<select id="fleet-profile-scope" value=${scope} onChange=${(e) => setScope(e.currentTarget.value)}>
            ${info.kind === 'group' ? html`<option value="">all groups (incl. future)</option>` : html`<option value="">pick a group…</option>`}
            ${groups.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}</select>`}
        <button id="fleet-profile-add-grant" type="button" onClick=${addGrant}>add</button>
        <span class="muted">${info.what}</span>
      </div>
    </div></div>
    <details class="fa-pe-advanced" open=${!!advanced}><summary>Advanced (JSON): ${ADVANCED.join(', ')}</summary>
      <textarea id="fleet-profile-advanced" rows="6" spellcheck="false" value=${advanced} onInput=${(e) => setAdvanced(e.currentTarget.value)} placeholder='{"worker_permissions": {}}'></textarea>
      <div class="muted">Grant launch settings beyond the cap, worker permission overrides, the teleport landing and the config bundle are kept as written; <code>tclaude federation profile --help</code> describes each.</div>
    </details>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" onClick=${onClose}>Cancel</button>
      <button id="fleet-profile-save" type="button" class="primary" onClick=${save}>${profile ? 'Save…' : 'Create…'}</button>
    </div>
  </${Overlay}>`;
}
