// groups-taskboard-new.js -- the Task board's one add entry point, "+ New
// task" (wizard: "+ New quest"), and the dialogs it opens (docs/taskboard.md,
// tcl-kp5ngc). Modelled on the Resources section's ＋ menu
// (groups-resources.js AddResource):
//
//   Write it myself     the New task form: title, brief, ref, stage, deps
//                       and (with can.assign) an assignee. Each field is a
//                       `taskboard add` flag; the assignee is `assign --to`.
//                       A pasted list is one task per line and a title that
//                       is only a tracker id is that ref, titled from the
//                       tracker, as the old capture box did.
//   Ask an agent        a prose request mailed to a member who may change the
//                       board (`taskboard ask <group> --to <agent> "…"`).
//   From awb/Linear/…   the import panel (`add --from-awb` and kin).

import { h } from 'preact';
import { useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Button, Check, Input, Menu, MenuItem, MenuSep, Select, Textarea } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { captureLines, isTrackerLine } from './taskboard-actions.js';

const html = htm.bind(h);

// Members as groups-taskboard.js reads them: active agents with a stable id,
// online ones first.
function activeMembers(group) {
  return (group?.members || []).filter((member) => member.agent_id && !member.retired)
    .sort((a, b) => Number(!!b.online) - Number(!!a.online) || String(a.title || '').localeCompare(String(b.title || '')));
}

// askableMembers are the members "Ask an agent" may mail: online, and able
// to change the board. Without the writer lock every member may; with it,
// the context's writers list says who.
export function askableMembers(group, entry) {
  const writers = entry?.locked ? new Set(entry.writers || []) : null;
  return activeMembers(group).filter((member) => member.online && (!writers || writers.has(member.agent_id)));
}

// defaultAskee is the group's lead: an online owner first, then a member
// whose role names it the lead, else the first one.
export function defaultAskee(members) {
  return members.find((member) => member.owner)
    || members.find((member) => /\blead\b/i.test(member.role || ''))
    || members[0] || null;
}

// askUnavailable is why "Ask an agent" is off, or '' when it is on.
export function askUnavailable(group, entry, wizard = false) {
  if (askableMembers(group, entry).length) return '';
  if (!activeMembers(group).some((member) => member.online)) return wizard ? 'No hero of this party is awake' : 'No member of this group is online';
  return wizard ? 'No awake hero may write on this board' : 'No online member may change this board (writer lock)';
}

// newTaskPlan turns the form into its writes, in order. `lines` is a pasted
// list: one task per line, as an import. A single title that is only a
// tracker id (and no ref) is that ref, titled from the tracker: an import
// too, whose brief and dependencies follow as an edit. Anything else is one
// `add`. The assignee, when picked, is an `assign` after it.
export function newTaskPlan(draft) {
  const next = draft.stage === 'next';
  if (draft.list) {
    return { kind: 'lines', text: captureLines(draft.list).join('\n'), next };
  }
  const title = String(draft.title || '').trim();
  const body = String(draft.body || '').trim();
  const ref = String(draft.ref || '').trim();
  const after = draft.after || [];
  const to = draft.to || '';
  if (!ref && isTrackerLine(title)) {
    return { kind: 'tracker', text: title, next, body, after, to };
  }
  return { kind: 'add', task: { title, body, ref, stage: next ? 'next' : 'idea', after, resolveRef: !!ref }, to };
}

// submitNewTask runs the plan. It resolves to { created, task, warning }:
// a refusal before anything landed throws; one after (the assign, say)
// is the warning, since the task is on the board by then.
export async function submitNewTask(api, groupName, draft) {
  const plan = newTaskPlan(draft);
  if (plan.kind === 'lines') {
    const out = await api.capture(groupName, plan.text, { next: plan.next });
    const refused = (out?.results || []).filter((result) => result.error);
    const warning = [
      refused.length ? `${refused.length} refused (${refused.map((result) => `${result.input}: ${result.code || result.error}`).join('; ')})` : '',
      out?.failed ? `stopped: ${out.failed.message}; ${out.failed.remaining.length} not sent` : '',
    ].filter(Boolean).join(' · ');
    return { created: out?.created || 0, task: null, warning };
  }
  let task;
  if (plan.kind === 'tracker') {
    const out = await api.capture(groupName, plan.text, { next: plan.next });
    const result = out?.results?.[0];
    if (!result?.task?.id) throw new Error(result?.error || 'the ref was not added');
    task = result.task;
    if (plan.body || plan.after.length) {
      try {
        task = await api.edit(task.id, { body: plan.body || undefined, addDeps: plan.after.length ? plan.after : undefined }) || task;
      } catch (cause) {
        return { created: 1, task, warning: `added as ${task.id}, but its brief or dependencies were not saved: ${cause?.message || cause}` };
      }
    }
  } else {
    task = await api.create(groupName, plan.task);
  }
  if (plan.to && task?.id) {
    try {
      await api.assign(task.id, plan.to);
    } catch (cause) {
      return { created: 1, task, warning: `added as ${task.id}, but not assigned: ${cause?.message || cause}` };
    }
  }
  return { created: 1, task, warning: '' };
}

// NewTaskMenu is the section head's "+ New task" button and its menu.
export function NewTaskMenu({ group, entry, wizard = false, onChoose }) {
  const [open, setOpen] = useState(false);
  const ref = useRef(null);
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') setOpen(false); });
  const off = askUnavailable(group, entry, wizard);
  const choose = (what) => () => { setOpen(false); onChoose(what); };
  const item = (attr, onClick, name, line, disabled = false, title = undefined) => html`<${MenuItem} ...${{ [attr]: true }} class="gr-new-item"
    onClick=${onClick} disabled=${disabled} title=${title}><span class="gr-new-name">${name}</span><span class="gr-new-line">${line}</span><//>`;
  // Icon-only, like the Resources ＋ beside it: the name lives in the
  // title and aria-label.
  const label = wizard ? 'New quest' : 'New task';
  const title = wizard ? `${label}: add a quest to the ${group.name} quest board` : `${label}: add a task to the ${group.name} task board`;
  return html`<span class="gr-add tb-new" ref=${ref}>
    <${Button} size="sm" variant="primary" class="spawn-btn gr-add-btn tb-new-btn" data-taskboard-new=${group.name} aria-haspopup="menu"
      aria-expanded=${open ? 'true' : 'false'} title=${title} aria-label=${label} onClick=${() => setOpen(!open)}><span class="gr-add-plus" aria-hidden="true">＋</span><span class="gr-add-caret" aria-hidden="true">▾</span><//>
    ${open && html`<${Menu} size="sm" class="gr-add-menu gr-new-menu tb-new-menu">
      ${item('data-taskboard-new-form', choose('form'), wizard ? 'Scribe it myself' : 'Write it myself',
        wizard ? 'Fill in the scroll: title, brief, ref, stage' : 'Fill in a form: title, brief, ref, stage')}
      ${item('data-taskboard-new-ask', choose('ask'), wizard ? 'Ask a hero' : 'Ask an agent',
        off || (wizard ? 'Tell it in your words; a hero scribes the quests' : 'Describe it; an agent writes the tasks'), !!off, off || undefined)}
      <${MenuSep} />
      ${item('data-taskboard-new-import', choose('import'), 'From awb/Linear/GitHub…', wizard ? 'One quest per tracker id, named from the tracker' : 'One task per tracker id, titled from the tracker')}
    <//>`}
  </span>`;
}

// The stage switch: Idea (the default) or Next.
function StagePick({ value, onChange, wizard, disabled }) {
  return html`<span class="tb-new-stage" role="radiogroup" aria-label="Stage">
    ${[['idea', wizard ? 'Vision' : 'Idea'], ['next', 'Next']].map(([key, text]) => html`<label key=${key} class="tb-new-stage-opt">
      <input type="radio" name="tb-new-stage" value=${key} checked=${value === key} disabled=${disabled} onChange=${() => onChange(key)} />
      <span>${text}</span></label>`)}
  </span>`;
}

// DepsPick lists the board's live tasks as checkboxes: `--after`.
function DepsPick({ tasks, value, onChange, disabled }) {
  if (!tasks.length) return html`<span class="ui-hint">No live tasks to wait on.</span>`;
  const picked = new Set(value);
  const toggle = (id) => onChange(picked.has(id) ? value.filter((v) => v !== id) : [...value, id]);
  return html`<div class="tb-new-deps" role="group" aria-label="Depends on">
    ${tasks.map((task, i) => html`<${Check} key=${task.id} checked=${picked.has(task.id)} disabled=${disabled}
      data-dep=${task.id} onChange=${() => toggle(task.id)}><span class="tb-new-dep-n">#${i + 1}</span> ${task.title} <span class="tb-id">${task.id}</span><//>`)}
  </div>`;
}

const EMPTY_DRAFT = { title: '', list: '', body: '', ref: '', stage: 'idea', after: [], to: '' };

// NewTaskDialog is "Write it myself". `canAssign` shows the assignee
// picker; `members` are the ones a task can go to.
export function NewTaskDialog({ group, entry, api, canAssign = false, members = [], wizard = false, initial = {}, onClose, onDone = () => {} }) {
  const [draft, setDraft] = useState({ ...EMPTY_DRAFT, ...initial });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const sending = useRef(false);
  const titleRef = useRef(null);
  const set = (key) => (value) => setDraft((current) => ({ ...current, [key]: value }));
  const listMode = !!draft.list;
  const lines = captureLines(draft.list);
  const tasks = entry?.tasks || [];
  const tracker = !listMode && !draft.ref.trim() && isTrackerLine(draft.title);
  const ready = listMode ? lines.length > 0 : draft.title.trim().length > 0;
  const dirty = Object.keys(EMPTY_DRAFT).some((key) => JSON.stringify(draft[key]) !== JSON.stringify({ ...EMPTY_DRAFT, ...initial }[key]));
  const submit = async () => {
    if (!ready || sending.current) return;
    sending.current = true;
    setBusy(true);
    setError('');
    try {
      const out = await submitNewTask(api, group.name, draft);
      if (out.warning) shellToast(out.warning, true);
      else if (out.task) shellToast(`Added ${out.task.id} to ${group.name}`);
      else shellToast(`Added ${out.created} to ${group.name}`);
      onDone(out);
      onClose();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      sending.current = false;
      setBusy(false);
    }
  };
  // A pasted list turns the title into a list: one task per line.
  const onPaste = (event) => {
    const pasted = event.clipboardData?.getData('text') || '';
    if (captureLines(pasted).length < 2) return;
    event.preventDefault();
    setDraft((current) => ({ ...current, list: [current.title, pasted].filter((s) => s.trim()).join('\n'), title: '' }));
  };
  const noun = wizard ? 'quest' : 'task';
  const heading = wizard ? `New quest · ${group.name}` : `New task · ${group.name}`;
  const submitLabel = busy ? 'Adding…' : listMode ? `Add ${lines.length} ${noun}${lines.length === 1 ? '' : 's'}` : `Add ${noun}`;
  // A div, not a label: the stage and deps rows hold several controls, and
  // every control carries its own aria-label.
  const row = (label, field, hint = null) => html`<div class="cron-create-row tb-new-row"><span class="cron-create-label">${label}</span>
    <span class="tb-new-field">${field}${hint && html`<span class="ui-hint tb-new-hint">${hint}</span>`}</span></div>`;
  return html`<${Dialog} id="taskboard-new-modal" title=${heading} onClose=${onClose} busy=${busy}
    dirty=${dirty} confirmDiscard=${shellConfirmDiscard} initialFocusRef=${titleRef} onSubmitHotkey=${() => void submit()}
    resizeKey="tclaude.dash.modalSize.taskboard-new" error=${error}
    cancel="Cancel"
    footerStart=${html`<span class="ui-hint tb-new-cli"><code>tclaude agent taskboard add ${group.name} …</code></span>`}
    footer=${html`<${Button} variant="primary" id="taskboard-new-submit" busy=${busy} disabled=${!ready || busy} onClick=${() => void submit()}>${submitLabel}<//>`}>
    <div class="modal-meta">${listMode
      ? `One ${noun} per line; a line that is only an awb, Linear or GitHub id becomes that ref, titled from the tracker.`
      : wizard ? 'The quest lands among the visions unless you queue it as next.' : 'The task lands in the ideas unless you queue it as next.'}</div>
    ${listMode
      ? row('Tasks', html`<${Textarea} class="tb-new-list" rows="6" aria-label="Tasks, one per line" value=${draft.list} disabled=${busy}
          domRef=${titleRef} onInput=${(event) => set('list')(event.currentTarget.value)} />
          <${Button} size="sm" variant="link" class="tb-new-single" disabled=${busy} onClick=${() => setDraft((current) => ({ ...current, list: '', title: captureLines(current.list)[0] || '' }))}>one ${noun} instead<//>`)
      : html`
        ${row('Title', html`<${Input} class="tb-new-title" aria-label="Title" required maxLength=${200} value=${draft.title} disabled=${busy}
          domRef=${titleRef} placeholder=${`one line (required); paste a list for one ${noun} per line`}
          onInput=${(event) => set('title')(event.currentTarget.value)} onPaste=${onPaste}
          onKeyDown=${(event) => { if (event.key === 'Enter' && !event.isComposing) { event.preventDefault(); void submit(); } }} />`,
          tracker ? 'only a tracker id: it becomes the ref, titled from the tracker' : null)}
        ${row('Brief', html`<${Textarea} class="tb-new-body" rows="4" aria-label="Brief" value=${draft.body} disabled=${busy}
          placeholder="what the assignee needs to know (optional)" onInput=${(event) => set('body')(event.currentTarget.value)} />`)}
        ${row('Ref', html`<${Input} class="tb-new-ref" aria-label="Ref" value=${draft.ref} disabled=${busy || tracker}
          placeholder="URL, owner/repo#12, tcl-a1b2c3 or LIN-123 (optional)" onInput=${(event) => set('ref')(event.currentTarget.value)} />`,
          draft.ref.trim() ? 'a bare awb or Linear id is linked through its tracker' : null)}`}
    ${row('Stage', html`<${StagePick} value=${draft.stage} wizard=${wizard} disabled=${busy} onChange=${set('stage')} />`)}
    ${!listMode && row('After', html`<${DepsPick} tasks=${tasks} value=${draft.after} disabled=${busy} onChange=${set('after')} />`)}
    ${!listMode && canAssign && row('Assignee', members.length
      ? html`<${Select} class="tb-new-assignee" aria-label="Assignee" value=${draft.to} disabled=${busy} onChange=${(event) => set('to')(event.currentTarget.value)}>
          <option value="">— unassigned —</option>
          ${members.map((member) => html`<option key=${member.agent_id} value=${member.agent_id}>${member.title || member.agent_id}${member.online ? '' : ' (offline)'}</option>`)}
        <//>`
      : html`<span class="ui-hint">No member to assign to.</span>`,
      draft.to ? `the brief goes to its inbox${draft.stage === 'idea' ? '; the idea joins the queue' : ''}` : null)}
  <//>`;
}

// AskAgentDialog is "Ask an agent": the request goes to the picked member's
// inbox, never as keys into its pane.
export function AskAgentDialog({ group, entry, api, wizard = false, onClose }) {
  const members = askableMembers(group, entry);
  const [to, setTo] = useState(defaultAskee(members)?.agent_id || '');
  const [prompt, setPrompt] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const sending = useRef(false);
  const promptRef = useRef(null);
  const picked = members.find((member) => member.agent_id === to) || null;
  const ready = !!picked && prompt.trim().length > 0;
  const submit = async () => {
    if (!ready || sending.current) return;
    sending.current = true;
    setBusy(true);
    setError('');
    try {
      await api.ask(group.name, picked.agent_id, prompt.trim());
      shellToast(`Asked ${picked.title || picked.agent_id}; it replies when the ${wizard ? 'quests' : 'tasks'} are on the board`);
      onClose();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      sending.current = false;
      setBusy(false);
    }
  };
  const noun = wizard ? 'quests' : 'tasks';
  return html`<${Dialog} id="taskboard-ask-modal" title=${wizard ? `Ask a hero · ${group.name}` : `Ask an agent · ${group.name}`}
    onClose=${onClose} busy=${busy} dirty=${prompt.trim().length > 0} confirmDiscard=${shellConfirmDiscard}
    initialFocusRef=${promptRef} onSubmitHotkey=${() => void submit()} resizeKey="tclaude.dash.modalSize.taskboard-ask" error=${error}
    cancel="Cancel"
    footerStart=${html`<span class="ui-hint tb-new-cli"><code>tclaude agent taskboard ask ${group.name} --to … "…"</code></span>`}
    footer=${html`<${Button} variant="primary" id="taskboard-ask-submit" busy=${busy} disabled=${!ready || busy} onClick=${() => void submit()}>${busy ? 'Sending…' : 'Ask'}<//>`}>
    <div class="modal-meta">Describe what you want in your own words. The agent turns it into well-formed ${noun} on this board
      with <code>tclaude agent taskboard add</code>, replies when done, and does not start the work.</div>
    <div class="cron-create-row tb-new-row"><span class="cron-create-label">Agent</span><span class="tb-new-field">
      <${Select} class="tb-ask-to" aria-label="Agent to ask" value=${to} disabled=${busy || !members.length} onChange=${(event) => setTo(event.currentTarget.value)}>
        ${members.map((member) => html`<option key=${member.agent_id} value=${member.agent_id}>${member.title || member.agent_id}${member.owner ? ' (owner)' : member.role ? ` (${member.role})` : ''}</option>`)}
      <//>
      ${!members.length && html`<span class="ui-hint">${askUnavailable(group, entry, wizard)}</span>`}
    </span></div>
    <div class="cron-create-row tb-new-row"><span class="cron-create-label">Request</span><span class="tb-new-field">
      <${Textarea} class="tb-ask-prompt" rows="6" aria-label="Request" value=${prompt} disabled=${busy} domRef=${promptRef}
        placeholder=${`e.g. "the three follow-ups from today's review of the lanes view, with their awb ids"`}
        onInput=${(event) => setPrompt(event.currentTarget.value)} />
      <span class="ui-hint tb-new-hint">sent to its inbox, never typed into its pane · <kbd>Ctrl</kbd>+<kbd>Enter</kbd> sends</span>
    </span></div>
  <//>`;
}
