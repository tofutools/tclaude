import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

const LABEL_RE = /^[A-Za-z0-9._-]{1,64}$/;
const HUB_RE = /^wss?:\/\/[^\s/?#]+(\/[^\s]*)?$/;

function errText(error) { return error?.message || String(error); }

export function splitLabels(text) {
  return [...new Set(String(text || '').split(/[\s,]+/).map((x) => x.trim()).filter(Boolean))];
}

// hubBody is the /api/federation/config body: only changed fields are sent
// (an omitted field keeps its value).
export function hubBody(self, form) {
  const body = {};
  if (form.hubURL.trim() !== (self.hubURL || '')) body.hub_url = form.hubURL.trim();
  if (form.name.trim() && form.name.trim() !== self.name) body.name = form.name.trim();
  if (form.invite.trim()) body.invite = form.invite.trim();
  if (form.caFile.trim()) body.hub_ca_file = form.caFile.trim();
  if (form.connect !== self.enabled) body.enabled = form.connect;
  return body;
}

// NodeSettingsDialog edits this node's hub connection (tclaude federation
// connect) and its node labels (tclaude federation node-labels).
export function NodeSettingsDialog({ self, actions, confirm, toast, onClose, onDone }) {
  const [form, setForm] = useState({ hubURL: self.hubURL || '', name: self.name === 'this node' ? '' : self.name, invite: '', caFile: '', connect: self.enabled || !self.hubURL });
  const [labels, setLabels] = useState(null);
  const [labelText, setLabelText] = useState('');
  const [error, setError] = useState('');
  useEffect(() => {
    actions.nodeLabels().then((l) => { setLabels(l); setLabelText(l.join(', ')); }).catch((e) => setLabels({ error: errText(e) }));
  }, []);
  const set = (k) => (e) => setForm({ ...form, [k]: e.currentTarget.type === 'checkbox' ? e.currentTarget.checked : e.currentTarget.value });

  const saveHub = () => {
    const body = hubBody(self, form);
    if (!Object.keys(body).length) { setError('Nothing changed.'); return; }
    if (form.hubURL.trim() && !HUB_RE.test(form.hubURL.trim())) { setError('The hub URL is wss://host[:port] (ws:// only to a loopback hub).'); return; }
    if (form.connect && !form.hubURL.trim()) { setError('Set a hub URL to connect.'); return; }
    if (body.hub_ca_file && !body.hub_ca_file.startsWith('/')) { setError('The CA file is an absolute path on this node (the daemon reads it).'); return; }
    setError('');
    const url = form.hubURL.trim();
    const moving = 'hub_url' in body && self.hubURL && self.enabled;
    confirm({
      title: !form.connect ? 'Disconnect from the hub?' : moving ? `Move this node to the hub at ${url}?` : self.enabled ? 'Update the hub connection?' : `Connect to the hub at ${url}?`,
      body: !form.connect
        ? 'No peer can reach this node and this node cannot reach any peer until you connect again: remote views, mail, attach and remote jobs stop in both directions. Trusted peers and grants are kept.'
        : `This node connects to ${url}${body.name ? ` as ${body.name}` : ''}. The hub and every node on it see this node's name, instance ID and fingerprint and can ask to be trusted; nothing here is shared with a peer until you trust it and grant it something.${moving ? ' The current hub connection drops; peers reachable only through the old hub become unreachable.' : ''}${body.invite ? ' The invite token is single-use.' : ''}${body.hub_ca_file ? ` TLS to the hub trusts the certificates in ${body.hub_ca_file}.` : ''}`,
      okLabel: !form.connect ? 'Disconnect' : self.enabled ? 'Save' : 'Connect',
      busyLabel: 'Saving…',
      action: () => actions.setHubConfig(body),
    }).then((r) => { if (r) onDone(!form.connect ? 'Disconnected from the hub' : 'Hub settings saved; connecting…'); })
      .catch((e) => setError(errText(e)));
  };

  const saveLabels = () => {
    const want = splitLabels(labelText);
    const bad = want.find((l) => !LABEL_RE.test(l));
    if (bad) { setError(`Label ${bad}: 1-64 letters, digits, . _ -`); return; }
    const have = Array.isArray(labels) ? labels : [];
    const add = want.filter((l) => !have.includes(l));
    const remove = have.filter((l) => !want.includes(l));
    if (!add.length && !remove.length) { setError('Labels unchanged.'); return; }
    setError('');
    confirm({
      title: 'Change this node\'s labels?',
      body: `${add.length ? `Adds ${add.join(', ')}. ` : ''}${remove.length ? `Removes ${remove.join(', ')}. ` : ''}Peers that can read this node (node.read) see its labels, and automatic placement of spawns and jobs uses them (require label=…): work asking for a label you remove stops landing here.`,
      okLabel: 'Save labels',
      busyLabel: 'Saving…',
      action: () => actions.setNodeLabels({ add, remove }),
    }).then((r) => {
      if (!r) return;
      toast('Node labels saved', false);
      actions.nodeLabels().then((l) => { setLabels(l); setLabelText(l.join(', ')); }).catch(() => {});
    }).catch((e) => setError(errText(e)));
  };

  return html`<${Overlay} id="fleet-node-settings" labelledby="fleet-node-settings-title" onClose=${onClose}>
    <h3 id="fleet-node-settings-title">Node settings</h3>
    <h4 class="fa-ns-h">Hub connection</h4>
    <label class="fa-ns-row"><span class="fa-k">hub URL</span><input id="fleet-hub-url" value=${form.hubURL} placeholder="wss://hub.example:8470" autocomplete="off" spellcheck="false" onInput=${set('hubURL')} /></label>
    <label class="fa-ns-row"><span class="fa-k">name</span><input id="fleet-hub-name" value=${form.name} placeholder="user@hostname (default)" autocomplete="off" onInput=${set('name')} /></label>
    <label class="fa-ns-row"><span class="fa-k">invite</span><input id="fleet-hub-invite" type="password" value=${form.invite} placeholder="single-use token from the hub admin (if it needs one)" autocomplete="off" onInput=${set('invite')} /></label>
    <label class="fa-ns-row"><span class="fa-k">CA file</span><input id="fleet-hub-ca" value=${form.caFile} placeholder="/abs/path/hub-ca.pem on this node (optional; leave empty to keep)" autocomplete="off" spellcheck="false" onInput=${set('caFile')} /></label>
    <label class="fa-ns-check"><input id="fleet-hub-connect" type="checkbox" checked=${form.connect} onChange=${set('connect')} /> connected</label>
    <div class="fa-ns-actions"><button id="fleet-hub-save" type="button" class="primary" onClick=${saveHub}>${self.enabled ? 'Save…' : 'Connect…'}</button>
      <span class="muted">CLI: <code>tclaude federation connect URL [--name …] [--invite …] [--ca-file …]</code>, <code>disconnect</code></span></div>
    <h4 class="fa-ns-h">Node labels</h4>
    <div class="muted">Used by automatic placement (<code>require label=…</code>) and shown to peers that can read this node.</div>
    ${labels?.error ? html`<div class="fa-danger">${labels.error}</div>` : !labels ? html`<div class="muted">Loading…</div>` : html`
      <label class="fa-ns-row"><span class="fa-k">labels</span><input id="fleet-node-labels" value=${labelText} placeholder="gpu, ci, linux" autocomplete="off" spellcheck="false" onInput=${(e) => setLabelText(e.currentTarget.value)} /></label>
      <div class="fa-ns-actions"><button id="fleet-labels-save" type="button" onClick=${saveLabels}>Save labels…</button>
        <span class="muted">CLI: <code>tclaude federation node-labels [--add …] [--remove …]</code></span></div>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}
