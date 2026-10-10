import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { BoardItems } from './fleet-admin-board-items.js';

const html = htm.bind(h);

// Content boards: share content between nodes through the hub without
// linking them (no trust, no peer access). The hub decides membership; content
// is encrypted end to end and items are signed by whoever posted them. This
// page covers boards and their members; a board's shared config (posting,
// reading, importing) is fleet-admin-board-items.js.

export const HUB_TRUST = "The hub decides who is a member: a dishonest hub operator could block a board or keep a removed member getting new posts, but cannot read anything or fake a post. Use a hub you trust.";

// ROLES in plain words; the wire values stay owner / publisher / reader.
export const ROLES = Object.freeze({ owner: 'owner', publisher: 'can post', reader: 'can read' });
export const roleText = (role) => ROLES[role] || role || '—';

// INVITE_TTLS are the expiry choices (the daemon accepts 1 minute to 7 days).
const INVITE_TTLS = Object.freeze([
  { s: 3600, label: '1 hour' },
  { s: 86400, label: '1 day' },
  { s: 7 * 86400, label: '7 days' },
]);

function errText(error) { return error?.message || String(error); }

// inviteHub reads the hub URL out of a pasted invite (board1_ + base64url
// JSON), for the "wrong hub" message only; the daemon does the real check.
export function inviteHub(token) {
  try {
    const raw = String(token || '').trim().replace(/^board1_/, '').replace(/-/g, '+').replace(/_/g, '/');
    const hub = JSON.parse(globalThis.atob(raw + '='.repeat((4 - (raw.length % 4)) % 4)))?.hub;
    return typeof hub === 'string' ? hub : '';
  } catch (_) {
    return '';
  }
}

// joinError says what to do about a refused join, in plain words.
export function joinError(e, token, hubURL) {
  switch (e?.code) {
    case 'hub_required': return 'Set this node\'s hub first (settings… at the top of ⚙ Fleet), then paste the invite again.';
    case 'hub_mismatch': {
      const hub = inviteHub(token);
      return `This invite is for ${hub ? `the hub at ${hub}` : 'another hub'}, but this node uses ${hubURL || 'no hub'}. Switch this node's hub first, or ask for an invite on your hub.`;
    }
    case 'board_invite': return 'That invite does not work: it is mistyped, expired, already used, or was cancelled. Ask the board owner for a new one.';
    case 'board_changed': return 'The board changed its key after this invite was made, or is frozen right now. Ask the board owner for a new invite.';
    default: return errText(e);
  }
}

function memberName(instance, view) {
  if (instance === view.self?.id) return 'this node';
  return [...(view.trusted || []), ...(view.waiting || [])].find((p) => p.id === instance)?.label || '';
}

function limits(b) {
  const parts = [];
  if (b.max_members) parts.push(`${b.max_members} members`);
  if (b.quota_bytes) parts.push(`${Math.round(b.quota_bytes / (1 << 20))} MiB`);
  if (b.max_versions) parts.push(`${b.max_versions} versions`);
  return parts.join(' · ') || '—';
}

// BoardDetail lists a board's members; an owner also invites, changes roles,
// removes members and changes the key.
function BoardDetail({ board, boards = [], view, actions, confirm, toast, copy, onChanged }) {
  const [members, setMembers] = useState(null);
  const [tick, setTick] = useState(0);
  const [invite, setInvite] = useState({ role: 'reader', ttl: 3600 });
  const [token, setToken] = useState(null);
  const owner = board.role === 'owner';
  useEffect(() => {
    let off = false;
    actions.boardMembers(board.id).then((m) => { if (!off) setMembers(m); }).catch((e) => { if (!off) setMembers({ error: errText(e) }); });
    return () => { off = true; };
  }, [board.id, tick]);
  const reload = () => setTick((n) => n + 1);
  const fail = (what) => (e) => toast(`${what} failed: ${errText(e)}`, true);
  const who = (m) => memberName(m.instance, view) || m.instance;

  const createInvite = () => confirm({
    title: `Invite someone to ${board.name}?`,
    body: `Anyone holding the invite can join ${board.name} once, within ${INVITE_TTLS.find((t) => t.s === invite.ttl)?.label || 'the expiry'}, and ${invite.role === 'publisher' ? 'post to it and read everything on it' : 'read everything on it'}. Joining gives no access to this node. The invite is shown only once; send it over a channel you trust.`,
    okLabel: 'Create invite',
    busyLabel: 'Creating…',
    action: () => actions.createBoardInvite(board.id, invite.role, invite.ttl),
  }).then((r) => { if (r?.token) setToken(r); }).catch(fail('Invite'));
  const cancelInvite = () => confirm({
    title: 'Cancel this invite?',
    body: 'The invite stops working. Anyone who already joined with it stays a member.',
    okLabel: 'Cancel invite',
    busyLabel: 'Cancelling…',
    action: () => actions.revokeBoardInvite(board.id, token.token_id),
  }).then((r) => { if (r) { setToken(null); toast('Invite cancelled', false); } }).catch(fail('Cancel invite'));
  const setRole = (m, role) => confirm({
    title: `Change ${who(m)} to ${roleText(role)}?`,
    body: role === 'owner'
      ? `${m.instance} becomes an owner of ${board.name}: it can invite, remove and change the role of anyone, including you.`
      : `${m.instance} ${role === 'publisher' ? 'can post to and read' : 'can only read'} ${board.name}.`,
    okLabel: 'Change role',
    busyLabel: 'Saving…',
    action: () => actions.setBoardMember(board.id, m.instance, role),
  }).then((r) => {
    if (r) toast('Role changed', false);
    reload();
    // Demoting this node changes what it may do here.
    if (r && m.instance === view.self?.id) onChanged();
  }).catch((e) => { fail('Role')(e); reload(); });
  const remove = (m) => confirm({
    title: `Remove ${who(m)} from ${board.name}?`,
    body: `${m.instance} loses access to ${board.name} now. What it already downloaded stays with it. To be sure a dishonest hub cannot pass it new posts, change the key afterwards.`,
    okLabel: 'Remove',
    busyLabel: 'Removing…',
    action: () => actions.removeBoardMember(board.id, m.instance),
  }).then((r) => { if (r) { toast('Member removed', false); reload(); } }).catch(fail('Remove'));
  const rotate = () => confirm({
    title: `Change ${board.name}'s key?`,
    body: `Everything posted from now on uses a new key that only the current members get, so members removed earlier cannot read it even through the hub. Unused invites stop working.`,
    okLabel: 'Change key',
    busyLabel: 'Changing…',
    action: () => actions.rotateBoardKey(board.id),
  }).then((r) => { if (r) { toast('Key changed', false); setToken(null); onChanged(); } }).catch(fail('Change key'));

  return html`<div class="fa-board-detail" id="fleet-board-detail" data-board=${board.id}>
    <${BoardItems} board=${board} boards=${boards} name=${(id) => memberName(id, view)} actions=${actions} confirm=${confirm} toast=${toast} />
    <div class="fa-row"><b>Members</b></div>
    ${members?.error ? html`<div class="fa-danger">${members.error}</div>` : !members ? html`<div class="muted">Loading members…</div>` : html`<table class="fa-table" id="fleet-board-members">
      <thead><tr><th>Member</th><th>Role</th><th></th></tr></thead>
      <tbody>${members.map((m) => html`<tr key=${m.instance} data-member=${m.instance}>
        <td>${memberName(m.instance, view) ? html`${memberName(m.instance, view)} ` : ''}<code>${m.instance}</code></td>
        <td>${owner ? html`<select data-board="role" value=${m.role} onChange=${(e) => setRole(m, e.currentTarget.value)}>
            ${Object.entries(ROLES).map(([v, l]) => html`<option key=${v} value=${v} selected=${m.role === v}>${l}</option>`)}</select>` : roleText(m.role)}</td>
        <td class="fa-acts">${owner && m.instance !== view.self?.id && html`<button type="button" data-board="remove" onClick=${() => remove(m)}>Remove…</button>`}</td>
      </tr>`)}</tbody></table>`}
    ${owner && html`<div class="fa-row" id="fleet-board-invite">
      Invite someone who <select id="fleet-board-invite-role" value=${invite.role} onChange=${(e) => setInvite({ ...invite, role: e.currentTarget.value })}>
        <option value="reader" selected=${invite.role === 'reader'}>can read</option><option value="publisher" selected=${invite.role === 'publisher'}>can post</option></select>
      valid for <select id="fleet-board-invite-ttl" value=${String(invite.ttl)} onChange=${(e) => setInvite({ ...invite, ttl: Number(e.currentTarget.value) })}>
        ${INVITE_TTLS.map((t) => html`<option key=${t.s} value=${String(t.s)} selected=${invite.ttl === t.s}>${t.label}</option>`)}</select>
      <button type="button" class="primary" id="fleet-board-invite-create" disabled=${!!board.frozen} title=${board.frozen ? 'The hub froze this board: no one can join until it is unfrozen' : ''} onClick=${createInvite}>Create invite…</button>
      <span class="spacer"></span>
      <button type="button" id="fleet-board-rotate" onClick=${rotate}>Change key…</button>
    </div>`}
    ${token?.token && html`<div class="fa-hub-token" id="fleet-board-new-invite">Invite, shown once (expires ${new Date(token.expires_at).toLocaleString()}): <code>${token.token}</code>
      <button type="button" class="fa-link" onClick=${() => copy(token.token).then(() => toast('Invite copied', false)).catch(() => toast('Copy it from the page', true))}>copy</button>
      <button type="button" class="fa-link" id="fleet-board-invite-cancel" onClick=${cancelInvite}>cancel invite…</button>
      <button type="button" class="fa-link" onClick=${() => setToken(null)}>hide</button></div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation boards members --board ${board.id}</code>${owner ? html`, <code>invite --board ${board.id} --role reader --ttl 1h</code>` : ''}</div>
  </div>`;
}

// BoardsPage lists this node's boards: join by pasting an invite, create one,
// open a board for its members, or leave it.
export function BoardsPage({ view, actions, confirm, toast, copy }) {
  const [boards, setBoards] = useState(null);
  const [tick, setTick] = useState(0);
  const [token, setToken] = useState('');
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [joinFail, setJoinFail] = useState('');
  const [open, setOpen] = useState('');
  useEffect(() => {
    let off = false;
    actions.boards().then((b) => { if (!off) setBoards(b); }).catch((e) => { if (!off) setBoards({ error: errText(e) }); });
    return () => { off = true; };
  }, [tick]);
  const reload = () => setTick((n) => n + 1);

  const join = () => {
    const t = token.trim();
    if (!t || busy) return;
    setBusy(true); setJoinFail('');
    actions.joinBoard(t).then((b) => { setToken(''); toast(`Joined ${b?.name || 'the board'} (${roleText(b?.role)})`, false); setOpen(b?.id || ''); reload(); })
      .catch((e) => setJoinFail(joinError(e, t, view.self?.hubURL)))
      .finally(() => setBusy(false));
  };
  const create = () => {
    const n = name.trim();
    if (!n || busy) return;
    setBusy(true);
    actions.createBoard(n).then((b) => { setName(''); toast(`Created ${b?.name || n}; you are its owner`, false); setOpen(b?.id || ''); reload(); })
      .catch((e) => toast(`Create failed: ${e?.code === 'board_create' ? 'creating a board needs this node to be admitted to the hub' : errText(e)}`, true))
      .finally(() => setBusy(false));
  };
  const leave = (b) => confirm({
    title: `Leave ${b.name}?`,
    body: `This node stops being a member of ${b.name} and loses access to it; what it already downloaded stays here. Coming back needs a new invite.${b.role === 'owner' ? ' The last owner cannot leave; make someone else an owner first.' : ''}`,
    okLabel: 'Leave board',
    busyLabel: 'Leaving…',
    action: () => actions.leaveBoard(b.id),
  }).then((r) => { if (r) { toast(`Left ${b.name}`, false); if (open === b.id) setOpen(''); reload(); } }).catch((e) => toast(`Leave failed: ${errText(e)}`, true));

  const list = Array.isArray(boards) ? boards : [];
  return html`<div class="fa-boards" id="fleet-boards">
    <div class="muted fa-wrap">Boards share content with other nodes through the hub, without linking the nodes or giving anyone access to this machine. ${HUB_TRUST}</div>
    <div class="fa-row" id="fleet-board-join">
      <input id="fleet-board-token" class="fa-grow" placeholder="Paste an invite to join a board" autocomplete="off" spellcheck="false" value=${token}
        onInput=${(e) => { setToken(e.currentTarget.value); setJoinFail(''); }} onKeyDown=${(e) => { if (e.key === 'Enter') join(); }} />
      <button type="button" class="primary" id="fleet-board-join-btn" disabled=${busy || !token.trim()} onClick=${join}>Join</button>
      <input id="fleet-board-name" placeholder="New board name" value=${name} onInput=${(e) => setName(e.currentTarget.value)} onKeyDown=${(e) => { if (e.key === 'Enter') create(); }} />
      <button type="button" id="fleet-board-create" disabled=${busy || !name.trim()} onClick=${create}>Create</button>
    </div>
    ${joinFail && html`<div class="fa-danger" id="fleet-board-join-error" role="alert">${joinFail}</div>`}
    ${boards?.error ? html`<div class="fa-danger">${boards.error}</div>` : !boards ? html`<div class="muted">Loading…</div>` : !list.length ? html`<div class="muted" id="fleet-boards-empty">No boards yet. Paste an invite above, or create one.</div>` : html`<table class="fa-table" id="fleet-boards-list">
      <thead><tr><th>Board</th><th>You</th><th>Limits</th><th></th></tr></thead>
      <tbody>${list.map((b) => html`<tr key=${b.id} data-board-id=${b.id} class=${open === b.id ? 'on' : ''}>
        <td><button type="button" class="fa-link" data-board="open" onClick=${() => setOpen(open === b.id ? '' : b.id)}>${b.name || b.id}</button>${b.frozen ? html` <span class="fa-warn-chip" title="The hub froze this board: no posting or joining until it is unfrozen">frozen</span>` : ''}</td>
        <td>${roleText(b.role)}</td>
        <td class="muted">${limits(b)}</td>
        <td class="fa-acts"><button type="button" data-board="leave" onClick=${() => leave(b)}>Leave…</button></td>
      </tr>`)}</tbody></table>`}
    ${list.find((b) => b.id === open) && html`<${BoardDetail} key=${open} board=${list.find((b) => b.id === open)} boards=${list} view=${view} actions=${actions} confirm=${confirm} toast=${toast} copy=${copy} onChanged=${reload} />`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation boards list</code>, <code>join --token …</code>, <code>create --name …</code></div>
  </div>`;
}

// HubBoards is the hub moderation list for admins holding hub.boards.manage:
// board metadata only — the hub never holds keys or readable content.
export function HubBoards({ actions, confirm, toast }) {
  const [boards, setBoards] = useState(null);
  const [tick, setTick] = useState(0);
  const [edit, setEdit] = useState(null);
  useEffect(() => {
    let off = false;
    actions.hubBoards().then((b) => { if (!off) setBoards(b); }).catch((e) => { if (!off) setBoards({ error: errText(e) }); });
    return () => { off = true; };
  }, [tick]);
  const reload = () => setTick((n) => n + 1);
  const fail = (what) => (e) => toast(`${what} failed: ${errText(e)}`, true);
  const freeze = (b) => confirm({
    title: `${b.frozen ? 'Unfreeze' : 'Freeze'} ${b.name || b.id}?`,
    body: b.frozen ? `Members of ${b.name || b.id} can post and join again.` : `Posting to and joining ${b.name || b.id} stop until it is unfrozen. Nothing is deleted.`,
    okLabel: b.frozen ? 'Unfreeze' : 'Freeze',
    busyLabel: 'Saving…',
    action: () => actions.patchHubBoard(b.id, { frozen: !b.frozen }),
  }).then((r) => { if (r) reload(); }).catch(fail('Freeze'));
  const remove = (b) => confirm({
    title: `Delete ${b.name || b.id} from the hub?`,
    body: `The board and everything stored for it on the hub are deleted for every member, and cannot be brought back. Members keep what they already downloaded.`,
    okLabel: 'Delete board',
    busyLabel: 'Deleting…',
    action: () => actions.deleteHubBoard(b.id),
  }).then((r) => { if (r) { toast('Board deleted', false); reload(); } }).catch(fail('Delete'));
  const save = () => {
    const patch = { quota_bytes: Math.round(Number(edit.mib) * (1 << 20)), max_members: Number(edit.members), max_versions: Number(edit.versions) };
    if (!Object.values(patch).every((v) => Number.isInteger(v) && v > 0)) { toast('Limits must be positive whole numbers', true); return; }
    confirm({
      title: `Change ${edit.board.name || edit.board.id}'s limits?`,
      body: `${edit.members} members, ${edit.mib} MiB, ${edit.versions} versions. Lowering a limit never removes members or content already there; it only stops growth past it.`,
      okLabel: 'Save limits',
      busyLabel: 'Saving…',
      action: () => actions.patchHubBoard(edit.board.id, patch),
    }).then((r) => { if (r) { setEdit(null); reload(); } }).catch(fail('Limits'));
  };
  const list = Array.isArray(boards) ? boards : [];
  return html`<div class="fa-hub-boards" id="fleet-hub-boards">
    ${boards?.error ? html`<div class="fa-danger">${boards.error}</div>` : !boards ? html`<div class="muted">Loading…</div>` : !list.length ? html`<div class="muted">No boards.</div>` : html`<table class="fa-table">
      <thead><tr><th>Board</th><th>ID</th><th>Limits</th><th></th></tr></thead>
      <tbody>${list.map((b) => html`<tr key=${b.id} data-hub-board=${b.id}>
        <td>${b.name || '—'}${b.frozen ? html` <span class="fa-warn-chip">frozen</span>` : ''}</td><td><code>${b.id}</code></td>
        <td>${edit?.board.id === b.id ? html`<span class="fa-row">
            <input class="fa-num" aria-label="members" value=${edit.members} onInput=${(e) => setEdit({ ...edit, members: e.currentTarget.value })} /> members
            <input class="fa-num" aria-label="MiB" value=${edit.mib} onInput=${(e) => setEdit({ ...edit, mib: e.currentTarget.value })} /> MiB
            <input class="fa-num" aria-label="versions" value=${edit.versions} onInput=${(e) => setEdit({ ...edit, versions: e.currentTarget.value })} /> versions</span>` : limits(b)}</td>
        <td class="fa-acts">${edit?.board.id === b.id
          ? html`<button type="button" class="primary" data-hub-board-act="save" onClick=${save}>Save…</button> <button type="button" onClick=${() => setEdit(null)}>Cancel</button>`
          : html`<button type="button" data-hub-board-act="limits" onClick=${() => setEdit({ board: b, members: String(b.max_members || ''), mib: String(Math.round((b.quota_bytes || 0) / (1 << 20)) || ''), versions: String(b.max_versions || '') })}>Limits…</button>
            <button type="button" data-hub-board-act="freeze" onClick=${() => freeze(b)}>${b.frozen ? 'Unfreeze…' : 'Freeze…'}</button>
            <button type="button" data-hub-board-act="delete" onClick=${() => remove(b)}>Delete…</button>`}</td>
      </tr>`)}</tbody></table>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation hub boards list|set|delete --board ID</code></div>
  </div>`;
}
