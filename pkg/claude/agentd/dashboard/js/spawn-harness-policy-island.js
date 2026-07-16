import { h, render } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { registerSpawnHarnessPolicyController } from './spawn-harness-policy-controller.js';

const html = htm.bind(h);

function edgeKey(source, target) { return `${source}\u0000${target}`; }
function errorMessage(error) { return error?.message || String(error); }

async function loadPolicy(group) {
  const url = group
    ? `/api/groups/${encodeURIComponent(group)}/spawn-harness-policy`
    : '/api/spawn-harness-policy';
  const response = await fetch(url, { credentials: 'same-origin' });
  if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
  return response.json();
}

async function savePolicy(group, rules) {
  const url = group
    ? `/api/groups/${encodeURIComponent(group)}/spawn-harness-policy`
    : '/api/spawn-harness-policy';
  const response = await fetch(url, {
    method: 'PUT', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ rules }),
  });
  if (!response.ok) throw new Error((await response.text()) || `HTTP ${response.status}`);
  return response.json();
}

function policyDraft(view) {
  const own = new Map((view.rules || []).map((rule) => [edgeKey(rule.source, rule.target), rule]));
  const draft = {};
  for (const source of view.harnesses || []) {
    for (const target of view.harnesses || []) {
      if (source.name === target.name) continue;
      const key = edgeKey(source.name, target.name);
      const rule = own.get(key);
      draft[key] = {
        source: source.name, target: target.name,
        decision: rule?.decision || (view.scope === 'group' ? 'inherit' : 'allow'),
        reason: rule?.reason || '',
      };
    }
  }
  return draft;
}

function effectiveGlobal(view, source, target) {
  const rule = (view.global_rules || []).find((item) => item.source === source && item.target === target);
  return rule || { decision: 'allow', reason: '' };
}

function SpawnHarnessPolicyDialog({ descriptor, close, confirmDiscard, notify }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [view, setView] = useState(null);
  const [draft, setDraft] = useState({});
  const [baseline, setBaseline] = useState('');
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    let live = true;
    setBusy(true);
    loadPolicy(descriptor.group).then((next) => {
      if (!live) return;
      const initial = policyDraft(next);
      setView(next);
      setDraft(initial);
      setBaseline(JSON.stringify(initial));
      setError('');
    }).catch((cause) => { if (live) setError(errorMessage(cause)); })
      .finally(() => { if (live) setBusy(false); });
    return () => { live = false; };
  }, [descriptor.key]);

  const dirty = baseline !== '' && JSON.stringify(draft) !== baseline;
  const groupScope = view?.scope === 'group';
  const title = descriptor.group
    ? `Cross-harness spawn policy · ${descriptor.group}`
    : 'Global cross-harness spawn policy';
  const update = (key, patch) => setDraft((current) => ({
    ...current, [key]: { ...current[key], ...patch },
  }));
  const submit = async () => {
    if (busy || !view) return;
    const rules = Object.values(draft)
      .filter((rule) => rule.decision !== 'inherit')
      .map((rule) => ({
        source: rule.source, target: rule.target,
        decision: rule.decision,
        reason: rule.decision === 'deny' ? rule.reason.trim() : '',
      }));
    const missing = rules.find((rule) => rule.decision === 'deny' && !rule.reason);
    if (missing) {
      setError(`Add a reason for ${missing.source} → ${missing.target}; agents receive this text when denied.`);
      return;
    }
    setBusy(true);
    setError('');
    try {
      await savePolicy(descriptor.group, rules);
      notify?.(descriptor.group
        ? `saved cross-harness spawn policy for ${descriptor.group}`
        : 'saved global cross-harness spawn policy');
      close();
    } catch (cause) {
      setError(errorMessage(cause));
      setBusy(false);
    }
  };

  return html`
    <${Overlay} id="spawn-harness-policy-modal" labelledby="spawn-harness-policy-title"
      onClose=${close} onSubmitHotkey=${submit} dirty=${dirty} blocked=${busy}
      confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
      <h3 id="spawn-harness-policy-title">${title}</h3>
      <p class="manage-intro">
        Controls agent-initiated delegation after profiles and defaults resolve the target harness.
        Human-initiated spawns and same-harness spawns are unaffected.
        ${groupScope ? 'Inherit defers this edge to the global matrix.' : 'Unset edges allow by default.'}
      </p>
      ${busy && !view ? html`<div class="empty">Loading spawn matrix…</div>` : null}
      ${view ? html`
        <div class="spawn-harness-matrix-wrap">
          <table class="spawn-harness-matrix">
            <thead><tr><th>Spawner ↓ / child →</th>${view.harnesses.map((target) => html`<th key=${target.name}>${target.display_name || target.name}</th>`)}</tr></thead>
            <tbody>${view.harnesses.map((source) => html`
              <tr key=${source.name}><th>${source.display_name || source.name}</th>
                ${view.harnesses.map((target) => {
                  if (source.name === target.name) return html`<td key=${target.name} class="spawn-harness-same">always allowed</td>`;
                  const key = edgeKey(source.name, target.name);
                  const edge = draft[key];
                  const inherited = groupScope ? effectiveGlobal(view, source.name, target.name) : null;
                  return html`<td key=${target.name} class=${`spawn-harness-cell ${edge?.decision || ''}`}>
                    <select value=${edge?.decision || 'allow'} disabled=${busy}
                      aria-label=${`${source.name} to ${target.name} decision`}
                      onChange=${(event) => update(key, { decision: event.currentTarget.value })}>
                      ${groupScope ? html`<option value="inherit">inherit global</option>` : null}
                      <option value="allow">allow</option>
                      <option value="deny">deny</option>
                    </select>
                    ${edge?.decision === 'deny' ? html`<textarea rows="3" maxlength="1000"
                      placeholder="Reason returned to the spawning agent" value=${edge.reason}
                      disabled=${busy} onInput=${(event) => update(key, { reason: event.currentTarget.value })}></textarea>` : null}
                    ${edge?.decision === 'inherit' ? html`<small class=${inherited.decision === 'deny' ? 'danger-text' : 'muted'}>
                      Effective: ${inherited.decision}${inherited.reason ? ` — ${inherited.reason}` : ''}
                    </small>` : null}
                  </td>`;
                })}
              </tr>
            `)}</tbody>
          </table>
        </div>
      ` : null}
      <div class="cron-create-error" role="alert">${error}</div>
      <div class="modal-buttons">
        <button type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
        <span class="spacer"></span>
        <button type="button" class="primary" disabled=${busy || !view} onClick=${submit}>Save policy</button>
      </div>
    </${Overlay}>
  `;
}

function SpawnHarnessPolicyIsland({ confirmDiscard, notify, registerCleanup }) {
  const [descriptor, setDescriptor] = useState(null);
  const controller = useMemo(() => ({
    open(group = '') {
      setDescriptor((current) => current || { group, key: Date.now() });
      return true;
    },
  }), []);
  useEffect(() => registerSpawnHarnessPolicyController(controller), [controller]);
  useEffect(() => {
    const button = document.querySelector('#spawn-harness-policy-open');
    if (!button) return undefined;
    const open = () => controller.open('');
    button.addEventListener('click', open);
    return () => button.removeEventListener('click', open);
  }, [controller]);
  if (!descriptor) return null;
  return html`<${SpawnHarnessPolicyDialog} key=${descriptor.key} descriptor=${descriptor}
    close=${() => setDescriptor(null)} confirmDiscard=${confirmDiscard} notify=${notify} />`;
}

export function mountSpawnHarnessPolicyIsland({ host, confirmDiscard, notify, registerCleanup }) {
  render(html`<${SpawnHarnessPolicyIsland} confirmDiscard=${confirmDiscard} notify=${notify}
    registerCleanup=${registerCleanup} />`, host);
  const cleanup = () => render(null, host);
  registerCleanup?.(cleanup);
  return cleanup;
}
