// row-actions.js — bindRowActions, the delegated click router for every
// per-row action button across the dashboard's tables.
//
// Extracted from dashboard.js in the Stage 2 module split. Owns
// renameEditing — the inline-rename-open flag refreshSuspended consults.

import { $, $$, shortId, groupOfflineOverride } from './helpers.js';
import { renderGroupsTab, renderSudoTab } from './tabs.js';
import {
  openSudoGrantModal, openCronCreateModal, openCronEditModal,
} from './modal-cron.js';
import { openMessageCreateModal, openPermEditModal } from './modal-message.js';
import { openGroupContextModal } from './modal-templates.js';
import { openLinkModal } from './modal-link-wt.js';
import {
  openAgentSpawnModal, openCloneAgentModal,
  openReincarnateAgentModal, openRenameAgentModal,
} from './modal-spawn.js';
// refresh()/toast() and the shared action modals live in refresh.js;
// lastSnapshot is dashboard.js's shared state, written here (rename
// rollback) via setLastSnapshot. Deliberate benign cycles (see
// render.js); TDZ-safe.
import {
  refresh, toast, confirmModal, addMemberModal, deleteAgentModal,
  editMemberModal, emergencyShutdown, openCleanupModal, openWindowModal,
  resumeAgentReq, retireConfirm, shutdownConfirm, stopAgentReq, termDirModal,
} from './refresh.js';
import { lastSnapshot, setLastSnapshot } from './dashboard.js';

// True while an inline rename input is open; suspends the auto-
// refresh so the 5s tick doesn't blow the input away mid-edit.
let renameEditing = false;
// bindRowActions delegates clicks on row-action buttons to the
// appropriate /api/groups/... call. After a successful mutation we
// re-fetch the snapshot so the badge / button state updates.
function bindRowActions() {
  document.addEventListener('click', async (e) => {
    const btn = e.target.closest('[data-act]');
    if (!btn) return;
    // Buttons may live inside <summary>, where the default click
    // action is to toggle the details. Stop that.
    e.preventDefault();
    const act = btn.getAttribute('data-act');
    const group = btn.getAttribute('data-group');
    const conv = btn.getAttribute('data-conv');
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
          if (next === 'inherit') localStorage.removeItem(okey);
          else localStorage.setItem(okey, next);
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
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(conv)}`, {
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
            body: JSON.stringify({ conv }),
          });
          ok = r.ok;
          if (!ok) toast(`Grant owner failed: ${await r.text()}`, true);
          break;
        }
        case 'jump': {
          // Non-destructive; no confirm modal, just fire-and-toast.
          const r = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Jump failed: ${await r.text()}`, true);
          // Skip the default refresh — focusing doesn't change any
          // dashboard state and the user just left the window.
          if (ok) toast(`focused: ${label}`);
          return;
        }
        case 'term': {
          // Pick which directory, then ask the daemon to spawn a
          // terminal window there. Non-destructive and changes no
          // dashboard state, so skip the refresh.
          const which = await termDirModal({ label });
          if (!which) return;
          const r = await fetch(`/api/term/${encodeURIComponent(conv)}`, {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ which }),
          });
          if (!r.ok) { toast(`Open terminal failed: ${await r.text()}`, true); return; }
          const info = await r.json().catch(() => ({}));
          toast(`terminal opened: ${info.dir || label}`);
          return;
        }
        case 'term-dir': {
          // Click on a CWD path cell — the cell already names one
          // specific directory, so open a terminal there straight
          // away, skipping the term button's 3-way picker modal.
          const which = btn.getAttribute('data-which') || 'current';
          const r = await fetch(`/api/term/${encodeURIComponent(conv)}`, {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ which }),
          });
          if (!r.ok) { toast(`Open terminal failed: ${await r.text()}`, true); return; }
          const info = await r.json().catch(() => ({}));
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
          // Click on the 🔓 badge: switch to the Sudo tab pre-
          // filtered to this agent so the human can revoke specific
          // grants without scrolling through unrelated rows.
          const filterInput = $('#filter-sudo');
          filterInput.value = shortId(conv);
          try { localStorage.setItem('tclaude.dash.filter.sudo', filterInput.value); } catch (_) {}
          $$('nav button').forEach(x => x.classList.toggle('active', x.dataset.tab === 'sudo'));
          $$('main section').forEach(s => s.classList.toggle('active', s.id === 'tab-sudo'));
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
          const choice = await retireConfirm({ label });
          if (!choice) return;
          const q = choice.shutdown ? '?shutdown=1' : '?shutdown=0';
          const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/retire${q}`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) { toast(`Retire failed: ${await r.text()}`, true); break; }
          toast(choice.shutdown ? `retired + session stopped: ${label}` : `retired: ${label}`);
          break;
        }
        case 'reinstate-agent': {
          const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reinstate`, {
            method: 'POST', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) { toast(`Reinstate failed: ${await r.text()}`, true); break; }
          toast(`reinstated: ${label}`);
          break;
        }
        case 'delete-agent': {
          const choice = await deleteAgentModal(conv, label);
          if (!choice) return;
          const q = choice.deleteWorktree ? '?delete_worktree=1' : '';
          const r = await fetch(`/api/agents/${encodeURIComponent(conv)}${q}`, {
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
        case 'edit-member': {
          const cur = {
            role: btn.getAttribute('data-role') || '',
            descr: btn.getAttribute('data-descr') || '',
          };
          const result = await editMemberModal({
            label: `${label} → ${group}`,
            role: cur.role, descr: cur.descr,
          });
          if (result === null) return; // cancelled
          if (result === 'noop') {
            toast('no changes');
            return;
          }
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}/members/${encodeURIComponent(conv)}`, {
            method: 'PATCH', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(result),
          });
          ok = r.ok;
          if (!ok) toast(`edit failed: ${await r.text()}`, true);
          break;
        }
        case 'wake-agent': {
          // Resume is non-destructive (only spawns a tmux session;
          // the conv jsonl is unchanged). No confirm modal — fire +
          // toast + refresh on success. Idempotent server-side.
          await resumeAgentReq(conv, label);
          return; // Skip the default toast — resumeAgentReq toasted.
        }
        case 'shutdown-agent': {
          const choice = await shutdownConfirm({label});
          if (!choice) return;
          await stopAgentReq(conv, label, choice === 'force');
          return; // Skip the default toast — stopAgentReq toasted.
        }
        case 'dot-toggle': {
          // The per-agent status light doubles as an on/off toggle.
          // It reuses the same resume / stop endpoints the "wake" and
          // "shut down" row buttons hit — no parallel endpoint.
          //   - offline dot → wake (resume). Non-destructive; starting
          //     a session never needs a confirm.
          //   - online dot → confirm first, then soft-stop. The
          //     confirm fires for EVERY online click, idle or busy.
          //     The dot's rendered state can be stale by click time
          //     (the snapshot refreshes asynchronously), so a dot
          //     that looks idle may front an agent that has since
          //     started working — skipping the confirm there would
          //     silently interrupt it. Always asking closes that race
          //     and keeps every green-dot click behaving identically.
          // online is read from data-* set by agentStatusDot.
          const online = btn.getAttribute('data-online') === '1';
          if (!online) {
            await resumeAgentReq(conv, label);
            return;
          }
          const confirmed = await confirmModal({
            title: 'Turn off this agent?',
            body: 'Turning this agent off injects /exit into its pane. If it is mid-task, any in-flight tool call is interrupted. The conversation is preserved and the agent can be turned back on (resumed) later — nothing is deleted or retired.',
            meta: label,
            okLabel: 'Turn off',
          });
          if (!confirmed) return;
          await stopAgentReq(conv, label, false);
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
          openCloneAgentModal(conv, label, btn.getAttribute('data-cwd') || '');
          return;
        }
        case 'reincarnate': {
          // Open the reincarnate modal pre-populated with this
          // agent. The modal enforces the required follow_up and
          // handles the POST + refresh.
          openReincarnateAgentModal(conv, label);
          return;
        }
        case 'rename-agent': {
          const current = btn.getAttribute('data-current') || '';
          openRenameAgentModal(conv, label, current);
          return;
        }
        case 'rename-group': {
          // Inline edit: replace the group's <strong> label with an
          // <input>. Enter saves (POST /api/groups/{old}/rename),
          // Esc cancels (revert without touching the daemon).
          // Background poll is suspended while editing so a 5s
          // refresh doesn't blow the input away mid-type.
          const summary = btn.closest('summary');
          const nameEl = summary && summary.querySelector('.group-name');
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
            const wasOpen = localStorage.getItem('tclaude.dash.group.' + oldName) === '1';
            localStorage.removeItem('tclaude.dash.group.' + oldName);
            if (wasOpen) localStorage.setItem('tclaude.dash.group.' + newName, '1');
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
        case 'set-group-dir': {
          // Inline edit of the group's default spawn directory.
          // The 📁 chip itself is the click target (data-act lives
          // on the .group-default-cwd span), so btn IS the chip:
          // replace it with an <input>, Enter saves (PATCH
          // /api/groups/{name}), Esc / blur cancels. Auto-refresh
          // suspended via renameEditing so the 5s tick can't drop
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
          input.placeholder = 'absolute path — empty clears the default';
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
        case 'set-group-max-members': {
          // Inline edit of the group's hard member cap (the 👥
          // chip). Mirrors set-group-dir: swap the chip for a number
          // <input>, Enter PATCHes /api/groups/{name}, Esc / blur
          // cancels. Auto-refresh suspended via renameEditing so the
          // 5s tick can't drop the input mid-edit.
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
        case 'emergency-shutdown-group': {
          // emergencyShutdown owns its confirm modal, POST, toast
          // and refresh — return so the default toast doesn't fire.
          await emergencyShutdown('group', group);
          return;
        }
        case 'emergency-shutdown-all': {
          // The top-bar button: shut down every running agent the
          // dashboard shows. No group context.
          await emergencyShutdown('all', null);
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
          const r = await fetch(`/api/groups/${encodeURIComponent(group)}/owners/${encodeURIComponent(conv)}`, {
            method: 'DELETE', credentials: 'same-origin',
          });
          ok = r.ok;
          if (!ok) toast(`Revoke failed: ${await r.text()}`, true);
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
        case 'link-new': {
          // From per-group "+ link" button: preset FROM to the
          // current group so the user only has to pick TO.
          const from = btn.getAttribute('data-from') || '';
          openLinkModal({ mode: 'create', preset: { from } });
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
          // Raise the sending agent's window AND mark the message
          // read — the human is acting on it. Both are non-destructive;
          // toast each, then refresh so the read state + badge update.
          const id = btn.getAttribute('data-id');
          const jr = await fetch(`/api/jump/${encodeURIComponent(conv)}`, {
            method: 'POST', credentials: 'same-origin',
          });
          if (jr.ok) toast(`focused: ${label}`);
          else toast(`Focus failed: ${await jr.text()}`, true);
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
}

export { bindRowActions, renameEditing };
