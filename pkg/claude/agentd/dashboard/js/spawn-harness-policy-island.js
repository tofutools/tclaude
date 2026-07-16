import { h, render } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { registerSpawnHarnessPolicyController } from './spawn-harness-policy-controller.js';
import { isWizardActive } from './slop.js';

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

function useWizardTheme() {
  const [wizard, setWizard] = useState(isWizardActive());
  useEffect(() => {
    const update = (event) => setWizard(
      event.detail?.active == null ? isWizardActive() : Boolean(event.detail.active),
    );
    document.addEventListener('tclaude:wizard', update);
    return () => document.removeEventListener('tclaude:wizard', update);
  }, []);
  return wizard;
}

function SpawnHarnessPolicyDialog({ descriptor, close, confirmDiscard, notify }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const wizard = useWizardTheme();
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
  const copy = wizard ? {
    title: descriptor.group
      ? `Cross-realm summons · party ${descriptor.group}`
      : 'Global cross-realm summons',
    intro: 'Governs familiar-initiated summons after patterns and defaults reveal the destination realm. Archmage-cast and same-realm summons are unaffected.',
    scope: groupScope
      ? 'Inherit follows the global ward for that passage.'
      : 'Unmarked passages are permitted by default.',
    loading: 'Consulting the realm wards…',
    axes: 'Summoner realm ↓ / familiar realm →',
    same: 'always permitted',
    inherit: 'inherit global ward',
    allow: 'permit',
    deny: 'forbid',
    reason: 'Reason revealed to the summoning familiar',
    effective: 'Global ward',
    cancel: 'Dismiss',
    save: 'Inscribe wards',
    saved: descriptor.group
      ? `inscribed cross-realm wards for party ${descriptor.group}`
      : 'inscribed global cross-realm wards',
  } : {
    title: descriptor.group
      ? `Cross-harness spawn policy · ${descriptor.group}`
      : 'Global cross-harness spawn policy',
    intro: 'Controls agent-initiated delegation after profiles and defaults resolve the target harness. Human-initiated spawns and same-harness spawns are unaffected.',
    scope: groupScope
      ? 'Inherit defers this edge to the global matrix.'
      : 'Unset edges allow by default.',
    loading: 'Loading spawn matrix…',
    axes: 'Spawner ↓ / child →',
    same: 'always allowed',
    inherit: 'inherit global',
    allow: 'allow',
    deny: 'deny',
    reason: 'Reason returned to the spawning agent',
    effective: 'Effective',
    cancel: 'Cancel',
    save: 'Save policy',
    saved: descriptor.group
      ? `saved cross-harness spawn policy for ${descriptor.group}`
      : 'saved global cross-harness spawn policy',
  };
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
      setError(wizard
        ? `Inscribe a reason for ${missing.source} → ${missing.target}; the summoning familiar receives it when forbidden.`
        : `Add a reason for ${missing.source} → ${missing.target}; agents receive this text when denied.`);
      return;
    }
    setBusy(true);
    setError('');
    try {
      await savePolicy(descriptor.group, rules);
      notify?.(copy.saved);
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
      <h3 id="spawn-harness-policy-title">${copy.title}</h3>
      <p class="manage-intro">${copy.intro} ${copy.scope}</p>
      ${busy && !view ? html`<div class="empty">${copy.loading}</div>` : null}
      ${view ? html`
        <div class="spawn-harness-matrix-wrap">
          <table class="spawn-harness-matrix">
            <thead><tr><th>${copy.axes}</th>${view.harnesses.map((target) => html`<th key=${target.name}>${target.display_name || target.name}</th>`)}</tr></thead>
            <tbody>${view.harnesses.map((source) => html`
              <tr key=${source.name}><th>${source.display_name || source.name}</th>
                ${view.harnesses.map((target) => {
                  if (source.name === target.name) return html`<td key=${target.name} class="spawn-harness-same">${copy.same}</td>`;
                  const key = edgeKey(source.name, target.name);
                  const edge = draft[key];
                  const inherited = groupScope ? effectiveGlobal(view, source.name, target.name) : null;
                  return html`<td key=${target.name} class=${`spawn-harness-cell ${edge?.decision || ''}`}>
                    <select value=${edge?.decision || 'allow'} disabled=${busy}
                      aria-label=${wizard
                        ? `${source.name} to ${target.name} summoning ward`
                        : `${source.name} to ${target.name} decision`}
                      onChange=${(event) => update(key, { decision: event.currentTarget.value })}>
                      ${groupScope ? html`<option value="inherit">${copy.inherit}</option>` : null}
                      <option value="allow">${copy.allow}</option>
                      <option value="deny">${copy.deny}</option>
                    </select>
                    ${edge?.decision === 'deny' ? html`<textarea rows="3" maxlength="1000"
                      aria-label=${wizard
                        ? `${source.name} to ${target.name} forbidden-summon reason`
                        : `${source.name} to ${target.name} denial reason`}
                      placeholder=${copy.reason} value=${edge.reason}
                      disabled=${busy} onInput=${(event) => update(key, { reason: event.currentTarget.value })}></textarea>` : null}
                    ${edge?.decision === 'inherit' ? html`<small class=${inherited.decision === 'deny' ? 'danger-text' : 'muted'}>
                      ${copy.effective}: ${wizard
                        ? (inherited.decision === 'deny' ? copy.deny : copy.allow)
                        : inherited.decision}${inherited.reason ? ` — ${inherited.reason}` : ''}
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
        <button type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>${copy.cancel}</button>
        <span class="spacer"></span>
        <button type="button" class="primary" disabled=${busy || !view} onClick=${submit}>${copy.save}</button>
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
