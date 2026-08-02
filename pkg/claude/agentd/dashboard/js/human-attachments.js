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
  return attachment?.url || `/api/human-messages/${encodeURIComponent(message.id)}/attachment`;
}
