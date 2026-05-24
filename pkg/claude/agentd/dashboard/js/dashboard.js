import { esc } from './helpers.js';
import { fmtRemaining } from './tabs.js';
import { applySlopThemeIfRequested } from './slop.js';
import {
  bindSlopClickFx, bindSlopMachineClicks, bindSlopStatusWatch,
  bindSlopCursorTrail, bindSlopMarquee,
} from './slop-fx.js';
import {
  bindFilter, bindTabs, bindCopy, bindDetailsPersistence, bindSortHeaders,
  refresh, startEventStream,
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
// Slop-mode flair — each binder installs a delegated listener (or
// starts an interval) once. They no-op while slop is off and the
// body-class check inside each handler is what actually gates the
// effect, so toggling slop mid-session needs no re-binding.
bindSlopClickFx();
bindSlopMachineClicks();
bindSlopStatusWatch();
bindSlopCursorTrail();
bindSlopMarquee();
refresh();
// SSE push: /api/events nudges refresh() the moment the daemon writes
// (debounced server-side; see events.go). Falls back transparently
// to the 2s safety-net poll below if the stream isn't available
// (browser refuses EventSource, daemon error, laptop sleep in
// flight, …). The two channels can occasionally fire near each
// other, but the daemon's broadcaster already coalesces upstream
// bursts so the worst case is one extra cheap snapshot fetch.
startEventStream();
setInterval(refresh, 2000);
