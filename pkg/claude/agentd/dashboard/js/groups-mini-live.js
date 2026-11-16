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
    'Save in small steps, so they see each one land, and explain each change in chat.',
  ].join('\n\n');
}
