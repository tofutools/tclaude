// config-service.js — Config › Service: how this node's agentd was started
// (systemd, launchd, tmux, a shell), what it was started with, a restart that
// goes through the supervisor, and starting agentd at login or boot.
//
// Unlike the rest of the Config form this is not config.json: it reads
// GET /api/node/service and acts at once through its POST routes, so its
// inputs are plain (not Config) controls and never mark the form dirty.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { RequestError, requestJSON } from './request-json.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { writeClipboard } from './insecure-origin.js';
import { LaunchProfile } from './config-service-launch.js';

const html = htm.bind(h);

export const SERVICE_URL = '/api/node/service';
// RESTART_POLL_MS paces the reads while the daemon restarts; RESTART_WAIT_MS
// is how long the panel waits for it to answer again before saying so.
const RESTART_POLL_MS = 1000;
const RESTART_WAIT_MS = 90000;
// POLL_TIMEOUT_MS bounds one read during a restart, so a request a proxy
// holds open cannot outlast the wait.
const POLL_TIMEOUT_MS = 5000;

const LAUNCHERS = {
  'systemd-user': 'systemd user unit',
  'systemd-system': 'systemd system unit',
  launchd: 'launchd',
  tmux: 'a tmux pane',
  shell: 'a shell',
  unknown: 'an unknown launcher',
};
export function launcherText(kind) { return LAUNCHERS[kind] || kind || 'unknown'; }

// observedArgv is what the running daemon was started with: the
// supervisor's command when there is one, else the process's own.
export function observedArgv(launcher) {
  return launcher?.argv?.length ? launcher.argv : (launcher?.process_argv || []);
}

// sameFlags compares two commands without their program path: a daemon
// started from a shell runs as "tclaude" where a unit names the full path.
export function sameFlags(a, b) {
  return (a || []).slice(1).join(' ') === (b || []).slice(1).join(' ');
}

// serveArgsOf splits the typed serve flags. They are passed to the daemon as
// separate arguments (never through a shell), and it validates each.
export function serveArgsOf(text) {
  return String(text || '').trim().split(/\s+/).filter(Boolean);
}

// restartHint says whether a Restart applies the installed autostart now:
// only when the running daemon is that same tclaude-managed unit.
export function restartAppliesAutostart(service) {
  const l = service?.launcher;
  const a = service?.autostart;
  return !!(l && a?.installed && a.managed && l.managed_by_tclaude && l.kind === a.kind);
}

function errorText(e) {
  return e?.message || String(e);
}

function Argv({ argv, id }) {
  if (!argv?.length) return html`<span class="muted">unknown</span>`;
  return html`<code class="cfg-svc-argv" id=${id}>${argv.join(' ')}</code>`;
}

function EnvList({ env }) {
  if (!env?.length) return html`<span class="muted">none</span>`;
  return html`<span class="cfg-svc-env">${env.map((e) => html`<code key=${e.key} title=${e.from ? `from ${e.from}` : ''}>${e.key}${e.value === '<redacted>' ? '=•••' : ''}</code>`)}</span>`;
}

function Row({ label, children }) {
  return html`<span class="cfg-svc-k">${label}</span><span class="cfg-svc-v">${children}</span>`;
}

// ServicePanel reads the service state while open and drives restart and
// autostart. fetchImpl/confirm/toast/copy/timers are injectable for tests.
export function ServicePanel({
  open = true, fetchImpl = (...a) => globalThis.fetch(...a), confirm = shellConfirm, toast = shellToast,
  copy = writeClipboard, timers = globalThis,
}) {
  const [service, setService] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState('');
  const [restarting, setRestarting] = useState(false);
  const [argsMode, setArgsMode] = useState('keep');
  const [argsText, setArgsText] = useState('');
  const [keepEnv, setKeepEnv] = useState(true);
  const [actionError, setActionError] = useState('');
  // launch is the launch profile's view (config-service-launch.js): while a
  // profile is set it decides the unit's flags and environment, so install
  // sends neither.
  const [launch, setLaunch] = useState(null);
  // launchReload reads the launch profile again after the daemon or its unit
  // changed here, so its comparison is of what runs now.
  const [launchReload, setLaunchReload] = useState(0);
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);

  const read = (refresh = false) => requestJSON(refresh ? `${SERVICE_URL}?refresh=1` : SERVICE_URL, { fetchImpl });
  const readBounded = () => new Promise((resolve, reject) => {
    const t = timers.setTimeout(() => reject(new Error('timed out')), POLL_TIMEOUT_MS);
    read(true).then(resolve, reject).finally(() => timers.clearTimeout?.(t));
  });
  const load = async (refresh = false) => {
    try {
      const s = await read(refresh);
      if (!alive.current) return;
      setService(s);
      setError(null);
    } catch (e) {
      if (alive.current) setError(e);
    }
  };
  // Each opening reads afresh, so a failed read or a restart made elsewhere
  // does not stick; a running restart keeps its own polling.
  useEffect(() => { if (open && !restarting) void load(true); }, [open]);

  // waitForDaemon polls until the restarted daemon answers again.
  const waitForDaemon = async () => {
    const deadline = Date.now() + RESTART_WAIT_MS;
    while (alive.current && Date.now() < deadline) {
      await new Promise((resolve) => timers.setTimeout(resolve, RESTART_POLL_MS));
      try {
        const s = await readBounded();
        if (!alive.current) return;
        if (!s?.restart_pending) {
          setService(s);
          setRestarting(false);
          setLaunchReload((n) => n + 1);
          toast('agentd is back');
          return;
        }
      } catch { /* still restarting */ }
    }
    if (alive.current) {
      setRestarting(false);
      toast('agentd has not answered yet; reload the page in a moment', true);
    }
  };

  const restart = async () => {
    const l = service.launcher;
    const viaSupervisor = l.restart_method === 'supervisor';
    if (!await confirm({
      title: 'Restart agentd?',
      body: viaSupervisor
        ? `agentd restarts through ${launcherText(l.kind)}${l.unit || l.label ? ` ${l.unit || l.label}` : ''}, with that unit's own arguments. Agent sessions keep running; the dashboard reconnects.`
        : 'agentd restarts itself with the same arguments. Agent sessions keep running; the dashboard reconnects.',
      okLabel: 'Restart',
    })) return;
    setBusy('restart');
    setActionError('');
    try {
      await requestJSON(`${SERVICE_URL}/restart`, { method: 'POST', body: {}, fetchImpl });
      setRestarting(true);
      void waitForDaemon();
    } catch (e) {
      if (!alive.current) return;
      setActionError(e?.code === 'restart_unavailable' ? `Restart is not available here: ${errorText(e)}` : `Restart failed: ${errorText(e)}`);
    } finally {
      if (alive.current) setBusy('');
    }
  };

  const autostart = async (action) => {
    const a = service.autostart;
    const byProfile = !!launch?.profile;
    const body = action === 'install' && !byProfile ? { action, keep_env: keepEnv } : { action };
    if (action === 'install' && !byProfile && argsMode === 'custom') body.serve_args = serveArgsOf(argsText);
    const ok = await confirm(action === 'install' ? {
      title: a.installed ? 'Rewrite the autostart unit?' : 'Start agentd on login?',
      body: `tclaude writes ${a.path || 'the unit'}${byProfile ? ' with the launch profile\'s flags and environment'
        : `${argsMode === 'custom' ? ` with: agentd serve ${body.serve_args.join(' ')}` : ', keeping the flags agentd runs with now'}${keepEnv ? ' and its environment' : ''}`}. It takes effect at the next login or boot; nothing restarts now.`,
      okLabel: a.installed ? 'Rewrite' : 'Install',
    } : {
      title: 'Stop starting agentd on login?',
      body: `tclaude removes ${a.path || 'the unit it wrote'}. agentd keeps running until it stops.`,
      okLabel: 'Remove',
    });
    if (!ok) return;
    setBusy(action);
    setActionError('');
    try {
      const s = await requestJSON(`${SERVICE_URL}/autostart`, { method: 'POST', body, fetchImpl });
      if (!alive.current) return;
      setService(s);
      setLaunchReload((n) => n + 1);
      toast(action === 'install' ? 'Autostart installed' : 'Autostart removed');
    } catch (e) {
      if (!alive.current) return;
      setActionError(e?.code === 'unit_not_managed'
        ? `${a.path || 'That unit'} was not written by tclaude, so tclaude leaves it alone.`
        : e?.code === 'launch_profile_set' ? 'A launch profile is set: it decides the unit\'s flags and environment. Edit it above instead.'
        : errorText(e));
    } finally {
      if (alive.current) setBusy('');
    }
  };

  if (error) {
    const absent = error instanceof RequestError && error.status === 404;
    return html`<div class="cfg-svc" id="cfg-service"><span class="muted" role="alert">${absent
      ? 'This daemon does not report how it was started yet.'
      : `Could not read the service state: ${errorText(error)}`}</span></div>`;
  }
  if (!service) return html`<div class="cfg-svc" id="cfg-service"><span class="muted">Loading…</span></div>`;

  const l = service.launcher || {};
  const a = service.autostart;
  const observed = observedArgv(l);
  const configured = a?.installed ? a.argv : null;
  const differs = !!(configured && observed.length && !sameFlags(configured, observed));
  const byProfile = !!launch?.profile;
  const customEmpty = !byProfile && argsMode === 'custom' && serveArgsOf(argsText).length === 0;
  const any = busy || restarting;
  return html`<div class="cfg-svc" id="cfg-service">
    <div class="cfg-section" role="group" aria-labelledby="cfg-svc-launcher-h">
      <h3 class="ui-eyebrow ui-eyebrow-section" id="cfg-svc-launcher-h">Launcher</h3>
      <p class="cfg-group-lede">Detected from the running daemon. A restart goes through its supervisor, so the unit's own arguments stay.</p>
      <div class="cfg-svc-dl">
        <${Row} label="Running under"><strong id="cfg-svc-kind">${launcherText(l.kind)}</strong>${l.unit || l.label ? html` <code>${l.unit || l.label}</code>` : ''}${l.managed_by_tclaude ? html` <span class="muted">· written by tclaude</span>` : ''}<//>
        ${(l.unit_path || l.domain) && html`<${Row} label="Unit">${l.unit_path ? html`<code>${l.unit_path}</code>` : ''}${l.domain ? html` <span class="muted">${l.domain}</span>` : ''}<//>`}
        <${Row} label="Command"><${Argv} id="cfg-svc-observed" argv=${observed} /><//>
        <${Row} label="Environment"><${EnvList} env=${l.env} />${(l.environment_files || []).map((f) => html` <span key=${f} class="muted">· from <code>${f}</code></span>`)}<//>
        ${(l.restart_policy || l.kill_mode) && html`<${Row} label="Policy"><span class="muted">${[l.restart_policy && `Restart=${l.restart_policy}`, l.kill_mode && `KillMode=${l.kill_mode}`].filter(Boolean).join(' · ')}</span><//>`}
        <${Row} label="Restart"><span>${l.restart_method === 'supervisor' ? 'through the supervisor' : 'agentd re-runs itself with the same arguments'}</span>
          <${Button} size="sm" id="cfg-svc-restart" disabled=${!!any} busy=${busy === 'restart' || restarting} onClick=${() => { void restart(); }}>${restarting ? 'Restarting…' : 'Restart agentd…'}<//><//>
      </div>
      ${(l.warnings || []).map((w) => html`<div key=${w} class="cfg-svc-warn">${w}</div>`)}
      ${service.restart_pending && html`<div class="cfg-svc-warn" id="cfg-svc-restart-pending">A restart (${service.restart_pending}) is pending.</div>`}
    </div>

    <div class="cfg-section" role="group" aria-labelledby="cfg-svc-launch-h">
      <h3 class="ui-eyebrow ui-eyebrow-section" id="cfg-svc-launch-h">Launch profile</h3>
      <${LaunchProfile} open=${open} reload=${launchReload} fetchImpl=${fetchImpl} confirm=${confirm} toast=${toast} onView=${setLaunch} />
    </div>

    <div class="cfg-section" role="group" aria-labelledby="cfg-svc-boot-h">
      <h3 class="ui-eyebrow ui-eyebrow-section" id="cfg-svc-boot-h">Start on login</h3>
      ${!a ? html`<p class="cfg-group-lede" id="cfg-svc-autostart-absent">This daemon cannot set up autostart yet. CLI: <code>tclaude service status</code>.</p>`
        : !a.supported ? html`<p class="cfg-group-lede" id="cfg-svc-autostart-unsupported">Autostart is not supported here.</p>`
        : html`
        <p class="cfg-group-lede">tclaude writes a ${a.kind === 'launchd' ? 'LaunchAgent' : 'systemd user unit'} that starts agentd when you log in${a.kind === 'launchd' ? '' : html`, with <code>KillMode=process</code> so tmux sessions survive restarts`}. Removing it removes only what tclaude wrote. CLI: <code>tclaude service install|uninstall|status</code>.</p>
        <div class="cfg-svc-dl">
          <${Row} label="Status"><span id="cfg-svc-autostart-state">${!a.installed ? 'not installed'
            : !a.managed ? html`<span class="cfg-svc-warn">installed by hand — tclaude leaves it alone</span>`
            : a.enabled ? 'installed and enabled' : 'installed, not enabled'}</span>${a.path ? html` <code>${a.path}</code>` : ''}<//>
          ${configured && html`<${Row} label="Unit runs"><${Argv} id="cfg-svc-configured" argv=${configured} />
            ${differs && html`<div class="cfg-svc-warn" id="cfg-svc-differs">Differs from what agentd runs with now.</div>`}<//>`}
          ${configured && html`<${Row} label="Unit environment"><${EnvList} env=${a.env} />${a.env_file ? html` <span class="muted">· from <code>${a.env_file}</code></span>` : ''}<//>`}
          ${(!a.installed || a.managed) && byProfile && html`<${Row} label="Flags and env"><span class="muted" id="cfg-svc-by-profile">from the launch profile above</span><//>`}
          ${(!a.installed || a.managed) && html`
            ${!byProfile && html`<${Row} label="Serve flags"><span class="cfg-svc-args">
              <label class="cfg-inline"><input type="radio" name="cfg-svc-args" value="keep" checked=${argsMode === 'keep'} onChange=${() => setArgsMode('keep')} /> keep the flags agentd runs with now</label>
              <label class="cfg-inline"><input type="radio" name="cfg-svc-args" value="custom" checked=${argsMode === 'custom'} onChange=${() => setArgsMode('custom')} /> use these:</label>
              <input type="text" class="ui-input ui-sm" id="cfg-svc-serve-args" spellcheck="false" autocomplete="off" aria-label="agentd serve flags"
                placeholder="--no-tray --dashboard-port 7878" disabled=${argsMode !== 'custom'} value=${argsText} onInput=${(e) => setArgsText(e.currentTarget.value)} />
            </span><//>
            <${Row} label="Environment"><label class="cfg-inline"><input type="checkbox" id="cfg-svc-keep-env" checked=${keepEnv} onChange=${(e) => setKeepEnv(e.currentTarget.checked)} /> keep the environment agentd runs with now (PATH and the rest)</label><//>`}
            <${Row} label=""><span class="cfg-svc-actions">
              <${Button} variant="primary" size="sm" id="cfg-svc-install" disabled=${!!any || customEmpty} title=${customEmpty ? 'Type the serve flags, or keep the ones agentd runs with now' : undefined} busy=${busy === 'install'} onClick=${() => { void autostart('install'); }}>${a.installed ? 'Rewrite unit…' : 'Start on login…'}<//>
              ${a.installed && html`<${Button} variant="danger" size="sm" id="cfg-svc-uninstall" disabled=${!!any} busy=${busy === 'uninstall'} onClick=${() => { void autostart('uninstall'); }}>Remove…<//>`}
            </span><//>`}
          ${restartAppliesAutostart(service) && differs && html`<${Row} label=""><span class="muted" id="cfg-svc-restart-hint">Restart agentd above to run with the unit's new settings now.</span><//>`}
          ${a.linger && html`<${Row} label="After logout">${a.linger.enabled ? 'keeps running (lingering is on)'
            : html`<span class="cfg-svc-warn">stops when you log out.</span> To keep it running, lingering needs sudo, which tclaude never runs for you:
              <span class="cfg-svc-cmd"><code id="cfg-svc-linger">${a.linger.command}</code> <${Button} size="sm" variant="ghost" onClick=${() => { void copy(a.linger.command); }}>Copy<//></span>`}<//>`}
        </div>
        ${(a.warnings || []).map((w) => html`<div key=${w} class="cfg-svc-warn">${w}</div>`)}`}
      ${actionError && html`<div class="cfg-svc-warn" role="alert" id="cfg-svc-error">${actionError}</div>`}
    </div>
  </div>`;
}
