import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { dashboardState } from './snapshot-store.js';
import { createFleetAdminActions } from './fleet-admin-actions.js';
import { relTime } from './helpers.js';
import { downloadBlob, findingsText, modelsFor, redactionText, spawnableHarnesses } from './paf-model.js';

const html = htm.bind(h);

function message(error) { return error?.message || String(error); }

function bytesText(n) {
  if (!n) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function when(iso) {
  if (!iso || String(iso).startsWith('0001-')) return '';
  return relTime(iso);
}

function everyText(seconds) {
  if (!seconds) return '';
  if (seconds % 86400 === 0) return `${seconds / 86400}d`;
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  return `${Math.round(seconds / 60)}m`;
}

// snapshotAgents lists the agents a snapshot can be taken of.
export function snapshotAgents(snapshot) {
  const seen = new Map();
  for (const group of snapshot?.groups || []) {
    for (const member of group.members || []) {
      const id = member.agent_id || member.conv_id;
      if (id && !seen.has(id)) seen.set(id, member.title || member.name || id);
    }
  }
  return [...seen.entries()].map(([id, label]) => ({ id, label })).sort((a, b) => a.label.localeCompare(b.label));
}

function Field({ label, children }) {
  return html`<label class="cron-create-row"><span class="cron-create-label">${label}</span>${children}</label>`;
}

function CreatePanel({ actions, preset, onDone, onError }) {
  const agents = snapshotAgents(dashboardState.snapshot.value);
  const [agent, setAgent] = useState(preset || agents[0]?.id || '');
  const [name, setName] = useState('');
  const [note, setNote] = useState('');
  const [expires, setExpires] = useState('');
  const [history, setHistory] = useState(true);
  const [busy, setBusy] = useState(false);
  const create = async () => {
    setBusy(true);
    try {
      const meta = await actions.snapshots.create({ agent, name: name.trim(), note: note.trim(), expires: expires.trim(), no_history: !history });
      onDone(`Snapshot ${meta.name} taken`);
    } catch (e) { onError(message(e)); } finally { setBusy(false); }
  };
  return html`<div class="snap-panel" id="snapshot-create">
    <div class="paf-sec">Take a snapshot</div>
    <${Field} label="Agent"><select id="snapshot-create-agent" value=${agent} onChange=${(e) => setAgent(e.currentTarget.value)}>
      ${agents.map((a) => html`<option key=${a.id} value=${a.id}>${a.label}</option>`)}</select></${Field}>
    <${Field} label="Name"><input id="snapshot-create-name" value=${name} placeholder="default: <agent>-<UTC time>" onInput=${(e) => setName(e.currentTarget.value)} /></${Field}>
    <${Field} label="Note"><input id="snapshot-create-note" value=${note} maxlength="1000" placeholder="optional, e.g. known good before the refactor" onInput=${(e) => setNote(e.currentTarget.value)} /></${Field}>
    <${Field} label="Expires"><input id="snapshot-create-expires" value=${expires} placeholder="e.g. 30d, 72h or never (default: library retention)" onInput=${(e) => setExpires(e.currentTarget.value)} /></${Field}>
    <label class="paf-check"><input type="checkbox" id="snapshot-create-history" checked=${history} onChange=${(e) => setHistory(e.currentTarget.checked)} />
      <span>Include the conversation<span class="paf-hint">A busy agent is captured up to its last complete turn.</span></span></label>
    <div class="paf-row-buttons"><button type="button" id="snapshot-create-submit" class="primary" disabled=${busy || !agent} onClick=${create}>${busy ? 'Taking…' : '📸 Take snapshot'}</button></div>
  </div>`;
}

function SpawnPanel({ actions, snap, onDone, onError }) {
  const snapshot = dashboardState.snapshot.value;
  const groups = (snapshot?.groups || []).map((g) => g.name);
  const [group, setGroup] = useState('');
  const [count, setCount] = useState(1);
  const [name, setName] = useState('');
  const [cwd, setCwd] = useState('');
  const [harness, setHarness] = useState('');
  const [model, setModel] = useState('');
  const [confirmCut, setConfirmCut] = useState(false);
  const [busy, setBusy] = useState(false);
  const spawn = async () => {
    setBusy(true);
    try {
      const res = await actions.snapshots.spawn(snap.name, { group, count: Number(count) || 1, name: name.trim(), cwd: cwd.trim(), harness, model, ...(confirmCut ? { confirm_history_loss: true } : {}) });
      onDone(`Spawned ${(res.spawned || []).length} agent${(res.spawned || []).length === 1 ? '' : 's'} from ${snap.name} into ${group}`);
    } catch (e) {
      const partial = e.body?.result?.spawned?.length;
      onError(partial ? `${message(e)} (${partial} already spawned and kept)` : message(e));
    } finally { setBusy(false); }
  };
  return html`<div class="snap-panel" id="snapshot-spawn">
    <div class="paf-sec">Spawn from ${snap.name}</div>
    <div class="paf-hint">Each agent is a new identity that continues from the snapshot's conversation; ${snap.agent_name || 'the original'} is untouched.</div>
    <${Field} label="Group"><select id="snapshot-spawn-group" value=${group} onChange=${(e) => setGroup(e.currentTarget.value)}>
      <option value="">(pick a group)</option>${groups.map((g) => html`<option key=${g} value=${g}>${g}</option>`)}</select></${Field}>
    <${Field} label="Count"><input id="snapshot-spawn-count" type="number" min="1" max="20" value=${count} onInput=${(e) => setCount(e.currentTarget.value)} /></${Field}>
    <${Field} label="Name"><input id="snapshot-spawn-name" value=${name} placeholder=${`default: ${snap.agent_name || 'the agent'}${Number(count) > 1 ? '-1, -2, …' : ''}`} onInput=${(e) => setName(e.currentTarget.value)} /></${Field}>
    <${Field} label="Working dir"><input id="snapshot-spawn-cwd" value=${cwd} placeholder="default: the recorded working directory" onInput=${(e) => setCwd(e.currentTarget.value)} /></${Field}>
    <div class="cron-create-row"><span class="cron-create-label">Land on</span><span class="paf-landing-selects">
      <select id="snapshot-spawn-harness" value=${harness} onChange=${(e) => { setHarness(e.currentTarget.value); setModel(''); }}>
        <option value="">${snap.harness || 'own harness'} (as captured)</option>
        ${spawnableHarnesses(snapshot).map((x) => html`<option key=${x.name} value=${x.name}>${x.display_name || x.name}</option>`)}</select>
      <select id="snapshot-spawn-model" value=${model} onChange=${(e) => setModel(e.currentTarget.value)}>
        <option value="">(model: default)</option>${modelsFor(snapshot, harness).map((m) => html`<option key=${m} value=${m}>${m}</option>`)}</select>
    </span></div>
    <label class="paf-check"><input type="checkbox" id="snapshot-spawn-confirm-loss" checked=${confirmCut} onChange=${(e) => setConfirmCut(e.currentTarget.checked)} />
      <span>Allow cutting the conversation<span class="paf-hint">A model with a smaller context window may keep only the start, the agent's summary and the newest turns; 25% or more needs this. The full history is saved beside each agent.</span></span></label>
    <div class="paf-row-buttons"><button type="button" id="snapshot-spawn-submit" class="primary" disabled=${busy || !group} onClick=${spawn}>${busy ? 'Spawning…' : 'Spawn'}</button></div>
  </div>`;
}

const SHARE_TTLS = ['1d', '3d', '7d', '14d', '30d'];

// SharePanel puts a snapshot on a Board for named members. Review shows what
// goes out and to whom; publishing needs the typed confirmation, which is
// recorded as consent for exactly these recipients. A flagged snapshot can
// only be shared as a redacted copy.
function SharePanel({ actions, snap, onDone, onError, fleet }) {
  const [boards, setBoards] = useState(null);
  const [board, setBoard] = useState('');
  const [members, setMembers] = useState([]);
  const [to, setTo] = useState([]);
  const [expires, setExpires] = useState('7d');
  const [redacted, setRedacted] = useState(null);
  const [review, setReview] = useState(null);
  const [typed, setTyped] = useState('');
  const [busy, setBusy] = useState('');
  useEffect(() => {
    let live = true;
    fleet.boards().then((rows) => { if (live) { setBoards(rows || []); setBoard(rows?.[0]?.id || ''); } }, (e) => { if (live) { setBoards([]); onError(`Boards: ${message(e)}`); } });
    return () => { live = false; };
  }, []);
  useEffect(() => {
    setMembers([]);
    setTo([]);
    setReview(null);
    if (!board) return undefined;
    let live = true;
    fleet.boardMembers(board).then((rows) => { if (live) setMembers(rows || []); }, () => { if (live) setMembers([]); });
    return () => { live = false; };
  }, [board]);
  // Consent covers exactly what was reviewed: a review records the
  // recipients and expiry it was built from and the share sends those, and a
  // review that returns after the choices changed is dropped.
  const choices = useRef(0);
  const changed = () => { choices.current += 1; setReview(null); };
  const reviewed = async (sent) => {
    const seq = choices.current;
    const result = await actions.snapshots.share(board, sent);
    if (seq === choices.current) { setReview({ ...result, sent }); setTyped(''); }
  };
  const doReview = async () => {
    setBusy('review');
    try { await reviewed(redacted ? { snapshot: '', name: snap.name, to, expires, digest: '', blob: redacted } : { snapshot: snap.name, name: '', to, expires, digest: '', blob: null }); } catch (e) {
      if (e.status === 422 && e.body?.code === 'flagged_credentials') setReview({ flagged: true });
      else onError(message(e));
    } finally { setBusy(''); }
  };
  const redactThenReview = async () => {
    setBusy('redact');
    try {
      const blob = await actions.snapshots.exportBlob(snap.name);
      const copy = await actions.redactPAF(blob, { mask_flagged: true, paths_to_placeholders: true }, false);
      setRedacted(copy);
      await reviewed({ snapshot: '', name: snap.name, to, expires, digest: '', blob: copy });
    } catch (e) { onError(message(e)); } finally { setBusy(''); }
  };
  const publish = async () => {
    setBusy('share');
    try {
      await actions.snapshots.share(board, { ...review.sent, digest: review.summary.content_digest });
      onDone(`Shared ${snap.name} on the board with ${review.recipients.length} recipient${review.recipients.length === 1 ? '' : 's'}`);
    } catch (e) { onError(message(e)); } finally { setBusy(''); }
  };
  const toggle = (id, on) => { setTo(on ? [...to, id] : to.filter((x) => x !== id)); changed(); };
  const s = review?.summary;
  return html`<div class="snap-panel" id="snapshot-share">
    <div class="paf-sec">Share ${snap.name} on a Board</div>
    ${boards && !boards.length ? html`<div class="paf-hint">This node is on no Board. Join or create one in ⚙ Fleet config → Boards.</div>` : html`
    <${Field} label="Board"><select id="snapshot-share-board" value=${board} disabled=${!!busy} onChange=${(e) => { changed(); setBoard(e.currentTarget.value); }}>
      ${(boards || []).map((b) => html`<option key=${b.id} value=${b.id}>${b.name || b.id}</option>`)}</select></${Field}>
    <div class="cron-create-row"><span class="cron-create-label">Recipients</span><span class="snap-recipients" id="snapshot-share-to">
      ${members.length === 0 ? html`<span class="paf-hint">no other members</span>` : members.map((m) => html`<label key=${m.instance} class="paf-check">
        <input type="checkbox" data-instance=${m.instance} checked=${to.includes(m.instance)} disabled=${!!busy} onChange=${(e) => toggle(m.instance, e.currentTarget.checked)} />
        <span>${m.label || m.name || m.instance}</span></label>`)}
      <span class="paf-hint">Only these members can open it; every member sees that it exists. This node is always a recipient.</span></span></div>
    <${Field} label="Expires"><select id="snapshot-share-expires" value=${expires} disabled=${!!busy} onChange=${(e) => { setExpires(e.currentTarget.value); changed(); }}>
      ${SHARE_TTLS.map((d) => html`<option key=${d} value=${d}>in ${d.replace('d', d === '1d' ? ' day' : ' days')}</option>`)}</select></${Field}>
    ${review?.flagged && html`<div class="paf-findings" id="snapshot-share-flagged">⚠ This snapshot has suspected credentials, so it cannot go on a board as it is.
      <div class="paf-row-buttons"><button type="button" id="snapshot-share-redact" disabled=${!!busy} onClick=${redactThenReview}>${busy === 'redact' ? 'Redacting…' : 'Redact a copy and review'}</button></div></div>`}
    ${s && html`<div class="paf-box" id="snapshot-share-review">
      <b>You are about to share${review.sent?.blob ? ' a redacted copy' : ''}</b>
      <div>${bytesText(s.bytes)} · ${s.turns} turn${s.turns === 1 ? '' : 's'}${s.from ? ` · ${new Date(s.from).toLocaleString()} → ${new Date(s.to).toLocaleString()}` : ''}</div>
      <div>To: ${review.recipients.join(', ')}</div>
      <div>Expires ${new Date(review.expires_at).toLocaleString()}; the hub deletes it then, even if pinned.</div>
      ${redactionText(s).map((line) => html`<div key=${line}>✓ ${line}</div>`)}
      ${findingsText(s.findings).length ? html`<div class="gi-warn">⚠ ${findingsText(s.findings).join('; ')}</div>` : html`<div class="paf-ok">✓ No suspected credentials.</div>`}
      <${Field} label="Confirm"><input id="snapshot-share-confirm" value=${typed} placeholder="type share" autocomplete="off" onInput=${(e) => setTyped(e.currentTarget.value)} /></${Field}>
    </div>`}
    <div class="paf-row-buttons">
      <button type="button" id="snapshot-share-review-btn" disabled=${!!busy || !board || !to.length} onClick=${doReview}>${busy === 'review' ? 'Reviewing…' : 'Review'}</button>
      <button type="button" id="snapshot-share-submit" class="primary" disabled=${!!busy || !s || typed.trim() !== 'share'} onClick=${publish}>${busy === 'share' ? 'Sharing…' : 'Share'}</button>
    </div>`}
  </div>`;
}

// PublishPanel makes a redacted, pseudonymous, signed public PAF of a
// snapshot. The human reviews the summary; the download is the consent.
function PublishPanel({ actions, snap, onDone, onError }) {
  const [opts, setOpts] = useState({ stub_tool_results: false, drop_media: false, drop_reasoning: false });
  const [review, setReview] = useState(null);
  const [ack, setAck] = useState(false);
  const [busy, setBusy] = useState('');
  const blob = useRef(null);
  const source = async () => (blob.current ||= await actions.snapshots.exportBlob(snap.name));
  const set = (key) => (e) => { setOpts({ ...opts, [key]: e.currentTarget.checked }); setReview(null); setAck(false); };
  const doReview = async () => {
    setBusy('review');
    try { setReview(await actions.snapshots.publish(await source(), opts)); } catch (e) { onError(message(e)); } finally { setBusy(''); }
  };
  const publish = async () => {
    setBusy('publish');
    try {
      const out = await actions.snapshots.publish(await source(), opts, review.summary.content_digest);
      downloadBlob(out, `${snap.name}.public.paf`);
      onDone(`Published ${snap.name}: the signed public file was downloaded. tclaude hosts nothing; you choose where it goes.`);
    } catch (e) { onError(message(e)); } finally { setBusy(''); }
  };
  const s = review?.summary;
  return html`<div class="snap-panel" id="snapshot-publish">
    <div class="paf-sec">Publish ${snap.name}</div>
    <div class="paf-hint">Publishing always masks suspected credentials, replaces paths with placeholders, strips the agent's name, ids and repository, and drops the native history. It is signed with this node's pseudonymous publisher key, never its federation key.</div>
    <label class="paf-check"><input type="checkbox" id="snapshot-publish-stub" checked=${opts.stub_tool_results} onChange=${set('stub_tool_results')} /><span>Stub old tool results</span></label>
    <label class="paf-check"><input type="checkbox" id="snapshot-publish-reasoning" checked=${opts.drop_reasoning} onChange=${set('drop_reasoning')} /><span>Drop reasoning</span></label>
    <label class="paf-check"><input type="checkbox" id="snapshot-publish-media" checked=${opts.drop_media} onChange=${set('drop_media')} /><span>Drop images and documents</span></label>
    ${s && html`<div class="paf-box" id="snapshot-publish-review">
      <div>${bytesText(s.bytes)} · ${s.turns} turn${s.turns === 1 ? '' : 's'}</div>
      ${redactionText(s).map((line) => html`<div key=${line}>✓ ${line}</div>`)}
      ${findingsText(s.findings).length ? html`<div class="gi-warn">⚠ Still flagged: ${findingsText(s.findings).join('; ')}</div>` : html`<div class="paf-ok">✓ No suspected credentials remain.</div>`}
      <label class="paf-check"><input type="checkbox" id="snapshot-publish-ack" checked=${ack} onChange=${(e) => setAck(e.currentTarget.checked)} />
        <span>I have reviewed this and consent to sharing it publicly</span></label>
    </div>`}
    <div class="paf-row-buttons">
      <button type="button" id="snapshot-publish-review-btn" disabled=${!!busy} onClick=${doReview}>${busy === 'review' ? 'Reviewing…' : 'Review'}</button>
      <button type="button" id="snapshot-publish-submit" class="primary" disabled=${!!busy || !s || !ack} onClick=${publish}>${busy === 'publish' ? 'Publishing…' : '⤓ Download public .paf'}</button>
    </div>
  </div>`;
}

function PolicyStatus({ policies }) {
  if (!policies?.length) return null;
  return html`<div id="snapshot-policies">
    <div class="paf-sec">Resilience policies</div>
    <table class="paf-member-table"><thead><tr><th>Target</th><th>Every</th><th>To</th><th>Agent</th><th>Last good</th><th>Status</th><th>Next due</th></tr></thead><tbody>
      ${policies.flatMap((p) => (p.agents?.length ? p.agents : [null]).map((a, i) => html`<tr key=${`${p.id}:${a?.agent || i}`} data-policy=${p.id}>
        <td>${i === 0 ? p.target : ''}</td><td>${i === 0 ? everyText(p.every_seconds) : ''}</td><td>${i === 0 ? (p.peer_label || p.dest?.peer || '') : ''}</td>
        <td>${a?.agent || html`<span class="paf-hint">no agent yet</span>`}</td>
        <td>${a ? (when(a.last_good) || '—') : ''}</td>
        <td>${!a ? '' : a.last_error ? html`<span class="gi-warn" title=${a.last_error}>⚠ error${a.failures ? ` ×${a.failures}` : ''}</span>`
          : a.deferred ? html`<span class="paf-hint" title=${a.deferred}>deferred</span>`
          : a.pending_offer ? html`<span class="paf-hint">sending…</span>` : html`<span class="paf-ok">ok</span>`}
          ${a?.findings ? html` <span class="gi-warn">${a.findings} finding${a.findings === 1 ? '' : 's'}</span>` : ''}</td>
        <td>${a ? (when(a.next_due) || '—') : ''}</td>
      </tr>`))}
    </tbody></table>
    <div class="paf-hint">Set and remove policies with <code>tclaude agent snapshot policy</code>.</div>
  </div>`;
}

// SnapshotsDialog is the Snapshots view (4d, tcl-3hjdm1): this node's
// snapshot library with create, spawn-from, export, share-on-Board, publish
// and remove, plus the status of resilience policies.
export function SnapshotsDialog({ descriptor, state, actions, confirm, confirmDiscard }) {
  // Boards and their members come from the Fleet actions; tests inject them.
  const [fleet] = useState(() => actions.fleet || createFleetAdminActions());
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [list, setList] = useState(null);
  const [policies, setPolicies] = useState([]);
  const [panel, setPanel] = useState(descriptor.agent ? { kind: 'create' } : null);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    actions.snapshots.list().then((rows) => { if (live) setList(rows); }, (e) => { if (live) { setList([]); setError(message(e)); } });
    actions.snapshots.policies().then((rows) => { if (live) setPolicies(rows); }, () => { if (live) setPolicies([]); });
    return () => { live = false; };
  }, [tick]);
  const done = (text) => { setNotice(text); setError(''); setPanel(null); setTick((n) => n + 1); };
  const fail = (text) => { setError(text); setNotice(''); };
  const open = (kind, snap) => { setPanel(panel?.kind === kind && panel?.snap?.name === snap?.name ? null : { kind, snap }); setError(''); setNotice(''); };
  const remove = async (snap) => {
    const ok = await confirm({ title: `Remove snapshot ${snap.name}?`, body: 'The snapshot is deleted from this node. Agents already spawned from it, and copies already exported or shared, are not affected.', okLabel: 'Remove' });
    if (!ok) return;
    try { await actions.snapshots.remove(snap.name); done(`Removed ${snap.name}`); } catch (e) { fail(message(e)); }
  };
  const exportOne = async (snap) => {
    try { downloadBlob(await actions.snapshots.exportBlob(snap.name), `${snap.name}.paf`); } catch (e) { fail(message(e)); }
  };
  const p = panel;
  return html`<${Overlay} id="snapshots-modal" labelledby="snapshots-title" onClose=${state.closeDialog}
    dirty=${false} blocked=${false} confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
    <h3 id="snapshots-title">📸 Snapshots</h3>
    <div class="modal-meta">Named, point-in-time copies of agents, kept on this node. A snapshot is a fork point: spawning from it creates new agents and leaves the original untouched.</div>
    <div class="snap-toolbar">
      <button type="button" id="snapshots-create-open" class="tool" onClick=${() => open('create')}>+ take snapshot…</button>
      <button type="button" id="snapshots-prune" class="tool" title="Remove expired snapshots now" onClick=${async () => { try { const r = await actions.snapshots.prune(); done(`Pruned ${(r.removed || []).length} expired`); } catch (e) { fail(message(e)); } }}>prune expired</button>
    </div>
    ${p?.kind === 'create' && html`<${CreatePanel} actions=${actions} preset=${descriptor.agent} onDone=${done} onError=${fail} />`}
    ${list === null ? html`<div class="empty">Loading…</div>`
      : list.length === 0 ? html`<div class="empty" id="snapshots-empty">No snapshots yet. Take one here, from an agent's ⚙ menu, or with <code>tclaude agent snapshot create</code>.</div>`
      : html`<table class="paf-member-table" id="snapshots-list"><thead><tr><th>Name</th><th>Agent</th><th>Harness</th><th>Taken</th><th>Size</th><th>Expires</th><th></th></tr></thead><tbody>
        ${list.map((snap) => html`<tr key=${snap.name} data-snapshot=${snap.name} class=${p?.snap?.name === snap.name ? 'active' : ''}>
          <td><div class="snap-trunc" title=${snap.name}><b>${snap.name}</b></div>${snap.note && html`<div class="paf-hint snap-trunc" title=${snap.note}>${snap.note}</div>`}</td>
          <td><div class="snap-trunc" title=${snap.agent_name}>${snap.agent_name}</div></td>
          <td><div class="snap-trunc" title=${[snap.harness, snap.model].filter(Boolean).join(' · ')}>${[snap.harness, snap.model].filter(Boolean).join(' · ')}</div>${!snap.history ? html`<div class="paf-hint">no history</div>` : ''}</td>
          <td title=${snap.created_at}>${when(snap.created_at)}${snap.state === 'last_complete_turn' ? html`<div class="paf-hint">last complete turn</div>` : ''}</td>
          <td>${bytesText(snap.bytes)}</td>
          <td>${snap.expires_at ? when(snap.expires_at) : 'never'}</td>
          <td class="snap-acts">
            ${(snap.findings || []).length > 0 && html`<span class="gi-warn" title=${findingsText(snap.findings).join('\n')}>⚠ ${snap.findings.length}</span>`}
            <button type="button" data-snap="spawn" onClick=${() => open('spawn', snap)}>Spawn…</button>
            <button type="button" data-snap="export" onClick=${() => exportOne(snap)}>⤓</button>
            <button type="button" data-snap="share" onClick=${() => open('share', snap)}>Share…</button>
            <button type="button" data-snap="publish" onClick=${() => open('publish', snap)}>Publish…</button>
            <button type="button" data-snap="remove" class="fa-danger" onClick=${() => remove(snap)}>✕</button>
          </td>
        </tr>`)}
      </tbody></table>`}
    ${p?.kind === 'spawn' && html`<${SpawnPanel} key=${p.snap.name} actions=${actions} snap=${p.snap} onDone=${done} onError=${fail} />`}
    ${p?.kind === 'share' && html`<${SharePanel} key=${p.snap.name} actions=${actions} snap=${p.snap} onDone=${done} onError=${fail} fleet=${fleet} />`}
    ${p?.kind === 'publish' && html`<${PublishPanel} key=${p.snap.name} actions=${actions} snap=${p.snap} onDone=${done} onError=${fail} />`}
    ${notice && html`<div class="paf-ok" id="snapshots-notice">✓ ${notice}</div>`}
    <div class="cron-create-error" role="alert" id="snapshots-error">${error}</div>
    <${PolicyStatus} policies=${policies} />
    <div class="modal-buttons"><span class="spacer"></span><button type="button" id="snapshots-close" onClick=${() => { void requestClose(); }}>Close</button></div>
  </${Overlay}>`;
}
