// Pure helpers for projecting unread human.notify rows onto the Groups view.
// Human notifications carry both a rotation-stable agent id and the sending
// conversation generation. Prefer the stable id, but retain the conv fallback
// for legacy/pre-identity senders.

export function unreadHumanMessages(snapshot) {
  return (snapshot?.messages || []).filter((message) => !message?.read);
}

function senderMatchesMember(message, member) {
  const agent = String(member?.agent_id || '');
  const conv = String(member?.conv_id || '');
  return !!(
    (agent && message?.from_agent === agent)
    || (conv && message?.from_conv === conv)
  );
}

export function memberUnreadHumanCount(member, snapshot) {
  return unreadHumanMessages(snapshot)
    .filter((message) => senderMatchesMember(message, member))
    .length;
}

export function groupHasUnreadHumanNotifications(members, snapshot) {
  return (members || []).some((member) => memberUnreadHumanCount(member, snapshot) > 0);
}

export function hasUnreadHumanNotifications(snapshot) {
  return Number(snapshot?.messages_unread || 0) > 0
    || unreadHumanMessages(snapshot).length > 0;
}

export function humanNotificationSenderQuery(member) {
  return String(member?.agent_id || member?.conv_id || '');
}
