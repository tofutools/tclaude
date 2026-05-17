import { $, $$, esc } from './helpers.js';
import { fmtRemaining } from './tabs.js';
import {
  bindFilter, bindTabs, bindCopy, bindDetailsPersistence, bindSortHeaders,
  refresh, toast,
} from './refresh.js';
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
  bindReincarnateAgentModal, bindRenameAgentModal,
} from './modal-spawn.js';


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
