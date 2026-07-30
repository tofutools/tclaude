// Pure helpers for projecting unread human.notify rows onto the Groups view.
// Human notifications carry both a rotation-stable agent id and the sending
// conversation generation. Prefer the stable id, but retain the conv fallback
// for legacy/pre-identity senders.

export function unreadHumanMessages(snapshot) {
  return (snapshot?.messages || []).filter((message) => !message?.read);
}

export function humanNotificationMatchesSender(message, sender) {
  const agent = String(sender?.agent || sender?.agent_id || '');
  const conv = String(sender?.conv || sender?.conv_id || '');
  return !!(
    (agent && message?.from_agent === agent)
    || (conv && message?.from_conv === conv)
  );
}

function senderMatchesMember(message, member) {
  return humanNotificationMatchesSender(message, member);
}

export function memberHumanMessages(member, snapshot, unreadOnly = false) {
  const messages = unreadOnly ? unreadHumanMessages(snapshot) : (snapshot?.messages || []);
  return messages.filter((message) => senderMatchesMember(message, member));
}

export function memberUnreadHumanCount(member, snapshot) {
  return memberHumanMessages(member, snapshot, true).length;
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
