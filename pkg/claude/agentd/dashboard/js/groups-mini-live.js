// groups-mini-live.js -- the inline editor and an agent working on the same
// process (tk_2dd66d02). The operator edits a process in its group's inline
// editor while an agent (a process scribe, the steward, an overlay chat) saves
// versions of the same template through the CLI. Each newer version the agent
// saves is merged into the open draft with the full editor's three-way merge
// (process-draft-diff.js): its changes land on the canvas as one undoable
// edit, and the operator's own unsaved edits stay. Only when both changed the
// same step does the operator choose, explicitly, whose to keep.
//
// No DOM, no fetch: jstest/groups-mini-live.test.mjs runs this exact file.

import { decideAllDraftChanges, diffTemplateDrafts, mergeDraftDecisions, sameDraftContent } from './process-draft-diff.js';

// LIVE_POLL_MS is how often an open inline editor reads its template's head.
export const LIVE_POLL_MS = 2000;

// The notice names at most this many changes, then "and N more".
const NOTICE_LABELS = 3;

// mergeIncoming merges theirs (a newer saved version) into ours (the open
// draft), base being the saved version ours rests on. choice settles the
// changes both sides made: '' leaves them for the operator (merged is then
// null while there are any), 'theirs' takes the agent's, 'mine' keeps ours.
// It answers { changes, conflicts, merged, clean }: merged is the editor
// content to adopt, and clean says it is exactly theirs, so nothing is left
// to save.
export function mergeIncoming({ base, ours, theirs, choice = '' } = {}) {
  const changes = diffTemplateDrafts({ base, ours, theirs });
  const conflicts = changes.filter((change) => change.conflict);
  if (conflicts.length && choice !== 'theirs' && choice !== 'mine') return { changes, conflicts, merged: null, clean: false };
  let decisions = decideAllDraftChanges(changes, {}, 'accept');
  if (choice === 'theirs') decisions = decideAllDraftChanges(changes, decisions, 'accept', { includeConflicts: true });
  if (choice === 'mine') decisions = decideAllDraftChanges(changes, decisions, 'reject', { includeConflicts: true });
  const merged = mergeDraftDecisions({ ours, theirs, changes, decisions });
  return { changes, conflicts, merged, clean: sameDraftContent(merged, theirs) };
}

// FRESH_MS is how long a step an agent just added or changed stays
// highlighted on the canvas; EDITING_IDLE_MS is how long after its last save
// the editor still says the agent is editing.
export const FRESH_MS = 8000;
export const EDITING_IDLE_MS = 30000;

// freshNodeIds is the steps a merged version added or changed, to highlight.
export function freshNodeIds(changes = []) {
  return new Set(changes.filter((change) => change.kind === 'node' && change.op !== 'removed' && change.nodeId)
    .map((change) => change.nodeId));
}

// stopAgent stops an agent's current turn, the way the phone's Interrupt does
// (Esc in a Claude Code pane). It carries on from there when next asked.
export async function stopAgent(agentId, fetchImpl = globalThis.fetch) {
  const response = await fetchImpl(`/api/agents/${encodeURIComponent(agentId)}/interrupt`, { method: 'POST', credentials: 'same-origin' });
  if (!response.ok) {
    const text = await response.text().catch(() => '');
    let body = {};
    try { body = JSON.parse(text); } catch { /* plain error */ }
    throw new Error(body.error || text || `${response.status}`);
  }
}

// changeList is the notice's short list of what changed.
export function changeList(changes = []) {
  const labels = changes.map((change) => `${change.op === 'added' ? '+ ' : change.op === 'removed' ? '− ' : ''}${change.label}`);
  if (labels.length <= NOTICE_LABELS) return labels.join(', ');
  return `${labels.slice(0, NOTICE_LABELS).join(', ')} and ${labels.length - NOTICE_LABELS} more`;
}

// inlineHelpNote is what an agent brought in from the inline editor is told
// on top of the process scribe brief: the operator is looking at the same
// draft, which saves itself, so a moved sourceHash is usually their own edit.
// It names nothing from the template: the brief's scope payload, fenced as
// untrusted data, already says which one.
export function inlineHelpNote() {
  return [
    'The human has this process open in their group\'s inline editor. It saves their edits as they go and merges each version you save onto their canvas, as one change they can undo.',
    'So a moved sourceHash is usually their own edit: reread, apply your change on top of the newest version, validate, and save. Never revert what they changed; if you disagree with it, say so in chat.',
    'They watch the graph grow as you save, with each new or changed step highlighted, so build it a piece at a time: add one step or one decision, validate, CAS-save, then the next. Do not hold the whole process back for one big save at the end. Say in chat what each save added. If they press Stop, your turn ends; wait for them.',
    'They may describe the process in chat, as a list, a long paste, or an attached file (a spec, a runbook, an approval policy). Read an attached file in full before you start. Model what the engine can run; ask short questions in chat where the description is ambiguous (who decides, what happens on a no, how many rounds) instead of guessing, and keep building the parts that are clear meanwhile. When you finish, list in chat what you could not model and why, naming the missing engine feature (see "Building a process live" in the process-templates skill).',
  ].join('\n\n');
}
