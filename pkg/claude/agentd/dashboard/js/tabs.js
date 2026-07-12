// tabs.js — the legacy Groups / Links tab renderers.
//
// Builds the listing tables for the Groups and Links
// tabs from snapshot data, each with its text-filter helper.
// Extracted from dashboard.js as part of the Stage 2 module split.

import { $, esc, relTime, syncBotAnimations, syncWizardOrbit } from './helpers.js';
import {
  sortHead, applySort,
  LINK_COLS, LINK_ACCESSORS,
} from './sort.js';
import {
  virtualUngroupedGroup, ungroupedVisible,
  virtualConversationsGroup, conversationsVisible,
  virtualRetiredGroup, retiredVisible,
  virtualPendingGroup,
  virtualReplacedGroup, replacedVisible,
  scribeVisible,
} from './virtual-groups.js';
import { renderGroups } from './render.js';
import { sortGroupsByPref } from './group-reorder.js';
import { morphInto } from './morph.js';
import { scribeGroupVisible } from './scribe-groups.js';

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
    const matchedPending = (g.pending || []).filter(p => pendingRowMatches(p, needle));
    if (nameHit || descrHit) {
      // Group name / descr matched: keep all members so the user can
      // see the whole group context.
      out.push(g);
    } else if (matchedMembers.length > 0 || matchedPending.length > 0) {
      // Members / pending spawns matched: show only the matching subset.
      out.push({ ...g, members: matchedMembers, pending: matchedPending });
    }
  }
  return out;
}

function pendingRowMatches(p, needle) {
  return ((p.label || '').toLowerCase().includes(needle)) ||
         ((p.name || '').toLowerCase().includes(needle)) ||
         ((p.role || '').toLowerCase().includes(needle)) ||
         ((p.descr || '').toLowerCase().includes(needle)) ||
         ((p.group || '').toLowerCase().includes(needle)) ||
         ((p.cwd || '').toLowerCase().includes(needle)) ||
         ((p.harness || '').toLowerCase().includes(needle));
}

function distributePendingToGroups(groups, pending) {
  const byGroup = new Map();
  const fallback = [];
  for (const p of pending || []) {
    const group = (p.group || '').trim();
    if (!group) {
      fallback.push(p);
      continue;
    }
    const rows = byGroup.get(group) || [];
    rows.push(p);
    byGroup.set(group, rows);
  }

  const withPending = groups.map(g => {
    const rows = byGroup.get(g.name);
    if (!rows || rows.length === 0) return g;
    byGroup.delete(g.name);
    return { ...g, pending: rows };
  });
  for (const rows of byGroup.values()) fallback.push(...rows);
  return { groups: withPending, fallback };
}

function renderGroupsTab() {
  if (!lastSnapshot) return;
  const q = $('#filter-groups').value;
  // A live daemon-created scribe is active work and must remain visible. The
  // view preference only controls dormant/offline scribe groups, which stay
  // hidden by default because they are machinery rather than managed teams.
  const showOfflineScribes = scribeVisible();
  const realGroups = (lastSnapshot.groups || [])
    .filter(g => scribeGroupVisible(g, showOfflineScribes));
  const pending = lastSnapshot.pending || [];
  const distributed = distributePendingToGroups(realGroups, pending);
  // Append the virtual "Ungrouped" group LAST so it always sorts to
  // the bottom of the listing. filterGroups preserves order, so the
  // text filter narrows it like any other group without moving it.
  // Gated solely on the "show ungrouped" checkbox — once ticked, the
  // group stays visible even when empty (a no-text filter never
  // hides it; a text filter narrows it like any group).
  //
  // Real groups render in the human's persisted drag-reorder order
  // (group-reorder.js); pending spawns ride inside their intended real group
  // while they wait for a conv-id. The virtual Pending group is now only a
  // fallback for rows whose target group is gone or hidden from this view.
  const list = sortGroupsByPref(distributed.groups.slice());
  if (distributed.fallback.length) {
    list.unshift(virtualPendingGroup(distributed.fallback));
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
  // Phase activity-bot animations to wall-clock. Under morph the bot nodes
  // PERSIST (their stamp preserved by morphAttributes), so these helpers re-stamp
  // only on a genuine (re)start — a newly-inserted agent, or a status change that
  // swaps the animation to a different period — and leave a stable, still-running
  // bot alone (re-stamping it would shift its phase and jump it every tick). See
  // helpers.js. NOT the old removable band-aid: under morph they are REQUIRED, in
  // reworked form, to keep the animations continuous and in lock-step.
  syncBotAnimations();
  // Same wall-clock phasing for the wizard "Channeling" pill's orbiting mote.
  // See syncWizardOrbit.
  syncWizardOrbit();
  // The count reflects real groups only — the virtual group is a
  // derived bucket, not a group the human created.
  const total = realGroups.length;
  const shownReal = filtered.filter(g => !g.virtual).length;
  $('#filter-groups-count').textContent = q
    ? `${shownReal} / ${total}` : `${total} group${total === 1 ? '' : 's'}`;
}

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
  renderGroupsTab, renderLinksTab, fmtRemaining,
};
