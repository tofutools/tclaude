// phone-board-model.js -- the phone Board as plain data: one group's board,
// one column at a time (Idea · Next · Started · its custom columns · Done),
// each column split into readiness sections (Ready, Waiting on you, Queued,
// Blocked), and what a task card shows. No DOM, no Preact: phone-board.js
// and phone-task-page.js render these.
//
// A task stands in the column the desktop Lanes put it in
// (groups-taskboard-lanes.js laneColumn), except that Blocked is a section
// of its column here rather than a column (a blocked task stays in the
// column it was blocked in), and a task the operator holds stays in its
// stage rather than In progress: Started is the agents' work.

import { COLUMN_STATE } from './taskboard-context.js';
import { openQuestions } from './groups-taskboard-answer.js';
import { readiness } from './groups-taskboard-readiness.js';

export const DONE = 'done';

// The fixed columns, in the order the chips show them; a board's custom
// columns (In review, QA, …) go between Started and Done.
const FIXED_BEFORE = Object.freeze([
  Object.freeze({ key: 'idea', label: 'Idea' }),
  Object.freeze({ key: 'next', label: 'Next' }),
  Object.freeze({ key: 'started', label: 'Started' }),
]);

// phoneColumnOf is the column a task stands in, or null for one the board
// does not show (dropped, or parked in the backlog).
export function phoneColumnOf(task) {
  if (!task || task.status === 'dropped') return null;
  if (task.status === 'done') return DONE;
  if (task.stage === 'backlog') return null;
  if (task.column) return COLUMN_STATE + task.column;
  // Started is an agent at work on it. A task the operator holds (a
  // question, an approval) stays in its stage, where Waiting on you lists it.
  if (task.assignee?.agent_id && !task.assignee.operator && !task.held) return 'started';
  return task.stage === 'idea' ? 'idea' : 'next';
}

// doneTasks are the board's finished tasks, the latest first.
export function doneTasks(entry) {
  return [...(entry?.closed || [])].filter((task) => task.status === 'done')
    .sort((a, b) => String(b.closed_at || '').localeCompare(String(a.closed_at || '')));
}

// tasksIn are the tasks of one column: the live ones in the board's own
// order, or the finished ones for Done.
export function tasksIn(entry, key) {
  if (key === DONE) return doneTasks(entry);
  return (entry?.tasks || []).filter((task) => phoneColumnOf(task) === key);
}

// boardColumns are the chips: every fixed column, the board's custom
// columns, and Done, each with its count.
export function boardColumns(entry) {
  const custom = (entry?.columns || []).map((column) => ({ key: COLUMN_STATE + column.slug, label: column.label, custom: true, cordoned: column.state === 'cordoned' }));
  return [...FIXED_BEFORE, ...custom, { key: DONE, label: 'Done' }]
    .map((column) => ({ ...column, count: tasksIn(entry, column.key).length }));
}

// waitsOnYou reports a task the operator holds, or one with a question to
// the operator still open: the board's "waiting on you".
export function waitsOnYou(task) {
  return !!task?.assignee?.operator || openQuestions(task).length > 0;
}

// The sections of a live column, in reading order. Ready, Waiting on you and
// Blocked are always drawn (an empty one says so); Queued only when it holds
// something.
export const SECTIONS = Object.freeze([
  Object.freeze({ id: 'ready', label: 'Ready', always: true }),
  Object.freeze({ id: 'waiting', label: 'Waiting on you', always: true }),
  Object.freeze({ id: 'queued', label: 'Queued', always: false }),
  Object.freeze({ id: 'blocked', label: 'Blocked', always: true }),
]);

// sectionOf is a live task's section: blocked first (it cannot move until
// unblocked), then what waits on the operator, then queued, else ready.
export function sectionOf(task) {
  const r = readiness(task);
  if (r === 'blocked') return 'blocked';
  if (waitsOnYou(task)) return 'waiting';
  if (r === 'queued') return 'queued';
  return 'ready';
}

// columnSections are one column's sections with their tasks; Done is one
// list, the latest first.
export function columnSections(entry, key) {
  const tasks = tasksIn(entry, key);
  if (key === DONE) return [{ id: DONE, label: 'Done', tasks }];
  return SECTIONS.map((section) => ({ ...section, tasks: tasks.filter((task) => sectionOf(task) === section.id) }))
    .filter((section) => section.always || section.tasks.length);
}

// defaultColumn is the column a board opens on: the first with something
// waiting on the operator, else Next when it has tasks, else the first live
// column with any, else Next.
export function defaultColumn(entry) {
  const columns = boardColumns(entry).filter((column) => column.key !== DONE);
  const waiting = columns.find((column) => tasksIn(entry, column.key).some(waitsOnYou));
  if (waiting) return waiting.key;
  if (columns.find((column) => column.key === 'next')?.count) return 'next';
  return columns.find((column) => column.count)?.key || 'next';
}

// actorName names an actor ("agent:<id>", "human:operator") for a card:
// "you" for the operator, an agent by its name when names knows it.
export function actorName(actor, names = new Map()) {
  const text = String(actor || '');
  if (!text) return '';
  if (text.startsWith('human:')) return 'you';
  if (text.startsWith('agent:')) {
    const id = text.slice('agent:'.length);
    return names.get(id)?.name || names.get(id) || id.slice(0, 12);
  }
  return text;
}

// The tags a card carries: who holds it, its pull request, its other
// references, and its kind when it is not a plain task.
export function cardTags(task, names = new Map()) {
  const tags = [];
  if (task.assignee?.operator) tags.push({ key: 'who', text: 'you' });
  else if (task.assignee?.agent_id) {
    const name = task.assignee.name || names.get(task.assignee.agent_id)?.name || task.assignee.agent_id.slice(0, 12);
    tags.push({ key: 'who', text: `@${name}` });
  }
  for (const ref of task.refs || (task.ref ? [task.ref] : [])) {
    if (ref?.pr?.number) {
      const checks = ref.pr.checks?.state;
      tags.push({ key: `pr-${ref.pr.number}`, text: `PR #${ref.pr.number}`, tone: checks === 'failing' ? 'bad' : checks === 'passing' ? 'ok' : '' });
    } else if (ref?.label) {
      tags.push({ key: `ref-${ref.label}`, text: ref.label });
    }
  }
  if (task.kind && task.kind !== 'task') tags.push({ key: 'kind', text: task.kind });
  return tags;
}

// cardQuestion is the question a card lets the operator answer in place:
// the task's first open one, with its options (the recommended one marked)
// and who asked it. null when none is open.
export function cardQuestion(task, names = new Map()) {
  const q = openQuestions(task)[0];
  if (!q) return null;
  const options = (q.options || []).map((option) => ({ key: option.key, label: option.label || option.key, recommended: option.key === q.recommend }));
  return {
    n: q.n || 0,
    title: q.title || task.title,
    asker: actorName(q.asked_by || task.assigned_by, names),
    options,
    multi: !!q.multi,
    // A question with no options, or one asking for several picks, is
    // answered on the task page.
    inline: options.length > 0 && !q.multi,
  };
}

// cardView is everything a task card shows.
export function cardView(task, names = new Map()) {
  const question = cardQuestion(task, names);
  const section = task.status === 'done' ? DONE : sectionOf(task);
  return {
    id: task.id,
    title: task.title,
    // "you" is what Waiting on you already says; the task's page keeps it.
    tags: cardTags(task, names).filter((tag) => !(tag.key === 'who' && task.assignee?.operator)),
    question,
    accent: section === 'waiting' ? 'q' : '',
    note: section === 'blocked' ? (task.block_reason || 'blocked') : '',
    done: task.status === 'done',
  };
}

// The positions a card can move to among the other cards of its section
// (taskboard move --before|--after): the body for the move route, or null
// where it would not move.
export function positionMoves(peers, task) {
  const list = (peers || []).filter((t) => t.status === 'open' || t.status === 'blocked');
  const i = list.findIndex((t) => t.id === task.id);
  if (i < 0) return { top: null, up: null, down: null };
  const stage = task.stage;
  return {
    top: i > 0 ? { before: list[0].id, stage } : null,
    up: i > 0 ? { before: list[i - 1].id, stage } : null,
    down: i < list.length - 1 ? { after: list[i + 1].id, stage } : null,
  };
}

// columnMoves are the columns a live task can be moved to from the Move
// sheet, each with whether it can go there now and why not. The write each
// one makes is in phone-board.js moveTo.
export function columnMoves(task, entry) {
  const here = phoneColumnOf(task);
  const agent = !!task.assignee?.agent_id && !task.assignee?.operator;
  return boardColumns(entry).map((column) => {
    const move = { key: column.key, label: column.label, current: column.key === here, disabled: false, why: '' };
    if (move.current) return move;
    // Started needs an agent: one holding it to start, or one already at
    // work on it in a custom column (it goes back to In progress).
    if (column.key === 'started' && !(agent && (task.held || task.column))) {
      move.disabled = true;
      move.why = agent ? '' : 'Give it to an agent first';
    }
    if (column.cordoned) {
      move.disabled = true;
      move.why = 'cordoned';
    }
    if (column.key === DONE && task.status !== 'open' && task.status !== 'blocked') move.disabled = true;
    return move;
  });
}

// taskStateLabel is the state a task page's pill says, with its tone: the
// column it stands in, or blocked, waiting on you, done.
export function taskStateLabel(task, entry) {
  if (task.status === 'done') return { text: 'done', tone: 'idle' };
  if (task.status === 'dropped') return { text: 'dropped', tone: 'off' };
  const section = sectionOf(task);
  if (section === 'blocked') return { text: 'blocked', tone: 'err' };
  if (section === 'waiting') return { text: 'waiting on you', tone: 'ask' };
  const key = phoneColumnOf(task);
  const label = boardColumns(entry).find((column) => column.key === key)?.label || key || '';
  return { text: label.toLowerCase(), tone: key === 'started' || String(key).startsWith('col:') ? 'work' : 'idle' };
}

// sectionPeers are the tasks a task moves up and down among: those of its
// section in its column, as the board shows them.
export function sectionPeers(entry, task) {
  const key = phoneColumnOf(task);
  if (!key || key === DONE) return [];
  const id = sectionOf(task);
  return columnSections(entry, key).find((section) => section.id === id)?.tasks || [];
}

// agentOfActor is the agent id an actor names ("agent:<id>"), or ''.
export function agentOfActor(actor) {
  const text = String(actor || '');
  return text.startsWith('agent:') ? text.slice('agent:'.length) : '';
}

// PERSON_ROLE says what an activity entry's actor did, for the task page's
// People: the entries that are someone working on the task.
const PERSON_ROLE = Object.freeze({
  created: 'created it', taken: 'took it', started: 'started it', assigned: 'assigned it',
  blocked: 'blocked it', done: 'closed it', dropped: 'dropped it', reopened: 'reopened it',
  edited: 'edited it', comment: 'commented',
});

// roleOf is what one activity entry says its actor did, '' for none. A
// report says its outcome ("reported done", "reported failed"); a question
// is "asked".
function roleOf(entry) {
  if (entry.kind === 'reported') {
    const outcome = String(entry.detail || '').split(':')[0].trim();
    return outcome ? `reported ${outcome}` : 'reported';
  }
  if (entry.comment?.question) return 'asked';
  return PERSON_ROLE[entry.kind] || '';
}

// taskPeople is who worked on a task, for its phone page: the assignee
// first, then each agent the activity names, the one heard from last
// first. [{ agentId, roles: [...], at }]. Only agents: the operator is
// "you", and is not someone to jump to.
export function taskPeople(task, entries = []) {
  const people = new Map();
  const note = (agentId, role, at = '') => {
    if (!agentId) return;
    const person = people.get(agentId) || { agentId, roles: [], at: '' };
    if (role && !person.roles.includes(role)) person.roles.push(role);
    if (at && at > person.at) person.at = at;
    people.set(agentId, person);
  };
  const assignee = task?.assignee?.agent_id && !task.assignee.operator ? task.assignee.agent_id : '';
  for (const entry of entries || []) {
    const actor = entry.comment ? entry.comment.author : entry.actor;
    note(agentOfActor(actor), roleOf(entry), entry.at || entry.comment?.at || '');
  }
  note(agentOfActor(task?.created_by), 'created it', task?.created_at || '');
  const list = [...people.values()].filter((person) => person.agentId !== assignee)
    .sort((a, b) => (b.at || '').localeCompare(a.at || ''));
  if (assignee) {
    const own = people.get(assignee) || { agentId: assignee, roles: [], at: '' };
    list.unshift({ ...own, roles: [task.status === 'done' ? 'did it' : 'assignee', ...own.roles] });
  }
  return list;
}
