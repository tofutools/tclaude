// chat-window-model.js -- a chat's overlay window, by agent. The id is the same
// for one agent every time, so its placement and place in a split come back.

export const CHAT_WINDOW_KIND = 'chat';
export const chatWindowId = (agent) => `chat:${agent}`;
