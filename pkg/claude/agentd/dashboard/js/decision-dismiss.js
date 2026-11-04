// decision-dismiss.js — the operator withdraws decisions agents put to them
// (notify-human --option) that no longer matter, without answering
// (tcl-wsmjga). POST /api/human-messages/dismiss takes the ids on screen, so
// "Dismiss all" covers exactly the list the operator sees. Each asking agent
// gets one inbox message naming its dismissed decisions.

// postDismiss dismisses the given decision ids and resolves to how many were
// dismissed. A single id that lost a race (already answered or dismissed)
// rejects with the server's reason.
export async function postDismiss(fetchImpl, ids, reason = '') {
  const response = await fetchImpl('/api/human-messages/dismiss', {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids, reason }),
  });
  const text = await response.text?.();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = null; }
  if (!response.ok) throw new Error(body?.error || text || `HTTP ${response.status}`);
  return Number(body?.dismissed) || 0;
}

// dismissedNote is the line a dismissed decision shows in place of its choices.
export function dismissedNote(message) {
  const who = message.dismissed_by === 'agent' ? (message.from_title || 'the agent') : 'you';
  return `Dismissed by ${who}${message.dismiss_reason ? `: ${message.dismiss_reason}` : ''}`;
}
