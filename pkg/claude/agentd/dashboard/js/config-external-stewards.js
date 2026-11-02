import { h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { writeClipboard } from './insecure-origin.js';
import { shellConfirm } from './shell-state.js';

const html = htm.bind(h);

const ROOT = '/api/external-stewards';

async function call(fetchImpl, path, body) {
  const options = { credentials: 'same-origin' };
  if (body) {
    options.method = 'POST';
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(body);
  }
  const response = await fetchImpl(path, options);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}

function expiryLabel(credential) {
  const date = new Date(credential.expires_at).toLocaleDateString();
  if (credential.live) return `Expires ${date}`;
  return new Date(credential.expires_at) < new Date() ? `Expired ${date}` : 'Ended';
}

// The credential exists in the browser only here, and only until Done. The mask
// keeps it out of anything a Steward can capture from this page.
function IssuedCredential({ issued, onDone, copy }) {
  const [copied, setCopied] = useState(false);
  const copyIt = async () => {
    if (await copy(issued.credential)) setCopied(true);
  };
  return html`<div class="cfg-steward-issued" role="status">
    <strong>Copy the credential for ${issued.name} now. It can't be shown again.</strong>
    <code class="cfg-steward-secret" data-capture-mask>${issued.credential}</code>
    <div class="cfg-steward-row-actions">
      <button type="button" class="cfg-list-add" onClick=${copyIt}>${copied ? 'Copied' : 'Copy'}</button>
      <button type="button" class="cfg-list-add" onClick=${onDone}>Done</button>
    </div>
  </div>`;
}

function selected(event) {
  return [...event.currentTarget.selectedOptions].map(option => option.value);
}

// Only the groups and profiles the daemon would accept are offered, so a choice
// here can't fail for a reason the operator could have seen. Its workers still stop
// for the human, which the Config page says beside this.
function WorkerPin({ steward, choices, busy, onSave, onClose }) {
  const [groups, setGroups] = useState(steward.spawn?.groups || []);
  const [spawnProfile, setSpawnProfile] = useState(steward.spawn?.spawn_profile || '');
  const [sandboxProfile, setSandboxProfile] = useState(steward.spawn?.sandbox_profile || '');
  const ready = groups.length > 0 && spawnProfile && sandboxProfile;
  const options = (values, chosen) => values.map(value => html`<option key=${value} value=${value} selected=${chosen.includes(value)}>${value}</option>`);
  return html`<form class="cfg-steward-pin" onSubmit=${(event) => {
    event.preventDefault();
    onSave({ groups, spawn_profile: spawnProfile, sandbox_profile: sandboxProfile });
  }}>
    <label>Groups <select multiple aria-label="Groups it can start workers in" onChange=${event => setGroups(selected(event))}>
      ${options(choices.groups || [], groups)}</select></label>
    <label>Spawn profile <select aria-label="Spawn profile for its workers" onChange=${event => setSpawnProfile(event.currentTarget.value)}>
      <option value="">Choose</option>${options(choices.spawn_profiles || [], [spawnProfile])}</select></label>
    <label>Sandbox profile <select aria-label="Sandbox profile for its workers" onChange=${event => setSandboxProfile(event.currentTarget.value)}>
      <option value="">Choose</option>${options(choices.sandbox_profiles || [], [sandboxProfile])}</select></label>
    <div class="cfg-steward-row-actions">
      <button type="submit" class="cfg-list-add" disabled=${busy || !ready}>Save</button>
      ${steward.spawn && html`<button type="button" class="cfg-list-add" disabled=${busy} onClick=${() => onSave({ off: true })}>Turn off</button>`}
      <button type="button" class="cfg-list-add" onClick=${onClose}>Cancel</button>
    </div>
  </form>`;
}

function workerSummary(steward) {
  const running = (steward.children || []).filter(child => child.online).length;
  const where = steward.spawn ? steward.spawn.groups.join(', ') : 'Off';
  return running ? `${where}. ${running} running` : where;
}

function StewardRow({ steward, act, busy, renewable, choices, onPin }) {
  const live = steward.credentials.some(credential => credential.live);
  const [editing, setEditing] = useState(false);
  return html`<tr>
    <td>${steward.name}${steward.retired ? ' (retired)' : ''}</td>
    <td>${steward.credentials.length
      ? steward.credentials.map(credential => html`<div key=${credential.token_id}>${expiryLabel(credential)}</div>`)
      : 'None'}</td>
    <td>
      ${workerSummary(steward)}
      ${!steward.retired && !editing && html` <button type="button" class="cfg-list-add" disabled=${busy} onClick=${() => setEditing(true)}>Choose</button>`}
      ${editing && html`<${WorkerPin} steward=${steward} choices=${choices} busy=${busy} onClose=${() => setEditing(false)}
        onSave=${async (pin) => { if (await onPin(steward.name, pin)) setEditing(false); }} />`}
    </td>
    <td class="cfg-steward-row-actions">${!steward.retired && html`
      <button type="button" class="cfg-list-add" disabled=${busy || !renewable} title="Make a new credential. The old ones keep working until they expire."
        onClick=${() => act(steward.name, 'renew')}>Renew</button>
      <button type="button" class="cfg-list-add" disabled=${busy || !live} title="End all of its credentials on the next request"
        onClick=${() => act(steward.name, 'revoke')}>Revoke</button>`}
    </td>
  </tr>`;
}

export function ExternalStewardManager({ enabled, fetchImpl = globalThis.fetch, copy = writeClipboard, confirm = shellConfirm }) {
  const [stewards, setStewards] = useState([]);
  const [choices, setChoices] = useState({});
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [issued, setIssued] = useState(null);
  const [name, setName] = useState('');
  const [days, setDays] = useState('');
  const mounted = useRef(true);
  useEffect(() => () => { mounted.current = false; }, []);

  const refresh = useCallback(async () => {
    const data = await call(fetchImpl, ROOT);
    if (mounted.current) {
      setStewards(data.external_stewards || []);
      setChoices(data.spawn_choices || {});
    }
  }, [fetchImpl]);
  useEffect(() => { refresh().catch(e => setError(e.message)); }, [refresh, enabled]);

  const run = async (path, body, label) => {
    setBusy(true);
    setError('');
    try {
      const data = await call(fetchImpl, path, body);
      if (data.credential && mounted.current) setIssued({ name: label, credential: data.credential });
      await refresh();
      return true;
    } catch (e) {
      if (mounted.current) setError(e.message);
      return false;
    } finally {
      if (mounted.current) setBusy(false);
    }
  };
  const act = async (label, verb) => {
    if (verb !== 'revoke') {
      void run(`${ROOT}/${encodeURIComponent(label)}/${verb}`, {}, label);
      return;
    }
    if (!await confirm({
      title: `Revoke ${label}?`,
      body: 'All of its credentials end, and its assistant stops working on its next request.',
      okLabel: 'Revoke',
    })) return;
    const running = (stewards.find(steward => steward.name === label)?.children || []).filter(child => child.online);
    const stop = running.length > 0 && await confirm({
      title: `Stop ${label}'s workers too?`,
      body: `These keep running unless you stop them: ${running.map(child => child.title || child.agent_id).join(', ')}.`,
      okLabel: 'Stop them',
      cancelLabel: 'Leave them running',
    });
    void run(`${ROOT}/${encodeURIComponent(label)}/revoke`, { stop_children: !!stop }, label);
  };
  const pin = (label, body) => run(`${ROOT}/${encodeURIComponent(label)}/spawn`, body, label);
  const add = async (event) => {
    event.preventDefault();
    // A second credential would replace the one on screen, which can't be shown again.
    if (issued) return;
    const label = name.trim();
    const body = { name: label };
    if (days) body.expires_days = Number(days);
    if (await run(ROOT, body, label)) { setName(''); setDays(''); }
  };

  return html`<div class="cfg-steward-manager">
    ${issued && html`<${IssuedCredential} key=${issued.credential} issued=${issued} copy=${copy} onDone=${() => setIssued(null)} />`}
    ${error && html`<div class="cfg-steward-error" role="alert">${error}</div>`}
    ${stewards.length > 0 && html`<table class="cfg-tool-list">
      <thead><tr><th scope="col">Name</th><th scope="col">Credentials</th><th scope="col">Workers</th><th scope="col">Actions</th></tr></thead>
      <tbody>${stewards.map(steward => html`<${StewardRow} key=${steward.agent_id} steward=${steward} act=${act} onPin=${pin} choices=${choices} busy=${busy} renewable=${enabled && !issued} />`)}</tbody>
    </table>`}
    ${enabled
      ? html`<form class="cfg-steward-add" onSubmit=${add}>
          <input type="text" aria-label="External steward name" placeholder="name" maxlength="40" pattern="[a-z0-9][a-z0-9\\-]*" title="Lowercase letters, digits and hyphens" required spellcheck="false"
            value=${name} onInput=${event => setName(event.currentTarget.value)} />
          <input type="number" aria-label="Days until the credential expires" placeholder="30" min="1" max="365"
            value=${days} onInput=${event => setDays(event.currentTarget.value)} />
          <span>days</span>
          <button type="submit" class="cfg-list-add" disabled=${busy || !!issued}>Add</button>
        </form>`
      : html`<div class="cfg-row-summary">Turn this on and save to add one. You can still revoke credentials while it's off.</div>`}
  </div>`;
}
