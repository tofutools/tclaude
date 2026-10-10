import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { shortID } from './fleet-admin-model.js';
import { BundleInspectDialog } from './fleet-admin-bundle.js';
import { LandingPicker, landingBlocks, landingSentence, receiverDecides, sentLanding } from './fleet-admin-landing.js';

const html = htm.bind(h);

// CONFIG_SECTIONS mirror configbundle.Sections (the offer-config selectors).
export const CONFIG_SECTIONS = Object.freeze(['roles', 'sandbox-profiles', 'profiles', 'templates', 'process-templates', 'default-permissions', 'config']);

const OPEN = new Set(['pending', 'ready']);

function errText(error) { return error?.message || String(error); }

function when(iso) {
  const t = new Date(iso);
  return Number.isFinite(t.getTime()) && t.getFullYear() > 1 ? t.toLocaleString() : '—';
}

function size(n) {
  if (!(n > 0)) return '—';
  return n < 1024 ? `${n} B` : n < 1 << 20 ? `${(n / 1024).toFixed(1)} KiB` : `${(n / (1 << 20)).toFixed(1)} MiB`;
}

// offerKind names what an offer brings: a config bundle, an agent, or an
// agent that moves or teleports here (its source stops on the peer).
export function offerKind(o) {
  const d = o?.offer || {};
  if (d.type !== 'agent') return d.type || 'config';
  return d.teleport ? 'agent teleport' : d.move ? 'agent move' : 'agent';
}

// importBody is the import request: config offers select by section/name
// (skip) and may replace conflicts; agent offers take launch placement.
// Placeholders resolve through values for both kinds.
export function importBody(offer, opts, apply) {
  const body = { apply: !!apply };
  const values = Object.fromEntries(Object.entries(opts.values || {}).filter(([, v]) => String(v).trim() !== ''));
  if (Object.keys(values).length) body.values = values;
  if (opts.keepPaths) body.keep_paths = true;
  if (offer.offer?.type === 'agent') {
    for (const k of ['cwd', 'worktree', 'group', 'name']) if (String(opts[k] || '').trim()) body[k] = opts[k].trim();
    // A typed path wins over a picked landing candidate, as --cwd over --landing.
    if (opts.landing && !body.cwd) body.landing = opts.landing;
    if (opts.skipHistory && !offer.offer.move && !offer.offer.teleport) body.skip_history = true;
  } else {
    if (opts.skip?.length) body.skip = [...opts.skip];
    if (opts.replace) body.replace = true;
  }
  return body;
}

// previewOf unwraps a preview, including the one a refused apply (conflict,
// unresolved placeholders or paths) returns beside its error. A landing
// refusal (landing_unresolved, landing_missing, landing_unowned,
// landing_candidate_changed) carries the landing to pick from instead.
function landingCode(e) { return /^landing_/.test(e?.code || '') ? e.code : ''; }

function previewOf(e, prev = null) {
  if (e?.body?.preview) return e.body.preview;
  if (e?.body?.landing) return { ...(prev || {}), landing: e.body.landing };
  return null;
}

// configConsequence spells out what applying a config preview does here.
export function configConsequence(peer, preview, skip) {
  const live = (preview?.changes || []).filter((c) => c.action !== 'unchanged' && !skip.includes(c.item));
  const creates = live.filter((c) => c.action === 'create').length;
  const replaces = live.filter((c) => c.action === 'replace').map((c) => c.item);
  const security = live.filter((c) => c.security).map((c) => c.item);
  return `Applies ${live.length} item${live.length === 1 ? '' : 's'} from ${peer} to this node's configuration and they take effect immediately: `
    + `${creates} new${replaces.length ? `, ${replaces.length} overwritten (${replaces.join(', ')})` : ''}.`
    + `${security.length ? ` Security-relevant: ${security.join(', ')} — roles, sandbox profiles, profiles, templates and default permissions decide what this node's agents may do.` : ''}`
    + ` ${peer} is told the offer was applied.`;
}

// agentConsequence spells out what applying an agent preview does here.
export function agentConsequence(peer, offer, preview) {
  const d = offer.offer || {};
  const name = preview?.agent?.name || 'the agent';
  const where = `${preview?.landing ? landingSentence(preview.landing) : preview?.cwd ? ` in ${preview.cwd}` : ''}${preview?.worktree ? ` (worktree ${preview.worktree})` : ''}${preview?.group ? ` in group ${preview.group}` : ''}`;
  const tail = d.teleport || d.move
    ? ` This is a ${d.teleport ? 'teleport' : 'move'}: once it runs here, ${peer} retires its source agent, so this copy becomes the only one.`
    : ` ${peer}'s source agent keeps running there; this is an independent copy.`;
  const mode = preview?.credentials || d.teleport?.credentials || '';
  const creds = d.teleport && mode && mode !== 'local'
    ? `with model access through credential mode ${mode} (arranged with ${peer}, not this node's own harness login), and this node's tools and files`
    : 'using this node\'s harness credentials, tools and files';
  return `Starts a new agent ${name} on this node${where}, ${creds}, under this node's spawn checks.`
    + `${preview?.history ? ' It starts from the shared conversation history, which may contain code, file contents and anything pasted into it.' : ' It starts without conversation history.'}`
    + `${(preview?.findings || []).length ? ` The history has suspected credentials: ${preview.findings.map((f) => `${f.kind} ×${f.count}`).join(', ')}.` : ''}`
    + ` Its permissions and ownership from ${peer} are not copied.${tail}`;
}

function OfferImportDialog({ offer, label, actions, confirm, onClose, onDone }) {
  const agent = offer.offer?.type === 'agent';
  const fixedHistory = !!(offer.offer?.move || offer.offer?.teleport);
  const [opts, setOpts] = useState({ skip: [], values: {}, replace: false, keepPaths: false, cwd: '', landing: '', worktree: '', group: '', name: '', skipHistory: false });
  const [preview, setPreview] = useState(null);
  // rows keeps every config item seen in any preview: a skipped item drops out
  // of the next preview but stays listed (unticked) so it can be re-included.
  const [rows, setRows] = useState([]);
  const [stale, setStale] = useState(true);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  // version counts option edits: a preview answering older options must not
  // mark the current ones as previewed.
  const version = useRef(0);
  const peer = label(offer.peer);

  // refusal is the landing_* code of the last answer: the daemon would not
  // start the agent where the picker points, so Start stays off until a
  // preview resolves again.
  const [refusal, setRefusal] = useState('');
  const show = (p, v = version.current, refused = '') => {
    setPreview(p); setStale(v !== version.current); setRefusal(refused);
    setRows((prev) => {
      const by = new Map(prev.map((c) => [c.item, c]));
      for (const c of p?.changes || []) by.set(c.item, c);
      return [...by.values()];
    });
  };
  const run = (o = opts) => {
    setBusy(true); setError('');
    const v = version.current;
    actions.importOffer(offer, importBody(offer, o, false))
      .then((p) => show(p, v))
      .catch((e) => {
        const p = previewOf(e, preview);
        if (p) show(p, v, landingCode(e));
        // A candidate the daemon no longer offers would fail every preview:
        // fall back to automatic so Preview again can succeed.
        if (e?.code === 'landing_candidate_changed') setOpts((o) => ({ ...o, landing: '' }));
        setError(errText(e));
      })
      .finally(() => setBusy(false));
  };
  useEffect(() => run(), []);
  const change = (patch) => { version.current += 1; const next = { ...opts, ...patch }; setOpts(next); setStale(true); return next; };
  // Picking a landing candidate previews at once, so "Will start in" always
  // echoes the daemon's answer; a typed path waits for Preview again.
  const choose = (id) => run(change({ landing: id, cwd: '' }));
  const set = (k) => (e) => change({ [k]: e.currentTarget.type === 'checkbox' ? e.currentTarget.checked : e.currentTarget.value });
  const toggleItem = (item) => (e) => change({ skip: e.currentTarget.checked ? opts.skip.filter((x) => x !== item) : [...opts.skip, item] });

  const changes = rows;
  const conflicts = changes.some((c) => c.action === 'replace' && !opts.skip.includes(c.item));
  const unresolved = preview?.unresolved || [];
  const missing = unresolved.some((u) => !String(opts.values[u.name] || '').trim());
  const nothing = !agent && !changes.some((c) => c.action !== 'unchanged' && !opts.skip.includes(c.item));
  const blocked = !preview || stale || busy || missing || nothing || (conflicts && !opts.replace) || (agent && (!!refusal || landingBlocks(preview?.landing)));

  const apply = () => confirm({
    title: agent ? `Start ${preview?.agent?.name || 'the agent'} from ${peer}?` : `Apply ${peer}'s config here?`,
    body: agent ? agentConsequence(peer, offer, preview) : configConsequence(peer, preview, opts.skip),
    okLabel: agent ? 'Start agent' : 'Apply',
    busyLabel: agent ? 'Starting…' : 'Applying…',
    action: () => actions.importOffer(offer, importBody(offer, opts, true)),
  }).then((r) => { if (r) onDone(agent ? `Started ${r.spawn?.agent_id || preview?.agent?.name || 'the agent'} from ${peer}'s offer` : `Applied ${(r.applied || []).length} items from ${peer}`); })
    .catch((e) => {
      const p = previewOf(e, preview);
      if (p) setPreview(p);
      if (landingCode(e)) { setRefusal(landingCode(e)); setStale(true); }
      setError(errText(e));
    });

  return html`<${Overlay} id="fleet-offer-import" labelledby="fleet-offer-import-title" onClose=${onClose}>
    <h3 id="fleet-offer-import-title">${offerKind(offer)} offer from ${peer}</h3>
    <div class="muted">${offer.offer?.summary || ''} · ${size(offer.offer?.bytes)} · expires ${when(offer.offer?.expires_at)}${offer.sender_agent ? ` · sent by ${offer.sender_agent}` : ''}</div>
    <div class="muted fa-wrap">sha256 <code>${offer.offer?.sha256 || '—'}</code></div>
    ${agent ? html`
      <div class="fa-of-row"><span class="fa-k">agent</span><span>${preview?.agent?.name || '…'}${preview?.agent?.harness ? html` <span class="muted">(${preview.agent.harness}${preview.agent.role ? `, role ${preview.agent.role}` : ''})</span>` : ''}</span></div>
      ${preview?.landing
        ? html`<${LandingPicker} landing=${preview.landing} refusal=${refusal} choice=${opts.landing} cwd=${opts.cwd} onChoose=${choose} onCwd=${(v) => change({ cwd: v })} />`
        : html`<label class="fa-of-row"><span class="fa-k">cwd</span><input id="fleet-offer-cwd" value=${opts.cwd} placeholder=${preview?.cwd || 'directory on this node'} autocomplete="off" spellcheck="false" onInput=${set('cwd')} /></label>`}
      <label class="fa-of-row"><span class="fa-k">group</span><input id="fleet-offer-group" value=${opts.group} placeholder=${offer.offer?.group ? `the group bound on receipt (${offer.offer.group}); type another to override` : 'receiving group here'} autocomplete="off" onInput=${set('group')} /></label>
      <label class="fa-of-row"><span class="fa-k">name</span><input id="fleet-offer-name" value=${opts.name} placeholder=${preview?.agent?.name || 'keep the offered name'} autocomplete="off" onInput=${set('name')} /></label>
      ${!fixedHistory && html`<label class="fa-of-check"><input id="fleet-offer-skip-history" type="checkbox" checked=${opts.skipHistory} onChange=${set('skipHistory')} /> start without the shared conversation history</label>`}
      ${preview && html`<div class="fa-plan"><ul>
        <li>${preview.history ? 'With conversation history.' : 'Without conversation history.'}</li>
        ${(preview.findings || []).map((f) => html`<li class="fa-danger">Suspected credentials in the history: ${f.kind} ×${f.count}${f.locations?.length ? ` (${f.locations.slice(0, 3).join(', ')})` : ''}</li>`)}
        ${(preview.warnings || []).map((w) => html`<li class="fa-warn">${w}</li>`)}
        ${preview.security && html`<li class="muted">${preview.security}</li>`}
      </ul></div>`}`
    : html`
      ${changes.length > 0 && html`<table class="fa-table" id="fleet-offer-changes"><thead><tr><th></th><th>Item</th><th>Change</th><th></th></tr></thead>
        <tbody>${changes.map((c) => html`<tr key=${c.item} data-item=${c.item}>
          <td><input type="checkbox" aria-label=${`include ${c.item}`} checked=${!opts.skip.includes(c.item)} disabled=${c.action === 'unchanged'} onChange=${toggleItem(c.item)} /></td>
          <td><code>${c.item}</code></td>
          <td class=${c.action === 'replace' ? 'fa-warn' : c.action === 'unchanged' ? 'muted' : ''}>${c.action === 'replace' ? 'overwrites yours' : c.action === 'create' ? 'new' : 'unchanged'}</td>
          <td>${c.security && c.action !== 'unchanged' ? html`<span class="fa-badge fa-sensitive" title="decides what this node's agents may do">security</span>` : ''}</td>
        </tr>`)}</tbody></table>`}
      ${preview && !changes.length && html`<div class="muted">The offer has no items.</div>`}
      ${(preview?.warnings || []).map((w) => html`<div class="fa-warn">${w}</div>`)}
      ${changes.some((c) => c.action === 'replace') && html`<label class="fa-of-check"><input id="fleet-offer-replace" type="checkbox" checked=${opts.replace} onChange=${set('replace')} /> overwrite my items that differ (otherwise untick them)</label>`}`}
    ${unresolved.length > 0 && html`<div class="fa-cli-note">Values this offer needs on this node:</div>
      ${unresolved.map((u) => html`<label class="fa-of-row" key=${u.name}><span class="fa-k">${u.name}</span><input data-placeholder=${u.name} value=${opts.values[u.name] || ''} placeholder=${u.original || `${u.item} ${u.field}`} autocomplete="off" spellcheck="false" onInput=${(e) => change({ values: { ...opts.values, [u.name]: e.currentTarget.value } })} /></label>`)}
      ${!(agent && preview?.landing) && html`<label class="fa-of-check"><input id="fleet-offer-keep-paths" type="checkbox" checked=${opts.keepPaths} onChange=${set('keepPaths')} /> keep the sender's recorded absolute paths</label>`}`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation offers import ${offer.offer?.id} --peer ${shortID(offer.peer)}${agent ? ' [--cwd … | --landing ID] [--group …]' : ' [--skip …] [--replace]'} [--apply]</code></div>
    <div class="modal-buttons">
      <span class="spacer"></span>
      <button id="fleet-offer-preview" type="button" disabled=${busy} onClick=${() => run()}>${busy ? 'Previewing…' : stale && preview ? 'Preview again' : 'Preview'}</button>
      <button id="fleet-offer-apply" type="button" class="primary" disabled=${blocked} title=${stale && preview ? 'Preview again after changing options' : ''} onClick=${apply}>${agent ? 'Start agent…' : 'Apply…'}</button>
      <button type="button" onClick=${onClose}>Close</button>
    </div>
  </${Overlay}>`;
}

function SendConfigDialog({ peers, actions, confirm, onClose, onDone }) {
  const [peer, setPeer] = useState(peers[0]?.id || '');
  const [only, setOnly] = useState([]);
  const [flags, setFlags] = useState(null);
  const [allow, setAllow] = useState(false);
  const [error, setError] = useState('');
  const label = peers.find((p) => p.id === peer)?.label || peer;
  const toggle = (s) => (e) => { setOnly(e.currentTarget.checked ? [...only, s] : only.filter((x) => x !== s)); setFlags(null); setAllow(false); };
  const send = () => {
    if (!peer) { setError('Pick a peer.'); return; }
    setError('');
    confirm({
      title: `Offer this node's config to ${label}?`,
      body: `Sends ${only.length ? only.join(', ') : 'every config section'} to ${label}'s operator, who previews it and decides what to import; nothing changes there until they apply it. Structured credentials are left out, but free text (role prompts, startup context, templates) is sent as written.${allow ? ` It includes ${flags.length} item${flags.length === 1 ? '' : 's'} flagged as possible credentials.` : ''}`,
      okLabel: 'Send offer',
      busyLabel: 'Sending…',
      action: () => actions.offerConfig({ peer, only, allow_flagged: allow }),
    }).then((r) => { if (r) onDone(`Offered config to ${label}`); })
      .catch((e) => { if (e?.code === 'flagged_credentials') { setFlags(e.body?.flags || []); setError('Some items look like credentials. Leave those sections out, or send them anyway.'); } else setError(errText(e)); });
  };
  return html`<${Overlay} id="fleet-offer-config" labelledby="fleet-offer-config-title" onClose=${onClose}>
    <h3 id="fleet-offer-config-title">Offer my config to a peer</h3>
    <label class="fa-of-row"><span class="fa-k">peer</span><select id="fleet-offer-config-peer" value=${peer} onChange=${(e) => setPeer(e.currentTarget.value)}>
      ${peers.map((p) => html`<option key=${p.id} value=${p.id}>${p.label}</option>`)}</select></label>
    <div class="fa-cli-note">Sections (none ticked = all):</div>
    <div class="fa-grant-form">${CONFIG_SECTIONS.map((s) => html`<label key=${s}><input type="checkbox" data-section=${s} checked=${only.includes(s)} onChange=${toggle(s)} /> ${s}</label>`)}</div>
    ${flags && html`<div class="fa-plan"><ul>${flags.map((f) => html`<li class="fa-warn"><code>${f.item}</code> ${f.field}: ${f.hint}</li>`)}</ul></div>
      <label class="fa-of-check"><input id="fleet-offer-config-allow" type="checkbox" checked=${allow} onChange=${(e) => setAllow(e.currentTarget.checked)} /> send the flagged items anyway</label>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation offer-config PEER [--only …] [--allow-flagged]</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button id="fleet-offer-config-send" type="button" class="primary" disabled=${!!flags && !allow} onClick=${send}>Send…</button>
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

function ShareAgentDialog({ peers, agents, actions, confirm, onClose, onDone }) {
  const [form, setForm] = useState({ agent: agents[0]?.id || '', peer: peers[0]?.id || '', group: '', history: false });
  const [findings, setFindings] = useState(null);
  const [allow, setAllow] = useState(false);
  const [error, setError] = useState('');
  const set = (k) => (e) => { setForm({ ...form, [k]: e.currentTarget.type === 'checkbox' ? e.currentTarget.checked : e.currentTarget.value }); setFindings(null); setAllow(false); };
  const peerLabel = peers.find((p) => p.id === form.peer)?.label || form.peer;
  const agentLabel = agents.find((a) => a.id === form.agent)?.label || form.agent;
  const send = () => {
    if (!form.agent || !form.peer) { setError('Pick an agent and a peer.'); return; }
    if (!form.group.trim()) { setError('Name the receiving group on the peer.'); return; }
    setError('');
    confirm({
      title: `Offer ${agentLabel} to ${peerLabel}?`,
      body: `Sends ${agentLabel}'s configuration (role, profile, startup context)${form.history ? ' and a copy of its conversation history — which may contain code, file contents and anything pasted into it —' : ''} to ${peerLabel}'s operator, who previews it and may start their own copy in group ${form.group.trim()}. ${agentLabel} keeps running here; its permissions are not copied. ${receiverDecides(peerLabel)}${allow ? ` The history includes suspected credentials (${findings.map((f) => `${f.kind} ×${f.count}`).join(', ')}).` : ''}`,
      okLabel: 'Send offer',
      busyLabel: 'Sending…',
      action: () => actions.shareAgent({ agent: form.agent, peer: form.peer, group: form.group.trim(), history: form.history, allow_flagged: allow }),
    }).then((r) => { if (r) onDone(`Offered ${agentLabel} to ${peerLabel}${sentLanding(r, peerLabel)}`); })
      .catch((e) => { if (e?.code === 'flagged_credentials') { setFindings(e.body?.findings || []); setError('The history looks like it contains credentials. Share without history, or send it anyway.'); } else setError(errText(e)); });
  };
  return html`<${Overlay} id="fleet-share-agent" labelledby="fleet-share-agent-title" onClose=${onClose}>
    <h3 id="fleet-share-agent-title">Offer an agent to a peer</h3>
    <label class="fa-of-row"><span class="fa-k">agent</span><select id="fleet-share-agent-agent" value=${form.agent} onChange=${set('agent')}>
      ${agents.map((a) => html`<option key=${a.id} value=${a.id}>${a.label}</option>`)}</select></label>
    <label class="fa-of-row"><span class="fa-k">peer</span><select id="fleet-share-agent-peer" value=${form.peer} onChange=${set('peer')}>
      ${peers.map((p) => html`<option key=${p.id} value=${p.id}>${p.label}</option>`)}</select></label>
    <label class="fa-of-row"><span class="fa-k">group</span><input id="fleet-share-agent-group" value=${form.group} placeholder="receiving group on the peer (it must grant agents.receive)" autocomplete="off" onInput=${set('group')} onKeyDown=${(e) => { if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); if (!(findings && !allow)) send(); } }} /></label>
    <label class="fa-of-check"><input id="fleet-share-agent-history" type="checkbox" checked=${form.history} onChange=${set('history')} /> include a copy of the conversation history</label>
    <div class="muted" id="fleet-share-agent-landing">Starting directory: the receiver chooses on accept.</div>
    ${findings && html`<div class="fa-plan"><ul>${findings.map((f) => html`<li class="fa-warn">${f.kind} ×${f.count}${f.locations?.length ? ` (${f.locations.slice(0, 3).join(', ')})` : ''}</li>`)}</ul></div>
      <label class="fa-of-check"><input id="fleet-share-agent-allow" type="checkbox" checked=${allow} onChange=${(e) => setAllow(e.currentTarget.checked)} /> send the history anyway</label>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation share-agent AGENT PEER --group … [--history]</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button id="fleet-share-agent-send" type="button" class="primary" disabled=${!!findings && !allow} onClick=${send}>Send…</button>
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

function OfferProfileDialog({ peers, profiles, actions, confirm, onClose, onDone }) {
  const [profile, setProfile] = useState(profiles[0] || '');
  const [peer, setPeer] = useState(peers[0]?.id || '');
  const [error, setError] = useState('');
  const label = peers.find((p) => p.id === peer)?.label || peer;
  const send = () => {
    if (!profile || !peer) { setError('Pick a profile and a peer.'); return; }
    setError('');
    confirm({
      title: `Offer profile ${profile}'s config to ${label}?`,
      body: `Sends the config and node labels of profile ${profile}, as applied to ${label}, to ${label}'s operator as one config offer. They preview it and choose what to import; nothing changes there until they do. Structured credentials are left out.`,
      okLabel: 'Send offer',
      busyLabel: 'Sending…',
      action: () => actions.offerProfile(profile, peer),
    }).then((r) => {
      if (!r) return;
      if (r.skipped) setError(`Nothing sent: ${r.reason || 'the profile carries no config or labels'}.`);
      else if (r.unchanged) setError(`Nothing sent: an identical offer${r.offer_id ? ` (${r.offer_id})` : ''} is still pending or already applied on ${label}.`);
      else onDone(`Offered profile ${profile} to ${label}`);
    }).catch((e) => setError(e?.status === 409 ? `${errText(e)} — apply the profile to ${label} first (Profiles & pools).` : errText(e)));
  };
  return html`<${Overlay} id="fleet-offer-profile" labelledby="fleet-offer-profile-title" onClose=${onClose}>
    <h3 id="fleet-offer-profile-title">Offer a profile's config to a peer</h3>
    <label class="fa-of-row"><span class="fa-k">profile</span><select id="fleet-offer-profile-name" value=${profile} onChange=${(e) => setProfile(e.currentTarget.value)}>
      ${profiles.map((p) => html`<option key=${p} value=${p}>${p}</option>`)}</select></label>
    <label class="fa-of-row"><span class="fa-k">peer</span><select id="fleet-offer-profile-peer" value=${peer} onChange=${(e) => setPeer(e.currentTarget.value)}>
      ${peers.map((p) => html`<option key=${p.id} value=${p.id}>${p.label}</option>`)}</select></label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation profile offer NAME PEER</code></div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button id="fleet-offer-profile-send" type="button" class="primary" onClick=${send}>Send…</button>
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}

// OffersPage lists bundle offers (config and agents) peers sent this node and
// the ones this node sent, previews and applies or declines incoming ones, and
// sends new ones.
export function OffersPage({ view, agents, actions, confirm, toast }) {
  const [incoming, setIncoming] = useState(null);
  const [outgoing, setOutgoing] = useState(null);
  const [profiles, setProfiles] = useState([]);
  const [error, setError] = useState('');
  const [dialog, setDialog] = useState(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let off = false;
    setError('');
    Promise.all([actions.offers('in'), actions.offers('out')])
      .then(([i, o]) => { if (!off) { setIncoming(i); setOutgoing(o); } })
      .catch((e) => { if (!off) setError(errText(e)); });
    return () => { off = true; };
  }, [tick]);
  useEffect(() => {
    actions.profiles().then((r) => setProfiles((r?.profiles || []).map((p) => p.name).filter(Boolean).sort())).catch(() => {});
  }, []);
  const peers = view.trusted;
  const label = (id) => peers.find((r) => r.id === id)?.label || shortID(id);
  const done = (msg) => { setDialog(null); toast(msg, false); setTick((n) => n + 1); };
  const decline = (o) => confirm({
    title: `Decline ${label(o.peer)}'s ${offerKind(o)} offer?`,
    body: `The downloaded payload is deleted and ${label(o.peer)} is told it was declined; to get it later, ${label(o.peer)} has to offer it again. ${o.import_agent
      ? `An earlier apply reserved agent ${o.import_agent} and it may already be running: declining does not stop it — check it first.`
      : o.last_error ? `An earlier attempt failed (${o.last_error}); items it applied before failing stay applied.` : 'Nothing from it was imported.'}`,
    okLabel: 'Decline',
    busyLabel: 'Declining…',
    action: () => actions.declineOffer(o),
  }).then((r) => { if (r) { toast('Offer declined', false); setTick((n) => n + 1); } })
    .catch((e) => toast(`Decline failed: ${errText(e)}`, true));

  const noPeers = !peers.length;
  return html`<div class="fa-offers">
    <div class="fa-grant-form">
      <button id="fleet-offer-config-open" type="button" disabled=${noPeers} onClick=${() => setDialog({ kind: 'config' })}>Offer my config…</button>
      <button id="fleet-share-agent-open" type="button" disabled=${noPeers || !agents.length} onClick=${() => setDialog({ kind: 'agent' })}>Offer an agent…</button>
      <button id="fleet-offer-profile-open" type="button" disabled=${noPeers || !profiles.length} onClick=${() => setDialog({ kind: 'profile' })}>Offer a profile's config…</button>
      <button id="fleet-offers-refresh" type="button" onClick=${() => setTick((n) => n + 1)}>Refresh</button>
      <span class="muted">${noPeers ? 'Trust a peer first. ' : ''}CLI: <code>tclaude federation offers [--outgoing]</code></span>
    </div>
    ${error && html`<div class="cron-create-error" role="alert">${error}</div>`}
    <h4>Offered to this node</h4>
    ${incoming == null ? (error ? '' : html`<div class="empty">Loading…</div>`) : !incoming.length ? html`<div class="empty">No offers from peers.</div>` : html`
    <table class="fa-table" id="fleet-offers-in">
      <thead><tr><th>From</th><th>Kind</th><th>Summary</th><th>Size</th><th>Expires</th><th>State</th><th></th></tr></thead>
      <tbody>${incoming.map((o) => html`<tr key=${`${o.peer}/${o.offer?.id}`} data-offer=${o.offer?.id}>
        <td>${label(o.peer)}${o.sender_agent ? html` <span class="muted">(${o.sender_agent})</span>` : ''}</td>
        <td class=${o.offer?.move || o.offer?.teleport ? 'fa-warn' : ''}>${offerKind(o)}</td>
        <td class="fa-wrap">${o.offer?.summary || ''}${o.offer?.group ? html` <span class="muted">→ group ${o.offer.group}</span>` : ''}${o.import_agent ? html` <span class="muted">agent ${o.import_agent}</span>` : ''}${o.last_error ? html`<div class="fa-danger">${o.last_error}</div>` : ''}</td>
        <td class="fa-nowrap">${size(o.offer?.bytes)}</td>
        <td class="fa-nowrap">${when(o.offer?.expires_at)}</td>
        <td class=${o.last_error ? 'fa-danger' : ''} title=${o.last_error || ''}>${o.state}</td>
        <td class="fa-acts">${OPEN.has(o.state) && html`<button type="button" data-fa="inspect" onClick=${() => setDialog({ kind: 'inspect', offer: o })}>Inspect…</button><button type="button" data-fa="preview" onClick=${() => setDialog({ kind: 'import', offer: o })}>Preview…</button><button type="button" data-fa="decline" onClick=${() => decline(o)}>Decline…</button>`}</td>
      </tr>`)}</tbody>
    </table>`}
    <h4>Sent by this node</h4>
    ${outgoing == null ? '' : !outgoing.length ? html`<div class="empty">No offers sent.</div>` : html`
    <table class="fa-table" id="fleet-offers-out">
      <thead><tr><th>To</th><th>Kind</th><th>Summary</th><th>Expires</th><th>State</th></tr></thead>
      <tbody>${outgoing.map((o) => html`<tr key=${`${o.peer}/${o.offer?.id}`} data-offer=${o.offer?.id}>
        <td>${label(o.peer)}</td><td>${offerKind(o)}</td><td class="fa-wrap">${o.offer?.summary || ''}${o.last_error ? html`<div class="fa-danger">${o.last_error}</div>` : ''}</td>
        <td class="fa-nowrap">${when(o.offer?.expires_at)}</td>
        <td class=${o.last_error || o.state === 'declined' ? 'fa-danger' : ''} title=${o.last_error || ''}>${o.state}</td>
      </tr>`)}</tbody>
    </table>`}
    ${dialog?.kind === 'inspect' && html`<${BundleInspectDialog} offer=${dialog.offer} label=${label} actions=${actions} toast=${toast}
      onImport=${() => setDialog({ kind: 'import', offer: dialog.offer })} onDecline=${() => { const o = dialog.offer; setDialog(null); decline(o); }} onClose=${() => { setDialog(null); setTick((n) => n + 1); }} />`}
    ${dialog?.kind === 'import' && html`<${OfferImportDialog} offer=${dialog.offer} label=${label} actions=${actions} confirm=${confirm} onClose=${() => { setDialog(null); setTick((n) => n + 1); }} onDone=${done} />`}
    ${dialog?.kind === 'config' && html`<${SendConfigDialog} peers=${peers} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${dialog?.kind === 'agent' && html`<${ShareAgentDialog} peers=${peers} agents=${agents} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${dialog?.kind === 'profile' && html`<${OfferProfileDialog} peers=${peers} profiles=${profiles} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
  </div>`;
}
