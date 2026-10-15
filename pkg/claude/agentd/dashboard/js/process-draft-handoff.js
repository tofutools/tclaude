// process-draft-handoff.js -- hands a process from another tab (the Groups ›
// Processes quick start, tcl-7p6bgt) to the Processes tab's full graph
// editor: a just-created template ({id, name, view}) to open, or {create:
// true} for the tab's own new-template dialog. It waits here until the
// Processes island takes it: at once when the island is mounted (the
// tclaude:process-draft event), or when it mounts.

let pending = null;

// handOffProcessDraft queues the draft and switches to the Processes tab.
export function handOffProcessDraft(draft, { doc = document } = {}) {
  pending = draft;
  doc.querySelector('nav [data-tab="processes"]')?.click();
  doc.dispatchEvent(new CustomEvent('tclaude:process-draft'));
}

// takeProcessDraft returns the queued draft, once.
export function takeProcessDraft() {
  const draft = pending;
  pending = null;
  return draft;
}
