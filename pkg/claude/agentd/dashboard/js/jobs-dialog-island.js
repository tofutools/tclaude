import { Fragment, h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { agentCandidates, groupMembers, groupsForPicker } from './message-access-dialog-model.js';
import { idTooltip, shortAgentId } from './helpers.js';
import { wizWord } from './slop.js';
import {
  buildCronMutation, createCronDraft, cronDraftDirty,
  resetCronDraftForAnother, validateCronDraft,
} from './jobs-dialog-model.js';

const html = htm.bind(h);
const INTERVAL_PRESETS = ['5m', '15m', '1h', '4h', '24h'];

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function useLiveTheme() {
  const [, setRevision] = useState(0);
  useEffect(() => {
    const refresh = () => setRevision((value) => value + 1);
    document.addEventListener('tclaude:wizard', refresh);
    return () => document.removeEventListener('tclaude:wizard', refresh);
  }, []);
}

function CronTargetPicker({ value, onChange, snapshot, onPick }) {
  useLiveTheme();
  const scope = value.scopeGroup || '';
  const groups = groupsForPicker(snapshot, scope);
  const members = scope ? groupMembers(snapshot, scope) : [];
  const groupOptions = value.groupName && !groups.includes(value.groupName)
    ? [value.groupName, ...groups] : groups;
  const memberOptions = value.target && !members.some((member) => member.key === value.target)
    ? [{ key: value.target, title: `${value.target} (missing)`, online: false }, ...members]
    : members;
  const setMode = (mode) => onChange({ ...value, mode });
  return html`<div class="cron-create-target" id="cron-create-target-picker">
    <div class="cron-target-modes">
      <label><input type="radio" name="cron-create-target-mode" value="solo" checked=${value.mode === 'solo'}
        onChange=${() => setMode('solo')} /> <${Words} plain="Solo agent" wizard="Solo familiar"/></label>
      <label><input type="radio" name="cron-create-target-mode" value="group" checked=${value.mode === 'group'}
        onChange=${() => setMode('group')} /> <${Words} plain="Group (multicast)" wizard="Party (multicast)"/></label>
    </div>
    ${value.mode === 'solo' && !scope && html`<div class="cron-target-input-row" id="cron-create-target-solo">
      <input id="cron-create-target" type="text" value=${value.target}
        placeholder="agt_ id / title / conv-id / 8+-char prefix" autocomplete="off" spellcheck=${false}
        onInput=${(event) => onChange({ ...value, target: event.currentTarget.value })} />
      <button type="button" id="cron-create-target-pick" title="Pick from the agent / familiar list" onClick=${onPick}>🔍</button>
    </div>`}
    ${value.mode === 'solo' && scope && html`<div class="cron-target-input-row" id="cron-create-target-scoped">
      <select id="cron-create-scoped-member" value=${value.target}
        onChange=${(event) => onChange({ ...value, target: event.currentTarget.value })}>
        ${memberOptions.length
          ? html`<${Fragment}><option value="">${wizWord('(pick a member)', '(pick a familiar)')}</option>${memberOptions.map((member) => html`
              <option key=${member.key} value=${member.key}>${member.title || member.conv_id}${member.online ? '' : ' (offline)'}</option>`)}</${Fragment}>`
          : html`<option value="">${wizWord('(no members in this group)', '(no familiars in this party)')}</option>`}
      </select>
    </div>`}
    ${value.mode === 'group' && html`<div class="cron-target-input-row" id="cron-create-target-group">
      <select id="cron-create-group" value=${value.groupName} disabled=${!!scope}
        onChange=${(event) => onChange({ ...value, groupName: event.currentTarget.value })}>
        ${groupOptions.length
          ? html`<${Fragment}><option value="">${wizWord('(pick a group)', '(pick a party)')}</option>${groupOptions.map((name) => html`
              <option key=${name} value=${name}>${name}${groups.includes(name) ? '' : ' (missing)'}</option>`)}</${Fragment}>`
          : html`<option value="">${wizWord('(no groups — create one first)', '(no parties — form one first)')}</option>`}
      </select>
    </div>`}
  </div>`;
}

function AgentPicker({ descriptor, snapshot, onChoose, onClose, confirmDiscard }) {
  const [query, setQuery] = useState('');
  const [includeOffline, setIncludeOffline] = useState(false);
  const [highlight, setHighlight] = useState(0);
  const highlightedRef = useRef(null);
  const candidates = agentCandidates(snapshot, { includeOffline, query });
  const bounded = Math.max(0, Math.min(highlight, Math.max(0, candidates.length - 1)));
  const activeID = candidates[bounded] ? `cron-pick-target-option-${bounded}` : undefined;
  const activeKey = candidates[bounded]?.agent_id || candidates[bounded]?.conv_id || '';
  useEffect(() => { if (bounded !== highlight) setHighlight(bounded); }, [bounded, highlight]);
  useEffect(() => { highlightedRef.current?.scrollIntoView?.({ block: 'nearest' }); }, [bounded, activeKey]);
  const choose = (agent) => onChoose(agent.agent_id || agent.conv_id);
  const onKeyDown = (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === 'ArrowDown') {
      event.preventDefault(); setHighlight(Math.min(bounded + 1, candidates.length - 1));
    } else if (event.key === 'ArrowUp') {
      event.preventDefault(); setHighlight(Math.max(bounded - 1, 0));
    } else if (event.key === 'Enter' && candidates[bounded]) {
      event.preventDefault(); choose(candidates[bounded]);
    }
  };
  return html`<${Overlay} id="cron-pick-target-modal" dialogClass="add-member-modal"
    labelledby="cron-pick-target-title" onClose=${onClose} dirty=${false} blocked=${false}
    confirmDiscard=${confirmDiscard}>
    <h3 id="cron-pick-target-title">${descriptor.title} <span class="muted"><${Words} plain="— pick agent" wizard="— pick familiar"/></span></h3>
    <input id="cron-pick-target-search" class="add-member-search" type="text" value=${query}
      placeholder="Filter by title / role / descr / conv-id / group…" autocomplete="off" spellcheck=${false}
      role="combobox" aria-label="Filter agents" aria-controls="cron-pick-target-list" aria-expanded="true"
      aria-autocomplete="list" aria-activedescendant=${activeID}
      onInput=${(event) => { setQuery(event.currentTarget.value); setHighlight(0); }} onKeyDown=${onKeyDown} />
    <div class="add-member-list" id="cron-pick-target-list" role="listbox">
      ${candidates.length === 0
        ? html`<div class="add-member-empty">No matching conversations. ${includeOffline ? '(Try a different filter.)' : '(Try ticking “Include offline / archived” for a wider pool.)'}</div>`
        : candidates.map((agent, index) => html`<div key=${agent.agent_id || agent.conv_id}
          ref=${index === bounded ? highlightedRef : null} id=${`cron-pick-target-option-${index}`}
          role="option" aria-selected=${index === bounded ? 'true' : 'false'}
          class=${`add-member-row${index === bounded ? ' highlighted' : ''}`} data-i=${index}
          onMouseDown=${() => choose(agent)}>
          <span class=${agent.online ? 'online' : 'offline'} title=${agent.online ? 'online' : 'offline'}>${agent.online ? '●' : '○'}</span>
          <span class="rowname">${agent.title || '(unnamed)'}</span>
          <span class="id" title=${idTooltip(agent.agent_id, agent.conv_id)}>${shortAgentId(agent.agent_id, agent.conv_id)}</span>
          ${agent.memberships.length ? html`<span class="groups-tag">in: ${agent.memberships.map((item) => item.group).join(', ')}</span>` : null}
        </div>`)}
    </div>
    <div class="add-member-foot"><label title="Include offline / archived agents">
      <input id="cron-pick-target-all" type="checkbox" checked=${includeOffline}
        onChange=${(event) => { setIncludeOffline(event.currentTarget.checked); setHighlight(0); }} />Include offline / archived
      </label><span class="spacer"></span><span><kbd>↑↓</kbd> nav · <kbd>Enter</kbd> pick · <kbd>Esc</kbd> close</span></div>
  </${Overlay}>`;
}

function CronExplanation({ result, loading, onRetry }) {
  if (loading) return html`<div class="muted">Explaining…</div>`;
  if (!result) return null;
  if (result.error) return html`<span class="cron-explain-error">explain failed: ${result.error}
    <button type="button" onClick=${onRetry}>retry</button></span>`;
  if (!result.valid) return html`<span class="cron-explain-error">${result.message || 'invalid expression'}</span>`;
  const fires = (result.next || []).map((value) => new Date(value).toLocaleString()).join(' · ');
  return html`<${Fragment}>
    ${result.description && html`<div class="cron-explain-desc">${result.description}</div>`}
    ${fires && html`<div>next: ${fires}</div>`}
    <div>evaluated in the daemon's timezone (${result.tz || 'local'}) unless the expression carries CRON_TZ=</div>
  </${Fragment}>`;
}

export function CronDialog({ descriptor, snapshot, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  useLiveTheme();
  const [initial, setInitial] = useState(() => createCronDraft(descriptor.prefill));
  const [draft, setDraft] = useState(() => createCronDraft(descriptor.prefill));
  const [picker, setPicker] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const busyRef = useRef(false);
  const nameRef = useRef(null);
  const explainSeq = useRef(0);
  const firstExplain = useRef(!!descriptor.prefill.cronExpr);
  const [explainRevision, setExplainRevision] = useState(0);
  const [explain, setExplain] = useState({ loading: false, result: null });
  const dirty = cronDraftDirty(draft, initial);
  const editing = descriptor.kind === 'edit';
  const scope = draft.target.scopeGroup;
  const update = (patch) => setDraft((value) => ({ ...value, ...patch }));

  useEffect(() => {
    if (draft.scheduleMode !== 'cron') {
      explainSeq.current += 1;
      setExplain({ loading: false, result: null });
      return undefined;
    }
    const expr = draft.cronExpr.trim();
    if (!expr) {
      explainSeq.current += 1;
      setExplain({ loading: false, result: null });
      return undefined;
    }
    const seq = ++explainSeq.current;
    let active = true;
    const delay = firstExplain.current ? 0 : 350;
    firstExplain.current = false;
    const timer = setTimeout(async () => {
      setExplain({ loading: true, result: null });
      try {
        const result = await actions.explainCron(expr);
        if (!active || seq !== explainSeq.current) return;
        setExplain({ loading: false, result: result.valid
          ? { valid: true, description: result.description || '', next: result.next || [], tz: result.tz || 'local' }
          : { valid: false, message: result.error || 'invalid expression' } });
      } catch (error) {
        if (!active || seq !== explainSeq.current) return;
        setExplain({ loading: false, result: { error: error?.message || String(error) } });
      }
    }, delay);
    return () => { active = false; clearTimeout(timer); };
  }, [draft.scheduleMode, draft.cronExpr, explainRevision]);

  const choose = (value) => {
    if (picker?.field === 'owner') update({ owner: value });
    else if (picker?.field === 'target') update({ target: { ...draft.target, target: value } });
    setPicker(null);
  };
  const submit = async (keepOpen = false) => {
    if (busyRef.current) return;
    const problem = validateCronDraft(descriptor, draft);
    if (problem) {
      const wizardMessages = {
        'group-target': 'Pick a party from the dropdown (or form one first via the Groups tab).',
        'scoped-target': 'This party has no familiars to nudge — switch to Party (multicast), or invite a familiar to the party first.',
      };
      setError(wizWord(problem.message, wizardMessages[problem.code] || problem.message));
      return;
    }
    setError('');
    busyRef.current = true;
    setBusy(true);
    try {
      await actions.saveCron(buildCronMutation(descriptor, draft));
      if (keepOpen) {
        const next = resetCronDraftForAnother(draft);
        setInitial(next);
        setDraft(next);
        nameRef.current?.focus();
      } else {
        actions.closeCronDialog();
      }
    } catch (error) {
      setError(error?.message || String(error));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };
  const plainTitle = editing
    ? 'Edit cron job'
    : descriptor.kind === 'duplicate'
      ? 'Duplicate cron job'
      : scope ? `Schedule a cron job for group "${scope}"` : 'Schedule a cron job';
  const wizardTitle = editing
    ? 'Re-bind the recurring ritual'
    : descriptor.kind === 'duplicate'
      ? 'Duplicate the recurring ritual'
      : scope ? `Bind a recurring ritual for party "${scope}"` : 'Bind a recurring ritual';
  const title = wizWord(plainTitle, wizardTitle);
  const meta = editing
    ? `#${descriptor.id} · ${descriptor.job?.name || '(unnamed)'}`
    : descriptor.kind === 'duplicate'
      ? `copy of #${descriptor.sourceID} · ${descriptor.job?.name || '(unnamed)'}` : '';

  return html`<${Fragment}>
    <${Overlay} id="cron-create-modal"
      overlayClass=${editing ? 'cron-editing' : descriptor.kind === 'duplicate' ? 'cron-duplicating' : ''}
      labelledby="cron-create-title" onClose=${actions.closeCronDialog}
      onSubmitHotkey=${busy ? null : () => submit(false)} dirty=${dirty} blocked=${busy}
      confirmDiscard=${confirmDiscard} registerClose=${registerClose}
      resizeKey="tclaude.dash.modalSize.cron-create">
      <h3 id="cron-create-title">${title}</h3>
      ${meta && html`<div class="modal-meta" id="cron-create-meta">${meta}</div>`}
      <label class="cron-create-row"><span class="cron-create-label">Name</span>
        <input ref=${nameRef} id="cron-create-name" type="text" value=${draft.name}
          placeholder="kebab-or-snake-case label" autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ name: event.currentTarget.value })} /></label>
      <label class="cron-create-row"><span class="cron-create-label">Owner</span><div class="cron-create-target"><div class="cron-target-input-row">
        <input id="cron-create-owner" type="text" value=${draft.owner}
          placeholder="(default: dashboard human) title / conv-id / 8+-char prefix" autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ owner: event.currentTarget.value })} />
        <button type="button" id="cron-create-owner-pick" title="Pick from the agent / familiar list"
          onClick=${() => setPicker({ field: 'owner', title: 'Pick owner' })}>🔍</button>
      </div></div></label>
      <label class="cron-create-row"><span class="cron-create-label">Target</span>
        <${CronTargetPicker} value=${draft.target} snapshot=${snapshot}
          onChange=${(target) => update({ target })}
          onPick=${() => setPicker({ field: 'target', title: 'Pick target' })}/></label>
      ${draft.target.mode === 'group' && html`<label class="cron-create-row" id="cron-create-role-row"><span class="cron-create-label"
        title="For a group / party target only: deliver only to members / familiars whose role / class matches. Blank or 'all' means the whole target.">
        <${Words} plain="Role filter" wizard="Class filter"/></span>
        <input id="cron-create-role" type="text" value=${draft.role}
          placeholder="optional — blank / all = entire target (e.g. dev)" autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ role: event.currentTarget.value })} /></label>`}
      <label class="cron-create-row"><span class="cron-create-label">Schedule</span><div class="cron-create-schedule">
        <div class="cron-target-modes">
          <label><input type="radio" name="cron-create-schedule-mode" value="interval" checked=${draft.scheduleMode === 'interval'}
            onChange=${() => update({ scheduleMode: 'interval' })} /> Interval</label>
          <label><input type="radio" name="cron-create-schedule-mode" value="cron" checked=${draft.scheduleMode === 'cron'}
            onChange=${() => update({ scheduleMode: 'cron' })} /> Cron expression</label>
        </div>
        ${draft.scheduleMode === 'interval' ? html`<div class="cron-schedule-interval" id="cron-create-schedule-interval">
          <div class="cron-schedule-chips" id="cron-create-chips">${INTERVAL_PRESETS.map((value) => html`
            <button type="button" data-chip=${value} class=${draft.interval.trim() === value ? 'selected' : ''}
              onClick=${() => update({ interval: value })}>${value === '24h' ? 'daily' : value}</button>`)}</div>
          <input id="cron-create-interval" type="text" value=${draft.interval}
            placeholder="custom (30s min, e.g. 10m, 2h, 1h30m)" autocomplete="off" spellcheck=${false}
            onInput=${(event) => update({ interval: event.currentTarget.value })} />
        </div>` : html`<div class="cron-schedule-cron" id="cron-create-schedule-cron">
          <input id="cron-create-cron" type="text" value=${draft.cronExpr}
            placeholder="min hour dom month dow — e.g. */5 * * * *, 0 9 * * 1-5, @daily" autocomplete="off" spellcheck=${false}
            onInput=${(event) => update({ cronExpr: event.currentTarget.value })} />
          <div class="cron-explain" id="cron-create-cron-explain"><${CronExplanation}
            loading=${explain.loading} result=${explain.result}
            onRetry=${() => setExplainRevision((value) => value + 1)}/></div>
        </div>`}
      </div></label>
      <label class="cron-create-row"><span class="cron-create-label">Subject</span>
        <input id="cron-create-subject" type="text" maxlength="100" value=${draft.subject}
          placeholder="optional, shows in inbox listings" autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ subject: event.currentTarget.value })} /></label>
      <label class="cron-create-row"><span class="cron-create-label">Body</span>
        <textarea id="cron-create-body" rows="4" value=${draft.body}
          placeholder="message text the cron job sends each tick (required)" spellcheck=${false}
          onInput=${(event) => update({ body: event.currentTarget.value })}></textarea></label>
      <label class="cron-create-enabled"><input id="cron-create-enabled" type="checkbox" checked=${draft.enabled}
        onChange=${(event) => update({ enabled: event.currentTarget.checked })} /> Enabled</label>
      <div class="cron-create-error" id="cron-create-error" role="alert">${error}</div>
      <div class="modal-buttons"><button id="cron-create-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
        <span class="spacer"></span>${!editing && html`<button id="cron-create-save-another" class="secondary" type="button" disabled=${busy}
          title="Save and reset the form so you can create another in the same sitting" onClick=${() => submit(true)}>Save & create another</button>`}
        <button id="cron-create-submit" class="primary" type="button" disabled=${busy} onClick=${() => submit(false)}>
          ${busy ? (editing ? 'Saving…' : 'Creating…') : (editing ? 'Save' : 'Create')}</button></div>
    </${Overlay}>
    ${picker && html`<${AgentPicker} descriptor=${picker} snapshot=${snapshot} onChoose=${choose}
      onClose=${() => setPicker(null)} confirmDiscard=${confirmDiscard}/>`}
  </${Fragment}>`;
}

export function JobsCronDialogRoot({ state, actions, confirmDiscard }) {
  const current = state.view.value;
  const descriptor = current.dialog;
  if (!descriptor) return null;
  return html`<${CronDialog} key=${descriptor.launchID} descriptor=${descriptor}
    snapshot=${current.dashboard || {}} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
}
