// render.js — the dashboard render functions.
//
// Builds the HTML for the group / member / permissions / slugs /
// messages / usage views from snapshot data. Extracted from
// dashboard.js as part of the Stage 2 module split.

import {
  $, esc, themeWords, shortId, shortAgentId, idTooltip, onlineDot, agentStatusDot, harnessLine, sandboxBadge, statePill, slopMachine, wizardPill, contextMeter, activityBadges,
  harnessCanRename, harnessCanRemoteControl,
  roleCell, descrCell, memberActions, ungroupedMemberActions, actionCog, relTime, shortCwd,
  cwdCell, branchCell, taskCell, offlineDefault, groupShowOffline,
} from './helpers.js';
import {
  sortHead, applySort, MEMBER_ACCESSORS, REPLACED_COLS, REPLACED_ACCESSORS,
  RETIRED_COLS, RETIRED_ACCESSORS, CONVERSATIONS_COLS, CONVERSATIONS_ACCESSORS,
  PENDING_COLS, PENDING_ACCESSORS,
} from './sort.js';
import { visibleMemberCols, memberColHidden } from './member-columns.js';
import { groupActivityHTML } from './group-activity.js';
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
// idPair is the optional "<agent_id> / <conv-id>" hover string, passed in
// only when the ID column is hidden (see memberRowHTML) so the identifiers
// stay reachable off the name; empty otherwise.
function renameNameCell(m, state, idPair = '') {
  const name = esc(m.title || '(unnamed)');
  // idPair (the "<agent_id> / <conv-id>" hover string) is folded into the
  // name's title ONLY when the ID column is hidden — the ID cell's own
  // idTooltip is gone in that view, so surfacing the pair here keeps both
  // identifiers reachable/copyable off a hover (the brief's suggestion).
  const idPrefix = idPair ? `${esc(idPair)} — ` : '';
  if (harnessCanRename(lastSnapshot, state.harness)) {
    return `<span class="rowname-text" data-act="rename-name" data-conv="${esc(m.conv_id)}" data-agent="${esc(m.agent_id || m.conv_id)}" data-current="${esc(m.title || '')}" data-label="${esc(m.title || m.conv_id)}" title="${idPrefix}Click to rename this agent — Enter saves, Esc cancels">${name}</span>`;
  }
  return `<span class="rowname-text rowname-fixed" title="${idPrefix}This agent's harness does not support renaming">${name}</span>`;
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
  // When the ID column is hidden, fold the id pair into the name cell's
  // hover so the identifiers aren't lost with the column.
  const idHidden = memberColHidden('id');
  const namePair = idHidden ? idTooltip(m.agent_id, m.conv_id) : '';
  // One <td> per MEMBER_COLS key. Only the visible keys are emitted, in
  // column order (visibleMemberCols) — the SAME filter sortHead uses for the
  // header — so header and body can never drift. A new column plugs in by
  // adding a MEMBER_COLS entry and a cell here under its key.
  const cells = {
    ctl:    `<td><div class="agent-ctl">${agentStatusDot(m)}${actions}</div>${harnessLine(m)}${sandboxBadge(m)}</td>`,
    id:     `<td class="id" title="${esc(idTooltip(m.agent_id, m.conv_id))}">${esc(shortAgentId(m.agent_id, m.conv_id))}</td>`,
    title:  `<td class="name-cell">
                  <div class="rowname">${renameNameCell(m, state, namePair)}${sudoBadge(sudoByConv[m.conv_id], m.conv_id)}</div>
                </td>`,
    state:  `<td class="state-cell">
                  ${contextMeter(state)}
                  ${statePill(state, m.online)}
                  ${slopMachine(state, m.online, m.conv_id)}
                  ${wizardPill(state, m.online, m.conv_id)}
                  ${m.online ? activityBadges(state) : ''}
                </td>`,
    last:   `<td><span class="last-hook">${esc(relTime(state.last_hook))}</span></td>`,
    age:    `<td><span class="last-hook" title="${esc(m.created_at || '')}">${esc(relTime(m.created_at))}</span></td>`,
    cwd:    `<td>${cwdCell(m)}</td>`,
    branch: `<td>${branchCell(m)}</td>`,
    role:   `<td>${roleCell(m, ctx.group)}</td>`,
    task:   `<td class="task-cell">${taskCell(m)}</td>`,
    descr:  `<td class="descr-cell">${descrCell(m, ctx.group)}</td>`,
  };
  // A visible column with no cell here falls back to an EMPTY <td>, not ''.
  // That keeps the body's cell count equal to the header's th count, so a
  // future MEMBER_COLS entry added without its matching cell degrades to a
  // blank column (fails ALIGNED) instead of silently shifting every later
  // cell left into a misaligned table — the invariant this module promises.
  const body = visibleMemberCols().map((c) => cells[c.key] ?? '<td></td>').join('');
  return `
              <tr class="dnd-draggable" draggable="true" data-key="${esc(m.conv_id)}" ${dndSource}
                  data-dnd-conv="${esc(m.conv_id)}"
                  data-dnd-agent="${esc(m.agent_id || m.conv_id)}"
                  data-dnd-label="${esc(m.title || m.conv_id)}">${body}</tr>`;
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
    ? `<div class="muted">${themeWords('(no ungrouped agents)', '(no unbound familiars)')}</div>`
    : visible.length === 0
    ? `<div class="muted">${themeWords(
        `(${hiddenOffline} offline agent${hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${hiddenOffline === 1 ? 'it' : 'them'})`,
        `(${hiddenOffline} slumbering familiar${hiddenOffline === 1 ? '' : 's'} hidden — enable "show slumbering" to reveal ${hiddenOffline === 1 ? 'it' : 'them'})`,
      )}</div>`
    : `
        <table>
          ${sortHead('members', visibleMemberCols())}
          <tbody>
            ${applySort('members', visible, MEMBER_ACCESSORS).map(m => memberRowHTML(m, {ungrouped: true})).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}" data-dnd-target-ungrouped="1"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${themeWords(g.name, 'Unbound')}</strong>
        ${groupActivityChip(members)}
        <span class="group-virtual-badge" title="${esc(isWizardActive() ? "An ethereal party, not a true one — it cannot be renamed, dispelled, whispered to, or scheduled. It gathers familiars bound to no party." : "A virtual group, not a real one — it can't be renamed, deleted, messaged or scheduled. It just collects agents that aren't in any group.")}">${themeWords('virtual', 'ethereal')}</span>
        <span class="muted">— ${themeWords(
          `${members.length} agent${members.length === 1 ? '' : 's'} not in any group${hiddenOffline > 0 ? ` · ${hiddenOffline} offline hidden` : ''}`,
          `${members.length} unbound familiar${members.length === 1 ? '' : 's'}${hiddenOffline > 0 ? ` · ${hiddenOffline} slumbering hidden` : ''}`,
        )}</span>
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
    ? `<div class="muted">${themeWords('(no non-agent conversations)', '(no plain scrolls)')}</div>`
    : `
        <table>
          ${sortHead('conversations', CONVERSATIONS_COLS)}
          <tbody>
            ${applySort('conversations', members, CONVERSATIONS_ACCESSORS).map(c => `
              <tr class="dnd-draggable" draggable="true" data-key="${esc(c.conv_id)}" data-dnd-source-conversation="1"
                  data-dnd-conv="${esc(c.conv_id)}"
                  data-dnd-agent="${esc(c.agent_id || c.conv_id)}"
                  data-dnd-label="${esc(c.title || c.conv_id)}">
                <td>${onlineDot(c.online)}</td>
                <td class="id">${esc(shortId(c.conv_id))}</td>
                <td><span class="rowname">${esc(c.title || '(untitled)')}</span></td>
                <td><span class="last-hook">${esc(c.modified ? relTime(c.modified) : '')}</span></td>
                <td><div class="row-actions"><button class="primary" data-act="promote-agent" data-conv="${esc(c.conv_id)}" data-label="${esc(c.title || c.conv_id)}" title="${esc(isWizardActive() ? 'Awaken this plain scroll as a familiar' : 'Promote this conversation into an agent')}">${themeWords('promote', 'awaken')}</button></div></td>
              </tr>`).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${themeWords(g.name, 'Plain scrolls')}</strong>
        <span class="group-virtual-badge" title="${esc(isWizardActive() ? 'An ethereal party of plain scrolls without familiars. Drag one onto a party, or awaken it, to call forth a familiar.' : "A virtual group, not a real one — recent conversations that aren't agents. Drag one onto a group, or click promote, to make it an agent.")}">${themeWords('virtual', 'ethereal')}</span>
        <span class="muted">— ${themeWords(
          `${total} conversation${total === 1 ? '' : 's'} that aren't agents`,
          `${total} plain scroll${total === 1 ? '' : 's'} awaiting ${total === 1 ? 'a familiar' : 'familiars'}`,
        )}</span>
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
    ? `<div class="muted">${themeWords('(no retired agents)', '(no banished familiars)')}</div>`
    : `
        <table>
          ${sortHead('retired', RETIRED_COLS)}
          <tbody>
            ${applySort('retired', members, RETIRED_ACCESSORS).map(a => `
              <tr class="dnd-draggable" draggable="true" data-key="${esc(a.conv_id)}" data-dnd-source-retired="1"
                  data-dnd-conv="${esc(a.conv_id)}"
                  data-dnd-agent="${esc(a.agent_id || a.conv_id)}"
                  data-dnd-label="${esc(a.title || a.conv_id)}">
                <td>${onlineDot(a.online)}</td>
                <td class="id" title="${esc(idTooltip(a.agent_id, a.conv_id))}">${esc(shortAgentId(a.agent_id, a.conv_id))}</td>
                <td><span class="rowname">${esc(a.title || '(untitled)')}</span></td>
                <td><span class="last-hook">${esc(a.retired_at ? relTime(a.retired_at) : '')}</span></td>
                <td${a.retired_by ? ` title="${esc(a.retired_by)}"` : ''}>${esc(a.retired_by_display || a.retired_by || '')}</td>
                <td class="muted">${esc(a.retire_reason || '')}</td>
                <td><div class="row-actions"><button class="primary" data-act="reinstate-agent" data-conv="${esc(a.conv_id)}" data-agent="${esc(a.agent_id || a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" title="${esc(isWizardActive() ? 'Restore this banished familiar to active status' : 'Reinstate this agent back to active status')}">${themeWords('reinstate', 'restore')}</button></div></td>
              </tr>`).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}" data-dnd-target-retired="1"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${themeWords(g.name, 'Banished')}</strong>
        <span class="group-virtual-badge" title="${esc(isWizardActive() ? 'An ethereal party of banished familiars returned to plain scrolls. Drag a familiar here to banish it; drag one onto a party, or restore it, to bring it back.' : 'A virtual group, not a real one — agents that were retired (demoted back to plain conversations). Drag an agent here to retire it; drag a retired row onto a group, or click reinstate, to bring it back.')}">${themeWords('virtual', 'ethereal')}</span>
        <span class="muted">— ${themeWords(
          `${total} retired agent${total === 1 ? '' : 's'}`,
          `${total} banished familiar${total === 1 ? '' : 's'}`,
        )}</span>
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
    ? `<div class="muted">${themeWords('(no replaced generations)', '(no past incarnations)')}</div>`
    : `
        <table>
          ${sortHead('replaced', REPLACED_COLS)}
          <tbody>
            ${applySort('replaced', members, REPLACED_ACCESSORS).map(a => {
              const actorName = a.actor_title || shortId(a.actor_conv_id);
              const replacedVia = a.reason || 'replaced';
              const replacedAge = a.replaced_at ? ' · ' + relTime(a.replaced_at) : '';
              return `
              <tr data-key="${esc(a.conv_id)}">
                <td>${onlineDot(a.online)}</td>
                <td class="id">${esc(shortId(a.conv_id))}</td>
                <td><span class="rowname">${esc(a.title || '(untitled)')}</span></td>
                <td><span class="muted" title="${esc((a.actor_title || a.actor_conv_id) + (a.actor_retired ? ' (retired actor)' : ''))}">${esc(actorName)}${a.actor_retired ? ' 🪦' : ''}</span></td>
                <td><span class="last-hook" title="${esc(a.replaced_at || '')}">${esc(replacedVia)}${esc(replacedAge)}</span></td>
                <td><div class="row-actions">
                  <button data-act="copy-generation-id" data-conv="${esc(a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" title="Copy this generation's full conv-id — inspect it out-of-band with 'claude --resume <id>' from its dir, or 'tclaude agent seance --target <id>'">copy id</button>
                  <button class="danger" data-act="delete-generation" data-conv="${esc(a.conv_id)}" data-label="${esc(a.title || a.conv_id)}" data-actor="${esc(actorName)}" title="${esc(isWizardActive() ? 'Forever erase only this past incarnation. The living familiar and its other incarnations remain untouched.' : 'Permanently delete just this past generation (its .jsonl + DB rows). The live agent and its other generations are untouched.')}">${themeWords('delete generation', 'erase incarnation')}</button>
                </div></td>
              </tr>`;
            }).join('')}
          </tbody>
        </table>`;
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${themeWords(g.name, 'Past incarnations')}</strong>
        <span class="group-virtual-badge" title="${esc(isWizardActive() ? 'An ethereal archive of superseded incarnations left by reincarnate / /clear. Copy a conv-id to scry one, or erase an incarnation to prune it. The living familiar is never affected.' : 'A virtual group, not a real one — superseded past generations of agents (left behind by reincarnate / /clear). Archival and read-mostly: copy a conv-id to inspect it, or delete a generation to prune it. The live agent is never affected.')}">${themeWords('virtual', 'ethereal')}</span>
        <span class="muted">— ${themeWords(
          `${total} replaced generation${total === 1 ? '' : 's'}`,
          `${total} past incarnation${total === 1 ? '' : 's'}`,
        )}</span>
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
function pendingFocusButton(p) {
  return p.online
    ? `<button class="primary" data-act="focus-pending" data-label="${esc(p.label)}" title="${esc(isWizardActive() ? "Open this would-be familiar's scrying portal so you can clear its summoning gate. Once cleared, it joins the living roster." : "Open this spawn's pane so you can clear its startup gate — trust the dir, dismiss the new-config prompt, or finish OpenAI auth. Once cleared it takes its first turn and becomes a normal agent.")}">${themeWords('focus', 'open portal')}</button>`
    : `<button disabled title="${esc(isWizardActive() ? 'This would-be familiar’s portal has vanished and will soon fade from the summoning gate.' : "This spawn's tmux pane is gone — it can no longer be focused, and will clear from this list shortly.")}">${themeWords('focus', 'open portal')}</button>`;
}

// pendingDeleteButton clears a stuck spawn that will never enrol — the
// escape hatch for a pending row wedged behind a startup gate the operator
// has given up on. Kills the pane and drops the pending + session rows
// (POST /api/pending/delete/{label}). The matching drag-to-trash gesture
// (dnd.js) invokes the same endpoint. Always enabled — a dead pane is
// exactly the case that most needs clearing.
function pendingDeleteButton(p) {
  return `<button class="danger" data-act="delete-pending" data-label="${esc(p.label)}" title="${esc(isWizardActive() ? 'Dispel this failed summoning and close its portal. It never became a familiar, so no conversation scroll is lost.' : 'Delete this stuck spawn — kills its pane (if any) and removes it from the pending list. Use when a spawn will never clear its startup gate. It never became a real agent, so there is no conversation to keep.')}">${themeWords('🗑 delete', '🪄 dispel')}</button>`;
}

function pendingTableHTML(rows) {
  return `
        <table>
          ${sortHead('pending', PENDING_COLS)}
          <tbody>
            ${applySort('pending', rows, PENDING_ACCESSORS).map(p => `
              <tr data-key="${esc(p.label)}" class="dnd-draggable" draggable="true" data-dnd-pending="1" data-dnd-conv="${esc(p.label)}" data-dnd-label="${esc(p.label)}">
                <td>${onlineDot(p.online)}</td>
                <td class="id">${esc(p.label)}</td>
                <td><span class="rowname">${esc(p.name || p.role || '(unnamed)')}</span></td>
                <td>${esc(p.group || '(none)')}</td>
                <td><span class="muted" title="${esc(p.cwd || '')}">${esc(p.cwd ? shortCwd(p.cwd) : '')}</span></td>
                <td><span class="last-hook">${esc(p.created_at ? relTime(p.created_at) : '')}</span></td>
                <td><div class="row-actions">${pendingFocusButton(p)}${pendingDeleteButton(p)}</div></td>
              </tr>`).join('')}
          </tbody>
        </table>`;
}

function renderGroupPendingBlock(g) {
  const pending = g.pending || [];
  if (!pending.length) return '';
  return `
        <div class="group-pending-block">
          <div class="group-pending-title"><span class="group-pending-title-regular">Pending spawns</span><span class="group-pending-title-wizard">Currently summoning...</span></div>
          ${pendingTableHTML(pending)}
        </div>`;
}

function renderVirtualPendingGroup(g) {
  const members = g.members || [];
  const key = g.key || g.name;
  const isOpen = dashPrefs.getItem('tclaude.dash.group.' + key) !== '0';
  const body = members.length === 0
    ? `<div class="muted">${themeWords('(no pending spawns)', '(no familiars awaiting summoning)')}</div>`
    : pendingTableHTML(members);
  return `
    <details class="group-virtual" data-group-key="${esc(key)}"${isOpen ? ' open' : ''}>
      <summary>
        <strong class="group-name">${themeWords(g.name, 'Summoning')}</strong>
        <span class="group-virtual-badge" title="${esc(isWizardActive() ? "An ethereal antechamber for familiars caught at the summoning gate. Open a familiar's portal to clear the ward; it then joins the living roster." : "A virtual group, not a real one — dashboard spawns waiting to clear a startup gate (untrusted dir / config prompt / OpenAI auth). Click a row's focus button to open its pane and clear the gate; it then becomes a normal agent.")}">${themeWords('virtual', 'ethereal')}</span>
        <span class="muted">— ${themeWords(
          `${members.length} pending spawn${members.length === 1 ? '' : 's'}`,
          `${members.length} familiar${members.length === 1 ? '' : 's'} caught at the summoning gate`,
        )}</span>
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
  const wizardText = on ? '🔔 omens: on' : '🔕 omens: silent';
  const wiz = isWizardActive();
  const tip = on
    ? wiz
      ? 'Omens are enabled for this party’s familiars — click to silence the whole party (a familiar’s own 🔔 boon still speaks)'
      : 'OS notifications on for this group’s agents — click to mute the whole group (a per-agent 🔔 override still notifies)'
    : wiz
      ? 'Omens are SILENCED for this party’s familiars — click to restore them (familiars with their own 🔔 boon still speak)'
      : 'OS notifications MUTED for this group’s agents — click to unmute (members with a per-agent 🔔 override notify anyway)';
  return `<button data-act="toggle-group-notify" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-enabled="${on ? '1' : '0'}" title="${esc(tip)}">${themeWords(text, wizardText)}</button>`;
}

// groupWebTermMenuItem renders the group ⚙ menu's "open web terminal" row — the
// group counterpart of the per-agent "web term" button. It opens an in-browser
// shell in the group's DEFAULT directory (agent_groups.default_cwd), streamed
// into a Terminals-tab pane (js/terminals-tab.js openGroupWebTermPane →
// /api/group-term-ws/{name}). Gated on the group HAVING a default dir: the
// action has no target without one, so the item is simply omitted (the dir chip
// in the summary is where you set one). The tip names the actual directory.
function groupWebTermMenuItem(g) {
  const dir = (g.default_cwd || '').trim();
  if (!dir) return '';
  const tip = isWizardActive()
    ? `Open a browser scrying portal in this party's default directory (${dir})`
    : `Open a terminal in this group's default directory (${dir}), in the browser (always a web terminal — never a native window)`;
  return `<button data-act="group-web-term" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(tip)}">${themeWords('🖥 open web terminal', '🔮 open party portal')}</button>`;
}

// A real group's controls are split across three renderers: spawn / power on /
// shutdown ride the expanded body (groupActionsHTML); the rest (add member,
// multicast cron, message, startup context, notifications, rename, nest,
// export, cleanup, windows, delete) are collected behind the ⚙ options cog,
// which now lives in the SUMMARY header (groupHeaderCogHTML → groupMenuItems).
// Every button keeps the exact data-act / data-* the row-action dispatcher
// expects — only their DOM position moves, and handlers anchor on
// closest('details'), so header vs body placement is transparent to them.
// Feather "user-plus": a person silhouette with a + alongside. Same
// monochrome-via-currentColor convention as the helpers.js eye icons.
const SPAWN_ICO_SVG = '<svg class="spawn-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="8.5" cy="7" r="4"/><line x1="20" y1="8" x2="20" y2="14"/><line x1="23" y1="11" x2="17" y2="11"/></svg>';
const SUBGROUP_ICO_SVG = '<svg class="subgroup-ico" viewBox="0 0 28 24" width="17" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="7" cy="7" r="3"/><circle cx="15" cy="7" r="3"/><path d="M1 21v-2a5 5 0 0 1 5-5h2a5 5 0 0 1 5 5v2"/><path d="M12 14h3a5 5 0 0 1 5 5v2"/><line x1="24" y1="7" x2="24" y2="13"/><line x1="27" y1="10" x2="21" y2="10"/></svg>';

// groupMenuItems builds the ⚙ menu's inner HTML (the flat run of <button>
// rows) for a group. Split out of groupActionsHTML (JOH-392 follow-up) so the
// cog can render up in the group HEADER while the spawn / power buttons stay in
// the expanded body, below the nested subgroups.
function groupMenuItems(g, members) {
  // Startup-context menu item: the label switches between
  // "📋 startup context (N chars)…" when one is configured and
  // "📋 set startup context…" when it isn't. The ellipsis matches
  // the "🪟 windows…" pattern signalling "opens a modal".
  const ctxLen = g.default_context ? g.default_context.length : 0;
  const ctxLabel = ctxLen > 0
    ? `📋 startup context (${ctxLen} chars)…`
    : `📋 set startup context…`;
  const ctxWizardLabel = ctxLen > 0
    ? `📜 standing orders (${ctxLen} chars)…`
    : `📜 decree standing orders…`;
  const ctxTitle = isWizardActive()
    ? (ctxLen > 0
        ? `Standing orders (${ctxLen} chars) delivered to every familiar summoned into this party — click to edit`
        : 'No standing orders — click to decree them')
    : (ctxLen > 0
        ? `Startup context (${ctxLen} chars) delivered to the inbox of agents spawned here — click to edit`
        : 'No startup context — click to set one');
  const groupPermsTitle = isWizardActive()
    ? 'Bestow party boons on every familiar in this party. Membership changes take effect immediately; a personal binding against one still wins.'
    : 'Grant permissions to every current member of this group. Membership changes take effect immediately; an agent-level Deny still wins.';
  // Quick-options pin toggle — only meaningful while auto-fold is on, so
  // it's omitted in "expanded" mode (nothing folds there). Pinning is a
  // per-browser dashPref (tclaude.dash.quickpin.<name>); render.js stamps
  // .quick-pinned on the <details> so the fold CSS skips this group.
  const qoFoldActive = !lastSnapshot || lastSnapshot.group_quick_options !== 'expanded';
  const qoPinned = dashPrefs.getItem('tclaude.dash.quickpin.' + g.name) === '1';
  const quickPinTitle = isWizardActive()
    ? (qoPinned
        ? 'Quick options are pinned open for this party — click to let its enchanted header chips fold again.'
        : "Pin this party's quick options open so its enchanted header chips stay expanded.")
    : (qoPinned
        ? 'Quick options are pinned open for this group — its header chips stay expanded even though auto-fold is on. Click to let them fold to icons again.'
        : "Pin this group's quick options open — its header chips stay expanded (not folded to icons) even while auto-fold is on. Click to fold them with the rest.");
  const quickPinItem = qoFoldActive
    ? `<button data-act="toggle-quick-pin" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-pinned="${qoPinned ? '1' : '0'}" title="${esc(quickPinTitle)}">${qoPinned ? '📌 unpin quick options' : '📌 pin quick options open'}</button>`
    : '';
  const menu =
    `<button data-act="add-member" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? 'Invite an existing conversation to become a familiar in this party' : 'Add an existing conversation to this group')}">${themeWords('+ add member', '+ add familiar')}</button>`
    + `<button data-act="cron-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name, scopeGroup: g.name}))}' data-label="${esc(g.name)}" title="${esc(isWizardActive() ? `Bind a recurring ritual to party ${g.name} — multicast every familiar, or nudge one` : `Schedule a recurring cron job scoped to ${g.name} — multicast the whole group, or nudge a single member`)}">${themeWords('⏰ multicast', '⏳ bind ritual')}</button>`
    + `<button data-act="message-new" data-prefill='${esc(JSON.stringify({targetMode: 'group', groupName: g.name}))}' data-label="${esc(g.name)}" title="${esc(isWizardActive() ? `Send a missive to party ${g.name} — every familiar, or a chosen subset` : `Send a one-shot message to ${g.name} — the whole group, or a ticked subset of its members`)}">${themeWords('✉ message', '✒ missive')}</button>`
    + `<button data-act="view-group-messages" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? "Open this party's missives in the Messages tab" : "Open this group's messages in the Messages tab — every message touching a member (sent or received) plus the group's own multicasts")}">${themeWords('🗂 view messages', '🗂 view missives')}</button>`
    + `<button data-act="set-group-context" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(ctxTitle)}">${themeWords(ctxLabel, ctxWizardLabel)}</button>`
    + `<button data-act="set-group-permissions" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(groupPermsTitle)}">🔑 <span class="group-perms-word-regular">group permissions</span><span class="group-perms-word-wizard">party boons</span>${(g.permissions || []).length ? ` (${g.permissions.length})` : ''}…</button>`
    + groupNotifyMenuItem(g)
    + remoteControlPolicyMenuItem(g)
    + quickPinItem
    + `<button data-act="rename-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? 'Rename this party' : 'Rename this group')}">${themeWords('rename', 'rename party')}</button>`
    + groupNestMenuItems(g)
    + `<button data-act="clone-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? 'Mirror this party — copy its lore, boons, owners, and optionally its familiars into a new party' : 'Clone this group — copy every setting (directory, description, startup context, default profile, group permissions, max-members, notify) and the owners into a new group. Optionally clone the member agents too.')}">${themeWords('⧉ clone…', '⧉ mirror party…')}</button>`
    + `<button data-act="template-from-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? "Trace this party into a reusable summoning circle — preserve its classes, owners, familiars' grimoires, and standing orders" : 'Save this group as a reusable template — snapshot its roles, owners, per-agent permissions and startup context into a blueprint you can instantiate as a fresh team')}">${themeWords('⧉ save as template…', '🕯 trace as circle…')}</button>`
    + `<button data-act="export-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? 'Seal this whole party — familiars, boons, missives, and conversation scrolls — into a portable archive' : 'Export this whole group — members, permissions, messages and every conversation — to a portable .zip archive')}">${themeWords('⤓ export', '⤓ seal party archive')}</button>`
    + `<button data-act="cleanup-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? 'Remove confirmed-slumbering familiars from this party' : 'Remove confirmed-offline members from this group')}">${themeWords('🧹 cleanup', '🧹 tidy party')}</button>`
    + `<button data-act="cleanup-worktrees-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? "Clean up git worktrees in this party's repos. Main repo and live-familiar worktrees are protected." : "Clean up git worktrees — scan this group's repo(s) for stale worktrees (leftovers from retired/deleted agents and hand-made branches) and remove the ones you pick. Main repo and live-agent worktrees are protected.")}">🧹 cleanup worktrees…</button>`
    + `<button data-act="window-modal-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? "Reveal or veil the scrying portals of familiars in this party. The familiars keep channeling either way." : "Focus / unfocus agent windows — open a modal to bulk-attach (focus) or bulk-detach (unfocus) the terminal windows of agents in this group. Window-only: the agents keep running either way.")}">${themeWords('🪟 windows…', '👁 familiars…')}</button>`
    + groupWebTermMenuItem(g)
    + `<button class="danger" data-act="delete-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-members="${members.length}" title="${esc(isWizardActive() ? 'Disband this party' : 'Delete this group')}">${themeWords('delete group', 'disband party')}</button>`;
  return menu;
}

// groupHeaderCogHTML renders the group's ⚙ options cog for the SUMMARY header
// (JOH-392 follow-up: nested subgroups push the body's action cluster down, so
// the most-used control moves up top). It reuses the .group-actions skin — the
// button/menu styling AND the position:relative anchor the dropdown needs — and
// adds .group-header-cog so the fold CSS reveals it only when the group's quick
// options are expanded (hover / open / pinned / expanded mode), or while its
// own menu is open. The menu's data-act handlers anchor on closest('details'),
// which resolves fine from inside the summary.
function groupHeaderCogHTML(g, members) {
  return `<span class="group-actions group-header-cog">${actionCog('group-menu', groupMenuItems(g, members))}</span>`;
}

// groupActionsHTML renders the group's action cluster for the expanded body:
// the spawn CTA + the power-on / shutdown controls. The ⚙ cog moved to the
// header (groupHeaderCogHTML); nested subgroups render ABOVE this cluster.
function groupActionsHTML(g, members) {
  // Spawn sits OUTSIDE .group-actions — the cluster fades to 0.4 at
  // rest, which made spawn (the primary CTA) hard to find. As a
  // sibling chip it keeps full opacity all the time and the
  // blue-accent .spawn-btn skin in dashboard.css makes it pop.
  // Quick controls are icon-only: their title attributes carry the hover copy
  // and aria-labels preserve explicit accessible names. Spawn swaps its user+
  // SVG for 🔮 in wizard mode; create-subgroup uses a two-person-plus SVG
  // (⚔＋ in wizard mode) so the regular icon does not depend on emoji fonts.
  return `<button class="spawn-btn" data-act="spawn-agent" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Spawn agent — start a new tclaude session and join this group" aria-label="Spawn agent into this group">${SPAWN_ICO_SVG}<span class="spawn-btn-label-wizard" aria-hidden="true">🔮</span></button>`
    + `<button class="spawn-btn subgroup-btn" data-act="create-subgroup" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Create subgroup — make a new group nested under this group" aria-label="Create a subgroup under this group">${SUBGROUP_ICO_SVG}<span class="subgroup-icon-wizard" aria-hidden="true">⚔＋</span></button>`
    + `<span class="group-actions">`
    // Power on / shutdown re-flavour their icons to ✨ / 🌙 in wizard mode.
    + `<button data-act="power-on-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" aria-label="Awaken — power on every offline agent in this group" title="Power on — resume every offline agent in this group. Each offline conversation is restarted in a fresh tmux session; agents already running are left alone. Resume only: nothing new is created."><span class="pwr-label-regular" aria-hidden="true">🟢</span><span class="pwr-label-wizard" aria-hidden="true">✨</span></button>`
    + `<button class="warn" data-act="shutdown-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" aria-label="Slumber — shutdown every running agent in this group" title="Shutdown — stop every running agent in this group. Sends /exit, then force-kills any agent still alive after a grace period. Stop only: nothing is deleted, every session can simply be resumed."><span class="pwr-label-regular" aria-hidden="true">🛑</span><span class="pwr-label-wizard" aria-hidden="true">🌙</span></button>`
    // Task-force info toggle — only for a deployed force, which is the only
    // group that HAS an info card (renderForceBlock). Sits alongside
    // awaken/slumber and flips the fold dashPref the card reads.
    + (isDeployedForce(g) ? forceFoldToggleHTML(g) : '')
    + `</span>`;
}

// forceFoldToggleHTML renders the 🎯 show/hide toggle for a deployed force's
// info card, sitting next to awaken/slumber in the group action row. It flips
// the per-browser dashPref that renderForceBlock reads (default open), so the
// human can tuck the card away and bring it back later. groupActionsHTML gates
// it on isDeployedForce — a plain group has no card, hence no toggle. The label
// span-swaps regular → wizard the same way the power buttons do.
function forceFoldToggleHTML(g) {
  const folded = isForceFolded(g.name);
  const tip = folded
    ? 'Task force info card is hidden — click to show it again (mission, phase, roles, re-brief / stand-down controls). Per-browser view state.'
    : 'Hide the task force info card (mission, phase, roles, controls). The 🎯 button stays here to bring it back. Per-browser view state.';
  return `<button class="force-fold-btn${folded ? ' folded' : ''}" data-act="toggle-force-fold" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-folded="${folded ? '1' : '0'}" aria-pressed="${folded ? 'true' : 'false'}" title="${esc(tip)}">🎯<span class="force-fold-label-regular">${folded ? ' show info' : ' hide info'}</span><span class="force-fold-label-wizard">${folded ? ' reveal quest' : ' hide quest'}</span></button>`;
}

// groupNestMenuItems renders the group ⚙ menu's nesting controls (n-level
// groups-in-groups, JOH-392): a "nest under…" item that opens the parent
// picker, plus — only when the group is already nested — a one-click
// "un-nest" that clears the parent (data-act="unnest-group"). Both are
// board-layout only; the picker's dialog spells that out.
function groupNestMenuItems(g) {
  const nestUnder = `<button data-act="nest-group" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? 'Nest this party under another party on the board — layout only' : 'Nest this group under another so it draws inside it on the board — collapse the parent to tuck the subgroup away. Board layout only.')}">${themeWords('📂 nest under…', '📂 nest party under…')}</button>`;
  if (!g.parent) return nestUnder;
  return nestUnder
    + `<button data-act="unnest-group" data-group="${esc(g.name)}" data-parent="${esc(g.parent)}" data-label="${esc(g.name)}" title="${esc(isWizardActive() ? `Move this party back to the top level (currently nested under ${g.parent}).` : `Move this group back to the top level (currently nested under ${g.parent}).`)}">${themeWords(`📂 un-nest (under ${g.parent})`, `📂 un-nest party (under ${g.parent})`)}</button>`;
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
// (regular + wizard emoji, slop sprites) when the flag is absent — a pre-flag
// daemon, or the moment before the first snapshot lands. The wizard row
// defaults to its fantasy-glyph re-skin ('emoji'); config
// dashboard.activity_bots.wizard = 'sprites' opts into the pixel spellcasters
// instead (or 'off' hides it), mirroring the regular/slop style choice.
function activityStyles() {
  const ab = (lastSnapshot && lastSnapshot.activity_bots) || {};
  return { regular: ab.regular || 'emoji', slop: ab.slop || 'sprites', wizard: ab.wizard || 'emoji' };
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

// groupProcessChip renders a group's advisory process state (JOH-242) as a
// compact "◆ phase 2/5: review" chip plus an advance control, both in the
// group summary. The chip's title tooltip carries the ordered phase map (the
// current phase marked, with its roles) and the transition log. Returns '' for
// a group with no process. The advance button is gated server-side
// (process.advance / owner-pass); a non-permitted click just gets a 403 toast.
function groupProcessChip(g) {
  const p = g.process;
  if (!p || !p.phases || !p.phases.length) return '';
  const idx = typeof p.phase_index === 'number' ? p.phase_index : -1;
  const chipText = idx >= 0
    ? `◆ phase ${idx + 1}/${p.phase_count}: ${p.current_phase}`
    : `◆ ${p.current_phase}`;
  const mapLines = p.phases.map((ph, i) => {
    const mark = ph.current ? '▸ ' : '  ';
    const roles = (ph.roles && ph.roles.length) ? ph.roles.join(', ') : 'any';
    return `${mark}${i + 1}. ${ph.name} [${roles}]`;
  });
  const trLines = (p.transitions || []).map(t => `${t.from || '(start)'} → ${t.to}`);
  const titleParts = ['Advisory process — tracked, not enforced', '', ...mapLines];
  if (trLines.length) titleParts.push('', 'transitions:', ...trLines);
  const title = titleParts.join('\n');
  // Only offer the advance button when there IS a next phase — at the last
  // phase (or a drifted current phase) "advance to next" is a server 409, which
  // would just surface as a confusing red toast. Correcting to a named phase is
  // the CLI's `process advance --to <phase>` job.
  const next = idx >= 0 && idx + 1 < p.phase_count ? p.phases[idx + 1].name : '';
  const advanceBtn = next
    ? `<button class="group-process-advance" data-act="advance-phase" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="${esc(`Advance to the next phase (${next})`)}">▸ advance</button>`
    : '';
  return `<span class="group-process-chip" title="${esc(title)}">${esc(chipText)}</span>` + advanceBtn;
}

// groupWavesChip renders a group's staged-spawn status (JOH-244) as a compact
// "🌊 wave 1/3 pending" chip while later waves are still deferred. Returns ''
// once the choreography completes (or for a single-wave deploy — no chip).
function groupWavesChip(g) {
  const wv = g.waves;
  if (!wv || !wv.pending_waves) return '';
  const titleParts = [
    `Staged spawn — ${wv.pending_agents} agent(s) in ${wv.pending_waves} more wave(s) will spawn as each wave settles`,
  ];
  if (wv.deadline_at) titleParts.push(`next wave by ${wv.deadline_at} at the latest`);
  return `<span class="group-waves-chip" title="${esc(titleParts.join('\n'))}">🌊 wave ${wv.current_wave}/${wv.total_waves} pending</span>`;
}

// groupPendingChip renders Codex spawns that are intended for this group but
// are not members yet because their conv-id has not materialised.
function groupPendingChip(g) {
  const n = (g.pending || []).length;
  if (!n) return '';
  const label = `${n} pending spawn${n === 1 ? '' : 's'}`;
  return `<span class="group-pending-chip" title="${esc(label + ' waiting for startup')}">⏳ ${esc(label)}</span>`;
}

// isDeployedForce reports whether a group looks like a deployed task force
// (JOH-247): it carries deployment provenance (a source template / mission) or
// live process / wave machinery. Degrades gracefully — any one signal is
// enough, so a plain-instantiated group (source template, no mission) still
// gets the force block, and a hand-built group with none of these does not.
function isDeployedForce(g) {
  return !!(g.source_template || g.mission || (g.process && g.process.phases && g.process.phases.length) || g.waves);
}

// isForceFolded reports whether the human has folded away this force's info
// card via the 🎯 toggle in the group action row. Per-browser view state in
// dashPrefs, keyed by group name; ABSENT = open, which is the default so a
// freshly deployed force shows its card. Only a stored '1' means folded (the
// same "default-open, flag = the non-default" idiom the quick-pin uses), so a
// never-toggled force always renders its card.
function isForceFolded(name) {
  return dashPrefs.getItem('tclaude.dash.forcefold.' + name) === '1';
}

// forceMemberLiveness classifies a member for the roles rollup + stalling
// glance: an offline / exited member is 'dead'; an online member is 'idle' only
// when its recorded status is literally idle, and 'working' for anything else in
// flight (working / main_agent_idle / awaiting_* / error / an as-yet-unreported
// online agent). The idle-vs-working split is deliberately conservative so the
// stalling hint (all-live-idle) never fires while anything is mid-turn.
function forceMemberLiveness(m) {
  if (!m.online) return 'dead';
  return ((m.state && m.state.status) || '') === 'idle' ? 'idle' : 'working';
}

// forceMemberPill renders one member in the roles rollup: a status glyph, its
// name, and — when the snapshot already carries it — its context pressure
// (e.g. 62%). No new data source: context_pct rides the same member.state the
// members table reads.
function forceMemberPill(m) {
  const live = forceMemberLiveness(m);
  const glyph = live === 'working' ? '●' : live === 'idle' ? '○' : '✕';
  const pct = Math.round(Number((m.state && m.state.context_pct) || 0));
  const name = m.title || (m.conv_id ? m.conv_id.slice(0, 8) : '(unnamed)');
  const ctx = pct > 0 ? ` <span class="force-member-ctx">${pct}%</span>` : '';
  const tip = `${name} — ${live}${pct > 0 ? ` · context ${pct}%` : ''}`;
  return `<span class="force-member force-member-${live}" title="${esc(tip)}">${glyph} ${esc(name)}${ctx}</span>`;
}

// forceRolesRollup groups a force's members by role (first-seen order) and
// renders a per-role line of member pills — the "who is working / idle / dead"
// glance. Reuses the snapshot's existing per-member status; no new collection.
function forceRolesRollup(members) {
  const order = [];
  const byRole = new Map();
  members.forEach(m => {
    const role = m.role || '(no role)';
    if (!byRole.has(role)) { byRole.set(role, []); order.push(role); }
    byRole.get(role).push(m);
  });
  return order.map(role => {
    const pills = byRole.get(role).map(forceMemberPill).join('');
    return `<div class="force-role-row"><span class="force-role-name">${esc(role)}</span><span class="force-role-members">${pills}</span></div>`;
  }).join('');
}

// forceStalling reports whether every LIVE member is idle — a lean, derived
// "nothing in flight" hint (presentation-only, no backend state). False when no
// member is live (a fully-offline force is dormant, not stalling).
function forceStalling(members) {
  const live = members.filter(m => m.online);
  return live.length > 0 && live.every(m => forceMemberLiveness(m) === 'idle');
}

// forcePhaseHistory renders a compact phase line + a transition-history affordance
// for the force block. The summary already carries the phase chip; here the
// transition log (already in the process payload) is one click/hover away via a
// small "history (N)" element — the lean "access to the transition history" the
// force view asks for. Returns '' for a force with no process.
function forcePhaseHistory(g) {
  const p = g.process;
  if (!p || !p.phases || !p.phases.length) return '';
  const idx = typeof p.phase_index === 'number' ? p.phase_index : -1;
  const chip = idx >= 0 ? `phase ${idx + 1}/${p.phase_count}: ${p.current_phase}` : p.current_phase;
  const trs = p.transitions || [];
  const histTip = trs.length
    ? trs.map(t => `${t.from || '(start)'} → ${t.to}${t.at ? '  ' + t.at : ''}`).join('\n')
    : 'no transitions yet';
  const hist = trs.length
    ? `<span class="force-phase-history" title="${esc(histTip)}">history (${trs.length})</span>`
    : '';
  return `<div class="force-phase"><span class="force-phase-label">◆ ${esc(chip)}</span>${hist}</div>`;
}

// renderForceBlock builds the deployed-task-force glance for a group's expanded
// body (JOH-247): mission (quest), phase + transition history, a per-role
// live-status rollup, a stalling hint, and the re-brief control. Advance lives
// in the summary chip and retire in the ⚙ cog (shutdown / delete) — not
// duplicated here. Returns '' for a group that is not a deployed force. Renders
// as ONE stable .group-force-block node so the 2s morph reconciles it in place
// (its children are positional — no keys to collide).
function renderForceBlock(g, members) {
  if (!isDeployedForce(g)) return '';
  // Folded away via the 🎯 toggle in the group action row — hide the card
  // entirely. The toggle (rendered by groupActionsHTML, always present for a
  // deployed force) is how it comes back. Returning '' drops the
  // .group-force-block node; the 2s morph re-adds it verbatim on unfold.
  if (isForceFolded(g.name)) return '';
  const parts = [];
  if (g.mission) {
    const from = g.source_template ? ` <span class="force-from">from ${esc(g.source_template)}</span>` : '';
    parts.push(`<div class="force-mission"><span class="force-mission-label-regular">🎯 Mission</span><span class="force-mission-label-wizard">🗺 Quest</span>: <span class="force-mission-text">${esc(g.mission)}</span>${from}</div>`);
  } else if (g.source_template) {
    parts.push(`<div class="force-mission force-mission-unset">Deployed from template <strong>${esc(g.source_template)}</strong> — no mission recorded</div>`);
  }
  parts.push(forcePhaseHistory(g));
  if (members.length) {
    const stalling = forceStalling(members)
      ? `<span class="force-stalling" title="Every live member is idle — nothing appears to be in flight. The force may be waiting on a nudge, a decision, or the next phase.">⚠ stalling</span>`
      : '';
    parts.push(`<div class="force-roles"><div class="force-roles-head"><span class="force-roles-label">Roles</span>${stalling}</div>${forceRolesRollup(members)}</div>`);
  }
  parts.push(`<div class="force-controls"><button class="force-rebrief-btn" data-act="rebrief-force" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Re-brief the force — re-deliver the source template's current work pattern to the live roster, with the mission interpolated. Useful when the roster drifted or the original briefing scrolled out of context.">↻ re-brief</button><button class="force-standdown-btn" data-act="stand-down-force" data-group="${esc(g.name)}" data-label="${esc(g.name)}" title="Stand down the force — the mirror of deploy. Retires every member and sweeps the deploy-seeded rhythms + pending waves, keeping the group as a dormant record (mission &amp; history preserved). Not a delete.">⏻ stand down</button></div>`);
  return `<div class="group-force-block">${parts.filter(Boolean).join('')}</div>`;
}

// renderGroups renders the Groups-tab list as a TREE (n-level groups-in-
// groups, JOH-392). A real group's `parent` names the group it nests under;
// a nested group is rendered INSIDE its parent's <details> body, so
// collapsing the parent hides the whole subtree — that is the board-declutter
// mechanism (no separate hide flag needed).
//
// Tree assembly is defensive by construction, so a bad `parent` never
// crashes the render (the daemon's FK + cycle checks are the first line, this
// is the second):
//   - A `parent` that is not among the groups currently being rendered
//     (filtered out by the text search, a virtual group, or — should not
//     happen — a dangling id) makes the child a ROOT, so it stays visible
//     instead of disappearing under a parent that isn't there.
//   - Virtual groups (Ungrouped/Retired/…) never nest and are never parents;
//     they always render at the top level in their fixed slots.
//   - A cycle is broken by a visited-set on the recursion path (a group is
//     rendered at most once), so even a corrupt loop terminates.
// Sibling order and the roots' order both follow the incoming `groups` order
// (already the human's persisted reorder from sortGroupsByPref).
function renderGroups(groups) {
  if (!groups || !groups.length) {
    // The button label the hint names swaps per theme too (the same
    // .group-create-label-* span pair as the filter-bar button), so the empty
    // state reads "⚔ Form a party" in 🧙 wizard mode — CSS reveals the active
    // variant, no JS theme read needed.
    return '<div class="empty">No groups yet. Create one with the <strong><span class="group-create-label-regular">+ new group</span><span class="group-create-label-wizard">⚔ Form a party</span></strong> button above.</div>';
  }
  // Only real (non-virtual) groups present in THIS render can be a parent.
  const present = new Set(groups.filter(g => !g.virtual).map(g => g.name));
  const childrenByParent = new Map();
  const roots = [];
  for (const g of groups) {
    const parent = !g.virtual && g.parent && present.has(g.parent) && g.parent !== g.name
      ? g.parent : '';
    if (parent) {
      if (!childrenByParent.has(parent)) childrenByParent.set(parent, []);
      childrenByParent.get(parent).push(g);
    } else {
      roots.push(g);
    }
  }
  const rendered = new Set(); // cycle guard: render each group at most once
  const renderNode = (g) => {
    if (g.virtual) return g.conversations ? renderVirtualConversationsGroup(g)
      : g.retired ? renderVirtualRetiredGroup(g)
      : g.replaced ? renderVirtualReplacedGroup(g)
      : g.pending ? renderVirtualPendingGroup(g)
      : renderVirtualGroup(g);
    if (rendered.has(g.name)) return ''; // already drawn — a cycle would loop here
    rendered.add(g.name);
    const kids = childrenByParent.get(g.name) || [];
    const childrenHTML = kids.length
      ? `<div class="group-subgroups">${kids.map(renderNode).join('')}</div>`
      : '';
    return renderRealGroup(g, childrenHTML);
  };
  let html = roots.map(renderNode).join('');
  // Orphan rescue: a group reachable only through a cycle (e.g. corrupt data
  // where A.parent=B and B.parent=A) is in nobody's root set and would never be
  // visited above. The server rejects cycles, so this is corruption-only — but
  // rather than let such groups silently vanish, draw any real group the tree
  // walk missed as a top-level node. renderNode's `rendered` guard keeps this
  // from double-drawing anything already shown.
  for (const g of groups) {
    if (!g.virtual && !rendered.has(g.name)) html += renderNode(g);
  }
  return html;
}

// renderRealGroup renders one non-virtual group's <details> block.
// childrenHTML is the already-rendered subgroup tree (empty string when the
// group has no children); it is inserted in the group body BELOW the header
// action buttons and ABOVE the direct member list, per the tree layout.
function renderRealGroup(g, childrenHTML) {
    const members = g.members || [];
    const pending = g.pending || [];
    // Offline visibility: per-group override falls back to the
    // tab-wide checkbox. Hidden members still count toward the
    // 👥 chip's online/total/cap counts so the header stays truthful.
    const visible = groupShowOffline(g.name) ? members : members.filter(m => m.online);
    const hiddenOffline = members.length - visible.length;
    // Restore expanded state across the 2s polling re-renders by
    // keying on group name. Persisted in localStorage so it
    // survives a full page reload too.
    const openPref = dashPrefs.getItem('tclaude.dash.group.' + g.name);
    const isOpen = openPref === '1' || (pending.length > 0 && openPref !== '0');
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
    // Every actionable chip carries tabindex="0" role="button" so it is
    // keyboard-reachable; spans (not <button>s) because the <summary>
    // fold/skin CSS is tuned to inline spans. Enter/Space activation is
    // delegated in row-actions.js (bindRowActions' keydown listener),
    // which routes through the same click dispatcher as the pointer path.
    // Focus survives the 2s poll because the Groups island promotes stable
    // renderer identities to Preact keys instead of swapping the list.
    return `
    <details data-group-key="${esc(g.name)}" data-dnd-target-group="${esc(g.name)}"${detailsClassAttr}${isOpen ? ' open' : ''}>
      <summary draggable="true" data-group-reorder="${esc(g.name)}" title="Drag this header to reorder the group">
        <strong class="group-name" data-group-name="${esc(g.name)}">${esc(g.name)}</strong>
        ${groupActivityChip(members)}
        ${groupProcessChip(g)}
        ${groupWavesChip(g)}
        ${groupPendingChip(g)}
        ${g.virtual ? '' : groupHeaderCogHTML(g, members)}
        <span class="group-descr${g.descr ? '' : ' unset'}" tabindex="0" role="button" data-act="set-group-descr" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-descr="${esc(g.descr || '')}" title="${g.descr ? 'Group description — click to edit' : 'No description — click to set one'}">📝<span class="qo-text"> ${g.descr ? esc(g.descr) : 'no description'}</span></span>
        <span class="group-default-cwd${g.default_cwd ? '' : ' unset'}" tabindex="0" role="button" data-act="set-group-dir" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-cwd="${esc(g.default_cwd || '')}" title="${g.default_cwd ? 'Default spawn directory: ' + esc(g.default_cwd) + ' — click the text to edit, the 📁 to browse' : 'No default spawn directory — click the text to type one, the 📁 to browse'}"><span class="gdc-pick" tabindex="0" role="button" data-act="pick-group-dir" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-cwd="${esc(g.default_cwd || '')}" title="Browse for a directory with a native picker">📁</span><span class="qo-text"> ${g.default_cwd ? esc(shortCwd(g.default_cwd)) : 'no default dir'}</span></span>
        <span class="${capChipClass}" tabindex="0" role="button" data-act="set-group-max-members" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-max="${g.max_members || 0}" title="${esc(capChipTitle)}">👥 ${capChipText}</span>
        <span class="group-default-model${g.default_profile ? '' : ' unset'}" tabindex="0" role="button" data-act="set-group-profile" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-profile="${esc(g.default_profile || '')}" title="${g.default_profile ? 'Default spawn profile for agents spawned into this group: ' + esc(g.default_profile) + ' — fills blank launch fields at spawn. Click to change.' : 'No default spawn profile — click to set one. (Spawns use their own fields until set.)'}">🧠<span class="qo-text">${g.default_profile ? ' ' + esc(g.default_profile) : ''}</span></span>
        ${g.virtual ? '' : `<span class="group-sandbox-profile${g.sandbox_profile ? '' : ' unset'}" tabindex="0" role="button" data-act="set-group-sandbox-profile" data-group="${esc(g.name)}" data-label="${esc(g.name)}" data-sandbox-profile="${esc(g.sandbox_profile || '')}" title="${g.sandbox_profile ? 'Sandbox profile for ' + esc(g.name) + ': ' + esc(g.sandbox_profile) + ' — composes after the global sandbox profile for newly launched agents. Click to change.' : 'No group sandbox profile — newly launched agents get the global one only. Click to set one.'}">🛡<span class="qo-text">${g.sandbox_profile ? ' ' + esc(g.sandbox_profile) : ''}</span></span>`}
        ${g.virtual ? '' : renderGroupLinkChips(g.name)}
      </summary>
      <div class="subtable">
        ${childrenHTML}
        ${renderGroupPendingBlock(g)}
        <div class="group-header-actions">${groupActionsHTML(g, members)}</div>
        ${renderForceBlock(g, members)}
        ${members.length === 0
          ? '<div class="muted">(no members yet)</div>'
          : visible.length === 0
          ? `<div class="muted">(${hiddenOffline} offline member${hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${hiddenOffline === 1 ? 'it' : 'them'})</div>`
          : `
        <table>
          ${sortHead('members', visibleMemberCols())}
          <tbody>
            ${applySort('members', visible, MEMBER_ACCESSORS).map(m => memberRowHTML(m, {group: g})).join('')}
          </tbody>
        </table>`}
        ${renderGroupLinksSection(g.name)}
      </div>
    </details>
  `;
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
  return `<span class="group-link-chips" tabindex="0" role="button" data-act="links-manage" title="Inter-group links — click to manage">🔗<span class="qo-text">${chips}</span></span>`;
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
        <span class="muted" style="font-size:11px">${themeWords('No links involving this group.', 'No arcane channels are woven to or from this party.')}</span>
        <button data-act="link-new" data-from="${esc(groupName)}" data-label="${esc(groupName)}" title="${esc(isWizardActive() ? 'Weave an outbound arcane channel from this party' : 'Add an outbound link from this group')}">${themeWords('+ link', '+ weave channel')}</button>
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
      <button data-act="link-new" data-from="${esc(groupName)}" data-label="${esc(groupName)}" title="${esc(isWizardActive() ? 'Weave an outbound arcane channel from this party' : 'Add an outbound link from this group')}">${themeWords('+ link', '+ weave channel')}</button>
    </div>
  `;
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
  el.setAttribute('aria-label', name ? `Dashboard default spawn profile: ${name}. Click to change.` : 'Set dashboard default spawn profile');
  el.textContent = '🧠' + (name ? ' ' + name : '');
  el.title = name
    ? `Dashboard default spawn profile: ${name} — pre-fills the spawn dialog when the chosen group has no default profile of its own. Click to change.`
    : 'No dashboard default spawn profile — click to set one. (Pre-fills the spawn dialog as a fallback after a group’s own default.)';
}

// Paint the global 🛡 chip from the snapshot without fetching profile payloads.
// While its one-shot picker is open, renameEditing suspends snapshot refresh;
// openProfilePicker also restores this same node before persistence so dock.js's
// identity cache remains valid.
function renderDashSandboxProfile() {
  const el = $('#dashboard-default-sandbox-profile');
  if (!el || !lastSnapshot) return;
  const name = lastSnapshot.sandbox_profile_default || '';
  el.classList.toggle('unset', !name);
  el.setAttribute('data-sandbox-profile', name);
  el.setAttribute('aria-label', name ? `Global sandbox profile: ${name}. Click to change.` : 'Set global sandbox profile');
  el.textContent = '🛡' + (name ? ' ' + name : '');
  el.title = name
    ? `Global sandbox profile: ${name} — newly launched agents inherit it before any group or explicit assignment. Click to change.`
    : 'No global sandbox profile — click to set one. Newly launched agents inherit it unless their group adds another assignment.';
}

export {
  renderGroups, renderDashDefaultProfile, renderDashSandboxProfile,
};
