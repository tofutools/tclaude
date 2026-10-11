import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { registerLinksController } from './links-controller.js';
import { relTime } from './helpers.js';
import { LINK_COLS } from './sort.js';
import { LINK_MODES, composeItems, composerSummary, modeText } from './proposals-model.js';

const html = htm.bind(h);

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function errorMessage(error) { return error?.message || String(error); }

function ErrorBanner({ error }) {
  if (!error) return html`<div class="cron-create-error" id="link-modal-error" role="alert"></div>`;
  return html`<div class="cron-create-error" id="link-modal-error" role="alert">${error}</div>`;
}

function SortHeader({ column, state }) {
  if (!column.col) return html`<th>${column.label || ''}</th>`;
  const current = state.sort.value;
  const active = current?.col === column.col;
  const arrow = active ? (current.dir === 'asc' ? '▲' : '▼') : '▾';
  return html`
    <th class=${active ? 'sortable sort-active' : 'sortable'}
      data-sort-table="links" data-sort-col=${column.col}
      title=${`Sort by ${column.label}`}
      onClick=${() => state.cycleSort(column.col)}>
      ${column.label}<span class="sort-arrow">${arrow}</span>
    </th>
  `;
}

export function LinksControls({ state, actions }) {
  const inputRef = useRef(null);
  const current = state.view.value;
  const regularCount = current.query
    ? `${current.filtered} / ${current.total}`
    : `${current.total} link${current.total === 1 ? '' : 's'}`;
  const wizardCount = current.query
    ? `${current.filtered} / ${current.total}`
    : `${current.total} channel${current.total === 1 ? '' : 's'}`;
  return html`
    <input ref=${inputRef} id="filter-links" type="text"
      aria-label="Filter inter-group links" placeholder="Filter (from / to / mode)"
      autocomplete="off" spellcheck=${false} value=${current.query}
      onInput=${(event) => state.setQuery(event.currentTarget.value)} />
    <span class="filter-count" id="filter-links-count" aria-live="polite">
      <span class="theme-copy-regular">${regularCount}</span>
      <span class="theme-copy-wizard">${wizardCount}</span>
    </span>
    <button class="clear-filter" id="filter-links-clear" title="Clear filter"
      aria-label="Clear link filter" onClick=${() => {
        state.setQuery('');
        inputRef.current?.focus();
      }}>×</button>
    <span class="spacer"></span>
    <button id="link-new-open" class="primary"
      onClick=${() => actions.openCreate()}
      title="Add a new inter-group communication link">
      <${Words} plain="+ new link" wizard="+ weave channel" />
    </button>
  `;
}

// Cross-node rows (tcl-2ksi0k) name the peer side as label:group and report
// each node's half separately: this node's authorization, and what the peer
// last told us about its own. An offline peer's half is unknown, not revoked.
const CROSS_COLS = [
  { label: 'This node', col: 'local_state' },
  { label: 'Peer side', col: 'remote_state' },
];

function remoteName(link, name) {
  return html`<span class="rowname">🌐 <span class="link-peer">${link.peer_label || String(link.peer).slice(0, 13)}</span>:${name || link.remote_group || '?'}</span>`;
}

function leftIn(deadline) {
  const ms = Date.parse(deadline || '') - Date.now();
  if (!(ms > 0)) return '';
  const hours = Math.floor(ms / 3600000);
  return hours >= 1 ? `${hours}h` : `${Math.max(1, Math.floor(ms / 60000))}m`;
}

function statePill(text, cls, title = '') {
  return html`<span class=${`link-state ${cls}`} title=${title}>${text}</span>`;
}

function localState(link) {
  const state = link.local_state || 'active';
  return statePill(state, state === 'active' ? 'ok' : state === 'revoked' || state === 'declined' ? 'bad' : 'wait');
}

function peerState(link) {
  const state = link.remote_state || 'unknown';
  const seen = link.remote_state_at ? relTime(link.remote_state_at) : '';
  if (link.remote_stale || state === 'unknown') {
    const last = state && state !== 'unknown' ? ` · last: ${state}` : '';
    return statePill(`unknown · offline${last}`, 'stale', seen ? `${link.peer_label || 'peer'} last reported ${seen}` : 'The peer is offline; its side is not verified');
  }
  if (state === 'pending') {
    const left = leftIn(link.deadline);
    return statePill(`pending${left ? ` · ${left}` : ''}`, 'wait', 'Waiting for the peer operator to decide');
  }
  if (state === 'connecting') return statePill('connecting', 'conn', 'Auto-approved; waiting for the peer to acknowledge this edge');
  if (state === 'active') return statePill(link.auto ? 'active · auto-approved' : 'active', 'ok');
  return statePill(state, state === 'declined' || state === 'revoked' || state === 'expired' ? 'bad' : 'wait');
}

export function LinksList({ state, actions }) {
  const current = state.view.value;
  const [deleting, setDeleting] = useState('');
  const remove = async (link) => {
    const key = String(link.id);
    if (deleting) return;
    setDeleting(key);
    const scope = link.peer && link.direction === 'in' ? link.to : link.from || link.to;
    const peerFields = link.peer ? { peerLabel: link.peer_label || String(link.peer).slice(0, 13), pending: link.direction !== 'in' && link.remote_state === 'pending' } : {};
    try { await actions.deleteLink({ ...link, ...peerFields, scope }); }
    finally { setDeleting(''); }
  };
  if (!current.rows.length) {
    return html`<div class="empty">
      <span class="theme-copy-regular">No inter-group links yet. Create one with the <strong>+ new link</strong> button above.</span>
      <span class="theme-copy-wizard">No arcane channels yet. Weave one with the <strong>+ weave channel</strong> button above.</span>
    </div>`;
  }
  // Local-only nodes keep exactly the columns they always had.
  const cross = current.rows.some((link) => link.peer);
  const columns = cross ? [...LINK_COLS.slice(0, 5), ...CROSS_COLS, ...LINK_COLS.slice(5)] : LINK_COLS;
  return html`
    <table aria-busy=${!!deleting}>
      <thead><tr>${columns.map((column, index) => html`
        <${SortHeader} key=${column.col || `plain-${index}`} column=${column} state=${state} />
      `)}</tr></thead>
      <tbody>${current.rows.map((link) => {
        const rowBusy = deleting === String(link.id);
        const remote = !!link.peer;
        const incoming = remote && link.direction === 'in';
        const pendingOut = remote && !incoming && link.remote_state === 'pending';
        return html`
          <tr key=${String(link.id)} data-key=${`link-${link.id}`} class=${remote ? 'link-cross' : ''}>
            <td class="id">${link.id}</td>
            <td>${incoming ? remoteName(link, link.from) : html`<span class="rowname">${link.from || '(deleted)'}</span>`}</td>
            <td class="muted">→</td>
            <td>${remote && !incoming ? remoteName(link, link.to) : html`<span class="rowname">${link.to || '(deleted)'}</span>`}</td>
            <td><span class="id">${link.mode}</span></td>
            ${cross && html`<td>${remote ? localState(link) : statePill('local', 'local')}</td>`}
            ${cross && html`<td>${remote ? peerState(link) : null}</td>`}
            <td><span class="muted">${relTime(link.created_at) || ''}</span></td>
            <td><div class="row-actions">
              ${!remote && html`<button type="button" disabled=${!!deleting}
                onClick=${() => actions.openEdit({ id: link.id, from: link.from || '', to: link.to || '', mode: link.mode || '' })}
                title="Change this link's mode"><${Words} plain="edit" wizard="rebind" /></button>`}
              <button type="button" class="danger" disabled=${!!deleting}
                onClick=${() => remove(link)}
                title=${pendingOut ? 'Withdraw this link request' : 'Remove this link'}>${pendingOut
    ? (rowBusy ? 'cancelling…' : 'cancel request')
    : html`<${Words} plain=${rowBusy ? 'deleting…' : 'delete'} wizard=${rowBusy ? 'severing…' : 'sever'} />`}</button>
            </div></td>
          </tr>
        `;
      })}</tbody>
    </table>
  `;
}

function LinksManager({ state, actions, confirmDiscard }) {
  return html`
    <${Overlay}
      id="links-manage-modal"
      manage=${true}
      labelledby="links-manage-title"
      onClose=${actions.closeManager}
      confirmDiscard=${confirmDiscard}
    >
      <h3 id="links-manage-title"><${Words} plain="Inter-group links" wizard="Arcane channels between parties" /></h3>
      <p class="manage-intro"><${Words}
        plain="Communication edges between groups: members of FROM may message members of TO. Direction matters — add a reverse edge for two-way reach."
        wizard="Missive channels between parties: familiars of FROM may whisper to familiars of TO. Direction matters — weave a reverse channel for two-way reach."
      /></p>
      <div class="filter-bar" id="links-filter-root"><${LinksControls} state=${state} actions=${actions} /></div>
      <div id="links-list"><${LinksList} state=${state} actions=${actions} /></div>
      <div class="modal-buttons">
        <span class="spacer"></span>
        <button id="links-manage-close" type="button" onClick=${actions.closeManager}><${Words} plain="Close" wizard="Dispel" /></button>
      </div>
    </${Overlay}>
  `;
}

function optionsFor(groups, ...selected) {
  return [...new Set([...(groups || []), ...selected.filter(Boolean)])];
}

// usablePeers keeps the peers a cross-node link can target: the backend
// already lists only known, mutually usable peers and the groups they share.
function usablePeers(targets) {
  return (targets?.peers || []).filter((peer) => peer && peer.peer);
}

// linkTargetsState loads the cross-node targets once per dialog. A daemon
// without cross-node links answers null, which keeps the dialog local-only.
function useLinkTargets(actions, enabled) {
  const [targets, setTargets] = useState(undefined);
  useEffect(() => {
    if (!enabled || typeof actions.loadLinkTargets !== 'function') { setTargets(null); return undefined; }
    let live = true;
    actions.loadLinkTargets().then(
      (value) => { if (live) setTargets(value || null); },
      () => { if (live) setTargets(null); },
    );
    return () => { live = false; };
  }, [actions, enabled]);
  return targets;
}

function PeerOutcome({ peer, group, connected, reason }) {
  if (!connected) {
    return html`<div class="link-peer-note off" id="link-modal-peer-note">🌐 Cross-node links need a hub connection.${reason ? ` ${reason}` : ''} Existing cross-node links keep their state.</div>`;
  }
  if (!peer) return html`<div class="link-peer-note off" id="link-modal-peer-note">No peer shares a group with you yet.</div>`;
  if (!peer.link_capable) {
    return html`<div class="link-peer-note off" id="link-modal-peer-note">${peer.label} runs an older tclaude without group links. Direct messages and grants still work.</div>`;
  }
  if (!group) return html`<div class="link-peer-note off" id="link-modal-peer-note">${peer.label} has not shared a group with you.</div>`;
  if (group.auto) {
    return html`<div class="link-peer-note auto" id="link-modal-peer-note"><strong>No prompt for ${peer.label}'s operator.</strong> ${peer.label} lets you link to ${group.name}, so the link shows <strong>connecting</strong> until ${peer.label} acknowledges this exact edge; while ${peer.label} is offline it stays queued.</div>`;
  }
  return html`<div class="link-peer-note request" id="link-modal-peer-note"><strong>Needs ${peer.label}'s approval.</strong> This sends a link request to ${peer.label}'s operator. The link stays <strong>pending</strong> until they approve, and is declined automatically if nobody decides within 24 hours.</div>`;
}

function LinkEditor({ descriptor, groups, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const edit = descriptor.kind === 'edit';
  const targets = useLinkTargets(actions, !edit);
  const peers = usablePeers(targets);
  const connected = !!targets?.hub_connected;
  const [target, setTarget] = useState('local');
  const [peerID, setPeerID] = useState('');
  const [peerGroupID, setPeerGroupID] = useState('');
  const peer = peers.find((candidate) => candidate.peer === peerID) || peers.find((candidate) => candidate.link_capable) || null;
  const peerGroups = peer?.link_capable ? (peer.groups || []) : [];
  const peerGroup = peerGroups.find((group) => group.id === peerGroupID) || peerGroups[0] || null;
  const remote = target === 'peer' && connected;
  const preset = edit ? descriptor : descriptor.preset;
  // The keyed editor owns one draft lifetime. Snapshot publishes keep the
  // group options live, but must not move the baseline used for dirty checks:
  // otherwise a changed draft can accidentally become "clean" when a group
  // disappears (or an untouched draft can become spuriously dirty).
  const [initial] = useState(() => {
    const from = preset.from || groups[0] || '';
    return {
      from,
      to: preset.to || groups.find((name) => name !== from) || '',
      mode: preset.linkMode || 'members->members',
    };
  });
  const [from, setFrom] = useState(initial.from);
  const [to, setTo] = useState(initial.to);
  const [mode, setMode] = useState(initial.mode);
  const [bidir, setBidir] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const submitGuard = useRef(false);
  const dirty = from !== initial.from || to !== initial.to || mode !== initial.mode || bidir || target !== 'local';
  const groupOptions = optionsFor(groups, from, to);

  const submit = async () => {
    if (submitGuard.current) return;
    submitGuard.current = true;
    try {
      setError('');
      if (remote) {
        if (!from || !peer?.link_capable || !peerGroup) {
          setError('pick a peer group to link to');
          return;
        }
        setBusy(true);
        await actions.createLink({
          from, mode, bidir,
          peer: { id: peer.peer, label: peer.label || peer.peer, groupID: peerGroup.id, groupName: peerGroup.name },
        });
        return;
      }
      if (!edit && (!from || !to)) {
        setError('from and to are required');
        return;
      }
      if (!edit && from === to) {
        setError('from and to must differ — use group membership for intra-group comm');
        return;
      }
      setBusy(true);
      if (edit) await actions.updateLink({ id: descriptor.id, from, to, mode });
      else await actions.createLink({ from, to, mode, bidir });
    } catch (cause) { setError(errorMessage(cause)); }
    finally {
      submitGuard.current = false;
      setBusy(false);
    }
  };

  return html`
    <${Overlay}
      id="link-modal"
      labelledby="link-modal-title"
      onClose=${actions.closeEditor}
      onSubmitHotkey=${submit}
      dirty=${dirty}
      blocked=${busy}
      confirmDiscard=${confirmDiscard}
      registerClose=${registerClose}
    >
      <h3 id="link-modal-title"><${Words}
        plain=${edit ? 'Edit link mode' : 'Add inter-group link'}
        wizard=${edit ? 'Rebind an arcane channel' : 'Weave an arcane channel'}
      /></h3>
      ${edit && html`<div class="modal-meta" id="link-modal-meta">#${descriptor.id} · ${from} → ${to}</div>`}
      <label class="cron-create-row">
        <span class="cron-create-label">From</span>
        <select id="link-modal-from" value=${from} disabled=${busy || edit || !!descriptor.preset?.from}
          onChange=${(event) => setFrom(event.currentTarget.value)}>
          ${groupOptions.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
        </select>
      </label>
      ${!edit && targets && html`
        <div class="cron-create-row" id="link-modal-target-row">
          <span class="cron-create-label">To</span>
          <span class="link-target-seg" role="radiogroup" aria-label="Link target">
            <button type="button" role="radio" aria-checked=${target === 'local'} id="link-modal-target-local"
              class=${target === 'local' ? 'on' : ''} disabled=${busy} onClick=${() => setTarget('local')}>This node</button>
            <button type="button" role="radio" aria-checked=${target === 'peer'} id="link-modal-target-peer"
              class=${target === 'peer' ? 'on' : ''} disabled=${busy || !connected}
              title=${connected ? 'Link to a group a peer node shares with you' : (targets.reason || 'Not connected to the hub')}
              onClick=${() => setTarget('peer')}>Peer node 🌐</button>
          </span>
        </div>`}
      ${remote ? html`
        <label class="cron-create-row">
          <span class="cron-create-label">Peer node</span>
          <select id="link-modal-peer" value=${peer?.peer || ''} disabled=${busy}
            onChange=${(event) => { setPeerID(event.currentTarget.value); setPeerGroupID(''); }}>
            ${peers.map((candidate) => html`<option key=${candidate.peer} value=${candidate.peer} disabled=${!candidate.link_capable}>
              ${candidate.label || candidate.peer} · ${candidate.trust}${candidate.link_capable ? '' : ' · group links unsupported'}</option>`)}
          </select>
        </label>
        <label class="cron-create-row">
          <span class="cron-create-label">Peer group</span>
          <select id="link-modal-peer-group" value=${peerGroup?.id || ''} disabled=${busy || !peerGroups.length}
            onChange=${(event) => setPeerGroupID(event.currentTarget.value)}>
            ${peerGroups.map((group) => html`<option key=${group.id} value=${group.id}>${group.name}</option>`)}
          </select>
        </label>` : html`
        <label class="cron-create-row">
          <span class="cron-create-label">${!edit && targets ? '' : 'To'}</span>
          <select id="link-modal-to" value=${to} disabled=${busy || edit}
            onChange=${(event) => setTo(event.currentTarget.value)}>
            ${groupOptions.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
          </select>
        </label>`}
      ${!edit && targets && (target === 'peer' || !connected) && html`<${PeerOutcome} peer=${peer} group=${peerGroup} connected=${connected} reason=${targets.reason} />`}
      ${remote && peer?.link_capable && typeof actions.openPropose === 'function' && html`<div class="proposal-hint">Want to share groups or link several at once?
        <button type="button" class="fa-link" id="link-modal-propose" disabled=${busy}
          onClick=${() => actions.openPropose({ peer: peer.peer, replace: true })}>Propose several items…</button></div>`}
      <label class="cron-create-row">
        <span class="cron-create-label">Mode</span>
        <select id="link-modal-mode" value=${mode} disabled=${busy}
          onChange=${(event) => setMode(event.currentTarget.value)}>
          <option value="members->members">members → members (any member of FROM may message any member of TO)</option>
          <option value="owners->members">owners → members (only owners of FROM may message members of TO)</option>
        </select>
      </label>
      ${!edit && html`
        <label class="cron-create-enabled" id="link-modal-bidir-row" title="Also create the reverse link (TO → FROM) with the same mode in one call.">
          <input id="link-modal-bidir" type="checkbox" checked=${bidir} disabled=${busy}
            onChange=${(event) => setBidir(event.currentTarget.checked)} />
          ${remote
    ? html`Also ask for the reverse link (${peer?.label || 'peer'}:${peerGroup?.name || '…'} → ${from || 'FROM'})`
    : html`<${Words} plain="Also create reverse link (TO → FROM)" wizard="Also weave a reverse channel (TO → FROM)" />`}
        </label>
      `}
      <${ErrorBanner} error=${error} />
      <div class="modal-buttons">
        <button id="link-modal-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}><${Words} plain="Cancel" wizard="Dispel" /></button>
        <span class="spacer"></span>
        <button id="link-modal-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>
          <${Words}
            plain=${busy ? (edit ? 'Saving…' : 'Creating…') : (edit ? 'Save changes' : remote && peerGroup && !peerGroup.auto ? 'Send link request' : 'Create link')}
            wizard=${busy ? (edit ? 'Rebinding…' : 'Weaving…') : (edit ? 'Rebind channel' : remote && peerGroup && !peerGroup.auto ? 'Send link petition' : 'Weave channel')}
          />
        </button>
      </div>
    </${Overlay}>
  `;
}

// ProposalComposer drafts a multi-item proposal to one peer (tcl-hj04jt):
// groups to share (name, description and member count; the member list only
// when ticked) and link edges between our groups and the peer's. Nothing
// changes on either node until the peer's operator accepts.
function ProposalComposer({ descriptor, groups, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const targets = useLinkTargets(actions, true);
  const connected = !!targets?.hub_connected;
  const peers = usablePeers(targets).filter((candidate) => candidate.link_capable);
  const [peerID, setPeerID] = useState(descriptor.peer || '');
  // A peer handed in by Fleet or the link dialog is never silently swapped
  // for another one; without a preset the first capable peer is offered.
  const peer = peers.find((candidate) => candidate.peer === peerID) || (peerID ? null : peers[0]) || null;
  const missing = !!peerID && targets && !peer
    ? (usablePeers(targets).find((candidate) => candidate.peer === peerID)?.label || peerID) : '';
  const label = peer?.label || peer?.peer || missing || 'the peer';
  const peerGroups = peer?.groups || [];
  const [shares, setShares] = useState([]);
  const [pairs, setPairs] = useState([]);
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const items = composeItems({ shares, pairs });
  const dirty = shares.length > 0 || pairs.length > 0 || note !== '';
  const shared = (name) => shares.find((share) => share.group === name);
  const toggleShare = (name, on) => setShares(on ? [...shares, { group: name, includeMembers: false }] : shares.filter((share) => share.group !== name));
  const setMembers = (name, on) => setShares(shares.map((share) => share.group === name ? { ...share, includeMembers: on } : share));
  const setPair = (index, patch) => setPairs(pairs.map((pair, at) => at === index ? { ...pair, ...patch } : pair));
  const addPair = () => setPairs([...pairs, { ours: groups[0]?.name || '', theirs: peerGroups[0]?.id || '', dir: 'both', mode: 'members->members' }]);
  const peerGroupName = (id) => peerGroups.find((group) => group.id === id)?.name || id;
  const summary = composerSummary({ peer: label, shares, pairs, peerGroupName });
  const submitGuard = useRef(false);

  const submit = async () => {
    // A hotkey and a click can both land before the busy re-render.
    if (submitGuard.current) return;
    if (!peer) { setError('pick a peer'); return; }
    if (!items.length) { setError('tick a group to share or add a link'); return; }
    submitGuard.current = true;
    setBusy(true);
    setError('');
    try {
      await actions.createProposal({ peer: peer.peer, peerLabel: label, note: note.trim(), items });
    } catch (cause) { setError(errorMessage(cause)); } finally { submitGuard.current = false; setBusy(false); }
  };

  return html`
    <${Overlay} id="proposal-modal" labelledby="proposal-modal-title" onClose=${actions.closeEditor}
      onSubmitHotkey=${submit} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
      <h3 id="proposal-modal-title">Propose to ${peer ? label : 'a peer node'} 🌐</h3>
      <div class="modal-meta">Nothing changes on either node until ${label}'s operator accepts. You can withdraw it until then.</div>
      ${targets === undefined ? html`<div class="empty">Loading peers…</div>`
        : !connected ? html`<div class="link-peer-note off" id="proposal-offline">🌐 Proposals need a hub connection.${targets?.reason ? ` ${targets.reason}` : ''}</div>`
        : missing ? html`<div class="link-peer-note off" id="proposal-no-peer">${missing} cannot receive proposals: it runs an older tclaude or has not shared its catalog with you.</div>`
        : !peers.length ? html`<div class="link-peer-note off" id="proposal-no-peer">No trusted peer runs a tclaude that supports proposals yet.</div>`
        : html`
        <label class="cron-create-row">
          <span class="cron-create-label">Peer node</span>
          <select id="proposal-peer" value=${peer?.peer || ''} disabled=${busy}
            onChange=${(event) => { setPeerID(event.currentTarget.value); setPairs([]); }}>
            ${peers.map((candidate) => html`<option key=${candidate.peer} value=${candidate.peer}>${candidate.label || candidate.peer} · ${candidate.trust}</option>`)}
          </select>
        </label>
        <div class="proposal-sec"><span>Groups to share with ${label}</span><span>name, description and member count</span></div>
        ${groups.map((group) => html`<div key=${group.name} class=${`proposal-item${shared(group.name) ? '' : ' off'}`} data-group=${group.name}>
          <label class="grow"><input type="checkbox" class="proposal-share" checked=${!!shared(group.name)} disabled=${busy}
            onChange=${(event) => toggleShare(group.name, event.currentTarget.checked)} /> <b>${group.name}</b> · ${group.members} member${group.members === 1 ? '' : 's'}</label>
          ${shared(group.name) && html`<label class="proposal-members" title="Also share the member list (agent names). Off by default; ${label}'s operator can untick it.">
            <input type="checkbox" class="proposal-share-members" checked=${shared(group.name).includeMembers} disabled=${busy}
              onChange=${(event) => setMembers(group.name, event.currentTarget.checked)} /> include member list</label>`}
        </div>`)}
        <div class="proposal-sec"><span>Links</span><span>each direction is a separate edge</span></div>
        ${!peerGroups.length && html`<div class="proposal-hint">${label} has not shared a group with you yet, so there is nothing to link to. Share yours and ${label} can propose links back.</div>`}
        ${pairs.map((pair, index) => html`<div key=${index} class="proposal-item proposal-pair">
          <select class="proposal-pair-ours" value=${pair.ours} disabled=${busy} onChange=${(event) => setPair(index, { ours: event.currentTarget.value })}>
            ${groups.map((group) => html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}</select>
          <select class="proposal-pair-dir" value=${pair.dir} disabled=${busy} onChange=${(event) => setPair(index, { dir: event.currentTarget.value })}>
            <option value="both">⇄ both ways</option><option value="out">→ to ${label}</option><option value="in">← from ${label}</option></select>
          <select class="proposal-pair-theirs" value=${pair.theirs} disabled=${busy} onChange=${(event) => setPair(index, { theirs: event.currentTarget.value })}>
            ${peerGroups.map((group) => html`<option key=${group.id} value=${group.id}>${label}:${group.name}</option>`)}</select>
          <select class="proposal-pair-mode" value=${pair.mode} disabled=${busy} onChange=${(event) => setPair(index, { mode: event.currentTarget.value })}>
            ${LINK_MODES.map((mode) => html`<option key=${mode} value=${mode}>${modeText(mode)}</option>`)}</select>
          <button type="button" class="fa-link proposal-pair-remove" title="Remove this link" disabled=${busy}
            onClick=${() => setPairs(pairs.filter((_, at) => at !== index))}>✕</button>
        </div>`)}
        ${peerGroups.length > 0 && groups.length > 0 && html`<button type="button" class="fa-link" id="proposal-add-pair" disabled=${busy} onClick=${addPair}>+ add a link</button>`}
        <label class="cron-create-row">
          <span class="cron-create-label">Note</span>
          <textarea id="proposal-note" rows="2" maxlength="4096" value=${note} disabled=${busy}
            placeholder=${`Optional, shown to ${label}'s operator`} onInput=${(event) => setNote(event.currentTarget.value)}></textarea>
        </label>
        ${summary.length > 0 && html`<div class="proposal-preview" id="proposal-preview">
          <div class="proposal-sec"><span>What you're proposing</span></div>
          <ul>${summary.map((line, index) => html`<li key=${index}>${line}</li>`)}</ul>
          <div class="proposal-hint">No other permission changes; nothing reaches further than these items.</div>
        </div>`}`}
      <${ErrorBanner} error=${error} />
      <div class="modal-buttons">
        <button id="proposal-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
        <span class="spacer"></span>
        <button id="proposal-submit" class="primary" type="button" disabled=${busy || !peer || !connected || !items.length} onClick=${submit}>
          ${busy ? 'Sending…' : 'Send proposal'}</button>
      </div>
    </${Overlay}>
  `;
}

export function LinksApp({ state, actions, confirmDiscard }) {
  const current = state.view.value;
  return html`
    ${current.managerOpen && html`<${LinksManager} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`}
    ${current.editor?.kind === 'propose'
    ? html`<${ProposalComposer} key=${current.editor.key} descriptor=${current.editor} groups=${current.groupInfo || []} actions=${actions} confirmDiscard=${confirmDiscard} />`
    : current.editor && html`<${LinkEditor} key=${current.editor.key} descriptor=${current.editor} groups=${current.groups} actions=${actions} confirmDiscard=${confirmDiscard} />`}
  `;
}

export function mountLinksIsland({ host, state, actions, confirmDiscard, registerCleanup }) {
  if (!host) throw new TypeError('links island requires a host');
  if (!state?.view) throw new TypeError('links island requires state');
  if (!actions) throw new TypeError('links island requires actions');
  if (typeof confirmDiscard !== 'function') throw new TypeError('links island requires confirmDiscard');
  if (typeof registerCleanup !== 'function') throw new TypeError('links island requires registerCleanup');
  state.initialize();
  render(html`<${LinksApp} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  const unregister = registerLinksController(actions);
  registerCleanup(() => { unregister(); render(null, host); });
}
