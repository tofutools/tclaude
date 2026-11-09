// groups-taskboard-epics.js -- epics on the Task board (tk_0a821784,
// docs/taskboard.md › Epics). An epic is a task with kind "epic": a
// container of the board's tasks (each task names it as its parent_id), not
// work of its own. Its progress is its children's, counted by the daemon.
//
// On the board:
//   - An open epic is a card in the epic strip above the board (EpicStrip):
//     its name, a progress bar and the counts; it never stands in a stage
//     column. A click opens it in the edit form; its chip filters the board.
//     When every task in it is closed it asks "close epic?" (close_suggested
//     _at), and never closes on its own.
//   - An epic still an idea is a card in Ideas, marked ◆ epic, as any idea.
//   - Each task in an epic carries the epic's chip, in the epic's colour;
//     a click on the chip filters the board to that epic.
//   - Epic ▾ (EpicFilterMenu) shows one epic's tasks (or the tasks in none):
//     the others dim, or hide.
//
// The board's views (List, Lanes) take the entry boardEntry() makes: the
// epics out of the stage columns, the filter applied. The epics and the
// filter reach the cards through EpicBoard.

import { createContext, h } from 'preact';
import { useContext, useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { dashPrefs } from './prefs.js';
import { ViewMenu, ViewMenuHeading, ViewMenuSep } from './view-menu.js';

const html = htm.bind(h);

export const EPIC = 'epic';
// NO_EPIC is the filter that shows the tasks in no epic.
export const NO_EPIC = 'none';

export const isEpic = (task) => task?.kind === EPIC;
const live = (task) => task?.status !== 'done' && task?.status !== 'dropped';

// EPIC_COLOURS is how many colours the epics' chips cycle through
// (dashboard.css .tb-epic-c1 … c8).
export const EPIC_COLOURS = 8;

// epicColour is an epic's colour, 1 to EPIC_COLOURS, from its id: the same
// on every board and every reload.
export function epicColour(id) {
  let hash = 0;
  for (const c of String(id || '')) hash = (hash * 31 + c.codePointAt(0)) >>> 0;
  return (hash % EPIC_COLOURS) + 1;
}

// boardEpics are the board's live epics, in the board's order.
export function boardEpics(entry) {
  return (entry?.tasks || []).filter((task) => isEpic(task) && live(task));
}

// stripEpics are the epics the strip shows: open ones out of Ideas. An
// epic still an idea is a card in Ideas.
export function stripEpics(entry) {
  return boardEpics(entry).filter((epic) => epic.stage !== 'idea');
}

// epicOf is the live epic a task is in, '' for none: a task whose epic
// was closed with its tasks open (done --force) keeps parent_id, but is in
// no epic the board shows. epics is a Set or Map of the live epics' ids.
export function epicOf(task, epics) {
  const parent = task?.parent_id || '';
  return parent && (!epics || epics.has(parent)) ? parent : '';
}

// epicMatches reports whether a task is one the filter shows: '' every
// task, NO_EPIC the tasks in no epic, an epic's id its tasks (and the epic).
export function epicMatches(task, epic, epics = null) {
  if (!epic) return true;
  if (epic === NO_EPIC) return !epicOf(task, epics) && !isEpic(task);
  return epicOf(task, epics) === epic || task.id === epic;
}

// liveEpicIds are the ids of an entry's (or task list's) live epics.
function liveEpicIds(tasks = []) {
  return new Set(tasks.filter((task) => isEpic(task) && live(task)).map((task) => task.id));
}

// boardEntry is the entry the board's views draw: the epics out of the
// stage columns (an idea epic stays, in Ideas), and with the filter set to
// hide, only the tasks it shows. A closed epic leaves the history too.
export function boardEntry(entry, filter = {}) {
  if (!entry) return entry;
  const epic = filter.epic || '';
  const epics = liveEpicIds(entry.tasks);
  const keep = (task) => (!isEpic(task) || (task.stage === 'idea' && live(task))) && (!filter.hide || epicMatches(task, epic, epics));
  const tasks = (entry.tasks || []).filter(keep);
  const closed = (entry.closed || []).filter(keep);
  if (tasks.length === (entry.tasks || []).length && closed.length === (entry.closed || []).length) return entry;
  return { ...entry, tasks, closed };
}

// EpicBoard carries the board's epics and its filter to the cards:
// { epics, byId, filter, setFilter }; null off a board.
export const EpicBoard = createContext(null);

const FILTER_PREF = (group) => `tclaude.dash.groups.taskboard.epic-filter.${group}`;

function readFilter(group) {
  try {
    const stored = JSON.parse(dashPrefs.getItem(FILTER_PREF(group)) || 'null');
    if (stored && typeof stored.epic === 'string') return { epic: stored.epic, hide: !!stored.hide };
  } catch (_) { /* a malformed value reads as none */ }
  return { epic: '', hide: false };
}

// useEpicBoard is a board's EpicBoard value. The filter is this viewer's,
// kept per group; one naming an epic that is gone reads as none.
export function useEpicBoard(group, entry) {
  const [filter, setState] = useState(() => readFilter(group));
  const epics = boardEpics(entry);
  const byId = new Map(epics.map((epic) => [epic.id, epic]));
  const known = !filter.epic || filter.epic === NO_EPIC || byId.has(filter.epic);
  const shown = known ? filter : { epic: '', hide: filter.hide };
  const setFilter = (next) => {
    const value = { ...shown, ...next };
    if (value.epic || value.hide) dashPrefs.setItem(FILTER_PREF(group), JSON.stringify(value));
    else dashPrefs.removeItem(FILTER_PREF(group));
    setState(value);
  };
  return { epics, byId, filter: shown, setFilter };
}

// epicDimmed reports whether a card dims under the filter (dim mode).
export function epicDimmed(board, task) {
  return !!board?.filter?.epic && !board.filter.hide && !epicMatches(task, board.filter.epic, board.byId);
}

// EpicChip is the epic a task is in, in its colour; a click shows only that
// epic's tasks (again: all of them).
export function EpicChip({ task }) {
  const board = useContext(EpicBoard);
  const epic = task?.parent_id && board?.byId.get(task.parent_id);
  if (!epic) return null;
  const on = board.filter.epic === epic.id;
  return html`<button data-ui-exempt type="button" class=${`ui-tag tb-epic-chip tb-epic-c${epicColour(epic.id)}${on ? ' is-on' : ''}`}
    data-epic-chip=${epic.id} title=${on ? `Epic: ${epic.title}. Click to show every task again` : `Epic: ${epic.title}. Click to show only its tasks`}
    onClick=${(event) => { event.stopPropagation(); board.setFilter({ epic: on ? '' : epic.id }); }}>${epic.title}</button>`;
}

// EpicMark is an epic's own mark on its card: ◆ epic, in its colour.
export function EpicMark({ task }) {
  if (!isEpic(task)) return null;
  return html`<span class=${`ui-tag tb-epic-mark tb-epic-c${epicColour(task.id)}`} title="An epic: a container of the board's tasks, not work of its own">◆ epic</span>`;
}

// progressParts are an epic's counts as words, the ones that are not 0.
export function progressParts(p = {}) {
  const active = (p.in_progress || 0) + (p.in_column || 0);
  return [
    `${p.done || 0}/${p.total || 0} done`,
    active && `${active} in progress`,
    p.blocked && `${p.blocked} blocked`,
    p.next && `${p.next} next`,
    p.ideas && `${p.ideas} idea${p.ideas === 1 ? '' : 's'}`,
  ].filter(Boolean);
}

// EpicProgress is an epic's bar: done, in progress (a custom column too)
// and blocked, out of its tasks (dropped ones not counted).
export function EpicProgress({ epic, large = false }) {
  const p = epic?.progress || {};
  const total = p.total || 0;
  const pct = (n) => (total ? `${Math.round(((n || 0) / total) * 1000) / 10}%` : '0%');
  return html`<span class=${`tb-epic-bar${large ? ' tb-epic-bar-large' : ''}`} role="img" aria-label=${progressParts(p).join(', ')}>
    <i class="tb-epic-bar-done" style=${`width:${pct(p.done)}`}></i><i class="tb-epic-bar-active" style=${`width:${pct((p.in_progress || 0) + (p.in_column || 0))}`}></i><i class="tb-epic-bar-blocked" style=${`width:${pct(p.blocked)}`}></i>
  </span>`;
}

const NUDGE_PREF = (id) => `tclaude.dash.groups.taskboard.epic-nudge.${id}`;

// closeNudge reports whether an epic asks "close epic?": every task in it
// closed (the daemon's close_suggested_at) and not put off with Not yet
// since its last task closed.
export function closeNudge(epic) {
  const at = epic?.close_suggested_at;
  return !!at && live(epic) && dashPrefs.getItem(NUDGE_PREF(epic.id)) !== at;
}

// EpicStripCard is one open epic in the strip.
function EpicStripCard({ epic, board, actions, onEdit }) {
  const [, bump] = useState(0);
  const p = epic.progress || {};
  const on = board.filter.epic === epic.id;
  const nudge = closeNudge(epic);
  const busy = actions?.busy === `${epic.id}:close`;
  return html`<div class=${`ui-card tb-epic-card tb-epic-c${epicColour(epic.id)}${on ? ' is-on' : ''}`} data-epic=${epic.id}>
    <div class="tb-epic-card-head">
      ${onEdit ? html`<button data-ui-exempt type="button" class="tb-epic-name" title=${`${epic.title}: opens the epic`} onClick=${() => onEdit(epic.id)}>${epic.title}</button>`
        : html`<span class="tb-epic-name">${epic.title}</span>`}
      <button data-ui-exempt type="button" class="tb-epic-filter" aria-pressed=${on ? 'true' : 'false'}
        title=${on ? 'Show every task again' : 'Show only this epic\'s tasks'} onClick=${() => board.setFilter({ epic: on ? '' : epic.id })}>${on ? '◉' : '○'}</button>
    </div>
    <${EpicProgress} epic=${epic} />
    <div class="tb-epic-counts">${progressParts(p).join(' · ')}</div>
    ${nudge && html`<div class="tb-epic-nudge" data-epic-nudge=${epic.id}><span>All its tasks are closed.</span>
      ${actions && html`<${Button} size="sm" class="tb-epic-close" busy=${busy} disabled=${busy}
        onClick=${() => actions.run(`${epic.id}:close`, 'Close epic', (api) => api.status(epic.id, 'done'))}>Close epic<//>`}
      <${Button} size="sm" variant="ghost" class="tb-epic-later" title="Ask again when another of its tasks changes"
        onClick=${() => { dashPrefs.setItem(NUDGE_PREF(epic.id), epic.close_suggested_at); bump((n) => n + 1); }}>Not yet<//>
    </div>`}
  </div>`;
}

// EpicStrip is the board's open epics, above the stage columns; nothing
// while it has none.
export function EpicStrip({ entry, actions = null, onEdit = null }) {
  const board = useContext(EpicBoard);
  const epics = stripEpics(entry);
  if (!board || !epics.length) return null;
  return html`<div class="tb-epic-strip" role="list" aria-label="Epics">
    <span class="ui-eyebrow tb-epic-strip-label">Epics</span>
    ${epics.map((epic) => html`<div role="listitem" key=${epic.id}><${EpicStripCard} epic=${epic} board=${board} actions=${actions} onEdit=${onEdit} /></div>`)}
  </div>`;
}

// EpicFilterMenu is the section toolbar's Epic ▾: which epic's tasks the
// board shows, and whether the others dim or hide. Nothing while the board
// has no epic.
export function EpicFilterMenu({ group }) {
  const board = useContext(EpicBoard);
  const [open, setOpen] = useState(false);
  if (!board || !board.epics.length) return null;
  const { filter, setFilter } = board;
  const current = filter.epic === NO_EPIC ? 'No epic' : board.byId.get(filter.epic)?.title || '';
  const id = `tb-epic-filter-${String(group).replace(/[^a-zA-Z0-9-]/g, (c) => `_${c.codePointAt(0).toString(16)}_`)}`;
  const pick = (epic) => html`<label class="filter-toggle tb-epic-pick" data-epic-pick=${epic || 'all'}>
    <input type="radio" name=${`${id}-epic`} checked=${filter.epic === epic} onChange=${() => setFilter({ epic })} />
    <span>${epic === '' ? 'All tasks' : epic === NO_EPIC ? 'No epic' : board.byId.get(epic)?.title}${epic && epic !== NO_EPIC
      && html` <span class="tb-epic-pick-n">${board.byId.get(epic)?.progress?.done || 0}/${board.byId.get(epic)?.progress?.total || 0}</span>`}</span>
    ${epic && epic !== NO_EPIC && html`<i class=${`tb-epic-swatch tb-epic-c${epicColour(epic)}`}></i>`}
  </label>`;
  return html`<${ViewMenu} open=${open} onOpenChange=${setOpen} label=${current ? `Epic: ${current} ▾` : 'Epic ▾'} badge=${filter.epic ? 1 : 0}
      buttonClass="tb-epic-filter-btn" ids=${{ button: `${id}-btn`, menu: `${id}-menu`, badge: `${id}-badge` }} title="Show one epic's tasks">
    <${ViewMenuHeading}>Show<//>
    ${pick('')}
    ${board.epics.map((epic) => pick(epic.id))}
    ${pick(NO_EPIC)}
    <${ViewMenuSep} />
    <${ViewMenuHeading}>The other tasks<//>
    <label class="filter-toggle"><input type="radio" name=${`${id}-mode`} checked=${!filter.hide} onChange=${() => setFilter({ hide: false })} /><span>dim</span></label>
    <label class="filter-toggle"><input type="radio" name=${`${id}-mode`} checked=${filter.hide} onChange=${() => setFilter({ hide: true })} /><span>hide</span></label>
  <//>`;
}

const WHERE_WORD = { idea: 'Idea', next: 'Next', progress: 'In progress', blocked: 'Blocked', done: 'Done', dropped: 'Dropped' };

// childWhere is where a task in an epic stands, as a word, and its class.
export function childWhere(task) {
  if (task.status === 'done' || task.status === 'dropped' || task.status === 'blocked') return task.status;
  if (task.column) return 'column';
  if (task.assignee?.agent_id && !task.held) return 'progress';
  return task.stage === 'idea' ? 'idea' : 'next';
}

// epicCandidates are the tasks an epic can take in: the board's live tasks
// in no epic, not epics themselves.
export function epicCandidates(tasks = []) {
  const epics = liveEpicIds(tasks);
  return tasks.filter((task) => !isEpic(task) && !epicOf(task, epics) && live(task));
}

// EpicChildren is the epic's tasks in its edit form: each with where it is
// and who holds it, a click opens it (onOpen), × takes it out of the epic;
// + adds a new task to the epic, or one of the board's tasks in no epic.
// Each is a write at once, not with Save (as a comment is). onChanged reads
// the board again after one.
export function EpicChildren({ epic, api, group, tasks = [], nameOf = (actor) => actor, onOpen = null, onChanged = () => {}, columnLabel = (slug) => slug }) {
  const [read, setRead] = useState({ state: 'loading', children: [] });
  const [title, setTitle] = useState('');
  const [pick, setPick] = useState('');
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const writes = typeof api?.edit === 'function' && api?.can?.write !== false;
  const load = () => {
    if (typeof api?.task !== 'function') { setRead({ state: 'failed', children: [] }); return Promise.resolve(); }
    return api.task(epic.id).then((full) => setRead({ state: 'ready', children: full?.children || [] }))
      .catch(() => setRead((current) => ({ ...current, state: 'failed' })));
  };
  useEffect(() => { void load(); }, [epic.id]);
  const run = async (key, fn) => {
    setBusy(key);
    setError('');
    try {
      await fn();
      await load();
      onChanged();
      return true;
    } catch (cause) {
      setError(cause?.message || String(cause));
      return false;
    } finally {
      setBusy('');
    }
  };
  const children = read.children;
  const candidates = epicCandidates(tasks).filter((task) => !children.some((c) => c.id === task.id));
  const who = (task) => (task.assignee?.operator ? 'operator' : task.assignee?.agent_id ? task.assignee.name || nameOf(`agent:${task.assignee.agent_id}`) : '—');
  const addChild = async () => {
    const text = title.trim();
    if (!text || busy) return;
    if (await run('add', () => api.create(group, { title: text, parent: epic.id }))) setTitle('');
  };
  return html`<div class="tb-epic-children" data-epic-children=${epic.id}>
    ${read.state === 'loading' ? html`<span class="ui-hint">reading its tasks…</span>`
      : read.state === 'failed' && !children.length ? html`<span class="ui-hint">its tasks could not be read</span>`
      : !children.length ? html`<span class="ui-hint">No tasks in it yet.</span>`
      : html`<table class="ui-compact tb-epic-children-table"><thead><tr><th>Task</th><th>Where</th><th>Who</th><th></th></tr></thead><tbody>
        ${children.map((task) => {
          const where = childWhere(task);
          return html`<tr key=${task.id} data-epic-child=${task.id} class=${where === 'done' || where === 'dropped' ? 'tb-epic-child-closed' : ''}>
            <td>${onOpen ? html`<button data-ui-exempt type="button" class="tb-epic-child-open" title=${`${task.title}: opens the task`} onClick=${() => onOpen(task.id)}>${task.title}</button>`
              : html`<span>${task.title}</span>`}${task.archived ? html` <span class="ui-hint">archived</span>` : null}</td>
            <td><span class=${`ui-tag tb-epic-where tb-epic-where-${where}`}>${where === 'column' ? columnLabel(task.column) : WHERE_WORD[where]}</span>
              ${where === 'blocked' && task.block_reason ? html` <span class="ui-hint" title=${task.block_reason}>${task.block_reason}</span>` : null}</td>
            <td>${who(task)}</td>
            <td>${writes && html`<button data-ui-exempt type="button" class="tb-epic-child-rm" disabled=${!!busy} title="Take it out of the epic (the task stays on the board)"
              aria-label=${`Take ${task.title} out of the epic`} onClick=${() => run(`rm:${task.id}`, () => api.edit(task.id, { clearParent: true }))}>×</button>`}</td>
          </tr>`;
        })}
      </tbody></table>`}
    ${writes && html`<div class="tb-epic-add">
      <input class="ui-input ui-sm tb-epic-add-title" aria-label="New task in the epic" placeholder="+ add a task to it (title)" maxLength=${200}
        value=${title} disabled=${!!busy} onInput=${(event) => setTitle(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Enter' && !event.isComposing) { event.preventDefault(); event.stopPropagation(); void addChild(); } }} />
      <${Button} size="sm" class="tb-epic-add-new" disabled=${!title.trim() || !!busy} busy=${busy === 'add'} onClick=${() => void addChild()}>Add<//>
      ${candidates.length > 0 && html`<select class="ui-select ui-sm tb-epic-add-pick" aria-label="A task of the board to add to the epic" value=${pick} disabled=${!!busy}
          onChange=${(event) => setPick(event.currentTarget.value)}>
          <option value="">add an existing task…</option>
          ${candidates.map((task) => html`<option key=${task.id} value=${task.id}>${task.title}</option>`)}
        </select>
        <${Button} size="sm" class="tb-epic-add-existing" disabled=${!pick || !!busy} busy=${busy === 'pick'}
          onClick=${async () => { if (await run('pick', () => api.edit(pick, { parent: epic.id }))) setPick(''); }}>Add<//>`}
    </div>`}
    ${error && html`<div class="ui-error tb-epic-error" role="alert">${error}</div>`}
  </div>`;
}

// EpicGroupRow heads an epic's group in the List grouped by epic: its name
// in its colour, its progress, how many of its tasks are in the queue, a
// fold toggle, and open (its edit form). epic null is No epic.
export function EpicGroupRow({ epic, count, colspan, folded = false, onFold, onOpen = null }) {
  const id = epic?.id || NO_EPIC;
  return html`<tr class=${`tb-epic-group${epic ? ` tb-epic-c${epicColour(epic.id)}` : ' tb-epic-group-none'}`} data-epic-group=${id}><td colspan=${colspan}>
    <span class="tb-epic-group-head">
      <button type="button" data-ui-exempt class="tb-epic-group-fold" aria-expanded=${!folded} title=${folded ? 'Show its tasks' : 'Fold its tasks'}
        onClick=${() => onFold(!folded)}>${folded ? '▸' : '▾'}</button>
      <span class=${`tb-epic-dot${epic ? '' : ' tb-epic-dot-none'}`}></span>
      <span class="tb-epic-group-name">${epic ? epic.title : 'No epic'}</span>
      ${epic && html`<${EpicProgress} epic=${epic} />`}
      <span class="muted">${epic ? progressParts(epic.progress).join(' · ') : `${count} task${count === 1 ? '' : 's'}`}</span>
      ${epic && onOpen && html`<button type="button" data-ui-exempt class="tb-epic-lane-open" data-epic-open=${epic.id} onClick=${() => onOpen(epic.id)}>open ›</button>`}
    </span>
  </td></tr>`;
}
