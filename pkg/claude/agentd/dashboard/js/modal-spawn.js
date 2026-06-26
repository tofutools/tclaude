// modal-spawn.js — the spawn / clone / reincarnate agent modals.
//
// Extracted from dashboard.js in the Stage 2 module split. The spawn and
// clone modals embed the worktree picker from modal-link-wt.

import { $, $$, esc, shortId, syncSelectTitle, bindSelectTitles, makeModalResizable, bindModalSubmitHotkey, showModalError, pickDirectory } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { loadProfiles, getProfile, getDashDefaultProfile } from './profiles.js';
import { openProfileEditor } from './modal-profiles.js';
import { groupDefaultContext } from './modal-templates.js';
import {
  WT_NEW, wtToggleNew, wtLoad, bindWtPicker, wtResolve, wtResolveCwd,
} from './modal-link-wt.js';
// lastSnapshot lives in dashboard.js; refresh() / toast in refresh.js.
// Imported back — benign cycles (see render.js); TDZ-safe.
import { lastSnapshot } from './dashboard.js';
import { refresh, toast, bindBackdropDiscard, confirmModal } from './refresh.js';
import { slopJackpot } from './slop-fx.js';
import { openTermModal } from './modal-term.js';
import { recordGroupInteraction } from './last-group.js';


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
// instead of an opaque "Default". The group's spawn-model default now comes
// from its default spawn profile (JOH-210; the per-group default_model was
// retired in JOH-220), so it resolves the group's default profile and reads
// its model; failing that, the daemon falls back to the user-level
// settings.json model, then to claude's own default. Async because the
// profile lookup may fetch — fire-and-forget; the label updates when it
// settles. Called on modal open and whenever the group <select> changes.
async function updateSpawnModelDefaultLabel(groupName) {
  const opt = $('#agent-spawn-model').querySelector('option[value=""]');
  if (!opt) return;
  const userModel = (lastSnapshot && lastSnapshot.user_default_model) || '';
  // Resolve the group's default profile to its model. Only a claude-harness
  // profile's model belongs in this (claude) Model dropdown's label — a
  // codex profile's model isn't a value this control offers.
  let groupModel = '';
  const profileName = groupDefaultProfileName(groupName);
  if (profileName) {
    try {
      const p = await getProfile(profileName);
      if (p && p.model && (!p.harness || p.harness === 'claude')) groupModel = p.model;
    } catch (_) { /* lookup failed — fall through to user/claude default */ }
    // The await yielded; bail if the group selection moved on under us.
    const sel = $('#agent-spawn-group');
    if (sel && sel.value !== groupName) return;
  }
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

  // Remote-control opt-in (JOH-258): "start with Remote Access on", gated on the
  // harness having built-in Remote Access (Claude Code's can_remote_control) —
  // hidden for a harness without it (Codex). Fail-open to shown when no catalog
  // entry yet (snapshot not loaded), matching the default Claude-Code layout
  // above. Off by default; hiding it for an unsupported harness also clears it
  // so the choice can't leak across a harness switch.
  const canRemoteControl = h ? !!h.can_remote_control : true;
  $('#agent-spawn-remote-control-row').style.display = canRemoteControl ? '' : 'none';
  if (!canRemoteControl) $('#agent-spawn-remote-control').checked = false;

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

// ---- Spawn name normalization (config agent.spawn_name_normalize) -------
//
// A spawn name doubles as a git worktree branch token and the conversation
// title, so it is restricted to [A-Za-z0-9_-]. Rather than reject a name
// that strays outside that set, the default behaviour auto-normalizes it
// (e.g. "code reviewer!" → "code-reviewer") so any typed name works. The
// three constants/functions below mirror the Go side (agent.MaxSpawnNameLen,
// isValidSpawnName, agent.NormalizeSpawnName) so the live preview matches
// exactly what the daemon would create; the daemon re-normalizes as the
// authoritative backstop.

const MAX_SPAWN_NAME_LEN = 64;

// SPAWN_NAME_VALID matches the daemon's isValidSpawnName charset gate. An
// empty name is separately allowed (the agent gets an auto-generated label),
// so callers test `name && !SPAWN_NAME_VALID.test(name)`.
const SPAWN_NAME_VALID = /^[A-Za-z0-9_-]{1,64}$/;

// spawnNameNormalizeEnabled reflects config agent.spawn_name_normalize
// (default on) from the latest snapshot. Undefined (snapshot not loaded yet
// / older daemon) defaults ON to match the Go default — the modal auto-fixes
// rather than rejecting.
function spawnNameNormalizeEnabled() {
  return !lastSnapshot || lastSnapshot.spawn_name_normalize !== false;
}

// normalizeSpawnName mirrors agent.NormalizeSpawnName (Go): collapse every
// run of disallowed characters to a single '-', trim leading/trailing '-',
// cap at MAX_SPAWN_NAME_LEN, and re-trim a trailing '-' a mid-run cut leaves.
// `for…of` iterates by code point, matching Go's rune loop. The output is
// all-ASCII so .length == char count == the Go byte cap. Idempotent; an
// all-invalid input yields "".
function normalizeSpawnName(name) {
  let out = '';
  let prevSep = false;
  for (const ch of name) {
    if (/[A-Za-z0-9_-]/.test(ch)) {
      out += ch;
      prevSep = false;
    } else if (!prevSep) {
      out += '-';
      prevSep = true;
    }
  }
  out = out.replace(/^-+/, '').replace(/-+$/, '');
  if (out.length > MAX_SPAWN_NAME_LEN) {
    out = out.slice(0, MAX_SPAWN_NAME_LEN).replace(/-+$/, '');
  }
  return out;
}

// updateSpawnNameHint shows a live preview under the Name field: when the
// typed name carries forbidden characters it either previews the normalized
// result (normalization on) or warns it'll be rejected (off). Purely
// advisory — it never rewrites the field (commitSpawnName does that on blur /
// submit), so typing stays jank-free. The .spawn-field-hint :empty rule
// hides it when there's nothing to say.
function updateSpawnNameHint() {
  const hintEl = $('#agent-spawn-name-hint');
  if (!hintEl) return;
  const raw = $('#agent-spawn-name').value.trim();
  hintEl.classList.remove('warn');
  if (!raw || SPAWN_NAME_VALID.test(raw)) {
    hintEl.textContent = '';
    return;
  }
  if (spawnNameNormalizeEnabled()) {
    const norm = normalizeSpawnName(raw);
    hintEl.textContent = norm
      ? `will be created as “${norm}”`
      : 'no usable characters — the agent will get an auto-generated name';
  } else {
    hintEl.classList.add('warn');
    hintEl.textContent = 'invalid — use only letters, digits, underscore and dash (max 64 chars)';
  }
}

// commitSpawnName applies the normalized name back into the Name field (and
// re-runs the name→branch sync so the worktree branch follows the fixed
// name). Called on the field's blur and from submit. A no-op when the name
// is already valid/empty or normalization is off — in the off case submit
// reports the inline error instead.
function commitSpawnName() {
  if (!spawnNameNormalizeEnabled()) return;
  const el = $('#agent-spawn-name');
  const raw = el.value.trim();
  if (!raw || SPAWN_NAME_VALID.test(raw)) return;
  el.value = normalizeSpawnName(raw);
  applyWtSync();
  updateSpawnNameHint();
}

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

// groupRemoteControlPolicy reads a group's remote-control policy from the latest
// snapshot — 'optin' | 'deny' | 'inherit'. "" when the group is unknown.
function groupRemoteControlPolicy(groupName) {
  const groups = (lastSnapshot && lastSnapshot.groups) || [];
  const g = groups.find(x => x.name === groupName);
  return (g && g.remote_control_policy) || '';
}

// applyRemoteControlPrefill (JOH-262 revised) pre-fills the "start with Remote
// Access on" checkbox from the spawn defaults, highest-priority first:
//   group policy (optin → on, deny → off)  >  picked profile's default  >  off
// The checkbox is then the AUTHORITATIVE per-spawn intent — submit always sends
// its state for a Remote-Access-capable harness, and the daemon honours it over
// the group/profile default. So this only seeds the form; the human can still
// toggle it and whatever they leave decides the spawn. Mirrors applySpawnHarness:
// for a harness with no Remote Access (Codex) the row is hidden and the box stays
// cleared, so we leave it off and bail. `profile` may be null (no profile picked).
function applyRemoteControlPrefill(groupName, profile) {
  const h = spawnHarnessByName($('#agent-spawn-harness').value);
  const canRemoteControl = h ? !!h.can_remote_control : true;
  if (!canRemoteControl) { $('#agent-spawn-remote-control').checked = false; return; }
  const policy = groupRemoteControlPolicy(groupName);
  let on;
  if (policy === 'optin') on = true;
  else if (policy === 'deny') on = false;
  else if (profile && profile.remote_control != null) on = !!profile.remote_control;
  else on = false;
  $('#agent-spawn-remote-control').checked = on;
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

  // remote_control is a *bool too (JOH-262): pre-fill the spawn dialog's Remote
  // Access box, with the group's remote-control policy (optin/deny) taking
  // precedence over this profile's own default. applySpawnHarness ran above, so
  // the box is already cleared+hidden for a harness with no Remote Access; the
  // helper re-checks that and no-ops there. The box is the authoritative intent —
  // whatever it shows after this decides the spawn (submit always sends it).
  applyRemoteControlPrefill($('#agent-spawn-group').value, p);

  if (p.agent_name) $('#agent-spawn-name').value = p.agent_name;
  if (p.role) $('#agent-spawn-role').value = p.role;
  if (p.descr) $('#agent-spawn-descr').value = p.descr;
  if (p.initial_message) $('#agent-spawn-init-msg').value = p.initial_message;
  if (p.auto_focus != null) $('#agent-spawn-focus').checked = !!p.auto_focus;
  if (p.include_group_default_context != null) {
    $('#agent-spawn-group-context').checked = !!p.include_group_default_context;
  }
  // The name may have changed → re-sync the worktree branch name + the
  // normalize preview.
  applyWtSync();
  updateSpawnNameHint();
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
  // Reset Remote Access to the group-policy default (no profile picked now).
  applyRemoteControlPrefill($('#agent-spawn-group').value, null);
  $('#agent-spawn-load-profile').value = '';
  syncSelectTitle($('#agent-spawn-load-profile'));
  applyWtSync();
  updateSpawnNameHint();
}

// populateSpawnProfileOptions rebuilds the Profile selector's <option> list
// from `profiles` and selects `selected` (when it's in the list) — WITHOUT
// applying it to the form. Shared by the open-time pre-fill and the "Save as
// profile" refresh.
function populateSpawnProfileOptions(profiles, selected) {
  const sel = $('#agent-spawn-load-profile');
  sel.innerHTML = `<option value="">— none (blank form) —</option>`
    + profiles.map(p => `<option value="${esc(p.name)}">${esc(p.name)}</option>`).join('');
  sel.value = (selected && profiles.some(p => p.name === selected)) ? selected : '';
  syncSelectTitle(sel);
}

// initSpawnProfileSelector populates the Profile selector from the saved
// profiles and applies the pre-fill for `groupName`: the group's own default
// profile, else the dashboard default. Runs async (the list is fetched), so it
// guards against the modal being closed or its group switched out from under a
// slow fetch before it touches the form.
async function initSpawnProfileSelector(groupName) {
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
  populateSpawnProfileOptions(profiles, prefill);
  if (prefill && profiles.some(p => p.name === prefill)) {
    applyProfileToSpawnForm(profiles.find(p => p.name === prefill));
  }
}

// spawnFormAsProfileSeed snapshots the dialog's current field values into a
// profile-shaped object for "Save as profile" — only the profile-storable
// fields (cwd / worktree are per-spawn and never stored). Harness-gated fields
// (sandbox / trust-dir) are included only for a harness that takes them, so the
// editor doesn't seed a value its harness would reject. The *bool fields come
// straight off the dialog's checkboxes (concrete on/off, not "unset").
function spawnFormAsProfileSeed() {
  const harness = $('#agent-spawn-harness').value;
  const hEntry = spawnHarnessByName(harness);
  const seed = {
    harness,
    model: activeSpawnModelEl().value.trim(),
    effort: $('#agent-spawn-effort').value,
    agent_name: $('#agent-spawn-name').value.trim(),
    role: $('#agent-spawn-role').value.trim(),
    descr: $('#agent-spawn-descr').value.trim(),
    initial_message: $('#agent-spawn-init-msg').value,
    auto_focus: $('#agent-spawn-focus').checked,
    sync_worktree: $('#agent-spawn-wt-sync').checked,
    include_group_default_context: $('#agent-spawn-group-context').checked,
  };
  if (hEntry && hEntry.can_sandbox) seed.sandbox = $('#agent-spawn-sandbox').value;
  if (harness === 'codex') seed.trust_dir = $('#agent-spawn-trust-dir').checked;
  return seed;
}

// ---- File / screenshot attachments --------------------------------------
//
// The spawn dialog can carry files for the new agent: chosen with the native
// picker, or pasted as screenshots from the clipboard (⌘/Ctrl-V anywhere in
// the modal — an image item is packaged as a PNG File). They're held client-
// side until submit, then uploaded to /api/spawn-attachments (which writes
// them to a temp dir) and the returned paths ride along in the spawn body as
// `attachments`; the daemon lists them in the new agent's startup briefing.
//
// Each entry is { id, file (Blob), name, size, url } where url is an object
// URL for an image preview (revoked on remove/clear), or '' for a non-image.
let spawnAttachments = [];
let spawnAttachSeq = 0;

// fmtAttachSize renders a byte count as a short human string for the list.
function fmtAttachSize(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10 * 1024 ? 1 : 0) + ' KB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}

// addSpawnAttachments appends Files/Blobs to the pending list and re-renders.
// A blob with no name (a raw pasted image) is given a generated PNG name.
function addSpawnAttachments(files) {
  for (const f of files) {
    if (!f) continue;
    let name = f.name;
    if (!name) {
      const ext = (f.type && f.type.split('/')[1]) || 'png';
      name = `pasted-${++spawnAttachSeq}.${ext}`;
    }
    const isImage = (f.type || '').startsWith('image/');
    spawnAttachments.push({
      id: ++spawnAttachSeq,
      file: f,
      name,
      size: f.size,
      url: isImage ? URL.createObjectURL(f) : '',
    });
  }
  renderSpawnAttachments();
}

// removeSpawnAttachment drops one entry by id, revoking its preview URL.
function removeSpawnAttachment(id) {
  const i = spawnAttachments.findIndex(a => a.id === id);
  if (i < 0) return;
  if (spawnAttachments[i].url) URL.revokeObjectURL(spawnAttachments[i].url);
  spawnAttachments.splice(i, 1);
  renderSpawnAttachments();
}

// clearSpawnAttachments empties the list and revokes every preview URL. Called
// on modal open and close so attachments never leak across spawns.
function clearSpawnAttachments() {
  for (const a of spawnAttachments) {
    if (a.url) URL.revokeObjectURL(a.url);
  }
  spawnAttachments = [];
  renderSpawnAttachments();
}

// renderSpawnAttachments repaints the list. Each row: thumbnail (image) or a
// 📄 icon, the name, the size, and a × remove button (id on the dataset; a
// delegated listener bound in bindAgentSpawnModal handles the click).
function renderSpawnAttachments() {
  const list = $('#agent-spawn-attachments-list');
  if (!list) return;
  list.innerHTML = spawnAttachments.map(a => {
    const thumb = a.url
      ? `<img class="att-thumb" src="${esc(a.url)}" alt="" />`
      : `<span class="att-icon">📄</span>`;
    return `<li>${thumb}`
      + `<span class="att-name" title="${esc(a.name)}">${esc(a.name)}</span>`
      + `<span class="att-size">${esc(fmtAttachSize(a.size))}</span>`
      + `<button type="button" class="att-remove" data-att-id="${a.id}" title="Remove" aria-label="Remove ${esc(a.name)}">✕</button>`
      + `</li>`;
  }).join('');
}

// handleSpawnPaste captures files pasted anywhere in the dialog: a screenshot
// taken to the clipboard ("⌘V" of raw image data) AND a file copied in Finder /
// Explorer ("⌘C" on a file, then ⌘V). It reads both clipboard surfaces and
// dedupes — .files carries Finder file copies and image files; .items carries
// raw bitmaps that some browsers don't expose in .files. A plain text paste
// (into the init-message textarea) has no file entries, so it's left untouched.
function handleSpawnPaste(e) {
  const dt = e.clipboardData;
  if (!dt) return;
  const collected = [];
  const seen = new Set();
  const add = (f) => {
    if (!f) return;
    const key = `${f.name}|${f.size}|${f.type}|${f.lastModified || ''}`;
    if (seen.has(key)) return;
    seen.add(key);
    collected.push(f);
  };
  if (dt.files) {
    for (let i = 0; i < dt.files.length; i++) add(dt.files[i]);
  }
  // DataTransferItemList isn't reliably for...of-iterable across browsers —
  // index into it.
  if (dt.items) {
    for (let i = 0; i < dt.items.length; i++) {
      if (dt.items[i].kind === 'file') add(dt.items[i].getAsFile());
    }
  }
  if (!collected.length) return;
  // We consumed file data — stop the default so a contenteditable / textarea
  // doesn't also try to handle it.
  e.preventDefault();
  addSpawnAttachments(collected);
}

// bindSpawnDragDrop wires Finder/Explorer drag-and-drop onto the spawn dialog.
// The handlers sit on the full-screen overlay (so a drop anywhere in the open
// dialog is captured — and can't fall through to the browser's default of
// navigating to the dropped file), and a dashed highlight on the card signals
// the drop target. A depth counter rides the dragenter/dragleave pair so moving
// the cursor across child elements doesn't flicker the highlight off.
let spawnDragDepth = 0;
function bindSpawnDragDrop() {
  const overlay = $('#agent-spawn-modal');
  const card = $('#agent-spawn-modal .cron-create-modal');
  // dataTransfer.types is an Array in modern browsers, a DOMStringList in older
  // ones — indexOf via the Array prototype handles both.
  const hasFiles = (e) => {
    const t = e.dataTransfer && e.dataTransfer.types;
    return !!t && Array.prototype.indexOf.call(t, 'Files') !== -1;
  };
  const clear = () => { spawnDragDepth = 0; card.classList.remove('spawn-drag-over'); };
  overlay.addEventListener('dragenter', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    spawnDragDepth++;
    card.classList.add('spawn-drag-over');
  });
  overlay.addEventListener('dragover', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  overlay.addEventListener('dragleave', (e) => {
    if (!hasFiles(e)) return;
    spawnDragDepth = Math.max(0, spawnDragDepth - 1);
    if (spawnDragDepth === 0) card.classList.remove('spawn-drag-over');
  });
  overlay.addEventListener('drop', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault(); // stop the browser from opening the dropped file
    clear();
    const files = e.dataTransfer.files;
    if (files && files.length) addSpawnAttachments(files);
  });
}

// uploadSpawnAttachments POSTs the pending files to /api/spawn-attachments and
// returns the stored absolute paths. Returns [] when there are none. Throws on
// a non-OK response so submit can surface the error and abort the spawn.
async function uploadSpawnAttachments() {
  if (!spawnAttachments.length) return [];
  const fd = new FormData();
  for (const a of spawnAttachments) {
    fd.append('file', a.file, a.name);
  }
  const r = await fetch('/api/spawn-attachments', {
    method: 'POST', credentials: 'same-origin', body: fd,
  });
  if (!r.ok) {
    throw new Error('attachment upload failed: ' + ((await r.text()) || `HTTP ${r.status}`));
  }
  const payload = await r.json();
  return (payload.files || []).map(f => f.path);
}

// openAgentSpawnModal opens the spawn dialog. opts:
//   • groupName    — pin to this group: the <select> is set and HIDDEN
//                    (the per-group "+ spawn agent" buttons and the
//                    palette's "Spawn agent in <group>…" commands).
//   • defaultGroup — preselect this group but keep the <select> VISIBLE
//                    so it can still be changed (the palette's plain
//                    "Spawn agent…", which defaults into the last group
//                    the operator touched but doesn't force it).
// groupName wins when both are set. Neither → the picker defaults to the
// first group, as before.
function openAgentSpawnModal(opts) {
  const groupName = (opts && opts.groupName) || '';
  const defaultGroup = (opts && opts.defaultGroup) || '';
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
    // Preselect defaultGroup when it's a live option; otherwise fall back
    // to the first group (the long-standing default).
    if (defaultGroup && [...select.options].some(o => o.value === defaultGroup)) {
      select.value = defaultGroup;
    } else if (!select.value && groups.length) {
      select.value = groups[0].name;
    }
  }
  $('#agent-spawn-name').value = '';
  $('#agent-spawn-role').value = '';
  $('#agent-spawn-descr').value = '';
  $('#agent-spawn-init-msg').value = '';
  $('#agent-spawn-model').value = '';
  $('#agent-spawn-model-codex').value = '';
  // Attachments are per-spawn (like cwd/worktree, not a profile field) — start
  // every open with an empty list and any prior preview URLs revoked.
  clearSpawnAttachments();
  $('#agent-spawn-attach-input').value = '';
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
  // Pre-fill Remote Access from the group's remote-control policy (the picked
  // profile's default is layered on later by initSpawnProfileSelector, which
  // wins via applyRemoteControlPrefill when the policy is "inherit"). The box is
  // then the authoritative per-spawn intent — whatever it shows decides the spawn
  // (JOH-262 revised). applySpawnHarness ran above, so it's already cleared+hidden
  // for a harness with no Remote Access. No profile loaded yet → pass null.
  applyRemoteControlPrefill(select.value, null);
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
  showModalError('agent-spawn-error', '');
  const meta = $('#agent-spawn-meta');
  if (groupName) {
    meta.textContent = `joining group: ${groupName}`;
    meta.style.display = '';
  } else {
    meta.style.display = 'none';
  }
  $('#agent-spawn-modal').classList.add('show');
  // Clear any stale normalize preview from a prior open (the name field was
  // reset above); a profile apply below refreshes it if it sets a name.
  updateSpawnNameHint();
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
  // Drop any pending attachments + revoke their preview URLs so a cancelled
  // dialog doesn't leak object URLs or carry files into the next open.
  clearSpawnAttachments();
}

async function submitAgentSpawn() {
  const group = $('#agent-spawn-group').value;
  let name = $('#agent-spawn-name').value.trim();
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
  showModalError(errEl, '');
  if (!group) {
    showModalError(errEl, 'group is required');
    return;
  }
  // Handle an invalid name client-side instead of a round-trip 400. An empty
  // name is fine (the agent gets an auto-generated label); a non-empty one
  // must be a safe token — the name doubles as a git worktree branch name and
  // becomes the conversation title, so only [A-Za-z0-9_-], 1–64 chars, are
  // allowed. When config's agent.spawn_name_normalize is on (the default) we
  // auto-fix the name to that charset (and re-sync the worktree branch + the
  // success label to match); off, we reject with the inline error. The daemon
  // re-normalizes/re-validates as the authoritative backstop.
  if (name && !SPAWN_NAME_VALID.test(name)) {
    if (spawnNameNormalizeEnabled()) {
      name = normalizeSpawnName(name);
      $('#agent-spawn-name').value = name;
      applyWtSync();
      updateSpawnNameHint();
    } else {
      showModalError(errEl, 'name may use only letters, digits, underscore and dash (max 64 chars)');
      return;
    }
  }
  // Require a name OR an initial description so the agent is identifiable.
  // With both blank the agent would get an auto-generated label — usually a
  // slip where the human typed only an initial message and forgot the name.
  // Pop the shared confirm overlay (z-index 1000, so it stacks on top of this
  // still-open modal) before going through with it. Its Esc / Cancel resolves
  // false WITHOUT closing the spawn modal — so a cancel lands the human right
  // back here with their fields intact to add a name/description and resubmit.
  if (!name && !descr) {
    const proceed = await confirmModal({
      title: 'Spawn without a name?',
      body: 'No agent name or description was given, so this agent will get an auto-generated name. Add a name or description, or spawn anyway?',
      okLabel: 'Spawn anyway',
    });
    if (!proceed) {
      // Drop focus on the Name field so a quick correction needs no mouse.
      $('#agent-spawn-name').focus();
      return;
    }
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
    // Upload any attached files / pasted screenshots before the spawn POST.
    // uploadSpawnAttachments writes them to a temp dir and returns the stored
    // paths; a failure throws and lands in the catch below (error line + button
    // re-enabled), so a botched upload never silently spawns an agent that's
    // told about files that aren't there.
    const attachmentPaths = await uploadSpawnAttachments();
    const body = { name, role, descr, initial_message: initMsg, auto_focus: autoFocus, include_group_context: includeGroupContext };
    if (attachmentPaths.length) body.attachments = attachmentPaths;
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
    // Remote-control (Claude Code only): the checkbox is the authoritative
    // per-spawn intent (JOH-262 revised), so send its state explicitly — true OR
    // false — and the daemon honours it over the group/profile default. Sent only
    // for a harness with built-in Remote Access; applySpawnHarness hides + clears
    // the box for a harness without it (Codex), so we omit it there and let the
    // daemon's policy stack resolve+clamp. The daemon re-gates an explicit true on
    // an unsupported harness (400) as defence in depth (JOH-258).
    if (harnessEntry && harnessEntry.can_remote_control) {
      body.remote_control = $('#agent-spawn-remote-control').checked;
    }
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
      showModalError(errEl, (await r.text()) || `HTTP ${r.status}`);
      return;
    }
    let payload = {};
    try { payload = await r.json(); } catch (_) {}
    closeAgentSpawnModal();
    const label = name || (payload.conv_id ? shortId(payload.conv_id) : 'agent');
    if (autoFocus && payload.focus_mode === 'browser' && payload.focus_ws) {
      // executeSpawn couldn't pop a native terminal window (headless
      // agentd, or no terminal emulator installed) — open the same
      // in-browser fallback the "open window" row action uses, rather
      // than claiming "opening terminal" and opening nothing.
      openTermModal({ wsPath: payload.focus_ws, label: payload.label || label });
      toast(`spawned ${label} → ${group} — opened in-browser terminal`);
    } else {
      toast(`spawned ${label} → ${group}${autoFocus ? ' — opening terminal' : ''}`);
    }
    // Vegas-themed celebration when slop is on; silent no-op otherwise.
    slopJackpot();
    // Keep the destination group expanded so the new member is visible.
    try { dashPrefs.setItem('tclaude.dash.group.' + group, '1'); } catch (_) {}
    // Remember it as the last group touched so the palette's plain "Spawn
    // agent…" defaults here next time.
    recordGroupInteraction(group);
    refresh();
  } catch (err) {
    showModalError(errEl, (err && err.message) || String(err));
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
    // Re-derive the Remote Access checkbox from the new group's policy. Unlike
    // the profile pre-fill below, this IS safe to re-run on group change: it's a
    // single field with no user-typed content to clobber, and the checkbox is the
    // authoritative per-spawn intent — leaving it on the old group's policy would
    // submit the new group's spawn with the wrong state. Pass null: the group's
    // default profile is not re-applied here, so seed from the group policy alone.
    applyRemoteControlPrefill(e.target.value, null);
    // Deliberately NOT re-running the profile pre-fill here: it's a one-shot
    // on open. Re-applying the new group's default profile mid-dialog would
    // clobber any name / role / model / init-msg the human already typed (the
    // profile sets many fields at once, unlike the field-scoped cwd prefill
    // above which protects user input). The group <select> is hidden in the
    // common per-group open anyway; to load a different group's profile, the
    // human picks it from the Profile selector.
  });
  // Switching the harness reshapes the Model + Sandbox rows for the new
  // harness (and re-applies the remembered effort for whatever Model
  // control becomes active).
  $('#agent-spawn-harness').addEventListener('change', (e) => {
    applySpawnHarness(e.target.value);
    // applySpawnHarness only CLEARS the Remote Access box for a harness with no
    // Remote Access; it doesn't re-fill when switching back to one. Re-derive
    // from the group policy so a Codex→Claude toggle restores the prefill instead
    // of leaving the box stuck off. No-op for a non-capable harness (the helper
    // re-checks the capability). Pass null: a loaded profile is not re-applied on
    // a manual harness toggle, so seed from the group policy alone.
    applyRemoteControlPrefill($('#agent-spawn-group').value, null);
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
  // Save as profile: open the profile editor pre-filled from the current
  // dialog fields (create mode — the human names it). On save, refresh the
  // Profile selector so the new profile appears and is selected.
  $('#agent-spawn-save-profile').addEventListener('click', () => {
    openProfileEditor(spawnFormAsProfileSeed(), {
      editExisting: false,
      onSaved: (newName) => {
        loadProfiles(true).then((profiles) => {
          if (!$('#agent-spawn-modal').classList.contains('show')) return;
          populateSpawnProfileOptions(profiles, newName);
        }).catch(() => { /* selector just keeps its current options */ });
      },
    });
  });
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
  // Live preview of the auto-normalized name on every keystroke; the field
  // itself is only rewritten on blur/submit (commitSpawnName) to keep typing
  // jank-free. A separate listener so the name→branch sync wiring above stays
  // byte-identical (guarded by TestDashboardHTML_WorktreeNameSyncWired).
  $('#agent-spawn-name').addEventListener('input', updateSpawnNameHint);
  $('#agent-spawn-name').addEventListener('blur', commitSpawnName);
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
  // Attachments: the "📎 Attach files" button opens the hidden native picker;
  // its change event adds the chosen files (then resets value so re-picking the
  // same file fires change again). Pasting a file/screenshot or dragging files
  // from Finder/Explorer onto the dialog adds them too. The list's × buttons
  // remove entries (delegated).
  $('#agent-spawn-attach-btn').addEventListener('click', () => $('#agent-spawn-attach-input').click());
  $('#agent-spawn-attach-input').addEventListener('change', (e) => {
    addSpawnAttachments(e.target.files);
    e.target.value = '';
  });
  $('#agent-spawn-modal').addEventListener('paste', handleSpawnPaste);
  bindSpawnDragDrop();
  $('#agent-spawn-attachments-list').addEventListener('click', (e) => {
    const btn = e.target.closest('.att-remove');
    if (btn) removeSpawnAttachment(Number(btn.dataset.attId));
  });
  // "Browse…" buttons beside CWD and Worktree-repo open the daemon's
  // native directory picker. We set the value then dispatch a synthetic
  // `input` event so the field's own listeners above run exactly as if
  // the human had typed — CWD still mirrors into Worktree-repo, and
  // both still re-list worktrees.
  const wireSpawnBrowse = (btnId, inputId, title) => {
    const btn = $('#' + btnId);
    const input = $('#' + inputId);
    if (!btn || !input) return;
    btn.addEventListener('click', async () => {
      const prev = btn.textContent;
      btn.disabled = true;
      btn.textContent = 'Opening…';
      try {
        const res = await pickDirectory({ startDir: input.value.trim(), title });
        if (res.error) { toast(res.error, true); return; }
        if (res.canceled) return;
        input.value = res.path;
        input.dispatchEvent(new Event('input', { bubbles: true }));
        input.focus();
      } finally {
        btn.disabled = false;
        btn.textContent = prev;
      }
    });
  };
  wireSpawnBrowse('agent-spawn-cwd-browse', 'agent-spawn-cwd', 'Select the working directory');
  wireSpawnBrowse('agent-spawn-wt-repo-browse', 'agent-spawn-wt-repo', 'Select the git repo to worktree');
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
  showModalError('clone-agent-error', '');
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
  showModalError(errEl, '');
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
      showModalError(errEl, (await r.text()) || `HTTP ${r.status}`);
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
      showModalError(errEl, `clone timed out after ${CLONE_FETCH_TIMEOUT_MS / 1000}s — the new agent may still come online; check ~/.tclaude/output.log and refresh in a moment.`);
    } else {
      showModalError(errEl, (err && err.message) || String(err));
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
  showModalError('reincarnate-agent-error', '');
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
  showModalError(errEl, '');
  const mode = reincarnateMode();
  let body;
  if (mode === 'force') {
    const followUp = normaliseFollowUp($('#reincarnate-agent-followup').value);
    if (!followUp) {
      showModalError(errEl, 'follow-up is required for force reincarnate');
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
      showModalError(errEl, (await r.text()) || `HTTP ${r.status}`);
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
    showModalError(errEl, (err && err.message) || String(err));
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
