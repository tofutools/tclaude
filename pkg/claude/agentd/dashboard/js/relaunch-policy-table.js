// relaunch-policy-table.js — the Config › Launch relaunch policy table
// (launch slice 9, operator decision Q9 option A).
//
// One global answer per configurable launch row: a Re-resolve | Reuse
// segmented control, or a select for the three-mode harness_config row. Every
// other row of the launch registry is shown read-only with a pill for its
// layer (Copied at spawn / Fixed / Live / …). The Why text comes from the
// registry (GET /api/relaunch-policy), the same source docs/launch-resolution.md
// is generated from, so the two never drift. A change saves at once (PUT) and
// applies on each agent's next relaunch. Writes need relaunch-policy.manage;
// the daemon reports can_manage, and without it the table is read-only.
import { h } from 'preact';
import { useCallback, useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Seg, Select } from './ui-components.js';
import { pageShares, pageWithheld } from './peer-view-limits.js';

const html = htm.bind(h);

const ROOT = '/api/relaunch-policy';

export const MODE_LABELS = Object.freeze({
  're-resolve': 'Re-resolve',
  reuse: 'Reuse',
  'explicit-wins': 'Explicit wins',
  'tighten-only': 'Tighten only',
  'chain-wins': 'Chain wins',
});

// The read-only sections, in display order: a layer key from the registry,
// the section heading and the pill its rows get. group setting and
// referenced profile rows that are not configurable are always re-read.
export const READONLY_SECTIONS = Object.freeze([
  { layers: ['group setting', 'referenced profile'], heading: 'Always re-read', pill: 'Re-read', kind: 'reread' },
  { layers: ['spawn copy'], heading: 'Copied at spawn · the spawn profile is shallow-copied', pill: 'Copied at spawn', kind: 'copy' },
  { layers: ['fixed'], heading: 'Fixed at first spawn', pill: 'Fixed', kind: 'fixed' },
  { layers: ['live'], heading: 'Live, not a launch input', pill: 'Live', kind: 'live' },
  { layers: ['spawn only'], heading: 'Used by the first spawn only', pill: 'Spawn only', kind: 'once' },
]);

async function call(fetchImpl, body) {
  const options = { credentials: 'same-origin' };
  if (body) {
    options.method = 'PUT';
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(body);
  }
  const response = await fetchImpl(ROOT, options);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || data.message || `HTTP ${response.status}`);
  return data;
}

function modeLabel(mode) {
  return MODE_LABELS[mode] || mode;
}

function WhyCell({ why, gap }) {
  return html`<td class="rlp-why">${why}${gap && html`<div class="rlp-gap ui-hint">Not yet: ${gap}</div>`}</td>`;
}

function ModeControl({ row, disabled, onSet }) {
  const label = `${row.label}: on relaunch`;
  if (row.modes.length !== 2) {
    return html`<${Select} size="sm" aria-label=${label} value=${row.mode} disabled=${disabled}
      onChange=${(e) => onSet(row.field, e.currentTarget.value)}>
      ${row.modes.map((mode) => html`<option key=${mode} value=${mode}>${modeLabel(mode)}${mode === row.default ? ' (default)' : ''}</option>`)}
    <//>`;
  }
  return html`<${Seg} aria-label=${label}>
    ${row.modes.map((mode) => html`<${Button} key=${mode} size="sm" pressed=${row.mode === mode} disabled=${disabled}
      onClick=${() => row.mode !== mode && onSet(row.field, mode)}>${modeLabel(mode)}<//>`)}
  <//>`;
}

// RELAUNCH_FEATURE is the peer-view feature behind /api/relaunch-policy: it
// has no peer grant, so a peer serves it only to unrestricted trust.
const RELAUNCH_FEATURE = 'local_dashboard';

export function RelaunchPolicyTable({ fetchImpl = globalThis.fetch, notify, gate = undefined }) {
  const [data, setData] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  // On a peer's page the table reads the peer's policy only once its
  // snapshot says it is shared; else it says so instead of showing a 403.
  const shared = pageShares(RELAUNCH_FEATURE, gate);
  const withheld = pageWithheld(RELAUNCH_FEATURE, 'The relaunch policy', gate);

  const load = useCallback(async () => {
    try {
      setData(await call(fetchImpl));
      setError('');
    } catch (e) { setError(e.message || String(e)); }
  }, [fetchImpl]);
  useEffect(() => { if (shared) load(); }, [load, shared]);

  const put = async (changes) => {
    setBusy(true); setError('');
    try {
      let latest = null;
      for (const change of changes) latest = await call(fetchImpl, change);
      if (latest) setData(latest);
      return true;
    } catch (e) {
      setError(e.message || String(e));
      await load();
      return false;
    } finally { setBusy(false); }
  };

  const setMode = async (field, mode) => {
    if (await put([{ field, mode }])) {
      const row = data?.rows?.find((r) => r.field === field);
      notify?.(`Relaunch policy: ${row?.label || field} → ${modeLabel(mode)}. Applies on each agent's next relaunch.`);
    }
  };
  const reset = async () => {
    const set = (data?.rows || []).filter((r) => r.set);
    if (set.length && await put(set.map((r) => ({ field: r.field, mode: '' })))) {
      notify?.('Relaunch policy reset to defaults.');
    }
  };

  if (withheld) return html`<div class="rlp"><p class="ui-hint" data-rlp-withheld>${withheld}.</p></div>`;
  if (!data) {
    return html`<div class="rlp">${error ? html`<div class="ui-error-box">${error}</div>` : html`<div class="ui-hint">Loading…</div>`}</div>`;
  }
  const canManage = !!data.can_manage;
  const rows = Array.isArray(data.rows) ? data.rows : [];
  const fields = Array.isArray(data.fields) ? data.fields : [];
  const disabled = !canManage || busy;
  const anySet = rows.some((r) => r.set);
  const fieldByKey = new Map(fields.map((f) => [f.key, f]));

  return html`<div class="rlp">
    ${error && html`<div class="ui-error-box" role="alert">${error}</div>`}
    ${!canManage && html`<p class="rlp-readonly ui-hint" data-rlp-readonly>Read-only: changing the relaunch policy needs the
      <code>relaunch-policy.manage</code> permission, and the peer view never edits it.</p>`}
    <table class="rlp-table ui-compact">
      <thead><tr><th scope="col">Field</th><th scope="col">On relaunch</th><th scope="col">Why</th></tr></thead>
      <tbody>
        <tr class="rlp-section"><th colspan="3" scope="colgroup">Configurable · group settings and referenced profiles</th></tr>
        ${rows.map((row) => html`<tr key=${row.field} data-rlp-field=${row.field}>
          <th scope="row">${row.label}${row.mode !== row.default && html` <span class="ui-tag ui-tag-accent" title=${`Default: ${modeLabel(row.default)}`}>changed</span>`}</th>
          <td class="rlp-ctl">${row.settable
            ? html`<${ModeControl} row=${row} disabled=${disabled} onSet=${setMode} />`
            : html`<span class="ui-tag" title="No relaunch reads this row yet">${modeLabel(row.mode)}</span>`}</td>
          <${WhyCell} why=${row.why} gap=${row.gap || fieldByKey.get(row.field)?.gap} />
        </tr>`)}
        ${READONLY_SECTIONS.map((section) => {
          const members = fields.filter((f) => !f.configurable && section.layers.includes(f.layer));
          if (!members.length) return null;
          return [
            html`<tr key=${`h-${section.kind}`} class="rlp-section"><th colspan="3" scope="colgroup">${section.heading}</th></tr>`,
            ...members.map((f) => html`<tr key=${f.key} data-rlp-field=${f.key}>
              <th scope="row">${f.label}</th>
              <td><span class=${`ui-tag rlp-pill rlp-pill-${section.kind}`}>${section.pill}</span></td>
              <${WhyCell} why=${f.why} gap=${f.gap} />
            </tr>`),
          ];
        })}
      </tbody>
    </table>
    <div class="rlp-actions">
      <span class="ui-hint">Changes save at once and apply on each agent's next resume or reincarnation.</span>
      <${Button} size="sm" disabled=${disabled || !anySet} onClick=${reset}>Reset to defaults<//>
    </div>
  </div>`;
}
