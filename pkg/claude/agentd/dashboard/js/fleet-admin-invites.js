import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Select } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { fmtAge } from './fleet-model.js';
import { PEER_SLUGS, TOKEN_TTLS, UNRESTRICTED_CONSEQUENCE, joinCommand, shortID, tokenState } from './fleet-admin-model.js';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

function until(iso, now) {
  const t = Date.parse(iso || '');
  if (!Number.isFinite(t)) return '—';
  return t > now ? `in ${fmtAge(t - now)}` : `${fmtAge(now - t)} ago`;
}

// TokenDialog shows a new invite's bearer exactly once. It lives only in this
// dialog's props: closing drops it, and nothing re-reads it from the daemon.
function TokenDialog({ created, self, copy, toast, onClose }) {
  const onCopy = (text, what) => copy(text).then(() => toast(`${what} copied`, false)).catch(() => toast('Copy failed — select the text instead', true));
  return html`<${Dialog} id="fleet-token-modal" onClose=${onClose} title=${html`Invite token for profile ${created.claims?.profile_name || ''}`}>
    <div class="fa-consequence" role="note">Shown once. It is a bearer secret: deliver it privately (not in chat or a ticket), and the joining operator should pass it on stdin.</div>
    <textarea data-capture-mask id="fleet-token-bearer" class="fa-bearer" readonly rows="3" onFocus=${(e) => e.currentTarget.select?.()}>${created.token}</textarea>
    <div class="fa-dl">
      <span class="fa-k">Master</span><code class="fa-wrap">${self.id}</code>
      <span class="fa-k">Fingerprint</span><code class="fa-wrap fa-fp-full">${created.master_fingerprint || self.fingerprint}</code>
      <span class="fa-k">Uses</span><span>${created.uses}</span>
      <span class="fa-k">Expires</span><span>${created.claims?.expires_at || '—'}</span>
      <span class="fa-k">On the node</span><code class="fa-wrap">${joinCommand(self.id)}</code>
    </div>
    <div class="modal-buttons">
      <${Button} onClick=${() => onCopy(created.token, 'Token')}>Copy token</${Button}>
      <${Button} onClick=${() => onCopy(joinCommand(self.id), 'Command')}>Copy command</${Button}>
      <span class="spacer"></span>
      <${Button} variant="primary" id="fleet-token-close" onClick=${onClose}>Done</${Button}>
    </div>
  </${Dialog}>`;
}

// JoinDialog enrolls this node to a master with a token it received: it
// accepts what the master offers and gives the master nothing here beyond
// restricted trust with no grants. It previews first: both fingerprints, the
// profile, expiry and the trust the master requests (shown as a request,
// never applied), and needs the fingerprint check ticked.
export function JoinDialog({ masters, actions, onClose, onDone }) {
  const [master, setMaster] = useState(masters[0]?.id || '');
  const [token, setToken] = useState('');
  const [preview, setPreview] = useState(null);
  const [checked, setChecked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const reset = () => { setPreview(null); setChecked(false); setError(''); };
  const doPreview = async () => {
    if (!master || !token.trim() || busy) return;
    setBusy(true); reset();
    try { setPreview(await actions.enrollPreview({ master, token: token.trim() })); } catch (e) { setError(errText(e)); } finally { setBusy(false); }
  };
  const enroll = async () => {
    if (!preview || !checked || busy) return;
    setBusy(true); setError('');
    try {
      await actions.enroll({ master, token: token.trim(), previewToken: preview.preview_token });
      onDone(`Enrolled with ${master}`);
    } catch (e) {
      setError(errText(e));
      if (e?.code === 'stale_preview') setPreview(null);
    } finally { setBusy(false); }
  };
  const c = preview?.claims || {};
  const requested = preview?.requested_trust || c.trust_level || 'restricted';
  return html`<${Dialog} id="fleet-join-modal" onClose=${onClose} blocked=${busy} dirty=${!!token} title="Join a master with an invite token">
    <label class="cron-create-row"><span class="cron-create-label">Master</span>
      <${Select} size="sm" id="fleet-join-master" value=${master} onChange=${(e) => { setMaster(e.currentTarget.value); reset(); }}>
        ${masters.map((m) => html`<option key=${m.id} value=${m.id}>${m.label} · ${shortID(m.id)}</option>`)}
      <//></label>
    <label class="cron-create-row"><span class="cron-create-label">Token</span>
      <input id="fleet-join-token" class="fa-token-input" type="password" autocomplete="off" spellcheck="false" placeholder="tcle1…" value=${token}
        onInput=${(e) => { setToken(e.currentTarget.value); reset(); }}
        onKeyDown=${(e) => { if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); if (busy) return; if (!preview) { if (master && token.trim()) doPreview(); } else if (checked) enroll(); } }} /></label>
    ${preview && html`<div class="fa-dl">
      <span class="fa-k">Master</span><code class="fa-wrap">${c.master}</code>
      <span class="fa-k">Master fingerprint</span><code class="fa-wrap fa-fp-full">${preview.master_fingerprint}</code>
      <span class="fa-k">This node</span><code class="fa-wrap">${preview.node_fingerprint}</code>
      <span class="fa-k">Profile</span><span>${c.profile_name} <span class="muted">(${c.profile_id} rev ${c.profile_revision})</span> — your authority on the master</span>
      <span class="fa-k">Expires</span><span>${c.expires_at}</span>
      <span class="fa-k">It gets here</span><span><b>restricted trust, no grants</b></span>
      <span class="fa-k">It asks for</span><span class=${`fa-level ${requested}`}>${requested} trust (a request; Enroll gives it nothing)</span>
    </div>
    ${requested === 'unrestricted' && html`<div class="fa-request" role="note">The master asks for unrestricted trust on this node. Enroll gives it nothing beyond restricted. To give it permissions here, use <b>Become a worker node</b>.</div>`}
    ${preview.consent && html`<div class="muted">${preview.consent}</div>`}
    <label class="fa-ack"><input id="fleet-join-ack" type="checkbox" checked=${checked} onChange=${(e) => setChecked(e.currentTarget.checked)} />
      <span>I compared the master fingerprint with its operator over another channel</span></label>`}
    <div class="cron-create-error ui-error-box" role="alert">${error}</div>
    <div class="modal-buttons">
      <${Button} disabled=${busy} onClick=${onClose}>Cancel</${Button}>
      <span class="spacer"></span>
      ${preview
        ? html`<${Button} id="fleet-join-submit" variant="primary" disabled=${busy || !checked} onClick=${enroll}>${busy ? 'Enrolling…' : 'Enroll'}</${Button}>`
        : html`<${Button} variant="primary" id="fleet-join-preview" disabled=${busy || !master || !token.trim()} onClick=${doPreview}>${busy ? 'Checking…' : 'Preview'}</${Button}>`}
    </div>
  </${Dialog}>`;
}

// BECOME_NODE_SLUGS are the permissions become-node gives: every peer grant
// the picker lists except a gateway's, which takes a policy of its own and is
// given afterwards with a grant (federation_enrollment.go fedBecomeNodeGrant).
export const BECOME_NODE_SLUGS = Object.freeze(PEER_SLUGS.filter((r) => !r.gateway));

// becomeNodeGives turns the picked rows (slug -> group name, '' for every
// group or a node grant) into the request's give_permissions. A scoped slug
// without its group is left out: the dialog does not submit until it has one.
export function becomeNodeGives(picked) {
  return Object.entries(picked)
    .filter(([slug, group]) => group || BECOME_NODE_SLUGS.find((r) => r.slug === slug)?.kind !== 'scoped')
    .map(([slug, group]) => ({ slug, scope: group ? `group=${group}` : '' }))
    .sort((a, b) => a.slug.localeCompare(b.slug));
}

function normFP(s) { return String(s || '').replace(/\s+/g, '').toLowerCase(); }

// BecomeNodeDialog enrolls this node as a worker node of a master and GIVES
// the master exactly the permissions ticked here, or unrestricted trust, on
// this machine. It is never preselected: nothing is ticked, Unrestricted is
// off, and it submits only once the master's fingerprint is pasted to match.
export function BecomeNodeDialog({ masters, groups, actions, onClose, onDone }) {
  const [master, setMaster] = useState(masters[0]?.id || '');
  const [token, setToken] = useState('');
  const [preview, setPreview] = useState(null);
  const [q, setQ] = useState('');
  const [picked, setPicked] = useState({});
  const [unrestricted, setUnrestricted] = useState(false);
  const [fp, setFP] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const reset = () => { setPreview(null); setError(''); };
  const doPreview = async () => {
    if (!master || !token.trim() || busy) return;
    setBusy(true); reset();
    try { setPreview(await actions.enrollPreview({ master, token: token.trim() })); } catch (e) { setError(errText(e)); } finally { setBusy(false); }
  };
  const gives = becomeNodeGives(picked);
  const missingGroup = Object.entries(picked).some(([slug, group]) => !group && BECOME_NODE_SLUGS.find((r) => r.slug === slug)?.kind === 'scoped');
  const fpOK = !!preview && normFP(fp) !== '' && normFP(fp) === normFP(preview.master_fingerprint);
  const ready = !!preview && fpOK && !busy && (unrestricted || (gives.length > 0 && !missingGroup));
  const submit = async () => {
    if (!ready) return;
    setBusy(true); setError('');
    try {
      await actions.becomeNode({ master, token: token.trim(), previewToken: preview.preview_token, confirmFingerprint: preview.master_fingerprint, ...(unrestricted ? { unrestricted: true } : { givePermissions: gives }) });
      onDone(`This node is now a worker node of ${master}`);
    } catch (e) {
      setError(errText(e));
      if (e?.code === 'stale_preview') setPreview(null);
    } finally { setBusy(false); }
  };
  const toggle = (row) => setPicked((cur) => {
    const next = { ...cur };
    if (row.slug in next) delete next[row.slug]; else next[row.slug] = '';
    return next;
  });
  const needle = q.trim().toLowerCase();
  // Ticked rows first, so what is given stays in view as the list scrolls.
  const rows = BECOME_NODE_SLUGS.filter((r) => !needle || r.slug.includes(needle) || (r.what || '').toLowerCase().includes(needle) || r.slug in picked)
    .sort((a, b) => Number(b.slug in picked) - Number(a.slug in picked));
  const c = preview?.claims || {};
  const requested = preview?.requested_trust || c.trust_level || 'restricted';
  const masterLabel = masters.find((m) => m.id === master)?.label || shortID(master);
  const whereText = (slug, group) => {
    const kind = BECOME_NODE_SLUGS.find((r) => r.slug === slug)?.kind;
    if (kind === 'node') return 'this node';
    return group ? `group ${group}` : 'every group, including future ones';
  };
  return html`<${Dialog} id="fleet-become-modal" onClose=${onClose} blocked=${busy} dirty=${!!token || gives.length > 0 || unrestricted} title="Become a worker node of a master">
    <label class="cron-create-row"><span class="cron-create-label">Master</span>
      <${Select} size="sm" id="fleet-become-master" value=${master} onChange=${(e) => { setMaster(e.currentTarget.value); reset(); }}>
        ${masters.map((m) => html`<option key=${m.id} value=${m.id}>${m.label} · ${shortID(m.id)}</option>`)}
      <//></label>
    <label class="cron-create-row"><span class="cron-create-label">Token</span>
      <input id="fleet-become-token" class="fa-token-input" type="password" autocomplete="off" spellcheck="false" placeholder="tcle1…" value=${token}
        onInput=${(e) => { setToken(e.currentTarget.value); reset(); }} /></label>
    ${preview && html`<div class="fa-dl">
      <span class="fa-k">Master fingerprint</span><code class="fa-wrap fa-fp-full">${preview.master_fingerprint}</code>
      <span class="fa-k">You get</span><span>profile ${c.profile_name} <span class="muted">(${c.profile_id} rev ${c.profile_revision})</span> — your authority on the master</span>
      <span class="fa-k">It asks for</span><span>${requested} trust <span class="muted">(a request: only what you tick below is given)</span></span>
    </div>
    <div class="fa-k">Give the master these permissions on this machine</div>
    <input id="fleet-become-filter" class="fa-become-filter" type="search" placeholder="Filter permissions…" value=${q} disabled=${unrestricted} onInput=${(e) => setQ(e.currentTarget.value)} />
    <div class=${`fa-become-perms${unrestricted ? ' off' : ''}`} id="fleet-become-perms">
      ${rows.map((r) => html`<div key=${r.slug} class="fa-become-perm" data-become-slug=${r.slug}>
        <input type="checkbox" checked=${r.slug in picked} disabled=${unrestricted} onChange=${() => toggle(r)} />
        <code>${r.slug}</code>
        <span>${r.kind !== 'node' && r.slug in picked && html`<${Select} size="sm" data-become-group=${r.slug} value=${picked[r.slug]} onChange=${(e) => setPicked({ ...picked, [r.slug]: e.currentTarget.value })}>
          ${r.kind === 'scoped' ? html`<option value="">choose a group…</option>` : html`<option value="">every group</option>`}
          ${groups.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}
        <//>`}${r.sensitive && html`<span class="fa-sensitive">Sensitive</span>`}</span>
        <span class="muted fa-become-what">${r.what}</span>
      </div>`)}
    </div>
    <label class="fa-ack fa-become-unrestricted"><input id="fleet-become-unrestricted" type="checkbox" checked=${unrestricted} onChange=${(e) => setUnrestricted(e.currentTarget.checked)} />
      <span><b>Unrestricted</b> — give the master everything here (disables the list)</span></label>
    ${(unrestricted || gives.length > 0) && html`<div class="fa-become-warn" role="alert" id="fleet-become-warning"><b>⚠ You are handing this machine to ${masterLabel}</b>
      ${unrestricted
        ? html`<div>${UNRESTRICTED_CONSEQUENCE}</div>`
        : html`<div>It will be able to, on this machine:</div><ul>${gives.map((g) => { const group = g.scope.replace(/^group=/, ''); const row = BECOME_NODE_SLUGS.find((r) => r.slug === g.slug); return html`<li key=${g.slug}><code>${g.slug}</code> (${whereText(g.slug, group)})${row?.sensitive ? html` <b>Sensitive</b>` : ''}: ${row?.what || ''}</li>`; })}</ul>`}
      <div>Take it back any time with Revoke on the Grants page, or untrust.</div></div>`}
    <label class="cron-create-row"><span class="cron-create-label">Fingerprint</span>
      <input id="fleet-become-fp" type="text" autocomplete="off" spellcheck="false" placeholder="paste the master's fingerprint, compared with its operator" value=${fp} onInput=${(e) => setFP(e.currentTarget.value)} /></label>
    ${fp && !fpOK && html`<div class="muted">Does not match the master's fingerprint.</div>`}
    ${missingGroup && !unrestricted && html`<div class="muted">Choose a group for each permission that needs one.</div>`}`}
    <div class="cron-create-error ui-error-box" role="alert">${error}</div>
    <div class="modal-buttons">
      <${Button} disabled=${busy} onClick=${onClose}>Cancel</${Button}>
      <span class="spacer"></span>
      ${preview
        ? html`<${Button} id="fleet-become-submit" variant="danger" disabled=${!ready} onClick=${submit}>${busy ? 'Enrolling…' : 'Become a worker node'}</${Button}>`
        : html`<${Button} variant="primary" id="fleet-become-preview" disabled=${busy || !master || !token.trim()} onClick=${doPreview}>${busy ? 'Checking…' : 'Preview'}</${Button}>`}
    </div>
  </${Dialog}>`;
}

// InvitesPage issues and revokes invite tokens (this node as master), joins
// a master with a token, and lists completed enrollments.
export function InvitesPage({ view, groups = [], actions, confirm, toast, copy, now }) {
  const [profiles, setProfiles] = useState(null);
  const [tokens, setTokens] = useState([]);
  const [enrollments, setEnrollments] = useState([]);
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  const [profile, setProfile] = useState('');
  const [uses, setUses] = useState(1);
  const [ttl, setTTL] = useState(86400);
  const [trustLevel, setTrustLevel] = useState('restricted');
  const [created, setCreated] = useState(null);
  const [joining, setJoining] = useState(false);
  const [becoming, setBecoming] = useState(false);

  useEffect(() => {
    let off = false;
    // Each list loads on its own, so one failed read does not hide the others
    // (a profile-read failure must not hide the tokens' Revoke buttons).
    Promise.allSettled([actions.profiles(), actions.tokens(), actions.enrollments()]).then(([p, t, e]) => {
      if (off) return;
      if (p.status === 'fulfilled') setProfiles(p.value?.profiles || []);
      if (t.status === 'fulfilled') setTokens(t.value);
      if (e.status === 'fulfilled') setEnrollments(e.value);
      const failed = [p, t, e].find((r) => r.status === 'rejected');
      setError(failed ? errText(failed.reason) : '');
    });
    return () => { off = true; };
  }, [tick]);

  const list = profiles || [];
  const chosen = list.find((p) => p.name === profile) || list[0] || null;
  const peerName = (id) => view.trusted.find((r) => r.id === id)?.label || shortID(id);

  const create = () => {
    if (!chosen) return undefined;
    const n = Math.max(1, Math.min(10000, Math.floor(Number(uses)) || 1));
    const ttlLabel = TOKEN_TTLS.find((x) => x.seconds === Number(ttl))?.label || `${ttl}s`;
    const enrolledLevel = chosen.definition?.trust_level === 'unrestricted' ? 'unrestricted' : 'restricted';
    return confirm({
      title: `Create an invite for profile ${chosen.name}?`,
      body: `Whoever holds this token and is on the hub can enroll ${n === 1 ? 'one node' : `up to ${n} nodes`} within ${ttlLabel}. `
        + `Each becomes a trusted peer here with profile ${chosen.name}'s pools, grants and worker defaults, at ${enrolledLevel} trust`
        + `${enrolledLevel === 'unrestricted' ? ` — ${UNRESTRICTED_CONSEQUENCE}` : '.'} `
        + `The token asks the joining node for ${trustLevel} trust; that is only a request, which its operator gives or not. `
        + 'Revoking the token later stops new enrollments only.',
      okLabel: 'Create token',
      busyLabel: 'Creating…',
      action: () => actions.createToken({ profile: chosen.name, uses: n, ttlSeconds: Number(ttl), trustLevel }),
    }).then((res) => { if (res && res.token) { setCreated(res); setTick((x) => x + 1); } })
      .catch((e) => toast(`Invite failed: ${errText(e)}`, true));
  };
  const revoke = (t) => confirm({
    title: `Revoke invite ${t.id}?`,
    body: `No new node can enroll with it (${t.used} of ${t.max_uses} uses spent). Nodes that already enrolled with it stay trusted — untrust them on the Peers page.`,
    okLabel: 'Revoke',
    busyLabel: 'Revoking…',
    action: () => actions.revokeToken(t.id),
  }).then((ok) => { if (ok) { toast(`Revoked invite ${t.id}`, false); setTick((x) => x + 1); } })
    .catch((e) => toast(`Revoke failed: ${errText(e)}`, true));

  const masters = [...view.waiting, ...view.trusted].filter((r) => r.online);
  return html`<div class="fa-invites">
    ${error && html`<div class="cron-create-error ui-error-box" role="alert">${error}</div>`}
    <h4>Invite a node</h4>
    <div class="muted">An invite lets a node already on the hub enroll here with a profile, and trust this node back. It is not a hub invite.</div>
    ${profiles && !list.length
      ? html`<div class="empty">Invites need a node profile: create one with <code>tclaude federation profile</code>.</div>`
      : html`<div class="fa-grant-form">
        <label><span class="fa-k">Profile</span><${Select} size="sm" id="fleet-invite-profile" value=${chosen?.name || ''} onChange=${(e) => setProfile(e.currentTarget.value)}>
          ${list.map((p) => html`<option key=${p.id} value=${p.name}>${p.name}${p.definition?.trust_level === 'unrestricted' ? ' (unrestricted)' : ''}</option>`)}
        <//></label>
        <label><span class="fa-k">Uses</span><input id="fleet-invite-uses" type="number" min="1" max="10000" value=${uses} style="width:5em" onInput=${(e) => setUses(e.currentTarget.value)} /></label>
        <label><span class="fa-k">Valid</span><${Select} size="sm" id="fleet-invite-ttl" value=${String(ttl)} onChange=${(e) => setTTL(Number(e.currentTarget.value))}>
          ${TOKEN_TTLS.map((x) => html`<option key=${x.seconds} value=${String(x.seconds)}>${x.label}</option>`)}
        <//></label>
        <label title="The trust this node requests of the joining node: a request only, which its operator gives or not (become-node)"><span class="fa-k">Ask the node for</span><${Select} size="sm" id="fleet-invite-level" value=${trustLevel} onChange=${(e) => setTrustLevel(e.currentTarget.value)}>
          <option value="restricted">restricted</option><option value="unrestricted">unrestricted</option>
        <//></label>
        <${Button} size="sm" variant="primary" id="fleet-invite-create" disabled=${!chosen} onClick=${create}>Create invite…<//>
      </div>`}
    ${tokens.length > 0 && html`<table class="fa-table ui-compact" id="fleet-tokens">
      <thead><tr><th>Token</th><th>Uses</th><th>Expires</th><th>State</th><th></th></tr></thead>
      <tbody>${tokens.map((t) => { const st = tokenState(t, now); return html`<tr key=${t.id} data-token=${t.id}>
        <td><code>${t.id}</code></td><td>${t.used} / ${t.max_uses}</td><td>${until(t.expires_at, now)}</td>
        <td class=${st === 'active' ? '' : 'muted'}>${st}</td>
        <td class="fa-acts">${st === 'active' ? html`<${Button} size="sm" variant="ghost danger" data-fa="revoke-token" onClick=${() => revoke(t)}>Revoke…<//>` : ''}</td>
      </tr>`; })}</tbody>
    </table>`}
    <h4>Join a master</h4>
    <div class="fa-grant-form">
      <${Button} size="sm" id="fleet-join-open" disabled=${!masters.length} onClick=${() => setJoining(true)}>Join with a token…<//>
      <span class="muted">${masters.length ? 'Enroll this node with an invite token another node issued. Gives the master nothing here.' : 'The issuing node must be online on the hub.'}</span>
    </div>
    <div class="fa-grant-form">
      <${Button} size="sm" variant="danger" id="fleet-become-open" disabled=${!masters.length} onClick=${() => setBecoming(true)}>Become a worker node…<//>
      <span class="muted">Enroll and give the master the permissions you pick on this machine: for a worker node of your own fleet.</span>
    </div>
    ${enrollments.length > 0 && html`<h4>Enrollments</h4><table class="fa-table ui-compact" id="fleet-enrollments">
      <thead><tr><th>Direction</th><th>Peer</th><th>Token</th><th>State</th></tr></thead>
      <tbody>${enrollments.map((e) => html`<tr key=${`${e.direction}|${e.token_id}|${e.peer}`}>
        <td>${e.direction}</td><td>${peerName(e.peer)}</td><td><code>${e.token_id}</code></td><td class=${e.retired ? 'muted' : ''}>${e.retired ? 'retired' : 'bound'}</td>
      </tr>`)}</tbody>
    </table>`}
    ${created && html`<${TokenDialog} created=${created} self=${view.self} copy=${copy} toast=${toast} onClose=${() => setCreated(null)} />`}
    ${joining && html`<${JoinDialog} masters=${masters} actions=${actions} onClose=${() => setJoining(false)}
      onDone=${(msg) => { setJoining(false); toast(msg, false); setTick((x) => x + 1); }} />`}
    ${becoming && html`<${BecomeNodeDialog} masters=${masters} groups=${groups} actions=${actions} onClose=${() => setBecoming(false)}
      onDone=${(msg) => { setBecoming(false); toast(msg, false); setTick((x) => x + 1); }} />`}
  </div>`;
}
