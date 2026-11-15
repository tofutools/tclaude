// phone-board.js -- the phone shell's Board view: one group's task board at
// a time, picked from a chip row, its open tasks by lane. A task opens the
// task's own location (the desktop task dialog for now).

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { companionAgents } from './companion-model.js';
import { boardGroups, boardLanes, taskPath } from './phone-shell-model.js';
import { ChipRow, Section } from './phone-ui.js';

const html = htm.bind(h);

export function BoardView({ source, state, actions }) {
  const [picked, setPicked] = useState('');
  useEffect(() => source.attach(), [source]);
  const status = source.status.value;
  if (status === 'unavailable') return html`<p class="ui-empty phone-empty">Task boards are turned off.</p>`;
  const context = source.context.value;
  if (!context) return html`<p class="ui-empty phone-empty">${status === 'error' ? `Could not read the boards: ${source.error.value}` : 'Loading…'}</p>`;
  const groups = boardGroups(context);
  if (groups.length === 0) return html`<p class="ui-empty phone-empty">No open tasks on any board.</p>`;
  const group = groups.find((g) => g.group === picked)?.group || groups[0].group;
  const entry = context.groups.find((g) => g.group === group);
  const agents = companionAgents(state.snapshot.value);
  return html`<div class="phone-board">
    <${ChipRow} label="Group" value=${group} onSelect=${setPicked}
      items=${groups.map((g) => ({ key: g.group, label: g.group, count: g.live, badge: g.waiting }))} />
    ${boardLanes(entry).map((lane) => html`<${Section} key=${lane.id} title=${lane.label} count=${lane.tasks.length}>
      <ul class="phone-list">
        ${lane.tasks.map((task) => html`<li key=${task.id}>
          <button data-ui-exempt type="button" class="phone-task" onClick=${() => actions.navigate(taskPath(task, group))}>
            <span class="phone-task-title">${task.title}</span>
            ${task.assignee?.agent_id && !task.assignee.operator && html`<span class="phone-task-who">@${agents.get(task.assignee.agent_id)?.name || task.assignee.agent_id.slice(0, 12)}</span>`}
            ${task.assignee?.operator && html`<span class="phone-task-who">you</span>`}
          </button>
        </li>`)}
      </ul>
    <//>`)}
  </div>`;
}
