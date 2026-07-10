import { esc } from './helpers.js';
import { fmtRemaining } from './tabs.js';
import { applySlopThemeIfRequested, bindSlopHotkey, bindWizardHotkey } from './slop.js';
import {
  bindWizardCursorTrail, bindWizardCastFx, bindWizardStatusWatch,
  bindWizardMarquee, bindWizardSpectacle, bindWizardEnterBanner,
} from './wizard-fx.js';
import {
  bindSlopClickFx, bindSlopMachineClicks, bindSlopStatusWatch,
  bindSlopCursorTrail, bindSlopMarquee,
} from './slop-fx.js';
import { bindSlopAudio } from './slop-audio.js';
import { bindSlopVolume } from './slop-volume.js';
import { bindSlopCredits } from './slop-credits.js';
import { bindSlopSpectacle } from './slop-spectacle.js';
import { bindVegasMusic } from './vegas.js';
import {
  bindFilter, bindTabs, bindTabHotkeys, bindAccessSubtabs, bindDetailsPersistence, bindGroupTitleToggle, bindGroupQuickHover, bindSortHeaders,
  bindListPagers,
  refresh,
} from './refresh.js';

// Cosmetic re-skins — slop (?slop=1) and wizard (?wizard=1), mutually
// exclusive (see `tclaude agent dashboard --slop|--wizard`). Run before any
// binders so the body class is in place when CSS-dependent modules first
// measure the layout. applySlopThemeIfRequested applies whichever of the two
// the URL carries.
//
// KEEP THIS BEFORE THE BOOTSTRAP IIFE'S BINDERS. It dispatches tclaude:wizard
// synchronously here, so a ?wizard=1 load emits its initial active:true edge
// *before* bindWizardEnterBanner() installs its listener — which is exactly why
// a page that merely LOADS in wizard mode doesn't flash the "It's wizard time!"
// enter banner (see wizard-fx.js). Moving this call into/after the IIFE, or
// making the dispatch async, would silently start firing that banner on load.
applySlopThemeIfRequested();
import { bindRowActions } from './row-actions.js';
import { bindDnd } from './dnd.js';
import { bindGroupReorder } from './group-reorder.js';
import { bindDockDnd } from './dock-dnd.js';
import { bindDockSaveDnd } from './dock-save-dnd.js';
import { bindCronModal } from './modal-cron.js';
import { bindTermModal } from './modal-term.js';
import { initTerminalsTab } from './terminals-tab.js';
import {
  bindMessageModal, bindSudoModal, bindPermEditModal, bindGroupCreateModal,
} from './modal-message.js';
import { bindHumanReplyModal } from './modal-human-reply.js';
import {
  bindTemplatesUI, bindGroupImportModal, bindGroupContextModal,
  bindGroupCloneModal,
} from './modal-templates.js';
import { bindProfilesUI } from './modal-profiles.js';
import { bindRolesUI } from './modal-roles.js';
import { bindCloneModal } from './modal-clone.js';
import { bindNestModal } from './modal-nest.js';
import { bindLinkModal } from './modal-link-wt.js';
import { bindExportModal } from './modal-export.js';
import {
  bindAgentSpawnModal, bindCloneAgentModal,
  bindReincarnateAgentModal,
} from './modal-spawn.js';
import { bindConfigTab } from './config.js';
import { bindNotifyMenu } from './notify-menu.js';
import { bindPluginsUI } from './plugins.js';
import { bindCostsTab, bindCostDisplayToggle } from './costs.js';
import { bindAuditTab } from './audit.js';
import { bindLogsTab } from './logs.js';
import { initProcessesTab } from './processes.js';
import { initMail, focusAccessRequest } from './mail.js';
import { initDashPrefs } from './prefs.js';
import { loadSortState } from './sort.js';
import { bindCommandPalette } from './palette.js';
import { bindDock } from './dock.js';
import { bindHScroll } from './hscroll.js';

// Last successful snapshot, kept so the filter inputs can re-render
// without a server roundtrip when the user types.
export let lastSnapshot = null;
// setLastSnapshot is the single writer entry-point for lastSnapshot.
// It has two writers in different modules — refresh() in refresh.js
// and the rename-rollback in row-actions.js — and an ES-module
// imported binding is read-only in the importer, so the shared state
// stays declared here and both writers route through this setter.
export function setLastSnapshot(v) { lastSnapshot = v; }

// webTerminalDefault reports whether the operator has opted into in-browser
// web terminals as the default for the dashboard's per-agent focus /
// open-window / open-terminal actions (config dashboard.default_terminal="web").
// Read off the latest snapshot so it tracks a live config change on the next
// 2s poll. row-actions.js and palette.js gate their native-vs-web routing on
// this; the dedicated "web term" / "web window" buttons ignore it (always web).
export function webTerminalDefault() {
  return !!(lastSnapshot && lastSnapshot.default_terminal === 'web');
}

// sudoBadge renders the per-row 🔓 indicator when an agent currently
// holds ≥1 active grant. Tooltip lists the slugs + soonest expiry so
// hovering tells the human everything they'd want to know without a
// tab switch.
export function sudoBadge(activeSudo, fallbackConvID) {
  if (!activeSudo || !activeSudo.length) return '';
  const lines = activeSudo.map(g => `${g.slug} (expires in ${fmtRemaining(g.remaining_seconds)})`);
  const title = `${activeSudo.length} active sudo grant${activeSudo.length === 1 ? '' : 's'} — click to manage:\n` + lines.join('\n');
  // sudoByConv entries carry their own conv_id; the caller-supplied
  // fallback (and finally '') just guarantees the badge always has a
  // click target even on an unexpected entry shape.
  const convID = activeSudo[0].conv_id || fallbackConvID || '';
  return `<span class="sudo-badge" data-act="sudo-manage" data-conv="${esc(convID)}" title="${esc(title)}">🔓</span>`;
}

// Boot. The dashboard's sticky view/config prefs now live server-side
// (prefs.js → /api/dashboard/prefs) because the random per-start port
// makes localStorage origin-scoped and thus reset-on-restart. We must
// load that cache BEFORE any bind or render reads a pref, so the whole
// boot runs inside an async IIFE that awaits it first. (An IIFE rather
// than top-level await, to keep this entry module's evaluation
// synchronous for the benign import cycles documented above.)
(async () => {
  await initDashPrefs();
  // sort.js seeds its in-memory sortState from a pref; re-seed it now
  // that the cache is populated (its import-time read saw an empty one).
  loadSortState();

  bindTabs();
  bindTabHotkeys();
  // Delegated pager footers for the Retired / Conversations / Replaced virtual
  // groups (their lists are paginated server-side now).
  bindListPagers();
  // Keep the full-bleed chrome bars sized to the scrollable content so a
  // horizontal page scrollbar doesn't leave them ragged (JOH-313).
  bindHScroll();
  // The Ctrl/Cmd-K command palette. After bindTabs() so its "Go to <tab>"
  // commands click nav buttons whose handlers are already wired.
  bindCommandPalette();
  // The retractable right-side palette dock (JOH-374). After initDashPrefs
  // (awaited above) so its open/collapsed state seeds from the persisted
  // pref; the shell + edge toggle are static so this binds once and survives
  // the poll (renderDock only reconciles #dock-body).
  bindDock();
  bindAccessSubtabs();
  bindDetailsPersistence();
  bindGroupTitleToggle();
  bindGroupQuickHover();
  bindSortHeaders();
  bindRowActions();
  bindDnd();
  bindGroupReorder();
  // Drag a palette dock profile/role card onto a group → spawn dialog prefilled
  // (JOH-375). Its own dockDragActive flag suspends auto-refresh mid-drag, and
  // its document-level listeners coexist with dnd.js / group-reorder.js via a
  // distinct custom MIME + a self-gating active flag (see dock-dnd.js).
  //
  // Order matters: keep this AFTER bindDnd() (as bindGroupReorder already is).
  // dnd.js's dragend is NOT flag-gated — it calls refresh() on EVERY drag-end,
  // dock drags included. Registered after bindDnd, dnd.js's dragend fires while
  // dockDragActive is still true (our dragend runs later), so refreshSuspended()
  // parks that refresh instead of re-rendering under the just-ended gesture.
  bindDockDnd();
  // The REVERSE palette drag (JOH-393): drag a live agent row / group header
  // ONTO the dock to capture it as a spawn profile / group template. Must be
  // registered AFTER bindDnd() + bindGroupReorder() so its dragover runs LAST
  // and wins the shared #dnd-pill over the dock — those two hide the pill when
  // the cursor is off THEIR targets (the dock is off their targets). Self-gates
  // on their active flags + a distinct .dock-save-over highlight (see
  // dock-save-dnd.js), so it coexists with all three other DnD modules.
  bindDockSaveDnd();
  bindFilter('groups');
  bindFilter('templates');
  bindFilter('jobs');
  bindFilter('sudo');
  bindFilter('links');
  bindFilter('plugins');
  bindFilter('messages');
  // The Jobs table's /api/jobs window is fetched only while its tab shows
  // (refresh.js gates on jobsTabActive), so activating the tab kicks an
  // immediate refresh instead of waiting up to 2s for the next poll — the
  // same lazy-load-on-click idiom as the Costs / Audit / Logs tabs.
  document.querySelector('nav button[data-tab="jobs"]')?.addEventListener('click', () => refresh());
  bindSudoModal();
  bindPermEditModal();
  bindCronModal();
  bindTermModal();
  // The in-SPA "Terminals" tab — mounts the multiplexer and starts hidden
  // (it reveals itself once "web term" / "web window" opens the first pane).
  initTerminalsTab();
  bindMessageModal();
  bindHumanReplyModal();
  bindGroupCreateModal();
  bindTemplatesUI();
  bindProfilesUI();
  bindRolesUI();
  bindCloneModal();
  bindNestModal();
  bindGroupImportModal();
  bindGroupContextModal();
  bindGroupCloneModal();
  bindLinkModal();
  bindExportModal();
  bindAgentSpawnModal();
  bindCloneAgentModal();
  bindReincarnateAgentModal();
  bindConfigTab();
  // The top-bar bell's notification-settings popover (master on/off +
  // per-type checklist + human-message knob), backed by /api/notifications.
  bindNotifyMenu();
  bindPluginsUI();
  bindCostsTab();
  bindCostDisplayToggle();
  bindAuditTab();
  bindLogsTab();
  initProcessesTab();
  initMail();
  // Slop-mode flair — each binder installs a delegated listener (or
  // starts an interval) once. They no-op while slop is off and the
  // body-class check inside each handler is what actually gates the
  // effect, so toggling slop mid-session needs no re-binding.
  bindSlopHotkey();
  bindSlopClickFx();
  bindSlopMachineClicks();
  bindSlopStatusWatch();
  bindSlopCursorTrail();
  bindSlopMarquee();
  // Wizard-mode flair — the 🧙 twin of the slop binders. Each installs a
  // delegated listener (or an interval) once and no-ops while wizard mode is
  // off (the body.wizard check inside each handler gates the effect), so a
  // mid-session toggle needs no re-binding.
  bindWizardHotkey();
  bindWizardCursorTrail();
  bindWizardCastFx();
  bindWizardStatusWatch();
  bindWizardMarquee();
  bindWizardSpectacle();
  // Flash "It's wizard time!" when the operator flips INTO wizard mode from
  // another theme. Bound here (after the top-level applySlopThemeIfRequested)
  // so a ?wizard=1 page load — which isn't "entering from another mode" —
  // fires its initial event before this listener exists and stays silent.
  bindWizardEnterBanner();
  // Slop-mode extras, all hung off the tclaude:slopfx bus slop-fx emits:
  // synthesized casino sound (default-muted, header toggle), a credits
  // counter + high-rollers leaderboard, and the Konami mega-jackpot / side
  // pull-lever / confetti spectacle. Each no-ops while slop is off.
  bindSlopAudio();
  bindSlopCredits();
  bindSlopSpectacle();
  // The volume mixer (header 🎚️ popover) — persistent music/FX sliders
  // backed by /api/slop/volumes (the "slop" block of config.json). Must
  // bind after applySlopThemeIfRequested() so an already-slop page load
  // is caught by its initial-state check.
  bindSlopVolume();
  // Vegas-mode soundtrack — the "Vegas" tab + lounge-radio player, started
  // and stopped with slop mode (listens for the tclaude:slop event slop.js
  // dispatches). Must bind after applySlopThemeIfRequested() above so an
  // already-slop page is handled by its initial-state check.
  bindVegasMusic();
  refresh();
  setInterval(refresh, 2000);

  // Deep link: ?tab=messages&access_request=<id> — the target the approval
  // auto-raise / tray "review" builds. Bring the Messages tab forward on the
  // access-requests folder; the card highlight applies once the first snapshot
  // paints the pending request in.
  const dlParams = new URLSearchParams(window.location.search);
  if (dlParams.get('tab') === 'messages') {
    const reqId = dlParams.get('access_request');
    if (reqId !== null) focusAccessRequest(reqId || undefined);
    else document.querySelector('nav button[data-tab="messages"]')?.click();
  }
})();
