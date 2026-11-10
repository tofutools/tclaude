// config-self-update.js — what Config › Installation › Updates knows beyond
// config.json: how this node's tclaude was installed, which update methods
// can run here, and the scheduler's last and next run. It comes from GET
// /api/node/update/settings and is display-only: the settings themselves are
// ordinary updates.self keys, loaded and saved with the rest of the Config
// form by config-form-adapter.js.
//
// The form is uncontrolled, so nothing here re-renders an input. The adapter
// mirrors the few form values these components depend on (method, install
// dir, setup flags, schedule) into selfUpdateDraft; the components below read
// that signal and the metadata signal on their own.

import { h } from 'preact';
import { signal } from '@preact/signals';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { writeClipboard } from './insecure-origin.js';

const html = htm.bind(h);

export const SELF_UPDATE_SETTINGS_URL = '/api/node/update/settings';

// The method ids the daemon accepts, in picker order, with labels used until
// (or if) the daemon's own list arrives. Package-manager methods are opt-in.
export const SELF_UPDATE_METHODS = Object.freeze([
  { id: 'release', label: 'Release download' },
  { id: 'go_install', label: 'Build with go install' },
  { id: 'source_checkout', label: 'Build from a checkout' },
  { id: 'custom', label: 'Custom command' },
  { id: 'homebrew', label: 'Let tclaude run Homebrew', package_manager: true },
  { id: 'nix_profile', label: 'Let tclaude run Nix', package_manager: true },
  { id: 'mise', label: 'Let tclaude run mise', package_manager: true },
  { id: 'apt', label: 'Let tclaude run apt', package_manager: true },
  { id: 'dnf', label: 'Let tclaude run dnf', package_manager: true },
  { id: 'manual', label: 'Notify me (manual)', notify_only: true },
]);

// TCLAUDE_DRIVEN are the methods that install a binary themselves instead of
// asking a package manager; next to a package-managed binary they need the
// operator's acknowledgement and an install dir outside the manager's tree.
export const TCLAUDE_DRIVEN = Object.freeze(['release', 'go_install', 'source_checkout', 'custom']);

// The `tclaude setup` flags a post-update setup may pass (config.SetupFlags).
export const SETUP_FLAGS = Object.freeze([
  { flag: 'install-agent-skills', label: 'Install agent skills' },
  { flag: 'install-default-agent-permissions', label: 'Default agent permissions' },
  { flag: 'install-utility-skills', label: 'Utility skills' },
  { flag: 'install-sandbox-hardening', label: 'Sandbox hardening' },
  { flag: 'install-proxy-skills', label: 'Proxy skills' },
  { flag: 'install-resume-threshold-override', label: 'Resume threshold override' },
  { flag: 'all-harnesses', label: 'All harnesses' },
  { flag: 'absolute-paths', label: 'Absolute paths' },
  { flag: 'install-all', label: 'Everything (install-all)' },
]);
export const SETUP_HARNESSES = Object.freeze(['claude', 'codex', 'opencode', 'copilot', 'gemini', 'pi']);
export const WEEKDAYS = Object.freeze(['monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday']);

export const SCHEDULE_DEFAULTS = Object.freeze({ frequency: 'off', weekday: 'monday', window_start: '03:00', window_end: '05:00' });
export const CUSTOM_TIMEOUT_DEFAULT = 1800;
export const INSTALL_DIR_DEFAULT = '~/.local/bin';

const MANAGER_NAMES = {
  homebrew: 'Homebrew', nix_profile: 'Nix', nix: 'Nix', mise: 'mise', asdf: 'asdf', dpkg: 'apt (dpkg)', rpm: 'rpm',
};
export function managerName(id) { return MANAGER_NAMES[id] || id; }

// selfUpdateMeta is the last settings read: null before the first, {error}
// when it failed, else the daemon's body.
export const selfUpdateMeta = signal(null);
// selfUpdateDraft mirrors the form values the notes below depend on.
export const selfUpdateDraft = signal({ method: '', installDir: '', flags: [], harness: '', setupEnabled: false, frequency: 'off', weekday: 'monday', windowStart: '03:00', windowEnd: '05:00', onStart: false, ack: false });
// selfUpdateLoads counts the times the form loaded updates.self, so the
// Installation section can fold its Updates part afresh after each read.
export const selfUpdateLoads = signal(0);

// methodInfo is a method's daemon entry, or its built-in fallback.
export function methodInfo(id, meta = selfUpdateMeta.value) {
  const known = SELF_UPDATE_METHODS.find((m) => m.id === id);
  const live = (meta?.methods || []).find((m) => m.id === id);
  return live ? { ...known, ...live } : known || null;
}

// needsPackageManagerAck reports whether this draft installs next to a
// package-managed binary, which the operator must acknowledge.
export function needsPackageManagerAck(draft = selfUpdateDraft.value, meta = selfUpdateMeta.value) {
  return !!meta?.detected?.managed_by && TCLAUDE_DRIVEN.includes(draft.method);
}

// selfUpdateDeps is set by the Config adapter, so the metadata is read with
// the same fetch as the config itself.
export const selfUpdateDeps = { fetchImpl: (...args) => globalThis.fetch(...args) };

// invalidateSelfUpdateMeta drops the metadata after a config load or save;
// SelfUpdateMetaLoader reads it again the next time the panel is open.
const metaGeneration = signal(0);
export function invalidateSelfUpdateMeta() {
  metaEpoch++;
  selfUpdateMeta.value = null;
  metaGeneration.value++;
}

// loadSelfUpdateMeta reads the settings metadata. installDir asks the daemon
// whether that (unsaved) dir would be shadowed on PATH. A stale answer — one
// that started before a newer read — is dropped.
let metaEpoch = 0;
export async function loadSelfUpdateMeta(fetchImpl, { installDir = '' } = {}) {
  const epoch = ++metaEpoch;
  const url = installDir ? `${SELF_UPDATE_SETTINGS_URL}?install_dir=${encodeURIComponent(installDir)}` : SELF_UPDATE_SETTINGS_URL;
  try {
    const r = await fetchImpl(url, { credentials: 'same-origin' });
    const body = await r.json().catch(() => ({}));
    if (epoch !== metaEpoch) return;
    selfUpdateMeta.value = r.ok ? body : { error: body?.error || `HTTP ${r.status}`, status: r.status };
  } catch (e) {
    if (epoch === metaEpoch) selfUpdateMeta.value = { error: e?.message || String(e) };
  }
}

// SelfUpdateMetaLoader reads the metadata while the Updates panel is open
// and none is loaded; it renders nothing.
export function SelfUpdateMetaLoader({ open }) {
  const missing = selfUpdateMeta.value === null;
  const generation = metaGeneration.value;
  useEffect(() => {
    if (open && missing) void loadSelfUpdateMeta(selfUpdateDeps.fetchImpl);
  }, [open, missing, generation]);
  return null;
}

function CopyCommand({ command, id }) {
  return html`<span class="cfg-su-cmd"><code id=${id}>${command}</code>
    <${Button} size="sm" variant="ghost" title="Copy" onClick=${() => { void writeClipboard(command); }}>Copy<//></span>`;
}

// ShowWhen shows its children only while test(draft, meta) holds. It hides
// rather than unmounts, so the uncontrolled inputs inside keep their values
// and the adapter can still read them.
export function ShowWhen({ test, children }) {
  return html`<div class="cfg-su-when" hidden=${!test(selfUpdateDraft.value, selfUpdateMeta.value)}>${children}</div>`;
}

// MethodOptions are the method <option>s. The list and order never change,
// so the uncontrolled select keeps its value; only labels and availability
// follow the daemon's metadata.
export function MethodOptions() {
  const meta = selfUpdateMeta.value;
  return html`<option value="">Not chosen (no automatic updates)</option>
    ${SELF_UPDATE_METHODS.map(({ id }) => {
      const m = methodInfo(id, meta);
      const off = m.available === false;
      return html`<option key=${id} value=${id} disabled=${off}>${m.label}${off && m.unavailable_reason ? ` — ${m.unavailable_reason}` : ''}</option>`;
    })}`;
}

// SelfUpdateDetected says how this node's tclaude was installed and what the
// operator can do about a package-managed or unrecognised install.
export function SelfUpdateDetected() {
  const meta = selfUpdateMeta.value;
  if (!meta) return html`<span class="muted">Checking how tclaude was installed…</span>`;
  if (meta.error) return html`<span class="muted" role="alert">Could not read the update settings: ${meta.error}</span>`;
  const d = meta.detected || {};
  const label = methodInfo(d.method, meta)?.label || d.method || 'unknown';
  const warnings = (meta.warnings || []).map((w) => html`<div key=${w} class="cfg-su-warn">${w}</div>`);
  if (!meta.detected) {
    return html`<div class="cfg-su-detected" id="cfg-selfupdate-detected">
      <span class="muted">Could not tell how tclaude was installed${meta.detection_error ? `: ${meta.detection_error}` : '.'}</span>${warnings}</div>`;
  }
  return html`<div class="cfg-su-detected" id="cfg-selfupdate-detected">
    ${d.managed_by
      ? html`<div><strong>Managed by ${managerName(d.managed_by)}.</strong> ${d.reason || ''}</div>
        ${d.manual_command && html`<div>To update it yourself: <${CopyCommand} id="cfg-selfupdate-manual-command" command=${d.manual_command} /></div>`}`
      : d.recognized === false
        ? html`<div class="cfg-su-alert" role="note">tclaude does not recognise how it was installed, so “Notify me” is the safe choice. If a package manager or script put it there, updating it another way may be overwritten or shadowed by that tool.</div>`
        : html`<div>Detected: <strong>${label}</strong>${d.reason ? html` — ${d.reason}` : ''}</div>`}
    ${((d.binaries || []).length > 0 || d.build_info) && html`<details class="cfg-more"><summary>What tclaude found</summary>
      <div class="cfg-hint ui-hint cfg-su-evidence">
        ${(d.binaries || []).map((b) => html`<div key=${b.name}><code>${b.name}</code> ${b.version ? html`<code>${b.version}</code> ` : ''}<span class="muted">${b.path}</span>
          ${b.managed_by ? html` · managed by ${managerName(b.managed_by)}${b.package ? ` (${b.package})` : ''}` : b.install_method ? html` · ${b.install_method.replaceAll('_', ' ')}` : ''}
          ${b.evidence ? html`<div class="muted">${b.evidence}</div>` : ''}</div>`)}
        ${d.build_info && html`<div class="muted">Build: ${d.build_info}</div>`}
      </div></details>`}
    ${warnings}
  </div>`;
}

// SelfUpdateMethodNote explains the picked method: what it runs, why it is
// unavailable, or that nothing is automatic until one is chosen.
export function SelfUpdateMethodNote() {
  const draft = selfUpdateDraft.value;
  const meta = selfUpdateMeta.value;
  if (!draft.method) {
    return html`<div class="cfg-su-note" id="cfg-selfupdate-method-note">No method chosen: scheduled and start-up updates stay off. <code>tclaude update --apply</code> and the Fleet Update button still use the detected method.</div>`;
  }
  const m = methodInfo(draft.method, meta);
  if (!m) return null;
  return html`<div class="cfg-su-note" id="cfg-selfupdate-method-note">
    ${m.description || ''}
    ${m.available === false && html`<div class="cfg-su-warn" role="alert">Not available here${m.unavailable_reason ? `: ${m.unavailable_reason}` : ''}. Updates with this method fail until that is fixed.</div>`}
    ${m.package_manager && m.command && html`<div>tclaude runs <code>${m.command}</code>, then checks the new version.</div>`}
  </div>`;
}

// PackageManagerNote is shown when a tclaude-driven method is picked while a
// package manager owns the binary: untrack it there, tclaude installs beside.
export function PackageManagerNote() {
  const meta = selfUpdateMeta.value;
  const d = meta?.detected || {};
  const pm = managerName(d.managed_by);
  return html`<div class="cfg-su-alert" id="cfg-selfupdate-pm-note">
    <div><strong>${pm} still owns tclaude.</strong> Untrack it there first, or ${pm} may bring its copy back or keep it first on PATH${d.untrack_command ? ':' : '.'}</div>
    ${d.untrack_command && html`<div><${CopyCommand} id="cfg-selfupdate-untrack-command" command=${d.untrack_command} /></div>`}
    <div>tclaude never writes into ${pm}'s tree; it installs to the directory below.</div>
  </div>`;
}

// ShadowNote warns when the install dir comes after the package-managed
// binary on PATH (the daemon checks the typed, unsaved value too).
export function ShadowNote() {
  const meta = selfUpdateMeta.value;
  if (!meta?.install_dir_shadowed) return null;
  const dir = selfUpdateDraft.value.installDir || INSTALL_DIR_DEFAULT;
  const by = meta.shadowed_by || 'the package-managed binary';
  return html`<div class="cfg-su-warn" id="cfg-selfupdate-shadowed" role="alert">⚠ <code>${dir}</code> comes after <code>${by}</code> on PATH, so that copy still runs until you untrack it.</div>`;
}

// ScheduleNote says when the schedule will (or will not) next run.
export function ScheduleNote() {
  const draft = selfUpdateDraft.value;
  const meta = selfUpdateMeta.value;
  if (draft.frequency === 'off') return null;
  if (!draft.method) return html`<div class="cfg-su-warn" id="cfg-selfupdate-schedule-note">Choose an update method above; until then the schedule does nothing.</div>`;
  const next = meta?.next_scheduled_run;
  return next ? html`<div class="cfg-su-note" id="cfg-selfupdate-schedule-note">Next run (as saved): ${new Date(next).toLocaleString()}</div>` : null;
}

const OUTCOMES = {
  applied: 'updated', up_to_date: 'already up to date', update_available: 'update available (notified)',
  method_unchosen: 'skipped: no method chosen', deferred: 'deferred: agents were working', busy: 'skipped: another update was running',
  failed: 'failed',
};

// LastScheduledRun is the scheduler's last run, if any.
export function LastScheduledRun() {
  const last = selfUpdateMeta.value?.last_scheduled_run;
  if (!last) return html`<span class="muted">No scheduled run yet.</span>`;
  return html`<span id="cfg-selfupdate-last-run" class=${last.outcome === 'failed' ? 'cfg-su-warn' : ''}>
    ${new Date(last.at).toLocaleString()} — ${OUTCOMES[last.outcome] || last.outcome}${last.detail ? ` (${last.detail})` : ''}</span>`;
}

// SetupPreview is the exact command a post-update setup runs.
export function SetupPreview() {
  const draft = selfUpdateDraft.value;
  const args = ['tclaude', 'setup', '--yes', ...draft.flags.map((f) => `--${f}`), ...(draft.harness ? ['--harness', draft.harness] : [])];
  return html`<div class="cfg-su-note" id="cfg-selfupdate-setup-preview">Runs: <code>${args.join(' ')}</code></div>`;
}
