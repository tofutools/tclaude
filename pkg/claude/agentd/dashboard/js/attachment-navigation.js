// Only files with an inline media viewer participate; keep message order.
export function previewAttachments(attachments) {
  return attachments.filter((attachment) => attachment?.previewable || attachment?.video);
}

export function attachmentNavigationStep(event, video) {
  if (event.defaultPrevented || event.isComposing || event.ctrlKey || event.metaKey || event.altKey) return 0;
  if (event.target?.closest?.('input, textarea, select, [contenteditable]:not([contenteditable="false"])')) return 0;
  // Plain arrows belong to native video seeking. Shift+arrows always browse.
  if (video && !event.shiftKey) return 0;
  if (event.key === 'ArrowLeft') return -1;
  if (event.key === 'ArrowRight') return 1;
  return 0;
}
