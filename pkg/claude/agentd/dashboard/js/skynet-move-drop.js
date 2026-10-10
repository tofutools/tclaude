// skynet-move-drop.js — dragging an agent onto a group on another node in
// "Groups · all nodes" (tcl-u4yodp). A drop confirms, then moves the agent
// with direct_if_allowed: the receiver alone decides whether it lands at once
// or waits for acceptance as an offer. Progress is read from the move record,
// never inferred from the first response: "landed" means the receiver has
// confirmed its copy is running.
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

export const MOVE_POLL_MS = 2000;
// A pulled move's record appears here only once the peer's signed offer
// arrives; until then the detail read is a 404.
const MOVE_APPEAR_MS = 30000;
// Past this the dialog stops polling and points at the Moves page.
const MOVE_WATCH_MS = 120000;
const FAILED = new Set(['blocked', 'expired', 'declined', 'abandoned', 'uncertain']);

// FLEET_PAGE_EVENT asks the ⚙ Fleet page to show one of its sections.
export const FLEET_PAGE_EVENT = 'tclaude:fleet-admin-page';

// The page waits here too, for a ⚙ Fleet island that mounts only once its
// tab is first shown.
let pendingFleetPage = '';
export function takeFleetPage() { const p = pendingFleetPage; pendingFleetPage = ''; return p; }

export function openFleetPage(page, doc = globalThis.document) {
  pendingFleetPage = page;
  doc?.querySelector?.('nav [data-tab="fleet-admin"]')?.click();
  doc?.dispatchEvent?.(new CustomEvent(FLEET_PAGE_EVENT, { detail: page }));
}

// dropPlan says what a drop of an agent from one node onto a group on
// another means from this node: a push (this node's agent to a peer), a pull
// (a peer's agent to this node), or nothing this node can do.
export function dropPlan({ source, target, self }) {
  if (!source?.node || !target?.node || !target.group) return { kind: 'none' };
  if (source.node === target.node) return { kind: 'same' };
  if (source.node === self) return { kind: 'push' };
  if (target.node === self) return { kind: 'pull' };
  return { kind: 'third' };
}

// moveProgress reads a move record into what the dialog shows.
export function moveProgress(rec) {
  if (!rec) return { phase: 'checking' };
  if (FAILED.has(rec.state)) return { phase: 'failed', state: rec.state, error: rec.last_error || '' };
  if (rec.disposition === 'landed') return { phase: 'landed', agent: rec.target_agent || '', cwd: rec.cwd || '' };
  if (rec.disposition === 'pending_acceptance' || rec.state === 'awaiting_acceptance') return { phase: 'waiting' };
  return { phase: 'checking' };
}

function errText(e) { return e?.message || String(e); }

// MoveDropDialog confirms a dropped move and follows it until it lands, waits
// for acceptance, or fails. Closing it never stops the move.
export function MoveDropDialog({ drop, plan, actions, peerActions, timers = globalThis, now = () => Date.now(), onClose, openPage = openFleetPage }) {
  const [phase, setPhase] = useState('confirm');
  const [findings, setFindings] = useState(null);
  const [allow, setAllow] = useState(false);
  const [error, setError] = useState('');
  const [progress, setProgress] = useState(null);
  const okRef = useRef(null);
  const timer = useRef(null);
  const push = plan.kind === 'push';
  const { agent, source, target } = drop;
  const there = push ? target.name : 'this node';
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; if (timer.current) timers.clearTimeout(timer.current); }, []);

  const follow = (id, started) => {
    const tick = async () => {
      timer.current = null;
      if (!alive.current) return;
      let rec = null;
      try { rec = await actions.moveDetail(id); } catch (e) {
        if (e?.status === 404 && now() - started < MOVE_APPEAR_MS) { timer.current = timers.setTimeout(tick, MOVE_POLL_MS); return; }
        if (alive.current) setProgress({ phase: 'unknown', error: errText(e) });
        return;
      }
      if (!alive.current) return;
      const p = moveProgress(rec);
      setProgress(p);
      if (p.phase === 'checking' && now() - started < MOVE_WATCH_MS) timer.current = timers.setTimeout(tick, MOVE_POLL_MS);
      else if (p.phase === 'checking') setProgress({ phase: 'slow' });
    };
    timer.current = timers.setTimeout(tick, MOVE_POLL_MS);
  };

  // pull asks the peer to move it here. A peer without direct moves refuses
  // the unknown field; then it is today's move, offered for acceptance.
  const pull = async (body) => {
    const routes = peerActions(source.node);
    try { return await routes.moveDirect(agent.id, body); } catch (e) {
      if (e?.status !== 400) throw e;
      await routes.move(agent.id, target.group);
      return { disposition: 'pending_acceptance' };
    }
  };

  const send = async () => {
    if (phase !== 'confirm' || (findings && !allow)) return;
    setPhase('sending'); setError('');
    try {
      const body = { group: target.group, direct_if_allowed: true, ...(allow ? { allow_flagged: true } : {}) };
      const res = push
        ? await actions.moveAgent({ agent: agent.id, peer: target.node, ...body })
        : await pull(body);
      if (!alive.current) return;
      const id = res?.move_id || '';
      setPhase('progress');
      // A daemon without direct moves answers with no move_id: that is
      // today's offer flow, which always waits for acceptance.
      if (res?.disposition === 'pending_acceptance' || !id) { setProgress({ phase: 'waiting' }); return; }
      setProgress({ phase: 'checking' });
      follow(id, now());
    } catch (e) {
      if (!alive.current) return;
      setPhase('confirm');
      if (!push && (e?.code === 'flagged_credentials' || e?.status === 422)) {
        setError(`${agent.name}'s history looks like it contains credentials, and a pulled move cannot override that. Move it from ${source.name}'s dashboard, where you can choose to send it anyway.`);
      } else if (push && e?.code === 'flagged_credentials') {
        setFindings(e.body?.findings || []);
        setError('The history looks like it contains credentials. A move always carries the history: send it anyway, or clean up first.');
      } else setError(errText(e));
    }
  };

  const moves = html`<button type="button" class="fa-link" data-move-drop="moves" onClick=${() => { onClose(); openPage(push ? 'moves' : 'offers'); }}>⚙ Fleet → ${push ? 'Moves' : 'Offers'}</button>`;
  const status = !progress ? null
    : progress.phase === 'checking' ? html`<div class="muted" role="status">⏳ Checking whether ${there} takes ${agent.name} in directly…</div>`
    : progress.phase === 'landed' ? html`<div role="status" data-move-drop-state="landed">✅ ${agent.name} landed in group ${target.group} on ${there}${progress.agent ? html` as <code>${progress.agent}</code>` : ''}${progress.cwd ? html`, starting in <code>${progress.cwd}</code>` : ''}. The original on ${source.name} is being retired; ${push ? html`follow that in ${moves}` : `${source.name}'s ⚙ Fleet → Moves shows when it is done`}.</div>`
    : progress.phase === 'waiting' ? html`<div role="status" data-move-drop-state="waiting">⏸ ${push ? `${there} did not take it in directly: it is waiting for ${there} to accept it (in ${there}'s ⚙ Fleet → Offers). ${agent.name} keeps running here until then; follow or abandon it in ` : `It is waiting for you to accept it in this node's `}${moves}.</div>`
    : progress.phase === 'failed' ? html`<div class="fa-danger" role="alert" data-move-drop-state="failed">The move ${progress.state}${progress.error ? `: ${progress.error}` : ''}. ${agent.name} ${push ? 'is still here' : `is still on ${source.name}`}. Details in ${moves}.</div>`
    : progress.phase === 'slow' ? html`<div class="muted" role="status">Still in progress; follow it in ${moves}.</div>`
    : html`<div class="fa-danger" role="alert">Could not follow the move (${progress.error}). Check ${moves}.</div>`;

  return html`<${Overlay} id="skynet-move-drop" labelledby="skynet-move-drop-title" onClose=${onClose} initialFocusRef=${okRef}>
    <h3 id="skynet-move-drop-title">Move ${agent.name} to ${target.group} on ${target.name}?</h3>
    ${phase !== 'progress' ? html`<p>${push
      ? `Sends ${agent.name} (${agent.id})'s config and full conversation history (it may hold code or pasted secrets) to group ${target.group} on ${target.name}.`
      : `${agent.name} (${agent.id}) leaves ${source.name} with its config and full conversation history, to land in group ${target.group} on this node.`}
      ${findings && allow ? ` The history includes suspected credentials (${findings.map((f) => `${f.kind} ×${f.count}`).join(', ')}).` : ''}
      ${' '}If ${push ? `${target.name}'s` : 'this node\'s'} permissions allow it, it lands at once; otherwise it waits for ${push ? target.name : 'you'} to accept it. Once its copy runs, the original on ${source.name} is retired.</p>
      <div class="fa-of-row"><span class="fa-k">from</span> ${source.name} · ${source.group}</div>
      <div class="fa-of-row"><span class="fa-k">to</span> ${target.name} · ${target.group}</div>
      <div class="fa-of-row"><span class="fa-k">starts in</span> <span class="muted">${push ? target.name : 'this node'} picks it by its landing policy (a matching repo, the agent's own path, or the group's default directory)</span></div>` : ''}
    ${findings && phase !== 'progress' && html`<div class="fa-plan"><ul>${findings.map((f) => html`<li class="fa-warn">${f.kind} ×${f.count}${f.locations?.length ? ` (${f.locations.slice(0, 3).join(', ')})` : ''}</li>`)}</ul></div>
      <label class="fa-of-check"><input id="skynet-move-drop-allow" type="checkbox" checked=${allow} onChange=${(e) => setAllow(e.currentTarget.checked)} /> send the history anyway</label>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    ${status}
    <div class="modal-buttons"><span class="spacer"></span>
      ${phase === 'progress'
        ? html`<button type="button" ref=${okRef} id="skynet-move-drop-close" onClick=${onClose}>${progress?.phase === 'checking' ? 'Close (the move continues)' : 'Close'}</button>`
        : html`<button type="button" id="skynet-move-drop-cancel" onClick=${onClose}>Cancel</button>
          <button type="button" ref=${okRef} class="confirm-danger" id="skynet-move-drop-ok" disabled=${phase === 'sending' || (!!findings && !allow)} onClick=${send}>${phase === 'sending' ? 'Sending…' : 'Move agent'}</button>`}
    </div>
  </${Overlay}>`;
}
