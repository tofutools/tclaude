// process-draft-handoff.js -- opens a process from another tab (the Groups
// tab's inline editor, tcl-7p6bgt; the dock's Edit) in the full graph editor: in an overlay
// window over that tab when there is one (openProcessDraft), else in the
// Processes tab's editor, handed over as a stored template ({id, name, view})
// to open. It waits here until the
// Processes island takes it: at once when the island is mounted (the
// tclaude:process-draft event), or when it mounts.

import { idempotentRequestHeaders } from './request-idempotency.js';
import { randomUUID } from './insecure-origin.js';

let pending = null;

// handOffProcessDraft queues the draft and switches to the Processes tab.
export function handOffProcessDraft(draft, { doc = document } = {}) {
  pending = draft;
  doc.querySelector('nav [data-tab="processes"]')?.click();
  doc.dispatchEvent(new CustomEvent('tclaude:process-draft'));
}

// openProcessDraft opens a saved process's full editor in an overlay window over
// this tab (tcl-kqiiv0), so the operator stays where they are; without overlay
// windows it hands the draft to the Processes tab as before. `group` is the
// group it was opened from (the inline editor's ⤢ Full editor), whose
// ← Back inline the overlay offers. A process opened
// while the Processes tab is already showing (the dock's Edit there), which
// opens in that tab's own editor. It answers where it opened: 'overlay' or
// 'processes'.
export async function openProcessDraft(draft, { doc = document } = {}) {
  if (draft?.id && !doc.querySelector('nav [data-tab="processes"].active')) {
    try {
      const { openProcessOverlay } = await import('./process-editor-place.js');
      if (openProcessOverlay(draft.id, draft.name || '', { view: draft.view || null, readable: true, group: draft.group || '' })) return 'overlay';
    } catch (error) {
      // A failed overlay (a stale module after an upgrade) still opens the
      // process, in the Processes tab.
      console.warn('process overlay failed; opening in the Processes tab', error);
    }
  }
  handOffProcessDraft(draft, { doc });
  return 'processes';
}

// takeProcessDraft returns the queued draft, once.
export function takeProcessDraft() {
  const draft = pending;
  pending = null;
  return draft;
}

// UNTITLED_PROCESS names a new process until the operator names it: every
// New process makes one at once, with no name asked first.
export const UNTITLED_PROCESS = 'Untitled process';

// createProcessTemplate stores a blank template named name, pinned to
// `group` (and then out of the library) when one is given, and answers its
// id. The store mints the id.
export async function createProcessTemplate(name, { group = '', fetchImpl = globalThis.fetch, attempt = randomUUID() } = {}) {
  const { blankEditView } = await import('./process-edit-model.js');
  const scaffold = blankEditView(name);
  const template = { ...scaffold.template };
  delete template.id;
  const path = '/v1/process/templates';
  const body = JSON.stringify({ template, edges: scaffold.edges, layout: scaffold.layout, ...(group ? { listed: false, pinGroup: group } : {}) });
  const response = await fetchImpl(path, {
    method: 'POST', headers: idempotentRequestHeaders('POST', path, body, attempt), credentials: 'same-origin', body,
  });
  const result = await response.json().catch(() => null);
  if (!response.ok) throw new Error(result?.message || result?.error || `${response.status} ${response.statusText}`);
  const id = String(result?.id || '').trim();
  if (!id) throw new Error('template creation returned no id');
  return id;
}

// newProcessThere is New process with no group's inline editor to open it
// in (the dock, Collab): an untitled template, the group's when one is given,
// else the library's, opened by `open(id, name)` where the operator is.
export async function newProcessThere({ group = '', open, notify, fetchImpl = globalThis.fetch }) {
  try {
    const id = await createProcessTemplate(UNTITLED_PROCESS, { group, fetchImpl });
    await open(id, UNTITLED_PROCESS);
    return id;
  } catch (cause) {
    notify?.(`Could not make a new process: ${cause?.message || cause}`, true);
    return '';
  }
}
