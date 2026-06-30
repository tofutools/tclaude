import { $, $$, esc } from './helpers.js';
import { toast, isCyclingTabs } from './refresh.js';
import { lastSnapshot } from './dashboard.js';
import { loadProfiles } from './profiles.js';
import { bindRemoteAdmin, loadRemoteAdmin } from './remote-admin.js';

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

// populateAskSelects fills the Ask-defaults Model / Effort dropdowns from
// the snapshot's harness catalog (the same source the spawn modal uses),
// so the lists track the server-side catalog with no hardcoded model list.
// Each select gets a leading "Built-in default" option (empty value = unpinned,
// resolves to the built-in default server-side). The claude harness is
// the only one wired for `tclaude ask` today; a hand-set value absent from
// the catalog is added back by setAskSelectValue so it still round-trips.
function populateAskSelects() {
  const harnesses = (lastSnapshot && lastSnapshot.harnesses) || [];
  const claude = harnesses.find(h => h.name === 'claude') || {};
  fillAskSelect($('#ask-model'), claude.models || []);
  fillAskSelect($('#ask-effort'), claude.effort_levels || []);
}
function fillAskSelect(sel, values) {
  if (!sel) return;
  sel.innerHTML = '<option value="">Built-in default</option>' +
    (values || []).map(v => `<option value="${esc(v)}">${esc(v)}</option>`).join('');
}
// populateAskProfileSelect fills the Ask-defaults Profile dropdown from the
// saved spawn profiles (the Groups-tab profiles), selecting `selected`. A
// chosen profile supplies the harness/model/effort a FRESH `tclaude ask` runs
// at (JOH-252) — the harness-independent way to ask Codex as well as Claude —
// so it overrides the Model/Effort selects, which applyAskProfileState then
// greys out. The fetch is async + best-effort: an endpoint error leaves just
// the "(none)" option. A hand-set profile that's since been deleted is kept as
// a "(missing)" option so the form shows what's on disk, not a silent reset.
async function populateAskProfileSelect(selected) {
  const sel = $('#ask-profile');
  if (!sel) return;
  const selectedName = (selected || '').trim();

  // Seed the selection SYNCHRONOUSLY before the async list load, so a Save that
  // races ahead of loadProfiles() still reads the real profile name off
  // #ask-profile rather than an empty value (which assembleConfig would persist
  // as "delete ask.profile", silently clearing a saved profile).
  sel.innerHTML = '<option value="">(none — use Model/Effort below)</option>';
  if (selectedName) {
    const pending = document.createElement('option');
    pending.value = selectedName;
    pending.textContent = `${selectedName} (loading…)`;
    sel.appendChild(pending);
  }
  sel.value = selectedName;
  applyAskProfileState();

  let profiles = [];
  try { profiles = await loadProfiles(); } catch { profiles = []; }
  sel.innerHTML = '<option value="">(none — use Model/Effort below)</option>' +
    profiles.map(p => `<option value="${esc(p.name)}">${esc(p.name)}</option>`).join('');
  if (selectedName && !profiles.some(p => p.name === selectedName)) {
    const o = document.createElement('option');
    o.value = selectedName;
    o.textContent = `${selectedName} (missing)`;
    sel.appendChild(o);
  }
  sel.value = selectedName;
  applyAskProfileState();
}

// applyAskProfileState greys out the Model / Effort selects while a profile is
// chosen — the profile supplies those, so the selects are inert (their stored
// values are kept, for when the profile is cleared) — and shows a one-line
// note. Called on load and on every Profile change.
function applyAskProfileState() {
  const sel = $('#ask-profile');
  const active = !!(sel && sel.value);
  const model = $('#ask-model'), effort = $('#ask-effort');
  if (model) model.disabled = active;
  if (effort) effort.disabled = active;
  const note = $('#ask-profile-state');
  if (note) {
    note.textContent = active
      ? `Profile “${sel.value}” supplies the harness/model/effort — the Model/Effort below are ignored.`
      : '';
  }
}

// setAskSelectValue selects value, first adding it as an option when the
// catalog doesn't list it (a hand-edited full model ID), so the form shows
// what is actually on disk rather than silently snapping to another option.
function setAskSelectValue(sel, value) {
  if (!sel) return;
  value = value || '';
  if (value && !Array.from(sel.options).some(o => o.value === value)) {
    const o = document.createElement('option');
    o.value = value;
    o.textContent = value;
    sel.appendChild(o);
  }
  sel.value = value;
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

// The "Notify me when an agent…" checklist is a friendly view over the
// raw transition rules: each canonical destination state maps to one
// wildcard rule {from:"*", to:<state>}. The raw "Advanced" list
// (#cfg-notif-transitions) stays the single source of truth read by
// assembleConfig — the checklist just reflects it and mutates it in
// place, so the two never disagree and from-specific / non-canonical
// rules round-trip untouched.
function syncCfgNotifyTypes() {
  const rules = readCfgTransitionList();
  $$('#cfg-notif-types [data-cfg-notify-type]').forEach(cb => {
    const to = cb.getAttribute('data-cfg-notify-type');
    cb.checked = rules.some(r => r.from === '*' && r.to === to);
  });
}
function setCfgNotifyType(to, on) {
  // Drop every wildcard rule for this destination, re-add one iff checked,
  // and leave all other rules (from-specific or non-canonical) intact.
  const rules = readCfgTransitionList().filter(r => !(r.from === '*' && r.to === to));
  if (on) rules.push({ from: '*', to });
  renderCfgTransitionList(rules);
  syncCfgNotifyTypes();
}

// cfgThresholdRow / renderCfgThresholdList / readCfgThresholdList edit
// the pre_compact_guard floor ladder: one (window_size → min_tokens)
// pair per row.
function cfgThresholdRow(windowSize, minTokens) {
  const row = document.createElement('div');
  row.className = 'cfg-list-row';
  const mk = (val, ph, role) => {
    const i = document.createElement('input');
    i.type = 'number';
    i.min = '1';
    i.value = (val != null && val !== '') ? val : '';
    i.placeholder = ph;
    i.setAttribute('aria-label', ph);
    i.autocomplete = 'off';
    i.dataset.role = role;
    i.style.minWidth = '170px';
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
  row.appendChild(mk(windowSize, 'window size (tokens)', 'window'));
  row.appendChild(arrow);
  row.appendChild(mk(minTokens, 'min used tokens before compaction', 'min'));
  row.appendChild(del);
  return row;
}
function renderCfgThresholdList(values) {
  const c = $('#cfg-precompact-thresholds');
  c.innerHTML = '';
  (values || []).forEach(t => c.appendChild(cfgThresholdRow(t.window_size, t.min_tokens)));
  const add = document.createElement('button');
  add.type = 'button';
  add.className = 'cfg-list-add';
  add.textContent = '+ add floor';
  add.addEventListener('click', () => {
    const row = cfgThresholdRow('', '');
    c.insertBefore(row, add);
    row.querySelector('input').focus();
  });
  c.appendChild(add);
}
function readCfgThresholdList() {
  const out = [];
  $$('#cfg-precompact-thresholds .cfg-list-row').forEach(row => {
    const w = row.querySelector('input[data-role=window]').value.trim();
    const m = row.querySelector('input[data-role=min]').value.trim();
    // A row with either side filled is kept so a half-filled row
    // surfaces as a server validation error instead of silently
    // vanishing; a fully blank row is dropped.
    if (w || m) out.push({ window_size: parseInt(w, 10) || 0, min_tokens: parseInt(m, 10) || 0 });
  });
  return out;
}

// syncCfgEnables greys out the companion inputs of any unchecked
// enable toggle, so the form reads the way it behaves.
function syncCfgEnables() {
  $('#cfg-precompact-blockmanual').disabled = !$('#cfg-precompact-enabled').checked;
  const rl = $('#cfg-ratelimit-enabled').checked;
  $('#cfg-ratelimit-5h').disabled = !rl;
  $('#cfg-ratelimit-7d').disabled = !rl;
  $('#cfg-agent-spawnmax').disabled = !$('#cfg-agent-spawnmax-enabled').checked;
  $('#cfg-agent-retired-cleanup-days').disabled = !$('#cfg-agent-retired-cleanup-enabled').checked;
  const nudge = $('#cfg-nudge-enabled').checked;
  $('#cfg-nudge-min').disabled = !nudge;
  $('#cfg-nudge-interval').disabled = !nudge;
}

// REMOTE_DEFAULT_PORT is the conventional tclaude HTTPS port — what
// `tclaude remote-access setup --bind 0.0.0.0:8443` and the docs use. The
// Config tab falls back to it when the listener is enabled without an explicit
// port, so a one-click enable yields a complete bind.
const REMOTE_DEFAULT_PORT = '8443';

// splitBind / joinBind decompose the single config.json `remote_access.bind`
// (a host:port string) into the two form fields and back. The Config tab edits
// interface + port separately for clarity, but the stored key stays one bind
// string so there is no second source of truth. Split on the LAST colon so an
// IPv6 host ("[::]:8443") survives; an empty join is "" (block dropped).
function splitBind(bind) {
  bind = (bind || '').trim();
  if (!bind) return { host: '', port: '' };
  const i = bind.lastIndexOf(':');
  if (i < 0) return { host: bind, port: '' };
  return { host: bind.slice(0, i), port: bind.slice(i + 1) };
}
function joinBind(host, port) {
  host = (host || '').trim();
  port = (port || '').trim();
  if (!host && !port) return '';
  // An empty host defaults to 0.0.0.0 (all interfaces); an empty port leaves a
  // trailing ":" so the server's Validate flags "set a port" rather than the
  // listener silently grabbing a random one.
  return (host || '0.0.0.0') + ':' + port;
}

// syncCfgRemoteStatus renders the live remote-access status line from the latest
// snapshot (material generated? listener running?) combined with the unsaved
// form state. It is the foot-gun guard: enabling without running
// `tclaude remote-access setup` first is a silent no-op, so the UI says so.
function syncCfgRemoteStatus() {
  const el = $('#cfg-remote-status');
  if (!el) return;
  const ra = (lastSnapshot && lastSnapshot.remote_access) || {};
  const enabled = $('#cfg-remote-enabled').checked;
  // Mirror assembleConfig's effective port so the "live on …" comparison and
  // the saved bind agree (a blank port while enabled resolves to 8443).
  let port = $('#cfg-remote-port').value.trim();
  if (enabled && !port) port = REMOTE_DEFAULT_PORT;
  const bind = joinBind($('#cfg-remote-host').value, port);
  el.classList.remove('ok', 'warn');

  if (enabled && !ra.material_exists) {
    el.classList.add('warn');
    el.textContent = '⚠ No certificates yet — run `tclaude remote-access setup` on the host first. ' +
      'Enabling without it does nothing.';
    return;
  }
  if (!enabled) {
    el.textContent = ra.running
      ? 'Listener is currently running — save (disabled) and restart agentd to take it down.'
      : 'Off — the dashboard stays loopback-only.';
    return;
  }
  // Enabled, with material present.
  if (ra.running && ra.running_bind === bind) {
    el.classList.add('ok');
    el.textContent = '✓ Listener live on https://' + bind + ' (mTLS + passphrase).';
    return;
  }
  el.classList.add('warn');
  el.textContent = ra.running
    ? 'Listener running on ' + ra.running_bind + ' — restart agentd to apply the new address.'
    : 'Ready — restart agentd to start the listener on ' + (bind || '(set an interface + port)') + '.';
}

function populateConfigForm(cfg) {
  cfg = cfg || {};
  $('#cfg-log-level').value = cfg.log_level || 'info';
  $('#cfg-terminal').value = cfg.terminal || '';
  const pcg = cfg.pre_compact_guard || {};
  $('#cfg-precompact-enabled').checked = !!pcg.enabled;
  $('#cfg-precompact-blockmanual').checked = !!pcg.block_manual;
  renderCfgThresholdList(pcg.thresholds || []);

  // Resume-from-summary prompt thresholds — blank when unset (Claude Code's
  // own default); a stored value (incl. 0 = always show) renders as-is so the
  // human sees exactly what's on disk, including the large suppress sentinel.
  const cr = cfg.claude_resume || {};
  $('#cfg-resume-minutes').value = cr.threshold_minutes != null ? cr.threshold_minutes : '';
  $('#cfg-resume-tokens').value = cr.token_threshold != null ? cr.token_threshold : '';

  $('#cfg-record-hooks').checked = !!cfg.record_hooks;
  $('#cfg-focus-raiseonly').checked = !!(cfg.focus && cfg.focus.raise_only);

  // Cost display multiplier — blank when unset (no adjustment); a stored
  // value shows as-is so the human sees exactly what's on disk.
  const cf = cfg.cost && cfg.cost.estimate_factor;
  $('#cfg-cost-factor').value = (cf != null && cf !== '') ? cf : '';
  // Show the Costs tab + per-agent cost on a subscription (WHAT-IF mode).
  // Default off (auto-hide on subscription).
  $('#cfg-cost-show-on-subscription').checked = !!(cfg.cost && cfg.cost.show_on_subscription);

  // Vegas music in regular mode — surface the Vegas tab / volume mixer /
  // radio outside slop mode. Default off; lives in the slop block.
  $('#cfg-slop-vegas-regular').checked = !!(cfg.slop && cfg.slop.vegas_in_regular_mode);

  // Hide the slop-mode side pull-lever. Default off (the lever shows);
  // lives in the slop block.
  $('#cfg-slop-hide-lever').checked = !!(cfg.slop && cfg.slop.hide_pull_lever);

  // Activity bots — per-mode style of the deduped robot indicator.
  // Defaults: regular emoji, slop sprites (mirrors the Go resolvers).
  const ab = (cfg.dashboard && cfg.dashboard.activity_bots) || {};
  $('#cfg-dashboard-activity-bots-regular').value = ab.regular || 'emoji';
  $('#cfg-dashboard-activity-bots-slop').value = ab.slop || 'sprites';

  // Always show the Plugins tab even with no plugins installed. Default off
  // (the tab auto-hides when empty); lives in the dashboard block.
  $('#cfg-dashboard-always-show-plugins').checked = !!(cfg.dashboard && cfg.dashboard.always_show_plugins_tab);

  // Horizontal-scroll chrome-bar mode. Default follow (checked); only an
  // explicit dashboard.hscroll_follow:false (static) unchecks it.
  $('#cfg-dashboard-hscroll-follow').checked = !(cfg.dashboard && cfg.dashboard.hscroll_follow === false);

  // Group quick-options auto-fold. Default hover (checked, the chips fold to
  // icons + expand on hover); only an explicit
  // dashboard.group_quick_options:"expanded" unchecks it.
  $('#cfg-dashboard-group-quick-fold').checked = !(cfg.dashboard && cfg.dashboard.group_quick_options === 'expanded');

  // Ask defaults — profile + model/effort for `tclaude ask`. Options come
  // from the harness catalog / saved spawn profiles; an unset field shows
  // "Built-in default" (empty). populateAskProfileSelect is async (it fetches
  // the profile list) and applies the Model/Effort disabled state when it
  // resolves.
  populateAskSelects();
  const ask = cfg.ask || {};
  setAskSelectValue($('#ask-model'), ask.model);
  setAskSelectValue($('#ask-effort'), ask.effort);
  void populateAskProfileSelect(ask.profile);

  const lr = cfg.log_rotation || {};
  $('#cfg-logrot-maxsize').value = lr.max_size || '';
  // keep: 0 and absent both mean "built-in default", so show a stored 0
  // as blank — the form drops it on save anyway. A negative keep is
  // shown as-is so the human sees the value the server rejects.
  $('#cfg-logrot-keep').value = (lr.keep != null && lr.keep !== 0) ? lr.keep : '';

  const n = cfg.notifications || {};
  $('#cfg-notif-enabled').checked = !!n.enabled;
  $('#cfg-notif-cooldown').value = n.cooldown_seconds != null ? n.cooldown_seconds : '';
  renderCfgTransitionList(n.transitions || []);
  syncCfgNotifyTypes();
  // human_messages defaults ON within an enabled block — only an explicit
  // false unchecks it.
  $('#cfg-notif-human').checked = n.human_messages !== false;
  renderCfgStringList('cfg-notif-command', n.notification_command || [], null, 'argument');

  const rl = cfg.ratelimit;
  $('#cfg-ratelimit-enabled').checked = !!rl;
  $('#cfg-ratelimit-5h').value = rl ? rl.five_hour_percent_max_used : '';
  $('#cfg-ratelimit-7d').value = rl ? rl.seven_day_percent_max_used : '';

  const a = cfg.agent || {};
  $('#cfg-agent-autolaunch').checked = !!a.auto_launch_dashboard;
  $('#cfg-agent-notray').checked = !!a.disable_tray;
  $('#cfg-agent-persisttoken').checked = !!a.persist_operator_token;
  // 0 and absent both mean "random free port", so show a stored 0 as
  // blank — the form drops it on save anyway. A negative / out-of-range
  // value is shown as-is so the human sees the value the server rejects.
  $('#cfg-agent-dashboardport').value = (a.dashboard_port != null && a.dashboard_port !== 0) ? a.dashboard_port : '';
  $('#cfg-agent-clonecooldown').value = a.clone_cooldown || '';
  // nil / true both mean "on" (the default); only an explicit false is off.
  $('#cfg-agent-spawnrestrict').checked = a.spawn_group_restriction !== false;
  // Same default-on *bool shape: nil / true = normalize names, only an
  // explicit false rejects invalid names.
  $('#cfg-agent-spawnnormalize').checked = a.spawn_name_normalize !== false;
  const smph = a.spawn_max_per_hour;
  $('#cfg-agent-spawnmax-enabled').checked = smph != null;
  $('#cfg-agent-spawnmax').value = smph != null ? smph : '';
  const cn = a.context_nudge || {};
  $('#cfg-nudge-enabled').checked = !!cn.enabled;
  // != null (not ||) so a stored 0 shows as 0, not blank — a 0 ladder
  // value while the nudge is enabled is a config Validate flags.
  $('#cfg-nudge-min').value = cn.min_pct != null ? cn.min_pct : '';
  $('#cfg-nudge-interval').value = cn.interval_pct != null ? cn.interval_pct : '';
  // Retired-agent cleanup (opt-in). Default off; a stored after_days
  // shows as-is (blank when unset → the built-in ~1-year default).
  const rc = a.retired_cleanup || {};
  $('#cfg-agent-retired-cleanup-enabled').checked = !!rc.enabled;
  $('#cfg-agent-retired-cleanup-days').value = (rc.after_days != null && rc.after_days !== 0) ? rc.after_days : '';

  renderCfgStringList('cfg-agent-permissions', a.default_permissions || [], 'cfg-slug-list', 'permission slug');
  renderCfgStringList('cfg-agent-allowedgroups', a.spawn_allowed_groups || [], 'cfg-group-list', 'group name');

  $('#cfg-sudo-json').value = a.sudo ? JSON.stringify(a.sudo, null, 2) : '';

  // Remote access — the single `bind` string splits into the interface + port
  // fields; status is rendered live from the snapshot (material/running state).
  const ra = cfg.remote_access || {};
  $('#cfg-remote-enabled').checked = !!ra.enabled;
  const rb = splitBind(ra.bind);
  $('#cfg-remote-host').value = rb.host;
  $('#cfg-remote-port').value = rb.port;
  syncCfgRemoteStatus();

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

  // pre_compact_guard. Clone the existing block so a future sub-field
  // with no widget round-trips. Keep block_manual / floors the user
  // entered even while disabled, so toggling off→on round-trips; drop
  // the whole block when nothing is left to keep.
  const pcg = (cfg.pre_compact_guard && typeof cfg.pre_compact_guard === 'object') ? cfg.pre_compact_guard : {};
  if ($('#cfg-precompact-enabled').checked) pcg.enabled = true; else delete pcg.enabled;
  if ($('#cfg-precompact-blockmanual').checked) pcg.block_manual = true; else delete pcg.block_manual;
  const pcgFloors = readCfgThresholdList();
  if (pcgFloors.length) pcg.thresholds = pcgFloors; else delete pcg.thresholds;
  if (Object.keys(pcg).length) cfg.pre_compact_guard = pcg; else delete cfg.pre_compact_guard;

  // claude_resume is an optional block — the CLAUDE_CODE_RESUME_* overrides
  // tclaude injects into spawned Claude Code panes. Clone the existing block so
  // a future sub-field with no widget round-trips. A blank field clears that
  // override (CC keeps its own default); a value — including 0 (always show) or
  // the large suppress sentinel — is written. A negative is written too, so the
  // server's Validate surfaces the error rather than the value silently
  // vanishing. Drop the whole block when nothing is left, so an all-default
  // config doesn't marshal a spurious "claude_resume": {} diff.
  const cr = (cfg.claude_resume && typeof cfg.claude_resume === 'object') ? cfg.claude_resume : {};
  const crMinRaw = $('#cfg-resume-minutes').value.trim();
  if (crMinRaw !== '') cr.threshold_minutes = cfgInt('cfg-resume-minutes', 0); else delete cr.threshold_minutes;
  const crTokRaw = $('#cfg-resume-tokens').value.trim();
  if (crTokRaw !== '') cr.token_threshold = cfgInt('cfg-resume-tokens', 0); else delete cr.token_threshold;
  if (Object.keys(cr).length) cfg.claude_resume = cr; else delete cfg.claude_resume;

  cfg.record_hooks = $('#cfg-record-hooks').checked;

  // cost is an optional block. Clone the existing one so a future
  // sub-field with no widget round-trips, then set the one form-owned
  // key. 1 (and blank) is the no-op default — drop the field, and the
  // block when it's all that's left, so an all-default cost block doesn't
  // marshal as a spurious "cost": {} diff. A 0/negative is written so the
  // server's Validate surfaces the out-of-range error.
  const cost = (cfg.cost && typeof cfg.cost === 'object') ? cfg.cost : {};
  const cfRaw = $('#cfg-cost-factor').value.trim();
  const cf = cfgFloat('cfg-cost-factor', 1);
  if (cfRaw !== '' && cf !== 1) cost.estimate_factor = cf; else delete cost.estimate_factor;
  // show_on_subscription: false is the default — drop it (matches the Go
  // `omitempty`) so an all-default cost block doesn't marshal a spurious key.
  if ($('#cfg-cost-show-on-subscription').checked) cost.show_on_subscription = true; else delete cost.show_on_subscription;
  if (Object.keys(cost).length) cfg.cost = cost; else delete cfg.cost;

  // slop is an optional block — its volumes/channel (owned by the header
  // mixer + picker, no widget on this page) ride along in the clone. Set
  // only this page's keys (vegas_in_regular_mode, hide_pull_lever): true
  // when checked, dropped otherwise (false is the omitempty default). Drop
  // the whole block only when nothing is left so an all-default slop doesn't
  // marshal as a spurious "slop": {} diff — but a block that still holds a
  // volume or channel survives.
  const slop = (cfg.slop && typeof cfg.slop === 'object') ? cfg.slop : {};
  if ($('#cfg-slop-vegas-regular').checked) slop.vegas_in_regular_mode = true; else delete slop.vegas_in_regular_mode;
  // hide_pull_lever: true when checked, dropped otherwise (false is the
  // omitempty default) so an all-default slop block doesn't marshal a
  // spurious key.
  if ($('#cfg-slop-hide-lever').checked) slop.hide_pull_lever = true; else delete slop.hide_pull_lever;
  if (Object.keys(slop).length) cfg.slop = slop; else delete cfg.slop;

  // dashboard is an optional block. activity_bots stores only the NON-
  // default per-mode styles (regular default emoji, slop default sprites),
  // dropping a default key — and the activity_bots / dashboard objects when
  // empty — so an all-default config marshals no spurious "dashboard": {}.
  // Mirrors the Go omitempty + per-mode-default resolvers.
  const dashboard = (cfg.dashboard && typeof cfg.dashboard === 'object') ? cfg.dashboard : {};
  const ab = (dashboard.activity_bots && typeof dashboard.activity_bots === 'object') ? dashboard.activity_bots : {};
  const abReg = $('#cfg-dashboard-activity-bots-regular').value;
  const abSlop = $('#cfg-dashboard-activity-bots-slop').value;
  if (abReg && abReg !== 'emoji') ab.regular = abReg; else delete ab.regular;
  if (abSlop && abSlop !== 'sprites') ab.slop = abSlop; else delete ab.slop;
  if (Object.keys(ab).length) dashboard.activity_bots = ab; else delete dashboard.activity_bots;
  // always_show_plugins_tab: true when checked, dropped otherwise (false is
  // the omitempty default) so an all-default dashboard block doesn't marshal a
  // spurious key.
  if ($('#cfg-dashboard-always-show-plugins').checked) dashboard.always_show_plugins_tab = true; else delete dashboard.always_show_plugins_tab;
  // hscroll_follow: follow is the default, so store only the NON-default
  // static (false) and drop the key when following — mirrors the Go *bool
  // omitempty + default-true resolver.
  if (!$('#cfg-dashboard-hscroll-follow').checked) dashboard.hscroll_follow = false; else delete dashboard.hscroll_follow;
  // group_quick_options: hover (fold) is the default, so store only the
  // NON-default "expanded" and drop the key when folding — mirrors the Go
  // omitempty + default-hover resolver.
  if (!$('#cfg-dashboard-group-quick-fold').checked) dashboard.group_quick_options = 'expanded'; else delete dashboard.group_quick_options;
  if (Object.keys(dashboard).length) cfg.dashboard = dashboard; else delete cfg.dashboard;

  // ask is an optional block. Clone the existing one so a future sub-field
  // with no widget round-trips, then set the two form-owned keys. An empty
  // value (the "Built-in default" option) clears that field, and a block with
  // nothing left is dropped so an all-default ask doesn't marshal as a
  // spurious "ask": {} diff.
  const ask = (cfg.ask && typeof cfg.ask === 'object') ? cfg.ask : {};
  const askProfile = $('#ask-profile') ? $('#ask-profile').value.trim() : '';
  const askModel = $('#ask-model').value.trim();
  const askEffort = $('#ask-effort').value.trim();
  // The profile selects the harness/model/effort a fresh ask runs at; the
  // Model/Effort here are kept (the no-profile fallback) but ignored while a
  // profile is set — resolveAskTarget applies that precedence server-side.
  if (askProfile) ask.profile = askProfile; else delete ask.profile;
  if (askModel) ask.model = askModel; else delete ask.model;
  if (askEffort) ask.effort = askEffort; else delete ask.effort;
  if (Object.keys(ask).length) cfg.ask = ask; else delete cfg.ask;

  // focus is an optional block. Clone the existing one so a future
  // sub-field with no widget round-trips, set the one form-owned key, and
  // drop the block when raise_only is off and nothing else remains — an
  // empty {} would marshal as a spurious diff against a config that simply
  // had no focus key.
  const fc = (cfg.focus && typeof cfg.focus === 'object') ? cfg.focus : {};
  if ($('#cfg-focus-raiseonly').checked) fc.raise_only = true; else delete fc.raise_only;
  if (Object.keys(fc).length) cfg.focus = fc; else delete cfg.focus;

  // log_rotation is an optional block. Clone the existing one so a
  // future sub-field with no widget round-trips, then set the two
  // form-owned keys. A blank max_size with a 0/blank keep leaves the
  // block genuinely absent — an empty {} would marshal as a spurious
  // diff against a config that simply had no log_rotation key.
  const lr = (cfg.log_rotation && typeof cfg.log_rotation === 'object') ? cfg.log_rotation : {};
  const lrMax = $('#cfg-logrot-maxsize').value.trim();
  if (lrMax) lr.max_size = lrMax; else delete lr.max_size;
  const lrKeepRaw = $('#cfg-logrot-keep').value.trim();
  const lrKeep = cfgInt('cfg-logrot-keep', 0);
  // keep > 0 is a real override; 0/blank means the built-in default and
  // is left out. A negative keep is still written so the server's
  // Validate surfaces "must not be negative".
  if (lrKeepRaw !== '' && lrKeep !== 0) lr.keep = lrKeep; else delete lr.keep;
  if (Object.keys(lr).length) cfg.log_rotation = lr;
  else delete cfg.log_rotation;

  const n = (cfg.notifications && typeof cfg.notifications === 'object') ? cfg.notifications : {};
  n.enabled = $('#cfg-notif-enabled').checked;
  n.cooldown_seconds = cfgInt('cfg-notif-cooldown', 5);
  // The raw "Advanced" list is the single source of truth — the friendly
  // checklist mutates it in place. An empty list is written as an absent
  // key (no state change notifies); the server preserves that rather than
  // re-seeding the defaults (see config.Normalize).
  const trans = readCfgTransitionList();
  if (trans.length) n.transitions = trans; else delete n.transitions;
  // human_messages: default-on. Persist only an explicit false (an unset
  // key reads as on), keeping the saved config minimal.
  if ($('#cfg-notif-human').checked) delete n.human_messages;
  else n.human_messages = false;
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
  if ($('#cfg-agent-notray').checked) a.disable_tray = true;
  else delete a.disable_tray;
  if ($('#cfg-agent-persisttoken').checked) a.persist_operator_token = true;
  else delete a.persist_operator_token;
  // dashboard_port: 0 / blank means the built-in default (random port) —
  // drop the key. A non-zero value (incl. an out-of-range one) is written
  // so the server's Validate surfaces "out of range" rather than the value
  // silently vanishing.
  const dpRaw = $('#cfg-agent-dashboardport').value.trim();
  const dp = cfgInt('cfg-agent-dashboardport', 0);
  if (dpRaw !== '' && dp !== 0) a.dashboard_port = dp; else delete a.dashboard_port;
  const cc = $('#cfg-agent-clonecooldown').value.trim();
  if (cc) a.clone_cooldown = cc; else delete a.clone_cooldown;
  // Checked = "on" = also the default (nil): preserve an existing nil
  // or true rather than introducing a redundant explicit `true`.
  if ($('#cfg-agent-spawnrestrict').checked) {
    if (a.spawn_group_restriction === false) delete a.spawn_group_restriction;
  } else {
    a.spawn_group_restriction = false;
  }
  // Default-on: preserve an existing nil/true rather than writing a
  // redundant explicit `true`; only persist the explicit `false`.
  if ($('#cfg-agent-spawnnormalize').checked) {
    if (a.spawn_name_normalize === false) delete a.spawn_name_normalize;
  } else {
    a.spawn_name_normalize = false;
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

  // retired_cleanup is an optional opt-in block. Clone the existing one so
  // a future sub-field with no widget round-trips, then set the form-owned
  // keys. Mirrors the context_nudge shape: when ENABLED we always write a
  // real after_days, filling a blank field with the built-in default (365),
  // so the common "tick + accept the default + Save" path produces a config
  // the server's Validate accepts (it requires after_days ≥ 1 while enabled)
  // — an explicit 0 is still written so Validate can surface "must be ≥1".
  const rc = (a.retired_cleanup && typeof a.retired_cleanup === 'object') ? a.retired_cleanup : {};
  if ($('#cfg-agent-retired-cleanup-enabled').checked) {
    rc.enabled = true;
    rc.after_days = cfgInt('cfg-agent-retired-cleanup-days', 365);
    a.retired_cleanup = rc;
  } else {
    // Disabled: drop the enable flag (false is the omitempty default) but
    // keep the window the user entered so toggling off→on round-trips. A
    // blank window leaves the block genuinely absent; drop it when nothing
    // is left so an all-default config doesn't marshal a spurious
    // "retired_cleanup": {} diff.
    delete rc.enabled;
    const rcDaysRaw = $('#cfg-agent-retired-cleanup-days').value.trim();
    if (rcDaysRaw !== '') rc.after_days = cfgInt('cfg-agent-retired-cleanup-days', 0); else delete rc.after_days;
    if (Object.keys(rc).length) a.retired_cleanup = rc; else delete a.retired_cleanup;
  }

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

  // remote_access is an optional block. Clone the existing one so a future
  // sub-field with no widget round-trips, then set the two form-owned keys:
  // `enabled` (only when checked — false is the omitempty default) and the
  // `bind` reassembled from the interface + port fields. The bind is kept even
  // while disabled so toggling off→on round-trips; the whole block is dropped
  // only when nothing is left (no enable, no bind), so an all-default block
  // doesn't marshal as a spurious "remote_access": {} diff.
  const raCfg = (cfg.remote_access && typeof cfg.remote_access === 'object') ? cfg.remote_access : {};
  const raEnabled = $('#cfg-remote-enabled').checked;
  if (raEnabled) raCfg.enabled = true; else delete raCfg.enabled;
  // Enabling without an explicit port falls back to the conventional 8443 so
  // the bind is complete (host defaults to 0.0.0.0 inside joinBind).
  let raPort = $('#cfg-remote-port').value.trim();
  if (raEnabled && !raPort) raPort = REMOTE_DEFAULT_PORT;
  const raBind = joinBind($('#cfg-remote-host').value, raPort);
  if (raBind) raCfg.bind = raBind; else delete raCfg.bind;
  if (Object.keys(raCfg).length) cfg.remote_access = raCfg; else delete cfg.remote_access;

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

// ===================================================================
// Section filter — a search box that hides/shows whole config sections
// by a title-OR-content match. cfgFilterBlocks() resolves the searchable
// blocks LIVE from the DOM (every .cfg-section plus the standalone
// Advanced sudo <details>), so a section added to dashboard.html is
// filtered automatically — there is no hardcoded section list to keep in
// sync. Hiding a section never affects Save: assembleConfig reads fields
// by id regardless of visibility, so a filtered-out setting still
// round-trips.
// ===================================================================
function cfgFilterBlocks() {
  return $$('#tab-config .cfg-section, #tab-config > details.cfg-advanced');
}

// cfgSearchText is the haystack for one block: its visible text (the <h3>
// title, every label, hint and <option> label) plus input placeholders
// and <option>/<datalist> values — so a term like "ghostty" or "8443"
// that only appears as an attribute still matches. Lower-cased once per
// call; the section count is tiny so there's no need to cache.
function cfgSearchText(block) {
  const parts = [block.textContent || ''];
  block.querySelectorAll('input, textarea').forEach(el => {
    if (el.placeholder) parts.push(el.placeholder);
  });
  block.querySelectorAll('option').forEach(o => { if (o.value) parts.push(o.value); });
  return parts.join(' ').toLowerCase();
}

// applyConfigFilter shows a block when every space-separated term is found
// somewhere in its searchable text (AND semantics); a blank query shows
// all. It also updates the match count, the clear button and the no-match
// line. Safe to call before the form has loaded — it only toggles
// visibility on whatever sections are in the DOM.
function applyConfigFilter() {
  const input = $('#cfg-filter');
  if (!input) return;
  const raw = input.value.trim().toLowerCase();
  const terms = raw ? raw.split(/\s+/) : [];
  const blocks = cfgFilterBlocks();
  let shown = 0;
  blocks.forEach(b => {
    const match = terms.length === 0 || (() => {
      const hay = cfgSearchText(b);
      return terms.every(t => hay.includes(t));
    })();
    b.hidden = !match;
    if (match) shown++;
  });
  const clearBtn = $('#cfg-filter-clear');
  if (clearBtn) clearBtn.hidden = terms.length === 0;
  const countEl = $('#cfg-filter-count');
  if (countEl) countEl.textContent = terms.length ? `${shown} / ${blocks.length} sections` : '';
  const emptyEl = $('#cfg-filter-empty');
  if (emptyEl) {
    emptyEl.hidden = !(terms.length && shown === 0);
    const q = $('#cfg-filter-empty-q');
    if (q) q.textContent = input.value.trim();
  }
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
  // Re-apply any standing filter — a Reload rebuilds the inner lists, and
  // the operator may have a query typed; keep what's hidden hidden.
  applyConfigFilter();
  // Localhost-only cert management renders independently of the config form's
  // load result (it has its own endpoint + error handling).
  void loadRemoteAdmin();
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

// focusConfigSearch puts the cursor in the section filter and selects any
// existing text, so a deliberate switch to the Config tab lands ready to
// type a filter (and overtype a stale one). The #cfg-filter input lives in
// static HTML, so this works even before loadConfigTab's fetch resolves;
// bindTabs (wired earlier in boot) has already made #tab-config visible by
// the time this fires, so the input is focusable.
function focusConfigSearch() {
  const filterInput = $('#cfg-filter');
  if (!filterInput) return;
  filterInput.focus();
  filterInput.select();
}

function bindConfigTab() {
  // Lazy-load on the first activation of the Config tab.
  const navBtn = $('nav button[data-tab="config"]');
  if (navBtn) navBtn.addEventListener('click', () => {
    if (!configLoaded) loadConfigTab();
    // Focus the search on a deliberate switch — a mouse click, the command
    // palette's "Go to Config", or a "Config ↗" deep link — but NOT during
    // keyboard tab-cycling ([ / ] and ←/→). Focusing the <input> there
    // would trap those very hotkeys (isEditableTarget) and strand the user
    // on Config; see isCyclingTabs in refresh.js.
    if (!isCyclingTabs()) focusConfigSearch();
  });
  $('#cfg-reload').addEventListener('click', loadConfigTab);
  $('#cfg-save').addEventListener('click', saveConfig);
  // Live section filter. Esc clears it; the clear button mirrors that.
  const filterInput = $('#cfg-filter');
  if (filterInput) {
    filterInput.addEventListener('input', applyConfigFilter);
    filterInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && filterInput.value) {
        e.preventDefault();
        filterInput.value = '';
        applyConfigFilter();
      }
    });
  }
  const filterClear = $('#cfg-filter-clear');
  if (filterClear) filterClear.addEventListener('click', () => {
    if (filterInput) { filterInput.value = ''; filterInput.focus(); }
    applyConfigFilter();
  });
  // Localhost-only remote-access cert management lives in the same tab; wire its
  // buttons once here (its data loads via loadConfigTab → loadRemoteAdmin).
  bindRemoteAdmin();
  ['cfg-precompact-enabled', 'cfg-ratelimit-enabled',
    'cfg-agent-spawnmax-enabled', 'cfg-nudge-enabled',
    'cfg-agent-retired-cleanup-enabled'].forEach(id => {
    $('#' + id).addEventListener('change', syncCfgEnables);
  });
  // The friendly per-type checklist edits the raw "Advanced" transition
  // list in place (one wildcard rule per checked type).
  $$('#cfg-notif-types [data-cfg-notify-type]').forEach(cb => {
    cb.addEventListener('change', () => setCfgNotifyType(cb.getAttribute('data-cfg-notify-type'), cb.checked));
  });
  // Keep the checklist honest when the Advanced raw editor is edited
  // directly: typing in a from/to input (input) or removing a row (the ×
  // delete fires a click that bubbles here; defer so the row is gone
  // first) re-derives which type boxes are lit. The container element
  // survives renderCfgTransitionList's innerHTML rebuilds, so this one
  // listener outlives the rows.
  const transList = $('#cfg-notif-transitions');
  if (transList) {
    transList.addEventListener('input', syncCfgNotifyTypes);
    transList.addEventListener('click', () => setTimeout(syncCfgNotifyTypes, 0));
  }
  // Toggle the Model/Effort selects live as the Ask profile changes.
  const askProf = $('#ask-profile');
  if (askProf) askProf.addEventListener('change', applyAskProfileState);

  // Re-render the remote-access status line as the interface / port change, so
  // the "run setup first" / "restart to apply" guidance stays honest.
  ['cfg-remote-host', 'cfg-remote-port'].forEach(id => {
    const elx = $('#' + id);
    if (elx) {
      elx.addEventListener('change', syncCfgRemoteStatus);
      elx.addEventListener('input', syncCfgRemoteStatus);
    }
  });
  // Enabling with an empty port pre-fills the conventional 8443 so the operator
  // sees the bind it will use, then refreshes the status line.
  const remoteEn = $('#cfg-remote-enabled');
  if (remoteEn) remoteEn.addEventListener('change', () => {
    const portEl = $('#cfg-remote-port');
    if (remoteEn.checked && portEl && !portEl.value.trim()) portEl.value = REMOTE_DEFAULT_PORT;
    syncCfgRemoteStatus();
  });
}

export { bindConfigTab };
