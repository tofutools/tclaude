// tabs.js — the Groups / Jobs (exports + cron) / Sudo / Links tab renderers.
//
// Builds the listing tables for the Groups, Jobs, Sudo and Links
// tabs from snapshot data, each with its text-filter helper.
// Extracted from dashboard.js as part of the Stage 2 module split.

import { $, esc, shortAgentId, idTooltip, relTime, syncBotAnimations, syncWizardOrbit } from './helpers.js';
import { renderExportStepper, fmtBytes } from './export-progress.js';
import {
  sortHead, applySort, JOBS_COLS, JOBS_ACCESSORS,
  SUDO_COLS, SUDO_ACCESSORS, LINK_COLS, LINK_ACCESSORS,
} from './sort.js';
import { listPagerHTML } from './list-paging.js';
import {
  virtualUngroupedGroup, ungroupedVisible,
  virtualConversationsGroup, conversationsVisible,
  virtualRetiredGroup, retiredVisible,
  virtualPendingGroup,
  virtualReplacedGroup, replacedVisible,
} from './virtual-groups.js';
import { renderGroups } from './render.js';
import { sortGroupsByPref } from './group-reorder.js';
import { morphInto } from './morph.js';

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
  // morphInto reconciles the fresh HTML against the live DOM instead of
  // replacing it wholesale, so a text selection / hover / open <details> /
  // scroll position under #groups-list survives the 2s poll (that's the
  // copy-paste fix). The stable #groups-list container is never replaced, so
  // its delegated listeners (bindGroupQuickHover, bindDnd, …) stay bound.
  morphInto($('#groups-list'), renderGroups(filtered));
  // Re-phase the activity-bot animations to wall-clock. Morphing no longer
  // restarts the CSS animations on an UNCHANGED bot (its node is reused), so
  // this is now only load-bearing for bots that were freshly inserted this
  // tick (a new/changed group); it stays because it's idempotent and cheap.
  // (Removable once #global-activity — still an innerHTML swap — is morphed
  // too; see the PR.)
  syncBotAnimations();
  // Same for the wizard "Channeling" pill's orbiting mote — otherwise the
  // light teleports back to its start on every poll. See syncWizardOrbit.
  syncWizardOrbit();
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

// cronScheduleCell renders a cron job's schedule for the info column:
// the cron expression verbatim ("cron: */5 * * * *", with the English
// description as the hover title when the server could render one) for
// an expression job, else the familiar "every 5m" interval.
function cronScheduleCell(j) {
  if (j.cron_expr) {
    return `<span class="id" title="${esc(j.cron_desc || '')}">cron: ${esc(j.cron_expr)}</span>`;
  }
  return `<span class="id">every ${esc(formatInterval(j.interval_seconds))}</span>`;
}

// cronStatusPill colorises the last_run_status. Empty / "ok" /
// anything else map to neutral / green / red respectively.
function cronStatusPill(s) {
  if (!s) return '<span class="state-pill state-offline" title="never run">never run</span>';
  if (s === 'ok') return `<span class="state-pill state-working" title="${esc(s)}">${esc(s)}</span>`;
  return `<span class="state-pill state-awaiting" title="${esc(s)}">${esc(s)}</span>`;
}

// -- Jobs tab: the unified job table --------------------------------------
// ONE listing for every job kind — per-agent "📋 summary…" export jobs
// (agentd/export.go) and recurring cron schedules — discriminated by a kind
// column. Rows are {kind, export?, cron?} served by GET /api/jobs, fetched
// alongside the 2s poll while the tab is active (refresh.js) and stitched
// onto the snapshot as lastSnapshot.jobs + paging.jobs. Pagination + the
// text filter are SERVER-side (the filter searches the whole set); column
// sorting orders the served window only (sort.js JOBS_ACCESSORS), like the
// retired/conversations/replaced sub-tables.
//
// Export rows: in-flight ones show the compact phase stepper
// (export-progress.js); settled ones keep a ready/failed pill and STAY
// LISTED until dismissed — the dismiss deletes the job + its artifact
// server-side — so a finished export never vanishes before its file is
// fetched.

// cronJobRowCells renders the shared row shape for a cron job:
// [dot, kind, id, name, agent, status, when, info, actions].
function cronJobRowCells(j) {
  const enabledDot = j.enabled
    ? '<span class="online" title="enabled">●</span>'
    : '<span class="offline" title="disabled">○</span>';
  const enableBtn = j.enabled
    ? `<button class="warn" data-act="cron-disable" data-id="${j.id}" data-label="${esc(j.name)}" title="Pause this cron job">disable</button>`
    : `<button data-act="cron-enable" data-id="${j.id}" data-label="${esc(j.name)}" title="Re-enable this cron job">enable</button>`;
  const runBtn = `<button data-act="cron-run-now" data-id="${j.id}" data-label="${esc(j.name)}" title="Fire this job immediately (also stamps last_run_at)">run now</button>`;
  const editBtn = `<button data-act="cron-edit" data-id="${j.id}" data-label="${esc(j.name)}" title="Edit this cron job">edit</button>`;
  const delBtn = `<button class="danger" data-act="cron-delete" data-id="${j.id}" data-label="${esc(j.name)}" title="Delete this cron job">delete</button>`;
  // The old dedicated Body column folded into the name cell: the subject
  // stays the muted subline, the full body rides the hover title (it is
  // still one click away in the edit modal).
  const bodySummary = (j.body || '').replace(/\s+/g, ' ').trim();
  return `
    <td>${enabledDot}</td>
    <td><span class="tag">⏰ cron</span></td>
    <td class="id">${j.id}</td>
    <td title="${esc(bodySummary)}"><div class="rowname">${esc(j.name)}</div>${j.subject ? `<div class="muted">${esc(j.subject)}</div>` : ''}</td>
    <td>${cronTargetCell(j)}<div class="muted" title="${esc(idTooltip(j.owner_agent, j.owner_conv))}">by ${esc(j.owner_label || shortAgentId(j.owner_agent, j.owner_conv))}</div></td>
    <td>${cronStatusPill(j.last_run_status)}</td>
    <td><span class="last-hook">${esc(relTime(j.last_run_at) || '—')}</span></td>
    <td>${cronScheduleCell(j)}</td>
    <td><div class="row-actions">${runBtn}${editBtn}${enableBtn}${delBtn}</div></td>`;
}

// exportJobNameCell renders an export row's name: the optional dialog Title,
// falling back to the delivered artifact's filename (promoted from the muted
// subline it occupies when a title exists), and for an in-flight, title-less
// job the preset — "(summary)" reads better than a bare "(untitled)".
function exportJobNameCell(j) {
  const name = j.title || j.artifact_name;
  if (!name) return `<span class="muted">(${esc(j.preset || 'untitled')})</span>`;
  const sub = j.title && j.artifact_name ? `<div class="muted">${esc(j.artifact_name)}</div>` : '';
  return `<div class="rowname">${esc(name)}</div>${sub}`;
}

// exportJobRowCells renders the same row shape for an export job.
function exportJobRowCells(j) {
  const settled = j.status === 'ready' || j.status === 'failed';
  let dot;
  let progress;
  if (!settled) {
    dot = '<span class="online" title="in flight">◐</span>';
    progress = renderExportStepper(j.status);
  } else if (j.status === 'ready') {
    dot = '<span class="online" title="ready">●</span>';
    progress = '<span class="ej-status ready">✓ ready</span>';
  } else {
    dot = '<span class="offline" title="failed">○</span>';
    progress = '<span class="ej-status failed">✗ failed</span>'
      + (j.error ? `<div class="ej-error" title="${esc(j.error)}">${esc(j.error)}</div>` : '');
  }
  const dlBtn = j.ready
    ? `<button data-act="export-job-download" data-id="${j.id}" data-label="${esc(j.artifact_name || j.title || ('#' + j.id))}" title="Download this export">⤓ download</button>`
    : '';
  const dismissBtn = `<button class="danger" data-act="export-job-dismiss" data-id="${j.id}" data-label="${esc(j.title || j.conv_label || ('#' + j.id))}" title="Dismiss — removes this export job from the list and deletes its file (if one was delivered)">dismiss</button>`;
  return `
    <td>${dot}</td>
    <td><span class="tag">📋 export</span></td>
    <td class="id">${j.id}</td>
    <td>${exportJobNameCell(j)}</td>
    <td><span class="rowname" title="${esc(j.conv_id || '')}">${esc(j.conv_label || '(unknown)')}</span></td>
    <td>${progress}</td>
    <td><span class="last-hook">${esc(relTime(j.created_at) || '—')}</span></td>
    <td>${j.artifact_size ? esc(fmtBytes(j.artifact_size)) : '<span class="muted">—</span>'}</td>
    <td><div class="row-actions">${dlBtn}${dismissBtn}</div></td>`;
}

function renderJobs(rows, paging) {
  if (!rows || !rows.length) {
    // The cron-button hint swaps per theme (the same .cron-open-label-* span
    // pair as the filter-bar button), so the empty state reads "⏳ Bind a
    // recurring ritual" in 🧙 wizard mode — CSS reveals the active variant.
    return '<div class="empty">No jobs yet. Agent exports appear here when started (an agent row\'s ⚙ menu → <strong>📋 summary…</strong>); schedule a cron job with the <strong><span class="cron-open-label-regular">+ new cron job</span><span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span></strong> button above.</div>';
  }
  return `
    <table>
      ${sortHead('jobs', JOBS_COLS)}
      <tbody>
        ${applySort('jobs', rows, JOBS_ACCESSORS).map(r =>
          `<tr data-key="${esc(r.kind + '-' + ((r.cron || r.export || {}).id ?? ''))}">${r.kind === 'cron' ? cronJobRowCells(r.cron || {}) : exportJobRowCells(r.export || {})}</tr>`
        ).join('')}
      </tbody>
    </table>
    ${listPagerHTML('jobs', paging)}
  `;
}

// renderJobsTab paints the unified table, the filter count and the Jobs nav
// badge. The badge counts IN-FLIGHT exports only (snapshot export_jobs_active
// — server-counted, so it's live even while the /api/jobs fetch is gated off
// on another tab); settled jobs stay in the list but stop demanding attention.
function renderJobsTab() {
  if (!lastSnapshot) return;
  const rows = lastSnapshot.jobs || [];
  const paging = (lastSnapshot.paging || {}).jobs;
  morphInto($('#jobs-list'), renderJobs(rows, paging));
  const q = $('#filter-jobs').value;
  const total = paging ? (paging.total || 0) : rows.length;
  const totalAll = paging ? (paging.total_unfiltered || 0) : rows.length;
  $('#filter-jobs-count').textContent = q
    ? `${total} / ${totalAll}` : `${totalAll} job${totalAll === 1 ? '' : 's'}`;
  const active = lastSnapshot.export_jobs_active || 0;
  const badge = $('#jobs-badge');
  if (badge) { badge.textContent = String(active); badge.hidden = active === 0; }
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
          <tr data-key="sudo-${esc(String(r.id))}">
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
  morphInto($('#sudo-list'), renderSudo(filtered));
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
          <tr data-key="link-${esc(String(l.id))}">
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
  morphInto($('#links-list'), renderLinks(filtered));
  $('#filter-links-count').textContent = q
    ? `${filtered.length} / ${rows.length}`
    : `${rows.length} link${rows.length === 1 ? '' : 's'}`;
}

export {
  renderGroupsTab, renderJobsTab, renderSudoTab, renderLinksTab,
  formatInterval, fmtRemaining,
};
