// modal-human-reply.js — the "reply to a human notification" dialog.
//
// The Messages tab's Human folder lists notifications agents raised via
// `tclaude agent notify-human`. This dialog is the operator's answer: it
// sends the reply straight back to the raising agent's inbox (and nudges
// its terminal) as a message from the operator. It is opened by the
// reader's `reply` button (row-actions.js's msg-reply handler), which
// passes the notification's id + sender attributes.
//
// Online gate: an offline agent has no live session to receive a reply,
// so the dialog surfaces the sender's live/offline state and disables
// Send while it is offline — the "check/indication" the operator asked
// for. It re-derives that state on every snapshot tick (tclaude:snapshot)
// so a dialog left open tracks the agent going offline. The backend
// (POST /api/human-messages/reply) enforces the same gate, so a race
// (offline between the last tick and the click) still can't slip through.

import { $ } from './helpers.js';
import { senderOnline } from './mail.js';
import { toast, refresh, bindBackdropDiscard } from './refresh.js';

// replyCtx holds { id, agent, conv, label, online } while the dialog is
// open, and null when closed. openHumanReplyModal sets it; the snapshot
// listener and submit read it. `sending` guards the in-flight window so a
// snapshot tick can't re-enable Send mid-request.
let replyCtx = null;
let sending = false;

// openHumanReplyModal opens the reply dialog for one notification.
// ctx = { id, agent, conv, label, subject } — id is the human_messages
// row being answered; agent/conv identify the sender (agent_id preferred,
// conv-id fallback) for the live-status check; label is the display name;
// subject is the notification's subject, echoed so the operator knows
// which ping they're answering.
function openHumanReplyModal(ctx) {
  ctx = ctx || {};
  replyCtx = {
    id: Number(ctx.id),
    agent: ctx.agent || '',
    conv: ctx.conv || '',
    label: ctx.label || ctx.conv || '(agent)',
    subject: ctx.subject || '',
  };
  sending = false;
  $('#human-reply-body').value = '';
  $('#human-reply-error').textContent = '';
  // "To" line: the agent's name; the notification's subject (if any) below
  // it so the operator sees which ping this answers.
  const toEl = $('#human-reply-to');
  toEl.textContent = '';
  const name = document.createElement('div');
  name.className = 'human-reply-to-name';
  name.textContent = replyCtx.label;
  toEl.appendChild(name);
  if (replyCtx.subject) {
    const subj = document.createElement('div');
    subj.className = 'human-reply-to-subject';
    subj.textContent = `re: ${replyCtx.subject}`;
    toEl.appendChild(subj);
  }
  syncReplyOnline();
  $('#human-reply-modal').classList.add('show');
  setTimeout(() => $('#human-reply-body').focus(), 0);
}

// syncReplyOnline recomputes the sender's live/offline state from the
// current snapshot and paints the status line + Send button. Called on
// open and on every tclaude:snapshot while the dialog is open, so the
// indication (and the Send gate) track the agent going offline. It never
// touches the button while a send is in flight — `sending` owns the
// disabled state then.
function syncReplyOnline() {
  if (!replyCtx) return;
  const online = senderOnline(replyCtx.agent, replyCtx.conv);
  replyCtx.online = online;
  const statusEl = $('#human-reply-status');
  if (statusEl) {
    statusEl.className = 'human-reply-status ' + (online ? 'online' : 'offline');
    statusEl.textContent = online
      ? '🟢 Online — your reply is delivered to its inbox and its terminal is nudged.'
      : '⚫ Offline — this agent has no live session, so it can’t receive a reply. Replying is disabled until it’s back online.';
  }
  if (!sending) {
    const submit = $('#human-reply-submit');
    if (submit) submit.disabled = !online;
  }
}

function closeHumanReplyModal() {
  $('#human-reply-modal').classList.remove('show');
  replyCtx = null;
  sending = false;
}

// submitReply POSTs the reply to /api/human-messages/reply. It re-checks
// the online gate client-side first (fast, avoids a doomed round-trip),
// but the server is the authority — a 409 with code "offline" (the agent
// went offline mid-dialog) is surfaced on the error line and the status
// line is re-synced so the button reflects reality.
async function submitReply() {
  if (!replyCtx) return;
  const errEl = $('#human-reply-error');
  errEl.textContent = '';
  const bodyText = $('#human-reply-body').value.trim();
  if (!bodyText) {
    errEl.textContent = 'Reply is required — type your answer.';
    return;
  }
  if (!senderOnline(replyCtx.agent, replyCtx.conv)) {
    // Went offline since the dialog opened — refuse and re-sync so the
    // status line + button reflect it.
    syncReplyOnline();
    errEl.textContent = 'The agent is offline — it has no live session to receive a reply.';
    return;
  }
  const submit = $('#human-reply-submit');
  sending = true;
  submit.disabled = true;
  try {
    const r = await fetch('/api/human-messages/reply', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: replyCtx.id, body: bodyText }),
    });
    if (!r.ok) {
      let msg = (await r.text()) || ('HTTP ' + r.status);
      try { const j = JSON.parse(msg); if (j && j.error) msg = j.error; } catch (_) {}
      errEl.textContent = msg;
      sending = false;
      syncReplyOnline();   // re-derive the button state (e.g. after a 409 offline)
      return;
    }
    const resp = await r.json().catch(() => ({}));
    toast(resp.held
      ? `reply queued for ${replyCtx.label} — it’s mid-prompt, will see it when it resumes`
      : `reply sent to ${replyCtx.label}`);
    closeHumanReplyModal();
    refresh();   // the original notification is now marked read
  } catch (e) {
    errEl.textContent = 'Network error: ' + e;
    sending = false;
    syncReplyOnline();
  }
}

function bindHumanReplyModal() {
  const modal = $('#human-reply-modal');
  if (!modal) return;
  $('#human-reply-cancel').addEventListener('click', closeHumanReplyModal);
  $('#human-reply-submit').addEventListener('click', submitReply);
  bindBackdropDiscard('human-reply-modal', closeHumanReplyModal);
  // Ctrl/⌘-Enter submits from the textarea, like a mail composer.
  $('#human-reply-body').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      submitReply();
    }
  });
  // Keep the live-status indication + Send gate current while the dialog
  // sits open: re-derive from each snapshot tick.
  document.addEventListener('tclaude:snapshot', () => {
    if (replyCtx) syncReplyOnline();
  });
}

export { openHumanReplyModal, bindHumanReplyModal };
