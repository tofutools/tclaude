// dashboard-operations.js — request and transaction launchers shared by
// row actions, toolbar actions, and the command palette.
//
// This module owns operation snapshots and daemon requests. It deliberately
// stays separate from refresh.js so the snapshot poll/reconciler never becomes
// an imperative action controller.

import { fetchListFull } from './list-paging.js';
import { lastSnapshot, webTerminalDefault } from './dashboard.js';
import { refresh } from './refresh.js';
import { wizWord } from './slop.js';
import {
  shellToast as toast,
  shellConfirm as confirmModal,
} from './shell-state.js';
import {
  buildCleanupDescriptor, buildWindowSelectionDescriptor, openCleanupDialog,
  openDeleteRetiredPreviewDialog,
  openDeleteGroupDialog, openGroupRetirePreviewDialog, openUngroupedRetirePreviewDialog,
  openWindowSelectionDialog,
} from './transaction-dialog-controller.js';
import { openWorktreeCleanup as openWorktreeCleanupDialog } from './worktree-cleanup-controller.js';

// --- inline mutations: action buttons + shared Preact feedback services ---

// shutdownScope drives the group-level and whole-dashboard Shutdown
// buttons. It counts the running agents in scope from the last
// snapshot, pops a confirm modal that states the count and spells out
// that this is stop-only (no data deleted), POSTs /api/shutdown, then
// toasts the outcome summary. scope is "group" (groupName set) or
// "all" (groupName ignored).
export async function shutdownScope(scope, groupName) {
  const snap = lastSnapshot || {};
  let running = 0;
  let where = '';
  let metaLine = '';
  if (scope === 'group') {
    const g = (snap.groups || []).find(x => x.name === groupName);
    running = g ? (g.online || 0) : 0;
    where = `group "${groupName}"`;
    metaLine = groupName;
  } else {
    running = (snap.agents || []).filter(a => a.online).length;
    where = 'the whole dashboard';
    metaLine = 'every group + ungrouped agents';
  }
  if (running === 0) {
    toast(`shutdown: no running agents in ${where}`);
    return;
  }
  const n = running === 1 ? '1 running agent' : `${running} running agents`;
  const confirmed = await confirmModal({
    title: 'Shutdown?',
    body: `This stops ${n} in ${where}. Each agent is sent /exit, then `
      + `force-killed only if it has not exited within the grace period. `
      + `Stop only — no conversations, group memberships, enrollment or `
      + `permissions are deleted. Resume any session to bring that agent back.`,
    meta: metaLine,
    okLabel: `Shut down ${running === 1 ? '1 agent' : running + ' agents'}`,
  });
  if (!confirmed) return;
  const payload = scope === 'group' ? {scope: 'group', group: groupName} : {scope: 'all'};
  let r;
  try {
    r = await fetch('/api/shutdown', {
      method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(payload),
    });
  } catch (e) {
    toast(`shutdown failed: ${e && e.message || e}`, true);
    return;
  }
  if (!r.ok) {
    toast(`shutdown failed: ${await r.text()}`, true);
    return;
  }
  const out = await r.json().catch(() => null);
  if (!out) {
    toast('shutdown: done');
    refresh();
    return;
  }
  const parts = [`${out.exited_gracefully} exited gracefully`, `${out.force_killed} force-killed`];
  if (out.already_offline) parts.push(`${out.already_offline} already offline`);
  if (out.failed) parts.push(`${out.failed} failed`);
  toast(`shutdown (${out.targeted} targeted): ${parts.join(', ')}`, out.failed > 0);
  refresh();
}

// powerOnScope is the inverse of shutdownScope — it drives the
// group-level and whole-dashboard Power On buttons. It counts the
// OFFLINE agents in scope from the last snapshot, pops a confirm modal,
// POSTs /api/power-on, then toasts the outcome summary. scope is
// "group" (groupName set) or "all" (groupName ignored).
export async function powerOnScope(scope, groupName) {
  const snap = lastSnapshot || {};
  let offline = 0;
  let where = '';
  let metaLine = '';
  if (scope === 'group') {
    const g = (snap.groups || []).find(x => x.name === groupName);
    offline = g ? (g.members || []).filter(m => !m.online).length : 0;
    where = `group "${groupName}"`;
    metaLine = groupName;
  } else {
    offline = (snap.agents || []).filter(a => !a.online).length;
    where = 'the whole dashboard';
    metaLine = 'every group + ungrouped agents';
  }
  if (offline === 0) {
    toast(`power on: no offline agents in ${where}`);
    return;
  }
  const n = offline === 1 ? '1 offline agent' : `${offline} offline agents`;
  const confirmed = await confirmModal({
    title: 'Power on?',
    body: `This resumes ${n} in ${where}. Each offline agent is restarted `
      + `in a fresh tmux session, resumed onto its existing conversation. `
      + `Agents already running are left alone. Resume only — nothing new `
      + `is created.`,
    meta: metaLine,
    okLabel: `Power on ${offline === 1 ? '1 agent' : offline + ' agents'}`,
  });
  if (!confirmed) return;
  const payload = scope === 'group' ? {scope: 'group', group: groupName} : {scope: 'all'};
  let r;
  try {
    r = await fetch('/api/power-on', {
      method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(payload),
    });
  } catch (e) {
    toast(`power on failed: ${e && e.message || e}`, true);
    return;
  }
  if (!r.ok) {
    toast(`power on failed: ${await r.text()}`, true);
    return;
  }
  const out = await r.json().catch(() => null);
  if (!out) {
    toast('power on: done');
    refresh();
    return;
  }
  const parts = [`${out.resumed} resumed`];
  if (out.already_online) parts.push(`${out.already_online} already online`);
  if (out.failed) parts.push(`${out.failed} failed`);
  toast(`power on (${out.targeted} targeted): ${parts.join(', ')}`, out.failed > 0);
  refresh();
}

// RETIRE_STATUS_LABELS maps a bulk-retire status token to the word used
// in the confirm/toast copy. Only the two the palette offers today
// (idle / offline) — the endpoint accepts more, but those are the only
// statuses a "tidy up the group" gesture should sweep.
const RETIRE_STATUS_LABELS = { idle: 'idle', offline: 'offline' };

// groupMembersByStatus returns the DISTINCT members of the named group
// that match a bulk-retire status token, using the SAME (online,
// state.status) definitions the snapshot renders — so the preview lists
// exactly the rows the human sees on the dashboard. This mirrors the
// server's normalizeMemberStatus filter; the server still applies that
// filter authoritatively on the legacy ?status= path, but the preview
// modal sends an EXPLICIT conv-id list built from these members, so what
// the human ticks is precisely what the BE retires.
//
// Each entry is {agent_id, conv_id, title, status, role} — enough to render a
// preview row. The group endpoint stays strictly conv-keyed: the transaction
// owner submits candidate.conv_id, never the stable agent selector used by the
// separate ungrouped cleanup endpoint.
function groupMembersByStatus(group, status) {
  const snap = lastSnapshot || {};
  const g = (snap.groups || []).find(x => x.name === group);
  if (!g) return [];
  const seen = new Set();
  const out = [];
  for (const m of (g.members || [])) {
    if (!m.conv_id || seen.has(m.conv_id)) continue; // dedupe owner + member rows
    seen.add(m.conv_id);
    const matches = status === 'offline'
      ? !m.online
      : (m.online && m.state && m.state.status === status);
    if (matches) {
      out.push({
        agent_id: m.agent_id || '',
        conv_id: m.conv_id,
        title: m.title || '',
        status: m.online ? ((m.state && m.state.status) || 'online') : 'offline',
        role: m.role || '',
      });
    }
  }
  return out;
}

// openRetirePreview runs the command palette's "Retire idle/offline
// agents in <group>" command. Rather than firing a status-filtered bulk
// retire that the server RE-RESOLVES from live state, it pops a PREVIEW
// modal so the human commits an exact list:
//   1. lists precisely the matching members (groupMembersByStatus), all
//      ticked by default, so the human sees exactly who will be retired;
//   2. lets the human opt individual agents out (per-row checkbox, plus
//      select-all / select-none and a title/id filter);
//   3. on submit, POSTs the EXPLICIT conv-id list the human approved to
//      /api/groups/{name}/retire {convs:[…]} — so the BE retires that
//      exact set, never a cohort it re-derived between preview and submit
//      (an agent that flips status in the meantime is still retired iff it
//      was on the previewed list).
//
// Demotion semantics are unchanged from the old confirm: each retired
// match is demoted to a plain, reinstatable conversation (leaves its
// groups, grants revoked) and — when the shutdown box is ticked (default
// on) — its running pane is soft-exited. A default-ON "delete each
// agent's git worktree + branch" box (coupled to shutdown, since removal
// can only run after a pane exits) sends delete_worktree to the BE, which
// cleans up each retired member's worktree under the same per-agent safety
// rules as the single retire (main repo / shared worktrees kept). Untick
// it to keep the worktrees. Cancel / Esc / backdrop is a no-op.
//
// The candidate list is snapshotted from lastSnapshot at open time and then
// frozen at the transaction-controller seam. Background snapshots keep flowing,
// but submit posts these exact conv-ids, so the cohort cannot shift under the
// human.
export function openRetirePreview(group, status) {
  const word = RETIRE_STATUS_LABELS[status] || status;
  const candidates = groupMembersByStatus(group, status);
  if (candidates.length === 0) {
    toast(`retire: no ${word} agents in group "${group}"`);
    return null;
  }
  return openGroupRetirePreviewDialog(group, status, candidates);
}

// ungroupedRetireCandidates builds the retire cohort for the command
// palette's "Retire ungrouped agents…" command from the snapshot's
// ungrouped[] list — every active agent that is a member of NO group
// (online and offline alike). Each entry is {agent_id, conv_id, title,
// status} — the same shape openRetirePreview's rows carry — so the
// preview renders identically. The submit leads with the stable
// agent_id (the BE resolves it back to the conv-id), falling back to
// conv_id for a row with no actor id yet.
function ungroupedRetireCandidates() {
  const snap = lastSnapshot || {};
  const seen = new Set();
  const out = [];
  for (const a of (snap.ungrouped || [])) {
    if (!a.conv_id || seen.has(a.conv_id)) continue;
    seen.add(a.conv_id);
    out.push({
      agent_id: a.agent_id || '',
      conv_id: a.conv_id,
      title: a.title || '',
      status: a.online ? ((a.state && a.state.status) || 'online') : 'offline',
    });
  }
  return out;
}

// openRetireUngroupedPreview runs the command palette's "Retire ungrouped
// agents…" command — the cross-group cleanup twin of the per-group
// openRetirePreview. Ungrouped agents belong to no group, so there is no
// group retire route to POST to; instead it opens the same keyed transaction
// owner as the group preview and submits the human-approved list to the
// group-agnostic bulk cleanup endpoint (/api/cleanup/agents {mode:"retire"}):
//   1. lists every ungrouped agent (ungroupedRetireCandidates), all ticked
//      by default, so the human sees exactly who will be retired;
//   2. lets the human opt individual agents out (per-row checkbox, plus
//      select-all / select-none and a title/id filter);
//   3. on submit, POSTs the EXPLICIT agent-id list the human approved with
//      include_online set — so a busy ungrouped agent the human left ticked
//      is retired (and soft-exited) rather than silently skipped, and the
//      BE acts on that exact set, never a cohort it re-derived.
//
// Demotion semantics match the per-group retire: each retired agent
// becomes a plain, reinstatable conversation (leaves its groups — none,
// here — and its grants are revoked) and, when the shutdown box is ticked
// (default on), its running pane is soft-exited. A default-ON "delete each
// agent's git worktree + branch" box (coupled to shutdown, since removal
// can only run after a pane exits) sends delete_worktrees; the BE cleans
// up each retired agent's worktree under the same per-agent safety rules
// as the single retire (main repo / shared worktrees kept). Untick it to
// keep the worktrees. Cancel / Esc / backdrop is a no-op.
//
// Like openRetirePreview, the candidate list is snapshotted and frozen at open
// time, so background snapshots cannot shift the population between preview
// and submit.
export function openRetireUngroupedPreview() {
  const candidates = ungroupedRetireCandidates();
  if (candidates.length === 0) {
    toast('retire: no ungrouped agents to retire');
    return null;
  }
  return openUngroupedRetirePreviewDialog(candidates);
}

// openDeleteRetiredPreview is the human-driven sibling of the timed
// agent.retired_cleanup auto-sweep (JOH-269): a dashboard tool to
// PERMANENTLY DELETE retired agents in bulk. Reachable from the command
// palette and the Groups ⚙ menu, and — like openRetirePreview — it pops
// a PREVIEW modal so the human commits an EXACT list rather than a filter
// the server re-resolves:
//   1. loads every retired agent from the complete endpoint (global,
//      newest-first),
//      each ticked by default, so the headline action deletes the whole
//      retired population — the human opts individual rows OUT;
//   2. two live filters re-render the list as the human types — a
//      title/conv-id substring scan (matching the retire-preview search)
//      and an age floor ("retired ≥ N days ago"); select-all/none act on
//      the currently-filtered rows only;
//   3. on submit, POSTs the EXPLICIT list of conv-ids that are BOTH ticked
//      AND still visible (pass the filters) to /api/cleanup/agents
//      {mode:"delete"} — the existing delete tier that wipes the .jsonl +
//      every DB row via conv.DeleteAgentAllGenerations.
//
// THE load-bearing invariant (JOH-31, operator-explicit): only rows that
// are BOTH ticked AND visible are sent — a row hidden by a filter is never
// deleted even if it was ticked before the filter narrowed. This is a
// DELIBERATE divergence from openRetirePreview, which posts c.checked
// regardless of the filter; do not "align" the two.
//
// delete_worktrees (default OFF) also removes each purged agent's git
// worktree under the BE's per-agent safety rules (main repo / shared
// worktrees kept). Retired agents are offline, so there is no shutdown or
// include_online toggle — the delete tier acts on them directly.
//
// The complete candidate list is normalized and frozen when it crosses the
// transaction-controller seam, so background snapshots cannot shift the
// population between preview and submit. On success the Preact owner swaps the
// editable list for the per-conv outcome log the cleanup endpoint returns.
export async function openDeleteRetiredPreview() {
  // retired[] in the snapshot is only one page now — fetch the COMPLETE list
  // (the /api/retired no-param path) so this bulk-delete preview acts on every
  // retired agent, not just the visible window.
  let retired;
  try {
    retired = await fetchListFull('retired');
  } catch (e) {
    toast('delete retired: failed to load (' + (e.message || e) + ')');
    return;
  }
  if (retired.length === 0) {
    toast('delete retired: no retired agents');
    return null;
  }
  return openDeleteRetiredPreviewDialog(retired);
}

// The worktree janitor is Preact-owned. This compatibility launcher keeps
// row actions, the palette, and the TCL-487 transaction handoff on one
// controller seam whose promise covers the full selection + result lifetime.
export function openWorktreeCleanup(group = '') {
  return openWorktreeCleanupDialog(group);
}

// openWindowModal is now only the snapshot launcher. It freezes the exact
// running roster and terminal preference before handing exclusive visual/state
// ownership to the keyed Preact transaction root.
export function openWindowModal(scope, groupName) {
  const descriptor = buildWindowSelectionDescriptor(
    lastSnapshot, scope, groupName, webTerminalDefault(),
  );
  if (descriptor.candidates.length === 0) {
    const where = scope === 'group' ? `group "${groupName}"` : 'the dashboard';
    const wizardWhere = scope === 'group' ? `party "${groupName}"` : 'the tower';
    toast(wizWord(
      `agent windows: no running agents in ${where}`,
      `scrying portals: no channeling familiars in ${wizardWhere}`,
    ));
    return null;
  }
  return openWindowSelectionDialog(descriptor);
}
// Snapshot launch adapter shared by the group menu and drag-to-banish path.
// The controller freezes the complete membership plan before the keyed Preact
// transaction root takes visual and state ownership.
export function openDeleteGroupModal(group) {
  return openDeleteGroupDialog(lastSnapshot, group);
}

// ---- 🧹 Cleanup dialog --------------------------------------------
//
// The launcher captures the current snapshot before any complete-list request,
// then hands one normalized descriptor to the keyed Preact transaction owner.
// Later polling and list pagination cannot retarget an open cleanup operation.
export async function openCleanupModal(options = {}) {
  const snapshot = lastSnapshot;
  let completeLists = {};
  if (options.mode === 'agents') {
    try {
      const [retired, conversations] = await Promise.all([
        fetchListFull('retired'),
        fetchListFull('conversations'),
      ]);
      completeLists = { retired, conversations };
    } catch (cause) {
      toast(`cleanup: failed to load candidates (${cause?.message || cause})`);
      return null;
    }
  }
  return openCleanupDialog(buildCleanupDescriptor(snapshot, options, completeLists));
}

// resumeAgentReq POSTs the resume endpoint, toasts the per-conv
// outcome, and refreshes on success. Driven by the offline status-dot
// click. Returns true on success.
//
// When the agent's recorded launch directory was deleted, the daemon
// answers {action: "error:missing_cwd", detail: <path>} instead of
// spawning a child that would wedge at startup. We pop a confirm and, on
// OK, retry with ?recreate=1 so the daemon recreates the dir empty first —
// the recreate opt-in is never automatic. The internal `recreate` flag is
// set only on that second call.
export async function resumeAgentReq(conv, label, recreate) {
  let r;
  const q = recreate ? '?recreate=1' : '';
  try {
    r = await fetch(`/api/agents/${encodeURIComponent(conv)}/resume${q}`, {
      method: 'POST', credentials: 'same-origin',
    });
  } catch (e) {
    toast(`wake failed: ${e && e.message || e}`, true);
    return false;
  }
  if (!r.ok) {
    toast(`wake failed: ${await r.text()}`, true);
    return false;
  }
  // Surface the daemon's per-conv result so an "already-online" no-op
  // shows up distinctly from a real wake. The body is JSON shaped
  // like {action: "resumed" | "skipped:already_online" | "error:missing_cwd" | ...}.
  let out = {};
  try { out = await r.json(); } catch (_) { /* non-JSON body: treat as bare ok */ }
  if (out.action === 'error:missing_cwd') {
    const dir = out.detail || 'the launch directory';
    const confirmed = await confirmModal({
      title: 'Launch directory missing',
      body: `${label}'s launch directory no longer exists, so it can't start. `
        + `Recreate it empty so the agent can wake up?`,
      meta: dir,
      okLabel: 'Recreate & wake',
    });
    if (!confirmed) {
      toast(`wake ${label}: cancelled — launch dir missing`);
      return false;
    }
    return resumeAgentReq(conv, label, true);
  }
  const action = String(out.action || '');
  if (action === 'error' || action.startsWith('error:')) {
    const detail = out.detail ? ` — ${out.detail}` : '';
    toast(`wake ${label}: ${action}${detail}`, true);
    return false;
  }
  toast(`wake ${label}: ${action || 'ok'}`);
  refresh();
  return true;
}
