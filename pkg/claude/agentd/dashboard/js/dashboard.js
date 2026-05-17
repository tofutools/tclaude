import {
  $, $$, esc, shortId, relTime, groupOfflineOverride,
} from './helpers.js';
import { cycleSort } from './sort.js';
import {
  renderPermissions, renderSlugs, showStatus,
  renderMessagesBadge, renderMessagesTab, renderUsage,
} from './render.js';
import {
  renderGroupsTab, renderCronTab, renderSudoTab, renderLinksTab, fmtRemaining,
} from './tabs.js';
import {
  openSudoGrantModal, openCronCreateModal, openCronEditModal, bindCronModal,
} from './modal-cron.js';
import {
  openMessageCreateModal, bindMessageModal, bindSudoModal,
  openPermEditModal, bindPermEditModal, bindGroupCreateModal,
} from './modal-message.js';
import {
  renderTemplatesTab, bindTemplatesUI, bindGroupImportModal,
  openGroupContextModal, bindGroupContextModal,
} from './modal-templates.js';
import { openLinkModal, bindLinkModal } from './modal-link-wt.js';
import {
  openAgentSpawnModal, bindAgentSpawnModal,
  openCloneAgentModal, bindCloneAgentModal,
  openReincarnateAgentModal, bindReincarnateAgentModal,
  openRenameAgentModal, bindRenameAgentModal,
} from './modal-spawn.js';


  // Last successful snapshot, kept so the filter inputs can re-render
  // without a server roundtrip when the user types.
  export let lastSnapshot = null;
  // True while an inline rename input is open; suspends the auto-
  // refresh so the 5s tick doesn't blow the input away mid-edit.
  let renameEditing = false;

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
    // Any modal overlay is open.
    if (document.querySelector('.modal-overlay.show')) return true;
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

  function bindFilter(tab) {
    const input = $(`#filter-${tab}`);
    const clear = $(`#filter-${tab}-clear`);
    const key = `tclaude.dash.filter.${tab}`;
    input.value = localStorage.getItem(key) || '';
    const rerender = () => {
      if (tab === 'groups') renderGroupsTab();
      else if (tab === 'templates') renderTemplatesTab();
      else if (tab === 'cron') renderCronTab();
      else if (tab === 'sudo') renderSudoTab();
      else if (tab === 'links') renderLinksTab();
      else if (tab === 'messages') renderMessagesTab();
    };
    const onChange = () => {
      const v = input.value;
      if (v) localStorage.setItem(key, v); else localStorage.removeItem(key);
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
      const saved = localStorage.getItem(okey);
      offline.checked = saved === null ? true : saved === '1';
      offline.addEventListener('change', () => {
        localStorage.setItem(okey, offline.checked ? '1' : '0');
        rerender();
      });
    }
    // Optional "show ungrouped" checkbox (groups tab only) — toggles
    // the virtual Ungrouped group. Persisted like the offline toggle;
    // defaults to checked when the user has never touched it.
    const ungrouped = $(`#filter-${tab}-ungrouped`);
    if (ungrouped) {
      const ukey = `tclaude.dash.ungrouped.${tab}`;
      const saved = localStorage.getItem(ukey);
      ungrouped.checked = saved === null ? true : saved === '1';
      ungrouped.addEventListener('change', () => {
        localStorage.setItem(ukey, ungrouped.checked ? '1' : '0');
        rerender();
      });
    }
    // Optional "show conversations" checkbox (groups tab only) —
    // toggles the virtual Conversations group. Defaults OFF (there can
    // be many conversations) when the user has never touched it.
    const conversations = $(`#filter-${tab}-conversations`);
    if (conversations) {
      const ckey = `tclaude.dash.conversations.${tab}`;
      const saved = localStorage.getItem(ckey);
      conversations.checked = saved === '1';
      conversations.addEventListener('change', () => {
        localStorage.setItem(ckey, conversations.checked ? '1' : '0');
        rerender();
      });
    }
    // Optional "show retired" checkbox (groups tab only) — toggles the
    // virtual Retired group. Defaults ON: a retired agent must stay
    // visible somewhere on the tab rather than silently disappearing.
    const retired = $(`#filter-${tab}-retired`);
    if (retired) {
      const rkey = `tclaude.dash.retired.${tab}`;
      const saved = localStorage.getItem(rkey);
      retired.checked = saved === null ? true : saved === '1';
      retired.addEventListener('change', () => {
        localStorage.setItem(rkey, retired.checked ? '1' : '0');
        rerender();
      });
    }
  }

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
      lastSnapshot = data;
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
      $('#tab-permissions').innerHTML = renderPermissions(data.permissions, data.agents);
      $('#tab-slugs').innerHTML = renderSlugs(data.slugs);
      renderMessagesTab();
      renderMessagesBadge(data.messages_unread || 0);
      renderUsage(data.usage);
      showStatus('● live', false);
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
        localStorage.setItem('tclaude.dash.group.' + key, '1');
      } else {
        localStorage.removeItem('tclaude.dash.group.' + key);
      }
    }, true);
  }

  // bindSortHeaders delegates clicks on sortable <th> cells. Headers
  // are re-rendered on every 5s refresh, so a single document-level
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
  // OK, false on Cancel / outside-click / Escape.
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
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onOk = () => cleanup(true);
      const onCancel = () => cleanup(false);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(false); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(false); };
      okBtn.addEventListener('click', onOk);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      okBtn.focus();
    });
  }

  // emergencyShutdown drives the group-level and whole-dashboard
  // emergency-shutdown buttons. It counts the running agents in scope
  // from the last snapshot, pops a confirm modal that states the
  // count and spells out that this is stop-only (no data deleted),
  // POSTs /api/emergency-shutdown, then toasts the outcome summary.
  // scope is "group" (groupName set) or "all" (groupName ignored).
  async function emergencyShutdown(scope, groupName) {
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
      toast(`emergency shutdown: no running agents in ${where}`);
      return;
    }
    const n = running === 1 ? '1 running agent' : `${running} running agents`;
    const confirmed = await confirmModal({
      title: 'Emergency shutdown?',
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
      r = await fetch('/api/emergency-shutdown', {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(payload),
      });
    } catch (e) {
      toast(`emergency shutdown failed: ${e && e.message || e}`, true);
      return;
    }
    if (!r.ok) {
      toast(`emergency shutdown failed: ${await r.text()}`, true);
      return;
    }
    const out = await r.json().catch(() => null);
    if (!out) {
      toast('emergency shutdown: done');
      refresh();
      return;
    }
    const parts = [`${out.exited_gracefully} exited gracefully`, `${out.force_killed} force-killed`];
    if (out.already_offline) parts.push(`${out.already_offline} already offline`);
    if (out.failed) parts.push(`${out.failed} failed`);
    toast(`emergency shutdown (${out.targeted} targeted): ${parts.join(', ')}`, out.failed > 0);
    refresh();
  }

  // openWindowModal drives the bulk window focus/unfocus feature. One
  // trigger per scope — a group-level button and the top-bar button —
  // opens this modal. Inside it the human picks the DIRECTION (focus
  // vs unfocus) and the agent SELECTION: every running agent in scope
  // is listed and ticked by default, and can be narrowed by role chip,
  // by individual checkbox, or by the text filter. Submit POSTs the
  // explicit conv-id list to /api/agent-windows.
  //
  // It is window-only: focus opens/raises terminal windows, unfocus
  // detaches them. Neither touches an agent process — the agents keep
  // running. scope is "group" (groupName set) or "all".
  function openWindowModal(scope, groupName) {
    const snap = lastSnapshot || {};
    const where = scope === 'group' ? `group "${groupName}"` : 'the dashboard';
    const NO_ROLE = '(no role)';

    // An agent's roles come from its group memberships — a top-level
    // agent row carries no role of its own, so the all-scope modal
    // collects them across every group.
    const rolesByConv = {};
    for (const g of (snap.groups || [])) {
      for (const m of (g.members || [])) {
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
        candidates.push({ conv_id: m.conv_id, title: m.title || '',
          roles: m.role ? [m.role] : [], checked: true });
      }
    } else {
      for (const a of (snap.agents || [])) {
        if (!a.online) continue;
        candidates.push({ conv_id: a.conv_id, title: a.title || '',
          roles: rolesByConv[a.conv_id] || [], checked: true });
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

    const overlay = $('#window-modal');
    const hintEl = $('#window-hint');
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
    function renderRoles() {
      // Chips only earn their space when there is more than one bucket.
      if (allRoleKeys.length < 2) { rolesEl.innerHTML = ''; return; }
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
    function render() { renderHint(); renderRoles(); renderList(); renderFooter(); }

    const findCandidate = (conv) => candidates.find(c => c.conv_id === conv);

    const onListChange = (e) => {
      const cb = e.target.closest('input[type=checkbox]');
      if (!cb) return;
      const c = findCandidate(cb.getAttribute('data-conv'));
      if (c) c.checked = cb.checked;
      renderRoles(); renderFooter();
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
  // copy as the old confirmModal-based prompt, plus an "also shut down
  // the running session" checkbox (checked by default). Resolves to
  // {shutdown: bool} on Retire, null on Cancel / outside-click /
  // Escape. Shared by the per-row retire button and the drag-onto-
  // Retired gesture so both ask the same question.
  function retireConfirm({label}) {
    return new Promise(resolve => {
      const overlay = $('#retire-modal');
      const okBtn = $('#retire-ok');
      const cancelBtn = $('#retire-cancel');
      const shutdownCb = $('#retire-shutdown');
      $('#retire-meta').textContent = label || '';
      $('#retire-meta').style.display = label ? 'block' : 'none';
      shutdownCb.checked = true; // default ON on every open
      const cleanup = (result) => {
        overlay.classList.remove('show');
        okBtn.removeEventListener('click', onOk);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onOk = () => cleanup({ shutdown: shutdownCb.checked });
      const onCancel = () => cleanup(null);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(null); };
      okBtn.addEventListener('click', onOk);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      okBtn.focus();
    });
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

  // editMemberModal pops the role/descr editor pre-filled with
  // current values. Resolves to the new {role, descr} object on
  // Save (only fields that actually changed are kept; unchanged fields
  // are sent as null so the daemon leaves them alone), or null on
  // Cancel / outside-click / Escape. Auto-refresh suspends while the
  // modal is open — refreshSuspended() sees its .modal-overlay.show.
  function editMemberModal({label, role, descr}) {
    return new Promise(resolve => {
      const overlay = $('#edit-member-modal');
      $('#edit-member-meta').textContent = label || '';
      $('#edit-member-meta').style.display = label ? 'block' : 'none';
      const roleEl = $('#edit-member-role');
      const descrEl = $('#edit-member-descr');
      roleEl.value = role || '';
      descrEl.value = descr || '';
      const saveBtn = $('#edit-member-save');
      const cancelBtn = $('#edit-member-cancel');
      const cleanup = (result) => {
        overlay.classList.remove('show');
        saveBtn.removeEventListener('click', onSave);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onSave = () => {
        // Only send fields that changed; unchanged fields go as null
        // so the daemon's PATCH leaves them untouched. Each field
        // either differs from the original (send the new value, even
        // if empty) or is unchanged (send null).
        const out = {};
        const newRole = roleEl.value;
        const newDescr = descrEl.value;
        if (newRole !== (role || '')) out.role = newRole;
        if (newDescr !== (descr || '')) out.descr = newDescr;
        cleanup(Object.keys(out).length === 0 ? 'noop' : out);
      };
      const onCancel = () => cleanup(null);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(null); };
      const onKey = (e) => {
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
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      roleEl.focus();
      roleEl.select();
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
      // for the 5s poll.
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
        const r = await fetch(`/api/groups/${encodeURIComponent(groupName)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv: cand.conv_id}),
        });
        if (!r.ok) {
          toast(`add failed: ${await r.text()}`, true);
          return;
        }
        const label = cand.title || cand.conv_id;
        toast(`added ${label} to ${groupName}`);
        // Optimistic local mutation: append to lastSnapshot's group so
        // the next render filters this row out without waiting for the
        // 5s poll. The poll will overwrite with the canonical state.
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

      const cleanup = (result) => {
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
        refresh();
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
  // outcome, and refreshes on success. Shared by the "wake" row button
  // and the offline status-dot click — both wake an agent the exact
  // same way. Returns true on success.
  async function resumeAgentReq(conv, label) {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/resume`, {
      method: 'POST', credentials: 'same-origin',
    });
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
  // outcome, and refreshes on success. Shared by the "shut down" row
  // button and the online status-dot click. Returns true on success.
  async function stopAgentReq(conv, label, force) {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/stop`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({force: !!force}),
    });
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
              lastSnapshot = prevSnapshot;
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
              lastSnapshot = prevSnapshot;
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
              lastSnapshot = prevSnapshot;
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

  // Drag-and-drop: move a member row from group A onto group B's
  // <summary> header to migrate. Optimistic local mutation runs first
  // so the user sees the move immediately; the daemon round-trip
  // confirms (or snaps back on failure).
  //
  // Order on success: POST /api/groups/B/members → DELETE
  // /api/groups/A/members/{conv}. POST first guarantees the conv is
  // never groupless mid-drag — on a failed delete it ends up in both
  // groups (visible, recoverable) instead of nowhere (silently lost).
  //
  // Auto-refresh suspends while a drag is in flight via the
  // dndDragActive flag below — refreshSuspended() checks it — so a 5s
  // tick doesn't blow our optimistic mutation away while the
  // round-trip is mid-air. The drag deliberately does NOT share the
  // modal suspension: a single shared boolean let a drag and a modal
  // clobber each other's reset, which is how auto-refresh used to
  // wedge after a drag-and-drop retire.
  let dndDragActive = false;
  // dndSourceUngrouped / dndSourceConversation / dndSourceRetired: which
  // virtual group the dragged row comes from. Set in dragstart, cleared
  // in dragend. dragover can't read the DataTransfer payload (browsers
  // gate getData to the drop event), so these module-level flags are
  // how the hover handlers tell an inert no-op (e.g. an ungrouped row
  // onto Ungrouped, or a retired row onto Retired) from a real op.
  let dndSourceUngrouped = false;
  let dndSourceConversation = false;
  let dndSourceRetired = false;
  // Every droppable summary — real group headers AND the two droppable
  // virtual group headers (Ungrouped, Retired). The DnD listeners share
  // this selector. The Conversations header is a drag SOURCE only.
  const DND_TARGET_SEL = 'summary[data-dnd-target-group],summary[data-dnd-target-ungrouped],summary[data-dnd-target-retired]';
  // updateDndPill positions + labels the hint pill that tracks the
  // cursor during a drag. `info` is null to hide the pill, else
  // {text, clone} — text is the action label, clone tints it green.
  function updateDndPill(e, info) {
    const pill = $('#dnd-pill');
    if (!info) {
      pill.classList.remove('show', 'clone');
      return;
    }
    pill.textContent = info.text;
    pill.classList.toggle('clone', !!info.clone);
    pill.classList.add('show');
    // Offset slightly from the cursor so the pill doesn't sit on top
    // of the user's pointer. clientX/clientY on `dragover` events
    // jitter on some browsers; the offset masks that.
    pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
  }
  function bindDnd() {
    document.addEventListener('dragstart', (e) => {
      const row = e.target.closest('.dnd-draggable');
      if (!row) return;
      const conv = row.getAttribute('data-dnd-conv');
      const sourceGroup = row.getAttribute('data-dnd-source-group');
      const sourceUngrouped = row.hasAttribute('data-dnd-source-ungrouped');
      const sourceConversation = row.hasAttribute('data-dnd-source-conversation');
      const sourceRetired = row.hasAttribute('data-dnd-source-retired');
      const label = row.getAttribute('data-dnd-label') || conv;
      // A draggable row is a real-group member (has a source group), a
      // virtual-Ungrouped row, a virtual-Conversations row, or a
      // virtual-Retired row. Anything else isn't a valid drag.
      if (!conv || (!sourceGroup && !sourceUngrouped && !sourceConversation && !sourceRetired)) return;
      // Stash the payload on the DataTransfer so the eventual drop can
      // read it without globals. The MIME type 'text/plain' is the
      // most-supported channel; the JSON body keeps the encoding
      // self-describing. We allow both move (default) and copy effects
      // so Ctrl-drag can flip the cursor hint via dropEffect.
      const payload = JSON.stringify({conv, sourceGroup: sourceGroup || '', sourceUngrouped, sourceConversation, sourceRetired, label});
      e.dataTransfer.setData('application/x-tclaude-member', payload);
      e.dataTransfer.setData('text/plain', payload);
      e.dataTransfer.effectAllowed = 'copyMove';
      row.classList.add('dnd-source-row');
      dndDragActive = true;
      dndSourceUngrouped = sourceUngrouped;
      dndSourceConversation = sourceConversation;
      dndSourceRetired = sourceRetired;
      // dndDragActive (set above) is what suspends auto-refresh for the
      // duration of the drag — see refreshSuspended().
    });
    document.addEventListener('dragend', (e) => {
      // Clear the drag state FIRST, ahead of any DOM cleanup below: if
      // a classList / query call here ever threw, auto-refresh must
      // still come back. dragend fires for every drag that had a
      // dragstart — a successful drop, an Escape-cancel, or a release
      // over nothing — so this is the one guaranteed reset covering
      // every drag-end outcome (join, leave, retire, reinstate,
      // promote, clone, cancelled drop, error path).
      dndDragActive = false;
      dndSourceUngrouped = false;
      dndSourceConversation = false;
      dndSourceRetired = false;
      const row = e.target.closest('.dnd-draggable');
      if (row) row.classList.remove('dnd-source-row');
      // Clear any lingering hover highlight (Firefox sometimes fires
      // dragend without a final dragleave on the target).
      $$('summary.dnd-drop-over').forEach(s => s.classList.remove('dnd-drop-over', 'dnd-effect-clone'));
      $('#dnd-pill').classList.remove('show', 'clone');
      refresh();
    });
    document.addEventListener('dragover', (e) => {
      if (!dndDragActive) return;
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) {
        updateDndPill(e, null);
        return;
      }
      const targetUngrouped = summary.hasAttribute('data-dnd-target-ungrouped');
      const targetRetired = summary.hasAttribute('data-dnd-target-retired');
      // No-op drops — don't preventDefault (so `drop` never fires) and
      // don't show a hint:
      //   - a row onto the virtual group it already lives in;
      //   - a plain conversation onto Retired (only agents can retire).
      if ((targetUngrouped && dndSourceUngrouped) ||
          (targetRetired && (dndSourceRetired || dndSourceConversation))) {
        updateDndPill(e, null);
        return;
      }
      e.preventDefault(); // required for drop to fire on this element
      // Clone is meaningful only for a real-group target, and never for
      // a retired source (that path reinstates, it doesn't clone).
      const isClone = (!!e.ctrlKey || !!e.metaKey) && !targetUngrouped && !targetRetired && !dndSourceRetired;
      e.dataTransfer.dropEffect = isClone ? 'copy' : 'move';
      summary.classList.toggle('dnd-effect-clone', isClone);
      let text;
      if (targetRetired) text = '↓ retire — demote to conversation';
      else if (targetUngrouped) text = dndSourceRetired ? '↓ reinstate (no group)' : dndSourceConversation ? '↓ promote (no group)' : '↓ remove from group';
      else if (isClone) text = '→ clone into group';
      else if (dndSourceRetired) text = '→ reinstate + join group';
      else if (dndSourceConversation) text = '→ promote into group';
      else if (dndSourceUngrouped) text = '→ add to group';
      else text = '→ move to group';
      updateDndPill(e, {text, clone: isClone});
    });
    document.addEventListener('dragenter', (e) => {
      if (!dndDragActive) return;
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) return;
      // No highlight for the inert no-ops — mirror the dragover guard.
      if ((summary.hasAttribute('data-dnd-target-ungrouped') && dndSourceUngrouped) ||
          (summary.hasAttribute('data-dnd-target-retired') && (dndSourceRetired || dndSourceConversation))) return;
      summary.classList.add('dnd-drop-over');
    });
    document.addEventListener('dragleave', (e) => {
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) return;
      // dragleave fires when the cursor enters a child element too;
      // only remove the highlight when the cursor has actually left
      // the summary.
      if (summary.contains(e.relatedTarget)) return;
      summary.classList.remove('dnd-drop-over', 'dnd-effect-clone');
    });
    document.addEventListener('drop', async (e) => {
      const summary = e.target.closest(DND_TARGET_SEL);
      if (!summary) return;
      e.preventDefault();
      summary.classList.remove('dnd-drop-over', 'dnd-effect-clone');
      $('#dnd-pill').classList.remove('show', 'clone');
      const raw = e.dataTransfer.getData('application/x-tclaude-member')
        || e.dataTransfer.getData('text/plain');
      let payload;
      try { payload = JSON.parse(raw); } catch (_) { return; }
      if (!payload || !payload.conv) return;
      const targetUngrouped = summary.hasAttribute('data-dnd-target-ungrouped');
      const targetRetired = summary.hasAttribute('data-dnd-target-retired');
      const targetGroup = summary.getAttribute('data-dnd-target-group');
      const sourceUngrouped = !!payload.sourceUngrouped;
      const sourceConversation = !!payload.sourceConversation;
      const sourceRetired = !!payload.sourceRetired;
      // Clone applies only to a real-group target, never to a retired
      // source (that path reinstates).
      const isClone = (!!e.ctrlKey || !!e.metaKey) && !targetUngrouped && !targetRetired && !sourceRetired;

      // Confirmation gate. Each runDnd* function below opens its own
      // tailored confirmation modal as its first step, BEFORE any
      // daemon call or optimistic snapshot mutation. The no-op short-
      // circuits above have already returned, so a modal is only ever
      // shown for a gesture that would really change something — an
      // inert drop never reaches a runDnd* function and never prompts.
      // On Cancel / Escape / outside-click the runDnd* function calls
      // refresh() (the modal suspended auto-refresh while it was open)
      // and returns without touching the daemon or lastSnapshot.
      // runDndRetire uses the richer retireConfirm modal — shutdown
      // checkbox and all — so a retire-by-drag and the per-row retire
      // button ask the identical question.

      // Target = the virtual Retired group → retire the agent,
      // demoting it back to a plain conversation.
      if (targetRetired) {
        if (sourceRetired || sourceConversation) return; // no-op (see dragover)
        await runDndRetire(payload);
        return;
      }
      // Target = the virtual Ungrouped group.
      if (targetUngrouped) {
        if (sourceUngrouped) return; // already ungrouped — no-op
        if (sourceRetired) {
          // A retired agent dropped here → reinstate to an active
          // agent, joining no group.
          await runDndReinstate(payload, null);
          return;
        }
        if (sourceConversation) {
          // A conversation dropped here → promote to agent, no group.
          await runDndPromoteToUngrouped(payload);
          return;
        }
        // A real-group member → remove from that group.
        await runDndRemoveFromGroup(payload);
        return;
      }
      // Target = a real group.
      if (sourceRetired) {
        // A retired agent dragged onto a group → reinstate + join.
        await runDndReinstate(payload, targetGroup);
        return;
      }
      if (isClone) {
        // Clone forks a sibling into the target group. Works whether
        // the source is grouped or ungrouped — runDndClone clones the
        // conv then POSTs the clone into the drop-target group.
        await runDndClone(payload, targetGroup);
        return;
      }
      if (sourceUngrouped || sourceConversation) {
        // An ungrouped agent OR a conversation dragged onto a group →
        // pure add. The membership write promotes a conversation.
        await runDndAddToGroup(payload, targetGroup);
        return;
      }
      // Real group → real group move. Move-onto-self is a no-op.
      if (payload.sourceGroup === targetGroup) return;
      await runDndMove(payload, targetGroup);
    });
  }

  // runDndClone forks the source conv via POST /api/agents/{conv}/clone,
  // then adds the new conv to the target group with POST
  // /api/groups/{target}/members. The clone inherits all source
  // memberships (including the source group) — the target-group POST
  // is the differentiator: it ensures the clone is in the dropped-on
  // group even when source wasn't already there.
  //
  // No optimistic UI: the new conv-id isn't known until the response
  // lands, and inventing a placeholder row would confuse the user
  // when the real conv-id replaces it on the next poll. Just await
  // both calls and refresh.
  async function runDndClone(payload, targetGroup) {
    const {conv, label} = payload;
    const confirmed = await confirmModal({
      title: 'Clone agent into group?',
      body: `Fork a new sibling agent from "${label}" and add the clone to `
        + `group "${targetGroup}". The original keeps running; the clone is a `
        + `sibling conversation that inherits the original's identity and a `
        + `copy of its conversation history.`,
      meta: label,
      okLabel: 'Clone',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const cloneRes = await fetch(`/api/agents/${encodeURIComponent(conv)}/clone`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({}),
      });
      if (!cloneRes.ok) {
        toast(`clone failed: ${await cloneRes.text()}`, true);
        return;
      }
      const out = await cloneRes.json();
      const newConv = out.new_conv;
      if (!newConv) {
        toast(`clone: response missing new_conv`, true);
        return;
      }
      // Add the new conv to the drop target group. Idempotent if the
      // clone already inherited that group from the source's
      // memberships.
      const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({conv: newConv}),
      });
      if (!addRes.ok) {
        toast(`clone add-to-${targetGroup} failed: ${await addRes.text()}`, true);
        return;
      }
      toast(`cloned ${label} → ${targetGroup} (new ${newConv.slice(0,8)})`);
    } catch (err) {
      toast(`clone failed: ${err && err.message || err}`, true);
    } finally {
      // The confirm modal suspended auto-refresh while it was open, and
      // the dragend-fired refresh() bailed for the same reason — so the
      // dashboard has not re-rendered since before the drag. Sync now.
      await refresh();
    }
  }

  // runDndMove performs the optimistic local mutation, then the
  // POST B → DELETE A sequence. Failure of either step rolls back
  // the local mutation and surfaces a toast.
  async function runDndMove(payload, targetGroup) {
    const {conv, sourceGroup, label} = payload;
    // Confirm BEFORE the lastSnapshot read + optimistic splice below,
    // so a cancelled move leaves the snapshot — and the render —
    // completely untouched.
    const confirmed = await confirmModal({
      title: 'Move agent to another group?',
      body: `Move "${label}" out of group "${sourceGroup}" and into group `
        + `"${targetGroup}". Its membership of "${sourceGroup}" is removed and a `
        + `membership of "${targetGroup}" is added.`,
      meta: label,
      okLabel: 'Move',
    });
    if (!confirmed) { await refresh(); return; }
    // Every post-confirm exit — a guard-clause return, the partial-
    // failure return, success, or an error — funnels through the
    // finally so the dashboard re-syncs. The dragend-fired refresh()
    // bailed while the confirm modal was open (refreshSuspended() saw
    // it), so without this a confirmed-then-aborted move would leave
    // the dashboard showing stale state until the next 5s tick.
    try {
      if (!lastSnapshot || !Array.isArray(lastSnapshot.groups)) {
        toast(`move: dashboard snapshot not loaded`, true);
        return;
      }
      // Snapshot the source row so we can restore it on rollback +
      // append it to the target so the optimistic render is correct.
      const source = lastSnapshot.groups.find(g => g.name === sourceGroup);
      const target = lastSnapshot.groups.find(g => g.name === targetGroup);
      if (!source || !target) {
        toast(`move: group not found in snapshot`, true);
        return;
      }
      const idx = (source.members || []).findIndex(m => m.conv_id === conv);
      if (idx < 0) {
        toast(`move: member not found in source group`, true);
        return;
      }
      const memberSnapshot = source.members[idx];
      // Optimistic mutation: pull from source, push onto target.
      source.members.splice(idx, 1);
      target.members = target.members || [];
      target.members.push(memberSnapshot);
      renderGroupsTab();

      const rollback = () => {
        // Re-insert at the original position so the visible ordering
        // doesn't drift mid-failure.
        source.members.splice(idx, 0, memberSnapshot);
        target.members = (target.members || []).filter(m => m.conv_id !== conv);
        renderGroupsTab();
      };

      try {
        const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv}),
        });
        if (!addRes.ok) {
          toast(`move add failed: ${await addRes.text()}`, true);
          rollback();
          return;
        }
        const delRes = await fetch(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(conv)}`, {
          method: 'DELETE', credentials: 'same-origin',
        });
        if (!delRes.ok) {
          // Add succeeded but remove failed: the conv is now in BOTH
          // groups. Report it so the human can manually clean up; do
          // NOT roll the optimistic mutation back, because the daemon
          // really did add it to the target.
          toast(`move partial: added to ${targetGroup} but failed to remove from ${sourceGroup}: ${await delRes.text()}`, true);
          return;
        }
        toast(`moved ${label}: ${sourceGroup} → ${targetGroup}`);
      } catch (err) {
        toast(`move failed: ${err && err.message || err}`, true);
        rollback();
      }
    } finally {
      await refresh();
    }
  }

  // runDndAddToGroup handles a drag FROM the virtual Ungrouped group
  // ONTO a real group's header — the agent joins that group. Pure add:
  // POST /api/groups/{B}/members. The agent was in no group, so there
  // is nothing to remove; on success it drops out of the Ungrouped
  // virtual group on the next snapshot.
  //
  // Non-optimistic (one round-trip, then refresh): the source isn't a
  // real group in lastSnapshot.groups, so the optimistic splice
  // runDndMove relies on doesn't apply. A single fast call + refresh
  // keeps the code simple and the failure mode obvious.
  async function runDndAddToGroup(payload, targetGroup) {
    const {conv, label} = payload;
    // The source is either an ungrouped agent or a plain conversation;
    // for a conversation the membership write also promotes it to an
    // agent, so the modal says so.
    const isConv = !!payload.sourceConversation;
    const confirmed = await confirmModal({
      title: isConv ? 'Promote conversation into group?' : 'Add agent to group?',
      body: isConv
        ? `Promote the conversation "${label}" to an agent and add it to group `
          + `"${targetGroup}".`
        : `Add the agent "${label}" to group "${targetGroup}". It keeps every `
          + `other group it already belongs to.`,
      meta: label,
      okLabel: isConv ? 'Promote' : 'Add',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
        method: 'POST', credentials: 'same-origin',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({conv}),
      });
      if (!r.ok) {
        toast(`add to ${targetGroup} failed: ${await r.text()}`, true);
        return;
      }
      toast(`added ${label} → ${targetGroup}`);
    } catch (err) {
      toast(`add failed: ${err && err.message || err}`, true);
    } finally {
      // The dragend handler's refresh() can race ahead of this
      // round-trip; refresh again once it has landed so the final
      // render reflects the mutation.
      await refresh();
    }
  }

  // runDndRemoveFromGroup handles a drag FROM a real group's member
  // row ONTO the virtual Ungrouped group — the agent leaves that
  // group. Pure remove: DELETE /api/groups/{A}/members/{conv}. If A
  // was the agent's only group it reappears in the Ungrouped virtual
  // group on the next snapshot; if it was in other groups too it
  // simply stays in those. Non-optimistic, same rationale as
  // runDndAddToGroup.
  async function runDndRemoveFromGroup(payload) {
    const {conv, sourceGroup, label} = payload;
    if (!sourceGroup) return; // not a real-group member — nothing to do
    const confirmed = await confirmModal({
      title: 'Remove agent from group?',
      body: `Remove "${label}" from group "${sourceGroup}". If this is its only `
        + `group it becomes an ungrouped agent; otherwise it stays in its other `
        + `groups.`,
      meta: label,
      okLabel: 'Remove',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/groups/${encodeURIComponent(sourceGroup)}/members/${encodeURIComponent(conv)}`, {
        method: 'DELETE', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`remove from ${sourceGroup} failed: ${await r.text()}`, true);
        return;
      }
      toast(`removed ${label} from ${sourceGroup}`);
    } catch (err) {
      toast(`remove failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // runDndRetire handles a drag of an AGENT row (a real-group member or
  // a virtual-Ungrouped row) ONTO the virtual Retired group — the agent
  // is retired, demoting it back to a plain conversation. Retire
  // revokes group memberships + grants, so it gets the same
  // retireConfirm modal — checkbox and all — as the per-row retire
  // button.
  async function runDndRetire(payload) {
    const {conv, label} = payload;
    const choice = await retireConfirm({ label });
    if (!choice) {
      await refresh(); // undo the optimistic dragend state
      return;
    }
    try {
      const q = choice.shutdown ? '?shutdown=1' : '?shutdown=0';
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/retire${q}`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`retire ${label} failed: ${await r.text()}`, true);
        return;
      }
      toast(choice.shutdown
        ? `retired ${label} — demoted + session stopped`
        : `retired ${label} — demoted to a conversation`);
    } catch (err) {
      toast(`retire failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // runDndPromoteToUngrouped handles a drag of a CONVERSATION row ONTO
  // the virtual Ungrouped group — the conversation is promoted to an
  // agent but joins no group, so it lands directly in the Ungrouped
  // virtual group on the next snapshot.
  async function runDndPromoteToUngrouped(payload) {
    const {conv, label} = payload;
    const confirmed = await confirmModal({
      title: 'Promote conversation to an agent?',
      body: `Promote the conversation "${label}" to an agent. It joins no group `
        + `and appears under Ungrouped.`,
      meta: label,
      okLabel: 'Promote',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/promote`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`promote ${label} failed: ${await r.text()}`, true);
        return;
      }
      toast(`promoted ${label} → agent (no group)`);
    } catch (err) {
      toast(`promote failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // runDndReinstate handles a drag of a RETIRED agent row OUT of the
  // virtual Retired group. The agent is reinstated — its retired flag
  // is cleared, making it an active agent again. Retire stripped the
  // agent's old group memberships and grants and reinstate does not
  // restore them, so the agent starts fresh: when targetGroup is given
  // (dropped onto a real group) it is then added to that group; when
  // null (dropped onto Ungrouped) it joins no group and lands in the
  // Ungrouped virtual group on the next snapshot.
  async function runDndReinstate(payload, targetGroup) {
    const {conv, label} = payload;
    const confirmed = await confirmModal({
      title: 'Reinstate retired agent?',
      body: targetGroup
        ? `Reinstate the retired agent "${label}" and add it to group `
          + `"${targetGroup}". Group memberships and permission grants stripped `
          + `when it was retired are NOT restored — it starts fresh.`
        : `Reinstate the retired agent "${label}" as an active, ungrouped agent. `
          + `Group memberships and permission grants stripped when it was retired `
          + `are NOT restored — it starts fresh.`,
      meta: label,
      okLabel: 'Reinstate',
    });
    if (!confirmed) { await refresh(); return; }
    try {
      const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reinstate`, {
        method: 'POST', credentials: 'same-origin',
      });
      if (!r.ok) {
        toast(`reinstate ${label} failed: ${await r.text()}`, true);
        return;
      }
      if (targetGroup) {
        const addRes = await fetch(`/api/groups/${encodeURIComponent(targetGroup)}/members`, {
          method: 'POST', credentials: 'same-origin',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({conv}),
        });
        if (!addRes.ok) {
          toast(`reinstated ${label}, but join ${targetGroup} failed: ${await addRes.text()}`, true);
          return;
        }
        toast(`reinstated ${label} → ${targetGroup}`);
      } else {
        toast(`reinstated ${label} → agent (no group)`);
      }
    } catch (err) {
      toast(`reinstate failed: ${err && err.message || err}`, true);
    } finally {
      await refresh();
    }
  }

  // ===================================================================
  // Config tab — visual editor for ~/.tclaude/config.json
  //
  // The form binds to a deep clone of the loaded config, so a Config
  // field with no dedicated widget still round-trips. A JSON key absent
  // from tclaude's config schema is dropped by the server's typed
  // decode (pre-existing config.Load behaviour) — the server reports
  // such keys and the tab shows a warning on load. Save is two-phase: a
  // dry-run POST validates server-side and returns the canonical
  // "after" JSON, diffed against the on-disk baseline and shown in a
  // confirmation modal before the real write. The POST carries that
  // baseline so the server can 409 if the file drifted underneath.
  // ===================================================================
  let configObj = null;     // last loaded full config object (clone source)
  let configBaseRaw = '';   // canonical JSON of the config currently on disk
  let configLoaded = false;
  let configFileMalformed = false; // on-disk file is corrupt → form shows defaults

  function cfgInt(id, fallback) {
    const v = parseInt($('#' + id).value, 10);
    return Number.isFinite(v) ? v : fallback;
  }
  function cfgFloat(id, fallback) {
    const v = parseFloat($('#' + id).value);
    return Number.isFinite(v) ? v : fallback;
  }

  // cfgStringRow / cfgTransitionRow build one removable row of a list
  // editor. renderCfg*List (re)populates a container with rows + an
  // "+ add" button; readCfg*List collects the non-blank values back.
  function cfgStringRow(value, datalistId, placeholder) {
    const row = document.createElement('div');
    row.className = 'cfg-list-row';
    const inp = document.createElement('input');
    inp.type = 'text';
    inp.value = value || '';
    inp.placeholder = placeholder || '';
    if (placeholder) inp.setAttribute('aria-label', placeholder);
    inp.autocomplete = 'off';
    inp.spellcheck = false;
    if (datalistId) inp.setAttribute('list', datalistId);
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'cfg-row-del';
    del.textContent = '×';
    del.title = 'Remove';
    del.addEventListener('click', () => row.remove());
    row.appendChild(inp);
    row.appendChild(del);
    return row;
  }
  function cfgTransitionRow(from, to) {
    const row = document.createElement('div');
    row.className = 'cfg-list-row';
    const mk = (val, ph, role) => {
      const i = document.createElement('input');
      i.type = 'text';
      i.value = val || '';
      i.placeholder = ph;
      i.setAttribute('aria-label', ph);
      i.autocomplete = 'off';
      i.spellcheck = false;
      i.dataset.role = role;
      i.setAttribute('list', 'cfg-state-list');
      return i;
    };
    const arrow = document.createElement('span');
    arrow.className = 'cfg-arrow';
    arrow.textContent = '→';
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'cfg-row-del';
    del.textContent = '×';
    del.title = 'Remove';
    del.addEventListener('click', () => row.remove());
    row.appendChild(mk(from, 'from state', 'from'));
    row.appendChild(arrow);
    row.appendChild(mk(to, 'to state', 'to'));
    row.appendChild(del);
    return row;
  }
  function renderCfgStringList(containerId, values, datalistId, placeholder) {
    const c = $('#' + containerId);
    c.innerHTML = '';
    (values || []).forEach(v => c.appendChild(cfgStringRow(v, datalistId, placeholder)));
    const add = document.createElement('button');
    add.type = 'button';
    add.className = 'cfg-list-add';
    add.textContent = '+ add';
    add.addEventListener('click', () => {
      const row = cfgStringRow('', datalistId, placeholder);
      c.insertBefore(row, add);
      row.querySelector('input').focus();
    });
    c.appendChild(add);
  }
  function renderCfgTransitionList(values) {
    const c = $('#cfg-notif-transitions');
    c.innerHTML = '';
    (values || []).forEach(t => c.appendChild(cfgTransitionRow(t.from, t.to)));
    const add = document.createElement('button');
    add.type = 'button';
    add.className = 'cfg-list-add';
    add.textContent = '+ add transition';
    add.addEventListener('click', () => {
      const row = cfgTransitionRow('', '');
      c.insertBefore(row, add);
      row.querySelector('input').focus();
    });
    c.appendChild(add);
  }
  function readCfgStringList(containerId) {
    return $$('#' + containerId + ' .cfg-list-row input')
      .map(i => i.value.trim()).filter(Boolean);
  }
  function readCfgTransitionList() {
    const out = [];
    $$('#cfg-notif-transitions .cfg-list-row').forEach(row => {
      const from = row.querySelector('input[data-role=from]').value.trim();
      const to = row.querySelector('input[data-role=to]').value.trim();
      // A row with either side filled is kept (a half-filled row then
      // surfaces as a server validation error rather than silently
      // vanishing); a fully blank row is dropped.
      if (from || to) out.push({ from, to });
    });
    return out;
  }

  // syncCfgEnables greys out the companion inputs of any unchecked
  // enable toggle, so the form reads the way it behaves.
  function syncCfgEnables() {
    $('#cfg-autocompact-pct').disabled = !$('#cfg-autocompact-enabled').checked;
    const rl = $('#cfg-ratelimit-enabled').checked;
    $('#cfg-ratelimit-5h').disabled = !rl;
    $('#cfg-ratelimit-7d').disabled = !rl;
    $('#cfg-agent-spawnmax').disabled = !$('#cfg-agent-spawnmax-enabled').checked;
    const nudge = $('#cfg-nudge-enabled').checked;
    $('#cfg-nudge-min').disabled = !nudge;
    $('#cfg-nudge-interval').disabled = !nudge;
  }

  function populateConfigForm(cfg) {
    cfg = cfg || {};
    $('#cfg-log-level').value = cfg.log_level || 'info';
    $('#cfg-terminal').value = cfg.terminal || '';
    const acp = cfg.auto_compact_percent;
    $('#cfg-autocompact-enabled').checked = acp != null;
    $('#cfg-autocompact-pct').value = acp != null ? acp : '';
    $('#cfg-record-hooks').checked = !!cfg.record_hooks;

    const n = cfg.notifications || {};
    $('#cfg-notif-enabled').checked = !!n.enabled;
    $('#cfg-notif-cooldown').value = n.cooldown_seconds != null ? n.cooldown_seconds : '';
    renderCfgTransitionList(n.transitions || []);
    renderCfgStringList('cfg-notif-command', n.notification_command || [], null, 'argument');

    const rl = cfg.ratelimit;
    $('#cfg-ratelimit-enabled').checked = !!rl;
    $('#cfg-ratelimit-5h').value = rl ? rl.five_hour_percent_max_used : '';
    $('#cfg-ratelimit-7d').value = rl ? rl.seven_day_percent_max_used : '';

    const a = cfg.agent || {};
    $('#cfg-agent-autolaunch').checked = !!a.auto_launch_dashboard;
    $('#cfg-agent-clonecooldown').value = a.clone_cooldown || '';
    // nil / true both mean "on" (the default); only an explicit false is off.
    $('#cfg-agent-spawnrestrict').checked = a.spawn_group_restriction !== false;
    const smph = a.spawn_max_per_hour;
    $('#cfg-agent-spawnmax-enabled').checked = smph != null;
    $('#cfg-agent-spawnmax').value = smph != null ? smph : '';
    const cn = a.context_nudge || {};
    $('#cfg-nudge-enabled').checked = !!cn.enabled;
    // != null (not ||) so a stored 0 shows as 0, not blank — a 0 ladder
    // value while the nudge is enabled is a config Validate flags.
    $('#cfg-nudge-min').value = cn.min_pct != null ? cn.min_pct : '';
    $('#cfg-nudge-interval').value = cn.interval_pct != null ? cn.interval_pct : '';
    renderCfgStringList('cfg-agent-permissions', a.default_permissions || [], 'cfg-slug-list', 'permission slug');
    renderCfgStringList('cfg-agent-allowedgroups', a.spawn_allowed_groups || [], 'cfg-group-list', 'group name');

    $('#cfg-sudo-json').value = a.sudo ? JSON.stringify(a.sudo, null, 2) : '';
    syncCfgEnables();
  }

  // assembleConfig builds the config object to submit. It starts from a
  // deep clone of the loaded config so Config fields with no dedicated
  // widget still round-trip, then the form widgets overwrite the paths
  // they own. Throws on unparseable advanced sudo JSON — the caller
  // surfaces that as a save error.
  function assembleConfig() {
    const cfg = JSON.parse(JSON.stringify(configObj || {}));

    cfg.log_level = $('#cfg-log-level').value;
    const term = $('#cfg-terminal').value.trim();
    if (term) cfg.terminal = term; else delete cfg.terminal;
    if ($('#cfg-autocompact-enabled').checked) cfg.auto_compact_percent = cfgInt('cfg-autocompact-pct', 80);
    else delete cfg.auto_compact_percent;
    cfg.record_hooks = $('#cfg-record-hooks').checked;

    const n = (cfg.notifications && typeof cfg.notifications === 'object') ? cfg.notifications : {};
    n.enabled = $('#cfg-notif-enabled').checked;
    n.cooldown_seconds = cfgInt('cfg-notif-cooldown', 5);
    const trans = readCfgTransitionList();
    if (trans.length) n.transitions = trans; else delete n.transitions;
    const cmd = readCfgStringList('cfg-notif-command');
    if (cmd.length) n.notification_command = cmd; else delete n.notification_command;
    cfg.notifications = n;

    if ($('#cfg-ratelimit-enabled').checked) {
      // Clone the existing block rather than build a fresh one, so a
      // future ratelimit sub-field with no widget still round-trips.
      const rl = (cfg.ratelimit && typeof cfg.ratelimit === 'object') ? cfg.ratelimit : {};
      rl.five_hour_percent_max_used = cfgFloat('cfg-ratelimit-5h', 99);
      rl.seven_day_percent_max_used = cfgFloat('cfg-ratelimit-7d', 99.9);
      cfg.ratelimit = rl;
    } else {
      // The whole section is switched off — the human chose to drop it.
      delete cfg.ratelimit;
    }

    const a = (cfg.agent && typeof cfg.agent === 'object') ? cfg.agent : {};
    // Set optional keys only when meaningful so an all-default agent
    // block stays genuinely empty (see the empty-agent drop below).
    if ($('#cfg-agent-autolaunch').checked) a.auto_launch_dashboard = true;
    else delete a.auto_launch_dashboard;
    const cc = $('#cfg-agent-clonecooldown').value.trim();
    if (cc) a.clone_cooldown = cc; else delete a.clone_cooldown;
    // Checked = "on" = also the default (nil): preserve an existing nil
    // or true rather than introducing a redundant explicit `true`.
    if ($('#cfg-agent-spawnrestrict').checked) {
      if (a.spawn_group_restriction === false) delete a.spawn_group_restriction;
    } else {
      a.spawn_group_restriction = false;
    }
    if ($('#cfg-agent-spawnmax-enabled').checked) a.spawn_max_per_hour = cfgInt('cfg-agent-spawnmax', 10);
    else delete a.spawn_max_per_hour;
    // Clone the existing context_nudge block so a future sub-field with
    // no widget round-trips, then set the ladder from the form.
    const cn = (a.context_nudge && typeof a.context_nudge === 'object') ? a.context_nudge : {};
    if ($('#cfg-nudge-enabled').checked) {
      cn.enabled = true;
      cn.min_pct = cfgInt('cfg-nudge-min', 30);
      cn.interval_pct = cfgInt('cfg-nudge-interval', 10);
      a.context_nudge = cn;
    } else {
      // Disabled: drop the enabled flag (false is the omitempty default)
      // but keep the ladder values the user entered so toggling off then
      // on round-trips. Drop the block only when nothing is left to keep.
      delete cn.enabled;
      const minRaw = $('#cfg-nudge-min').value.trim();
      const ivRaw = $('#cfg-nudge-interval').value.trim();
      if (minRaw) cn.min_pct = cfgInt('cfg-nudge-min', 0); else delete cn.min_pct;
      if (ivRaw) cn.interval_pct = cfgInt('cfg-nudge-interval', 0); else delete cn.interval_pct;
      if (Object.keys(cn).length) a.context_nudge = cn;
      else delete a.context_nudge;
    }
    const perms = readCfgStringList('cfg-agent-permissions');
    if (perms.length) a.default_permissions = perms; else delete a.default_permissions;
    const grps = readCfgStringList('cfg-agent-allowedgroups');
    if (grps.length) a.spawn_allowed_groups = grps; else delete a.spawn_allowed_groups;

    const sudoRaw = $('#cfg-sudo-json').value.trim();
    if (sudoRaw) {
      let parsed;
      try {
        parsed = JSON.parse(sudoRaw);
      } catch (e) {
        throw new Error('Advanced sudo JSON is not valid JSON: ' + (e.message || e));
      }
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        throw new Error('Advanced sudo JSON must be a JSON object (or left blank).');
      }
      a.sudo = parsed;
    } else {
      delete a.sudo;
    }
    // An all-default agent block marshals to "agent": {} on the server,
    // which would show as a spurious diff against a config that simply
    // had no agent key. Drop it when nothing is set.
    if (Object.keys(a).length) cfg.agent = a;
    else delete cfg.agent;
    return cfg;
  }

  function clearConfigErrors() {
    const el = $('#cfg-errors');
    el.style.display = 'none';
    el.innerHTML = '';
  }
  function showConfigErrors(errs) {
    const el = $('#cfg-errors');
    el.innerHTML = '<strong>Cannot save — fix these first:</strong><ul>' +
      errs.map(e => `<li>${esc(e)}</li>`).join('') + '</ul>';
    el.style.display = 'block';
    el.scrollIntoView({ block: 'nearest' });
  }

  // The notice box (amber) carries load-time facts about the file the
  // form cannot represent: a malformed file shown as defaults, or
  // keys the running tclaude does not model and a save would drop.
  function renderConfigNotice(messages) {
    const el = $('#cfg-notice');
    if (!messages.length) {
      el.style.display = 'none';
      el.innerHTML = '';
      return;
    }
    el.innerHTML = '<strong>Heads up:</strong><ul>' +
      messages.map(m => `<li>${esc(m)}</li>`).join('') + '</ul>';
    el.style.display = 'block';
  }

  async function loadConfigTab() {
    // Refresh the slug / group datalists from the latest snapshot.
    const snap = lastSnapshot || {};
    $('#cfg-slug-list').innerHTML = (snap.slugs || [])
      .map(s => `<option value="${esc(s.slug)}"></option>`).join('');
    $('#cfg-group-list').innerHTML = (snap.groups || [])
      .map(g => `<option value="${esc(g.name)}"></option>`).join('');
    $('#cfg-status').textContent = 'loading…';
    clearConfigErrors();
    renderConfigNotice([]);
    try {
      const r = await fetch('/api/config', { credentials: 'same-origin' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const data = await r.json();
      configBaseRaw = data.raw || '{}';
      configObj = JSON.parse(configBaseRaw);
      configFileMalformed = !!data.malformed;
      if (data.path) $('#cfg-path').textContent = data.path;
      populateConfigForm(configObj);
      configLoaded = true;
      const notices = [];
      if (data.warning) notices.push(data.warning);
      if (data.unknown_keys && data.unknown_keys.length) {
        notices.push('config.json also contains key(s) this version of tclaude does not ' +
          'model: ' + data.unknown_keys.join(', ') + '. They are not shown here, and ' +
          'saving from this tab will remove them.');
      }
      renderConfigNotice(notices);
      $('#cfg-status').textContent = notices.length
        ? 'loaded with a notice — see above'
        : 'loaded — edits stay in this form until you Save';
    } catch (e) {
      configLoaded = false;
      $('#cfg-status').textContent = 'failed to load';
      showConfigErrors(['Could not load config: ' + (e.message || e)]);
    }
  }

  // cfgLineDiff returns an LCS-based line diff of two strings. Config
  // JSON is tiny (tens of lines) so the O(n·m) table is trivial.
  function cfgLineDiff(aStr, bStr) {
    const a = aStr.split('\n'), b = bStr.split('\n');
    const n = a.length, m = b.length;
    const dp = [];
    for (let i = 0; i <= n; i++) dp.push(new Array(m + 1).fill(0));
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
    const out = [];
    let i = 0, j = 0;
    while (i < n && j < m) {
      if (a[i] === b[j]) { out.push({ t: 'ctx', s: a[i] }); i++; j++; }
      else if (dp[i + 1][j] >= dp[i][j + 1]) { out.push({ t: 'del', s: a[i] }); i++; }
      else { out.push({ t: 'add', s: b[j] }); j++; }
    }
    while (i < n) out.push({ t: 'del', s: a[i++] });
    while (j < m) out.push({ t: 'add', s: b[j++] });
    return out;
  }

  // configDiffModal renders the before/after diff and resolves true on
  // confirm, false on cancel / outside-click / Escape. When malformed
  // is set the on-disk file is corrupt: a red banner spells out that
  // the whole file is being replaced and the diff is only against
  // defaults, so the human cannot wipe a corrupt config unawares.
  function configDiffModal(beforeRaw, afterRaw, malformed) {
    return new Promise(resolve => {
      const overlay = $('#config-diff-modal');
      const diff = cfgLineDiff(beforeRaw, afterRaw);
      const adds = diff.filter(d => d.t === 'add').length;
      const dels = diff.filter(d => d.t === 'del').length;
      const warnEl = $('#config-diff-warn');
      if (malformed) {
        warnEl.textContent = '⚠ config.json on disk is corrupt and could not be parsed. ' +
          'The form shows DEFAULT values, not your previous settings. Saving replaces the ' +
          'corrupt file entirely — anything it contained is lost. The diff below is against defaults.';
        warnEl.style.display = 'block';
      } else {
        warnEl.style.display = 'none';
      }
      $('#config-diff-confirm').textContent = malformed
        ? 'Replace corrupt config.json' : 'Save to config.json';
      $('#config-diff-sub').textContent =
        `${adds} line(s) added, ${dels} removed — writing to ${$('#cfg-path').textContent}`;
      const sign = { add: '+', del: '-', ctx: ' ' };
      $('#config-diff-body').innerHTML = diff
        .map(d => `<span class="dl ${d.t}">${esc(sign[d.t] + ' ' + d.s)}</span>`).join('');
      const okBtn = $('#config-diff-confirm');
      const cancelBtn = $('#config-diff-cancel');
      const cleanup = (result) => {
        overlay.classList.remove('show');
        okBtn.removeEventListener('click', onOk);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onOverlay);
        document.removeEventListener('keydown', onKey);
        resolve(result);
      };
      const onOk = () => cleanup(true);
      const onCancel = () => cleanup(false);
      const onOverlay = (e) => { if (e.target === overlay) cleanup(false); };
      const onKey = (e) => { if (e.key === 'Escape') cleanup(false); };
      okBtn.addEventListener('click', onOk);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onOverlay);
      document.addEventListener('keydown', onKey);
      overlay.classList.add('show');
      okBtn.focus();
    });
  }

  // reportConfigHTTPError surfaces a non-OK /api/config response and
  // returns true when it handled one — the caller then aborts. 400 is
  // the structured validation contract; 409 is the drift guard (the
  // file changed under the editor).
  async function reportConfigHTTPError(resp) {
    if (resp.status === 400) {
      const d = await resp.json().catch(() => ({}));
      showConfigErrors(d.errors && d.errors.length ? d.errors : ['Config rejected by the server.']);
      return true;
    }
    if (resp.status === 409) {
      const d = await resp.json().catch(() => ({}));
      showConfigErrors([(d.error || 'config.json changed on disk') +
        ' — press Reload to pick up the current file, then re-apply your edits.']);
      return true;
    }
    if (!resp.ok) {
      showConfigErrors(['Server error: HTTP ' + resp.status]);
      return true;
    }
    return false;
  }

  async function saveConfig() {
    if (!configLoaded) { toast('Config not loaded yet', true); return; }
    clearConfigErrors();
    let edited;
    try {
      edited = assembleConfig();
    } catch (e) {
      showConfigErrors([e.message || String(e)]);
      return;
    }
    // The body carries the edited config plus the canonical baseline
    // the form loaded — the server 409s if the file drifted since.
    const body = JSON.stringify({ config: edited, base: configBaseRaw });
    const post = (query) => fetch('/api/config' + query, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' }, body,
    });
    const saveBtn = $('#cfg-save');
    saveBtn.disabled = true;
    try {
      // Phase 1: dry-run — server validates and returns the canonical
      // "after" without writing anything.
      const dry = await post('?dry_run=1');
      if (await reportConfigHTTPError(dry)) return;
      const after = (await dry.json()).raw || '';
      // When the on-disk file is corrupt the diff baseline is "defaults",
      // so after===base can hold even though the save still meaningfully
      // replaces the corrupt file — don't skip it then.
      if (after === configBaseRaw && !configFileMalformed) { toast('No changes to save'); return; }

      // Phase 2: human confirms the diff before the real write.
      const ok = await configDiffModal(configBaseRaw, after, configFileMalformed);
      if (!ok) { toast('Save cancelled'); return; }

      // replace_malformed acknowledges wiping a corrupt on-disk file.
      const res = await post(configFileMalformed ? '?replace_malformed=1' : '');
      if (await reportConfigHTTPError(res)) return;
      const data = await res.json();
      configBaseRaw = data.raw || configBaseRaw;
      configObj = JSON.parse(configBaseRaw);
      configFileMalformed = false; // the file is canonical after a save
      populateConfigForm(configObj);
      // The saved file is canonical now — any load-time notice (a
      // malformed file, or unknown keys that this save dropped) is
      // stale, so clear it.
      renderConfigNotice([]);
      $('#cfg-status').textContent = 'saved · ' + new Date().toLocaleTimeString();
      toast('Config saved to ' + $('#cfg-path').textContent);
    } catch (e) {
      showConfigErrors(['Save failed: ' + (e.message || e)]);
    } finally {
      saveBtn.disabled = false;
    }
  }

  function bindConfigTab() {
    // Lazy-load on the first activation of the Config tab.
    const navBtn = $('nav button[data-tab="config"]');
    if (navBtn) navBtn.addEventListener('click', () => {
      if (!configLoaded) loadConfigTab();
    });
    $('#cfg-reload').addEventListener('click', loadConfigTab);
    $('#cfg-save').addEventListener('click', saveConfig);
    ['cfg-autocompact-enabled', 'cfg-ratelimit-enabled',
      'cfg-agent-spawnmax-enabled', 'cfg-nudge-enabled'].forEach(id => {
      $('#' + id).addEventListener('change', syncCfgEnables);
    });
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
  bindRenameAgentModal();
  bindConfigTab();
  refresh();
  setInterval(refresh, 5000);
