// render.js — the dashboard render functions.
//
// Builds the HTML for the group / member / permissions / slugs /
// messages / usage views from snapshot data. Extracted from
// dashboard.js as part of the Stage 2 module split.

import {
  $, esc, shortId, onlineDot, agentStatusDot, statePill, slopMachine, contextMeter,
  roleCell, memberActions, ungroupedMemberActions, actionCog, relTime, shortCwd,
  cwdCell, branchCell, offlineDefault, groupShowOffline,
} from './helpers.js';
import { sortHead, applySort, MEMBER_COLS, MEMBER_ACCESSORS } from './sort.js';

// lastSnapshot and sudoBadge live in dashboard.js; sudoByConv lives in
// refresh.js (refresh() rebuilds it on every poll). Imported back here —
// deliberate, benign cycles: render.js runs no top-level code that reads
// them — the render functions touch them only when called, long after
// every module finishes evaluating (sudoBadge is a hoisted function;
// lastSnapshot / sudoByConv are read-only live bindings here).
import { lastSnapshot, sudoBadge } from './dashboard.js';
import { sudoByConv } from './refresh.js';

// memberRowHTML renders one draggable member <tr>. `ctx` selects the
// drag wiring + action set:
//   - {group: <groupObj>}  — a real group member: the source group
//     is recorded for drag-and-drop; full memberActions (incl. owner
//     toggle / remove-from-group).
//   - {ungrouped: true}    — a row in the virtual Ungrouped group:
//     tagged as an ungrouped drag source; agent-level actions only.
function memberRowHTML(m, ctx) {
  const state = m.state || {};
  const subagents = state.subagent_count || 0;
  const dndSource = ctx.ungrouped
    ? 'data-dnd-source-ungrouped="1"'
    : `data-dnd-source-group="${esc(ctx.group.name)}"`;
  const actions = ctx.ungrouped ? ungroupedMemberActions(m) : memberActions(ctx.group, m);
  return `
              <tr class="dnd-draggable" draggable="true" ${dndSource}
                  data-dnd-conv="${esc(m.conv_id)}"
                  data-dnd-label="${esc(m.title || m.conv_id)}">
                <td><div class="agent-ctl">${agentStatusDot(m)}${actions}</div></td>
                <td class="id">${esc(shortId(m.conv_id))}</td>
                <td>
                  <div class="rowname"><span class="rowname-text" data-act="rename-name" data-conv="${esc(m.conv_id)}" data-current="${esc(m.title || '')}" data-label="${esc(m.title || m.conv_id)}" title="Click to rename this agent — Enter saves, Esc cancels">${esc(m.title || '(unnamed)')}</span>${sudoBadge(sudoByConv[m.conv_id], m.conv_id)}</div>
                </td>
                <td class="state-cell">
                  ${contextMeter(state)}
                  ${statePill(state, m.online)}
                  ${slopMachine(state, m.online, m.conv_id)}
                  ${subagents > 0 ? `<span class="state-detail">+${subagents}</span>` : ''}
                </td>
                <td><span class="last-hook">${esc(relTime(state.last_hook))}</span></td>
                <td>${cwdCell(m)}</td>
                <td>${branchCell(m)}</td>
                <td>${roleCell(m)}</td>
                <td class="muted">${esc(m.descr || '')}</td>
              </tr>`;
}

// renderVirtualGroup renders the synthetic "Ungrouped" group. It is
// intentionally inert AS A GROUP — no rename / delete / multicast /
// cron / add-member / spawn buttons, no default-cwd / default-context.
// Its only interactive role is drag-and-drop:
//   - member rows are draggable INTO real groups (→ join);
//   - the summary is a drop target for real-group members (→ leave
//     that group; if it was their only group they reappear here).
// The summary carries data-dnd-target-ungrouped (NOT -target-group),
// so the move/clone code paths can never mistake it for a real group.
//
// Offline visibility: the group honours the tab-wide "show offline"
// checkbox (it has no per-group override). Hidden members still
// count toward the header total so it stays truthful.
function renderVirtualGroup(g) {
  const members = g.members || [];
  const visible = offlineDefault('groups') ? members : members.filter(m => m.online);
  const hiddenOffline = members.length - visible.length;
  const key = g.key || g.name;
  const isOpen = localStorage.getItem('tclaude.dash.group.' + key) === '1';
  const body = members.length === 0
    ? '<div class="muted">(no ungrouped agents)</div>'
    : visible.length === 0
    ? `<div class="muted">(${hiddenOffline} offline agent${hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${hiddenOffline === 1 ? 'it' : 'them'})</div>`
    : `
        <table>
          ${sortHead('members', MEMBER_COLS)}
          <tbody>
            ${applySort('members', visible, MEMBER_ACCESSORS).map(m => memberRowHTML(m, {ungrouped: true})).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary data-dnd-target-ungrouped="1">
        <strong class="group-name">${esc(g.name)}</strong>
        <span class="group-virtual-badge" title="A virtual group, not a real one — it can't be renamed, deleted, messaged or scheduled. It just collects agents that aren't in any group.">virtual</span>
        <span class="muted">— ${members.length} agent${members.length === 1 ? '' : 's'} not in any group${hiddenOffline > 0 ? ` · ${hiddenOffline} offline hidden` : ''}</span>
        <span class="muted group-virtual-hint">drag a row onto a group to add it · drag a group member here to remove it</span>
      </summary>
      <div class="subtable">
        ${body}
      </div>
    </details>
  `;
}

// renderVirtualConversationsGroup renders the synthetic
// "Conversations" group — non-agent conversations. Like the virtual
// Ungrouped group it is inert AS A GROUP, but it IS a drag source:
// its rows (data-dnd-source-conversation) drag onto a real group →
// promote + join, or onto the Ungrouped header → promote with no
// group. It is NOT a drop target — retiring an agent is done by
// dragging it onto the "Retired" group instead.
function renderVirtualConversationsGroup(g) {
  const members = g.members || [];
  const key = g.key || g.name;
  const isOpen = localStorage.getItem('tclaude.dash.group.' + key) === '1';
  const body = members.length === 0
    ? '<div class="muted">(no non-agent conversations)</div>'
    : `
        <table>
          <thead><tr><th></th><th>conv</th><th>title</th><th>last activity</th><th></th></tr></thead>
          <tbody>
            ${members.map(c => `
              <tr class="dnd-draggable" draggable="true" data-dnd-source-conversation="1"
                  data-dnd-conv="${esc(c.conv_id)}"
                  data-dnd-label="${esc(c.title || c.conv_id)}">
                <td>${onlineDot(c.online)}</td>
                <td class="id">${esc(shortId(c.conv_id))}</td>
                <td><span class="rowname">${esc(c.title || '(untitled)')}</span></td>
                <td><span class="last-hook">${esc(c.modified ? relTime(c.modified) : '')}</span></td>
                <td><div class="row-actions"><button class="primary" data-act="promote-agent" data-conv="${esc(c.conv_id)}" data-label="${esc(c.title || c.conv_id)}" title="Promote this conversation into an agent">promote</button></div></td>
              </tr>`).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${esc(g.name)}</strong>
        <span class="group-virtual-badge" title="A virtual group, not a real one — recent conversations that aren't agents. Drag one onto a group, or click promote, to make it an agent.">virtual</span>
        <span class="muted">— ${members.length} conversation${members.length === 1 ? '' : 's'} that aren't agents</span>
        <span class="muted group-virtual-hint">drag a row onto a group to promote + add it</span>
      </summary>
      <div class="subtable">
        ${body}
      </div>
    </details>
  `;
}

// renderVirtualRetiredGroup renders the synthetic "Retired" group —
// agents demoted back to plain conversations. Like the virtual
// Conversations group it is inert AS A GROUP, but it IS a drag source
// AND a drag target:
//   - its summary (data-dnd-target-retired) accepts an agent row →
//     retire it, demoting the agent back to a plain conversation;
//   - its rows (data-dnd-source-retired) drag onto a real group →
//     reinstate + join, or onto the Ungrouped header → reinstate with
//     no group.
// It is the landing surface so a just-retired agent stays visible on
// the tab. Each row also keeps the per-row "reinstate" button.
function renderVirtualRetiredGroup(g) {
  const members = g.members || [];
  const key = g.key || g.name;
  const isOpen = localStorage.getItem('tclaude.dash.group.' + key) === '1';
  const body = members.length === 0
    ? '<div class="muted">(no retired agents)</div>'
    : `
        <table>
          <thead><tr><th></th><th>conv</th><th>title</th><th>retired</th><th>by</th><th>reason</th><th></th></tr></thead>
          <tbody>
            ${members.map(a => `
              <tr class="dnd-draggable" draggable="true" data-dnd-source-retired="1"
                  data-dnd-conv="${esc(a.conv_id)}"
                  data-dnd-label="${esc(a.title || a.conv_id)}">
                <td>${onlineDot(a.online)}</td>
                <td class="id">${esc(shortId(a.conv_id))}</td>
                <td><span class="rowname">${esc(a.title || '(untitled)')}</span></td>
                <td><span class="last-hook">${esc(a.retired_at ? relTime(a.retired_at) : '')}</span></td>
                <td>${esc(a.retired_by || '')}</td>
                <td class="muted">${esc(a.retire_reason || '')}</td>
                <td><div class="row-actions"><button class="primary" data-act="reinstate-agent" data-conv="${esc(a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" title="Reinstate this agent back to active status">reinstate</button></div></td>
              </tr>`).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary data-dnd-target-retired="1">
        <strong class="group-name">${esc(g.name)}</strong>
        <span class="group-virtual-badge" title="A virtual group, not a real one — agents that were retired (demoted back to plain conversations). Drag an agent here to retire it; drag a retired row onto a group, or click reinstate, to bring it back.">virtual</span>
        <span class="muted">— ${members.length} retired agent${members.length === 1 ? '' : 's'}</span>
        <span class="muted group-virtual-hint">drag an agent here to retire it · drag a retired row onto a group to reinstate + join it</span>
      </summary>
      <div class="subtable">
        ${body}
      </div>
    </details>
  `;
}

// groupActionsHTML renders a real group header's action cluster. The
// three most-used controls — spawn, power on, shutdown — stay at the
// TOP LEVEL; the rest (add member, multicast cron, message, rename,
// export, cleanup, windows, delete) are collected behind the ⚙ options
// cog so the header stays readable. Every button keeps the exact
// data-act / data-* the row-action dispatcher already expects — only
// their DOM position moves.
// Feather "user-plus": a person silhouette with a + alongside. Same
// monochrome-via-currentColor convention as the helpers.js eye icons.
const SPAWN_ICO_SVG = '<svg class="spawn-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="8.5" cy="7" r="4"/><line x1="20" y1="8" x2="20" y2="14"/><line x1="23" y1="11" x2="17" y2="11"/></svg>';

function groupActionsHTML(g, members) {
  const menu =
    `<button data-act="add-member" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Add an existing conversation to this group">+ add member</button>`
    + `<button data-act="cron-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name, scopeGroup: g.name}))}' data-label="${esc(g.name)}" title="Schedule a recurring cron job scoped to ${esc(g.name)} — multicast the whole group, or nudge a single member">⏰ multicast</button>`
    + `<button data-act="message-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name}))}' data-label="${esc(g.name)}" title="Send a one-shot message to ${esc(g.name)} — the whole group, or a ticked subset of its members">✉ message</button>`
    + `<button data-act="rename-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Rename this group">rename</button>`
    + `<button data-act="export-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Export this whole group — members, permissions, messages and every conversation — to a portable .zip archive">⤓ export</button>`
    + `<button data-act="cleanup-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Remove confirmed-offline members from this group">🧹 cleanup</button>`
    + `<button data-act="window-modal-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" aria-label="Focus or unfocus this group's agent windows" title="Focus / unfocus agent windows — open a modal to bulk-attach (focus) or bulk-detach (unfocus) the terminal windows of agents in this group. Window-only: the agents keep running either way.">🪟 windows…</button>`
    + `<button class="danger" data-act="delete-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-members="${members.length}" title="Delete this group">delete group</button>`;
  // Spawn sits OUTSIDE .group-actions — the cluster fades to 0.4 at
  // rest, which made spawn (the primary CTA) hard to find. As a
  // sibling chip it keeps full opacity all the time and the
  // blue-accent .spawn-btn skin in dashboard.css makes it pop.
  return `<button class="spawn-btn" data-act="spawn-agent" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Spawn a new tclaude session and join this group" aria-label="Spawn a new agent into this group">${SPAWN_ICO_SVG}<span>spawn</span></button>`
    + `<span class="group-actions">`
    + `<button data-act="power-on-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Power on — resume every offline agent in this group. Each offline conversation is restarted in a fresh tmux session; agents already running are left alone. Resume only: nothing new is created.">🟢 power on</button>`
    + `<button class="warn" data-act="shutdown-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Shutdown — stop every running agent in this group. Sends /exit, then force-kills any agent still alive after a grace period. Stop only: nothing is deleted, every session can simply be resumed.">🛑 shutdown</button>`
    + actionCog('group-menu', menu)
    + `</span>`;
}

function renderGroups(groups) {
  if (!groups || !groups.length) {
    return '<div class="empty">No groups yet. Create one with: <code>tclaude agent groups create &lt;name&gt;</code></div>';
  }
  return groups.map(g => {
    if (g.virtual) return g.conversations ? renderVirtualConversationsGroup(g)
      : g.retired ? renderVirtualRetiredGroup(g)
      : renderVirtualGroup(g);
    const members = g.members || [];
    // Offline visibility: per-group override falls back to the
    // tab-wide checkbox. Hidden members still count toward the
    // 👥 chip's online/total/cap counts so the header stays truthful.
    const visible = groupShowOffline(g.name) ? members : members.filter(m => m.online);
    const hiddenOffline = members.length - visible.length;
    // Restore expanded state across the 5s polling re-renders by
    // keying on group name. Persisted in localStorage so it
    // survives a full page reload too.
    const isOpen = localStorage.getItem('tclaude.dash.group.' + g.name) === '1';
    // 👥 chip: <online>/<total>/<cap> — but collapse the online
    // slot to <total>/<cap> when everyone is online (the common
    // case), so the chip stays terse and only grows a third slot
    // when there is actually offline membership to surface. ∞
    // holds the cap slot when unset so its slot layout stays
    // stable; .unset still signals "click to set one". Absorbs
    // the verbose "X members, Y online" header span that used to
    // live alongside it.
    const onlineCount = g.online || 0;
    const atCap = !!g.max_members && members.length >= g.max_members;
    const capValueText = g.max_members || '∞';
    const capChipText = onlineCount === members.length
      ? `${members.length}/${capValueText}`
      : `${onlineCount}/${members.length}/${capValueText}`;
    const capChipClass = `group-max-members${atCap ? ' full' : ''}${g.max_members ? '' : ' unset'}`;
    const capChipTitleParts = [
      `${members.length} member${members.length === 1 ? '' : 's'} (${onlineCount} online)`,
      g.max_members ? `cap ${g.max_members}` : 'no cap',
    ];
    if (atCap) capChipTitleParts.push('group is full, spawns refused');
    if (hiddenOffline > 0) capChipTitleParts.push(`${hiddenOffline} offline hidden in this view`);
    const capChipTitle = capChipTitleParts.join(' · ') + (g.max_members ? ' — click to edit cap' : ' — click to set a cap');
    return `
    <details data-group-key="${esc(g.name)}"${isOpen ? ' open' : ''}>
      <summary data-dnd-target-group="${esc(g.name)}">
        <strong class="group-name" data-group-name="${esc(g.name)}">${esc(g.name)}</strong>
        <span class="group-descr${g.descr ? '' : ' unset'}" data-act="set-group-descr" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-descr="${esc(g.descr || '')}" title="${g.descr ? 'Group description — click to edit' : 'No description — click to set one'}">📝 ${g.descr ? esc(g.descr) : 'no description'}</span>
        <span class="group-default-cwd${g.default_cwd ? '' : ' unset'}" data-act="set-group-dir" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-cwd="${esc(g.default_cwd || '')}" title="${g.default_cwd ? 'Default spawn directory: ' + esc(g.default_cwd) + ' — click to edit' : 'No default spawn directory — click to set one'}">📁 ${g.default_cwd ? esc(shortCwd(g.default_cwd)) : 'no default dir'}</span>
        <span class="group-default-context${g.default_context ? '' : ' unset'}" data-act="set-group-context" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${g.default_context ? 'Startup context (' + g.default_context.length + ' chars) delivered to the inbox of agents spawned here — click to edit' : 'No startup context — click to set one'}">📋 ${g.default_context ? 'startup context' : 'no startup context'}</span>
        <span class="${capChipClass}" data-act="set-group-max-members" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-max="${g.max_members || 0}" title="${esc(capChipTitle)}">👥 ${capChipText}</span>
        ${groupActionsHTML(g, members)}
      </summary>
      <div class="subtable">
        ${members.length === 0
          ? '<div class="muted">(no members yet)</div>'
          : visible.length === 0
          ? `<div class="muted">(${hiddenOffline} offline member${hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${hiddenOffline === 1 ? 'it' : 'them'})</div>`
          : `
        <table>
          ${sortHead('members', MEMBER_COLS)}
          <tbody>
            ${applySort('members', visible, MEMBER_ACCESSORS).map(m => memberRowHTML(m, {group: g})).join('')}
          </tbody>
        </table>`}
        ${renderGroupLinksSection(g.name)}
        <code class="copy-cmd" data-copy="tclaude agent groups members ${g.name}">tclaude agent groups members ${esc(g.name)}</code>
      </div>
    </details>
  `;
  }).join('');
}

// renderGroupLinksSection: per-group outbound/inbound link rows. Reads
// from lastSnapshot.links (the snapshot already carries every edge)
// so we don't need a second round-trip. Edit/delete buttons hit the
// shared row-action dispatcher; add opens the link modal preset to
// FROM=this group.
function renderGroupLinksSection(groupName) {
  const all = (lastSnapshot && lastSnapshot.links) || [];
  const out = all.filter(l => l.from === groupName);
  const inc = all.filter(l => l.to === groupName);
  const total = out.length + inc.length;
  if (total === 0) {
    return `
      <div class="group-links-section">
        <span class="muted" style="font-size:11px">No links involving this group.</span>
        <button data-act="link-new" data-from="${esc(groupName)}" data-label="${esc(groupName)}" title="Add an outbound link from this group">+ link</button>
      </div>
    `;
  }
  const renderRow = (l, dir) => {
    const other = dir === 'out' ? l.to : l.from;
    const arrow = dir === 'out' ? '→' : '←';
    return `
      <tr>
        <td><span class="muted">${arrow}</span></td>
        <td><span class="rowname">${esc(other || '(deleted)')}</span></td>
        <td><span class="id">${esc(l.mode)}</span></td>
        <td><div class="row-actions">
          <button data-act="link-edit" data-id="${l.id}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" data-mode="${esc(l.mode)}" title="Change this link's mode">edit</button>
          <button class="danger" data-act="link-delete" data-id="${l.id}" data-group="${esc(groupName)}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" title="Remove this link">×</button>
        </div></td>
      </tr>
    `;
  };
  return `
    <div class="group-links-section">
      <strong style="font-size:11px">Links</strong>
      <table>
        <thead>
          <tr><th></th><th>Other group</th><th>Mode</th><th></th></tr>
        </thead>
        <tbody>
          ${out.map(l => renderRow(l, 'out')).join('')}
          ${inc.map(l => renderRow(l, 'in')).join('')}
        </tbody>
      </table>
      <button data-act="link-new" data-from="${esc(groupName)}" data-label="${esc(groupName)}" title="Add an outbound link from this group">+ link</button>
    </div>
  `;
}

function renderPermissions(perm, agents) {
  const titleByConv = Object.fromEntries((agents || []).map(a => [a.conv_id, a.title]));
  const overrides = perm.overrides || {};
  const defaults = perm.defaults || [];
  // Split each conv's tri-state override map into granted / denied
  // slug lists for display. The per-agent "permissions" button on the
  // Groups tab is the write path; this tab is the roster.
  const rows = Object.entries(overrides).map(([k, slugEffects]) => {
    const granted = [], denied = [];
    for (const [slug, effect] of Object.entries(slugEffects || {})) {
      (effect === 'deny' ? denied : granted).push(slug);
    }
    granted.sort(); denied.sort();
    return { k, granted, denied };
  }).sort((a, b) => a.k < b.k ? -1 : 1);
  return `
    <h3 style="margin-top:0">Defaults <span class="muted" style="font-size:11px">— granted to every agent (config.json)</span></h3>
    ${defaults.length === 0
      ? '<div class="empty">No defaults set. Grant with: <code>tclaude agent permissions grant default &lt;slug&gt;</code></div>'
      : `<div>${defaults.map(s => `<span class="tag default slug">${esc(s)}</span>`).join(' ')}</div>`}
    <h3>Per-agent overrides <span class="muted" style="font-size:11px">— permanent grant / deny on top of defaults (SQLite agent_permissions). Edit via the per-agent “permissions” button.</span></h3>
    ${rows.length === 0
      ? '<div class="empty">No per-agent overrides yet. Use the per-agent “permissions” button, or <code>tclaude agent permissions grant|deny &lt;conv&gt; &lt;slug&gt;</code></div>'
      : `<table>
          <thead><tr><th>ID</th><th>Title</th><th>Granted</th><th>Denied</th></tr></thead>
          <tbody>
            ${rows.map(r => `
              <tr>
                <td class="id">${esc(shortId(r.k))}</td>
                <td class="rowname">${esc(titleByConv[r.k] || '(unknown)')}</td>
                <td>${r.granted.map(s => `<span class="tag slug">${esc(s)}</span>`).join(' ') || '<span class="muted">—</span>'}</td>
                <td>${r.denied.map(s => `<span class="tag slug deny">${esc(s)}</span>`).join(' ') || '<span class="muted">—</span>'}</td>
              </tr>
            `).join('')}
          </tbody>
        </table>`}
  `;
}

function renderSlugs(slugs) {
  if (!slugs || !slugs.length) return '<div class="empty">No slugs registered.</div>';
  return `
    <table>
      <thead><tr><th>Slug</th><th>Description</th></tr></thead>
      <tbody>
        ${slugs.map(s => `
          <tr>
            <td><span class="slug">${esc(s.slug)}</span></td>
            <td>${esc(s.description || '')}</td>
          </tr>
        `).join('')}
      </tbody>
    </table>
  `;
}

function showStatus(text, isError) {
  const el = $('#status');
  el.textContent = text;
  el.classList.toggle('error', !!isError);
}

// === Messages tab — human-facing notifications from agents ===
// The unread count drives a badge on the Messages nav button, so the
// human sees there's something to read from whatever tab they're on.
function renderMessagesBadge(unread) {
  const badge = $('#messages-badge');
  if (!badge) return;
  badge.textContent = unread > 99 ? '99+' : String(unread);
  badge.hidden = unread === 0;
}

function renderMessagesTab() {
  if (!lastSnapshot) return;
  const all = lastSnapshot.messages || [];
  // Conv-ids with a live tmux window — focus is only meaningful for
  // those. Cross-referenced from the agent lists already in the
  // snapshot, so no extra round-trip.
  const onlineConvs = new Set();
  (lastSnapshot.agents || []).concat(lastSnapshot.ungrouped || [])
    .forEach(a => { if (a && a.online) onlineConvs.add(a.conv_id); });
  const q = ($('#filter-messages').value || '').toLowerCase();
  const filtered = q
    ? all.filter(m => [m.from_title, m.group, m.subject, m.body]
        .some(s => (s || '').toLowerCase().includes(q)))
    : all;
  $('#messages-list').innerHTML = renderMessages(filtered, onlineConvs);
  const total = all.length;
  $('#filter-messages-count').textContent = q
    ? `${filtered.length} / ${total}`
    : `${total} message${total === 1 ? '' : 's'}`;
}

function renderMessages(msgs, onlineConvs) {
  if (!msgs || !msgs.length) return '<div class="empty">No messages.</div>';
  return msgs.map(m => {
    const unread = !m.read;
    const when = m.created_at ? new Date(m.created_at).toLocaleString() : '';
    const grp = m.group ? `<span class="msg-group">· ${esc(m.group)}</span>` : '';
    const subj = m.subject ? `<div class="msg-subject">${esc(m.subject)}</div>` : '';
    // Focus raises the sending agent's terminal window. Only offered
    // when the agent is online — disabled otherwise, never an error.
    const focusable = m.from_conv && onlineConvs.has(m.from_conv);
    const focusBtn = m.from_conv
      ? `<button data-act="msg-focus" data-conv="${esc(m.from_conv)}" data-id="${m.id}" data-label="${esc(m.from_title || m.from_conv)}"${focusable ? '' : ' disabled'} title="${focusable ? 'Focus this agent’s terminal window and mark the message read' : 'Sending agent is offline — no window to focus'}">focus</button>`
      : '';
    const readBtn = unread
      ? `<button data-act="msg-mark-read" data-id="${m.id}" title="Mark this message read">mark read</button>`
      : '';
    return `<div class="msg-card${unread ? ' msg-unread' : ''}">
      <div class="msg-head">
        ${unread ? '<span class="msg-dot" title="unread">●</span>' : ''}
        <span class="msg-from">${esc(m.from_title || '(unknown sender)')}</span>
        ${grp}
        <span class="msg-id">#${m.id}</span>
        <span class="spacer"></span>
        <span class="msg-time">${esc(when)}</span>
      </div>
      ${subj}
      <div class="msg-body">${esc(m.body)}</div>
      <div class="msg-actions">${focusBtn}${readBtn}</div>
    </div>`;
  }).join('');
}

// === Subscription-usage readout (top bar, left of the live dot) ===
// Account-wide, not per-agent: one readout for the whole dashboard.
// Mirrors the statusbar's "5h [bar] 17% (2h16m) | 7d ..." format.
const USAGE_BAR_WIDTH = 8;

// usageBarColor matches the statusbar thresholds: red >=80, yellow
// >=60, green otherwise.
function usageBarColor(pct) {
  if (pct >= 80) return '#f85149';
  if (pct >= 60) return '#d29922';
  return '#3fb950';
}

// usageBar renders a mini progress bar from a single block glyph (█)
// for BOTH the filled and the empty cells — they differ only in
// colour, never in glyph. Using one glyph guarantees the filled and
// empty segments render at an identical height: mixing in a shade
// glyph (░) for the empty run made it render taller than the █ fill
// in the browser's monospace font.
function usageBar(pct) {
  const p = Math.max(0, Math.min(100, pct || 0));
  const filled = Math.round(p / 100 * USAGE_BAR_WIDTH);
  const empty = USAGE_BAR_WIDTH - filled;
  return '<span class="ubar-fill" style="color:' + usageBarColor(p) + '">'
    + '█'.repeat(filled) + '</span>'
    + '<span class="ubar-empty">' + '█'.repeat(empty) + '</span>';
}

// usageWindowHTML renders one rolling-limit window: label, coloured
// mini bar, percent, and the remaining-time hint.
function usageWindowHTML(label, win) {
  const pct = win.pct || 0;
  const rem = win.remaining
    ? ' <span class="urem">(' + esc(win.remaining) + ')</span>' : '';
  return '<span class="uw"><span class="ulabel">' + label + '</span>'
    + '<span class="ubar">' + usageBar(pct) + '</span>'
    + '<span class="upct">' + Math.round(pct) + '%</span>' + rem + '</span>';
}

// renderUsage paints the top-bar readout from snapshot.usage. When
// usage data is unavailable it degrades to a muted "usage: n/a"
// rather than a broken or error state.
function renderUsage(u) {
  const el = $('#usage');
  if (!el) return;
  const wins = [];
  if (u && u.available) {
    if (u.five_hour) wins.push(usageWindowHTML('5h', u.five_hour));
    if (u.seven_day) wins.push(usageWindowHTML('7d', u.seven_day));
  }
  if (wins.length) {
    el.classList.remove('na');
    el.innerHTML = wins.join('');
    el.title = 'Subscription usage limits — 5-hour and 7-day rolling windows';
  } else {
    el.classList.add('na');
    el.textContent = 'usage: n/a';
    el.title = 'Subscription usage data is currently unavailable';
  }
}

export {
  renderGroups, renderPermissions, renderSlugs, showStatus,
  renderMessagesBadge, renderMessagesTab, renderUsage,
};
