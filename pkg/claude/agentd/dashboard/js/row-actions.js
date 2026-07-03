// row-actions.js — bindRowActions, the delegated click router for every
// per-row action button across the dashboard's tables.
//
// Extracted from dashboard.js in the Stage 2 module split. Owns
// renameEditing — the inline-rename-open flag refreshSuspended consults.

import { $, $$, shortId, groupOfflineOverride, pickDirectory } from './helpers.js';
import { renderGroupsTab, renderSudoTab } from './tabs.js';
import { dashPrefs } from './prefs.js';
import { loadProfiles, setDashDefaultProfile } from './profiles.js';
import { openProfileEditor } from './modal-profiles.js';
import { renderDashDefaultProfile } from './render.js';
import {
  openSudoGrantModal, openCronCreateModal, openCronEditModal,
} from './modal-cron.js';
import { openMessageCreateModal, openPermEditModal } from './modal-message.js';
import { openHumanReplyModal } from './modal-human-reply.js';
import { openGroupContextModal, openGroupCloneModal, openFromGroupModal } from './modal-templates.js';
import { openLinkModal, openLinksManageModal } from './modal-link-wt.js';
import { openExportModal } from './modal-export.js';
import { triggerExportDownload } from './export-progress.js';
import { openTermModal } from './modal-term.js';
import {
  openTerminalPane, closeTerminalsForConvs, focusTerminalForConv,
  openWebWindowPane, openWebTermPane,
} from './terminals-tab.js';
import {
  openAgentSpawnModal, openCloneAgentModal,
  openReincarnateAgentModal,
} from './modal-spawn.js';
// openMailbox brings the Messages tab forward + selects a folder; mail.js
// doesn't import row-actions.js, so this is a one-way edge (no cycle).
import { openMailbox } from './mail.js';
import { wizWord } from './slop.js';
// refresh()/toast() and the shared action modals live in refresh.js;
// lastSnapshot is dashboard.js's shared state, written here (rename
// rollback) via setLastSnapshot. Deliberate benign cycles (see
// render.js); TDZ-safe.
import {
  refresh, toast, confirmModal, addMemberModal, deleteAgentModal,
  editMemberModal, shutdownScope, powerOnScope, openCleanupModal, openWindowModal,
  openWorktreeCleanup,
  resumeAgentReq, retireAgentInteractive, shutdownConfirm, stopAgentReq, termDirModal,
  showAccessTab,
} from './refresh.js';
import { lastSnapshot, setLastSnapshot, webTerminalDefault } from './dashboard.js';

// True while an inline rename input is open; suspends the auto-
// refresh so the 2s tick doesn't blow the input away mid-edit.
let renameEditing = false;

// inlineEdit turns a static element into a one-field click-to-edit:
// it swaps `el` for a focused <input>, commits on Enter, and reverts
// on Esc / blur. The 2s auto-refresh is suspended (renameEditing) for
// the input's whole lifetime so a poll can't blow it away mid-edit;
// if the host row is a drag source its draggable attr is parked too,
// so selecting text in the input can't accidentally start a row drag.
//
// onSave(value) is the caller's commit, invoked with the input still
// in the DOM. It returns one of:
//   'saved'  — the daemon accepted the change; inlineEdit calls
//              refresh(), whose re-render replaces the input.
//   'revert' — nothing to persist (value unchanged) or the caller
//              already toasted a failure; restore the original element.
// A thrown error is caught, toasted, and treated as 'revert'.
//
// This is the canonical inline-edit primitive. The group-header chips
// (rename-group, set-group-dir / -descr / -max-members) predate it and
// still hand-roll the same pattern — migrating them is a deliberate
// follow-up, kept out of this rename-focused change.
function inlineEdit({ el, value, type = 'text', inputClass, placeholder, listId, onSave }) {
  const prevSnapshot = lastSnapshot;
  renameEditing = true;
  // Park the host row's drag source while the input is open — an
  // <input> inside a draggable <tr> otherwise hands text-selection
  // drags to the row-drag machinery. Restored on revert; the success
  // path's refresh() rebuilds the row outright so no restore needed.
  const dragRow = el.closest('[draggable="true"]');
  if (dragRow) dragRow.setAttribute('draggable', 'false');
  const origEl = el.cloneNode(true);
  const input = document.createElement('input');
  input.type = type;
  if (inputClass) input.className = inputClass;
  input.value = value;
  if (placeholder) input.placeholder = placeholder;
  // Optional <datalist> suggestions (e.g. the model-alias list) —
  // free text stays allowed, the list is just one click away.
  if (listId) input.setAttribute('list', listId);
  input.spellcheck = false;
  input.autocomplete = 'off';
  el.replaceWith(input);
  input.focus();
  input.select();
  // Datalist-backed editor: pop the suggestion list open right away —
  // the click that opened the editor is the user reaching for a value,
  // so make them visible without hunting for the input's tiny arrow.
  // showPicker() needs transient user activation (the opening click
  // provides it) and isn't supported everywhere; failure just means
  // the list opens on typing/arrow-down as before. Typing afterwards
  // keeps filtering the list normally. Note Chromium filters the list
  // against the current value, so a chip with an existing value shows
  // the matching subset until the text is replaced.
  if (listId) {
    try { input.showPicker(); } catch (_) { /* no activation / unsupported — fine */ }
  }
  // phase: editing → committing (during the await) → done. Guards
  // against a blur firing mid-commit and against a double Enter.
  let phase = 'editing';
  const teardownRestore = () => {
    if (input.parentNode) input.replaceWith(origEl);
    if (dragRow) dragRow.setAttribute('draggable', 'true');
    renameEditing = false;
    setLastSnapshot(prevSnapshot);
  };
  const revert = () => {
    if (phase !== 'editing') return;
    phase = 'done';
    teardownRestore();
  };
  const commit = async () => {
    if (phase !== 'editing') return;
    phase = 'committing';
    let outcome;
    try {
      outcome = await onSave(input.value);
    } catch (err) {
      toast(`save failed: ${(err && err.message) || err}`, true);
      outcome = 'revert';
    }
    phase = 'done';
    if (outcome === 'saved') {
      renameEditing = false;
      refresh();
    } else {
      teardownRestore();
    }
  };
  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') {
      // Datalist-backed editor: this Enter may be ACCEPTING a
      // highlighted suggestion — the browser applies the replacement
      // as the keydown's default action, i.e. after this handler. So
      // don't preventDefault (that can cancel the acceptance) and
      // commit on the next tick, once the final value is in place.
      // One Enter then both accepts and saves. The phase guard inside
      // commit() absorbs the case where the pick's `input` event
      // below already committed.
      if (listId) { setTimeout(commit, 0); return; }
      ev.preventDefault(); commit();
    } else if (ev.key === 'Escape') { ev.preventDefault(); revert(); }
  });
  // Picking a datalist suggestion with the MOUSE saves immediately —
  // the user clicked a concrete choice, and requiring a follow-up
  // Enter reads as the click not working. Typed edits keep the
  // explicit-Enter contract: a pick arrives as an `input` event whose
  // inputType is 'insertReplacementText' (Chromium) or undefined
  // (Firefox/Safari), never the per-keystroke 'insertText' — and only
  // counts when the value matches one of the list's options exactly.
  if (listId) {
    const list = document.getElementById(listId);
    input.addEventListener('input', (ev) => {
      const picked = ev.inputType === undefined || ev.inputType === 'insertReplacementText';
      if (!picked || !list) return;
      if ([...list.options].some(o => o.value === input.value)) commit();
    });
  }
  // Blur cancels rather than commits — explicit Enter to save, same
  // contract as the group-header chips.
  input.addEventListener('blur', revert);
}

// openProfilePicker turns a 🧠 spawn-profile chip into a one-shot <select>
// of the saved profile names (+ a leading "(none)" to clear), suspending the
// 2s auto-refresh while it's open (renameEditing) so a poll can't blow it
// away. Picking an option (or pressing it then leaving) restores the chip
// element and calls onCommit(name) — which performs the persistence and any
// repaint and resolves to true on success, false (or throws) to revert.
// Escape / blur cancel. Shared by the group-default and dashboard-default
// pickers, which differ only in onCommit. The profile list is fetched
// (loadProfiles) so a freshly-created profile shows up; a current value no
// longer in the list is kept as a "(missing)" option so it's still visible
// and changeable.
// Sentinel <option> value for the picker's "＋ new profile…" entry. A leading
// slash can never appear in a real profile name (server-side validateGroupName
// rejects "/" and "\"), so this value can't collide with a profile.
const PROFILE_PICKER_NEW = '/new-profile';

async function openProfilePicker(chipEl, current, onCommit) {
  const prevSnapshot = lastSnapshot;
  // Fetch the list BEFORE suspending the refresh or touching the DOM, so a
  // slow (cold-cache) fetch can't leave the picker half-open. Critically,
  // renameEditing is set only AFTER the await: were it set before, a second
  // click during the fetch would start a rival picker whose chip is already
  // detached — its replaceWith no-ops, so it never mounts, its listeners
  // never fire, and the only code that resets renameEditing never runs,
  // wedging the auto-refresh permanently.
  let profiles = [];
  try { profiles = await loadProfiles(); } catch (_) { profiles = []; }
  // Bail if another picker already opened (renameEditing) or this chip was
  // repainted away (a poll re-rendered it) while we were fetching — either
  // way, mounting a <select> here would strand it.
  if (renameEditing || !chipEl.isConnected) return;
  renameEditing = true;
  const origEl = chipEl.cloneNode(true);
  const select = document.createElement('select');
  select.className = 'group-default-profile-select';
  // "＋ new profile…" sits at the top — picking it jumps to the editor to
  // create one (and sets it as this default on save), so an empty profile
  // list isn't a dead end. Re-lettered "＋ new pattern…" in 🧙 wizard mode,
  // matching the editor it opens (New familiar pattern).
  select.add(new Option(wizWord('＋ new profile…', '＋ new pattern…'), PROFILE_PICKER_NEW));
  select.add(new Option('(none)', ''));
  for (const p of profiles) select.add(new Option(p.name, p.name));
  if (current && !profiles.some(p => p.name === current)) {
    select.add(new Option(`${current} (missing)`, current));
  }
  select.value = current;
  chipEl.replaceWith(select);
  select.focus();
  let done = false;
  const cancel = () => {
    if (done) return;
    done = true;
    if (select.parentNode) select.replaceWith(origEl);
    renameEditing = false;
    setLastSnapshot(prevSnapshot);
  };
  const commit = async () => {
    if (done) return;
    const name = select.value;
    if (name === PROFILE_PICKER_NEW) {
      // Jump to the editor (create mode): close the picker, then on a
      // successful save set the new profile as this default via onCommit.
      done = true;
      if (select.parentNode) select.replaceWith(origEl);
      renameEditing = false;
      openProfileEditor(null, { onSaved: (newName) => onCommit(newName) });
      return;
    }
    if (name === current) { cancel(); return; }
    done = true;
    // Put the chip element back before persisting so onCommit's refresh /
    // re-render has a stable mount point and no stray <select> survives.
    if (select.parentNode) select.replaceWith(origEl);
    renameEditing = false;
    try {
      const ok = await onCommit(name);
      if (!ok) setLastSnapshot(prevSnapshot);
    } catch (err) {
      toast((err && err.message) || String(err), true);
      setLastSnapshot(prevSnapshot);
    }
  };
  select.addEventListener('change', commit);
  select.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') { ev.preventDefault(); cancel(); }
  });
  select.addEventListener('blur', cancel);
}

// closeAllActionMenus collapses every open ⚙ options menu. Called on
// any non-cog click — outside-click dismissal and menu-item dismissal
// alike — by the cog toggle, and by the Escape handler, so at most one
// menu is ever open. While a menu is open refreshSuspended() pauses
// the 2s poll (it sees .action-menu.open); dropping the .open class
// here is therefore also what releases that suspension. It also keeps
// the cog's aria-expanded in sync and — when focus sat inside a menu
// about to be display:none'd — hands focus back to that cog so it
// doesn't fall to <body> and get lost.
function closeAllActionMenus() {
  $$('.action-menu.open').forEach((menu) => {
    const cog = menu.parentElement
      && menu.parentElement.querySelector('.cog-btn');
    const focusInside = menu.contains(document.activeElement);
    menu.classList.remove('open');
    if (cog) {
      cog.setAttribute('aria-expanded', 'false');
      if (focusInside) cog.focus();
    }
  });
}

// bindRowActions delegates clicks on row-action buttons to the
// appropriate /api/groups/... call. After a successful mutation we
// re-fetch the snapshot so the badge / button state updates.
function bindRowActions() {
  document.addEventListener('click', async (e) => {
    const btn = e.target.closest('[data-act]');
    const act = btn ? btn.getAttribute('data-act') : null;
    // ⚙ options-menu dismissal. The cog toggles its own menu (the
    // row-menu / group-menu cases below) — leave open menus alone for
    // it. Otherwise: a click on a menu ITEM closes the menu it came
    // from, then falls through to dispatch the item (btn stays valid —
    // only the .open class is dropped); a click anywhere OUTSIDE every
    // menu closes them too (click-away). A click on a menu's own
    // padding — inside a menu but not on an item — leaves it open.
    const onCog = act === 'row-menu' || act === 'group-menu' || act === 'filter-bar-menu';
    const inMenu = !!e.target.closest('.action-menu');
    // Menus self-dismiss on any click that lands on a button inside
    // them. data-act items are caught by the `btn` check below, but
    // the filter-bar-cog's menu items dispatch via their id-bound
    // listeners and so have no data-act — `inMenuButton` covers
    // them too.
    const inMenuButton = inMenu && !!e.target.closest('.action-menu button');
    if (!onCog && (btn || inMenuButton || !inMenu)) closeAllActionMenus();
    if (!btn) return;
    // Buttons may live inside <summary>, where the default click
    // action is to toggle the details. Stop that.
    e.preventDefault();
    const group = btn.getAttribute('data-group');
    const conv = btn.getAttribute('data-conv');
    // `agent` is the per-agent action SELECTOR: the rotation-immune stable
    // agent_id when the row carries one (data-agent, = agent_id || conv-id
    // at render time), falling back to the conv-id otherwise. The server
    // endpoints below resolve it through agent.ResolveSelector, which takes
    // an `agt_` id OR a conv-id — so routing per-agent actions by `agent`
    // targets the actor across reincarnation/`/clear`, while a pre-identity
    // or plain-conversation row (no data-agent) still resolves by conv-id
    // (JOH-322). The conv-keyed cases that legitimately target a specific
    // conversation generation (copy/delete-generation), a plain conversation
    // (promote), a conv-keyed mailbox folder (view-agent-messages) or the
    // conv-keyed permissions/sudo snapshot (perm-edit / sudo-grant, D3) keep
    // using `conv`.
    const agent = btn.getAttribute('data-agent') || conv;
    const label = btn.getAttribute('data-label') || conv;
    try {
      let ok = false;
      switch (act) {
        case 'cycle-group-offline': {
          // Pure client-side view state — cycle the per-group
          // offline override inherit → show → hide and re-render.
          // No daemon round-trip.
          const okey = 'tclaude.dash.group.offline.' + group;
          const cur = groupOfflineOverride(group);
          const next = cur === 'inherit' ? 'show' : cur === 'show' ? 'hide' : 'inherit';
          if (next === 'inherit') dashPrefs.removeItem(okey);
          else dashPrefs.setItem(okey, next);
          renderGroupsTab();
          return;
        }
        case 'toggle-quick-pin': {
          // Pure client-side view state — pin/unpin this group's quick
          // options so the body.group-quick-fold accordion skips it. Stored
          // in dashPrefs (server-side, per browser) like the offline override
          // above. No daemon round-trip; re-render shows the new state.
          const pkey = 'tclaude.dash.quickpin.' + group;
          if (dashPrefs.getItem(pkey) === '1') dashPrefs.removeItem(pkey);
          else dashPrefs.setItem(pkey, '1');
          renderGroupsTab();
          return;
        }
        case 'remove-member': {
          const confirmed = await confirmModal({
            title: 'Remove member from group?',
            body: 'This unsubscribes them from group messages and severs the manager-pattern path. Their conv keeps running.',
            meta: `${label} → ${group}`,
            okLabel: 'Remove',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(agent)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Remove failed: ${await r.text()}`, true);
          break;
        }
        case 'grant-owner': {
          // Granting owner is non-destructive; skip the confirm
          // modal but still re-fetch on success.
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}/owners`, {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ conv: agent }),
          });
          ok = r.ok;
          if (!ok) toast(`Grant owner failed: ${await r.text()}`, true);
          break;
        }
        case 'toggle-group-notify': {
          // The group-header bell: flip agent_groups.notify_enabled.
          // Non-destructive and instantly reversible — no confirm.
          const cur = btn.getAttribute('data-enabled') === '1';
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
            method: 'PATCH', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ notify_enabled: !cur }),
          });
          if (!r.ok) { toast(`Notification toggle failed: ${await r.text()}`, true); return; }
          toast(cur ? `${group}: notifications muted 🔕` : `${group}: notifications on 🔔`);
          refresh();
          return;
        }
        case 'toggle-agent-notify': {
          // The member-row bell: cycle the per-agent override
          // inherit → off → on → inherit (mirrors the offline-view
          // cycle pattern, but persisted daemon-side).
          const cur = btn.getAttribute('data-mode') || 'inherit';
          const next = cur === 'inherit' ? 'off' : cur === 'off' ? 'on' : 'inherit';
          const r = await fetch(`/api/agents/${encodeURIComponent(agent)}/notify`, {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ mode: next }),
          });
          if (!r.ok) { toast(`Notification toggle failed: ${await r.text()}`, true); return; }
          toast(`${label}: notifications ${next === 'inherit' ? 'inherit from group' : next}`);
          refresh();
          return;
        }
        case 'toggle-remote-control': {
          // Per-agent Remote Access toggle. data-intent is the OPPOSITE of
          // the current best-known state (set at render time), so one click
          // flips it. The server owns the toggle direction + the disable
          // confirm-Enter; the UI only sends intent and reconciles on the
          // refresh below — the harness has no readback, so the state is
          // best-known, not authoritative. (JOH-259)
          const intent = btn.getAttribute('data-intent') || 'toggle';
          const r = await fetch(`/api/agents/${encodeURIComponent(agent)}/remote-control`, {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ intent }),
          });
          if (!r.ok) { toast(`Remote control toggle failed: ${await r.text()}`, true); return; }
          let resp = {};
          try { resp = await r.json(); } catch (_) { /* tolerate a bodyless 200 */ }
          const on = !!resp.remote_control;
          toast(`${label}: remote access ${on ? 'ON — reachable from the Claude app' : 'OFF'}`);
          refresh();
          return;
        }
        case 'jump': {
          // If this agent already has an open web terminal / window pane in the
          // dashboard's Terminals tab, jump to THAT instead of raising a native
          // OS window — the browser terminal is the live view the human means.
          if (focusTerminalForConv([agent])) { toast(`focused: ${label}`); return; }
          // With web terminals as the default (config dashboard.default_terminal
          // = "web"), "focus" opens the agent's live session as a browser pane
          // rather than raising a native OS window. openWebWindowPane keys on
          // the agent selector, so this focuses an existing pane rather than
          // duplicating (the focusTerminalForConv check above already handled
          // the common already-open case).
          if (webTerminalDefault()) { openWebWindowPane(agent, label); toast(`focused: ${label}`); return; }
          // Non-destructive; no confirm modal, just fire-and-toast.
          const r = await fetch(`/api/jump/${encodeURIComponent(agent)}`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Jump failed: ${await r.text()}`, true);
          // Skip the default refresh — focusing doesn't change any
          // dashboard state and the user just left the window.
          if (ok) toast(`focused: ${label}`);
          return;
        }
        case 'hide': {
          // The inverse of 'jump' — detaches the agent's terminal
          // window (tmux detach-client). Window-only: the agent keeps
          // running, so no confirm modal and no dashboard-state change.
          // Idempotent server-side: an already-detached agent reports
          // detached:0 — a clean no-op, toasted as "already hidden".
          const r = await fetch(`/api/hide/${encodeURIComponent(agent)}`, {
            method: 'POST', credentials: 'same-origin',
          });
          if (!r.ok) { toast(`Hide failed: ${await r.text()}`, true); return; }
          const info = await r.json().catch(() => ({}));
          // The agent's live-session tmux client was just detached — close its
          // multiplexer pane too (if one is open) so the terminal tab doesn't
          // linger showing "disconnected". The server-side detach already ran,
          // so this closes WITHOUT re-hiding.
          closeTerminalsForConvs([agent]);
          // Skip the default refresh — detaching a window doesn't
          // change any dashboard state (the agent stays online).
          toast(info.detached > 0 ? `hidden: ${label}` : `already hidden: ${label}`);
          return;
        }
        case 'term': {
          // Pick which directory, then ask the daemon to spawn a
          // terminal window there. Non-destructive and changes no
          // dashboard state, so skip the refresh. Native-first: the
          // daemon falls back to an in-browser PTY (mode:"browser") only
          // when it can't pop a native window — see handleDashboardTermAPI.
          // With web terminals as the default (config dashboard.default_terminal
          // = "web"), route this straight to a browser web-term pane — same as
          // the dedicated "web term" button. Hand the picker promise through so
          // a cancelled pick is a clean no-op.
          if (webTerminalDefault()) { openWebTermPane(agent, label, termDirModal({ label })); return; }
          const which = await termDirModal({ label });
          if (!which) return;
          const r = await fetch(`/api/term/${encodeURIComponent(agent)}`, {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ which }),
          });
          if (!r.ok) { toast(`Open terminal failed: ${await r.text()}`, true); return; }
          const info = await r.json().catch(() => ({}));
          if (info.mode === 'browser') { openTermModal({ wsPath: info.ws, label }); return; }
          toast(`terminal opened: ${info.dir || label}`);
          return;
        }
        case 'web-term': {
          // The dedicated "web term" button ALWAYS streams an in-browser PTY,
          // opened as a pane in the dashboard's own "Terminals" tab
          // (js/terminals-tab.js) — an in-SPA nav tab that holds many live
          // terminals at once — instead of the blocking in-page modal, so
          // several agents' terminals can be open simultaneously without
          // covering the dashboard. openWebTermPane takes the which-dir picker
          // promise directly and reveals the tab once it resolves (a cancelled
          // pick is a no-op). Same helper the "open terminal" action uses when
          // web terminals are the default.
          openWebTermPane(agent, label, termDirModal({ label }));
          return;
        }
        case 'open-window': {
          // Open a terminal attached to the agent's live session — the
          // explicit way to get a console. Non-destructive, changes no
          // dashboard state, so skip the refresh. Native-first; the daemon
          // falls back to an in-browser PTY only when it can't pop a native
          // window — see handleDashboardOpenWindowAPI.
          //
          // With web terminals as the default (config dashboard.default_terminal
          // = "web"), open the live session as a browser pane instead — same as
          // the dedicated "web window" button; the revealed Terminals tab is the
          // feedback (parity with web-open-window, which likewise doesn't toast).
          if (webTerminalDefault()) { openWebWindowPane(agent, label); return; }
          const r = await fetch(`/api/open-window/${encodeURIComponent(agent)}`, {
            method: 'POST', credentials: 'same-origin',
          });
          if (!r.ok) { toast(`Open window failed: ${await r.text()}`, true); return; }
          const info = await r.json().catch(() => ({}));
          // Pass hideConv so the modal's Detach/Close runs the real server-side
          // detach (/api/hide/{conv}) — closing the in-browser window must drop
          // the tmux client, or the agent session stays "attached" and can't be
          // reattached. Only the open-window attach (the agent's live session)
          // gets this; web-term opens its own throwaway session.
          if (info.mode === 'browser') { openTermModal({ wsPath: info.ws, label, hideConv: agent }); return; }
          toast(`window opened: ${label}`);
          return;
        }
        case 'web-open-window': {
          // Like "web term" but attached to the agent's live session (its
          // Claude Code TUI) rather than a fresh shell. ALWAYS a browser
          // terminal, opened in the Terminals tab — connects to
          // /api/open-window-ws/{conv}. openWebWindowPane sets hideConv so
          // closing the pane runs the reliable server-side detach. Same helper
          // the "focus" / "open window" actions use when web terminals are the
          // default.
          openWebWindowPane(agent, label);
          return;
        }
        case 'focus-pending': {
          // Open the pane of a PENDING spawn (JOH-205) — keyed on its
          // LABEL, since a pending agent has no conv-id yet. Opening the
          // pane lets the operator clear the startup gate; the sweeper
          // then promotes it into a real agent, which the 2s poll picks
          // up — so skip the immediate refresh.
          const r = await fetch(`/api/pending/focus/${encodeURIComponent(label)}`, {
            method: 'POST', credentials: 'same-origin',
          });
          if (!r.ok) { toast(`Focus failed: ${await r.text()}`, true); return; }
          toast(`opened pending spawn: ${label}`);
          return;
        }
        case 'term-dir': {
          // Click on a CWD path cell — the cell already names one
          // specific directory, so open a terminal there straight
          // away, skipping the term button's 3-way picker modal.
          const which = btn.getAttribute('data-which') || 'current';
          // With web terminals as the default (config dashboard.default_terminal
          // = "web"), open a browser web-term pane in that directory instead of
          // a native window. The dir is already known, so no picker promise.
          if (webTerminalDefault()) { openWebTermPane(agent, label, which); return; }
          const r = await fetch(`/api/term/${encodeURIComponent(agent)}`, {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ which }),
          });
          if (!r.ok) { toast(`Open terminal failed: ${await r.text()}`, true); return; }
          const info = await r.json().catch(() => ({}));
          if (info.mode === 'browser') { openTermModal({ wsPath: info.ws, label }); return; }
          toast(`terminal opened: ${info.dir || which}`);
          return;
        }
        case 'sudo-grant': {
          // Per-row affordance: open the same modal the Sudo tab's
          // "+ Grant sudo" button uses, pre-filled with this conv.
          // Modal handles the rest (validation, POST /api/sudo,
          // refresh).
          openSudoGrantModal(conv);
          return;
        }
        case 'perm-edit': {
          // Per-row affordance: open the permanent-permission editor
          // for this agent. Distinct from sudo-grant — that elevation
          // is time-bounded, these overrides persist.
          openPermEditModal(conv, label);
          return;
        }
        case 'sudo-manage': {
          // Click on the 🔓 badge: open the Access tab's Sudo sub-view
          // pre-filtered to this agent so the human can revoke specific
          // grants without scrolling through unrelated rows.
          const filterInput = $('#filter-sudo');
          filterInput.value = shortId(conv);
          try { dashPrefs.setItem('tclaude.dash.filter.sudo', filterInput.value); } catch (_) {}
          showAccessTab('sudo');
          renderSudoTab();
          return;
        }
        case 'sudo-revoke': {
          const id = btn.getAttribute('data-id');
          const slug = btn.getAttribute('data-slug') || '';
          const confirmed = await confirmModal({
            title: 'Revoke sudo grant?',
            body: 'The agent loses access to this slug immediately. They can request again if needed.',
            meta: `#${id} ${slug ? '· ' + slug : ''}${label ? ' · ' + label : ''}`,
            okLabel: 'Revoke',
          });
          if (!confirmed) return;
          const r = await fetch('/api/sudo/' + encodeURIComponent(id), {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Revoke failed: ${await r.text()}`, true);
          break;
        }
        case 'promote-agent': {
          // Conversations list → roster. Backend PromoteAgent also
          // reinstates a retired conv, so this one button covers
          // both "never an agent" and "was retired".
          const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/promote`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) { toast(`Promote failed: ${await r.text()}`, true); break; }
          toast(`promoted to agent: ${label}`);
          break;
        }
        case 'retire-agent': {
          // The whole confirm → POST → dangling-recovery → toast → refresh
          // flow lives in refresh.js so the command palette's "Retire
          // agent: <name>" runs the identical path. Retire stays conv-keyed
          // (uses `conv`, not `agent`): the dashboardEnrollmentVerb dangling
          // path only fires for a UUID-shaped selector that FAILS to resolve
          // (a dangling agent whose conversation is gone); a stable agent_id
          // resolves successfully even then and would silently demote the
          // orphan instead of offering to remove it (JOH-322).
          await retireAgentInteractive(conv, label);
          return;
        }
        case 'reinstate-agent': {
          const r = await fetch(`/api/agents/${encodeURIComponent(agent)}/reinstate`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) { toast(`Reinstate failed: ${await r.text()}`, true); break; }
          toast(`reinstated: ${label}`);
          break;
        }
        case 'delete-agent': {
          const choice = await deleteAgentModal(agent, label);
          if (!choice) return;
          const q = choice.deleteWorktree ? '?delete_worktree=1' : '';
          const r = await fetch(`/api/agents/${encodeURIComponent(agent)}${q}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) {
            toast(`Delete failed: ${await r.text()}`, true);
            break;
          }
          // Surface the worktree outcome when one was requested —
          // the DELETE returns 200 + JSON in that case.
          try {
            const out = await r.json();
            toast(out.worktree ? `deleted ${label} · ${out.worktree}` : `deleted ${label}`);
          } catch (_) {
            toast(`deleted ${label}`);
          }
          refresh();
          return;
        }
        case 'copy-generation-id': {
          // The "Replaced generations" view's lightweight inspect: copy the
          // dead generation's full conv-id so the operator can examine it
          // out-of-band — `claude --resume <id>` from its dir, or
          // `tclaude agent seance --target <id>`. A one-click in-dashboard
          // open of the grave is a planned follow-up.
          // Only claim "copied" when the clipboard API exists AND the write
          // resolves. `navigator.clipboard?.writeText(...)` returns undefined
          // (not a rejection) when clipboard is missing — awaiting that would
          // still hit the success toast without anything being written. So gate
          // on the API first, and fall back to the conv-id toast otherwise.
          if (navigator.clipboard && navigator.clipboard.writeText) {
            try {
              await navigator.clipboard.writeText(conv);
              toast(`conv-id copied: ${shortId(conv)} — inspect with 'claude --resume' or 'tclaude agent seance --target'`);
              return;
            } catch (_) {
              // fall through to the conv-id toast below
            }
          }
          toast(`conv-id: ${conv}`);
          return;
        }
        case 'delete-generation': {
          // Exact, single-generation delete via the dedicated endpoint, which
          // refuses the actor's live head (409) — so pruning a past generation
          // can never touch the live agent. Distinct from delete-agent (which
          // tears the whole actor down).
          const actor = btn.getAttribute('data-actor') || '';
          if (!await confirmModal({
            title: `Delete past generation?`,
            body: `Permanently delete the superseded generation ${label}${actor ? ` of ${actor}` : ''}? `
              + `This removes just this past generation (its transcript .jsonl + DB rows). `
              + `The live agent and its other generations are NOT affected.`,
            okLabel: 'delete generation',
          })) {
            return;
          }
          const r = await fetch(`/api/agent-generations/${encodeURIComponent(conv)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) { toast(`Delete failed: ${await r.text()}`, true); break; }
          toast(`deleted generation: ${label}`);
          refresh();
          return;
        }
        case 'edit-role':
        case 'edit-member': {
          // The single per-agent edit panel: title (incl. the "auto"
          // self-rename), group role, group description, the group-owner
          // toggle, and a Permissions… button. The same panel backs two
          // entry points: the ⚙ "edit" button (focuses Title) and the
          // click-to-edit role cell (data-act="edit-role", focuses Role).
          // The modal yields up to THREE independent edits — a rename
          // (conv title, injected via tmux), a membership PATCH (role /
          // descr), and an owner grant/revoke. They hit different
          // endpoints, so apply each on its own: one failing must not
          // silently swallow the others.
          const result = await editMemberModal({
            label: `${label} → ${group}`,
            title: btn.getAttribute('data-current') || '',
            role: btn.getAttribute('data-role') || '',
            descr: btn.getAttribute('data-descr') || '',
            owner: btn.getAttribute('data-owner') === '1',
            focusRole: act === 'edit-role',
            // openPermEditModal pre-fills from the conv-keyed permissions
            // snapshot, so it keeps the conv-id (the agent-id keying of the
            // permissions surface is D3); the rename / membership / owner
            // writes below route by the stable `agent`.
            openPerms: () => openPermEditModal(conv, label),
          });
          if (result === null) return; // cancelled
          if (result === 'noop') {
            toast('no changes');
            return;
          }
          let anyOk = false;
          if (result.rename) {
            const r = await fetch(`/api/agents/${encodeURIComponent(agent)}/rename`, {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(result.rename),
            });
            if (r.ok) {
              anyOk = true;
              toast(result.rename.auto
                ? `auto-rename nudge sent: ${label}`
                : `renaming ${label} → ${result.rename.title}`);
            } else {
              toast(`rename failed: ${await r.text()}`, true);
            }
          }
          if ('role' in result || 'descr' in result) {
            const body = {};
            if ('role' in result) body.role = result.role;
            if ('descr' in result) body.descr = result.descr;
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(agent)}`, {
              method: 'PATCH', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(body),
            });
            if (r.ok) {
              anyOk = true;
              toast(`updated ${label}`);
            } else {
              toast(`edit failed: ${await r.text()}`, true);
            }
          }
          if ('owner' in result) {
            // Owner is structural, not a membership column — route the
            // toggle to the owners grant (POST) / revoke (DELETE)
            // endpoints, the same ones the ⚙ make/revoke-owner buttons
            // use. Unlike the cog's revoke button there's no extra
            // confirm here: ticking the box + clicking Save IS the
            // deliberate gesture.
            const r = result.owner
              ? await fetch(`/api/groups/${encodeURIComponent(group)}/owners`, {
                  method: 'POST', credentials: 'same-origin',
                  headers: { 'Content-Type': 'application/json' },
                  body: JSON.stringify({ conv: agent }),
                })
              : await fetch(`/api/groups/${encodeURIComponent(group)}/owners/${encodeURIComponent(agent)}`, {
                  method: 'DELETE', credentials: 'same-origin',
                });
            if (r.ok) {
              anyOk = true;
              toast(result.owner
                ? `${label} is now an owner of ${group}`
                : `${label} is no longer an owner of ${group}`);
            } else {
              toast(`owner change failed: ${await r.text()}`, true);
            }
          }
          if (anyOk) refresh();
          return;
        }
        case 'dot-toggle': {
          // The per-agent status light is the agent's sole power
          // control — there are no separate per-agent wake/shutdown
          // row buttons (the dot fully replaces them). It is
          // context-aware:
          //   - offline dot → wake (resume). Non-destructive; starting
          //     a session never needs a confirm.
          //   - online dot → open the 3-way shutdownConfirm modal
          //     (Cancel / Soft exit / Force kill), then stop with the
          //     chosen force flag. The confirm fires for EVERY online
          //     click, idle or busy: the dot's rendered state can be
          //     stale by click time (the snapshot refreshes
          //     asynchronously), so a dot that looks idle may front an
          //     agent that has since started working — skipping the
          //     confirm there would silently interrupt it.
          // online is read from data-* set by agentStatusDot.
          const online = btn.getAttribute('data-online') === '1';
          if (!online) {
            await resumeAgentReq(agent, label);
            return;
          }
          const choice = await shutdownConfirm({label});
          if (!choice) return;
          await stopAgentReq(agent, label, choice === 'force');
          return;
        }
        case 'add-member': {
          // Pop the candidate-list overlay. The overlay manages its
          // own POSTs + optimistic refresh; we just await its
          // close so the trailing toast/refresh logic doesn't fire
          // (the overlay already handled that per-add).
          await addMemberModal(group);
          return;
        }
        case 'spawn-agent': {
          // Open the spawn modal pre-pinned to this group. The
          // modal manages its own POST + refresh on success.
          openAgentSpawnModal({groupName: group});
          return;
        }
        case 'clone': {
          // Open the clone modal pre-populated with this agent. The
          // modal handles the POST + refresh. data-cwd seeds the
          // worktree picker with the source agent's repo.
          openCloneAgentModal(agent, label, btn.getAttribute('data-cwd') || '');
          return;
        }
        case 'reincarnate': {
          // Open the reincarnate modal pre-populated with this
          // agent. The modal enforces the required follow_up and
          // handles the POST + refresh.
          openReincarnateAgentModal(agent, label);
          return;
        }
        case 'export-summary': {
          // Open the export modal — it asks the live agent to produce a
          // shareable artifact, then polls + downloads it. The button is
          // disabled while the agent is offline, so a click means it was
          // online at render; the daemon re-checks and fast-fails if not.
          openExportModal(agent, label);
          return;
        }
        case 'rename-name': {
          // Inline click-to-edit of an agent's title: the .rowname-text
          // span swaps to an <input>, Enter POSTs /api/agents/{conv}/
          // rename {title}, Esc / blur cancels. Same endpoint the edit
          // modal's Save uses for an explicit-title rename. data-act
          // lives on the span itself, so btn IS the click target.
          const oldTitle = btn.getAttribute('data-current') || '';
          inlineEdit({
            el: btn,
            value: oldTitle,
            inputClass: 'rowname-input',
            placeholder: '1-64 chars: A-Za-z0-9 _ - [ ] { } ( ) — Enter saves, Esc cancels',
            onSave: async (raw) => {
              const title = raw.trim();
              if (title === '' || title === oldTitle) return 'revert';
              const r = await fetch(`/api/agents/${encodeURIComponent(agent)}/rename`, {
                method: 'POST', credentials: 'same-origin',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ title }),
              });
              if (!r.ok) {
                toast(`rename failed: ${await r.text()}`, true);
                return 'revert';
              }
              toast(`renaming ${label} → ${title}`);
              return 'saved';
            },
          });
          return;
        }
        case 'rename-group': {
          // Inline edit: replace the group's <strong> label with an
          // <input>. Enter saves (POST /api/groups/{old}/rename),
          // Esc cancels (revert without touching the daemon).
          // Background poll is suspended while editing so a 2s
          // refresh doesn't blow the input away mid-type.
          //
          // The rename button lives in the group ⚙ menu, which sits in
          // the expanded .subtable — a SIBLING of <summary>, not a
          // descendant (moved there in #212). So we can't walk up to the
          // <summary>; instead climb to the enclosing group <details>
          // and query the (single) .group-name it contains. This is the
          // real group render, whose .group-name lives in its summary.
          const details = btn.closest('details');
          const nameEl = details && details.querySelector('.group-name');
          if (!nameEl) {
            toast('rename: could not locate group name element', true);
            return;
          }
          // Suspend the auto-refresh while the input is open. The
          // refresh re-runs renderGroups which would replace our
          // input back with the static strong, losing keystrokes.
          const prevSnapshot = lastSnapshot;
          renameEditing = true;
          const oldName = group;
          const input = document.createElement('input');
          input.type = 'text';
          input.className = 'group-rename-input';
          input.value = oldName;
          input.spellcheck = false;
          input.autocomplete = 'off';
          // Replace + focus + select.
          nameEl.replaceWith(input);
          input.focus();
          input.select();
          const restore = () => {
            const restored = document.createElement('strong');
            restored.className = 'group-name';
            restored.dataset.groupName = oldName;
            restored.textContent = oldName;
            if (input.parentNode) input.replaceWith(restored);
            renameEditing = false;
            setLastSnapshot(prevSnapshot);
          };
          const commit = async () => {
            const newName = input.value;
            if (newName === oldName || newName.trim() === '') {
              restore();
              return;
            }
            const r = await fetch(`/api/groups/${encodeURIComponent(oldName)}/rename`, {
              method: 'POST', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ new_name: newName }),
            });
            if (!r.ok) {
              toast(`rename failed: ${await r.text()}`, true);
              restore();
              return;
            }
            // Move the persisted "is open" flag onto the new key so
            // the details stays in the state the user left it in.
            const wasOpen = dashPrefs.getItem('tclaude.dash.group.' + oldName) === '1';
            dashPrefs.removeItem('tclaude.dash.group.' + oldName);
            if (wasOpen) dashPrefs.setItem('tclaude.dash.group.' + newName, '1');
            renameEditing = false;
            toast(`renamed: ${oldName} → ${newName}`);
            refresh();
          };
          input.addEventListener('keydown', (ev) => {
            if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
            else if (ev.key === 'Escape') { ev.preventDefault(); restore(); }
          });
          input.addEventListener('blur', () => {
            // Blur cancels rather than commits — avoids accidentally
            // posting a stale name when the user clicks elsewhere
            // mid-edit. They have to commit explicitly with Enter.
            if (renameEditing) restore();
          });
          return; // Skip the default refresh; commit() / restore() handle it.
        }
        case 'pick-group-dir': {
          // Click the 📁 icon → open the daemon's native directory
          // picker and save the choice as the group's default spawn dir
          // (PATCH /api/groups/{name}). The text beside it stays a
          // click-to-edit text field via set-group-dir below.
          const startDir = btn.getAttribute('data-cwd') || '';
          const res = await pickDirectory({ startDir, title: `Default spawn directory for "${group}"` });
          if (res.canceled) return;
          if (res.error) { toast(`pick dir failed: ${res.error}`, true); return; }
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
            method: 'PATCH', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ default_cwd: res.path }),
          });
          if (!r.ok) { toast(`set dir failed: ${await r.text()}`, true); return; }
          toast(`${group}: default dir → ${res.path}`);
          refresh();
          return;
        }
        case 'set-group-dir': {
          // Inline edit of the group's default spawn directory.
          // The 📁 chip itself is the click target (data-act lives
          // on the .group-default-cwd span), so btn IS the chip:
          // replace it with an <input>, Enter saves (PATCH
          // /api/groups/{name}), Esc / blur cancels. Auto-refresh
          // suspended via renameEditing so the 2s tick can't drop
          // the input. Fall back to a summary lookup in case the
          // click landed on a descendant rather than the span.
          const cwdEl = btn.classList.contains('group-default-cwd')
            ? btn
            : (btn.closest('summary') && btn.closest('summary').querySelector('.group-default-cwd'));
          if (!cwdEl) {
            toast('start dir: could not locate the dir element', true);
            return;
          }
          const prevSnapshot = lastSnapshot;
          renameEditing = true;
          const origEl = cwdEl.cloneNode(true);
          const oldCwd = cwdEl.getAttribute('data-cwd') || '';
          const input = document.createElement('input');
          input.type = 'text';
          input.className = 'group-default-cwd-input';
          input.value = oldCwd;
          input.placeholder = 'absolute path (~ OK) — empty clears the default';
          input.spellcheck = false;
          input.autocomplete = 'off';
          cwdEl.replaceWith(input);
          input.focus();
          input.select();
          const restore = () => {
            if (input.parentNode) input.replaceWith(origEl);
            renameEditing = false;
            setLastSnapshot(prevSnapshot);
          };
          const commit = async () => {
            const newCwd = input.value.trim();
            if (newCwd === oldCwd) {
              restore();
              return;
            }
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
              method: 'PATCH', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ default_cwd: newCwd }),
            });
            if (!r.ok) {
              toast(`set dir failed: ${await r.text()}`, true);
              restore();
              return;
            }
            renameEditing = false;
            toast(newCwd ? `${group}: default dir → ${newCwd}` : `${group}: default dir cleared`);
            refresh();
          };
          input.addEventListener('keydown', (ev) => {
            if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
            else if (ev.key === 'Escape') { ev.preventDefault(); restore(); }
          });
          input.addEventListener('blur', () => {
            // Blur cancels (like rename) — explicit Enter to save.
            if (renameEditing) restore();
          });
          return; // Skip the default refresh; commit() / restore() handle it.
        }
        case 'set-group-descr': {
          // Inline edit of the group's own description (the 📝 chip).
          // Mirrors set-group-dir: swap the chip for a text <input>,
          // Enter saves (PATCH /api/groups/{name}), Esc / blur
          // cancels. Auto-refresh suspended via renameEditing so the
          // 2s tick can't drop the input mid-edit. Fall back to a
          // summary lookup in case the click landed on a descendant.
          const descrEl = btn.classList.contains('group-descr')
            ? btn
            : (btn.closest('summary') && btn.closest('summary').querySelector('.group-descr'));
          if (!descrEl) {
            toast('description: could not locate the description element', true);
            return;
          }
          const prevSnapshot = lastSnapshot;
          renameEditing = true;
          const origEl = descrEl.cloneNode(true);
          const oldDescr = descrEl.getAttribute('data-descr') || '';
          const input = document.createElement('input');
          input.type = 'text';
          input.className = 'group-descr-input';
          input.value = oldDescr;
          input.placeholder = 'group description — empty clears it';
          input.spellcheck = false;
          input.autocomplete = 'off';
          descrEl.replaceWith(input);
          input.focus();
          input.select();
          const restore = () => {
            if (input.parentNode) input.replaceWith(origEl);
            renameEditing = false;
            setLastSnapshot(prevSnapshot);
          };
          const commit = async () => {
            const newDescr = input.value.trim();
            if (newDescr === oldDescr) {
              restore();
              return;
            }
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
              method: 'PATCH', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ descr: newDescr }),
            });
            if (!r.ok) {
              toast(`set description failed: ${await r.text()}`, true);
              restore();
              return;
            }
            renameEditing = false;
            toast(newDescr ? `${group}: description → ${newDescr}` : `${group}: description cleared`);
            refresh();
          };
          input.addEventListener('keydown', (ev) => {
            if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
            else if (ev.key === 'Escape') { ev.preventDefault(); restore(); }
          });
          input.addEventListener('blur', () => {
            // Blur cancels (like rename) — explicit Enter to save.
            if (renameEditing) restore();
          });
          return; // Skip the default refresh; commit() / restore() handle it.
        }
        case 'set-group-max-members': {
          // Inline edit of the group's hard member cap (the 👥
          // chip). Mirrors set-group-dir: swap the chip for a number
          // <input>, Enter PATCHes /api/groups/{name}, Esc / blur
          // cancels. Auto-refresh suspended via renameEditing so the
          // 2s tick can't drop the input mid-edit.
          const capEl = btn.classList.contains('group-max-members')
            ? btn
            : (btn.closest('summary') && btn.closest('summary').querySelector('.group-max-members'));
          if (!capEl) {
            toast('max members: could not locate the cap element', true);
            return;
          }
          const prevSnapshot = lastSnapshot;
          renameEditing = true;
          const origEl = capEl.cloneNode(true);
          const oldMax = parseInt(capEl.getAttribute('data-max') || '0', 10) || 0;
          const input = document.createElement('input');
          input.type = 'number';
          input.min = '0';
          input.step = '1';
          input.className = 'group-max-members-input';
          input.value = String(oldMax);
          input.title = '0 clears the cap (unlimited)';
          capEl.replaceWith(input);
          input.focus();
          input.select();
          const restore = () => {
            if (input.parentNode) input.replaceWith(origEl);
            renameEditing = false;
            setLastSnapshot(prevSnapshot);
          };
          const commit = async () => {
            const newMax = parseInt(input.value, 10);
            if (!Number.isInteger(newMax) || newMax < 0) {
              toast('max members must be a non-negative integer (0 = unlimited)', true);
              restore();
              return;
            }
            if (newMax === oldMax) {
              restore();
              return;
            }
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
              method: 'PATCH', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ max_members: newMax }),
            });
            if (!r.ok) {
              toast(`set max members failed: ${await r.text()}`, true);
              restore();
              return;
            }
            renameEditing = false;
            toast(newMax > 0 ? `${group}: member cap → ${newMax}` : `${group}: member cap cleared`);
            refresh();
          };
          input.addEventListener('keydown', (ev) => {
            if (ev.key === 'Enter') { ev.preventDefault(); commit(); }
            else if (ev.key === 'Escape') { ev.preventDefault(); restore(); }
          });
          input.addEventListener('blur', () => {
            if (renameEditing) restore();
          });
          return; // Skip the default refresh; commit() / restore() handle it.
        }
        case 'set-group-profile': {
          // The group 🧠 chip: pick the group's default spawn profile from a
          // <select> of saved profiles (+ "(none)"). PATCH /api/groups/{name}
          // {default_profile}, then refresh() so the badge repaints. The chip
          // span is the click target; fall back to a summary lookup if the
          // click landed on a descendant.
          const chipEl = btn.classList.contains('group-default-model')
            ? btn
            : (btn.closest('summary') && btn.closest('summary').querySelector('.group-default-model'));
          if (!chipEl) {
            toast('default profile: could not locate the chip', true);
            return;
          }
          const current = chipEl.getAttribute('data-profile') || '';
          await openProfilePicker(chipEl, current, async (name) => {
            const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
              method: 'PATCH', credentials: 'same-origin',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ default_profile: name }),
            });
            if (!r.ok) {
              toast(`set default profile failed: ${await r.text()}`, true);
              return false;
            }
            toast(name ? `${group}: default profile → ${name}` : `${group}: default profile cleared`);
            refresh();
            return true;
          });
          return; // openProfilePicker owns the refresh.
        }
        case 'set-group-remote-control': {
          // The group remote-control-policy chip: cycle the group's
          // remote_control_policy inherit → optin → deny → inherit. The chip's
          // data-next carries the value one click sends (computed at render
          // time, mirroring the per-agent notify bell). The policy is a spawn
          // DEFAULT that overrides each spawn profile's own remote_control default
          // — inherit defers to the profile, optin/deny default it on/off — but a
          // per-spawn checkbox / flag still wins over it (JOH-262 revised).
          // PATCH /api/groups/{name} {remote_control_policy}, the same endpoint
          // + method the default_profile / notify_enabled chips use.
          const next = btn.getAttribute('data-next') || 'inherit';
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
            method: 'PATCH', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ remote_control_policy: next }),
          });
          if (!r.ok) { toast(`set remote-control policy failed: ${await r.text()}`, true); return; }
          const pretty = next === 'optin' ? 'opt-in (default on)' : next === 'deny' ? 'deny (default off)' : 'inherit (defer to profile)';
          toast(`${group}: remote-control policy → ${pretty}`);
          refresh();
          return;
        }
        // 'set-group-model' was retired with the per-group default_model
        // (JOH-210): the group 🧠 chip is now a clickable spawn-profile picker
        // (set-group-profile, above), not a model editor. No data-act emits
        // this case.
        case 'set-dash-profile': {
          // The dashboard-level 🧠 chip (groups filter bar): pick the
          // dashboard default spawn profile, which pre-fills the spawn dialog
          // when a group has no default of its own. Pure client state — stored
          // in dashPrefs, not on the server — so onCommit persists locally and
          // re-renders the (static-HTML) chip rather than refresh()ing. This
          // replaced the retired user-default-model chip (JOH-210 inc3).
          const current = btn.getAttribute('data-profile') || '';
          await openProfilePicker(btn, current, async (name) => {
            setDashDefaultProfile(name);
            toast(name ? `dashboard default profile → ${name}` : 'dashboard default profile cleared');
            renderDashDefaultProfile();
            return true;
          });
          return; // openProfilePicker owns the chip lifecycle + re-render.
        }
        case 'export-group': {
          // Export is a file download, not a mutation. Trigger it via
          // a transient anchor so the browser saves the .zip (the
          // endpoint sets Content-Disposition); the cookie rides along
          // on the same-origin GET. Return so the default toast +
          // refresh do not fire — nothing changed.
          const a = document.createElement('a');
          a.href = `/api/groups/${encodeURIComponent(group)}/export`;
          a.download = '';
          document.body.appendChild(a);
          a.click();
          a.remove();
          toast(`Exporting group "${group}"…`);
          return;
        }
        case 'cleanup-group': {
          // Open the bulk-cleanup overlay scoped to this group. The
          // modal manages its own POST + refresh on success.
          openCleanupModal({ mode: 'group', group });
          return;
        }
        case 'cleanup-worktrees-group': {
          // Open the repo-wide worktree janitor scoped to this group's
          // repo(s). The modal loads + classifies the candidates, owns
          // its POST to /api/worktrees/cleanup, and refreshes on success.
          openWorktreeCleanup(group);
          return;
        }
        case 'shutdown-group': {
          // shutdownScope owns its confirm modal, POST, toast
          // and refresh — return so the default toast doesn't fire.
          await shutdownScope('group', group);
          return;
        }
        case 'shutdown-all': {
          // The top-bar button: shut down every running agent the
          // dashboard shows. No group context.
          await shutdownScope('all', null);
          return;
        }
        case 'power-on-group': {
          // The inverse of shutdown-group: resume every offline agent
          // in this group. powerOnScope owns its confirm + POST + toast.
          await powerOnScope('group', group);
          return;
        }
        case 'power-on-all': {
          // The top-bar button: resume every offline agent the
          // dashboard shows. No group context.
          await powerOnScope('all', null);
          return;
        }
        case 'window-modal-group': {
          // openWindowModal owns its modal, POST and toast — return
          // so the default toast/refresh doesn't fire.
          openWindowModal('group', group);
          return;
        }
        case 'window-modal-all': {
          // The top-bar button: focus/unfocus windows across every
          // agent on the dashboard. No group context.
          openWindowModal('all', null);
          return;
        }
        case 'set-group-context': {
          // Open the group startup-context editor. Unlike the cwd
          // chip's inline <input>, the context is multi-line, so it
          // gets its own modal with a <textarea>.
          openGroupContextModal(group);
          return; // Modal owns the save + refresh.
        }
        case 'clone-group': {
          // Open the clone-group modal (new name + with/without agents).
          // The modal owns its POST + toast + refresh.
          openGroupCloneModal(group);
          return;
        }
        case 'template-from-group': {
          // Open the save-group-as-template modal with this group
          // preselected — the quick "turn this working group into a
          // reusable blueprint" path. The modal owns submit + refresh.
          openFromGroupModal(group);
          return;
        }
        case 'delete-group': {
          const memberCount = parseInt(btn.getAttribute('data-members') || '0', 10);
          const confirmed = await confirmModal({
            title: 'Delete group?',
            body: `This drops the group plus all ${memberCount} membership row(s), any owner grants, and the entire group message history. The conversations themselves keep running.`,
            meta: group,
            okLabel: 'Delete group',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Delete failed: ${await r.text()}`, true);
          break;
        }
        case 'revoke-owner': {
          const confirmed = await confirmModal({
            title: 'Revoke owner status?',
            body: 'They will lose the implicit power to manage other members of this group (message, reincarnate, compact, rename, clone). The membership row stays.',
            meta: `${label} → ${group}`,
            okLabel: 'Revoke',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}/owners/${encodeURIComponent(agent)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Revoke failed: ${await r.text()}`, true);
          break;
        }
        case 'export-job-download': {
          // Browser download of a ready export's artifact — no state change,
          // nothing to refresh.
          triggerExportDownload(btn.getAttribute('data-id'));
          return;
        }
        case 'export-job-dismiss': {
          const id = btn.getAttribute('data-id');
          const confirmed = await confirmModal({
            title: 'Dismiss this export?',
            body: 'Removes the export job from the Jobs list and deletes its file from the server (if one was delivered). A still-running job is discarded when it lands.',
            meta: label,
            okLabel: 'Dismiss',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/export-jobs/${encodeURIComponent(id)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`dismiss failed: ${await r.text()}`, true);
          break;
        }
        case 'cron-enable':
        case 'cron-disable': {
          const id = btn.getAttribute('data-id');
          const verb = act === 'cron-enable' ? 'enable' : 'disable';
          const r = await fetch(`/api/cron/${encodeURIComponent(id)}/${verb}`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`${verb} failed: ${await r.text()}`, true);
          break;
        }
        case 'cron-run-now': {
          const id = btn.getAttribute('data-id');
          // Run-now is non-destructive (it just fires the job once)
          // but it does send a real message to the target — confirm
          // so a stray click doesn't paste into someone's pane.
          const confirmed = await confirmModal({
            title: 'Fire this cron job now?',
            body: 'Sends the job\'s message to its target immediately. Stamps last_run_at so the regular cadence resumes from now.',
            meta: label,
            okLabel: 'Fire now',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/cron/${encodeURIComponent(id)}/run-now`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`run-now failed: ${await r.text()}`, true);
          break;
        }
        case 'cron-delete': {
          const id = btn.getAttribute('data-id');
          const confirmed = await confirmModal({
            title: 'Delete cron job?',
            body: 'Removes the job and its run history. The target itself is unaffected; you can re-create the job with `tclaude agent cron add`.',
            meta: label,
            okLabel: 'Delete job',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/cron/${encodeURIComponent(id)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`delete failed: ${await r.text()}`, true);
          break;
        }
        case 'cron-edit': {
          const id = parseInt(btn.getAttribute('data-id'), 10);
          const job = (lastSnapshot?.cron || []).find(j => j.id === id);
          if (!job) {
            toast(`edit: job #${id} not in current snapshot`, true);
            return;
          }
          openCronEditModal(job);
          return;
        }
        case 'cron-new': {
          // Context-aware "+ new cron job" buttons from the Agents
          // tab (per-agent + per-group) land here. data-prefill is a JSON blob
          // describing the prefill state (targetMode, target,
          // groupName, owner). Empty / missing → default form.
          let prefill = {};
          const raw = btn.getAttribute('data-prefill');
          if (raw) {
            try { prefill = JSON.parse(raw); } catch (_) {}
          }
          openCronCreateModal(prefill);
          return;
        }
        case 'message-new': {
          // Context-aware "send message" buttons from the Agents row
          // (✉, solo) and group headers (✉ message, group multicast).
          // data-prefill is a JSON blob: { from, targetMode, target,
          // groupName }. Empty / missing → default form.
          let prefill = {};
          const raw = btn.getAttribute('data-prefill');
          if (raw) {
            try { prefill = JSON.parse(raw); } catch (_) {}
          }
          openMessageCreateModal(prefill);
          return;
        }
        case 'view-agent-messages': {
          // Deep link from a member-row ⚙ menu: jump to the Messages tab
          // and open this agent's mailbox folder. Read-only navigation —
          // no daemon round-trip, no refresh.
          openMailbox(conv);
          return;
        }
        case 'view-group-messages': {
          // Deep link from a group ⚙ menu: jump to the Messages tab and
          // open this group's folder (all member traffic + the group's
          // multicasts). The "group:<name>" id matches the server's
          // groupMailboxPrefix.
          openMailbox('group:' + group);
          return;
        }
        case 'link-new': {
          // From per-group "+ link" button: preset FROM to the
          // current group so the user only has to pick TO.
          const from = btn.getAttribute('data-from') || '';
          openLinkModal({ mode: 'create', preset: { from } });
          return;
        }
        case 'links-manage': {
          // From a group-header link chip: open the full cross-group
          // Links… management overlay (the former Links tab).
          openLinksManageModal();
          return;
        }
        case 'link-edit': {
          const id = btn.getAttribute('data-id');
          const from = btn.getAttribute('data-from') || '';
          const to = btn.getAttribute('data-to') || '';
          const linkMode = btn.getAttribute('data-mode') || '';
          openLinkModal({ mode: 'edit', linkID: id, preset: { from, to, linkMode } });
          return;
        }
        case 'link-delete': {
          const id = btn.getAttribute('data-id');
          const from = btn.getAttribute('data-from') || '';
          const to = btn.getAttribute('data-to') || '';
          // The dashboard's DELETE endpoint requires the link to
          // touch the group in the URL — pass the from group when
          // available, otherwise fall back to the explicit data-group
          // attribute.
          const scope = btn.getAttribute('data-group') || from || to;
          const confirmed = await confirmModal({
            title: 'Remove this link?',
            body: 'Members of FROM lose the ability to message members of TO via this edge. Other groups / links are unaffected. This can\'t be undone — recreate to restore.',
            meta: `#${id} · ${from} → ${to}`,
            okLabel: 'Remove link',
          });
          if (!confirmed) return;
          const r = await fetch(`/api/groups/${encodeURIComponent(scope)}/links/${encodeURIComponent(id)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Remove link failed: ${await r.text()}`, true);
          break;
        }
        case 'msg-focus': {
          // Focus the sending agent's terminal AND mark the message read — the
          // human is acting on it. Both are non-destructive; refresh after so
          // the read state + badge update. Focus behaves exactly like the
          // per-row 'jump' eye button: jump to an already-open web pane if there
          // is one; else, with web terminals as the default (config
          // dashboard.default_terminal = "web"), open the agent's live session
          // as a browser pane; else raise the native OS window.
          const id = btn.getAttribute('data-id');
          if (focusTerminalForConv([conv])) {
            toast(`focused: ${label}`);
          } else if (webTerminalDefault()) {
            openWebWindowPane(conv, label);
            toast(`focused: ${label}`);
          } else {
            const jr = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
              method: 'POST', credentials: 'same-origin',
            });
            if (jr.ok) toast(`focused: ${label}`);
            else toast(`Focus failed: ${await jr.text()}`, true);
          }
          const rr = await fetch('/api/human-messages/read', {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: parseInt(id, 10) }),
          });
          // Surface a read failure rather than swallow it (parity with
          // msg-mark-read); the jump already happened, so still refresh.
          if (!rr.ok) toast(`Mark read failed: ${await rr.text()}`, true);
          refresh();
          return;
        }
        case 'msg-reply': {
          // Open the reply dialog for this notification. The dialog owns
          // the online gate + Send; here we just hand it the id + sender
          // attributes the reader button carried.
          openHumanReplyModal({
            id: btn.getAttribute('data-id'),
            agent: btn.getAttribute('data-agent') || '',
            conv: btn.getAttribute('data-conv') || '',
            label: btn.getAttribute('data-label') || conv,
            subject: btn.getAttribute('data-subject') || '',
          });
          return;
        }
        case 'msg-mark-read': {
          const id = btn.getAttribute('data-id');
          const r = await fetch('/api/human-messages/read', {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: parseInt(id, 10) }),
          });
          if (!r.ok) { toast(`Mark read failed: ${await r.text()}`, true); return; }
          toast('message marked read');
          refresh();
          return;
        }
        case 'msg-mark-unread': {
          // The opt-out for the auto-mark-on-open: flag a read notification
          // back to unread, like a mail client. Same endpoint as mark-read,
          // with read:false.
          const id = btn.getAttribute('data-id');
          const r = await fetch('/api/human-messages/read', {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: parseInt(id, 10), read: false }),
          });
          if (!r.ok) { toast(`Mark unread failed: ${await r.text()}`, true); return; }
          toast('message marked unread');
          refresh();
          return;
        }
        case 'msg-mark-all-read': {
          const r = await fetch('/api/human-messages/read', {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ all: true }),
          });
          if (!r.ok) { toast(`Mark all read failed: ${await r.text()}`, true); return; }
          const res = await r.json().catch(() => ({}));
          toast(`marked ${res.marked || 0} message(s) read`);
          refresh();
          return;
        }
        case 'msg-clear': {
          const confirmed = await confirmModal({
            title: 'Clear read messages?',
            body: 'Permanently deletes every message that has been marked read. Unread messages are kept.',
            okLabel: 'Clear read',
          });
          if (!confirmed) return;
          const r = await fetch('/api/human-messages/clear', {
            method: 'POST', credentials: 'same-origin',
          });
          if (!r.ok) { toast(`Clear failed: ${await r.text()}`, true); return; }
          const res = await r.json().catch(() => ({}));
          toast(`cleared ${res.deleted || 0} read message(s)`);
          refresh();
          return;
        }
        case 'msg-delete': {
          // Per-message delete — read OR unread, the single-row
          // complement to "clear read". Confirm before the hard delete.
          const id = btn.getAttribute('data-id');
          const confirmed = await confirmModal({
            title: 'Delete this message?',
            body: 'Permanently deletes this one message, read or unread. This cannot be undone.',
            meta: `#${id}`,
            okLabel: 'Delete',
          });
          if (!confirmed) return;
          const r = await fetch('/api/human-messages/delete', {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: parseInt(id, 10) }),
          });
          if (!r.ok) { toast(`Delete failed: ${await r.text()}`, true); return; }
          toast('message deleted');
          refresh();
          return;
        }
        case 'row-menu':
        case 'group-menu':
        case 'filter-bar-menu': {
          // The ⚙ cog: toggle this row's / group's / filter-bar's
          // options menu. The menu is the cog's sibling inside the
          // surrounding .row-actions / .group-actions / .filter-bar-cog;
          // for a group cog the e.preventDefault() above already stops
          // the click from toggling the <details>.
          // closeAllActionMenus first so opening one always closes any
          // other; opening a menu suspends the auto-refresh
          // (refreshSuspended sees .action-menu.open) so a 2s poll
          // can't re-render it away mid-use.
          const menu = btn.parentElement
            && btn.parentElement.querySelector('.action-menu');
          const willOpen = !!menu && !menu.classList.contains('open');
          closeAllActionMenus();
          if (willOpen) {
            menu.classList.remove('opens-up');
            menu.classList.add('open');
            btn.setAttribute('aria-expanded', 'true');
            // Flip the menu above the cog when its default downward
            // position would run off the viewport bottom — but only
            // when it actually fits above.
            const mr = menu.getBoundingClientRect();
            if (mr.bottom > window.innerHeight
                && mr.height < btn.getBoundingClientRect().top) {
              menu.classList.add('opens-up');
            }
          }
          return;
        }
        default:
          return;
      }
      if (ok) {
        toast(`${act.replace('-', ' ')}: ${label}`);
        refresh();
      }
    } catch (err) {
      toast(`Request failed: ${err && err.message || err}`, true);
    }
  });

  // Escape closes any open ⚙ options menu — parity with the modal /
  // inline-edit Escape handling. closeAllActionMenus restores focus to
  // the owning cog when focus sat inside the menu.
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!document.querySelector('.action-menu.open')) return;
    e.preventDefault();
    closeAllActionMenus();
  });
}

export { bindRowActions, renameEditing };
