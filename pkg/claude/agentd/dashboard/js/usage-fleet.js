import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ENFORCE_STATES, FLEET_PRESETS, MEMBER_STATES, barTone, blankRule, draftToRule, fleetTargetOptions, fmtAmount,
  nodeGrants, ruleSummary, ruleToDraft, worstLimit,
} from './usage-rules-model.js';

const html = htm.bind(h);

// usage-fleet.js — the "Fleet" tab of the usage manager: fleet limits (spend
// summed across trusted peers, a node pool or one peer), what each peer
// shares with this node, and peers' fleet limits in force here
// (usage_fleet_api.go). Rule and bar components come from usage-rules.js.

function money(v) { return fmtAmount('total_usd', v || 0); }

function GrantChips({ node }) {
  const g = nodeGrants(node);
  const chip = (on, label, title) => html`<span class=${`usage-grant-chip${on ? ' on' : ''}`} title=${title}>${on ? '✓' : '·'} ${label}</span>`;
  return html`<span class="usage-grant-chips">
    ${chip(g.read, 'usage', 'usage.read: its totals count towards your fleet limits')}
    ${chip(g.manage, 'rules', 'usage-rules.manage: you can manage its usage rules from here')}
    ${chip(g.enforce, 'enforce', 'usage-rules.enforce: your fleet limits act on its agents in the groups it granted')}
  </span>`;
}

function MemberRows({ status }) {
  return html`<table class="usage-fleet-members">
    <tbody>${(status.members || []).map((m) => html`<tr key=${m.node} class=${m.counted ? '' : 'muted'} data-fleet-member=${m.node}>
      <td>${m.label}</td>
      <td>${MEMBER_STATES[m.state] || m.state}</td>
      <td class="num">${m.counted || m.value ? money(m.value) : '—'}</td>
      <td title=${m.enforce === 'not_granted' ? 'It has not granted you usage-rules.enforce: steps there only alert you' : ''}>${ENFORCE_STATES[m.enforce] || ''}${m.send_error ? html` <span class="usage-fleet-err" title=${m.send_error}>⚠ not delivered</span>` : ''}${m.applied?.block ? ' · blocking spawns' : ''}${m.applied?.stop ? ' · holding stopped' : ''}</td>
    </tr>`)}</tbody>
  </table>`;
}

function FleetBar({ status }) {
  if (!status) return html`<span class="usage-rule-bar-empty muted">no reading yet</span>`;
  const pct = Math.max(0, status.pct || 0);
  return html`<span class=${`usage-rule-reading ${barTone(status)}`}>
    <span class="usage-rule-bar"><span class="usage-rule-bar-fill" style=${`width:${Math.min(100, pct)}%`}></span></span>
    <span class="usage-rule-figure">${money(status.value)} / ${money(status.budget)} · ${Math.round(pct)}% · ${status.counted} node${status.counted === 1 ? '' : 's'} counted</span>
    ${status.reached ? html`<span class="usage-rule-reached">${status.reached.replace('_', ' ')}</span>` : null}
  </span>`;
}

// FleetTab loads the fleet view itself: reading every peer's summary can take
// a few seconds, so the rest of the manager never waits for it.
export function FleetTab({ api, busy, reloadKey = 0, onEdit, onToggle, onManageNode }) {
  const [fleet, setFleet] = useState(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const live = useRef(true);
  const load = async (fresh = false) => {
    setLoading(true);
    try {
      const next = await api.fleet(fresh);
      if (live.current) { setFleet(next); setError(''); }
    } catch (cause) { if (live.current) setError(cause.message); }
    finally { if (live.current) setLoading(false); }
  };
  useEffect(() => { live.current = true; void load(); return () => { live.current = false; }; }, [reloadKey]);
  if (!fleet) {
    return error
      ? html`<div class="cron-create-error" role="alert">${error} <button type="button" onClick=${() => void load()}>Retry</button></div>`
      : html`<div class="empty">Reading the fleet…</div>`;
  }
  const statusFor = (limit) => (fleet.statuses || []).find((s) => s.fleet_id === limit.id);
  const selfTotals = fleet.self?.totals;
  return html`<div class="usage-rules-tab usage-fleet-tab">
    <p class="modal-hint">A fleet limit sums spend across nodes: every trusted node, a node pool (a sub-fleet) or one node, and this node unless you leave it out. Nodes count once they grant you <code>usage.read</code>; a step acts on a node's agents only where it granted you <code>usage-rules.enforce</code>, and the node carries it out itself.</p>
    <${FleetPresets} disabled=${busy} onPick=${(preset) => onEdit({ id: 0, target: 'all', include_self: true, enabled: true, rule: preset ? preset.rule : blankRule('global') }, fleet)}/>
    ${(fleet.limits || []).length ? html`<div class="usage-rule-cards">${fleet.limits.map((limit) => {
      const status = statusFor(limit);
      return html`<div key=${limit.id} class=${`usage-rule-card${limit.enabled ? '' : ' disabled'}`} data-fleet-limit-id=${limit.id}>
        <div class="usage-rule-card-head">
          <span class="usage-rule-name">${limit.rule.name}</span>
          <span class="usage-rule-scope">${limit.target_label}${limit.include_self ? ' + this node' : ''}</span>
          ${limit.rule.dry_run ? html`<span class="usage-rule-tag">dry run</span>` : null}
          <span class="spacer"></span>
          <label class="usage-rule-enabled"><input type="checkbox" checked=${limit.enabled} disabled=${busy} onChange=${() => onToggle(limit)}/> enabled</label>
          <button type="button" class="tool" disabled=${busy} onClick=${() => onEdit(limit, fleet)}>edit</button>
        </div>
        <div class="usage-rule-summary">${ruleSummary(limit.rule)}</div>
        ${limit.enabled ? html`<${FleetBar} status=${status}/>${status ? html`<${MemberRows} status=${status}/>` : null}` : null}
      </div>`;
    })}</div>` : html`<div class="template-empty">No fleet limits yet.</div>`}

    <h4 class="usage-fleet-h">Nodes</h4>
    <div class="usage-fleet-nodes-wrap"><table class="usage-fleet-nodes">
      <thead><tr><th>Node</th><th class="num">Today</th><th class="num">This week</th><th class="num">This month</th><th>Its limits</th><th>Shares with you</th><th></th></tr></thead>
      <tbody>
        ${selfTotals ? html`<tr class="usage-fleet-self"><td>this node</td>
          <td class="num">${money(selfTotals.day?.total_usd)}</td><td class="num">${money(selfTotals.week?.total_usd)}</td><td class="num">${money(selfTotals.month?.total_usd)}</td>
          <td>${limitCell(fleet.self)}</td><td></td><td></td></tr>` : null}
        ${(fleet.nodes || []).map((n) => {
          const t = n.summary?.totals;
          const g = nodeGrants(n);
          return html`<tr key=${n.node} data-fleet-node=${n.node}>
            <td>${n.label}${n.pools?.length ? html` <span class="muted">${n.pools.join(', ')}</span>` : null}${n.error ? html` <span class="usage-fleet-err" title=${n.error}>⚠ ${n.error === 'offline' ? 'offline' : 'unread'}</span>` : null}</td>
            ${t ? html`<td class="num">${money(t.day?.total_usd)}</td><td class="num">${money(t.week?.total_usd)}</td><td class="num">${money(t.month?.total_usd)}</td><td>${limitCell(n.summary)}</td>`
              : html`<td class="muted" colspan="4">${n.summary ? 'shares no usage with you' : 'not read'}</td>`}
            <td><${GrantChips} node=${n}/></td>
            <td>${g.manage ? html`<button type="button" class="tool" disabled=${busy} onClick=${() => onManageNode(n)}>manage its rules…</button>` : null}</td>
          </tr>`;
        })}
      </tbody>
    </table></div>
    ${(fleet.nodes || []).length ? html`<p class="modal-hint">A node shares with you what its operator grants you: <code>usage.read</code> (its totals), <code>usage-rules.manage</code> (its rules, from here), <code>usage-rules.enforce</code> (your fleet limits act on its agents). Ask from its peer view (the <b>peer view</b> pill → request…), or its operator runs <code>${'tclaude federation grant <this node> usage.read'}</code>.</p>`
      : html`<div class="template-empty">No trusted peers. Link nodes under Fleet to build a fleet.</div>`}

    ${(fleet.controls || []).length ? html`<h4 class="usage-fleet-h">Peers' fleet limits in force here</h4>
      <ul class="usage-fleet-controls">${fleet.controls.map((c) => html`<li key=${c.peer + c.rule_key}>
        <b>${c.peer_label}</b>'s "${c.rule_name}" (${c.fleet}) at ${Math.round(c.pct)}%: ${[c.block ? 'blocks spawns' : '', c.stop ? 'stops agents that run' : ''].filter(Boolean).join(' and ')} in the groups its grant covers, until ${new Date(c.until).toLocaleTimeString()} unless renewed. Revoke its <code>usage-rules.enforce</code> grant to end it.</li>`)}</ul>` : null}
    <div class="usage-fleet-foot">
      ${fleet.evaluated_at ? html`<span class="muted">fleet readings from ${new Date(fleet.evaluated_at).toLocaleTimeString()}</span>` : null}
      <span class="spacer"></span>
      <button type="button" class="tool" disabled=${loading} onClick=${() => void load(true)}>${loading ? 'Reading…' : '↻ read nodes now'}</button>
    </div>
  </div>`;
}

function limitCell(summary) {
  const worst = worstLimit(summary);
  const budgets = summary?.budgets;
  const parts = [];
  if (worst) parts.push(html`<span class=${`usage-fleet-limit ${barTone(worst)}`} title=${`${worst.name}: ${fmtAmount(worst.metric, worst.value)} / ${fmtAmount(worst.metric, worst.budget)}`}>${worst.name} ${Math.round(worst.pct)}%</span>`);
  if (budgets?.over) parts.push(html`<span class="usage-fleet-limit over">${budgets.over} agent${budgets.over === 1 ? '' : 's'} over budget</span>`);
  else if (budgets?.warn) parts.push(html`<span class="usage-fleet-limit warn">${budgets.warn} near budget</span>`);
  return parts.length ? parts : html`<span class="muted">—</span>`;
}

function FleetPresets({ disabled, onPick }) {
  return html`<div class="usage-presets">
    <button type="button" class="tool" disabled=${disabled} onClick=${() => onPick(null)}>＋ Blank fleet limit</button>
    ${FLEET_PRESETS.map((preset) => html`<button key=${preset.id} type="button" class="tool" disabled=${disabled}
      title=${ruleSummary(preset.rule)} onClick=${() => onPick(preset)}>＋ Fleet ${preset.label.toLowerCase()}</button>`)}
  </div>`;
}

// FleetLimitEditor edits one fleet limit: its target, whether it counts this
// node, and its rule (RuleEditor from usage-rules.js, scope 'fleet').
export function FleetLimitEditor({ limit, fleet, busy, RuleEditor, onSave, onDelete, onCancel, onDirty }) {
  const [draft, setDraft] = useState(() => ruleToDraft(limit.rule));
  const [target, setTarget] = useState(limit.target || 'all');
  const [includeSelf, setIncludeSelf] = useState(limit.include_self !== false);
  const [enabled, setEnabled] = useState(limit.enabled !== false);
  const [error, setError] = useState('');
  const touch = (fn) => (value) => { onDirty(); fn(value); };
  const options = fleetTargetOptions(fleet);
  if (!options.some((o) => o.value === target)) options.push({ value: target, label: limit.target_label || target });
  const save = () => {
    try {
      setError('');
      onSave({ id: limit.id, target, include_self: includeSelf, enabled, rule: draftToRule(draft, 'fleet') });
    } catch (cause) { setError(cause.message); }
  };
  return html`<div class="usage-rules-tab usage-editor" id="usage-fleet-limit-editor">
    <h4>${limit.id ? `Edit fleet limit "${limit.rule.name}"` : 'New fleet limit'}</h4>
    <div class="usage-rule-fields">
      <label>Covers <select aria-label="Fleet limit target" value=${target} disabled=${busy}
        onChange=${(e) => touch(setTarget)(e.currentTarget.value)}>${options.map((o) => html`<option key=${o.value} value=${o.value}>${o.label}</option>`)}</select></label>
      <label title="Add this node's own spend, and let agent steps reach its agents"><input type="checkbox" checked=${includeSelf} disabled=${busy}
        aria-label="Count this node" onChange=${(e) => touch(setIncludeSelf)(e.currentTarget.checked)}/> count this node</label>
      <label><input type="checkbox" checked=${enabled} disabled=${busy} onChange=${(e) => touch(setEnabled)(e.currentTarget.checked)}/> enabled</label>
    </div>
    <${RuleEditor} draft=${draft} scope="fleet" windows=${[]} disabled=${busy} onChange=${touch(setDraft)}/>
    <p class="modal-hint">Agent steps (notify agents, wrap up, hard stop, block spawns) reach this node's agents when it is counted, and each member's agents in the groups its <code>usage-rules.enforce</code> grant covers. Elsewhere they only alert you.</p>
    <div role="alert" class="cron-create-error">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onCancel}>Back</button>
      ${limit.id ? html`<button type="button" disabled=${busy} onClick=${() => onDelete(limit)}>Delete fleet limit</button>` : null}
      <span class="spacer"></span>
      <button type="button" class="primary" id="usage-fleet-limit-save" disabled=${busy} onClick=${save}>${busy ? 'Saving…' : 'Save fleet limit'}</button>
    </div>
  </div>`;
}
