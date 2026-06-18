// modal-spawn.js — the spawn / clone / reincarnate agent modals.
//
// Extracted from dashboard.js in the Stage 2 module split. The spawn and
// clone modals embed the worktree picker from modal-link-wt.

import { $, $$, esc, shortId, syncSelectTitle, bindSelectTitles, makeModalResizable, bindModalSubmitHotkey } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { loadProfiles, getProfile, getDashDefaultProfile } from './profiles.js';
import { groupDefaultContext } from './modal-templates.js';
import {
  WT_NEW, wtToggleNew, wtLoad, bindWtPicker, wtResolve, wtResolveCwd,
} from './modal-link-wt.js';
// lastSnapshot lives in dashboard.js; refresh() / toast in refresh.js.
// Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, toast, bindBackdropDiscard } from './refresh.js';
import { slopJackpot } from './slop-fx.js';


// ---- Agent spawn modal --------------------------------------------------
//
// Opens with `{groupName}` pre-filled from a group header's
// "+ spawn agent" button — the group is fixed and the <select> stays
// hidden. (The form still supports an empty open, showing the group
// <select>, for any future caller.) On submit it POSTs to
// /api/groups/{name}/spawn, which forks `tclaude session new` and waits
// for the conv-id before returning.

// Tracks the cwd value the spawn form last auto-filled from a group
// default, so switching the group <select> can refresh the prefill
// without clobbering a path the user typed by hand.
let lastSpawnCwdPrefill = '';

// True once the human has typed in the "Worktree repo" field. Until
// then that field mirrors CWD; after, CWD changes leave it alone so
// a deliberately-pointed sub-repo path isn't clobbered.
let spawnWtRepoEdited = false;

// groupDefaultCwd looks up a group's default spawn dir from the
// latest snapshot. "" when the group is unknown or has no default.
function groupDefaultCwd(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const g = groups.find(x => x.name === groupName);
  return (g && g.default_cwd) || '';
}

// updateSpawnModelDefaultLabel rewrites the Model dropdown's "Default"
// option so it names the model an omitted pick actually resolves to —
// instead of an opaque "Default". It shows a migrated group's legacy
// default_model when present; otherwise the daemon fills blank launch
// fields from the group's default spawn profile (JOH-210) at spawn, and
// claude itself falls back to the user-level settings.json model. (Full
// profile-aware prefill is a coming update.) Called on modal open and
// whenever the group <select> changes.
function updateSpawnModelDefaultLabel(groupName) {
  const opt = $('#agent-spawn-model').querySelector('option[value=""]');
  if (!opt) return;
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const g = groups.find(x => x.name === groupName);
  const groupModel = (g && g.default_model) || '';
  const userModel = (lastSnapshot && lastSnapshot.user_default_model) || '';
  if (groupModel) {
    opt.textContent = `Default (${groupModel} — group default)`;
  } else if (userModel) {
    opt.textContent = `Default (${userModel} — user settings)`;
  } else {
    opt.textContent = "Default (claude's own)";
  }
  // The Default label changed under a possibly-unchanged selection (no
  // `change` event), so refresh the closed-box tooltip — this label can
  // be long enough to clip in the width-limited field.
  syncSelectTitle($('#agent-spawn-model'));
}

// ---- Harness selection --------------------------------------------------
//
// The spawn dialog drives its harness selector + per-harness Model / Effort
// / Sandbox menus off the snapshot's `harnesses` catalog (JOH-162). The
// default harness (Claude Code) keeps its curated Model <select>; a harness
// with no curated model list (Codex) swaps in a free-text Model input, and
// a harness that takes a launch sandbox (Codex) reveals the Sandbox <select>.

// spawnHarnessCatalog returns the snapshot's harness catalog (array), or []
// when it hasn't loaded yet.
function spawnHarnessCatalog() {
  return (lastSnapshot && lastSnapshot.harnesses) || [];
}

// spawnHarnessByName returns the catalog entry for a harness name, or null.
function spawnHarnessByName(name) {
  return spawnHarnessCatalog().find(h => h.name === name) || null;
}

// populateSpawnHarnessSelect fills the harness <select> from the catalog,
// defaulting the selection to Claude Code (the registry's default) when
// present, else the first entry. A catalog with a single harness still
// renders the selector (it just has one option) so the row's shape is
// stable; an empty catalog (snapshot not yet loaded) leaves it empty and
// applySpawnHarness falls back to the default-harness layout.
function populateSpawnHarnessSelect() {
  const sel = $('#agent-spawn-harness');
  const cat = spawnHarnessCatalog();
  sel.innerHTML = cat
    .map(h => `<option value="${esc(h.name)}">${esc(h.display_name || h.name)}</option>`)
    .join('');
  if (cat.some(h => h.name === 'claude')) sel.value = 'claude';
  else if (cat.length) sel.value = cat[0].name;
}

// activeSpawnModelEl returns the Model control currently in play for the
// selected harness — the curated <select> for a harness with a model list,
// or the free-text <input> for one without (Codex). Used so submit + the
// per-model effort memory read whichever control is visible.
function activeSpawnModelEl() {
  const h = spawnHarnessByName($('#agent-spawn-harness').value);
  const codexStyle = h && (!h.models || h.models.length === 0);
  return codexStyle ? $('#agent-spawn-model-codex') : $('#agent-spawn-model');
}

// populateSpawnEffortSelect rebuilds the Effort <select> options from the
// harness's effort levels, making the catalog (server-side
// clcommon.ValidEffortLevels) the single source of truth — adding a level
// needs no dashboard edit, and a future harness with its own reasoning
// scale just works. Keeps the leading Default ("") option and preserves the
// current selection when the new list still offers it. Leaves the static
// HTML options in place when the catalog hasn't loaded (h is null) or
// carries no levels, so the field still works pre-snapshot.
function populateSpawnEffortSelect(h) {
  const levels = h && h.effort_levels;
  if (!levels || !levels.length) return; // keep the static fallback options
  const sel = $('#agent-spawn-effort');
  const prev = sel.value;
  sel.innerHTML = `<option value="">Default (harness's own)</option>`
    + levels.map(l => `<option value="${esc(l)}">${esc(l)}</option>`).join('');
  if ([...sel.options].some(o => o.value === prev)) sel.value = prev;
}

// applySpawnHarness reshapes the Model + Sandbox + Effort menus for the
// chosen harness: a harness with a curated model list shows the <select>,
// one without shows the free-text input; a harness that takes a launch
// sandbox reveals the Sandbox <select> (populated from its modes, defaulted
// to its secure default), and one without hides it; the Effort menu is
// rebuilt from the harness's levels (today identical across harnesses, but
// data-driven so it tracks the catalog). Re-applies the remembered effort
// for whatever Model control is now active.
function applySpawnHarness(harnessName) {
  const h = spawnHarnessByName(harnessName);
  // No catalog entry (snapshot not loaded, or unknown harness): fall back
  // to the default Claude-Code layout — curated model select, no sandbox.
  const hasModelList = !h || (h.models && h.models.length > 0);
  $('#agent-spawn-model-claude-row').style.display = hasModelList ? '' : 'none';
  $('#agent-spawn-model-codex-row').style.display = hasModelList ? 'none' : '';

  const canSandbox = !!(h && h.can_sandbox && h.sandbox_modes && h.sandbox_modes.length);
  const sandboxRow = $('#agent-spawn-sandbox-row');
  sandboxRow.style.display = canSandbox ? '' : 'none';
  if (canSandbox) {
    const sandSel = $('#agent-spawn-sandbox');
    // The default mode (Codex: the managed tclaude-agent profile) is flagged
    // "(recommended)" in its label — data-driven off default_sandbox, so no
    // mode name is hardcoded here. The option value stays the raw mode token.
    sandSel.innerHTML = h.sandbox_modes
      .map(m => `<option value="${esc(m)}">${esc(m)}${m === h.default_sandbox ? ' (recommended)' : ''}</option>`)
      .join('');
    // Pre-select the harness's secure default (the managed profile for Codex).
    sandSel.value = h.default_sandbox || h.sandbox_modes[0];
    applySpawnSandboxHint(h);
  } else {
    applySpawnSandboxHint(null);
  }

  // Codex-only: the opt-in "pre-trust this dir" checkbox (JOH-205). It edits
  // the user's ~/.codex/config.toml, so it is OFF by default and never
  // auto-checked; hiding it for a non-Codex harness also clears it so the
  // choice can't leak across a harness switch. Gated on the harness name, the
  // same way the body below gates `harness !== 'claude'`.
  const isCodexHarness = !!(h && h.name === 'codex');
  $('#agent-spawn-trust-dir-row').style.display = isCodexHarness ? '' : 'none';
  if (!isCodexHarness) $('#agent-spawn-trust-dir').checked = false;

  // Rebuild the Effort menu for this harness (data-driven off the catalog),
  // then re-apply the effort remembered for the now-active model control.
  populateSpawnEffortSelect(h);
  applyRememberedEffort(activeSpawnModelEl().value);
}

// applySpawnSandboxHint sets the live help line under the Sandbox selector to
// the catalog's description of the currently-selected mode — especially its
// agentd-socket reachability, the thing that surprises operators. A description
// carrying the "⚠" caveat marker (the raw --sandbox modes, which can't reach
// agentd, and danger-full-access, which disables the sandbox) is shown in the
// warn colour. Passing null (a harness with no launch sandbox) clears it.
function applySpawnSandboxHint(h) {
  const hintEl = $('#agent-spawn-sandbox-hint');
  if (!hintEl) return;
  const help = (h && h.sandbox_mode_help) || {};
  const text = help[$('#agent-spawn-sandbox').value] || '';
  // Trusted catalog copy (not user input): escape, then render `…` spans as
  // <code> so the `tclaude agent` references read as code.
  hintEl.innerHTML = esc(text).replace(/`([^`]+)`/g, '<code>$1</code>');
  hintEl.classList.toggle('warn', text.includes('⚠'));
}

// spawnAutoFocusPref reads the persisted "auto focus" checkbox state
// for the spawn modal. Defaults to true: a freshly-spawned agent runs
// detached with no window, so the common case is wanting one opened.
function spawnAutoFocusPref() {
  try {
    const v = dashPrefs.getItem('tclaude.dash.spawn.autofocus');
    return v === null ? true : v === '1';
  } catch (_) { return true; }
}

// ---- Per-model effort memory --------------------------------------------
//
// When the human picks an effort for a given Model and spawns, we
// remember it keyed by the exact Model <select> value ("" = the
// Default option) so re-selecting that model in a later spawn dialog
// re-applies the same effort. The use case: default to high for fable
// models but xhigh for opus models, without re-picking every spawn.
// Stored as a JSON object { model: effort } in dashPrefs.
const SPAWN_MODEL_EFFORT_KEY = 'tclaude.dash.spawn.modelEffort';

// loadModelEffortMap reads the persisted model→effort map. Returns an
// empty object on any error (missing key, corrupt JSON, non-object).
function loadModelEffortMap() {
  try {
    const obj = JSON.parse(dashPrefs.getItem(SPAWN_MODEL_EFFORT_KEY));
    return (obj && typeof obj === 'object') ? obj : {};
  } catch (_) { return {}; }
}

// rememberModelEffort records `effort` as the remembered default for
// `model`. A blank effort (the Default option) drops any prior entry
// so the model falls back to Default next time, keeping the map tidy.
function rememberModelEffort(model, effort) {
  try {
    const map = loadModelEffortMap();
    if (effort) map[model] = effort;
    else delete map[model];
    dashPrefs.setItem(SPAWN_MODEL_EFFORT_KEY, JSON.stringify(map));
  } catch (_) {}
}

// applyRememberedEffort sets the Effort <select> to the value last
// remembered for `model`, or back to Default ("") when none is stored.
// Call it on modal open and whenever the Model <select> changes.
function applyRememberedEffort(model) {
  $('#agent-spawn-effort').value = loadModelEffortMap()[model] || '';
}

// prefillSpawnCwd fills #agent-spawn-cwd with the group's default
// dir. With force=false it leaves a user-typed value alone — it
// only overwrites an empty field or a stale prior auto-prefill.
function prefillSpawnCwd(groupName, force) {
  const cwdEl = $('#agent-spawn-cwd');
  if (!force && cwdEl.value.trim() !== '' && cwdEl.value !== lastSpawnCwdPrefill) {
    return;
  }
  const dflt = groupDefaultCwd(groupName);
  cwdEl.value = dflt;
  lastSpawnCwdPrefill = dflt;
}

// updateSpawnGroupContextRow shows the "include group default
// context" checkbox only when the selected group actually has a
// startup context — there's nothing to opt into otherwise. The
// checkbox is (re)set to checked whenever the row becomes visible
// so switching groups always lands on the opt-in default.
function updateSpawnGroupContextRow(groupName) {
  const hasContext = groupDefaultContext(groupName).trim() !== '';
  $('#agent-spawn-group-context-row').style.display = hasContext ? '' : 'none';
  if (hasContext) $('#agent-spawn-group-context').checked = true;
}

// Label for the leading "no worktree" option in the spawn modal's
// worktree picker.
const SPAWN_WT_NONE = '(no worktree — use CWD above)';

// applyWtSync reflects the "Sync worktree branch with name"
// checkbox into the spawn modal's worktree picker. Call it after
// the picker (re)loads, after the name changes, and whenever the
// checkbox itself is toggled.
//
// The sync only works when the picker landed on a usable git repo —
// wtRefresh leaves the <select> disabled in every other state ((no
// CWD), (not a repo), still loading) — so the checkbox is disabled
// to match. When checked with a non-empty name it forces the
// picker into "+ create new worktree" and mirrors the name into
// the new-branch field; clearing the name drops it back to "no
// worktree".
function applyWtSync() {
  const syncEl = $('#agent-spawn-wt-sync');
  const select = $('#agent-spawn-worktree');
  const usable = !select.disabled;
  syncEl.disabled = !usable;
  $('#agent-spawn-wt-sync-row').classList.toggle('disabled', !usable);
  if (!usable || !syncEl.checked) return;
  const name = $('#agent-spawn-name').value.trim();
  if (name) {
    if (select.value !== WT_NEW) select.value = WT_NEW;
    wtToggleNew('agent-spawn', true);
    $('#agent-spawn-wt-branch').value = name;
  } else if (select.value === WT_NEW) {
    // Name cleared while syncing — fall back to "no worktree".
    select.value = '';
    wtToggleNew('agent-spawn', false);
    $('#agent-spawn-wt-branch').value = '';
  }
}

// spawnWtLoad reloads the spawn worktree picker for `cwd`, then
// re-applies the name-sync checkbox once the list settles (the
// checkbox's usable state depends on whether `cwd` is a git repo).
function spawnWtLoad(cwd) {
  return wtLoad('agent-spawn', cwd, SPAWN_WT_NONE).then(applyWtSync);
}

// ---- Load-from-profile pre-fill (JOH-210) -------------------------------
//
// A spawn profile is a saved bundle of (most of) this dialog's fields. The
// "Profile" row at the top of the modal loads one in: on open the group's
// own default profile (or, failing that, the dashboard default) is applied
// automatically, and the human can pick another from the selector or Clear
// back to blank. Only the dialog fields are touched — cwd / worktree are a
// "where", not part of a profile, so they're left alone.

// groupDefaultProfileName looks up a group's default spawn profile from the
// latest snapshot. "" when the group is unknown or has none.
function groupDefaultProfileName(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const g = groups.find(x => x.name === groupName);
  return (g && g.default_profile) || '';
}

// applyProfileToSpawnForm fills the dialog from a profile object. Every field
// is optional: a field the profile leaves unset keeps the dialog's own blank
// default (so a sparse profile only overrides what it actually sets). Harness
// goes first because it reshapes the Model / Sandbox / Effort / trust-dir rows;
// the per-field controls are then set into that reshaped layout. cwd, worktree
// and the group are deliberately untouched.
function applyProfileToSpawnForm(p) {
  if (!p) return;
  // Harness first — applySpawnHarness rebuilds the Model/Sandbox/Effort/trust
  // controls for it. Only switch when the profile names a harness the catalog
  // actually offers; otherwise leave the selector on its current value.
  if (p.harness) {
    const hSel = $('#agent-spawn-harness');
    if ([...hSel.options].some(o => o.value === p.harness)) hSel.value = p.harness;
  }
  applySpawnHarness($('#agent-spawn-harness').value);

  // Model goes into whichever control the (now-applied) harness uses — the
  // curated <select> for Claude, the free-text <input> for Codex. A curated
  // value not in the <select>'s options silently stays on the prior pick.
  if (p.model) activeSpawnModelEl().value = p.model;

  // Effort: an explicit profile value wins (when the harness offers it);
  // otherwise fall back to the per-model effort memory for the model just set,
  // exactly as picking that model by hand would (applySpawnHarness already
  // applied it for the blank model, so re-apply now that the model changed).
  const effSel = $('#agent-spawn-effort');
  if (p.effort && [...effSel.options].some(o => o.value === p.effort)) {
    effSel.value = p.effort;
  } else if (!p.effort) {
    applyRememberedEffort(activeSpawnModelEl().value);
  }

  if (p.sandbox) {
    const sandSel = $('#agent-spawn-sandbox');
    if ([...sandSel.options].some(o => o.value === p.sandbox)) {
      sandSel.value = p.sandbox;
      applySpawnSandboxHint(spawnHarnessByName($('#agent-spawn-harness').value));
    }
  }
  // trust_dir is a *bool — apply only when the profile set it (null = unset).
  // The row is Codex-only and hidden otherwise; setting the checkbox while
  // hidden is harmless (submit reads it only for Codex).
  if (p.trust_dir != null) $('#agent-spawn-trust-dir').checked = !!p.trust_dir;

  if (p.agent_name) $('#agent-spawn-name').value = p.agent_name;
  if (p.role) $('#agent-spawn-role').value = p.role;
  if (p.descr) $('#agent-spawn-descr').value = p.descr;
  if (p.initial_message) $('#agent-spawn-init-msg').value = p.initial_message;
  if (p.auto_focus != null) $('#agent-spawn-focus').checked = !!p.auto_focus;
  if (p.include_group_default_context != null) {
    $('#agent-spawn-group-context').checked = !!p.include_group_default_context;
  }
  // The name may have changed → re-sync the worktree branch name.
  applyWtSync();
}

// clearSpawnProfileFields resets the profile-controlled fields to their
// fresh-open blank defaults and drops the Profile selector back to "(none)".
// It deliberately leaves the group, cwd and worktree alone — those aren't part
// of a profile. Mirrors the blank-init subset of openAgentSpawnModal; shared
// by the Clear button and the selector's "(none)" choice.
function clearSpawnProfileFields() {
  $('#agent-spawn-name').value = '';
  $('#agent-spawn-role').value = '';
  $('#agent-spawn-descr').value = '';
  $('#agent-spawn-init-msg').value = '';
  $('#agent-spawn-model').value = '';
  $('#agent-spawn-model-codex').value = '';
  populateSpawnHarnessSelect();
  applySpawnHarness($('#agent-spawn-harness').value);
  applyRememberedEffort(activeSpawnModelEl().value);
  $('#agent-spawn-focus').checked = spawnAutoFocusPref();
  updateSpawnGroupContextRow($('#agent-spawn-group').value);
  $('#agent-spawn-load-profile').value = '';
  syncSelectTitle($('#agent-spawn-load-profile'));
  applyWtSync();
}

// initSpawnProfileSelector populates the Profile selector from the saved
// profiles and applies the pre-fill for `groupName`: the group's own default
// profile, else the dashboard default. Runs async (the list is fetched), so it
// guards against the modal being closed or its group switched out from under a
// slow fetch before it touches the form.
async function initSpawnProfileSelector(groupName) {
  const sel = $('#agent-spawn-load-profile');
  const prefill = groupDefaultProfileName(groupName) || getDashDefaultProfile();
  let profiles = [];
  try {
    profiles = await loadProfiles();
  } catch (_) {
    profiles = []; // endpoint error — leave the selector with just "(none)"
  }
  // Stale-guard: a fast close/reopen (or group switch) may have superseded
  // this fetch — don't stomp the now-current dialog state.
  if (!$('#agent-spawn-modal').classList.contains('show')) return;
  if ($('#agent-spawn-group').value !== groupName) return;
  sel.innerHTML = `<option value="">— none (blank form) —</option>`
    + profiles.map(p => `<option value="${esc(p.name)}">${esc(p.name)}</option>`).join('');
  if (prefill && profiles.some(p => p.name === prefill)) {
    sel.value = prefill;
    applyProfileToSpawnForm(profiles.find(p => p.name === prefill));
  } else {
    sel.value = '';
  }
  syncSelectTitle(sel);
}

function openAgentSpawnModal(opts) {
  const groupName = (opts && opts.groupName) || '';
  const groupRow = $('#agent-spawn-group-row');
  const select = $('#agent-spawn-group');
  // Populate the <select> from the latest snapshot. The select stays
  // hidden when groupName is fixed; we still set the value so submit
  // can read it from one place.
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  select.innerHTML = groups.map(g => `<option value="${esc(g.name)}">${esc(g.name)}</option>`).join('');
  if (groupName) {
    // Pre-pinned: append/select the target group even if it isn't in
    // the snapshot yet (paranoid — the user just clicked its header
    // so it must be there, but defend anyway).
    if (![...select.options].some(o => o.value === groupName)) {
      const opt = document.createElement('option');
      opt.value = groupName;
      opt.textContent = groupName;
      select.appendChild(opt);
    }
    select.value = groupName;
    groupRow.style.display = 'none';
  } else {
    groupRow.style.display = '';
    if (!select.value && groups.length) select.value = groups[0].name;
  }
  $('#agent-spawn-name').value = '';
  $('#agent-spawn-role').value = '';
  $('#agent-spawn-descr').value = '';
  $('#agent-spawn-init-msg').value = '';
  $('#agent-spawn-model').value = '';
  $('#agent-spawn-model-codex').value = '';
  // Populate the harness selector from the catalog and reshape the Model /
  // Sandbox rows for the chosen harness (default Claude Code). This also
  // re-applies the remembered effort for the now-active Model control, so
  // the explicit applyRememberedEffort below is only the fallback for an
  // empty / not-yet-loaded catalog.
  populateSpawnHarnessSelect();
  applySpawnHarness($('#agent-spawn-harness').value);
  // Restore the effort last remembered for the selected model (the
  // Default model on a fresh open) — see rememberModelEffort.
  applyRememberedEffort(activeSpawnModelEl().value);
  $('#agent-spawn-cwd').value = '';
  // Restore the auto-focus checkbox from the human's last choice
  // (defaults on — see spawnAutoFocusPref).
  $('#agent-spawn-focus').checked = spawnAutoFocusPref();
  // Prefill the cwd from the selected group's default spawn dir.
  // force=true: the modal just opened fresh, so there's no
  // user-typed value to protect.
  prefillSpawnCwd(select.value, true);
  // Show the "include group default context" checkbox iff the
  // selected group carries a startup context.
  updateSpawnGroupContextRow(select.value);
  // Name what "Default" in the Model dropdown resolves to for this
  // group (group default → user settings → claude's own).
  updateSpawnModelDefaultLabel(select.value);
  $('#agent-spawn-wt-branch').value = '';
  // The worktree picker targets a separate "Worktree repo" field.
  // It mirrors CWD until the human edits it; for a monorepo CWD the
  // field's datalist offers the nested repos to drill into.
  spawnWtRepoEdited = false;
  $('#agent-spawn-subrepo-list').innerHTML = '';
  $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
  // Restore the name→branch sync to its default-on state.
  $('#agent-spawn-wt-sync').checked = true;
  // Load the worktree picker against the Worktree-repo field, then
  // apply the name-sync checkbox once it settles.
  spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
  $('#agent-spawn-error').textContent = '';
  const meta = $('#agent-spawn-meta');
  if (groupName) {
    meta.textContent = `joining group: ${groupName}`;
    meta.style.display = '';
  } else {
    meta.style.display = 'none';
  }
  $('#agent-spawn-modal').classList.add('show');
  // Populate the Profile selector and apply the group/dashboard default
  // profile. Runs after the modal is shown so its own stale-guard (which
  // checks the modal is still open) is satisfied; the fields above stand as
  // the blank baseline a profile then overlays.
  initSpawnProfileSelector(select.value);
  setTimeout(() => {
    if (groupName) $('#agent-spawn-name').focus();
    else select.focus();
  }, 0);
}

function closeAgentSpawnModal() {
  $('#agent-spawn-modal').classList.remove('show');
}

async function submitAgentSpawn() {
  const group = $('#agent-spawn-group').value;
  const name = $('#agent-spawn-name').value.trim();
  const role = $('#agent-spawn-role').value.trim();
  const descr = $('#agent-spawn-descr').value.trim();
  // The initial message is delivered to the new agent's inbox (an
  // agent_messages row), not typed into its pane — so newlines are
  // preserved. Send the textarea verbatim; the daemon trims it.
  const initMsg = $('#agent-spawn-init-msg').value;
  // Empty value = the "Default" option → omit effort/model entirely.
  // For model that means the daemon fills the blank field from the group's
  // default spawn profile (JOH-210), and failing that claude resolves its
  // own default (user settings.json, then built-in). A chosen value rides
  // along in the POST body.
  const effort = $('#agent-spawn-effort').value;
  // Harness drives which Model control is active (curated <select> vs the
  // Codex free-text input) and whether a Sandbox was chosen.
  const harness = $('#agent-spawn-harness').value;
  const model = activeSpawnModelEl().value.trim();
  const harnessEntry = spawnHarnessByName(harness);
  const sandbox = (harnessEntry && harnessEntry.can_sandbox)
    ? $('#agent-spawn-sandbox').value : '';
  const cwd = $('#agent-spawn-cwd').value.trim();
  const wtRepo = $('#agent-spawn-wt-repo').value.trim();
  const autoFocus = $('#agent-spawn-focus').checked;
  const includeGroupContext = $('#agent-spawn-group-context').checked;
  const errEl = $('#agent-spawn-error');
  errEl.textContent = '';
  if (!group) {
    errEl.textContent = 'group is required';
    return;
  }
  // Persist the checkbox so the human's choice sticks across spawns.
  try { dashPrefs.setItem('tclaude.dash.spawn.autofocus', autoFocus ? '1' : '0'); } catch (_) {}
  // Remember this model's effort so re-selecting the model in a later
  // spawn dialog re-applies it (per-model memory). Both values are the
  // raw <select> values, "" included.
  rememberModelEffort(model, effort);
  const submitBtn = $('#agent-spawn-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = 'Spawning…';
  // Slop-mode lever yank — the body.slop CSS swaps the button text
  // for "🎰 PULL!" via ::before and listens for this class to play
  // the yank-down animation. Self-removes after the animation so a
  // failed submit retry yanks again on the next click. Harmless in
  // non-slop mode (the class has no styling there).
  submitBtn.classList.add('slop-pull-active');
  setTimeout(() => submitBtn.classList.remove('slop-pull-active'), 700);
  try {
    // Resolve the worktree picker (it targets the "Worktree repo"
    // field, which may differ from CWD). Two outcomes:
    //   • Worktree repo == CWD → the worktree becomes the spawn cwd
    //     (the long-standing single-directory behaviour).
    //   • Worktree repo is a sub-repo of a monorepo CWD → the agent
    //     still launches in CWD; the worktree path + branch ride
    //     along so the daemon's welcome points the agent at it.
    const sel = await wtResolve('agent-spawn', wtRepo);
    const body = { name, role, descr, initial_message: initMsg, auto_focus: autoFocus, include_group_context: includeGroupContext };
    if (effort) body.effort = effort;
    if (model) body.model = model;
    // Send the harness only when it's not the default (Claude Code), so a
    // plain CC spawn body is unchanged; the daemon treats an omitted
    // harness as the default. Send the sandbox only for a harness that
    // takes one (Codex) — the select carries its secure default until the
    // human picks otherwise.
    if (harness && harness !== 'claude') body.harness = harness;
    if (sandbox) body.sandbox = sandbox;
    // Opt-in dir-trust (Codex only): the daemon pre-trusts the cwd by editing
    // ~/.codex/config.toml, so it is sent ONLY when the human explicitly
    // ticked the checkbox — never defaulted.
    if (harness === 'codex' && $('#agent-spawn-trust-dir').checked) body.trust_dir = true;
    if (sel.path && wtRepo && wtRepo !== cwd) {
      body.cwd = cwd;
      body.worktree_path = sel.path;
      body.worktree_branch = sel.branch;
    } else if (sel.path) {
      body.cwd = sel.path;
    } else {
      body.cwd = cwd;
    }
    const r = await fetch(`/api/groups/${encodeURIComponent(group)}/spawn`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    let payload = {};
    try { payload = await r.json(); } catch (_) {}
    closeAgentSpawnModal();
    const label = name || (payload.conv_id ? shortId(payload.conv_id) : 'agent');
    toast(`spawned ${label} → ${group}${autoFocus ? ' — opening terminal' : ''}`);
    // Vegas-themed celebration when slop is on; silent no-op otherwise.
    slopJackpot();
    // Keep the destination group expanded so the new member is visible.
    try { dashPrefs.setItem('tclaude.dash.group.' + group, '1'); } catch (_) {}
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = 'Spawn';
  }
}

function bindAgentSpawnModal() {
  // The spawn modal is opened per-group from each group's
  // "+ spawn agent" button (data-act="spawn-agent"); it has no
  // global open button. Switching the group <select> re-prefills
  // the cwd from the newly-chosen group's default, mirrors it into
  // Worktree-repo (unless the human pinned that), and reloads the
  // picker.
  $('#agent-spawn-group').addEventListener('change', (e) => {
    prefillSpawnCwd(e.target.value, false);
    updateSpawnGroupContextRow(e.target.value);
    updateSpawnModelDefaultLabel(e.target.value);
    if (!spawnWtRepoEdited) $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
    spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
  });
  // Switching the harness reshapes the Model + Sandbox rows for the new
  // harness (and re-applies the remembered effort for whatever Model
  // control becomes active).
  $('#agent-spawn-harness').addEventListener('change', (e) => {
    applySpawnHarness(e.target.value);
  });
  // Picking a different sandbox mode refreshes the live help line (its
  // agentd-reachability caveat changes per mode).
  $('#agent-spawn-sandbox').addEventListener('change', () => {
    applySpawnSandboxHint(spawnHarnessByName($('#agent-spawn-harness').value));
  });
  // Switching the Model re-applies that model's remembered effort (or
  // resets to Default when it has none), so each model carries its own
  // effort default — see rememberModelEffort. Both Model controls (the
  // curated <select> and the Codex free-text <input>) feed it.
  $('#agent-spawn-model').addEventListener('change', (e) => {
    applyRememberedEffort(e.target.value);
  });
  $('#agent-spawn-model-codex').addEventListener('input', (e) => {
    applyRememberedEffort(e.target.value.trim());
  });
  // Load-from-profile selector: picking a profile applies it; "(none)"
  // clears the profile-filled fields back to blank.
  $('#agent-spawn-load-profile').addEventListener('change', async (e) => {
    const name = e.target.value;
    if (!name) { clearSpawnProfileFields(); return; }
    try {
      const p = await getProfile(name);
      if (p) applyProfileToSpawnForm(p);
    } catch (_) { /* fetch error — leave the form as-is */ }
  });
  // Clear resets the profile-controlled fields (leaving group/cwd/worktree).
  $('#agent-spawn-clear').addEventListener('click', clearSpawnProfileFields);
  $('#agent-spawn-cancel').addEventListener('click', closeAgentSpawnModal);
  $('#agent-spawn-submit').addEventListener('click', submitAgentSpawn);
  // Ctrl/Cmd+Enter spawns from anywhere in the dialog (incl. the
  // init-message textarea), so power users needn't mouse to the button.
  bindModalSubmitHotkey($('#agent-spawn-modal'), $('#agent-spawn-submit'));
  bindWtPicker('agent-spawn');
  // Keep every <select> in the modal tooltip-synced (one delegated change
  // listener + an initial pass) so a clipped label stays readable on hover.
  bindSelectTitles($('#agent-spawn-modal'));
  // Remember the dialog's dragged size (the .cron-create-modal card carries
  // the resize grip) across reopens and restarts.
  makeModalResizable($('#agent-spawn-modal .cron-create-modal'), 'tclaude.dash.modalSize.agent-spawn');
  // Name-sync wiring: typing in the name mirrors into the
  // worktree branch; toggling the checkbox re-applies the sync;
  // hand-editing the branch or picking a worktree by hand turns the
  // sync off so it stops fighting the human.
  $('#agent-spawn-name').addEventListener('input', applyWtSync);
  $('#agent-spawn-wt-sync').addEventListener('change', applyWtSync);
  $('#agent-spawn-wt-branch').addEventListener('input', () => {
    $('#agent-spawn-wt-sync').checked = false;
  });
  $('#agent-spawn-worktree').addEventListener('change', (e) => {
    if (e.target.value !== WT_NEW) $('#agent-spawn-wt-sync').checked = false;
  });
  // Re-list worktrees when the CWD field settles (debounced). CWD
  // mirrors into Worktree-repo until the human edits the latter.
  let spawnCwdTimer;
  $('#agent-spawn-cwd').addEventListener('input', () => {
    clearTimeout(spawnCwdTimer);
    spawnCwdTimer = setTimeout(() => {
      if (!spawnWtRepoEdited) $('#agent-spawn-wt-repo').value = $('#agent-spawn-cwd').value;
      spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
    }, 350);
  });
  // Editing "Worktree repo" detaches it from CWD and reloads the
  // picker against the typed/picked repo (e.g. a monorepo sub-repo).
  let spawnWtRepoTimer;
  $('#agent-spawn-wt-repo').addEventListener('input', () => {
    spawnWtRepoEdited = true;
    clearTimeout(spawnWtRepoTimer);
    spawnWtRepoTimer = setTimeout(() => {
      spawnWtLoad($('#agent-spawn-wt-repo').value.trim());
    }, 350);
  });
  bindBackdropDiscard('agent-spawn-modal', closeAgentSpawnModal);
}

// ---- Clone agent modal --------------------------------------------------
//
// Submit POSTs to /api/agents/{conv}/clone with `{follow_up, no_copy_conv}`.
// Follow-up is optional; newlines are stripped client-side because the
// server rejects them (tmux send-keys would split them into multiple
// submits).

function openCloneAgentModal(conv, label, cwd) {
  cwd = cwd || '';
  const meta = $('#clone-agent-meta');
  const src = label || shortId(conv);
  meta.textContent = cwd ? `source: ${src}  ·  ${cwd}` : `source: ${src}`;
  $('#clone-agent-followup').value = '';
  $('#clone-agent-copy-conv').checked = true;
  $('#clone-agent-wt-branch').value = '';
  $('#clone-agent-error').textContent = '';
  $('#clone-agent-modal').dataset.conv = conv;
  $('#clone-agent-modal').dataset.label = label || '';
  $('#clone-agent-modal').dataset.cwd = cwd;
  // The picker lists worktrees of the source agent's repo; "+ create"
  // forks a new one and the clone spawns there.
  wtLoad('clone-agent', cwd, '(no worktree — same directory as source)');
  $('#clone-agent-modal').classList.add('show');
  setTimeout(() => $('#clone-agent-followup').focus(), 0);
}

function closeCloneAgentModal() {
  $('#clone-agent-modal').classList.remove('show');
}

// normaliseFollowUp collapses newlines/tabs/runs-of-whitespace to a
// single space and trims. Server rejects newlines outright; this
// keeps the textarea ergonomic while staying safe.
function normaliseFollowUp(s) {
  return String(s || '').replace(/[\r\n\t]+/g, ' ').replace(/\s+/g, ' ').trim();
}

// Server's spawn poll is 30 s (reincarnateSpawnTimeout in clone.go).
// Give a small grace window before the UI surfaces a timeout so a
// just-barely-late response is treated as success, not error.
const CLONE_FETCH_TIMEOUT_MS = 35_000;

async function submitCloneAgent() {
  const modal = $('#clone-agent-modal');
  const conv = modal.dataset.conv;
  const label = modal.dataset.label || shortId(conv);
  const followUp = normaliseFollowUp($('#clone-agent-followup').value);
  const copyConv = $('#clone-agent-copy-conv').checked;
  const errEl = $('#clone-agent-error');
  errEl.textContent = '';
  const submitBtn = $('#clone-agent-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = 'Cloning…';
  // AbortController gives us a clean "the server is hung" path instead
  // of leaving the modal in 'Cloning…' until the browser's default
  // network timeout (which can be minutes). The window is generous
  // because the server itself polls up to 30 s for the new tmux
  // session to register.
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), CLONE_FETCH_TIMEOUT_MS);
  try {
    // Resolve the worktree picker → optional cwd override. An empty
    // result means "inherit the source's cwd" (historical behaviour).
    const cwd = await wtResolveCwd('clone-agent', modal.dataset.cwd || '', '');
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/clone`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ follow_up: followUp, no_copy_conv: !copyConv, cwd }),
      signal: ctrl.signal,
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    let payload = {};
    try { payload = await r.json(); } catch (_) {}
    closeCloneAgentModal();
    const dst = payload.new_conv ? ' → ' + shortId(payload.new_conv) : '';
    if (payload.warning) {
      // Server returned 200 but flagged a partial-success — keep the
      // user informed instead of silently celebrating; isErr=true
      // styles the toast as a warning.
      toast(`cloned ${label}${dst} (warning: ${payload.warning})`, true);
    } else {
      toast(`cloned ${label}${dst}`);
    }
    refresh();
  } catch (err) {
    if (err && err.name === 'AbortError') {
      errEl.textContent = `clone timed out after ${CLONE_FETCH_TIMEOUT_MS / 1000}s — the new agent may still come online; check ~/.tclaude/output.log and refresh in a moment.`;
    } else {
      errEl.textContent = (err && err.message) || String(err);
    }
  } finally {
    clearTimeout(timer);
    submitBtn.disabled = false;
    submitBtn.textContent = 'Clone';
  }
}

function bindCloneAgentModal() {
  $('#clone-agent-cancel').addEventListener('click', closeCloneAgentModal);
  $('#clone-agent-submit').addEventListener('click', submitCloneAgent);
  // Ctrl/Cmd+Enter clones — same hotkey as the spawn dialog.
  bindModalSubmitHotkey($('#clone-agent-modal'), $('#clone-agent-submit'));
  bindWtPicker('clone-agent');
  bindSelectTitles($('#clone-agent-modal'));
  makeModalResizable($('#clone-agent-modal .cron-create-modal'), 'tclaude.dash.modalSize.clone-agent');
  bindBackdropDiscard('clone-agent-modal', closeCloneAgentModal);
}

// ---- Reincarnate agent modal --------------------------------------------
//
// Two modes, chosen by the radiogroup; both POST to
// /api/agents/{conv}/reincarnate:
//   - "self" (the DEFAULT): POST {mode:'self', focus_hint?} — the
//     daemon messages the agent to reincarnate itself. focus_hint is
//     OPTIONAL, so Submit is always enabled.
//   - "force": POST {mode:'force', follow_up} — the immediate
//     daemon-driven reincarnation. follow_up is REQUIRED, so Submit
//     is disabled until the follow-up textarea has content.

function reincarnateMode() {
  const checked = $('input[name=reincarnate-mode]:checked');
  return (checked && checked.value) || 'self';
}

// updateReincarnateMode shows the fields for the selected mode,
// relabels Submit, and recomputes its disabled state. Self-mode's
// Submit is always enabled (the focus hint is optional); force-mode's
// is gated on a non-empty follow-up.
function updateReincarnateMode() {
  const isForce = reincarnateMode() === 'force';
  $('#reincarnate-self-fields').hidden = isForce;
  $('#reincarnate-force-fields').hidden = !isForce;
  const submitBtn = $('#reincarnate-agent-submit');
  submitBtn.textContent = isForce ? 'Force reincarnate' : 'Ask agent';
  submitBtn.disabled = isForce && !normaliseFollowUp($('#reincarnate-agent-followup').value);
}

function openReincarnateAgentModal(conv, label) {
  const meta = $('#reincarnate-agent-meta');
  meta.textContent = label ? `target: ${label}` : `target: ${shortId(conv)}`;
  $('#reincarnate-agent-followup').value = '';
  $('#reincarnate-agent-focus').value = '';
  $('#reincarnate-agent-error').textContent = '';
  // Every open resets to the self-reincarnate default.
  const selfRadio = $('input[name=reincarnate-mode][value=self]');
  if (selfRadio) selfRadio.checked = true;
  updateReincarnateMode();
  $('#reincarnate-agent-modal').dataset.conv = conv;
  $('#reincarnate-agent-modal').dataset.label = label || '';
  $('#reincarnate-agent-modal').classList.add('show');
  setTimeout(() => $('#reincarnate-agent-focus').focus(), 0);
}

function closeReincarnateAgentModal() {
  $('#reincarnate-agent-modal').classList.remove('show');
}

async function submitReincarnateAgent() {
  const modal = $('#reincarnate-agent-modal');
  const conv = modal.dataset.conv;
  const label = modal.dataset.label || shortId(conv);
  const errEl = $('#reincarnate-agent-error');
  errEl.textContent = '';
  const mode = reincarnateMode();
  let body;
  if (mode === 'force') {
    const followUp = normaliseFollowUp($('#reincarnate-agent-followup').value);
    if (!followUp) {
      errEl.textContent = 'follow-up is required for force reincarnate';
      return;
    }
    body = { mode: 'force', follow_up: followUp };
  } else {
    // Focus hint is optional — send it trimmed, or omit when blank.
    const hint = $('#reincarnate-agent-focus').value.trim();
    body = { mode: 'self' };
    if (hint) body.focus_hint = hint;
  }
  const submitBtn = $('#reincarnate-agent-submit');
  submitBtn.disabled = true;
  submitBtn.textContent = mode === 'force' ? 'Reincarnating…' : 'Asking…';
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(conv)}/reincarnate`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      errEl.textContent = (await r.text()) || `HTTP ${r.status}`;
      return;
    }
    let payload = {};
    try { payload = await r.json(); } catch (_) {}
    closeReincarnateAgentModal();
    if (mode === 'force') {
      const suffix = payload.new_title ? ' → ' + payload.new_title : (payload.new_conv ? ' → ' + shortId(payload.new_conv) : '');
      toast(`reincarnated ${label}${suffix}`);
    } else {
      toast(`asked ${label} to reincarnate itself`);
    }
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    // Recompute label + disabled state for whatever mode is selected
    // (relevant only on the error path — success closed the modal).
    updateReincarnateMode();
  }
}

function bindReincarnateAgentModal() {
  $('#reincarnate-agent-cancel').addEventListener('click', closeReincarnateAgentModal);
  $('#reincarnate-agent-submit').addEventListener('click', submitReincarnateAgent);
  // Ctrl/Cmd+Enter submits — same hotkey as the spawn dialog. The button
  // is disabled in force-mode until a follow-up is typed, and the helper
  // no-ops on a disabled button, so the hotkey honours that gate too.
  bindModalSubmitHotkey($('#reincarnate-agent-modal'), $('#reincarnate-agent-submit'));
  $('#reincarnate-agent-followup').addEventListener('input', updateReincarnateMode);
  $$('input[name=reincarnate-mode]').forEach(rdo => {
    rdo.addEventListener('change', () => {
      updateReincarnateMode();
      const focusEl = reincarnateMode() === 'force'
        ? $('#reincarnate-agent-followup') : $('#reincarnate-agent-focus');
      setTimeout(() => focusEl.focus(), 0);
    });
  });
  bindBackdropDiscard('reincarnate-agent-modal', closeReincarnateAgentModal);
}

// Renaming an agent is no longer a modal of its own — it folded into
// the per-agent edit panel (editMemberModal, refresh.js) and the
// click-to-edit name cell (the rename-name handler, row-actions.js).
// Both POST /api/agents/{conv}/rename, same as this modal once did.

export {
  openAgentSpawnModal, bindAgentSpawnModal,
  openCloneAgentModal, bindCloneAgentModal,
  openReincarnateAgentModal, bindReincarnateAgentModal,
};
