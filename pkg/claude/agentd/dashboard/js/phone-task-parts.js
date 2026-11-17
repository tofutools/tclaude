// phone-task-parts.js -- sections of a task's phone page (phone-task-page.js)
// that bring it level with the desktop's task form (tk_2d942cb6): who worked
// on it, each a tap from their terminal; the brief with the images it names;
// the task's files; and a comment as the desktop's Activity shows it (its
// kind, edited or deleted, its own files and images).
//
// The files and images reuse the desktop's (groups-taskboard-files.js):
// tiles read through the board's api, opening the attachment viewer Mail
// uses on a phone too. A phone does not attach, remove or edit: those stay
// the desktop form's.

import { h } from 'preact';
import htm from 'htm';
import { MarkdownDocument } from './markdown-document.js';
import { StoredFiles, useMarkdownFiles } from './groups-taskboard-files.js';
import { RowMore, Section, StatePill } from './phone-ui.js';
import { relTime } from './helpers.js';
import { rowState } from './phone-shell-model.js';
import { actorName, agentOfActor, taskPeople } from './phone-board-model.js';

const html = htm.bind(h);

// jumpTo goes to an agent: an online one's terminal, else its menu (Power
// on, Message, …), which any agent has. Nothing when the page has neither.
export function jumpTo(actions, agent) {
  if (!agent?.agentId) return;
  if (agent.online && typeof actions?.openTerminal === 'function') actions.openTerminal(agent);
  else if (typeof actions?.agentMenu === 'function') actions.agentMenu(agent);
}

// canJump says whether a tap on agent goes somewhere: an agent the
// snapshot lists (one it does not, retired or another node's, has no
// terminal or menu here).
function canJump(actions, agent) {
  if (!agent?.agentId || agent.unlisted) return false;
  return typeof actions?.agentMenu === 'function' || (agent.online && typeof actions?.openTerminal === 'function');
}

// unlisted is an agent the snapshot does not know, named by its id.
const unlisted = (agentId) => ({ agentId, name: agentId.slice(0, 12), online: false, unlisted: true });

// agentFor is the agent an actor names, as the snapshot knows it, or a bare
// { agentId, name } for one it does not list (another node's, retired).
function agentFor(actor, names) {
  const id = agentOfActor(actor);
  if (!id) return null;
  return names.get(id) || unlisted(id);
}

// PersonName is an actor's name in the Activity: an agent's is a link to it
// (jumpTo), anyone else's (you, a peer node) plain text.
export function PersonName({ actor, shown = '', names, actions }) {
  const agent = agentFor(actor, names);
  // The operator is "you", as everywhere on the phone; anyone else by the
  // name the server gave.
  const text = String(actor || '').startsWith('human:') ? actorName(actor, names) : (shown || actorName(actor, names));
  if (!canJump(actions, agent)) return html`<span class="phone-task-who">${text}</span>`;
  return html`<button data-ui-exempt type="button" class="phone-task-who phone-task-who-link" data-person=${agent.agentId}
    aria-label=${`${text}: ${agent.online ? 'open its terminal' : 'its menu'}`} onClick=${() => jumpTo(actions, agent)}>${text}</button>`;
}

// PeopleSection is who worked on the task (taskPeople): the assignee, then
// the agents its activity names, each with what they did and their state. A
// tap goes to the agent; ⋯ is its menu.
export function PeopleSection({ task, entries, names, actions }) {
  const people = taskPeople(task, entries);
  if (!people.length) return null;
  return html`<${Section} title="People" count=${people.length}>
    <ul class="phone-list phone-task-people">
      ${people.map((person) => {
        const agent = names.get(person.agentId) || unlisted(person.agentId);
        const state = agent.online ? rowState(agent) : 'offline';
        const jump = canJump(actions, agent);
        const roles = person.roles.join(' · ');
        return html`<li key=${person.agentId} class="phone-task-person" data-person=${person.agentId}>
          <button data-ui-exempt type="button" class="phone-row phone-task-person-open" disabled=${!jump}
            aria-label=${`${agent.name}, ${state}${roles ? `, ${roles}` : ''}${jump ? `: ${agent.online ? 'open its terminal' : 'its menu'}` : ''}`}
            onClick=${() => jumpTo(actions, agent)}>
            <span class="phone-row-text"><span class="phone-who-name">${agent.name}</span>
              ${roles && html`<span class="phone-task-person-roles">${roles}${person.at ? ` · ${relTime(person.at)}` : ''}</span>`}</span>
            <${StatePill} state=${state} />
          </button>
          ${jump && typeof actions?.agentMenu === 'function' && html`<${RowMore} label=${`${agent.name}: more`}
            class="phone-task-person-more" onClick=${() => actions.agentMenu(agent)} />`}
        </li>`;
      })}
    </ul>
  <//>`;
}

// TaskBrief is the brief as Markdown, with the task's images it names drawn
// in place, as the desktop form's brief draws them.
export function TaskBrief({ api, task, body }) {
  const files = useMarkdownFiles(api, task, [], body);
  return html`<${MarkdownDocument} source=${body} attachments=${files} className="markdown-document phone-task-md" />`;
}

// FilesSection is the task's files: a tile each (an image's thumbnail),
// opening the attachment viewer, or a PDF or download for a file it cannot
// show.
export function FilesSection({ api, task }) {
  const files = task?.attachments || [];
  if (!files.length) return null;
  return html`<${Section} title="Files" count=${files.length}>
    <${StoredFiles} api=${api} taskId=${task.id} attachments=${files} class="phone-task-files" surface="phone-task" />
  <//>`;
}

// COMMENT_KIND names a typed comment's reason, as the desktop's Activity does.
const COMMENT_KIND = Object.freeze({ block_reason: 'blocked', done_reason: 'done', drop_reason: 'dropped', report_note: 'report' });

// CommentHead is what a comment says beside its author: its kind and
// whether it was edited.
export function CommentHead({ comment }) {
  const kind = COMMENT_KIND[comment.kind];
  return html`${kind && html`<span class="phone-task-comment-kind" data-comment-kind=${comment.kind}>${kind}</span>`}${
    comment.edited_at && !comment.deleted_at && html`<span class="phone-task-when">· edited</span>`}`;
}

// CommentBody is a comment's text with the images it names, then its own
// files; a deleted one says so.
export function CommentBody({ api, taskId, comment }) {
  const deleted = !!comment.deleted_at;
  const files = useMarkdownFiles(api, { id: taskId, attachments: deleted ? [] : comment.attachments }, [], comment.body || '');
  if (deleted) return html`<p class="phone-task-what ui-hint">deleted</p>`;
  return html`<${MarkdownDocument} source=${comment.body || ''} attachments=${files} className="markdown-document phone-task-md" />
    ${comment.attachments?.length > 0 && html`<${StoredFiles} api=${api} taskId=${taskId} attachments=${comment.attachments}
      class="phone-task-files phone-task-comment-files" surface=${`phone-comment-${comment.id}`} />`}`;
}
