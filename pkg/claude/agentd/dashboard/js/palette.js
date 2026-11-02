import { openGitRepositories } from './git-repositories-controller.js';
// palette.js — the Ctrl/Cmd-K command palette ("spotlight").
//
// A keyboard-first overlay that searches across the dashboard's
// EXISTING operations and runs the picked one on Enter. It is a thin
// SURFACE, not new behaviour: every command it lists delegates to a
// function or endpoint that already exists —
//
//   - navigation        → clicks the matching <nav> button, reusing
//                          bindTabs() plus each tab's own load-on-click
//                          handler (audit/costs/messages fetch on click)
//   - window focus/hide  → POST /api/agent-windows (bulk, all or one
//                          group) or POST /api/jump|/api/hide (per
//                          agent) — the very calls the 🪟 windows… modal
//                          and the per-row eye buttons make
//   - pick-a-subset      → opens the existing window modal
//   - spawn              → opens the existing spawn modal
//   - power control      → POST /api/shutdown | /api/power-on (global or
//                          per-group, via shutdownScope/powerOnScope) and
//                          per-agent stop/resume (transaction dialog /
//                          resumeAgentReq) — the same calls the dashboard's
//                          Shutdown/Power-on buttons and status dots make
//   - retire             → the per-agent / per-group demote-to-conversation
//                          flows
//   - global managers    → opens the same import, cleanup, profile, template,
//                          role, sandbox, harness-policy, and links surfaces as
//                          the Groups toolbar's global cog
//
// So the palette only adds a fast keyboard entry point to the window
// hide/show, power control, retire + navigation the dashboard already
// does; it owns no agent state of its own and builds from the accepted
// dashboardState snapshot injected by the Preact state boundary. NB power control STOPS or
// RESUMES agent processes, unlike the window ops which only detach/raise
// terminals — see section 1b.
//
// palette-island.js renders it as a .modal-overlay for the shared backdrop.
// Preact owns its input, selection and focus, so snapshot publishes continue
// underneath without yanking the search box mid-type.
//
// Trigger: Ctrl/Cmd-K (claimed with preventDefault; pressing it again
// closes). Esc or a backdrop click closes. ↑/↓ move the selection one
// row (wrapping), PageUp/PageDown jump a viewport-worth (clamping at the
// ends), Enter runs it, typing filters.

import { $, $$ } from './helpers.js';
import { featureState } from './feature-state-registry.js';
import { chooseGroupsFocus } from './groups-focus.js';
import { setActivitySubtab } from './activity-state.js';
import { toast, noteGroupDisclosureIntent } from './refresh.js';
import {
  openWindowModal,
  openRetireAllPreview, openRetirePreview, openRetireUngroupedPreview,
  openDeleteRetiredPreview,
  openWorktreeCleanup, openCleanupModal,
  shutdownScope, powerOnScope, resumeAgentReq,
} from './dashboard-operations.js';
import {
  openRetireAgentDialog, openShutdownAgentDialog,
} from './transaction-dialog-controller.js';
import { buildProfileSpawnCommands } from './palette-spawn.js';
import { openAgentSpawnModal } from './agent-spawn-controller.js';
import { openProfilesManageModal } from './modal-profiles.js';
import { openRolesManageModal } from './modal-roles.js';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { openGroupImport, openSnapshots, openTemplatesManageModal } from './modal-templates.js';
import { openSandboxProfilesManageModal } from './sandbox-profiles.js';
import { openSpawnHarnessPolicy } from './spawn-harness-policy-controller.js';
import { openLinksManager } from './links-controller.js';
import { openGroupCreateModal } from './group-create-controller.js';
import { toggleSlop, isSlopActive, toggleWizard, isWizardActive } from './slop.js';
import { recordGroupInteraction, lastInteractedGroup } from './last-group.js';
import { setDockOpen } from './dock.js';
import { scribeVisible } from './virtual-groups.js';
import { scribeGroupVisible } from './scribe-groups.js';
import { closeTerminalsForConvs, closeTerminalsForWindowOp, openGroupWebTermPane, openWebWindowPane } from './terminals-tab.js';
import { createPaletteWindowOperator, webWindowTargets } from './palette-window-ops.js';
import { buildRegisteredCommands } from './command-registry.js';
import { openMessageCreateModal, openOperatorMessageDialog } from './message-access-dialog-controller.js';
import { limitHint, limitPeerViewCommands, shellControl } from './peer-view-limits.js';
import { buildFusedNodeCommands } from './palette-fused.js';
import { fusedGroupListing } from './group-order.js';
import { jumpAgent } from './agent-jump.js';

// wiz(regular, wizard) returns the arcane string in 🧙 mode, else the plain
// one. buildCommands wraps every command's PRESENTED label + hint (and its
// action icon) in it, so the Spellbook reads Summon / Slumber / Veil / Banish
// under body.wizard while the functional wording lives on. The per-command
// keywords are NOT wizard-gated — they always carry BOTH vocabularies (the old
// plain terms AND the new arcane synonyms), so every command stays findable by
// either word in either theme: the wizard set is ADDED, never swapped in. Read
// live (not cached at build) so the tclaude:wizard listener's rebuild re-skins
// the open list on a mid-session theme flip, matching the placeholder swap.
function wiz(regular, wizard) {
  return isWizardActive() ? wizard : regular;
}

// -- POST helpers — the same endpoints the windows modal / eye buttons
//    hit. Each toasts its own outcome (matching the existing modal's
//    wording) and never touches an agent process: window-only.

const bulkWindowOp = createPaletteWindowOperator({
  fetchImpl: (...args) => fetch(...args),
  notify: toast,
  openWebWindowPane,
  closeTerminalsForWindowOp,
});

async function hideAgent(conv, label) {
  try {
    const r = await fetch(`/api/hide/${encodeURIComponent(conv)}`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) { toast(`hide ${label}: ${await r.text()}`, true); return; }
    const out = await r.json().catch(() => ({}));
    // Close this agent's multiplexer pane too (if open) — its live-session
    // client was just detached, so the terminal tab would otherwise linger
    // showing "disconnected". Server-side detach already ran → close, don't hide.
    closeTerminalsForConvs([conv]);
    toast(out.detached > 0 ? `hidden: ${label}` : `already hidden: ${label}`);
  } catch (e) {
    toast(`hide ${label}: ${(e && e.message) || e}`, true);
  }
}

// -- Group fold helpers — collapse/expand the Groups-tab listing. Each
//    group renders as <details data-group-key>; assigning .open fires a
//    native toggle event that bindDetailsPersistence (refresh.js) catches,
//    so the state sticks across the 2s re-render. We switch to the Groups
//    tab first so the change is actually visible.

function gotoGroupsTab() {
  const btn = $('nav [data-tab="groups"]');
  if (btn) btn.click();
}

// openGroupsFocus switches the Groups lens. From another tab the focus is set
// first so the Groups nav click records /groups/<focus> as one entry.
function openConversationSearch() {
  const groups = featureState('groups');
  if (!groups?.contentSearchOpen) return;
  groups.contentSearchOpen.value = true;
  const btn = $('nav [data-tab="groups"]');
  if (btn && !btn.classList.contains('active')) btn.click();
}

function openGroupsFocus(focus) {
  const groups = featureState('groups');
  if (!groups?.setFocus) return;
  const btn = $('nav [data-tab="groups"]');
  if (btn && !btn.classList.contains('active')) {
    groups.setSubview?.('members', '');
    groups.setFocus(focus);
    btn.click();
    return;
  }
  chooseGroupsFocus(groups, focus);
}

// openRemote opens a peer's agent terminal or shell here, through the
// federation bridge (remote-terminal.js), loaded on first use.
const openRemote = (fn, opts) => import('./remote-terminal.js').then((m) => m[fn](opts));
const SAFE_AGENT = /^agt_[A-Za-z0-9]{4,64}$/;

// pageNodeName is how a peer's page names the peer it shows.
function pageNodeName(remote) {
  return document.documentElement.dataset.remoteNodeName || remote.id.slice(0, 13);
}

function setGroupOpen(name, open) {
  gotoGroupsTab();
  const d = $(`#tab-groups details[data-group-key="${CSS.escape(name)}"]`);
  if (!d) { toast(`group ${name}: not listed on the Groups tab`, true); return; }
  // Record only once we know the fold will actually happen — symmetric with
  // the modal record sites, which stamp after their success point.
  recordGroupInteraction(name);
  if (d.open !== open) {
    noteGroupDisclosureIntent(name);
    d.open = open; // fires toggle → bindDetailsPersistence persists the state
  }
  const sum = d.querySelector('summary');
  if (sum) sum.scrollIntoView({ block: 'nearest' });
}

function setAllGroupsOpen(open) {
  gotoGroupsTab();
  const all = $$('#tab-groups details[data-group-key]');
  for (const d of all) {
    if (d.open === open) continue;
    noteGroupDisclosureIntent(d.getAttribute('data-group-key'));
    d.open = open;
  }
  const n = `${all.length} group${all.length === 1 ? '' : 's'}`;
  toast(open ? `expanded ${n}` : `collapsed ${n}`);
}

// -- Command list ------------------------------------------------------

// buildCommands assembles the command list from the live snapshot plus
// the current <nav>. Order is "headline first": global window hide/show
// then global power control (shut down / power on all), then spawn,
// theme, tab navigation, group fold (collapse/expand), then the
// per-group and per-agent window ops, each followed by its power-control
// (stop/start) sibling, then the retire blocks.
// rankCommands re-ranks by query, so this order only governs the
// empty-query view.
export function buildCommands(snapshot) {
  const snap = snapshot || {};
  const preferWebTerminal = snap.default_terminal === 'web';
  const allWebWindowTargets = webWindowTargets(snap.agents);
  // Mirror the Groups tab's scribe treatment: live scribe groups and their
  // commands are always visible; dormant/offline ones follow the view toggle.
  // Per-AGENT commands (§7/§7b, sourced from snap.agents) are unaffected: the
  // scribe agent still shows in the Agents tab, so it stays focusable/stoppable
  // by name.
  const showOfflineScribes = scribeVisible();
  const groups = (snap.groups || [])
    .filter(g => scribeGroupVisible(g, showOfflineScribes));
  const cmds = [];

  // 1) Global window ops — "hide all windows" (and its inverse), plus
  //    the modal for picking an arbitrary subset. In 🧙 mode a terminal
  //    window is a hero's "scrying portal", so hide → Veil, focus →
  //    Reveal (arcane presented label; the plain keywords stay searchable).
  cmds.push({
    peerView: 'own', icon: wiz('⏏', '🌫'), label: wiz('Hide all windows', 'Veil all heroes'),
    hint: wiz('detach every agent terminal window (agents keep running)',
      "draw the veil over every hero's scrying portal (they keep channeling)"),
    keywords: 'hide unfocus all windows declutter detach panic minimize'
      + ' veil conceal cloak shroud portal scrying vision heroes',
    run: () => bulkWindowOp({ direction: 'unfocus', scope: 'all' },
      'hide all windows', { webTerminal: preferWebTerminal, targets: allWebWindowTargets }),
  });
  cmds.push({
    peerView: 'own', icon: wiz('◎', '👁'), label: wiz('Focus all windows', 'Reveal all heroes'),
    hint: wiz('raise / open a terminal window for every running agent',
      'conjure a scrying portal for every channeling hero'),
    keywords: 'show all windows raise focus bring up'
      + ' reveal behold conjure portal scrying vision heroes',
    run: () => bulkWindowOp({ direction: 'focus', scope: 'all' },
      'focus all windows', { webTerminal: preferWebTerminal, targets: allWebWindowTargets }),
  });
  cmds.push({
    peerView: 'own', icon: wiz('▦', '👁'), label: wiz('Pick windows to focus / hide…', 'Choose heroes to reveal / veil…'),
    hint: wiz('open the window modal to choose a subset',
      'open the scrying circle to choose which portals to reveal or veil'),
    keywords: 'windows subset choose select modal some'
      + ' reveal veil portals scrying heroes',
    run: () => openWindowModal('all', null),
  });

  // 1b) Global power control — shut down / power on EVERY agent. The
  //     POWER analog of the window ops above: hiding a window only
  //     detaches a terminal (the agent keeps running), whereas these
  //     actually stop the processes (/exit, then force-kill on grace
  //     timeout) or resume them. Both reuse shutdownScope/powerOnScope —
  //     the same count + confirm + POST the dashboard's "Shutdown all" /
  //     "Power on all" buttons fire. Each is gated on its live count so
  //     the palette never lists a no-op (no running agents → no "shut
  //     down all"; nothing offline → no "power on all").
  for (const mode of ['pull', 'sync']) {
    const verb = mode === 'pull' ? 'Pull' : 'Sync';
    cmds.push({
      icon: mode === 'pull' ? '↓' : '↻',
      label: wiz(`${verb} all repositories…`, mode === 'pull' ? 'Summon latest code · all parties…' : 'Harmonize repositories · all parties…'),
      hint: wiz('Update repositories in group homes and two levels below', 'Refresh repositories in party homes and two levels below'),
      keywords: `git ${mode} all repositories groups home directories fetch summon latest harmonize`,
      run: () => openGitRepositories(mode),
    });
  }
  const onlineAll = (snap.agents || []).filter(a => a.online).length;
  const offlineAll = (snap.agents || []).filter(a => !a.online).length;
  if (onlineAll) {
    const plural = onlineAll === 1 ? '' : 's';
    cmds.push({
      peerView: 'operator', what: 'announcing to every live agent', icon: wiz('📣', '📯'), label: wiz('Announce to all live agents…', 'Proclaim to all channeling heroes…'),
      hint: wiz(`send one human-authored message to ${onlineAll} currently live agent${plural}`,
        `sound one human proclamation to ${onlineAll} channeling hero${plural && 'es'}`),
      keywords: 'announce broadcast message notify all live online agents global everyone'
        + ' proclaim herald trumpet channeling heroes',
      run: () => openOperatorMessageDialog({
        allLive: true,
        label: `all live agents (currently ${onlineAll})`,
      }),
    });
    cmds.push({
      icon: wiz('⏻', '🌙'), label: wiz('Shut down all agents', 'Slumber all heroes'),
      hint: wiz(`stop ${onlineAll} running agent${plural} (resumable; no data deleted)`,
        `lull ${onlineAll} channeling hero${plural && 'es'} into slumber (rousable; nothing is lost)`),
      keywords: 'shutdown shut down stop kill power off halt all agents global everything batch'
        + ' slumber sleep rest lull dormant quell still heroes',
      run: () => shutdownScope('all', null),
    });
  }
  if (offlineAll) {
    const plural = offlineAll === 1 ? '' : 's';
    cmds.push({
      icon: wiz('⏼', '✨'), label: wiz('Power on all agents', 'Awaken all heroes'),
      hint: wiz(`resume ${offlineAll} offline agent${plural} onto their conversations`,
        `rouse ${offlineAll} slumbering hero${plural && 'es'} back onto their scrolls`),
      keywords: 'power on start resume wake boot up all agents global everything batch'
        + ' awaken rouse stir revive kindle heroes',
      run: () => powerOnScope('all', null),
    });
  }

  for (const g of (snap.groups || [])) {
    for (const mode of ['pull', 'sync']) {
      const verb = mode === 'pull' ? 'Pull' : 'Sync';
      cmds.push({
        icon: mode === 'pull' ? '↓' : '↻',
        label: wiz(`${verb} ${g.name}…`, `${mode === 'pull' ? 'Summon latest code' : 'Harmonize repositories'} · ${g.name}…`),
        hint: wiz(`Update repositories in and below group ${g.name}`, `Refresh repositories in and below party ${g.name}`),
        keywords: `git ${mode} ${g.name} repositories group home fetch summon latest harmonize`,
        run: () => { recordGroupInteraction(g.name); return openGitRepositories(mode, g.name); },
      });
    }
    // Open a throwaway web shell in the group's home (default) directory —
    // the palette entry point for the group ⚙ menu's "open web terminal"
    // (row action group-web-term). Like that menu item it is only offered
    // when the group HAS a default dir, so the server resolve has a target.
    const home = (g.default_cwd || '').trim();
    const page = globalThis.__tclaudeRemoteNode;
    if (home && page?.id) {
      // On a peer's page the group's shell is the peer's, opened here in the
      // browser under its sessions.shell grant on the group, as the ⚙ item.
      const control = shellControl({ dataset: { act: 'group-web-term', group: g.name } }, snap.peer_view);
      const name = pageNodeName(page);
      cmds.push({
        node: page.id, icon: wiz('🖥', '🔮'),
        label: wiz(`Open terminal in group: ${g.name}`, `Open party portal: ${g.name}`),
        hint: wiz(`web shell in ${home} on ${name}`, `browser scrying portal in ${home} on ${name}`),
        keywords: `open terminal shell console web term cd home dir directory default cwd group ${g.name} terminal ${g.name} shell ${g.name}`
          + ' portal scrying party',
        ...(control.state === 'shell'
          ? { run: () => { recordGroupInteraction(g.name); void openRemote('openRemoteShell', { instance: page.id, peerLabel: name, ...control.shell }); } }
          : { enabled: false, disabledReason: limitHint(control.state, control.what, name, control.needs), run: () => {} }),
      });
    } else if (home) {
      cmds.push({
        peerView: 'own', icon: wiz('🖥', '🔮'),
        label: wiz(`Open terminal in group: ${g.name}`, `Open party portal: ${g.name}`),
        hint: wiz(`web shell in ${home}`, `browser scrying portal in ${home}`),
        keywords: `open terminal shell console web term cd home dir directory default cwd group ${g.name} terminal ${g.name} shell ${g.name}`
          + ' portal scrying party',
        run: () => { recordGroupInteraction(g.name); openGroupWebTermPane(g.name, g.name); },
      });
    }
  }

  // 1c) Create a new group. Opens the very dialog the Groups-tab
  //     "+ new group" button opens (openGroupCreateModal → POST
  //     /api/groups on submit) — a thin surface over the existing flow,
  //     no new behaviour. It comes before the archive importer so the broad
  //     "group" / "party" queries keep their established create-group default.
  cmds.push({
    icon: wiz('＋', '⚔'), label: wiz('Create new group…', 'Form a party…'),
    hint: wiz('open the new-group dialog',
      'gather a fresh band — muster a new adventuring party'),
    keywords: 'new group create make add team squad'
      + ' party form fellowship warband adventuring muster gather assemble guild',
    run: () => openGroupCreateModal(),
  });

  // 1d) The unconditional global Groups-cog actions. Every entry delegates to
  //     the same controller/modal opener as its menu twin, keeping Ctrl/Cmd-K a
  //     complete keyboard route to the global menu rather than a second
  //     implementation. General cleanup precedes the narrower conditional
  //     cleanup actions below so a broad "cleanup" / "tidy" query opens the
  //     all-categories tool.
  cmds.push({
    icon: wiz('⤒', '📜'), label: wiz('Import an agent or group…', 'Unseal a party archive…'),
    hint: wiz('preview and import an agent .paf, or recreate an exported group with its agents, permissions, messages, and conversations',
      'unseal an exported party archive with its heroes, wards, missives, and scrolls'),
    keywords: 'import group agent paf archive zip restore recreate upload'
      + ' unseal party archive hero wards missives scrolls',
    run: () => openGroupImport(),
  });
  cmds.push({
    peerView: 'operator', what: 'its snapshot library', icon: '📸', label: wiz('Snapshots…', 'Captured heroes…'),
    hint: wiz("this node's agent snapshot library: take, spawn from, share or publish snapshots",
      'the library of captured heroes: capture, summon from, share or publish them'),
    keywords: 'snapshot snapshots library paf fork spawn share board publish resilience capture',
    run: () => openSnapshots(),
  });
  cmds.push({
    icon: '🧹', label: wiz('Clean up agents and conversations…', 'Tidy heroes and scrolls…'),
    hint: wiz('open the all-categories cleanup tool to unjoin, retire, delete, or reinstate',
      'open the tower-wide tidying tool to release, banish, dispel, or restore'),
    keywords: 'clean up agents and conversations cleanup all categories unjoin retire delete reinstate'
      + ' tidy heroes and scrolls release banish dispel restore',
    run: () => openCleanupModal({ mode: 'agents' }),
  });

  // 1e) Global delete-retired — "Delete retired agents…". The human-driven
  //     sibling of the timed agent.retired_cleanup auto-sweep (JOH-269):
  //     opens a PREVIEW modal (openDeleteRetiredPreview) listing every
  //     retired agent, all ticked, with live title/age filters and a
  //     per-row opt-out, then POSTs the explicit ticked-AND-visible list to
  //     /api/cleanup/agents {mode:"delete"}. 🗑 distinguishes it from the ♻
  //     retire commands. Gated on ≥1 retired agent so the palette never
  //     offers a no-op.
  // retired[] is fetched only on the Groups tab now. The cheap total remains
  // in every snapshot so this cross-tab command stays accurate. (The modal
  // itself fetches the full list on open.)
  const retiredCount = snap.retired_total || 0;
  if (retiredCount) {
    const plural = retiredCount === 1 ? '' : 's';
    cmds.push({
      icon: wiz('🗑', '🔥'), label: wiz('Delete retired agents…', 'Dispel banished heroes…'),
      hint: wiz(`preview + permanently delete any of ${retiredCount} retired agent${plural} (cannot be undone)`,
        `preview + forever dispel any of ${retiredCount} banished hero${plural && 'es'} (cannot be undone)`),
      keywords: 'delete purge retired cleanup remove wipe agents'
        + ' dispel banished obliterate destroy erase vanquish incinerate heroes',
      run: () => openDeleteRetiredPreview(),
    });
  }

  // 1f) Global retire-ungrouped — "Retire ungrouped agents…". The
  //     cross-group cleanup twin of the per-group retire (section 8):
  //     ungrouped agents belong to no group, so there is no group retire
  //     command to reach them. Opens a PREVIEW modal
  //     (openRetireUngroupedPreview) listing every agent in no group —
  //     online and offline alike — all ticked, with live title/id filters
  //     and a per-row opt-out, then POSTs the explicit list to
  //     /api/cleanup/agents {mode:"retire"}. ♻ marks it a (reinstatable)
  //     retire, distinct from the 🗑 delete-retired above it. Gated on ≥1
  //     ungrouped agent so the palette never offers a no-op. In 🧙 mode
  //     these loose agents are "unbound heroes" and retire → banish.
  const ungroupedCount = new Set((snap.ungrouped || []).map(a => a.conv_id).filter(Boolean)).size;
  if (ungroupedCount) {
    const plural = ungroupedCount === 1 ? '' : 's';
    cmds.push({
      icon: wiz('♻', '🪄'), label: wiz('Retire ungrouped agents…', 'Banish unbound heroes…'),
      hint: wiz(`preview + demote ${ungroupedCount} agent${plural} that are in no group to plain conversations`,
        `preview + banish ${ungroupedCount} unbound hero${plural && 'es'} back to plain scrolls`),
      keywords: 'retire demote cleanup remove tidy bulk ungrouped no group groupless loose solo orphan stray agents'
        + ' banish exile dismiss unbound loose unattached heroes',
      run: () => openRetireUngroupedPreview(),
    });
  }

  // 2) Spawn a new agent. The plain command DEFAULTS the dialog's group
  //    picker to the group the operator last interacted with (folded /
  //    spawned / palette-touched) but leaves it changeable; the per-group
  //    variants below PIN a specific group each (hiding the picker). Both
  //    reuse the existing spawn modal — `defaultGroup` preselects without
  //    forcing, `groupName` fixes + hides the picker.
  const lastGroup = lastInteractedGroup();
  const lastGroupLive = groups.some(g => g.name === lastGroup);
  cmds.push({
    peerView: 'spawn', icon: wiz('＋', '🔮'), label: wiz('Spawn agent…', 'Summon a hero…'),
    hint: lastGroupLive
      ? wiz(`open the spawn dialog (defaults to ${lastGroup} — last used)`,
        `open the summoning circle (defaults to ${lastGroup} — last used)`)
      : wiz('open the spawn dialog', 'open the summoning circle'),
    keywords: 'new agent create spawn launch start'
      + ' summon conjure invoke call forth hero'
      + (lastGroupLive ? ' ' + lastGroup : ''),
    run: () => openAgentSpawnModal(lastGroupLive ? { defaultGroup: lastGroup } : {}),
  });
  cmds.push(...buildProfileSpawnCommands(snap, {
    defaultGroup: lastGroupLive ? lastGroup : '',
    wiz,
    openSpawn: openAgentSpawnModal,
  }));
  // 2b) Manage spawn profiles — open the profiles overlay where the saved
  //     spawn profiles (reusable bundles of the spawn dialog's fields) are
  //     listed to view / edit / delete / add. Reuses the very overlay the
  //     Groups cog's "⧉ profiles…" entry opens (openProfilesManageModal); the
  //     palette just adds a keyboard entry point, owning no state of its own.
  //     In 🧙 mode the whole profiles vocabulary re-letters to "loadouts"
  //     (a saved spawn recipe is a "loadout"), so it presents as
  //     "Manage loadouts…"; the plain words stay searchable in the
  //     keywords.
  cmds.push({
    icon: wiz('⧉', '📜'), label: wiz('Manage spawn profiles…', 'Manage loadouts…'),
    hint: wiz('open the spawn-profiles manager — view, edit, or add reusable spawn recipes',
      'open the loadout grimoire — inscribe, revise, or weave summoning recipes'),
    keywords: 'spawn profiles profile edit manage recipe recipes bundle preset presets defaults'
      + ' loadouts loadout gear weave inscribe grimoire loom blueprint',
    run: () => openProfilesManageModal(),
  });
  // 2c) Manage group templates — reusable team blueprints, called summoning
  //     circles throughout wizard mode. Reuses the Groups-cog overlay.
  cmds.push({
    icon: wiz('⧉', '🕯'), label: wiz('Manage group templates…', 'Manage summoning circles…'),
    hint: wiz('open the template library — create, edit, import, or deploy reusable team blueprints',
      'open the circle library — chalk, revise, unseal, or cast reusable party circles'),
    keywords: 'group templates template manage team blueprint blueprints roster deploy import library'
      + ' summoning circles circle party chalk trace cast unseal rite',
    run: () => openTemplatesManageModal(),
  });
  // 2d) Manage the role library — the named, reusable agent-role defaults a
  //     template roster agent references (a canonical brief + a default launch
  //     shape + a default permission set). Reuses the overlay the Groups cog's
  //     "⧉ roles…" entry opens (openRolesManageModal). In 🧙 mode roles are the
  //     party's "classes".
  cmds.push({
    icon: wiz('⧉', '🎭'), label: wiz('Manage role library…', 'Manage class library…'),
    hint: wiz('open the role library — view, edit, or add reusable agent-role defaults',
      'open the class library — inscribe, revise, or add hero classes'),
    keywords: 'roles role library edit manage brief defaults permission permissions class library classes'
      + ' reviewer tester lead dev designer po party',
    run: () => openRolesManageModal(),
  });
  // 2e) The policy/library managers that round out the global Groups cog.
  cmds.push({
    icon: '🛡', label: wiz('Manage sandbox profiles…', 'Manage wards…'),
    hint: wiz('open the sandbox-profile library for filesystem, environment, and network launch policy',
      'open the ward library for filesystem, environment, and realm-boundary protections'),
    keywords: 'sandbox profiles profile manage policy filesystem environment network launch security'
      + ' wards ward protections boundaries realm',
    run: () => openSandboxProfilesManageModal(),
  });
  cmds.push({
    icon: '💰', label: wiz('Manage usage limits & budgets…', 'Manage coffer wards & tithes…'),
    hint: wiz('spend caps, quota guards, per-agent usage profiles and their alerts',
      'caps on the coffers, mana guards and per-hero tithes'),
    keywords: 'usage limits budgets budget spend cap alert alerts cost costs quota profile profiles wrap up hard stop block spawn',
    run: () => openUsageRulesManager(),
  });
  // Fleet limits are this node's; a peer's page manages the peer.
  if (!globalThis.__tclaudeRemoteNode?.id) cmds.push({
    icon: '💰', label: wiz('Fleet usage limits…', 'Fleet coffer wards…'),
    hint: wiz('spend caps summed across nodes or a node pool, and what each node shares with you',
      'coffer caps across the realm or a circle of nodes'),
    keywords: 'fleet usage limits budgets spend cap nodes peers pool sub-fleet federation share enforce',
    run: () => openUsageRulesManager({ tab: 'fleet' }),
  });
  cmds.push({
    icon: '⇄', label: wiz('Manage cross-harness spawns…', 'Manage cross-realm summons…'),
    hint: wiz('edit the global source-to-target harness spawn policy',
      'inscribe which source realms may summon heroes into other realms'),
    keywords: 'cross-harness spawns cross harness spawn manage policy matrix source target allow deny delegation'
      + ' cross-realm summons realm realms summon wards hero',
    run: () => openSpawnHarnessPolicy(),
  });
  cmds.push({
    icon: '🔗', label: wiz('Manage inter-group links…', 'Manage arcane channels between parties…'),
    hint: wiz('open the directed communication-link manager for all groups',
      'open the directed missive-channel manager for all parties'),
    keywords: 'inter-group links inter group link manage communication directed edge message groups'
      + ' arcane channels channel parties missive whisper weave',
    run: () => openLinksManager(),
  });
  // One pinned spawn and one pinned message per group, so the operator can
  // launch or compose straight into a named group without first picking it in
  // the dialog. The message command reuses the same group-scoped composer as
  // the per-group cog's "message" / "missive" item.
  for (const g of groups) {
    cmds.push({
      peerView: 'spawn', icon: wiz('＋', '🔮'), label: wiz(`Spawn agent in ${g.name}…`, `Summon a hero into ${g.name}…`),
      hint: wiz('open the spawn dialog pinned to this group',
        'open the summoning circle bound to this party'),
      keywords: 'new agent create spawn launch start group ' + g.name
        + ' summon conjure invoke hero party',
      run: () => { recordGroupInteraction(g.name); openAgentSpawnModal({ groupName: g.name }); },
    });
    cmds.push({
      peerView: 'messaging', icon: wiz('✉', '✒'), label: wiz(`Message group: ${g.name}…`, `Send missive to party: ${g.name}…`),
      hint: wiz('compose a one-shot message to the whole group or a chosen subset',
        'inscribe a missive for every hero in this party or a chosen circle'),
      keywords: 'message send compose notify group broadcast multicast all members agents ' + g.name
        + ' missive inscribe party heroes circle',
      run: () => {
        recordGroupInteraction(g.name);
        openMessageCreateModal({ targetMode: 'group', groupName: g.name });
      },
    });
  }
  // The fused Groups view's other nodes' groups (group@node) take theirs on
  // their node.
  const fused = fusedGroupListing();
  cmds.push(...buildFusedNodeCommands(fused, { wiz }));

  // 3) Theme toggles — regular, slop (🎰) and wizard (🧙). Each command
  //    toggles ONE re-skin on or off (the header icon cycles only
  //    regular ↔ wizard; slop is reached via this, its hotkey or ?slop=1). Labelled by the DESTINATION so each reads as an action, not a
  //    state: "Switch to slop theme" when off, "Switch off slop theme" when
  //    it's the active one. slop and wizard are mutually exclusive, so at
  //    most one shows an "off" label at a time.
  const slopOn = isSlopActive();
  // The theme, the panels and the fold state are this page's own chrome:
  // view commands, runnable on any peer's page.
  cmds.push({
    peerView: 'view', icon: slopOn ? '🤝' : '🎰',
    // slopOn ⇒ slop is the live theme ⇒ wizard is off, so wiz() falls to the
    // plain "Switch off slop theme"; the arcane variant only ever shows from
    // wizard mode, where it reads as leaving the Tower for the casino halls.
    label: slopOn ? 'Switch off slop theme' : wiz('Switch to slop theme', 'Descend to the slop machine'),
    hint: wiz('toggle the dashboard theme', 'trade the Tower for the slot-machine halls'),
    keywords: 'toggle switch theme slop regular vegas casino mode appearance'
      + ' descend leave depart halls machine',
    run: () => toggleSlop(),
  });
  const wizardOn = isWizardActive();
  cmds.push({
    peerView: 'view', icon: wizardOn ? '🤝' : '🧙',
    // wizardOn ⇒ wizard is live ⇒ wiz() returns the arcane "Leave the Tower";
    // when it's off the plain "Switch to wizard theme" shows (the string the
    // wizard HTML guard pins), since labels only re-skin inside wizard mode.
    label: wizardOn ? wiz('Switch off wizard theme', "Leave the Wizard's Tower") : 'Switch to wizard theme',
    hint: wiz('toggle the dashboard theme', 'doff the robes and return to the plain dashboard'),
    keywords: 'toggle switch theme wizard magic arcane dnd dungeon fantasy mode appearance'
      + ' robes tower doff leave depart spellbook',
    run: () => toggleWizard(),
  });

  // 3b) Show / hide the right-side PALETTE DOCK (JOH-374) — the retractable
  //     right-edge panel of drag-to-spawn profiles / templates / roles. A
  //     persisted dashboard-chrome toggle, like the theme switches above; it
  //     owns no state of its own but flips the exact dashPrefs-backed open flag
  //     the dock's edge tab + in-dock collapse button flip (setDockOpen in
  //     dock.js). Offered as a directional PAIR (like the group collapse/expand
  //     commands) rather than one state-gated toggle, so both verbs are always
  //     listed and findable by either word; running the redundant direction is
  //     a harmless idempotent re-apply. Named "right panel" (not "palette") in
  //     the label to avoid colliding with THIS command palette's own name — the
  //     dock's internal/UI vocabulary rides in the keywords + hint. The dock is
  //     Groups-tab-only, so SHOW switches to the Groups tab first (matching the
  //     group-fold commands) so the panel is actually visible; HIDE just flips
  //     the pref (nothing to reveal). In 🧙 mode the dock is the grimoire — Furl
  //     / Unfurl — matching its edge-toggle titles.
  cmds.push({
    peerView: 'view', icon: wiz('◨', '📖'), label: wiz('Show right panel', 'Unfurl the grimoire'),
    hint: wiz('reveal the right-side dock of profiles, templates & roles',
      'unfurl the grimoire of loadouts, circles & classes'),
    keywords: 'show open reveal expand right panel dock sidebar drawer palette rail'
      + ' profiles templates roles'
      + ' unfurl reveal grimoire tome scroll',
    run: () => { gotoGroupsTab(); setDockOpen(true); },
  });
  cmds.push({
    peerView: 'view', icon: wiz('▭', '📕'), label: wiz('Hide right panel', 'Furl the grimoire'),
    hint: wiz('collapse the right-side dock of profiles, templates & roles',
      'roll up the grimoire of loadouts, circles & classes'),
    keywords: 'hide close collapse fold right panel dock sidebar drawer palette rail'
      + ' profiles templates roles'
      + ' furl conceal grimoire tome scroll',
    run: () => setDockOpen(false),
  });

  // 4) Navigation — one command per VISIBLE nav button, reusing its own
  //    click handler (which also triggers each tab's data load). A
  //    CSS-hidden tab (Costs auto-hidden, Vegas off-slop) has no
  //    offsetParent, so it isn't a place the human can currently go.
  for (const btn of $$('nav [data-tab]')) {
    if (btn.offsetParent === null || btn.getAttribute('aria-disabled') === 'true') continue;
    // Each tab carries a plain/wizard label-span pair (dashboard.html). Read
    // whichever the active theme SHOWS so the command reads "Go to Costs"
    // plainly and "Scry the Coffers" in wizard mode — btn.textContent would
    // concatenate BOTH spans (e.g. "Costs💰 Coffers") and any badge count.
    // Fall back to the raw text (minus a trailing badge count) for any button
    // that lacks the pair.
    const wizEl = btn.querySelector('.tab-label-wizard');
    const regEl = btn.querySelector('.tab-label-regular, .tab-label-vegas');
    const name = (wizEl || regEl)
      ? ((isWizardActive() ? (wizEl || regEl) : (regEl || wizEl)).textContent || '').trim()
      : (btn.textContent || '').replace(/\s*\d+\s*$/, '').trim();
    if (!name) continue;
    cmds.push({
      peerView: 'tab', tab: btn.dataset.tab, icon: wiz('⤢', '🪞'), label: wiz(`Go to ${name}`, `Scry the ${name}`),
      hint: wiz('switch tab', 'peer into this chamber of the Tower'),
      keywords: 'tab navigate go open ' + (btn.dataset.tab || '')
        + ' scry peer gaze behold chamber vision',
      run: () => btn.click(),
    });
  }
  // Processes is an Automations subtab, so its nav anchor is always hidden and
  // the loop above skips it; offer it explicitly while the feature is on.
  const processesTab = $$('nav [data-tab="processes"]')[0];
  if (processesTab && !document.body.classList.contains('hide-processes')) {
    cmds.push({
      peerView: 'tab', tab: 'processes', icon: wiz('⤢', '🪞'), label: wiz('Go to Processes', 'Scry the Rites'),
      hint: wiz('Automations › Processes', 'peer into the Labours’ rites'),
      keywords: 'tab navigate go open processes process templates automations'
        + ' scry peer gaze behold chamber vision rites',
      run: () => processesTab.click(),
    });
  }
  if (!document.documentElement.classList.contains('remote-node') && !document.documentElement.classList.contains('scope-fused')) cmds.push({
    icon: wiz('📜', '📜'), label: wiz('Search conversation content', 'Search the scrolls'),
    hint: wiz('find past conversations by the text inside them', 'find a scroll by the words within it'),
    keywords: 'search find grep conversation conversations transcript content text history scrolls',
    run: openConversationSearch,
  });
  // The Groups Both | Agents | Processes focus switch (features.processes only).
  if (processesTab && !document.body.classList.contains('hide-processes') && featureState('groups')?.setFocus) {
    for (const [focus, plain, arcane, words] of [
      ['both', 'Both', 'Whole party', 'both fused combined agents processes heroes rites'],
      ['processes', 'Processes', 'Rites', 'processes process runs graphs rites'],
      ['agents', 'Agents', 'Heroes', 'agents members tables heroes'],
    ]) {
      cmds.push({
        peerView: 'view', icon: wiz('◈', '🔮'), label: wiz(`Groups › ${plain} focus`, `Party lens: ${arcane}`),
        hint: wiz('switch the Groups tab focus', 'turn the party lens'),
        keywords: `groups focus lens view switch toggle ${words}`,
        run: () => openGroupsFocus(focus),
      });
    }
  }
  // The Usage tab's Quota and Costs views were tabs of their own: keep them
  // one command away (and findable as "costs") rather than behind Usage.
  const usageBtn = $('nav [data-tab="usage"]');
  const usageHub = featureState('usage-hub');
  if (usageHub && usageBtn && usageBtn.offsetParent !== null && usageBtn.getAttribute('aria-disabled') !== 'true') {
    for (const [view, plain, wizardly, words] of [
      ['quota', 'Quota', 'Reserves', 'quota limits forecast subscription window reserves mana'],
      ['costs', 'Costs', 'Coffers', 'costs spend money billing dollars coffers gold'],
    ]) {
      if (document.body.classList.contains(`hide-usage-${view}`)) continue;
      cmds.push({
        peerView: 'tab', tab: 'usage', icon: wiz('⤢', '🪞'),
        label: wiz(`Go to Usage › ${plain}`, `Scry the ${wizardly}`),
        hint: wiz('switch tab', 'peer into this chamber of the Tower'),
        keywords: `tab navigate go open usage ${words}`,
        run: () => usageHub.open(view),
      });
    }
  }
  // Diagnostics folds Audit, Logs, Performance and Doctor into one tab; each
  // view keeps a direct command so "logs", "debug" or "doctor" still finds it. Only Audit is
  // offered on a peer view (peerView 'tab' gates it on the Diagnostics tab);
  // the others fall to the default local-only gate there.
  const activityBtn = $$('nav [data-tab="diagnostics"]')[0];
  if (activityBtn && activityBtn.offsetParent !== null) {
    for (const [subtab, plain, arcane, keywords] of [
      ['audit', 'Audit', 'Deeds', 'audit trail log who commands denied chronicle deeds'],
      ['logs', 'Logs', 'Runes', 'logs daemon log output tail errors warnings runes'],
      ['performance', 'Performance', 'Alchemy', 'performance perf debug latency poll timing slow diagnostics alchemy'],
      ['doctor', 'Doctor', 'Apothecary', 'doctor health setup missing tools install sandbox decision apothecary'],
    ]) {
      cmds.push({
        ...(subtab === 'audit' ? { peerView: 'tab', tab: 'diagnostics' } : {}),
        icon: wiz('⤢', '🪞'), label: wiz(`Go to Diagnostics › ${plain}`, `Scry the Chronicle › ${arcane}`),
        hint: wiz('switch tab', 'peer into this chamber of the Tower'),
        keywords: 'tab navigate go open diagnostics activity ' + keywords,
        // From another tab, preselect the view so the one nav click records
        // a single /diagnostics/<subtab> entry; on Diagnostics, the subtab click
        // records it.
        run: () => {
          if (activityBtn.classList.contains('active')) {
            document.querySelector(`#tab-diagnostics .activity-subtab[data-subtab="${subtab}"]`)?.click();
          } else {
            setActivitySubtab(subtab);
            activityBtn.click();
          }
        },
      });
    }
  }
  // The fleet views: their anchors are always hidden (⌘ Fleet opens them),
  // so the per-button loop above never lists them.
  const fleetMap = $$('nav [data-tab="map"]')[0];
  if (fleetMap && document.querySelector('#node-chips-root :is(.has-fleet, .has-map)')) {
    cmds.push({
      peerView: 'view', icon: '⌘', label: 'Go to map',
      hint: 'every linked node and the hub, on one map',
      keywords: 'fleet map nodes peers hub topology overview linked',
      run: () => fleetMap.click(),
    });
  }
  const fleetAdmin = $$('nav [data-tab="fleet-admin"]')[0];
  if (fleetAdmin) {
    cmds.push({
      peerView: 'view', icon: '⚙', label: 'Fleet administration',
      hint: 'set up federation, trust peers, compare fingerprints, hub connection',
      keywords: 'fleet federation setup guide join invite peers trust untrust fingerprint hub disconnect admin nodes',
      run: () => fleetAdmin.click(),
    });
  }

  // 5) Group view — collapse / expand the Groups-tab listing. These
  //    apply to EVERY group (even idle ones — folding an idle group is
  //    valid), unlike the window ops below which need a running member.
  cmds.push({
    peerView: 'view', icon: wiz('⊟', '📕'), label: wiz('Collapse all groups', 'Furl all parties'),
    hint: wiz('fold every group on the Groups tab', "roll up every party's scroll on the Groups tab"),
    keywords: 'collapse fold close all groups view rows'
      + ' furl seal roll scroll parties',
    run: () => setAllGroupsOpen(false),
  });
  cmds.push({
    peerView: 'view', icon: wiz('⊞', '📖'), label: wiz('Expand all groups', 'Unfurl all parties'),
    hint: wiz('unfold every group on the Groups tab', "unroll every party's scroll on the Groups tab"),
    keywords: 'expand unfold open all groups view rows'
      + ' unfurl unseal unroll scroll parties',
    run: () => setAllGroupsOpen(true),
  });
  // The fused view lists every node's groups, by their merged names.
  for (const g of fused ? (fused.groups || []).filter((x) => scribeGroupVisible(x, showOfflineScribes)) : groups) {
    cmds.push({
      peerView: 'view', icon: wiz('⊟', '📕'), label: wiz(`Collapse group: ${g.name}`, `Furl party: ${g.name}`),
      hint: wiz('fold this group', 'roll up this party'),
      keywords: 'collapse fold close group ' + g.name
        + ' furl seal roll scroll party',
      run: () => setGroupOpen(g.name, false),
    });
    cmds.push({
      peerView: 'view', icon: wiz('⊞', '📖'), label: wiz(`Expand group: ${g.name}`, `Unfurl party: ${g.name}`),
      hint: wiz('unfold this group', 'unroll this party'),
      keywords: 'expand unfold open group ' + g.name
        + ' unfurl unseal unroll scroll party',
      run: () => setGroupOpen(g.name, true),
    });
  }

  // 6) Per-group window ops — only groups with at least one running
  //    member (an idle group has no window to focus or hide).
  for (const g of groups) {
    const online = (g.members || []).filter(m => m.online).length;
    if (!online) continue;
    const groupWebWindowTargets = webWindowTargets(g.members);
    const n = `${online} window${online === 1 ? '' : 's'}`;
    const nPortals = `${online} scrying portal${online === 1 ? '' : 's'}`;
    cmds.push({
      peerView: 'own', icon: wiz('⏏', '🌫'), label: wiz(`Hide group: ${g.name}`, `Veil party: ${g.name}`),
      hint: wiz(`hide ${n}`, `draw the veil over ${nPortals}`),
      keywords: 'hide unfocus group windows ' + g.name
        + ' veil conceal cloak portal scrying party',
      run: () => { recordGroupInteraction(g.name); bulkWindowOp(
        { direction: 'unfocus', scope: 'group', group: g.name },
        `hide group ${g.name}`,
        { webTerminal: preferWebTerminal, targets: groupWebWindowTargets }); },
    });
    cmds.push({
      peerView: 'own', icon: wiz('◎', '👁'), label: wiz(`Focus group: ${g.name}`, `Reveal party: ${g.name}`),
      hint: wiz(`raise ${n}`, `conjure ${nPortals}`),
      keywords: 'focus show group windows ' + g.name
        + ' reveal behold conjure portal scrying party',
      run: () => { recordGroupInteraction(g.name); bulkWindowOp(
        { direction: 'focus', scope: 'group', group: g.name },
        `focus group ${g.name}`,
        { webTerminal: preferWebTerminal, targets: groupWebWindowTargets }); },
    });
  }

  // 6b) Per-group power control — shut down / power on every agent in a
  //     group. The batch analog of the per-group window ops above, and
  //     the per-group counterpart of the global commands. shutdownScope
  //     counts RUNNING members (`g.online`, exactly what it confirms),
  //     powerOnScope counts OFFLINE members; each variant gates on its
  //     own live count so neither lists a no-op.
  for (const g of groups) {
    const onlineG = g.online || 0;
    if (onlineG) {
      const plural = onlineG === 1 ? '' : 's';
      cmds.push({
        groupGrant: { act: 'shutdown-group', group: g.name },
        icon: wiz('⏻', '🌙'), label: wiz(`Shut down group: ${g.name}`, `Slumber party: ${g.name}`),
        hint: wiz(`stop ${onlineG} running agent${plural} (resumable; no data deleted)`,
          `lull ${onlineG} channeling hero${plural && 'es'} into slumber (rousable; nothing is lost)`),
        keywords: 'shutdown shut down stop kill power off halt group batch ' + g.name
          + ' slumber sleep rest lull dormant party heroes',
        run: () => { recordGroupInteraction(g.name); shutdownScope('group', g.name); },
      });
    }
    const offlineG = (g.members || []).filter(m => !m.online).length;
    if (offlineG) {
      const plural = offlineG === 1 ? '' : 's';
      cmds.push({
        groupGrant: { act: 'power-on-group', group: g.name },
        icon: wiz('⏼', '✨'), label: wiz(`Power on group: ${g.name}`, `Awaken party: ${g.name}`),
        hint: wiz(`resume ${offlineG} offline agent${plural} onto their conversations`,
          `rouse ${offlineG} slumbering hero${plural && 'es'} back onto their scrolls`),
        keywords: 'power on start resume wake boot up group batch ' + g.name
          + ' awaken rouse stir revive kindle party heroes',
        run: () => { recordGroupInteraction(g.name); powerOnScope('group', g.name); },
      });
    }
  }

  // 7) Per-agent window ops — RUNNING agents only.
  for (const a of (snap.agents || [])) {
    if (!a.online) continue;
    const label = a.title || (a.conv_id || '').slice(0, 8);
    // Route by the rotation-immune stable agent_id (conv-id fallback for a
    // pre-identity row); the server resolves it via agent.ResolveSelector.
    const sel = a.agent_id || a.conv_id;
    const page = globalThis.__tclaudeRemoteNode;
    if (page?.id) {
      // On a peer's page an agent's terminal is the peer's, opened here in the
      // browser terminal, as its row's jump does; the peer authorises it.
      if (!SAFE_AGENT.test(a.agent_id || '')) continue;
      const name = pageNodeName(page);
      cmds.push({
        id: `agent-focus:${sel}`,
        node: page.id, icon: wiz('◎', '👁'), label: wiz(`Open terminal: ${label}`, `Reveal hero: ${label}`),
        hint: wiz(`open this agent's terminal on ${name} here`, `conjure this hero's scrying portal from ${name}`),
        keywords: 'focus show jump open terminal window agent ' + label + ' ' + (a.conv_id || '')
          + ' reveal behold conjure portal scrying hero',
        run: () => void openRemote('openRemoteTerminal', { instance: page.id, agent: a.agent_id, peerLabel: name, label }),
      });
      continue;
    }
    cmds.push({
      id: `agent-focus:${sel}`,
      peerView: 'own',
      icon: wiz('◎', '👁'), label: wiz(`Focus window: ${label}`, `Reveal hero: ${label}`),
      hint: wiz("raise / open this agent's terminal", "conjure this hero's scrying portal"),
      keywords: 'focus show jump bring up window agent ' + label + ' ' + (a.conv_id || '')
        + ' reveal behold conjure portal scrying hero',
      run: () => jumpAgent(sel, label, snap.default_terminal === 'web', a.state?.harness || '', toast),
    });
    cmds.push({
      id: `agent-hide:${sel}`,
      peerView: 'own',
      icon: wiz('⏏', '🌫'), label: wiz(`Hide window: ${label}`, `Veil hero: ${label}`),
      hint: wiz("detach this agent's terminal", "draw the veil over this hero's scrying portal"),
      keywords: 'hide detach window agent ' + label + ' ' + (a.conv_id || '')
        + ' veil conceal cloak portal scrying hero',
      run: () => hideAgent(sel, label),
    });
  }

  // 7b) Per-agent power control — stop a running agent or resume an
  //     offline one. The single-agent analog of the per-agent window
  //     ops above, mirroring the per-row status-dot toggle: a stop opens
  //     the Preact transaction (Cancel / Soft exit / Force kill), a
  //     resume fires straight away (non-destructive). Each agent is
  //     listed for its CURRENT state only — online → Stop, offline →
  //     Resume — so the palette never offers the wrong verb.
  for (const a of (snap.agents || [])) {
    const label = a.title || (a.conv_id || '').slice(0, 8);
    const sel = a.agent_id || a.conv_id;
    if (a.online) {
      cmds.push({
        id: `agent-stop:${sel}`,
        grant: { act: 'dot-toggle', agent: a.agent_id || '', dataset: { online: '1' } },
        icon: wiz('⏻', '🌙'), label: wiz(`Stop agent: ${label}`, `Slumber hero: ${label}`),
        hint: wiz('soft-exit, then force-kill if it does not exit (resumable)',
          'lull into slumber: a gentle /exit, then a firmer hand if it lingers (rousable)'),
        keywords: 'stop shutdown shut down kill power off halt agent ' + label + ' ' + (a.conv_id || '')
          + ' slumber sleep rest lull dormant hero',
        run: () => openShutdownAgentDialog(sel, label),
      });
    } else if (!a.external_steward) {
      cmds.push({
        id: `agent-resume:${sel}`,
        grant: { act: 'dot-toggle', agent: a.agent_id || '', dataset: { online: '0' } },
        icon: wiz('⏼', '✨'), label: wiz(`Resume agent: ${label}`, `Awaken hero: ${label}`),
        hint: wiz('restart in a fresh tmux session, resumed onto its conversation',
          'rouse into a fresh tmux session, resumed onto its scroll'),
        keywords: 'resume start power on wake boot up agent ' + label + ' ' + (a.conv_id || '')
          + ' awaken rouse stir revive kindle hero',
        run: () => resumeAgentReq(sel, label),
      });
    }
  }

  // 8) Status-filtered bulk retire. Each idle/offline cohort gets separate
  //    commands for the whole active roster (all real groups + Ungrouped),
  //    the virtual Ungrouped group, and each real group. Every command opens
  //    the same explicit-selection preview and is offered only when its
  //    snapshot scope has a live match.
  for (const status of ['idle', 'offline']) {
    const allCount = new Set((snap.agents || [])
      .filter(agent => status === 'offline'
        ? !agent.online
        : agent.online && agent.state?.status === status)
      .map(agent => agent.conv_id)
      .filter(Boolean)).size;
    if (allCount) {
      const plural = allCount === 1 ? '' : 's';
      cmds.push({
        icon: wiz('♻', '🪄'),
        label: wiz(`Retire ${status} agents across all groups`, `Banish ${status} heroes across all parties`),
        hint: wiz(`preview + demote ${allCount} ${status} agent${plural} across every group, including Ungrouped`,
          `preview + banish ${allCount} ${status} hero${plural && 'es'} across every party, including the Unbound`),
        keywords: 'retire demote cleanup remove tidy bulk ' + status
          + ' agents all groups global everywhere fleet ungrouped'
          + ' banish exile dismiss heroes all parties tower unbound',
        run: () => openRetireAllPreview(status),
      });
    }

    const ungroupedN = new Set((snap.ungrouped || [])
      .filter(agent => status === 'offline'
        ? !agent.online
        : agent.online && agent.state?.status === status)
      .map(agent => agent.conv_id)
      .filter(Boolean)).size;
    if (ungroupedN) {
      const plural = ungroupedN === 1 ? '' : 's';
      cmds.push({
        icon: wiz('♻', '🪄'),
        label: wiz(`Retire ${status} agents in Ungrouped`, `Banish ${status} heroes in Unbound`),
        hint: wiz(`preview + demote ${ungroupedN} ${status} ungrouped agent${plural} to plain conversations`,
          `preview + banish ${ungroupedN} ${status} unbound hero${plural && 'es'} back to plain scrolls`),
        keywords: 'retire demote cleanup remove tidy bulk ' + status
          + ' agents ungrouped no group groupless loose'
          + ' banish exile dismiss heroes unbound unattached',
        run: () => openRetireUngroupedPreview(status),
      });
    }

    for (const g of groups) {
      const group = (snap.groups || []).find(candidate => candidate.name === g.name);
      const n = new Set((group?.members || [])
        .filter(member => status === 'offline'
          ? !member.online
          : member.online && member.state?.status === status)
        .map(member => member.conv_id)
        .filter(Boolean)).size;
      if (!n) continue;
      const plural = n === 1 ? '' : 's';
      cmds.push({
        icon: wiz('♻', '🪄'), label: wiz(`Retire ${status} agents in ${g.name}`, `Banish ${status} heroes in ${g.name}`),
        hint: wiz(`preview + demote ${n} ${status} agent${plural} to plain conversations`,
          `preview + banish ${n} ${status} hero${plural && 'es'} back to plain scrolls`),
        keywords: 'retire demote cleanup remove tidy bulk ' + status + ' agents group ' + g.name
          + ' banish exile dismiss heroes party',
        run: () => { recordGroupInteraction(g.name); openRetirePreview(g.name, status); },
      });
    }
  }

  // 8b) Worktree cleanup. The all-groups command scans the union of every
  //     scannable group's repos; the per-group commands retain the narrower
  //     scope. Both open the same explicit-selection preview.
  const scannableGroups = groups.filter(g =>
    (g.default_cwd && g.default_cwd.trim()) || (g.members && g.members.length));
  if (scannableGroups.length) {
    cmds.push({
      icon: wiz('🧹', '🍂'), label: wiz('Cleanup worktrees across all groups', 'Prune stray branches across all parties'),
      hint: wiz('scan every group repo for stale worktrees and remove the ones you pick',
        'scan every party grove for withered branches and prune the ones you pick'),
      keywords: 'cleanup worktree worktrees all groups global tidy remove stale orphan branch git '
        + 'prune withered grove branches parties',
      run: () => openWorktreeCleanup(),
    });
  }

  // Per-group worktree cleanup — "Cleanup worktrees in <group>". The
  //     repo-wide janitor: scans the group's default dir (∪ its agents'
  //     history dirs) for stale git worktrees and opens a preview modal
  //     to pick which to remove. Offered whenever the group has somewhere
  //     to scan (a default dir or any members); the modal itself reports
  //     "nothing to clean up" when a scan comes back empty, so the gate is
  //     deliberately loose rather than firing a probe per group here.
  for (const g of scannableGroups) {
    cmds.push({
      icon: wiz('🧹', '🍂'), label: wiz(`Cleanup worktrees in ${g.name}`, `Prune stray branches in ${g.name}`),
      hint: wiz("scan this group's repo for stale worktrees and remove the ones you pick",
        "scan this party's grove for withered branches and prune the ones you pick"),
      keywords: 'cleanup worktree worktrees tidy remove stale orphan branch git group ' + g.name
        + ' prune withered grove branches party',
      run: () => { recordGroupInteraction(g.name); openWorktreeCleanup(g.name); },
    });
  }

  // 9) Per-agent retire — "Retire agent: <name>". Demotes one agent back
  //    to a plain conversation via the same confirm + flags the per-row
  //    ⚙ Retire button uses (openRetireAgentDialog). Listed for every
  //    agent on the roster, online OR offline — retire is valid on an
  //    offline agent too (there is just no pane to soft-exit).
  for (const a of (snap.agents || [])) {
    const label = a.title || (a.conv_id || '').slice(0, 8);
    const status = a.online ? ((a.state && a.state.status) || 'online') : 'offline';
    cmds.push({
      id: `agent-retire:${a.conv_id}`,
      grant: { act: 'retire-agent', agent: a.agent_id || '' },
      icon: wiz('♻', '🪄'), label: wiz(`Retire agent: ${label}`, `Banish hero: ${label}`),
      hint: wiz(`demote to a plain conversation (${status})`, `banish back to a plain scroll (${status})`),
      keywords: 'retire demote cleanup remove agent ' + label + ' ' + (a.conv_id || '') + ' ' + status
        + ' banish exile dismiss hero',
      // Retire stays conv-keyed (not agent_id): the server's dangling-agent
      // recovery only triggers for a UUID-shaped selector that fails to
      // resolve, so a stable agent_id would silently demote a dangling
      // orphan instead of offering to remove it (JOH-322).
      // On a peer's page its route takes the stable agent_id only.
      run: () => (globalThis.__tclaudeRemoteNode?.id && a.agent_id ? openRetireAgentDialog(a.agent_id, label) : openRetireAgentDialog(a.conv_id, label)),
    });
  }

  // Feature-owned contextual commands join the same global surface. Providers
  // are evaluated last and live, so opening the palette in the process editor
  // sees its current selection without the shell owning editor state.
  cmds.push(...buildRegisteredCommands({ snapshot: snap }));
  return limitPeerViewCommands(cmds, snap);
}
