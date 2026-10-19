import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

// PROVIDERS preset where a provider's API lives and which header carries its
// key. "custom" leaves all of it to the operator (a corporate gateway, a
// regional endpoint).
export const PROVIDERS = {
  anthropic: { label: 'Anthropic API', dialect: 'anthropic', url: 'https://api.anthropic.com', header: 'x-api-key', prefix: '', models: ['claude-*'] },
  openai: { label: 'OpenAI API', dialect: 'openai', url: 'https://api.openai.com', header: 'Authorization', prefix: 'Bearer ', models: ['gpt-*'] },
  custom: { label: 'Custom endpoint (Anthropic- or OpenAI-compatible)' },
};

// LIMIT_FIELDS are the policy numbers the gateway refuses to serve without.
export const LIMIT_FIELDS = [
  ['daily_requests', 'requests a day, all peers'], ['daily_tokens', 'tokens a day, all peers'],
  ['peer_daily_requests', 'requests a day, per peer'], ['peer_daily_tokens', 'tokens a day, per peer'],
  ['session_daily_requests', 'requests a day, per session'], ['session_daily_tokens', 'tokens a day, per session'],
  ['max_input_tokens', 'input tokens per request'], ['max_output_tokens', 'output tokens per request'],
  ['max_concurrent', 'requests at once'], ['requests_per_minute', 'requests a minute'],
];

// LIMIT_PRESETS size the budgets for agentic coding: tokens include cache
// reads, so one busy Claude Code session can use tens of millions a day.
export const LIMIT_PRESETS = {
  light: { label: 'Light (a helper now and then)', daily_requests: 1000, daily_tokens: 50e6, peer_daily_requests: 500, peer_daily_tokens: 25e6, session_daily_requests: 300, session_daily_tokens: 15e6, max_input_tokens: 200000, max_output_tokens: 32000, max_concurrent: 2, requests_per_minute: 30 },
  standard: { label: 'Standard (a few workers a day)', daily_requests: 5000, daily_tokens: 250e6, peer_daily_requests: 2500, peer_daily_tokens: 125e6, session_daily_requests: 1000, session_daily_tokens: 50e6, max_input_tokens: 200000, max_output_tokens: 64000, max_concurrent: 4, requests_per_minute: 60 },
  heavy: { label: 'Heavy (a fleet working all day)', daily_requests: 20000, daily_tokens: 1e9, peer_daily_requests: 10000, peer_daily_tokens: 500e6, session_daily_requests: 3000, session_daily_tokens: 200e6, max_input_tokens: 1000000, max_output_tokens: 128000, max_concurrent: 8, requests_per_minute: 120 },
};

const NAME_RE = /^[A-Za-z0-9_.-]{1,64}$/;

function presetOf(policy) {
  return Object.keys(LIMIT_PRESETS).find((k) => LIMIT_FIELDS.every(([f]) => policy[f] === LIMIT_PRESETS[k][f])) || 'custom';
}

function providerOf(setup) {
  const dialect = setup.policy?.dialect || 'anthropic';
  return Object.keys(PROVIDERS).find((k) => {
    const p = PROVIDERS[k];
    return p.url && p.dialect === dialect && p.url === (setup.url || '').replace(/\/+$/, '') && p.header.toLowerCase() === (setup.header || '').toLowerCase() && p.prefix === (setup.header_prefix || '');
  }) || 'custom';
}

// gatewayDraft turns the daemon's setup view (or nothing, for a new gateway)
// into the dialog's form.
export function gatewayDraft(setup) {
  if (!setup) {
    const p = PROVIDERS.anthropic;
    const limits = LIMIT_PRESETS.standard;
    return { name: '', provider: 'anthropic', dialect: p.dialect, url: p.url, header: p.header, prefix: p.prefix, keyMode: 'paste', key: '', keyFile: '', credential: 'none', models: p.models.join(', '), preset: 'standard', limits: Object.fromEntries(LIMIT_FIELDS.map(([f]) => [f, limits[f]])), enabled: true, policy: {} };
  }
  const policy = setup.policy || {};
  return {
    name: setup.name, provider: providerOf(setup), dialect: policy.dialect || 'anthropic', url: setup.url || '', header: setup.header || '', prefix: setup.header_prefix || '',
    keyMode: setup.credential === 'file' ? 'file' : 'paste', key: '', keyFile: setup.key_file || '', storedKeyFile: setup.key_file || '', credential: setup.credential || 'none',
    models: (policy.models || []).join(', '), preset: presetOf(policy), limits: Object.fromEntries(LIMIT_FIELDS.map(([f]) => [f, policy[f] || 0])), enabled: !!policy.enabled, policy,
  };
}

export function splitModels(text) {
  return [...new Set(String(text || '').split(/[\s,]+/).map((m) => m.trim()).filter(Boolean))];
}

// gatewayBody validates the form and builds the save/test body. Policy fields
// the dialog does not edit (byte caps, lease idle, precount) round-trip.
export function gatewayBody(form, { create = false } = {}) {
  const name = form.name.trim();
  if (!NAME_RE.test(name)) return { error: 'The name is 1-64 letters, digits, . _ - (peers pick it as name@this-node).' };
  if (!/^https?:\/\/[^/?#]+/.test(form.url.trim())) return { error: 'Give the provider URL, such as https://api.anthropic.com.' };
  if (!form.header.trim()) return { error: 'Name the header that carries the key.' };
  const stored = form.credential !== 'none' && !create;
  if (form.keyMode === 'paste' && !form.key.trim() && !stored) return { error: 'Paste the provider API key.' };
  if (form.keyMode === 'file' && !/^(\/|~\/)/.test(form.keyFile.trim()) && !stored) return { error: 'Give the absolute path of the file that holds the key.' };
  const models = splitModels(form.models);
  if (!models.length) return { error: 'Allow at least one model (an exact ID or a prefix ending in *).' };
  for (const [f, what] of LIMIT_FIELDS) {
    const v = Number(form.limits[f]);
    if (!Number.isInteger(v) || v <= 0) return { error: `Set ${what} to a whole number above 0: a gateway refuses every request without it.` };
  }
  const { blocked_peers: _blocked, ...kept } = form.policy || {};
  const policy = { ...kept, dialect: form.dialect, enabled: !!form.enabled, models, ...Object.fromEntries(LIMIT_FIELDS.map(([f]) => [f, Number(form.limits[f])])) };
  if (form.dialect === 'openai') delete policy.precount_input;
  const body = { name, url: form.url.trim(), header: form.header.trim(), header_prefix: form.prefix, policy };
  if (form.keyMode === 'paste' && form.key.trim()) body.key = form.key.trim();
  // An unchanged key file keeps the stored credential (and its prefix).
  if (form.keyMode === 'file' && form.keyFile.trim() && (form.credential !== 'file' || form.keyFile.trim() !== form.storedKeyFile)) body.key_file = form.keyFile.trim();
  if (create) body.create = true;
  return { body };
}

const CREDENTIAL_TEXT = {
  managed: 'A key is stored for this gateway. Leave blank to keep it.',
  file: 'The key is read from the file below.',
  inline: 'The key is written inline in config. Paste one here to move it to a private key file.',
};

// GatewayDialog creates or edits one model gateway. The key is sent once and
// never shown again; the connection test spends no tokens.
export function GatewayDialog({ setup, actions, confirm, onClose, onDone }) {
  const create = !setup;
  const [form, setForm] = useState(() => gatewayDraft(setup));
  const [error, setError] = useState('');
  const [test, setTest] = useState(null);
  const set = (k) => (e) => setForm({ ...form, [k]: e.currentTarget.type === 'checkbox' ? e.currentTarget.checked : e.currentTarget.value });
  const setProvider = (e) => {
    const provider = e.currentTarget.value;
    const p = PROVIDERS[provider];
    setTest(null);
    setForm(p.url ? { ...form, provider, dialect: p.dialect, url: p.url, header: p.header, prefix: p.prefix, models: create || !form.models ? p.models.join(', ') : form.models } : { ...form, provider });
  };
  const setPreset = (e) => {
    const preset = e.currentTarget.value;
    setForm({ ...form, preset, limits: preset === 'custom' ? form.limits : Object.fromEntries(LIMIT_FIELDS.map(([f]) => [f, LIMIT_PRESETS[preset][f]])) });
  };
  const setLimit = (f) => (e) => setForm({ ...form, preset: 'custom', limits: { ...form.limits, [f]: e.currentTarget.value === '' ? '' : Number(e.currentTarget.value) } });
  const allowed = splitModels(form.models);
  const toggleModel = (id) => setForm({ ...form, models: (allowed.includes(id) ? allowed.filter((m) => m !== id) : [...allowed, id]).join(', ') });

  const runTest = () => {
    const { body, error: e } = gatewayBody({ ...form, models: form.models || 'x', limits: Object.fromEntries(LIMIT_FIELDS.map(([f]) => [f, 1])) }, { create });
    if (e) { setError(e); return; }
    setError('');
    setTest({ busy: true });
    actions.testModelGateway(body).then(setTest).catch((x) => setTest({ ok: false, message: errText(x) }));
  };
  const save = () => {
    const { body, error: e } = gatewayBody(form, { create });
    if (e) { setError(e); return; }
    setError('');
    const limits = `${form.limits.daily_tokens.toLocaleString()} tokens and ${form.limits.daily_requests.toLocaleString()} requests a day in all`;
    confirm({
      title: create ? `Add model gateway ${body.name}?` : `Save model gateway ${body.name}?`,
      body: body.policy.enabled
        ? `Peers this node trusts unrestricted, and peers granted models.proxy for ${body.name}, can send model requests through it, charged to this provider account: ${body.policy.models.join(', ')}, up to ${limits}.${body.key ? ' The key is stored in a private file on this node and is never shown again.' : ''}`
        : `The gateway is saved but stays off: no peer can use it until you enable it.${body.key ? ' The key is stored in a private file on this node and is never shown again.' : ''}${!create && setup.policy?.enabled ? ' Its active leases are revoked.' : ''}`,
      okLabel: create ? 'Add' : 'Save', busyLabel: 'Saving…',
      action: () => actions.saveModelGateway(body.name, body),
    }).then((r) => { if (r) onDone(`Model gateway ${body.name} saved`); })
      .catch((x) => setError(errText(x)));
  };
  const custom = form.provider === 'custom';
  return html`<${Overlay} id="fleet-model-gateway" labelledby="fleet-model-gateway-title" onClose=${onClose}>
    <h3 id="fleet-model-gateway-title">${create ? 'Add a model gateway' : `Edit model gateway ${setup.name}`}</h3>
    <div class="muted">Lets workers on your other nodes use this node's provider API key without the key leaving this node. It needs an API key from the provider console; a Claude or ChatGPT subscription login cannot be shared.</div>
    <label class="fa-jb-row"><span class="fa-k">name</span><input id="fleet-mg-name" value=${form.name} disabled=${!create} placeholder="e.g. anthropic" autocomplete="off" spellcheck="false" onInput=${set('name')} /></label>
    <label class="fa-jb-row"><span class="fa-k">provider</span><select id="fleet-mg-provider" value=${form.provider} onChange=${setProvider}>
      ${Object.entries(PROVIDERS).map(([k, p]) => html`<option key=${k} value=${k}>${p.label}</option>`)}
    </select></label>
    ${custom && html`<div class="muted">Any endpoint serving the Anthropic Messages or OpenAI Responses API with a key in one header, such as a LiteLLM proxy or a company gateway. Chat Completions-only APIs and signed credentials (Bedrock, Vertex) need a translating proxy in front.</div>`}
    ${custom && html`<label class="fa-jb-row"><span class="fa-k">API</span><select id="fleet-mg-dialect" value=${form.dialect} onChange=${set('dialect')}>
      <option value="anthropic">Anthropic Messages (Claude Code)</option>
      <option value="openai">OpenAI Responses (Codex, Copilot)</option>
    </select></label>`}
    <label class="fa-jb-row"><span class="fa-k">URL</span><input id="fleet-mg-url" value=${form.url} disabled=${!custom} autocomplete="off" spellcheck="false" onInput=${set('url')} /></label>
    ${custom && html`<label class="fa-jb-row"><span class="fa-k">header</span><input id="fleet-mg-header" value=${form.header} placeholder="x-api-key or Authorization" autocomplete="off" spellcheck="false" onInput=${set('header')} /></label>
    <label class="fa-jb-row"><span class="fa-k">prefix</span><input id="fleet-mg-prefix" value=${form.prefix} placeholder='before the key, e.g. "Bearer "' autocomplete="off" spellcheck="false" onInput=${set('prefix')} /></label>`}
    <div class="fa-jb-row"><span class="fa-k">API key</span><div>
      <label><input type="radio" name="fleet-mg-keymode" checked=${form.keyMode === 'paste'} onChange=${() => setForm({ ...form, keyMode: 'paste' })} /> paste</label>
      <label><input type="radio" name="fleet-mg-keymode" data-mode="file" checked=${form.keyMode === 'file'} onChange=${() => setForm({ ...form, keyMode: 'file' })} /> read from a file</label>
      ${form.keyMode === 'paste'
        ? html`<input id="fleet-mg-key" type="password" value=${form.key} placeholder=${form.credential === 'none' ? 'provider API key' : 'stored; leave blank to keep'} autocomplete="new-password" spellcheck="false" onInput=${set('key')} />`
        : html`<input id="fleet-mg-keyfile" value=${form.keyFile} placeholder="/abs/path or ~/path of a file holding only the key" autocomplete="off" spellcheck="false" onInput=${set('keyFile')} />`}
      ${!create && CREDENTIAL_TEXT[form.credential] && html`<div class="muted">${CREDENTIAL_TEXT[form.credential]}</div>`}
    </div></div>
    <div class="fa-jb-row"><span></span><div>
      <button id="fleet-mg-test" type="button" disabled=${test?.busy} onClick=${runTest}>${test?.busy ? 'Testing…' : 'Test the key'}</button>
      ${test && !test.busy && html` <span id="fleet-mg-test-result" class=${test.ok ? '' : 'fa-danger'}>${test.message}</span>`}
    </div></div>
    <label class="fa-jb-row"><span class="fa-k">models</span><input id="fleet-mg-models" value=${form.models} placeholder="exact IDs or prefixes ending in *, e.g. claude-sonnet-*" autocomplete="off" spellcheck="false" onInput=${set('models')} /></label>
    ${test?.ok && (test.models || []).length > 0 && html`<div class="fa-jb-row"><span></span><div id="fleet-mg-offered">
      <span class="muted">The provider offers (click to allow or drop): </span>
      ${test.models.map((m) => html`<button key=${m} type="button" class=${`fa-chip${allowed.includes(m) ? ' on' : ''}`} data-model=${m} aria-pressed=${allowed.includes(m)} onClick=${() => toggleModel(m)}>${m}</button> `)}
    </div></div>`}
    <label class="fa-jb-row"><span class="fa-k">limits</span><select id="fleet-mg-preset" value=${form.preset} onChange=${setPreset}>
      ${Object.entries(LIMIT_PRESETS).map(([k, p]) => html`<option key=${k} value=${k}>${p.label}</option>`)}
      <option value="custom">Custom</option>
    </select></label>
    <div class="fa-jb-row"><span></span><div class="fa-mg-limits">
      ${LIMIT_FIELDS.map(([f, what]) => html`<label key=${f}><input type="number" min="1" step="1" data-limit=${f} value=${form.limits[f]} onInput=${setLimit(f)} /> <span class="muted">${what}</span></label>`)}
    </div></div>
    <div class="fa-jb-row"><span></span><div class="muted">Tokens include cache reads: a busy Claude Code session can use tens of millions a day. Requests over a limit are refused, never billed.</div></div>
    <label class="fa-jb-row"><span class="fa-k">on</span><span><input id="fleet-mg-enabled" type="checkbox" checked=${form.enabled} onChange=${set('enabled')} /> serve peers now</span></label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">Restricted peers also need a <code>models.proxy</code> grant for this gateway (Peer grants), and their worker a matching permission on their side.</div>
    <div class="modal-buttons"><span class="spacer"></span>
      <button id="fleet-mg-save" type="button" class="primary" onClick=${save}>${create ? 'Add…' : 'Save…'}</button>
      <button type="button" onClick=${onClose}>Close</button></div>
  </${Overlay}>`;
}
