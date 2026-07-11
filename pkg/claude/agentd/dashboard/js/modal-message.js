// modal-message.js — the message, sudo-modal-binding, permission-edit
// and group-create modals.
//
// Extracted from dashboard.js in the Stage 2 module split. The message
// modal reuses modal-cron's target picker; bindSudoModal wires the
// sudo-grant modal (defined in modal-cron) to its DOM controls.

import { $, $$, esc, shortId, shortAgentId, idTooltip, pickDirectory } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { recordGroupInteraction } from './last-group.js';
import {
  bindTargetPicker, populateTargetPicker, readTargetPicker, pickCronTargetModal,
  openSudoGrantModal, closeSudoGrantModal, submitSudoGrant, pickSudoAgentModal,
} from './modal-cron.js';
// lastSnapshot lives in dashboard.js; refresh() / toast / openCleanupModal
// in refresh.js. Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, toast, openCleanupModal, openDeleteRetiredPreview, bindBackdropDiscard } from './refresh.js';
// Party profile picker (JOH-356): the create-group dialog can start from a
// template / summoning circle. The circle readback + roster preview reuse the
// shared template helpers, and the "⧉ manage circles…" affordance opens the
// same templates manager the Groups cog does. wizWord swaps the vocabulary for
// 🧙 wizard mode in the JS-rendered spots (dropdown options, toasts).
import { openTemplatesManageModal, templateReadbackBadges, templateRosterRowsHTML } from './modal-templates.js';
import { wizWord } from './slop.js';


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

// messageScopedGroup holds { name, members:[{agent_id,conv_id,title,
// online,checked}] } while the modal is open in group-scoped mode, and
// null in solo mode. openMessageCreateModal sets it; submitMessageForm
// branches on it. The subset send leads with agent_id (JOH-27, the
// rotation-immune key the backend's fanOutToGroup accepts), keeping
// conv_id as the snapshot / display fallback.
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
    agent_id: m.agent_id || '', conv_id: m.conv_id, title: m.title || '',
    online: !!m.online, checked: true,
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
      // Lead the id column with the stable agent_id (conv-id prefix as the
      // fallback), showing the full "agent_id / conv-id" pair on hover. The
      // checkbox stays keyed by conv_id — every member has one, so it's a
      // stable internal handle for the change listener regardless of agent_id.
      return `<div class="cleanup-row"><label>`
        + `<input type="checkbox" data-conv="${esc(m.conv_id)}"${m.checked ? ' checked' : ''} />`
        + `<span class="title">${esc(m.title || '(untitled)')}</span>`
        + `<span class="id" title="${esc(idTooltip(m.agent_id, m.conv_id))}">${esc(shortAgentId(m.agent_id, m.conv_id))}</span>`
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
  // is the raw selector / "group:NAME" token, `members` is an explicit
  // agent-id subset (group-scoped mode only; conv-id fallback) or null.
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
    // reached). A subset → send the explicit member list, keyed by stable
    // agent_id (the backend's fanOutToGroup accepts agent_id OR conv_id, so
    // a pre-identity member with no agent_id still resolves via its conv_id).
    if (picked.length < all.length) {
      members = picked.map(m => m.agent_id || m.conv_id);
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
    // Delivery is async (JOH-310): the send only confirms the message was
    // queued; the per-agent worker delivers (or holds for human-input) after
    // this returns. So we report "queued", not a live delivered/held verdict.
    if (mode === 'group') {
      const recipients = resp.recipients || [];
      const n = recipients.length;
      const queued = recipients.filter(x => x.queued).length;
      toast(n
        ? `multicast queued for ${queued} member${queued === 1 ? '' : 's'} of ${resp.via_group || to}`
        : `no recipients reached in ${resp.via_group || to} — nothing sent`);
    } else {
      const ahead = (resp.pending || 0) - 1;
      toast(ahead > 0
        ? `message queued in recipient inbox (${ahead} ahead in delivery queue)`
        : 'message queued in recipient inbox');
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
  // From picker reuses the cron-pick-target agent overlay, which resolves
  // to the picked agent's stable agent_id (conv-id fallback) — a selector
  // the ResolveSelector-backed From field accepts (JOH-312).
  $('#message-create-from-pick').addEventListener('click', async () => {
    const picked = await pickCronTargetModal();
    if (picked) $('#message-create-from').value = picked;
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
// permEditOwnsGroup: does the agent being edited own at least one group?
// Owner-conferred ("owner_implied") slugs are effectively held by a group
// owner via the daemon's owner-bypass, even at Default, so the effective
// indicator must reflect that — set per-open in openPermEditor.
let permEditOwnsGroup = false;
// permEditOwnedGroups names the group(s) the edited subject owns, for the
// owner-note. For a live agent it comes from the snapshot; for a to-be-spawned
// agent it is the destination group, gated on the spawn dialog's owner checkbox.
let permEditOwnedGroups = [];
// permEditOnSave is the save sink for the active editor, set per-open so the
// shared build/submit logic serves two callers: the conv-backed editor POSTs
// the full selection to /api/permissions (the daemon diffs it), and the
// pre-spawn buffer editor writes the spawn dialog's in-memory overrides. It
// receives the collected slug→effect map and returns a Promise; throwing
// surfaces on the modal's error line. null means no editor is open.
let permEditOnSave = null;

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

// openPermEditor is the shared renderer behind both permission editors. It
// builds one tri-state row per registry slug, pre-selected from `overrides`
// (slug→'grant'|'deny'|'default'), wires the owner-note + "via owner" hints
// from ownsGroup/ownedGroups, and stows `onSave` as the submit sink. The
// conv-backed editor (openPermEditModal) and the pre-spawn buffer editor
// (openSpawnPermEditor) differ only in their seed + save; everything visual is
// shared so the spawn dialog gets the identical editor the live-agent path has.
function openPermEditor({ subtitle, overrides, ownsGroup, ownedGroups, onSave }) {
  const snap = lastSnapshot || {};
  const perms = snap.permissions || {};
  const slugs = (snap.slugs || []).slice().sort((a, b) => a.slug < b.slug ? -1 : 1);
  const defaultSet = new Set(perms.defaults || []);
  overrides = overrides || {};
  permEditOwnsGroup = !!ownsGroup;
  permEditOwnedGroups = ownedGroups || [];
  permEditOnSave = onSave;
  $('#perm-edit-subtitle').textContent = subtitle || '';
  // Owner note: a group owner holds the owner-conferred (👑) slugs below
  // for its owned groups / their members even at Default. Surface that so
  // the human isn't misled by a "✗ denied"-looking Default tri-state.
  const ownerNote = $('#perm-edit-owner-note');
  if (ownerNote) {
    if (permEditOwnsGroup && permEditOwnedGroups.length) {
      ownerNote.innerHTML =
        `👑 <strong>Group owner</strong> of ${permEditOwnedGroups.map(g => `<code>${esc(g)}</code>`).join(', ')}. ` +
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

// openPermEditModal is the conv-backed (live-agent) editor: it seeds from the
// snapshot's per-conv overrides and saves by POSTing the full selection to
// /api/permissions, which diffs it against what is persisted.
function openPermEditModal(conv, label) {
  const overrides = ((lastSnapshot?.permissions?.overrides) || {})[conv] || {};
  const ownedGroups = convOwnedGroups(conv);
  openPermEditor({
    subtitle: `Agent: ${label || shortId(conv)} · ${shortId(conv)}`,
    overrides,
    ownsGroup: ownedGroups.length > 0,
    ownedGroups,
    onSave: async (selection) => {
      const r = await fetch('/api/permissions', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ conv, overrides: selection }),
      });
      if (!r.ok) {
        throw new Error((await r.text()) || ('HTTP ' + r.status));
      }
      const resp = await r.json().catch(() => ({}));
      const n = resp.changed || 0;
      toast(`Permissions saved — ${n} change${n === 1 ? '' : 's'}`);
      await refresh();
    },
  });
}

// openSpawnPermEditor is the pre-spawn (buffer) editor: the agent does not
// exist yet, so it seeds from an in-memory override map the spawn dialog keeps
// and saves by handing the non-default selection back to `onSave` (no network).
// ownsGroup mirrors the spawn dialog's "Group owner" checkbox so the "via
// owner" hints preview accurately; group is the spawn destination.
function openSpawnPermEditor({ overrides, ownsGroup, group, label, onSave }) {
  openPermEditor({
    subtitle: `New agent${label ? ` “${label}”` : ''} → ${group} · applied when it spawns`,
    overrides: overrides || {},
    ownsGroup: !!ownsGroup,
    ownedGroups: ownsGroup ? [group] : [],
    onSave: (selection) => {
      // Keep only the real overrides (grant/deny) in the dialog's buffer, so
      // the indicator counts intent and the spawn body stays terse.
      const kept = {};
      Object.keys(selection).forEach(slug => {
        if (selection[slug] === 'grant' || selection[slug] === 'deny') kept[slug] = selection[slug];
      });
      onSave(kept);
    },
  });
}

function closePermEditModal() {
  $('#perm-edit-modal').classList.remove('show');
}

// submitPermEdit collects the full tri-state selection from the DOM and hands
// it to the active editor's onSave sink (set per-open by openPermEditor). The
// conv path POSTs + refreshes; the buffer path writes the spawn dialog's
// overrides. Both share this UI shell: disable while saving, surface any thrown
// error on the modal's error line, close on success.
async function submitPermEdit() {
  const errEl = $('#perm-edit-error');
  errEl.textContent = '';
  if (!permEditOnSave) { errEl.textContent = 'No save target.'; return; }
  const overrides = {};
  $$('#perm-edit-list .perm-row').forEach(row => {
    const active = row.querySelector('.perm-tristate button.active');
    overrides[row.dataset.slug] = active ? active.dataset.effect : 'default';
  });
  const btn = $('#perm-edit-submit');
  btn.disabled = true;
  try {
    await permEditOnSave(overrides);
    closePermEditModal();
  } catch (e) {
    errEl.textContent = (e && e.message) || ('Error: ' + e);
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
//
// The dialog creates an EMPTY group by default ("(blank party)"), but can also
// start from a template / summoning circle (JOH-356): pick a circle and the
// group's own editable copy of descr / startup context is prefilled, a roster
// readback appears, the per-instantiation Task field is surfaced, and submit
// routes through the template instantiate path (spawns the whole roster) instead
// of the empty-group create. Agent : spawn-profile :: party : template.

// gcTemplateCache holds a freshly-fetched template list for the picker, or null
// to fall back to the live snapshot. It exists because opening "⧉ manage
// circles…" stacks the templates manager over this (refresh-suspending) modal,
// so lastSnapshot.templates goes stale while a circle is created/edited there —
// on the manager's close we fetch /api/templates directly and hold the result
// here so a just-created circle is not only visible in the dropdown but also
// resolvable (selectable + submittable) by every reader below.
let gcTemplateCache = null;

// groupCreateTemplates returns the templates the picker should offer — the
// freshly-fetched list when we have one (after managing circles), else the live
// snapshot.
function groupCreateTemplates() {
  if (gcTemplateCache) return gcTemplateCache;
  return (lastSnapshot && lastSnapshot.templates) || [];
}

// selectedGroupCreateTemplate resolves the party-profile dropdown's current
// value to its full template object, or null for "(blank party)".
function selectedGroupCreateTemplate() {
  const name = $('#group-create-template').value;
  if (!name) return null;
  return groupCreateTemplates().find(t => t.name === name) || null;
}

function groupCreateMirrorOptions() {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const opts = [`<option value="">${wizWord('template settings (top-level)', 'circle lore (top-level)')}</option>`];
  for (const g of groups) {
    if (g && g.name) opts.push(`<option value="${esc(g.name)}">${esc(g.name)}</option>`);
  }
  return opts.join('');
}

function groupCreateMirrorSource() {
  const sel = $('#group-create-source');
  return sel ? sel.value.trim() : '';
}

function groupCreateSnapshot(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  return groups.find(g => g && g.name === groupName) || null;
}

function combineGroupAndTemplateContext(groupContext, templateContext) {
  const group = String(groupContext || '').trim();
  const tmpl = String(templateContext || '').trim();
  if (group && tmpl) {
    return `## Mirrored group context\n\n${group}\n\n## Template context\n\n${tmpl}`;
  }
  return group || tmpl;
}

function prefillGroupCreateFromSource(groupName) {
  const g = groupCreateSnapshot(groupName);
  if (!g) return;
  const tmpl = selectedGroupCreateTemplate();
  $('#group-create-descr').value = g.descr || '';
  $('#group-create-cwd').value = g.default_cwd || '';
  $('#group-create-context').value = combineGroupAndTemplateContext(
    g.default_context,
    tmpl && tmpl.default_context);
}

// populateGroupCreateTemplates fills the party-profile dropdown with a blank
// default + every template. preset preselects a named circle (the redirected
// "from template" cog shortcut); a preset that no longer exists falls back to
// blank. Preserves nothing else — callers re-apply the selection's effects.
function populateGroupCreateTemplates(preset) {
  const sel = $('#group-create-template');
  const templates = groupCreateTemplates();
  const opts = [`<option value="">${wizWord('(blank party)', '(no circle — a blank party)')}</option>`];
  for (const t of templates) opts.push(`<option value="${esc(t.name)}">${esc(t.name)}</option>`);
  sel.innerHTML = opts.join('');
  if (preset && templates.some(t => t.name === preset)) sel.value = preset;
}

// applyGroupCreateTemplate reacts to a party-profile selection: prefill the
// group's OWN editable copy of descr / startup context from the picked circle
// (or clear them back for "(blank party)"), toggle the Task field + roster
// preview + Max-members visibility, and re-flavour the submit button. The
// prefill OVERWRITES those template-derived fields — they are the circle's
// suggested starting point, edited freely before submit; the stored template is
// never touched. Name, cwd and max-members are user fields and are not prefilled
// (a template carries no name or cwd; max-members is not honoured by the
// instantiate path, so its row is hidden while a circle is picked).
function applyGroupCreateTemplate() {
  const t = selectedGroupCreateTemplate();
  const taskRow = $('#group-create-task-row');
  const previewRow = $('#group-create-template-preview-row');
  const maxRow = $('#group-create-max-members-row');
  const sourceRow = $('#group-create-source-row');
  const parentRow = $('#group-create-parent-row');
  const submitBtn = $('#group-create-submit');
  if (!t) {
    $('#group-create-descr').value = '';
    $('#group-create-context').value = '';
    $('#group-create-task').value = '';
    $('#group-create-source').value = '';
    $('#group-create-parent').checked = false;
    sourceRow.style.display = 'none';
    parentRow.style.display = 'none';
    taskRow.style.display = 'none';
    previewRow.style.display = 'none';
    maxRow.style.display = '';
    submitBtn.textContent = 'Create';
    return;
  }
  const source = groupCreateMirrorSource();
  if (source) {
    prefillGroupCreateFromSource(source);
  } else {
    $('#group-create-descr').value = t.descr || '';
    $('#group-create-context').value = t.default_context || '';
  }
  sourceRow.style.display = '';
  // A per-group quick-create already pins the parent. Keep the mirror-source
  // checkbox out of that path so it cannot imply a different nesting target.
  parentRow.style.display = source && !groupCreateParent ? '' : 'none';
  taskRow.style.display = '';
  previewRow.style.display = '';
  maxRow.style.display = 'none';
  submitBtn.textContent = 'Create & spawn';
  renderGroupCreateTemplatePreview();
}

// renderGroupCreateTemplatePreview paints the picked circle's readback badges +
// the roster's final agent names under the typed group name — reusing the same
// helpers the instantiate / deploy previews use, so all three read identically.
function renderGroupCreateTemplatePreview() {
  const t = selectedGroupCreateTemplate();
  const host = $('#group-create-template-preview');
  if (!t) { host.innerHTML = ''; return; }
  host.innerHTML =
    `<div class="tp-badges">${templateReadbackBadges(t)}</div>`
    + templateRosterRowsHTML(t, $('#group-create-name').value);
}

// Set only by the per-group "create subgroup" shortcut. Keeping the parent as
// modal state lets blank and template-backed creation share this dialog.
let groupCreateParent = '';

function openGroupCreateModal(presetTemplate, parentGroup) {
  // Start from the live snapshot each open; a stale manage-fetch cache from a
  // prior session must not shadow it.
  gcTemplateCache = null;
  groupCreateParent = parentGroup || '';
  const regularTitle = $('#group-create-title .group-create-title-regular');
  const wizardTitle = $('#group-create-title .group-create-title-wizard');
  regularTitle.textContent = groupCreateParent
    ? `Create a subgroup under ${groupCreateParent}`
    : 'Create a new agent group';
  wizardTitle.textContent = groupCreateParent
    ? `⚔ Form a sub-party under ${groupCreateParent}`
    : '⚔ Form a party';
  $('#group-create-name').value = '';
  $('#group-create-descr').value = '';
  $('#group-create-cwd').value = '';
  $('#group-create-context').value = '';
  $('#group-create-task').value = '';
  $('#group-create-max-members').value = '';
  $('#group-create-source').innerHTML = groupCreateMirrorOptions();
  $('#group-create-source').value = '';
  $('#group-create-parent').checked = false;
  $('#group-create-error').textContent = '';
  populateGroupCreateTemplates(presetTemplate);
  applyGroupCreateTemplate();
  $('#group-create-modal').classList.add('show');
  // With a circle preselected the roster preview is the point of interest, but
  // the group name is still the first thing to type — focus it either way.
  setTimeout(() => $('#group-create-name').focus(), 0);
}

function closeGroupCreateModal() {
  $('#group-create-modal').classList.remove('show');
}

async function submitGroupCreate() {
  // A picked party profile routes through the template instantiate path; the
  // blank default keeps the original empty-group create verbatim.
  const tmpl = selectedGroupCreateTemplate();
  if (tmpl) { await submitGroupCreateFromTemplate(tmpl); return; }

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
      body: JSON.stringify({
        name, parent: groupCreateParent, descr, default_cwd: cwd,
        default_context: context, max_members: maxMembers,
      }),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    closeGroupCreateModal();
    toast(groupCreateParent
      ? `subgroup created: ${name} under ${groupCreateParent}`
      : `group created: ${name}`);
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

// submitGroupCreateFromTemplate deploys the picked circle: create the group and
// spawn its whole roster via the template instantiate endpoint, sending the
// group's edited copy of descr / startup context (context_override) + the Task.
// Respects the endpoint's 409-on-existing-name. Mirrors the instantiate modal's
// spawn-count / work-pattern toasts so the outcome reads the same wherever a
// circle is cast.
async function submitGroupCreateFromTemplate(tmpl) {
  const name = $('#group-create-name').value.trim();
  const descr = $('#group-create-descr').value.trim();
  const cwd = $('#group-create-cwd').value.trim();
  const context = $('#group-create-context').value;
  const task = $('#group-create-task').value;
  const mirrorSource = groupCreateMirrorSource();
  const errEl = $('#group-create-error');
  errEl.textContent = '';
  if (!name) {
    errEl.textContent = 'name is required';
    return;
  }
  const submitBtn = $('#group-create-submit');
  submitBtn.disabled = true;
  try {
    const payload = { group_name: name, task, cwd, descr_override: descr, context_override: context };
    if (groupCreateParent) payload.parent = groupCreateParent;
    else if (mirrorSource && $('#group-create-parent').checked) payload.parent = mirrorSource;
    const r = await fetch(`/api/templates/${encodeURIComponent(tmpl.name)}/instantiate`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    const txt = await r.text();
    if (!r.ok) { errEl.textContent = txt || `HTTP ${r.status}`; return; }
    let resp = {};
    try { resp = JSON.parse(txt); } catch (_) {}
    closeGroupCreateModal();
    const failed = resp.failed || 0;
    toast(failed
      ? `group ${name}: spawned ${resp.spawned || 0}, ${failed} failed — check the group`
      : `group ${name}: spawned ${resp.spawned || 0} agent${resp.spawned === 1 ? '' : 's'}`,
      failed > 0);
    // A silently-skipped kick-off briefing gets its own toast — it must not
    // hide behind a happy spawn count.
    const perrs = resp.pattern_errors || [];
    if (perrs.length) {
      toast(`⚠ work pattern: ${perrs.length} step${perrs.length === 1 ? '' : 's'} not sent — ${perrs[0]}`, true);
    } else if (resp.pattern_delivered) {
      toast(`work pattern: ${resp.pattern_delivered} briefing${resp.pattern_delivered === 1 ? '' : 's'} sent`);
    }
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

// repopulateGroupCreateTemplatesIfOpen refreshes the party-profile dropdown
// after the templates manager (opened from "⧉ manage circles…") closes, so a
// circle created / renamed / deleted there shows up — but only while the
// create-group dialog is still open behind it. Because that dialog is a
// refresh-suspending modal-overlay, the live snapshot went stale while the
// manager was stacked on top, so we fetch /api/templates DIRECTLY and hold it
// in gcTemplateCache (a failed fetch falls back to the snapshot). If the
// selection survived, the human's edited copy of descr / context / task is left
// intact (no re-prefill over their edits); if it vanished (deleted in the
// manager), the dependent fields are reconciled back to the blank state.
async function repopulateGroupCreateTemplatesIfOpen() {
  if (!$('#group-create-modal').classList.contains('show')) return;
  const cur = $('#group-create-template').value;
  try {
    const r = await fetch('/api/templates', { credentials: 'same-origin' });
    if (r.ok) {
      const list = await r.json();
      if (Array.isArray(list)) gcTemplateCache = list;
    }
  } catch (_) { /* keep the snapshot fallback */ }
  populateGroupCreateTemplates(cur);
  if ($('#group-create-template').value !== cur) applyGroupCreateTemplate();
  else renderGroupCreateTemplatePreview();
}

function bindGroupCreateModal() {
  $('#group-create-open').addEventListener('click', () => openGroupCreateModal());
  $('#group-create-cwd-browse').addEventListener('click', browseGroupCreateCwd);
  // Party profile picker (JOH-356): selecting a circle prefills + reveals the
  // template-only fields; typing the group name re-flows the roster preview's
  // "<group>-<agent>" names.
  $('#group-create-template').addEventListener('change', applyGroupCreateTemplate);
  $('#group-create-source').addEventListener('change', () => {
    $('#group-create-parent').checked = false;
    applyGroupCreateTemplate();
  });
  $('#group-create-name').addEventListener('input', renderGroupCreateTemplatePreview);
  // "⧉ manage circles…" opens the same templates manager the Groups cog does
  // (the JOH-350 "⧉ manage…" idiom); its create/edit/delete is picked up when
  // it closes (both close paths — Close button and backdrop).
  $('#group-create-manage-templates').addEventListener('click', () => openTemplatesManageModal());
  $('#templates-manage-close').addEventListener('click', repopulateGroupCreateTemplatesIfOpen);
  $('#templates-manage-modal').addEventListener('click', (e) => {
    if (e.target === $('#templates-manage-modal')) repopulateGroupCreateTemplatesIfOpen();
  });
  // The Groups cog's standalone "⎘ from template" shortcut now opens THIS dialog
  // with the first circle preselected (JOH-356 — one obvious create-a-group
  // surface) instead of the separate instantiate modal.
  $('#group-from-template-open').addEventListener('click', () => {
    // Read the live snapshot (not gcTemplateCache) — openGroupCreateModal resets
    // the cache, so the preselected circle must exist in the current snapshot.
    const templates = (lastSnapshot && lastSnapshot.templates) || [];
    if (!templates.length) {
      toast(wizWord(
        'no templates yet — define one via the Groups cog ⚙ → ⧉ templates… first',
        'no summoning circles yet — chalk one via the Groups cog ⚙ → ⧉ circles… first'), true);
      return;
    }
    openGroupCreateModal(templates[0].name);
  });
  // 🧹 cleanup: the Groups tab's "clean up" button opens the rich
  // multi-category cleanup modal — bulk unjoin / retire / delete /
  // reinstate spanning active agents, retired agents and plain
  // conversations (openCleanupModal mode 'agents').
  $('#cleanup-all-open').addEventListener('click', () => openCleanupModal({ mode: 'agents' }));
  // Retired-scoped batch delete (JOH-31) — the filterable preview of every
  // retired agent, all ticked, with per-row opt-out before the purge. The
  // discoverable twin of the command palette's "Delete retired agents…".
  $('#delete-retired-open').addEventListener('click', () => openDeleteRetiredPreview());
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
  openPermEditModal, openSpawnPermEditor, bindPermEditModal,
  bindGroupCreateModal, openGroupCreateModal,
};
