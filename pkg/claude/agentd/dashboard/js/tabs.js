// tabs.js — the Groups / Cron / Sudo / Links tab renderers.
//
// Builds the listing tables for the Groups, Cron, Sudo and Links
// tabs from snapshot data, each with its text-filter helper.
// Extracted from dashboard.js as part of the Stage 2 module split.

import { $, esc, shortAgentId, idTooltip, relTime, syncBotAnimations } from './helpers.js';
import {
  sortHead, applySort, CRON_COLS, CRON_ACCESSORS,
  SUDO_COLS, SUDO_ACCESSORS, LINK_COLS, LINK_ACCESSORS,
} from './sort.js';
import {
  virtualUngroupedGroup, ungroupedVisible,
  virtualConversationsGroup, conversationsVisible,
  virtualRetiredGroup, retiredVisible,
  virtualPendingGroup,
  virtualReplacedGroup, replacedVisible,
} from './virtual-groups.js';
import { renderGroups } from './render.js';
import { sortGroupsByPref } from './group-reorder.js';

// lastSnapshot still lives in dashboard.js — the snapshot-refresh
// cluster is not extracted yet. Importing it back forms a deliberate,
// benign cycle (dashboard.js <-> tabs.js): it is safe because tabs.js
// runs no top-level code that reads it — the render*Tab functions
// touch it only when called, long after both modules finish
// evaluating (it is a read-only live binding here). This import
// re-points to the proper module once the snapshot cluster is
// extracted in a later PR.
import { lastSnapshot } from './dashboard.js';

function filterGroups(groups, q) {
  if (!q) return groups;
  const needle = q.toLowerCase();
  const out = [];
  for (const g of groups) {
    const nameHit = (g.name || '').toLowerCase().includes(needle);
    const descrHit = (g.descr || '').toLowerCase().includes(needle);
    const matchedMembers = (g.members || []).filter(m => {
      const state = m.state || {};
      return ((m.title || '').toLowerCase().includes(needle)) ||
             ((m.agent_id || '').toLowerCase().includes(needle)) ||
             ((m.conv_id || '').toLowerCase().includes(needle)) ||
             ((m.role || '').toLowerCase().includes(needle)) ||
             ((m.descr || '').toLowerCase().includes(needle)) ||
             ((m.branch || '').toLowerCase().includes(needle)) ||
             ((m.startup_branch || '').toLowerCase().includes(needle)) ||
             ((state.cwd || '').toLowerCase().includes(needle)) ||
             ((m.startup_dir || '').toLowerCase().includes(needle)) ||
             ((m.current_dir || '').toLowerCase().includes(needle)) ||
             // Replaced-generation rows (virtual "Replaced" group) carry the
             // owning actor's context + replacement reason — match those too so
             // an owner name or a `reincarnate`/`/clear` reason finds the row.
             ((m.actor_title || '').toLowerCase().includes(needle)) ||
             ((m.actor_conv_id || '').toLowerCase().includes(needle)) ||
             ((m.reason || '').toLowerCase().includes(needle));
    });
    if (nameHit || descrHit) {
      // Group name / descr matched: keep all members so the user can
      // see the whole group context.
      out.push(g);
    } else if (matchedMembers.length > 0) {
      // Members matched: show only the matching subset.
      out.push({ ...g, members: matchedMembers });
    }
  }
  return out;
}

function renderGroupsTab() {
  if (!lastSnapshot) return;
  const q = $('#filter-groups').value;
  const realGroups = lastSnapshot.groups || [];
  // Append the virtual "Ungrouped" group LAST so it always sorts to
  // the bottom of the listing. filterGroups preserves order, so the
  // text filter narrows it like any other group without moving it.
  // Gated solely on the "show ungrouped" checkbox — once ticked, the
  // group stays visible even when empty (a no-text filter never
  // hides it; a text filter narrows it like any group).
  //
  // Real groups render in the human's persisted drag-reorder order
  // (group-reorder.js); the virtual groups below keep their fixed slots
  // (Pending prepended, the rest appended). sortGroupsByPref returns the
  // groups unchanged when no custom order is saved — i.e. backend order
  // (alphabetical).
  const list = sortGroupsByPref(realGroups.slice());
  // The virtual "Pending" group is PREPENDED so a just-spawned agent
  // still stuck behind a startup gate (JOH-205) is the first thing the
  // operator sees — it's an actionable alert, not a routine bucket. Only
  // built when there are pending spawns: no opt-out checkbox and no
  // persistent empty box (unlike the other virtual groups below).
  const pending = lastSnapshot.pending || [];
  if (pending.length) {
    list.unshift(virtualPendingGroup(pending));
  }
  if (ungroupedVisible()) {
    list.push(virtualUngroupedGroup(lastSnapshot.ungrouped || []));
  }
  // The virtual "Retired" group sits above Conversations: a retired
  // agent is one step further along (it WAS an agent), so it lands
  // somewhere visible on the tab instead of vanishing the moment it
  // leaves its last group. On by default for the same reason.
  if (retiredVisible()) {
    list.push(virtualRetiredGroup(lastSnapshot.retired || [], lastSnapshot.paging?.retired));
  }
  // The virtual "Conversations" group sorts below even Ungrouped —
  // it's the rawest bucket (not even agents yet). Opt-in via its
  // checkbox.
  if (conversationsVisible()) {
    list.push(virtualConversationsGroup(lastSnapshot.conversations || [], lastSnapshot.paging?.conversations));
  }
  // The virtual "Replaced generations" group sits at the very bottom — it's
  // the most archival bucket (superseded past generations of agents, left
  // behind by reincarnate / /clear). Opt-in via its checkbox (default off),
  // like Conversations: it grows over an agent's life and is read-mostly.
  if (replacedVisible()) {
    list.push(virtualReplacedGroup(lastSnapshot.replaced || [], lastSnapshot.paging?.replaced));
  }
  const filtered = filterGroups(list, q);
  $('#groups-list').innerHTML = renderGroups(filtered);
  // Re-phase the activity-bot animations to wall-clock so this wholesale
  // innerHTML swap (every 2s poll, plus filter/sort/drag) doesn't restart
  // them with a visible jump. See helpers.syncBotAnimations.
  syncBotAnimations();
  // The count reflects real groups only — the virtual group is a
  // derived bucket, not a group the human created.
  const total = realGroups.length;
  const shownReal = filtered.filter(g => !g.virtual).length;
  $('#filter-groups-count').textContent = q
    ? `${shownReal} / ${total}` : `${total} group${total === 1 ? '' : 's'}`;
}

// formatInterval renders an integer second count as a coarse human
// string ("30s", "5m", "2h", "1d"). Mirrors the cron CLI output so
// dashboard + terminal read the same.
function formatInterval(sec) {
  if (!sec) return '';
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h';
  return Math.floor(sec / 86400) + 'd';
}

// cronTargetCell describes where a cron job fires. Two shapes:
//  - group:<name>  → group-target job; the scheduler fans the body
//                    out to every current member of the group.
//  - <conv-label>  → conv target; one recipient (the conv-routing
//                    group_id, if any, is not shown here).
// The discriminator is target_kind, NOT group_id>0 — a conv-target
// job routed through a shared group also carries a non-zero group_id.
function cronTargetCell(j) {
  if (j.target_kind === 'group') {
    return `<span class="tag">group:${esc(j.group_name || ('#' + j.group_id))}</span>`;
  }
  if (j.target_conv) {
    // Lead with the stable agent_id (the cutover); keep the conv title as a
    // muted second line so the human-readable name isn't lost (the cron tab
    // has no separate name column like the roster does). The full "agent_id /
    // conv-id" pair stays the hover title for inspectability, matching the
    // other roster cells.
    return `<span class="rowname" title="${esc(idTooltip(j.target_agent, j.target_conv))}">${esc(shortAgentId(j.target_agent, j.target_conv))}</span>`
      + (j.target_label ? `<div class="muted">${esc(j.target_label)}</div>` : '');
  }
  return '<span class="muted">(no target)</span>';
}

// cronStatusPill colorises the last_run_status. Empty / "ok" /
// anything else map to neutral / green / red respectively.
function cronStatusPill(s) {
  if (!s) return '<span class="state-pill state-offline" title="never run">never run</span>';
  if (s === 'ok') return `<span class="state-pill state-working" title="${esc(s)}">${esc(s)}</span>`;
  return `<span class="state-pill state-awaiting" title="${esc(s)}">${esc(s)}</span>`;
}

function renderCron(jobs) {
  if (!jobs || !jobs.length) {
    return '<div class="empty">No cron jobs yet. Create one with the <strong>+ new cron job</strong> button above.</div>';
  }
  return `
    <table>
      ${sortHead('cron', CRON_COLS)}
      <tbody>
        ${applySort('cron', jobs, CRON_ACCESSORS).map(j => {
          const enabledDot = j.enabled
            ? '<span class="online" title="enabled">●</span>'
            : '<span class="offline" title="disabled">○</span>';
          const enableBtn = j.enabled
            ? `<button class="warn" data-act="cron-disable" data-id="${j.id}" data-label="${esc(j.name)}" title="Pause this cron job">disable</button>`
            : `<button data-act="cron-enable" data-id="${j.id}" data-label="${esc(j.name)}" title="Re-enable this cron job">enable</button>`;
          const runBtn = `<button data-act="cron-run-now" data-id="${j.id}" data-label="${esc(j.name)}" title="Fire this job immediately (also stamps last_run_at)">run now</button>`;
          const editBtn = `<button data-act="cron-edit" data-id="${j.id}" data-label="${esc(j.name)}" title="Edit this cron job">edit</button>`;
          const delBtn = `<button class="danger" data-act="cron-delete" data-id="${j.id}" data-label="${esc(j.name)}" title="Delete this cron job">delete</button>`;
          const bodySummary = (j.body || '').replace(/\s+/g, ' ').trim();
          const bodyTrunc = bodySummary.length > 80 ? bodySummary.slice(0, 80) + '…' : bodySummary;
          return `
            <tr>
              <td>${enabledDot}</td>
              <td class="id">${j.id}</td>
              <td><div class="rowname">${esc(j.name)}</div>${j.subject ? `<div class="muted">${esc(j.subject)}</div>` : ''}</td>
              <td><span class="muted" title="${esc(idTooltip(j.owner_agent, j.owner_conv))}">${esc(shortAgentId(j.owner_agent, j.owner_conv))}</span>${j.owner_label ? `<div class="muted">${esc(j.owner_label)}</div>` : ''}</td>
              <td>${cronTargetCell(j)}</td>
              <td><span class="id">${esc(formatInterval(j.interval_seconds))}</span></td>
              <td><span class="last-hook">${esc(relTime(j.last_run_at) || '—')}</span></td>
              <td>${cronStatusPill(j.last_run_status)}</td>
              <td><span class="muted" title="${esc(j.body || '')}">${esc(bodyTrunc)}</span></td>
              <td><div class="row-actions">${runBtn}${editBtn}${enableBtn}${delBtn}</div></td>
            </tr>
          `;
        }).join('')}
      </tbody>
    </table>
  `;
}

function filterCron(jobs, q) {
  if (!q) return jobs;
  const needle = q.toLowerCase();
  return jobs.filter(j =>
    ((j.name || '').toLowerCase().includes(needle)) ||
    ((j.owner_label || '').toLowerCase().includes(needle)) ||
    ((j.owner_agent || '').toLowerCase().includes(needle)) ||
    // conv_id stays a supported selector (still shown in row tooltips), so keep
    // matching it after the agent_id cutover.
    ((j.owner_conv || '').toLowerCase().includes(needle)) ||
    ((j.target_label || '').toLowerCase().includes(needle)) ||
    ((j.target_agent || '').toLowerCase().includes(needle)) ||
    ((j.target_conv || '').toLowerCase().includes(needle)) ||
    ((j.group_name || '').toLowerCase().includes(needle)) ||
    ((j.subject || '').toLowerCase().includes(needle)) ||
    ((j.body || '').toLowerCase().includes(needle))
  );
}

function renderCronTab() {
  if (!lastSnapshot) return;
  const q = $('#filter-cron').value;
  const filtered = filterCron(lastSnapshot.cron || [], q);
  $('#cron-list').innerHTML = renderCron(filtered);
  const total = (lastSnapshot.cron || []).length;
  $('#filter-cron-count').textContent = q
    ? `${filtered.length} / ${total}` : `${total} job${total === 1 ? '' : 's'}`;
}

// -- Sudo tab ---------------------------------------------------------

function fmtRemaining(secs) {
  if (!secs || secs <= 0) return 'expired';
  if (secs < 60) return secs + 's';
  if (secs < 3600) {
    const m = Math.floor(secs / 60);
    const s = secs % 60;
    return s > 0 ? `${m}m${s}s` : `${m}m`;
  }
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  return m > 0 ? `${h}h${m}m` : `${h}h`;
}

function filterSudo(rows, q) {
  if (!q) return rows;
  const needle = q.toLowerCase();
  return rows.filter(r =>
    (r.conv_title || '').toLowerCase().includes(needle) ||
    (r.conv_id || '').toLowerCase().includes(needle) ||
    (r.agent_id || '').toLowerCase().includes(needle) ||
    (r.slug || '').toLowerCase().includes(needle) ||
    (r.reason || '').toLowerCase().includes(needle));
}

function renderSudo(rows) {
  if (!rows || !rows.length) {
    return '<div class="empty">No active sudo grants.</div>';
  }
  return `
    <table>
      ${sortHead('sudo', SUDO_COLS)}
      <tbody>
        ${applySort('sudo', rows, SUDO_ACCESSORS).map(r => `
          <tr>
            <td>
              <span class="rowname">${esc(r.conv_title || '(unknown)')}</span>
              <span class="id" title="${esc(idTooltip(r.agent_id, r.conv_id))}">${esc(shortAgentId(r.agent_id, r.conv_id))}</span>
            </td>
            <td><span class="tag slug">${esc(r.slug)}</span></td>
            <td><span class="last-hook">${esc(relTime(r.granted_at))}</span></td>
            <td><span class="last-hook">${esc(fmtRemaining(r.remaining_seconds))}</span></td>
            <td>${esc(r.reason || '')}</td>
            <td><span class="muted" title="${esc(r.granted_by || '')}">${esc(r.granted_by || '')}</span></td>
            <td><button class="danger" data-act="sudo-revoke" data-id="${r.id}" data-slug="${esc(r.slug)}" data-conv="${esc(r.conv_title || r.conv_id)}" title="Revoke this grant">revoke</button></td>
          </tr>`).join('')}
      </tbody>
    </table>
  `;
}

function renderSudoTab() {
  if (!lastSnapshot) return;
  const q = $('#filter-sudo').value;
  const rows = lastSnapshot.sudo || [];
  const filtered = filterSudo(rows, q);
  $('#sudo-list').innerHTML = renderSudo(filtered);
  $('#filter-sudo-count').textContent = q
    ? `${filtered.length} / ${rows.length}`
    : `${rows.length} active grant${rows.length === 1 ? '' : 's'}`;
}

// -- Links tab --------------------------------------------------------
// Inter-group communication links surface as a flat read-only table
// in v1. Use `tclaude agent groups link add/rm` to mutate. The list
// shows direction (FROM → TO) and mode so the human can reason about
// who can message whom.
function renderLinks(rows) {
  if (!rows || !rows.length) {
    return '<div class="empty">No inter-group links yet. Create one with the <strong>+ new link</strong> button above.</div>';
  }
  return `
    <table>
      ${sortHead('links', LINK_COLS)}
      <tbody>
        ${applySort('links', rows, LINK_ACCESSORS).map(l => `
          <tr>
            <td class="id">${l.id}</td>
            <td><span class="rowname">${esc(l.from || '(deleted)')}</span></td>
            <td class="muted">→</td>
            <td><span class="rowname">${esc(l.to || '(deleted)')}</span></td>
            <td><span class="id">${esc(l.mode)}</span></td>
            <td><span class="muted">${esc(relTime(l.created_at) || '')}</span></td>
            <td><div class="row-actions">
              <button data-act="link-edit" data-id="${l.id}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" data-mode="${esc(l.mode)}" title="Change this link's mode">edit</button>
              <button class="danger" data-act="link-delete" data-id="${l.id}" data-group="${esc(l.from)}" data-from="${esc(l.from)}" data-to="${esc(l.to)}" title="Remove this link">delete</button>
            </div></td>
          </tr>
        `).join('')}
      </tbody>
    </table>
  `;
}
function filterLinks(rows, q) {
  if (!q) return rows;
  const needle = q.toLowerCase();
  return rows.filter(l =>
    ((l.from || '').toLowerCase().includes(needle)) ||
    ((l.to || '').toLowerCase().includes(needle)) ||
    ((l.mode || '').toLowerCase().includes(needle))
  );
}
function renderLinksTab() {
  if (!lastSnapshot) return;
  const q = $('#filter-links').value;
  const rows = lastSnapshot.links || [];
  const filtered = filterLinks(rows, q);
  $('#links-list').innerHTML = renderLinks(filtered);
  $('#filter-links-count').textContent = q
    ? `${filtered.length} / ${rows.length}`
    : `${rows.length} link${rows.length === 1 ? '' : 's'}`;
}

export {
  renderGroupsTab, renderCronTab, renderSudoTab, renderLinksTab,
  formatInterval, fmtRemaining,
};
