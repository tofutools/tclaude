// Pure helpers for projecting unread human.notify rows onto the surfaces that
// draw the yellow attention glyph — Groups member rows and Terminals tabs.
// Human notifications carry both a rotation-stable agent id and the sending
// conversation generation. Prefer the stable id, but retain the conv fallback
// for legacy/pre-identity senders.

// Every attention glyph opens the SAME quick reader, which is mounted once on
// its own body-level host (see mountGroupsIsland). Surfaces ask for it by
// event rather than by import so a tab that is not the Groups tab can raise it
// without owning a reader of its own.
export const OPEN_HUMAN_NOTIFICATION_EVENT = 'tclaude:open-human-notification';

export function openHumanNotificationReader({
  sender, messageID, launcher = null, returnFocus = null, documentRef = globalThis.document,
} = {}) {
  if (!sender || !messageID || !documentRef) return false;
  documentRef.dispatchEvent(new CustomEvent(OPEN_HUMAN_NOTIFICATION_EVENT, {
    detail: { sender, messageId: messageID, launcher, returnFocus },
  }));
  return true;
}

export function unreadHumanMessages(snapshot) {
  return (snapshot?.messages || []).filter((message) => !message?.read);
}

export function humanNotificationMatchesSender(message, sender) {
  const selector = typeof sender === 'string' ? sender : '';
  const agent = String(selector || sender?.agent || sender?.agent_id || '');
  const conv = String(selector || sender?.conv || sender?.conv_id || '');
  return !!(
    (agent && message?.from_agent === agent)
    || (conv && message?.from_conv === conv)
  );
}

export function humanNotificationTargetPage(snapshot, sender, messageID, pageSize) {
  const size = Math.max(1, Number(pageSize) || 1);
  const index = (snapshot?.messages || [])
    .filter((message) => humanNotificationMatchesSender(message, sender))
    .findIndex((message) => message.id === Number(messageID));
  return index < 0 ? 1 : Math.floor(index / size) + 1;
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
