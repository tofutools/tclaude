import { $, esc } from './helpers.js';
import { toast } from './refresh.js';

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

// triggerDownload fetches a same-origin file endpoint via a transient <a>, so
// the browser honours the server's Content-Disposition (attachment) and the
// session cookie + Referer ride along for the auth check.
function triggerDownload(url) {
  const a = document.createElement('a');
  a.href = url;
  a.download = ''; // let the endpoint's Content-Disposition name the file
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

function adminMsg(text, isErr) {
  const el = $('#ra-admin-msg');
  if (!el) return;
  el.textContent = text;
  el.classList.toggle('err', !!isErr);
  el.style.display = text ? 'block' : 'none';
}

// fmtExpiry renders a device cert's not_after; a Go zero time (year < 2000)
// or unparseable value shows nothing rather than "1/1/1".
function fmtExpiry(s) {
  if (!s) return '';
  const d = new Date(s);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '';
  return 'expires ' + d.toLocaleDateString();
}

function renderSANs(sans) {
  const c = $('#ra-admin-sans');
  if (!c) return;
  if (!sans || !sans.length) { c.innerHTML = '<span class="cfg-hint" style="padding-left:0">—</span>'; return; }
  c.innerHTML = sans.map(s => `<span class="ra-chip">${esc(s)}</span>`).join('');
}

function renderDevices(clients) {
  const c = $('#ra-admin-devices');
  if (!c) return;
  if (!clients || !clients.length) {
    c.innerHTML = '<span class="cfg-hint" style="padding-left:0">No devices issued yet.</span>';
    return;
  }
  c.innerHTML = clients.map(d => {
    const name = esc(d.name);
    const exp = fmtExpiry(d.not_after);
    const dl = d.has_p12
      ? `<a class="ra-link" href="/api/remote-access/client?name=${encodeURIComponent(d.name)}" download>Download .p12</a>`
      : `<span class="cfg-hint" style="padding-left:0">.p12 removed</span>`;
    return `<div class="ra-device"><span class="ra-device-name">${name}</span>` +
      `<span class="ra-device-exp">${esc(exp)}</span>${dl}</div>`;
  }).join('');
}

function renderState(info) {
  const el = $('#ra-admin-state');
  if (!el) return;
  if (!info.material_exists) {
    el.textContent = 'No material generated yet — use “First-time setup” below to create the CA, server cert, first device, and passphrase.';
    const det = $('#ra-setup-details');
    if (det) det.open = true; // guide the operator straight to setup
    return;
  }
  let s = 'Material present. ';
  if (info.running) s += `Listener live on https://${info.running_bind}.`;
  else if (info.enabled) s += 'Enabled — restart agentd to start the listener.';
  else s += 'Not enabled (see the “Remote access” toggle above).';
  el.textContent = s;
}

// loadRemoteAdmin fetches /api/remote-access/info and renders the section.
// Called on Config-tab load.
async function loadRemoteAdmin() {
  const section = $('#cfg-remote-admin');
  if (!section) return;
  section.style.display = '';
  try {
    const r = await fetch('/api/remote-access/info', { credentials: 'same-origin' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const info = await r.json();
    renderState(info);
    renderSANs(info.sans);
    renderDevices(info.clients);
  } catch (e) {
    const el = $('#ra-admin-state');
    if (el) el.textContent = 'Failed to load remote-access info: ' + (e.message || e);
  }
}

async function onAddHosts() {
  const inp = $('#ra-addhosts-input');
  const hosts = (inp.value || '').trim();
  if (!hosts) { adminMsg('Enter one or more host names.', true); return; }
  try {
    await postJSON('/api/remote-access/add-hosts', { hosts });
    inp.value = '';
    adminMsg('');
    toast('Server cert reissued — restart agentd to serve it');
    await loadRemoteAdmin();
  } catch (e) { adminMsg('Add host names failed: ' + (e.message || e), true); }
}

async function onAddClient() {
  const name = ($('#ra-addclient-name').value || '').trim();
  const pw = $('#ra-addclient-pw').value || '';
  if (!name || !pw) { adminMsg('A device name and a .p12 password are required.', true); return; }
  try {
    const res = await postJSON('/api/remote-access/add-client', { name, p12_password: pw });
    $('#ra-addclient-name').value = '';
    $('#ra-addclient-pw').value = '';
    adminMsg('');
    toast(`Device “${res.name || name}” issued`);
    await loadRemoteAdmin();
  } catch (e) { adminMsg('Add device failed: ' + (e.message || e), true); }
}

async function onSetup() {
  const regenerate = $('#ra-setup-regenerate').checked;
  if (regenerate && !window.confirm(
    'Regenerate ALL remote-access material?\n\nThis rotates the CA and INVALIDATES every client certificate already installed on a device — each device must reinstall a fresh .p12.')) {
    return;
  }
  const body = {
    bind: ($('#ra-setup-bind').value || '').trim(),
    hosts: ($('#ra-setup-hosts').value || '').trim(),
    passphrase: $('#ra-setup-pass').value || '',
    p12_password: $('#ra-setup-p12pw').value || '',
    client_name: ($('#ra-setup-client').value || '').trim(),
    regenerate,
    enable: $('#ra-setup-enable').checked,
  };
  if (!body.bind) { adminMsg('A bind address is required (e.g. 0.0.0.0:8443).', true); return; }
  if (!body.passphrase) { adminMsg('A login passphrase is required.', true); return; }
  if (!body.p12_password) { adminMsg('A device .p12 password is required.', true); return; }
  try {
    const res = await postJSON('/api/remote-access/setup', body);
    $('#ra-setup-pass').value = '';
    $('#ra-setup-p12pw').value = '';
    adminMsg('');
    toast(res.enabled ? 'Material generated + enabled — restart agentd' : 'Material generated');
    await loadRemoteAdmin();
  } catch (e) { adminMsg('Setup failed: ' + (e.message || e), true); }
}

// bindRemoteAdmin wires the section's buttons once (idempotent). Called from
// the Config tab's one-time bind.
let wired = false;
function bindRemoteAdmin() {
  if (wired) return;
  wired = true;
  const on = (id, fn) => { const el = $('#' + id); if (el) el.addEventListener('click', fn); };
  on('ra-addhosts-btn', onAddHosts);
  on('ra-addclient-btn', onAddClient);
  on('ra-setup-btn', onSetup);
  on('ra-ca-btn', () => triggerDownload('/api/remote-access/ca.crt'));
}

export { bindRemoteAdmin, loadRemoteAdmin };
// dashboard-imperative-boundary: config-adapter
