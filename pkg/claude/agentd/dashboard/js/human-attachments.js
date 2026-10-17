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

// withPeerAttachments points a peer's notification files at this node's peer
// attachment route (dashboard_peer_attachments.go). The URLs the peer sends
// name its own routes, which on this origin would read this node's files.
export function withPeerAttachments(nodeID, message) {
  const list = messageAttachments(message);
  if (!nodeID || !list.length) return message;
  // A file without an ID has no peer route; it is left out rather than
  // falling back to a local URL.
  const attachments = list.filter((a) => Number(a?.id) > 0).map((a) => ({
    ...a, url: `/api/peer-attachments/${encodeURIComponent(nodeID)}/${encodeURIComponent(message.id)}/${encodeURIComponent(a.id)}`,
  }));
  return { ...message, attachments, attachment: attachments[0] || null };
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

// messageExportHref is the whole-message download the Messages reader offers:
// the notification as Markdown, or — with attachments, when it has any — a
// .zip of message.md plus attachments/. The daemon builds both, so the zip's
// message.md and the plain download never disagree.
export function messageExportHref(messageID, withAttachments) {
  const href = `/api/human-messages/${encodeURIComponent(messageID)}/export`;
  return withAttachments ? `${href}?attachments=1` : href;
}
