import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

function errText(error) { return error?.message || String(error); }

// HEALTH_SIGNALS are the notice kinds (tclaude federation nodes health).
const HEALTH_SIGNALS = [
  { key: 'presence', label: 'offline / back' },
  { key: 'resources', label: 'low disk, sustained high memory' },
  { key: 'failures', label: 'repeated job / spawn failures' },
];

// HEALTH_NUMBERS mirror config.FederationHealthPolicy's limits; def is the
// daemon's built-in default (effectiveFleetPolicy).
export const HEALTH_NUMBERS = [
  { key: 'debounce_seconds', def: 15, label: 'debounce', unit: 's', max: 86400 },
  { key: 'disk_free_percent', def: 10, label: 'disk free under', unit: '%', max: 100, float: true },
  { key: 'ram_free_percent', def: 10, label: 'RAM free under', unit: '%', max: 100, float: true },
  { key: 'memory_seconds', def: 120, label: 'for at least', unit: 's', max: 86400 },
  { key: 'failure_count', def: 3, label: 'failures', unit: '', max: 256 },
  { key: 'failure_window_seconds', def: 600, label: 'within', unit: 's', max: 86400 },
  { key: 'cooldown_seconds', def: 600, label: 'cooldown', unit: 's', max: 86400 },
];

export function policyForm(p) {
  const f = { presence: p?.presence !== false, resources: !!p?.resources, failures: !!p?.failures };
  for (const n of HEALTH_NUMBERS) f[n.key] = p?.[n.key] == null ? '' : String(p[n.key]);
  return f;
}

// policyBody turns the form into the replacement policy, or an error string.
// An empty number, or one equal to the built-in default, is left out so the
// policy keeps following the built-in default.
export function policyBody(form) {
  const body = { presence: !!form.presence, resources: !!form.resources, failures: !!form.failures };
  for (const n of HEALTH_NUMBERS) {
    const raw = String(form[n.key] ?? '').trim();
    const v = raw === '' ? 0 : Number(raw);
    if (!Number.isFinite(v) || v < 0 || v > n.max || (!n.float && !Number.isInteger(v))) {
      return `${n.label}: ${n.float ? 'a number' : 'a whole number'} from 0 to ${n.max} (empty or 0 = default)`;
    }
    if (v && v !== n.def) body[n.key] = v;
  }
  return body;
}

// HealthPolicySection reads and replaces the fleet health notice policy: the
// defaults, or one trusted peer's own policy.
export function HealthPolicySection({ peers, actions, confirm, toast }) {
  const [peer, setPeer] = useState('');
  const [form, setForm] = useState(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let off = false;
    setForm(null);
    actions.healthPolicy(peer).then((p) => { if (!off) { setForm(policyForm(p)); setError(''); } })
      .catch((e) => { if (!off) { setForm({ error: errText(e) }); } });
    return () => { off = true; };
  }, [peer]);
  const label = peers.find((p) => p.id === peer)?.label || peer;
  const set = (k) => (e) => setForm({ ...form, [k]: e.currentTarget.type === 'checkbox' ? e.currentTarget.checked : e.currentTarget.value });
  const num = (k) => html`<input data-num=${k} inputmode="decimal" value=${form[k]} placeholder="default" aria-label=${HEALTH_NUMBERS.find((n) => n.key === k).label} onInput=${set(k)} />`;

  const save = () => {
    const body = policyBody(form);
    if (typeof body === 'string') { setError(body); return; }
    setError('');
    const on = HEALTH_SIGNALS.filter((s) => body[s.key]).map((s) => s.label);
    const scope = peer
      ? `${label}'s own policy. From then on ${label} no longer follows the defaults.`
      : 'the defaults, used for every trusted peer without its own policy.';
    confirm({
      title: peer ? `Replace ${label}'s fleet health policy?` : 'Replace the default fleet health policy?',
      body: `Replaces ${scope} ${on.length ? `Notices on: ${on.join('; ')}.` : 'All notices off: nothing is reported for these peers.'} Notices arrive in Messages and as desktop notifications.`,
      okLabel: 'Save policy',
      busyLabel: 'Saving…',
      action: () => actions.setHealthPolicy(peer, body),
    }).then((p) => { if (p) { setForm(policyForm(p)); toast('Fleet health policy saved', false); } })
      .catch((e) => setError(errText(e)));
  };

  return html`<div id="fleet-health">
    <h4 class="fa-ns-h">Fleet health notices</h4>
    <label class="fa-ns-row"><span class="fa-k">policy for</span><select id="fleet-health-peer" value=${peer} onChange=${(e) => setPeer(e.currentTarget.value)}>
      <option value="">defaults (peers without their own)</option>
      ${peers.map((p) => html`<option key=${p.id} value=${p.id}>${p.label}</option>`)}</select></label>
    ${form?.error ? html`<div class="fa-danger">${form.error}</div>` : !form ? html`<div class="muted">Loading…</div>` : html`
      <div class="fa-ns-check fa-hp-signals">${HEALTH_SIGNALS.map((s) => html`<label key=${s.key}><input type="checkbox" data-signal=${s.key} checked=${form[s.key]} onChange=${set(s.key)} /> ${s.label}</label>`)}</div>
      <div class="fa-hp-rows">
        <div>offline / back after ${num('debounce_seconds')} s steady</div>
        <div>disk free under ${num('disk_free_percent')} %, RAM free under ${num('ram_free_percent')} % for ${num('memory_seconds')} s</div>
        <div>${num('failure_count')} failures within ${num('failure_window_seconds')} s, then at most one notice per ${num('cooldown_seconds')} s</div>
      </div>
      <div class="fa-ns-actions"><button id="fleet-health-save" type="button" onClick=${save}>Save policy…</button>
        <span class="muted">CLI: <code>tclaude federation nodes health [--peer …] [--set '{…}']</code></span></div>`}
    ${error && html`<div class="fa-danger" role="alert">${error}</div>`}
  </div>`;
}
