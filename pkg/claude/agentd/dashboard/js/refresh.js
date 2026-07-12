// refresh.js — the auto-refresh loop, tab / copy / sort wiring, the
// shared confirm / window / cleanup / agent-action modals, and toast.
//
// Extracted from dashboard.js in the Stage 2 module split. refresh() is
// the 2-second snapshot poll that re-renders every tab.

import { $, $$, isModifiedClick, esc, shortId, relTime, captureFocus, restoreFocus } from './helpers.js';
import { cycleSort } from './sort.js';
import { hideableMemberCols, memberColHidden, setMemberColHidden, memberColDeviationCount } from './member-columns.js';
import { dashPrefs } from './prefs.js';
import { listParams, syncServedOffset, listPagerNav, setListPageSize, fetchListFull, resetListOffsets } from './list-paging.js';
import { conversationsVisible, replacedVisible } from './virtual-groups.js';
import { recordGroupInteraction } from './last-group.js';
import {
  renderPermissions, renderSlugs, showStatus,
  renderMessagesBadge, renderUsage, renderDashDefaultProfile, renderDashSandboxProfile,
  renderNotifyGlobal, renderGlobalActivity,
} from './render.js';
import { renderMailTab, onMailSearchChanged, renderAccessRequests } from './mail.js';
import {
  renderGroupsTab, renderSudoTab, renderLinksTab,
} from './tabs.js';
import { renderTemplatesTab } from './modal-templates.js';
import { renderPluginsTab, renderPluginsBadge } from './plugins.js';
import { applyProcessesTabVisibility } from './processes.js';
import { morphInto } from './morph.js';
import { renderDock } from './dock.js';
// renameEditing (row-actions.js) and dndDragActive (dnd.js) are owned by
// their feature modules; refreshSuspended() only reads them. lastSnapshot
// is dashboard.js's shared state — read directly, written via the
// setLastSnapshot setter (two writers: refresh() here, and the
// row-actions rename-rollback). All deliberate, benign cycles (see
// render.js): TDZ-safe — no top-level code reads a cyclic import.
import { renameEditing } from './row-actions.js';
import { closeTerminalsForWindowOp, openWebWindowPane } from './terminals-tab.js';
import { dndDragActive } from './dnd.js';
import { groupReorderActive } from './group-reorder.js';
import { dockDragActive } from './dock-dnd.js';
import { lastSnapshot, setLastSnapshot, webTerminalDefault } from './dashboard.js';
import { setVegasRegularMode, isWizardActive } from './slop.js';
import { setHScrollFollow } from './hscroll.js';
import { noteConnected, noteDisconnected } from './connection.js';
import { syncDashDefaultProfile } from './profiles.js';
import { dashboardState } from './snapshot-store.js';
import { featureState } from './feature-state-registry.js';

// refreshSuspended() is the single source of truth for whether the
// auto-refresh is allowed to re-render the DOM right now. refresh()
// consults it both BEFORE its /api/snapshot fetch and AGAIN after,
// so a refresh that started before a drag/modal opened can never
// resume mid-gesture and re-render underneath it.
//
// Modal state is derived from the DOM (.modal-overlay.show) rather
// than a hand-maintained boolean on purpose: a flag must be reset on
// every close path or it leaks and wedges auto-refresh forever — the
// exact failure mode behind the drag-retire-freezes-refresh bug. The
// DOM cannot leak: once an overlay's .show class is gone the modal
// simply stops suspending, with no reset to forget. It is also
// uniform — every modal, present and future, shares .modal-overlay,
// so all of them suspend auto-refresh while open without each having
// to remember to toggle a flag.
//
// ignoreModals bypasses ONLY the open-modal suspension (not the drag /
// rename / reorder / open-menu / slop-pull guards). A handful of
// template/circle mutations fire refresh from INSIDE a modal that stays
// open — installing a starter (the picker stays up to copy several),
// snapshot-a-group (it reopens the editor on the fresh circle) — so a
// plain, modal-suspended refresh would drop their tick and leave the
// circle list stale until the human closed and reopened it. Those callers
// pass force so the list behind the modal repaints immediately; the truly
// destructive gestures still suspend, because re-rendering under an active
// drag/rename/menu breaks it (that is the wedge class of bug this predicate
// was written to avoid — see TestDashboardHTML_RefreshGuardCannotWedge).
function refreshSuspended({ ignoreModals = false } = {}) {
  // An inline rename <input> is open — re-rendering would destroy it
  // mid-keystroke.
  if (renameEditing) return true;
  // A drag-and-drop gesture is in flight — re-rendering would detach
  // the dragged row, and a dragend dispatched on a now-detached node
  // never bubbles up to the document-level handler, so the drag's
  // own cleanup (this suspension included) would be lost forever.
  if (dndDragActive) return true;
  // A group-reorder drag is in flight — same reasoning as dndDragActive:
  // re-rendering the Groups tab would detach the dragged grip mid-drag and
  // lose the drag's own dragend cleanup (group-reorder.js).
  if (groupReorderActive) return true;
  // A palette-dock drag is in flight (a profile/role card headed for a group) —
  // same reasoning: re-rendering the dock (#dock-body morph) or the Groups tab
  // would detach the drag source or the drop target mid-gesture and lose the
  // drag's own dragend cleanup (dock-dnd.js).
  if (dockDragActive) return true;
  // Any modal overlay is open (unless a force-refresh opted out — see the
  // ignoreModals note above).
  if (!ignoreModals && document.querySelector('.modal-overlay.show')) return true;
  // A ⚙ options menu is open — re-rendering the Groups tab would
  // rebuild the row/group and collapse the menu out from under the
  // pointer. Closing the menu drops the .open class, lifting this.
  // .dock-card-menu.open is the palette dock's own per-card actions menu
  // (Edit / Clone); the dock morphs its cards on the poll, so an open one
  // must pause the reconcile the same way (dock.js closeDockMenus lifts it).
  if (document.querySelector('.action-menu.open, .dock-card-menu.open')) return true;
  // A slop-mode slot machine is mid-pull. manualPull() in slop-fx.js
  // spins a row's .slop-machine for ~900ms, then holds the settled
  // combo for ~1.8s, tagging the cell with a sentinel data-status of
  // 'pull-spinning' then 'pull-stopped' for the whole ~2.7s. A
  // re-render rebuilds the Groups tab and detaches the cell mid-spin —
  // the bug where pulling the handle gets cancelled by the next poll.
  // Defer the tick until the pull settles; slop-fx restores the cell's
  // real data-status at the end, which lifts this on its own. Like the
  // checks above it's DOM-derived, so there's no flag to leak: a cell
  // detached mid-pull keeps a stale sentinel but is no longer in the
  // live DOM, so it can't match here. Pulls are bounded (~2.7s each),
  // so this only ever briefly delays a refresh — it can't wedge it.
  // (Keep these sentinel values in sync with manualPull in slop-fx.js.)
  if (document.querySelector('.slop-machine[data-status="pull-spinning"], .slop-machine[data-status="pull-stopped"]')) {
    return true;
  }
  return false;
}
// sudoGrantBlocklist: slugs the sudo-grant modal refuses to offer.
// Read by modal-cron's openSudoGrantModal; re-seeded on each refresh.
export let sudoGrantBlocklist = ['permissions.grant', 'permissions.revoke'];
// sudoByConv: conv-id → list of active grants. Built from
// snapshot.sudo on every refresh so any renderer (Agents, Groups
// members) can consult it for the 🔓 badge without a server-side
// duplication of dashboardMember.active_sudo.
export let sudoByConv = {};
function bindFilter(tab) {
  const input = $(`#filter-${tab}`);
  const clear = $(`#filter-${tab}-clear`);
  const key = `tclaude.dash.filter.${tab}`;
  input.value = dashPrefs.getItem(key) || '';
  const rerender = () => {
    if (tab === 'groups') {
      renderGroupsTab();
      // The three paginated virtual lists (Retired/Conversations/Replaced)
      // filter SERVER-side — their full set isn't in memory — so any groups
      // filter-bar change (a query edit, or newly showing a list) needs a
      // round-trip to refetch the right q-matched window. Debounced so a fast
      // typist fires one fetch (mirrors the Messages tab's server search). The
      // page-1 reset on a QUERY change lives in onChange, not here, so toggling
      // a "show X" checkbox doesn't bounce the other lists' pagers.
      clearTimeout(groupsFilterTimer);
      groupsFilterTimer = setTimeout(refresh, 250);
    }
    else if (tab === 'templates') renderTemplatesTab();
    else if (tab === 'sudo') renderSudoTab();
    else if (tab === 'links') renderLinksTab();
    else if (tab === 'plugins') renderPluginsTab();
    // The Messages search is server-side (pagination must span the whole
    // folder, not a client-loaded prefix), so a filter change resets to
    // page 1 and triggers a debounced reload rather than a cache repaint.
    else if (tab === 'messages') onMailSearchChanged();
  };
  const onChange = () => {
    const v = input.value;
    if (v) dashPrefs.setItem(key, v); else dashPrefs.removeItem(key);
    // A groups QUERY change resets the three paginated lists to page 1 — a
    // page-3 view of the old query is meaningless once the query (and its
    // server-side result set) changes. rerender() then triggers the debounced
    // refetch that sends the new q.
    if (tab === 'groups') resetListOffsets();
    rerender();
  };
  input.addEventListener('input', onChange);
  clear.addEventListener('click', () => { input.value = ''; onChange(); input.focus(); });
  // Optional per-tab "show offline" checkbox (the 'groups' tab only).
  // Restore its persisted state — defaults to checked (show all)
  // when the user has never touched it.
  const offline = $(`#filter-${tab}-offline`);
  if (offline) {
    const okey = `tclaude.dash.offline.${tab}`;
    const saved = dashPrefs.getItem(okey);
    offline.checked = saved === null ? true : saved === '1';
    offline.addEventListener('change', () => {
      dashPrefs.setItem(okey, offline.checked ? '1' : '0');
      rerender();
    });
  }
  // Optional "show ungrouped" checkbox (groups tab only) — toggles
  // the virtual Ungrouped group. Persisted like the offline toggle;
  // defaults to checked when the user has never touched it.
  const ungrouped = $(`#filter-${tab}-ungrouped`);
  if (ungrouped) {
    const ukey = `tclaude.dash.ungrouped.${tab}`;
    const saved = dashPrefs.getItem(ukey);
    ungrouped.checked = saved === null ? true : saved === '1';
    ungrouped.addEventListener('change', () => {
      dashPrefs.setItem(ukey, ungrouped.checked ? '1' : '0');
      rerender();
    });
  }
  // Optional "show conversations" checkbox (groups tab only) —
  // toggles the virtual Conversations group. Defaults OFF (there can
  // be many conversations) when the user has never touched it.
  const conversations = $(`#filter-${tab}-conversations`);
  if (conversations) {
    const ckey = `tclaude.dash.conversations.${tab}`;
    const saved = dashPrefs.getItem(ckey);
    conversations.checked = saved === '1';
    conversations.addEventListener('change', () => {
      dashPrefs.setItem(ckey, conversations.checked ? '1' : '0');
      rerender();
    });
  }
  // Optional "show replaced generations" checkbox (groups tab only) —
  // toggles the virtual Replaced-generations group (superseded past
  // generations of agents). Defaults OFF (it's an archival, read-mostly
  // list that grows over time) when the user has never touched it.
  const replaced = $(`#filter-${tab}-replaced`);
  if (replaced) {
    const rgkey = `tclaude.dash.replaced.${tab}`;
    const saved = dashPrefs.getItem(rgkey);
    replaced.checked = saved === '1';
    replaced.addEventListener('change', () => {
      dashPrefs.setItem(rgkey, replaced.checked ? '1' : '0');
      rerender();
    });
  }
  // Optional "show retired" checkbox (groups tab only) — toggles the
  // virtual Retired group. Defaults ON: a retired agent must stay
  // visible somewhere on the tab rather than silently disappearing.
  const retired = $(`#filter-${tab}-retired`);
  if (retired) {
    const rkey = `tclaude.dash.retired.${tab}`;
    const saved = dashPrefs.getItem(rkey);
    retired.checked = saved === null ? true : saved === '1';
    retired.addEventListener('change', () => {
      dashPrefs.setItem(rkey, retired.checked ? '1' : '0');
      rerender();
    });
  }
  // Optional "show offline scribes" checkbox (groups tab only). Live scribe
  // groups are always visible; this preference reveals dormant system groups.
  // Defaults OFF because those are machinery rather than managed teams.
  const scribe = $(`#filter-${tab}-scribe`);
  if (scribe) {
    const sckey = `tclaude.dash.scribe.${tab}`;
    const saved = dashPrefs.getItem(sckey);
    scribe.checked = saved === '1';
    scribe.addEventListener('change', () => {
      dashPrefs.setItem(sckey, scribe.checked ? '1' : '0');
      rerender();
    });
  }
  // Optional ▾ view popover (groups tab only) — collapses the six
  // "show X" checkboxes above behind a single button so the filter
  // bar stays compact. Restoration of each checkbox's state has
  // already happened above; this only wires the trigger + open/close
  // behaviour + a badge that surfaces the number of toggles deviating
  // from their defaults (so a user can see at a glance whether
  // anything is being hidden).
  const viewBtn = $(`#filter-${tab}-view-btn`);
  const viewMenu = $(`#filter-${tab}-view-menu`);
  const viewBadge = $(`#filter-${tab}-view-badge`);
  if (viewBtn && viewMenu && viewBadge) {
    // Defaults match the `checked` attributes in dashboard.html. The
    // first three default ON (showing everything); 'conversations',
    // 'replaced' and 'scribe' default OFF (each can grow large / is
    // archival / is machinery). Edit BOTH places together if the defaults
    // ever change.
    const viewDefaults = {
      [`filter-${tab}-offline`]: true,
      [`filter-${tab}-ungrouped`]: true,
      [`filter-${tab}-retired`]: true,
      [`filter-${tab}-conversations`]: false,
      [`filter-${tab}-replaced`]: false,
      [`filter-${tab}-scribe`]: false,
    };
    const updateViewBadge = () => {
      let n = 0;
      for (const [id, want] of Object.entries(viewDefaults)) {
        const el = document.getElementById(id);
        if (el && el.checked !== want) n++;
      }
      // Each member column whose visibility differs from its default is one
      // more deviation from the default view, so it adds to the same badge.
      n += memberColDeviationCount();
      if (n === 0) {
        viewBadge.hidden = true;
        viewBadge.textContent = '';
      } else {
        viewBadge.hidden = false;
        viewBadge.textContent = String(n);
      }
    };
    // Populate the "Columns" section (groups tab only) — one checkbox per
    // hideable member column, built from MEMBER_COLS so the menu can't drift
    // from the table. Checked = shown. Toggling persists via
    // setMemberColHidden (dashPrefs) and rerenders; the badge picks the
    // change up through the bubbled `change` listener below.
    const colsBox = $(`#filter-${tab}-cols`);
    if (colsBox) {
      colsBox.replaceChildren();
      for (const c of hideableMemberCols()) {
        const label = document.createElement('label');
        label.className = 'filter-toggle';
        label.title = `Show the "${c.label}" column`;
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.id = `filter-${tab}-col-${c.key}`;
        cb.checked = !memberColHidden(c.key);
        cb.addEventListener('change', () => {
          setMemberColHidden(c.key, !cb.checked);
          rerender();
        });
        const span = document.createElement('span');
        span.textContent = c.label;
        label.append(cb, span);
        colsBox.append(label);
      }
    }
    updateViewBadge();
    // change bubbles up from the contained inputs, so one listener on
    // the popover covers all the checkboxes (row + column toggles alike).
    // The per-checkbox handlers already persist + rerender; this only
    // refreshes the badge.
    viewMenu.addEventListener('change', updateViewBadge);
    const closeViewMenu = () => {
      viewMenu.classList.remove('open');
      viewBtn.setAttribute('aria-expanded', 'false');
    };
    viewBtn.addEventListener('click', () => {
      const willOpen = !viewMenu.classList.contains('open');
      viewMenu.classList.toggle('open', willOpen);
      viewBtn.setAttribute('aria-expanded', willOpen ? 'true' : 'false');
    });
    // Outside-click dismissal. The trigger and the popover both live
    // inside .view-popover-wrap, so any click that lands inside the
    // wrapper (the button toggle, or a checkbox in the popover) is
    // left alone; everything else closes.
    document.addEventListener('click', (e) => {
      if (!viewMenu.classList.contains('open')) return;
      if (e.target.closest('.view-popover-wrap')) return;
      closeViewMenu();
    });
    // Escape closes — parity with the ⚙ action menus and modals.
    document.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape') return;
      if (!viewMenu.classList.contains('open')) return;
      e.preventDefault();
      closeViewMenu();
      viewBtn.focus();
    });
  }
}

// groupsFilterTimer debounces the server-side refetch the Groups filter box
// triggers (the three paginated lists filter in SQL, so a query change needs a
// round-trip — see bindFilter).
let groupsFilterTimer = null;

// groupsTabActive reports whether the Groups tab is the visible one — used to
// skip the (default-hidden, expensive) conversations/replaced sub-fetches when
// their virtual group can't be on screen anyway.
function groupsTabActive() {
  const s = $('#tab-groups');
  return !!s && s.classList.contains('active');
}

// Focus preservation across the 2s re-render lives in helpers.js
// (captureFocus / restoreFocus / withPreservedFocus) — shared with
// mail.js, which wraps its own async mail repaint the same way. refresh()
// spreads capture and restore apart by hand below because they straddle
// the non-render snapshot bookkeeping; a single-call wrapper wouldn't fit.
export async function refresh(opts = {}) {
  // force: proceed even while a .modal-overlay is open. Passed by the
  // template/circle mutation handlers that fire from inside (or immediately
  // reopen) a modal, so the list behind it repaints without the human having
  // to close and reopen the view. opts may also be an Event object when
  // refresh is used bare as a callback — .force is simply undefined there, so
  // it reads as a normal (non-forced) refresh. See refreshSuspended's
  // ignoreModals note.
  const force = !!(opts && opts.force);
  if (refreshSuspended({ ignoreModals: force })) {
    // An inline-edit input, a modal, or a drag is in progress;
    // re-rendering now would blow the input away mid-keystroke,
    // disrupt the modal, or detach the dragged row. Skip this tick —
    // the commit / cancel / dragend handlers each re-trigger
    // refresh() once the user is done.
    return;
  }
  // responded flips true the instant the /api/snapshot fetch resolves (agentd
  // answered, any status). The catch below counts a disconnect ONLY when it's
  // still false — i.e. the fetch itself REJECTED (agentd unreachable) — so an
  // error thrown after agentd already answered (json parse, a renderer) never
  // masquerades as a connection drop. See connection.js.
  let responded = false;
  const requestId = dashboardState.beginRequest();
  const jobs = featureState('jobs');
  const jobsActive = dashboardState.activeTab.value === 'jobs' && !!jobs;
  if (jobsActive) jobs.beginRequest(requestId);
  try {
    // The three heavy, ever-growing lists — retired / conversations / replaced
    // — no longer ride inside the snapshot. Each has its own paginated endpoint
    // fetched ALONGSIDE the snapshot (one Promise.all, one render); the windowed
    // pages are stitched back on below.
    //
    // Two gates keep this cheap: (1) conversations + replaced are fetched only
    // when the Groups tab is showing their (default-hidden) virtual group — no
    // point pulling them every tick on another tab or when collapsed; retired is
    // always fetched (default-visible + it drives the palette's delete-retired
    // count). (2) the Groups filter box value rides along as the server-side `q`
    // so the filter searches the WHOLE list, not just the loaded page.
    //
    // List sub-fetches swallow a network rejection (→ null) so a blip on one
    // degrades to "keep the previous rows" (stitchListPage) rather than failing
    // the tick. The snapshot fetch keeps its original behaviour — its network
    // error rejects to the outer catch.
    const groupsQ = ($('#filter-groups')?.value || '').trim();
    const onGroups = groupsTabActive();
    // The Jobs tab's unified table (exports + cron) is windowed the same way —
    // fetched only while its tab is showing; the nav badge stays live off the
    // snapshot's export_jobs_active count regardless.
    const get = (path) => fetch(path, { credentials: 'same-origin' }).catch(() => null);
    const [snapR, retiredR, convR, replacedR, jobsR] = await Promise.all([
      fetch('/api/snapshot', { credentials: 'same-origin' }),
      get('/api/retired?' + listParams('retired', groupsQ)),
      (onGroups && conversationsVisible()) ? get('/api/conversations?' + listParams('conversations', groupsQ)) : Promise.resolve(undefined),
      (onGroups && replacedVisible()) ? get('/api/replaced?' + listParams('replaced', groupsQ)) : Promise.resolve(undefined),
      jobsActive ? get('/api/jobs?' + jobs.params.value) : Promise.resolve(undefined),
    ]);
    // agentd answered this poll (any HTTP status) — we're connected. Clear the
    // disconnect banner + resume music if it had been raised. Done before the
    // stale-request bail below so even a superseded run registers the reconnect.
    responded = true;
    noteConnected();
    // A newer refresh() (a pager click, a filter change, or the next interval
    // tick) started while this one's fetches were in flight — drop this stale
    // run before it touches any shared state. Without this, a slow older refresh
    // resuming LAST clobbers the newer page and resets the stored offset
    // (the shared store owns the request generation used for this guard).
    if (!dashboardState.isCurrentRequest(requestId)) {
      jobs?.discardRequest(requestId);
      return;
    }
    if (!snapR.ok) {
      jobs?.failRequest(requestId, `HTTP ${snapR.status}`);
      dashboardState.failRequest(requestId, `HTTP ${snapR.status}`, { responded: true });
      showStatus('snapshot failed: HTTP ' + snapR.status, true);
      return;
    }
    const data = await snapR.json();
    if (!dashboardState.isCurrentRequest(requestId)) {
      jobs?.discardRequest(requestId);
      return;
    }
    // Query/page controls invalidate the feature request immediately, before
    // their debounced/immediate successor refresh starts. Never publish rows
    // fetched with parameters that no longer match the visible controls.
    if (jobsActive && !jobs.acceptsRequest(requestId)) {
      dashboardState.discardRequest(requestId, { responded });
      return;
    }
    // The suspend guard was sampled BEFORE the fetch; a drag/modal may have
    // opened since. Re-check before touching the DOM (this preserves any
    // optimistic drag mutation on the old snapshot; its teardown re-runs us).
    if (refreshSuspended({ ignoreModals: force })) {
      dashboardState.discardRequest(requestId, { responded });
      jobs?.discardRequest(requestId);
      return;
    }
    // Stitch each windowed list onto the snapshot so the downstream renderers
    // keep reading data.retired / .conversations / .replaced unchanged.
    // data.paging carries each list's {offset,limit,total,total_unfiltered} for
    // the pagers + count summaries. A failed OR gated-off (undefined) sub-fetch
    // keeps the previous tick's rows for that list — a blip / a collapsed group
    // must not blank a section.
    const prevSnap = lastSnapshot || {};
    data.paging = {};
    await stitchListPage(data, 'retired', retiredR, prevSnap);
    await stitchListPage(data, 'conversations', convR, prevSnap);
    await stitchListPage(data, 'replaced', replacedR, prevSnap);
    const jobsResult = await stitchListPage(data, 'jobs', jobsR, prevSnap);
    // stitchListPage awaited resp.json() (async boundaries) — re-check the request
    // (a newer refresh may have started) AND the suspend guard (a drag/modal may
    // have opened) before mutating shared offset state and the DOM.
    if (!dashboardState.isCurrentRequest(requestId)) {
      jobs?.discardRequest(requestId);
      return;
    }
    if (jobsActive && !jobs.acceptsRequest(requestId)) {
      dashboardState.discardRequest(requestId, { responded });
      return;
    }
    if (refreshSuspended({ ignoreModals: force })) {
      dashboardState.discardRequest(requestId, { responded });
      jobs?.discardRequest(requestId);
      return;
    }
    // Reconcile each list's stored offset with the server's CLAMPED served
    // offset — done HERE, after the request guard, so a stale refresh can never
    // write it (the pager-clobber bug). No-op when the offset didn't move.
    syncServedOffset('retired', data.paging.retired.offset);
    syncServedOffset('conversations', data.paging.conversations.offset);
    syncServedOffset('replaced', data.paging.replaced.offset);
    // A failed sub-fetch carries the previous successful paging envelope so
    // stale rows remain visible. Do not mistake that fallback for a served
    // offset: Retry must keep targeting the page the user requested.
    if (jobsActive && jobsResult.ok) jobs.syncServedOffset(data.paging.jobs.offset);
    // Snapshot the keyboard focus before the renders below replace the
    // tab bodies wholesale, so a Tab-navigating user isn't bounced to
    // the top of the page on every poll. Restored at the end once the
    // fresh DOM is in place.
    const focusToken = captureFocus();
    setLastSnapshot(data);
    syncDashDefaultProfile(data.spawn_profile_default);
    // Split into a stable URL span (written once / only when the base changes)
    // and a per-tick timestamp span, morphed in place. A single textContent
    // write recreated the whole text node every 2s, so selecting the URL to
    // copy it died on the next tick; now the URL span is isEqualNode-identical
    // across ticks and skipped, so a selection anchored in it survives.
    const dashboardVersion = data.version || 'unknown';
    morphInto($('#meta'),
      `<span class="meta-version">tclaude version ${esc(dashboardVersion)}</span>`
      + `<span class="meta-sep"> · </span><span class="meta-base">${esc(data.popup_base)}</span>`
      + `<span class="meta-sep"> · </span>refreshed <span class="meta-time">${esc(new Date(data.generated_at).toLocaleTimeString())}</span>`);
    // Refresh the proactive-grant blocklist hint from the snapshot
    // when present; falls back to the v1 hardcoded pair otherwise.
    // (Snapshot doesn't carry the resolved blocklist directly; the
    // server returns 403 on submit if the picker missed one — the
    // UI just dims the well-known pair so the common case is
    // self-explanatory.)
    sudoGrantBlocklist = ['permissions.grant', 'permissions.revoke'];
    sudoByConv = {};
    (data.sudo || []).forEach(g => {
      if (!sudoByConv[g.conv_id]) sudoByConv[g.conv_id] = [];
      sudoByConv[g.conv_id].push(g);
    });
    renderGroupsTab();
    renderGlobalActivity();
    renderTemplatesTab();
    // The right-side palette dock (JOH-374) rides the poll like the rest —
    // keyed morphInto so its selection/scroll survive and a manager edit
    // shows up on the next tick.
    renderDock();
    renderSudoTab();
    renderLinksTab();
    renderPluginsTab();
    renderPluginsBadge(data.plugins_warn || 0);
    applyPluginsTabVisibility(data);
    applyProcessesTabVisibility(data);
    // Permissions + Slug registry now live as sub-panels of the merged
    // "Access" tab; the renderers write into the per-panel mount divs.
    // morphInto reconciles rather than swapping innerHTML, so a selection in
    // the roster survives the 2s poll (the copy-paste fix); the mount divs
    // themselves are never replaced.
    morphInto($('#permissions-body'), renderPermissions(data.permissions, data.agents));
    morphInto($('#slugs-body'), renderSlugs(data.slugs));
    renderMailTab();
    renderMessagesBadge(data.messages_unread || 0, data.access_requests_pending || 0);
    renderAccessRequests(data.access_requests || [], data.access_requests_pending || 0);
    renderUsage(data.usage);
    renderDashDefaultProfile();
    renderDashSandboxProfile();
    renderNotifyGlobal(!!data.notifications_enabled);
    applyCostTabVisibility(data);
    setVegasRegularMode(!!data.vegas_in_regular_mode);
    // Horizontal-scroll chrome-bar mode (config dashboard.hscroll_follow,
    // default follow) — replaces the old per-browser header toggle button.
    setHScrollFollow(data.hscroll_follow !== false);
    // Group quick-options fold mode (config dashboard.group_quick_options,
    // default "hover"). body.group-quick-fold drives the CSS horizontal
    // accordion: the editable chips in each group <summary> collapse to
    // icon-only at rest and expand on header hover. "expanded" keeps them
    // full. A plain class toggle, like hide-slop-lever below — render.js
    // already rebuilt the (re-rendered) group rows this same tick, so the
    // class is the only extra state. Folding is gated to hover-capable
    // pointers in CSS, so touch devices stay expanded whatever the mode.
    document.body.classList.toggle('group-quick-fold', data.group_quick_options !== 'expanded');
    // Hide the slop-mode side pull-lever when config slop.hide_pull_lever is
    // set. body.hide-slop-lever drops the lever via CSS while leaving the
    // rest of slop mode intact; a plain class toggle (like hide-costs).
    document.body.classList.toggle('hide-slop-lever', !!data.hide_pull_lever);
    // Per-agent "hide window" button visibility (config
    // dashboard.show_agent_hide_button, default off). The slashed-eye "hide"
    // icon beside "focus" detaches the agent's terminal window — far less used
    // than focus — so it's hidden by default to keep the row's quick-control
    // cluster tight; CSS drops it unless body.show-agent-hide-btn is present.
    // A plain class toggle, like group-quick-fold above.
    document.body.classList.toggle('show-agent-hide-btn', !!data.show_agent_hide_button);
    // Group description chip visibility (config dashboard.show_group_description,
    // default off). Group descriptions are a deprecated, display-only feature —
    // the 📝 chip in each group header is hidden unless body.show-group-description
    // is present, brought back only by an explicit opt-in. Plain class toggle.
    document.body.classList.toggle('show-group-description', !!data.show_group_description);
    // The leading ● is rendered by CSS (#status::before) so it can
    // pick up the green "live" colour without us round-tripping HTML
    // through showStatus.
    showStatus('live', false);
    // Notify out-of-tree consumers (currently slop-fx.js's marquee)
    // that fresh snapshot data is now in lastSnapshot. A custom event
    // keeps the dependency one-way — refresh.js doesn't have to
    // import any feature module that wants to react to a tick.
    document.dispatchEvent(new CustomEvent('tclaude:snapshot'));
    // Re-focus whatever the keyboard user had selected before the
    // re-render detached it. No-op when focus was never stolen.
    restoreFocus(focusToken);
    // Publish only after the full legacy render succeeds. Signal subscribers
    // can now react without observing a snapshot the current UI failed to
    // finish applying.
    dashboardState.commitRequest(requestId, data);
    if (jobsActive) {
      if (jobsResult.ok) jobs.commitRequest(requestId);
      else jobs.failRequest(requestId, jobsResult.error);
    }
  } catch (e) {
    jobs?.failRequest(requestId, e);
    if (!dashboardState.failRequest(requestId, e, { responded })) return;
    // Only a REJECTED /api/snapshot fetch (agentd unreachable — connection
    // refused / network down, so `responded` never flipped) counts toward the
    // disconnect banner; a fault thrown after agentd already answered is a
    // client-side error, not a lost connection.
    if (!responded) noteDisconnected();
    showStatus('snapshot failed: ' + (e.message || e), true);
  }
}

// stitchListPage folds one paginated list endpoint's response onto the
// snapshot object so the virtual-group renderers + pagers read it like a
// plain snapshot field. On a failed / non-OK sub-fetch it keeps the previous
// tick's rows + paging for that list, so a transient blip never blanks a
// section mid-poll.
async function stitchListPage(data, kind, resp, prevSnap) {
  try {
    if (resp && resp.ok) {
      const body = await resp.json();
      data[kind] = body.rows || [];
      data.paging[kind] = {
        offset: body.offset || 0,
        limit: body.limit || 0,
        total: body.total || 0,
        total_unfiltered: body.total_unfiltered || 0,
      };
      // Offset reconciliation (syncServedOffset) is deliberately NOT done here —
      // it mutates shared module state, so refresh() applies it only after its
      // request guard, so a stale run can't write a clobbering offset.
      return { ok: true, error: null };
    }
  } catch (error) {
    data[kind] = (prevSnap && prevSnap[kind]) || [];
    data.paging[kind] = (prevSnap && prevSnap.paging && prevSnap.paging[kind])
      || { offset: 0, limit: 0, total: (data[kind] || []).length, total_unfiltered: (data[kind] || []).length };
    return { ok: false, error };
  }
  data[kind] = (prevSnap && prevSnap[kind]) || [];
  data.paging[kind] = (prevSnap && prevSnap.paging && prevSnap.paging[kind])
    || { offset: 0, limit: 0, total: (data[kind] || []).length, total_unfiltered: (data[kind] || []).length };
  return {
    ok: resp === undefined,
    error: resp === undefined ? null : new Error(resp ? `HTTP ${resp.status}` : 'network error'),
  };
}

// bindListPagers wires the per-list pager footers rendered inside the Retired /
// Conversations / Replaced virtual groups. Delegated on the stable #groups-list
// parent (the group bodies are re-rendered wholesale every tick). Pager
// controls carry data-pager (not data-act) so the global row-action handler
// leaves them alone. A nav/size change updates the list's offset/limit, then
// re-fetches via refresh() — keeping it the same single coordinated tick.
export function bindListPagers() {
  // The Groups tab's virtual lists still render legacy pager HTML. The Jobs
  // island owns its pager events directly.
  for (const root of [$('#groups-list')]) {
    if (!root) continue;
    root.addEventListener('click', (e) => {
      const btn = e.target.closest('button[data-pager]');
      if (!btn || btn.disabled) return;
      const kind = btn.getAttribute('data-list');
      const action = btn.getAttribute('data-pager');
      const total = (lastSnapshot && lastSnapshot.paging && lastSnapshot.paging[kind]
        && lastSnapshot.paging[kind].total) || 0;
      if (listPagerNav(kind, action, total)) refresh();
    });
    root.addEventListener('change', (e) => {
      const sel = e.target.closest('select[data-pager="size"]');
      if (!sel) return;
      setListPageSize(sel.getAttribute('data-list'), Number(sel.value) || 50);
      refresh();
    });
  }
}

function bindTabs() {
  $$('nav [data-tab]').forEach(b => {
    b.addEventListener('click', e => {
      // The tabs are real <a href> anchors: a modified/middle click is left to
      // the browser, which opens the location in a new tab (this view untouched).
      // A plain left-click (including a synthetic element.click() from the
      // command palette or [/] cycling) switches in place, so preventDefault
      // stops the anchor's own navigation. Vegas stays a <button> — no href, so
      // preventDefault is a harmless no-op there.
      if (isModifiedClick(e)) return;
      e.preventDefault();
      $$('nav [data-tab]').forEach(x => x.classList.toggle('active', x === b));
      $$('main section').forEach(s => {
        s.classList.toggle('active', s.id === 'tab-' + b.dataset.tab);
      });
      dashboardState.setActiveTab(b.dataset.tab);
      if (b.dataset.tab === 'jobs') void refresh();
    });
    // <a> activates on Enter only, whereas the former <button> also switched on
    // Space; restore that parity so a keyboard user's Space still selects the
    // focused tab (preventDefault stops the page from scrolling instead). The
    // synthetic click routes through the handler above. Vegas is a real
    // <button> — Space fires its click natively — so skip it to avoid a
    // double toggle.
    if (b.tagName === 'A') {
      b.addEventListener('keydown', e => {
        if (e.key !== ' ' && e.key !== 'Spacebar') return;
        e.preventDefault();
        b.click();
      });
    }
  });
}

// visibleTabButtons returns the nav tab buttons that are actually on
// screen, in DOM (left-to-right) order. offsetParent === null means a
// display:none somewhere up the chain — which is exactly how the Vegas
// tab (hidden unless body.slop) and the Costs tab (hidden via
// body.hide-costs) drop out. Checking visibility instead of naming those
// two keeps the cycler correct if more conditional tabs appear later.
function visibleTabButtons() {
  return $$('nav [data-tab]').filter(b => b.offsetParent !== null);
}

// cyclingTabs is true only while cycleTab() is dispatching its synthetic
// nav-button .click() for a keyboard tab-cycle ([ / ] or ←/→). A per-tab
// activation handler can read isCyclingTabs() to behave differently for a
// keyboard cycle than for a deliberate switch (mouse click / command
// palette / deep link). Today only the Config tab uses it: it focuses its
// search box on a deliberate switch, but NOT mid-cycle — focusing the
// <input> would make isEditableTarget() true and trap the very [ / ] / ←/→
// keys used to keep cycling, stranding the user on Config. .click()
// dispatches its listeners synchronously, so the flag is observably true
// for exactly the duration of the handlers it triggers.
let cyclingTabs = false;
export function isCyclingTabs() { return cyclingTabs; }

// cycleTab moves the active tab by `dir` (+1 = right / next, -1 = left /
// prev) across the visible tabs, wrapping around at both ends. Activation
// goes through the button's own .click() on purpose: several tabs hang an
// extra click listener on their nav button to lazy-load their data
// (loadCosts, loadAudit, the Messages/Config tabs…), and a synthetic
// .click() fires every one of them — so keyboard cycling behaves
// identically to a mouse click. Returns the newly-activated button (or
// null if there are somehow no visible tabs).
function cycleTab(dir) {
  const tabs = visibleTabButtons();
  if (!tabs.length) return null;
  const active = tabs.findIndex(b => b.classList.contains('active'));
  // active < 0 ⇒ the current tab is itself hidden (e.g. you were on Vegas
  // and slop just turned off); start the step from the first visible tab.
  const from = active < 0 ? 0 : active;
  const next = (from + dir + tabs.length) % tabs.length;
  // Mark this as a keyboard cycle so handlers fired by the synthetic click
  // (e.g. the Config tab's search-focus) can opt out — see isCyclingTabs.
  cyclingTabs = true;
  try {
    tabs[next].click();
  } finally {
    cyclingTabs = false;
  }
  return tabs[next];
}

// isEditableTarget guards the bare-bracket hotkey so it never hijacks
// text entry — while you're in any input/textarea/select or a
// contenteditable, [ and ] type their literal character as usual.
function isEditableTarget(el) {
  if (!el) return false;
  const tag = el.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' ||
    el.isContentEditable;
}

// bindTabHotkeys wires keyboard tab cycling. The obvious chords
// (Cmd/Ctrl+[ ], Ctrl+Tab, Cmd/Ctrl+1..9) are all reserved by the browser
// for switching ITS OWN tabs and can't be intercepted from a page, so we
// use the two conflict-free idioms web apps settle on:
//   • bare [ / ] cycle prev / next — but only when you're not typing in a
//     field and no modal/overlay is open, so it never steals a keystroke.
//   • ←/→ cycle while the tab bar itself holds keyboard focus (the
//     WAI-ARIA tablist pattern); roving focus follows the activated tab so
//     repeated arrows keep stepping.
function bindTabHotkeys() {
  document.addEventListener('keydown', e => {
    // Leave every modifier chord to the browser / app — plain keys only.
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    // ←/→ while focus is on the tab bar (ARIA tablist idiom). preventDefault
    // so the arrow doesn't also scroll, and move focus onto the new tab.
    if ((e.key === 'ArrowLeft' || e.key === 'ArrowRight') &&
        document.activeElement && document.activeElement.closest('nav')) {
      e.preventDefault();
      const moved = cycleTab(e.key === 'ArrowRight' ? 1 : -1);
      if (moved) moved.focus();
      return;
    }

    // Bare [ / ] anywhere on the page — except while typing or with a modal
    // open (a modal traps interaction; cycling tabs behind it is surprising).
    // Compared against e.key (not e.code) so it follows the user's layout.
    if ((e.key === '[' || e.key === ']') && !isEditableTarget(e.target) &&
        !document.querySelector('.modal-overlay.show, .manage-overlay.show')) {
      e.preventDefault();
      cycleTab(e.key === ']' ? 1 : -1);
    }
  });
}

// applyCostTabVisibility drives the Costs tab's auto-hide and WHAT-IF mode
// off the snapshot's server-computed flags:
//   - cost_tab_visible false → hide the Costs nav button + section entirely
//     (a subscription account with no real spend and no WHAT-IF opt-in: the
//     tab would only show an empty chart). The 💲 cost toggle hides too (CSS).
//   - cost_tab_whatif true → body.cost-whatif lets the per-agent cost badge
//     (helpers.js harnessLine) and the Costs tab render the hypothetical
//     pay-per-token-equivalent figures, with a banner.
// If the Costs tab is the active one when it gets hidden (e.g. the human just
// turned the opt-in off in the Config tab), fall back to Groups so they
// aren't stranded on a now-invisible section — mirrors leaveVegasTabIfActive
// in vegas.js (kept local to avoid a circular import).
function applyCostTabVisibility(data) {
  const visible = !!(data && data.cost_tab_visible);
  const whatif = !!(data && data.cost_tab_whatif);
  document.body.classList.toggle('hide-costs', !visible);
  document.body.classList.toggle('cost-whatif', whatif);
  if (!visible) {
    const sec = document.getElementById('tab-costs');
    if (sec && sec.classList.contains('active')) {
      $$('nav [data-tab]').forEach(b => b.classList.toggle('active', b.dataset.tab === 'groups'));
      $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-groups'));
      dashboardState.setActiveTab('groups');
    }
  }
}

// applyPluginsTabVisibility drives the Plugins tab's auto-hide off the
// server's plugins_tab_visible flag (dashboard.go), mirroring
// applyCostTabVisibility: most users never define a plugin, so an empty
// Plugins tab is just clutter. body.hide-plugins removes the nav button +
// section via CSS; the server keeps the tab visible whenever something IS
// there to manage (≥1 plugin, a broken plugins.json, or the
// dashboard.always_show_plugins_tab opt-in). If the Plugins tab is the
// active one when it gets hidden — e.g. the human deleted their last plugin,
// or turned the opt-in off in the Config tab — fall back to Groups so they
// aren't stranded on a now-invisible section.
function applyPluginsTabVisibility(data) {
  const visible = !!(data && data.plugins_tab_visible);
  document.body.classList.toggle('hide-plugins', !visible);
  if (!visible) {
    const sec = document.getElementById('tab-plugins');
    if (sec && sec.classList.contains('active')) {
      $$('nav [data-tab]').forEach(b => b.classList.toggle('active', b.dataset.tab === 'groups'));
      $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-groups'));
      dashboardState.setActiveTab('groups');
    }
  }
}

// The "Access" tab merges three former tabs — Permissions, Slug
// registry and Sudo — behind one nav button. Inside it a segmented
// control switches between the three sub-panels. Unlike the top-level
// tabs, the per-panel innerHTML keeps getting refreshed every 2s
// regardless of which sub-tab is visible (the panels are just hidden),
// so selecting a sub-tab is purely a CSS .active toggle that survives
// the poll.
function bindAccessSubtabs() {
  const subnav = $('#tab-access .access-subnav');
  if (!subnav) return;
  subnav.addEventListener('click', e => {
    const btn = e.target.closest('[data-subtab]');
    if (!btn) return;
    // Real <a href> subtab links: a modified/middle click opens /access/<sub>
    // in a new tab; a plain click switches in place (preventDefault stops the
    // anchor's navigation). See isModifiedClick / bindTabs.
    if (isModifiedClick(e)) return;
    e.preventDefault();
    activateAccessSubtab(btn.dataset.subtab);
  });
  // Space-activation parity for the anchor subtabs (see bindTabs): <a> switches
  // on Enter only, so shim Space to keep the former <button> keyboard behaviour.
  subnav.addEventListener('keydown', e => {
    if (e.key !== ' ' && e.key !== 'Spacebar') return;
    const a = e.target.closest('a[data-subtab]');
    if (!a) return;
    e.preventDefault();
    a.click();
  });
}

// activateAccessSubtab selects one of the Access tab's sub-views
// (permissions / slugs / sudo) by toggling .active on the matching
// segmented-control button and its panel. Exported so deep links (the
// 🔓 sudo-manage badge) can jump straight to a sub-view.
export function activateAccessSubtab(name) {
  $$('#tab-access .access-subtab').forEach(b => {
    const on = b.dataset.subtab === name;
    b.classList.toggle('active', on);
    b.setAttribute('aria-selected', on ? 'true' : 'false');
  });
  $$('#tab-access .access-panel').forEach(p => {
    p.classList.toggle('active', p.id === 'access-' + name);
  });
  // Tell the history router the location changed (→ /access/<sub>). One-way
  // event so refresh.js doesn't import nav-history.js; nav-history records it as
  // user navigation (no-op during its own programmatic restore). See
  // nav-history.js recordCurrentLocation.
  document.dispatchEvent(new CustomEvent('tclaude:navigated'));
}

// showAccessTab brings the top-level Access tab forward and (optionally)
// selects a sub-view. Used by the sudo-manage deep link so a click on an
// agent's 🔓 badge lands on the Sudo sub-view pre-filtered to that agent.
export function showAccessTab(subtab) {
  $$('nav [data-tab]').forEach(x => x.classList.toggle('active', x.dataset.tab === 'access'));
  $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-access'));
  dashboardState.setActiveTab('access');
  if (subtab) activateAccessSubtab(subtab);
}

// Group <details> headers fold/unfold ONLY when the group title is
// clicked — not when the click lands on a header chip (📝/📁/👥/🧠), a
// badge, a link chip, or the empty space to the right of them. The
// <summary> is one wide hit target (it doubles as a drag/drop zone), so
// before this a stray click anywhere on the header toggled the group;
// scoping the toggle to the .group-name label (the disclosure triangle
// now rides on it, see dashboard.css) leaves the rest of the header inert
// for folding. Other <details> in the dashboard (the permissions
// disclosure, the advanced-config panel) carry no data-group-key and keep
// native whole-summary toggling.
//
// Capturing listener so preventDefault lands before the browser's default
// toggle. Keyboard activation (Enter/Space on a focused summary) arrives
// as a synthetic click with detail === 0 — normally left alone so the header
// stays keyboard-foldable.
//
// The exception: an inline-edit field (📝 descr / 📁 dir / 👥 cap / 🧠 profile)
// is swapped in *inside* the <summary>, so while you type in it a Space press
// fires that same detail===0 summary activation and would fold/unfold the
// group on every space — most visible in the free-text description. So when an
// edit field within this summary holds focus, suppress the keyboard toggle (the
// space still lands in the input; only the fold is cancelled). Verified in
// Chromium: the Space activation is a synthetic click on the <summary>, and a
// capture-phase preventDefault cancels the fold without eating the character.
// hoveredGroupKey: the group whose <summary> header the pointer is currently
// over, tracked in JS so the quick-options auto-fold reveal (render.js +
// dashboard.css) survives the 2s wholesale innerHTML re-render of #groups-list.
//
// A pure CSS :hover can't carry the reveal across that re-render on its own: a
// freshly-inserted node sitting under a STATIONARY cursor is not re-matched by
// :hover in Blink/WebKit until the next mouse move, so the rebuilt header would
// compute to the folded state and the chips would snap shut on every poll while
// the user holds still to read them. renderGroups re-stamps .quick-hover from
// this key each render, and bindGroupQuickHover keeps it live between renders,
// so the reveal is deterministic regardless of the browser's :hover bookkeeping.
// (The CSS keeps :hover too, for the instant smooth reveal during live movement.)
export let hoveredGroupKey = null;

// bindGroupQuickHover tracks the hovered group header on the stable
// #groups-list container — bound once at init, delegated, because the
// container's inner HTML is replaced every poll but the container itself is
// not. mouseover sets the key to the group whose <summary> the pointer is over
// (null over the expanded member body, matching the header-only reveal), and
// mouseleave clears it when the pointer exits the list entirely. It also
// toggles .quick-hover on the live <details> immediately so live interaction is
// smooth without waiting for the next render.
function bindGroupQuickHover() {
  const root = $('#groups-list');
  if (!root) return;
  const setHover = key => {
    if (key === hoveredGroupKey) return;
    hoveredGroupKey = key;
    // Re-sync the live DOM now; the next renderGroups also re-stamps from the
    // key, so a poll landing between events can't lose it.
    root.querySelectorAll('details[data-group-key]').forEach(d => {
      d.classList.toggle('quick-hover', d.getAttribute('data-group-key') === key);
    });
  };
  root.addEventListener('mouseover', e => {
    // closest() with a child combinator matches a <summary> that is a direct
    // child of a group <details> — true when the pointer is over the header or
    // any chip in it, false over the expanded subtable below.
    const summary = e.target.closest('details[data-group-key] > summary');
    setHover(summary ? summary.parentElement.getAttribute('data-group-key') : null);
  });
  // mouseleave (not mouseout) fires once when the pointer truly exits the
  // container, so a header left stale by the last in-list mouseover is cleared.
  root.addEventListener('mouseleave', () => setHover(null));
}

function bindGroupTitleToggle() {
  document.addEventListener('click', e => {
    const summary = e.target.closest('summary');
    if (!summary) return;
    const details = summary.parentElement;
    if (!details || !details.hasAttribute('data-group-key')) return;
    if (e.detail === 0) {
      // Keyboard activation. Keep native folding when the summary itself is
      // focused, but block it while typing in an inline-edit field it hosts.
      const ae = document.activeElement;
      if (ae && summary.contains(ae) && ae.matches('input, textarea, select')) {
        e.preventDefault();
        return;
      }
      // A chip activation synthesized by row-actions' Enter/Space delegate
      // also arrives with detail === 0. It's a chip action, not a
      // fold/unfold — don't retarget the palette's default spawn group off
      // it (a mouse click on the same chip doesn't either). The dispatcher's
      // own preventDefault stops the summary toggle.
      if (e.target.closest('[data-act]')) return;
      // Genuine keyboard fold/unfold — remember it as the last group touched
      // (drives the command palette's default spawn target).
      recordGroupInteraction(details.getAttribute('data-group-key'));
      return;
    }
    if (e.target.closest('.group-name')) {
      // Genuine mouse fold/unfold of the group title — remember it.
      recordGroupInteraction(details.getAttribute('data-group-key'));
      return; // the title — allow toggle
    }
    e.preventDefault();
  }, true);
}

// <details> only fires `toggle` on the element itself (not bubbling),
// so use a capturing listener at the document level rather than
// re-binding per-element after every render.
function bindDetailsPersistence() {
  document.addEventListener('toggle', e => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement)) return;
    const key = d.getAttribute('data-group-key');
    if (!key) return;
    if (d.open) {
      dashPrefs.setItem('tclaude.dash.group.' + key, '1');
    } else {
      dashPrefs.removeItem('tclaude.dash.group.' + key);
    }
  }, true);
}

// bindSortHeaders delegates clicks on sortable <th> cells. Headers
// are re-rendered on every 2s refresh, so a single document-level
// listener is simpler than re-binding per render (same approach as
// bindDetailsPersistence). Clicking re-renders just the
// affected tab so the new ordering — and the header arrow — show
// immediately, without waiting for the next poll.
function bindSortHeaders() {
  document.addEventListener('click', e => {
    const th = e.target.closest('th[data-sort-table]');
    if (!th) return;
    const tableKey = th.dataset.sortTable;
    cycleSort(tableKey, th.dataset.sortCol);
    // 'replaced'/'retired'/'conversations'/'pending' are the virtual
    // sub-tables (Replaced generations / Retired / Conversations / Pending),
    // all rendered as part of the groups tab — so re-render that, same as
    // 'members'.
    if (tableKey === 'members' || tableKey === 'replaced'
        || tableKey === 'retired' || tableKey === 'conversations'
        || tableKey === 'pending') renderGroupsTab();
    else if (tableKey === 'sudo') renderSudoTab();
    else if (tableKey === 'links') renderLinksTab();
  });
}

// --- inline mutations: action buttons + confirm modal + toast ---

// confirmModal pops the confirmation overlay; resolves true on
// OK, false on Cancel / outside-click / Escape. Escape is handled in
// capture phase with stopImmediatePropagation so that dismissing a
// confirm popped on top of a form modal cancels only the confirm —
// the Escape never leaks down to the underlying form's own dismiss
// handler.
export function confirmModal({title, body, meta, okLabel, cancelLabel}) {
  return new Promise(resolve => {
    const overlay = $('#confirm-modal');
    $('#confirm-title').textContent = title;
    $('#confirm-body').textContent = body;
    $('#confirm-meta').textContent = meta || '';
    $('#confirm-meta').style.display = meta ? 'block' : 'none';
    const okBtn = $('#confirm-ok');
    okBtn.textContent = okLabel || 'Confirm';
    const cancelBtn = $('#confirm-cancel');
    // The cancel button text is reset every call (the modal is a shared
    // singleton); default 'Cancel' matches the static HTML, so callers that
    // don't pass cancelLabel are unaffected.
    cancelBtn.textContent = cancelLabel || 'Cancel';
    const cleanup = (result) => {
      overlay.classList.remove('show');
      okBtn.removeEventListener('click', onOk);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey, true);
      resolve(result);
    };
    const onOk = () => cleanup(true);
    const onCancel = () => cleanup(false);
    const onOverlay = (e) => { if (e.target === overlay) cleanup(false); };
    const onKey = (e) => {
      if (e.key !== 'Escape') return;
      e.preventDefault();
      e.stopImmediatePropagation();
      cleanup(false);
    };
    okBtn.addEventListener('click', onOk);
    cancelBtn.addEventListener('click', onCancel);
    overlay.addEventListener('click', onOverlay);
    document.addEventListener('keydown', onKey, true);
    overlay.classList.add('show');
    okBtn.focus();
  });
}

// confirmDiscard pops the shared "Discard input?" confirmation used
// whenever a dirty form is about to be dismissed by an ACCIDENTAL
// gesture (Escape / backdrop click). Resolves true when the human
// accepts the discard, false to keep editing. Extracted from
// bindBackdropDiscard so a per-open Promise-based modal that can't use
// the startup binding (e.g. editMemberModal) reuses the exact same copy
// and confirm behavior instead of hand-rolling a second discard prompt.
export function confirmDiscard() {
  return confirmModal({
    title: 'Discard input?',
    body: 'Closing the form will discard any unsaved input. Continue?',
    okLabel: 'Discard',
  });
}

// bindBackdropDiscard wires the dismissal handlers that protect a
// data-entry modal from accidental close — both the backdrop click
// and the Escape key route through the same dirty-check + confirm
// flow. Three gestures previously closed the modal and threw away
// whatever the user had entered:
//
//   1. A genuine backdrop click — pops the shared confirm overlay if
//      the user has actually interacted with any control inside the
//      modal (typed in a text field, toggled a checkbox, picked a
//      file, changed a select). An untouched modal still closes in
//      one click. Pre-populated edit modals only count as dirty once
//      the user changes something — opening then immediately closing
//      an edit modal you didn't touch is friction-free.
//
//   2. A mouse-up that lands on the backdrop after a mouse-down inside
//      the dialog — text-selection drags out of a textarea, drag-and-
//      drop releases onto the backdrop, scrollbar drags that overshoot.
//      The default `click` event fires on the lowest common ancestor
//      (the backdrop) in all three cases, so without this guard the
//      modal would dismiss mid-gesture. We require both endpoints to
//      land on the backdrop before treating it as a dismiss.
//
//   3. Escape — same dirty-check + confirm flow. A clean modal still
//      closes instantly. A nested picker overlay claims ESC with its
//      own capture-phase stopImmediatePropagation so this handler
//      doesn't run while a picker is up. The handler also bails when
//      the shared confirm overlay is already on top, so we never race
//      to pop a second confirm on top of the first.
//
// The explicit Cancel button remains an instant unconditional dismiss
// path. Pass the modal's id (without leading #) and the close function
// to invoke once the user confirms (or the modal is clean). An optional
// canDismiss predicate suppresses both the confirmation and close gesture
// while a caller-owned operation such as an async save is in flight.

// isTopmostOverlay reports whether `el` is the front-most shown overlay, so a
// document-level Escape dismisses only the modal on top — not every shown
// modal at once. The backdrop-CLICK path needs no equivalent (its event lands
// on the top overlay's own element), but Escape is global. Front-most =
// highest computed z-index, DOM order breaking ties (a later sibling paints on
// top). Single-modal (the overwhelmingly common case) short-circuits to true,
// so this only matters once a modal is stacked on another (e.g. the profile
// editor opened over the spawn dialog).
function isTopmostOverlay(el) {
  const shown = [...document.querySelectorAll('.modal-overlay.show, .manage-overlay.show')];
  if (shown.length <= 1) return true;
  const zOf = (n) => parseInt(getComputedStyle(n).zIndex, 10) || 0;
  const myZ = zOf(el);
  return !shown.some((other) => {
    if (other === el) return false;
    const oz = zOf(other);
    if (oz !== myZ) return oz > myZ;
    // Tie on z-index: whichever is later in the DOM is painted on top.
    return !!(el.compareDocumentPosition(other) & Node.DOCUMENT_POSITION_FOLLOWING);
  });
}

export function bindBackdropDiscard(modalId, closeFn, canDismiss = () => true) {
  const el = $('#' + modalId);
  if (!el) return;

  // Dirty tracking: input fires for typed text and pastes; change fires
  // for checkbox/radio toggles, select changes, and file picks. Both
  // bubble up to the modal element. Programmatic value assignment from
  // the modal's open* function does NOT fire these, so pre-population
  // never marks dirty. We reset on every (re)open by observing when the
  // .show class is added.
  let dirty = false;
  const markDirty = () => { dirty = true; };
  el.addEventListener('input', markDirty);
  el.addEventListener('change', markDirty);
  new MutationObserver(() => {
    if (el.classList.contains('show')) dirty = false;
  }).observe(el, { attributes: true, attributeFilter: ['class'] });

  // tryDismiss is the shared exit path: if the modal has been touched,
  // pop the confirm overlay first; otherwise (or once the user accepts
  // the discard) call closeFn.
  const tryDismiss = async () => {
    if (!canDismiss()) return;
    if (dirty && !(await confirmDiscard())) return;
    closeFn();
  };

  // Gesture tracking: capture where the mouse-down originated, so we
  // can distinguish a true backdrop click from a mouse-up that happens
  // to land on the backdrop after a drag from inside.
  let pressedOnBackdrop = false;
  el.addEventListener('mousedown', (e) => {
    pressedOnBackdrop = (e.target === el);
  });

  el.addEventListener('click', (e) => {
    const isBackdropClick = (e.target === el) && pressedOnBackdrop;
    pressedOnBackdrop = false;
    if (!isBackdropClick) return;
    tryDismiss();
  });

  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!el.classList.contains('show')) return;
    // The confirm overlay's capture-phase handler already swallows
    // Escape, but check anyway so we never race to pop a second
    // confirm on top of the first.
    if ($('#confirm-modal').classList.contains('show')) return;
    // Only the front-most modal responds, so an editor opened on top of
    // another modal (Save-as-profile) doesn't also dismiss the one beneath.
    if (!isTopmostOverlay(el)) return;
    e.preventDefault();
    // stopImmediatePropagation is what actually keeps the underlying modal
    // shut. isTopmostOverlay alone isn't enough: the clean-modal dismiss
    // path closes this modal *synchronously* inside tryDismiss, so any other
    // bindBackdropDiscard keydown listener registered after ours (the spawn
    // dialog's, when this is the profile editor stacked on top — bindProfilesUI
    // runs before bindAgentSpawnModal) would then fire for the SAME Escape,
    // re-evaluate isTopmostOverlay — now true, because we just removed our own
    // .show — and dismiss the dialog beneath too. Claiming the event here stops
    // those later sibling listeners before they can run.
    e.stopImmediatePropagation();
    tryDismiss();
  });

  // Return a small handle so a caller can consult the SAME dirty flag before an
  // action that would abandon the form some other way — e.g. the template
  // editor's "Edit with agent" button, which closes the editor to hand off to a
  // scribe and must offer the discard confirm first (JOH-361). markDirty lets a
  // caller flag an edit the DOM listeners can't see — a model-only mutation
  // applied from a STACKED modal (the template editor's per-agent custom launch
  // config, applied from the profile editor on top) fires no input/change in
  // this modal, yet abandoning it would lose real work. Existing callers ignore
  // the return value, so this is purely additive.
  return { isDirty: () => dirty, markDirty };
}

// bindManageOverlayDismiss wires backdrop-click + Escape close for the
// Templates… / Links… management overlays. Unlike bindBackdropDiscard it
// does NOT dirty-track: these panels are a live listing plus a filter
// box, not an editable form, so closing them can never lose unsaved input
// and should be friction-free (no "discard?" prompt for a typed filter).
// A child .modal-overlay open ON TOP (the editor / instantiate / link modals
// these panels launch) claims the Escape / backdrop click first, so the
// child dismisses and only a subsequent gesture reaches the panel.
export function bindManageOverlayDismiss(id, closeFn) {
  const el = $('#' + id);
  if (!el) return;
  let pressedOnBackdrop = false;
  el.addEventListener('mousedown', (e) => { pressedOnBackdrop = (e.target === el); });
  el.addEventListener('click', (e) => {
    const isBackdrop = (e.target === el) && pressedOnBackdrop;
    pressedOnBackdrop = false;
    if (isBackdrop) closeFn();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!el.classList.contains('show')) return;
    // Only the front-most overlay responds to Escape. A child form modal open
    // ON TOP (the editor / instantiate / link modals these panels launch) is
    // topmost, so we yield and its own handler takes the Escape. Crucially,
    // a plain .modal-overlay open BENEATH us must NOT block us — e.g. this
    // templates panel opened over the "Form a party" group-create dialog via
    // its "⧉ manage circles…" button (JOH-356), which stays shown underneath.
    // A bare querySelector('.modal-overlay.show') guard couldn't tell "child
    // above" from "parent below" and wrongly swallowed the Escape in that case,
    // leaving the panel un-closable; the z-index/DOM-order topmost test can.
    if (!isTopmostOverlay(el)) return;
    e.preventDefault();
    // Claim the event so a later-registered sibling handler doesn't re-run for
    // the same Escape once we synchronously drop our .show and an overlay
    // beneath becomes topmost (mirrors bindBackdropDiscard's reasoning).
    e.stopImmediatePropagation();
    closeFn();
  });
}

// shutdownScope drives the group-level and whole-dashboard Shutdown
// buttons. It counts the running agents in scope from the last
// snapshot, pops a confirm modal that states the count and spells out
// that this is stop-only (no data deleted), POSTs /api/shutdown, then
// toasts the outcome summary. scope is "group" (groupName set) or
// "all" (groupName ignored).
async function shutdownScope(scope, groupName) {
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
async function powerOnScope(scope, groupName) {
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

// retireAgentInteractive runs the full per-agent retire flow shared by
// the per-row ⚙ "Retire" button and the command palette's "Retire
// agent: <name>" command: pop the retire confirm (shutdown + optional
// worktree delete), POST /api/agents/{conv}/retire with the chosen
// flags, take the dangling-entry recovery path on a 409, then toast the
// outcome and refresh. A cancelled confirm is a no-op.
async function retireAgentInteractive(conv, label) {
  // The retire work runs inside retireConfirm's `perform` so the confirm
  // modal stays up with a spinner on the OK button while the POST is in
  // flight (matching the bulk-retire preview). retireConfirm only invokes
  // perform after the human confirms, so the confirmation gate is intact;
  // close() dismisses the modal once the POST settles, before the toast or
  // the dangling-recovery modal.
  await retireConfirm({
    label, conv,
    perform: async (choice, close) => {
      const q = `?shutdown=${choice.shutdown ? 1 : 0}`
        + (choice.deleteWorktree ? '&delete_worktree=1' : '');
      let r;
      try {
        r = await fetch(`/api/agents/${encodeURIComponent(conv)}/retire${q}`, {
          method: 'POST', credentials: 'same-origin',
        });
      } catch (e) {
        close();
        toast(`Retire failed: ${(e && e.message) || e}`, true);
        return;
      }
      if (!r.ok) {
        close();
        // A dangling entry (conversation gone) can't be retired — offer to
        // remove it instead of a dead-end error toast.
        if (await maybeHandleDanglingRetire(r, conv, label)) return;
        toast(`Retire failed: ${await r.text()}`, true);
        return;
      }
      let retireResp = null;
      try { retireResp = await r.json(); } catch (_) {}
      close();
      toast(retireToast(label, choice, retireResp));
      refresh();
    },
  });
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
// Each entry is {agent_id, conv_id, title, status, role} — enough to
// render a preview row and to post the explicit selection (the submit
// leads with agent_id, falling back to conv_id for a member that has no
// stable actor id yet).
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

// countGroupMembersByStatus is the cardinality of groupMembersByStatus —
// the palette gates each per-group retire command on a non-zero count so
// it never offers a no-op sweep.
function countGroupMembersByStatus(group, status) {
  return groupMembersByStatus(group, status).length;
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
// The candidate list is snapshotted from lastSnapshot at open time and
// then OWNED by the modal: the 2s auto-refresh is suspended while a
// .modal-overlay is open and submit posts these exact conv-ids, so the
// cohort cannot shift under the human between preview and submit.
function openRetirePreview(group, status) {
  const word = RETIRE_STATUS_LABELS[status] || status;
  const candidates = groupMembersByStatus(group, status).map(m => ({ ...m, checked: true }));
  if (candidates.length === 0) {
    toast(`retire: no ${word} agents in group "${group}"`);
    return;
  }

  const overlay = $('#retire-preview-modal');
  const titleEl = $('#retire-preview-title');
  const hintEl = $('#retire-preview-hint');
  const listEl = $('#retire-preview-list');
  const countEl = $('#retire-preview-count');
  const errEl = $('#retire-preview-error');
  const searchEl = $('#retire-preview-search');
  const shutdownCb = $('#retire-preview-shutdown');
  const wtRow = $('#retire-preview-wt-row');
  const wtCb = $('#retire-preview-wt');
  const submitBtn = $('#retire-preview-submit');
  const cancelBtn = $('#retire-preview-cancel');
  const selAllBtn = $('#retire-preview-select-all');
  const selNoneBtn = $('#retire-preview-select-none');

  // Reset transient state on every open.
  errEl.textContent = '';
  searchEl.value = '';
  shutdownCb.checked = true;
  wtCb.checked = true; // worktree delete defaults ON (the BE keeps main/shared/no-worktree members)
  wtCb.disabled = false;
  wtRow.classList.remove('disabled');
  for (const c of candidates) c.checked = true;
  titleEl.textContent = `Retire ${word} agents in "${group}"`;

  // The worktree box is coupled to shutdown: a worktree is removed only
  // after its agent's pane exits, so deleting one requires shutting the
  // sessions down. Unticking shutdown disables + unticks the box (the
  // single-agent retire modal couples the same way).
  const syncWtCoupling = () => {
    if (shutdownCb.checked) {
      wtCb.disabled = false;
      wtRow.classList.remove('disabled');
    } else {
      wtCb.checked = false;
      wtCb.disabled = true;
      wtRow.classList.add('disabled');
    }
  };
  syncWtCoupling(); // reflect the (default-on) shutdown state on open

  const checkedCount = () => candidates.filter(c => c.checked).length;
  const matchesFilter = (c) => {
    const q = searchEl.value.trim().toLowerCase();
    if (!q) return true;
    return c.title.toLowerCase().includes(q) || c.conv_id.toLowerCase().includes(q);
  };

  function renderHint() {
    hintEl.textContent = `These ${word} agents in group "${group}" will be demoted to plain, `
      + `reinstatable conversations — each leaves all its groups and its grants are revoked. `
      + `Untick any you want to keep; only the ticked agents are retired.`;
  }
  function renderList() {
    const rows = candidates.filter(matchesFilter);
    if (rows.length === 0) {
      listEl.innerHTML = '<div class="cleanup-empty">no agents match the filter</div>';
      return;
    }
    listEl.innerHTML = rows.map(c => {
      const badges = `<span class="cleanup-badge">${esc(c.status)}</span>`
        + (c.role ? `<span class="cleanup-badge">${esc(c.role)}</span>` : '');
      return `<div class="cleanup-row"><label>`
        + `<input type="checkbox" data-conv="${esc(c.conv_id)}"${c.checked ? ' checked' : ''} />`
        + `<span class="title">${esc(c.title || '(untitled)')}</span>`
        + `<span class="id">${esc(c.conv_id.slice(0, 8))}</span>`
        + `${badges}</label></div>`;
    }).join('');
  }
  function renderFooter() {
    const n = checkedCount();
    countEl.textContent = `${n} of ${candidates.length} selected`;
    // textContent (not innerHTML) also clears any in-flight busy spinner —
    // renderFooter is the canonical "button reflects the selection, ready
    // for input" state, so it's where the busy state is torn down.
    submitBtn.textContent = n === 1 ? 'Retire 1 agent' : `Retire ${n} agents`;
    submitBtn.disabled = n === 0;
    submitBtn.removeAttribute('aria-busy');
  }
  function render() { renderHint(); renderList(); renderFooter(); }

  const findCandidate = (conv) => candidates.find(c => c.conv_id === conv);
  const onListChange = (e) => {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb) return;
    const c = findCandidate(cb.getAttribute('data-conv'));
    if (c) c.checked = cb.checked;
    renderFooter();
  };
  const onSearch = () => renderList();
  // select-all / select-none act on the CURRENTLY-VISIBLE rows, so under
  // an active filter "select none" only clears the rows the human can see
  // — it never silently unticks (or ticks) agents hidden by the filter.
  // With no filter, matchesFilter passes everything, so they behave as a
  // plain all/none. The global "n of N selected" count keeps the true
  // total honest even when some checked rows are filtered out of view.
  const onSelectAll = () => { for (const c of candidates.filter(matchesFilter)) c.checked = true; render(); };
  const onSelectNone = () => { for (const c of candidates.filter(matchesFilter)) c.checked = false; render(); };

  const cleanup = () => {
    overlay.classList.remove('show');
    listEl.removeEventListener('change', onListChange);
    searchEl.removeEventListener('input', onSearch);
    selAllBtn.removeEventListener('click', onSelectAll);
    selNoneBtn.removeEventListener('click', onSelectNone);
    shutdownCb.removeEventListener('change', syncWtCoupling);
    submitBtn.removeEventListener('click', onSubmit);
    cancelBtn.removeEventListener('click', cleanup);
    overlay.removeEventListener('click', onOverlay);
    document.removeEventListener('keydown', onKey);
  };
  const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
  const onKey = (e) => { if (e.key === 'Escape') cleanup(); };

  async function onSubmit() {
    // Snapshot the ticked agent-ids at click time — this is the list sent
    // to the BE verbatim, the same list the human just reviewed. We lead
    // with the stable agent_id (the BE resolves it back to the member's
    // conv-id), falling back to conv_id for a member with no actor id —
    // which also keeps a dangling agent retirable by its raw conv-id.
    const convs = candidates.filter(c => c.checked).map(c => c.agent_id || c.conv_id);
    if (convs.length === 0) return;
    // Snapshot the worktree choice too — coupled to shutdown, so a box
    // disabled by an unticked shutdown never sends delete_worktree.
    const deleteWorktree = wtCb.checked && !wtCb.disabled;
    // Busy feedback: disable + swap the label for a spinner while the POST
    // is in flight, so a click that takes a beat doesn't look ignored. The
    // busy state is torn down by renderFooter on any error path (it resets
    // textContent + clears aria-busy); on success the modal just closes.
    submitBtn.disabled = true;
    submitBtn.setAttribute('aria-busy', 'true');
    submitBtn.innerHTML = '<span class="btn-spinner" aria-hidden="true"></span>Retiring…';
    errEl.textContent = '';
    let r;
    try {
      r = await fetch(`/api/groups/${encodeURIComponent(group)}/retire`, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ convs, shutdown: shutdownCb.checked, delete_worktree: deleteWorktree }),
      });
    } catch (e) {
      errEl.textContent = `retire failed: ${(e && e.message) || e}`;
      renderFooter();
      return;
    }
    if (!r.ok) {
      errEl.textContent = await r.text();
      renderFooter();
      return;
    }
    const out = await r.json().catch(() => null);
    cleanup();
    const members = (out && out.members) || [];
    const retired = members.filter(m => m.action === 'retired').length;
    const errors = members.filter(m => m.action === 'error').length;
    let msg = `retired ${retired} agent${retired === 1 ? '' : 's'} in "${group}"`;
    if (errors) msg += `, ${errors} failed`;
    // Summarise the worktree cleanup when it was requested. Most retired
    // panes are still draining their /exit at response time, so the BE
    // reports "scheduled" (removed after the pane exits) far more often
    // than an inline "removed"; "cleaned up" covers both without
    // overstating that the dirs are already gone. The BE keeps
    // main/shared/no-worktree members, which we don't tally; a deferred
    // removal that later fails surfaces on its own in the Messages tab.
    if (deleteWorktree) {
      const wt = members.map(m => m.worktree).filter(Boolean);
      const swept = wt.filter(p => p.action === 'removed' || p.action === 'scheduled').length;
      if (swept) msg += ` · ${swept} worktree${swept === 1 ? '' : 's'} cleaned up`;
    }
    const warns = (out && out.warnings) || [];
    if (warns.length) msg += ` · ${warns.join('; ')}`;
    toast(msg, errors > 0);
    refresh();
  }

  listEl.addEventListener('change', onListChange);
  searchEl.addEventListener('input', onSearch);
  selAllBtn.addEventListener('click', onSelectAll);
  selNoneBtn.addEventListener('click', onSelectNone);
  shutdownCb.addEventListener('change', syncWtCoupling);
  submitBtn.addEventListener('click', onSubmit);
  cancelBtn.addEventListener('click', cleanup);
  overlay.addEventListener('click', onOverlay);
  document.addEventListener('keydown', onKey);

  render();
  overlay.classList.add('show');
  setTimeout(() => submitBtn.focus(), 0);
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

// countUngroupedAgents is the cardinality of ungroupedRetireCandidates —
// the palette gates the "Retire ungrouped agents…" command on a non-zero
// count so it never offers a no-op sweep.
function countUngroupedAgents() {
  return ungroupedRetireCandidates().length;
}

// openRetireUngroupedPreview runs the command palette's "Retire ungrouped
// agents…" command — the cross-group cleanup twin of the per-group
// openRetirePreview. Ungrouped agents belong to no group, so there is no
// group retire route to POST to; instead it drives the same PREVIEW modal
// (#retire-preview-modal, reused verbatim — the two flows are never open
// at once) and submits the human-approved list to the group-agnostic
// bulk cleanup endpoint (/api/cleanup/agents {mode:"retire"}):
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
// Like openRetirePreview, the candidate list is snapshotted at open time
// and OWNED by the modal: the 2s auto-refresh is suspended while a
// .modal-overlay is open, so the population can't shift between preview
// and submit.
function openRetireUngroupedPreview() {
  const candidates = ungroupedRetireCandidates().map(c => ({ ...c, checked: true }));
  if (candidates.length === 0) {
    toast('retire: no ungrouped agents to retire');
    return;
  }

  const overlay = $('#retire-preview-modal');
  const titleEl = $('#retire-preview-title');
  const hintEl = $('#retire-preview-hint');
  const listEl = $('#retire-preview-list');
  const countEl = $('#retire-preview-count');
  const errEl = $('#retire-preview-error');
  const searchEl = $('#retire-preview-search');
  const shutdownCb = $('#retire-preview-shutdown');
  const wtRow = $('#retire-preview-wt-row');
  const wtCb = $('#retire-preview-wt');
  const submitBtn = $('#retire-preview-submit');
  const cancelBtn = $('#retire-preview-cancel');
  const selAllBtn = $('#retire-preview-select-all');
  const selNoneBtn = $('#retire-preview-select-none');

  // Reset transient state on every open.
  errEl.textContent = '';
  searchEl.value = '';
  shutdownCb.checked = true;
  wtCb.checked = true; // worktree delete defaults ON (the BE keeps main/shared/no-worktree members)
  wtCb.disabled = false;
  wtRow.classList.remove('disabled');
  for (const c of candidates) c.checked = true;
  titleEl.textContent = 'Retire ungrouped agents';

  // The worktree box is coupled to shutdown: a worktree is removed only
  // after its agent's pane exits, so deleting one requires shutting the
  // sessions down. Unticking shutdown disables + unticks the box (the
  // per-group retire modal couples the same way).
  const syncWtCoupling = () => {
    if (shutdownCb.checked) {
      wtCb.disabled = false;
      wtRow.classList.remove('disabled');
    } else {
      wtCb.checked = false;
      wtCb.disabled = true;
      wtRow.classList.add('disabled');
    }
  };
  syncWtCoupling(); // reflect the (default-on) shutdown state on open

  const checkedCount = () => candidates.filter(c => c.checked).length;
  const matchesFilter = (c) => {
    const q = searchEl.value.trim().toLowerCase();
    if (!q) return true;
    return c.title.toLowerCase().includes(q) || c.conv_id.toLowerCase().includes(q);
  };

  function renderHint() {
    hintEl.textContent = 'These agents are not in any group. Each ticked agent will be demoted to a '
      + 'plain, reinstatable conversation and its grants revoked. Untick any you want to keep; '
      + 'only the ticked agents are retired.';
  }
  function renderList() {
    const rows = candidates.filter(matchesFilter);
    if (rows.length === 0) {
      listEl.innerHTML = '<div class="cleanup-empty">no agents match the filter</div>';
      return;
    }
    listEl.innerHTML = rows.map(c => {
      const badges = `<span class="cleanup-badge">${esc(c.status)}</span>`;
      return `<div class="cleanup-row"><label>`
        + `<input type="checkbox" data-conv="${esc(c.conv_id)}"${c.checked ? ' checked' : ''} />`
        + `<span class="title">${esc(c.title || '(untitled)')}</span>`
        + `<span class="id">${esc(c.conv_id.slice(0, 8))}</span>`
        + `${badges}</label></div>`;
    }).join('');
  }
  function renderFooter() {
    const n = checkedCount();
    countEl.textContent = `${n} of ${candidates.length} selected`;
    submitBtn.textContent = n === 1 ? 'Retire 1 agent' : `Retire ${n} agents`;
    submitBtn.disabled = n === 0;
    submitBtn.removeAttribute('aria-busy');
  }
  function render() { renderHint(); renderList(); renderFooter(); }

  const findCandidate = (conv) => candidates.find(c => c.conv_id === conv);
  const onListChange = (e) => {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb) return;
    const c = findCandidate(cb.getAttribute('data-conv'));
    if (c) c.checked = cb.checked;
    renderFooter();
  };
  const onSearch = () => renderList();
  // select-all / select-none act on the CURRENTLY-VISIBLE rows only, so
  // under an active filter they never silently touch hidden agents.
  const onSelectAll = () => { for (const c of candidates.filter(matchesFilter)) c.checked = true; render(); };
  const onSelectNone = () => { for (const c of candidates.filter(matchesFilter)) c.checked = false; render(); };

  const cleanup = () => {
    overlay.classList.remove('show');
    listEl.removeEventListener('change', onListChange);
    searchEl.removeEventListener('input', onSearch);
    selAllBtn.removeEventListener('click', onSelectAll);
    selNoneBtn.removeEventListener('click', onSelectNone);
    shutdownCb.removeEventListener('change', syncWtCoupling);
    submitBtn.removeEventListener('click', onSubmit);
    cancelBtn.removeEventListener('click', cleanup);
    overlay.removeEventListener('click', onOverlay);
    document.removeEventListener('keydown', onKey);
  };
  const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
  const onKey = (e) => { if (e.key === 'Escape') cleanup(); };

  async function onSubmit() {
    // Snapshot the ticked agent-ids at click time — this is the list sent
    // to the BE verbatim, the same list the human just reviewed. Lead with
    // the stable agent_id (the BE resolves it back to the conv-id), falling
    // back to conv_id for a row with no actor id yet.
    const agents = candidates.filter(c => c.checked).map(c => c.agent_id || c.conv_id);
    if (agents.length === 0) return;
    // Snapshot the worktree choice too — coupled to shutdown, so a box
    // disabled by an unticked shutdown never sends delete_worktrees.
    const deleteWorktrees = wtCb.checked && !wtCb.disabled;
    submitBtn.disabled = true;
    submitBtn.setAttribute('aria-busy', 'true');
    submitBtn.innerHTML = '<span class="btn-spinner" aria-hidden="true"></span>Retiring…';
    errEl.textContent = '';
    let r;
    try {
      // include_online: a busy ungrouped agent the human left ticked is
      // retired + soft-exited rather than skipped by the endpoint's
      // default skip-online guard.
      r = await fetch('/api/cleanup/agents', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agents, mode: 'retire', include_online: true, shutdown: shutdownCb.checked, delete_worktrees: deleteWorktrees }),
      });
    } catch (e) {
      errEl.textContent = `retire failed: ${(e && e.message) || e}`;
      renderFooter();
      return;
    }
    if (!r.ok) {
      errEl.textContent = await r.text();
      renderFooter();
      return;
    }
    const out = await r.json().catch(() => null);
    cleanup();
    const retired = (out && out.retired) || 0;
    const failed = (out && out.failed) || 0;
    const skipped = (out && out.skipped) || 0;
    let msg = `retired ${retired} ungrouped agent${retired === 1 ? '' : 's'}`;
    if (skipped) msg += `, ${skipped} skipped`;
    if (failed) msg += `, ${failed} failed`;
    // Summarise the worktree cleanup when it was requested. The retire
    // endpoint folds each agent's worktree plan into its outcome detail
    // ("worktree … removed" inline, "… will be removed after the agent
    // exits" deferred), so tally the outcomes that report a removal: the
    // detail mentions a worktree and the word "removed" (present in both
    // the inline and deferred notes). "kept"/"already gone" notes lack it,
    // and an explicit "worktree removal failed" is excluded outright so a
    // git error string that happens to contain "removed" never inflates
    // the cleaned-up count.
    if (deleteWorktrees) {
      const outcomes = (out && out.outcomes) || [];
      const swept = outcomes.filter(o => {
        const d = o.detail || '';
        return /worktree/i.test(d) && /\bremoved\b/i.test(d) && !/removal failed/i.test(d);
      }).length;
      if (swept) msg += ` · ${swept} worktree${swept === 1 ? '' : 's'} cleaned up`;
    }
    const warns = (out && out.warnings) || [];
    if (warns.length) msg += ` · ${warns.join('; ')}`;
    toast(msg, failed > 0);
    refresh();
  }

  listEl.addEventListener('change', onListChange);
  searchEl.addEventListener('input', onSearch);
  selAllBtn.addEventListener('click', onSelectAll);
  selNoneBtn.addEventListener('click', onSelectNone);
  shutdownCb.addEventListener('change', syncWtCoupling);
  submitBtn.addEventListener('click', onSubmit);
  cancelBtn.addEventListener('click', cleanup);
  overlay.addEventListener('click', onOverlay);
  document.addEventListener('keydown', onKey);

  render();
  overlay.classList.add('show');
  setTimeout(() => submitBtn.focus(), 0);
}

// openDeleteRetiredPreview is the human-driven sibling of the timed
// agent.retired_cleanup auto-sweep (JOH-269): a dashboard tool to
// PERMANENTLY DELETE retired agents in bulk. Reachable from the command
// palette and the Groups ⚙ menu, and — like openRetirePreview — it pops
// a PREVIEW modal so the human commits an EXACT list rather than a filter
// the server re-resolves:
//   1. lists every retired agent from the snapshot (global, newest-first),
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
// Like the retire preview, the candidate list is snapshotted at open time
// and OWNED by the modal: the 2s auto-refresh is suspended while a
// .modal-overlay is open, so the population can't shift between preview and
// submit. On success the editable list is swapped for the per-conv outcome
// log the cleanup endpoint returns (the result phase).
async function openDeleteRetiredPreview() {
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
  const candidates = retired
    .map(r => ({
      agent_id: r.agent_id || '',
      conv_id: r.conv_id,
      title: r.title || '',
      retired_at: r.retired_at || '',
      retired_by: r.retired_by_display || r.retired_by || '',
      // online is ~always false for a retired agent, but the BE's delete
      // tier skips a still-running session unless include_online (which
      // this modal never sends) — so flag it on the row so the rare online
      // row reads as "will be skipped" rather than silently no-op'ing.
      online: !!r.online,
      checked: true,
    }))
    // Newest retirement first — the snapshot already sorts this way, but a
    // local sort keeps the modal independent of that contract.
    .sort((a, b) => (b.retired_at || '').localeCompare(a.retired_at || ''));
  if (candidates.length === 0) {
    toast('delete retired: no retired agents');
    return;
  }

  const overlay = $('#delete-retired-modal');
  const hintEl = $('#delete-retired-hint');
  const listEl = $('#delete-retired-list');
  const countEl = $('#delete-retired-count');
  const errEl = $('#delete-retired-error');
  const searchEl = $('#delete-retired-search');
  const ageEl = $('#delete-retired-age');
  const wtCb = $('#delete-retired-wt');
  const submitBtn = $('#delete-retired-submit');
  const cancelBtn = $('#delete-retired-cancel');
  const selAllBtn = $('#delete-retired-select-all');
  const selNoneBtn = $('#delete-retired-select-none');
  let phase = 'select';

  // Reset transient state on every open.
  errEl.textContent = '';
  searchEl.value = '';
  ageEl.value = '0'; // plain show-all default (JOH-31 Q4 — not wired to after_days for v1)
  wtCb.checked = false; // worktree delete defaults OFF
  for (const c of candidates) c.checked = true;

  // ageDays — whole days since retirement. A missing / unparseable stamp
  // sorts as "infinitely old" so it always clears an age floor (an age
  // filter must never hide a row the human might still want to purge).
  const ageDays = (c) => {
    if (!c.retired_at) return Infinity;
    const t = Date.parse(c.retired_at);
    if (isNaN(t)) return Infinity;
    return (Date.now() - t) / 86400000;
  };
  const minAgeDays = () => Math.max(0, parseFloat(ageEl.value) || 0);
  // matchesFilter composes the two live filters: title/conv-id substring
  // (case-insensitive) AND the age floor. A row is "visible" iff it passes
  // BOTH.
  const matchesFilter = (c) => {
    const q = searchEl.value.trim().toLowerCase();
    if (q && !(c.title.toLowerCase().includes(q) || c.conv_id.toLowerCase().includes(q))) return false;
    // Only apply the age floor when it's positive — at 0 ("show all") a
    // future-dated retired_at (client clock skew) yields a negative age that
    // would otherwise be wrongly hidden by `age < 0`.
    const minAge = minAgeDays();
    if (minAge > 0 && ageDays(c) < minAge) return false;
    return true;
  };
  // visibleChecked is the load-bearing set (JOH-31): rows that are BOTH
  // ticked AND pass the current filters. This is what the footer counts
  // and what submit POSTs — verbatim, never re-resolved server-side.
  const visibleChecked = () => candidates.filter(c => c.checked && matchesFilter(c));

  function renderHint() {
    hintEl.textContent = 'Permanently deletes the ticked retired agents — wipes each conversation '
      + 'from disk and drops every agent / group / permission row. Only agents that are both ticked '
      + 'AND visible under the current filters are deleted. This cannot be undone.';
  }
  function renderList() {
    const rows = candidates.filter(matchesFilter);
    if (rows.length === 0) {
      listEl.innerHTML = '<div class="cleanup-empty">no retired agents match the filter</div>';
      return;
    }
    listEl.innerHTML = rows.map(c => {
      const age = c.retired_at ? `retired ${relTime(c.retired_at)}` : 'retired (unknown)';
      // An online retired agent is skipped by the BE (no include_online
      // here), so badge it so the human isn't surprised by a "skipped" row.
      const online = c.online ? '<span class="cleanup-badge online">online — will skip</span>' : '';
      const by = c.retired_by ? `<span class="cleanup-badge">by ${esc(c.retired_by)}</span>` : '';
      return `<div class="cleanup-row"><label>`
        + `<input type="checkbox" data-conv="${esc(c.conv_id)}"${c.checked ? ' checked' : ''} />`
        + `<span class="title">${esc(c.title || '(untitled)')}</span>`
        + `<span class="id">${esc(shortId(c.conv_id))}</span>`
        + `<span class="seen">${esc(age)}</span>`
        + `${online}${by}</label></div>`;
    }).join('');
  }
  function renderFooter() {
    const n = visibleChecked().length;
    countEl.textContent = `${n} of ${candidates.length} selected`;
    // textContent (not innerHTML) also clears any in-flight busy spinner —
    // renderFooter is the canonical "button reflects the selection" state,
    // so it's where the busy state is torn down on an error path.
    submitBtn.textContent = n === 1 ? 'Delete 1 agent' : `Delete ${n} agents`;
    submitBtn.disabled = n === 0;
    submitBtn.removeAttribute('aria-busy');
  }
  function render() { renderHint(); renderList(); renderFooter(); }

  const findCandidate = (conv) => candidates.find(c => c.conv_id === conv);
  const onListChange = (e) => {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb) return;
    const c = findCandidate(cb.getAttribute('data-conv'));
    if (c) c.checked = cb.checked;
    renderFooter();
  };
  const onSearch = () => { renderList(); renderFooter(); };
  const onAge = () => { renderList(); renderFooter(); };
  // select-all / select-none act on the CURRENTLY-VISIBLE rows only, so
  // under an active filter they never tick / untick agents hidden by it.
  const onSelectAll = () => { for (const c of candidates.filter(matchesFilter)) c.checked = true; render(); };
  const onSelectNone = () => { for (const c of candidates.filter(matchesFilter)) c.checked = false; render(); };

  const cleanup = () => {
    overlay.classList.remove('show');
    listEl.removeEventListener('change', onListChange);
    searchEl.removeEventListener('input', onSearch);
    ageEl.removeEventListener('input', onAge);
    selAllBtn.removeEventListener('click', onSelectAll);
    selNoneBtn.removeEventListener('click', onSelectNone);
    submitBtn.removeEventListener('click', onSubmit);
    cancelBtn.removeEventListener('click', onCancel);
    overlay.removeEventListener('click', onOverlay);
    document.removeEventListener('keydown', onKey);
    // After a completed delete the roster shrank — pull the post-delete
    // snapshot once the overlay is gone (refresh is suppressed while a
    // .modal-overlay is open).
    if (phase === 'result') refresh();
  };
  const onCancel = () => cleanup();
  const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
  const onKey = (e) => { if (e.key === 'Escape') cleanup(); };

  // renderResult swaps the modal into its read-only result phase — the
  // editable list becomes the per-conv outcome log the cleanup endpoint
  // returns, and the danger button becomes a plain "Done".
  function renderResult(resp) {
    phase = 'result';
    const outcomes = (resp && resp.outcomes) || [];
    listEl.innerHTML = outcomes.length
      ? outcomes.map(o => `<div class="cleanup-row">
          <span class="cleanup-badge ${esc(o.result)}">${esc(o.result)}</span>
          <span class="title">${esc(o.title || shortId(o.conv_id))}</span>
          <span class="id">${esc(shortId(o.conv_id))}</span>
          <span class="meta">${esc(o.detail || '')}</span>
        </div>`).join('')
      : '<div class="cleanup-empty">Nothing to do.</div>';
    const bits = [];
    if (resp && resp.deleted) bits.push(resp.deleted + ' deleted');
    if (resp && resp.skipped) bits.push(resp.skipped + ' skipped');
    if (resp && resp.failed) bits.push(resp.failed + ' failed');
    hintEl.className = 'cleanup-hint';
    hintEl.textContent = 'Delete complete — ' + (bits.join(' · ') || 'nothing to do') + '.';
    countEl.textContent = ''; // the "n of N selected" tally is meaningless once results are in
    errEl.textContent = (resp && (resp.warnings || []).length) ? '⚠ ' + resp.warnings.join('  ⚠ ') : '';
    submitBtn.textContent = 'Done';
    submitBtn.disabled = false;
    submitBtn.classList.remove('danger');
    submitBtn.removeAttribute('aria-busy');
    cancelBtn.style.display = 'none';
    // The filters + options are meaningless once results are in.
    searchEl.disabled = true; ageEl.disabled = true;
    selAllBtn.disabled = true; selNoneBtn.disabled = true; wtCb.disabled = true;
  }

  async function onSubmit() {
    if (phase === 'result') { cleanup(); return; }
    // Snapshot the ticked-AND-visible conv-ids at click time — this is the
    // list POSTed verbatim, never re-resolved server-side (JOH-31). Lead
    // with the stable agent_id (the BE maps it back to a conv-id), falling
    // back to conv_id for a row with no actor id.
    const convs = visibleChecked().map(c => c.agent_id || c.conv_id);
    if (convs.length === 0) return;
    const deleteWorktrees = wtCb.checked;
    // Busy feedback: disable + swap the label for a spinner while the POST
    // is in flight. Torn down by renderFooter on any error path.
    submitBtn.disabled = true;
    submitBtn.setAttribute('aria-busy', 'true');
    submitBtn.innerHTML = '<span class="btn-spinner" aria-hidden="true"></span>Deleting…';
    errEl.textContent = '';
    let r;
    try {
      r = await fetch('/api/cleanup/agents', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ agents: convs, mode: 'delete', delete_worktrees: deleteWorktrees }),
      });
    } catch (e) {
      errEl.textContent = `delete failed: ${(e && e.message) || e}`;
      renderFooter();
      return;
    }
    if (!r.ok) {
      errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
      renderFooter();
      return;
    }
    const out = await r.json().catch(() => null);
    renderResult(out || {});
  }

  listEl.addEventListener('change', onListChange);
  searchEl.addEventListener('input', onSearch);
  ageEl.addEventListener('input', onAge);
  selAllBtn.addEventListener('click', onSelectAll);
  selNoneBtn.addEventListener('click', onSelectNone);
  submitBtn.addEventListener('click', onSubmit);
  cancelBtn.addEventListener('click', onCancel);
  overlay.addEventListener('click', onOverlay);
  document.addEventListener('keydown', onKey);

  // Reset chrome a prior result phase may have changed.
  hintEl.className = 'cleanup-hint danger';
  cancelBtn.style.display = '';
  searchEl.disabled = false; ageEl.disabled = false;
  selAllBtn.disabled = false; selNoneBtn.disabled = false; wtCb.disabled = false;
  submitBtn.classList.add('danger');

  render();
  overlay.classList.add('show');
  setTimeout(() => submitBtn.focus(), 0);
}

// openWorktreeCleanup drives the group cog's "🧹 cleanup worktrees…"
// command — the repo-wide worktree janitor. Unlike openRetirePreview
// (which reads the cohort from lastSnapshot), this LOADS its candidates
// from the daemon: GET /api/groups/{group}/worktrees resolves the
// group's default dir (∪ its agents' history dirs) to repo root(s) and
// lists every linked worktree of those repos, each classified
// (main / live / agent / orphan) and dirty-flagged, with a smart-default
// `checked` flag — orphans pre-ticked, resume-bound and dirty ones left
// for the human to review.
//
// The modal then OWNS that list (the 2s auto-refresh is suspended while a
// .modal-overlay is open). The human edits the selection — per-row, the
// category mass-toggle chips, select-all/none over the filtered view, the
// title/path filter — and a live "⟳ rescan" re-pulls the candidate set
// (preserving rows the human manually toggled). Submit POSTs the EXACT
// ticked path list to /api/worktrees/cleanup; the daemon re-validates
// every path (skips the main repo + any live-agent worktree) and removes
// the rest — force, plus the branch when the toggle is on. Cancel / Esc /
// backdrop is a no-op.
async function openWorktreeCleanup(group) {
  const overlay = $('#worktree-cleanup-modal');
  const titleEl = $('#worktree-cleanup-title');
  const hintEl = $('#worktree-cleanup-hint');
  const catsEl = $('#worktree-cleanup-categories');
  const listEl = $('#worktree-cleanup-list');
  const countEl = $('#worktree-cleanup-count');
  const errEl = $('#worktree-cleanup-error');
  const searchEl = $('#worktree-cleanup-search');
  const branchesCb = $('#worktree-cleanup-branches');
  const submitBtn = $('#worktree-cleanup-submit');
  const cancelBtn = $('#worktree-cleanup-cancel');
  const rescanBtn = $('#worktree-cleanup-rescan');
  const selAllBtn = $('#worktree-cleanup-select-all');
  const selNoneBtn = $('#worktree-cleanup-select-none');

  let candidates = [];
  let repoRoots = [];
  // Paths the human has manually toggled — preserved across a live rescan
  // so a re-scan refreshes untouched rows to the fresh server default
  // without clobbering an explicit choice.
  const touched = new Set();

  errEl.textContent = '';
  searchEl.value = '';
  branchesCb.checked = true;
  titleEl.textContent = `Clean up worktrees in "${group}"`;
  countEl.textContent = '';
  catsEl.innerHTML = '';
  submitBtn.disabled = true;

  // The set the modal acts on excludes the main repo (never removable):
  // every selection / count / toggle below operates over removable rows.
  const removable = () => candidates.filter(c => !c.is_main);
  const checkedRows = () => removable().filter(c => c.checked);
  const matchesFilter = (c) => {
    const q = searchEl.value.trim().toLowerCase();
    if (!q) return true;
    const hay = `${c.path} ${c.branch} ${(c.agents || []).map(a => a.title).join(' ')}`.toLowerCase();
    return hay.includes(q);
  };
  const catRows = (cat) => removable().filter(c => c.category === cat);
  const dirtyRows = () => removable().filter(c => c.dirty);

  const agentLabel = (agents) => {
    if (!agents || !agents.length) return '';
    const names = agents.map(a => a.title || (a.conv_id || '').slice(0, 8));
    return 'agent: ' + names.join(', ');
  };

  function renderHint() {
    const n = removable().length;
    const where = repoRoots.length ? repoRoots.join(', ') : "this group's repo";
    hintEl.textContent = n === 0
      ? `No removable worktrees found in ${where}.`
      : `${n} removable worktree${n === 1 ? '' : 's'} in ${where}. `
        + `Orphans (no agent) and retired-agent leftovers are pre-ticked; worktrees a still-enrolled agent `
        + `uses (resume-bound) and ones with uncommitted changes are left unticked for you to review. `
        + `Only ticked worktrees are removed.`;
  }

  // Category mass-toggle chips: one per non-empty category (orphan /
  // retired / agent / live) plus a cross-cutting "uncommitted" chip. Each
  // shows checked/total and flips its whole set at once (ignoring the
  // filter — that's the point of a bulk toggle); .active marks a
  // fully-ticked set.
  function renderCats() {
    const defs = [['orphan', 'orphans'], ['retired', 'retired'], ['agent', 'agent-bound'], ['live', 'live']];
    let html = '';
    for (const [cat, label] of defs) {
      const rows = catRows(cat);
      if (!rows.length) continue;
      const on = rows.filter(c => c.checked).length;
      html += `<button type="button" data-cat="${esc(cat)}" class="${on === rows.length ? 'active' : ''}"`
        + ` title="Toggle all ${rows.length} ${esc(label)} worktrees">${esc(label)} ${on}/${rows.length}</button>`;
    }
    const dr = dirtyRows();
    if (dr.length) {
      const on = dr.filter(c => c.checked).length;
      html += `<button type="button" data-dirty="1" class="${on === dr.length ? 'active' : ''}"`
        + ` title="Toggle all ${dr.length} worktrees with uncommitted changes">uncommitted ${on}/${dr.length}</button>`;
    }
    catsEl.innerHTML = html;
    catsEl.style.display = html ? '' : 'none';
  }

  function renderList() {
    const rows = removable().filter(matchesFilter);
    // Main worktrees are shown too (disabled, for context) but never
    // filtered out by the removable() gate above — append them at the end.
    const mains = candidates.filter(c => c.is_main && matchesFilter(c));
    const all = rows.concat(mains);
    if (all.length === 0) {
      listEl.innerHTML = '<div class="cleanup-empty">no worktrees match the filter</div>';
      return;
    }
    listEl.innerHTML = all.map(c => {
      const dis = c.is_main;
      const badges = `<span class="cleanup-badge cat-${esc(c.category)}">${esc(c.category)}</span>`
        + (c.dirty ? `<span class="cleanup-badge dirty">uncommitted</span>` : '')
        + (c.agents && c.agents.length ? `<span class="cleanup-badge">${esc(agentLabel(c.agents))}</span>` : '');
      return `<div class="cleanup-row${dis ? ' disabled' : ''}" title="${esc(c.reason || '')}"><label>`
        + `<input type="checkbox" data-path="${esc(c.path)}"${c.checked ? ' checked' : ''}${dis ? ' disabled' : ''} />`
        + `<span class="branch">${esc(c.branch || '(detached)')}</span>`
        + `${badges}`
        + `<span class="path" title="${esc(c.path)}">${esc(c.path)}</span>`
        + `</label></div>`;
    }).join('');
  }

  function renderFooter() {
    const n = checkedRows().length;
    const total = removable().length;
    countEl.textContent = `${n} of ${total} selected`;
    submitBtn.textContent = n === 1 ? 'Remove 1 worktree' : `Remove ${n} worktrees`;
    submitBtn.disabled = n === 0;
    submitBtn.removeAttribute('aria-busy');
  }
  function render() { renderHint(); renderCats(); renderList(); renderFooter(); }

  const findCandidate = (p) => candidates.find(c => c.path === p);
  const onListChange = (e) => {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb) return;
    const c = findCandidate(cb.getAttribute('data-path'));
    if (c && !c.is_main) { c.checked = cb.checked; touched.add(c.path); }
    renderCats();
    renderFooter();
  };
  const onSearch = () => renderList();
  const onSelectAll = () => { for (const c of removable().filter(matchesFilter)) { c.checked = true; touched.add(c.path); } render(); };
  const onSelectNone = () => { for (const c of removable().filter(matchesFilter)) { c.checked = false; touched.add(c.path); } render(); };
  const onCats = (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    let rows = null;
    if (b.dataset.cat) rows = catRows(b.dataset.cat);
    else if (b.dataset.dirty) rows = dirtyRows();
    if (!rows) return;
    const allOn = rows.length > 0 && rows.every(c => c.checked);
    for (const c of rows) { c.checked = !allOn; touched.add(c.path); }
    render();
  };

  // load (re)pulls the candidate set. On a rescan, a row the human
  // manually toggled keeps that choice; every untouched row takes the
  // fresh server default, so the scan reflects live state (an agent that
  // went offline flips its worktree orphan→ticked) without undoing an
  // explicit opt-in/out.
  async function load(isRescan) {
    errEl.textContent = '';
    listEl.innerHTML = '<div class="cleanup-empty">scanning…</div>';
    const prev = new Map(candidates.map(c => [c.path, c.checked]));
    let r;
    try {
      r = await fetch(`/api/groups/${encodeURIComponent(group)}/worktrees`, { credentials: 'same-origin' });
    } catch (e) {
      errEl.textContent = `scan failed: ${(e && e.message) || e}`;
      listEl.innerHTML = '<div class="cleanup-empty">scan failed</div>';
      return;
    }
    if (!r.ok) {
      errEl.textContent = await r.text();
      listEl.innerHTML = '<div class="cleanup-empty">scan failed</div>';
      return;
    }
    const data = await r.json().catch(() => null);
    repoRoots = (data && data.repo_roots) || [];
    candidates = ((data && data.worktrees) || []).map(wt => {
      let checked = !!wt.checked;
      if (isRescan && touched.has(wt.path) && prev.has(wt.path)) checked = prev.get(wt.path);
      return { ...wt, checked: wt.is_main ? false : checked };
    });
    render();
  }

  const cleanup = () => {
    overlay.classList.remove('show');
    listEl.removeEventListener('change', onListChange);
    searchEl.removeEventListener('input', onSearch);
    selAllBtn.removeEventListener('click', onSelectAll);
    selNoneBtn.removeEventListener('click', onSelectNone);
    catsEl.removeEventListener('click', onCats);
    rescanBtn.removeEventListener('click', onRescan);
    submitBtn.removeEventListener('click', onSubmit);
    cancelBtn.removeEventListener('click', cleanup);
    overlay.removeEventListener('click', onOverlay);
    document.removeEventListener('keydown', onKey);
  };
  const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
  const onKey = (e) => { if (e.key === 'Escape') cleanup(); };
  const onRescan = async () => {
    rescanBtn.disabled = true;
    try { await load(true); } finally { rescanBtn.disabled = false; }
  };

  async function onSubmit() {
    const paths = checkedRows().map(c => c.path);
    if (paths.length === 0) return;
    submitBtn.disabled = true;
    submitBtn.setAttribute('aria-busy', 'true');
    submitBtn.innerHTML = '<span class="btn-spinner" aria-hidden="true"></span>Removing…';
    errEl.textContent = '';
    let r;
    try {
      r = await fetch('/api/worktrees/cleanup', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ paths, delete_branches: branchesCb.checked }),
      });
    } catch (e) {
      errEl.textContent = `cleanup failed: ${(e && e.message) || e}`;
      renderFooter();
      return;
    }
    if (!r.ok) {
      errEl.textContent = await r.text();
      renderFooter();
      return;
    }
    const out = await r.json().catch(() => null);
    cleanup();
    const removed = (out && out.removed) || 0;
    const branches = (out && out.branches) || 0;
    const skipped = (out && out.skipped) || 0;
    const failed = (out && out.failed) || 0;
    let msg = `removed ${removed} worktree${removed === 1 ? '' : 's'}`;
    if (branches) msg += ` (+${branches} branch${branches === 1 ? '' : 'es'})`;
    if (skipped) msg += `, ${skipped} skipped`;
    if (failed) msg += `, ${failed} failed`;
    toast(msg, failed > 0);
    refresh();
  }

  listEl.addEventListener('change', onListChange);
  searchEl.addEventListener('input', onSearch);
  selAllBtn.addEventListener('click', onSelectAll);
  selNoneBtn.addEventListener('click', onSelectNone);
  catsEl.addEventListener('click', onCats);
  rescanBtn.addEventListener('click', onRescan);
  submitBtn.addEventListener('click', onSubmit);
  cancelBtn.addEventListener('click', cleanup);
  overlay.addEventListener('click', onOverlay);
  document.addEventListener('keydown', onKey);

  overlay.classList.add('show');
  await load(false);
}

// openWindowModal drives the bulk window focus/unfocus feature. One
// trigger per scope — a group-level button and the top-bar button —
// opens this modal. Inside it the human picks the DIRECTION (focus
// vs unfocus) and the agent SELECTION: every running agent in scope
// is listed and ticked by default, and can be narrowed by group chip,
// by role chip, by individual checkbox, or by the text filter. Submit
// POSTs the explicit conv-id list to /api/agent-windows. The group and
// role filter rows are always present — a single group (or role) still
// earns its chip, so the common one-group dashboard can bulk-toggle by
// group.
//
// It is terminal-view-only: focus opens/raises native terminal windows, or web
// terminal panes when dashboard.default_terminal="web"; unfocus detaches the
// selected live-session clients. Neither touches an agent process — the agents
// keep running. scope is "group" (groupName set) or "all".
function openWindowModal(scope, groupName) {
  const snap = lastSnapshot || {};
  const where = scope === 'group' ? `group "${groupName}"` : 'the dashboard';
  const NO_ROLE = '(no role)';
  const NO_GROUP = '(no group)';

  // An agent's roles come from its group memberships — a top-level
  // agent row carries no role of its own, so the all-scope modal
  // collects them across every group.
  const rolesByConv = {};
  // The groups each agent belongs to — the all-scope modal's group
  // chips bucket by these. Like roles, an agent can be a member of more
  // than one group, so this is a list per conv.
  const groupsByConv = {};
  for (const g of (snap.groups || [])) {
    for (const m of (g.members || [])) {
      const gs = groupsByConv[m.conv_id] || (groupsByConv[m.conv_id] = []);
      if (!gs.includes(g.name)) gs.push(g.name);
      if (!m.role) continue;
      const rs = rolesByConv[m.conv_id] || (rolesByConv[m.conv_id] = []);
      if (!rs.includes(m.role)) rs.push(m.role);
    }
  }
  // Candidates — RUNNING agents only: an offline agent has no window
  // to focus or detach. Each carries its own `checked` flag so the
  // text filter can re-render the list without losing the selection.
  const candidates = [];
  if (scope === 'group') {
    const g = (snap.groups || []).find(x => x.name === groupName);
    for (const m of (g && g.members || [])) {
      if (!m.online) continue;
      // A group-scoped modal is already one group, so the candidate
      // carries only that group — its group filter row shows a single
      // chip for it (a redundant-but-consistent twin of select-all).
      candidates.push({ agent_id: m.agent_id || '', conv_id: m.conv_id, title: m.title || '',
        roles: m.role ? [m.role] : [], groups: [groupName], checked: true });
    }
  } else {
    for (const a of (snap.agents || [])) {
      if (!a.online) continue;
      candidates.push({ agent_id: a.agent_id || '', conv_id: a.conv_id, title: a.title || '',
        roles: rolesByConv[a.conv_id] || [], groups: groupsByConv[a.conv_id] || [],
        checked: true });
    }
  }
  if (candidates.length === 0) {
    toast(`agent windows: no running agents in ${where}`);
    return;
  }
  // roleKeys(c) — the role buckets a candidate belongs to (for the
  // chips). An agent with no role lands in the synthetic NO_ROLE
  // bucket so it stays reachable by a chip.
  const roleKeys = (c) => c.roles.length ? c.roles : [NO_ROLE];
  const allRoleKeys = [];
  for (const c of candidates) {
    for (const k of roleKeys(c)) {
      if (!allRoleKeys.includes(k)) allRoleKeys.push(k);
    }
  }
  allRoleKeys.sort((a, b) => (a === NO_ROLE) - (b === NO_ROLE) || a.localeCompare(b));

  // groupKeys(c) — the group buckets a candidate belongs to (for the
  // group chips). An ungrouped agent lands in the synthetic NO_GROUP
  // bucket so it stays reachable by a chip, mirroring NO_ROLE.
  const groupKeys = (c) => c.groups.length ? c.groups : [NO_GROUP];
  const allGroupKeys = [];
  for (const c of candidates) {
    for (const k of groupKeys(c)) {
      if (!allGroupKeys.includes(k)) allGroupKeys.push(k);
    }
  }
  allGroupKeys.sort((a, b) => (a === NO_GROUP) - (b === NO_GROUP) || a.localeCompare(b));

  const overlay = $('#window-modal');
  const hintEl = $('#window-hint');
  const groupsEl = $('#window-groups');
  const rolesEl = $('#window-roles');
  const listEl = $('#window-list');
  const countEl = $('#window-count');
  const errEl = $('#window-error');
  const searchEl = $('#window-search');
  const submitBtn = $('#window-submit');
  const cancelBtn = $('#window-cancel');
  const selAllBtn = $('#window-select-all');
  const selNoneBtn = $('#window-select-none');
  const dirRadios = overlay.querySelectorAll('input[name=window-direction]');

  // Reset transient state on every open.
  errEl.textContent = '';
  searchEl.value = '';
  for (const r of dirRadios) r.checked = (r.value === 'focus');
  for (const c of candidates) c.checked = true;

  const direction = () => {
    for (const r of dirRadios) if (r.checked) return r.value;
    return 'focus';
  };
  const checkedCount = () => candidates.filter(c => c.checked).length;
  const matchesFilter = (c) => {
    const q = searchEl.value.trim().toLowerCase();
    if (!q) return true;
    return c.title.toLowerCase().includes(q) || c.conv_id.toLowerCase().includes(q);
  };

  function renderHint() {
    // In 🧙 wizard mode the copy re-flavours to the palette's scrying-portal
    // voice (focus → Reveal, unfocus → Veil); the CSS-swapped direction labels
    // and this JS-set hint move together. Read live so a mid-modal wizard toggle
    // (the tclaude:wizard re-render below) picks the right voice.
    const wiz = isWizardActive();
    if (direction() === 'focus') {
      hintEl.textContent = wiz
        ? `Conjure a scrying portal for each chosen channeling familiar in ${where}.`
        : webTerminalDefault()
          ? `Open or focus a web terminal pane for each selected running agent in ${where}.`
          : `Open or raise a terminal window for each selected running agent in ${where}.`;
    } else {
      hintEl.textContent = wiz
        ? `Draw the veil over the chosen familiars' scrying portals in ${where} so the `
          + `desktop is decluttered. The familiars keep channeling — only the portals are dismissed.`
        : webTerminalDefault()
          ? `Detach the web terminal panes of the selected running agents in ${where}. `
            + `The agents keep running — only the terminal views are dismissed.`
          : `Detach the terminal windows of the selected running agents in ${where} so the `
            + `desktop is decluttered. The agents keep running — only the windows are dismissed.`;
    }
  }
  function renderGroups() {
    // The group filter is always shown when there's at least one agent
    // in scope — even a single group earns its chip, so a one-group
    // dashboard (the common case) can still bulk-toggle by group. The
    // synthetic NO_GROUP bucket appears whenever an ungrouped agent is
    // in scope, sorted last.
    let html = '<span class="roles-label">groups</span>';
    for (const k of allGroupKeys) {
      const inK = candidates.filter(c => groupKeys(c).includes(k));
      const on = inK.filter(c => c.checked).length;
      const cls = on === 0 ? '' : (on === inK.length ? ' on' : ' partial');
      html += `<button type="button" class="window-role-chip${cls}" data-group-chip="${esc(k)}">`
        + `${esc(k)} (${on}/${inK.length})</button>`;
    }
    groupsEl.innerHTML = html;
  }
  function renderRoles() {
    // Like the group filter, the role filter is always shown — even a
    // single role (or the synthetic NO_ROLE bucket alone) earns its
    // chip, so the two filter rows are consistently present.
    let html = '<span class="roles-label">roles</span>';
    for (const k of allRoleKeys) {
      const inK = candidates.filter(c => roleKeys(c).includes(k));
      const on = inK.filter(c => c.checked).length;
      const cls = on === 0 ? '' : (on === inK.length ? ' on' : ' partial');
      html += `<button type="button" class="window-role-chip${cls}" data-role="${esc(k)}">`
        + `${esc(k)} (${on}/${inK.length})</button>`;
    }
    rolesEl.innerHTML = html;
  }
  function renderList() {
    const rows = candidates.filter(matchesFilter);
    if (rows.length === 0) {
      listEl.innerHTML = '<div class="cleanup-empty">no agents match the filter</div>';
      return;
    }
    listEl.innerHTML = rows.map(c => {
      const badges = c.roles.map(r => `<span class="cleanup-badge">${esc(r)}</span>`).join('');
      return `<div class="cleanup-row"><label>`
        + `<input type="checkbox" data-conv="${esc(c.conv_id)}"${c.checked ? ' checked' : ''} />`
        + `<span class="title">${esc(c.title || '(untitled)')}</span>`
        + `<span class="id">${esc(c.conv_id.slice(0, 8))}</span>`
        + `${badges}</label></div>`;
    }).join('');
  }
  function renderFooter() {
    const n = checkedCount();
    countEl.textContent = `${n} of ${candidates.length} selected`;
    // The submit lever's live-count label swaps verb + noun for the theme:
    // "Focus 3 agents" → "Reveal 3 familiars" (focus) / "Veil 3 familiars"
    // (unfocus). Both nouns just take a trailing "s" in the plural.
    const wiz = isWizardActive();
    const verb = direction() === 'focus' ? (wiz ? 'Reveal' : 'Focus') : (wiz ? 'Veil' : 'Unfocus');
    const noun = wiz ? 'familiar' : 'agent';
    submitBtn.textContent = n === 1 ? `${verb} 1 ${noun}` : `${verb} ${n} ${noun}s`;
    submitBtn.disabled = n === 0;
  }
  function render() { renderHint(); renderGroups(); renderRoles(); renderList(); renderFooter(); }

  const findCandidate = (conv) => candidates.find(c => c.conv_id === conv);

  const onListChange = (e) => {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb) return;
    const c = findCandidate(cb.getAttribute('data-conv'));
    if (c) c.checked = cb.checked;
    renderGroups(); renderRoles(); renderFooter();
  };
  const onGroupsClick = (e) => {
    const chip = e.target.closest('.window-role-chip');
    if (!chip) return;
    const k = chip.getAttribute('data-group-chip');
    const inK = candidates.filter(c => groupKeys(c).includes(k));
    // Toggle: if every agent in this group is already selected, clear
    // them; otherwise select them all.
    const allOn = inK.every(c => c.checked);
    for (const c of inK) c.checked = !allOn;
    render();
  };
  const onRolesClick = (e) => {
    const chip = e.target.closest('.window-role-chip');
    if (!chip) return;
    const k = chip.getAttribute('data-role');
    const inK = candidates.filter(c => roleKeys(c).includes(k));
    // Toggle: if every agent in this role is already selected, clear
    // them; otherwise select them all.
    const allOn = inK.every(c => c.checked);
    for (const c of inK) c.checked = !allOn;
    render();
  };
  const onDirChange = () => { renderHint(); renderFooter(); };
  const onSearch = () => renderList();
  const onSelectAll = () => { for (const c of candidates) c.checked = true; render(); };
  const onSelectNone = () => { for (const c of candidates) c.checked = false; render(); };
  // A +W / palette wizard toggle while the modal is open re-skins the CSS-swapped
  // direction labels instantly; re-render so the JS-set hint + submit verb follow.
  const onWizard = () => render();

  const cleanup = () => {
    overlay.classList.remove('show');
    listEl.removeEventListener('change', onListChange);
    groupsEl.removeEventListener('click', onGroupsClick);
    rolesEl.removeEventListener('click', onRolesClick);
    for (const r of dirRadios) r.removeEventListener('change', onDirChange);
    searchEl.removeEventListener('input', onSearch);
    selAllBtn.removeEventListener('click', onSelectAll);
    selNoneBtn.removeEventListener('click', onSelectNone);
    submitBtn.removeEventListener('click', onSubmit);
    cancelBtn.removeEventListener('click', cleanup);
    overlay.removeEventListener('click', onOverlay);
    document.removeEventListener('keydown', onKey);
    document.removeEventListener('tclaude:wizard', onWizard);
  };
  const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
  const onKey = (e) => { if (e.key === 'Escape') cleanup(); };

  async function onSubmit() {
    // Lead with the stable agent_id (the BE resolves it back to the conv-id
    // the universe is keyed on), falling back to conv_id for a candidate
    // with no actor id.
    const selected = candidates.filter(c => c.checked);
    const convs = selected.map(c => c.agent_id || c.conv_id);
    if (convs.length === 0) return;
    const dir = direction();
    // The default-terminal setting applies to this bulk focus just like it does
    // to the per-agent focus action. Each helper call opens (or deduplicates and
    // focuses) one live-session pane in the dashboard's Terminals tab. Skip the
    // native-only /api/agent-windows focus path entirely: it cannot open browser
    // panes and reports only that a best-effort desktop focus was dispatched.
    // Bulk unfocus still uses the endpoint below so it reliably detaches the
    // selected tmux clients and closes their matching web panes.
    if (dir === 'focus' && webTerminalDefault()) {
      cleanup();
      for (const c of selected) {
        openWebWindowPane(c.agent_id || c.conv_id, c.title || c.conv_id.slice(0, 8));
      }
      toast(`focus web terminals: ${selected.length} focused`);
      return;
    }
    const payload = { direction: dir, scope, convs };
    if (scope === 'group') payload.group = groupName;
    submitBtn.disabled = true;
    errEl.textContent = '';
    let r;
    try {
      r = await fetch('/api/agent-windows', {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
    } catch (e) {
      errEl.textContent = `request failed: ${e && e.message || e}`;
      renderFooter();
      return;
    }
    if (!r.ok) {
      errEl.textContent = await r.text();
      renderFooter();
      return;
    }
    const out = await r.json().catch(() => null);
    cleanup();
    if (!out) { toast('agent windows: done'); return; }
    if (dir === 'focus') {
      const extra = out.failed ? `, ${out.failed} failed` : '';
      toast(`focus windows (${out.targeted} targeted): ${out.focused} focused${extra}`, out.failed > 0);
    } else {
      // Close the multiplexer panes of exactly the agents this subset unfocus
      // detached (out.agents), so their terminal tabs don't linger showing
      // "disconnected". Precise — agents outside the ticked selection are
      // untouched.
      closeTerminalsForWindowOp(out.agents);
      const parts = [`${out.detached} detached`];
      if (out.no_window) parts.push(`${out.no_window} had no window`);
      if (out.failed) parts.push(`${out.failed} failed`);
      toast(`unfocus windows (${out.targeted} targeted): ${parts.join(', ')}`, out.failed > 0);
    }
  }

  listEl.addEventListener('change', onListChange);
  groupsEl.addEventListener('click', onGroupsClick);
  rolesEl.addEventListener('click', onRolesClick);
  for (const r of dirRadios) r.addEventListener('change', onDirChange);
  searchEl.addEventListener('input', onSearch);
  selAllBtn.addEventListener('click', onSelectAll);
  selNoneBtn.addEventListener('click', onSelectNone);
  submitBtn.addEventListener('click', onSubmit);
  cancelBtn.addEventListener('click', cleanup);
  overlay.addEventListener('click', onOverlay);
  document.addEventListener('keydown', onKey);
  document.addEventListener('tclaude:wizard', onWizard);

  render();
  overlay.classList.add('show');
  setTimeout(() => submitBtn.focus(), 0);
}

function groupDeletePlan(group) {
  const snap = lastSnapshot || {};
  const groups = Array.isArray(snap.groups) ? snap.groups : [];
  const target = groups.find(g => g.name === group);
  const members = (target && Array.isArray(target.members)) ? target.members : [];
  const memberships = new Map();
  for (const g of groups) {
    for (const m of (g.members || [])) {
      const key = m.agent_id || m.conv_id;
      if (!key) continue;
      if (!memberships.has(key)) memberships.set(key, []);
      memberships.get(key).push(g.name);
    }
  }
  return members.map(m => {
    const key = m.agent_id || m.conv_id;
    const groupsForAgent = memberships.get(key) || [group];
    const otherGroups = groupsForAgent.filter(n => n !== group);
    const onlyThisGroup = otherGroups.length === 0;
    return {
      agent_id: m.agent_id || '',
      conv_id: m.conv_id || '',
      title: m.title || '',
      status: m.online ? ((m.state && m.state.status) || 'online') : 'offline',
      role: m.role || '',
      otherGroups,
      onlyThisGroup,
      checked: onlyThisGroup,
    };
  });
}

// openDeleteGroupModal is the destructive group-delete confirmation shared by
// the group cog and the drag-group-to-banish overlay. When "retire agents" is
// checked (default), members whose sole group is this one are checked for
// retirement by default; multi-group members are listed but unchecked, so they
// are detached by default and can be explicitly included when desired.
function openDeleteGroupModal(group) {
  const members = groupDeletePlan(group);
  const modalWords = () => {
    const wiz = isWizardActive();
    return {
      agent: wiz ? 'familiar' : 'agent',
      agents: wiz ? 'familiars' : 'agents',
      group: wiz ? 'party' : 'group',
      groups: wiz ? 'parties' : 'groups',
      retire: wiz ? 'banish' : 'retire',
      retired: wiz ? 'banished' : 'retired',
      retirement: wiz ? 'banishment' : 'retirement',
      deleteTitle: wiz ? 'Disband this party?' : 'Delete group',
      deleteVerb: wiz ? 'disband' : 'delete',
      deleting: wiz ? 'Disbanding' : 'Deleting',
      deleted: wiz ? 'disbanded' : 'deleted',
      retireDecision: wiz ? 'banish familiar + stop' : 'retire + stop',
    };
  };
  const overlay = $('#delete-group-modal');
  const titleEl = $('#delete-group-title');
  const hintEl = $('#delete-group-hint');
  const listEl = $('#delete-group-list');
  const countEl = $('#delete-group-count');
  const retireCb = $('#delete-group-retire');
  const errEl = $('#delete-group-error');
  const submitBtn = $('#delete-group-submit');
  const cancelBtn = $('#delete-group-cancel');
  if (!overlay || !listEl || !retireCb || !submitBtn || !cancelBtn) {
    toast('delete group: modal missing', true);
    return;
  }

  errEl.textContent = '';
  retireCb.checked = true;
  titleEl.textContent = modalWords().deleteTitle;

  const memberKey = (m) => m.agent_id || m.conv_id;
  const retireTargets = () => members.filter(m => retireCb.checked && m.checked);
  const detachTargets = () => members.filter(m => !retireCb.checked || !m.checked);

  function renderHint() {
    const w = modalWords();
    const retireN = retireTargets().length;
    const detachN = detachTargets().length;
    const bits = [];
    if (retireN) bits.push(`${retireN} ${w.retired}`);
    if (detachN) bits.push(`${detachN} detached`);
    hintEl.textContent = isWizardActive()
      ? `${w.deleting} "${group}" erases the ${w.group}, owner marks, memberships, and ${w.group} message history. `
        + `Conversation scrolls are kept. ${bits.length ? `Preview: ${bits.join(', ')}.` : `The ${w.group} has no ${w.agents}.`}`
      : `${w.deleting} "${group}" drops the ${w.group}, owner rows, memberships, and ${w.group} message history. `
        + `Conversations are kept. ${bits.length ? `Preview: ${bits.join(', ')}.` : `The ${w.group} has no ${w.agents}.`}`;
  }
  function renderList() {
    const w = modalWords();
    if (members.length === 0) {
      listEl.innerHTML = `<div class="cleanup-empty">no ${w.agents} in this ${w.group}</div>`;
      return;
    }
    listEl.innerHTML = members.map(m => {
      const willRetire = retireCb.checked && m.checked;
      const decision = willRetire ? w.retireDecision : 'detach only';
      const why = willRetire
        ? (m.otherGroups.length ? `also in ${m.otherGroups.join(', ')} — explicitly included` : `only member of this ${w.group}`)
        : m.otherGroups.length
        ? `also in ${m.otherGroups.join(', ')} — not auto-${w.retired}`
        : retireCb.checked
        ? `exempted from ${w.retirement}`
        : `${w.retire} option off`;
      const badges = `<span class="cleanup-badge">${esc(m.status)}</span>`
        + (m.role ? `<span class="cleanup-badge">${esc(m.role)}</span>` : '')
        + `<span class="cleanup-badge">${esc(decision)}</span>`;
      const checkbox = `<input type="checkbox" data-agent="${esc(memberKey(m))}"${m.checked && retireCb.checked ? ' checked' : ''}${retireCb.checked ? '' : ' disabled'} />`;
      return `<div class="cleanup-row"><label>${checkbox}`
        + `<span class="title">${esc(m.title || '(untitled)')}</span>`
        + `<span class="id">${esc((m.conv_id || '').slice(0, 8))}</span>`
        + `${badges}<span class="muted"> ${esc(why)}</span></label></div>`;
    }).join('');
  }
  function renderFooter() {
    const w = modalWords();
    const retireN = retireTargets().length;
    const detachN = detachTargets().length;
    countEl.textContent = `${members.length} ${members.length === 1 ? w.agent : w.agents}: ${retireN} to ${w.retire}, ${detachN} detach`;
    submitBtn.textContent = w.deleteTitle;
    submitBtn.disabled = false;
    submitBtn.removeAttribute('aria-busy');
  }
  function render() { renderHint(); renderList(); renderFooter(); }

  const onListChange = (e) => {
    const cb = e.target.closest('input[type=checkbox][data-agent]');
    if (!cb) return;
    const m = members.find(x => memberKey(x) === cb.getAttribute('data-agent'));
    if (m) m.checked = cb.checked;
    render();
  };
  const cleanup = () => {
    overlay.classList.remove('show');
    listEl.removeEventListener('change', onListChange);
    retireCb.removeEventListener('change', render);
    submitBtn.removeEventListener('click', onSubmit);
    cancelBtn.removeEventListener('click', cleanup);
    overlay.removeEventListener('click', onOverlay);
    document.removeEventListener('keydown', onKey);
  };
  const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
  const onKey = (e) => { if (e.key === 'Escape') cleanup(); };

  async function onSubmit() {
    const toRetire = retireTargets().map(m => m.agent_id || m.conv_id).filter(Boolean);
    submitBtn.disabled = true;
    submitBtn.setAttribute('aria-busy', 'true');
    submitBtn.innerHTML = '<span class="btn-spinner" aria-hidden="true"></span>Deleting…';
    errEl.textContent = '';

    let retired = 0;
    if (toRetire.length) {
      let rr;
      try {
        rr = await fetch(`/api/groups/${encodeURIComponent(group)}/retire`, {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ convs: toRetire, shutdown: true, delete_worktree: false }),
        });
      } catch (e) {
        errEl.textContent = `${modalWords().retire} failed: ${(e && e.message) || e}`;
        renderFooter();
        return;
      }
      if (!rr.ok) {
        errEl.textContent = await rr.text();
        renderFooter();
        return;
      }
      const out = await rr.json().catch(() => null);
      const rows = (out && out.members) || [];
      const errors = rows.filter(m => m.action === 'error').length;
      retired = rows.filter(m => m.action === 'retired').length;
      if (errors) {
        const w = modalWords();
        errEl.textContent = `${w.retire} failed for ${errors} ${errors === 1 ? w.agent : w.agents}; ${w.group} was not ${w.deleted}`;
        renderFooter();
        return;
      }
    }

    let r;
    try {
      r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
    } catch (e) {
      errEl.textContent = `${modalWords().deleteVerb} failed: ${(e && e.message) || e}`;
      renderFooter();
      return;
    }
    if (!r.ok) {
      errEl.textContent = await r.text();
      renderFooter();
      return;
    }
    const detached = members.length - retired;
    cleanup();
    const w = modalWords();
    let msg = `${w.deleted} ${w.group} "${group}"`;
    if (retired) msg += ` · ${w.retired} ${retired}`;
    if (detached) msg += ` · detached ${detached}`;
    toast(msg);
    refresh();
  }

  listEl.addEventListener('change', onListChange);
  retireCb.addEventListener('change', render);
  submitBtn.addEventListener('click', onSubmit);
  cancelBtn.addEventListener('click', cleanup);
  overlay.addEventListener('click', onOverlay);
  document.addEventListener('keydown', onKey);

  render();
  overlay.classList.add('show');
  setTimeout(() => submitBtn.focus(), 0);
}

// retireConfirm pops the retire confirmation: the same explanatory
// copy as the old confirmModal-based prompt, plus two checkboxes —
// "also shut down the running session" (checked by default) and "also
// delete the git worktree + branch" (checked by default when the agent
// has a removable worktree). The worktree status is fetched async from
// /api/agents/{conv}/worktree, mirroring deleteAgentModal: a removable
// worktree gets an enabled, checked box; a main-repo or shared one a
// disabled, greyed box explaining why it's kept; an agent with no
// worktree shows no row at all.
//
// Deleting the worktree requires the session to stop first (the agent's
// cwd IS the worktree, and removal happens only after it exits), so the
// worktree box is coupled to the shutdown box: unticking shutdown
// disables + unticks it. Resolves to {shutdown, deleteWorktree} on
// Retire, null on Cancel / outside-click / Escape. Shared by the
// per-row retire button and the drag-onto-Retired gesture so both ask
// the same question.
//
// `perform` is an optional async (choice, close) => … the modal runs when
// the human clicks Retire, WITH the modal still open and the OK button
// swapped to a spinner — the same in-flight feedback the bulk-retire
// preview gives (openRetirePreview), so a retire that takes a beat doesn't
// look ignored. perform owns the outcome (toast / refresh / dangling
// recovery) and calls the close() it is handed to dismiss the modal once
// the POST settles, before any toast or follow-up modal; a throw falls
// back to a generic error toast. While perform runs the dismiss handlers
// (Cancel / Esc / backdrop) are gated so a stray click can't resolve out
// from under the in-flight POST. When perform is omitted the OK button
// resolves the choice and closes immediately (plain-confirm contract).
function retireConfirm({label, conv, perform}) {
  return new Promise(resolve => {
    const overlay = $('#retire-modal');
    const okBtn = $('#retire-ok');
    const cancelBtn = $('#retire-cancel');
    const shutdownCb = $('#retire-shutdown');
    const wtRow = $('#retire-wt-row');
    const wtCb = $('#retire-wt');
    const wtLabel = $('#retire-wt-label');
    $('#retire-meta').textContent = label || '';
    $('#retire-meta').style.display = label ? 'block' : 'none';
    shutdownCb.checked = true; // default ON on every open
    // Worktree row hidden until the fetch tells us there is one.
    wtRow.style.display = 'none';
    wtRow.classList.remove('disabled');
    wtCb.checked = false;
    wtCb.disabled = false;

    // wtInfo is the probe result once it lands; null until then (and for
    // a main/shared/absent worktree, where deletion is never offered).
    // renderWtRow paints the row from wtInfo + the shutdown state, so it
    // is re-run whenever shutdown toggles.
    let wtInfo = null;
    const renderWtRow = () => {
      if (!wtInfo || !wtInfo.removable) return; // non-removable rows are painted once, by the probe
      const pathTxt = wtInfo.path + (wtInfo.branch ? ' · ' + wtInfo.branch : '');
      if (shutdownCb.checked) {
        wtCb.disabled = false;
        wtCb.checked = true; // default ON when removable + shutting down
        wtRow.classList.remove('disabled');
        wtLabel.innerHTML = 'Also delete the git worktree + branch '
          + `<span class="wt-note">${esc(pathTxt)} — removed after the agent exits</span>`;
      } else {
        wtCb.disabled = true;
        wtCb.checked = false;
        wtRow.classList.add('disabled');
        wtLabel.innerHTML = 'Delete the git worktree + branch '
          + `<span class="wt-note">${esc(pathTxt)} — requires shutting down the session first</span>`;
      }
    };

    // busy is set while a perform() runs: the OK button shows a spinner
    // and the dismiss handlers are gated so an Esc/backdrop/cancel can't
    // resolve the promise out from under the in-flight POST. setBusy also
    // restores the button to its idle "Retire" label, so cleanup leaves
    // the (reusable) modal clean for the next open.
    let busy = false;
    const setBusy = (on) => {
      busy = on;
      okBtn.disabled = on;
      cancelBtn.disabled = on;
      if (on) {
        okBtn.setAttribute('aria-busy', 'true');
        okBtn.innerHTML = '<span class="btn-spinner" aria-hidden="true"></span>Retiring…';
      } else {
        okBtn.removeAttribute('aria-busy');
        okBtn.textContent = 'Retire';
      }
    };

    // active guards the background worktree fetch: once the modal closes
    // (and possibly reopens for another agent) a late response must not
    // mutate the now-foreign modal DOM. cleanup is idempotent so perform's
    // close() and the onOk finally can both call it harmlessly.
    let active = true;
    const cleanup = (result) => {
      if (!active) return;
      active = false;
      setBusy(false);
      overlay.classList.remove('show');
      okBtn.removeEventListener('click', onOk);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey);
      shutdownCb.removeEventListener('change', renderWtRow);
      resolve(result);
    };
    const onOk = async () => {
      const choice = {
        shutdown: shutdownCb.checked,
        deleteWorktree: wtCb.checked && !wtCb.disabled,
      };
      // No perform → plain confirm: resolve the choice and let the caller
      // do the work (and the dismissing) itself.
      if (typeof perform !== 'function') { cleanup(choice); return; }
      // Run the work with the modal up and the OK button spinning. perform
      // calls close() to dismiss once the POST settles; cleanup() in the
      // finally is the idempotent safety net (no-op if perform closed).
      setBusy(true);
      try {
        await perform(choice, () => cleanup(choice));
      } catch (e) {
        toast(`Retire failed: ${(e && e.message) || e}`, true);
      } finally {
        cleanup(choice);
      }
    };
    const onCancel = () => { if (!busy) cleanup(null); };
    const onOverlay = (e) => { if (!busy && e.target === overlay) cleanup(null); };
    const onKey = (e) => { if (!busy && e.key === 'Escape') cleanup(null); };
    okBtn.addEventListener('click', onOk);
    cancelBtn.addEventListener('click', onCancel);
    overlay.addEventListener('click', onOverlay);
    document.addEventListener('keydown', onKey);
    shutdownCb.addEventListener('change', renderWtRow);
    overlay.classList.add('show');
    okBtn.focus();

    // Resolve the worktree in the background — the modal is already
    // usable (retire works) before this lands. If the human clicks
    // through before it resolves the worktree is simply kept, the safe
    // default. conv may be absent for callers that don't pass it; then
    // no worktree row is shown.
    if (!conv) return;
    fetch(`/api/agents/${encodeURIComponent(conv)}/worktree`, { credentials: 'same-origin' })
      .then(r => r.ok ? r.json() : null)
      .then(wt => {
        // Skip once busy too: after the human clicks Retire the choice is
        // locked and the OK button shows the spinner, so a late probe must
        // not paint the worktree row behind it.
        if (!active || busy) return;
        if (!wt || wt.kind === 'none' || !wt.path) return;
        wtInfo = wt;
        wtRow.style.display = '';
        const pathTxt = wt.path + (wt.branch ? ' · ' + wt.branch : '');
        if (wt.removable) {
          renderWtRow(); // checked/enabled, coupled to the shutdown box
        } else {
          wtCb.checked = false;
          wtCb.disabled = true;
          wtRow.classList.add('disabled');
          const why = wt.kind === 'main' ? 'the repo’s main worktree, never removed'
            : wt.shared ? 'shared with another agent'
            : 'not removable';
          wtLabel.innerHTML = 'Git worktree kept '
            + `<span class="wt-note">${esc(pathTxt)} — ${esc(why)}</span>`;
        }
      })
      .catch(() => {});
  });
}

// retireToast builds the post-retire toast from the human's choices and
// the daemon's response. The worktree outcome is reported from the
// response's `worktree.detail` (scheduled / removed / kept) rather than
// guessed, since the removal is deferred until the agent exits. Shared
// by the per-row retire button and the drag-onto-Retired gesture.
function retireToast(label, choice, resp) {
  let msg = choice.shutdown ? `retired + session stopped: ${label}` : `retired: ${label}`;
  const detail = choice.deleteWorktree && resp && resp.worktree && resp.worktree.detail;
  if (detail) msg += ` · ${detail}`;
  return msg;
}

// maybeHandleDanglingRetire inspects a FAILED retire response. The daemon
// flags a dangling agent entry — an enrollment whose conversation data is
// gone, so it can't be retired (there's nothing to demote) — with HTTP
// 409 + {dangling:true}. When it does, we pop a confirm offering to remove
// the dangling entry, and on OK purge the orphan rows via the DELETE
// endpoint (whose union cleanup is a no-op on the missing conversation but
// drops the leftover enrollment/group/permission rows). This is what
// unsticks the entry that retire alone never could.
//
// Returns true when it handled the response (dangling — the caller must
// stop and NOT surface its own retire-failed toast), false otherwise
// (the caller falls through to its normal error handling). The response
// is read via clone() so a false return leaves the caller free to read
// the original body for its error toast.
async function maybeHandleDanglingRetire(resp, conv, label) {
  if (!resp || resp.status !== 409) return false;
  let body = null;
  try { body = await resp.clone().json(); } catch (_) { return false; }
  if (!body || !body.dangling) return false;
  const confirmed = await confirmModal({
    title: 'Remove dangling agent entry?',
    body: 'No conversation data was found for this agent — its conversation is '
      + 'gone, so it can’t be retired (there’s nothing to demote). Remove the '
      + 'dangling entry instead? This purges its leftover enrollment, group '
      + 'and permission rows. It cannot be undone.',
    meta: label || conv,
    okLabel: 'Remove dangling entry',
  });
  if (!confirmed) { toast('dangling entry kept'); return true; }
  // Delete by the daemon-confirmed conv_id from the 409 body, falling
  // back to the caller's conv — they match today (dashboard rows carry
  // the full conv-id), but trusting the resolved id keeps this correct
  // if a future caller ever passes a prefix or title.
  const delConv = (body.conv_id || conv);
  const dr = await fetch(`/api/agents/${encodeURIComponent(delConv)}`, {
    method: 'DELETE', credentials: 'same-origin',
  });
  if (!dr.ok) {
    toast(`Remove failed: ${await dr.text()}`, true);
    return true;
  }
  toast(`removed dangling entry: ${label || conv}`);
  refresh();
  return true;
}

// shutdownConfirm pops a 3-button confirm: Soft exit (default),
// Force kill (destructive), Cancel. Resolves to "soft" / "force" /
// null. Mirrors the existing confirmModal but with two distinct
// confirm paths so the human can pick blast radius.
function shutdownConfirm({label}) {
  return new Promise(resolve => {
    const overlay = $('#shutdown-modal');
    $('#shutdown-meta').textContent = label || '';
    $('#shutdown-meta').style.display = label ? 'block' : 'none';
    const softBtn = $('#shutdown-soft');
    const forceBtn = $('#shutdown-force');
    const cancelBtn = $('#shutdown-cancel');
    const cleanup = (result) => {
      overlay.classList.remove('show');
      softBtn.removeEventListener('click', onSoft);
      forceBtn.removeEventListener('click', onForce);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey);
      resolve(result);
    };
    const onSoft = () => cleanup('soft');
    const onForce = () => cleanup('force');
    const onCancel = () => cleanup(null);
    const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
    const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
    softBtn.addEventListener('click', onSoft);
    forceBtn.addEventListener('click', onForce);
    cancelBtn.addEventListener('click', onCancel);
    overlay.addEventListener('click', onOverlay);
    document.addEventListener('keydown', onKey);
    overlay.classList.add('show');
    softBtn.focus();
  });
}

// termDirModal pops a 4-button picker: Current dir (default),
// Worktree dir, Launch dir, Cancel. Resolves to
// "current" / "worktree" / "start" / null. The caller POSTs the
// choice to /api/term/{conv}; the daemon opens the terminal window
// out-of-sandbox via terminal.OpenWithCommand.
function termDirModal({label}) {
  return new Promise(resolve => {
    const overlay = $('#term-modal');
    $('#term-meta').textContent = label || '';
    $('#term-meta').style.display = label ? 'block' : 'none';
    const currentBtn = $('#term-current');
    const worktreeBtn = $('#term-worktree');
    const startBtn = $('#term-start');
    const cancelBtn = $('#term-cancel');
    const cleanup = (result) => {
      overlay.classList.remove('show');
      currentBtn.removeEventListener('click', onCurrent);
      worktreeBtn.removeEventListener('click', onWorktree);
      startBtn.removeEventListener('click', onStart);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey);
      resolve(result);
    };
    const onCurrent = () => cleanup('current');
    const onWorktree = () => cleanup('worktree');
    const onStart = () => cleanup('start');
    const onCancel = () => cleanup(null);
    const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
    const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
    currentBtn.addEventListener('click', onCurrent);
    worktreeBtn.addEventListener('click', onWorktree);
    startBtn.addEventListener('click', onStart);
    cancelBtn.addEventListener('click', onCancel);
    overlay.addEventListener('click', onOverlay);
    document.addEventListener('keydown', onKey);
    overlay.classList.add('show');
    currentBtn.focus();
  });
}

// isValidRenameTitleJS mirrors the daemon's isValidRenameTitle
// (agentd/handlers.go): 1-64 chars from [A-Za-z0-9_-[]{}() ], no
// double spaces. A client-side pre-check so an obviously-bad title
// is caught in the modal instead of bouncing off a 400 — the daemon
// still enforces it authoritatively.
function isValidRenameTitleJS(t) {
  if (!t || t.length > 64) return false;
  if (t.includes('  ')) return false;
  return /^[A-Za-z0-9_\-[\]{}() ]+$/.test(t);
}

// editMemberModal is the single per-agent edit panel: conversation
// title (incl. the "auto" self-rename), group role, group description.
// Pre-filled from the current values. Resolves on Save to either
// 'noop' (nothing changed) or an object carrying only what changed:
//   - rename: {title} or {auto:true} — the conv-title edit, applied
//     by the caller via POST /api/agents/{conv}/rename.
//   - role / descr — present only when changed, applied via the
//     group-members PATCH. An unchanged field is omitted entirely.
// Resolves to null on Cancel / outside-click / Escape. Auto-refresh
// suspends while the modal is open — refreshSuspended() sees its
// .modal-overlay.show.
// editMemberModal is the single per-agent edit panel: title (incl. the
// "auto" self-rename), group role, group description, the group-owner
// toggle, and a Permissions… button that opens the permanent-permission
// editor on top. `owner` seeds the checkbox; `focusRole` lands the
// caret in the Role field (the click-to-edit role cell opens here);
// `openPerms` is the caller-supplied callback that opens the perm
// editor for this conv (passed in so refresh.js doesn't take a
// modal-message.js import). Resolves to:
//   null            — cancelled
//   'noop'          — opened, nothing changed
//   {rename?, role?, descr?, owner?} — only the fields that changed
function editMemberModal({label, title, role, descr, tags, owner, focusRole, focusDescr, openPerms}) {
  return new Promise(resolve => {
    const overlay = $('#edit-member-modal');
    $('#edit-member-meta').textContent = label || '';
    $('#edit-member-meta').style.display = label ? 'block' : 'none';
    const titleEl = $('#edit-member-title-input');
    const autoEl = $('#edit-member-auto');
    const roleEl = $('#edit-member-role');
    const ownerEl = $('#edit-member-owner');
    const descrEl = $('#edit-member-descr');
    const tagsEl = $('#edit-member-tags');
    const errEl = $('#edit-member-error');
    const permsBtn = $('#edit-member-perms');
    titleEl.value = title || '';
    titleEl.disabled = false;
    autoEl.checked = false;
    roleEl.value = role || '';
    ownerEl.checked = !!owner;
    descrEl.value = descr || '';
    tagsEl.value = tags || '';
    errEl.textContent = '';
    // The Permissions… button only makes sense for a known conv — the
    // caller wires openPerms when it has one. Hide it otherwise.
    permsBtn.style.display = openPerms ? '' : 'none';
    const saveBtn = $('#edit-member-save');
    const cancelBtn = $('#edit-member-cancel');
    // Auto and an explicit title are mutually exclusive — disable the
    // text field while auto is checked so the two paths can't be
    // ambiguous (the rename modal this folded in did the same).
    const onAuto = () => { titleEl.disabled = autoEl.checked; };
    const onPerms = () => { if (openPerms) openPerms(); };
    // Dirty tracking (mirrors bindBackdropDiscard): the fields above are
    // pre-populated programmatically, which fires neither input nor change,
    // so opening then immediately Escaping an untouched panel stays
    // friction-free. Any real edit — typing a title/description, toggling
    // auto/owner, changing the role — bubbles up to the overlay and marks
    // the form dirty, so an accidental Escape / backdrop click then asks
    // before throwing the edits away.
    let dirty = false;
    const markDirty = () => { dirty = true; };
    const cleanup = (result) => {
      overlay.classList.remove('show');
      saveBtn.removeEventListener('click', onSave);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      overlay.removeEventListener('mousedown', onMouseDown);
      overlay.removeEventListener('input', markDirty);
      overlay.removeEventListener('change', markDirty);
      autoEl.removeEventListener('change', onAuto);
      permsBtn.removeEventListener('click', onPerms);
      document.removeEventListener('keydown', onKey, true);
      resolve(result);
    };
    // tryDismiss is the ACCIDENTAL-close path (Escape / backdrop click):
    // confirm before discarding a dirty form, otherwise close. The explicit
    // Cancel button stays an instant unconditional dismiss (onCancel), same
    // split bindBackdropDiscard draws for the other data-entry modals.
    const tryDismiss = async () => {
      if (dirty && !(await confirmDiscard())) return;
      cleanup(null);
    };
    const onSave = () => {
      errEl.textContent = '';
      const out = {};
      // Title half: auto wins if checked; otherwise an explicit title
      // is sent only when it actually changed. Validated client-side
      // so a bad title is caught here, not after a 400 round-trip.
      if (autoEl.checked) {
        out.rename = {auto: true};
      } else {
        const newTitle = titleEl.value.trim();
        if (newTitle !== (title || '')) {
          if (!isValidRenameTitleJS(newTitle)) {
            errEl.textContent =
              'title must be 1-64 chars of letters, digits, space or _ - [ ] { } ( ) — no double spaces';
            return;
          }
          out.rename = {title: newTitle};
        }
      }
      // Membership half: send only fields that changed (an empty
      // value still counts as a change — it clears the field). The
      // owner toggle is a boolean — the dispatcher routes it to the
      // owners grant/revoke endpoints, separate from the role/descr
      // PATCH.
      const newRole = roleEl.value;
      const newDescr = descrEl.value;
      if (newRole !== (role || '')) out.role = newRole;
      if (newDescr !== (descr || '')) out.descr = newDescr;
      // Tags half: parse the comma-separated field into a de-duped set and
      // send the whole array (the endpoint is a replace-set) only when the
      // SET actually changed — reordering or extra whitespace alone is not
      // a change. The daemon does the real charset/length/count validation
      // and 400s a bad tag, so the panel stays lenient here.
      const newTags = parseTagsField(tagsEl.value);
      if (!sameTagSet(newTags, parseTagsField(tags || ''))) out.tags = newTags;
      if (ownerEl.checked !== !!owner) out.owner = ownerEl.checked;
      cleanup(Object.keys(out).length === 0 ? 'noop' : out);
    };
    const onCancel = () => cleanup(null);
    // Gesture guard (mirrors bindBackdropDiscard): only treat a click as a
    // backdrop dismiss when the mousedown ALSO originated on the backdrop, so
    // a text-selection drag out of the description textarea (or a scrollbar
    // drag) that releases on the backdrop doesn't dismiss the form.
    let pressedOnBackdrop = false;
    const onMouseDown = (e) => { pressedOnBackdrop = (e.target === overlay); };
    const onOverlay = (e) => {
      const isBackdrop = (e.target === overlay) && pressedOnBackdrop;
      pressedOnBackdrop = false;
      if (isBackdrop) tryDismiss();
    };
    const onKey = (e) => {
      // While the Permissions editor is stacked on top (it opens from
      // this modal's Permissions… button), it owns the keyboard — let
      // its own Esc / inputs handle the event, so an Esc up there can't
      // also tear THIS modal down underneath it. The guard MUST see the
      // perm modal still open at the instant Esc fires, so this handler
      // is registered in the CAPTURE phase (below): capture runs before
      // the perm editor's bubble-phase dismiss removes its .show, so we
      // read the true "is it stacked?" state rather than a state the
      // perm dismiss has already torn down one handler earlier.
      if ($('#perm-edit-modal').classList.contains('show')) return;
      // A discard confirm we popped is on top — let confirmModal's own
      // capture handler take the Escape (it cancels the confirm and stops
      // the event) instead of racing to pop a second confirm underneath.
      // This capture listener was registered before confirmModal's, so it
      // fires first; without this bail it would re-enter tryDismiss.
      if ($('#confirm-modal').classList.contains('show')) return;
      if (e.key === 'Escape') { e.preventDefault(); tryDismiss(); }
      // Ctrl/Cmd+Enter saves from anywhere in the modal so power
      // users don't have to mouse over to the Save button.
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault(); onSave();
      }
    };
    saveBtn.addEventListener('click', onSave);
    cancelBtn.addEventListener('click', onCancel);
    overlay.addEventListener('click', onOverlay);
    overlay.addEventListener('mousedown', onMouseDown);
    overlay.addEventListener('input', markDirty);
    overlay.addEventListener('change', markDirty);
    autoEl.addEventListener('change', onAuto);
    permsBtn.addEventListener('click', onPerms);
    // Capture phase (see onKey) so the stacked-perm-editor guard reads
    // accurate state. We never stopPropagation here — this is the
    // BOTTOM modal, so it must not swallow keys destined for overlays
    // above it.
    document.addEventListener('keydown', onKey, true);
    overlay.classList.add('show');
    // The role cell's click-to-edit lands here on the Role field; the
    // description cell's lands on the Tags field (the tags are what that
    // cell's chips show); the ⚙ "edit" button lands on Title (the broader
    // edit).
    const focusEl = focusDescr ? tagsEl : (focusRole ? roleEl : titleEl);
    focusEl.focus();
    focusEl.select();
  });
}

// parseTagsField splits a comma-separated tags input into a de-duplicated,
// order-preserving list of trimmed non-empty tags. The mirror of tagsAttr
// (helpers.js), used both to prefill-compare and to build the replace-set
// payload. Real validation (charset/length/count) is the daemon's.
function parseTagsField(value) {
  const seen = new Set();
  const out = [];
  for (const raw of String(value || '').split(',')) {
    const t = raw.trim();
    if (t && !seen.has(t)) { seen.add(t); out.push(t); }
  }
  return out;
}

// sameTagSet reports whether two tag lists are the same SET (order- and
// duplicate-insensitive) — so reordering or whitespace-only edits to the
// Tags field don't count as a change worth a write.
function sameTagSet(a, b) {
  if (a.length !== b.length) return false;
  const sb = new Set(b);
  return a.every(t => sb.has(t));
}

// addMemberModal opens an overlay anchored conceptually to a group's
// header, with a live-filtered candidate list. Returns when the user
// closes (Esc / click-outside / X). The overlay STAYS OPEN after a
// successful add — close-on-add is the pain we're fixing here.
// Uses /api/snapshot directly (no second endpoint) since both the
// ungrouped[] and agents[] arrays already ship.
function addMemberModal(groupName) {
  return new Promise(resolve => {
    const overlay = $('#add-member-modal');
    const groupLabel = $('#add-member-group');
    const search = $('#add-member-search');
    const list = $('#add-member-list');
    const includeAll = $('#add-member-all');
    groupLabel.textContent = groupName;
    search.value = '';
    includeAll.checked = false;

    // Highlighted row index (in the currently-rendered candidate
    // list). Reset when the candidate set changes; clamped on render.
    let highlight = 0;
    let candidates = [];
    // Full (un-paginated) promote pool — recent non-agent conversations.
    // Populated by the fetch below (conversations[] is windowed in the snapshot
    // now, so we can't read the full set off lastSnapshot).
    let promoteConvs = [];

    // Members already in this group — exclude from candidates so the
    // list shows ONLY rows the user can actually add. Looked up from
    // lastSnapshot once at open time + refreshed on each render so
    // a successful add immediately removes the row without waiting
    // for the 2s poll.
    function existingMembers() {
      const g = (lastSnapshot?.groups || []).find(gr => gr.name === groupName);
      return new Set((g?.members || []).map(m => m.conv_id));
    }

    // Build the candidate list from the snapshot. Default pool is
    // (agents ∪ ungrouped) — the agents list covers anyone in any
    // group, and ungrouped covers fresh-spawned online convs that
    // aren't in any group yet. With "Include offline / archived"
    // ticked, the snapshot's whole `agents` set is unioned in even
    // when its rows are offline.
    function buildCandidates() {
      const seen = new Set();
      const out = [];
      const exclude = existingMembers();
      const push = (a) => {
        if (!a || !a.conv_id) return;
        if (seen.has(a.conv_id) || exclude.has(a.conv_id)) return;
        if (!includeAll.checked && !a.online) {
          // Default pool: only currently-online convs (matches the
          // ungrouped + active-pool intuition). The "include all"
          // checkbox lifts this gate.
          // Ungrouped[] is online-only by daemon construction, but
          // agents[] can carry offline rows for previously-grouped
          // convs.
          return;
        }
        seen.add(a.conv_id);
        out.push(a);
      };
      for (const a of lastSnapshot?.ungrouped || []) push(a);
      for (const a of lastSnapshot?.agents   || []) push(a);
      // Non-agent conversations too: adding one to a group promotes
      // it to an agent (the daemon enrolls it on the membership
      // write). Tagged with _promote so the row flags the
      // side-effect. Same online-gating as everything else.
      for (const a of promoteConvs) push({ ...a, _promote: true });
      // Sort: online first, then by title.
      out.sort((a, b) => {
        if (!!b.online !== !!a.online) return (b.online ? 1 : 0) - (a.online ? 1 : 0);
        return (a.title || '').localeCompare(b.title || '');
      });
      return out;
    }

    // Pull role / descr off the per-group member row in any
    // group the agent already belongs to. Lets the search match on
    // human-meaningful fields the snapshot's `agents[]` view doesn't
    // surface (it dedupes across groups). A conv that's a member of
    // two groups uses the first-seen row.
    function memberMetaForConv(convID) {
      for (const g of lastSnapshot?.groups || []) {
        for (const m of g.members || []) {
          if (m.conv_id === convID) {
            return {role: m.role || '', descr: m.descr || ''};
          }
        }
      }
      return {role: '', descr: ''};
    }

    function applyFilter(list, q) {
      if (!q) return list;
      const needle = q.toLowerCase();
      return list.filter(a => {
        const meta = memberMetaForConv(a.conv_id);
        return ((a.title || '').toLowerCase().includes(needle)) ||
               ((a.conv_id || '').toLowerCase().includes(needle)) ||
               ((meta.role  || '').toLowerCase().includes(needle)) ||
               ((meta.descr || '').toLowerCase().includes(needle)) ||
               (a.groups || []).some(g => g.toLowerCase().includes(needle));
      });
    }

    function render() {
      candidates = applyFilter(buildCandidates(), search.value);
      if (highlight >= candidates.length) highlight = Math.max(0, candidates.length - 1);
      if (highlight < 0) highlight = 0;
      if (!candidates.length) {
        list.innerHTML = '<div class="add-member-empty">No matching conversations. ' +
          (includeAll.checked
            ? '(Try a different filter.)'
            : '(Try ticking "Include offline / archived" for a wider pool.)') +
          '</div>';
        return;
      }
      list.innerHTML = candidates.map((a, i) => {
        const meta = memberMetaForConv(a.conv_id);
        const display = a.title || '(unnamed)';
        const dot = a.online
          ? '<span class="online" title="online">●</span>'
          : '<span class="offline" title="offline">○</span>';
        const role = meta.role ? `<span class="role">${esc(meta.role)}</span>` : '';
        const descr = meta.descr ? `<span class="descr">${esc(meta.descr)}</span>` : '';
        const groups = (a.groups || []).length
          ? `<span class="groups-tag">in: ${esc((a.groups || []).join(', '))}</span>`
          : '';
        const promote = a._promote
          ? '<span class="groups-tag promote-tag" title="Not an agent yet — adding it here promotes it">promotes to agent</span>'
          : '';
        return `<div class="add-member-row${i === highlight ? ' highlighted' : ''}" data-i="${i}">` +
               `${dot}<span class="rowname">${esc(display)}</span>` +
               `<span class="id">${esc(shortId(a.conv_id))}</span>` +
               `${role}${descr}${groups}${promote}` +
               `</div>`;
      }).join('');
      // Scroll the highlighted row into view.
      const hl = list.querySelector('.add-member-row.highlighted');
      if (hl) hl.scrollIntoView({block: 'nearest'});
    }

    // conversations[] is windowed in the snapshot now; fetch the full list
    // (the /api/conversations no-param path) so the promote picker offers any
    // recent non-agent conv, not just the visible page. agents/ungrouped come
    // from the snapshot (not paginated). Re-renders when it lands.
    fetchListFull('conversations')
      .then(rows => { promoteConvs = rows; render(); })
      .catch(() => { /* keep promoteConvs empty; agents/ungrouped still offered */ });

    async function addOne(idx) {
      const cand = candidates[idx];
      if (!cand) return;
      // Hybrid picker: an agent candidate carries its rotation-immune stable
      // agent_id, a plain-conversation candidate (promoted on add) carries
      // none — send agent_id when present, conv-id otherwise. The membership
      // POST resolves either via agent.ResolveSelector (JOH-322).
      const sel = cand.agent_id || cand.conv_id;
      let r;
      try {
        r = await fetch(`/api/groups/${encodeURIComponent(groupName)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv: sel}),
        });
      } catch (e) {
        toast(`add failed: ${e && e.message || e}`, true);
        return;
      }
      if (!r.ok) {
        toast(`add failed: ${await r.text()}`, true);
        return;
      }
      const label = cand.title || cand.conv_id;
      toast(`added ${label} to ${groupName}`);
      // Optimistic local mutation: append to lastSnapshot's group so
      // the next render filters this row out without waiting for the
      // 2s poll. The poll will overwrite with the canonical state.
      const grp = (lastSnapshot?.groups || []).find(g => g.name === groupName);
      if (grp) {
        grp.members = grp.members || [];
        grp.members.push({conv_id: cand.conv_id, title: cand.title, online: cand.online});
      }
      // Re-render the dashboard groups tab so the just-added row
      // appears under the group header without a poll round-trip.
      renderGroupsTab();
      render();
    }

    const cleanup = () => {
      overlay.classList.remove('show');
      search.removeEventListener('input', onInput);
      includeAll.removeEventListener('change', onInput);
      list.removeEventListener('click', onListClick);
      list.removeEventListener('mousemove', onListMouseMove);
      document.removeEventListener('keydown', onKey, true);
      overlay.removeEventListener('click', onOverlay);
      resolve();
    };
    const onInput = () => { highlight = 0; render(); };
    const onListClick = (e) => {
      const row = e.target.closest('.add-member-row');
      if (!row) return;
      const i = parseInt(row.getAttribute('data-i'), 10);
      if (Number.isFinite(i)) addOne(i);
    };
    const onListMouseMove = (e) => {
      const row = e.target.closest('.add-member-row');
      if (!row) return;
      const i = parseInt(row.getAttribute('data-i'), 10);
      if (Number.isFinite(i) && i !== highlight) {
        highlight = i;
        render();
      }
    };
    const onKey = (e) => {
      if (e.key === 'Escape') { e.preventDefault(); cleanup(); return; }
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        if (candidates.length) { highlight = (highlight + 1) % candidates.length; render(); }
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        if (candidates.length) { highlight = (highlight - 1 + candidates.length) % candidates.length; render(); }
        return;
      }
      if (e.key === 'Enter') {
        e.preventDefault();
        if (candidates.length) addOne(highlight);
        return;
      }
    };
    const onOverlay = (e) => { if (e.target === overlay) cleanup(); };

    search.addEventListener('input', onInput);
    includeAll.addEventListener('change', onInput);
    list.addEventListener('click', onListClick);
    list.addEventListener('mousemove', onListMouseMove);
    document.addEventListener('keydown', onKey, true);
    overlay.addEventListener('click', onOverlay);
    overlay.classList.add('show');
    render();
    search.focus();
  });
}

// toast shows a transient message in the bottom-right. error=true
// makes the left border red. Auto-dismisses after 3s.
let toastTimer = null;
export function toast(message, error) {
  const el = $('#toast');
  el.textContent = message;
  el.classList.toggle('error', !!error);
  el.classList.add('show');
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), 3000);
}

// deleteAgentModal is the per-row "delete forever" confirm. Beyond
// confirm/cancel it offers an opt-in to also remove the agent's git
// worktree. The worktree's status is fetched async from
// /api/agents/{conv}/worktree: a removable worktree gets a checked,
// enabled checkbox; a main-repo or shared worktree gets a disabled,
// greyed one explaining why it's kept; an agent with no worktree
// shows no row at all. Resolves to null (cancelled) or
// {deleteWorktree: bool}.
function deleteAgentModal(conv, label) {
  return new Promise(resolve => {
    const overlay = $('#delete-agent-modal');
    const wtRow = $('#delete-agent-wt-row');
    const wtCb = $('#delete-agent-wt');
    const wtLabel = $('#delete-agent-wt-label');
    const okBtn = $('#delete-agent-ok');
    const cancelBtn = $('#delete-agent-cancel');
    $('#delete-agent-body').textContent =
      'Wipes the conversation history (.jsonl) from disk and drops every group / '
      + 'membership / ownership / permission row for this agent. This cannot be undone.';
    $('#delete-agent-meta').textContent = label || conv;
    // Worktree row hidden until the fetch tells us there is one.
    wtRow.style.display = 'none';
    wtRow.classList.remove('disabled');
    wtCb.checked = false;
    wtCb.disabled = false;

    // active guards the background worktree fetch below: once the modal
    // is closed (and possibly reopened for another agent) a late
    // response must not mutate the now-foreign modal DOM.
    let active = true;
    const cleanup = (result) => {
      active = false;
      overlay.classList.remove('show');
      okBtn.removeEventListener('click', onOk);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey);
      resolve(result);
    };
    const onOk = () => cleanup({ deleteWorktree: wtCb.checked && !wtCb.disabled });
    const onCancel = () => cleanup(null);
    const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
    const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
    okBtn.addEventListener('click', onOk);
    cancelBtn.addEventListener('click', onCancel);
    overlay.addEventListener('click', onOverlay);
    document.addEventListener('keydown', onKey);
    overlay.classList.add('show');
    okBtn.focus();

    // Resolve the worktree in the background — the modal is already
    // usable (delete works) before this lands. If the human clicks
    // through before it resolves the worktree is simply kept, the
    // safe default.
    fetch(`/api/agents/${encodeURIComponent(conv)}/worktree`, { credentials: 'same-origin' })
      .then(r => r.ok ? r.json() : null)
      .then(wt => {
        if (!active) return;
        if (!wt || wt.kind === 'none' || !wt.path) return;
        wtRow.style.display = '';
        const pathTxt = wt.path + (wt.branch ? ' · ' + wt.branch : '');
        if (wt.removable) {
          wtCb.checked = true;
          wtCb.disabled = false;
          wtRow.classList.remove('disabled');
          wtLabel.innerHTML = 'Also delete the git worktree '
            + `<span class="wt-note">${esc(pathTxt)} — directory removed, branch kept</span>`;
        } else {
          wtCb.checked = false;
          wtCb.disabled = true;
          wtRow.classList.add('disabled');
          const why = wt.kind === 'main' ? 'the repo’s main worktree, never removed'
            : wt.shared ? 'shared with another agent'
            : 'not removable';
          wtLabel.innerHTML = 'Git worktree kept '
            + `<span class="wt-note">${esc(pathTxt)} — ${esc(why)}</span>`;
        }
      })
      .catch(() => {});
  });
}

// ---- 🧹 Cleanup modal ---------------------------------------------
//
// CLEANUP_CATS — the three conversation categories the 'agents'-mode
// cleanup modal spans, in display order. Each maps to a disjoint
// snapshot list (agents / retired / conversations).
const CLEANUP_CATS = ['agent', 'retired', 'conversation'];
const CLEANUP_CAT_LABEL = {
  agent: 'Active agents', retired: 'Retired agents', conversation: 'Conversations',
};

// openCleanupModal drives the bulk-cleanup overlay. opts.mode:
//   'group'      — remove confirmed-offline members from opts.group.
//   'agents'     — the rich multi-category tool: spans all three
//                  categories (active agents, retired agents, plain
//                  conversations) with category / online / search
//                  filters and four tiers (unjoin, retire, delete,
//                  reinstate). opts.categories pre-scopes the
//                  category filter; opts.tier pre-selects the tier.
//
// The overlay builds its candidate list from the current snapshot,
// lets the human edit the include/exclude selection (and bulk-pick
// by inactivity age), POSTs the explicit conv-id list to
// /api/cleanup/… and renders the per-item result back. The daemon
// re-checks tmux liveness for every conv-id, so a conv that came
// back online between snapshot and submit is reported skipped unless
// "include online sessions" was opted into.
export async function openCleanupModal(opts) {
  const overlay = $('#cleanup-modal');
  const listEl = $('#cleanup-list');
  const optsEl = $('#cleanup-options');
  const catsEl = $('#cleanup-cats');
  const hintEl = $('#cleanup-hint');
  const warnEl = $('#cleanup-warn');
  const errEl = $('#cleanup-error');
  const countEl = $('#cleanup-count');
  const toolbar = $('#cleanup-toolbar');
  const ageInput = $('#cleanup-age');
  const searchInput = $('#cleanup-search');
  const submitBtn = $('#cleanup-submit');
  const cancelBtn = $('#cleanup-cancel');
  const mode = opts.mode;
  const groupName = opts.group || '';
  let phase = 'select';
  // multiCat — only 'agents' mode spans categories and gets
  // the category / search filters and the reinstate tier.
  const multiCat = mode === 'agents';
  // The cleanup tier: unjoin | retire | delete | reinstate.
  // 'agents' mode defaults to delete so every category is visible on
  // open (retire/reinstate would hide other categories); 'group'
  // mode always unjoins.
  let tier = multiCat ? 'delete' : 'unjoin';
  if (opts.tier) tier = opts.tier;
  // Category filter for 'agents' mode. opts.categories, when
  // supplied by a caller, pre-scopes which categories start ticked.
  const catOn = {};
  for (const k of CLEANUP_CATS) {
    catOn[k] = opts.categories ? opts.categories.includes(k) : true;
  }
  // includeOnline — opt-in that lets a tier act on still-running
  // sessions. Off by default: the offline-only safety stance.
  let includeOnline = false;
  let searchText = '';
  // 'agents' mode spans retired + conversations, which the 2s snapshot now
  // only ships a page of. Fetch the FULL lists (the endpoints' no-param path)
  // so a bulk cleanup acts on every candidate, not just the visible window.
  // agents[] is not paginated, so it still comes from the snapshot.
  let fullRetired = [];
  let fullConversations = [];
  if (mode === 'agents') {
    try {
      [fullRetired, fullConversations] = await Promise.all([
        fetchListFull('retired'),
        fetchListFull('conversations'),
      ]);
    } catch (e) {
      toast('cleanup: failed to load candidates (' + (e.message || e) + ')');
      return;
    }
  }

  // Build the candidate list from the current snapshot. Each entry
  // carries its own `checked` flag so re-renders (filter changes)
  // preserve the human's hand-tuned selection. `category` tags which
  // snapshot list it came from; `lastActivity` is the per-category
  // recency stamp (last_hook / retired_at / modified).
  function buildCandidates() {
    const out = [];
    if (mode === 'group') {
      const g = (lastSnapshot?.groups || []).find(gr => gr.name === groupName);
      for (const m of (g?.members || [])) {
        if (m.online) continue;
        out.push({
          agent_id: m.agent_id || '',
          conv_id: m.conv_id, title: m.title || '', category: 'agent',
          online: false, lastActivity: (m.state || {}).last_hook || '',
          owner: !!m.owner, groups: [],
          checked: !m.owner, // owners excluded by default
        });
      }
    } else {
      // agents mode — all three categories, online + offline alike.
      // Nothing is pre-checked: with delete as the default tier,
      // auto-selection would be a footgun.
      for (const a of (lastSnapshot?.agents || [])) {
        out.push({
          agent_id: a.agent_id || '',
          conv_id: a.conv_id, title: a.title || '', category: 'agent',
          online: !!a.online, lastActivity: (a.state || {}).last_hook || '',
          owner: !!(a.owned_groups || []).length,
          groups: a.groups || [], checked: false,
        });
      }
      for (const r of fullRetired) {
        out.push({
          agent_id: r.agent_id || '',
          conv_id: r.conv_id, title: r.title || '', category: 'retired',
          online: !!r.online, lastActivity: r.retired_at || '',
          owner: false, groups: [], checked: false,
        });
      }
      for (const c of fullConversations) {
        out.push({
          conv_id: c.conv_id, title: c.title || '', category: 'conversation',
          online: !!c.online, lastActivity: c.modified || '',
          owner: false, groups: [], checked: false,
        });
      }
    }
    // Longest-inactive first — what a human cleaning up wants at the
    // top. Missing stamp (orphan / never had a session) sorts oldest.
    out.sort((x, y) => {
      const tx = x.lastActivity ? Date.parse(x.lastActivity) : 0;
      const ty = y.lastActivity ? Date.parse(y.lastActivity) : 0;
      return tx - ty;
    });
    return out;
  }
  const candidates = buildCandidates();

  function inactivityHours(c) {
    if (!c.lastActivity) return Infinity;
    const t = Date.parse(c.lastActivity);
    if (isNaN(t)) return Infinity;
    return (Date.now() - t) / 3600000;
  }
  // activityLabel — the per-category recency line shown on each row.
  function activityLabel(c) {
    if (!c.lastActivity) return 'no recent activity';
    const rel = relTime(c.lastActivity);
    if (c.category === 'retired') return 'retired ' + rel;
    if (c.category === 'conversation') return 'last activity ' + rel;
    return 'last seen ' + rel;
  }

  // cleanupTier is the effective tier for the current mode: group
  // mode is hardwired to unjoin (it hits the single-group endpoint);
  // agents mode reads the radio-backed `tier` variable.
  function cleanupTier() {
    return mode === 'group' ? 'unjoin' : tier;
  }
  // tierCategories — which categories the current tier can act on.
  // delete is universal; reinstate is retired-only; retire / unjoin
  // are agent-only. The tier therefore doubles as a category gate.
  function tierCategories() {
    const t = cleanupTier();
    if (t === 'delete') return CLEANUP_CATS;
    if (t === 'reinstate') return ['retired'];
    return ['agent'];
  }
  function tierRadio(val, label, note) {
    return '<label><input type="radio" name="cleanup-tier" value="' + val + '"' +
      (val === tier ? ' checked' : '') + ' /> ' + label +
      ' <span class="opt-note">— ' + note + '</span></label>';
  }
  function renderOptions() {
    if (mode === 'group') {
      optsEl.innerHTML =
        '<label><input type="checkbox" id="cleanup-opt-owners" /> ' +
        'Include offline owners <span class="opt-note">— also strips their owner status</span></label>';
      return;
    }
    // agents mode: the tier selector (group mode returned above).
    // The reinstate tier has no meaning for a single-group
    // membership cleanup, so it only appears here.
    let radios =
      tierRadio('unjoin', 'Unjoin from groups',
        'stays an agent — only its group memberships are dropped') +
      tierRadio('retire', 'Retire (soft-delete)',
        'demote to a plain conversation: revokes groups + permissions, keeps the .jsonl, reinstatable') +
      tierRadio('delete', 'Delete permanently',
        'wipes the conversation from disk and every agent row — cannot be undone');
    if (multiCat) {
      radios += tierRadio('reinstate', 'Reinstate',
        'return a retired agent to the active roster — groups and permissions are not restored');
    }
    const ownersOpt =
      '<label id="cleanup-opt-owners-row"><input type="checkbox" id="cleanup-opt-owners" /> ' +
      'Include offline owners <span class="opt-note">— unjoin tier only; retire and delete drop owner rows anyway</span></label>';
    const onlineOpt = multiCat
      ? '<label id="cleanup-opt-online-row"><input type="checkbox" id="cleanup-opt-online"' +
        (includeOnline ? ' checked' : '') + ' /> ' +
        'Include online sessions <span class="opt-note">— also act on conversations whose tmux ' +
        'session is still running. Delete force-stops them first; retire / unjoin leave the process ' +
        'running. Reinstate ignores liveness either way.</span></label>'
      : '';
    const wtOpt =
      '<label id="cleanup-opt-wt-row"><input type="checkbox" id="cleanup-opt-wt" checked /> ' +
      'Delete associated git worktrees <span class="opt-note">— removes the worktree directory; ' +
      'the branch and its commits are kept. The main repo and worktrees shared with another ' +
      'agent are always skipped.</span></label>';
    const shutdownOpt =
      '<label id="cleanup-opt-shutdown-row"><input type="checkbox" id="cleanup-opt-shutdown" checked /> ' +
      'Also shut down running sessions <span class="opt-note">— retire tier only; soft-exits ' +
      '(/exit) the tmux pane of each retired agent that is still running. The conversation is ' +
      'kept and reinstatable either way.</span></label>';
    optsEl.innerHTML =
      '<div class="cleanup-tier">' + radios + '</div>' +
      ownersOpt + onlineOpt + shutdownOpt + wtOpt;
    syncTierLocks();
  }
  // syncTierLocks enables each sub-option only for the tier it
  // applies to: owners → unjoin, worktrees → delete, shutdown →
  // retire, include-online → every tier except reinstate (which
  // ignores liveness).
  function syncTierLocks() {
    if (mode === 'group') return;
    const tr = cleanupTier();
    const lock = (id, rowId, enabledWhen) => {
      const cb = $(id), row = $(rowId);
      if (!cb || !row) return;
      cb.disabled = !enabledWhen;
      row.classList.toggle('disabled', !enabledWhen);
    };
    lock('#cleanup-opt-owners', '#cleanup-opt-owners-row', tr === 'unjoin');
    lock('#cleanup-opt-wt', '#cleanup-opt-wt-row', tr === 'delete');
    lock('#cleanup-opt-shutdown', '#cleanup-opt-shutdown-row', tr === 'retire');
    lock('#cleanup-opt-online', '#cleanup-opt-online-row', tr !== 'reinstate');
  }
  function optInclOwners() {
    const cb = $('#cleanup-opt-owners');
    return !!(cb && cb.checked && !cb.disabled);
  }
  function optDeleteWorktrees() {
    const cb = $('#cleanup-opt-wt');
    return !!(cb && cb.checked && !cb.disabled);
  }
  function optIncludeOnline() {
    const cb = $('#cleanup-opt-online');
    return !!(cb && cb.checked && !cb.disabled);
  }
  function optShutdown() {
    const cb = $('#cleanup-opt-shutdown');
    return !!(cb && cb.checked && !cb.disabled);
  }

  // matchesSearch / rowVisible / rowEnabled compose the filter
  // pipeline. A row is visible when it passes the search box, the
  // category checkboxes, the current tier's category gate and the
  // online filter; it is selectable when, additionally, it is not a
  // locked group-mode owner row.
  function matchesSearch(c) {
    if (!searchText) return true;
    const q = searchText.toLowerCase();
    return (c.title || '').toLowerCase().includes(q) ||
           c.conv_id.toLowerCase().includes(q);
  }
  function rowVisible(c) {
    if (!matchesSearch(c)) return false;
    if (!multiCat) return true;
    if (!catOn[c.category]) return false;
    if (!tierCategories().includes(c.category)) return false;
    // Online rows are hidden unless opted in — except under
    // reinstate, which is non-destructive and ignores liveness.
    if (c.online && !includeOnline && cleanupTier() !== 'reinstate') return false;
    return true;
  }
  function rowEnabled(c) {
    if (mode === 'group' && c.owner) return optInclOwners();
    return true;
  }
  // selected() only counts rows the human can currently see — a row
  // checked then hidden by a filter change is not submitted.
  function selected() {
    return candidates.filter(c => rowVisible(c) && rowEnabled(c) && c.checked);
  }

  // renderCategories draws the category-filter row ('agents' mode only).
  function renderCategories() {
    if (!multiCat) { catsEl.style.display = 'none'; return; }
    catsEl.style.display = '';
    catsEl.innerHTML = '<span class="cleanup-cats-label">categories:</span>' +
      CLEANUP_CATS.map(cat => {
        const n = candidates.filter(c => c.category === cat).length;
        return `<label class="cleanup-cat-toggle">
          <input type="checkbox" data-cat="${cat}"${catOn[cat] ? ' checked' : ''} />
          ${esc(CLEANUP_CAT_LABEL[cat])} <span class="muted">(${n})</span>
        </label>`;
      }).join('');
  }

  function rowHTML(c) {
    const enabled = rowEnabled(c);
    const checked = enabled && c.checked;
    const ownerBadge = c.owner ? '<span class="cleanup-badge owner">owner</span>' : '';
    const onlineBadge = c.online ? '<span class="cleanup-badge online">online</span>' : '';
    const metaText = (c.groups && c.groups.length) ? 'in: ' + c.groups.join(', ') : '';
    return `<div class="cleanup-row${enabled ? '' : ' disabled'}">
      <label>
        <input type="checkbox" data-conv="${esc(c.conv_id)}"${checked ? ' checked' : ''}${enabled ? '' : ' disabled'} />
        <span class="title">${esc(c.title || shortId(c.conv_id))}</span>
        <span class="id">${esc(shortId(c.conv_id))}</span>
        ${ownerBadge}${onlineBadge}
        <span class="meta">${esc(metaText)}</span>
        <span class="seen">${esc(activityLabel(c))}</span>
      </label>
    </div>`;
  }
  function renderList() {
    const vis = candidates.filter(rowVisible);
    if (!vis.length) {
      listEl.innerHTML = '<div class="cleanup-empty">' +
        (candidates.length ? 'No conversations match the current filters.'
                           : 'Nothing to clean up.') + '</div>';
      return;
    }
    if (!multiCat) {
      listEl.innerHTML = vis.map(rowHTML).join('');
      return;
    }
    // 'agents' mode: group the visible rows under category sub-headers.
    let html = '';
    for (const cat of CLEANUP_CATS) {
      const rows = vis.filter(c => c.category === cat);
      if (!rows.length) continue;
      html += `<div class="cleanup-cat-head">${esc(CLEANUP_CAT_LABEL[cat])} <span>(${rows.length})</span></div>`;
      html += rows.map(rowHTML).join('');
    }
    listEl.innerHTML = html;
  }

  function recompute() {
    const n = selected().length;
    const tr = cleanupTier();
    countEl.textContent = n + ' selected';
    let label;
    if (mode === 'group') {
      label = n ? `Remove ${n} from ${groupName}` : 'Remove from group';
    } else if (tr === 'delete') {
      label = n ? `Delete ${n} conversation${n === 1 ? '' : 's'} permanently` : 'Delete conversations';
    } else if (tr === 'retire') {
      label = n ? `Retire ${n} agent${n === 1 ? '' : 's'}` : 'Retire agents';
    } else if (tr === 'reinstate') {
      label = n ? `Reinstate ${n} agent${n === 1 ? '' : 's'}` : 'Reinstate agents';
    } else {
      label = n ? `Remove ${n} agent${n === 1 ? '' : 's'} from all groups` : 'Remove from groups';
    }
    submitBtn.textContent = label;
    submitBtn.disabled = n === 0;
    submitBtn.classList.toggle('danger', tr === 'delete');
    applyHint();
  }

  // Bulk-select every visible, selectable row whose inactivity meets
  // the age threshold (0 h selects all visible). A convenience on top
  // of the per-row checkboxes the human can still hand-tune.
  function applyAge() {
    const h = Math.max(0, parseFloat(ageInput.value) || 0);
    for (const c of candidates) {
      if (!rowVisible(c) || !rowEnabled(c)) continue;
      c.checked = inactivityHours(c) >= h;
    }
    renderList();
    recompute();
  }

  function applyChrome() {
    const titleEl = $('#cleanup-title');
    if (mode === 'group') {
      titleEl.textContent = '🧹 Clean up group: ' + groupName;
    } else {
      titleEl.textContent = '🧹 Clean up agents and conversations';
    }
    applyHint();
  }
  // applyHint sets the modal's explanatory line. For group mode it is
  // static; otherwise it tracks the selected tier so the human always
  // sees exactly what the action will do.
  function applyHint() {
    if (phase === 'result') return;
    if (mode === 'group') {
      hintEl.className = 'cleanup-hint';
      hintEl.textContent = 'Removes the selected confirmed-offline members from this group. '
        + 'Their conversations keep running and stay on disk — only the membership is dropped. '
        + 'Owners are excluded unless you opt in below.';
      return;
    }
    const tr = cleanupTier();
    if (tr === 'delete') {
      hintEl.className = 'cleanup-hint danger';
      hintEl.textContent = 'Permanently deletes the selected conversations — wipes the history from '
        + 'disk and drops every group / owner / permission row. Works on active agents, retired '
        + 'agents and plain conversations alike. Cannot be undone.';
    } else if (tr === 'retire') {
      hintEl.className = 'cleanup-hint';
      hintEl.textContent = 'Retires the selected agents: revokes their group memberships and '
        + 'permission grants so they stop being agents — the conversations stay on disk and can '
        + 'be reinstated later. The non-destructive soft-delete. Running sessions are also '
        + 'soft-stopped unless you untick the option below.';
    } else if (tr === 'reinstate') {
      hintEl.className = 'cleanup-hint';
      hintEl.textContent = 'Reinstates the selected retired agents — returns them to the active '
        + 'roster. Their former groups and permissions are not restored; they start fresh.';
    } else {
      hintEl.className = 'cleanup-hint';
      hintEl.textContent = 'Removes the selected agents from every group they belong to. '
        + 'They stay agents (and stay on disk) — only the group memberships are dropped.';
    }
  }

  async function submit() {
    // Lead with the stable agent_id (the BE's resolveCleanupConv maps it
    // back to a conv-id); a plain conversation has no actor id, so it falls
    // back to conv_id — which also keeps a dangling agent reachable by its
    // raw conv-id.
    const picks = selected().map(c => c.agent_id || c.conv_id);
    if (!picks.length) return;
    errEl.textContent = '';
    submitBtn.disabled = true;
    let url, payload;
    if (mode === 'group') {
      url = '/api/cleanup/group';
      payload = { group: groupName, members: picks, include_owners: optInclOwners() };
    } else {
      url = '/api/cleanup/agents';
      payload = {
        agents: picks,
        mode: cleanupTier(),
        include_owners: optInclOwners(),
        include_online: optIncludeOnline(),
        delete_worktrees: optDeleteWorktrees(),
        shutdown: optShutdown(),
      };
    }
    try {
      const r = await fetch(url, {
        method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!r.ok) {
        errEl.textContent = (await r.text()) || ('HTTP ' + r.status);
        recompute();
        return;
      }
      renderResult(await r.json());
    } catch (err) {
      errEl.textContent = 'Request failed: ' + (err && err.message || err);
      recompute();
    }
  }

  // renderResult swaps the modal into its read-only result phase:
  // the editable list becomes a per-conv outcome log, the action
  // button becomes "Done".
  function renderResult(resp) {
    phase = 'result';
    toolbar.style.display = 'none';
    optsEl.style.display = 'none';
    catsEl.style.display = 'none';
    const outcomes = resp.outcomes || [];
    listEl.innerHTML = outcomes.length
      ? outcomes.map(o => `<div class="cleanup-row">
          <span class="cleanup-badge ${esc(o.result)}">${esc(o.result)}</span>
          <span class="title">${esc(o.title || shortId(o.conv_id))}</span>
          <span class="id">${esc(shortId(o.conv_id))}</span>
          <span class="meta">${esc(o.detail || '')}</span>
        </div>`).join('')
      : '<div class="cleanup-empty">Nothing to do.</div>';
    const bits = [];
    if (resp.removed) bits.push(resp.removed + ' removed');
    if (resp.retired) bits.push(resp.retired + ' retired');
    if (resp.reinstated) bits.push(resp.reinstated + ' reinstated');
    if (resp.deleted) bits.push(resp.deleted + ' deleted');
    if (resp.skipped) bits.push(resp.skipped + ' skipped');
    if (resp.failed) bits.push(resp.failed + ' failed');
    hintEl.className = 'cleanup-hint';
    hintEl.textContent = 'Cleanup complete — ' + (bits.join(' · ') || 'nothing to do') + '.';
    if ((resp.warnings || []).length) {
      warnEl.style.display = 'block';
      warnEl.textContent = '⚠ ' + resp.warnings.join('\n⚠ ');
    }
    errEl.textContent = '';
    submitBtn.textContent = 'Done';
    submitBtn.disabled = false;
    submitBtn.classList.remove('danger');
    cancelBtn.style.display = 'none';
  }

  function close() {
    overlay.classList.remove('show');
    cancelBtn.removeEventListener('click', onCancel);
    submitBtn.removeEventListener('click', onSubmit);
    overlay.removeEventListener('click', onOverlay);
    document.removeEventListener('keydown', onKey);
    $('#cleanup-select-all').removeEventListener('click', onSelectAll);
    $('#cleanup-select-none').removeEventListener('click', onSelectNone);
    ageInput.removeEventListener('input', applyAge);
    searchInput.removeEventListener('input', onSearch);
    catsEl.removeEventListener('change', onCatChange);
    optsEl.removeEventListener('change', onOptChange);
    listEl.removeEventListener('change', onListChange);
    // refresh() belongs here, not in submit(): submit() runs while the
    // overlay still carries .show, so refreshSuspended() would drop the
    // re-render. After a completed cleanup (phase === 'result') the
    // dashboard needs the post-cleanup snapshot — refresh once the
    // overlay is gone.
    if (phase === 'result') refresh();
  }
  function onCancel() { close(); }
  function onSubmit() { if (phase === 'result') close(); else submit(); }
  function onOverlay(e) { if (e.target === overlay) close(); }
  function onKey(e) { if (e.key === 'Escape') close(); }
  function onSelectAll() {
    for (const c of candidates) { if (rowVisible(c) && rowEnabled(c)) c.checked = true; }
    renderList(); recompute();
  }
  function onSelectNone() {
    for (const c of candidates) c.checked = false;
    renderList(); recompute();
  }
  function onSearch() {
    searchText = searchInput.value.trim();
    renderList(); recompute();
  }
  function onCatChange(e) {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb) return;
    catOn[cb.getAttribute('data-cat')] = cb.checked;
    renderList(); recompute();
  }
  function onOptChange(e) {
    // Group mode: toggling "include owners" unlocks owner rows and
    // pre-selects them (the human can still hand-uncheck any).
    if (e.target.id === 'cleanup-opt-owners' && mode === 'group') {
      for (const c of candidates) { if (c.owner) c.checked = e.target.checked; }
    }
    // "Include online sessions" toggled — reveal / hide online rows.
    if (e.target.id === 'cleanup-opt-online') {
      includeOnline = e.target.checked;
    }
    // agents mode: a tier radio changed — update the tier and
    // re-lock the dependent sub-options.
    if (e.target.name === 'cleanup-tier') {
      tier = e.target.value;
      syncTierLocks();
    }
    renderList();
    recompute();
  }
  function onListChange(e) {
    const cb = e.target.closest('input[type=checkbox]');
    if (!cb) return;
    const c = candidates.find(x => x.conv_id === cb.getAttribute('data-conv'));
    if (c) c.checked = cb.checked;
    recompute();
  }

  // Reset chrome — a prior result-phase render may have hidden bits.
  toolbar.style.display = '';
  optsEl.style.display = '';
  cancelBtn.style.display = '';
  cancelBtn.textContent = 'Cancel';
  warnEl.style.display = 'none';
  warnEl.textContent = '';
  errEl.textContent = '';
  ageInput.value = '0';
  searchInput.value = '';
  searchInput.style.display = multiCat ? '' : 'none';
  submitBtn.classList.remove('danger');

  applyChrome();
  renderOptions();
  renderCategories();
  renderList();
  recompute();

  cancelBtn.addEventListener('click', onCancel);
  submitBtn.addEventListener('click', onSubmit);
  overlay.addEventListener('click', onOverlay);
  document.addEventListener('keydown', onKey);
  $('#cleanup-select-all').addEventListener('click', onSelectAll);
  $('#cleanup-select-none').addEventListener('click', onSelectNone);
  ageInput.addEventListener('input', applyAge);
  searchInput.addEventListener('input', onSearch);
  catsEl.addEventListener('change', onCatChange);
  optsEl.addEventListener('change', onOptChange);
  listEl.addEventListener('change', onListChange);
  overlay.classList.add('show');
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
async function resumeAgentReq(conv, label, recreate) {
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
  toast(`wake ${label}: ${out.action || 'ok'}`);
  refresh();
  return true;
}

// stopAgentReq POSTs the stop endpoint with the given blast radius
// (force=false → soft /exit, force=true → tmux kill), toasts the
// outcome, and refreshes on success. Driven by the online status-dot
// click (via the 3-way shutdown confirm). Returns true on success.
async function stopAgentReq(conv, label, force) {
  let r;
  try {
    r = await fetch(`/api/agents/${encodeURIComponent(conv)}/stop`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({force: !!force}),
    });
  } catch (e) {
    toast(`shutdown failed: ${e && e.message || e}`, true);
    return false;
  }
  if (!r.ok) {
    toast(`shutdown failed: ${await r.text()}`, true);
    return false;
  }
  try {
    const out = await r.json();
    toast(`shutdown ${label}: ${out.action || 'ok'}`);
  } catch (_) {
    toast(`shutdown ${label}: ok`);
  }
  refresh();
  return true;
}

export {
  bindFilter, bindTabs, bindTabHotkeys, bindAccessSubtabs, bindDetailsPersistence, bindGroupTitleToggle, bindGroupQuickHover, bindSortHeaders,
  shutdownScope, powerOnScope, openWindowModal, retireConfirm, retireToast, shutdownConfirm,
  maybeHandleDanglingRetire, retireAgentInteractive, openRetirePreview, openRetireUngroupedPreview, openDeleteRetiredPreview, openWorktreeCleanup,
  openDeleteGroupModal,
  groupMembersByStatus, countGroupMembersByStatus, countUngroupedAgents,
  termDirModal, editMemberModal, addMemberModal, deleteAgentModal,
  resumeAgentReq, stopAgentReq,
};
