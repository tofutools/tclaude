// process-template-delete.js -- the confirmation for deleting a process
// template. Both delete affordances use it: Automations › Processes (the row
// button and the drag-to-bin drop) and the dock's process library (⚙ →
// Delete, or Delete in the preview), so the copy cannot drift between them.

// processTemplateDeleteConfirmation is the confirm() request. Deleting drops
// the whole version history for the id.
export function processTemplateDeleteConfirmation({ id, name = '', versionCount = 0, wizard = false } = {}) {
  const label = String(name || '').trim() || id;
  const versions = Number(versionCount) || 0;
  return {
    title: wizard ? 'Unmake this rite?' : 'Delete this process template?',
    body: wizard
      ? `This unmakes ${label} and every one of its ${versions || 'stored'} inscribed version${versions === 1 ? '' : 's'}, along with its authorship trail.`
      : `This permanently deletes ${label} and all ${versions || 'stored'} version${versions === 1 ? '' : 's'} of it, including its authorship history. This cannot be undone.`,
    meta: id,
    okLabel: wizard ? 'Unmake rite' : 'Delete template',
  };
}
