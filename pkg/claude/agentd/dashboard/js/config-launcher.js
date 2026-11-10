// config-launcher.js — the App launcher part of Config › Installation (and of
// the Fleet node dialog's Installation view): a desktop entry (Linux) or an
// app bundle (macOS) that opens the dashboard, starting agentd first when it
// does not answer, and the agentd serve flags it starts agentd with.
//
// Like Start automatically it is not config.json through the form: it reads
// GET <base>/launcher and acts at once through POST <base>/launcher/install
// and /uninstall, so its controls are plain and never mark the Config form
// dirty. The serve flags are stored by an install (service.launcher.
// serve_args), so changing them rewrites the launcher.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { RequestError, requestJSON } from './request-json.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { InstallPart, useFold } from './config-installation.js';

const html = htm.bind(h);

export const LAUNCHER_BASE = '/api/node';
const DEFAULT_PORT = '7878';

// LAUNCHER_FLAGS are the serve flags the part offers as controls, each with
// what it does. A peer without full access may set only these.
export const LAUNCHER_FLAGS = Object.freeze([
  { name: '--persist-operator-token', label: 'Keep the dashboard sign-in', meaning: 'Keeps the operator token on disk, so the dashboard stays signed in across restarts and the launcher can sign in to it.' },
  { name: '--dashboard-port', label: 'Fixed dashboard port', meaning: 'Serves the dashboard on the same port every time, so bookmarks and open tabs keep working.', value: true },
  { name: '--no-print-human-token', label: 'Do not print the sign-in token', meaning: 'A launcher has no terminal, so a printed token would only end up in the log.' },
  { name: '--no-tray', label: 'No tray icon', meaning: 'Starts agentd without its system tray icon.' },
]);
const KNOWN = new Set(LAUNCHER_FLAGS.map((f) => f.name));

const SOURCES = {
  profile: 'from the launch profile, which overrides the launcher\'s own setting and the defaults',
  launcher: 'the launcher\'s own setting',
  default: 'tclaude\'s defaults',
};

// controlOf reads a serve flag a control can show: a bare known flag, or
// --dashboard-port=<port>. Anything else (--persist-operator-token=false, a
// bare --dashboard-port, an unknown flag) is "other", kept as typed.
function controlOf(token) {
  const [name, value] = token.split(/=(.*)/s);
  if (!KNOWN.has(name)) return null;
  if (name === '--dashboard-port') return /^\d+$/.test(value || '') ? { name, port: value } : null;
  return value === undefined ? { name } : null;
}

// flagsDraft splits serve flags into the controls: which known flags are on,
// the dashboard port, and the other flags as text. It keeps the flags it was
// made from, so an untouched draft gives them back token for token.
export function flagsDraft(args) {
  const base = [...(args || [])];
  const on = {};
  let port = '';
  const other = [];
  for (const a of base) {
    const c = controlOf(a);
    if (!c) { other.push(a); continue; }
    on[c.name] = true;
    if (c.port) port = c.port;
  }
  const text = other.join(' ');
  return { on, port, other: text, base, otherBase: text };
}

// draftArgs is the serve flags a draft stands for: the flags it was made
// from, in their order, less the controls turned off (and with the port as
// set), then the controls turned on, then any other flags typed anew.
export function draftArgs(d) {
  const base = d.base || [];
  const otherEdited = (d.other || '') !== (d.otherBase || '');
  const out = [];
  const seen = new Set();
  const flagToken = (f) => (f.value ? `${f.name}=${String(d.port).trim() || DEFAULT_PORT}` : f.name);
  for (const a of base) {
    const c = controlOf(a);
    if (!c) { if (!otherEdited) out.push(a); continue; }
    if (seen.has(c.name) || !d.on[c.name]) continue;
    seen.add(c.name);
    out.push(flagToken(LAUNCHER_FLAGS.find((f) => f.name === c.name)));
  }
  for (const f of LAUNCHER_FLAGS) if (d.on[f.name] && !seen.has(f.name)) out.push(flagToken(f));
  if (otherEdited) out.push(...String(d.other || '').trim().split(/\s+/).filter(Boolean));
  return out;
}

const same = (a, b) => JSON.stringify(a || []) === JSON.stringify(b || []);

// launcherStatus is the part's status: state, a sentence, its main actions
// and whether it is set up and healthy enough to fold its detail away.
export function launcherStatus(st) {
  if (!st?.supported) return { state: 'unknown', text: 'An app launcher is supported on Linux and macOS only.' };
  const where = st.kind === 'app-bundle' ? 'Applications' : 'the app menu';
  if (!st.installed) {
    return { state: 'off', text: `Not in ${where}. A launcher would open the dashboard from the desktop, starting agentd first when it is not running.`, action: 'install' };
  }
  if (!st.managed) return { state: 'on', text: `In ${where} from ${st.path}, which tclaude did not write, so tclaude leaves it alone.` };
  if (st.stale) {
    return { state: 'warn', text: `In ${where}, but out of date${st.stale_reason ? `: ${st.stale_reason}` : ''}. agentd rewrites it when it next starts, or reinstall it now.`, action: 'reinstall' };
  }
  const starts = st.options?.start_daemon === false ? '' : ', starting agentd first when it is not running';
  if ((st.warnings || []).length) return { state: 'warn', text: `In ${where}: it opens the dashboard${starts}, but see the warnings below.`, action: 'uninstall' };
  return { state: 'on', text: `In ${where}: it opens the dashboard${starts}.`, action: 'uninstall', healthy: true };
}

const TOKEN_STORES = { file: 'on disk (a 0600 file)', keychain: 'in the keychain', ephemeral: 'in memory only' };

function errorText(e) { return e?.message || String(e); }

function Row({ label, children, id }) {
  return html`<span class="cfg-svc-k">${label}</span><span class="cfg-svc-v" id=${id}>${children}</span>`;
}

// LauncherPanel reads the launcher while open and installs or removes it.
// base names the node (LAUNCHER_BASE, or a peer's /api/peer/<id>/node); idp
// prefixes its element ids. fetchImpl/confirm/toast are injectable for tests.
export function LauncherPanel({
  open = true, base = LAUNCHER_BASE, idp = 'cfg-',
  fetchImpl = (...a) => globalThis.fetch(...a), confirm = shellConfirm, toast = shellToast,
}) {
  const [st, setSt] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState('');
  const [actionError, setActionError] = useState('');
  const [startDaemon, setStartDaemon] = useState(true);
  const [draft, setDraft] = useState(flagsDraft([]));
  const [reads, setReads] = useState(0);
  const alive = useRef(true);
  useEffect(() => () => { alive.current = false; }, []);
  const url = `${base}/launcher`;
  const [collapsed, toggle, expand] = useFold(!!(st && launcherStatus(st).healthy), reads);

  const take = (s) => {
    setSt(s);
    // A launcher not installed yet has no options: an install starts agentd
    // by default, as the API does.
    setStartDaemon(!s?.installed || s?.options?.start_daemon !== false);
    setDraft(flagsDraft(s?.serve_args));
  };
  const load = async () => {
    try {
      const s = await requestJSON(url, { fetchImpl });
      if (!alive.current) return;
      take(s);
      setError(null);
      setReads((n) => n + 1);
    } catch (e) {
      if (alive.current) setError(e);
    }
  };
  useEffect(() => { if (open) void load(); }, [open, base]);

  const title = 'App launcher';
  if (error) {
    const text = error instanceof RequestError && error.status === 404 ? 'This daemon has no app launcher yet.'
      : error?.status === 403 ? 'You may not see this node\'s app launcher (it needs node.read there).'
      : `Could not read the app launcher: ${errorText(error)}`;
    return html`<${InstallPart} idp=${idp} id="launcher" title=${title} state="unknown"
      status=${html`<span class="muted" role="alert" id="${idp}launcher">${text}</span>`} />`;
  }
  if (!st) return html`<${InstallPart} idp=${idp} id="launcher" title=${title} state="unknown" status=${html`<span class="muted" id="${idp}launcher">Loading…</span>`} />`;

  const status = launcherStatus(st);
  const readOnly = st.access === 'read';
  const full = !st.access || st.access === 'full';
  const canAct = !readOnly && st.supported && (!st.installed || st.managed);
  const args = draftArgs(draft);
  const byProfile = st.serve_args_source === 'profile';
  // While a launch profile is set the API reports its flags, not the
  // launcher's own setting, so the controls only show them: an edit here
  // would replace that setting with the profile's flags.
  const flagsChanged = !byProfile && !same(args, st.serve_args);
  // A peer without full access may send only the flags with a control, so
  // it cannot change flags that hold others (the node would refuse them).
  const otherLocked = !full && !!draft.other;
  const files = st.files?.length ? st.files : st.path ? [st.path] : [];

  const install = async ({ serveArgs } = {}) => {
    const body = { start_daemon: startDaemon };
    if (serveArgs !== undefined) body.serve_args = serveArgs;
    else if (flagsChanged) body.serve_args = args;
    const flags = body.serve_args === null ? st.default_serve_args : body.serve_args || st.serve_args;
    const ok = await confirm({
      title: st.installed ? 'Rewrite the app launcher?' : 'Add tclaude to the app menu?',
      body: `tclaude writes ${files.join(', ') || 'the launcher'}. It runs ${st.current_exe || 'this tclaude'}${startDaemon ? `, starting agentd first when it is not running, with: agentd serve ${(flags || []).join(' ')}` : ' and never starts agentd'}.${byProfile && body.serve_args !== undefined ? ' The launch profile is set, so it overrides these flags while it is.' : ''}`,
      okLabel: st.installed ? 'Rewrite' : 'Add',
    });
    if (!ok) return;
    setBusy('install');
    setActionError('');
    try {
      const s = await requestJSON(`${url}/install`, { method: 'POST', body, fetchImpl });
      if (!alive.current) return;
      take(s);
      toast(st.installed ? 'App launcher rewritten' : 'App launcher added');
    } catch (e) {
      if (!alive.current) return;
      setActionError(e?.code === 'launcher_not_managed' ? `${st.path || 'That launcher'} was not written by tclaude, so tclaude leaves it alone.`
        : e?.code === 'local_only_setting' ? `${errorText(e)}`
        : errorText(e));
    } finally {
      if (alive.current) setBusy('');
    }
  };
  const uninstall = async () => {
    if (!await confirm({
      title: 'Remove the app launcher?',
      body: `tclaude removes ${files.join(', ') || 'the launcher it wrote'}. agentd keeps running.`,
      okLabel: 'Remove',
    })) return;
    setBusy('uninstall');
    setActionError('');
    try {
      const s = await requestJSON(`${url}/uninstall`, { method: 'POST', body: {}, fetchImpl });
      if (!alive.current) return;
      take(s);
      toast('App launcher removed');
    } catch (e) {
      if (alive.current) setActionError(errorText(e));
    } finally {
      if (alive.current) setBusy('');
    }
  };

  const addLabel = st.kind === 'app-bundle' ? 'Add to Applications…' : 'Add to app menu…';
  const actions = readOnly ? html`<span class="muted" id="${idp}launcher-readonly">Read only: you may see this node's launcher, not change it.</span>`
    : !canAct ? null
      : html`${status.action === 'install' && html`<${Button} variant="primary" size="sm" id="${idp}install-launcher-on" disabled=${!!busy} busy=${busy === 'install'} onClick=${() => { void install(); }}>${addLabel}<//>`}
        ${status.action === 'reinstall' && html`<${Button} variant="primary" size="sm" id="${idp}install-launcher-reinstall" disabled=${!!busy} busy=${busy === 'install'} onClick=${() => { void install(); }}>Reinstall…<//>`}
        ${st.installed && html`<${Button} variant="danger" size="sm" id="${idp}install-launcher-off" disabled=${!!busy} busy=${busy === 'uninstall'} onClick=${() => { void uninstall(); }}>Remove…<//>`}
        ${actionError && collapsed ? html`<span class="cfg-svc-warn" role="alert">${actionError}</span>` : ''}`;

  const setOn = (name, on) => { setDraft((d) => ({ ...d, on: { ...d.on, [name]: on } })); expand(); };
  const disabled = !canAct || !!busy;
  const flagsDisabled = disabled || byProfile;
  return html`<${InstallPart} idp=${idp} id="launcher" title=${title} state=${status.state} collapsed=${collapsed} onToggle=${st.supported ? toggle : null}
    status=${html`<span>${status.text}</span>`} actions=${actions}>
    ${st.supported && html`<div class="cfg-svc" id="${idp}launcher">
    <div class="cfg-section" role="group" aria-labelledby="${idp}launcher-h">
      <h3 class="ui-eyebrow ui-eyebrow-section" id="${idp}launcher-h">Launcher</h3>
      <p class="cfg-group-lede">${st.kind === 'app-bundle' ? 'An app in ~/Applications, registered with Launch Services so Spotlight and Launchpad find it.' : 'A desktop entry in the app menu, with its icon and a launcher script.'} It runs the tclaude binary agentd runs, and agentd rewrites it when that binary changes. CLI: <code>tclaude service launcher status|install|uninstall</code>.</p>
      <div class="cfg-svc-dl">
        <${Row} label="Status" id="${idp}launcher-state">${!st.installed ? 'not installed' : !st.managed ? html`<span class="cfg-svc-warn">installed by hand — tclaude leaves it alone</span>` : st.stale ? html`<span class="cfg-svc-warn">out of date${st.stale_reason ? `: ${st.stale_reason}` : ''}</span>` : 'installed'}<//>
        <${Row} label=${st.installed ? 'Files' : 'Writes'}>${files.map((f) => html`<code key=${f}>${f}</code>`)}<//>
        ${st.managed && st.exe && html`<${Row} label="Runs"><code>${st.exe}</code>${st.version ? html` <span class="muted">${st.version}</span>` : ''}<//>`}
        ${(!st.installed || st.stale) && st.current_exe && html`<${Row} label=${st.installed ? 'Would run' : 'Will run'}><code>${st.current_exe}</code>${st.current_version ? html` <span class="muted">${st.current_version}</span>` : ''}<//>`}
        <${Row} label="Start agentd"><label class="cfg-inline"><input type="checkbox" id="${idp}launcher-start-daemon" checked=${startDaemon} disabled=${disabled}
          onChange=${(e) => setStartDaemon(e.currentTarget.checked)} /> start agentd first when it is not running</label>
          <span class="muted">through the Start on login unit when there is one, else as <code>tclaude agentd serve</code></span><//>
        ${st.operator_token_store && html`<${Row} label="Sign-in token"><span class="muted" id="${idp}launcher-token">agentd keeps it ${TOKEN_STORES[st.operator_token_store] || st.operator_token_store}</span><//>`}
      </div>
    </div>

    <div class="cfg-section" role="group" aria-labelledby="${idp}launcher-flags-h">
      <h3 class="ui-eyebrow ui-eyebrow-section" id="${idp}launcher-flags-h">Serve flags</h3>
      <p class="cfg-group-lede">What the launcher starts <code>agentd serve</code> with when agentd is not running. In effect now: <span id="${idp}launcher-source">${SOURCES[st.serve_args_source] || st.serve_args_source || 'unknown'}</span>.</p>
      ${byProfile && html`<div class="cfg-svc-warn" id="${idp}launcher-by-profile">The launch profile (under Start automatically) is set, so it decides these flags: change them there. The launcher's own setting is kept, and takes over once the profile is cleared.</div>`}
      <div class="cfg-launcher-flags">
        ${LAUNCHER_FLAGS.map((f) => html`<label key=${f.name} class="cfg-launcher-flag">
          <input type="checkbox" data-launcher-flag=${f.name} checked=${!!draft.on[f.name]} disabled=${flagsDisabled} onChange=${(e) => setOn(f.name, e.currentTarget.checked)} />
          <span class="cfg-launcher-flag-text"><span>${f.label} <code class="cfg-key">${f.name}${f.value ? '=<port>' : ''}</code></span>
            <span class="muted">${f.meaning}</span>
            ${f.value && draft.on[f.name] && html`<span><input type="number" class="ui-input ui-sm" id="${idp}launcher-port" min="1" max="65535" placeholder=${DEFAULT_PORT} aria-label="Dashboard port"
              value=${draft.port} disabled=${flagsDisabled} onInput=${(e) => { const v = e.currentTarget.value; setDraft((d) => ({ ...d, port: v })); }} /></span>`}</span>
        </label>`)}
      </div>
      ${full && html`<label class="cfg-inline cfg-launcher-other">Other flags
        <input type="text" class="ui-input ui-sm" id="${idp}launcher-other" spellcheck="false" autocomplete="off" aria-label="Other agentd serve flags"
          placeholder="--dashboard-bind 127.0.0.1" value=${draft.other} disabled=${flagsDisabled} onInput=${(e) => { const v = e.currentTarget.value; setDraft((d) => ({ ...d, other: v })); }} /></label>`}
      ${!full && !readOnly && draft.other && html`<div class="muted" id="${idp}launcher-other-locked">Also: <code>${draft.other}</code>. Only the node itself or an unrestricted peer can change the flags while they include these.</div>`}
      <div class="cfg-svc-dl">
        <${Row} label="Starts with"><code id="${idp}launcher-args">agentd serve ${args.join(' ')}</code><//>
        ${st.installed && st.managed && st.installed_serve_args && !same(st.installed_serve_args, st.serve_args) && html`<${Row} label="Installed"><code>agentd serve ${st.installed_serve_args.join(' ')}</code> <span class="cfg-svc-warn">differs; reinstall to apply</span><//>`}
      </div>
      ${canAct && html`<div class="cfg-svc-actions">
        <${Button} variant=${flagsChanged ? 'primary' : undefined} size="sm" id="${idp}launcher-apply" disabled=${!!busy || (!flagsChanged && st.installed && startDaemon === (st.options?.start_daemon !== false)) || (flagsChanged && otherLocked)}
          busy=${busy === 'install'} onClick=${() => { void install(); }}>${st.installed ? 'Rewrite launcher…' : addLabel}<//>
        ${st.serve_args_source === 'launcher' && html`<${Button} size="sm" variant="ghost" id="${idp}launcher-defaults" disabled=${!!busy}
          onClick=${() => { void install({ serveArgs: null }); }}>Back to tclaude's defaults…<//>`}
        ${flagsChanged && html`<${Button} size="sm" variant="ghost" id="${idp}launcher-discard" disabled=${!!busy} onClick=${() => setDraft(flagsDraft(st.serve_args))}>Discard changes<//>`}
      </div>`}
    </div>
    ${(st.warnings || []).map((w) => html`<div key=${w} class="cfg-svc-warn">${w}</div>`)}
    ${actionError && html`<div class="cfg-svc-warn" role="alert" id="${idp}launcher-error">${actionError}</div>`}
    </div>`}
  <//>`;
}
