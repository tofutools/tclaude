import { Fragment, h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Select } from './ui-components.js';
import { useGuardedOverlayClose } from './management-overlay.js';
import { Dialog } from './ui-dialog.js';
import { AgentPickerDialog, TargetPicker } from './agent-picker.js';
import { wizWord } from './slop.js';
import { nodeSnapshot } from './node-api.js';
import {
  buildCronMutation, createCronDraft, cronDraftDirty,
  resetCronDraftForAnother, validateCronDraft,
} from './jobs-dialog-model.js';
import { SpawnActionFields } from './spawn-action-fields.js';
import { triggersAvailable } from './feature-availability.js';

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

function CronExplanation({ result, loading, onRetry }) {
  if (loading) return html`<div class="muted">Explaining…</div>`;
  if (!result) return null;
  if (result.error) return html`<span class="cron-explain-error ui-error">explain failed: ${result.error}
    <${Button} size="sm" onClick=${onRetry}>retry<//></span>`;
  if (!result.valid) return html`<span class="cron-explain-error ui-error">${result.message || 'invalid expression'}</span>`;
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
  const triggersEnabled = triggersAvailable(snapshot);
  const profileOptions = [...new Set((snapshot?.profiles || []).flatMap((profile) => [
    profile.name, ...(profile.aliases || []),
  ]).filter(Boolean))];
  const update = (patch) => setDraft((value) => ({ ...value, ...patch }));
  const setActionKind = (actionKind) => setDraft((value) => ({
    ...value,
    actionKind,
    target: actionKind === 'spawn' ? { ...value.target, mode: 'group', target: '' } : value.target,
  }));

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
        const result = await actions.explainCron(expr, descriptor.node || '');
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
        'scoped-target': 'This party has no heroes to nudge — switch to Party (multicast), or invite a hero to the party first.',
      };
      setError(wizWord(problem.message, wizardMessages[problem.code] || problem.message));
      return;
    }
    setError('');
    busyRef.current = true;
    setBusy(true);
    try {
      await actions.saveCron({ ...buildCronMutation(descriptor, draft), node: descriptor.node || '' });
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
    <${Dialog} id="cron-create-modal"
      overlayClass=${editing ? 'cron-editing' : descriptor.kind === 'duplicate' ? 'cron-duplicating' : ''} onClose=${actions.closeCronDialog}
      onSubmitHotkey=${busy ? null : () => submit(false)} dirty=${dirty} blocked=${busy}
      confirmDiscard=${confirmDiscard} guard=${{ requestClose, registerClose }}
      resizeKey="tclaude.dash.modalSize.cron-create" title=${title}>
      ${meta && html`<div class="modal-meta" id="cron-create-meta">${meta}</div>`}
      <label class="cron-create-row"><span class="cron-create-label">Name</span>
        <input ref=${nameRef} id="cron-create-name" type="text" value=${draft.name}
          placeholder="kebab-or-snake-case label" autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ name: event.currentTarget.value })} /></label>
      <label class="cron-create-row"><span class="cron-create-label"
        title="Blank = fires as you, the human operator (recipients see 'the human operator'); or attribute it to a sender agent">Owner</span><div class="cron-create-target"><div class="cron-target-input-row">
        <input id="cron-create-owner" type="text" value=${draft.owner}
          placeholder="blank = you (the operator) · or a sender: title / conv-id / 8+-char prefix" autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ owner: event.currentTarget.value })} />
        <${Button} id="cron-create-owner-pick" title="Pick from the agent / hero list"
          onClick=${() => setPicker({ field: 'owner', title: 'Pick owner' })}>🔍<//>
      </div></div></label>
      <label class="cron-create-row"><span class="cron-create-label">Target</span>
        <${TargetPicker} prefix="cron-create" value=${draft.target} snapshot=${snapshot}
          groupOnly=${draft.actionKind === 'spawn'}
          onChange=${(target) => update({ target })}
          onPick=${() => setPicker({ field: 'target', title: 'Pick target' })}/></label>
      <label class="cron-create-row"><span class="cron-create-label">Action</span>
        <${Select} id="cron-create-action-kind" value=${draft.actionKind}
          onChange=${(event) => setActionKind(event.currentTarget.value)}>
          <option value="message">message</option>
          ${triggersEnabled && html`<option value="spawn">spawn agent</option>`}
        <//>
      </label>
      ${draft.target.mode === 'group' && draft.actionKind === 'message' && html`<label class="cron-create-row" id="cron-create-role-row"><span class="cron-create-label"
        title="For a group / party target only: deliver only to members / heroes whose role / class matches. Blank or 'all' means the whole target.">
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
            <button data-ui-exempt type="button" data-chip=${value} class=${draft.interval.trim() === value ? 'selected' : ''}
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
      ${draft.actionKind === 'message' ? html`<${Fragment}>
      <label class="cron-create-row"><span class="cron-create-label">Subject</span>
        <input id="cron-create-subject" type="text" maxlength="100" value=${draft.subject}
          placeholder="optional, shows in inbox listings" autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ subject: event.currentTarget.value })} /></label>
      <label class="cron-create-row"><span class="cron-create-label">Body</span>
        <textarea id="cron-create-body" rows="4" value=${draft.body}
          placeholder="message text the cron job sends each tick (required)" spellcheck=${false}
          onInput=${(event) => update({ body: event.currentTarget.value })}></textarea></label>
      </${Fragment}>` : html`<div class="cron-create-row cron-spawn-action" id="cron-create-spawn-fields">
        <span class="cron-create-label">Spawn</span><div class="cron-spawn-action-body">
        <${SpawnActionFields} fieldPrefix="cron-create-spawn" profileOptions=${profileOptions}
          placeholderTokens=${['{{fire_time}}']}
          instructionPlaceholder="Describe the recurring work for this worker."
          value=${{
            profile: draft.spawn.profile, roles: draft.spawn.roles,
            nameTemplate: draft.spawn.nameTemplate,
            instructionTemplate: draft.spawn.instructionTemplate,
            workerDeadlineSeconds: draft.spawn.workerDeadlineSeconds,
          }} onChange=${(spawn) => update({ spawn: {
            ...draft.spawn, profile: spawn.profile, roles: spawn.roles,
            nameTemplate: spawn.nameTemplate, instructionTemplate: spawn.instructionTemplate,
            workerDeadlineSeconds: spawn.workerDeadlineSeconds,
          } })} />
        <div class="trigger-action-fields cron-spawn-concurrency">
          <label>Concurrency policy<${Select} id="cron-create-spawn-concurrency" value=${draft.spawn.concurrencyPolicy}
            onChange=${(event) => update({ spawn: { ...draft.spawn, concurrencyPolicy: event.currentTarget.value } })}>
            <option value="Forbid">Forbid — skip while a worker is live</option>
            <option value="Replace">Replace — stop the live worker first</option>
            <option value="Allow">Allow — run workers concurrently</option>
          <//></label>
          ${draft.spawn.concurrencyPolicy === 'Allow' && html`<label>Max live workers<input
            id="cron-create-spawn-max-live" type="number" min="1" value=${draft.spawn.maxLiveWorkers}
            onInput=${(event) => update({ spawn: { ...draft.spawn, maxLiveWorkers: Number(event.currentTarget.value) } })} /></label>`}
        </div>
        <div class="trigger-permission-warning" role="note">⚠ Each firing is re-authorized as the owning principal. Missing or revoked spawn permissions are recorded in run history.</div>
      </div></div>`}
      <label class="cron-create-enabled"><input id="cron-create-enabled" type="checkbox" checked=${draft.enabled}
        onChange=${(event) => update({ enabled: event.currentTarget.checked })} /> Enabled</label>
      <label class="cron-create-enabled" title="On create, or when changed from off to on during an edit, fire exactly once now. The normal cadence continues from that fire.">
        <input id="cron-create-run-immediately" type="checkbox" checked=${draft.runImmediately}
          onChange=${(event) => update({ runImmediately: event.currentTarget.checked })} /> Run immediately once
      </label>
      ${draft.actionKind === 'message' && html`<label class="cron-create-enabled" title="By default, a tick is discarded when its target has no live agent pane. Enable this only when stale reminders should be retained until the target returns.">
        <input id="cron-create-queue-when-offline" type="checkbox" checked=${draft.queueWhenOffline}
          onChange=${(event) => update({ queueWhenOffline: event.currentTarget.checked })} /> Queue while offline
      </label>`}
      <div class="cron-create-error ui-error-box" id="cron-create-error" role="alert">${error}</div>
      <div class="modal-buttons"><${Button} id="cron-create-cancel" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</${Button}>
        <span class="spacer"></span>${!editing && html`<${Button} variant="ghost" id="cron-create-save-another" disabled=${busy}
          title="Save and reset the form so you can create another in the same sitting" onClick=${() => submit(true)}>Save & create another</${Button}>`}
        <${Button} variant="primary" id="cron-create-submit" disabled=${busy} onClick=${() => submit(false)}>
          ${busy ? (editing ? 'Saving…' : 'Creating…') : (editing ? 'Save' : 'Create')}</${Button}></div>
    </${Dialog}>
    ${picker && html`<${AgentPickerDialog} descriptor=${picker} snapshot=${snapshot} onChoose=${choose}
      onClose=${() => setPicker(null)} confirmDiscard=${confirmDiscard}/>`}
  </${Fragment}>`;
}

export function JobsCronDialogRoot({ state, actions, confirmDiscard }) {
  const current = state.view.value;
  const descriptor = current.dialog;
  if (!descriptor) return null;
  // Another node's job reads that node's snapshot, never the page's (a null
  // means the node is the page's own).
  const snapshot = (descriptor.node && nodeSnapshot(descriptor.node)) || current.dashboard || {};
  return html`<${CronDialog} key=${descriptor.launchID} descriptor=${descriptor}
    snapshot=${snapshot} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
}
