import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { fmtAge } from './skynet-model.js';
import { TOKEN_TTLS, UNRESTRICTED_CONSEQUENCE, joinCommand, shortID, tokenState } from './fleet-admin-model.js';

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
  return html`<${Overlay} id="fleet-token-modal" labelledby="fleet-token-title" onClose=${onClose}>
    <h3 id="fleet-token-title">Invite token for profile ${created.claims?.profile_name || ''}</h3>
    <div class="fa-consequence" role="note">Shown once. It is a bearer secret: deliver it privately (not in chat or a ticket), and the joining operator should pass it on stdin.</div>
    <textarea id="fleet-token-bearer" class="fa-bearer" readonly rows="3" onFocus=${(e) => e.currentTarget.select?.()}>${created.token}</textarea>
    <div class="fa-dl">
      <span class="fa-k">Master</span><code class="fa-wrap">${self.id}</code>
      <span class="fa-k">Fingerprint</span><code class="fa-wrap fa-fp-full">${created.master_fingerprint || self.fingerprint}</code>
      <span class="fa-k">Uses</span><span>${created.uses}</span>
      <span class="fa-k">Expires</span><span>${created.claims?.expires_at || '—'}</span>
      <span class="fa-k">On the node</span><code class="fa-wrap">${joinCommand(self.id)}</code>
    </div>
    <div class="modal-buttons">
      <button type="button" onClick=${() => onCopy(created.token, 'Token')}>Copy token</button>
      <button type="button" onClick=${() => onCopy(joinCommand(self.id), 'Command')}>Copy command</button>
      <span class="spacer"></span>
      <button id="fleet-token-close" type="button" class="primary" onClick=${onClose}>Done</button>
    </div>
  </${Overlay}>`;
}

// JoinDialog enrolls this node to a master with a token it received. It
// previews first: both fingerprints, the profile, expiry and the trust level
// this node would grant the master, and needs the fingerprint check ticked.
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
  return html`<${Overlay} id="fleet-join-modal" labelledby="fleet-join-title" onClose=${onClose} blocked=${busy} dirty=${!!token}>
    <h3 id="fleet-join-title">Join a master with an invite token</h3>
    <label class="cron-create-row"><span class="cron-create-label">Master</span>
      <select id="fleet-join-master" value=${master} onChange=${(e) => { setMaster(e.currentTarget.value); reset(); }}>
        ${masters.map((m) => html`<option key=${m.id} value=${m.id}>${m.label} · ${shortID(m.id)}</option>`)}
      </select></label>
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
      <span class="fa-k">You grant it</span><span class=${`fa-level ${c.trust_level}`}>${c.trust_level} trust on this node</span>
    </div>
    ${c.trust_level === 'unrestricted' && html`<div class="fa-consequence" role="note"><b>The master gets unrestricted trust here.</b> ${UNRESTRICTED_CONSEQUENCE}</div>`}
    ${preview.consent && html`<div class="muted">${preview.consent}</div>`}
    <label class="fa-ack"><input id="fleet-join-ack" type="checkbox" checked=${checked} onChange=${(e) => setChecked(e.currentTarget.checked)} />
      <span>I compared the master fingerprint with its operator over another channel</span></label>`}
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onClose}>Cancel</button>
      <span class="spacer"></span>
      ${preview
        ? html`<button id="fleet-join-submit" type="button" class=${c.trust_level === 'unrestricted' ? 'danger' : 'primary'} disabled=${busy || !checked} onClick=${enroll}>${busy ? 'Enrolling…' : 'Enroll'}</button>`
        : html`<button id="fleet-join-preview" type="button" class="primary" disabled=${busy || !master || !token.trim()} onClick=${doPreview}>${busy ? 'Checking…' : 'Preview'}</button>`}
    </div>
  </${Overlay}>`;
}

// InvitesPage issues and revokes invite tokens (this node as master), joins
// a master with a token, and lists completed enrollments.
export function InvitesPage({ view, actions, confirm, toast, copy, now }) {
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
        + `The joining node grants this node ${trustLevel} trust${trustLevel === 'unrestricted' ? ' (every peer permission on all its groups, including future ones)' : ''}. `
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
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    <h4>Invite a node</h4>
    <div class="muted">An invite lets a node already on the hub enroll here with a profile, and trust this node back. It is not a hub invite.</div>
    ${profiles && !list.length
      ? html`<div class="empty">Invites need a node profile: create one with <code>tclaude federation profile</code>.</div>`
      : html`<div class="fa-grant-form">
        <label><span class="fa-k">Profile</span><select id="fleet-invite-profile" value=${chosen?.name || ''} onChange=${(e) => setProfile(e.currentTarget.value)}>
          ${list.map((p) => html`<option key=${p.id} value=${p.name}>${p.name}${p.definition?.trust_level === 'unrestricted' ? ' (unrestricted)' : ''}</option>`)}
        </select></label>
        <label><span class="fa-k">Uses</span><input id="fleet-invite-uses" type="number" min="1" max="10000" value=${uses} style="width:5em" onInput=${(e) => setUses(e.currentTarget.value)} /></label>
        <label><span class="fa-k">Valid</span><select id="fleet-invite-ttl" value=${String(ttl)} onChange=${(e) => setTTL(Number(e.currentTarget.value))}>
          ${TOKEN_TTLS.map((x) => html`<option key=${x.seconds} value=${String(x.seconds)}>${x.label}</option>`)}
        </select></label>
        <label title="The trust the joining node grants this node"><span class="fa-k">Node trusts us</span><select id="fleet-invite-level" value=${trustLevel} onChange=${(e) => setTrustLevel(e.currentTarget.value)}>
          <option value="restricted">restricted</option><option value="unrestricted">unrestricted</option>
        </select></label>
        <button id="fleet-invite-create" type="button" class="primary" disabled=${!chosen} onClick=${create}>Create invite…</button>
      </div>`}
    ${tokens.length > 0 && html`<table class="fa-table" id="fleet-tokens">
      <thead><tr><th>Token</th><th>Uses</th><th>Expires</th><th>State</th><th></th></tr></thead>
      <tbody>${tokens.map((t) => { const st = tokenState(t, now); return html`<tr key=${t.id} data-token=${t.id}>
        <td><code>${t.id}</code></td><td>${t.used} / ${t.max_uses}</td><td>${until(t.expires_at, now)}</td>
        <td class=${st === 'active' ? '' : 'muted'}>${st}</td>
        <td class="fa-acts">${st === 'active' ? html`<button type="button" class="fa-danger" data-fa="revoke-token" onClick=${() => revoke(t)}>Revoke…</button>` : ''}</td>
      </tr>`; })}</tbody>
    </table>`}
    <h4>Join a master</h4>
    <div class="fa-grant-form">
      <button id="fleet-join-open" type="button" disabled=${!masters.length} onClick=${() => setJoining(true)}>Join with a token…</button>
      <span class="muted">${masters.length ? 'Enroll this node with an invite token another node issued.' : 'The issuing node must be online on the hub.'}</span>
    </div>
    ${enrollments.length > 0 && html`<h4>Enrollments</h4><table class="fa-table" id="fleet-enrollments">
      <thead><tr><th>Direction</th><th>Peer</th><th>Token</th><th>State</th></tr></thead>
      <tbody>${enrollments.map((e) => html`<tr key=${`${e.direction}|${e.token_id}|${e.peer}`}>
        <td>${e.direction}</td><td>${peerName(e.peer)}</td><td><code>${e.token_id}</code></td><td class=${e.retired ? 'muted' : ''}>${e.retired ? 'retired' : 'bound'}</td>
      </tr>`)}</tbody>
    </table>`}
    ${created && html`<${TokenDialog} created=${created} self=${view.self} copy=${copy} toast=${toast} onClose=${() => setCreated(null)} />`}
    ${joining && html`<${JoinDialog} masters=${masters} actions=${actions} onClose=${() => setJoining(false)}
      onDone=${(msg) => { setJoining(false); toast(msg, false); setTick((x) => x + 1); }} />`}
  </div>`;
}
