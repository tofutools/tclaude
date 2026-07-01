// render.js — the dashboard render functions.
//
// Builds the HTML for the group / member / permissions / slugs /
// messages / usage views from snapshot data. Extracted from
// dashboard.js as part of the Stage 2 module split.

import {
  $, esc, shortId, shortAgentId, idTooltip, onlineDot, agentStatusDot, harnessLine, sandboxBadge, statePill, slopMachine, wizardPill, contextMeter, activityBadges,
  harnessCanRename, harnessCanRemoteControl,
  roleCell, memberActions, ungroupedMemberActions, actionCog, relTime, shortCwd,
  cwdCell, branchCell, offlineDefault, groupShowOffline, syncBotAnimations,
} from './helpers.js';
import {
  sortHead, applySort, MEMBER_COLS, MEMBER_ACCESSORS, REPLACED_COLS, REPLACED_ACCESSORS,
  RETIRED_COLS, RETIRED_ACCESSORS, CONVERSATIONS_COLS, CONVERSATIONS_ACCESSORS,
  PENDING_COLS, PENDING_ACCESSORS,
} from './sort.js';
import { groupActivityHTML, activitySummary, styledBotsHTML, wizardBotsHTML, aggregateActivity, themedSummaryText } from './group-activity.js';
import { isWizardActive } from './slop.js';
import { dashPrefs } from './prefs.js';
import { listPagerHTML } from './list-paging.js';
import { getDashDefaultProfile } from './profiles.js';

// lastSnapshot and sudoBadge live in dashboard.js; sudoByConv lives in
// refresh.js (refresh() rebuilds it on every poll). Imported back here —
// deliberate, benign cycles: render.js runs no top-level code that reads
// them — the render functions touch them only when called, long after
// every module finishes evaluating (sudoBadge is a hoisted function;
// lastSnapshot / sudoByConv are read-only live bindings here).
import { lastSnapshot, sudoBadge } from './dashboard.js';
import { sudoByConv, hoveredGroupKey } from './refresh.js';

// renameNameCell renders the agent's name — the click-to-edit rename
// affordance when the agent's harness supports a rename (the common case:
// both Claude Code and Codex do, via /rename and ConvStore respectively),
// or a plain non-editable name when it does not. The capability comes from
// the snapshot's harness catalog (harnessCanRename), so a mixed-harness
// group hides the rename only on a harness that genuinely can't deliver one
// — never on Codex, which renames out-of-band. The click handler
// (row-actions.js, data-act="rename-name") is wired only on the editable
// variant, so a non-renameable agent's name simply does nothing on click.
function renameNameCell(m, state) {
  const name = esc(m.title || '(unnamed)');
  if (harnessCanRename(lastSnapshot, state.harness)) {
    return `<span class="rowname-text" data-act="rename-name" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-current="${esc(m.title || '')}" data-label="${esc(m.title || m.conv_id)}" title="Click to rename this agent — Enter saves, Esc cancels">${name}</span>`;
  }
  return `<span class="rowname-text rowname-fixed" title="This agent's harness does not support renaming">${name}</span>`;
}

// memberRowHTML renders one draggable member <tr>. `ctx` selects the
// drag wiring + action set:
//   - {group: <groupObj>}  — a real group member: the source group
//     is recorded for drag-and-drop; full memberActions (incl. owner
//     toggle / remove-from-group).
//   - {ungrouped: true}    — a row in the virtual Ungrouped group:
//     tagged as an ungrouped drag source; agent-level actions only.
// The per-agent OS-notification control is no longer a standalone bell
// in this cell — it moved into the ⚙ options menu (notifyMenuItem in
// helpers.js) to declutter the row. The global master switch (top-bar
// bell) still sits above all of it.
function memberRowHTML(m, ctx) {
  const state = m.state || {};
  const dndSource = ctx.ungrouped
    ? 'data-dnd-source-ungrouped="1"'
    : `data-dnd-source-group="${esc(ctx.group.name)}"`;
  // canRemote gates the per-row remote-control toggle on the agent's harness
  // capability (harnessCanRemoteControl), computed here where lastSnapshot is
  // in scope — the same place renameNameCell reads harnessCanRename.
  const canRemote = harnessCanRemoteControl(lastSnapshot, state.harness);
  const actions = ctx.ungrouped ? ungroupedMemberActions(m, canRemote) : memberActions(ctx.group, m, canRemote);
  return `
              <tr class="dnd-draggable" draggable="true" ${dndSource}
                  data-dnd-conv="${esc(m.conv_id)}"
                  data-dnd-agent="${esc(m.agent_id || m.conv_id)}"
                  data-dnd-label="${esc(m.title || m.conv_id)}">
                <td><div class="agent-ctl">${agentStatusDot(m)}${actions}</div>${harnessLine(m)}${sandboxBadge(m)}</td>
                <td class="id" title="${esc(idTooltip(m.agent_id, m.conv_id))}">${esc(shortAgentId(m.agent_id, m.conv_id))}</td>
                <td class="name-cell">
                  <div class="rowname">${renameNameCell(m, state)}${sudoBadge(sudoByConv[m.conv_id], m.conv_id)}</div>
                </td>
                <td class="state-cell">
                  ${contextMeter(state)}
                  ${statePill(state, m.online)}
                  ${slopMachine(state, m.online, m.conv_id)}
                  ${wizardPill(state, m.online, m.conv_id)}
                  ${m.online ? activityBadges(state) : ''}
                </td>
                <td><span class="last-hook">${esc(relTime(state.last_hook))}</span></td>
                <td><span class="last-hook" title="${esc(m.created_at || '')}">${esc(relTime(m.created_at))}</span></td>
                <td>${cwdCell(m)}</td>
                <td>${branchCell(m)}</td>
                <td>${roleCell(m, ctx.group)}</td>
                <td class="descr-cell muted">${esc(m.descr || '')}</td>
              </tr>`;
}

// renderVirtualGroup renders the synthetic "Ungrouped" group. It is
// intentionally inert AS A GROUP — no rename / delete / multicast /
// cron / add-member / spawn buttons, no default-cwd / default-context.
// Its only interactive role is drag-and-drop:
//   - member rows are draggable INTO real groups (→ join);
//   - the whole group box is a drop target for real-group members
//     (→ leave that group; if it was their only group they reappear
//     here).
// The <details> carries data-dnd-target-ungrouped (NOT -target-group),
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
  const isOpen = dashPrefs.getItem('tclaude.dash.group.' + key) === '1';
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
    <details class="group-virtual" data-group-key="${esc(key)}" data-dnd-target-ungrouped="1"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${esc(g.name)}</strong>
        ${groupActivityChip(members)}
        <span class="group-virtual-badge" title="A virtual group, not a real one — it can't be renamed, deleted, messaged or scheduled. It just collects agents that aren't in any group.">virtual</span>
        <span class="muted">— ${members.length} agent${members.length === 1 ? '' : 's'} not in any group${hiddenOffline > 0 ? ` · ${hiddenOffline} offline hidden` : ''}</span>
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
  const isOpen = dashPrefs.getItem('tclaude.dash.group.' + key) === '1';
  // members is one server page now; the count + pager read the full total off
  // the list's pagination envelope (g.paging), falling back to the page length.
  const total = g.paging ? g.paging.total : members.length;
  const pager = listPagerHTML('conversations', g.paging);
  const body = members.length === 0
    ? '<div class="muted">(no non-agent conversations)</div>'
    : `
        <table>
          ${sortHead('conversations', CONVERSATIONS_COLS)}
          <tbody>
            ${applySort('conversations', members, CONVERSATIONS_ACCESSORS).map(c => `
              <tr class="dnd-draggable" draggable="true" data-dnd-source-conversation="1"
                  data-dnd-conv="${esc(c.conv_id)}"
                  data-dnd-agent="${esc(c.agent_id || c.conv_id)}"
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
        <span class="muted">— ${total} conversation${total === 1 ? '' : 's'} that aren't agents</span>
      </summary>
      <div class="subtable">
        ${body}
        ${pager}
      </div>
    </details>
  `;
}

// renderVirtualRetiredGroup renders the synthetic "Retired" group —
// agents demoted back to plain conversations. Like the virtual
// Conversations group it is inert AS A GROUP, but it IS a drag source
// AND a drag target:
//   - its whole group box (data-dnd-target-retired) accepts an agent
//     row → retire it, demoting the agent back to a plain conversation;
//   - its rows (data-dnd-source-retired) drag onto a real group →
//     reinstate + join, or onto the Ungrouped box → reinstate with
//     no group.
// It is the landing surface so a just-retired agent stays visible on
// the tab. Each row also keeps the per-row "reinstate" button.
function renderVirtualRetiredGroup(g) {
  const members = g.members || [];
  const key = g.key || g.name;
  const isOpen = dashPrefs.getItem('tclaude.dash.group.' + key) === '1';
  // members is one server page now; the count + pager read the full total off
  // the list's pagination envelope (g.paging), falling back to the page length.
  const total = g.paging ? g.paging.total : members.length;
  const pager = listPagerHTML('retired', g.paging);
  const body = members.length === 0
    ? '<div class="muted">(no retired agents)</div>'
    : `
        <table>
          ${sortHead('retired', RETIRED_COLS)}
          <tbody>
            ${applySort('retired', members, RETIRED_ACCESSORS).map(a => `
              <tr class="dnd-draggable" draggable="true" data-dnd-source-retired="1"
                  data-dnd-conv="${esc(a.conv_id)}"
                  data-dnd-agent="${esc(a.agent_id || a.conv_id)}"
                  data-dnd-label="${esc(a.title || a.conv_id)}">
                <td>${onlineDot(a.online)}</td>
                <td class="id" title="${esc(idTooltip(a.agent_id, a.conv_id))}">${esc(shortAgentId(a.agent_id, a.conv_id))}</td>
                <td><span class="rowname">${esc(a.title || '(untitled)')}</span></td>
                <td><span class="last-hook">${esc(a.retired_at ? relTime(a.retired_at) : '')}</span></td>
                <td${a.retired_by ? ` title="${esc(a.retired_by)}"` : ''}>${esc(a.retired_by_display || a.retired_by || '')}</td>
                <td class="muted">${esc(a.retire_reason || '')}</td>
                <td><div class="row-actions"><button class="primary" data-act="reinstate-agent" data-conv="${esc(a.conv_id)}" data-agent="${esc(a.agent_id || a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" title="Reinstate this agent back to active status">reinstate</button></div></td>
              </tr>`).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}" data-dnd-target-retired="1"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${esc(g.name)}</strong>
        <span class="group-virtual-badge" title="A virtual group, not a real one — agents that were retired (demoted back to plain conversations). Drag an agent here to retire it; drag a retired row onto a group, or click reinstate, to bring it back.">virtual</span>
        <span class="muted">— ${total} retired agent${total === 1 ? '' : 's'}</span>
      </summary>
      <div class="subtable">
        ${body}
        ${pager}
      </div>
    </details>
  `;
}

// renderVirtualReplacedGroup renders the synthetic "Replaced generations"
// group — superseded predecessor conversation generations of still-existing
// actors (a reincarnate / Claude Code /clear advanced the actor's live pointer
// and left these behind, JOH-26). Since the Retired tray became actor-level
// these no longer surface anywhere, so this archival, read-mostly list brings
// them back. It is inert as a group and is NOT a drag source/target. Each row
// links back to its owning live actor and offers two actions: copy the
// generation's conv-id (to inspect it out-of-band via `claude --resume <id>`
// from its dir, or `tclaude agent seance --target <id>`), and an EXACT delete
// of just that generation. The delete hits the dedicated
// /api/agent-generations endpoint, which refuses the actor's live head — so it
// can never touch the live agent.
function renderVirtualReplacedGroup(g) {
  const members = g.members || [];
  const key = g.key || g.name;
  const isOpen = dashPrefs.getItem('tclaude.dash.group.' + key) === '1';
  // members is one server page now; the count + pager read the full total off
  // the list's pagination envelope (g.paging), falling back to the page length.
  const total = g.paging ? g.paging.total : members.length;
  const pager = listPagerHTML('replaced', g.paging);
  const body = members.length === 0
    ? '<div class="muted">(no replaced generations)</div>'
    : `
        <table>
          ${sortHead('replaced', REPLACED_COLS)}
          <tbody>
            ${applySort('replaced', members, REPLACED_ACCESSORS).map(a => {
              const actorName = a.actor_title || shortId(a.actor_conv_id);
              const replacedVia = a.reason || 'replaced';
              const replacedAge = a.replaced_at ? ' · ' + relTime(a.replaced_at) : '';
              return `
              <tr>
                <td>${onlineDot(a.online)}</td>
                <td class="id">${esc(shortId(a.conv_id))}</td>
                <td><span class="rowname">${esc(a.title || '(untitled)')}</span></td>
                <td><span class="muted" title="${esc((a.actor_title || a.actor_conv_id) + (a.actor_retired ? ' (retired actor)' : ''))}">${esc(actorName)}${a.actor_retired ? ' 🪦' : ''}</span></td>
                <td><span class="last-hook" title="${esc(a.replaced_at || '')}">${esc(replacedVia)}${esc(replacedAge)}</span></td>
                <td><div class="row-actions">
                  <button data-act="copy-generation-id" data-conv="${esc(a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" title="Copy this generation's full conv-id — inspect it out-of-band with 'claude --resume <id>' from its dir, or 'tclaude agent seance --target <id>'">copy id</button>
                  <button class="danger" data-act="delete-generation" data-conv="${esc(a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" data-actor="${esc(actorName)}" title="Permanently delete just this past generation (its .jsonl + DB rows). The live agent and its other generations are untouched.">delete generation</button>
                </div></td>
              </tr>`;
            }).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${esc(g.name)}</strong>
        <span class="group-virtual-badge" title="A virtual group, not a real one — superseded past generations of agents (left behind by reincarnate / /clear). Archival and read-mostly: copy a conv-id to inspect it, or delete a generation to prune it. The live agent is never affected.">virtual</span>
        <span class="muted">— ${total} replaced generation${total === 1 ? '' : 's'}</span>
      </summary>
      <div class="subtable">
        ${body}
        ${pager}
      </div>
    </details>
  `;
}

// renderVirtualPendingGroup renders the synthetic "Pending" group —
// dashboard spawns whose conv-id hasn't materialised yet (JOH-205 inc2).
// Each row is a live-but-gated spawn keyed on its LABEL (it has no
// conv-id yet); its only action is the focus button, which opens the pane
// (POST /api/pending/focus/{label}) so the operator can clear the startup
// gate. Not a drag source or target — a pending spawn isn't an agent yet,
// so there is nothing to drag into a group. Defaults OPEN: it's an alert,
// so the rows should be visible without a click (collapsible all the same,
// persisted like every other group). The empty branch is defensive — the
// caller (tabs.js) only builds this group when there are pending spawns.
function renderVirtualPendingGroup(g) {
  const members = g.members || [];
  const key = g.key || g.name;
  const isOpen = dashPrefs.getItem('tclaude.dash.group.' + key) !== '0';
  const body = members.length === 0
    ? '<div class="muted">(no pending spawns)</div>'
    : `
        <table>
          ${sortHead('pending', PENDING_COLS)}
          <tbody>
            ${applySort('pending', members, PENDING_ACCESSORS).map(p => {
              const focusBtn = p.online
                ? `<button class="primary" data-act="focus-pending" data-label="${esc(p.label)}" title="Open this spawn's pane so you can clear its startup gate — trust the dir, dismiss the new-config prompt, or finish OpenAI auth. Once cleared it takes its first turn and becomes a normal agent.">focus</button>`
                : `<button disabled title="This spawn's tmux pane is gone — it can no longer be focused, and will clear from this list shortly.">focus</button>`;
              return `
              <tr>
                <td>${onlineDot(p.online)}</td>
                <td class="id">${esc(p.label)}</td>
                <td><span class="rowname">${esc(p.name || p.role || '(unnamed)')}</span></td>
                <td>${esc(p.group || '(none)')}</td>
                <td><span class="muted" title="${esc(p.cwd || '')}">${esc(p.cwd ? shortCwd(p.cwd) : '')}</span></td>
                <td><span class="last-hook">${esc(p.created_at ? relTime(p.created_at) : '')}</span></td>
                <td><div class="row-actions">${focusBtn}</div></td>
              </tr>`;
            }).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${esc(g.name)}</strong>
        <span class="group-virtual-badge" title="A virtual group, not a real one — dashboard spawns waiting to clear a startup gate (untrusted dir / config prompt / OpenAI auth). Click a row's focus button to open its pane and clear the gate; it then becomes a normal agent.">virtual</span>
        <span class="muted">— ${members.length} pending spawn${members.length === 1 ? '' : 's'}</span>
      </summary>
      <div class="subtable">
        ${body}
      </div>
    </details>
  `;
}

// groupNotifyMenuItem renders the per-group OS-notification toggle as a
// ⚙ options-menu row. It used to be a 🔔/🔕 chip in the group summary's
// header strip, but that strip was getting crowded, so the control moved
// into the menu. One click still flips agent_groups.notify_enabled; the
// data-act / data-enabled the row-action dispatcher reads are unchanged.
function groupNotifyMenuItem(g) {
  const on = g.notify_enabled;
  const text = on ? '🔔 notifications: on' : '🔕 notifications: muted';
  const tip = on
    ? 'OS notifications on for this group’s agents — click to mute the whole group (a per-agent 🔔 override still notifies)'
    : 'OS notifications MUTED for this group’s agents — click to unmute (members with a per-agent 🔔 override notify anyway)';
  return `<button data-act="toggle-group-notify" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-enabled="${on ? '1' : '0'}" title="${esc(tip)}">${esc(text)}</button>`;
}

// groupActionsHTML renders a real group header's action cluster. The
// three most-used controls — spawn, power on, shutdown — stay at the
// TOP LEVEL; the rest (add member, multicast cron, message, startup
// context, notifications, rename, export, cleanup, windows, delete) are
// collected behind the ⚙ options cog so the header stays readable. Every
// button keeps the exact data-act / data-* the row-action dispatcher
// already expects — only their DOM position moves.
// Feather "user-plus": a person silhouette with a + alongside. Same
// monochrome-via-currentColor convention as the helpers.js eye icons.
const SPAWN_ICO_SVG = '<svg class="spawn-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="8.5" cy="7" r="4"/><line x1="20" y1="8" x2="20" y2="14"/><line x1="23" y1="11" x2="17" y2="11"/></svg>';

function groupActionsHTML(g, members) {
  // Startup-context menu item: the label switches between
  // "📋 startup context (N chars)…" when one is configured and
  // "📋 set startup context…" when it isn't. The ellipsis matches
  // the "🪟 windows…" pattern signalling "opens a modal".
  const ctxLen = g.default_context ? g.default_context.length : 0;
  const ctxLabel = ctxLen > 0
    ? `📋 startup context (${ctxLen} chars)…`
    : `📋 set startup context…`;
  const ctxTitle = ctxLen > 0
    ? `Startup context (${ctxLen} chars) delivered to the inbox of agents spawned here — click to edit`
    : 'No startup context — click to set one';
  // Quick-options pin toggle — only meaningful while auto-fold is on, so
  // it's omitted in "expanded" mode (nothing folds there). Pinning is a
  // per-browser dashPref (tclaude.dash.quickpin.<name>); render.js stamps
  // .quick-pinned on the <details> so the fold CSS skips this group.
  const qoFoldActive = !lastSnapshot || lastSnapshot.group_quick_options !== 'expanded';
  const qoPinned = dashPrefs.getItem('tclaude.dash.quickpin.' + g.name) === '1';
  const quickPinItem = qoFoldActive
    ? `<button data-act="toggle-quick-pin" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-pinned="${qoPinned ? '1' : '0'}" title="${qoPinned ? 'Quick options are pinned open for this group — its header chips stay expanded even though auto-fold is on. Click to let them fold to icons again.' : "Pin this group's quick options open — its header chips stay expanded (not folded to icons) even while auto-fold is on. Click to fold them with the rest."}">${qoPinned ? '📌 unpin quick options' : '📌 pin quick options open'}</button>`
    : '';
  const menu =
    `<button data-act="add-member" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Add an existing conversation to this group">+ add member</button>`
    + `<button data-act="cron-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name, scopeGroup: g.name}))}' data-label="${esc(g.name)}" title="Schedule a recurring cron job scoped to ${esc(g.name)} — multicast the whole group, or nudge a single member">⏰ multicast</button>`
    + `<button data-act="message-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name}))}' data-label="${esc(g.name)}" title="Send a one-shot message to ${esc(g.name)} — the whole group, or a ticked subset of its members">✉ message</button>`
    + `<button data-act="view-group-messages" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Open this group's messages in the Messages tab — every message touching a member (sent or received) plus the group's own multicasts">🗂 view messages</button>`
    + `<button data-act="set-group-context" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(ctxTitle)}">${ctxLabel}</button>`
    + groupNotifyMenuItem(g)
    + remoteControlPolicyMenuItem(g)
    + quickPinItem
    + `<button data-act="rename-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Rename this group">rename</button>`
    + `<button data-act="clone-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Clone this group — copy every setting (directory, description, startup context, default profile, max-members, notify) and the owners into a new group. Optionally clone the member agents too.">⧉ clone…</button>`
    + `<button data-act="export-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Export this whole group — members, permissions, messages and every conversation — to a portable .zip archive">⤓ export</button>`
    + `<button data-act="cleanup-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Remove confirmed-offline members from this group">🧹 cleanup</button>`
    + `<button data-act="cleanup-worktrees-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Clean up git worktrees — scan this group's repo(s) for stale worktrees (leftovers from retired/deleted agents and hand-made branches) and remove the ones you pick. Main repo and live-agent worktrees are protected.">🧹 cleanup worktrees…</button>`
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

// remoteControlPolicyMenuItem renders the group's remote-control policy as a
// click-to-cycle item in the group ⚙ menu (JOH-262). The group policy is a spawn
// DEFAULT that overrides the spawn profile's own remote_control default: "inherit"
// defers to the profile, "optin" defaults Remote Access on at spawn, "deny"
// defaults it off. It is a default, not a lock — it pre-fills the spawn modal's
// checkbox, and an explicit per-spawn value (the checkbox / CLI flag) still wins.
// One click cycles inherit → opt-in → deny → inherit (the same cycle pattern as
// the per-agent notify bell); data-policy is the current value and data-next is
// the value one click sends. The wire tokens (inherit/optin/deny) match the
// group PATCH's remote_control_policy field exactly. It lives in the cog menu
// (not as a header chip) so the group summary stays terse — the menu re-renders
// after each PATCH, so reopening the cog shows the advanced policy.
function remoteControlPolicyMenuItem(g) {
  const policy = g.remote_control_policy || 'inherit';
  const next = policy === 'inherit' ? 'optin' : policy === 'optin' ? 'deny' : 'inherit';
  const label = policy === 'optin' ? 'opt-in' : policy === 'deny' ? 'deny' : 'inherit';
  const ico = policy === 'deny' ? '🚫' : '📱';
  const tip = policy === 'inherit'
    ? "Remote-control policy: inherit — defers to each spawn profile's own default. Click to set a group default: opt-in pre-checks Remote Access on, deny pre-checks it off, for agents spawned into this group (a per-spawn checkbox / flag still wins)."
    : policy === 'optin'
    ? 'Remote-control policy: opt-in — defaults Claude Code Remote Access ON for agents spawned into this group (overrides the profile default; a per-spawn checkbox / flag still wins). Click to cycle to deny.'
    : 'Remote-control policy: deny — defaults Remote Access OFF for agents spawned into this group (overrides the profile default; a per-spawn checkbox / flag still wins). Click to cycle back to inherit.';
  return `<button data-act="set-group-remote-control" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-policy="${esc(policy)}" data-next="${esc(next)}" title="${esc(tip)}">${ico} remote policy: ${esc(label)}</button>`;
}

// activityStyles reads the per-mode activity-bot styles from the snapshot
// (config dashboard.activity_bots). Falls back to the Go-side defaults
// (regular emoji, slop sprites) when the flag is absent — a pre-flag
// daemon, or the moment before the first snapshot lands. The wizard row is a
// single fantasy-glyph re-skin (no emoji/sprites choice) with no config knob
// today, so it defaults ON — the 🧙 theme always gets its own bots; the
// `ab.wizard` read is just a forward-compatible hook for a future on/off.
function activityStyles() {
  const ab = (lastSnapshot && lastSnapshot.activity_bots) || {};
  return { regular: ab.regular || 'emoji', slop: ab.slop || 'sprites', wizard: ab.wizard || 'wizard' };
}

// groupActivityChip builds the tri-mode bot row for a group <summary>
// using the configured per-mode styles. Empty when every mode is 'off'
// or the group has no members.
function groupActivityChip(members) {
  const st = activityStyles();
  // No live-theme arg: each wrapper carries its own fixed-flavour tooltip
  // (regular/slop → plain nouns, wizard → the arcane "N familiars channeling"),
  // and CSS reveals the one for the active theme — so a mid-session flip is
  // instantly correct, no 2s re-render lag on the hover text.
  return groupActivityHTML(members, st.regular, st.slop, st.wizard);
}

function renderGroups(groups) {
  if (!groups || !groups.length) {
    return '<div class="empty">No groups yet. Create one with the <strong>+ new group</strong> button above.</div>';
  }
  return groups.map(g => {
    if (g.virtual) return g.conversations ? renderVirtualConversationsGroup(g)
      : g.retired ? renderVirtualRetiredGroup(g)
      : g.replaced ? renderVirtualReplacedGroup(g)
      : g.pending ? renderVirtualPendingGroup(g)
      : renderVirtualGroup(g);
    const members = g.members || [];
    // Offline visibility: per-group override falls back to the
    // tab-wide checkbox. Hidden members still count toward the
    // 👥 chip's online/total/cap counts so the header stays truthful.
    const visible = groupShowOffline(g.name) ? members : members.filter(m => m.online);
    const hiddenOffline = members.length - visible.length;
    // Restore expanded state across the 2s polling re-renders by
    // keying on group name. Persisted in localStorage so it
    // survives a full page reload too.
    const isOpen = dashPrefs.getItem('tclaude.dash.group.' + g.name) === '1';
    // Quick-options pin: a per-group, per-browser opt-out of the
    // body.group-quick-fold accordion. A pinned group carries .quick-pinned
    // on its <details>, which the fold CSS excludes, so its chips stay
    // expanded even when auto-fold is on. Persisted in dashPrefs (server-side,
    // like the open/closed + offline-visibility view state). Toggled from the
    // group ⚙ menu (toggle-quick-pin). No effect in "expanded" mode — nothing
    // folds there to opt out of.
    const quickPinned = dashPrefs.getItem('tclaude.dash.quickpin.' + g.name) === '1';
    // Compose the <details> class list: .quick-pinned opts out of folding,
    // .quick-hover re-stamps the JS-tracked hover so the reveal survives the
    // 2s re-render (see bindGroupQuickHover / hoveredGroupKey in refresh.js).
    const detailsClasses = [];
    if (quickPinned) detailsClasses.push('quick-pinned');
    if (!quickPinned && g.name === hoveredGroupKey) detailsClasses.push('quick-hover');
    const detailsClassAttr = detailsClasses.length ? ` class="${detailsClasses.join(' ')}"` : '';
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
    <details data-group-key="${esc(g.name)}" data-dnd-target-group="${esc(g.name)}"${detailsClassAttr}${isOpen ? ' open' : ''}>
      <summary draggable="true" data-group-reorder="${esc(g.name)}" title="Drag this header to reorder the group">
        <strong class="group-name" data-group-name="${esc(g.name)}">${esc(g.name)}</strong>
        ${groupActivityChip(members)}
        <span class="group-descr${g.descr ? '' : ' unset'}" data-act="set-group-descr" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-descr="${esc(g.descr || '')}" title="${g.descr ? 'Group description — click to edit' : 'No description — click to set one'}">📝<span class="qo-text"> ${g.descr ? esc(g.descr) : 'no description'}</span></span>
        <span class="group-default-cwd${g.default_cwd ? '' : ' unset'}" data-act="set-group-dir" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-cwd="${esc(g.default_cwd || '')}" title="${g.default_cwd ? 'Default spawn directory: ' + esc(g.default_cwd) + ' — click the text to edit, the 📁 to browse' : 'No default spawn directory — click the text to type one, the 📁 to browse'}"><span class="gdc-pick" data-act="pick-group-dir" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-cwd="${esc(g.default_cwd || '')}" title="Browse for a directory with a native picker">📁</span><span class="qo-text"> ${g.default_cwd ? esc(shortCwd(g.default_cwd)) : 'no default dir'}</span></span>
        <span class="${capChipClass}" data-act="set-group-max-members" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-max="${g.max_members || 0}" title="${esc(capChipTitle)}">👥 ${capChipText}</span>
        <span class="group-default-model${g.default_profile ? '' : ' unset'}" data-act="set-group-profile" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-profile="${esc(g.default_profile || '')}" title="${g.default_profile ? 'Default spawn profile for agents spawned into this group: ' + esc(g.default_profile) + ' — fills blank launch fields at spawn. Click to change.' : 'No default spawn profile — click to set one. (Spawns use their own fields until set.)'}">🧠<span class="qo-text">${g.default_profile ? ' ' + esc(g.default_profile) : ''}</span></span>
        ${g.virtual ? '' : renderGroupLinkChips(g.name)}
      </summary>
      <div class="subtable">
        <div class="group-header-actions">${groupActionsHTML(g, members)}</div>
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
      </div>
    </details>
  `;
  }).join('');
}

// renderGroupLinkChips: compact, always-visible (even when collapsed)
// link badges for a group's <summary> header. Outbound chips (→ X) name
// groups this group can message; inbound chips (← Y) name groups that can
// message it. Each chip carries data-act="links-manage", so a click opens
// the full Links… management overlay (and the row-action dispatcher's
// e.preventDefault() stops it from toggling the <details>). Returns '' for
// a group with no links so the header stays uncluttered. The expanded body
// still shows the editable per-group links table (renderGroupLinksSection).
function renderGroupLinkChips(groupName) {
  const all = (lastSnapshot && lastSnapshot.links) || [];
  const out = all.filter(l => l.from === groupName);
  const inc = all.filter(l => l.to === groupName);
  if (!out.length && !inc.length) return '';
  const chip = (other, dir, mode) => {
    const arrow = dir === 'out' ? '→' : '←';
    const title = dir === 'out'
      ? `Members of this group can message "${other}" (${mode}) — click to manage links`
      : `Members of "${other}" can message this group (${mode}) — click to manage links`;
    return `<span class="group-link-chip ${dir}" data-act="links-manage" title="${esc(title)}">${arrow} ${esc(other || '(deleted)')}</span>`;
  };
  const chips = [
    ...out.map(l => chip(l.to, 'out', l.mode)),
    ...inc.map(l => chip(l.from, 'in', l.mode)),
  ].join('');
  return `<span class="group-link-chips" data-act="links-manage" title="Inter-group links — click to manage">🔗<span class="qo-text">${chips}</span></span>`;
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
  // Lead each roster row with the stable agent_id (rotation-immune); the
  // cell hover shows both ids via idTooltip. The overrides map is
  // conv-keyed, so resolve agent_id via the agents array (carries both).
  const agentIdByConv = Object.fromEntries((agents || []).map(a => [a.conv_id, a.agent_id]));
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
      ? '<div class="empty">No defaults set.</div>'
      : `<div>${defaults.map(s => `<span class="tag default slug">${esc(s)}</span>`).join(' ')}</div>`}
    <h3>Per-agent overrides <span class="muted" style="font-size:11px">— permanent grant / deny on top of defaults (SQLite agent_permissions). Edit via the per-agent “permissions” button.</span></h3>
    ${rows.length === 0
      ? '<div class="empty">No per-agent overrides yet. Use the per-agent “permissions” button.</div>'
      : `<table>
          <thead><tr><th>ID</th><th>Title</th><th>Granted</th><th>Denied</th></tr></thead>
          <tbody>
            ${rows.map(r => `
              <tr>
                <td class="id" title="${esc(idTooltip(agentIdByConv[r.k], r.k))}">${esc(shortAgentId(agentIdByConv[r.k], r.k))}</td>
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
    <div class="muted" style="font-size:11px;margin-bottom:6px">
      👑 = group ownership confers this slug for owned groups / their members, without an explicit grant (a per-agent deny still suppresses it).
    </div>
    <table>
      <thead><tr><th>Slug</th><th>Owner</th><th>Description</th></tr></thead>
      <tbody>
        ${slugs.map(s => `
          <tr>
            <td><span class="slug">${esc(s.slug)}</span></td>
            <td>${s.owner_implied ? '<span class="owner-badge" title="Conferred by group ownership">👑</span>' : '<span class="muted">—</span>'}</td>
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
  // "live" lights the leading dot green via the #status.live::before
  // rule in dashboard.css. Error path stays red; empty path renders
  // no dot at all (the ::before is gated on :not(:empty)).
  el.classList.toggle('live', !isError && !!text);
}

// === Messages tab — the mail client's nav-button badge ===
// The unread count drives a badge on the Messages nav button, so the
// human sees there's something to read from whatever tab they're on.
// It tracks the human.notify channel (the operator's own folder) — the
// agent-to-agent folders carry their own per-mailbox unread badges in
// the mail-client sidebar (see mail.js). The rest of the Messages tab —
// sidebar / list / reading pane — lives in mail.js.
function renderMessagesBadge(unread) {
  const badge = $('#messages-badge');
  if (!badge) return;
  badge.textContent = unread > 99 ? '99+' : String(unread);
  badge.hidden = unread === 0;
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
  // Always emit the .urem column, even when a window has no remaining-time
  // text. In the two-line Claude/Codex layout .urem carries a fixed min-width
  // so the windows line up field-for-field between the rows; a harness idle
  // long enough that a window has reset reports no reset time (remaining ""),
  // and dropping the span entirely collapsed its column and slid every
  // following token left — an empty Codex 5h hint pulled its 7d window out
  // from under Claude's. An empty span keeps the reserved width.
  //
  // No leading space before .urem: .uw is a flex row whose `gap` owns the
  // spacing, and a literal space would become a stray anonymous flex item
  // that throws off the monospace column widths in the two-line layout.
  const remText = win.remaining ? '(' + esc(win.remaining) + ')' : '';
  const rem = '<span class="urem">' + remText + '</span>';
  return '<span class="uw"><span class="ulabel">' + label + '</span>'
    + '<span class="ubar">' + usageBar(pct) + '</span>'
    + '<span class="upct">' + Math.round(pct) + '%</span>' + rem + '</span>';
}

// fmtCost renders a dollar amount the way the per-agent harness line
// does: "$0.42", with sub-cent totals as "<1¢" rather than a lying
// "$0.00".
function fmtCost(cost) {
  return cost >= 0.005 ? '$' + cost.toFixed(2) : '<1¢';
}

// costTokenHTML renders the API cost as one top-bar token. It always
// carries the month-to-date headline ("$0.42 (mtd)") and, whenever any
// spend was recorded today, today's figure ahead of it ("$0.12 (today)
// $0.42 (mtd)"). Today is shown even when it equals the mtd — e.g. on the
// first of the month, when the whole month's spend is today's, both read
// the same number on purpose, so the "(today)" figure never silently
// vanishes on the day a user is most likely to be watching it. (today is
// always ≤ mtd — the same DB delta walk windowed to today vs. the month,
// both scaled by the same cost factor — so the pair never reads inverted;
// see the TodayCostUSD ≤ TotalCostUSD invariant in usage.go.) The token
// links to the Costs tab (data-goto-tab, wired in costs.js).
function costTokenHTML(today, mtd) {
  const amt = (v, label) => '<span class="ucost-amt">' + esc(fmtCost(v)) + '</span>'
    + ' <span class="urem">(' + label + ')</span>';
  const parts = [];
  if (today > 0) parts.push(amt(today, 'today'));
  parts.push(amt(mtd, 'mtd'));
  return '<span class="uw ucost" data-goto-tab="costs">'
    + '<span class="ulabel">api</span>' + parts.join(' ') + '</span>';
}

// subscriptionWindowsHTML builds the 5h/7d window tokens for one usage
// source (the Claude top-level fields, or the nested .codex object —
// both share the {available, five_hour, seven_day} shape). Returns an
// array of token strings, empty when the source reports nothing.
//
// When the source is available it always emits BOTH windows — a missing
// five_hour/seven_day renders as a 0% placeholder bar rather than being
// dropped. The Claude and Codex rows are stacked and column-aligned (see
// renderUsage), so the two rows MUST carry the same window slots or they
// desync: a row with only a 7d token would slide its 7d under the other
// row's 5h. The Go snapshot already pairs the windows ("show both or
// neither" — pairUsageWindows zero-fills the absent one), but rendering
// the placeholder here too keeps the layout aligned independent of that
// guarantee — better a little too much usage data than a misaligned row.
function subscriptionWindowsHTML(src) {
  const wins = [];
  if (src && src.available) {
    const zero = { pct: 0, remaining: '' };
    wins.push(usageWindowHTML('5h', src.five_hour || zero));
    wins.push(usageWindowHTML('7d', src.seven_day || zero));
  }
  return wins;
}

// usageLineHTML wraps one labelled readout line for the two-line
// Claude/Codex layout: a right-aligned monospace source label (so the
// colons stack — "Claude:" over " Codex:") followed by that source's
// window tokens. An empty label keeps the column width so a trailing
// cost line aligns its tokens under the windows above.
function usageLineHTML(label, tokens) {
  return '<span class="uline"><span class="usrc">' + esc(label) + '</span>'
    + tokens.join('') + '</span>';
}

// renderUsage paints the top-bar readout from snapshot.usage:
// subscription windows when available, plus the month-to-date API
// cost summed across agent sessions when any was recorded — an
// API-billing account has cost but no windows, so the cost token
// replaces "usage: n/a" there; an account with both shows them side
// by side. Only when neither exists does it degrade to a muted
// "usage: n/a" rather than a broken or error state.
//
// When Codex usage is present (snapshot.usage.codex), the readout
// switches to a labelled two-line layout — "Claude" over "Codex",
// monospace-aligned so the windows line up in one column — instead of
// the single unlabelled row the Claude-only case keeps.
function renderUsage(u) {
  const el = $('#usage');
  if (!el) return;
  const titles = [];

  const claudeWins = subscriptionWindowsHTML(u);
  if (claudeWins.length) titles.push('Claude subscription usage limits — 5-hour and 7-day rolling windows');
  const codexWins = subscriptionWindowsHTML(u && u.codex);
  if (codexWins.length) titles.push('Codex subscription usage limits — 5-hour and weekly rolling windows');

  const mtd = Number((u && u.total_cost_usd) || 0);
  const today = Number((u && u.today_cost_usd) || 0);
  const costHTML = mtd > 0 ? costTokenHTML(today, mtd) : '';
  if (costHTML) {
    let t = `API cost month-to-date: $${mtd.toFixed(4)}, summed across agent sessions recorded in tclaude's DB`;
    if (today > 0) t += ` · today: $${today.toFixed(4)}`;
    titles.push(t + ' · click to open the Costs tab');
  }

  // Codex data present → labelled two-line (Claude / Codex) layout, with
  // the cost token on its own column-aligned line below when there is one.
  if (codexWins.length) {
    el.classList.add('multiline');
    const lines = [];
    if (claudeWins.length) lines.push(usageLineHTML('Claude:', claudeWins));
    lines.push(usageLineHTML('Codex:', codexWins));
    if (costHTML) lines.push(usageLineHTML('', [costHTML]));
    el.classList.remove('na');
    el.innerHTML = lines.join('');
    el.title = titles.join(' · ');
    return;
  }

  // Claude only → the original single unlabelled row (windows then cost).
  el.classList.remove('multiline');
  const wins = claudeWins.slice();
  if (costHTML) wins.push(costHTML);
  if (wins.length) {
    el.classList.remove('na');
    el.innerHTML = wins.join('');
    el.title = titles.join(' · ');
  } else {
    el.classList.add('na');
    el.textContent = 'usage: n/a';
    el.title = 'Subscription usage data is currently unavailable';
  }
}

// renderNotifyGlobal paints the top-bar master notification bell from
// snapshot.notifications_enabled (config.notifications.enabled). The
// button stays hidden until the first snapshot so it never flashes a
// wrong state. Clicking the bell opens the per-type popover
// (notify-menu.js), which fetches its own fresh state — the master on/off
// lives inside it now, not on the bell itself. data-enabled is kept in
// sync as a plain reflection of the snapshot for any external reader.
function renderNotifyGlobal(enabled) {
  const el = $('#notify-global');
  if (!el) return;
  el.hidden = false;
  el.classList.toggle('muted', !enabled);
  el.setAttribute('data-enabled', enabled ? '1' : '0');
  el.textContent = enabled ? '🔔' : '🔕';
  el.title = enabled
    ? 'OS notifications ON — click to choose which notifications you want'
    : 'OS notifications OFF — nothing notifies, regardless of group/agent bells. Click to configure.';
}

// renderDashDefaultProfile paints the groups-tab 🧠 chip showing the
// DASHBOARD-level default spawn profile (a dashPrefs value, not server
// state). It pre-fills the spawn dialog as the fallback when a group has no
// default profile of its own. The chip is click-to-pick (set-dash-profile in
// row-actions.js); the data-profile attr carries the current name for the
// picker. Replaces the retired user-default-model chip (JOH-210 inc3) — the
// settings.json model-editing affordance is gone from the dashboard; the
// /api/claude-settings/default-model endpoint and the snapshot's
// user_default_model field (which the spawn modal's Default label still
// reads) are untouched.
function renderDashDefaultProfile() {
  const el = $('#dashboard-default-profile');
  if (!el) return;
  const name = getDashDefaultProfile();
  el.classList.toggle('unset', !name);
  el.setAttribute('data-profile', name);
  el.textContent = '🧠' + (name ? ' ' + name : '');
  el.title = name
    ? `Dashboard default spawn profile: ${name} — pre-fills the spawn dialog when the chosen group has no default profile of its own. Click to change.`
    : 'No dashboard default spawn profile — click to set one. (Pre-fills the spawn dialog as a fallback after a group’s own default.)';
}

// renderGlobalActivity paints the top-bar #global-activity slot: the
// same deduped bot row as the group headers, but aggregated across every
// real group PLUS the ungrouped bucket — one glance tells you if anything
// anywhere is working or needs you, without scanning the list or
// unfolding groups. The tooltip breaks the total down per group (only
// those with live, non-offline activity, so it stays short). Group names
// go through the .title DOM PROPERTY (never innerHTML), so they're inert
// — no escaping needed; the bot row HTML carries no caller input.
function renderGlobalActivity() {
  const el = $('#global-activity');
  if (!el) return;
  const snap = lastSnapshot;
  if (!snap) { el.innerHTML = ''; el.removeAttribute('title'); return; }
  const groups = snap.groups || [];
  const lists = groups.map(g => g.members || []);
  lists.push(snap.ungrouped || []);
  // aggregateActivity dedups by conv_id — an agent in several groups is in
  // each group's member list, so a naive flatten would multiply its counts.
  const s = aggregateActivity(lists);
  // Emit a regular-mode + slop-mode + wizard-mode wrapper in their configured
  // styles, exactly like the group chips — CSS shows the right one per active
  // theme. Clear out when every mode renders nothing (style off, or zero
  // members).
  const st = activityStyles();
  // The container's own title (a DOM property, set below) can't CSS-swap, so
  // it's re-flavoured live per theme; the per-wrapper bots carry fixed
  // flavours (regular/slop plain, wizard arcane) and CSS reveals the active
  // one, so a mid-session flip re-flavours the container title on the next
  // 2s poll while the bot glyphs + their tooltips swap instantly.
  const theme = isWizardActive() ? 'wizard' : '';
  const wrap = (cls, inner) =>
    inner ? `<span class="${cls} level-${s.level}">${inner}</span>` : '';
  const reg = wrap('ga-regular', styledBotsHTML(s, st.regular));
  const slop = wrap('ga-slop', styledBotsHTML(s, st.slop));
  const wiz = wrap('ga-wizard', (st.wizard && st.wizard !== 'off') ? wizardBotsHTML(s) : '');
  if (!reg && !slop && !wiz) { el.innerHTML = ''; el.removeAttribute('title'); return; }
  // Per-source breakdown for the tooltip — skip sources that are only
  // offline so the list highlights what's actually live.
  const lines = [];
  for (const g of groups) {
    const gs = activitySummary(g.members || []);
    if (gs.present.length && gs.level !== 'offline') lines.push(`${g.name}: ${themedSummaryText(gs, theme)}`);
  }
  const ung = activitySummary(snap.ungrouped || []);
  if (ung.present.length && ung.level !== 'offline') lines.push(`Ungrouped: ${themedSummaryText(ung, theme)}`);
  el.innerHTML = reg + slop + wiz;
  syncBotAnimations(); // re-phase to wall-clock so the 2s poll doesn't restart-jump
  el.title = `Activity across all groups — ${themedSummaryText(s, theme)}`
    + (lines.length ? '\n' + lines.join('\n') : '');
}

export {
  renderGroups, renderGlobalActivity, renderPermissions, renderSlugs, showStatus,
  renderMessagesBadge, renderUsage, renderDashDefaultProfile,
  renderNotifyGlobal,
};
