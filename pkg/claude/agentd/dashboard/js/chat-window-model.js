// chat-window-model.js -- a chat's overlay window, by agent. The id is the same
// for one agent every time, so its placement and place in a split come back.

export const CHAT_WINDOW_KIND = 'chat';
export const chatWindowId = (agent) => `chat:${agent}`;

// chatTab is an agent's tab, in whichever window holds it.
export const chatTab = (agent, label = '') => ({ id: chatWindowId(agent), kind: CHAT_WINDOW_KIND, agent, label });

// The window that opens for the Steward, and for any agent with nowhere else to
// go. The operator can move tabs out of it, so the Steward may be elsewhere.
export const HOME_WINDOW = 'companion';

// The Steward's window also holds these, which name no agent.
export const START_WINDOW_KIND = 'start';
export const DECIDE_WINDOW_KIND = 'decide';
export const startTab = () => ({ id: 'companion-start', kind: START_WINDOW_KIND, label: 'Steward' });
export const decideTab = () => ({ id: 'companion-decide', kind: DECIDE_WINDOW_KIND, label: 'To decide' });
