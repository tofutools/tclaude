import { esc } from './helpers.js';
import { fmtRemaining } from './tabs.js';
import { applySlopThemeIfRequested, bindSlopHotkey } from './slop.js';
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
  bindFilter, bindTabs, bindCopy, bindDetailsPersistence, bindSortHeaders,
  refresh,
} from './refresh.js';

// Slop theme — a purely cosmetic re-skin tagged onto the URL with ?slop=1
// (see `tclaude agent dashboard --slop` / `tclaude agentd serve --slop`).
// Run before any binders so the body class is in place when CSS-dependent
// modules first measure the layout.
applySlopThemeIfRequested();
import { bindRowActions } from './row-actions.js';
import { bindDnd } from './dnd.js';
import { bindCronModal } from './modal-cron.js';
import {
  bindMessageModal, bindSudoModal, bindPermEditModal, bindGroupCreateModal,
} from './modal-message.js';
import {
  bindTemplatesUI, bindGroupImportModal, bindGroupContextModal,
} from './modal-templates.js';
import { bindLinkModal } from './modal-link-wt.js';
import {
  bindAgentSpawnModal, bindCloneAgentModal,
  bindReincarnateAgentModal,
} from './modal-spawn.js';
import { bindConfigTab } from './config.js';
import { bindPluginsUI } from './plugins.js';
import { bindCostsTab } from './costs.js';
import { initMail } from './mail.js';
import { initDashPrefs } from './prefs.js';
import { loadSortState } from './sort.js';

// Last successful snapshot, kept so the filter inputs can re-render
// without a server roundtrip when the user types.
export let lastSnapshot = null;
// setLastSnapshot is the single writer entry-point for lastSnapshot.
// It has two writers in different modules — refresh() in refresh.js
// and the rename-rollback in row-actions.js — and an ES-module
// imported binding is read-only in the importer, so the shared state
// stays declared here and both writers route through this setter.
export function setLastSnapshot(v) { lastSnapshot = v; }

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
  bindCopy();
  bindDetailsPersistence();
  bindSortHeaders();
  bindRowActions();
  bindDnd();
  bindFilter('groups');
  bindFilter('templates');
  bindFilter('cron');
  bindFilter('sudo');
  bindFilter('links');
  bindFilter('plugins');
  bindFilter('messages');
  bindSudoModal();
  bindPermEditModal();
  bindCronModal();
  bindMessageModal();
  bindGroupCreateModal();
  bindTemplatesUI();
  bindGroupImportModal();
  bindGroupContextModal();
  bindLinkModal();
  bindAgentSpawnModal();
  bindCloneAgentModal();
  bindReincarnateAgentModal();
  bindConfigTab();
  bindPluginsUI();
  bindCostsTab();
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
})();
