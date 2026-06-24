// refresh.js — the auto-refresh loop, tab / copy / sort wiring, the
// shared confirm / window / cleanup / agent-action modals, and toast.
//
// Extracted from dashboard.js in the Stage 2 module split. refresh() is
// the 2-second snapshot poll that re-renders every tab.

import { $, $$, esc, shortId, relTime, captureFocus, restoreFocus } from './helpers.js';
import { cycleSort } from './sort.js';
import { dashPrefs } from './prefs.js';
import { recordGroupInteraction } from './last-group.js';
import {
  renderPermissions, renderSlugs, showStatus,
  renderMessagesBadge, renderUsage, renderDashDefaultProfile,
  renderNotifyGlobal,
} from './render.js';
import { renderMailTab, onMailSearchChanged } from './mail.js';
import {
  renderGroupsTab, renderCronTab, renderSudoTab, renderLinksTab,
} from './tabs.js';
import { renderTemplatesTab } from './modal-templates.js';
import { renderPluginsTab, renderPluginsBadge } from './plugins.js';
// renameEditing (row-actions.js) and dndDragActive (dnd.js) are owned by
// their feature modules; refreshSuspended() only reads them. lastSnapshot
// is dashboard.js's shared state — read directly, written via the
// setLastSnapshot setter (two writers: refresh() here, and the
// row-actions rename-rollback). All deliberate, benign cycles (see
// render.js): TDZ-safe — no top-level code reads a cyclic import.
import { renameEditing } from './row-actions.js';
import { dndDragActive } from './dnd.js';
import { groupReorderActive } from './group-reorder.js';
import { lastSnapshot, setLastSnapshot } from './dashboard.js';
import { setVegasRegularMode } from './slop.js';

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
function refreshSuspended() {
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
  // Any modal overlay is open.
  if (document.querySelector('.modal-overlay.show')) return true;
  // A ⚙ options menu is open — re-rendering the Groups tab would
  // rebuild the row/group and collapse the menu out from under the
  // pointer. Closing the menu drops the .open class, lifting this.
  if (document.querySelector('.action-menu.open')) return true;
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
    if (tab === 'groups') renderGroupsTab();
    else if (tab === 'templates') renderTemplatesTab();
    else if (tab === 'cron') renderCronTab();
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
  // Optional ▾ view popover (groups tab only) — collapses the four
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
    // first three default ON (showing everything); 'conversations'
    // defaults OFF since there are usually many. Edit BOTH places
    // together if the defaults ever change.
    const viewDefaults = {
      [`filter-${tab}-offline`]: true,
      [`filter-${tab}-ungrouped`]: true,
      [`filter-${tab}-retired`]: true,
      [`filter-${tab}-conversations`]: false,
    };
    const updateViewBadge = () => {
      let n = 0;
      for (const [id, want] of Object.entries(viewDefaults)) {
        const el = document.getElementById(id);
        if (el && el.checked !== want) n++;
      }
      if (n === 0) {
        viewBadge.hidden = true;
        viewBadge.textContent = '';
      } else {
        viewBadge.hidden = false;
        viewBadge.textContent = String(n);
      }
    };
    updateViewBadge();
    // change bubbles up from the contained inputs, so one listener on
    // the popover covers all four. The per-checkbox handlers above
    // already persist + rerender; this only refreshes the badge.
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

// Focus preservation across the 2s re-render lives in helpers.js
// (captureFocus / restoreFocus / withPreservedFocus) — shared with
// mail.js, which wraps its own async mail repaint the same way. refresh()
// spreads capture and restore apart by hand below because they straddle
// the non-render snapshot bookkeeping; a single-call wrapper wouldn't fit.
export async function refresh() {
  if (refreshSuspended()) {
    // An inline-edit input, a modal, or a drag is in progress;
    // re-rendering now would blow the input away mid-keystroke,
    // disrupt the modal, or detach the dragged row. Skip this tick —
    // the commit / cancel / dragend handlers each re-trigger
    // refresh() once the user is done.
    return;
  }
  try {
    const r = await fetch('/api/snapshot', { credentials: 'same-origin' });
    if (!r.ok) {
      showStatus('snapshot failed: HTTP ' + r.status, true);
      return;
    }
    const data = await r.json();
    // The guard above was sampled BEFORE the fetch. A drag or a modal
    // may have opened while it was in flight — re-check now, before
    // touching the DOM. Bailing here (ahead of the lastSnapshot
    // assignment) also preserves any optimistic drag mutation already
    // applied to the old snapshot; the drag/modal teardown re-runs
    // refresh() when it finishes.
    if (refreshSuspended()) return;
    // Snapshot the keyboard focus before the renders below replace the
    // tab bodies wholesale, so a Tab-navigating user isn't bounced to
    // the top of the page on every poll. Restored at the end once the
    // fresh DOM is in place.
    const focusToken = captureFocus();
    setLastSnapshot(data);
    $('#meta').textContent = data.popup_base + ' · refreshed ' + new Date(data.generated_at).toLocaleTimeString();
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
    renderTemplatesTab();
    renderCronTab();
    renderSudoTab();
    renderLinksTab();
    renderPluginsTab();
    renderPluginsBadge(data.plugins_warn || 0);
    // Permissions + Slug registry now live as sub-panels of the merged
    // "Access" tab; the renderers write into the per-panel mount divs.
    $('#permissions-body').innerHTML = renderPermissions(data.permissions, data.agents);
    $('#slugs-body').innerHTML = renderSlugs(data.slugs);
    renderMailTab();
    renderMessagesBadge(data.messages_unread || 0);
    renderUsage(data.usage);
    renderDashDefaultProfile();
    renderNotifyGlobal(!!data.notifications_enabled);
    applyCostTabVisibility(data);
    setVegasRegularMode(!!data.vegas_in_regular_mode);
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
  } catch (e) {
    showStatus('snapshot failed: ' + (e.message || e), true);
  }
}

function bindTabs() {
  $$('nav button').forEach(b => {
    b.addEventListener('click', () => {
      $$('nav button').forEach(x => x.classList.toggle('active', x === b));
      $$('main section').forEach(s => {
        s.classList.toggle('active', s.id === 'tab-' + b.dataset.tab);
      });
    });
  });
}

// visibleTabButtons returns the nav tab buttons that are actually on
// screen, in DOM (left-to-right) order. offsetParent === null means a
// display:none somewhere up the chain — which is exactly how the Vegas
// tab (hidden unless body.slop) and the Costs tab (hidden via
// body.hide-costs) drop out. Checking visibility instead of naming those
// two keeps the cycler correct if more conditional tabs appear later.
function visibleTabButtons() {
  return $$('nav button[data-tab]').filter(b => b.offsetParent !== null);
}

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
  tabs[next].click();
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
      $$('nav button').forEach(b => b.classList.toggle('active', b.dataset.tab === 'groups'));
      $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-groups'));
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
    const btn = e.target.closest('button[data-subtab]');
    if (!btn) return;
    activateAccessSubtab(btn.dataset.subtab);
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
}

// showAccessTab brings the top-level Access tab forward and (optionally)
// selects a sub-view. Used by the sudo-manage deep link so a click on an
// agent's 🔓 badge lands on the Sudo sub-view pre-filtered to that agent.
export function showAccessTab(subtab) {
  $$('nav button').forEach(x => x.classList.toggle('active', x.dataset.tab === 'access'));
  $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-access'));
  if (subtab) activateAccessSubtab(subtab);
}

function bindCopy() {
  document.addEventListener('click', e => {
    const t = e.target.closest('[data-copy]');
    if (!t) return;
    const cmd = t.getAttribute('data-copy');
    navigator.clipboard?.writeText(cmd).then(() => {
      const orig = t.textContent;
      t.textContent = '✓ copied: ' + cmd;
      setTimeout(() => { t.textContent = orig; }, 1200);
    }).catch(() => {});
  });
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
// bindCopy / bindDetailsPersistence). Clicking re-renders just the
// affected tab so the new ordering — and the header arrow — show
// immediately, without waiting for the next poll.
function bindSortHeaders() {
  document.addEventListener('click', e => {
    const th = e.target.closest('th[data-sort-table]');
    if (!th) return;
    const tableKey = th.dataset.sortTable;
    cycleSort(tableKey, th.dataset.sortCol);
    if (tableKey === 'members') renderGroupsTab();
    else if (tableKey === 'cron') renderCronTab();
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
export function confirmModal({title, body, meta, okLabel}) {
  return new Promise(resolve => {
    const overlay = $('#confirm-modal');
    $('#confirm-title').textContent = title;
    $('#confirm-body').textContent = body;
    $('#confirm-meta').textContent = meta || '';
    $('#confirm-meta').style.display = meta ? 'block' : 'none';
    const okBtn = $('#confirm-ok');
    okBtn.textContent = okLabel || 'Confirm';
    const cancelBtn = $('#confirm-cancel');
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
// to invoke once the user confirms (or the modal is clean).

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

export function bindBackdropDiscard(modalId, closeFn) {
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
    if (dirty) {
      const ok = await confirmModal({
        title: 'Discard input?',
        body: 'Closing the form will discard any unsaved input. Continue?',
        okLabel: 'Discard',
      });
      if (!ok) return;
    }
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
}

// bindManageOverlayDismiss wires backdrop-click + Escape close for the
// Templates… / Links… management overlays. Unlike bindBackdropDiscard it
// does NOT dirty-track: these panels are a live listing plus a filter
// box, not an editable form, so closing them can never lose unsaved input
// and should be friction-free (no "discard?" prompt for a typed filter).
// Both paths no-op while any child .modal-overlay (the editor /
// instantiate / link modals these panels launch) is open on top, so a
// backdrop click or Escape dismisses the child first and only reaches the
// panel once the child is gone.
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
    // A child form modal sits on top — let its own handler take the Escape.
    if (document.querySelector('.modal-overlay.show')) return;
    e.preventDefault();
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
// It is window-only: focus opens/raises terminal windows, unfocus
// detaches them. Neither touches an agent process — the agents keep
// running. scope is "group" (groupName set) or "all".
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
      candidates.push({ conv_id: m.conv_id, title: m.title || '',
        roles: m.role ? [m.role] : [], groups: [groupName], checked: true });
    }
  } else {
    for (const a of (snap.agents || [])) {
      if (!a.online) continue;
      candidates.push({ conv_id: a.conv_id, title: a.title || '',
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
    hintEl.textContent = direction() === 'focus'
      ? `Open or raise a terminal window for each selected running agent in ${where}.`
      : `Detach the terminal windows of the selected running agents in ${where} so the `
        + `desktop is decluttered. The agents keep running — only the windows are dismissed.`;
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
    const verb = direction() === 'focus' ? 'Focus' : 'Unfocus';
    submitBtn.textContent = n === 1 ? `${verb} 1 agent` : `${verb} ${n} agents`;
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
  };
  const onOverlay = (e) => { if (e.target === overlay) cleanup(); };
  const onKey = (e) => { if (e.key === 'Escape') cleanup(); };

  async function onSubmit() {
    const convs = candidates.filter(c => c.checked).map(c => c.conv_id);
    if (convs.length === 0) return;
    const dir = direction();
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
function retireConfirm({label, conv}) {
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

    // active guards the background worktree fetch: once the modal closes
    // (and possibly reopens for another agent) a late response must not
    // mutate the now-foreign modal DOM.
    let active = true;
    const cleanup = (result) => {
      active = false;
      overlay.classList.remove('show');
      okBtn.removeEventListener('click', onOk);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      document.removeEventListener('keydown', onKey);
      shutdownCb.removeEventListener('change', renderWtRow);
      resolve(result);
    };
    const onOk = () => cleanup({
      shutdown: shutdownCb.checked,
      deleteWorktree: wtCb.checked && !wtCb.disabled,
    });
    const onCancel = () => cleanup(null);
    const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
    const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
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
        if (!active) return;
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
function editMemberModal({label, title, role, descr, owner, focusRole, openPerms}) {
  return new Promise(resolve => {
    const overlay = $('#edit-member-modal');
    $('#edit-member-meta').textContent = label || '';
    $('#edit-member-meta').style.display = label ? 'block' : 'none';
    const titleEl = $('#edit-member-title-input');
    const autoEl = $('#edit-member-auto');
    const roleEl = $('#edit-member-role');
    const ownerEl = $('#edit-member-owner');
    const descrEl = $('#edit-member-descr');
    const errEl = $('#edit-member-error');
    const permsBtn = $('#edit-member-perms');
    titleEl.value = title || '';
    titleEl.disabled = false;
    autoEl.checked = false;
    roleEl.value = role || '';
    ownerEl.checked = !!owner;
    descrEl.value = descr || '';
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
    const cleanup = (result) => {
      overlay.classList.remove('show');
      saveBtn.removeEventListener('click', onSave);
      cancelBtn.removeEventListener('click', onCancel);
      overlay.removeEventListener('click', onOverlay);
      autoEl.removeEventListener('change', onAuto);
      permsBtn.removeEventListener('click', onPerms);
      document.removeEventListener('keydown', onKey, true);
      resolve(result);
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
      if (ownerEl.checked !== !!owner) out.owner = ownerEl.checked;
      cleanup(Object.keys(out).length === 0 ? 'noop' : out);
    };
    const onCancel = () => cleanup(null);
    const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
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
      if (e.key === 'Escape') { e.preventDefault(); cleanup(null); }
      // Ctrl/Cmd+Enter saves from anywhere in the modal so power
      // users don't have to mouse over to the Save button.
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault(); onSave();
      }
    };
    saveBtn.addEventListener('click', onSave);
    cancelBtn.addEventListener('click', onCancel);
    overlay.addEventListener('click', onOverlay);
    autoEl.addEventListener('change', onAuto);
    permsBtn.addEventListener('click', onPerms);
    // Capture phase (see onKey) so the stacked-perm-editor guard reads
    // accurate state. We never stopPropagation here — this is the
    // BOTTOM modal, so it must not swallow keys destined for overlays
    // above it.
    document.addEventListener('keydown', onKey, true);
    overlay.classList.add('show');
    // The role cell's click-to-edit lands here on the Role field; the
    // ⚙ "edit" button lands on Title (the broader edit).
    const focusEl = focusRole ? roleEl : titleEl;
    focusEl.focus();
    focusEl.select();
  });
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
      for (const a of lastSnapshot?.conversations || []) push({ ...a, _promote: true });
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

    async function addOne(idx) {
      const cand = candidates[idx];
      if (!cand) return;
      let r;
      try {
        r = await fetch(`/api/groups/${encodeURIComponent(groupName)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv: cand.conv_id}),
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
export function openCleanupModal(opts) {
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
          conv_id: a.conv_id, title: a.title || '', category: 'agent',
          online: !!a.online, lastActivity: (a.state || {}).last_hook || '',
          owner: !!(a.owned_groups || []).length,
          groups: a.groups || [], checked: false,
        });
      }
      for (const r of (lastSnapshot?.retired || [])) {
        out.push({
          conv_id: r.conv_id, title: r.title || '', category: 'retired',
          online: !!r.online, lastActivity: r.retired_at || '',
          owner: false, groups: [], checked: false,
        });
      }
      for (const c of (lastSnapshot?.conversations || [])) {
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
    const picks = selected().map(c => c.conv_id);
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
async function resumeAgentReq(conv, label) {
  let r;
  try {
    r = await fetch(`/api/agents/${encodeURIComponent(conv)}/resume`, {
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
  // like {action: "resumed" | "skipped:already_online" | ...}.
  try {
    const out = await r.json();
    toast(`wake ${label}: ${out.action || 'ok'}`);
  } catch (_) {
    toast(`wake ${label}: ok`);
  }
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
  bindFilter, bindTabs, bindTabHotkeys, bindAccessSubtabs, bindCopy, bindDetailsPersistence, bindGroupTitleToggle, bindSortHeaders,
  shutdownScope, powerOnScope, openWindowModal, retireConfirm, retireToast, shutdownConfirm,
  maybeHandleDanglingRetire,
  termDirModal, editMemberModal, addMemberModal, deleteAgentModal,
  resumeAgentReq, stopAgentReq,
};
