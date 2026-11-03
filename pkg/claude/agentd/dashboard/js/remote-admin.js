import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { shellConfirm as confirmModal, shellToast as toast } from './shell-state.js';
import { dashboardState } from './snapshot-store.js';

const html = htm.bind(h);

// ===================================================================
// Remote access — certificate management (Config tab, JOH-278).
//
// Served over both the loopback dashboard and the remote (mTLS +
// passphrase) listener — a remote session is already a full
// control-plane operator, so cert management is consistent with that
// privilege level (requireCertAdmin = the standard dashboard auth).
//
// The section reads /api/remote-access/info (material state, server-cert
// SANs, issued devices) and drives the add-host / add-device / setup
// forms. Cert material is handled host-side; this UI never sees private
// keys except as an opaque .p12 download stream.
// ===================================================================

// postJSON POSTs a JSON body and returns the parsed response, throwing the
// server's plain-text error (the endpoints http.Error on failure) on non-2xx.
async function postJSON(path, body) {
  const r = await fetch(path, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  });
  if (!r.ok) {
    const t = await r.text().catch(() => '');
    throw new Error((t || ('HTTP ' + r.status)).trim());
  }
  return r.json().catch(() => ({}));
}

// fmtExpiry renders a device cert's not_after; a Go zero time (year < 2000)
// or unparseable value shows nothing rather than "1/1/1".
export function fmtExpiry(s) {
  if (!s) return '';
  const d = new Date(s);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '';
  return 'expires ' + d.toLocaleDateString();
}

function stateText(info, error) {
  if (error) return 'Failed to load remote-access info: ' + error;
  if (!info) return '';
  if (!info.material_exists) return 'No material generated yet — use “First-time setup” below to create the CA, server cert, first device, and passphrase.';
  let s = 'Material present. ';
  if (info.running) s += `Listener live on https://${info.running_bind}.`;
  else if (info.enabled) s += 'Enabled — restart agentd to start the listener.';
  else s += 'Not enabled (see the “Remote access” toggle above).';
  return s;
}

// Fields stay raw <input>s, like every Config field (ConfigInput): the
// .cfg-field / .ra-form-grid skin paints them as one form.
const hint = (text) => html`<span class="cfg-hint" style="padding-left:0">${text}</span>`;
const EMPTY_SETUP = { bind: '', hosts: '', passphrase: '', p12_password: '', client_name: '', enable: true, regenerate: false };

// RemoteAdminPanel reloads /api/remote-access/info each time the Config tab
// opens, so a restart or another operator's change shows on return.
export function RemoteAdminPanel({ activeTab = dashboardState.activeTab }) {
  const [info, setInfo] = useState(null);
  const [loadError, setLoadError] = useState('');
  const [msg, setMsg] = useState(null);
  const [hosts, setHosts] = useState('');
  const [device, setDevice] = useState({ name: '', pw: '' });
  const [setup, setSetup] = useState(EMPTY_SETUP);
  const [setupOpen, setSetupOpen] = useState(false);

  const load = async () => {
    try {
      const r = await fetch('/api/remote-access/info', { credentials: 'same-origin' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const next = await r.json();
      setInfo(next);
      setLoadError('');
      if (!next.material_exists) setSetupOpen(true); // guide the operator straight to setup
    } catch (e) { setLoadError(e.message || String(e)); }
  };
  useEffect(() => activeTab.subscribe((tab) => { if (tab === 'config') void load(); }), [activeTab]);

  const fail = (text) => setMsg({ text, err: true });
  // run posts one form, clears the message, and reloads the panel; a failure
  // shows in the panel's error banner, prefixed with what was attempted.
  const run = async (label, request, done) => {
    try {
      done(await request());
      setMsg(null);
      await load();
    } catch (e) { fail(`${label} failed: ` + (e.message || e)); }
  };

  const addHosts = () => {
    const value = hosts.trim();
    if (!value) return fail('Enter one or more host names.');
    return run('Add host names', () => postJSON('/api/remote-access/add-hosts', { hosts: value }), () => {
      setHosts('');
      toast('Server cert reissued — restart agentd to serve it');
    });
  };
  const addClient = () => {
    const name = device.name.trim();
    if (!name || !device.pw) return fail('A device name and a .p12 password are required.');
    return run('Add device', () => postJSON('/api/remote-access/add-client', { name, p12_password: device.pw }), (res) => {
      setDevice({ name: '', pw: '' });
      toast(`Device “${res.name || name}” issued`);
    });
  };
  const runSetup = async () => {
    const body = { ...setup, bind: setup.bind.trim(), hosts: setup.hosts.trim(), client_name: setup.client_name.trim() };
    if (body.regenerate && !await confirmModal({
      title: 'Regenerate all remote-access material?',
      body: 'This rotates the CA and INVALIDATES every client certificate already installed on a device — each device must reinstall a fresh .p12.',
      okLabel: 'Regenerate',
    })) return;
    if (!body.bind) return fail('A bind address is required (e.g. 0.0.0.0:8443).');
    if (!body.passphrase) return fail('A login passphrase is required.');
    if (!body.p12_password) return fail('A device .p12 password is required.');
    await run('Setup', () => postJSON('/api/remote-access/setup', body), (res) => {
      setSetup((s) => ({ ...s, passphrase: '', p12_password: '' }));
      toast(res.enabled ? 'Material generated + enabled — restart agentd' : 'Material generated');
    });
  };

  const field = (key) => ({ value: setup[key], onInput: (e) => { const value = e.currentTarget.value; setSetup((s) => ({ ...s, [key]: value })); } });
  const check = (key) => ({ checked: setup[key], onChange: (e) => { const value = e.currentTarget.checked; setSetup((s) => ({ ...s, [key]: value })); } });
  const sans = info?.sans || [];
  const clients = info?.clients || [];
  return html`<div class="cfg-section" id="cfg-remote-admin">
    <h3 class="ui-eyebrow ui-eyebrow-section">Remote access — certificate management</h3>
    <p class="cfg-hint" style="padding-left:0">
      Generate and manage the remote-access secret material (CA, device
      <code>.p12</code> bundles, the login passphrase) without dropping to the
      <code>tclaude remote-access</code> CLI.
    </p>
    ${msg && html`<div id="ra-admin-msg" class=${`ra-msg${msg.err ? ' err' : ''}`}>${msg.text}</div>`}

    <div class="cfg-field">
      <span class="cfg-label">Material</span>
      <span class="cfg-hint" id="ra-admin-state" style="padding-left:0">${stateText(info, loadError)}</span>
    </div>

    <div class="cfg-field">
      <span class="cfg-label">Cert valid for</span>
      <div style="flex-basis:100%">
        <div id="ra-admin-sans" class="ra-chips">
          ${sans.length ? sans.map((s) => html`<span key=${s} class="ra-chip">${s}</span>`) : hint('—')}
        </div>
        ${hint('The host names / IPs a device may dial without a certificate-name mismatch (the server cert\'s SANs).')}
        <div class="ra-inline-form">
          <input type="text" id="ra-addhosts-input" placeholder="myhost.tailnet.ts.net, example.com" aria-label="Additional host names" autocomplete="off" spellcheck="false"
            value=${hosts} onInput=${(e) => setHosts(e.currentTarget.value)} />
          <${Button} id="ra-addhosts-btn" onClick=${addHosts}>Add host name(s)<//>
        </div>
        <span class="cfg-hint" style="padding-left:0">Adds SANs to the <strong>server</strong> cert (keeps the existing CA), so a public URL / new name verifies — <em>installed devices keep working</em>, no reinstall. Restart agentd to serve the updated cert.</span>
      </div>
    </div>

    <div class="cfg-field">
      <span class="cfg-label">Devices</span>
      <div style="flex-basis:100%">
        <div id="ra-admin-devices" class="ra-devices">
          ${clients.length ? clients.map((d) => html`<div key=${d.name} class="ra-device">
            <span class="ra-device-name">${d.name}</span><span class="ra-device-exp">${fmtExpiry(d.not_after)}</span>
            ${d.has_p12
              ? html`<a class="ui-btn ui-btn-link ui-sm ra-link" href=${`/api/remote-access/client?name=${encodeURIComponent(d.name)}`} download>Download .p12</a>`
              : hint('.p12 removed')}
          </div>`) : hint('No devices issued yet.')}
        </div>
        <div class="ra-inline-form">
          <input type="text" id="ra-addclient-name" placeholder="device name (e.g. phone)" aria-label="New device name" autocomplete="off" spellcheck="false"
            value=${device.name} onInput=${(e) => { const value = e.currentTarget.value; setDevice((d) => ({ ...d, name: value })); }} />
          <input type="password" id="ra-addclient-pw" placeholder=".p12 password" aria-label="New device .p12 password" autocomplete="new-password"
            value=${device.pw} onInput=${(e) => { const value = e.currentTarget.value; setDevice((d) => ({ ...d, pw: value })); }} />
          <${Button} id="ra-addclient-btn" onClick=${addClient}>Add device<//>
        </div>
        <span class="cfg-hint" style="padding-left:0">Issues a new <code>.p12</code> from the existing CA, then download it to install on the device.${' '}
          <a id="ra-ca-btn" class="ui-btn ui-btn-link ui-sm ra-link" href="/api/remote-access/ca.crt" download>Download CA cert</a></span>
      </div>
    </div>

    <details class="cfg-advanced" id="ra-setup-details" open=${setupOpen} onToggle=${(e) => setSetupOpen(e.currentTarget.open)}>
      <summary>First-time setup / regenerate material</summary>
      <div class="cfg-advanced-body">
        <p class="cfg-hint">Generate the CA + server cert + first device, set the login passphrase, and (optionally) enable the listener. If material already exists, <strong>regenerate</strong> rotates everything including the CA — this <strong>invalidates every installed device</strong> and they must reinstall.</p>
        <div class="ra-form-grid">
          <label>Bind <input type="text" id="ra-setup-bind" placeholder="0.0.0.0:8443" autocomplete="off" spellcheck="false" ...${field('bind')} /></label>
          <label>Extra host names <input type="text" id="ra-setup-hosts" placeholder="myhost.tailnet.ts.net (optional)" autocomplete="off" spellcheck="false" ...${field('hosts')} /></label>
          <label>Login passphrase <input type="password" id="ra-setup-pass" autocomplete="new-password" ...${field('passphrase')} /></label>
          <label>Device .p12 password <input type="password" id="ra-setup-p12pw" autocomplete="new-password" ...${field('p12_password')} /></label>
          <label>First device name <input type="text" id="ra-setup-client" placeholder="phone" autocomplete="off" spellcheck="false" ...${field('client_name')} /></label>
        </div>
        <label class="cfg-inline"><input type="checkbox" id="ra-setup-enable" ...${check('enable')} /> enable in config.json (restart agentd to start the listener)</label>
        <label class="cfg-inline"><input type="checkbox" id="ra-setup-regenerate" ...${check('regenerate')} /> regenerate even though material exists (rotates the CA — invalidates installed devices)</label>
        <div class="ra-inline-form"><${Button} variant="primary" id="ra-setup-btn" onClick=${runSetup}>Generate material<//></div>
      </div>
    </details>
  </div>`;
}
