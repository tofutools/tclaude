// refresh.js — the auto-refresh loop plus tab, reconciliation, and navigation
// wiring. Operator-initiated requests and transaction launchers live in
// dashboard-operations.js.
//
// Extracted from dashboard.js in the Stage 2 module split. refresh() is
// the 2-second snapshot poll that re-renders every tab.

import { $, $$, isModifiedClick } from './helpers.js';
import { dashPrefs } from './prefs.js';
import {
  fetchVisibleGroupListPages, syncServedOffset,
} from './list-paging.js';
import { recordGroupInteraction } from './last-group.js';
import {
  renderDashDefaultProfile, renderDashSandboxProfile,
} from './toolbar-profile-renderers.js';
import { focusNextMessagesAttention, renderMailTab, renderAccessRequests } from './mail-bridge.js';
import { renderGroupsTab, renderLinksTab } from './tabs.js';
import { renderTemplatesTab } from './modal-templates.js';
import { applyProcessesTabVisibility } from './processes.js';
import { renderDock } from './dock.js';
// lastSnapshot is dashboard.js's shared state — read directly, written via the
// setLastSnapshot setter. All deliberate, benign cycles are TDZ-safe —
// no top-level code reads a cyclic import.
import { reconcileTerminalsForAgentRoster } from './terminals-tab.js';
import { lastSnapshot, setLastSnapshot } from './dashboard.js';
import { setVegasRegularMode } from './slop.js';
import { setHScrollFollow } from './hscroll.js';
import { noteConnected, noteDisconnected } from './connection.js';
import { syncDashDefaultProfile } from './profiles.js';
import { dashboardState } from './snapshot-store.js';
import { featureState } from './feature-state-registry.js';
import {
  showShellStatus as showStatus,
  shellToast as toast,
  shellConfirm as confirmModal,
  shellConfirmDiscard as confirmDiscard,
} from './shell-state.js';
import { disclosurePreference } from './group-tree-activity.js';

// groupsTabActive reports whether the Groups tab is the visible one — used to
// skip the (default-hidden, expensive) conversations/replaced sub-fetches when
// their virtual group can't be on screen anyway.
function groupsTabActive() {
  const s = $('#tab-groups');
  return !!s && s.classList.contains('active');
}

export async function refresh() {
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
    // Two gates keep this cheap: (1) all three lists are fetched only on the
    // Groups tab and only while their default-hidden virtual group is visible.
    // The palette's cross-tab delete-retired count comes from
    // snapshot.retired_total, so it does not need the retired roster poll.
    // (2) the Groups filter box value rides along as the server-side `q` so the
    // filter searches the WHOLE list, not just the loaded page.
    //
    // List sub-fetches swallow a network rejection (→ null) so a blip on one
    // degrades to "keep the previous rows" (stitchListPage) rather than failing
    // the tick. The snapshot fetch keeps its original behaviour — its network
    // error rejects to the outer catch.
    const groups = featureState('groups');
    const groupsQ = (groups?.query.value || '').trim();
    const onGroups = groupsTabActive();
    // The Jobs tab's unified table (exports + cron) is windowed the same way —
    // fetched only while its tab is showing; the nav badge stays live off the
    // snapshot's export_jobs_active count regardless.
    const get = (path) => fetch(path, { credentials: 'same-origin' }).catch(() => null);
    const staticVersion = lastSnapshot?.static_version || '';
    const [retiredRequest, convRequest, replacedRequest] = fetchVisibleGroupListPages(
      groups, onGroups, groupsQ, get,
    );
    const [snapR, retiredR, convR, replacedR, jobsR] = await Promise.all([
      fetch('/api/snapshot' + (staticVersion
        ? '?static_version=' + encodeURIComponent(staticVersion)
        : ''), { credentials: 'same-origin', cache: 'no-store' }),
      retiredRequest,
      convRequest,
      replacedRequest,
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
    // Stitch each windowed list onto the snapshot so the downstream renderers
    // keep reading data.retired / .conversations / .replaced unchanged.
    // data.paging carries each list's {offset,limit,total,total_unfiltered} for
    // the pagers + count summaries. A failed OR gated-off (undefined) sub-fetch
    // keeps the previous tick's rows for that list — a blip / a collapsed group
    // must not blank a section.
    const prevSnap = lastSnapshot || {};
    if (data.static_unchanged && prevSnap.static_version === data.static_version) {
      for (const key of ['slugs', 'templates', 'profiles', 'roles', 'plugins_catalog']) {
        data[key] = prevSnap[key];
      }
    }
    data.paging = {};
    await stitchListPage(data, 'retired', retiredR, prevSnap);
    await stitchListPage(data, 'conversations', convR, prevSnap);
    await stitchListPage(data, 'replaced', replacedR, prevSnap);
    const jobsResult = await stitchListPage(data, 'jobs', jobsR, prevSnap);
    // stitchListPage awaited resp.json() (async boundaries) — re-check the
    // request before mutating shared offset state and the DOM.
    if (!dashboardState.isCurrentRequest(requestId)) {
      jobs?.discardRequest(requestId);
      return;
    }
    if (jobsActive && !jobs.acceptsRequest(requestId)) {
      dashboardState.discardRequest(requestId, { responded });
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
    // Reconcile BEFORE lastSnapshot is replaced or any renderer can throw.
    // The terminal module keeps its own last-authoritative roster baseline, so
    // a degraded snapshot cannot close panes or consume a later retirement.
    reconcileTerminalsForAgentRoster(data.agents, data.agent_roster_authoritative);
    setLastSnapshot(data);
    syncDashDefaultProfile(data.spawn_profile_default);
    renderGroupsTab();
    renderTemplatesTab();
    // Publish the shared snapshot into the keyed Preact-owned palette dock.
    renderDock();
    renderLinksTab();
    applyProcessesTabVisibility(data);
    applyDebugTabVisibility(data);
    renderMailTab();
    renderAccessRequests(data.access_requests || [], data.access_requests_pending || 0);
    renderDashDefaultProfile();
    renderDashSandboxProfile();
    setVegasRegularMode(!!data.vegas_in_regular_mode);
    // Horizontal-scroll chrome-bar mode (config dashboard.hscroll_follow,
    // default follow) — replaces the old per-browser header toggle button.
    setHScrollFollow(data.hscroll_follow !== false);
    // Group quick-options fold mode (config dashboard.group_quick_options,
    // default "hover"). body.group-quick-fold drives the CSS horizontal
    // accordion: the editable chips in each group <summary> collapse to
    // icon-only at rest and expand on header hover. "expanded" keeps them
    // full. A plain class toggle, like hide-slop-lever below — the native
    // Groups tree already reconciled its rows this same tick, so the
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
    // Publish only after every renderer has succeeded. Signal subscribers
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

let tabsCleanup = null;
function bindTabs() {
  if (tabsCleanup) return tabsCleanup;
  const bindings = [];
  $$('nav [data-tab]').forEach(b => {
    const onClick = e => {
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
      const changed = dashboardState.setActiveTab(b.dataset.tab);
      // A badge on Messages means there is operator work waiting. Put that
      // work under the cursor instead of merely restoring the last-opened
      // mailbox. The Messages controller applies access-before-notification
      // priority and oldest-first ordering; explicit mailbox deep links
      // suppress this one-shot shortcut in mail-bridge.js.
      if (b.dataset.tab === 'messages') focusNextMessagesAttention(lastSnapshot);
      if (!changed) {
        document.dispatchEvent(new CustomEvent('tclaude:tab-reselected', { detail: { tab: b.dataset.tab } }));
      }
      if (b.dataset.tab === 'jobs') void refresh();
    };
    b.addEventListener('click', onClick);
    bindings.push([b, 'click', onClick]);
    // <a> activates on Enter only, whereas the former <button> also switched on
    // Space; restore that parity so a keyboard user's Space still selects the
    // focused tab (preventDefault stops the page from scrolling instead). The
    // synthetic click routes through the handler above. Vegas is a real
    // <button> — Space fires its click natively — so skip it to avoid a
    // double toggle.
    if (b.tagName === 'A') {
      const onKeyDown = e => {
        if (e.key !== ' ' && e.key !== 'Spacebar') return;
        e.preventDefault();
        b.click();
      };
      b.addEventListener('keydown', onKeyDown);
      bindings.push([b, 'keydown', onKeyDown]);
    }
  });
  const cleanup = () => {
    for (const [target, type, listener] of bindings) target.removeEventListener(type, listener);
    if (tabsCleanup === cleanup) tabsCleanup = null;
  };
  tabsCleanup = cleanup;
  return cleanup;
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
let tabHotkeysCleanup = null;
function bindTabHotkeys() {
  if (tabHotkeysCleanup) return tabHotkeysCleanup;
  const onKeyDown = e => {
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
  };
  document.addEventListener('keydown', onKeyDown);
  const cleanup = () => {
    document.removeEventListener('keydown', onKeyDown);
    if (tabHotkeysCleanup === cleanup) tabHotkeysCleanup = null;
  };
  tabHotkeysCleanup = cleanup;
  return cleanup;
}

// applyDebugTabVisibility drives the Debug tab's auto-hide off the
// server's debug_tab_visible flag (config dashboard.show_debug_tab,
// TCL-376), mirroring applyPluginsTabVisibility: the tab is a
// maintainer/troubleshooting surface, hidden by default to keep the nav
// tight. Display-only — the daemon records poll timings and serves
// /api/perf regardless, so history exists from before the toggle. If
// the Debug tab is the active one when it gets hidden (the human just
// turned the opt-in off in the Config tab), fall back to Groups so they
// aren't stranded on a now-invisible section.
function applyDebugTabVisibility(data) {
  const visible = !!(data && data.debug_tab_visible);
  document.body.classList.toggle('hide-debug', !visible);
  if (!visible) {
    const sec = document.getElementById('tab-debug');
    if (sec && sec.classList.contains('active')) {
      $$('nav [data-tab]').forEach(b => b.classList.toggle('active', b.dataset.tab === 'groups'));
      $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-groups'));
      dashboardState.setActiveTab('groups');
    }
  }
}
// activateAccessSubtab selects one of the Access tab's sub-views
// (permissions / slugs / sudo) by toggling .active on the matching
// segmented-control button and its panel. Exported so deep links (the
// 🔓 sudo-manage badge) can jump straight to a sub-view.
function activateAccessSubtab(name) {
  featureState('access')?.setSubtab(name);
  // Tell the history router the location changed (→ /access/<sub>). One-way
  // event so refresh.js doesn't import nav-history.js; nav-history records it as
  // user navigation (no-op during its own programmatic restore). See
  // nav-history.js recordCurrentLocation.
  document.dispatchEvent(new CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'access', subtab: name } },
  }));
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
const groupDisclosureIntents = new Set();

// noteGroupDisclosureIntent marks the next native toggle for one group as a
// real command rather than reconciliation noise. Title clicks call it here;
// the command palette calls the exported boundary before assigning .open.
export function noteGroupDisclosureIntent(key) {
  if (key) groupDisclosureIntents.add(key);
}

let groupTitleToggleCleanup = null;
function bindGroupTitleToggle() {
  if (groupTitleToggleCleanup) return groupTitleToggleCleanup;
  const root = $('#groups-list');
  if (!root) return () => {};
  const onClick = e => {
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
      const key = details.getAttribute('data-group-key');
      noteGroupDisclosureIntent(key);
      recordGroupInteraction(key);
      return;
    }
    if (e.target.closest('.group-name')) {
      // Genuine mouse fold/unfold of the group title — remember it.
      const key = details.getAttribute('data-group-key');
      noteGroupDisclosureIntent(key);
      recordGroupInteraction(key);
      return; // the title — allow toggle
    }
    e.preventDefault();
  };
  root.addEventListener('click', onClick, true);
  const cleanup = () => {
    root.removeEventListener('click', onClick, true);
    if (groupTitleToggleCleanup === cleanup) groupTitleToggleCleanup = null;
  };
  groupTitleToggleCleanup = cleanup;
  return cleanup;
}

// <details> only fires `toggle` on the element itself (not bubbling), so use a
// capturing listener on the stable Groups host rather than re-binding keyed
// details elements after every render.
let detailsPersistenceCleanup = null;
function bindDetailsPersistence() {
  if (detailsPersistenceCleanup) return detailsPersistenceCleanup;
  const root = $('#groups-list');
  if (!root) return () => {};
  const onToggle = e => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement)) return;
    const key = d.getAttribute('data-group-key');
    if (!key) return;
    const previous = dashPrefs.getItem('tclaude.dash.group.' + key);
    const intentional = groupDisclosureIntents.delete(key);
    const next = disclosurePreference(d.open, intentional, previous);
    if (next === null) {
      dashPrefs.removeItem('tclaude.dash.group.' + key);
    } else {
      dashPrefs.setItem('tclaude.dash.group.' + key, next);
    }
    // Folding changes which visible group header owns descendant activity.
    // Recompute immediately instead of leaving the bot counts stale until the
    // next two-second snapshot poll.
    // Re-created <details> nodes can emit a no-op toggle during reconciliation;
    // avoid turning that into a second render. A genuine disclosure change
    // necessarily changes the persisted open preference.
    const changed = next === null ? previous !== null : previous !== next;
    if (changed) featureState('groups')?.rerender();
  };
  root.addEventListener('toggle', onToggle, true);
  const cleanup = () => {
    root.removeEventListener('toggle', onToggle, true);
    if (detailsPersistenceCleanup === cleanup) detailsPersistenceCleanup = null;
  };
  detailsPersistenceCleanup = cleanup;
  return cleanup;
}


export {
  bindTabs, bindTabHotkeys, bindDetailsPersistence, bindGroupTitleToggle,
  toast, confirmModal, confirmDiscard,
};
