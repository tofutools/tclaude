// taskboard-agent-indicator.js -- "☰ n tasks · <title>" under an agent's
// name in a group's member table: the live Task board tasks assigned to it in
// that group (docs/taskboard.md). Kept apart from groups-taskboard.js so the
// member table reads the board without importing the section.

import { h } from 'preact';
import htm from 'htm';
import { taskboardContext, useTaskboardEntry } from './taskboard-context.js';

const html = htm.bind(h);

// agentTasks are the group's live tasks assigned to agentId, in board order.
export function agentTasks(entry, agentId) {
  if (!entry || !agentId) return [];
  return (entry.tasks || []).filter((task) => task.assignee?.agent_id === agentId);
}

export function AgentTaskIndicator({ group, member, source = taskboardContext }) {
  const entry = useTaskboardEntry(group?.name || '', source);
  if (group?.fleet_node && !group.fleet_node.local) return null;
  const tasks = agentTasks(entry, member?.agent_id);
  if (!tasks.length) return null;
  const blocked = tasks.some((task) => task.status === 'blocked');
  const title = tasks.map((task) => `${task.id} · ${task.title}${task.status === 'blocked' ? ' (blocked)' : ''}`).join('\n');
  return html`<div class=${`tb-agent-tasks${blocked ? ' blocked' : ''}`} data-agent-tasks=${member.agent_id} title=${title}>
    ☰ ${tasks.length} task${tasks.length === 1 ? '' : 's'} · <span class="tb-agent-task-title">${tasks[0].title}</span>
  </div>`;
}
