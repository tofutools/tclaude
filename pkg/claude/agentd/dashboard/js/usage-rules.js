import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { usageRulesApi } from './usage-rules-actions.js';
import {
  LIMIT_PRESETS, PROFILE_PRESETS, actionsFor, agentReadings, agentTitle, assignedNames, barTone, blankRule,
  blankStep, draftToRule, fmtAmount, metricsFor, periodsFor, profileUses, ruleSummary, ruleToDraft, snoozeActive,
} from './usage-rules-model.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);

export const USAGE_RULE_TABS = Object.freeze([
  { id: 'limits', label: 'Limits' },
  { id: 'profiles', label: 'Usage profiles' },
  { id: 'assignments', label: 'Assignments' },
  { id: 'agents', label: 'Agents' },
]);

const DEFAULT_WINDOWS = ['anthropic:five_hour', 'anthropic:seven_day'];

function Bar({ status }) {
  if (!status) return html`<span class="usage-rule-bar-empty muted">no reading yet</span>`;
  const pct = Math.max(0, status.pct || 0);
  return html`<span class=${`usage-rule-reading ${barTone(status)}`} title=${status.next ? `next: ${status.next.action} at ${status.next.at}%` : ''}>
    <span class="usage-rule-bar"><span class="usage-rule-bar-fill" style=${`width:${Math.min(100, pct)}%`}></span></span>
    <span class="usage-rule-figure">${fmtAmount(status.metric, status.value)} / ${fmtAmount(status.metric, status.budget)} · ${Math.round(pct)}%</span>
    ${status.reached ? html`<span class="usage-rule-reached">${status.reached.replace('_', ' ')}</span>` : null}
    ${status.paused ? html`<span class="usage-rule-paused">${status.paused}</span>` : null}
  </span>`;
}

// StepRow edits one rung of a ladder.
function StepRow({ step, scope, disabled, onChange, onRemove }) {
  const set = (patch) => onChange({ ...step, ...patch });
  const notify = step.action === 'notify';
  const toggle = (target) => set({ notify: step.notify.includes(target) ? step.notify.filter((t) => t !== target) : [...step.notify, target] });
  return html`<div class="usage-step-row">
    <label class="usage-step-at">at <input type="number" min="1" max="1000" step="1" value=${step.at} disabled=${disabled}
      aria-label="Step threshold percent" onInput=${(e) => set({ at: e.currentTarget.value })}/>%</label>
    <select aria-label="Step action" value=${step.action} disabled=${disabled}
      onChange=${(e) => set({ action: e.currentTarget.value, notify: e.currentTarget.value === 'notify' ? (step.notify.length ? step.notify : ['operator']) : [] })}>
      ${actionsFor(scope).map((a) => html`<option key=${a.id} value=${a.id}>${a.label}</option>`)}
    </select>
    ${notify ? html`<span class="usage-step-targets">
      <label><input type="checkbox" checked=${step.notify.includes('operator')} disabled=${disabled} onChange=${() => toggle('operator')}/> operator</label>
      <label title=${scope === 'agent' ? 'The agent itself' : 'Every active agent in scope'}><input type="checkbox" checked=${step.notify.includes('agent')} disabled=${disabled} onChange=${() => toggle('agent')}/> ${scope === 'agent' ? 'agent' : 'agents'}</label>
      <input class="usage-step-repeat" value=${step.repeat} disabled=${disabled} placeholder="repeat: 10% / 30m"
        aria-label="Repeat every" title="Repeat this notification every N percentage points (10%) or every N minutes (30m) while above"
        onInput=${(e) => set({ repeat: e.currentTarget.value })}/>
    </span>` : html`<span class="usage-step-targets muted">${{ wrap_up: 'ask to hand off and go idle', hard_stop: 'kill the agent process', block_spawn: 'refuse new spawns until the period rolls over' }[step.action]}</span>`}
    <button type="button" class="tool" aria-label="Remove step" disabled=${disabled} onClick=${onRemove}>×</button>
  </div>`;
}

// RuleEditor edits one rule draft (ruleToDraft shape) for a scope:
// 'global' | 'group' limits or 'agent' profile rules.
function RuleEditor({ draft, scope, windows, disabled, onChange }) {
  const set = (patch) => onChange({ ...draft, ...patch });
  const windowPct = draft.metric === 'window_pct';
  const periods = periodsFor(scope);
  return html`<div class="usage-rule-editor">
    <div class="usage-rule-fields">
      <label>Name <input value=${draft.name} disabled=${disabled} maxlength="64" placeholder="e.g. Monthly spend"
        aria-label="Rule name" onInput=${(e) => set({ name: e.currentTarget.value })}/></label>
      <label>Measure <select aria-label="Metric" value=${draft.metric} disabled=${disabled}
        onChange=${(e) => {
          const metric = e.currentTarget.value;
          set(metric === 'window_pct'
            ? { metric, period: 'window', window: draft.window || windows[0] || DEFAULT_WINDOWS[0], budget: 100 }
            : { metric, period: periods.some((p) => p.id === draft.period) ? draft.period : periods[0].id });
        }}>${metricsFor(scope).map((m) => html`<option key=${m.id} value=${m.id} title=${m.hint}>${m.label}</option>`)}</select></label>
      ${windowPct ? html`<label>Window <input list="usage-rule-windows" value=${draft.window} disabled=${disabled}
          aria-label="Quota window" placeholder="anthropic:five_hour" onInput=${(e) => set({ window: e.currentTarget.value })}/>
        <datalist id="usage-rule-windows">${[...new Set([...windows, ...DEFAULT_WINDOWS])].map((w) => html`<option key=${w} value=${w}/>`)}</datalist></label>`
    : html`<label>Budget $<input type="number" min="0.01" step="0.01" value=${draft.budget} disabled=${disabled}
          aria-label="Budget USD" onInput=${(e) => set({ budget: e.currentTarget.value })}/></label>
        <label>Period <select aria-label="Period" value=${draft.period} disabled=${disabled}
          onChange=${(e) => set({ period: e.currentTarget.value })}>${periods.map((p) => html`<option key=${p.id} value=${p.id}>${p.label}</option>`)}</select></label>`}
      <label class="usage-rule-dry" title="Record and tell the operator what would happen, without messaging agents, stopping them or blocking spawns">
        <input type="checkbox" checked=${draft.dry_run} disabled=${disabled} onChange=${(e) => set({ dry_run: e.currentTarget.checked })}/> dry run</label>
    </div>
    <div class="usage-steps">
      ${draft.steps.map((step, index) => html`<${StepRow} key=${index} step=${step} scope=${scope} disabled=${disabled}
        onChange=${(next) => set({ steps: draft.steps.map((s, i) => (i === index ? next : s)) })}
        onRemove=${() => set({ steps: draft.steps.filter((_, i) => i !== index) })}/>`)}
      <button type="button" class="tool usage-step-add" disabled=${disabled || draft.steps.length >= 16}
        onClick=${() => set({ steps: [...draft.steps, blankStep(Math.min(1000, Math.max(100, ...draft.steps.map((s) => Number(s.at) || 0)) + (draft.steps.length ? 20 : 0)))] })}>＋ step</button>
    </div>
  </div>`;
}

function PresetButtons({ presets, disabled, onPick, blankLabel }) {
  return html`<div class="usage-presets">
    <button type="button" class="tool" disabled=${disabled} onClick=${() => onPick(null)}>${blankLabel}</button>
    ${presets.map((preset) => html`<button key=${preset.id} type="button" class="tool" disabled=${disabled}
      title=${ruleSummary(preset.rule)} onClick=${() => onPick(preset)}>＋ ${preset.label}</button>`)}
  </div>`;
}

function LimitsTab({ overview, busy, onEdit, onToggle }) {
  const limits = overview.limits || [];
  const statusFor = (limit) => (overview.limit_statuses || []).find((s) => s.limit_id === limit.id);
  return html`<div class="usage-rules-tab">
    <p class="modal-hint">Limits cap an aggregate — the whole instance or one group — with one shared counter. Every agent in scope is affected when a step fires.</p>
    <${PresetButtons} presets=${LIMIT_PRESETS} disabled=${busy} blankLabel="＋ Blank limit"
      onPick=${(preset) => onEdit({ id: 0, scope: 'global', group: '', enabled: true, rule: preset ? preset.rule : blankRule('global') })}/>
    ${limits.length ? html`<div class="usage-rule-cards">${limits.map((limit) => html`<div key=${limit.id} class=${`usage-rule-card${limit.enabled ? '' : ' disabled'}`} data-limit-id=${limit.id}>
      <div class="usage-rule-card-head">
        <span class="usage-rule-name">${limit.rule.name}</span>
        <span class="usage-rule-scope">${limit.scope === 'group' ? `group ${limit.group}` : 'global'}</span>
        ${limit.rule.dry_run ? html`<span class="usage-rule-tag">dry run</span>` : null}
        <span class="spacer"></span>
        <label class="usage-rule-enabled"><input type="checkbox" checked=${limit.enabled} disabled=${busy} onChange=${() => onToggle(limit)}/> enabled</label>
        <button type="button" class="tool" disabled=${busy} onClick=${() => onEdit(limit)}>edit</button>
      </div>
      <div class="usage-rule-summary">${ruleSummary(limit.rule)}</div>
      ${limit.enabled ? html`<${Bar} status=${statusFor(limit)}/>` : null}
    </div>`)}</div>` : html`<div class="template-empty">No limits yet. A monthly spend cap is a good start.</div>`}
  </div>`;
}

function ProfilesTab({ overview, busy, onEdit }) {
  const profiles = overview.profiles || [];
  return html`<div class="usage-rules-tab">
    <p class="modal-hint">A usage profile is a set of budgets applied to each agent separately. Assign profiles globally, per group, per spawn profile or per agent; the nearest layer wins.</p>
    <${PresetButtons} presets=${PROFILE_PRESETS} disabled=${busy} blankLabel="＋ Blank profile"
      onPick=${(preset) => onEdit({ id: 0, name: '', descr: '', rules: [preset ? preset.rule : blankRule('agent')] })}/>
    ${profiles.length ? html`<div class="usage-rule-cards">${profiles.map((profile) => {
      const uses = profileUses(overview.assignments, profile.id);
      return html`<div key=${profile.id} class="usage-rule-card" data-profile-id=${profile.id}>
        <div class="usage-rule-card-head">
          <span class="usage-rule-name">${profile.name}</span>
          ${uses.length ? html`<span class="usage-rule-scope">${uses.join(' · ')}</span>` : html`<span class="usage-rule-scope muted">unassigned</span>`}
          <span class="spacer"></span>
          <button type="button" class="tool" disabled=${busy} onClick=${() => onEdit({ ...profile, id: 0, name: `${profile.name} copy` })}>clone</button>
          <button type="button" class="tool" disabled=${busy} onClick=${() => onEdit(profile)}>edit</button>
        </div>
        ${profile.descr ? html`<div class="usage-rule-descr">${profile.descr}</div>` : null}
        ${(profile.rules || []).map((rule) => html`<div key=${rule.name} class="usage-rule-summary"><strong>${rule.name}</strong>: ${ruleSummary(rule)}</div>`)}
      </div>`;
    })}</div>` : html`<div class="template-empty">No usage profiles yet.</div>`}
  </div>`;
}

// ProfileChips picks the ordered profile set for one assignment layer and
// saves it at once.
function ProfileChips({ names, profiles, disabled, onChange, label }) {
  const free = profiles.filter((p) => !names.includes(p.name));
  return html`<span class="usage-assign-chips">
    ${names.map((name) => html`<span key=${name} class="agent-spawn-role-chip">
      <span class="role-chip-inspect">${name}</span>
      <button type="button" class="role-chip-remove" disabled=${disabled} aria-label=${`Remove ${name} from ${label}`}
        onClick=${() => onChange(names.filter((n) => n !== name))}>×</button></span>`)}
    <select value="" disabled=${disabled || !free.length} aria-label=${`Add usage profile to ${label}`}
      onChange=${(e) => { const next = e.currentTarget.value; e.currentTarget.value = ''; if (next) onChange([...names, next]); }}>
      <option value="">${profiles.length ? '＋ add…' : 'no profiles'}</option>
      ${free.map((p) => html`<option key=${p.id} value=${p.name}>${p.name}</option>`)}
    </select>
  </span>`;
}

function AssignmentsTab({ overview, busy, onAssign }) {
  const profiles = overview.profiles || [];
  const [agentPick, setAgentPick] = useState('');
  const row = (scope, target, title, sub = '') => {
    const names = assignedNames(overview.assignments, scope, target);
    return html`<div key=${`${scope}:${target}`} class="usage-assign-row">
      <span class="usage-assign-target">${title}${sub ? html` <span class="muted">${sub}</span>` : null}</span>
      <${ProfileChips} names=${names} profiles=${profiles} disabled=${busy} label=${title}
        onChange=${(next) => onAssign(scope, target, next)}/>
    </div>`;
  };
  const agentRows = [...new Set((overview.assignments || []).filter((a) => a.scope === 'agent').map((a) => a.scope_id))];
  const pickable = (overview.agents || []).filter((a) => !agentRows.includes(a.id));
  return html`<div class="usage-rules-tab">
    <p class="modal-hint">An agent uses the profiles of its nearest non-empty layer: agent › its groups (combined) › global default. A spawn profile's usage profiles are copied onto each agent it spawns.</p>
    <h4>Global default</h4>
    ${row('global', '', 'All agents')}
    <h4>Groups</h4>
    ${(overview.groups || []).length ? overview.groups.map((g) => row('group', g, g)) : html`<div class="muted">No groups.</div>`}
    <h4>Spawn profiles</h4>
    ${(overview.spawn_profiles || []).length ? overview.spawn_profiles.map((sp) => row('spawn_profile', sp, sp)) : html`<div class="muted">No spawn profiles.</div>`}
    <h4>Agents</h4>
    ${agentRows.map((id) => row('agent', id, agentTitle(overview, id) || id, agentTitle(overview, id) ? id : ''))}
    <div class="usage-assign-row">
      <select aria-label="Agent to assign" value=${agentPick} disabled=${busy || !pickable.length} onChange=${(e) => setAgentPick(e.currentTarget.value)}>
        <option value="">${pickable.length ? 'Pick an agent…' : 'No other agents'}</option>
        ${pickable.map((a) => html`<option key=${a.id} value=${a.id}>${a.title || a.id}</option>`)}
      </select>
      ${agentPick ? html`<${ProfileChips} names=${[]} profiles=${profiles} disabled=${busy} label=${agentTitle(overview, agentPick) || agentPick}
        onChange=${(next) => { setAgentPick(''); onAssign('agent', agentPick, next); }}/>` : null}
    </div>
  </div>`;
}

function AgentsTab({ overview, busy, onAdjust }) {
  const rows = agentReadings(overview);
  // extending: the "<agent>|<rule key>" whose extra-budget field is open.
  const [extending, setExtending] = useState('');
  const [amount, setAmount] = useState('');
  const openExtend = (entry, status) => {
    setExtending(`${entry.agentID}|${status.rule_key}`);
    setAmount(String(entry.override?.extensions?.[status.rule_key] || ''));
  };
  const applyExtend = (entry, status) => {
    const value = amount.trim() === '' ? 0 : Number(amount);
    if (!Number.isFinite(value) || value < 0) { shellToast('extra budget must be a non-negative amount', true); return; }
    setExtending('');
    onAdjust(entry.agentID, { rule_key: status.rule_key, extra_budget: value });
  };
  return html`<div class="usage-rules-tab">
    <p class="modal-hint">Each agent's budgets from its usage profiles. Snoozed or exempt agents keep counting but nothing fires; extra budget raises one rule for one agent.</p>
    ${rows.length ? rows.map((entry) => {
      const snoozed = snoozeActive(entry.override);
      const exempt = !!entry.override?.exempt;
      return html`<div key=${entry.agentID} class="usage-rule-card" data-agent-id=${entry.agentID}>
        <div class="usage-rule-card-head">
          <span class="usage-rule-name">${entry.title || entry.agentID}</span>
          ${entry.title ? html`<span class="usage-rule-scope muted">${entry.agentID}</span>` : null}
          <span class="spacer"></span>
          <button type="button" class="tool" disabled=${busy} title="Pause this agent's steps for an hour"
            onClick=${() => onAdjust(entry.agentID, { snooze_minutes: snoozed ? 0 : 60 })}>${snoozed ? 'unsnooze' : 'snooze 1h'}</button>
          <button type="button" class="tool" disabled=${busy} title="Exempt this agent from every usage profile"
            onClick=${() => onAdjust(entry.agentID, { exempt: !exempt })}>${exempt ? 'unexempt' : 'exempt'}</button>
        </div>
        ${entry.statuses.map((status) => html`<div key=${status.rule_key} class="usage-agent-rule">
          <span class="usage-agent-rule-name" title=${`${status.profile} (${status.layer} layer)`}>${status.name}</span>
          <${Bar} status=${status}/>
          ${entry.override?.extensions?.[status.rule_key] ? html`<span class="usage-rule-tag">+${fmtAmount(status.metric, entry.override.extensions[status.rule_key])}</span>` : null}
          ${extending === `${entry.agentID}|${status.rule_key}` ? html`<span class="usage-extend">
            <input type="number" min="0" step="1" value=${amount} aria-label="Extra budget" placeholder="extra (0 clears)"
              onInput=${(e) => setAmount(e.currentTarget.value)}
              onKeyDown=${(e) => { if (e.key === 'Enter') { e.preventDefault(); applyExtend(entry, status); } }}/>
            <button type="button" class="tool" disabled=${busy} onClick=${() => applyExtend(entry, status)}>set</button>
            <button type="button" class="tool" onClick=${() => setExtending('')}>cancel</button>
          </span>` : html`<button type="button" class="tool" disabled=${busy} title="Raise this rule's budget for this agent only"
            onClick=${() => openExtend(entry, status)}>extend…</button>`}
        </div>`)}
        ${!entry.statuses.length ? html`<div class="muted">No active budgets.${exempt ? ' Exempt.' : ''}${snoozed ? ' Snoozed.' : ''}</div>` : null}
      </div>`;
    }) : html`<div class="template-empty">No agent is under a usage profile yet. Assign one on the Assignments tab.</div>`}
  </div>`;
}

function LimitEditor({ limit, overview, busy, onSave, onDelete, onCancel, onDirty }) {
  const [draft, setDraft] = useState(() => ruleToDraft(limit.rule));
  const [scope, setScope] = useState(limit.scope);
  const [group, setGroup] = useState(limit.group || overview.groups?.[0] || '');
  const [enabled, setEnabled] = useState(limit.enabled);
  const [error, setError] = useState('');
  const touch = (fn) => (value) => { onDirty(); fn(value); };
  const save = () => {
    try {
      setError('');
      onSave({ id: limit.id, scope, group, enabled, rule: draftToRule(draft, scope) });
    } catch (cause) { setError(cause.message); }
  };
  return html`<div class="usage-rules-tab usage-editor" id="usage-limit-editor">
    <h4>${limit.id ? `Edit limit "${limit.rule.name}"` : 'New limit'}</h4>
    <div class="usage-rule-fields">
      <label>Scope <select aria-label="Limit scope" value=${scope} disabled=${busy || !!limit.id}
        onChange=${(e) => {
          const next = e.currentTarget.value;
          touch(setScope)(next);
          // Quota windows are account-wide: a group limit measures spend.
          if (next !== 'global' && draft.metric === 'window_pct') setDraft({ ...draft, metric: 'total_usd', period: 'month', window: '', budget: 500 });
        }}>
        <option value="global">Global (whole instance)</option>
        <option value="group" disabled=${!(overview.groups || []).length}>Group</option>
      </select></label>
      ${scope === 'group' ? html`<label>Group <select aria-label="Limit group" value=${group} disabled=${busy || !!limit.id}
        onChange=${(e) => touch(setGroup)(e.currentTarget.value)}>${(overview.groups || []).map((g) => html`<option key=${g} value=${g}>${g}</option>`)}</select></label>` : null}
      <label><input type="checkbox" checked=${enabled} disabled=${busy} onChange=${(e) => touch(setEnabled)(e.currentTarget.checked)}/> enabled</label>
    </div>
    <${RuleEditor} draft=${draft} scope=${scope} windows=${overview.windows || []} disabled=${busy}
      onChange=${touch(setDraft)}/>
    <div role="alert" class="cron-create-error">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onCancel}>Back</button>
      ${limit.id ? html`<button type="button" disabled=${busy} onClick=${() => onDelete(limit)}>Delete limit</button>` : null}
      <span class="spacer"></span>
      <button type="button" class="primary" id="usage-limit-save" disabled=${busy} onClick=${save}>${busy ? 'Saving…' : 'Save limit'}</button>
    </div>
  </div>`;
}

function ProfileEditor({ profile, busy, onSave, onDelete, onCancel, onDirty }) {
  const [name, setName] = useState(profile.name);
  const [descr, setDescr] = useState(profile.descr || '');
  const [rules, setRules] = useState(() => (profile.rules || []).map(ruleToDraft));
  const [error, setError] = useState('');
  const touch = (fn) => (value) => { onDirty(); fn(value); };
  const save = () => {
    try {
      setError('');
      if (!name.trim()) throw new Error('give the profile a name');
      onSave({ id: profile.id, body: { name: name.trim(), descr: descr.trim(), rules: rules.map((r) => draftToRule(r, 'agent')) } });
    } catch (cause) { setError(cause.message); }
  };
  return html`<div class="usage-rules-tab usage-editor" id="usage-profile-editor">
    <h4>${profile.id ? `Edit usage profile "${profile.name}"` : 'New usage profile'}</h4>
    <div class="usage-rule-fields">
      <label>Name <input aria-label="Usage profile name" value=${name} disabled=${busy} maxlength="64" placeholder="e.g. standard"
        onInput=${(e) => touch(setName)(e.currentTarget.value)}/></label>
      <label class="usage-wide">Description <input aria-label="Usage profile description" value=${descr} disabled=${busy}
        onInput=${(e) => touch(setDescr)(e.currentTarget.value)}/></label>
    </div>
    ${rules.map((rule, index) => html`<div key=${index} class="usage-rule-block">
      <${RuleEditor} draft=${rule} scope="agent" windows=${[]} disabled=${busy}
        onChange=${touch((next) => setRules(rules.map((r, i) => (i === index ? next : r))))}/>
      <button type="button" class="tool usage-rule-remove" disabled=${busy} onClick=${() => touch(setRules)(rules.filter((_, i) => i !== index))}>remove rule</button>
    </div>`)}
    <${PresetButtons} presets=${PROFILE_PRESETS} disabled=${busy} blankLabel="＋ Blank rule"
      onPick=${(preset) => touch(setRules)([...rules, ruleToDraft(preset ? preset.rule : blankRule('agent'))])}/>
    <div role="alert" class="cron-create-error">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${onCancel}>Back</button>
      ${profile.id ? html`<button type="button" disabled=${busy} onClick=${() => onDelete(profile)}>Delete profile</button>` : null}
      <span class="spacer"></span>
      <button type="button" class="primary" id="usage-profile-save" disabled=${busy} onClick=${save}>${busy ? 'Saving…' : 'Save profile'}</button>
    </div>
  </div>`;
}

// UsageRulesDialog is the "Usage limits & budgets" manager: global/group
// limits, usage profiles, their assignments and per-agent readings.
// descriptor.tab picks the opening tab.
export function UsageRulesDialog({ descriptor, onClose, confirmDiscard, api = usageRulesApi, confirm = shellConfirm }) {
  const [overview, setOverview] = useState(null);
  const [loadError, setLoadError] = useState('');
  const [tab, setTab] = useState(USAGE_RULE_TABS.some((t) => t.id === descriptor?.tab) ? descriptor.tab : 'limits');
  const [editing, setEditing] = useState(null); // {kind: 'limit'|'profile', item}
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const live = useRef(true);
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const reload = async () => {
    try {
      const next = await api.overview();
      if (live.current) { setOverview(next); setLoadError(''); }
    } catch (cause) { if (live.current) setLoadError(cause.message); }
  };
  useEffect(() => { live.current = true; void reload(); return () => { live.current = false; }; }, []);
  const run = async (work, done = '') => {
    if (busyRef.current) return false;
    busyRef.current = true; setBusy(true);
    try {
      await work();
      if (done) shellToast(done);
      await reload();
      // The daemon re-evaluates in the background; pick up its new readings.
      setTimeout(() => { if (live.current) void reload(); }, 1500);
      return true;
    } catch (cause) {
      shellToast(cause.message, true);
      return false;
    } finally { busyRef.current = false; if (live.current) setBusy(false); }
  };
  const leaveEditor = async () => {
    if (dirty && !await confirmDiscard()) return;
    setEditing(null); setDirty(false);
  };
  const finish = (ok) => { if (ok) { setEditing(null); setDirty(false); } };
  const saveLimit = async ({ id, scope, group, enabled, rule }) => finish(await run(
    () => (id ? api.updateLimit(id, { rule, enabled }) : api.createLimit({ scope, group: scope === 'group' ? group : '', rule, enabled })),
    `saved limit ${rule.name}`));
  const deleteLimit = async (limit) => {
    if (!await confirm({ title: 'Delete limit?', body: `Delete the ${limit.scope} limit "${limit.rule.name}"? Its alert history goes with it.`, okLabel: 'Delete' })) return;
    finish(await run(() => api.deleteLimit(limit.id), `deleted limit ${limit.rule.name}`));
  };
  const toggleLimit = (limit) => void run(() => api.updateLimit(limit.id, { rule: limit.rule, enabled: !limit.enabled }));
  const saveProfile = async ({ id, body }) => finish(await run(
    () => (id ? api.updateProfile(id, body) : api.createProfile(body)), `saved usage profile ${body.name}`));
  const deleteProfile = async (profile) => {
    if (!await confirm({ title: 'Delete usage profile?', body: `Delete "${profile.name}"? Every assignment of it is removed too.`, okLabel: 'Delete' })) return;
    finish(await run(() => api.deleteProfile(profile.id), `deleted usage profile ${profile.name}`));
  };
  const assign = (scope, target, profiles) => void run(() => api.assign(scope, target, profiles));
  const adjust = (agentID, body) => void run(() => api.adjust(agentID, body));
  const switchTab = async (id) => {
    if (editing) { if (dirty && !await confirmDiscard()) return; setEditing(null); setDirty(false); }
    setTab(id);
  };
  const markDirty = () => setDirty(true);
  let body;
  if (!overview) {
    body = loadError
      ? html`<div class="cron-create-error" role="alert">${loadError} <button type="button" onClick=${() => void reload()}>Retry</button></div>`
      : html`<div class="empty">Loading…</div>`;
  } else if (editing?.kind === 'limit') {
    body = html`<${LimitEditor} limit=${editing.item} overview=${overview} busy=${busy} onDirty=${markDirty}
      onSave=${saveLimit} onDelete=${deleteLimit} onCancel=${() => void leaveEditor()}/>`;
  } else if (editing?.kind === 'profile') {
    body = html`<${ProfileEditor} profile=${editing.item} busy=${busy} onDirty=${markDirty}
      onSave=${saveProfile} onDelete=${deleteProfile} onCancel=${() => void leaveEditor()}/>`;
  } else if (tab === 'limits') {
    body = html`<${LimitsTab} overview=${overview} busy=${busy} onToggle=${toggleLimit}
      onEdit=${(item) => { setEditing({ kind: 'limit', item }); setDirty(!item.id); }}/>`;
  } else if (tab === 'profiles') {
    body = html`<${ProfilesTab} overview=${overview} busy=${busy}
      onEdit=${(item) => { setEditing({ kind: 'profile', item }); setDirty(!item.id); }}/>`;
  } else if (tab === 'assignments') {
    body = html`<${AssignmentsTab} overview=${overview} busy=${busy} onAssign=${assign}/>`;
  } else {
    body = html`<${AgentsTab} overview=${overview} busy=${busy} onAdjust=${adjust}/>`;
  }
  return html`<${Overlay} id="usage-rules-modal" labelledby="usage-rules-title" dialogClass="cron-create-modal usage-rules-modal"
    onClose=${onClose} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
    <h3 id="usage-rules-title">Usage limits & budgets</h3>
    <div class="fa-subtabs usage-rules-tabs" role="tablist">${USAGE_RULE_TABS.map((t) => html`<button type="button" role="tab" key=${t.id}
      class=${`fa-subtab${tab === t.id ? ' on' : ''}`} aria-selected=${tab === t.id ? 'true' : 'false'} data-usage-rules-tab=${t.id}
      onClick=${() => void switchTab(t.id)}>${t.label}</button>`)}</div>
    ${body}
    ${editing ? null : html`<div class="modal-buttons">
      ${overview?.evaluated_at ? html`<span class="muted usage-rules-evaluated">readings from ${new Date(overview.evaluated_at).toLocaleTimeString()}</span>` : null}
      <span class="spacer"></span>
      <button type="button" disabled=${busy} onClick=${() => void requestClose()}>Close</button>
    </div>`}
  </${Overlay}>`;
}
