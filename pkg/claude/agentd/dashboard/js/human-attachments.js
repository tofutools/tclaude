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

// bodilessNotice is what a reader shows where the body would be when a
// notification deliberately has none: `tclaude agent notify-human --subject …
// --attach …` lets the published file BE the message. Without it the reader is
// a subject over a blank gap and the human cannot tell an intentionally terse
// notification from a broken one. Empty string when there is a body to render,
// or when nothing was published to point at — a bodiless message with no
// attachment is genuinely empty, not this.
export function bodilessNotice(message) {
  if (String(message?.body || '').trim()) return '';
  return messageAttachments(message).length
    ? 'No message — the attached file is the notification.'
    : '';
}

export function attachmentSize(bytes) {
  const size = Number(bytes || 0);
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(size < 10 * 1024 ? 1 : 0)} KiB`;
  return `${(size / (1024 * 1024)).toFixed(size < 10 * 1024 * 1024 ? 1 : 0)} MiB`;
}
