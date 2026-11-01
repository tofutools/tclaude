// message-action.js -- the button an agent can put on a notification
// (notify-human --action, tcl-99t8tr). An agent names an action from a fixed
// list; the daemon resolves its target and writes the text here, and nothing
// happens until the operator clicks. The click asks the daemon what to do, so
// nothing in the page decides where a link goes. The agent gets no answer back.

import { h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { openBoardPane } from './board-overlay-state.js';
import { shellToast } from './shell-state.js';
import { openWebTermPane } from './terminals-tab.js';

const html = htm.bind(h);

const SHELL_WHICH = 'worktree';

const defaultOpeners = {
  terminal: (target) => openWebTermPane(target.agent, `shell · ${target.title || 'agent'}`, SHELL_WHICH, { group: target.group || '' }),
  board: (target) => openBoardPane(target.board, target.title),
  url: (target) => globalThis.open(target.url, '_blank', 'noopener'),
  path: async (target) => (await import('./nav-history.js')).navigateToPath(target.path),
};

// runMessageAction carries out what the daemon resolved. It reports false when
// a terminal could not open here.
export async function runMessageAction(target, openers = defaultOpeners) {
  if (target?.kind === 'terminal') return !!(await openers.terminal(target));
  if (target?.kind === 'board') await openers.board(target);
  else if (target?.kind === 'pr' && /^https?:\/\//.test(target.url || '')) await openers.url(target);
  else if (target?.kind === 'tab' && String(target.path || '').startsWith('/')) return (await openers.path(target)) !== false;
  else return false;
  return true;
}

async function refusal(response) {
  try { return (await response.json())?.error || `HTTP ${response.status}`; } catch { return `HTTP ${response.status}`; }
}

// MessageAction is the button, what it does, and where. A peer's page never
// shows one: the click would open things on the machine the page runs on.
export function MessageAction({ message, remote = false, buttonClass = 'primary', toast = shellToast, onDone, onRefused, openers, fetchImpl = (...args) => globalThis.fetch(...args) }) {
  const [busy, setBusy] = useState(false);
  const action = message?.action;
  if (!action || remote) return null;
  async function run() {
    setBusy(true);
    try {
      const response = await fetchImpl('/api/human-messages/action', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: message.id, dir: action.detail || '' }),
      });
      if (!response.ok) {
        onRefused?.();
        throw new Error(await refusal(response));
      }
      if (!(await runMessageAction(await response.json(), openers))) throw new Error('it could not open here');
      onDone?.();
    } catch (error) {
      toast?.(`That action did not run: ${error?.message || error}`, true);
    } finally {
      setBusy(false);
    }
  }
  const off = action.unavailable || '';
  return html`<div class="message-action" role="group" aria-label="Action from the agent">
    <button type="button" class=${buttonClass} disabled=${busy || !!off} title=${off || 'Runs only when you click it'} onClick=${run}>${action.label}</button>
    ${action.detail && html`<code class="message-action-detail">${action.detail}</code>`}
    ${(off || action.note) && html`<span class="message-action-note">${off || action.note}</span>`}
  </div>`;
}

// ActionCards are an agent's unread actions in chat, in a strip that scrolls so
// every button stays in reach and the terminal keeps its room.
export function ActionCards({ messages, notify, fetchImpl }) {
  if (!messages.length) return null;
  return html`<div class="companion-actions">
    ${messages.map((message) => html`<${ActionCard} key=${message.id} message=${message} fetchImpl=${fetchImpl} notify=${notify} />`)}
  </div>`;
}

// ActionCard is a message's button in chat, for as long as the message is
// unread. It stays on the message in Messages.
export function ActionCard({ message, notify, fetchImpl = (...args) => globalThis.fetch(...args), openers }) {
  const [gone, setGone] = useState(false);
  if (gone) return null;
  async function dismiss() {
    setGone(true);
    try {
      const response = await fetchImpl('/api/human-messages/read', {
        method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: message.id }),
      });
      if (!response.ok) throw new Error(await refusal(response));
    } catch (error) {
      setGone(false);
      notify?.(`That message could not be dismissed: ${error?.message || error}`, true);
    }
  }
  const heading = message.subject || 'Action';
  return html`<div class="companion-decision companion-action" role="group" aria-label=${`Action: ${heading}`}>
    <div class="companion-approval-head"><b>${heading}</b>
      <button type="button" class="companion-action-dismiss" aria-label=${`Dismiss ${heading}`} title="Mark this message read. It stays in Messages." onClick=${dismiss}>×</button></div>
    ${message.body && message.body !== message.subject && html`<p class="companion-decision-body">${message.body}</p>`}
    <${MessageAction} message=${message} buttonClass="companion-primary" toast=${notify} openers=${openers} fetchImpl=${fetchImpl} onDone=${() => setGone(true)} />
  </div>`;
}
