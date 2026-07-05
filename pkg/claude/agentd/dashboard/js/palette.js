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
//                          per-agent stop/resume (stopAgentReq /
//                          resumeAgentReq) — the same calls the dashboard's
//                          Shutdown/Power-on buttons and status dots make
//   - retire             → the per-agent / per-group demote-to-conversation
//                          flows
//
// So the palette only adds a fast keyboard entry point to the window
// hide/show, power control, retire + navigation the dashboard already
// does; it owns no agent state of its own and reads the live roster
// fresh from lastSnapshot each time it opens. NB power control STOPS or
// RESUMES agent processes, unlike the window ops which only detach/raise
// terminals — see section 1b.
//
// It is a .modal-overlay so it picks up the shared backdrop AND pauses
// the 2s auto-refresh while open (refreshSuspended() keys on
// .modal-overlay.show), which keeps a re-render from yanking focus out
// of the search box mid-type.
//
// Trigger: Ctrl/Cmd-K (claimed with preventDefault; pressing it again
// closes). Esc or a backdrop click closes. ↑/↓ move the selection one
// row (wrapping), PageUp/PageDown jump a viewport-worth (clamping at the
// ends), Enter runs it, typing filters.

import { $, $$, esc } from './helpers.js';
import { lastSnapshot, webTerminalDefault } from './dashboard.js';
import {
  toast, openWindowModal,
  retireAgentInteractive, openRetirePreview, openRetireUngroupedPreview, openDeleteRetiredPreview,
  countGroupMembersByStatus, countUngroupedAgents,
  openWorktreeCleanup,
  shutdownScope, powerOnScope, shutdownConfirm, stopAgentReq, resumeAgentReq,
} from './refresh.js';
import { openAgentSpawnModal } from './modal-spawn.js';
import { openProfilesManageModal } from './modal-profiles.js';
import { openRolesManageModal } from './modal-roles.js';
import { openGroupCreateModal } from './modal-message.js';
import { toggleSlop, isSlopActive, toggleWizard, isWizardActive } from './slop.js';
import { rankCommands } from './palette-score.js';
import { recordGroupInteraction, lastInteractedGroup } from './last-group.js';
import { setDockOpen } from './dock.js';
import { scribeVisible } from './virtual-groups.js';
import { closeTerminalsForConvs, closeTerminalsForWindowOp, focusTerminalForConv, openWebWindowPane } from './terminals-tab.js';

const MODAL_ID = 'command-palette-modal';

// Wizard-mode copy — the placeholder + empty-state line get an arcane
// re-flavour when body.wizard is set (the palette becomes "The Spellbook").
// The placeholder can't be swapped in pure CSS (it's an attribute, not
// content), so openPalette sets it per-theme on each open; the regular text
// is captured from the HTML at bind time so the two never drift.
const WIZARD_PLACEHOLDER =
  'Speak an incantation…  (banish a familiar · scry a tab · summon…)';
const WIZARD_EMPTY = 'No such incantation in this tome';
let defaultPlaceholder = '';

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

// Module state for the current open. commands is the full list built at
// open time; filtered is the current query's subset; selected indexes
// into filtered. Cached element refs are filled in bindCommandPalette.
let commands = [];
let filtered = [];
let selected = 0;
let overlay = null;
let input = null;
let list = null;
// The element focused when the palette opened, so closing returns focus
// there (the 🔍 button, or wherever the hotkey was pressed) instead of
// dropping it to <body>.
let lastFocus = null;

function isOpen() {
  return overlay !== null && overlay.classList.contains('show');
}

// -- POST helpers — the same endpoints the windows modal / eye buttons
//    hit. Each toasts its own outcome (matching the existing modal's
//    wording) and never touches an agent process: window-only.

async function bulkWindowOp(payload, what) {
  let r;
  try {
    r = await fetch('/api/agent-windows', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
  } catch (e) {
    toast(`${what}: request failed: ${(e && e.message) || e}`, true);
    return;
  }
  if (!r.ok) { toast(`${what}: ${await r.text()}`, true); return; }
  const out = await r.json().catch(() => null);
  if (!out) { toast(`${what}: done`); return; }
  if (payload.direction === 'focus') {
    const extra = out.failed ? `, ${out.failed} failed` : '';
    toast(`${what}: ${out.focused} focused${extra}`, out.failed > 0);
  } else {
    // Close the multiplexer panes of exactly the agents this bulk unfocus
    // detached (out.agents carries the per-agent outcome), so their terminal
    // tabs don't linger showing "disconnected". Precise — panes for agents
    // outside the op's scope are untouched.
    closeTerminalsForWindowOp(out.agents);
    const extra = out.failed ? `, ${out.failed} failed` : '';
    toast(`${what}: ${out.detached} detached${extra}`, out.failed > 0);
  }
}

async function jumpAgent(conv, label) {
  // If this agent already has an open web terminal / window pane in the
  // Terminals tab, jump to THAT instead of raising a native OS window — mirrors
  // the per-agent 'jump' row action.
  if (focusTerminalForConv([conv])) { toast(`focused: ${label}`); return; }
  // With web terminals as the default (config dashboard.default_terminal =
  // "web"), open the agent's live session as a browser pane rather than raising
  // a native OS window — same as the per-agent 'jump' row action.
  if (webTerminalDefault()) { openWebWindowPane(conv, label); toast(`focused: ${label}`); return; }
  try {
    const r = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
      method: 'POST', credentials: 'same-origin',
    });
    if (!r.ok) { toast(`focus ${label}: ${await r.text()}`, true); return; }
    toast(`focused: ${label}`);
  } catch (e) {
    toast(`focus ${label}: ${(e && e.message) || e}`, true);
  }
}

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

// stopAgentInteractive mirrors the per-row status-dot toggle's STOP path
// (row-actions 'dot-toggle'): pop the 3-way shutdownConfirm (Cancel /
// Soft exit / Force kill) and, unless cancelled, stop with the chosen
// force flag via the shared stopAgentReq. Resume needs no wrapper — it
// is non-destructive, so the palette calls resumeAgentReq directly.
async function stopAgentInteractive(conv, label) {
  const choice = await shutdownConfirm({ label });
  if (!choice) return; // Cancel
  await stopAgentReq(conv, label, choice === 'force');
}

// -- Group fold helpers — collapse/expand the Groups-tab listing. Each
//    group renders as <details data-group-key>; assigning .open fires a
//    native toggle event that bindDetailsPersistence (refresh.js) catches,
//    so the state sticks across the 2s re-render. We switch to the Groups
//    tab first so the change is actually visible.

function gotoGroupsTab() {
  const btn = $('nav button[data-tab="groups"]');
  if (btn) btn.click();
}

function setGroupOpen(name, open) {
  gotoGroupsTab();
  const d = $(`#tab-groups details[data-group-key="${CSS.escape(name)}"]`);
  if (!d) { toast(`group ${name}: not listed on the Groups tab`, true); return; }
  // Record only once we know the fold will actually happen — symmetric with
  // the modal record sites, which stamp after their success point.
  recordGroupInteraction(name);
  d.open = open; // fires toggle → bindDetailsPersistence persists the state
  const sum = d.querySelector('summary');
  if (sum) sum.scrollIntoView({ block: 'nearest' });
}

function setAllGroupsOpen(open) {
  gotoGroupsTab();
  const all = $$('#tab-groups details[data-group-key]');
  for (const d of all) d.open = open;
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
function buildCommands() {
  const snap = lastSnapshot || {};
  // Mirror the Groups tab's default-hidden treatment of the daemon-created
  // scribe's system group (snapshot `scribe` flag): its per-group commands
  // (spawn / collapse / expand / hide / focus / shut down / retire in
  // circle-scribe) are kept out of the palette until the human ticks "show
  // circle-scribe" in the view popover — so hiding it there hides it here too.
  // Per-AGENT commands (§7/§7b, sourced from snap.agents) are unaffected: the
  // scribe agent still shows in the Agents tab, so it stays focusable/stoppable
  // by name.
  const groups = (snap.groups || []).filter(g => scribeVisible() || !g.scribe);
  const cmds = [];

  // 1) Global window ops — "hide all windows" (and its inverse), plus
  //    the modal for picking an arbitrary subset. In 🧙 mode a terminal
  //    window is a familiar's "scrying portal", so hide → Veil, focus →
  //    Reveal (arcane presented label; the plain keywords stay searchable).
  cmds.push({
    icon: wiz('⏏', '🌫'), label: wiz('Hide all windows', 'Veil all familiars'),
    hint: wiz('detach every agent terminal window (agents keep running)',
      "draw the veil over every familiar's scrying portal (they keep channeling)"),
    keywords: 'hide unfocus all windows declutter detach panic minimize'
      + ' veil conceal cloak shroud portal scrying vision familiars',
    run: () => bulkWindowOp({ direction: 'unfocus', scope: 'all' },
      'hide all windows'),
  });
  cmds.push({
    icon: wiz('◎', '👁'), label: wiz('Focus all windows', 'Reveal all familiars'),
    hint: wiz('raise / open a terminal window for every running agent',
      'conjure a scrying portal for every channeling familiar'),
    keywords: 'show all windows raise focus bring up'
      + ' reveal behold conjure portal scrying vision familiars',
    run: () => bulkWindowOp({ direction: 'focus', scope: 'all' },
      'focus all windows'),
  });
  cmds.push({
    icon: wiz('▦', '👁'), label: wiz('Pick windows to focus / hide…', 'Choose familiars to reveal / veil…'),
    hint: wiz('open the window modal to choose a subset',
      'open the scrying circle to choose which portals to reveal or veil'),
    keywords: 'windows subset choose select modal some'
      + ' reveal veil portals scrying familiars',
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
  const onlineAll = (snap.agents || []).filter(a => a.online).length;
  const offlineAll = (snap.agents || []).filter(a => !a.online).length;
  if (onlineAll) {
    const plural = onlineAll === 1 ? '' : 's';
    cmds.push({
      icon: wiz('⏻', '🌙'), label: wiz('Shut down all agents', 'Slumber all familiars'),
      hint: wiz(`stop ${onlineAll} running agent${plural} (resumable; no data deleted)`,
        `lull ${onlineAll} channeling familiar${plural} into slumber (rousable; nothing is lost)`),
      keywords: 'shutdown shut down stop kill power off halt all agents global everything batch'
        + ' slumber sleep rest lull dormant quell still familiars',
      run: () => shutdownScope('all', null),
    });
  }
  if (offlineAll) {
    const plural = offlineAll === 1 ? '' : 's';
    cmds.push({
      icon: wiz('⏼', '✨'), label: wiz('Power on all agents', 'Awaken all familiars'),
      hint: wiz(`resume ${offlineAll} offline agent${plural} onto their conversations`,
        `rouse ${offlineAll} slumbering familiar${plural} back onto their scrolls`),
      keywords: 'power on start resume wake boot up all agents global everything batch'
        + ' awaken rouse stir revive kindle familiars',
      run: () => powerOnScope('all', null),
    });
  }

  // 1c) Global delete-retired — "Delete retired agents…". The human-driven
  //     sibling of the timed agent.retired_cleanup auto-sweep (JOH-269):
  //     opens a PREVIEW modal (openDeleteRetiredPreview) listing every
  //     retired agent, all ticked, with live title/age filters and a
  //     per-row opt-out, then POSTs the explicit ticked-AND-visible list to
  //     /api/cleanup/agents {mode:"delete"}. 🗑 distinguishes it from the ♻
  //     retire commands. Gated on ≥1 retired agent so the palette never
  //     offers a no-op.
  // retired[] is windowed in the snapshot now — the true count lives in the
  // pagination envelope. (The modal itself fetches the full list on open.)
  const retiredCount = (snap.paging && snap.paging.retired && snap.paging.retired.total) || 0;
  if (retiredCount) {
    const plural = retiredCount === 1 ? '' : 's';
    cmds.push({
      icon: wiz('🗑', '🔥'), label: wiz('Delete retired agents…', 'Dispel banished familiars…'),
      hint: wiz(`preview + permanently delete any of ${retiredCount} retired agent${plural} (cannot be undone)`,
        `preview + forever dispel any of ${retiredCount} banished familiar${plural} (cannot be undone)`),
      keywords: 'delete purge retired cleanup remove wipe agents'
        + ' dispel banished obliterate destroy erase vanquish incinerate familiars',
      run: () => openDeleteRetiredPreview(),
    });
  }

  // 1d) Global retire-ungrouped — "Retire ungrouped agents…". The
  //     cross-group cleanup twin of the per-group retire (section 8):
  //     ungrouped agents belong to no group, so there is no group retire
  //     command to reach them. Opens a PREVIEW modal
  //     (openRetireUngroupedPreview) listing every agent in no group —
  //     online and offline alike — all ticked, with live title/id filters
  //     and a per-row opt-out, then POSTs the explicit list to
  //     /api/cleanup/agents {mode:"retire"}. ♻ marks it a (reinstatable)
  //     retire, distinct from the 🗑 delete-retired above it. Gated on ≥1
  //     ungrouped agent so the palette never offers a no-op. In 🧙 mode
  //     these loose agents are "unbound familiars" and retire → banish.
  const ungroupedCount = countUngroupedAgents();
  if (ungroupedCount) {
    const plural = ungroupedCount === 1 ? '' : 's';
    cmds.push({
      icon: wiz('♻', '🪄'), label: wiz('Retire ungrouped agents…', 'Banish unbound familiars…'),
      hint: wiz(`preview + demote ${ungroupedCount} agent${plural} that are in no group to plain conversations`,
        `preview + banish ${ungroupedCount} unbound familiar${plural} back to plain scrolls`),
      keywords: 'retire demote cleanup remove tidy bulk ungrouped no group groupless loose solo orphan stray agents'
        + ' banish exile dismiss unbound loose unattached familiars',
      run: () => openRetireUngroupedPreview(),
    });
  }

  // 1e) Create a new group. Opens the very dialog the Groups-tab
  //     "+ new group" button opens (openGroupCreateModal → POST
  //     /api/groups on submit) — a thin surface over the existing flow,
  //     no new behaviour. A headline "create" action that pairs with the
  //     spawn commands just below: a fresh group is where you then summon
  //     familiars. Unconditional — you can always form a new group. In 🧙
  //     mode the group-create dialog already titles itself "⚔ Form a
  //     party", so the command reads the same (icon ⚔ / label "Form a
  //     party…") to match the button it fronts.
  cmds.push({
    icon: wiz('＋', '⚔'), label: wiz('Create new group…', 'Form a party…'),
    hint: wiz('open the new-group dialog',
      'gather a fresh band — muster a new adventuring party'),
    keywords: 'new group create make add team squad'
      + ' party form fellowship warband adventuring muster gather assemble guild',
    run: () => openGroupCreateModal(),
  });

  // 2) Spawn a new agent. The plain command DEFAULTS the dialog's group
  //    picker to the group the operator last interacted with (folded /
  //    spawned / palette-touched) but leaves it changeable; the per-group
  //    variants below PIN a specific group each (hiding the picker). Both
  //    reuse the existing spawn modal — `defaultGroup` preselects without
  //    forcing, `groupName` fixes + hides the picker.
  const lastGroup = lastInteractedGroup();
  const lastGroupLive = groups.some(g => g.name === lastGroup);
  cmds.push({
    icon: wiz('＋', '🔮'), label: wiz('Spawn agent…', 'Summon a familiar…'),
    hint: lastGroupLive
      ? wiz(`open the spawn dialog (defaults to ${lastGroup} — last used)`,
        `open the summoning circle (defaults to ${lastGroup} — last used)`)
      : wiz('open the spawn dialog', 'open the summoning circle'),
    keywords: 'new agent create spawn launch start'
      + ' summon conjure invoke call forth familiar'
      + (lastGroupLive ? ' ' + lastGroup : ''),
    run: () => openAgentSpawnModal(lastGroupLive ? { defaultGroup: lastGroup } : {}),
  });
  // 2b) Manage spawn profiles — open the profiles overlay where the saved
  //     spawn profiles (reusable bundles of the spawn dialog's fields) are
  //     listed to view / edit / delete / add. Reuses the very overlay the
  //     Groups cog's "⧉ profiles…" entry opens (openProfilesManageModal); the
  //     palette just adds a keyboard entry point, owning no state of its own.
  //     In 🧙 mode the whole profiles vocabulary re-letters to "patterns"
  //     (a saved spawn recipe is a "familiar pattern"), so it presents as
  //     "Edit familiar patterns…"; the plain words stay searchable in the
  //     keywords.
  cmds.push({
    icon: wiz('⧉', '📜'), label: wiz('Edit profiles…', 'Edit familiar patterns…'),
    hint: wiz('open the spawn-profiles manager — view, edit, or add reusable spawn recipes',
      'open the familiar-pattern grimoire — inscribe, revise, or weave summoning recipes'),
    keywords: 'profiles profile edit manage spawn recipe recipes bundle preset presets defaults'
      + ' patterns pattern familiar weave inscribe grimoire loom blueprint',
    run: () => openProfilesManageModal(),
  });
  // 2c) Manage the role library — the named, reusable agent-role defaults a
  //     template roster agent references (a canonical brief + a default launch
  //     shape + a default permission set). Reuses the overlay the Groups cog's
  //     "⧉ roles…" entry opens (openRolesManageModal). In 🧙 mode roles are the
  //     party's "classes".
  cmds.push({
    icon: wiz('⧉', '🎭'), label: wiz('Edit roles…', 'Edit classes…'),
    hint: wiz('open the role library — view, edit, or add reusable agent-role defaults',
      'open the class library — inscribe, revise, or add familiar classes'),
    keywords: 'roles role edit manage library brief defaults permission permissions class classes'
      + ' reviewer tester lead dev designer po party',
    run: () => openRolesManageModal(),
  });
  // One pinned spawn per group, so the operator can launch straight into a
  // named group without first picking it in the dialog.
  for (const g of groups) {
    cmds.push({
      icon: wiz('＋', '🔮'), label: wiz(`Spawn agent in ${g.name}…`, `Summon a familiar into ${g.name}…`),
      hint: wiz('open the spawn dialog pinned to this group',
        'open the summoning circle bound to this party'),
      keywords: 'new agent create spawn launch start group ' + g.name
        + ' summon conjure invoke familiar party',
      run: () => { recordGroupInteraction(g.name); openAgentSpawnModal({ groupName: g.name }); },
    });
  }

  // 3) Theme toggles — regular, slop (🎰) and wizard (🧙). Each command
  //    toggles ONE re-skin on or off (the header 🤝/🎰/🧙 icon cycles all
  //    three). Labelled by the DESTINATION so each reads as an action, not a
  //    state: "Switch to slop theme" when off, "Switch off slop theme" when
  //    it's the active one. slop and wizard are mutually exclusive, so at
  //    most one shows an "off" label at a time.
  const slopOn = isSlopActive();
  cmds.push({
    icon: slopOn ? '🤝' : '🎰',
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
    icon: wizardOn ? '🤝' : '🧙',
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
    icon: wiz('◨', '📖'), label: wiz('Show right panel', 'Unfurl the grimoire'),
    hint: wiz('reveal the right-side dock of profiles, templates & roles',
      'unfurl the grimoire of patterns, circles & classes'),
    keywords: 'show open reveal expand right panel dock sidebar drawer palette rail'
      + ' profiles templates roles'
      + ' unfurl reveal grimoire tome scroll',
    run: () => { gotoGroupsTab(); setDockOpen(true); },
  });
  cmds.push({
    icon: wiz('▭', '📕'), label: wiz('Hide right panel', 'Furl the grimoire'),
    hint: wiz('collapse the right-side dock of profiles, templates & roles',
      'roll up the grimoire of patterns, circles & classes'),
    keywords: 'hide close collapse fold right panel dock sidebar drawer palette rail'
      + ' profiles templates roles'
      + ' furl conceal grimoire tome scroll',
    run: () => setDockOpen(false),
  });

  // 4) Navigation — one command per VISIBLE nav button, reusing its own
  //    click handler (which also triggers each tab's data load). A
  //    CSS-hidden tab (Costs auto-hidden, Vegas off-slop) has no
  //    offsetParent, so it isn't a place the human can currently go.
  for (const btn of $$('nav button')) {
    if (btn.offsetParent === null) continue;
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
      icon: wiz('⤢', '🪞'), label: wiz(`Go to ${name}`, `Scry the ${name}`),
      hint: wiz('switch tab', 'peer into this chamber of the Tower'),
      keywords: 'tab navigate go open ' + (btn.dataset.tab || '')
        + ' scry peer gaze behold chamber vision',
      run: () => btn.click(),
    });
  }

  // 5) Group view — collapse / expand the Groups-tab listing. These
  //    apply to EVERY group (even idle ones — folding an idle group is
  //    valid), unlike the window ops below which need a running member.
  cmds.push({
    icon: wiz('⊟', '📕'), label: wiz('Collapse all groups', 'Furl all parties'),
    hint: wiz('fold every group on the Groups tab', "roll up every party's scroll on the Groups tab"),
    keywords: 'collapse fold close all groups view rows'
      + ' furl seal roll scroll parties',
    run: () => setAllGroupsOpen(false),
  });
  cmds.push({
    icon: wiz('⊞', '📖'), label: wiz('Expand all groups', 'Unfurl all parties'),
    hint: wiz('unfold every group on the Groups tab', "unroll every party's scroll on the Groups tab"),
    keywords: 'expand unfold open all groups view rows'
      + ' unfurl unseal unroll scroll parties',
    run: () => setAllGroupsOpen(true),
  });
  for (const g of groups) {
    cmds.push({
      icon: wiz('⊟', '📕'), label: wiz(`Collapse group: ${g.name}`, `Furl party: ${g.name}`),
      hint: wiz('fold this group', 'roll up this party'),
      keywords: 'collapse fold close group ' + g.name
        + ' furl seal roll scroll party',
      run: () => setGroupOpen(g.name, false),
    });
    cmds.push({
      icon: wiz('⊞', '📖'), label: wiz(`Expand group: ${g.name}`, `Unfurl party: ${g.name}`),
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
    const n = `${online} window${online === 1 ? '' : 's'}`;
    const nPortals = `${online} scrying portal${online === 1 ? '' : 's'}`;
    cmds.push({
      icon: wiz('⏏', '🌫'), label: wiz(`Hide group: ${g.name}`, `Veil party: ${g.name}`),
      hint: wiz(`hide ${n}`, `draw the veil over ${nPortals}`),
      keywords: 'hide unfocus group windows ' + g.name
        + ' veil conceal cloak portal scrying party',
      run: () => { recordGroupInteraction(g.name); bulkWindowOp(
        { direction: 'unfocus', scope: 'group', group: g.name },
        `hide group ${g.name}`); },
    });
    cmds.push({
      icon: wiz('◎', '👁'), label: wiz(`Focus group: ${g.name}`, `Reveal party: ${g.name}`),
      hint: wiz(`raise ${n}`, `conjure ${nPortals}`),
      keywords: 'focus show group windows ' + g.name
        + ' reveal behold conjure portal scrying party',
      run: () => { recordGroupInteraction(g.name); bulkWindowOp(
        { direction: 'focus', scope: 'group', group: g.name },
        `focus group ${g.name}`); },
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
        icon: wiz('⏻', '🌙'), label: wiz(`Shut down group: ${g.name}`, `Slumber party: ${g.name}`),
        hint: wiz(`stop ${onlineG} running agent${plural} (resumable; no data deleted)`,
          `lull ${onlineG} channeling familiar${plural} into slumber (rousable; nothing is lost)`),
        keywords: 'shutdown shut down stop kill power off halt group batch ' + g.name
          + ' slumber sleep rest lull dormant party familiars',
        run: () => { recordGroupInteraction(g.name); shutdownScope('group', g.name); },
      });
    }
    const offlineG = (g.members || []).filter(m => !m.online).length;
    if (offlineG) {
      const plural = offlineG === 1 ? '' : 's';
      cmds.push({
        icon: wiz('⏼', '✨'), label: wiz(`Power on group: ${g.name}`, `Awaken party: ${g.name}`),
        hint: wiz(`resume ${offlineG} offline agent${plural} onto their conversations`,
          `rouse ${offlineG} slumbering familiar${plural} back onto their scrolls`),
        keywords: 'power on start resume wake boot up group batch ' + g.name
          + ' awaken rouse stir revive kindle party familiars',
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
    cmds.push({
      icon: wiz('◎', '👁'), label: wiz(`Focus window: ${label}`, `Reveal familiar: ${label}`),
      hint: wiz("raise / open this agent's terminal", "conjure this familiar's scrying portal"),
      keywords: 'focus show jump bring up window agent ' + label + ' ' + (a.conv_id || '')
        + ' reveal behold conjure portal scrying familiar',
      run: () => jumpAgent(sel, label),
    });
    cmds.push({
      icon: wiz('⏏', '🌫'), label: wiz(`Hide window: ${label}`, `Veil familiar: ${label}`),
      hint: wiz("detach this agent's terminal", "draw the veil over this familiar's scrying portal"),
      keywords: 'hide detach window agent ' + label + ' ' + (a.conv_id || '')
        + ' veil conceal cloak portal scrying familiar',
      run: () => hideAgent(sel, label),
    });
  }

  // 7b) Per-agent power control — stop a running agent or resume an
  //     offline one. The single-agent analog of the per-agent window
  //     ops above, mirroring the per-row status-dot toggle: a stop pops
  //     the 3-way shutdownConfirm (Cancel / Soft exit / Force kill), a
  //     resume fires straight away (non-destructive). Each agent is
  //     listed for its CURRENT state only — online → Stop, offline →
  //     Resume — so the palette never offers the wrong verb.
  for (const a of (snap.agents || [])) {
    const label = a.title || (a.conv_id || '').slice(0, 8);
    const sel = a.agent_id || a.conv_id;
    if (a.online) {
      cmds.push({
        icon: wiz('⏻', '🌙'), label: wiz(`Stop agent: ${label}`, `Slumber familiar: ${label}`),
        hint: wiz('soft-exit, then force-kill if it does not exit (resumable)',
          'lull into slumber: a gentle /exit, then a firmer hand if it lingers (rousable)'),
        keywords: 'stop shutdown shut down kill power off halt agent ' + label + ' ' + (a.conv_id || '')
          + ' slumber sleep rest lull dormant familiar',
        run: () => stopAgentInteractive(sel, label),
      });
    } else {
      cmds.push({
        icon: wiz('⏼', '✨'), label: wiz(`Resume agent: ${label}`, `Awaken familiar: ${label}`),
        hint: wiz('restart in a fresh tmux session, resumed onto its conversation',
          'rouse into a fresh tmux session, resumed onto its scroll'),
        keywords: 'resume start power on wake boot up agent ' + label + ' ' + (a.conv_id || '')
          + ' awaken rouse stir revive kindle familiar',
        run: () => resumeAgentReq(sel, label),
      });
    }
  }

  // 8) Per-group bulk retire — "Retire idle / offline agents in <group>".
  //    A cleanup sweep that demotes a whole cohort of a group's members
  //    to plain (reinstatable) conversations. Opens a PREVIEW modal
  //    (openRetirePreview) listing precisely the matching members so the
  //    human can opt individual agents out before the batch fires; submit
  //    POSTs the explicit conv-id list to /api/groups/{name}/retire, so
  //    the BE retires exactly what was previewed. Listed only when the
  //    group actually HAS members of that status, so the palette never
  //    offers a no-op.
  for (const g of groups) {
    for (const status of ['idle', 'offline']) {
      const n = countGroupMembersByStatus(g.name, status);
      if (!n) continue;
      const plural = n === 1 ? '' : 's';
      cmds.push({
        icon: wiz('♻', '🪄'), label: wiz(`Retire ${status} agents in ${g.name}`, `Banish ${status} familiars in ${g.name}`),
        hint: wiz(`preview + demote ${n} ${status} agent${plural} to plain conversations`,
          `preview + banish ${n} ${status} familiar${plural} back to plain scrolls`),
        keywords: 'retire demote cleanup remove tidy bulk ' + status + ' agents group ' + g.name
          + ' banish exile dismiss familiars party',
        run: () => { recordGroupInteraction(g.name); openRetirePreview(g.name, status); },
      });
    }
  }

  // 8b) Per-group worktree cleanup — "Cleanup worktrees in <group>". The
  //     repo-wide janitor: scans the group's default dir (∪ its agents'
  //     history dirs) for stale git worktrees and opens a preview modal
  //     to pick which to remove. Offered whenever the group has somewhere
  //     to scan (a default dir or any members); the modal itself reports
  //     "nothing to clean up" when a scan comes back empty, so the gate is
  //     deliberately loose rather than firing a probe per group here.
  for (const g of groups) {
    const scannable = (g.default_cwd && g.default_cwd.trim()) || (g.members && g.members.length);
    if (!scannable) continue;
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
  //    ⚙ Retire button uses (retireAgentInteractive). Listed for every
  //    agent on the roster, online OR offline — retire is valid on an
  //    offline agent too (there is just no pane to soft-exit).
  for (const a of (snap.agents || [])) {
    const label = a.title || (a.conv_id || '').slice(0, 8);
    const status = a.online ? ((a.state && a.state.status) || 'online') : 'offline';
    cmds.push({
      icon: wiz('♻', '🪄'), label: wiz(`Retire agent: ${label}`, `Banish familiar: ${label}`),
      hint: wiz(`demote to a plain conversation (${status})`, `banish back to a plain scroll (${status})`),
      keywords: 'retire demote cleanup remove agent ' + label + ' ' + (a.conv_id || '') + ' ' + status
        + ' banish exile dismiss familiar',
      // Retire stays conv-keyed (not agent_id): the server's dangling-agent
      // recovery only triggers for a UUID-shaped selector that fails to
      // resolve, so a stable agent_id would silently demote a dangling
      // orphan instead of offering to remove it (JOH-322).
      run: () => retireAgentInteractive(a.conv_id, label),
    });
  }

  return cmds;
}

// -- Rendering ---------------------------------------------------------

function render(q) {
  filtered = rankCommands(commands, q);
  if (selected >= filtered.length) selected = filtered.length - 1;
  if (selected < 0) selected = 0;
  if (!filtered.length) {
    const empty = isWizardActive() ? WIZARD_EMPTY : 'No matching commands';
    list.innerHTML = `<div class="palette-empty">${esc(empty)}</div>`;
    input.removeAttribute('aria-activedescendant');
    return;
  }
  // Each option carries a stable id so the combobox input can point its
  // aria-activedescendant at the keyboard-selected row — that's how a
  // screen reader announces the active command as ↑/↓ move.
  list.innerHTML = filtered.map((c, i) => `
    <div class="palette-item${i === selected ? ' selected' : ''}" data-idx="${i}"
         id="palette-opt-${i}" role="option" aria-selected="${i === selected ? 'true' : 'false'}">
      <span class="palette-ico">${esc(c.icon || '•')}</span>
      <span class="palette-label">${esc(c.label)}</span>
      ${c.hint ? `<span class="palette-hint">${esc(c.hint)}</span>` : ''}
    </div>`).join('');
  paintSelection();
}

// paintSelection updates the highlight + ARIA without a full re-render
// and scrolls the active row into view — used by ↑/↓ and hover. It also
// re-points the input's aria-activedescendant at the active option.
function paintSelection() {
  const items = list.querySelectorAll('.palette-item');
  items.forEach((el, i) => {
    const on = i === selected;
    el.classList.toggle('selected', on);
    el.setAttribute('aria-selected', on ? 'true' : 'false');
    if (on) el.scrollIntoView({ block: 'nearest' });
  });
  if (filtered.length) input.setAttribute('aria-activedescendant', 'palette-opt-' + selected);
  else input.removeAttribute('aria-activedescendant');
}

function move(d) {
  if (!filtered.length) return;
  selected = (selected + d + filtered.length) % filtered.length;
  paintSelection();
}

// pageSize is roughly how many rows a PageUp/PageDown jumps — the count
// of items that fit in the visible scroll viewport (clientHeight / an
// item's rendered height), floored to at least 1. clientHeight includes
// the list's padding, so this can round up by a row versus a strict page;
// harmless (scrollIntoView keeps the landing row visible and every row
// stays reachable), so it's not worth reading computed padding to shave.
// Measured live off the DOM so a resized window or a shorter list scales
// the jump; falls back to a constant when the list isn't measurable yet
// (no rows, or zero-height pre-layout).
const PAGE_FALLBACK = 10;
function pageSize() {
  const first = list.querySelector('.palette-item');
  const itemH = first ? first.offsetHeight : 0;
  if (!itemH) return PAGE_FALLBACK;
  return Math.max(1, Math.floor(list.clientHeight / itemH));
}

// movePage jumps the selection by a page and CLAMPS at the ends — unlike
// ↑/↓ (move), which wrap. A page jump that wrapped top↔bottom would be
// disorienting, and clamping matches how PageUp/PageDown behave in a
// native list: hitting the top/bottom parks the selection there.
function movePage(d) {
  if (!filtered.length) return;
  const next = selected + d * pageSize();
  selected = Math.min(filtered.length - 1, Math.max(0, next));
  paintSelection();
}

function runSelected() {
  const cmd = filtered[selected];
  if (!cmd) return;
  // Close first so a command that opens its OWN modal (windows / spawn)
  // isn't stacked under our overlay.
  closePalette();
  try {
    cmd.run();
  } catch (e) {
    toast('command failed: ' + ((e && e.message) || e), true);
  }
}

// -- Open / close ------------------------------------------------------

// applyThemeCopy re-flavours the JS-driven text (the input placeholder) for
// the live theme — the wizard "Spellbook" prompt vs. the captured regular
// one. Called on each open AND on a mid-open theme flip (the CSS chrome
// swaps instantly via body.wizard, so the JS copy must follow to match).
function applyThemeCopy() {
  input.placeholder = isWizardActive() ? WIZARD_PLACEHOLDER : defaultPlaceholder;
}

function openPalette() {
  // Remember where focus was so closePalette can return it.
  lastFocus = document.activeElement;
  commands = buildCommands();
  selected = 0;
  input.value = '';
  // Re-flavour the prompt per the live theme — wizard mode may have been
  // toggled since the last open, so pick fresh each time.
  applyThemeCopy();
  overlay.classList.add('show');
  render('');
  // Focus after the show so the box is laid out; select-all is moot on
  // an empty field but keeps the behaviour obvious if reopened dirty.
  input.focus();
}

function closePalette() {
  overlay.classList.remove('show');
  input.removeAttribute('aria-activedescendant');
  // Return focus to the trigger element rather than letting it drop to
  // <body>. Guarded — the element may have been re-rendered away.
  if (lastFocus && typeof lastFocus.focus === 'function') {
    try { lastFocus.focus(); } catch (_) { /* element gone */ }
  }
  lastFocus = null;
}

// bindCommandPalette wires the global Ctrl/Cmd-K trigger and the
// in-overlay interactions. Called once from dashboard.js boot.
export function bindCommandPalette() {
  overlay = $('#' + MODAL_ID);
  input = $('#palette-input');
  list = $('#palette-list');
  // Defensive: if the markup ever goes missing, do nothing rather than
  // throw and break the rest of boot.
  if (!overlay || !input || !list) return;

  // Capture the HTML's placeholder as the regular-theme prompt so the
  // wizard swap in openPalette can restore it without duplicating the copy.
  defaultPlaceholder = input.getAttribute('placeholder') || '';

  // Global trigger. e.key is layout-stable for a plain letter; lower-
  // case both so Shift+Ctrl+K (some setups) still matches. e.repeat is
  // ignored so holding the chord doesn't strobe open/close.
  document.addEventListener('keydown', (e) => {
    if (e.repeat) return;
    if (!(e.ctrlKey || e.metaKey)) return;
    if ((e.key || '').toLowerCase() !== 'k') return;
    e.preventDefault();
    if (isOpen()) { closePalette(); return; }
    // Don't pop the launcher on top of another open dialog (e.g. mid
    // spawn-form): stacked overlays are a surprise and the dialog
    // beneath keeps its own state. The hotkey resumes once it closes.
    if (document.querySelector('.modal-overlay.show, .manage-overlay.show')) return;
    openPalette();
  });

  // Typing filters; ↑/↓ move; Enter runs; Esc closes. Bound to the
  // input (which holds focus the whole time the palette is open).
  input.addEventListener('input', () => { selected = 0; render(input.value); });
  input.addEventListener('keydown', (e) => {
    switch (e.key) {
      case 'ArrowDown': e.preventDefault(); move(1); break;
      case 'ArrowUp': e.preventDefault(); move(-1); break;
      case 'PageDown': e.preventDefault(); movePage(1); break;
      case 'PageUp': e.preventDefault(); movePage(-1); break;
      case 'Enter': e.preventDefault(); runSelected(); break;
      case 'Escape': e.preventDefault(); closePalette(); break;
      default: break;
    }
  });

  // Mouse: hover selects, click runs.
  list.addEventListener('mousemove', (e) => {
    const item = e.target.closest('.palette-item');
    if (!item) return;
    const idx = Number(item.dataset.idx);
    if (idx !== selected) { selected = idx; paintSelection(); }
  });
  list.addEventListener('click', (e) => {
    const item = e.target.closest('.palette-item');
    if (!item) return;
    selected = Number(item.dataset.idx);
    runSelected();
  });

  // Backdrop click closes (a click on the box itself does not).
  overlay.addEventListener('click', (e) => {
    if (e.target === overlay) closePalette();
  });

  // Theme flipped WHILE the palette is open (the +W hotkey fires even with
  // focus in the search box). The CSS re-skin — the Spellbook title, the
  // arcane chrome — swaps instantly on the body.wizard class, so re-apply the
  // JS-driven copy so it doesn't lag a theme behind: the placeholder (via
  // applyThemeCopy), the empty-state line (via the re-render), AND the command
  // labels/hints themselves — those are baked by buildCommands' wiz() calls at
  // open time, so we rebuild the list here to re-skin every Summon/Slumber/Veil
  // presented label for the new theme. slop.js dispatches tclaude:wizard per
  // toggle. Rebuilding re-reads lastSnapshot, same as a fresh open.
  document.addEventListener('tclaude:wizard', () => {
    if (!isOpen()) return;
    // Reset the selection before re-ranking: a theme flip can re-order the
    // filtered list (e.g. "veil" jumps from a weak keyword hit in the plain
    // theme to a label-prefix hit in wizard mode), so a preserved index would
    // land on a different command than the one that was highlighted. Start
    // fresh at the top, the same way a keystroke does.
    applyThemeCopy();
    commands = buildCommands();
    selected = 0;
    render(input.value);
  });

  // The header 🔍 button is the discoverable entry point for anyone who
  // doesn't know the hotkey.
  const btn = $('#command-palette-btn');
  if (btn) btn.addEventListener('click', openPalette);
}
