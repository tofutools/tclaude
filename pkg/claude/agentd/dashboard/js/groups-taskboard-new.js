// groups-taskboard-new.js -- the Task board's one add entry point, "+ New
// task" (wizard: "+ New quest"), and the dialogs it opens (docs/taskboard.md,
// tcl-kp5ngc). Modelled on the Resources section's ＋ menu
// (groups-resources.js AddResource):
//
//   Write it myself     the New task form: title, brief, ref, stage, deps,
//                       files and (with can.assign) an assignee. Each field
//                       is a `taskboard add` flag (files: --attach).
//                       A pasted list is one task per line and a title that
//                       is only a tracker id is that ref, titled from the
//                       tracker, as the old capture box did.
//   Ask an agent        a prose request mailed to a member who may change the
//                       board, files with it (`taskboard ask <group> --to
//                       <agent> "…" --attach …`).
//   From a tracker…     a form for one ticket's URL, or several ids
//                       (groups-taskboard-tracker.js: `add --ref`, and
//                       `add --from-awb` and kin).

import { h } from 'preact';
import { useContext, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { useDismissablePopover } from './hover-popover.js';
import { Button, Input, Menu, MenuItem, MenuSep, Select, Textarea } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { captureLines, isTrackerLine, taskFileLimits } from './taskboard-actions.js';
import { useAttachmentIntake } from './attachment-intake.js';
import { FilesField, taskPastedName, useMarkdownFiles } from './groups-taskboard-files.js';
import { MarkdownEditor, submitKeyLabel } from './markdown-editor.js';
import { DepsPicker } from './groups-taskboard-deps.js';
import { RefsField, draftRefs } from './groups-taskboard-refs-field.js';

import { StagePicker, columnSlug } from './groups-taskboard-stage.js';
import { EPIC, EpicBoard } from './groups-taskboard-epics.js';
const html = htm.bind(h);

// Members as groups-taskboard.js reads them: active agents with a stable id,
// online ones first.
function activeMembers(group) {
  return (group?.members || []).filter((member) => member.agent_id && !member.retired)
    .sort((a, b) => Number(!!b.online) - Number(!!a.online) || String(a.title || '').localeCompare(String(b.title || '')));
}

// askableMembers are the members "Ask an agent" may mail: online, and able
// to add tasks to the board. The context's askable list says who (those
// holding taskboard.write who pass the writer lock; a member that only
// tracks its own work is not one). A node that does not send it yet falls
// back to the writer lock: without it every member, with it its writers.
export function askableMembers(group, entry) {
  if (Array.isArray(entry?.askable)) {
    const askable = new Set(entry.askable);
    return activeMembers(group).filter((member) => member.online && askable.has(member.agent_id));
  }
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
  if (wizard) return 'No awake hero may write on this board';
  return Array.isArray(entry?.askable) && !entry?.locked
    ? 'No online member may add tasks to this board (it takes taskboard.write)'
    : 'No online member may change this board (writer lock)';
}

// START_STATES are the board columns a new task can start in: the stage
// (idea, next), in progress (assigned), blocked (with its reason) and done
// (work logged after the fact). `taskboard add` takes the same: --next,
// --assignee, --blocked "<reason>", --done.
export const START_STATES = [
  { key: 'idea', label: 'Idea', wizard: 'Vision' },
  { key: 'next', label: 'Next', wizard: 'Next' },
  { key: 'progress', label: 'In progress', wizard: 'Underway', assign: true },
  { key: 'blocked', label: 'Blocked', wizard: 'Blocked' },
  { key: 'done', label: 'Done', wizard: 'Fulfilled' },
];

// startFields are the create's fields for a start state. An assignee with
// Idea or Next holds the task there, not started (add --assignee X --idea /
// --next); In progress starts it.
export function startFields(start, { to = '', reason = '' } = {}) {
  // A custom column of the board: add --stage <column>, in the same write.
  const slug = columnSlug(start);
  if (slug) return { stage: slug, status: '', to };
  switch (start) {
    case 'next': return to ? { stage: 'next', status: '', to, hold: true } : { stage: 'next', status: '', to };
    case 'progress': return { stage: 'next', status: '', to };
    case 'blocked': return { stage: 'next', status: 'blocked', blockReason: reason.trim(), to };
    case 'done': return { stage: 'idea', status: 'done', to };
    default: return to ? { stage: 'idea', status: '', to, hold: true } : { stage: 'idea', status: '', to };
  }
}

// newTaskPlan turns the form into its writes, in order. `lines` is a pasted
// list: one task per line, as an import, in Ideas or Next. A single title
// that is only a tracker id (and no ref) is that ref, titled from the
// tracker: an import too, whose brief, dependencies, assignee and status
// follow as edits. Anything else is one `add`, its start state and assignee
// included.
export function newTaskPlan(draft) {
  const start = draft.start || 'idea';
  const next = start !== 'idea' && start !== 'done';
  // An epic is one: a list (one task per line) is never an epic's.
  if (draft.list && draft.kind !== EPIC) {
    return { kind: 'lines', text: captureLines(draft.list).join('\n'), next: start === 'next' };
  }
  const title = String(draft.title || '').trim();
  const body = String(draft.body || '').trim();
  // The refs: the first is the task's ref, the rest add_refs (tk_c447158c).
  const refs = draftRefs({ refs: draft.refs || [], newRef: draft.newRef || '' });
  const ref = refs[0] || '';
  const addRefs = refs.slice(1);
  const after = draft.after || [];
  // An epic starts in Ideas or Next with no assignee; a task may go in one.
  if (draft.kind === EPIC) {
    return { kind: 'add', task: { title, body, ref, ...(addRefs.length ? { addRefs } : {}), resolveRef: !!ref, kind: EPIC, stage: start === 'next' ? 'next' : 'idea', status: '', to: '' } };
  }
  const parent = draft.parent ? { parent: draft.parent } : {};
  const fields = startFields(start, { to: draft.to || '', reason: draft.reason || '' });
  // A title that is only a tracker id is an import, titled from the tracker,
  // when it starts in Ideas or Next unassigned. With any other start state
  // it is one add with the id as its ref (linked through its tracker), so
  // the state lands in the same call: an import cannot carry it.
  if (!ref && isTrackerLine(title) && !fields.to && !fields.status && !draft.parent && (start === 'idea' || start === 'next')) {
    return { kind: 'tracker', text: title, next, body, after };
  }
  if (!ref && isTrackerLine(title)) {
    return { kind: 'add', task: { title, body, ref: title, after, resolveRef: true, ...fields, ...parent } };
  }
  return { kind: 'add', task: { title, body, ref, ...(addRefs.length ? { addRefs } : {}), after, resolveRef: !!ref, ...fields, ...parent } };
}

// submitNewTask runs the plan. It resolves to { created, task, warning }:
// a refusal before anything landed throws; one after (the assign, say)
// is the warning, since the task is on the board by then.
export async function submitNewTask(api, groupName, draft, files = []) {
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
    // An import takes no files: they follow the task.
    if (files.length) {
      try {
        await api.attach(task.id, files);
      } catch (cause) {
        return { created: 1, task, warning: `added as ${task.id}, but its files were not attached: ${cause?.message || cause}` };
      }
    }
    if (plan.body || plan.after.length) {
      try {
        task = await api.edit(task.id, { body: plan.body || undefined, addDeps: plan.after.length ? plan.after : undefined }) || task;
      } catch (cause) {
        return { created: 1, task, warning: `added as ${task.id}, but its brief or dependencies were not saved: ${cause?.message || cause}` };
      }
    }
  } else {
    return { created: 1, task: await api.create(groupName, files.length ? { ...plan.task, files } : plan.task), warning: '' };
  }
  return { created: 1, task, warning: '' };
}

// CELL_MENU_HEIGHT is about the add menu's height: less room than this below
// a cell's "+" opens it above.
const CELL_MENU_HEIGHT = 170;

// NewTaskMenu is the section head's "+ New task" button and its menu. With
// `cell` it is a lanes cell's small "+" instead ({ label, title }): the same
// menu, anchored at the "+" and fixed to the viewport, so the lanes grid's
// own scrolling never clips it; the caller carries the cell's start state
// into whichever choice is made.
export function NewTaskMenu({ group, entry, wizard = false, onChoose, cell = null }) {
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState(null);
  const ref = useRef(null);
  useDismissablePopover(ref, open, (why) => { if (why !== 'away') setOpen(false); });
  const off = askUnavailable(group, entry, wizard);
  const choose = (what) => () => { setOpen(false); onChoose(what); };
  // The cell menu is fixed at its "+": below it, or above it where the
  // viewport has no room below; a scroll moves the "+" away, so it closes.
  const toggle = (event) => {
    if (cell && !open) {
      const rect = event.currentTarget.getBoundingClientRect?.();
      const width = globalThis.document?.documentElement?.clientWidth || globalThis.innerWidth || 0;
      const height = globalThis.innerHeight || 0;
      const room = height - (rect?.bottom || 0);
      setAt(rect ? { right: Math.max(4, width - rect.right),
        ...(height && room < CELL_MENU_HEIGHT ? { bottom: Math.max(4, height - rect.top + 2) } : { top: rect.bottom + 2 }) } : null);
    }
    setOpen(!open);
  };
  useEffect(() => {
    if (!cell || !open) return undefined;
    const close = () => setOpen(false);
    // Capturing on the document sees the page's scroll and any scroller's.
    document.addEventListener('scroll', close, true);
    return () => document.removeEventListener('scroll', close, true);
  }, [cell, open]);
  const item = (attr, onClick, name, line, disabled = false, title = undefined) => html`<${MenuItem} ...${{ [attr]: true }} class="gr-new-item"
    onClick=${onClick} disabled=${disabled} title=${title}><span class="gr-new-name">${name}</span><span class="gr-new-line">${line}</span><//>`;
  // Icon-only, like the Resources ＋ beside it: the name lives in the
  // title and aria-label.
  const label = wizard ? 'New quest' : 'New task';
  const title = wizard ? `${label}: add a quest to the ${group.name} quest board` : `${label}: add a task to the ${group.name} task board`;
  const button = cell
    ? html`<${Button} size="sm" variant="ghost" class="tb-cell-add" aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
        title=${cell.title} aria-label=${cell.label} onClick=${toggle}>+<//>`
    : html`<${Button} size="sm" variant="primary" class="spawn-btn gr-add-btn tb-new-btn" data-taskboard-new=${group.name} aria-haspopup="menu"
      aria-expanded=${open ? 'true' : 'false'} title=${title} aria-label=${label} onClick=${toggle}><span class="gr-add-plus" aria-hidden="true">＋</span><span class="gr-add-caret" aria-hidden="true">▾</span><//>`;
  const fixed = cell && at ? `position: fixed; right: ${at.right}px; left: auto; ${at.bottom !== undefined ? `bottom: ${at.bottom}px; top: auto;` : `top: ${at.top}px;`}` : undefined;
  return html`<span class=${cell ? 'tb-cell-add-wrap' : 'gr-add tb-new'} ref=${ref}>
    ${button}
    ${open && html`<${Menu} size="sm" class=${`gr-add-menu gr-new-menu tb-new-menu${cell ? ' tb-cell-menu' : ''}`} style=${fixed}>
      ${item('data-taskboard-new-form', choose('form'), wizard ? 'Scribe it myself' : 'Write it myself',
        wizard ? 'Fill in the scroll: title, brief, ref, where it starts' : 'Fill in a form: title, brief, ref, where it starts')}
      ${item('data-taskboard-new-ask', choose('ask'), wizard ? 'Ask a hero' : 'Ask an agent',
        off || (wizard ? 'Tell it in your words; a hero scribes the quests' : 'Describe it; an agent writes the tasks'), !!off, off || undefined)}
      ${!cell && item('data-taskboard-new-epic', choose('epic'), 'New epic',
        'A container for a concern\'s tasks: its progress is theirs')}
      <${MenuSep} />
      ${item('data-taskboard-new-import', choose('import'), 'From a tracker…',
        wizard ? 'Paste a ticket URL (awb, Linear, GitHub…); the quest is named from it' : 'Paste a ticket URL (awb, Linear, GitHub…); the task is titled from it')}
    <//>`}
  </span>`;
}

// StartPick is where the task starts: a board column, picked in the shared
// stage picker (groups-taskboard-stage.js), the board's custom columns after
// the built-in ones. In progress needs an assignee, so only a viewer who may
// assign gets it; a list starts in Ideas or Next.
function StartPick({ value, onChange, wizard, disabled, canAssign, listMode, columns = [] }) {
  const places = START_STATES.filter((s) => !listMode || s.key === 'idea' || s.key === 'next');
  const off = (key) => (key === 'progress' && !canAssign ? 'needs taskboard.assign: it starts with an assignee' : '');
  return html`<${StagePicker} id="taskboard-new-stage" value=${value} onChange=${(key) => onChange(key || 'idea')} places=${places}
    columns=${listMode ? [] : columns} wizard=${wizard} off=${off} disabled=${disabled} ariaLabel="Starts in" />`;
}

const EMPTY_DRAFT = { title: '', list: '', body: '', refs: [], newRef: '', start: 'idea', reason: '', after: [], to: '', parent: '' };

// NewTaskDialog is "Write it myself". `canAssign` shows the assignee
// picker; `members` are the ones a task can go to.
// operatorOptionLabel names the "operator" assignee in a picker: you on this
// node's boards, a linked node's operator on its boards.
export function operatorOptionLabel(group) {
  const node = group?.fleet_node;
  return node && !node.local ? `operator of ${node.name || node.id}` : 'You (operator)';
}

export function NewTaskDialog({ group, entry, api, canAssign = false, members = [], wizard = false, initial = {}, onClose, onDone = () => {} }) {
  // A board that hides its Ideas starts a new task in Next (tk_bf5d2b02).
  const ideasHidden = entry?.builtin_columns?.ideas === 'hidden';
  const [draft, setDraft] = useState({ ...EMPTY_DRAFT, ...(ideasHidden ? { start: 'next' } : {}), ...initial });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const sending = useRef(false);
  const titleRef = useRef(null);
  const [files, setFiles] = useState([]);
  const briefEditor = useRef(null);
  // A file pasted or dropped on the brief is linked there too.
  const intake = useAttachmentIntake({ setAttachments: setFiles, attachments: files, busy: () => sending.current,
    limits: taskFileLimits(0), onReject: (text) => shellToast(text, true), nameFile: taskPastedName,
    onAdded: (added, event) => { if (event?.target?.closest?.('.tb-new-body')) briefEditor.current?.insertFiles(added.map((a) => a.name), event); } });
  const briefFiles = useMarkdownFiles(api, null, files);
  const set = (key) => (value) => setDraft((current) => ({ ...current, [key]: value }));
  const epic = draft.kind === EPIC;
  const epics = useContext(EpicBoard)?.epics || [];
  const listMode = !epic && !!draft.list;
  const lines = captureLines(draft.list);
  const tasks = entry?.tasks || [];
  const hasRefs = draftRefs(draft).length > 0;
  const tracker = !listMode && !hasRefs && isTrackerLine(draft.title);
  // In progress needs its assignee, blocked its reason.
  const missing = listMode ? '' : draft.start === 'progress' && !draft.to ? 'pick who it is in progress with'
    : draft.start === 'blocked' && !draft.reason.trim() ? 'say why it is blocked' : '';
  const ready = (listMode ? lines.length > 0 : draft.title.trim().length > 0) && !missing;
  const dirty = files.length > 0 || Object.keys(EMPTY_DRAFT).some((key) => JSON.stringify(draft[key]) !== JSON.stringify({ ...EMPTY_DRAFT, ...initial }[key]));
  const submit = async () => {
    if (!ready || sending.current) return;
    sending.current = true;
    setBusy(true);
    setError('');
    try {
      const out = await submitNewTask(api, group.name, draft, listMode ? [] : files.map(({ file, name }) => ({ file, name })));
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
    if (epic || captureLines(pasted).length < 2) return;
    event.preventDefault();
    setDraft((current) => ({ ...current, list: [current.title, pasted].filter((s) => s.trim()).join('\n'), title: '' }));
  };
  const noun = wizard ? 'quest' : 'task';
  const heading = epic ? `New epic · ${group.name}` : wizard ? `New quest · ${group.name}` : `New task · ${group.name}`;
  const submitLabel = busy ? 'Adding…' : listMode ? `Add ${lines.length} ${noun}${lines.length === 1 ? '' : 's'}` : epic ? 'Add epic' : `Add ${noun}`;
  // A div, not a label: the stage and deps rows hold several controls, and
  // every control carries its own aria-label.
  const row = (label, field, hint = null) => html`<div class="cron-create-row tb-new-row"><span class="cron-create-label">${label}</span>
    <span class="tb-new-field">${field}${hint && html`<span class="ui-hint tb-new-hint">${hint}</span>`}</span></div>`;
  return html`<${Dialog} id="taskboard-new-modal" title=${heading} onClose=${onClose} busy=${busy}
    ...${intake.handlers} dialogClass=${intake.dragOver ? 'files-drag-over' : ''}
    dirty=${dirty} confirmDiscard=${shellConfirmDiscard} initialFocusRef=${titleRef} onSubmitHotkey=${() => void submit()}
    resizeKey="tclaude.dash.modalSize.taskboard-new" error=${error}
    cancel="Cancel"
    footerStart=${html`<span class="ui-hint tb-new-cli"><code>tclaude agent taskboard add ${group.name} …</code></span>`}
    footer=${html`<${Button} variant="primary" id="taskboard-new-submit" busy=${busy} disabled=${!ready || busy} onClick=${() => void submit()}>${submitLabel}<//>`}>
    <div class="modal-meta">${listMode
      ? `One ${noun} per line; a line that is only an awb, Linear or GitHub id becomes that ref, titled from the tracker.${
        draft.body.trim() || hasRefs || draft.after.length || draft.to || files.length ? ' A list takes no brief, ref, dependencies, assignee or files: they are not used.' : ''}`
      : epic ? 'An epic groups the tasks of one concern: its progress is theirs, and it has no assignee. As an idea it stays in Ideas; in Next it joins the epic strip above the board.'
      : wizard ? 'The quest lands among the visions unless you start it elsewhere.' : 'The task lands in the ideas unless you start it in another column.'}</div>
    ${listMode
      ? row('Tasks', html`<${Textarea} class="tb-new-list" rows="6" aria-label="Tasks, one per line" value=${draft.list} disabled=${busy}
          domRef=${titleRef} onInput=${(event) => set('list')(event.currentTarget.value)} />
          <${Button} size="sm" variant="link" class="tb-new-single" disabled=${busy} onClick=${() => setDraft((current) => ({ ...current, list: '', title: captureLines(current.list)[0] || '' }))}>one ${noun} instead<//>`)
      : html`
        ${row('Title', html`<${Input} class="tb-new-title" aria-label="Title" required maxLength=${200} value=${draft.title} disabled=${busy}
          domRef=${titleRef} placeholder=${`one line (required); paste a list for one ${noun} per line`}
          onInput=${(event) => set('title')(event.currentTarget.value)} onPaste=${onPaste}
          onKeyDown=${(event) => { if (event.key === 'Enter' && !event.isComposing) { event.preventDefault(); void submit(); } }} />`,
          tracker ? (draft.to || !['idea', 'next'].includes(draft.start)
            ? 'only a tracker id: it becomes the ref, and stays the title (an import cannot start it here or assign it)'
            : 'only a tracker id: it becomes the ref, titled from the tracker') : null)}
        ${row('Brief', html`<${MarkdownEditor} class="tb-new-body" label="Brief" value=${draft.body} disabled=${busy}
          placeholder="what the assignee needs to know (optional; Markdown)" onInput=${set('body')}
          editorRef=${briefEditor} attachments=${briefFiles} onPickFiles=${(picked) => intake.add(picked).map((a) => a.name)}
          modeKey="tclaude.dash.mdEditor.brief" hint=${`${submitKeyLabel()} adds`} />`)}
        ${row('Refs', html`<${RefsField} draft=${draft} busy=${busy || tracker} pending="linked when added"
          onChange=${set('refs')} onNewRef=${set('newRef')} />`,
          hasRefs ? 'the first is the task\'s ref for --ref and the List (↑ makes another first); a bare awb or Linear id is linked through its tracker' : null)}`}
    ${row('Starts in', html`<${StartPick} value=${(listMode || epic) && draft.start !== 'next' ? 'idea' : draft.start} wizard=${wizard} disabled=${busy}
      canAssign=${canAssign} listMode=${listMode || epic} columns=${entry?.columns || []} onChange=${set('start')} />`, missing || null)}
    ${!listMode && !epic && epics.length > 0 && row('Epic', html`<${Select} class="tb-new-epic" aria-label="Epic" value=${draft.parent} disabled=${busy}
        onChange=${(event) => set('parent')(event.currentTarget.value)}>
        <option value="">— in no epic —</option>
        ${epics.map((e) => html`<option key=${e.id} value=${e.id}>${e.title}</option>`)}
      <//>`)}
    ${!listMode && draft.start === 'blocked' && row('Reason', html`<${Textarea} class="tb-new-reason" rows=${2} aria-label="Block reason" required maxLength=${400}
      value=${draft.reason} disabled=${busy} placeholder="why it is blocked (required)" onInput=${(event) => set('reason')(event.currentTarget.value)} />`)}
    ${!listMode && !epic && row('After', html`<${DepsPicker} tasks=${tasks} value=${draft.after} disabled=${busy} onChange=${set('after')} />`)}
    ${!listMode && row('Files', html`<${FilesField} intake=${intake} attachments=${files} busy=${busy} />`)}
    ${!listMode && !epic && canAssign && row('Assignee', html`<${Select} class="tb-new-assignee" aria-label="Assignee" value=${draft.to} disabled=${busy} onChange=${(event) => set('to')(event.currentTarget.value)}>
          <option value="">— unassigned —</option>
          <option value="operator">${operatorOptionLabel(group)}</option>
          ${members.map((member) => html`<option key=${member.agent_id} value=${member.agent_id}>${member.title || member.agent_id}${member.online ? '' : ' (offline)'}</option>`)}
        <//>`,
      draft.start === 'done' ? (draft.to ? 'who did it; nobody is mailed' : 'optional: who did it')
        : draft.to && (draft.start === 'idea' || draft.start === 'next')
          ? `${draft.start === 'idea' ? 'an idea for them to triage' : 'queued for them'}, not started${draft.to === 'operator' ? '' : '; a short note goes to its inbox'}`
        : draft.to === 'operator' ? 'it waits on the operator, who closes it as done'
        : draft.to ? 'the brief goes to its inbox' : null)}
  <//>`;
}

// AskAgentDialog is "Ask an agent": the request goes to the picked member's
// inbox, never as keys into its pane.
// askHint is the request's opening line for a start state picked in a lanes
// cell: where the agent should put the tasks it writes.
export function askHint(initial = {}, wizard = false) {
  const noun = wizard ? 'quests' : 'tasks';
  const who = initial.toName || initial.to || '';
  switch (initial.start) {
    case 'next': return who ? `Land the ${noun} in Next for ${who}, not started (taskboard add --assignee ${who} --next).\n\n` : `Land the ${noun} in Next.\n\n`;
    case 'progress': return `Land the ${noun} in Next for ${who} (assign them to ${who}).\n\n`;
    case 'blocked': return `Land the ${noun} as blocked${who ? ` for ${who}` : ''}, with the reason.\n\n`;
    case 'done': return `Log the ${noun} as done${who ? `, done by ${who}` : ''} (taskboard add --done).\n\n`;
    case 'idea': return who ? `Land the ${noun} as ideas for ${who} to triage (taskboard add --assignee ${who} --idea).\n\n` : `Land the ${noun} in the ideas.\n\n`;
    default: return '';
  }
}

export function AskAgentDialog({ group, entry, api, wizard = false, initial = {}, onClose }) {
  const members = askableMembers(group, entry);
  const [pickedId, setTo] = useState(defaultAskee(members)?.agent_id || '');
  // The pick falls back to the default while it is not (or no longer) one
  // of them: an agent can go offline, or the writer lock change, while the
  // dialog is open.
  const to = members.some((member) => member.agent_id === pickedId) ? pickedId : (defaultAskee(members)?.agent_id || '');
  const opening = askHint(initial, wizard);
  const [prompt, setPrompt] = useState(opening);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const sending = useRef(false);
  const promptRef = useRef(null);
  const [files, setFiles] = useState([]);
  const intake = useAttachmentIntake({ setAttachments: setFiles, attachments: files, busy: () => sending.current,
    limits: taskFileLimits(0), onReject: (text) => shellToast(text, true) });
  const picked = members.find((member) => member.agent_id === to) || null;
  // The cell's opening line alone is not a request.
  const ready = !!picked && prompt.trim().length > 0 && prompt.trim() !== opening.trim();
  const submit = async () => {
    if (!ready || sending.current) return;
    sending.current = true;
    setBusy(true);
    setError('');
    try {
      if (files.length) await api.ask(group.name, picked.agent_id, prompt.trim(), files.map(({ file, name }) => ({ file, name })));
      else await api.ask(group.name, picked.agent_id, prompt.trim());
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
    onClose=${onClose} busy=${busy} dirty=${files.length > 0 || (prompt.trim().length > 0 && prompt !== opening)} confirmDiscard=${shellConfirmDiscard}
    ...${intake.handlers} dialogClass=${intake.dragOver ? 'files-drag-over' : ''}
    initialFocusRef=${promptRef} onSubmitHotkey=${() => void submit()} resizeKey="tclaude.dash.modalSize.taskboard-ask" error=${error}
    cancel="Cancel"
    footerStart=${html`<span class="ui-hint tb-new-cli"><code>tclaude agent taskboard ask ${group.name} --to … "…"${files.length ? ' --attach …' : ''}</code></span>`}
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
    <div class="cron-create-row tb-new-row"><span class="cron-create-label">Files</span><span class="tb-new-field">
      <${FilesField} intake=${intake} attachments=${files} busy=${busy} />
      ${files.length > 0 && html`<span class="ui-hint tb-new-hint">sent with the request; the agent puts them on the tasks it writes</span>`}
    </span></div>
  <//>`;
}
