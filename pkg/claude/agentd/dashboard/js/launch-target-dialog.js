// launch-target-dialog.js — one dialog for sending an agent elsewhere
// (tcl-o9d5d9): Move, Teleport, Clone, Morph and Recall home. The ⚙ menu's
// "Send elsewhere" items open it on a mode; a drop in "Groups · all nodes"
// opens it on Move with the node and group pre-filled. Each mode shows only
// the fields it needs, says what carries over, and lists unavailable targets
// with the reason instead of hiding them.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { MovePanel, dropPlan, moveProgress, MOVE_POLL_MS, openFleetPage } from './skynet-move-drop.js';

const html = htm.bind(h);

// TELEPORT_ROUTES gates Teleport, Clone to a peer and Recall home on the
// operator teleport routes (POST /api/agents/{id}/teleport and the recall
// route's home return, #2883). Off, the modes stay visible but greyed with the
// reason, and out of the ⚙ menu; it stays a switch for tests and rollbacks.
export const TELEPORT_ROUTES = true;

export const MODES = Object.freeze([
  { key: 'move', label: 'Move' },
  { key: 'teleport', label: 'Teleport' },
  { key: 'clone', label: 'Clone' },
  { key: 'morph', label: 'Morph' },
  { key: 'home', label: 'Recall home' },
]);

const NOT_YET = 'Needs the operator teleport routes, which this dashboard does not have yet.';

// modeState says whether a mode can be used for this agent from this
// dashboard. local: the agent runs on this node. presence: its presenceKind.
export function modeState(mode, { local, presence = '', homeIsSelf = false, source = 'its node', teleport = TELEPORT_ROUTES }) {
  const there = `Do this from ${source}'s dashboard.`;
  switch (mode) {
    case 'move': return { ok: true };
    case 'clone':
    case 'morph': return local ? { ok: true } : { ok: false, why: there };
    case 'teleport':
      if (!teleport) return { ok: false, why: NOT_YET };
      return local ? { ok: true } : { ok: false, why: there };
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

// receivingGroups lists a peer's catalog groups that take agents from this
// node, from the federation status (status.remote[].groups[].caps).
export function receivingGroups(status, peer, mode) {
  const remote = (Array.isArray(status?.remote) ? status.remote : []).find((r) => r?.peer === peer);
  const caps = mode === 'move' ? ['agents_receive'] : ['agents_receive', 'teleport_receive'];
  return (remote?.groups || []).filter((g) => g?.name && (g.caps || []).some((c) => caps.includes(c))).map((g) => g.name).sort();
}

// nodeTargets lists every node with whether this mode may send the agent
// there, and why not. self is this node; source is where the agent runs.
export function nodeTargets({ mode, fleet, self, source, status, localGroups = [], teleport = TELEPORT_ROUTES }) {
  const nodes = [self, ...(fleet?.peers || [])].filter((n) => n?.id);
  return nodes.map((n) => {
    const here = n.id === source.node;
    const isSelf = n.id === self.id;
    const row = { id: n.id, name: isSelf ? `${n.name} (this node)` : n.name, plain: n.name, color: n.color, local: isSelf, ok: true, why: '', groups: [] };
    if (here && mode === 'clone' && !isSelf) return { ...row, ok: false, why: `clone it from ${source.name}'s dashboard` };
    if (here) return { ...row, ok: mode === 'clone', why: mode === 'clone' ? '' : 'it is already here — drag it onto another group to move it within this node', groups: [] };
    if (!isSelf && n.online === false) return { ...row, ok: false, why: 'offline' };
    if (source.node !== self.id) {
      // A peer's agent can only be pulled here, and only by a move.
      if (!isSelf || mode !== 'move') return { ...row, ok: false, why: `do this from ${source.name}'s dashboard` };
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
      ['↻', 'Identity', 'a new sibling that inherits its groups, permissions and ownership'],
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
        rows.push(['≈', 'History', `translated ${hist.source} → ${hist.target}: ${hist.carried_entries ?? '?'} of ${hist.source_entries ?? '?'} entries carried${hist.carried_estimated_tokens > 0 ? `; estimated tokens: ${hist.source_estimated_tokens ?? '?'} source → ${hist.carried_estimated_tokens} carried` : ''}${lost ? ` (${lost})` : ''}`]);
      } else rows.push(['✓', 'History', 'kept: it resumes the same conversation']);
      rows.push(morph?.after?.sandbox ? ['↻', 'Sandbox', `${morph.after.sandbox} (the new harness's own mode)`] : ['✗', 'Sandbox', 'unchanged (operator-only)']);
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
    teleport: (id, body) => call('POST', agent(id, 'teleport'), body),
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
function MorphPanel({ agent, api, onClose, setCarry, okRef, onPhase }) {
  const [profiles, setProfiles] = useState(null);
  const [profile, setProfile] = useState('');
  const [now, setNow] = useState(false);
  const [preview, setPreview] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(null);
  useEffect(() => { onPhase?.(busy || done ? 'busy' : 'confirm'); }, [busy, done]);
  useEffect(() => {
    let live = true;
    api.profiles().then((list) => { if (live) setProfiles((list || []).filter((p) => p?.name && !p.disabled)); })
      .catch((e) => { if (live) { setProfiles([]); setError(`Could not load spawn profiles: ${errText(e)}`); } });
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
  const apply = async () => {
    if (!profile || !preview || busy) return;
    setBusy(true); setError('');
    try { setDone(await api.morph(agent.conv || agent.id, { profile, ...(now ? { now: true } : {}) })); } catch (e) { setError(errText(e)); }
    setBusy(false);
  };
  if (done) {
    return html`<div role="status" data-lt-state=${done.result}>${done.result === 'pending'
      ? `${agent.name} morphs to ${profile} when it next goes idle (within 30 minutes, or the request lapses). Its row shows the pending morph.`
      : `${agent.name} is now ${profile}. It got a short inbox note saying what changed.`}${done.detail ? ` ${done.detail}` : ''}</div>
      <div class="modal-buttons"><span class="spacer"></span><button type="button" ref=${okRef} id="lt-close" onClick=${onClose}>Close</button></div>`;
  }
  return html`
    <label class="lt-row"><span class="lt-k">Stays on</span><span class="muted">this node · ${agent.group || 'its groups'}</span></label>
    <label class="lt-row"><span class="lt-k">Profile</span>${profiles === null ? html`<span class="muted">loading…</span>` : html`<select id="lt-morph-profile" value=${profile} disabled=${busy} onChange=${(e) => setProfile(e.currentTarget.value)}>
      <option value="">pick a spawn profile…</option>
      ${profiles.map((p) => html`<option key=${p.name} value=${p.name}>${p.name}${p.harness || p.model ? ` — ${[p.harness, p.model, p.effort && `effort ${p.effort}`].filter(Boolean).join(' · ')}` : ''}</option>`)}</select>`}</label>
    ${preview && html`<div class="lt-morph-preview"><div><span class="lt-k">was</span> ${formLine(preview.before)}</div><div><span class="lt-k">now</span> ${formLine(preview.after)}</div></div>`}
    <label class="lt-check"><input id="lt-morph-now" type="checkbox" checked=${now} onChange=${(e) => setNow(e.currentTarget.checked)} /> now: stop it even if it is busy, and resume it from its last saved history</label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span>
      <button type="button" id="lt-cancel" onClick=${onClose}>Cancel</button>
      <button type="button" ref=${okRef} class="primary" id="lt-ok" disabled=${!preview || busy} onClick=${apply}>${busy ? 'Morphing…' : now ? 'Morph now' : 'Morph at next idle turn'}</button></div>`;
}

export function formLine(v) {
  if (!v) return '';
  return [v.harness, v.model || 'default model', v.effort && `effort ${v.effort}`, v.approval && `approval ${v.approval}`, v.profile && `profile ${v.profile}`].filter(Boolean).join(' · ');
}

// TeleportPanel sends it (Teleport, Clone to a peer, Recall home) and
// follows the move record until its copy runs there.
function TeleportPanel({ mode, agent, target, home, api, actions, timers, now, onClose, openPage, okRef, viaPeer, onPhase }) {
  const [note, setNote] = useState('');
  const [phase, setPhase] = useState('confirm');
  useEffect(() => { onPhase?.(phase); }, [phase]);
  const [error, setError] = useState('');
  const [progress, setProgress] = useState(null);
  const alive = useRef(true);
  const timer = useRef(null);
  useEffect(() => () => { alive.current = false; if (timer.current) timers.clearTimeout(timer.current); }, []);
  const follow = (id) => {
    const started = now();
    const tick = async () => {
      timer.current = null;
      if (!alive.current) return;
      let rec;
      try { rec = await actions.moveDetail(id); } catch (e) { if (alive.current) setProgress({ phase: 'unknown', error: errText(e) }); return; }
      if (!alive.current) return;
      const p = moveProgress(rec);
      setProgress(p);
      if (p.phase === 'checking' && now() - started < 120000) timer.current = timers.setTimeout(tick, MOVE_POLL_MS);
      else if (p.phase === 'checking') setProgress({ phase: 'slow' });
    };
    timer.current = timers.setTimeout(tick, MOVE_POLL_MS);
  };
  const send = async () => {
    if (phase !== 'confirm') return;
    setPhase('sending'); setError('');
    try {
      let res;
      if (mode === 'home' && viaPeer) res = await api.recall(agent.id);
      else res = await api.teleport(agent.conv || agent.id, mode === 'home'
        ? { home: true, ...(note ? { note } : {}) }
        : { peer: target.node, group: target.group, ...(mode === 'clone' ? { clone: true } : {}), ...(note ? { note } : {}) });
      if (!alive.current) return;
      setPhase('progress');
      // A return to a paused home backup answers 202 {state: "returning"}
      // with no move record: its home presence is what to watch then.
      if (res?.move_id && !viaPeer) { setProgress({ phase: 'checking' }); follow(res.move_id); } else setProgress({ phase: res?.state === 'returning' ? 'returning' : res?.teleport_state === 'offered' ? 'offered' : 'sent' });
    } catch (e) { if (alive.current) { setPhase('confirm'); setError(errText(e)); } }
  };
  const dest = mode === 'home' ? home : target.name;
  const verb = mode === 'home' ? (viaPeer ? 'Recall home' : 'Send home') : mode === 'clone' ? `Clone to ${target.name}` : `Teleport to ${target.name}`;
  const status = !progress ? null
    : progress.phase === 'checking' ? html`<div class="muted" role="status">⏳ Sent. Waiting for ${dest} to run its copy…</div>`
    : progress.phase === 'landed' ? html`<div role="status" data-lt-state="landed">✅ ${agent.name} runs on ${dest}${progress.agent ? html` as <code>${progress.agent}</code>` : ''}.${mode === 'clone' ? '' : ' Its copy here is being retired.'}</div>`
    : progress.phase === 'waiting' ? html`<div role="status" data-lt-state="waiting">⏸ It waits for ${dest} to accept it. ${agent.name} keeps running here until then.</div>`
    : progress.phase === 'failed' ? html`<div class="fa-danger" role="alert" data-lt-state="failed">It ${progress.state}${progress.error ? `: ${progress.error}` : ''}. ${agent.name} is still here.</div>`
    : progress.phase === 'returning' ? html`<div role="status" data-lt-state="returning">${agent.name} is returning to its paused copy on ${home || 'its home'}, which resumes with the history from this trip. Its home row shows it back when it runs.</div>`
    : progress.phase === 'offered' ? html`<div role="status" data-lt-state="offered">Offered to ${dest}: the copy starts there once ${dest} takes it in (${dest}'s ⚙ Fleet → Offers). ${agent.name} keeps running here.</div>`
    : progress.phase === 'sent' ? html`<div role="status" data-lt-state="sent">Requested. Its row here turns back into ${agent.name} when it arrives.</div>`
    : html`<div class="muted" role="status">Still in progress; follow it in <button type="button" class="fa-link" onClick=${() => { onClose(); openPage('moves'); }}>⚙ Fleet → Moves</button>.</div>`;
  return html`
    ${phase !== 'progress' ? html`<p class="lt-consequence">${mode === 'home'
      ? `${agent.name} goes straight home to ${home}, not back through the nodes it visited. It leaves at once: it keeps running ${viaPeer ? `on ${agent.nodeName}` : 'here'} until its copy runs at home, then that copy is retired.`
      : mode === 'clone'
        ? `Sends a copy of ${agent.name} with its full conversation history (it may hold code or pasted secrets) to ${target.group} on ${target.name}. The original keeps running here.`
        : `Sends ${agent.name} with its full conversation history (it may hold code or pasted secrets) to ${target.group} on ${target.name}. It leaves at once: it keeps running here until its copy runs there, then stops here; its groups keep a placeholder row.`}</p>
      ${viaPeer ? '' : html`<label class="lt-row"><span class="lt-k">Note</span><textarea id="lt-note" rows="2" placeholder="Optional: told to the agent on arrival" value=${note} onInput=${(e) => setNote(e.currentTarget.value)}></textarea></label>`}` : ''}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    ${status}
    <div class="modal-buttons"><span class="spacer"></span>
      ${phase === 'progress'
        ? html`<button type="button" ref=${okRef} id="lt-close" onClick=${onClose}>Close</button>`
        : html`<button type="button" id="lt-cancel" onClick=${onClose}>Cancel</button>
          <button type="button" ref=${okRef} class="primary" id="lt-ok" disabled=${phase === 'sending' || (mode !== 'home' && (!target?.node || !target?.group))} onClick=${send}>${phase === 'sending' ? 'Sending…' : verb}</button>`}
    </div>`;
}

// LaunchTargetDialog: agent {id, conv, name, node, nodeName, group,
// presence, homeName}; self and fleet describe the nodes; target pre-fills a
// drop ({node, name, group}).
export function LaunchTargetDialog({
  agent, self, fleet, initialMode = 'move', target: initialTarget = null, fromDrop = false,
  api = createLaunchTargetApi(), actions, peerActions, localGroups = [], onClose, onLocalClone,
  timers = globalThis, now = () => Date.now(), openPage = openFleetPage, teleport = TELEPORT_ROUTES,
}) {
  const firstCtx = { local: agent.node === self.id, presence: agent.presence || '', homeIsSelf: agent.homeNode === self.id, source: agent.nodeName || self.name, teleport };
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
  const ctx = { local, presence: agent.presence || '', homeIsSelf: agent.homeNode === self.id, source: source.name, teleport };
  useEffect(() => {
    let live = true;
    if (local) api.status().then((s) => { if (live) setStatus(s); }).catch(() => { if (live) setStatus({}); });
    return () => { live = false; };
  }, []);
  const needsNode = mode === 'move' || mode === 'teleport' || mode === 'clone';
  const targets = needsNode ? nodeTargets({ mode, fleet, self, source, status, localGroups, teleport }) : [];
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
  if (mode === 'morph') body = html`<${MorphPanel} key="morph" agent=${agent} api=${api} onClose=${onClose} setCarry=${setCarry} okRef=${okRef} onPhase=${onPhase} />`;
  else if (mode === 'home') body = html`<${TeleportPanel} key="home" mode="home" agent=${agent} home=${agent.homeName || 'its home'} viaPeer=${!local || agent.presence === 'away'} api=${api} actions=${actions} timers=${timers} now=${now} onClose=${onClose} openPage=${openPage} okRef=${okRef} onPhase=${onPhase} />`;
  else if (cloneHere) body = html`<p class="lt-consequence">A sibling of ${agent.name} starts on this node. The next step picks its working directory or worktree, its first message, and whether it copies the conversation.</p>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" id="lt-cancel" onClick=${onClose}>Cancel</button>
      <button type="button" ref=${okRef} class="primary" id="lt-ok" onClick=${() => onLocalClone?.()}>Clone here…</button></div>`;
  else if (mode === 'move' && plan && (plan.kind === 'push' || plan.kind === 'pull')) body = html`<${MovePanel} key=${`${targetRef.node}/${targetRef.group}`} drop=${{ agent: { id: agent.id, name: agent.name }, source, target: targetRef }} plan=${plan} actions=${actions} peerActions=${peerActions} timers=${timers} now=${now} onClose=${onClose} openPage=${openPage} okRef=${okRef} onPhase=${onPhase} />`;
  else if ((mode === 'teleport' || mode === 'clone') && targetRef?.group) body = html`<${TeleportPanel} key=${`${mode}/${targetRef.node}/${targetRef.group}`} mode=${mode} agent=${agent} target=${targetRef} api=${api} actions=${actions} timers=${timers} now=${now} onClose=${onClose} openPage=${openPage} okRef=${okRef} onPhase=${onPhase} />`;
  else body = html`<div class="muted lt-hint">${picked ? 'Pick the receiving group there.' : 'Pick the node to send it to.'}</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" ref=${okRef} id="lt-cancel" onClick=${onClose}>Cancel</button></div>`;

  const enter = () => { const b = okRef.current; if (b && !b.disabled) b.click(); };
  return html`<${Overlay} id="launch-target" labelledby="launch-target-title" onClose=${onClose} initialFocusRef=${okRef} onSubmitEnter=${enter}>
    <h3 id="launch-target-title">${title}</h3>
    <div class="lt-modes" role="tablist" aria-label="What to do">
      ${MODES.map((m) => { const st = modeState(m.key, ctx); return html`<button key=${m.key} type="button" role="tab" aria-selected=${m.key === mode ? 'true' : 'false'}
        class=${`lt-mode${m.key === mode ? ' sel' : ''}`} disabled=${!st.ok || (locked && m.key !== mode)} title=${st.ok ? '' : st.why} data-lt-mode=${m.key} onClick=${() => switchMode(m.key)}>${m.label}</button>`; })}
    </div>
    ${fromDrop && html`<div class="muted lt-drop-note">↓ Opened by dragging ${agent.name} onto ${initialTarget?.group}@${initialTarget?.name}: node and group are pre-filled.</div>`}
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
  };
}
