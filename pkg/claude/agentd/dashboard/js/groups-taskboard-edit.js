// groups-taskboard-edit.js -- the Task board's edit form (tcl-jy91ej,
// docs/taskboard.md): one dialog for everything about a task. Title, brief,
// ref, where it is (Idea, Next, In progress, Blocked with its reason, Done,
// Dropped: the New task form's "Starts in", plus Dropped), the assignee
// (with can.assign), its dependencies and its files. Save writes only what
// changed, each through the route its CLI verb uses:
//
//   title, brief, ref, deps   edit <task> --title … --body … --ref … --after/--not-after
//   where it is               promote / demote · block --reason · unblock ·
//                             done · drop · (reopen: status open)
//   assignee                  assign --to [--idea|--next] · unassign ·
//                             start · hold (an assignee holds a task in Idea
//                             or Next without starting it)
//   files                     attach · detach
//
// A task's questions for the operator head the form (TaskQuestions): an
// open one's Answer… (there or in the Activity) opens the Answer dialog,
// which answers at once, not with Save.
//
// Each change is its own write, so one can be refused while the rest land:
// the form then stays open and names what was not saved.

import { Fragment, h } from 'preact';
import { useContext, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';
import { COMMENT_AREA, TaskActivity } from './groups-taskboard-comments.js';
import { Button, Input, Select, Textarea } from './ui-components.js';
import { Dialog } from './ui-dialog.js';
import { shellConfirmDiscard, shellToast } from './shell-state.js';
import { taskFileLimits } from './taskboard-actions.js';
import { useAttachmentIntake } from './attachment-intake.js';
import { TaskFiles, taskPastedName, useMarkdownFiles } from './groups-taskboard-files.js';
import { MarkdownEditor, submitKeyLabel } from './markdown-editor.js';
import { DepsPicker } from './groups-taskboard-deps.js';
import { writeClipboard } from './insecure-origin.js';
import { operatorOptionLabel } from './groups-taskboard-new.js';
import { refKey, refsOf } from './groups-taskboard-card.js';
import { MAX_REFS, RefsField, draftRefs, multiRefs } from './groups-taskboard-refs-field.js';

export { MAX_REFS, RefsField, draftRefs, multiRefs };
import { AnswerWith, TaskQuestions, askerOf, taskQuestions } from './groups-taskboard-answer.js';

import { StagePicker, columnSlug } from './groups-taskboard-stage.js';
import { COLUMN_STATE, columnLabel } from './taskboard-context.js';
import { EpicBoard, EpicChildren, EpicProgress, isEpic, progressParts } from './groups-taskboard-epics.js';
const html = htm.bind(h);

// WHERE are the places a task can be, as the form offers them.
export const WHERE = [
  { key: 'idea', label: 'Idea', wizard: 'Vision' },
  { key: 'next', label: 'Next', wizard: 'Next' },
  { key: 'progress', label: 'In progress', wizard: 'Underway' },
  { key: 'blocked', label: 'Blocked', wizard: 'Blocked' },
  { key: 'done', label: 'Done', wizard: 'Fulfilled' },
  { key: 'dropped', label: 'Dropped', wizard: 'Abandoned' },
];

const CLOSED = new Set(['done', 'dropped']);

// EPIC_WHERE are the places an epic can be: it is no work of its own, so
// never in progress, blocked or in a column; its tasks are.
export const EPIC_WHERE = WHERE.filter((w) => !['progress', 'blocked'].includes(w.key));

// whereOf is where a task is now.
export function whereOf(task) {
  if (task.status === 'done' || task.status === 'dropped' || task.status === 'blocked') return task.status;
  if (task.column) return COLUMN_STATE + task.column;
  if (task.assignee?.agent_id && !task.held) return 'progress';
  return task.stage === 'idea' ? 'idea' : 'next';
}

// draftOf is the form's starting draft for a task.
export function draftOf(task) {
  return {
    title: task.title || '',
    body: task.body || '',
    refs: refsOf(task).map(refKey),
    newRef: '',
    where: whereOf(task),
    reason: task.status === 'blocked' || task.status === 'dropped' ? task.block_reason || '' : '',
    to: task.assignee?.agent_id || '',
    deps: [...(task.depends_on || [])],
    parent: task.parent_id || '',
  };
}

// setWhere moves the draft to `where`. In Idea or Next an assignee holds
// the task without starting it; In progress needs one.
export function setWhere(draft, where) {
  return { ...draft, where };
}

// setAssignee gives the draft an assignee: an unassigned idea or queued task
// that gets one is in progress, as assign starts it (pick Idea or Next after
// to have it held there instead); one in progress that loses it is back in
// Next.
export function setAssignee(draft, to) {
  const next = { ...draft, to };
  if (to && !draft.to && (draft.where === 'idea' || draft.where === 'next')) next.where = 'progress';
  if (!to && draft.where === 'progress') next.where = 'next';
  return next;
}

// missingOf says what the draft still needs before it can be saved, or ''.
export function missingOf(draft) {
  if (!draft.title.trim()) return 'a title';
  if (draft.where === 'progress' && !draft.to) return 'who it is in progress with';
  if (draft.where === 'blocked' && !draft.reason.trim()) return 'why it is blocked';
  return '';
}

// editPlan is the writes that take task to draft (and its files), in order,
// each { name, run(api) }. The text first; then the state: back to open when
// the task must be (to be queued, or to take a new assignee), the assignee
// taken off, the stage, the new assignee, and last the block or the close,
// so a task assigned and closed in one Save is closed by its assignee.
export function editPlan(task, draft, { add = [], remove = [], bodyKnown = true } = {}) {
  const steps = [];
  const change = {};
  const names = [];
  if (draft.title.trim() !== task.title) { change.title = draft.title.trim(); names.push('title'); }
  // The brief is compared only once the task has been read in full (the
  // board's context leaves it out): never overwrite one the form has not seen.
  if (bodyKnown && draft.body.trim() !== (task.body || '').trim()) { change.body = draft.body.trim(); names.push('brief'); }
  const refs = refsPlan(task, draft);
  if (refs) { Object.assign(change, refs); names.push(refs.addRefs || refs.rmRefs || refs.firstRef ? 'refs' : 'ref'); }
  const before = task.depends_on || [];
  const addDeps = draft.deps.filter((id) => !before.includes(id));
  const rmDeps = before.filter((id) => !draft.deps.includes(id));
  if (addDeps.length) change.addDeps = addDeps;
  if (rmDeps.length) change.rmDeps = rmDeps;
  if (addDeps.length || rmDeps.length) names.push('dependencies');
  // The epic it is in (edit --parent / --rm-parent).
  const parent = draft.parent ?? (task.parent_id || '');
  if (parent !== (task.parent_id || '')) {
    if (parent) change.parent = parent; else change.clearParent = true;
    names.push('epic');
  }
  if (names.length) steps.push({ name: names.join(', '), run: (api) => api.edit(task.id, change) });

  // A custom column (In review, …) stands in next: the task is queued, then
  // moved into it (`move --stage <column>`), or reopened straight into it.
  const column = columnSlug(draft.where);
  const where = column ? 'next' : draft.where;
  const from = task.assignee?.agent_id || '';
  const to = draft.to;
  const status = task.status || 'open';
  const assigning = !!to && to !== from;
  const unassigning = !to && !!from;
  const openTarget = where === 'idea' || where === 'next' || where === 'progress';
  // An assignment takes an open task: a blocked or closed one opens first,
  // and is blocked or closed again after.
  let open = status === 'open';
  // A closed task is blocked by reopening it first.
  // A closed task reopens into its column in the same write, with its
  // assignee as the form has it (kept, changed or taken off).
  let placed = false;
  if (column && CLOSED.has(status)) {
    const opts = { stage: column };
    if (unassigning) opts.to = '';
    else if (assigning) opts.to = to;
    steps.push({ name: 'reopen', run: (api) => api.reopen(task.id, opts) });
    open = true;
    placed = true;
  } else if (!open && (openTarget || assigning || (where === 'blocked' && CLOSED.has(status)))) {
    steps.push({ name: CLOSED.has(status) ? 'reopen' : 'unblock', run: (api) => api.status(task.id, 'open') });
    open = true;
  }
  if (unassigning && !placed) steps.push({ name: 'unassign', run: (api) => api.unassign(task.id) });
  // Blocked, done and dropped keep the stage the task is in.
  const stage = where === 'idea' ? 'idea' : where === 'next' || where === 'progress' ? 'next' : '';
  // The same assignee starts a task it held, or holds a started one in its
  // stage: either write also moves the stage.
  const kept = !!to && to === from;
  const starting = kept && open && where === 'progress' && !!task.held;
  const holding = !column && kept && open && (where === 'idea' || where === 'next') && !task.held;
  // An assignment places the task on its own: in Next started, or held in
  // the stage it lands in.
  if (!placed && !column && stage && stage !== (task.stage || 'idea') && !assigning && !starting && !holding) {
    steps.push({ name: stage === 'idea' ? 'back to the ideas' : 'into Next', run: (api) => api.stage(task.id, stage) });
  }
  // Out of its column: back in Next (in progress for a started task).
  if (!placed && !column && task.column && !CLOSED.has(task.status) && (where === 'next' || where === 'progress')) {
    steps.push({ name: 'out of the column', run: (api) => api.stage(task.id, 'next') });
  }
  if (starting) steps.push({ name: 'start', run: (api) => api.start(task.id) });
  if (holding) steps.push({ name: 'not started', run: (api) => api.hold(task.id, stage) });
  if (assigning && !placed) {
    const hold = !column && (where === 'idea' || where === 'next') ? stage : '';
    steps.push({ name: 'assignee', run: (api) => (hold ? api.assign(task.id, to, { hold }) : api.assign(task.id, to)) });
  }
  if (column && !placed && (task.column !== column || assigning)) {
    steps.push({ name: 'into the column', run: (api) => api.stage(task.id, column) });
  }
  const reason = draft.reason.trim();
  if (where === 'blocked' && (open || status !== 'blocked' || reason !== (task.block_reason || ''))) {
    steps.push({ name: 'block', run: (api) => api.status(task.id, 'blocked', reason) });
  }
  // An epic closed here closes with tasks still open in it, which stay
  // open (status --force); the form says so.
  if (where === 'done' && (open || status !== 'done')) {
    steps.push({ name: 'done', run: (api) => (isEpic(task) ? api.status(task.id, 'done', '', { force: true }) : api.status(task.id, 'done')) });
  }
  if (where === 'dropped' && (open || status !== 'dropped')) steps.push({ name: 'drop', run: (api) => api.status(task.id, 'dropped', reason) });

  for (const id of remove) {
    const file = (task.attachments || []).find((a) => a.id === id);
    steps.push({ name: `remove ${file?.name || `#${id}`}`, run: (api) => api.detach(task.id, id) });
  }
  if (add.length) steps.push({ name: add.length === 1 ? `attach ${add[0].name}` : `attach ${add.length} files`, run: (api) => api.attach(task.id, add) });
  return steps;
}

// refsPlan is the edit fields that take the task's refs to the draft's, or
// null with no change: refs taken off (rm_refs) and new ones added
// (add_refs, each linked through its tracker). A node with one ref per task
// gets the first one through ref / clear_ref.
export function refsPlan(task, draft) {
  const before = refsOf(task).map(refKey);
  const after = draftRefs(draft);
  if (!multiRefs(task)) {
    const ref = after[0] || '';
    if (ref === (before[0] || '')) return null;
    return ref ? { ref, resolveRef: true } : { clearRef: true };
  }
  const rmRefs = before.filter((key) => !after.includes(key));
  const addRefs = after.filter((key) => !before.includes(key));
  // The daemon keeps the rest in order and appends the new ones; a draft
  // whose first is another ref names it (first_ref).
  const kept = [...before.filter((key) => after.includes(key)), ...addRefs];
  const firstRef = after.length && kept[0] !== after[0] ? after[0] : '';
  if (!rmRefs.length && !addRefs.length && !firstRef) return null;
  const out = {};
  if (rmRefs.length) out.rmRefs = rmRefs;
  if (addRefs.length) { out.addRefs = addRefs; out.resolveRef = true; }
  if (firstRef) out.firstRef = firstRef;
  return out;
}

// runPlan runs the steps in order. A refusal does not stop the rest: each is
// its own change. Resolves to { saved: [names], refused: [{ name, message }] }.
export async function runPlan(api, steps) {
  const saved = [];
  const refused = [];
  for (const step of steps) {
    try {
      await step.run(api);
      saved.push(step.name);
    } catch (cause) {
      refused.push({ name: step.name, message: cause?.message || String(cause) });
    }
  }
  return { saved, refused };
}

function WherePick({ value, onChange, wizard, disabled, canAssign, canClose, current, columns = [], epic = false }) {
  // In progress takes an assignee: only a viewer who may assign moves a task
  // there. Done and Dropped follow who may close it.
  const off = (key) => {
    if (key === current) return '';
    if (key === 'progress' && !canAssign) return 'needs taskboard.assign: in progress is with an assignee';
    if (CLOSED.has(key) && !canClose) return 'not yours to close';
    return '';
  };
  return html`<${StagePicker} id="taskboard-edit-where" value=${value} onChange=${(key) => { if (key) onChange(key); }} places=${epic ? EPIC_WHERE : WHERE}
    columns=${epic ? [] : columns} wizard=${wizard} off=${off} disabled=${disabled} ariaLabel="Where it is" />`;
}

function whereHint(draft, task) {
  if (isEpic(task)) {
    const open = (task.progress?.total || 0) - (task.progress?.done || 0);
    if (draft.where === 'done' && task.status !== 'done') return open > 0 ? `closes the epic; its ${open} open task${open === 1 ? '' : 's'} stay open` : 'closes the epic';
    if (draft.where === 'dropped' && task.status !== 'dropped') return 'drops the epic; its tasks stay on the board';
    if (draft.where === 'next' && task.status === 'open' && task.stage === 'idea') return 'promotes it: it moves to the epic strip above the board';
  }
  switch (draft.where) {
    case 'blocked': return 'the reason shows on the card';
    case 'dropped': return 'optional: why it is dropped (e.g. "parked in awb")';
    case 'done': return task.status === 'done' ? 'closed; pick another place to reopen it' : 'closes it';
    case 'progress': return draft.to ? '' : 'pick its assignee below';
    default:
      if (columnSlug(draft.where)) return CLOSED.has(task.status) ? 'reopens it in that column' : 'a column of the board\'s own: its agent may wait there without an idle alarm';
      if (CLOSED.has(task.status)) return 'reopens it';
      return draft.to ? `held by its assignee, not started` : '';
  }
}

// EditTaskDialog is the edit form. members are the ones a task can go to;
// canAssign shows the assignee picker, canClose offers Done and Dropped.
// onSaved runs after a Save (also a partial one), to read the board again.
// onOpenTask(id), when given, opens another task's form (an epic's task).
export function EditTaskDialog({ group, entry, task, api, canAssign = false, canClose = true, members = [], wizard = false,
  nameOf = (actor) => actor, me = '', onClose, onSaved = () => {}, dirtyRef = null, onOpenTask = null }) {
  // The board's context carries no brief: the form reads the task in full,
  // and the brief field waits for it.
  const [brief, setBrief] = useState({ state: typeof task.body === 'string' && task.body ? 'ready' : 'loading', body: task.body || '' });
  // Save writes what the form changed since it opened (or since the last
  // Save), so changes others make meanwhile (an agent taking the task,
  // reporting it done) stand: the plan compares the draft with this
  // snapshot, not with the live row.
  const [opened, setOpened] = useState(task);
  const base = brief.state === 'ready' ? { ...opened, body: brief.body } : opened;
  const start = draftOf(base);
  const [draft, setDraft] = useState(start);
  useEffect(() => {
    if (brief.state !== 'loading' || typeof api?.task !== 'function') {
      if (brief.state === 'loading') setBrief({ state: 'failed', body: '' });
      return undefined;
    }
    let live = true;
    api.task(task.id).then((full) => {
      if (!live) return;
      const body = full?.body || '';
      setBrief({ state: 'ready', body });
      setDraft((current) => ({ ...current, body }));
    }).catch(() => { if (live) setBrief({ state: 'failed', body: '' }); });
    return () => { live = false; };
  }, [task.id]);
  const looked = useTrackerLookups(api, group?.name, task);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [removed, setRemoved] = useState([]);
  const [pending, setPending] = useState([]);
  const sending = useRef(false);
  const titleRef = useRef(null);
  const held = (task.attachments || []).length - removed.length;
  const briefEditor = useRef(null);
  const intake = useAttachmentIntake({ setAttachments: setPending, attachments: pending, busy: () => sending.current,
    limits: taskFileLimits(held), onReject: (text) => shellToast(text, true), nameFile: taskPastedName,
    // A paste or a drop in the Activity's comment box is the comment's.
    ignore: (event) => !!event.target?.closest?.(COMMENT_AREA),
    // One on the brief is linked there too.
    onAdded: (added, event) => { if (event?.target?.closest?.('.tb-edit-body')) briefEditor.current?.insertFiles(added.map((a) => a.name), event); } });
  const briefFiles = useMarkdownFiles(api, task, pending, draft.body);
  const set = (key) => (value) => setDraft((current) => ({ ...current, [key]: value }));
  const epic = isEpic(task);
  // The board's epics, for the Epic picker (an epic is in none).
  const epics = useContext(EpicBoard)?.epics || [];
  const tasks = (entry?.tasks || []).filter((t) => !isEpic(t));
  const steps = editPlan(base, draft, { add: pending.map(({ file, name }) => ({ file, name })), remove: removed, bodyKnown: brief.state === 'ready' });
  const missing = missingOf(draft);
  // A comment being written counts too: closing the form would lose it.
  const [commenting, setCommenting] = useState(false);
  // The question being answered, by number (0: the task's first open one);
  // null while none is.
  const [answering, setAnswering] = useState(null);
  const answerable = typeof api?.answer === 'function' && api?.can?.assign !== false;
  const onAnswer = answerable ? (n) => setAnswering(n) : null;
  const dirty = steps.length > 0 || commenting;
  // The board reads it when Back leaves the form (groups-taskboard.js).
  if (dirtyRef) dirtyRef.current = dirty;
  // A closed form has nothing to lose.
  useEffect(() => () => { if (dirtyRef) dirtyRef.current = false; }, []);
  const save = async () => {
    if (missing || sending.current) return;
    // An unsent comment (or an open comment edit) keeps the form open, and
    // Save writes nothing until it is sent or cleared: Save does not send it.
    if (commenting) { shellToast('A comment is not sent yet: send it, or clear it, then Save', true); return; }
    if (!steps.length) { onClose(); return; }
    sending.current = true;
    setBusy(true);
    setError('');
    try {
      const out = await runPlan(api, steps);
      onSaved(out);
      if (!out.refused.length) {
        shellToast(`Saved ${task.id}`);
        onClose();
        return;
      }
      setError(`${out.saved.length ? `Saved: ${out.saved.join(', ')}. ` : ''}Not saved: ${out.refused.map((r) => `${r.name} (${r.message})`).join('; ')}`);
      // What landed is no longer a change: the form compares with the task
      // as it is now, and the files attached or removed go from the form, so
      // a second Save does not send them again.
      const now = await api.task?.(task.id).catch(() => null);
      if (now) {
        setOpened(now);
        if (brief.state === 'ready') setBrief({ state: 'ready', body: now.body || '' });
      }
      if (out.saved.some((name) => name.startsWith('attach'))) {
        for (const p of pending) if (p.url) URL.revokeObjectURL(p.url);
        setPending([]);
      }
      setRemoved((before) => before.filter((id) => !out.saved.includes(`remove ${(opened.attachments || []).find((a) => a.id === id)?.name || `#${id}`}`)));
    } finally {
      sending.current = false;
      setBusy(false);
    }
  };
  const row = (label, field, hint = null) => html`<div class="cron-create-row tb-new-row"><span class="cron-create-label">${label}</span>
    <span class="tb-new-field">${field}${hint && html`<span class="ui-hint tb-new-hint">${hint}</span>`}</span></div>`;
  const assignee = task.assignee?.agent_id;
  const parentGone = draft.parent && !epics.some((e) => e.id === draft.parent);
  const meta = [
    `Created ${task.created_by ? `by ${nameOf(task.created_by)} ` : ''}${relTime(task.created_at)}`,
    assignee && `assigned to ${task.assignee?.name || nameOf(`agent:${assignee}`)}${task.assigned_by ? ` by ${nameOf(task.assigned_by)}` : ''}${task.assigned_at ? ` ${relTime(task.assigned_at)}` : ''}`,
    task.closed_at && `${task.status} ${relTime(task.closed_at)}`,
  ].filter(Boolean).join(' · ');
  const reasonRow = draft.where === 'blocked' || draft.where === 'dropped';
  const hint = missing ? `needs ${missing}` : whereHint(draft, task);
  // The Answer dialog is the form's sibling, not inside it: its keys and
  // pastes are its own, not the form's Save or Files.
  const answerDialog = answering !== null && html`<${AnswerWith} task=${task} n=${answering} api=${api}
    askedBy=${nameOf(askerOf(task, answering))} onClose=${() => setAnswering(null)} onAnswered=${() => onSaved({ saved: ['answer'], refused: [] })} />`;
  return html`<${Fragment}><${Dialog} id="taskboard-edit-modal" title=${html`${epic ? 'Edit epic' : wizard ? 'Edit quest' : 'Edit task'} · ${group.shownName || group.name} <span class="tb-id">${task.id}</span><button data-ui-exempt type="button" class="tb-id-copy" title="Copy the id"
      aria-label=${`Copy ${task.id}`} onClick=${async () => {
        if (await writeClipboard(task.id)) shellToast(`Copied ${task.id}`); else shellToast(`Could not copy ${task.id}`, true);
      }}>⧉</button>`}
    onClose=${onClose} busy=${busy} dirty=${dirty} confirmDiscard=${shellConfirmDiscard} initialFocusRef=${titleRef}
    onSubmitHotkey=${() => void save()} resizeKey="tclaude.dash.modalSize.taskboard-edit" error=${error || undefined}
    ...${intake.handlers} dialogClass=${intake.dragOver ? 'files-drag-over' : ''}
    cancel="Cancel"
    footerStart=${html`<span class="ui-hint tb-new-cli"><code>tclaude agent taskboard edit ${task.id} …</code></span>`}
    footer=${html`<${Button} variant="primary" id="taskboard-edit-save" busy=${busy} disabled=${!!missing || busy}
      onClick=${() => void save()}>${busy ? 'Saving…' : 'Save'}<//>`}>
    <div class="modal-meta tb-edit-meta">${meta}</div>
    ${taskQuestions(task).length > 0 && row('Questions', html`<${TaskQuestions} task=${task} nameOf=${nameOf} onAnswer=${onAnswer} />`,
      onAnswer ? 'an answer is sent at once, not with Save' : null)}
    ${row('Title', html`<${Input} class="tb-edit-title" aria-label="Title" required maxLength=${200} value=${draft.title} disabled=${busy}
      domRef=${titleRef} onInput=${(event) => set('title')(event.currentTarget.value)}
      onKeyDown=${(event) => { if (event.key === 'Enter' && !event.isComposing) { event.preventDefault(); void save(); } }} />`)}
    ${row('Brief', html`<${MarkdownEditor} class="tb-edit-body" label="Brief" value=${draft.body} disabled=${busy || brief.state !== 'ready'}
      placeholder=${brief.state === 'loading' ? 'reading the brief…' : 'what the assignee needs to know (Markdown)'} onInput=${set('body')}
      editorRef=${briefEditor} attachments=${briefFiles} onPickFiles=${(picked) => intake.add(picked).map((a) => a.name)}
      modeKey="tclaude.dash.mdEditor.brief" hint=${`${submitKeyLabel()} saves`} />`,
      brief.state === 'failed' ? 'the brief could not be read, so Save leaves it as it is' : null)}
    ${row('Refs', html`<${RefsField} task=${base} draft=${draft} busy=${busy} looked=${looked}
      onChange=${(refs) => set('refs')(refs)} onNewRef=${set('newRef')} />`, refsHint(base, draft))}
    ${epic && row('Progress', html`<span class="tb-epic-progress-row"><${EpicProgress} epic=${task} large=${true} />
      <span class="tb-epic-counts">${progressParts(task.progress).join(' · ')}</span></span>`)}
    ${epic && row('Tasks', html`<${EpicChildren} epic=${task} api=${api} group=${group.name} tasks=${entry?.tasks || []} nameOf=${nameOf}
      onOpen=${onOpenTask} columnLabel=${(slug) => columnLabel(COLUMN_STATE + slug, entry?.columns || [])}
      onChanged=${() => onSaved({ saved: ['tasks'], refused: [] })} />`, 'a change here is made at once, not with Save')}
    ${row('Where it is', html`<${WherePick} value=${draft.where} current=${start.where} wizard=${wizard} disabled=${busy} columns=${entry?.columns || []} epic=${epic}
      canAssign=${canAssign} canClose=${canClose} onChange=${(where) => setDraft((current) => setWhere(current, where))} />
      ${reasonRow && html`<${Textarea} class="tb-edit-reason" rows=${2} aria-label=${draft.where === 'blocked' ? 'Block reason' : 'Drop reason'} maxLength=${400}
        value=${draft.reason} disabled=${busy} placeholder=${draft.where === 'blocked' ? 'why it is blocked (required)' : 'why drop it (optional)'}
        onInput=${(event) => set('reason')(event.currentTarget.value)} />`}`, hint || null)}
    ${!epic && (epics.length > 0 || draft.parent) && row('Epic', html`<${Select} class="tb-edit-epic" aria-label="Epic" value=${draft.parent} disabled=${busy}
        onChange=${(event) => set('parent')(event.currentTarget.value)}>
        <option value="">— in no epic —</option>
        ${epics.map((e) => html`<option key=${e.id} value=${e.id}>${e.title}</option>`)}
        ${parentGone && html`<option value=${draft.parent}>${draft.parent}</option>`}
      <//>`, draft.parent !== start.parent ? (draft.parent ? 'its chip shows the epic on the board' : 'it leaves the epic, and stays on the board') : null)}
    ${!epic && (canAssign || assignee) && row('Assignee', canAssign
      ? html`<${Select} class="tb-edit-assignee" aria-label="Assignee" value=${draft.to} disabled=${busy}
          onChange=${(event) => { const to = event.currentTarget.value; setDraft((current) => setAssignee(current, to)); }}>
          <option value="">— unassigned —</option>
          <option value="operator">${operatorOptionLabel(group)}</option>
          ${members.map((member) => html`<option key=${member.agent_id} value=${member.agent_id}>${member.title || member.agent_id}${member.online ? '' : ' (offline)'}</option>`)}
          ${assignee && assignee !== 'operator' && !members.some((m) => m.agent_id === assignee) && html`<option value=${assignee}>${task.assignee?.name || assignee}</option>`}
        <//>`
      : html`<span>${task.assignee?.operator ? operatorOptionLabel(group) : task.assignee?.name || assignee}</span>`,
      canAssign && draft.to !== start.to ? (draft.to === 'operator' ? 'it waits on the operator, who closes it as done'
        : draft.to ? 'the brief goes to its inbox' : 'its assignee is told') : null)}
    ${!epic && row('After', html`<${DepsPicker} taskId=${task.id} tasks=${tasks} closed=${entry?.closed || []} value=${draft.deps} disabled=${busy} onChange=${set('deps')} />`)}
    ${row('Files', html`<${TaskFiles} api=${api} task=${task} removed=${removed} pending=${pending} intake=${intake} busy=${busy}
      nameOf=${nameOf} onToggle=${(id) => setRemoved((before) => (before.includes(id) ? before.filter((x) => x !== id) : [...before, id]))} />`)}
    ${row('Activity', html`<${TaskActivity} api=${api} task=${task} me=${me} nameOf=${nameOf} onDraft=${setCommenting} onAnswer=${onAnswer} />`,
      'a comment is sent at once, not with Save')}
  <//>${answerDialog}<//>`;
}

// useTrackerLookups reads each awb or Linear ref's issue once when the form
// opens, as `ref-lookup` does (which also keeps it for the board's chips):
// { [refKey]: { title, status } }. Only a human opening the form asks the
// tracker; the board shows what the last lookup found.
function useTrackerLookups(api, groupName, task) {
  const [found, setFound] = useState({});
  useEffect(() => {
    if (!groupName || typeof api?.lookupRef !== 'function') return undefined;
    let live = true;
    for (const ref of refsOf(task)) {
      if (ref.pr || (ref.kind !== 'awb' && ref.kind !== 'linear')) continue;
      const key = refKey(ref);
      api.lookupRef(groupName, ref.id || ref.label || ref.url).then((out) => {
        if (!live || out?.note || !(out?.title || out?.status)) return;
        setFound((current) => ({ ...current, [key]: { title: out.title || '', status: out.status || '' } }));
      }).catch(() => {});
    }
    return () => { live = false; };
  }, [task.id]);
  return found;
}

function refsHint(task, draft) {
  const added = draftRefs(draft).filter((key) => !refsOf(task).map(refKey).includes(key));
  if (!multiRefs(task) && draftRefs(draft).length > 1) return 'this board\'s node keeps one ref per task: only the first is saved';
  if (added.length) return 'a bare awb or Linear id is linked through its tracker on Save';
  return 'the first ref is the task\'s ref for --ref and the List; a PR shows its CI and review as on the agent rows';
}

