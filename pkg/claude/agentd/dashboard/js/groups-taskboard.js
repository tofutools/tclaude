// groups-taskboard.js -- the Task board section of a group card (wizard:
// Quest board), and its counts on the group's summary line
// (docs/taskboard.md, tcl-o1tzq6). Read-only for now: the queue in the
// operator's order, then the Ideas band, each row with its ref chip, who
// holds it, its state, a block reason and its dependencies; under them a
// history line that unfolds the recently closed tasks. Capture, reordering,
// assigning and the lanes view come in later slices.
//
// Data comes from GET /v1/taskboard/context (taskboard-context.js). While
// features.taskboard is off, or for a group this viewer cannot read, nothing
// is drawn. A peer's group draws nothing either: the federation read path is
// a later slice.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { relTime } from './helpers.js';
import { Button } from './ui-components.js';
import { isWizardActive } from './slop.js';
import { FusedSection } from './groups-processes.js';
import {
  actorAgentId, taskRowState, taskboardContext, taskboardCountParts, taskboardCountsText, useTaskboardEntry,
} from './taskboard-context.js';

const html = htm.bind(h);

function localGroup(group) {
  return !group?.fleet_node || !!group.fleet_node.local;
}

// agentLabel names an agent from the snapshot: the group's own members
// first (an assignee is one), then any group's.
function agentLabel(agentId, group, snapshot) {
  if (!agentId) return '';
  const find = (members) => (members || []).find((member) => member.agent_id === agentId);
  const member = find(group?.members)
    || (snapshot?.groups || []).map((g) => find(g.members)).find(Boolean);
  return member?.title || agentId.slice(0, 12);
}

function actorLabel(actor, group, snapshot) {
  if (actor === 'human:operator') return 'operator';
  const id = actorAgentId(actor);
  return id ? agentLabel(id, group, snapshot) : actor || '';
}

const STATE_LABELS = {
  blocked: 'blocked', 'in-progress': 'in progress', queued: 'queued', idea: 'idea', open: 'open', done: 'done', dropped: 'dropped',
};

function RefChip({ task }) {
  const ref = task.ref;
  if (!ref) return html`<span class="tb-ref none">—</span>`;
  const label = ref.label || ref.url || ref.kind;
  const title = `${ref.kind}${ref.url ? ` · ${ref.url}` : ''}`;
  // The store only keeps http(s) URLs; check again before making a link.
  if (ref.url && /^https?:\/\//i.test(ref.url)) {
    return html`<a class="tb-ref" href=${ref.url} target="_blank" rel="noopener noreferrer" title=${title}>🔗 ${label}</a>`;
  }
  return html`<span class="tb-ref" title=${title}>🔗 ${label}</span>`;
}

function DepChips({ task, numbers }) {
  const unmet = new Set(task.unmet || []);
  return (task.depends_on || []).map((dep) => {
    const n = numbers.get(dep);
    const text = n ? `after #${n}` : `after ${dep}`;
    return html`<span key=${dep} class=${`tb-dep${unmet.has(dep) ? ' unmet' : ''}`}
      title=${`${unmet.has(dep) ? 'Waits on' : 'Done:'} ${dep}`}>${text}</span>`;
  });
}

function Who({ task, group, snapshot }) {
  const agent = task.assignee?.agent_id;
  if (!agent) return html`<span class="tb-unassigned">unassigned</span>`;
  const name = agentLabel(agent, group, snapshot);
  const by = actorLabel(task.assigned_by, group, snapshot);
  return html`<span class="tb-who" title=${task.assigned_at ? `assigned ${relTime(task.assigned_at)}` : agent}>
    <span class="tb-avatar" aria-hidden="true">${name.slice(0, 1).toUpperCase()}</span>
    <span class="tb-who-name">${name}</span>
    ${by && html`<span class="tb-by">by ${by}</span>`}
  </span>`;
}

function StatePill({ task, numbers }) {
  const state = taskRowState(task);
  let text = STATE_LABELS[state];
  if (state === 'in-progress' && task.assigned_at) text = `in progress · ${relTime(task.assigned_at).replace(/ ago$/, '')}`;
  if (state === 'queued') {
    const waits = (task.unmet || []).map((dep) => (numbers.get(dep) ? `#${numbers.get(dep)}` : dep));
    if (waits.length) text = `queued · waits on ${waits.join(', ')}`;
  }
  if (state === 'idea' && task.created_at) text = `idea · ${relTime(task.created_at)}`;
  if (state === 'done' || state === 'dropped') {
    const when = relTime(task.closed_at);
    if (when) text = `${text} · ${when}`;
  }
  return html`<span class=${`tb-state tb-state-${state}`}>${text}</span>`;
}

function TaskRow({ task, n, numbers, group, snapshot }) {
  const state = taskRowState(task);
  const rows = [html`<tr key=${task.id} class=${`tb-row tb-row-${state}`} data-task-id=${task.id}>
    <td class="tb-n">${n || ''}</td>
    <td class="tb-title-cell">
      <span class="tb-title">${task.title}</span>
      <span class="tb-id" title=${task.id}>${task.id}</span>
      <${DepChips} task=${task} numbers=${numbers} />
    </td>
    <td><${RefChip} task=${task} /></td>
    <td><${Who} task=${task} group=${group} snapshot=${snapshot} /></td>
    <td><${StatePill} task=${task} numbers=${numbers} /></td>
  </tr>`];
  if (task.status === 'blocked' && task.block_reason) {
    rows.push(html`<tr key=${`${task.id}-reason`} class="tb-reason-row"><td></td>
      <td colspan="4" class="tb-reason">blocked: “${task.block_reason}”</td></tr>`);
  }
  return rows;
}

function HistoryLine({ entry, open, onToggle, wizard }) {
  const counts = entry.counts || {};
  const closed = entry.closed || [];
  const parts = [`${counts.done_this_week || 0} done this week`];
  if (counts.dropped) parts.push(`${counts.dropped} dropped`);
  return html`<div class="tb-history">
    ${parts.join(' · ')}
    ${closed.length > 0 && html` · <${Button} size="sm" variant="link" class="tb-history-toggle" aria-expanded=${open ? 'true' : 'false'}
      onClick=${onToggle}>${open ? '▾' : '▸'} ${wizard ? 'chronicle' : 'history'}<//>`}
    <span class="tb-history-note"> — ${wizard
      ? 'a quest leaves the board when it is fulfilled; it stays in the chronicle'
      : 'a task leaves the list when it is done or dropped; it stays in history'}</span>
  </div>`;
}

// TaskboardRows is the open section's list: the queue, the Ideas band, the
// history line and, unfolded, the recently closed tasks.
export function TaskboardRows({ entry, group, snapshot, wizard = isWizardActive() }) {
  const [history, setHistory] = useState(false);
  const tasks = entry.tasks || [];
  const queue = tasks.filter((task) => task.stage !== 'idea');
  const ideas = tasks.filter((task) => task.stage === 'idea');
  // Numbers run down the whole list, as the operator reads it.
  const numbers = new Map(tasks.map((task, i) => [task.id, i + 1]));
  const more = (entry.counts?.live || 0) - tasks.length;
  const name = group.name;
  return html`<div class="tb-board" data-taskboard=${name}>
    ${tasks.length ? html`<table class="tb-table">
      <thead><tr><th class="tb-n">#</th><th>${wizard ? 'Quest · top is next' : 'Task · operator order, top is next'}</th>
        <th>Ref</th><th>Who</th><th>State</th></tr></thead>
      <tbody>
        ${queue.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${numbers.get(task.id)} numbers=${numbers} group=${group} snapshot=${snapshot} />`)}
        ${ideas.length > 0 && html`<tr class="tb-ideas-divider"><td></td><td colspan="4">${wizard ? 'Visions' : 'Ideas'} · ${ideas.length} · captured, not yet in the queue</td></tr>`}
        ${ideas.map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${numbers.get(task.id)} numbers=${numbers} group=${group} snapshot=${snapshot} />`)}
      </tbody>
    </table>` : html`<div class="gf-empty ui-empty tb-empty">${wizard ? 'No quests on the board.' : 'No tasks on the board.'} Add one with <code>tclaude agent taskboard add ${name} "…"</code>.</div>`}
    ${more > 0 && html`<div class="tb-more muted">+ ${more} more · <code>tclaude agent taskboard ls ${name}</code></div>`}
    <${HistoryLine} entry=${entry} open=${history} wizard=${wizard} onToggle=${() => setHistory(!history)} />
    ${history && (entry.closed || []).length > 0 && html`<table class="tb-table tb-closed"><tbody>
      ${(entry.closed || []).map((task) => html`<${TaskRow} key=${task.id} task=${task} n=${0} numbers=${numbers} group=${group} snapshot=${snapshot} />`)}
    </tbody></table>`}
  </div>`;
}

// TaskboardSection is the group card's Task board: folded, one line of
// counts; it opens on its own when something needs the operator, until they
// fold or open it themselves (remembered per group).
export function TaskboardSection({ group, snapshot, source = taskboardContext }) {
  const entry = useTaskboardEntry(group.name, source);
  if (!entry || !localGroup(group)) return null;
  const wizard = isWizardActive();
  const counts = entry.counts || {};
  return html`<${FusedSection} group=${group.name} section="taskboard" label=${wizard ? 'Quest board' : 'Task board'}
      summary=${html`<${TaskboardCounts} counts=${counts} />`} auto=${(counts.needs_you || 0) > 0}
      attrs=${{ 'data-fused-taskboard': group.name }}>
    <${TaskboardRows} entry=${entry} group=${group} snapshot=${snapshot} wizard=${wizard} />
  <//>`;
}

function TaskboardCounts({ counts }) {
  const parts = taskboardCountParts(counts);
  return html`<span class="tb-counts" title=${taskboardCountsText(counts)}>
    <span class=${`gf-count${counts.live ? '' : ' zero'}`}>${counts.live || 0}</span>
    ${parts.map((part) => html`<span key=${part.key} class=${`tb-count tb-count-${part.key}`}> · ${part.text}</span>`)}
  </span>`;
}

// TaskboardSummaryChip carries the counts on the group's summary line, so a
// folded group still says what is on its board. Nothing when it is empty.
export function TaskboardSummaryChip({ group, source = taskboardContext }) {
  const entry = useTaskboardEntry(group.name, source);
  if (!entry || !localGroup(group) || !entry.counts?.live) return null;
  const parts = taskboardCountParts(entry.counts);
  const wizard = isWizardActive();
  return html`<span class="group-taskboard-chip" data-taskboard-chip=${group.name} title=${`${wizard ? 'Quest board' : 'Task board'}: ${taskboardCountsText(entry.counts)}`}>
    ${wizard ? 'quest board' : 'task board'}: ${parts.length
      ? parts.map((part, i) => html`<span key=${part.key} class=${`tb-count tb-count-${part.key}`}>${i ? ' · ' : ''}${part.text}</span>`)
      : `${entry.counts.live}`}
  </span>`;
}
