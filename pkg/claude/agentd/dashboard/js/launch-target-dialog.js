// launch-target-dialog.js — one dialog for sending an agent elsewhere
// (tcl-o9d5d9): Move, Teleport, Clone, Morph and Recall home. The ⚙ menu's
// "Send elsewhere" items open it on a mode; a drop in "Groups · all nodes"
// opens it on Move with the node and group pre-filled, any mode a pick away.
// Each mode shows only
// the fields it needs, says what carries over, and lists unavailable targets
// with the reason instead of hiding them.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { MovePanel, dropPlan, moveProgress, MOVE_POLL_MS, openFleetPage } from './fleet-move-drop.js';
import { shellToast } from './shell-state.js';
import { missingPullGrants, needsSuffix } from './peer-view-limits.js';
import { nodeFetch } from './node-api.js';
import { unrestrictedOn } from './node-trust.js';
import { markInTransit } from './drop-landing.js';
import { modelAccessOptions, ownGateways } from './teleport-model-access.js';

const html = htm.bind(h);

// TELEPORT_ROUTES gates Teleport, Clone to a peer and Recall home on the
// operator teleport routes (POST /api/agents/{id}/teleport and the recall
// route's home return, #2883). Off, the modes stay visible but greyed with the
// reason, and out of the ⚙ menu; it stays a switch for tests and rollbacks.
export const TELEPORT_ROUTES = true;

// Each mode's hint says in plain words what it does, under the mode tabs.
export const MODES = Object.freeze([
  { key: 'move', label: 'Move', hint: 'Transfers it to the target node as a member there; it stops on its current node once its copy runs.' },
  { key: 'teleport', label: 'Teleport', hint: 'Sends it on a trip to the target node, with an optional arrival note; Recall home brings it back.' },
  { key: 'clone', label: 'Clone', hint: 'Copies it: a new agent starts with its history; the original keeps running.' },
  { key: 'morph', label: 'Morph', hint: 'Keeps it here as the same agent, on another harness, model or profile.' },
  { key: 'home', label: 'Recall home', hint: 'Brings a teleported agent back to its home node.' },
]);

const NOT_YET = 'Needs the operator teleport routes, which this dashboard does not have yet.';

// modeState says whether a mode can be used for this agent from this
// dashboard. local: the agent runs on this node. presence: its presenceKind.
// acting: the agent's node trusts this one unrestricted, so a morph runs on
// it as on that node's own dashboard.
// pull: a peer's agent can be asked here through that peer's action routes,
// which teleport or clone it to this node only. missing: the peer grants
// that peer withholds on the agent, which every pull needs.
export function modeState(mode, { local, acting = false, presence = '', homeIsSelf = false, source = 'its node', teleport = TELEPORT_ROUTES, pull = false, missing = [] }) {
  const there = `Do this from ${source}'s dashboard.`;
  if (!local && missing.length && (mode === 'move' || ((mode === 'teleport' || mode === 'clone') && pull))) return { ok: false, why: grantWhy(source, missing) };
  switch (mode) {
    case 'move': return { ok: true };
    case 'clone': return local || (pull && teleport) ? { ok: true } : { ok: false, why: there };
    case 'morph': return local || acting ? { ok: true } : { ok: false, why: there };
    case 'teleport':
      if (!teleport) return { ok: false, why: NOT_YET };
      return local || pull ? { ok: true } : { ok: false, why: there };
    case 'home':
      if (!teleport) return { ok: false, why: NOT_YET };
      // From its home (the away placeholder, or its visiting row in a fused
      // view) it is recalled; where it runs, it is sent home.
      if (presence === 'away' || (presence === 'visiting' && homeIsSelf)) return { ok: true };
      if (presence !== 'visiting') return { ok: false, why: 'It is at home.' };
      return local ? { ok: true } : { ok: false, why: `Recall it from its home's dashboard or send it home from ${source}'s.` };
    default: return { ok: false, why: '' };
  }
}

// grantWhy says which grants a peer must give this node for a pull.
export function grantWhy(source, missing) {
  return `${source} does not let you bring this agent here${needsSuffix(missing)}`;
}

// receivingGroups lists a peer's catalog groups that take agents from this
// node, from the federation status (status.remote[].groups[].caps).
export function receivingGroups(status, peer, mode) {
  const remote = (Array.isArray(status?.remote) ? status.remote : []).find((r) => r?.peer === peer);
  const caps = mode === 'move' ? ['agents_receive'] : ['agents_receive', 'teleport_receive'];
  return (remote?.groups || []).filter((g) => g?.name && (g.caps || []).some((c) => caps.includes(c))).map((g) => g.name).sort();
}

// nodeTargets lists every node with whether this mode may send the agent
// there, and why not. self is this node; source is where the agent runs.
// relay is set when the source is a peer that trusts this node unrestricted:
// it can then send its agent on to a third node (nodeGroups[id] lists that
// node's groups), as its own operator could.
export function nodeTargets({ mode, fleet, self, source, status, localGroups = [], teleport = TELEPORT_ROUTES, relay = false, nodeGroups = {}, pull = false, missing = [] }) {
  const nodes = [self, ...(fleet?.peers || [])].filter((n) => n?.id);
  return nodes.map((n) => {
    const here = n.id === source.node;
    const isSelf = n.id === self.id;
    const row = { id: n.id, name: isSelf ? `${n.name} (this node)` : n.name, plain: n.name, color: n.color, local: isSelf, ok: true, why: '', groups: [] };
    if (here && mode === 'clone' && !isSelf) return { ...row, ok: false, why: `clone it from ${source.name}'s dashboard` };
    if (here) return { ...row, ok: mode === 'clone', why: mode === 'clone' ? '' : 'it is already here — drag it onto another group to move it within this node', groups: [] };
    if (!isSelf && n.online === false) return { ...row, ok: false, why: 'offline' };
    if (source.node !== self.id) {
      // A peer's agent can only be pulled here: by a move, or through the
      // peer's teleport route (a clone too). Only a move goes on to a third
      // node, when the peer relays it.
      if (!isSelf) return relay && mode === 'move' ? { ...row, groups: nodeGroups[n.id] || [] } : { ...row, ok: false, why: `do this from ${source.name}'s dashboard` };
      const pulls = mode === 'move' || (pull && teleport && (mode === 'teleport' || mode === 'clone'));
      if (!pulls) return { ...row, ok: false, why: `do this from ${source.name}'s dashboard` };
      if (missing.length) return { ...row, ok: false, why: `not granted by ${source.name}${needsSuffix(missing)}` };
      return { ...row, groups: localGroups };
    }
    if (mode === 'clone' && !teleport) return { ...row, ok: false, why: 'a peer clone needs the operator teleport routes (not on this dashboard yet)' };
    return { ...row, groups: receivingGroups(status, n.id, mode) };
  });
}

// carries is the "what carries over" panel: [mark, label, text] rows.
export function carries(mode, { node = 'the target', home = 'its home', morph = null, local = true } = {}) {
  switch (mode) {
    case 'move': return [
      ['✓', 'History', 'the full conversation history goes with it'],
      ['↻', 'Identity', `it arrives on ${node} as an agent there`],
      ['✗', 'Permissions', `not carried; ${node}'s group defaults apply`],
      ['✗', 'Here', 'retired once its copy runs there'],
    ];
    case 'teleport': return [
      ['✓', 'History', 'the full conversation history goes with it'],
      ['↻', 'Identity', `a visiting identity on ${node}; its home stays ${home} for Recall home`],
      ['✗', 'Permissions', `not carried; ${node}'s group defaults apply`],
      ['✓', 'Here', 'a placeholder row stays in its groups'],
    ];
    case 'clone': return local ? [
      ['✓', 'History', 'copied (or a fresh context: choose in the next step)'],
      ['↻', 'Identity', 'a new sibling that inherits its groups and permissions (ownership only if you ask)'],
      ['✓', 'Here', 'the original keeps running'],
    ] : [
      ['✓', 'History', 'the full conversation history is copied'],
      ['↻', 'Identity', `an independent agent on ${node}`],
      ['✓', 'Here', 'the original keeps running'],
    ];
    case 'morph': {
      const rows = [['✓', 'Identity', 'same agent_id, inbox, groups and permissions']];
      const hist = morph?.history;
      if (hist && hist.target && hist.source !== hist.target) {
        const lost = (hist.items || []).map((i) => `${i.kind} ×${i.count}`).join(', ');
        const estimates = hist.carried_estimated_tokens > 0 ? `; estimated tokens: ${hist.source_estimated_tokens ?? '?'} source → ${hist.carried_estimated_tokens} carried` : '';
        const window = hist.context_window_tokens > 0 ? `; window ${hist.context_window_tokens} (${hist.context_window_source}), budget ${hist.budget_tokens}` : '';
        rows.push(['≈', 'History', `translated ${hist.source} → ${hist.target}: ${hist.carried_entries ?? '?'} of ${hist.source_entries ?? '?'} entries carried${estimates}${window}${lost ? ` (${lost})` : ''}`]);
      } else rows.push(['✓', 'History', 'kept: it resumes the same conversation']);
      // Both layers, as the daemon resolves them for the launch: the harness's
      // own mode alone reads as "off" for an agent tclaude's sandbox wraps.
      const sb = morph?.sandbox;
      if (sb) {
        rows.push([sb.unsandboxed ? '⚠' : '✓', 'tclaude sandbox', sb.tclaude]);
        rows.push(['↻', 'Harness sandbox', sb.harness]);
        if (sb.warning) rows.push(['⚠', 'Unsandboxed', sb.warning]);
      } else if (morph?.after?.sandbox) rows.push(['↻', 'Harness sandbox', `${morph.after.harness ? `${morph.after.harness} ` : ''}${morph.after.sandbox}`]);
      else rows.push(['✓', 'Sandbox', 'unchanged']);
      return rows;
    }
    case 'home': return [
      ['✓', 'Identity', `the same agent_id again on ${home}: its inbox, groups and permissions there`],
      ['✓', 'History', 'its history from the trip comes with it'],
      ['↻', 'Here', `${node} retires its visiting copy once ${home} confirms it is running`],
    ];
    default: return [];
  }
}

function errText(e) { return e?.body?.error || e?.message || String(e); }

// nodeApi is the dialog's I/O on another node (nodeFetch).
function nodeApi(node) { return createLaunchTargetApi({ fetchImpl: nodeFetch(node) }); }

// createLaunchTargetApi is the dialog's I/O. Errors carry status, code and
// the parsed body, as the federation actions' errors do.
export function createLaunchTargetApi({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(method, url, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
    const res = await fetchImpl(url, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) {
      const e = new Error(data?.error || `${res.status}`);
      e.status = res.status; e.code = data?.code || ''; e.body = data;
      throw e;
    }
    return data;
  }
  const agent = (id, tail) => `/api/agents/${encodeURIComponent(id)}/${tail}`;
  return Object.freeze({
    status: () => call('GET', '/api/federation/status'),
    profiles: async () => (await call('GET', '/api/spawn-profiles')) || [],
    morph: (id, body) => call('POST', agent(id, 'morph'), body),
    morphStatus: (id) => call('GET', agent(id, 'morph')),
    teleport: (id, body) => call('POST', agent(id, 'teleport'), body),
    // Model access for a teleport: what the receiving node accepts from this
    // node, this node's own gateways, and the gateways peers share with it.
    teleportCredentials: (node, group) => call('GET', `/api/peer/${encodeURIComponent(node)}/teleport-credentials?group=${encodeURIComponent(group)}`),
    models: () => call('GET', '/api/federation/models/control'),
    sharesReceived: () => call('GET', '/api/federation/models/shares/received'),
    // This operator's own Claude subscription: whether a token is stored, and
    // sending it for a teleport offer (this node's peer proxy adds the token).
    claudeSubscription: () => call('GET', '/api/federation/claude-subscription'),
    teleportSubscription: (node, offer) => call('POST', `/api/peer/${encodeURIComponent(node)}/teleport-subscription`, { offer, claude_subscription: true }),
    // recall asks where an away agent runs to send it straight home; the
    // home group defaults to its first group here.
    recall: (id) => call('POST', `/api/federation/agents/${encodeURIComponent(id)}/recall`, {}),
  });
}

function Carries({ rows, title }) {
  if (!rows.length) return null;
  return html`<div class="lt-carries" data-lt-carries="">
    <div class="lt-carries-title">${title}</div>
    ${rows.map(([mark, label, text]) => html`<div key=${label} class="lt-carry"><span class=${`lt-mark lt-mark-${mark === '✓' ? 'ok' : mark === '✗' ? 'no' : 'chg'}`}>${mark}</span><span class="lt-carry-k">${label}</span><span class="lt-carry-v">${text}</span></div>`)}
  </div>`;
}

// NodePicker lists every node, greying the ones this mode cannot use with
// the reason beside them.
function NodePicker({ targets, value, onPick, locked }) {
  return html`<div class="lt-nodes" role="radiogroup" aria-label="Target node">
    ${targets.map((n) => html`<button key=${n.id} type="button" role="radio" aria-checked=${n.id === value ? 'true' : 'false'}
      class=${`lt-node${n.id === value ? ' sel' : ''}`} disabled=${!n.ok || locked} data-lt-node=${n.id}
      title=${n.ok ? `Send it to ${n.plain}` : n.why} onClick=${() => onPick(n.id)}>
      <i style=${n.color ? `--nc:${n.color}` : undefined}></i><span class="lt-node-name">${n.name}</span><span class="lt-node-why">${n.ok ? '' : n.why}</span></button>`)}
  </div>`;
}

function GroupPicker({ groups, value, onChange, idPrefix, disabled }) {
  const [typed, setTyped] = useState(false);
  // A dropped group the catalog does not list stays as typed text, so what
  // is shown is what is sent.
  if (groups.length && !typed && (!value || groups.includes(value))) {
    return html`<select id=${`${idPrefix}-group`} value=${value} disabled=${disabled} onChange=${(e) => { if (e.currentTarget.value === '\u0000other') { setTyped(true); onChange(''); } else onChange(e.currentTarget.value); }}>
      <option value="">pick a receiving group…</option>
      ${groups.map((g) => html`<option key=${g} value=${g} selected=${value === g}>${g}</option>`)}
      <option value=${'\u0000other'}>another group (type its name)…</option></select>`;
  }
  return html`<input id=${`${idPrefix}-group`} value=${value} disabled=${disabled} placeholder="receiving group there (it must grant agents.receive)" autocomplete="off" onInput=${(e) => onChange(e.currentTarget.value)} />`;
}

// MorphPanel picks a spawn profile, previews the change with a dry run and
// applies it at the agent's next idle turn (or now).
function MorphPanel({ agent, api, onClose, setCarry, okRef, onPhase, toast = shellToast }) {
  const [profiles, setProfiles] = useState(null);
  const [profile, setProfile] = useState('');
  const [now, setNow] = useState(false);
  const [confirmCut, setConfirmCut] = useState(false);
  const [proceedWithoutSummary, setProceedWithoutSummary] = useState(false);
  const [waiting, setWaiting] = useState(null);
  const [remaining, setRemaining] = useState(0);
  const [preview, setPreview] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => { onPhase?.(busy ? 'busy' : 'confirm'); }, [busy]);
  useEffect(() => {
    let live = true;
    api.profiles().then((list) => { if (live) setProfiles((list || []).filter((p) => p?.name && !p.disabled)); })
      .catch((e) => { if (live) { setProfiles([]); setError(`Could not load spawn profiles: ${errText(e)}`); } });
    return () => { live = false; };
  }, []);
  useEffect(() => {
    let live = true;
    api.morphStatus?.(agent.id).then((status) => {
      if (live && status?.pending_morph?.history === 'summary') { setWaiting(status.pending_morph); if(status.pending_morph.target?.profile) setProfile(status.pending_morph.target.profile); }
    }).catch(() => {});
    return () => { live = false; };
  }, []);
  useEffect(() => {
    setPreview(null); setCarry(null);
    if (!profile) return undefined;
    let live = true;
    setError('');
    api.morph(agent.conv || agent.id, { profile, dry_run: true })
      .then((r) => { if (live) { setPreview(r); setCarry(r); } })
      .catch((e) => { if (live) setError(errText(e)); });
    return () => { live = false; };
  }, [profile]);

  useEffect(() => {
    if (!waiting) return undefined;
    let live = true, timer;
    const tick = async () => {
      if (!live) return;
      setRemaining(Math.max(0, Math.ceil((Date.parse(waiting.expires_at) - Date.now())/1000)));
      try {
        const status = await api.morphStatus(agent.id);
        if (!live) return;
        if (!status?.pending_morph) { setWaiting(null); if(status?.conv_id !== agent.conv) {setPreview(null); setError('Morph completed. Close to see its new terminal.');} else setError('The wait ended; the source is unchanged. You can explicitly continue without a summary below.'); return; }
        setWaiting(status.pending_morph);
      } catch (e) { if (live) setError(`Could not refresh pending morph: ${errText(e)}`); }
      if (live) timer = setTimeout(tick, 2000);
    };
    timer = setTimeout(tick, 0);
    return () => { live = false; clearTimeout(timer); };
  }, [!!waiting]);
  const cancelPending = async () => {
    setBusy(true);
    try { await api.morph(agent.id, { cancel: true }); setWaiting(null); toast('Pending morph cancelled; the source stays here. You can explicitly continue without a summary.'); }
    catch (e) { setError(errText(e)); }
    finally { setBusy(false); }
  };
  const continueWithoutSummary = async () => {
    const targetProfile = profile || waiting?.target?.profile;
    if (!proceedWithoutSummary || !targetProfile || busy) return;
    setBusy(true); setError('');
    try {
      await api.morph(agent.id, {profile:targetProfile, proceed_without_summary:true, now:true});
      toast('Morphing with readable history only; the full transcript is saved.'); onClose();
    } catch(e) { setError(errText(e)); }
    finally { setBusy(false); }
  };
  const apply = async () => {
    if (!profile || !preview || busy || (preview.history?.needs_summary && !confirmCut && !proceedWithoutSummary)) return;
    setBusy(true); setError('');
    let done;
    try { done = await api.morph(agent.conv || agent.id, { profile, ...(now && !preview.history?.needs_summary && !preview.history?.needs_handoff ? { now: true } : {}), ...(confirmCut ? { confirm_history_loss: true } : {}), ...(proceedWithoutSummary ? { proceed_without_summary: true } : {}) }); } catch (e) { setError(errText(e)); setBusy(false); return; }
    if (done?.result === 'pending' && done?.pending_morph?.history === 'summary') {
      setWaiting(done.pending_morph); setRemaining(Math.max(0,Math.ceil((Date.parse(done.pending_morph.expires_at)-Date.now())/1000))); setBusy(false); return;
    }
    // Close on success so the operator sees the agent (and, from Terminals,
    // its new terminal); the toast keeps the result.
    toast(`${done?.result === 'pending'
      ? `${agent.name} morphs to ${profile} when it next goes idle (within 30 minutes, or the request lapses). Its row shows the pending morph.`
      : `${agent.name} is now ${profile}. It got a short inbox note saying what changed.`}${done?.detail ? ` ${done.detail}` : ''}`);
    onClose();
  };
  return html`
    ${waiting && html`<div role="status" data-lt-state="waiting-summary">Waiting for ${agent.name}'s history summary · ${Math.floor(remaining/60)}m ${remaining%60}s left. Timeout leaves it unchanged.<button type="button" onClick=${cancelPending} disabled=${busy}>Cancel pending morph</button><button type="button" onClick=${onClose}>Close</button></div>`}
    ${(waiting || preview?.history?.needs_summary || preview?.history?.needs_handoff) && html`<label key="loss-fallback" class="lt-check"><input id="lt-proceed-without-summary" type="checkbox" checked=${proceedWithoutSummary} onChange=${(e) => setProceedWithoutSummary(e.currentTarget.checked)} /> Source cannot answer (for example, provider outage): continue with readable history and the full-transcript pointer. Earlier context may be lost.</label>`}
    ${waiting && html`<button type="button" id="lt-proceed-loss" disabled=${!proceedWithoutSummary || busy || !(profile || waiting.target?.profile)} onClick=${continueWithoutSummary}>Continue now without a summary</button>`}
    <label class="lt-row"><span class="lt-k">Stays on</span><span class="muted">this node · ${agent.group || 'its groups'}</span></label>
    <label class="lt-row"><span class="lt-k">Profile</span>${profiles === null ? html`<span class="muted">loading…</span>` : html`<select id="lt-morph-profile" value=${profile} disabled=${busy || !!waiting} onChange=${(e) => { setConfirmCut(false); setProceedWithoutSummary(false); setProfile(e.currentTarget.value); }}>
      <option value="">pick a spawn profile…</option>
      ${profiles.map((p) => html`<option key=${p.name} value=${p.name}>${p.name}${p.harness || p.model ? ` — ${[p.harness, p.model, p.effort && `effort ${p.effort}`].filter(Boolean).join(' · ')}` : ''}</option>`)}</select>`}</label>
    ${preview && html`<div class="lt-morph-preview"><div><span class="lt-k">was</span> ${formLine(preview.before)}</div><div><span class="lt-k">now</span> ${formLine(preview.after)}</div></div>`}
    ${preview?.history?.needs_handoff && html`<div role="status">Compaction summary unreadable. Ask ${agent.name} for a handoff, carried beside its readable tail. If the source cannot answer, explicitly confirm readable-history-only continuation below.</div>`}
    ${preview?.history?.budget_warning && html`<div role="status">${preview.history.kept_percent}% kept · ${preview.history.dropped_percent}% dropped from inline history. ${preview.history.selection_reason}</div>`}
    ${preview?.history?.needs_summary && html`<label key="summary-confirm" class="lt-check"><input id="lt-confirm-cut" type="checkbox" checked=${confirmCut} disabled=${!!waiting} onChange=${(e) => setConfirmCut(e.currentTarget.checked)} /> Drop ${preview.history.dropped_percent}% of inline history, carry a source-written summary. Original task and latest instructions stay available.</label>`}
    <label class="lt-check"><input id="lt-morph-now" type="checkbox" checked=${now} disabled=${!!waiting || preview?.history?.needs_summary || preview?.history?.needs_handoff} onChange=${(e) => setNow(e.currentTarget.checked)} /> now: stop it even if it is busy, and resume it from its last saved history</label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" id="lt-cancel" onClick=${onClose}>Cancel</button>
      <button type="button" ref=${okRef} class="primary" id="lt-ok" disabled=${!preview || busy || !!waiting || (preview?.history?.needs_summary && !confirmCut && !proceedWithoutSummary)} onClick=${apply}>${busy ? 'Morphing…' : preview?.history?.needs_handoff ? 'Request handoff and morph' : preview?.history?.needs_summary ? 'Request summary and morph' : now ? 'Morph now' : 'Morph at next idle turn'}</button></div>`;
}

export function formLine(v) {
  if (!v) return '';
  return [v.harness, v.model || 'default model', v.effort && `effort ${v.effort}`, v.approval && `approval ${v.approval}`, v.profile && `profile ${v.profile}`].filter(Boolean).join(' · ');
}

// followMove polls a move record until its copy runs there (or fails, or two
// minutes pass), reporting each outcome to onProgress. It is not tied to the
// dialog, which closes as soon as the move is sent.
export function followMove({ id, actions, timers = globalThis, now = () => Date.now(), onProgress }) {
  const started = now();
  const tick = async () => {
    let rec;
    try { rec = await actions.moveDetail(id); } catch (e) { onProgress({ phase: 'unknown', error: errText(e) }); return; }
    const p = moveProgress(rec);
    if (p.phase === 'checking' && now() - started < 120000) timers.setTimeout(tick, MOVE_POLL_MS);
    else onProgress(p.phase === 'checking' ? { phase: 'slow' } : p);
  };
  timers.setTimeout(tick, MOVE_POLL_MS);
}

// useModelAccess loads the Model access choices for a teleport or clone:
// for an agent sent from here, what the receiving node accepts and this
// node's own gateways; for one pulled here from a node that trusts this one
// unrestricted, the gateways peers share with this node. null hides the row
// (a recall, a pull this node may not choose for, or nothing to load).
function useModelAccess({ mode, agent, target, api, pull, acting }) {
  const [options, setOptions] = useState(null);
  useEffect(() => {
    if (mode === 'home' || !target?.node || !target?.group || (pull && !acting)) return undefined;
    let live = true;
    const dest = pull ? 'this node' : target.name;
    const settle = (p) => p.then((v) => v, () => null);
    const loads = pull
      ? [Promise.resolve(null), Promise.resolve(null), settle(api.sharesReceived?.() || Promise.reject())]
      : [settle(api.teleportCredentials?.(target.node, target.group) || Promise.reject()), settle(api.models?.() || Promise.reject()), Promise.resolve(null)];
    loads.push(pull ? Promise.resolve(null) : settle(api.claudeSubscription?.() || Promise.reject()));
    // This node's trust of the other node: a pull's source is held to this
    // node's landing policy, and a lease to a push's receiver needs a grant.
    const other = pull ? agent.node : target.node;
    loads.push(settle(api.status?.() || Promise.reject()));
    Promise.all(loads).then(([creds, control, received, subscription, status]) => {
      if (!live) return;
      setOptions(modelAccessOptions({
        dest, harness: agent.harness || '', creds, pull,
        own: pull ? [] : ownGateways(control),
        shares: pull ? (received?.shares || []) : (creds?.shares || []),
        trust: (status?.peers || []).find((p) => p.instance_id === other)?.level || '',
        source: agent.node, sourceName: agent.nodeName,
        selfIds: [status?.instance_id, status?.name], subscription,
      }));
    });
    return () => { live = false; };
  }, [mode, target?.node, target?.group, pull, acting]);
  return options;
}

// TeleportPanel sends it (Teleport, Clone to a peer, Recall home), then closes;
// followMove reports how the move ends.
// pullActions, set for a peer's agent, asks that peer to send it here.
function TeleportPanel({ mode, agent, target, home, api, actions, timers, now, onClose, openPage, okRef, viaPeer, pullActions = null, acting = false, onPhase, toast = shellToast }) {
  const [note, setNote] = useState('');
  const [access, setAccess] = useState('');
  const accessOptions = useModelAccess({ mode, agent, target, api, pull: !!pullActions, acting });
  const accessPick = accessOptions?.find((o) => o.value === access && !o.reason) ? access : '';
  const credentials = accessPick ? { credentials: accessPick } : {};
  const [phase, setPhase] = useState('confirm');
  useEffect(() => { onPhase?.(phase); }, [phase]);
  const [error, setError] = useState('');
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);
  const send = async () => {
    if (phase !== 'confirm') return;
    setPhase('sending'); setError('');
    try {
      let res;
      if (mode === 'home' && viaPeer) res = await api.recall(agent.id);
      else if (pullActions) res = await pullActions.teleport(agent.id, { group: target.group, ...(mode === 'clone' ? { clone: true } : {}), ...(note ? { note } : {}), ...credentials });
      else res = await api.teleport(agent.conv || agent.id, mode === 'home'
        ? { home: true, ...(note ? { note } : {}) }
        : { peer: target.node, group: target.group, ...(mode === 'clone' ? { clone: true } : {}), ...(note ? { note } : {}), ...credentials });
      // On your own Claude subscription the offer names only the mode: send
      // the token for it now. Without it the agent lands offline there, for
      // you to send it from its ⚙ menu.
      if (accessPick === 'claude-subscription' && res?.move_id && !pullActions) {
        try { await api.teleportSubscription(target.node, res.move_id); } catch (e) {
          toast(`${agent.name} goes on your Claude subscription, but sending it to ${target.name} failed: ${errText(e)}. It lands offline there; send it from its ⚙ menu.`, true);
        }
      }
      // Close once it is sent; the outcome arrives as toasts, even when the
      // dialog was cancelled while sending. A pulled move's record lives on
      // the peer, so there is none here to follow. A return to a
      // paused home backup answers 202 {state: "returning"} with no move
      // record: its home presence is what to watch then.
      // The receiving group carries an "arriving" marker (drop-landing.js)
      // until the copy runs there; a recall goes home, to no group here.
      const transit = mode !== 'home' && res?.state !== 'returning' && res?.teleport_state !== 'offered'
        ? markInTransit({ group: target.group, node: target.node, ids: [agent.id], from: agent.nodeName || '', label: agent.name, newRow: mode === 'clone' && !!pullActions })
        : null;
      if (res?.move_id && !viaPeer && !pullActions) {
        toast(`${verb}: sent. Waiting for ${dest} to run its copy…`);
        followMove({ id: res.move_id, actions, timers, now, onProgress: (p) => {
          if (p.phase === 'landed') transit?.land([p.agent]);
          else if (p.phase === 'failed') transit?.fail();
          else if (p.phase === 'waiting') transit?.cancel();
          const t = progressToast(p); if (t) toast(t.text, t.error);
        } });
      } else {
        const t = progressToast({ phase: res?.state === 'returning' ? 'returning' : res?.teleport_state === 'offered' ? 'offered' : 'sent' });
        toast(t.text);
      }
      onClose();
    } catch (e) { if (alive.current) { setPhase('confirm'); setError(errText(e)); } else toast(`${verb} failed: ${errText(e)}`, true); }
  };
  // progressToast words a move outcome for a toast once the dialog is gone.
  const progressToast = (p) => p.phase === 'landed' ? { text: `✅ ${agent.name} runs on ${dest}${p.agent ? ` as ${p.agent}` : ''}.${mode === 'clone' ? '' : ' Its copy here is being retired.'}` }
    : p.phase === 'waiting' ? { text: `⏸ ${agent.name} waits for ${dest} to accept it; it keeps running here until then.` }
    : p.phase === 'failed' ? { text: `${verb} ${p.state}${p.error ? `: ${p.error}` : ''}. ${agent.name} is still here.`, error: true }
    : p.phase === 'returning' ? { text: `${agent.name} is returning to its paused copy on ${home || 'its home'}; its home row shows it back when it runs.` }
    : p.phase === 'offered' && pullActions ? { text: `${agent.nodeName} offered ${agent.name}'s copy to this node: it starts here once this node takes it in (⚙ Fleet → Offers).` }
    : p.phase === 'offered' ? { text: `Offered to ${dest}: the copy starts there once ${dest} takes it in (${dest}'s ⚙ Fleet → Offers).` }
    : p.phase === 'sent' && pullActions ? { text: `${verb}: requested from ${agent.nodeName}. ${agent.name} shows up in ${target.group} here once its copy runs; if it waits for acceptance, see ⚙ Fleet → Offers.` }
    : p.phase === 'sent' ? { text: `${verb}: requested. Its row here turns back into ${agent.name} when it arrives.` }
    : p.phase === 'checking' ? null
    : { text: `${verb} is still in progress; follow it in ⚙ Fleet → Moves.` };
  const dest = mode === 'home' ? home : pullActions ? 'this node' : target.name;
  const from = pullActions ? `on ${agent.nodeName}` : 'here';
  const verb = mode === 'home' ? (viaPeer ? 'Recall home' : 'Send home') : mode === 'clone' ? `Clone to ${target.name}` : `Teleport to ${target.name}`;
  return html`
    ${html`<p class="lt-consequence">${mode === 'home'
      ? `${agent.name} goes straight home to ${home}, not back through the nodes it visited. It leaves at once: it keeps running ${viaPeer ? `on ${agent.nodeName}` : 'here'} until its copy runs at home, then that copy is retired.`
      : mode === 'clone'
        ? `Sends a copy of ${agent.name} with its full conversation history (it may hold code or pasted secrets) to ${target.group} on ${dest}. The original keeps running ${from}.`
        : `Sends ${agent.name} with its full conversation history (it may hold code or pasted secrets) to ${target.group} on ${dest}. It leaves at once: it keeps running ${from} until its copy runs ${pullActions ? 'here' : 'there'}, then stops ${pullActions ? 'there' : 'here'}; its groups keep a placeholder row.`}</p>
      ${accessOptions && html`<label class="lt-row" title="Whose provider account the agent's model calls use where it lands: that node's own login, or a model gateway. Your own gateway is paid by you through a lease."><span class="lt-k">Model access</span>
        <select id="lt-model-access" value=${accessPick} disabled=${phase !== 'confirm'} onChange=${(e) => setAccess(e.currentTarget.value)}>
          ${accessOptions.map((o) => html`<option key=${o.value} value=${o.value} disabled=${!!o.reason} title=${o.reason}>${o.label}${o.reason ? ` — ${o.reason}` : ''}</option>`)}
        </select></label>`}
      ${viaPeer ? '' : html`<label class="lt-row"><span class="lt-k">Note</span><textarea id="lt-note" rows="2" placeholder="Optional: told to the agent on arrival" value=${note} onInput=${(e) => setNote(e.currentTarget.value)}></textarea></label>`}`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" id="lt-cancel" onClick=${onClose}>Cancel</button>
      <button type="button" ref=${okRef} class="primary" id="lt-ok" disabled=${phase === 'sending' || (mode !== 'home' && (!target?.node || !target?.group))} onClick=${send}>${phase === 'sending' ? 'Sending…' : verb}</button>
    </div>`;
}

// LaunchTargetDialog: agent {id, conv, name, node, nodeName, group,
// presence, homeName}; self and fleet describe the nodes; target pre-fills a
// drop ({node, name, group}). peerView is the agent's node's peer_view
// metadata, which says what that peer lets this node pull.
export function LaunchTargetDialog({
  agent, self, fleet, initialMode = 'move', target: initialTarget = null, fromDrop = false, peerView = null,
  api = createLaunchTargetApi(), actions, peerActions, localGroups = [], relay = false, nodeGroups = {}, onClose, onLocalClone,
  timers = globalThis, now = () => Date.now(), openPage = openFleetPage, teleport = TELEPORT_ROUTES, toast = shellToast,
}) {
  const acting = agent.node !== self.id && unrestrictedOn(agent.node);
  // A morph on another node runs through that node's routes.
  // (Built once acting holds: before the node's snapshot loads, Morph stays
  // disabled, and nothing goes to this node under the agent's ID.)
  const morphApi = useRef(null);
  if (acting && !morphApi.current) morphApi.current = nodeApi(agent.node);
  // A peer's agent is pulled here through that peer's action routes.
  const pull = agent.node !== self.id && !!peerActions;
  const missing = agent.node === self.id ? [] : missingPullGrants(peerView, agent.id);
  const firstCtx = { local: agent.node === self.id, acting, presence: agent.presence || '', homeIsSelf: agent.homeNode === self.id, source: agent.nodeName || self.name, teleport, pull, missing };
  // A mode this agent cannot use here never opens: fall back to the first
  // one it can (Move always can).
  const [mode, setMode] = useState(modeState(initialMode, firstCtx).ok ? initialMode : (MODES.find((m) => modeState(m.key, firstCtx).ok)?.key || 'move'));
  // locked freezes the mode, node and group once a panel has left its
  // confirm step, so a second send cannot start from the same dialog.
  const [locked, setLocked] = useState(false);
  const onPhase = (phase) => setLocked(phase !== 'confirm');
  const [node, setNode] = useState(initialTarget?.node || '');
  const [group, setGroup] = useState(initialTarget?.group || '');
  const [status, setStatus] = useState(null);
  const [carry, setCarry] = useState(null);
  const okRef = useRef(null);
  const local = agent.node === self.id;
  const source = { node: agent.node, name: agent.nodeName || self.name, group: agent.group };
  const ctx = { local, acting, presence: agent.presence || '', homeIsSelf: agent.homeNode === self.id, source: source.name, teleport, pull, missing };
  useEffect(() => {
    let live = true;
    if (local) api.status().then((s) => { if (live) setStatus(s); }).catch(() => { if (live) setStatus({}); });
    return () => { live = false; };
  }, []);
  const needsNode = mode === 'move' || mode === 'teleport' || mode === 'clone';
  const targets = needsNode ? nodeTargets({ mode, fleet, self, source, status, localGroups, teleport, relay, nodeGroups, pull, missing }) : [];
  const picked = targets.find((n) => n.id === node && n.ok) || null;
  const pick = (id) => { setNode(id); if (id !== node) setGroup(''); };
  const switchMode = (key) => { if (locked) return; setMode(key); setCarry(null); };
  const targetRef = picked ? { node: picked.id, name: picked.plain, group: group.trim() } : null;
  const plan = targetRef && targetRef.group ? dropPlan({ source, target: targetRef, self: self.id }) : null;
  const titleVerb = { move: 'Move', teleport: 'Teleport', clone: 'Clone', morph: 'Morph', home: 'Recall' }[mode];
  const recall = !local || agent.presence === 'away';
  const title = mode === 'home' ? `⌂ ${recall ? 'Recall' : 'Send'} ${agent.name} home to ${agent.homeName || 'its home'}?` : `${titleVerb} ${agent.name}${mode === 'morph' ? '…' : ' to…'}`;
  const cloneHere = mode === 'clone' && picked?.id === source.node;

  let body;
  if (mode === 'morph') body = html`<${MorphPanel} key="morph" agent=${agent} api=${acting ? morphApi.current : api} toast=${toast} onClose=${onClose} setCarry=${setCarry} okRef=${okRef} onPhase=${onPhase} />`;
  else if (mode === 'home') body = html`<${TeleportPanel} key="home" mode="home" agent=${agent} home=${agent.homeName || 'its home'} viaPeer=${!local || agent.presence === 'away'} api=${api} actions=${actions} timers=${timers} now=${now} toast=${toast} onClose=${onClose} openPage=${openPage} okRef=${okRef} onPhase=${onPhase} />`;
  else if (cloneHere) body = html`<p class="lt-consequence">A sibling of ${agent.name} starts on this node. The next step picks its working directory or worktree, its first message, and whether it copies the conversation.</p>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" id="lt-cancel" onClick=${onClose}>Cancel</button>
      <button type="button" ref=${okRef} class="primary" id="lt-ok" onClick=${() => onLocalClone?.()}>Clone here…</button></div>`;
  else if (mode === 'move' && plan && (plan.kind === 'push' || plan.kind === 'pull' || (plan.kind === 'third' && relay))) body = html`<${MovePanel} key=${`${targetRef.node}/${targetRef.group}`} drop=${{ agent: { id: agent.id, name: agent.name }, source, target: targetRef }} plan=${plan} actions=${actions} peerActions=${peerActions} timers=${timers} now=${now} onClose=${onClose} openPage=${openPage} okRef=${okRef} onPhase=${onPhase} />`;
  else if ((mode === 'teleport' || mode === 'clone') && targetRef?.group) body = html`<${TeleportPanel} key=${`${mode}/${targetRef.node}/${targetRef.group}`} mode=${mode} agent=${agent} target=${targetRef} api=${api} actions=${actions} timers=${timers} now=${now} toast=${toast} onClose=${onClose} openPage=${openPage} okRef=${okRef} onPhase=${onPhase}
    pullActions=${local ? null : peerActions(source.node)} acting=${acting} />`;
  else body = html`<div class="muted lt-hint">${picked ? 'Pick the receiving group there.' : 'Pick the node to send it to.'}</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" ref=${okRef} id="lt-cancel" onClick=${onClose}>Cancel</button></div>`;

  const enter = () => { const b = okRef.current; if (b && !b.disabled) b.click(); };
  return html`<${Overlay} id="launch-target" labelledby="launch-target-title" onClose=${onClose} initialFocusRef=${okRef} onSubmitEnter=${enter}>
    <h3 id="launch-target-title">${title}</h3>
    <div class="lt-modes" role="tablist" aria-label="What to do">
      ${MODES.map((m) => { const st = modeState(m.key, ctx); return html`<button key=${m.key} type="button" role="tab" aria-selected=${m.key === mode ? 'true' : 'false'}
        class=${`lt-mode${m.key === mode ? ' sel' : ''}`} disabled=${!st.ok || (locked && m.key !== mode)} title=${st.ok ? m.hint : st.why} data-lt-mode=${m.key} onClick=${() => switchMode(m.key)}>${m.label}</button>`; })}
    </div>
    <div class="muted lt-mode-hint" data-lt-hint="">${MODES.find((m) => m.key === mode)?.hint}</div>
    ${fromDrop && html`<div class="muted lt-drop-note">↓ Opened by dragging ${agent.name} onto ${initialTarget?.group}@${initialTarget?.name}: node and group are pre-filled; pick another mode above to teleport or clone it instead.</div>`}
    <div class="lt-body">
      <div class="lt-fields">
        ${needsNode && html`<div class="lt-row"><span class="lt-k">Target node</span><${NodePicker} targets=${targets} value=${node} onPick=${pick} locked=${locked} /></div>`}
        ${needsNode && picked && !cloneHere && html`<label class="lt-row"><span class="lt-k">Group</span><${GroupPicker} key=${picked.id} groups=${picked.groups} value=${group} onChange=${setGroup} idPrefix="lt" disabled=${locked} /></label>`}
        ${body}
      </div>
      <${Carries} title=${`What carries over${picked ? ` to ${picked.plain}` : ''}`} rows=${carries(mode, { node: picked?.plain, home: agent.homeName, morph: carry, local: cloneHere || mode !== 'clone' })} />
    </div>
    <div class="muted fa-cli-note">Enter confirms · Esc cancels · CLI: <code>${cliHint(mode)}</code></div>
  </${Overlay}>`;
}

function cliHint(mode) {
  return {
    move: 'tclaude federation move-agent AGENT PEER --group …',
    teleport: 'tclaude agent teleport --peer … --group …',
    clone: 'tclaude agent clone AGENT',
    morph: 'tclaude agent morph AGENT --profile …',
    home: 'tclaude agent teleport --home',
  }[mode];
}

// launchAgent builds the dialog's agent from a ⚙ "Send elsewhere" item's
// data attributes and the node its row belongs to.
export function launchAgent(data, node) {
  return {
    id: data.agent || data.conv || '', conv: data.conv || '', name: data.label || data.agent || '',
    node: node.id || '', nodeName: node.name || '', group: data.ownGroup || data.group || '',
    presence: data.presence || '', homeNode: data.homeNode || '', homeName: data.homeName || '', cwd: data.cwd || '',
    harness: data.harness || '',
  };
}
