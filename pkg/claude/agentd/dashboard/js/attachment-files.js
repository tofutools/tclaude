// attachment-files.js -- the pure helpers behind a dialog's attachments
// (attachment-intake.js): a file's identity for paste de-duplication, its
// size as people read it, and a check against a dialog's limits. No Preact,
// so model code (agent-spawn-model.js) can use them too.

import { attachmentSize } from './human-attachments.js';
export function attachKey(file) {
  return `${file?.name || ''}|${file?.size || 0}|${file?.type || ''}`;
}

export function formatAttachmentSize(size) {
  return attachmentSize(size);
}

// pastedName names a pasted file that has none (a screenshot): pasted-<n>
// with the extension its type gives.
export function pastedName(file, n) {
  const extension = (file?.type && file.type.split('/')[1]) || 'png';
  return `pasted-${n}.${extension}`;
}

// checkAttachment says why file cannot join a list that already holds
// `count` files under limits ({ maxFiles, maxBytes, accept }), or '' when it
// can. accept(file, name) returns '' or the reason a type is refused.
export function checkAttachment(file, name, count, limits) {
  if (!limits) return '';
  if (limits.maxFiles && count >= limits.maxFiles) return `at most ${limits.maxFiles} files`;
  if (limits.maxBytes && (file?.size || 0) > limits.maxBytes) return `over ${formatAttachmentSize(limits.maxBytes)}`;
  return limits.accept ? limits.accept(file, name) || '' : '';
}
