import { esc } from './helpers.js';
import { fmtRemaining } from './tabs.js';
import { applySlopThemeIfRequested, bindSlopHotkey, bindWizardHotkey, wizWord } from './slop.js';
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
  bindTabs, bindTabHotkeys, bindDetailsPersistence, bindGroupTitleToggle, bindGroupQuickHover,
  confirmDiscard, confirmModal, isCyclingTabs, refresh, toast,
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
import { bindTermModal } from './modal-term.js';
import { initTerminalsTab } from './terminals-tab.js';
import {
  bindGroupCreateModal,
} from './modal-message.js';
import {
  openPermEditModal, openSudoGrantModal, openSpawnPermEditor, pickAgent,
} from './message-access-dialog-controller.js';
import {
  bindTemplatesUI, bindGroupImportModal, summonTemplateScribe,
} from './modal-templates.js';
import { bindProfilesUI } from './modal-profiles.js';
import { bindSandboxProfilesUI, refreshSpawnSandboxProfileUI, summonSandboxScribe } from './sandbox-profiles.js';
import { bindRolesUI } from './modal-roles.js';
import {
  bindAgentSpawnModal,
} from './modal-spawn.js';
import { bindRemoteAdmin, loadRemoteAdmin } from './remote-admin.js';
import { bindCostDisplayToggle } from './cost-display-toggle.js';
import { focusAccessRequest } from './mail-bridge.js';
import { dashPrefs, initDashPrefs } from './prefs.js';
import { initTerminalThemeSync } from './terminal-theme.js';
import { recordGroupInteraction } from './last-group.js';
import { loadSortState } from './sort.js';
import { bindDock } from './dock.js';
import { bindHScroll } from './hscroll.js';
import { initNavHistory } from './nav-history.js';
import {
  mountAccessFeature, mountActionDialogsFeature, mountAuditFeature, mountConfigFeature, mountCostsFeature, mountDebugFeature, mountDirectoryPickerFeature, mountDockFeature, mountGroupsFeature, mountJobsFeature, mountLinksFeature, mountLogsFeature, mountManagementFeature, mountMessageAccessDialogsFeature, mountMessagesFeature, mountPluginsFeature, mountProcessesFeature, mountShellFeature,
} from './preact-loader.js';
import { configureDashboardActions, dashboardActions } from './dashboard-actions.js';
import { triggerExportDownload } from './export-progress.js';
import { startSnapshotPoll } from './snapshot-poll.js';

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
// open-window / open-terminal actions and bulk windows-modal focus (config
// dashboard.default_terminal="web").
// Read off the latest snapshot so it tracks a live config change on the next
// 2s poll. row-actions.js, palette.js and refresh.js gate their native-vs-web
// routing on this; the dedicated "web term" / "web window" buttons ignore it
// (always web).
export function webTerminalDefault() {
  return !!(lastSnapshot && lastSnapshot.default_terminal === 'web');
}

// Let rAF/ResizeObserver-driven geometry (full-bleed bars and the dock's nav
// inset) converge before the boot curtain lifts. Background tabs may throttle
// rAF indefinitely, so each turn has a short timer fallback; a hidden document
// is not painting layout shifts, and it will have settled by the time it becomes
// visible.
async function settleInitialLayout() {
  for (let i = 0; i < 2; i++) {
    await new Promise((resolve) => {
      const timer = setTimeout(resolve, 100);
      requestAnimationFrame(() => {
        clearTimeout(timer);
        resolve();
      });
    });
  }
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
  // Start cross-window terminal-palette synchronization only after the local
  // preference cache is authoritative. Popped-out terminals do the same in
  // terminals.js, so toggling either surface repaints the other immediately.
  initTerminalThemeSync();
  // Preact islands call this stable action boundary rather than
  // importing refresh.js. The interval below remains the sole poll owner.
  configureDashboardActions({ refresh });
  // sort.js seeds its in-memory sortState from a pref; re-seed it now
  // that the cache is populated (its import-time read saw an empty one).
  loadSortState();
  const pageCleanups = [];

  // Mount the cross-tab shell before feature islands and legacy binders. Its
  // feedback services retain the existing function APIs, while Preact owns
  // their stable DOM hosts and document-level lifecycle.
  pageCleanups.push(await mountShellFeature({ notify: toast }));

  // The Jobs pilot owns its filter, table, sort, paging, and badge subtrees.
  // Await the mount before legacy modal binders look up the create button.
  pageCleanups.push(await mountJobsFeature({
    requestMutation: dashboardActions.requestMutation,
    refresh: dashboardActions.refresh,
    confirm: confirmModal,
    notify: toast,
    download: triggerExportDownload,
    confirmDiscard,
  }));
  // The remaining bounded islands are independent. Load them concurrently so
  // navigation setup is delayed by only the slowest optional feature import,
  // not by the sum of seven dynamic-import chains. Await the whole group before
  // initNavHistory below so initial deep links still find every lazy loader.
  const featureCleanups = await Promise.all([
    mountMessageAccessDialogsFeature({
      refresh: dashboardActions.refresh,
      notify: toast,
      confirmDiscard,
      words: wizWord,
    }),
    mountGroupsFeature({
      refresh: dashboardActions.refresh,
      notify: toast,
      confirmDiscard,
      openMemberPermissions: openPermEditModal,
    }),
    mountLinksFeature({
      refresh: dashboardActions.refresh,
      confirm: confirmModal,
      confirmDiscard,
      notify: toast,
      words: wizWord,
    }),
    mountDockFeature(),
    mountPluginsFeature({
      requestMutation: dashboardActions.requestMutation,
      refresh: dashboardActions.refresh,
      confirm: confirmModal,
      notify: toast,
    }),
    mountCostsFeature(),
    mountAccessFeature({
      requestMutation: dashboardActions.requestMutation,
      confirm: confirmModal,
      notify: toast,
      openGrant: async () => {
        const convID = await pickAgent({
          title: 'Grant sudo to', identity: 'conv', showSudo: true,
          includeOfflineHint: 'Include offline / archived agents (the daemon will still grant; the agent sees the slug on next wake)',
        });
        if (convID) openSudoGrantModal(convID);
      },
    }),
    mountLogsFeature(),
    mountMessagesFeature(),
    mountAuditFeature(),
    mountDebugFeature(),
    mountConfigFeature({ toast, isCyclingTabs }),
    mountProcessesFeature({ confirm: confirmModal, confirmDiscard, notify: toast }),
    mountDirectoryPickerFeature({
      prefersWeb: () => lastSnapshot?.default_directory_picker === 'web',
    }),
    mountManagementFeature({
      confirm: confirmModal, confirmDiscard, notify: toast,
      getSnapshot: () => lastSnapshot,
      openProfilePermissions: (options) => openSpawnPermEditor({ ...options, group: 'the spawn group' }),
      refreshSandboxSpawn: () => refreshSpawnSandboxProfileUI(document.querySelector('#agent-spawn-group')?.value || ''),
      summonSandboxScribe,
      summonTemplateScribe,
      refresh,
      onGroupImported: (name) => { dashPrefs.setItem(`tclaude.dash.group.${name}`, '1'); recordGroupInteraction(name); },
      onGroupDeployed: (name) => {
        dashPrefs.setItem(`tclaude.dash.group.${name}`, '1'); recordGroupInteraction(name);
        document.querySelector('nav [data-tab="groups"]')?.click();
      },
    }),
    mountActionDialogsFeature({
      confirmDiscard,
      refresh: dashboardActions.refresh,
      notify: toast,
      downloadExport: triggerExportDownload,
      getSnapshot: () => lastSnapshot,
    }),
  ]);
  pageCleanups.push(...featureCleanups);

  bindTabs();
  bindTabHotkeys();
  // Keep the full-bleed chrome bars sized to the scrollable content so a
  // horizontal page scrollbar doesn't leave them ragged (JOH-313).
  bindHScroll();
  // The retractable right-side palette dock (JOH-374). After initDashPrefs
  // (awaited above) so its open/collapsed state seeds from the persisted
  // pref; the shell + edge toggle are static so this binds once and survives
  // the poll (renderDock only reconciles #dock-body).
  bindDock();
  bindDetailsPersistence();
  bindGroupTitleToggle();
  bindGroupQuickHover();
  bindRowActions();
  pageCleanups.push(bindDnd(), bindGroupReorder());
  // Drag a palette dock profile/role card onto a group → spawn dialog prefilled
  // (JOH-375). Its document-level listeners coexist with dnd.js /
  // group-reorder.js via a distinct custom MIME + self-gating state.
  //
  // Keep registration order stable so shared pill/highlight integrations see
  // events in the same sequence as before the ownership migration.
  pageCleanups.push(bindDockDnd());
  // The REVERSE palette drag (JOH-393): drag a live agent row / group header
  // ONTO the dock to capture it as a spawn profile / group template. Must be
  // registered AFTER bindDnd() + bindGroupReorder() so its dragover runs LAST
  // and wins the shared #dnd-pill over the dock — those two hide the pill when
  // the cursor is off THEIR targets (the dock is off their targets). Self-gates
  // on their active flags + a distinct .dock-save-over highlight (see
  // dock-save-dnd.js), so it coexists with all three other DnD modules.
  pageCleanups.push(bindDockSaveDnd());
  window.addEventListener('pagehide', (event) => {
    // A persisted pagehide enters the back-forward cache; bootstrap does not
    // rerun on pageshow, so retain listeners for that suspended document.
    if (event.persisted) return;
    for (const cleanup of pageCleanups.reverse()) cleanup?.();
  });
  bindTermModal();
  // The in-SPA "Terminals" tab — mounts the multiplexer and starts hidden
  // (it reveals itself once "web term" / "web window" opens the first pane).
  initTerminalsTab();
  bindGroupCreateModal();
  bindTemplatesUI();
  bindProfilesUI();
  bindSandboxProfilesUI();
  bindRolesUI();
  bindGroupImportModal();
  bindAgentSpawnModal();
  bindRemoteAdmin();
  document.querySelector('nav [data-tab="config"]')?.addEventListener('click', () => { void loadRemoteAdmin(); });
  bindCostDisplayToggle();
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
  pageCleanups.push(bindSlopCredits());
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

  // Capture the legacy deep-link params (?tab=…&access_request=…) BEFORE
  // initNavHistory rewrites the address bar to the canonical path — the rewrite
  // drops these query params, so the Messages block below must read them first.
  const dlParams = new URLSearchParams(window.location.search);

  // Back/forward navigation (TCL-317). Initialised LAST, after every tab binder
  // above is installed: restoring a deep-link URL (e.g. /costs) activates that
  // tab by clicking it, and the click must find its lazy-loader already wired.
  // It reads the initial location from the path (honouring the legacy ?tab=
  // alias) and mirrors navigation through the browser History API.
  initNavHistory();

  // Deep link: ?tab=messages&access_request=<id> — the target the approval
  // auto-raise / tray "review" builds. Bring the Messages tab forward on the
  // access-requests folder; the card highlight applies once the first snapshot
  // paints the pending request in.
  if (dlParams.get('tab') === 'messages') {
    const reqId = dlParams.get('access_request');
    if (reqId !== null) focusAccessRequest(reqId || undefined);
    else document.querySelector('nav [data-tab="messages"]')?.click();
  }

  // The static shell deliberately starts paint-curtained: URL theme classes,
  // server-backed dock preferences, and snapshot-owned feature islands all
  // change geometry during bootstrap. Complete one authoritative refresh after
  // initial navigation has selected its real tab, then reveal a fully-laid-out
  // first frame. Two animation-frame turns let Preact/Signals and the
  // ResizeObserver-driven dock/nav geometry settle before visibility changes.
  // dashboard.css carries an eight-second CSS-only failsafe in case the module
  // graph faults before reaching this point.
  let resolveFirstSnapshot;
  const firstSnapshot = new Promise((resolve) => { resolveFirstSnapshot = resolve; });
  const onFirstSnapshot = () => resolveFirstSnapshot();
  document.addEventListener('tclaude:snapshot', onFirstSnapshot, { once: true });
  let bootTimeout;
  const bootTimedOut = new Promise((resolve) => {
    // Beat the CSS-only eight-second failsafe slightly. If a snapshot request
    // remains pending forever, the already-installed poll keeps retrying while
    // this bound guarantees the usable/error shell is eventually revealed.
    bootTimeout = setTimeout(resolve, 7500);
  });

  // Install the recurring cadence BEFORE awaiting the first attempt. A fetch
  // can remain pending at the network layer; later request generations must
  // still get their 2s retry opportunities instead of bootstrap wedging.
  pageCleanups.push(startSnapshotPoll(refresh, { immediate: false }));
  await Promise.race([refresh(), firstSnapshot, bootTimedOut]);
  clearTimeout(bootTimeout);
  document.removeEventListener('tclaude:snapshot', onFirstSnapshot);
  await settleInitialLayout();
  document.body.classList.remove('dashboard-booting');
})();
