// process-draft-handoff.js -- opens a process from another tab (the Groups ›
// Processes quick start, tcl-7p6bgt; the dock's Edit) in the full graph editor: in an overlay
// window over that tab when there is one (openProcessDraft), else in the
// Processes tab's editor, handed over as a just-created template ({id, name, view}) to open, or {create:
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

// openProcessDraft opens a saved process's full editor in an overlay window over
// this tab (tcl-kqiiv0), so the operator stays where they are; without overlay
// windows it hands the draft to the Processes tab as before. {create: true}
// always goes there, to its new-template dialog, and so does a process opened
// while the Processes tab is already showing (the dock's Edit there), which
// opens in that tab's own editor. It answers where it opened: 'overlay' or
// 'processes'.
export async function openProcessDraft(draft, { doc = document } = {}) {
  if (draft?.id && !doc.querySelector('nav [data-tab="processes"].active')) {
    const { openProcessOverlay } = await import('./process-editor-place.js');
    if (openProcessOverlay(draft.id, draft.name || '', { view: draft.view || null, readable: true })) return 'overlay';
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
