import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { HUB_CONSEQUENCE } from './fleet-admin-run.js';
import { HubBoards } from './fleet-admin-boards.js';
import { NodeUpdateDialog } from './node-update.js';

const html = htm.bind(h);

// Fleet → Hub administers the tclaude-hub this node is connected to
// (tclaude federation hub …). Requests are signed by this node's key and
// relayed over its hub connection; hub admin grants no access to any node's
// content. Every mutation is confirmed first.

// An instance ID is inst_ plus 26 base32 characters of its key hash, and its
// fingerprint is that same hash in groups of four (proto.InstanceFingerprint),
// so a pasted fingerprint can be checked against the ID it claims to belong to.
const INSTANCE_RE = /^inst_[a-z2-7]{26}$/;
const SPACE_RE = /^[A-Za-z0-9._-]{1,64}$/;
const LOG_PAGE = 200;

function errText(error) { return error?.message || String(error); }

// absent is a hub route this daemon does not have (404) or an older hub does
// not know (400 "operation"): the feature is missing, not failing.
function absent(error) {
  return error?.status === 404 || (error?.status === 400 && error?.code === 'operation');
}

function when(iso) {
  const t = new Date(iso);
  return Number.isFinite(t.getTime()) && t.getFullYear() > 1 ? t.toLocaleString() : '—';
}

function bytes(n) {
  if (!(n >= 0)) return '—';
  return n < 1 << 20 ? `${(n / 1024).toFixed(0)} KiB` : n < 1 << 30 ? `${(n / (1 << 20)).toFixed(1)} MiB` : `${(n / (1 << 30)).toFixed(2)} GiB`;
}

export function instanceFingerprint(instance) {
  return INSTANCE_RE.test(instance) ? instance.slice(5).match(/.{1,4}/g).join('-') : '';
}

export function splitSpaces(text) {
  return [...new Set(String(text || '').split(/[\s,]+/).map((x) => x.trim()).filter(Boolean))];
}

// settingValue shows a setting's value with its unit.
export function settingValue(s, v = s?.effective) {
  if (v == null || v === '') return '—';
  return s?.unit ? `${v} ${s.unit}` : String(v);
}

// parseSetting validates an edited value against the descriptor; it returns
// {value} or {error}.
export function parseSetting(s, raw) {
  const text = String(raw ?? '').trim();
  if (s.type === 'bool') return text === 'true' || text === 'false' ? { value: text === 'true' } : { error: `${s.key}: true or false` };
  if (s.type === 'int' || s.type === 'duration' || s.type === 'number') {
    if (!/^-?\d+(\.\d+)?$/.test(text)) return { error: `${s.key}: a number${s.unit ? ` of ${s.unit}` : ''}` };
    const n = Number(text);
    if (s.type !== 'number' && !Number.isInteger(n)) return { error: `${s.key}: a whole number${s.unit ? ` of ${s.unit}` : ''}` };
    if (s.min != null && n < s.min) return { error: `${s.key}: at least ${settingValue(s, s.min)}` };
    if (s.max != null && n > s.max) return { error: `${s.key}: at most ${settingValue(s, s.max)}` };
    return { value: n };
  }
  if (!text) return { error: `${s.key}: a value` };
  return { value: text };
}

// settingsPlan turns the edited form into the PATCH overrides and the
// confirmation lines; a reverted setting (null) falls back to the hub's
// serve flag or built-in default.
export function settingsPlan(settings, edits) {
  const overrides = {};
  const lines = [];
  for (const s of settings) {
    if (!(s.key in edits)) continue;
    const e = edits[s.key];
    if (e === null) {
      if (s.source !== 'remote' && s.source !== 'db') continue;
      overrides[s.key] = null;
      lines.push(`${s.key}: back to the ${s.boot != null ? `serve flag or default (${settingValue(s, s.boot)})` : 'default'}`);
      continue;
    }
    const p = parseSetting(s, e);
    if (p.error) return { error: p.error };
    if (p.value === s.effective) continue;
    overrides[s.key] = p.value;
    lines.push(`${s.key}: ${settingValue(s)} → ${settingValue(s, p.value)}${s.source === 'flag' ? ' (overrides the serve flag)' : ''}${s.restart_required ? ' — takes effect after a hub restart' : ''}`);
  }
  return { overrides, lines };
}

function ClaimDialog({ self, actions, confirm, onClose, onDone }) {
  const [token, setToken] = useState('');
  const [error, setError] = useState('');
  const claim = () => {
    const t = token.trim();
    if (!t) { setError('Paste the claim token from the hub host.'); return; }
    setError('');
    confirm({
      title: 'Claim hub admin?',
      body: `Binds hub admin to this node's key: ${self.name} (${self.id}, fingerprint ${self.fingerprint}). The token is single-use and is consumed. As admin this node can admit and revoke instances, change hub settings and add or remove other admins; it gains no access to any node's content (the hub only relays ciphertext).`,
      okLabel: 'Claim admin',
      busyLabel: 'Claiming…',
      action: () => actions.hubClaim(t),
    }).then((r) => { if (r) onDone('This node is now a hub admin'); })
      .catch((e) => setError(e?.code === 'claim_used' ? 'That token was already used.' : e?.code === 'claim_expired' ? 'That token has expired. A fresh one is generated on the next hub start while the hub has no admins.' : errText(e)));
  };
  return html`<${Overlay} id="fleet-hub-claim" labelledby="fleet-hub-claim-title" onClose=${onClose}>
    <h3 id="fleet-hub-claim-title">Claim hub admin</h3>
    <p>The hub prints a one-time admin claim token on its first start and writes it to a file only the hub user can read. Paste it here. It works once and expires after 24 hours.</p>
    <label class="fa-pe-row"><span class="fa-k">claim token</span><input id="fleet-hub-claim-token" type="password" value=${token} autocomplete="off" spellcheck="false" onInput=${(e) => setToken(e.currentTarget.value)} onKeyDown=${(e) => { if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); claim(); } }} /></label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation hub claim TOKEN</code>. Lost every admin key? Only <code>tclaude-hub admin reset</code> on the hub host recovers it.</div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Cancel</button><button type="button" class="primary" id="fleet-hub-claim-send" onClick=${claim}>Claim…</button></div>
  </${Overlay}>`;
}

// fingerprintError checks a pasted fingerprint against the one derived from
// the instance ID it is said to belong to.
function fingerprintError(instance, typed) {
  const derived = instanceFingerprint(instance);
  if (!typed) return `Paste ${instance}'s full fingerprint, compared with its operator over another channel.`;
  if (typed.toLowerCase().replace(/[\s-]/g, '') !== derived.replace(/-/g, '')) return `That fingerprint does not belong to ${instance}; its fingerprint is ${derived}. Check the ID and the fingerprint with its operator again.`;
  return '';
}

function rowText(a) {
  return a ? `${a.name ? `${a.name} ` : ''}${a.instance} (fingerprint ${a.fingerprint || instanceFingerprint(a.instance) || 'unknown'}${(a.spaces || []).length ? `, spaces ${a.spaces.join(', ')}` : ''})` : 'none';
}

// IdentityDialog recovers a lost instance's admission onto a new instance, or
// revokes a rotated instance's predecessor. Each previews on the hub first and
// applies only after a confirm that shows the hub's warning.
function IdentityDialog({ actions, confirm, onClose, onDone }) {
  const [mode, setMode] = useState('recover');
  const [form, setForm] = useState({ old: '', next: '', fingerprint: '' });
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setForm({ ...form, [k]: e.currentTarget.value });
  const recover = mode === 'recover';
  const run = () => {
    const old = form.old.trim();
    const next = form.next.trim();
    const fp = form.fingerprint.trim();
    if (!INSTANCE_RE.test(old)) { setError(`The ${recover ? 'lost' : 'predecessor'} instance ID is inst_ followed by 26 characters.`); return; }
    if (recover && !INSTANCE_RE.test(next)) { setError('The replacement instance ID is inst_ followed by 26 characters.'); return; }
    if (recover && next === old) { setError('The replacement must be a different instance.'); return; }
    const bad = fingerprintError(recover ? next : old, fp);
    if (bad) { setError(bad); return; }
    setError(''); setBusy(true);
    const preview = recover ? actions.recoverHubIdentity(old, next) : actions.revokeOldHubIdentity(old);
    // The confirm and the apply use the fingerprint derived from the ID the
    // operator checked, never one the hub echoes; a preview for anything else
    // is refused.
    const nextFp = instanceFingerprint(next);
    const oldFp = instanceFingerprint(old);
    preview.then((p) => {
      const echoed = recover ? [p?.new, p?.old?.instance, p?.new_fingerprint] : [p?.instance, p?.fingerprint];
      const want = recover ? [next, old, nextFp] : [old, oldFp];
      if (echoed.some((v, i) => v != null && v !== want[i])) throw new Error('The hub previewed a different identity than the one entered; nothing was changed.');
      const oldSpaces = (p?.old?.spaces || []).join(', ') || 'none';
      return confirm(recover ? {
        title: `Recover ${old}'s admission onto ${next}?`,
        body: `Revokes the old admission ${old} (fingerprint ${oldFp}) and its admin capabilities, and admits ${next} with fingerprint ${nextFp} in its place with the old spaces (${oldSpaces})${p?.replacement ? `, replacing its current spaces (${(p.replacement.spaces || []).join(', ') || 'none'})` : ''}. Admin authority is not transferred, and each node's trust in ${next} still needs recovering there.${p?.warning ? ` Hub: ${p.warning}` : ''}`,
        okLabel: 'Recover',
        busyLabel: 'Recovering…',
        action: () => actions.recoverHubIdentity(old, next, nextFp),
      } : {
        title: `Revoke the old identity ${old}?`,
        body: `${old} (fingerprint ${oldFp}) is revoked as a rotated predecessor and can no longer connect, and any pending automatic rotation from it is cancelled. An already accepted successor stays current.${p?.warning ? ` Hub: ${p.warning}` : ''}`,
        okLabel: 'Revoke old identity',
        busyLabel: 'Revoking…',
        action: () => actions.revokeOldHubIdentity(old, oldFp),
      });
    })
      .then((r) => { if (r) onDone(recover ? `${old} recovered onto ${next}` : `${old} revoked`); })
      .catch((e) => setError(errText(e)))
      .finally(() => setBusy(false));
  };
  return html`<${Overlay} id="fleet-hub-identity" labelledby="fleet-hub-identity-title" onClose=${onClose} blocked=${busy}>
    <h3 id="fleet-hub-identity-title">Hub identity recovery</h3>
    <div class="fa-pe-row" role="radiogroup" aria-label="Action">
      <label><input type="radio" name="fleet-hub-identity-mode" id="fleet-hub-identity-recover" checked=${recover} onClick=${() => setMode('recover')} /> Recover a lost instance onto a new one</label>
      <label><input type="radio" name="fleet-hub-identity-mode" id="fleet-hub-identity-revoke" checked=${!recover} onClick=${() => setMode('revoke')} /> Revoke a rotated instance's old identity</label>
    </div>
    <label class="fa-pe-row"><span class="fa-k">${recover ? 'lost instance' : 'old instance'}</span><input id="fleet-hub-identity-old" value=${form.old} placeholder="inst_…" autocomplete="off" spellcheck="false" onInput=${set('old')} /></label>
    ${recover && html`<label class="fa-pe-row"><span class="fa-k">new instance</span><input id="fleet-hub-identity-new" value=${form.next} placeholder="inst_…" autocomplete="off" spellcheck="false" onInput=${set('next')} /></label>`}
    <label class="fa-pe-row"><span class="fa-k">${recover ? 'new fingerprint' : 'old fingerprint'}</span><input id="fleet-hub-identity-fp" value=${form.fingerprint} placeholder="full fingerprint" autocomplete="off" spellcheck="false" onInput=${set('fingerprint')} /></label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation hub identity recover OLD NEW</code> · <code>tclaude federation hub identity revoke-old INSTANCE</code></div>
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Cancel</button><button type="button" class="primary" id="fleet-hub-identity-send" disabled=${busy} onClick=${run}>${recover ? 'Recover…' : 'Revoke…'}</button></div>
  </${Overlay}>`;
}

function AdmitDialog({ peers, actions, confirm, onClose, onDone }) {
  const [form, setForm] = useState({ instance: '', fingerprint: '', spaces: '' });
  const [error, setError] = useState('');
  const set = (k) => (e) => setForm({ ...form, [k]: e.currentTarget.value });
  const known = peers.find((p) => p.id === form.instance.trim());
  const admit = () => {
    const instance = form.instance.trim();
    const fp = form.fingerprint.trim();
    const spaces = splitSpaces(form.spaces);
    if (!INSTANCE_RE.test(instance)) { setError('The instance ID is inst_… as the instance reports it (tclaude federation identity on that node).'); return; }
    const fpError = fingerprintError(instance, fp);
    if (fpError) { setError(fpError); return; }
    const derived = instanceFingerprint(instance);
    if (known?.fingerprint && known.fingerprint !== derived) { setError(`${instance} does not match the fingerprint this node knows for it (${known.fingerprint}).`); return; }
    const bad = spaces.find((s) => !SPACE_RE.test(s));
    if (bad) { setError(`Space ${bad}: letters, digits, . _ -`); return; }
    setError('');
    confirm({
      title: `Admit ${instance} to the hub?`,
      body: `${instance} with fingerprint ${derived} may connect to the hub${spaces.length ? ` and join spaces ${spaces.join(', ')}` : ''}. Admission lets it reach other nodes through the hub; each node still decides for itself whether to trust it.`,
      okLabel: 'Admit',
      busyLabel: 'Admitting…',
      action: () => actions.admitToHub(instance, spaces),
    }).then((r) => { if (r) onDone(`${instance} admitted`); }).catch((e) => setError(errText(e)));
  };
  return html`<${Overlay} id="fleet-hub-admit" labelledby="fleet-hub-admit-title" onClose=${onClose}>
    <h3 id="fleet-hub-admit-title">Admit an instance</h3>
    <label class="fa-pe-row"><span class="fa-k">instance</span><input id="fleet-hub-admit-instance" value=${form.instance} placeholder="inst_…" autocomplete="off" spellcheck="false" onInput=${set('instance')} /></label>
    <label class="fa-pe-row"><span class="fa-k">fingerprint</span><input id="fleet-hub-admit-fp" value=${form.fingerprint} placeholder="full fingerprint" autocomplete="off" spellcheck="false" onInput=${set('fingerprint')} /></label>
    <label class="fa-pe-row"><span class="fa-k">spaces</span><input id="fleet-hub-admit-spaces" value=${form.spaces} placeholder="space, …" autocomplete="off" spellcheck="false" onInput=${set('spaces')} /></label>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="modal-buttons"><span class="spacer"></span><button type="button" onClick=${onClose}>Cancel</button><button type="button" class="primary" id="fleet-hub-admit-send" onClick=${admit}>Admit…</button></div>
  </${Overlay}>`;
}

function SettingsSection({ settings, actions, confirm, toast, reload }) {
  const [edits, setEdits] = useState({});
  const [error, setError] = useState('');
  // Clearing a field drops the edit rather than saving an empty value.
  const edit = (key, v) => setEdits((cur) => {
    const next = { ...cur };
    if (v === '') delete next[key]; else next[key] = v;
    return next;
  });
  const save = () => {
    const plan = settingsPlan(settings, edits);
    if (plan.error) { setError(plan.error); return; }
    if (!plan.lines.length) { setError('Nothing changed.'); return; }
    setError('');
    confirm({
      title: 'Change hub settings?',
      body: `${plan.lines.join('; ')}. These apply to every node connected to the hub. Remote settings are stored in the hub's database and take precedence over its serve flags.`,
      okLabel: 'Save settings',
      busyLabel: 'Saving…',
      action: () => actions.patchHubSettings(plan.overrides),
    }).then((r) => { if (r) { setEdits({}); toast('Hub settings saved', false); reload(); } }).catch((e) => setError(errText(e)));
  };
  return html`<table class="fa-table" id="fleet-hub-settings">
      <thead><tr><th>Setting</th><th>Effective</th><th>Source</th><th>Serve flag / default</th><th>New value</th><th></th></tr></thead>
      <tbody>${settings.map((s) => html`<tr key=${s.key} data-setting=${s.key}>
        <td><code>${s.key}</code>${s.description ? html`<div class="muted">${s.description}</div>` : ''}</td>
        <td>${settingValue(s)}${s.restart_required ? html` <span class="muted" title="Takes effect after a hub restart">⟳</span>` : ''}</td>
        <td>${s.source}${s.flag_overridden ? html` <span class="fa-warn" data-flag-overridden title="A serve flag sets this too; the remote setting wins">flag overridden</span>` : ''}</td>
        <td class="muted">${settingValue(s, s.boot)}</td>
        <td>${s.remote_writable === false
          ? html`<span class="muted" data-host-only>set on the hub host</span>`
          : s.type === 'bool'
          ? html`<select aria-label=${`New ${s.key}`} value=${s.key in edits && edits[s.key] !== null ? edits[s.key] : ''} onChange=${(e) => edit(s.key, e.currentTarget.value)}><option value="">—</option><option value="true">true</option><option value="false">false</option></select>`
          : html`<input aria-label=${`New ${s.key}`} inputmode=${s.type === 'string' ? null : 'numeric'} value=${s.key in edits && edits[s.key] !== null ? edits[s.key] : ''} placeholder=${s.min != null || s.max != null ? `${s.min ?? ''}–${s.max ?? ''}` : ''} onInput=${(e) => edit(s.key, e.currentTarget.value)} />`}</td>
        <td class="fa-acts">${(s.source === 'remote' || s.source === 'db') && html`<button type="button" class="fa-link" data-revert=${s.key} onClick=${() => edit(s.key, null)}>${edits[s.key] === null ? 'reverts on save' : 'revert'}</button>`}</td>
      </tr>`)}</tbody>
    </table>
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    <div class="fa-ns-actions"><button type="button" id="fleet-hub-settings-save" onClick=${save}>Save settings…</button>
      <span class="muted">CLI: <code>tclaude federation hub settings [--set key=value] [--unset key]</code></span></div>`;
}

// RemoteScripts shows the hub's accept switch, which only the hub host can
// turn on, and opens the Run page on the hub when this node may use it.
function RemoteScripts({ actions, onRunHub }) {
  const [st, setSt] = useState(null);
  useEffect(() => {
    let off = false;
    actions.hubRunStatus().then((v) => { if (!off) setSt(v || {}); }).catch((e) => { if (!off) setSt({ error: e }); });
    return () => { off = true; };
  }, []);
  if (!st) return html`<div class="muted">Loading…</div>`;
  if (st.error) return html`<div class="muted" id="fleet-hub-scripts">${st.error.status === 403 ? 'Remote scripts on the hub need hub.exec.' : absent(st.error) ? 'Remote scripts on the hub are not available on this build of tclaude or the hub.' : errText(st.error)}</div>`;
  const on = !!st.accept_remote_scripts;
  return html`<div id="fleet-hub-scripts" class="fa-hub-scripts">
    <div>
      <span class=${on ? 'fa-warn' : 'muted'} data-accept=${on ? 'on' : 'off'}>${on ? `⚠ accepts scripts from admins with hub.exec (set by ${st.switch_source || 'the hub host'})` : 'off'}</span>
      · <span class="muted">${st.can_exec ? 'this node holds hub.exec' : 'this node does not hold hub.exec'}</span>
      ${on && st.can_exec && html` · <button type="button" class="fa-link" id="fleet-hub-run-open" onClick=${onRunHub}>run a script on the hub…</button>`}
    </div>
    ${st.warning && html`<div class="fa-warn">${st.warning}</div>`}
    <div class="muted">Only the hub host can turn this on: <code>accept_remote_scripts</code> in hub-config.json beside the hub database, or <code>tclaude-hub serve --accept-remote-scripts</code>; it can never be switched from here.${st.can_exec ? '' : html` hub.exec is granted on the hub host too: <code>tclaude-hub admin grant-exec ${'<instance>'}</code>.`} Scripts run as the hub's service user${st.service_user ? ` (${st.service_user})` : ''} with bounded output and runtime, and the hub audits the full script. ${HUB_CONSEQUENCE}</div>
  </div>`;
}

// AuditSection is the hub's audit trail, newest first. Exec entries show the
// script as plain text, and only to holders of hub.exec; the hub redacts it
// for everyone else.
function AuditSection({ actions }) {
  const [entries, setEntries] = useState(null);
  const [cursor, setCursor] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [unavailable, setUnavailable] = useState(false);
  const load = (from = '') => {
    setBusy(true); setError('');
    actions.hubAudit(from)
      .then((r) => { const got = r?.entries || []; setEntries((prev) => (from ? [...(prev || []), ...got] : got)); setCursor(got.length ? r?.next_cursor || null : null); })
      .catch((e) => {
        if (absent(e)) { setEntries([]); setCursor(null); setUnavailable(true); return; }
        setError(e?.status === 403 ? 'Reading the hub audit needs a hub admin capability this node does not hold.' : errText(e)); if (!from) setEntries([]);
      })
      .finally(() => setBusy(false));
  };
  useEffect(() => load(), []);
  const detail = (e) => {
    const d = e.detail || {};
    const bits = [];
    if (d.from_version || d.to_version) bits.push(`${d.from_version || '?'} → ${d.to_version || '?'}`);
    if (d.exit_code != null) bits.push(`exit ${d.exit_code}`);
    if (d.timed_out) bits.push('timed out');
    return bits.join(' · ');
  };
  return html`<div id="fleet-hub-audit">
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    ${unavailable ? html`<div class="muted">The hub audit is not available on this build of tclaude or the hub.</div>` : entries == null ? html`<div class="muted">Loading…</div>` : !entries.length ? (!error && html`<div class="muted">No audit entries.</div>`) : html`<table class="fa-table">
      <thead><tr><th>When</th><th>Actor</th><th>Action</th><th>Outcome</th><th>Detail</th></tr></thead>
      <tbody>${entries.map((e, i) => html`<tr key=${e.id || i} data-kind=${e.kind || ''}>
        <td class="fa-nowrap">${when(e.at)}</td>
        <td><code>${e.actor || '—'}</code></td>
        <td>${e.kind || '—'}</td>
        <td class=${/fail|denied|refused|rolled/.test(e.outcome || '') ? 'fa-danger' : ''}>${e.outcome || '—'}</td>
        <td>${detail(e)}
          ${e.kind === 'exec' && (e.detail?.script != null
            ? html`<details class="fa-hub-script"><summary>script (${e.detail.script.length} chars)</summary><pre class="fa-bi-text">${e.detail.script}</pre></details>`
            : html` <span class="muted" data-redacted>the script needs hub.exec${e.detail?.script_sha256 ? html` · sha256 <code>${String(e.detail.script_sha256).slice(0, 12)}…</code>` : ''}</span>`)}</td>
      </tr>`)}</tbody></table>`}
    <div class="fa-ns-actions">
      <button type="button" disabled=${busy} onClick=${() => load()}>Refresh</button>
      ${cursor && html`<button type="button" id="fleet-hub-audit-more" disabled=${busy} onClick=${() => load(cursor)}>Load more</button>`}
      <span class="muted">CLI: <code>tclaude federation hub audit</code></span></div>
  </div>`;
}

function LogsSection({ actions }) {
  const [entries, setEntries] = useState(null);
  const [cursor, setCursor] = useState(null);
  const [caughtUp, setCaughtUp] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const load = (from = '') => {
    setBusy(true); setError('');
    actions.hubLogs(from, LOG_PAGE)
      .then((r) => {
        const got = r?.entries || [];
        setEntries((prev) => (from ? [...(prev || []), ...got] : got));
        // The hub always returns the resume cursor, even at the end, and a page
        // can stop short of LOG_PAGE on its byte budget; only an empty page
        // means nothing newer yet.
        setCursor(r?.next_cursor || null);
        setCaughtUp(!!from && !got.length);
      })
      .catch((e) => setError(errText(e)))
      .finally(() => setBusy(false));
  };
  useEffect(() => load(), []);
  return html`<div id="fleet-hub-logs">
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
    ${entries == null ? html`<div class="muted">Loading…</div>` : !entries.length ? html`<div class="muted">No log entries.</div>`
      : html`<pre class="fa-bi-text fa-hub-log">${entries.map((e) => `${when(e.at)}  ${String(e.level || '').toUpperCase().padEnd(5)}  ${e.message || ''}`).join('\n')}</pre>`}
    <div class="fa-ns-actions">
      <button type="button" id="fleet-hub-logs-refresh" disabled=${busy} onClick=${() => load()}>Refresh</button>
      ${cursor && html`<button type="button" id="fleet-hub-logs-more" disabled=${busy} onClick=${() => load(cursor)}>Load more</button>`}
      ${caughtUp && html`<span class="muted" id="fleet-hub-logs-end">nothing newer yet</span>`}
      <span class="muted">Redacted on the hub; CLI: <code>tclaude federation hub logs</code></span></div>
  </div>`;
}

// HubPage is Fleet → Hub: the hub's status and health, its admissions,
// invites and admins, its settings and a log tail. Without admin it offers
// only the claim.
export function HubPage({ view, actions, updateActions, confirm, toast, copy, timers = globalThis, onRunHub }) {
  const [status, setStatus] = useState(null);
  const [data, setData] = useState({});
  const [dialog, setDialog] = useState(null);
  const [tick, setTick] = useState(0);
  const [invite, setInvite] = useState({ space: '', ttl: '24' });
  const [newToken, setNewToken] = useState(null);
  const reload = () => setTick((n) => n + 1);
  useEffect(() => {
    let off = false;
    actions.hubStatus().then((s) => {
      if (off) return;
      setStatus(s);
      if (!s?.admin) return;
      const part = (k, p) => p.then((v) => { if (!off) setData((d) => ({ ...d, [k]: v })); }).catch((e) => { if (!off) setData((d) => ({ ...d, [k]: { error: errText(e) } })); });
      part('health', actions.hubHealth());
      part('admissions', actions.hubAdmissions());
      part('invites', actions.hubInvites());
      part('admins', actions.hubAdmins());
      part('settings', actions.hubSettings());
    }).catch((e) => { if (!off) setStatus({ error: errText(e) }); });
    return () => { off = true; };
  }, [tick]);
  const self = view.self;
  const done = (msg) => { setDialog(null); toast(msg, false); reload(); };
  const fail = (what) => (e) => toast(`${what} failed: ${errText(e)}`, true);

  if (!status) return html`<div class="empty">Loading…</div>`;
  if (status.error) return html`<div class="fa-danger" role="alert">${status.error}</div>`;
  const ok = (v) => Array.isArray(v);
  const listOr = (v, render) => (v?.error ? html`<div class="fa-danger">${v.error}</div>` : !v ? html`<div class="muted">Loading…</div>` : render(v));

  const revokeAdmission = (a) => confirm({
    title: `Revoke ${a.name || a.instance}'s hub admission?`,
    body: `${a.instance} (fingerprint ${a.fingerprint || 'unknown'}) is disconnected from the hub and can no longer reach any node through it until admitted again. Its trust relationships on each node are unchanged.`,
    okLabel: 'Revoke admission',
    busyLabel: 'Revoking…',
    action: () => actions.revokeHubAdmission(a.instance),
  }).then((r) => { if (r) done(`${a.instance}'s admission revoked`); }).catch(fail('Revoke'));
  const editSpaces = (a, text) => {
    const spaces = splitSpaces(text);
    const bad = spaces.find((s) => !SPACE_RE.test(s));
    if (bad) { toast(`Space ${bad}: letters, digits, . _ -`, true); reload(); return; }
    confirm({
      title: `Change ${a.name || a.instance}'s spaces?`,
      body: `${a.instance} will be in ${spaces.length ? spaces.join(', ') : 'no spaces'} (was ${(a.spaces || []).join(', ') || 'none'}). It can reach only nodes that share a space with it.`,
      okLabel: 'Save spaces',
      busyLabel: 'Saving…',
      action: () => actions.setHubSpaces(a.instance, spaces),
    }).then((r) => { if (r) done('Spaces saved'); else reload(); }).catch((e) => { fail('Spaces')(e); reload(); });
  };
  const createInvite = () => {
    const space = invite.space.trim();
    const hours = Number(invite.ttl);
    if (space && !SPACE_RE.test(space)) { toast('Space: letters, digits, . _ -', true); return; }
    if (!(hours >= 1 && hours <= 24 * 7)) { toast('Expiry: 1 hour to 7 days', true); return; }
    confirm({
      title: 'Create a hub invite?',
      body: `Anyone holding the token can join the hub${space ? ` into space ${space}` : ' into its default space'} once, within ${hours} hours. It is shown only once; send it over a channel you trust.`,
      okLabel: 'Create invite',
      busyLabel: 'Creating…',
      action: () => actions.createHubInvite(space, Math.round(hours * 3600)),
    }).then((r) => { if (r) { setNewToken(r); reload(); } }).catch(fail('Invite'));
  };
  const revokeInvite = (i) => confirm({
    title: 'Revoke this invite?',
    body: `The invite${i.space ? ` for space ${i.space}` : ''} (expires ${when(i.expires_at)}) stops working. Instances that already joined with it stay admitted.`,
    okLabel: 'Revoke invite',
    busyLabel: 'Revoking…',
    action: () => actions.revokeHubInvite(i.token_hash),
  }).then((r) => { if (r) done('Invite revoked'); }).catch(fail('Revoke'));
  const removeAdmin = (a) => {
    const me = a.instance === self.id;
    if (!(status.admin_count > 1)) { toast('The last admin cannot be removed here; add another admin first.', true); return; }
    confirm({
      title: me ? 'Give up hub admin for this node?' : `Remove ${a.name || a.instance} as hub admin?`,
      body: me
        ? `This node (${self.id}) can no longer administer the hub; another admin has to add it back. ${status.admin_count - 1} admin(s) remain.`
        : `${a.instance} (fingerprint ${a.fingerprint || 'unknown'}) can no longer administer the hub. Its admission is unchanged. ${status.admin_count - 1} admin(s) remain.`,
      okLabel: me ? 'Give up admin' : 'Remove admin',
      busyLabel: 'Removing…',
      action: () => actions.removeHubAdmin(a.instance),
    }).then((r) => { if (r) done(me ? 'This node is no longer a hub admin' : 'Admin removed'); }).catch(fail('Remove'));
  };
  // A new admin gets exactly this node's capabilities, named in the confirm.
  const caps = status.my_capabilities || [];
  const addAdmin = (a) => confirm({
    title: `Make ${a.name || a.instance} a hub admin?`,
    body: `${a.instance} (fingerprint ${a.fingerprint || 'unknown'}) gets these hub admin capabilities: ${caps.join(', ')}.${caps.includes('hub.admins.manage') ? ' With hub.admins.manage it can add or remove admins, including this node.' : ''} Hub admin grants no access to any node's content.`,
    okLabel: 'Make admin',
    busyLabel: 'Saving…',
    action: () => actions.addHubAdmin(a.instance, caps),
  }).then((r) => { if (r) done(`${a.instance} is now a hub admin`); }).catch(fail('Add admin'));

  const h4 = (t, extra = '') => html`<h4 class="fa-h">${t}${extra}</h4>`;
  const admins = ok(data.admins) ? data.admins : [];
  return html`<div class="fa-hub-page" id="fleet-hub">
    <div class="fa-hub-head">
      <span><span class="fa-k">hub</span> <code>${status.hub_url || self.hubURL || '—'}</code></span>
      <span><span class="fa-k">version</span> ${status.hub_version || '—'}${status.admin && html` <button type="button" class="fa-link" id="fleet-hub-update-open" onClick=${() => setDialog('update')}>Updates</button>`}</span>
      <span><span class="fa-k">id</span> <code>${status.hub_id || '—'}</code></span>
      <span class=${status.connected ? '' : 'fa-danger'}>${status.connected ? 'connected' : 'not connected'}</span>
      <span>${status.admin ? html`<b>you are a hub admin</b> <span class="muted">(${status.admin_count} admin${status.admin_count === 1 ? '' : 's'})</span>` : html`<span class="muted">not a hub admin</span>`}</span>
    </div>
    ${!status.admin ? html`<div class="fa-hub-claim">
        <p>${status.bootstrap_claimable ? 'This hub has no admin yet. Claim it with the one-time token the hub printed on its first start.' : 'Only a hub admin can manage the hub. An existing admin can add this node, or redeem a claim token if the hub has none.'}</p>
        <button type="button" class="primary" id="fleet-hub-claim-open" disabled=${!status.connected} onClick=${() => setDialog('claim')}>Claim hub admin…</button>
      </div>`
    : html`
      ${h4('Health')}
      ${listOr(data.health, (hl) => html`<div class="fa-hub-health" id="fleet-hub-health">
        <span><span class="fa-k">connected</span> ${hl.connected_instances ?? '—'}</span>
        <span><span class="fa-k">streams</span> ${hl.streams ?? '—'}</span>
        <span><span class="fa-k">goroutines</span> ${hl.load?.goroutines ?? hl.goroutines ?? '—'}</span>
        <span><span class="fa-k">heap</span> ${bytes(hl.load?.heap_bytes ?? hl.heap_bytes)}</span>
        ${hl.uptime_seconds != null && html`<span><span class="fa-k">up</span> ${Math.round(hl.uptime_seconds / 3600)} h</span>`}
        ${(hl.recent_errors || []).length ? html`<ul class="fa-hub-errors">${hl.recent_errors.map((e, i) => html`<li key=${i} class="fa-danger">${when(e.at)} ${e.code ? html`<code>${e.code}</code> ` : ''}${e.message || ''}</li>`)}</ul>` : html`<span class="muted">no recent errors</span>`}
      </div>`)}
      ${h4('Admissions', html` <button type="button" class="fa-link" id="fleet-hub-admit-open" onClick=${() => setDialog('admit')}>admit…</button> <button type="button" class="fa-link" id="fleet-hub-identity-open" onClick=${() => setDialog('identity')}>identity recovery…</button>`)}
      ${listOr(data.admissions, (rows) => html`<table class="fa-table" id="fleet-hub-admissions">
        <thead><tr><th>Instance</th><th>Fingerprint</th><th>Spaces</th><th>Seen</th><th></th></tr></thead>
        <tbody>${rows.map((a) => (a.revoked ? html`<tr key=${a.instance} data-instance=${a.instance} data-revoked class="muted">
          <td>${a.name || ''} <code>${a.instance}</code></td>
          <td><code>${a.fingerprint || '—'}</code></td>
          <td>${(a.spaces || []).join(', ') || '—'}</td>
          <td class="fa-nowrap">revoked</td><td></td>
        </tr>` : html`<tr key=${a.instance} data-instance=${a.instance}>
          <td>${a.name || ''} <code>${a.instance}</code>${a.instance === self.id ? html` <span class="muted">(this node)</span>` : ''}</td>
          <td><code>${a.fingerprint || '—'}</code></td>
          <td><input class="fa-hub-spaces" aria-label=${`Spaces for ${a.instance}`} value=${(a.spaces || []).join(', ')} onChange=${(e) => editSpaces(a, e.currentTarget.value)} /></td>
          <td class="fa-nowrap">${a.connected ? 'connected' : when(a.last_seen)}</td>
          <td class="fa-acts">${ok(data.admins) && caps.length > 0 && !admins.some((x) => x.instance === a.instance) && html`<button type="button" data-hub="make-admin" onClick=${() => addAdmin(a)}>Make admin…</button>`}
            ${a.instance !== self.id && html`<button type="button" data-hub="revoke" onClick=${() => revokeAdmission(a)}>Revoke…</button>`}</td>
        </tr>`))}</tbody></table>`)}
      ${h4('Invites')}
      <div class="fa-grant-form">
        <input id="fleet-hub-invite-space" placeholder="space (optional)" value=${invite.space} onInput=${(e) => setInvite({ ...invite, space: e.currentTarget.value })} />
        <label>expires in <input id="fleet-hub-invite-ttl" inputmode="numeric" style="width:4em" value=${invite.ttl} onInput=${(e) => setInvite({ ...invite, ttl: e.currentTarget.value })} /> h</label>
        <button type="button" id="fleet-hub-invite-create" onClick=${createInvite}>Create invite…</button>
      </div>
      ${newToken?.token && html`<div class="fa-hub-token" id="fleet-hub-new-token">Invite token, shown once: <code>${newToken.token}</code>
        <button type="button" class="fa-link" onClick=${() => copy(newToken.token).then(() => toast('Invite token copied', false)).catch(() => toast('Copy it from the page', true))}>copy</button>
        <button type="button" class="fa-link" onClick=${() => setNewToken(null)}>hide</button></div>`}
      ${listOr(data.invites, (rows) => (!rows.length ? html`<div class="muted">No open invites.</div>` : html`<table class="fa-table" id="fleet-hub-invites">
        <thead><tr><th>Token</th><th>Space</th><th>Created</th><th>Expires</th><th>Used</th><th></th></tr></thead>
        <tbody>${rows.map((i) => html`<tr key=${i.token_hash} data-token=${i.token_hash}>
          <td><code>${String(i.token_hash || '').slice(0, 12)}…</code></td><td>${i.space || '—'}</td><td>${when(i.created_at)}</td><td>${when(i.expires_at)}</td>
          <td>${i.used ? (i.used_by || 'yes') : 'no'}</td>
          <td class="fa-acts">${!i.used && html`<button type="button" data-hub="revoke-invite" onClick=${() => revokeInvite(i)}>Revoke…</button>`}</td>
        </tr>`)}</tbody></table>`))}
      ${h4('Admins')}
      ${listOr(data.admins, (rows) => html`<table class="fa-table" id="fleet-hub-admins">
        <thead><tr><th>Instance</th><th>Fingerprint</th><th>Capabilities</th><th>Added</th><th></th></tr></thead>
        <tbody>${rows.map((a) => html`<tr key=${a.instance} data-instance=${a.instance}>
          <td>${a.name || ''} <code>${a.instance}</code>${a.instance === self.id ? html` <span class="muted">(this node)</span>` : ''}</td>
          <td><code>${a.fingerprint || '—'}</code></td>
          <td class="fa-wrap">${(a.capabilities || []).join(', ') || '—'}</td>
          <td class="fa-nowrap">${when(a.added_at)}${a.added_by ? html` <span class="muted">by ${a.added_by}</span>` : ''}</td>
          <td class="fa-acts"><button type="button" data-hub="remove-admin" disabled=${status.admin_count <= 1} title=${status.admin_count <= 1 ? 'The last admin cannot be removed' : ''} onClick=${() => removeAdmin(a)}>${a.instance === self.id ? 'Give up admin…' : 'Remove…'}</button></td>
        </tr>`)}</tbody></table>`)}
      ${h4('Settings')}
      ${listOr(data.settings, (rows) => html`<${SettingsSection} settings=${rows} actions=${actions} confirm=${confirm} toast=${toast} reload=${reload} />`)}
      ${caps.includes('hub.boards.manage') && html`${h4('Boards')}<${HubBoards} actions=${actions} confirm=${confirm} toast=${toast} />`}
      ${h4('Remote scripts')}
      <${RemoteScripts} actions=${actions} onRunHub=${onRunHub} />
      ${h4('Audit')}
      <${AuditSection} actions=${actions} />
      ${h4('Log tail')}
      <${LogsSection} actions=${actions} />`}
    <div class="muted fa-cli-note">CLI: <code>tclaude federation hub status|admissions|invites|admins|settings|health|logs</code>. Hub admin never sees node content; the hub relays ciphertext only.</div>
    ${dialog === 'claim' && html`<${ClaimDialog} self=${self} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${dialog === 'update' && html`<${NodeUpdateDialog} node=${{ id: 'hub', label: 'the hub', hub: true }} actions=${updateActions} confirm=${confirm} toast=${toast} timers=${timers} onClose=${() => { setDialog(null); reload(); }} />`}
    ${dialog === 'identity' && html`<${IdentityDialog} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
    ${dialog === 'admit' && html`<${AdmitDialog} peers=${[...view.trusted, ...view.waiting]} actions=${actions} confirm=${confirm} onClose=${() => setDialog(null)} onDone=${done} />`}
  </div>`;
}
