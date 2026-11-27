// phone-group-links.js -- a group's links on the phone: the Links section of
// its page (phone-agents-island.js GroupPage), each link a row a tap
// configures, and ＋ Link adding one from this group.
//
// A link lets the members of FROM message the members of TO (docs/
// agents-and-groups.md); a cross-node link goes to or comes from a group a
// peer node shares (docs/federation-peers.md). The sheets post through the
// desktop's own link actions (links-actions.js createLinksActions): the same
// routes, the same notices, the same removal questions, so the desktop, the
// CLI and the phone reach the same links. The server decides what is
// allowed and its refusal shows in the sheet; an archived group's links can
// be looked at only. Several items to one peer at once (a proposal) stay on
// the desktop.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { createLinksActions } from './links-actions.js';
import { createLinksState } from './links-state.js';
import { LINK_MODES, modeText } from './proposals-model.js';
import { openPhoneSheet, useSheetGuard } from './phone-sheet.js';
import { ButtonRow, Empty, ErrorNote, Section, SheetHead, SheetTitle, StatePill } from './phone-ui.js';
import { Switch } from './ui-components.js';
import { dashboardActions } from './dashboard-actions.js';
import { shellToast } from './shell-state.js';
import { deleteFields, groupLinks, linkState } from './group-links-model.js';

export { deleteFields, groupLinks, linkState };

const html = htm.bind(h);

// MODE_HINTS say who may message whom on a link of each mode.
export const MODE_HINTS = Object.freeze({
  'members->members': 'Any member of FROM may message any member of TO.',
  'owners->members': 'Only the owners of FROM may message the members of TO.',
});

// phoneLinkActions is the desktop's link actions with the phone's notices and
// a removal question the sheet asks (ask(spec) → Promise<boolean>).
export function phoneLinkActions({ fetchImpl = globalThis.fetch, refresh = () => dashboardActions.refresh(), notify = shellToast, ask = async () => true } = {}) {
  return createLinksActions({ state: createLinksState(), fetchImpl, refresh, notify, confirm: (spec) => ask(spec) });
}

// GroupLinks is the Links section of a group's page. readOnly (an archived
// group) leaves the rows to look at, no ＋ Link and no changes.
export function GroupLinks({ group, snapshot, readOnly = false, fetchImpl = globalThis.fetch, refresh, notify }) {
  const rows = groupLinks(snapshot, group);
  const deps = { fetchImpl, refresh, notify };
  const add = () => openPhoneSheet({ label: 'New link', Body: LinkAddSheet, props: { from: group, snapshot, ...deps } });
  const open = (row) => openPhoneSheet({ label: `Link ${row.link.from} → ${row.link.to}`, Body: LinkSheet, props: { row, group, readOnly, ...deps } });
  const more = !readOnly && html`<button data-ui-exempt type="button" class="phone-link phone-links-add" aria-haspopup="dialog"
    aria-label=${`New link from ${group}`} onClick=${add}>＋ Link</button>`;
  return html`<${Section} title="Links" count=${rows.length || null} more=${more} class="phone-links">
    ${rows.length
      ? html`<ul class="phone-list phone-links-list">${rows.map((row) => html`<li key=${`${row.out ? 'out' : 'in'}-${row.link.id}`}>
          <button data-ui-exempt type="button" class="phone-links-row" aria-haspopup="dialog" data-link-id=${row.link.id}
            aria-label=${`${row.out ? 'To' : 'From'} ${row.other}, ${modeText(row.link.mode)}${row.state ? `, ${row.state}` : ''}. ${readOnly ? 'Look at it' : 'Change or remove it'}`}
            onClick=${() => open(row)}>
            <span class="phone-links-dir" aria-hidden="true">${row.out ? '→' : '←'}</span>
            <span class="phone-links-text"><span class="phone-links-other">${row.remote ? '🌐 ' : ''}${row.other}</span>
              <span class="phone-links-mode">${modeText(row.link.mode)}</span></span>
            ${row.state && html`<${StatePill} tone=${row.tone}>${row.state}<//>`}
            <span class="phone-agents-chev" aria-hidden="true">›</span>
          </button></li>`)}</ul>`
      : html`<p class="phone-agents-note">None. ${readOnly ? '' : '＋ Link lets this group message another.'}</p>`}
  <//>`;
}

// useAsk is a removal question the sheet shows in place of its buttons: ask
// resolves true on its OK, false on Keep or on Back. While busy() (a save or
// a removal in flight) the sheet holds: a close then would leave the result
// with nowhere to show.
function useAsk(busy) {
  const [spec, setSpec] = useState(null);
  const answer = useRef(null);
  const settle = (yes) => { answer.current?.(yes); answer.current = null; setSpec(null); };
  useSheetGuard(() => {
    if (!answer.current) return busy();
    settle(false);
    return true;
  });
  useEffect(() => () => answer.current?.(false), []);
  const ask = (next) => new Promise((resolve) => { answer.current = resolve; setSpec(next); });
  return { spec, ask, settle };
}

function ModeField({ mode, setMode, disabled }) {
  return html`<label class="phone-agents-label">Mode
    <select name="link-mode" class="ui-select phone-agents-field" value=${mode} disabled=${disabled}
      onChange=${(event) => setMode(event.currentTarget.value)}>
      ${LINK_MODES.map((key) => html`<option key=${key} value=${key} selected=${key === mode}>${modeText(key)}</option>`)}
    </select>
    <small class="ui-hint">${MODE_HINTS[mode] || ''}</small>
  </label>`;
}

// LinkSheet is one link: where it goes, its mode (a local link's changes
// here), and Remove, which asks first in the desktop's words. A cross-node
// link's mode is the two nodes' agreement, so it can only be removed (or a
// request withdrawn) here, as on the desktop.
export function LinkSheet({ row, group, readOnly = false, close, fetchImpl, refresh, notify, actions: given = null }) {
  const lock = useRef(false);
  const asking = useAsk(() => lock.current);
  const [actions] = useState(() => given || phoneLinkActions({ fetchImpl, refresh, notify, ask: asking.ask }));
  const { link, remote } = row;
  const [mode, setMode] = useState(link.mode || 'members->members');
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  // The question takes the focus (its Keep); Keep gives it back to Remove.
  const sheet = useRef(null);
  const asked = useRef(false);
  useLayoutEffect(() => {
    if (asking.spec) sheet.current?.querySelector('.phone-link-ask button')?.focus();
    else if (asked.current) sheet.current?.querySelector('[data-link-remove]')?.focus();
    asked.current = !!asking.spec;
  }, [asking.spec]);
  const run = async (what, job) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(what);
    setError('');
    try {
      if (await job() !== false) close();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      lock.current = false;
      setBusy('');
    }
  };
  const save = () => run('save', () => actions.updateLink({ id: link.id, from: link.from, to: link.to, mode }));
  // deleteLink reports its own failure (a toast) and answers false.
  const remove = () => run('remove', () => actions.deleteLink(deleteFields(link)));
  const pending = !remote ? false : deleteFields(link).pending;
  const spec = asking.spec;
  return html`<div ref=${sheet} class="phone-agents-spawn phone-link-sheet" data-link-sheet=${link.id}>
    <${SheetTitle} clip>${link.from || '(deleted)'} → ${remote && row.out ? row.other : link.to || '(deleted)'}<//>
    <p class="phone-agent-sheet-meta">#${link.id} · ${row.out ? `from ${group}` : `into ${group}`}${remote ? ` · across nodes${row.state ? `: ${row.state}` : ''}` : ''}</p>
    ${remote || readOnly
      ? html`<p class="phone-agents-note">${modeText(link.mode)}. ${MODE_HINTS[link.mode] || ''}${remote && !readOnly ? ' A cross-node link keeps the mode both nodes agreed; remove it and link again to change it.' : ''}</p>`
      : html`<${ModeField} mode=${mode} setMode=${setMode} disabled=${!!busy} />`}
    ${error && html`<${ErrorNote}>${error}<//>`}
    ${spec
      ? html`<div class="phone-link-ask" role="alertdialog" aria-label=${spec.title}>
          <${SheetHead}>${spec.title}<//>
          <p class="phone-agents-note">${spec.body}</p>
          <${ButtonRow} buttons=${[
            { key: 'keep', label: 'Keep', onClick: () => asking.settle(false) },
            { key: 'ok', label: spec.okLabel || 'Remove', tone: 'no', onClick: () => asking.settle(true) },
          ]} />
        </div>`
      : readOnly
        ? html`<${ButtonRow} buttons=${[{ key: 'close', label: 'Close', onClick: () => close() }]} />`
        : html`<${ButtonRow} buttons=${[
          { key: 'remove', label: busy === 'remove' ? 'Removing…' : pending ? 'Cancel request' : 'Remove link', tone: 'no', disabled: !!busy, onClick: remove,
            attrs: { 'data-link-remove': '' } },
          !remote && { key: 'save', label: busy === 'save' ? 'Saving…' : 'Save', tone: 'pri', disabled: !!busy || mode === link.mode, onClick: save },
        ].filter(Boolean)} />`}
  </div>`;
}

// peerNote says what a cross-node link to the picked peer group will do,
// as the desktop's link dialog does (links-island.js PeerOutcome).
export function peerNote({ connected, reason = '', peer, group }) {
  if (!connected) return `Cross-node links need a hub connection.${reason ? ` ${reason}` : ''}`;
  if (!peer) return 'No peer shares a group with you yet.';
  if (!peer.link_capable) return `${peer.label} runs an older tclaude without group links.`;
  if (!group) return `${peer.label} has not shared a group with you.`;
  if (group.auto) return `No prompt for ${peer.label}'s operator: the link shows connecting until ${peer.label} acknowledges it.`;
  return `Needs ${peer.label}'s approval: a link request goes to its operator and stays pending until they decide, 24 hours at most.`;
}

// useLinkTargets loads the peers and peer groups a cross-node link may go
// to, once: null when this daemon has none (an older one, or a peer's page).
function useLinkTargets(actions, enabled) {
  const [targets, setTargets] = useState(enabled ? undefined : null);
  useEffect(() => {
    if (!enabled) return undefined;
    let live = true;
    actions.loadLinkTargets().then((value) => { if (live) setTargets(value || null); }, () => { if (live) setTargets(null); });
    return () => { live = false; };
  }, [actions, enabled]);
  return targets;
}

// LinkAddSheet adds a link from this group: to another group here or, when
// the node is on the hub, to a group a peer shares; a mode; and whether to
// link back too.
export function LinkAddSheet({ from, snapshot, close, fetchImpl, refresh, notify, actions: given = null, remotePage = !!globalThis.__tclaudeRemoteNode?.id }) {
  const [actions] = useState(() => given || phoneLinkActions({ fetchImpl, refresh, notify }));
  const groups = (snapshot?.groups || []).map((group) => group.name).filter((name) => name && name !== from);
  const targets = useLinkTargets(actions, !remotePage);
  const peers = (targets?.peers || []).filter((peer) => peer?.peer);
  const connected = !!targets?.hub_connected;
  const [target, setTarget] = useState('local');
  const [to, setTo] = useState(groups[0] || '');
  const [peerID, setPeerID] = useState('');
  const [peerGroupID, setPeerGroupID] = useState('');
  const [mode, setMode] = useState('members->members');
  const [bidir, setBidir] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const lock = useRef(false);
  const peer = peers.find((p) => p.peer === peerID) || peers.find((p) => p.link_capable) || null;
  const peerGroups = peer?.link_capable ? (peer.groups || []) : [];
  const peerGroup = peerGroups.find((g) => g.id === peerGroupID) || peerGroups[0] || null;
  const remote = target === 'peer' && connected;
  const ready = remote ? !!peerGroup : !!to;
  useSheetGuard(() => busy);
  const submit = async (event) => {
    event?.preventDefault?.();
    if (!ready || lock.current) return;
    lock.current = true;
    setBusy(true);
    setError('');
    try {
      await actions.createLink(remote
        ? { from, mode, bidir, peer: { id: peer.peer, label: peer.label || peer.peer, groupID: peerGroup.id, groupName: peerGroup.name } }
        : { from, to, mode, bidir });
      close();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      lock.current = false;
      setBusy(false);
    }
  };
  const seg = (key, label, disabled = false) => html`<button data-ui-exempt key=${key} type="button" disabled=${busy || disabled}
    class=${`phone-agents-seg-btn${target === key ? ' on' : ''}`} aria-pressed=${target === key ? 'true' : 'false'}
    onClick=${() => setTarget(key)}>${label}</button>`;
  const far = remote ? `${peer?.label || 'peer'}:${peerGroup?.name || '…'}` : to || 'TO';
  return html`<form class="phone-agents-spawn phone-link-add" data-link-add onSubmit=${submit}>
    <${SheetTitle}>New link<//>
    <p class="phone-agent-sheet-meta">From ${from}. Its members may message the group you pick.</p>
    ${targets && html`<div class="phone-agents-seg" role="group" aria-label="Link to">
      ${seg('local', 'This node')}${seg('peer', 'Peer node 🌐', !connected)}
    </div>`}
    ${remote
      ? peers.length > 0 && html`<label class="phone-agents-label">Peer node
          <select name="link-peer" class="ui-select phone-agents-field" value=${peer?.peer || ''} disabled=${busy}
            onChange=${(event) => { setPeerID(event.currentTarget.value); setPeerGroupID(''); }}>
            ${peers.map((p) => html`<option key=${p.peer} value=${p.peer} disabled=${!p.link_capable} selected=${p.peer === peer?.peer}>${p.label || p.peer}${p.link_capable ? '' : ' (no group links)'}</option>`)}
          </select></label>
        <label class="phone-agents-label">Peer group
          <select name="link-peer-group" class="ui-select phone-agents-field" value=${peerGroup?.id || ''} disabled=${busy || !peerGroups.length}
            onChange=${(event) => setPeerGroupID(event.currentTarget.value)}>
            ${peerGroups.map((g) => html`<option key=${g.id} value=${g.id} selected=${g.id === peerGroup?.id}>${g.name}</option>`)}
          </select></label>`
      : groups.length
        ? html`<label class="phone-agents-label">To
            <select name="link-to" class="ui-select phone-agents-field" value=${to} disabled=${busy} onChange=${(event) => setTo(event.currentTarget.value)}>
              ${groups.map((name) => html`<option key=${name} value=${name} selected=${name === to}>${name}</option>`)}
            </select></label>`
        : html`<${Empty}>No other group here to link to.<//>`}
    ${targets && (target === 'peer' || !connected) && html`<p class="ui-hint phone-link-peer-note" data-peer-note>🌐 ${peerNote({ connected, reason: targets.reason, peer, group: peerGroup })}</p>`}
    <${ModeField} mode=${mode} setMode=${setMode} disabled=${busy} />
    <label class="phone-agents-view-switch">
      <span class="phone-agents-view-text">${remote ? 'Also ask for the link back' : 'Also link back'}<small>${far} → ${from}, the same mode</small></span>
      <${Switch} name="link-bidir" checked=${bidir} disabled=${busy} onChange=${(event) => setBidir(event.currentTarget.checked)} />
    </label>
    ${error && html`<${ErrorNote}>${error}<//>`}
    <${ButtonRow} buttons=${[
      { key: 'cancel', label: 'Cancel', disabled: busy, onClick: () => close() },
      { key: 'add', tone: 'pri', disabled: busy || !ready, onClick: submit,
        label: busy ? 'Linking…' : remote && peerGroup && !peerGroup.auto ? 'Send request' : 'Link' },
    ]} />
  </form>`;
}
