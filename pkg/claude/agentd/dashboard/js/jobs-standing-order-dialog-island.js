import { Fragment, h } from 'preact';
import { useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { agentCandidates, groupMembers, groupsForPicker } from './message-access-dialog-model.js';
import { idTooltip, shortAgentId } from './helpers.js';
import {
  buildStandingOrderMutation, createStandingOrderDraft,
  standingOrderDraftDirty, validateStandingOrderDraft,
} from './jobs-dialog-model.js';

const html = htm.bind(h);
const SESSION_SOURCES = [
  ['startup', 'Startup'],
  ['resume', 'Resume'],
  ['clear', 'Clear / new generation'],
  ['compact', 'Context compaction'],
];
const TRIGGER_EVENTS = [
  ['session.start', 'Session boundary'],
  ['user.prompt', 'User prompt submitted'],
  ['tool.before', 'Before tool use'],
  ['tool.after', 'After tool use'],
];
const MATCH_FIELDS = {
  'session.start': [['', 'Any matching boundary'], ['cwd', 'Working directory']],
  'user.prompt': [['', 'Any submitted prompt'], ['prompt', 'Prompt text'], ['cwd', 'Working directory']],
  'tool.before': [['', 'Any tool call'], ['tool_name', 'Tool name'], ['tool_input', 'Tool input (compact JSON)'], ['cwd', 'Working directory']],
  'tool.after': [['', 'Any completed tool call'], ['tool_name', 'Tool name'], ['tool_input', 'Tool input (compact JSON)'], ['cwd', 'Working directory']],
};

function StandingOrderTargetPicker({ value, onChange, snapshot }) {
  const scope = value.scopeGroup || '';
  const groups = groupsForPicker(snapshot, scope);
  const scopedMembers = scope ? groupMembers(snapshot, scope) : [];
  const candidates = useMemo(
    () => scope
      ? scopedMembers
      : agentCandidates(snapshot, { includeOffline: true, query: '' }),
    [snapshot, scope],
  );
  const groupOptions = value.groupName && !groups.includes(value.groupName)
    ? [value.groupName, ...groups] : groups;
  const targetOptions = value.target && !candidates.some(
    (agent) => (agent.agent_id || agent.key || agent.conv_id) === value.target,
  )
    ? [{ agent_id: value.target, title: `${value.target} (missing)`, online: false }, ...candidates]
    : candidates;
  const setMode = (mode) => onChange({ ...value, mode });
  return html`<div class="cron-create-target standing-order-target-picker">
    <div class="cron-target-modes">
      <label><input type="radio" name="standing-order-target-mode" value="solo"
        checked=${value.mode === 'solo'} onChange=${() => setMode('solo')} /> Solo agent</label>
      <label><input type="radio" name="standing-order-target-mode" value="group"
        checked=${value.mode === 'group'} onChange=${() => setMode('group')} /> Group (multicast)</label>
    </div>
    ${value.mode === 'solo' && html`<select id="standing-order-target" value=${value.target}
      onChange=${(event) => onChange({ ...value, target: event.currentTarget.value })}>
      <option value="">(pick an agent)</option>
      ${targetOptions.map((agent) => {
        const key = agent.agent_id || agent.key || agent.conv_id;
        return html`<option key=${key} value=${key}
          title=${idTooltip(agent.agent_id || agent.key, agent.conv_id)}>
          ${agent.title || shortAgentId(agent.agent_id || agent.key, agent.conv_id)}
          ${agent.online ? '' : ' (offline)'}
        </option>`;
      })}
    </select>`}
    ${value.mode === 'group' && html`<select id="standing-order-group" value=${value.groupName}
      disabled=${!!scope}
      onChange=${(event) => onChange({ ...value, groupName: event.currentTarget.value })}>
      <option value="">(pick a group)</option>
      ${groupOptions.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
    </select>`}
  </div>`;
}

function RadioChoice({ name, value, checked, title, detail, onChange }) {
  return html`<label class="standing-order-choice">
    <input type="radio" name=${name} value=${value} checked=${checked} onChange=${onChange} />
    <span><strong>${title}</strong><span class="muted">${detail}</span></span>
  </label>`;
}

export function StandingOrderDialog({ descriptor, snapshot, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [initial] = useState(() => createStandingOrderDraft(descriptor.prefill));
  const [draft, setDraft] = useState(() => createStandingOrderDraft(descriptor.prefill));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const busyRef = useRef(false);
  const nameRef = useRef(null);
  const editing = descriptor.kind === 'edit';
  const dirty = standingOrderDraftDirty(draft, initial);
  const update = (patch) => setDraft((value) => ({ ...value, ...patch }));
  const updateSource = (source, checked) => {
    const next = checked
      ? [...new Set([...draft.sources, source])]
      : draft.sources.filter((value) => value !== source);
    update({ sources: next });
  };
  const submit = async () => {
    if (busyRef.current) return;
    const problem = validateStandingOrderDraft(descriptor, draft);
    if (problem) {
      setError(problem.message);
      return;
    }
    setError('');
    busyRef.current = true;
    setBusy(true);
    try {
      await actions.saveStandingOrder(buildStandingOrderMutation(descriptor, draft));
      actions.closeStandingOrderDialog();
    } catch (saveError) {
      setError(saveError?.message || String(saveError));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  return html`<${Overlay} id="standing-order-modal"
    overlayClass=${editing ? 'standing-order-editing' : ''}
    labelledby="standing-order-title" onClose=${actions.closeStandingOrderDialog}
    onSubmitHotkey=${busy ? null : submit} dirty=${dirty} blocked=${busy}
    confirmDiscard=${confirmDiscard} registerClose=${registerClose}
    resizeKey="tclaude.dash.modalSize.standing-order">
    <h3 id="standing-order-title">${editing ? 'Edit standing order' : 'Create standing order'}</h3>
    ${editing && html`<div class="modal-meta">#${descriptor.id} · revision ${draft.revision}</div>`}
    ${descriptor.order?.disabled_reason && html`<div class="jobs-error standing-order-retirement-warning">
      This order was disabled automatically: ${descriptor.order.disabled_reason}.
      Leave Enabled off to preserve that retirement state; explicitly enabling it clears the marker.
    </div>`}
    <label class="cron-create-row"><span class="cron-create-label">Name</span>
      <input ref=${nameRef} id="standing-order-name" type="text" value=${draft.name}
        placeholder="short stable label" autocomplete="off" spellcheck=${false}
        onInput=${(event) => update({ name: event.currentTarget.value })} /></label>
    <label class="cron-create-row"><span class="cron-create-label">Target</span>
      <${StandingOrderTargetPicker} value=${draft.target} snapshot=${snapshot}
        onChange=${(target) => update({ target })} /></label>
    ${draft.target.mode === 'group' && html`<label class="cron-create-row">
      <span class="cron-create-label">Role filter</span>
      <input id="standing-order-role" type="text" value=${draft.role}
        placeholder="optional — blank / all = entire group"
        onInput=${(event) => update({ role: event.currentTarget.value })} /></label>`}
    <div class="cron-create-row"><span class="cron-create-label">Trigger</span>
      <div class="standing-order-field">
        <select id="standing-order-trigger" value=${draft.triggerEvent}
          onChange=${(event) => update({
            triggerEvent: event.currentTarget.value,
            matchField: '',
            matchRegex: '',
          })}>
          ${TRIGGER_EVENTS.map(([value, label]) => html`
            <option key=${value} value=${value}>${label}</option>`)}
        </select>
        ${draft.triggerEvent === 'session.start' && html`<div class="cron-target-modes">
          <label><input type="radio" name="standing-order-source-mode" value="any"
            checked=${draft.sourceMode === 'any'} onChange=${() => update({ sourceMode: 'any' })} />
            Any source</label>
          <label><input type="radio" name="standing-order-source-mode" value="selected"
            checked=${draft.sourceMode === 'selected'} onChange=${() => update({ sourceMode: 'selected' })} />
            Selected sources</label>
        </div>`}
        ${draft.triggerEvent === 'session.start' && draft.sourceMode === 'selected' && html`
        <div class="standing-order-options" id="standing-order-sources">
          ${SESSION_SOURCES.map(([value, label]) => html`<label>
            <input type="checkbox" value=${value} checked=${draft.sources.includes(value)}
              onChange=${(event) => updateSource(value, event.currentTarget.checked)} /> ${label}
          </label>`)}
        </div>`}
        ${draft.triggerEvent !== 'session.start' && html`<div class="muted">
          Action triggers currently deliver inline on Claude and Codex. OpenCode is shown as unsupported
          until queued deliveries can suppress their own trigger origin.
        </div>`}
      </div>
    </div>
    <div class="cron-create-row"><span class="cron-create-label">Condition</span>
      <div class="standing-order-field">
        <select id="standing-order-match-field" value=${draft.matchField}
          onChange=${(event) => update({
            matchField: event.currentTarget.value,
            matchRegex: event.currentTarget.value ? draft.matchRegex : '',
          })}>
          ${(MATCH_FIELDS[draft.triggerEvent] || []).map(([value, label]) => html`
            <option key=${value || 'any'} value=${value}>${label}</option>`)}
        </select>
        ${draft.matchField && html`<input id="standing-order-match-regex" type="text"
          maxlength="1024" value=${draft.matchRegex}
          placeholder="RE2 expression, e.g. (?i)^bash$ or deploy"
          autocomplete="off" spellcheck=${false}
          onInput=${(event) => update({ matchRegex: event.currentTarget.value })} />`}
        <div class="muted">Optional RE2 match. Expressions are case-sensitive unless they include
          flags such as <code>(?i)</code>. Tool input is normalized to compact JSON.</div>
      </div>
    </div>
    <div class="cron-create-row"><span class="cron-create-label">Delivery guarantee</span>
      <div class="standing-order-field">
        <${RadioChoice} name="standing-order-timing" value="same-continuation"
          checked=${draft.timing === 'same-continuation'} title="Same continuation"
          detail="Inject as hook context before the model continues. If a harness cannot guarantee this, the order visibly does not deliver."
          onChange=${() => update({ timing: 'same-continuation' })} />
        <${RadioChoice} name="standing-order-timing" value="next-turn"
          checked=${draft.timing === 'next-turn'} title="Next turn"
          detail="Use the strongest valid message transport; OpenCode queues this for the next turn."
          onChange=${() => update({ timing: 'next-turn' })} />
      </div>
    </div>
    <div class="cron-create-row"><span class="cron-create-label">Cadence</span>
      <div class="standing-order-field">
        <${RadioChoice} name="standing-order-cadence" value="always"
          checked=${draft.cadence === 'always'} title="Every matching boundary"
          detail="Best for reminders that must survive each compaction."
          onChange=${() => update({ cadence: 'always' })} />
        <${RadioChoice} name="standing-order-cadence" value="once-per-generation"
          checked=${draft.cadence === 'once-per-generation'} title="Once per conversation generation"
          detail="Deliver once, then suppress repeats until the agent starts a new generation."
          onChange=${() => update({ cadence: 'once-per-generation' })} />
      </div>
    </div>
    <label class="cron-create-row"><span class="cron-create-label">Minimum interval</span>
      <div class="standing-order-field">
        <input id="standing-order-cooldown" type="number" min="0" max="31536000" step="1"
          value=${draft.cooldownSeconds}
          onInput=${(event) => update({ cooldownSeconds: Number(event.currentTarget.value) })} />
        <div class="muted">Seconds between successful deliveries to each stable recipient agent. 0 disables cooldown.</div>
      </div>
    </label>
    <label class="cron-create-row"><span class="cron-create-label">Instruction</span>
      <textarea id="standing-order-summary" rows="6" maxlength="2000" value=${draft.summary}
        placeholder="The short instruction delivered to the agent when this order matches"
        onInput=${(event) => update({ summary: event.currentTarget.value })}></textarea></label>
    <label class="cron-create-enabled"><input id="standing-order-enabled" type="checkbox"
      checked=${draft.enabled} onChange=${(event) => update({ enabled: event.currentTarget.checked })} />
      Enabled</label>
    <div class="cron-create-error" id="standing-order-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button id="standing-order-cancel" type="button" disabled=${busy}
        onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="standing-order-submit" class="primary" type="button" disabled=${busy}
        onClick=${submit}>${busy ? 'Saving…' : editing ? 'Save' : 'Create'}</button>
    </div>
  </${Overlay}>`;
}

export function JobsStandingOrderDialogRoot({ state, actions, confirmDiscard }) {
  const current = state.view.value;
  const descriptor = current.orderDialog;
  if (!descriptor) return null;
  return html`<${StandingOrderDialog} key=${descriptor.launchID} descriptor=${descriptor}
    snapshot=${current.dashboard || {}} actions=${actions} confirmDiscard=${confirmDiscard} />`;
}
