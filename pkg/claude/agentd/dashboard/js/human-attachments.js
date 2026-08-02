// Shared reading of the files an agent published with a human notification.
//
// A notification carries a LIST of files: `tclaude agent notify-human` sends a
// small set as separate attachments (so an image stays viewable) and only
// packages a large set or a directory as one zip. Snapshots keep the legacy
// single `attachment` field populated with the first file, and each item
// carries its own download `url` plus the daemon's `previewable` and `markdown`
// verdicts on whether the dashboard may render it inline.

export function messageAttachments(message) {
  if (message?.attachments?.length) return message.attachments;
  return message?.attachment ? [message.attachment] : [];
}

export function attachmentHref(message, attachment) {
  return attachmentHrefByID(message.id, attachment);
}

// attachmentHrefByID is the same route for a surface that holds the message id
// rather than the message — the inline viewers, which are handed one file.
export function attachmentHrefByID(messageID, attachment) {
  return attachment?.url || `/api/human-messages/${encodeURIComponent(messageID)}/attachment`;
}

export function attachmentSize(bytes) {
  const size = Number(bytes || 0);
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(size < 10 * 1024 ? 1 : 0)} KiB`;
  return `${(size / (1024 * 1024)).toFixed(size < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
}
