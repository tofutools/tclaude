// modal-message.js — the message, sudo-modal-binding, permission-edit
// and group-create modals.
//
// Extracted from dashboard.js in the Stage 2 module split. The message
// modal reuses modal-cron's target picker; bindSudoModal wires the
// sudo-grant modal (defined in modal-cron) to its DOM controls.

import { $, $$, esc, shortId, pickDirectory } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { recordGroupInteraction } from './last-group.js';
import {
  bindTargetPicker, populateTargetPicker, readTargetPicker, pickCronTargetModal,
  openSudoGrantModal, closeSudoGrantModal, submitSudoGrant, pickSudoAgentModal,
} from './modal-cron.js';
// lastSnapshot lives in dashboard.js; refresh() / toast / openCleanupModal
// in refresh.js. Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, toast, openCleanupModal, bindBackdropDiscard } from './refresh.js';


// --- one-shot message modal -----------------------------------------
// Sends a single immediate message through POST /api/message. The
// modal has two shapes, chosen by the entry-point button:
//
//   - per-agent ✉ → SOLO mode: the shared solo/group target picker
//     (prefix "message-create") picks any agent or any group, exactly
//     as before.
//   - per-group ✉ message → GROUP-SCOPED mode: the modal locks to
//     that one group and replaces the target picker with a checkbox
//     list of its members, all ticked by default. Sending with every
//     box ticked is a plain group multicast; unticking some narrows
//     the send to the chosen subset.
//
// Either way there is no schedule — the send fires once, now.

// messageScopedGroup holds { name, members:[{conv_id,title,online,
// checked}] } while the modal is open in group-scoped mode, and null
// in solo mode. openMessageCreateModal sets it; submitMessageForm
// branches on it.
let messageScopedGroup = null;

// openMessageCreateModal opens the modal. prefill is an optional
// object { from, targetMode, target, groupName } — the context-aware
// entry-point buttons (per-agent ✉, per-group ✉ message) drop the
// relevant defaults in before opening. A prefill naming a group
// (targetMode 'group' + groupName) opens the modal scoped to that
// group; anything else opens the solo/group target picker.
function openMessageCreateModal(prefill) {
  prefill = prefill || {};
  $('#message-create-from').value = prefill.from || '';
  $('#message-create-subject').value = '';
  $('#message-create-body').value = '';
  $('#message-create-error').textContent = '';
  // Enabled by default; group-scoped mode disables it again below
  // when the group has no members to tick (updateMessageMembersCount).
  $('#message-create-submit').disabled = false;
  const scoped = prefill.targetMode === 'group' && !!prefill.groupName;
  if (scoped) {
    setupMessageGroupScope(prefill.groupName);
  } else {
    messageScopedGroup = null;
    $('#message-create-target-row').style.display = '';
    $('#message-create-group-row').style.display = 'none';
    populateTargetPicker('message-create', prefill);
  }
  $('#message-create-title').textContent = scoped
    ? `Send a message to group "${prefill.groupName}"`
    : 'Send a message';
  $('#message-create-desc').textContent = scoped
    ? `Delivers one immediate message to the members of "${prefill.groupName}" ticked below — every member is ticked by default, untick any to send to just a subset. Each recipient gets an inbox row plus a tmux nudge if online.`
    : 'Delivers one immediate message to a single agent, or multicasts it to every member of a group. Each recipient gets an inbox row plus a tmux nudge if online.';
  $('#message-create-modal').classList.add('show');
  setTimeout(() => $('#message-create-from').focus(), 0);
}

// setupMessageGroupScope switches the modal into group-scoped mode:
// it hides the solo/group target picker, shows the member checkbox
// list, and snapshots the group's members (all ticked) into
// messageScopedGroup.
function setupMessageGroupScope(groupName) {
  $('#message-create-target-row').style.display = 'none';
  $('#message-create-group-row').style.display = '';
  const g = (lastSnapshot?.groups || []).find(x => x.name === groupName);
  const members = ((g && g.members) || []).map(m => ({
    conv_id: m.conv_id, title: m.title || '', online: !!m.online, checked: true,
  }));
  messageScopedGroup = { name: groupName, members };
  $('#message-create-group-hint').textContent = members.length
    ? `Members of "${groupName}" — all ticked; untick any to message a subset.`
    : `Group "${groupName}" has no members to message.`;
  renderMessageMembers();
}

// renderMessageMembers redraws the member checkbox list from
// messageScopedGroup (preserving each member's checked state) and
// refreshes the "N of M selected" count.
function renderMessageMembers() {
  if (!messageScopedGroup) return;
  const listEl = $('#message-create-members');
  const members = messageScopedGroup.members;
  if (members.length === 0) {
    listEl.innerHTML = '<div class="cleanup-empty">no members in this group</div>';
  } else {
    listEl.innerHTML = members.map(m => {
      const online = m.online ? '<span class="cleanup-badge online">online</span>' : '';
      return `<div class="cleanup-row"><label>`
        + `<input type="checkbox" data-conv="${esc(m.conv_id)}"${m.checked ? ' checked' : ''} />`
        + `<span class="title">${esc(m.title || '(untitled)')}</span>`
        + `<span class="id">${esc(m.conv_id.slice(0, 8))}</span>`
        + `${online}</label></div>`;
    }).join('');
  }
  updateMessageMembersCount();
}

// updateMessageMembersCount refreshes the "N of M selected" readout
// without re-rendering the whole list (cheap on a checkbox toggle),
// and disables Send when nothing is ticked — an empty group, or every
// box cleared, has no recipient, so the send is blocked at the button
// rather than failing on submit.
function updateMessageMembersCount() {
  if (!messageScopedGroup) return;
  const members = messageScopedGroup.members;
  const n = members.filter(m => m.checked).length;
  $('#message-create-members-count').textContent = `${n} of ${members.length} selected`;
  $('#message-create-submit').disabled = n === 0;
}

function closeMessageCreateModal() {
  $('#message-create-modal').classList.remove('show');
  messageScopedGroup = null;
}

// submitMessageForm POSTs /api/message. In solo mode it reads the
// shared target picker; in group-scoped mode it sends to the ticked
// members — a plain group: multicast when every box is ticked, or an
// explicit member subset when some are unticked. On a multicast it
// reports how many members the send reached; on a solo send whether
// the recipient was nudged live or the row just queued in their inbox.
async function submitMessageForm() {
  const errEl = $('#message-create-error');
  errEl.textContent = '';
  const from = $('#message-create-from').value.trim();
  const subject = $('#message-create-subject').value.trim();
  const bodyText = $('#message-create-body').value;
  // Client-side gates with inline errors — the daemon validates
  // authoritatively too, so this only catches the obvious misses.
  if (!from) {
    errEl.textContent = 'From is required — type a sender agent or use 🔍 to pick.';
    return;
  }
  // Resolve the recipient(s): `mode` drives the success toast, `to`
  // is the raw selector / "group:NAME" token, `members` is an
  // explicit conv-id subset (group-scoped mode only) or null.
  let mode, to, members = null;
  if (messageScopedGroup) {
    mode = 'group';
    to = 'group:' + messageScopedGroup.name;
    const all = messageScopedGroup.members;
    const picked = all.filter(m => m.checked);
    if (picked.length === 0) {
      errEl.textContent = 'Pick at least one recipient — tick the members this message should reach.';
      return;
    }
    // Every member ticked → a plain group: multicast, which tracks the
    // LIVE roster (a member who joined since the modal opened is still
    // reached). A subset → send the explicit conv-id list.
    if (picked.length < all.length) {
      members = picked.map(m => m.conv_id);
    }
  } else {
    const picked = readTargetPicker('message-create');
    mode = picked.mode;
    to = picked.target;
    if (!to) {
      errEl.textContent = mode === 'solo'
        ? 'Target is required — type a title / conv-id or use 🔍 to pick.'
        : 'Pick a group from the dropdown (or create one first via the Groups tab).';
      return;
    }
  }
  if (!bodyText) {
    errEl.textContent = 'Body is required (the message text to send).';
    return;
  }
  const payload = { from, to, subject, body: bodyText };
  if (members) payload.members = members;
  const submitBtn = $('#message-create-submit');
  submitBtn.disabled = true;
  try {
    const r = await fetch('/api/message', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
      return;
    }
    const resp = await r.json();
    if (mode === 'group') {
      const recipients = resp.recipients || [];
      const n = recipients.length;
      const live = recipients.filter(x => x.delivered).length;
      // Members blocked on a human are held, not nudged — call them out
      // separately so "nudged live" isn't conflated with offline-queued.
      const held = recipients.filter(x => x.held).length;
      const heldNote = held ? `, ${held} held (awaiting human input)` : '';
      toast(n
        ? `multicast reached ${n} member${n === 1 ? '' : 's'} of ${resp.via_group || to} (${live} nudged live${heldNote})`
        : `no recipients reached in ${resp.via_group || to} — nothing sent`);
    } else if (resp.held) {
      // Recipient is alive but blocked on a human (a permission prompt or
      // elicitation dialog), so we deliberately did NOT nudge — the
      // keystrokes would be captured as the human's answer. It delivers
      // once they resume.
      toast('message placed in mailbox — recipient is waiting on human input, delivers when they resume');
    } else {
      toast(resp.delivered
        ? 'message sent — recipient nudged live'
        : 'message sent — queued in recipient inbox');
    }
    closeMessageCreateModal();
  } catch (e) {
    errEl.textContent = 'Network error: ' + e;
  } finally {
    // In group-scoped mode the Send button's disabled state tracks
    // the live recipient selection — re-derive it (rather than
    // blindly re-enabling) so a failed request still honours the
    // disabled-when-empty invariant, even if the user cleared the
    // last checkbox while the request was in flight.
    if (messageScopedGroup) updateMessageMembersCount();
    else submitBtn.disabled = false;
  }
}

function bindMessageModal() {
  // Solo/group target picker — markup + mode radios + 🔍 button.
  bindTargetPicker('message-create');
  $('#message-create-cancel').addEventListener('click', closeMessageCreateModal);
  $('#message-create-submit').addEventListener('click', submitMessageForm);
  bindBackdropDiscard('message-create-modal', closeMessageCreateModal);
  // Group-scoped recipient list: per-member checkbox changes + the
  // select all / none shortcuts. The list markup is (re)rendered by
  // renderMessageMembers; these delegated listeners are bound once.
  $('#message-create-members').addEventListener('change', (e) => {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb || !messageScopedGroup) return;
    const m = messageScopedGroup.members.find(x => x.conv_id === cb.getAttribute('data-conv'));
    if (m) m.checked = cb.checked;
    updateMessageMembersCount();
  });
  $('#message-create-members-all').addEventListener('click', () => {
    if (!messageScopedGroup) return;
    messageScopedGroup.members.forEach(m => { m.checked = true; });
    renderMessageMembers();
  });
  $('#message-create-members-none').addEventListener('click', () => {
    if (!messageScopedGroup) return;
    messageScopedGroup.members.forEach(m => { m.checked = false; });
    renderMessageMembers();
  });
  // From picker reuses the cron-pick-target agent overlay.
  $('#message-create-from-pick').addEventListener('click', async () => {
    const conv = await pickCronTargetModal();
    if (conv) $('#message-create-from').value = conv;
  });
}

function bindSudoModal() {
  $('#sudo-grant-open').addEventListener('click', async () => {
    const convID = await pickSudoAgentModal();
    if (convID) openSudoGrantModal(convID);
  });
  $('#sudo-grant-cancel').addEventListener('click', closeSudoGrantModal);
  $('#sudo-grant-submit').addEventListener('click', submitSudoGrant);
  // Select-all / select-none act on every non-disabled checkbox
  // (disabled ones are blocklisted slugs that the server will
  // reject anyway). Per-checkbox onchange handlers update the
  // .checked styling so the visual matches state.
  $('#sudo-grant-select-all').addEventListener('click', () => {
    $$('#sudo-grant-slugs input[type=checkbox]:not([disabled])').forEach(cb => {
      if (!cb.checked) {
        cb.checked = true;
        cb.parentElement.classList.add('checked');
      }
    });
  });
  $('#sudo-grant-select-none').addEventListener('click', () => {
    $$('#sudo-grant-slugs input[type=checkbox]').forEach(cb => {
      if (cb.checked) {
        cb.checked = false;
        cb.parentElement.classList.remove('checked');
      }
    });
  });
  bindBackdropDiscard('sudo-grant-modal', closeSudoGrantModal);
}

// ---- Permanent-permission editor ----------------------------------------
//
// openPermEditModal builds one tri-state row (Default / Grant / Deny)
// per registry slug, pre-selected from the agent's current per-conv
// overrides in the snapshot. Save POSTs the full selection to
// /api/permissions, which diffs it against what's persisted. This is
// the PERMANENT analog of the "+ sudo" elevation — the banner in the
// modal spells out the difference.
let permEditConv = '';
// permEditOwnsGroup: does the agent being edited own at least one group?
// Owner-conferred ("owner_implied") slugs are effectively held by a group
// owner via the daemon's owner-bypass, even at Default, so the effective
// indicator must reflect that — set per-open in openPermEditModal.
let permEditOwnsGroup = false;

// permRowEffective recomputes the "✓ granted / ✗ denied / ✓ via owner"
// indicator on one row from its selected effect, whether the slug is a
// global default, and — for an owner — whether the slug is owner-conferred.
// Mirrors the daemon precedence: an explicit Grant or a global default
// grants; an explicit Deny is authoritative and suppresses the owner
// bypass; otherwise a Default on an owner-conferred slug is held "via
// ownership" when the agent owns a group.
function permRowEffective(row) {
  const active = row.querySelector('.perm-tristate button.active');
  const effect = active ? active.dataset.effect : 'default';
  const inDefault = row.dataset.indefault === '1';
  const ownerImplied = row.dataset.ownerimplied === '1';
  const viaOwner = effect === 'default' && !inDefault && ownerImplied && permEditOwnsGroup;
  const granted = effect === 'grant' || (effect === 'default' && inDefault) || viaOwner;
  const el = row.querySelector('.perm-row-eff');
  if (viaOwner) {
    el.textContent = '✓ via owner';
    el.className = 'perm-row-eff owner';
  } else {
    el.textContent = granted ? '✓ granted' : '✗ denied';
    el.className = 'perm-row-eff ' + (granted ? 'granted' : 'denied');
  }
}

// convOwnedGroups returns the group names the conv owns, read from the
// snapshot's per-agent owned_groups (empty for non-owners / plain convs).
function convOwnedGroups(conv) {
  const a = (lastSnapshot?.agents || []).find(x => x.conv_id === conv);
  return (a && a.owned_groups) || [];
}

function openPermEditModal(conv, label) {
  const snap = lastSnapshot || {};
  const perms = snap.permissions || {};
  const slugs = (snap.slugs || []).slice().sort((a, b) => a.slug < b.slug ? -1 : 1);
  const defaultSet = new Set(perms.defaults || []);
  const overrides = (perms.overrides || {})[conv] || {};
  const ownedGroups = convOwnedGroups(conv);
  permEditConv = conv;
  permEditOwnsGroup = ownedGroups.length > 0;
  $('#perm-edit-subtitle').textContent =
    `Agent: ${label || shortId(conv)} · ${shortId(conv)}`;
  // Owner note: a group owner holds the owner-conferred (👑) slugs below
  // for its owned groups / their members even at Default. Surface that so
  // the human isn't misled by a "✗ denied"-looking Default tri-state.
  const ownerNote = $('#perm-edit-owner-note');
  if (ownerNote) {
    if (permEditOwnsGroup) {
      ownerNote.innerHTML =
        `👑 <strong>Group owner</strong> of ${ownedGroups.map(g => `<code>${esc(g)}</code>`).join(', ')}. ` +
        `Owner-conferred slugs (marked 👑) are effectively held for those groups and their members ` +
        `even at <strong>Default</strong> — shown as “✓ via owner”. A <strong>Deny</strong> still suppresses one.`;
      ownerNote.hidden = false;
    } else {
      ownerNote.hidden = true;
      ownerNote.innerHTML = '';
    }
  }
  $('#perm-edit-error').textContent = '';
  $('#perm-edit-filter').value = '';
  const list = $('#perm-edit-list');
  if (!slugs.length) {
    list.innerHTML = '<div class="empty" style="padding:10px">No permission slugs registered.</div>';
  } else {
    list.innerHTML = slugs.map(s => {
      const cur = overrides[s.slug] || 'default'; // grant | deny | default
      const inDefault = defaultSet.has(s.slug);
      const ownerImplied = !!s.owner_implied;
      const mk = (eff, txt) =>
        `<button type="button" data-effect="${eff}"${cur === eff ? ' class="active"' : ''}>${txt}</button>`;
      const ownerBadge = ownerImplied
        ? ' <span class="owner-badge" title="Group ownership confers this slug for owned groups / their members, without an explicit grant. A per-agent Deny still suppresses it.">👑 owner</span>'
        : '';
      return `<div class="perm-row" data-slug="${esc(s.slug)}" data-indefault="${inDefault ? '1' : '0'}" data-ownerimplied="${ownerImplied ? '1' : '0'}">
        <div class="perm-row-info">
          <span class="perm-row-slug">${esc(s.slug)}${ownerBadge}</span>
          <span class="perm-row-desc" title="${esc(s.description || '')}">${esc(s.description || '')}</span>
        </div>
        <div class="perm-tristate">${mk('default', 'Default')}${mk('grant', 'Grant')}${mk('deny', 'Deny')}</div>
        <span class="perm-row-eff"></span>
      </div>`;
    }).join('');
    list.querySelectorAll('.perm-row').forEach(permRowEffective);
  }
  list.scrollTop = 0;
  $('#perm-edit-modal').classList.add('show');
  setTimeout(() => $('#perm-edit-filter').focus(), 0);
}

function closePermEditModal() {
  $('#perm-edit-modal').classList.remove('show');
}

async function submitPermEdit() {
  const errEl = $('#perm-edit-error');
  errEl.textContent = '';
  if (!permEditConv) { errEl.textContent = 'No agent selected.'; return; }
  const overrides = {};
  $$('#perm-edit-list .perm-row').forEach(row => {
    const active = row.querySelector('.perm-tristate button.active');
    overrides[row.dataset.slug] = active ? active.dataset.effect : 'default';
  });
  if (!Object.keys(overrides).length) { errEl.textContent = 'Nothing to save.'; return; }
  const btn = $('#perm-edit-submit');
  btn.disabled = true;
  try {
    const r = await fetch('/api/permissions', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ conv: permEditConv, overrides }),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
      return;
    }
    const resp = await r.json().catch(() => ({}));
    const n = resp.changed || 0;
    toast(`Permissions saved — ${n} change${n === 1 ? '' : 's'}`);
    closePermEditModal();
    await refresh();
  } catch (e) {
    errEl.textContent = 'Network error: ' + (e.message || e);
  } finally {
    btn.disabled = false;
  }
}

function bindPermEditModal() {
  $('#perm-edit-cancel').addEventListener('click', closePermEditModal);
  $('#perm-edit-submit').addEventListener('click', submitPermEdit);
  // Tri-state clicks are delegated: pick the clicked effect, drop the
  // active class from its siblings, refresh the row's effective hint.
  $('#perm-edit-list').addEventListener('click', (e) => {
    const b = e.target.closest('.perm-tristate button');
    if (!b) return;
    b.parentElement.querySelectorAll('button')
      .forEach(x => x.classList.toggle('active', x === b));
    permRowEffective(b.closest('.perm-row'));
  });
  $('#perm-edit-filter').addEventListener('input', () => {
    const q = $('#perm-edit-filter').value.trim().toLowerCase();
    $$('#perm-edit-list .perm-row').forEach(row => {
      const slug = row.dataset.slug.toLowerCase();
      const desc = (row.querySelector('.perm-row-desc').textContent || '').toLowerCase();
      row.classList.toggle('hidden', q !== '' && !slug.includes(q) && !desc.includes(q));
    });
  });
  $('#perm-edit-reset').addEventListener('click', () => {
    $$('#perm-edit-list .perm-row').forEach(row => {
      row.querySelectorAll('.perm-tristate button')
        .forEach(x => x.classList.toggle('active', x.dataset.effect === 'default'));
      permRowEffective(row);
    });
  });
  bindBackdropDiscard('perm-edit-modal', closePermEditModal);
}

// ---- Group create modal -------------------------------------------------

function openGroupCreateModal() {
  $('#group-create-name').value = '';
  $('#group-create-descr').value = '';
  $('#group-create-cwd').value = '';
  $('#group-create-context').value = '';
  $('#group-create-max-members').value = '';
  $('#group-create-error').textContent = '';
  $('#group-create-modal').classList.add('show');
  setTimeout(() => $('#group-create-name').focus(), 0);
}

function closeGroupCreateModal() {
  $('#group-create-modal').classList.remove('show');
}

async function submitGroupCreate() {
  const name = $('#group-create-name').value.trim();
  const descr = $('#group-create-descr').value.trim();
  const cwd = $('#group-create-cwd').value.trim();
  const context = $('#group-create-context').value.trim();
  const errEl = $('#group-create-error');
  errEl.textContent = '';
  if (!name) {
    errEl.textContent = 'name is required';
    return;
  }
  // Max members: blank means unlimited (0); a negative value is a
  // mistake — surface it rather than letting the daemon clamp it.
  const maxRaw = $('#group-create-max-members').value.trim();
  let maxMembers = 0;
  if (maxRaw !== '') {
    maxMembers = parseInt(maxRaw, 10);
    if (!Number.isInteger(maxMembers) || maxMembers < 0) {
      errEl.textContent = 'max members must be a non-negative integer (0 = unlimited)';
      return;
    }
  }
  const submitBtn = $('#group-create-submit');
  submitBtn.disabled = true;
  try {
    const r = await fetch('/api/groups', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, descr, default_cwd: cwd, default_context: context, max_members: maxMembers }),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    closeGroupCreateModal();
    toast(`group created: ${name}`);
    // Persist the expanded state so the new group shows expanded on next render.
    try { dashPrefs.setItem('tclaude.dash.group.' + name, '1'); } catch (_) {}
    recordGroupInteraction(name);
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submitBtn.disabled = false;
  }
}

// Ask the daemon to open a native directory picker and drop the chosen
// path into the Default cwd field. The browser can't pop an OS folder
// chooser itself, so agentd — running on the human's desktop — does it
// and reports the path back (POST /api/pick-directory). The fetch stays
// pending while the dialog is open; a cancel leaves the field untouched.
async function browseGroupCreateCwd() {
  const input = $('#group-create-cwd');
  const btn = $('#group-create-cwd-browse');
  const errEl = $('#group-create-error');
  errEl.textContent = '';
  const prevLabel = btn.textContent;
  btn.disabled = true;
  btn.textContent = 'Opening…';
  try {
    const res = await pickDirectory({
      startDir: input.value.trim(),
      title: 'Select the group default working directory',
    });
    if (res.error) { errEl.textContent = res.error; return; }
    if (res.canceled) return; // dialog dismissed — leave the field as-is
    input.value = res.path;
    input.focus();
  } finally {
    btn.disabled = false;
    btn.textContent = prevLabel;
  }
}

function bindGroupCreateModal() {
  $('#group-create-open').addEventListener('click', openGroupCreateModal);
  $('#group-create-cwd-browse').addEventListener('click', browseGroupCreateCwd);
  // 🧹 cleanup: the Groups tab's "clean up" button opens the rich
  // multi-category cleanup modal — bulk unjoin / retire / delete /
  // reinstate spanning active agents, retired agents and plain
  // conversations (openCleanupModal mode 'agents').
  $('#cleanup-all-open').addEventListener('click', () => openCleanupModal({ mode: 'agents' }));
  $('#group-create-cancel').addEventListener('click', closeGroupCreateModal);
  $('#group-create-submit').addEventListener('click', submitGroupCreate);
  bindBackdropDiscard('group-create-modal', closeGroupCreateModal);
  $('#group-create-modal').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.target.id === 'group-create-name' || e.target.id === 'group-create-descr' || e.target.id === 'group-create-cwd')) {
      e.preventDefault();
      submitGroupCreate();
    }
  });
}

export {
  openMessageCreateModal, bindMessageModal, bindSudoModal,
  openPermEditModal, bindPermEditModal, bindGroupCreateModal,
};
