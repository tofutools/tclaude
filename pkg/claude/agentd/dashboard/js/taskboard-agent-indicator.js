// taskboard-agent-indicator.js -- "☰ n tasks · <title>" under an agent's
// name in a group's member table: the live Task board tasks assigned to it in
// that group (docs/taskboard.md). A linked node's group reads that node's
// board, as the section does (nodeTaskboard).

import { h } from 'preact';
import htm from 'htm';
import { taskboardContext, useTaskboardEntry } from './taskboard-context.js';
import { nodeTaskboard, ownBoardGroup } from './groups-taskboard.js';

const html = htm.bind(h);

// agentTasks are the group's live tasks assigned to agentId, in board order.
export function agentTasks(entry, agentId) {
  if (!entry || !agentId) return [];
  return (entry.tasks || []).filter((task) => task.assignee?.agent_id === agentId);
}

export function AgentTaskIndicator({ group, member, source = taskboardContext, boards = nodeTaskboard }) {
  const node = group?.fleet_node && !group.fleet_node.local ? group.fleet_node.id : '';
  // A linked node's board knows the group by its own name, not name@node.
  const entry = useTaskboardEntry(ownBoardGroup(group)?.name || '', node ? boards(node).source : source);
  const tasks = agentTasks(entry, member?.agent_id);
  if (!tasks.length) return null;
  const blocked = tasks.some((task) => task.status === 'blocked');
  const title = tasks.map((task) => `${task.id} · ${task.title}${task.status === 'blocked' ? ' (blocked)' : ''}`).join('\n');
  return html`<div class=${`tb-agent-tasks${blocked ? ' blocked' : ''}`} data-agent-tasks=${member.agent_id} title=${title}>
    ☰ ${tasks.length} task${tasks.length === 1 ? '' : 's'} · <span class="tb-agent-task-title">${tasks[0].title}</span>
  </div>`;
}
