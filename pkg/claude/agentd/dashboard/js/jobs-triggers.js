import { Fragment, h } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { relTime } from './helpers.js';

const html = htm.bind(h);
const TRIGGER_SOURCES = Object.freeze([
  { value: 'pr.opened', label: 'PR opened', summary: 'a PR is opened' },
  { value: 'pr.updated', label: 'PR updated', summary: 'a PR is updated' },
  { value: 'pr.merged', label: 'PR merged', summary: 'a PR is merged' },
  { value: 'ci.failed', label: 'CI failed', summary: 'CI transitions to failing' },
  { value: 'ci.succeeded', label: 'CI succeeded', summary: 'CI transitions to passing' },
]);
const PLACEHOLDERS = [
  '{{pr.url}}', '{{pr.number}}', '{{pr.branch}}', '{{pr.author_agent}}', '{{group}}',
  '{{event.source}}', '{{event.previous_state}}', '{{event.current_state}}',
];

function triggerSource(source) {
  return TRIGGER_SOURCES.find((entry) => entry.value === source) ||
    { value: source || 'pr.opened', label: source || 'PR opened', summary: source || 'a PR event occurs' };
}

function secondsLabel(value) {
  const seconds = Number(value || 0);
  if (!seconds) return 'none';
  if (seconds % 86400 === 0) return `${seconds / 86400}d`;
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
}

function firingField(row, lower, upper) {
  return row?.[lower] ?? row?.[upper];
}

function triggerState(rule) {
  if (!rule.enabled) return { key: 'disabled', label: 'disabled' };
  const last = rule.firings?.[0];
  const finished = firingField(last, 'finished_at', 'FinishedAt');
  const cooldown = Number(rule.cooldown_seconds || 0) * 1000;
  if (finished && cooldown > 0) {
    const left = new Date(finished).getTime() + cooldown - Date.now();
    if (left > 0) return { key: 'cooling', label: `cooling down ${secondsLabel(Math.ceil(left / 1000))}` };
  }
  return { key: 'armed', label: 'armed' };
}

function whenSummary(rule) {
  const author = rule.author_is_agent === true ? 'agent PRs' : rule.author_is_agent === false ? 'human PRs' : 'any author';
  const drafts = rule.draft_filter === 'exclude' ? 'no drafts' : rule.draft_filter === 'only' ? 'drafts only' : 'drafts included';
  return `${rule.source || 'pr.opened'} · ${author} · ${drafts}`;
}

function actionSummary(action) {
  if (action.type === 'spawn') {
    const spawn = action.spawn || {};
    const roles = (spawn.roles || []).join(', ');
    return `spawn ${spawn.profile || 'profile required'}${roles ? ` · ${roles}` : ''}`;
  }
  const message = action.message || {};
  return `message ${message.target === 'group' ? 'group' : 'PR author'}`;
}

function scopeSummary(rule) {
  return rule.scope === 'group' ? `group:${rule.group || ('#' + rule.group_id)}` : 'global';
}

function ownerSummary(rule) {
  return rule.operator_authored ? 'operator owned' : `owner ${rule.owner_agent || 'agent'}`;
}

function lastFiring(rule) {
  const firing = rule.firings?.[0];
  if (!firing) return html`<span class="muted">never</span>`;
  const outcome = firingField(firing, 'outcome', 'Outcome') || 'unknown';
  const started = firingField(firing, 'started_at', 'StartedAt');
  return html`<${Fragment}><strong class=${outcome === 'ok' ? 'trigger-ok' : 'trigger-warn'}>${outcome}</strong>
    <span class="muted">${started ? relTime(started) : ''}</span></${Fragment}>`;
}

function TriggerInspector({ rule, actions, onEdit }) {
  const [firings, setFirings] = useState(rule.firings || []);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  useEffect(() => {
    let active = true;
    setLoading(true);
    actions.loadTriggerFirings(rule.id).then((rows) => {
      if (active) setFirings(rows);
    }).catch((err) => { if (active) setError(err.message || String(err)); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [rule.id, rule.row_version]);
  const configured = [
    ['source', triggerSource(rule.source).label],
    ['author', rule.author_is_agent === true ? 'agent only' : rule.author_is_agent === false ? 'human only' : 'any author'],
    ['draft policy', rule.draft_filter || 'include'],
    ['scope', scopeSummary(rule)],
    ['debounce / cooldown', `${secondsLabel(rule.debounce_seconds)} / ${secondsLabel(rule.cooldown_seconds)}`],
  ];
  return html`<div class="trigger-inspector" onClick=${(event) => event.stopPropagation()}>
    <div class="trigger-inspector-head">
      <div><strong>Why this rule matches</strong><span class="muted">Configured conditions; fire-time permission checks are recorded below.</span></div>
      <button type="button" onClick=${onEdit}>edit rule</button>
    </div>
    <div class="trigger-inspector-grid">
      <section>
        <h4>Configured match</h4>
        <div class="trigger-verdicts">${configured.map(([label, detail]) => html`
          <div key=${label}><span class="muted">•</span>
            <strong>${label}</strong><span class="muted">— ${detail}</span></div>`)}</div>
        <div class="trigger-fact-note">The current API does not expose a live fact snapshot. Unknown event facts are evaluated at firing time, never displayed as false.</div>
      </section>
      <section>
        <h4>Recent firings</h4>
        ${error && html`<div class="jobs-error" role="alert">${error}</div>`}
        ${loading ? html`<div class="muted">Loading firing history…</div>` : firings.length === 0
          ? html`<div class="muted">No firings recorded.</div>`
          : html`<div class="trigger-firings">${firings.map((firing) => {
              const id = firingField(firing, 'id', 'ID');
              const started = firingField(firing, 'started_at', 'StartedAt');
              const outcome = firingField(firing, 'outcome', 'Outcome');
              const detail = firingField(firing, 'detail', 'Detail');
              const eventRef = firingField(firing, 'event_ref', 'EventRef');
              const source = firingField(firing, 'source', 'Source');
              const previousState = firingField(firing, 'previous_state', 'PreviousState');
              const currentState = firingField(firing, 'current_state', 'CurrentState');
              const outcomes = firingField(firing, 'actions', 'Actions') || [];
              return html`<div class="trigger-firing" key=${id}>
                <div><strong class=${outcome === 'ok' ? 'trigger-ok' : 'trigger-warn'}>${outcome}</strong>
                  <span>${started ? new Date(started).toLocaleString() : ''}</span>
                  <span class="muted">${eventRef || ''}</span></div>
                ${(source || previousState || currentState) && html`<div class="trigger-firing-context">
                  ${source && html`<span><span class="muted">source</span> <code>${source}</code></span>`}
                  ${(previousState || currentState) && html`<span><span class="muted">transition</span> ${previousState || 'unknown'} → ${currentState || 'unknown'}</span>`}
                </div>`}
                ${detail && html`<div class="muted">${detail}</div>`}
                ${outcomes.map((action) => {
                  const actionOutcome = firingField(action, 'outcome', 'Outcome');
                  const success = ['spawned', 'queued', 'ok'].includes(actionOutcome);
                  const failure = !success && (/denied|invalid|not_found|\bio\b|fail|error/.test(actionOutcome) ||
                    ['max_live_workers', 'rate_limited'].includes(actionOutcome));
                  return html`<div class="trigger-action-outcome" key=${firingField(action, 'id', 'ID')}>
                    <span class=${success ? 'trigger-pass' : failure ? 'trigger-fail' : 'trigger-warn'}>${success ? '✓' : failure ? '×' : '!'}</span>
                    <code>${firingField(action, 'action_type', 'ActionType')}</code>
                    <strong>${actionOutcome}</strong>
                    <span class="muted">${firingField(action, 'detail', 'Detail') || ''}</span>
                  </div>`;
                })}
              </div>`;
            })}</div>`}
      </section>
    </div>
  </div>`;
}

function TriggerRow({ rule, selected, actions, onSelect, onEdit, onChanged }) {
  const state = triggerState(rule);
  return html`<${Fragment}>
    <tr class=${`trigger-row${selected ? ' selected' : ''}`} data-key=${`trigger-${rule.id}`}
      tabindex="0" onClick=${() => onSelect(rule.id)}
      onKeyDown=${(event) => {
        if (event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
        event.preventDefault();
        onSelect(rule.id);
      }} aria-expanded=${selected ? 'true' : 'false'}>
      <td class="trigger-disclosure">${selected ? '▾' : '▸'}</td>
      <td><div class="rowname">${rule.name}</div><div class="muted">${ownerSummary(rule)}</div></td>
      <td><strong class="trigger-source">${rule.source}</strong><div class="muted">${whenSummary(rule).replace(`${rule.source} · `, '')}</div></td>
      <td><span class=${`trigger-scope ${rule.scope}`}>${scopeSummary(rule)}</span></td>
      <td>${(rule.actions || []).map((action, index) => html`<div key=${index} class="trigger-action-chip">${actionSummary(action)}</div>`)}</td>
      <td><${Fragment}>${lastFiring(rule)}</${Fragment}></td>
      <td><span class=${`trigger-state ${state.key}`}>${state.label}</span></td>
      <td><label class="trigger-switch" onClick=${(event) => event.stopPropagation()}>
        <input type="checkbox" checked=${!!rule.enabled} aria-label=${`${rule.enabled ? 'Disable' : 'Enable'} ${rule.name}`}
          onChange=${async (event) => {
            const input = event.currentTarget;
            if (await actions.toggleTrigger(rule)) onChanged();
            else input.checked = !!rule.enabled;
          }} /><span></span></label></td>
      <td><div class="row-actions" onClick=${(event) => event.stopPropagation()}>
        <button type="button" onClick=${onEdit}>edit</button>
        <button type="button" class="danger" onClick=${async () => { if (await actions.deleteTrigger(rule)) onChanged(); }}>delete</button>
      </div></td>
    </tr>
    ${selected && html`<tr class="trigger-inspector-row"><td colspan="9">
      <${TriggerInspector} rule=${rule} actions=${actions} onEdit=${onEdit} />
    </td></tr>`}
  </${Fragment}>`;
}

export function TriggerWorkspace({ state, actions }) {
  const [rules, setRules] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [selected, setSelected] = useState(null);
  const [query, setQuery] = useState('');
  const revision = state.triggerRevision.value;
  const load = async () => {
    // Keep the last authoritative rows visible during a post-mutation reload;
    // only the first load needs to replace the table with a spinner.
    if (rules.length === 0) setLoading(true);
    setError('');
    try { setRules(await actions.loadTriggers()); }
    catch (err) { setError(err.message || String(err)); }
    finally { setLoading(false); }
  };
  useEffect(() => { void load(); }, [revision]);
  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return rules;
    return rules.filter((rule) => [rule.name, rule.source, rule.group, scopeSummary(rule),
      ...(rule.actions || []).map(actionSummary)].some((value) => String(value || '').toLowerCase().includes(needle)));
  }, [rules, query]);
  return html`<div class="triggers-workspace">
    <div class="filter-bar trigger-filter-bar">
      <input type="search" aria-label="Filter triggers" placeholder="Filter triggers (name + source + scope + action)"
        value=${query} onInput=${(event) => setQuery(event.currentTarget.value)} />
      <span class="filter-count">${shown.length}${query ? ` / ${rules.length}` : ` trigger${rules.length === 1 ? '' : 's'}`}</span>
      <button class="clear-filter" title="Clear trigger filter" onClick=${() => setQuery('')}>×</button>
      <span class="spacer"></span>
      <button id="trigger-create-open" class="primary" onClick=${() => state.openTriggerCreate()}>+ new trigger</button>
    </div>
    ${error && html`<div class="jobs-error" role="alert">${error}<button onClick=${() => void load()}>retry</button></div>`}
    ${loading ? html`<div class="empty">Loading triggers…</div>` : shown.length === 0
      ? html`<div class="empty">${rules.length ? 'No triggers match this filter.' : 'No triggers yet. Create an event rule to automate a bounded response.'}</div>`
      : html`<table class="triggers-table"><thead><tr><th></th><th>Trigger</th><th>When</th><th>Where</th><th>Then</th><th>Last fired</th><th>State</th><th>Enabled</th><th></th></tr></thead>
          <tbody>${shown.map((rule) => html`<${TriggerRow} key=${rule.id} rule=${rule} selected=${selected === rule.id}
            actions=${actions} onSelect=${(id) => setSelected(selected === id ? null : id)}
            onEdit=${() => state.openTriggerEdit(rule)} onChanged=${() => void load()} />`)}</tbody></table>`}
  </div>`;
}

function emptyAction(type = 'spawn') {
  return type === 'message'
    ? { type: 'message', message: { target: 'pr.author_agent', subject_template: '', body_template: '' } }
    : { type: 'spawn', spawn: { profile: '', roles: [], name_template: '', instruction_template: '', max_live_workers: 1, worker_deadline_seconds: 3600 } };
}

function createDraft(rule = {}) {
  return {
    name: rule.name || '', enabled: rule.enabled ?? true, row_version: rule.row_version,
    source: rule.source || 'pr.opened', author_is_agent: rule.author_is_agent === undefined ? true : rule.author_is_agent,
    draft_filter: rule.draft_filter || 'exclude', debounce_seconds: Number(rule.debounce_seconds || 0),
    cooldown_seconds: Number(rule.cooldown_seconds || 0), scope: rule.scope || 'global', group: rule.group || '',
    actions: (rule.actions || []).length ? structuredClone(rule.actions) : [emptyAction()],
  };
}

function draftValid(draft) {
  if (!draft.name.trim() || (draft.scope === 'group' && !draft.group) || !draft.actions.length) return false;
  return draft.actions.every((action) => action.type === 'spawn'
    ? action.spawn?.profile?.trim() && action.spawn?.instruction_template?.trim() && Number(action.spawn?.max_live_workers) > 0
    : action.message?.body_template?.trim());
}

function Step({ name, summary, expanded, onToggle, valid, children }) {
  return html`<section class=${`trigger-editor-step${expanded ? ' expanded' : ''}`}>
    <button type="button" class="trigger-step-head" onClick=${onToggle} aria-expanded=${expanded ? 'true' : 'false'}>
      <span class=${`trigger-step-label ${name.toLowerCase()}`}>${name}</span>
      <span class="trigger-step-summary">${summary}</span>
      <span class=${valid ? 'trigger-step-valid' : 'trigger-step-required'}>${valid ? '✓' : 'required'}</span>
      <span>${expanded ? '▾' : '▸'}</span>
    </button>
    ${expanded && html`<div class="trigger-step-body">${children}</div>`}
  </section>`;
}

function ActionEditor({ action, index, update, remove }) {
  const insert = (field, token) => {
    if (action.type === 'spawn') update({ ...action, spawn: { ...action.spawn, [field]: `${action.spawn?.[field] || ''}${token}` } });
    else update({ ...action, message: { ...action.message, [field]: `${action.message?.[field] || ''}${token}` } });
  };
  return html`<div class="trigger-action-editor">
    <div class="trigger-action-editor-head"><strong>${index + 1} · ${action.type === 'spawn' ? '⚡ Spawn agent' : '✉ Message'}</strong>
      <select value=${action.type} onChange=${(event) => update(emptyAction(event.currentTarget.value))}>
        <option value="spawn">spawn agent</option><option value="message">message</option>
      </select><button type="button" class="danger" onClick=${remove} disabled=${false}>×</button></div>
    ${action.type === 'spawn' ? html`<div class="trigger-action-fields">
      <label>Profile<input value=${action.spawn?.profile || ''} required placeholder="spawn profile name"
        onInput=${(event) => update({ ...action, spawn: { ...action.spawn, profile: event.currentTarget.value } })} /></label>
      <label>Roles<input value=${(action.spawn?.roles || []).join(', ')} placeholder="reviewer, read-only"
        onInput=${(event) => update({ ...action, spawn: { ...action.spawn, roles: event.currentTarget.value.split(',').map((v) => v.trim()).filter(Boolean) } })} /></label>
      <label>Name template<input value=${action.spawn?.name_template || ''} placeholder="review-{{pr.number}}"
        onInput=${(event) => update({ ...action, spawn: { ...action.spawn, name_template: event.currentTarget.value } })} /></label>
      <label class="trigger-template-field">Instruction template<textarea rows="5" required value=${action.spawn?.instruction_template || ''}
        placeholder="Review {{pr.url}} and report significant findings."
        onInput=${(event) => update({ ...action, spawn: { ...action.spawn, instruction_template: event.currentTarget.value } })}></textarea>
        <span class="trigger-placeholder-chips">${PLACEHOLDERS.map((token) => html`<button type="button" key=${token} onClick=${() => insert('instruction_template', token)}>${token}</button>`)}</span></label>
      <label>Max live workers<input type="number" min="1" value=${action.spawn?.max_live_workers || 1}
        onInput=${(event) => update({ ...action, spawn: { ...action.spawn, max_live_workers: Number(event.currentTarget.value) } })} /></label>
      <label>Worker deadline (seconds)<input type="number" min="0" value=${action.spawn?.worker_deadline_seconds || 0}
        onInput=${(event) => update({ ...action, spawn: { ...action.spawn, worker_deadline_seconds: Number(event.currentTarget.value) } })} /></label>
    </div>` : html`<div class="trigger-action-fields">
      <label>Target<select value=${action.message?.target || 'pr.author_agent'}
        onChange=${(event) => update({ ...action, message: { ...action.message, target: event.currentTarget.value } })}>
        <option value="pr.author_agent">PR author agent</option><option value="group">event group</option></select></label>
      <label>Subject template<input value=${action.message?.subject_template || ''}
        onInput=${(event) => update({ ...action, message: { ...action.message, subject_template: event.currentTarget.value } })} /></label>
      <label class="trigger-template-field">Body template<textarea rows="4" required value=${action.message?.body_template || ''}
        onInput=${(event) => update({ ...action, message: { ...action.message, body_template: event.currentTarget.value } })}></textarea>
        <span class="trigger-placeholder-chips">${PLACEHOLDERS.map((token) => html`<button type="button" key=${token} onClick=${() => insert('body_template', token)}>${token}</button>`)}</span></label>
    </div>`}
  </div>`;
}

export function TriggerDialogRoot({ state, actions }) {
  const descriptor = state.triggerDialog.value;
  if (!descriptor) return null;
  return html`<${TriggerDialog} key=${descriptor.launchID} descriptor=${descriptor} state=${state} actions=${actions} />`;
}

function TriggerDialog({ descriptor, state, actions }) {
  const editing = descriptor.kind === 'edit';
  const [draft, setDraft] = useState(() => createDraft(descriptor.rule));
  const [step, setStep] = useState('when');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const snapshot = state.view.value.dashboard;
  const groups = (snapshot?.groups || []).filter((group) => !group.virtual).map((group) => group.name);
  const source = triggerSource(draft.source);
  const updateAction = (index, action) => setDraft((value) => ({ ...value, actions: value.actions.map((item, i) => i === index ? action : item) }));
  const removeAction = (index) => setDraft((value) => ({ ...value, actions: value.actions.filter((_, i) => i !== index) }));
  const whenValid = !!draft.source;
  const whereValid = draft.scope === 'global' || !!draft.group;
  const thenValid = draft.actions.length > 0 && draftValid({ ...draft, name: draft.name || 'x', scope: 'global' });
  const save = async (event) => {
    event.preventDefault();
    if (!draftValid(draft) || busy) return;
    setBusy(true); setError('');
    try {
      await actions.saveTrigger({ editing, id: descriptor.rule?.id, payload: draft });
      state.closeTriggerDialog();
    } catch (err) { setError(err.message || String(err)); }
    finally { setBusy(false); }
  };
  return html`<${Overlay} id="trigger-modal" dialogClass="cron-create-modal trigger-modal" labelledby="trigger-modal-title"
    onClose=${state.closeTriggerDialog} blocked=${busy} resizeKey="tclaude.dash.modalSize.trigger">
    <form onSubmit=${save}>
      <div class="trigger-modal-heading"><div><h3 id="trigger-modal-title">${editing ? `Edit trigger — ${descriptor.rule.name}` : 'New trigger'}</h3>
        <p class="muted">Build a bounded reaction from cause to scope to ordered actions.</p></div>
        <label class="trigger-enabled-label"><input type="checkbox" checked=${draft.enabled}
          onChange=${(event) => setDraft({ ...draft, enabled: event.currentTarget.checked })} /> enabled</label></div>
      <label class="trigger-name-field">Name<input autofocus required maxlength="80" value=${draft.name}
        onInput=${(event) => setDraft({ ...draft, name: event.currentTarget.value })} /></label>
      <${Step} name="WHEN" expanded=${step === 'when'} onToggle=${() => setStep(step === 'when' ? '' : 'when')}
        valid=${whenValid} summary=${`${source.summary} · ${draft.author_is_agent === true ? 'by an agent' : draft.author_is_agent === false ? 'by a human' : 'any author'} · ${draft.draft_filter === 'exclude' ? 'not a draft' : draft.draft_filter === 'only' ? 'draft only' : 'drafts included'} · debounce ${secondsLabel(draft.debounce_seconds)}`}>
        <div class="trigger-fields">
          <label>Source<select value=${draft.source} onChange=${(event) => setDraft({ ...draft, source: event.currentTarget.value })}>
            ${TRIGGER_SOURCES.map((entry) => html`<option key=${entry.value} value=${entry.value}>${entry.label} · ${entry.value}</option>`)}
          </select></label>
          <label>Author<select value=${String(draft.author_is_agent)} onChange=${(event) => setDraft({ ...draft, author_is_agent: event.currentTarget.value === 'null' ? null : event.currentTarget.value === 'true' })}>
            <option value="true">agent only</option><option value="false">human only</option><option value="null">any author</option></select></label>
          <label>Draft PRs<select value=${draft.draft_filter} onChange=${(event) => setDraft({ ...draft, draft_filter: event.currentTarget.value })}>
            <option value="exclude">exclude</option><option value="include">include</option><option value="only">only drafts</option></select></label>
          <label>Debounce (seconds)<input type="number" min="0" value=${draft.debounce_seconds} onInput=${(event) => setDraft({ ...draft, debounce_seconds: Number(event.currentTarget.value) })} /></label>
          <label>Cooldown (seconds)<input type="number" min="0" value=${draft.cooldown_seconds} onInput=${(event) => setDraft({ ...draft, cooldown_seconds: Number(event.currentTarget.value) })} /></label>
        </div>
      </${Step}>
      <${Step} name="WHERE" expanded=${step === 'where'} onToggle=${() => setStep(step === 'where' ? '' : 'where')}
        valid=${whereValid} summary=${draft.scope === 'global' ? 'global' : `group ${draft.group || 'required'}`}>
        <div class="trigger-fields trigger-scope-fields">
          <label><input type="radio" name="trigger-scope" checked=${draft.scope === 'global'} onChange=${() => setDraft({ ...draft, scope: 'global', group: '' })} /> global</label>
          <label><input type="radio" name="trigger-scope" checked=${draft.scope === 'group'} onChange=${() => setDraft({ ...draft, scope: 'group' })} /> group</label>
          ${draft.scope === 'group' && html`<label>Group<select required value=${draft.group} onChange=${(event) => setDraft({ ...draft, group: event.currentTarget.value })}>
            <option value="">pick a group…</option>${groups.map((group) => html`<option key=${group} value=${group}>${group}</option>`)}</select></label>`}
        </div>
      </${Step}>
      <${Step} name="THEN" expanded=${step === 'then'} onToggle=${() => setStep(step === 'then' ? '' : 'then')}
        valid=${thenValid} summary=${draft.actions.length ? draft.actions.map(actionSummary).join(' → ') : 'action required'}>
        <div class="trigger-actions-editor">${draft.actions.map((action, index) => html`<${ActionEditor} key=${index} action=${action} index=${index}
          update=${(next) => updateAction(index, next)} remove=${() => removeAction(index)} />`)}
          <div class="trigger-add-actions"><button type="button" onClick=${() => setDraft({ ...draft, actions: [...draft.actions, emptyAction('spawn')] })}>+ spawn action</button>
            <button type="button" onClick=${() => setDraft({ ...draft, actions: [...draft.actions, emptyAction('message')] })}>+ message action</button></div>
          ${draft.actions.some((action) => action.type === 'spawn') && html`<div class="trigger-permission-warning" role="note">
            ⚠ This rule spawns agents. Firings are re-authorized as the owning principal; revoked or missing permissions are recorded as denied, not silently dropped.
          </div>`}
        </div>
      </${Step}>
      ${error && html`<div class="jobs-error" role="alert">${error}</div>`}
      <div class="modal-buttons"><button type="button" onClick=${state.closeTriggerDialog} disabled=${busy}>Cancel</button>
        <button type="submit" class="primary" disabled=${busy || !draftValid(draft)}>${busy ? 'Saving…' : editing ? 'Save trigger' : 'Create trigger'}</button></div>
    </form>
  </${Overlay}>`;
}

export { actionSummary, scopeSummary, triggerSource, triggerState, whenSummary };
