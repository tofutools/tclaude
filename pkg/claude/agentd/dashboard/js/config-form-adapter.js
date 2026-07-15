import { $, $$ } from './helpers.js';
import { lineDiff as configLineDiff } from './line-diff.js';
import { dashboardState } from './snapshot-store.js';
import { loadProfiles, findProfileByHandle, profileChoices } from './profiles.js';

let configLifecycle = {};
const fallbackLists = new Map();
let configDependencies = {
  toast: () => {},
  isCyclingTabs: () => false,
  fetchImpl: (...args) => globalThis.fetch(...args),
  confirmDiff: async () => false,
  lists: {
    get: id => fallbackLists.get(id) || [],
    set: (id, values) => fallbackLists.set(id, values),
  },
};
export function configureConfigLifecycle(callbacks = {}) { configLifecycle = callbacks; }
export function configureConfigAdapter(dependencies = {}) { configDependencies = { ...configDependencies, ...dependencies }; }
const latestSnapshot = () => dashboardState.snapshot.value || {};

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
let configBindingEpoch = 0;

function cfgInt(id, fallback) {
  const v = parseInt($('#' + id).value, 10);
  return Number.isFinite(v) ? v : fallback;
}
function cfgFloat(id, fallback) {
  const v = parseFloat($('#' + id).value);
  return Number.isFinite(v) ? v : fallback;
}
function setSelectValue(select, value) {
  if (!select) return;
  const wanted = String(value ?? '');
  const options = Array.from(select.querySelectorAll('option'));
  options.forEach(option => {
    option.removeAttribute('selected');
    option.selected = false;
  });
  const selected = options.find(option => (option.getAttribute('value') ?? option.value ?? '') === wanted);
  if (selected) {
    selected.setAttribute('selected', '');
    selected.selected = true;
  }
}
function controlValue(control) {
  if (!control) return '';
  if (control.tagName === 'SELECT') {
    const options = Array.from(control.querySelectorAll('option'));
    const selected = options.find(option => option.selected || option.hasAttribute('selected'));
    return selected?.getAttribute('value') ?? selected?.value
      ?? options[0]?.getAttribute('value') ?? options[0]?.value ?? '';
  }
  return control.value;
}
function makeOption(value, label = value) {
  const option = document.createElement('option');
  option.value = value;
  option.textContent = label;
  return option;
}
function replaceOptions(select, entries) {
  select?.replaceChildren(...entries.map(([value, label]) => makeOption(value, label)));
}

// populateAskSelects fills the Ask-defaults Model / Effort dropdowns from
// the snapshot's harness catalog (the same source the spawn modal uses),
// so the lists track the server-side catalog with no hardcoded model list.
// Each select gets a leading "Built-in default" option (empty value = unpinned,
// resolves to the built-in default server-side). The claude harness is
// the only one wired for `tclaude ask` today; a hand-set value absent from
// the catalog is added back by setAskSelectValue so it still round-trips.
function populateAskSelects() {
  const harnesses = latestSnapshot().harnesses || [];
  const claude = harnesses.find(h => h.name === 'claude') || {};
  fillAskSelect($('#ask-model'), claude.models || []);
  fillAskSelect($('#ask-effort'), claude.effort_levels || []);
}
function fillAskSelect(sel, values) {
  if (!sel) return;
  replaceOptions(sel, [['', 'Built-in default'], ...(values || []).map(value => [value, value])]);
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
  const bindingEpoch = configBindingEpoch;
  const sel = $('#ask-profile');
  if (!sel) return;
  const selectedName = (selected || '').trim();

  // Seed the selection SYNCHRONOUSLY before the async list load, so a Save that
  // races ahead of loadProfiles() still reads the real profile name off
  // #ask-profile rather than an empty value (which assembleConfig would persist
  // as "delete ask.profile", silently clearing a saved profile).
  replaceOptions(sel, [['', '(none — use Model/Effort below)']]);
  if (selectedName) {
    const pending = document.createElement('option');
    pending.value = selectedName;
    pending.textContent = `${selectedName} (loading…)`;
    sel.appendChild(pending);
  }
  setSelectValue(sel, selectedName);
  applyAskProfileState();

  let profiles = [];
  try { profiles = await loadProfiles(); } catch { profiles = []; }
  if (bindingEpoch !== configBindingEpoch || sel !== $('#ask-profile')) return;
  replaceOptions(sel, [['', '(none — use Model/Effort below)'], ...profileChoices(profiles).map(choice => [choice.value, choice.label])]);
  if (selectedName && !findProfileByHandle(profiles, selectedName)) {
    const o = document.createElement('option');
    o.value = selectedName;
    o.textContent = `${selectedName} (missing)`;
    sel.appendChild(o);
  }
  setSelectValue(sel, selectedName);
  applyAskProfileState();
}

// applyAskProfileState greys out the Model / Effort selects while a profile is
// chosen — the profile supplies those, so the selects are inert (their stored
// values are kept, for when the profile is cleared) — and shows a one-line
// note. Called on load and on every Profile change.
function applyAskProfileState() {
  const sel = $('#ask-profile');
  const active = !!controlValue(sel);
  const model = $('#ask-model'), effort = $('#ask-effort');
  if (model) model.disabled = active;
  if (effort) effort.disabled = active;
  const note = $('#ask-profile-state');
  if (note) {
    note.textContent = active
      ? `Profile “${controlValue(sel)}” supplies the harness/model/effort — the Model/Effort below are ignored.`
      : '';
  }
}

// setAskSelectValue selects value, first adding it as an option when the
// catalog doesn't list it (a hand-edited full model ID), so the form shows
// what is actually on disk rather than silently snapping to another option.
function setAskSelectValue(sel, value) {
  if (!sel) return;
  value = value || '';
  if (value && !Array.from(sel.querySelectorAll('option')).some(o => (o.getAttribute('value') ?? o.value) === value)) {
    const o = document.createElement('option');
    o.value = value;
    o.textContent = value;
    sel.appendChild(o);
  }
  setSelectValue(sel, value);
}

// populateScribeProfileSelect fills the Scribe-defaults Profile dropdown from
// the saved spawn profiles (the Groups-tab profiles), selecting `selected`. A
// chosen profile is the launch shape a FRESHLY summoned scribe adopts
// (JOH-371) — the harness-independent way to run scribes on Codex as well as
// Claude. Profile-only (no Model/Effort twins like Ask): the profile carries
// those. The fetch is async + best-effort: an endpoint error leaves just the
// "(default)" option. A hand-set profile that's since been deleted is kept as a
// "(missing)" option so the form shows what's on disk, not a silent reset.
async function populateScribeProfileSelect(selected) {
  const bindingEpoch = configBindingEpoch;
  const sel = $('#scribe-profile');
  if (!sel) return;
  const selectedName = (selected || '').trim();

  // Seed the selection SYNCHRONOUSLY before the async list load, so a Save that
  // races ahead of loadProfiles() still reads the real profile name off
  // #scribe-profile rather than an empty value (which assembleConfig would
  // persist as "delete scribe.profile", silently clearing a saved profile).
  replaceOptions(sel, [['', '(default — harness default)']]);
  if (selectedName) {
    const pending = document.createElement('option');
    pending.value = selectedName;
    pending.textContent = `${selectedName} (loading…)`;
    sel.appendChild(pending);
  }
  setSelectValue(sel, selectedName);

  let profiles = [];
  try { profiles = await loadProfiles(); } catch { profiles = []; }
  if (bindingEpoch !== configBindingEpoch || sel !== $('#scribe-profile')) return;
  replaceOptions(sel, [['', '(default — harness default)'], ...profileChoices(profiles).map(choice => [choice.value, choice.label])]);
  if (selectedName && !findProfileByHandle(profiles, selectedName)) {
    const o = document.createElement('option');
    o.value = selectedName;
    o.textContent = `${selectedName} (missing)`;
    sel.appendChild(o);
  }
  setSelectValue(sel, selectedName);
}

function renderCfgStringList(containerId, values, datalistId, placeholder) {
  configDependencies.lists.set(containerId, [...(values || [])]);
}
function renderCfgTransitionList(values) {
  configDependencies.lists.set('cfg-notif-transitions', (values || []).map(value => ({ ...value })));
}
function readCfgStringList(containerId) {
  return configDependencies.lists.get(containerId).map(value => String(value).trim()).filter(Boolean);
}
function readCfgTransitionList() {
  return configDependencies.lists.get('cfg-notif-transitions').flatMap(item => {
    const from = String(item.from || '').trim();
    const to = String(item.to || '').trim();
    // A row with either side filled is kept (a half-filled row then
    // surfaces as a server validation error rather than silently
    // vanishing); a fully blank row is dropped.
    return from || to ? [{ from, to }] : [];
  });
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
  configDependencies.lists.set('cfg-notif-transitions', rules);
  syncCfgNotifyTypes();
}

function renderCfgThresholdList(values) {
  configDependencies.lists.set('cfg-precompact-thresholds', (values || []).map(value => ({ ...value })));
}
function readCfgThresholdList() {
  return configDependencies.lists.get('cfg-precompact-thresholds').flatMap(item => {
    const w = String(item.window_size ?? '').trim();
    const m = String(item.min_tokens ?? '').trim();
    // A row with either side filled is kept so a half-filled row
    // surfaces as a server validation error instead of silently
    // vanishing; a fully blank row is dropped.
    return w || m ? [{ window_size: parseInt(w, 10) || 0, min_tokens: parseInt(m, 10) || 0 }] : [];
  });
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
  const ra = latestSnapshot().remote_access || {};
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
  setSelectValue($('#cfg-log-level'), cfg.log_level || 'info');
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

  // claude_cleanup_period_days — the Claude Code cleanupPeriodDays override
  // tclaude feeds into ~/.claude/settings.json. Blank when unset (Claude Code's
  // own 30-day default stands); a stored value shows as-is.
  $('#cfg-claude-cleanup-days').value = (cfg.claude_cleanup_period_days != null && cfg.claude_cleanup_period_days !== 0)
    ? cfg.claude_cleanup_period_days : '';

  $('#cfg-record-hooks').checked = !!cfg.record_hooks;

  // tui.color_scheme — the interactive watch views' palette. Absent/unknown
  // resolves to 'default'; only 'dark-high-contrast' is the non-default choice.
  setSelectValue($('#cfg-tui-color-scheme'), (cfg.tui && cfg.tui.color_scheme === 'dark-high-contrast')
    ? 'dark-high-contrast' : 'default');

  $('#cfg-focus-raiseonly').checked = !!(cfg.focus && cfg.focus.raise_only);
  // window_title: on is the default, so it's checked unless an explicit
  // focus.window_title:false (skip the title) unchecks it.
  $('#cfg-focus-window-title').checked = !(cfg.focus && cfg.focus.window_title === false);

  // focus.tile — auto-tiling after a bulk focus. Layout blank/absent
  // shows as "grid" (its resolved default); gap/margin blank when unset
  // so the human sees exactly what's on disk (an explicit 0 stays 0).
  const tile = (cfg.focus && cfg.focus.tile) || {};
  $('#cfg-focus-tile').checked = !!tile.enabled;
  setSelectValue($('#cfg-focus-tile-layout'), tile.layout || 'grid');
  $('#cfg-focus-tile-resize').checked = !!tile.resize;
  $('#cfg-focus-tile-gap').value = (tile.gap != null) ? tile.gap : '';
  $('#cfg-focus-tile-margin').value = (tile.margin != null) ? tile.margin : '';

  // Cost display multiplier — blank when unset (no adjustment); a stored
  // value shows as-is so the human sees exactly what's on disk.
  const cf = cfg.cost && cfg.cost.estimate_factor;
  $('#cfg-cost-factor').value = (cf != null && cf !== '') ? cf : '';
  // Show the Costs tab + per-agent cost on a subscription (WHAT-IF mode).
  // Default off (auto-hide on subscription).
  $('#cfg-cost-show-on-subscription').checked = !!(cfg.cost && cfg.cost.show_on_subscription);

  // Usage readout — statusline/cached by default, with an explicit opt-in for
  // periodic Anthropic usage API polling. Idle timeout is how long the Claude
  // 5h/7d bars keep their last-known reading after the source goes quiet.
  // Blank when unset (the Go default, 72h). A stored value shows as-is so the
  // human sees exactly what's on disk.
  $('#cfg-usage-poll-anthropic-api').checked = !!(cfg.usage && cfg.usage.poll_anthropic_api);
  const uit = cfg.usage && cfg.usage.idle_timeout;
  $('#cfg-usage-idle-timeout').value = (uit != null && uit !== '') ? uit : '';

  // Vegas music in regular mode — surface the Vegas tab / volume mixer /
  // radio outside slop mode. Default off; lives in the slop block.
  $('#cfg-slop-vegas-regular').checked = !!(cfg.slop && cfg.slop.vegas_in_regular_mode);

  // Hide the slop-mode side pull-lever. Default off (the lever shows);
  // lives in the slop block.
  $('#cfg-slop-hide-lever').checked = !!(cfg.slop && cfg.slop.hide_pull_lever);

  // Experimental feature flags — opt-in toggles for in-development features
  // (config features.*). Default off.
  $('#cfg-feature-processes').checked = !!(cfg.features && cfg.features.processes);
  $('#cfg-feature-agent-dirs-mount-parent').checked = !!(cfg.features && cfg.features.agent_dirs_mount_parent);

  // Activity bots — per-mode style of the deduped robot indicator.
  // Defaults: regular + wizard emoji, slop sprites (mirrors the Go resolvers).
  const ab = (cfg.dashboard && cfg.dashboard.activity_bots) || {};
  setSelectValue($('#cfg-dashboard-activity-bots-regular'), ab.regular || 'emoji');
  setSelectValue($('#cfg-dashboard-activity-bots-slop'), ab.slop || 'sprites');
  setSelectValue($('#cfg-dashboard-activity-bots-wizard'), ab.wizard || 'emoji');

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

  // Default terminal — route the plain focus / open-window / open-terminal
  // actions to in-browser web terminals. Default native (unchecked); only an
  // explicit dashboard.default_terminal:"web" checks it.
  $('#cfg-dashboard-default-web-terminal').checked = !!(cfg.dashboard && cfg.dashboard.default_terminal === 'web');

  // Local dashboards keep the native chooser by default; checked forces the
  // Preact web picker locally too. Remote origins use it regardless.
  $('#cfg-dashboard-default-web-directory-picker').checked = !!(cfg.dashboard && cfg.dashboard.default_directory_picker === 'web');

  // Per-agent "hide window" button — the slashed-eye beside "focus". Hidden
  // by default (unchecked); only an explicit dashboard.show_agent_hide_button
  // true checks it.
  $('#cfg-dashboard-show-agent-hide-btn').checked = !!(cfg.dashboard && cfg.dashboard.show_agent_hide_button);

  // Group description chip — the deprecated 📝 blurb in each group header.
  // Hidden by default (unchecked); only an explicit
  // dashboard.show_group_description true checks it.
  $('#cfg-dashboard-show-group-description').checked = !!(cfg.dashboard && cfg.dashboard.show_group_description);

  // Debug tab — daemon poll-timing diagnostics (TCL-376). Hidden by
  // default (unchecked); only an explicit dashboard.show_debug_tab true
  // checks it.
  $('#cfg-dashboard-show-debug-tab').checked = !!(cfg.dashboard && cfg.dashboard.show_debug_tab);

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

  // Scribe defaults — the spawn profile a freshly summoned scribe launches
  // with (JOH-371). Options are the saved spawn profiles; "(default)" (empty)
  // means the harness default. Async (fetches the profile list), like the ask
  // profile select above.
  void populateScribeProfileSelect((cfg.scribe || {}).profile);

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
  $('#cfg-agent-access-autoopen').checked = !!a.access_request_auto_open_browser;
  $('#cfg-agent-access-notify').checked = !!a.access_request_system_notification;
  $('#cfg-agent-notray').checked = !!a.disable_tray;
  $('#cfg-agent-persisttoken').checked = !!a.persist_operator_token;
  // 0 and absent both mean "random free port", so show a stored 0 as
  // blank — the form drops it on save anyway. A negative / out-of-range
  // value is shown as-is so the human sees the value the server rejects.
  $('#cfg-agent-dashboardport').value = (a.dashboard_port != null && a.dashboard_port !== 0) ? a.dashboard_port : '';
  // dashboard_bind: host the local dashboard binds to. Empty = loopback default.
  $('#cfg-agent-dashboardbind').value = a.dashboard_bind || '';
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

  cfg.log_level = controlValue($('#cfg-log-level'));
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

  // claude_cleanup_period_days is a top-level scalar (the Claude Code
  // cleanupPeriodDays override). Blank / 0 is the omitempty default — drop the
  // key so an all-default config doesn't marshal it. A non-empty value is
  // written verbatim so the server's Validate surfaces a bad one (e.g. a
  // negative) rather than it silently vanishing.
  const ccdRaw = $('#cfg-claude-cleanup-days').value.trim();
  if (ccdRaw !== '') cfg.claude_cleanup_period_days = cfgInt('cfg-claude-cleanup-days', 0); else delete cfg.claude_cleanup_period_days;

  cfg.record_hooks = $('#cfg-record-hooks').checked;

  // tui is an optional block. Clone the existing one so a future sub-field
  // with no widget round-trips, then set the one form-owned key. 'default' is
  // the omitempty default — drop the field, and the block when it's all that's
  // left, so an all-default config doesn't marshal a spurious "tui": {} diff.
  const tui = (cfg.tui && typeof cfg.tui === 'object') ? cfg.tui : {};
  const scheme = controlValue($('#cfg-tui-color-scheme'));
  if (scheme && scheme !== 'default') tui.color_scheme = scheme; else delete tui.color_scheme;
  if (Object.keys(tui).length) cfg.tui = tui; else delete cfg.tui;

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

  // usage is an optional block. Clone the existing one so a future sub-field
  // with no widget round-trips, then set the form-owned keys. poll_anthropic_api
  // is false by default, so drop it when unchecked. A blank idle_timeout is the
  // omitempty default (the Go default applies) — drop the field, and the block
  // when it's all that's left, so an all-default config doesn't marshal a
  // spurious "usage": {} diff. A non-empty value is written verbatim (even if
  // unparseable) so the server's Validate surfaces the error rather than the
  // value silently vanishing.
  const usage = (cfg.usage && typeof cfg.usage === 'object') ? cfg.usage : {};
  if ($('#cfg-usage-poll-anthropic-api').checked) usage.poll_anthropic_api = true; else delete usage.poll_anthropic_api;
  const uitRaw = $('#cfg-usage-idle-timeout').value.trim();
  if (uitRaw !== '') usage.idle_timeout = uitRaw; else delete usage.idle_timeout;
  if (Object.keys(usage).length) cfg.usage = usage; else delete cfg.usage;

  // features is an optional block — opt-in flags for in-development features
  // (config features.*). Clone the existing one so a future flag with no
  // widget round-trips. false is the omitempty default — drop the key, and
  // the block when it's all that's left, so an all-default config doesn't
  // marshal a spurious "features": {} diff.
  const feats = (cfg.features && typeof cfg.features === 'object') ? cfg.features : {};
  if ($('#cfg-feature-processes').checked) feats.processes = true; else delete feats.processes;
  if ($('#cfg-feature-agent-dirs-mount-parent').checked) feats.agent_dirs_mount_parent = true; else delete feats.agent_dirs_mount_parent;
  if (Object.keys(feats).length) cfg.features = feats; else delete cfg.features;

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
  // default per-mode styles (regular + wizard default emoji, slop default
  // sprites), dropping a default key — and the activity_bots / dashboard
  // objects when empty — so an all-default config marshals no spurious
  // "dashboard": {}. Mirrors the Go omitempty + per-mode-default resolvers.
  const dashboard = (cfg.dashboard && typeof cfg.dashboard === 'object') ? cfg.dashboard : {};
  const ab = (dashboard.activity_bots && typeof dashboard.activity_bots === 'object') ? dashboard.activity_bots : {};
  const abReg = controlValue($('#cfg-dashboard-activity-bots-regular'));
  const abSlop = controlValue($('#cfg-dashboard-activity-bots-slop'));
  const abWiz = controlValue($('#cfg-dashboard-activity-bots-wizard'));
  if (abReg && abReg !== 'emoji') ab.regular = abReg; else delete ab.regular;
  if (abSlop && abSlop !== 'sprites') ab.slop = abSlop; else delete ab.slop;
  if (abWiz && abWiz !== 'emoji') ab.wizard = abWiz; else delete ab.wizard;
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
  // default_terminal: native is the default, so store only the NON-default
  // "web" and drop the key otherwise — mirrors the Go omitempty + default-
  // native resolver.
  if ($('#cfg-dashboard-default-web-terminal').checked) dashboard.default_terminal = 'web'; else delete dashboard.default_terminal;
  // Remote dashboards always route to the web picker; this persisted opt-in
  // chooses the same UI on localhost instead of the native OS dialog.
  if ($('#cfg-dashboard-default-web-directory-picker').checked) dashboard.default_directory_picker = 'web'; else delete dashboard.default_directory_picker;
  // show_agent_hide_button: false (hidden) is the default, so store only the
  // NON-default true and drop the key otherwise — mirrors the Go omitempty.
  if ($('#cfg-dashboard-show-agent-hide-btn').checked) dashboard.show_agent_hide_button = true; else delete dashboard.show_agent_hide_button;
  // show_group_description: false (hidden) is the default, so store only the
  // NON-default true and drop the key otherwise — mirrors the Go omitempty.
  if ($('#cfg-dashboard-show-group-description').checked) dashboard.show_group_description = true; else delete dashboard.show_group_description;
  // show_debug_tab: false (hidden) is the default, so store only the
  // NON-default true and drop the key otherwise — mirrors the Go omitempty.
  if ($('#cfg-dashboard-show-debug-tab').checked) dashboard.show_debug_tab = true; else delete dashboard.show_debug_tab;
  if (Object.keys(dashboard).length) cfg.dashboard = dashboard; else delete cfg.dashboard;

  // ask is an optional block. Clone the existing one so a future sub-field
  // with no widget round-trips, then set the two form-owned keys. An empty
  // value (the "Built-in default" option) clears that field, and a block with
  // nothing left is dropped so an all-default ask doesn't marshal as a
  // spurious "ask": {} diff.
  const ask = (cfg.ask && typeof cfg.ask === 'object') ? cfg.ask : {};
  const askProfile = controlValue($('#ask-profile')).trim();
  const askModel = controlValue($('#ask-model')).trim();
  const askEffort = controlValue($('#ask-effort')).trim();
  // The profile selects the harness/model/effort a fresh ask runs at; the
  // Model/Effort here are kept (the no-profile fallback) but ignored while a
  // profile is set — resolveAskTarget applies that precedence server-side.
  if (askProfile) ask.profile = askProfile; else delete ask.profile;
  if (askModel) ask.model = askModel; else delete ask.model;
  if (askEffort) ask.effort = askEffort; else delete ask.effort;
  if (Object.keys(ask).length) cfg.ask = ask; else delete cfg.ask;

  // scribe is an optional block (JOH-371). Clone the existing one so a future
  // sub-field with no widget round-trips, set the one form-owned key, and drop
  // the block when the profile is cleared and nothing else remains — an
  // all-default scribe must not marshal as a spurious "scribe": {} diff.
  const scribe = (cfg.scribe && typeof cfg.scribe === 'object') ? cfg.scribe : {};
  const scribeProfile = controlValue($('#scribe-profile')).trim();
  if (scribeProfile) scribe.profile = scribeProfile; else delete scribe.profile;
  if (Object.keys(scribe).length) cfg.scribe = scribe; else delete cfg.scribe;

  // focus is an optional block. Clone the existing one so a future
  // sub-field with no widget round-trips, set the one form-owned key, and
  // drop the block when raise_only is off and nothing else remains — an
  // empty {} would marshal as a spurious diff against a config that simply
  // had no focus key.
  const fc = (cfg.focus && typeof cfg.focus === 'object') ? cfg.focus : {};
  if ($('#cfg-focus-raiseonly').checked) fc.raise_only = true; else delete fc.raise_only;
  // window_title: on is the default, so store only the NON-default (unchecked
  // → focus.window_title:false); checked leaves the key absent.
  if (!$('#cfg-focus-window-title').checked) fc.window_title = false; else delete fc.window_title;

  // focus.tile sub-block. Reuse the existing block object (a reference, so
  // a future sub-field with no widget round-trips untouched — same pattern
  // as fc / log_rotation above), then set only the form-owned keys. Each
  // key is dropped at its default (layout=grid, blank gap/margin) so an
  // untouched form leaves the block genuinely absent rather than
  // marshalling a spurious diff. gap/margin honour an explicit 0 (flush)
  // vs blank (default).
  const tc = (fc.tile && typeof fc.tile === 'object') ? fc.tile : {};
  if ($('#cfg-focus-tile').checked) tc.enabled = true; else delete tc.enabled;
  const tileLayout = controlValue($('#cfg-focus-tile-layout'));
  if (tileLayout && tileLayout !== 'grid') tc.layout = tileLayout; else delete tc.layout;
  if ($('#cfg-focus-tile-resize').checked) tc.resize = true; else delete tc.resize;
  const tileGap = $('#cfg-focus-tile-gap').value.trim();
  if (tileGap !== '' && Number.isFinite(parseInt(tileGap, 10))) tc.gap = parseInt(tileGap, 10); else delete tc.gap;
  const tileMargin = $('#cfg-focus-tile-margin').value.trim();
  if (tileMargin !== '' && Number.isFinite(parseInt(tileMargin, 10))) tc.margin = parseInt(tileMargin, 10); else delete tc.margin;
  if (Object.keys(tc).length) fc.tile = tc; else delete fc.tile;

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
  if ($('#cfg-agent-access-autoopen').checked) a.access_request_auto_open_browser = true;
  else delete a.access_request_auto_open_browser;
  if ($('#cfg-agent-access-notify').checked) a.access_request_system_notification = true;
  else delete a.access_request_system_notification;
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
  // dashboard_bind: host only. Empty / "127.0.0.1" = loopback default — drop the
  // key so config.json stays tidy; any other host is written verbatim (the
  // server's Validate rejects a host:port with a friendly message).
  const dbRaw = $('#cfg-agent-dashboardbind').value.trim();
  if (dbRaw !== '' && dbRaw !== '127.0.0.1') a.dashboard_bind = dbRaw; else delete a.dashboard_bind;
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
  el.replaceChildren();
}
function renderMessageList(el, heading, messages) {
  const strong = document.createElement('strong');
  strong.textContent = heading;
  const list = document.createElement('ul');
  list.append(...messages.map(message => {
    const item = document.createElement('li');
    item.textContent = message;
    return item;
  }));
  el.replaceChildren(strong, list);
}
function showConfigErrors(errs) {
  const el = $('#cfg-errors');
  renderMessageList(el, 'Cannot save — fix these first:', errs);
  el.style.display = 'block';
  el.scrollIntoView?.({ block: 'nearest' });
}

// The notice box (amber) carries load-time facts about the file the
// form cannot represent: a malformed file shown as defaults, or
// keys the running tclaude does not model and a save would drop.
function renderConfigNotice(messages) {
  const el = $('#cfg-notice');
  if (!messages.length) {
    el.style.display = 'none';
    el.replaceChildren();
    return;
  }
  renderMessageList(el, 'Heads up:', messages);
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
  const bindingEpoch = configBindingEpoch;
  // Refresh the slug / group datalists from the latest snapshot.
  const snap = latestSnapshot();
  replaceOptions($('#cfg-slug-list'), (snap.slugs || []).map(slug => [slug.slug, '']));
  replaceOptions($('#cfg-group-list'), (snap.groups || []).map(group => [group.name, '']));
  $('#cfg-status').textContent = 'loading…';
  configLifecycle.loading?.();
  clearConfigErrors();
  renderConfigNotice([]);
  try {
    const r = await configDependencies.fetchImpl('/api/config', { credentials: 'same-origin' });
    if (bindingEpoch !== configBindingEpoch) return;
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const data = await r.json();
    if (bindingEpoch !== configBindingEpoch) return;
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
    configLifecycle.loaded?.(data);
  } catch (e) {
    if (bindingEpoch !== configBindingEpoch) return;
    configLoaded = false;
    $('#cfg-status').textContent = 'failed to load';
    showConfigErrors(['Could not load config: ' + (e.message || e)]);
    configLifecycle.failed?.(e);
  }
  // Re-apply any standing filter — a Reload rebuilds the inner lists, and
  // the operator may have a query typed; keep what's hidden hidden.
  applyConfigFilter();
}

// reportConfigHTTPError surfaces a non-OK /api/config response and
// returns true when it handled one — the caller then aborts. 400 is
// the structured validation contract; 409 is the drift guard (the
// file changed under the editor).
async function reportConfigHTTPError(resp) {
  if (resp.status === 400) {
    const d = await resp.json().catch(() => ({}));
    showConfigErrors(d.errors && d.errors.length ? d.errors : ['Config rejected by the server.']);
    configLifecycle.failed?.(new Error('Config rejected by the server.'));
    return true;
  }
  if (resp.status === 409) {
    const d = await resp.json().catch(() => ({}));
    showConfigErrors([(d.error || 'config.json changed on disk') +
      ' — press Reload to pick up the current file, then re-apply your edits.']);
    configLifecycle.failed?.(new Error(d.error || 'config.json changed on disk'));
    return true;
  }
  if (!resp.ok) {
    showConfigErrors(['Server error: HTTP ' + resp.status]);
    configLifecycle.failed?.(new Error('Server error: HTTP ' + resp.status));
    return true;
  }
  return false;
}

async function saveConfig() {
  const bindingEpoch = configBindingEpoch;
  if (!configLoaded) { configDependencies.toast('Config not loaded yet', true); return; }
  clearConfigErrors();
  let edited;
  try {
    edited = assembleConfig();
  } catch (e) {
    showConfigErrors([e.message || String(e)]);
    configLifecycle.failed?.(e);
    return;
  }
  // The body carries the edited config plus the canonical baseline
  // the form loaded — the server 409s if the file drifted since.
  const body = JSON.stringify({ config: edited, base: configBaseRaw });
  const post = (query) => configDependencies.fetchImpl('/api/config' + query, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' }, body,
  });
  const saveBtn = $('#cfg-save');
  saveBtn.disabled = true;
  configLifecycle.saving?.();
  try {
    // Phase 1: dry-run — server validates and returns the canonical
    // "after" without writing anything.
    const dry = await post('?dry_run=1');
    if (bindingEpoch !== configBindingEpoch) return;
    if (await reportConfigHTTPError(dry)) return;
    const after = (await dry.json()).raw || '';
    if (bindingEpoch !== configBindingEpoch) return;
    // When the on-disk file is corrupt the diff baseline is "defaults",
    // so after===base can hold even though the save still meaningfully
    // replaces the corrupt file — don't skip it then.
    if (after === configBaseRaw && !configFileMalformed) { configDependencies.toast('No changes to save'); configLifecycle.ready?.(); return; }

    // Phase 2: human confirms the diff before the real write.
    const ok = await configDependencies.confirmDiff(
      configBaseRaw, after, configFileMalformed, $('#cfg-path').textContent,
    );
    if (bindingEpoch !== configBindingEpoch) return;
    if (!ok) { configDependencies.toast('Save cancelled'); configLifecycle.ready?.(); return; }

    // replace_malformed acknowledges wiping a corrupt on-disk file.
    const res = await post(configFileMalformed ? '?replace_malformed=1' : '');
    if (bindingEpoch !== configBindingEpoch) return;
    if (await reportConfigHTTPError(res)) return;
    const data = await res.json();
    if (bindingEpoch !== configBindingEpoch) return;
    configBaseRaw = data.raw || configBaseRaw;
    configObj = JSON.parse(configBaseRaw);
    configFileMalformed = false; // the file is canonical after a save
    populateConfigForm(configObj);
    // The saved file is canonical now — any load-time notice (a
    // malformed file, or unknown keys that this save dropped) is
    // stale, so clear it.
    renderConfigNotice([]);
    $('#cfg-status').textContent = 'saved · ' + new Date().toLocaleTimeString();
    configDependencies.toast('Config saved to ' + $('#cfg-path').textContent);
    configLifecycle.saved?.(data);
  } catch (e) {
    if (bindingEpoch !== configBindingEpoch) return;
    showConfigErrors(['Save failed: ' + (e.message || e)]);
    configLifecycle.failed?.(e);
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

function handleConfigEvent(event) {
  const target = event.target;
  const id = target.id;
  if (event.type === 'input') {
    if (id === 'cfg-filter') applyConfigFilter();
    if (target.closest?.('#cfg-notif-transitions')) syncCfgNotifyTypes();
    if (id === 'cfg-remote-host' || id === 'cfg-remote-port') syncCfgRemoteStatus();
    return;
  }
  if (event.type === 'keydown' && id === 'cfg-filter' && event.key === 'Escape' && target.value) {
    event.preventDefault();
    target.value = '';
    applyConfigFilter();
    return;
  }
  if (event.type === 'change') {
    if (['cfg-precompact-enabled', 'cfg-ratelimit-enabled', 'cfg-agent-spawnmax-enabled',
      'cfg-nudge-enabled', 'cfg-agent-retired-cleanup-enabled'].includes(id)) syncCfgEnables();
    const notifyType = target.getAttribute?.('data-cfg-notify-type');
    if (notifyType) setCfgNotifyType(notifyType, target.checked);
    if (id === 'ask-profile') applyAskProfileState();
    if (id === 'cfg-remote-host' || id === 'cfg-remote-port') syncCfgRemoteStatus();
    if (id === 'cfg-remote-enabled') {
      const portEl = $('#cfg-remote-port');
      if (target.checked && portEl && !portEl.value.trim()) portEl.value = REMOTE_DEFAULT_PORT;
      syncCfgRemoteStatus();
    }
    return;
  }
  if (event.type !== 'click') return;
  if (id === 'cfg-reload') { void loadConfigTab(); return; }
  if (id === 'cfg-save') { void saveConfig(); return; }
  if (id === 'cfg-filter-clear') {
    const filterInput = $('#cfg-filter');
    if (filterInput) { filterInput.value = ''; filterInput.focus(); }
    applyConfigFilter();
  }
}

function bindConfigActivation() {
  const bindingEpoch = ++configBindingEpoch;
  // Lazy-load on the first activation of the Config tab.
  const navBtn = $('nav [data-tab="config"]');
  const activate = () => {
    if (!configLoaded) loadConfigTab();
    // Focus the search on a deliberate switch — a mouse click, the command
    // palette's "Go to Config", or a "Config ↗" deep link — but NOT during
    // keyboard tab-cycling ([ / ] and ←/→). Focusing the <input> there
    // would trap those very hotkeys (isEditableTarget) and strand the user
    // on Config; see isCyclingTabs in refresh.js.
    if (!configDependencies.isCyclingTabs()) focusConfigSearch();
  };
  navBtn?.addEventListener('click', activate);
  return () => {
    if (configBindingEpoch === bindingEpoch) configBindingEpoch++;
    configLoaded = false;
    navBtn?.removeEventListener('click', activate);
  };
}

export { assembleConfig, bindConfigActivation, configLineDiff, handleConfigEvent, loadConfigTab, populateConfigForm, saveConfig };
