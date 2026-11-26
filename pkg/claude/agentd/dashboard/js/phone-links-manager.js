// phone-links-manager.js -- the phone's Links manager, the desktop Groups
// menu's "🔗 links…" (links-island.js LinksApp) on a full-screen view: every
// link across the groups, a filter and a sort over them, a tap on one to
// change its mode or remove it (phone-group-links.js LinkSheet), ＋ New link
// (LinkAddSheet, FROM picked too), and Propose… (ProposeSheet), the peer
// proposal composer: groups to share, with their member lists or not, and
// link pairs with a peer's groups, each ⇄ both ways, → or ←.
//
// The Groups top bar's 🔗 opens it; a group page's Links (N) › opens it
// scoped to that group (a chip that clears the scope). Every request is the
// desktop's (links-actions.js createLinksActions, proposals-model.js
// composeItems), so its notices and questions are the desktop's too, and the
// server decides what is allowed: a refusal shows in the sheet.

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { filterLinks } from './links-state.js';
import { LINK_ACCESSORS, applySortState } from './sort.js';
import { LINK_MODES, composeItems, composerSummary, modeText } from './proposals-model.js';
import { relTime } from './helpers.js';
import { openPhoneSheet, requestClosePhoneSheet } from './phone-sheet.js';
import { ButtonRow, ChipRow, Empty, ErrorNote, SheetHead, SheetTitle, StatePill } from './phone-ui.js';
import { Check } from './ui-components.js';
import { DiscardAsk, useDraftGuard } from './phone-draft-guard.js';
import { LinkAddSheet, LinkSheet, linkEnds, linkState, phoneLinkActions, useLinkTargets } from './phone-group-links.js';

const html = htm.bind(h);

// SORTS are the manager's orders, the desktop table's sortable columns:
// [key, label, sort state] (null: the server's order, oldest first).
export const SORTS = Object.freeze([
  ['newest', 'Newest', { col: 'created', dir: 'desc' }],
  ['from', 'From', { col: 'from', dir: 'asc' }],
  ['to', 'To', { col: 'to', dir: 'asc' }],
  ['mode', 'Mode', { col: 'mode', dir: 'asc' }],
  ['state', 'State', { col: 'remote_state', dir: 'asc' }],
]);

// managerRows is what the manager lists: the links touching group (all with
// none), those the filter matches, in the picked order.
export function managerRows(snapshot, { group = '', query = '', sort = 'newest' } = {}) {
  const all = snapshot?.links || [];
  const scoped = group ? all.filter((link) => link.from === group || link.to === group) : all;
  const order = SORTS.find(([key]) => key === sort)?.[2] || null;
  return { rows: applySortState(filterLinks(scoped, query), LINK_ACCESSORS, order), total: scoped.length };
}

// PhoneLinksManager is the view. group scopes it to one group until its chip
// is cleared.
export function PhoneLinksManager({ snapshot, group: scope = '', fetchImpl = globalThis.fetch, refresh, notify,
  remotePage = !!globalThis.__tclaudeRemoteNode?.id }) {
  const [group, setGroup] = useState(scope);
  const [query, setQuery] = useState('');
  const [sort, setSort] = useState('newest');
  const { rows, total } = managerRows(snapshot, { group, query, sort });
  const deps = { fetchImpl, refresh, notify };
  const propose = (peer = '') => openPhoneSheet({ label: 'Propose to a peer', Body: ProposeSheet, props: { snapshot, peer, ...deps } });
  const add = () => openPhoneSheet({ label: 'New link', Body: LinkAddSheet,
    props: { from: '', snapshot, onPropose: remotePage ? null : propose, ...deps } });
  const open = (link) => {
    const state = linkState(link);
    openPhoneSheet({ label: `Link #${link.id}`, Body: LinkSheet, props: { row: { link, remote: !!link.peer, out: true, ...state }, ...deps } });
  };
  const count = query ? `${rows.length} of ${total}` : `${total} link${total === 1 ? '' : 's'}`;
  return html`<div class="phone-links-manager" data-links-manager>
    <p class="phone-agents-note phone-links-intro">Members of FROM may message members of TO. Direction matters: add the reverse for two-way reach.</p>
    <${ButtonRow} buttons=${[
      { key: 'new', label: '＋ New link', tone: 'pri', onClick: add, attrs: { 'aria-haspopup': 'dialog', 'data-link-new': '' } },
      !remotePage && { key: 'propose', label: '🌐 Propose…', onClick: () => propose(), attrs: { 'aria-haspopup': 'dialog', 'data-link-propose': '' } },
    ].filter(Boolean)} />
    <input name="filter-links" class="ui-input phone-mail-search phone-links-filter" type="search" placeholder="Filter (from / to / mode / peer)"
      aria-label="Filter links" autocomplete="off" spellcheck=${false} value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} />
    <div class="phone-links-bar">
      ${group && html`<button data-ui-exempt type="button" class="phone-links-scope" aria-label=${`Only ${group}'s links. Show every link`}
        onClick=${() => setGroup('')}>${group} ✕</button>`}
      <span class="phone-links-count" aria-live="polite">${count}</span>
    </div>
    <${ChipRow} label="Order links by" value=${sort} onSelect=${setSort} items=${SORTS.map(([key, label]) => ({ key, label }))} />
    ${rows.length
      ? html`<ul class="phone-list phone-links-list">${rows.map((link) => html`<${ManagerRow} key=${String(link.id)} link=${link} onOpen=${open} />`)}</ul>`
      : html`<${Empty}>${total ? 'No link matches the filter.' : group ? `No links to or from ${group} yet.` : 'No links yet. ＋ New link adds one.'}<//>`}
  </div>`;
}

function ManagerRow({ link, onOpen }) {
  const ends = linkEnds(link);
  const { state, tone } = linkState(link);
  const when = relTime(link.created_at) || '';
  return html`<li><button data-ui-exempt type="button" class="phone-links-row" aria-haspopup="dialog" data-link-id=${link.id}
    aria-label=${`${ends.from} to ${ends.to}, ${modeText(link.mode)}${state ? `, ${state}` : ''}. Change or remove it`} onClick=${() => onOpen(link)}>
    <span class="phone-links-text"><span class="phone-links-other">${link.peer ? '🌐 ' : ''}${ends.from} → ${ends.to}</span>
      <span class="phone-links-mode">${modeText(link.mode)} · #${link.id}${when ? ` · ${when}` : ''}</span></span>
    ${state && html`<${StatePill} tone=${tone}>${state}<//>`}
    <span class="phone-agents-chev" aria-hidden="true">›</span>
  </button></li>`;
}

// DIRS are a proposed pair's directions, as the desktop composer words them.
const DIRS = Object.freeze([['both', '⇄ both ways'], ['out', '→ to'], ['in', '← from']]);

// ProposeSheet is the peer proposal composer (links-island.js
// ProposalComposer): a peer, our groups to share (name, description and
// member count, the member list only when ticked), link pairs with its
// groups, and a note. Nothing changes on either node until the peer's
// operator accepts.
export function ProposeSheet({ snapshot, peer: preset = '', close, fetchImpl, refresh, notify, actions: given = null }) {
  const [actions] = useState(() => given || phoneLinkActions({ fetchImpl, refresh, notify }));
  const targets = useLinkTargets(actions, true);
  const connected = !!targets?.hub_connected;
  const capable = (targets?.peers || []).filter((p) => p?.peer && p.link_capable);
  const [peerID, setPeerID] = useState(preset);
  const peer = capable.find((p) => p.peer === peerID) || (peerID ? null : capable[0]) || null;
  const missing = !!peerID && targets && !peer ? ((targets.peers || []).find((p) => p.peer === peerID)?.label || peerID) : '';
  const label = peer?.label || peer?.peer || missing || 'the peer';
  const peerGroups = peer?.groups || [];
  const groups = (snapshot?.groups || []).map((group) => ({ name: group.name, members: (group.members || []).length }));
  const [shares, setShares] = useState([]);
  const [pairs, setPairs] = useState([]);
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const lock = useRef(false);
  const items = composeItems({ shares, pairs });
  const form = useRef(null);
  // A draft asks before a close drops it; a send in flight holds the sheet.
  const guard = useDraftGuard(() => shares.length > 0 || pairs.length > 0 || !!note.trim(), () => lock.current, () => form.current?.querySelector('textarea'));
  const shared = (name) => shares.find((share) => share.group === name);
  const toggleShare = (name, on) => setShares(on ? [...shares, { group: name, includeMembers: false }] : shares.filter((share) => share.group !== name));
  const setMembers = (name, on) => setShares(shares.map((share) => (share.group === name ? { ...share, includeMembers: on } : share)));
  const setPair = (index, patch) => setPairs(pairs.map((pair, at) => (at === index ? { ...pair, ...patch } : pair)));
  const addPair = () => setPairs([...pairs, { ours: groups[0]?.name || '', theirs: peerGroups[0]?.id || '', dir: 'both', mode: 'members->members' }]);
  const peerGroupName = (id) => peerGroups.find((group) => group.id === id)?.name || id;
  const summary = composerSummary({ peer: label, shares, pairs, peerGroupName });
  const submit = async () => {
    if (lock.current) return;
    if (!peer) { setError('Pick a peer.'); return; }
    if (!items.length) { setError('Tick a group to share or add a link.'); return; }
    lock.current = true;
    setBusy(true);
    setError('');
    try {
      await actions.createProposal({ peer: peer.peer, peerLabel: label, note: note.trim(), items });
      close();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      lock.current = false;
      setBusy(false);
    }
  };
  const select = (name, value, options, onChange, aria) => html`<select name=${name} class="ui-select phone-agents-field" aria-label=${aria} value=${value} disabled=${busy}
    onChange=${(event) => onChange(event.currentTarget.value)}>${options.map(([key, text]) => html`<option key=${key} value=${key} selected=${key === value}>${text}</option>`)}</select>`;
  const blocked = targets === undefined ? 'Loading peers…'
    : !connected ? `Proposals need a hub connection.${targets?.reason ? ` ${targets.reason}` : ''}`
      : missing ? `${missing} cannot receive proposals: it runs an older tclaude or has not shared its catalog with you.`
        : !capable.length ? 'No trusted peer runs a tclaude that supports proposals yet.' : '';
  return html`<div ref=${form} class="phone-agents-spawn phone-propose" data-propose>
    <${SheetTitle}>Propose to ${peer ? label : 'a peer node'} 🌐<//>
    <p class="phone-agent-sheet-meta">Nothing changes on either node until ${label}'s operator accepts. You can withdraw it until then.</p>
    ${blocked ? html`<p class="ui-hint phone-link-peer-note" data-propose-blocked>🌐 ${blocked}</p>` : html`
      <label class="phone-agents-label">Peer node
        ${select('proposal-peer', peer?.peer || '', capable.map((p) => [p.peer, p.label || p.peer]), (next) => { setPeerID(next); setPairs([]); }, 'Peer node')}</label>
      <${SheetHead}>Groups to share with ${label}<//>
      <p class="ui-hint phone-propose-hint">Name, description and member count; the member list only when ticked.</p>
      <div class="phone-propose-shares">${groups.map((group) => html`<div key=${group.name} class="phone-propose-share" data-share=${group.name}>
        <${Check} name="proposal-share" checked=${!!shared(group.name)} disabled=${busy} onChange=${(event) => toggleShare(group.name, event.currentTarget.checked)}>
          <b>${group.name}</b> · ${group.members} member${group.members === 1 ? '' : 's'}<//>
        ${shared(group.name) && html`<${Check} name="proposal-share-members" labelClass="phone-propose-members" checked=${shared(group.name).includeMembers} disabled=${busy}
          onChange=${(event) => setMembers(group.name, event.currentTarget.checked)}>include member list<//>`}
      </div>`)}</div>
      <${SheetHead}>Links<//>
      <p class="ui-hint phone-propose-hint">Each direction is a separate edge.</p>
      ${!peerGroups.length && html`<p class="ui-hint phone-propose-hint">${label} has not shared a group with you yet, so there is nothing to link to. Share yours and ${label} can propose links back.</p>`}
      ${pairs.map((pair, index) => html`<div key=${index} class="phone-propose-pair" role="group" aria-label=${`Link ${index + 1}`} data-pair=${index}>
        ${select('proposal-pair-ours', pair.ours, groups.map((g) => [g.name, g.name]), (ours) => setPair(index, { ours }), 'Our group')}
        ${select('proposal-pair-dir', pair.dir, DIRS.map(([key, text]) => [key, key === 'both' ? text : `${text} ${label}`]), (dir) => setPair(index, { dir }), 'Direction')}
        ${select('proposal-pair-theirs', pair.theirs, peerGroups.map((g) => [g.id, `${label}:${g.name}`]), (theirs) => setPair(index, { theirs }), `${label}'s group`)}
        <div class="phone-propose-pair-foot">
          ${select('proposal-pair-mode', pair.mode, LINK_MODES.map((mode) => [mode, modeText(mode)]), (mode) => setPair(index, { mode }), 'Mode')}
          <button data-ui-exempt type="button" class="phone-link phone-propose-remove" disabled=${busy} aria-label=${`Remove link ${index + 1}`}
            onClick=${() => setPairs(pairs.filter((_, at) => at !== index))}>Remove</button>
        </div>
      </div>`)}
      ${peerGroups.length > 0 && groups.length > 0 && html`<button data-ui-exempt type="button" class="phone-link phone-propose-add" data-pair-add disabled=${busy} onClick=${addPair}>＋ Add a link</button>`}
      <label class="phone-agents-label">Note
        <textarea name="proposal-note" class="ui-input phone-agents-field" rows="2" maxlength="4096" value=${note} disabled=${busy}
          placeholder=${`Optional, shown to ${label}'s operator`} onInput=${(event) => setNote(event.currentTarget.value)}></textarea></label>
      ${summary.length > 0 && html`<div class="phone-propose-preview" data-propose-preview>
        <${SheetHead}>What you're proposing<//>
        <ul>${summary.map((line, index) => html`<li key=${index}>${line}</li>`)}</ul>
        <p class="ui-hint phone-propose-hint">No other permission changes; nothing reaches further than these items.</p>
      </div>`}`}
    ${error && html`<${ErrorNote}>${error}<//>`}
    ${guard.asking
      ? html`<${DiscardAsk} what="this proposal" guard=${guard} close=${close} />`
      : html`<${ButtonRow} buttons=${[
          { key: 'cancel', label: 'Cancel', disabled: busy, onClick: () => requestClosePhoneSheet() },
          { key: 'send', label: busy ? 'Sending…' : 'Send proposal', tone: 'pri', disabled: busy || !peer || !connected || !items.length, onClick: submit },
        ]} />`}
  </div>`;
}
