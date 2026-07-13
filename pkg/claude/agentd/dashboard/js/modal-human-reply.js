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
// for. The backend (POST /api/human-messages/reply) enforces the same
// gate authoritatively, so a race (offline between the last poll and the
// click) still can't slip through — and a 409 from it is trusted over the
// client's own view.
//
// Keeping the indicator live: the main dashboard poll continues underneath the
// modal. Its post-commit snapshot event re-runs the gate from the authoritative
// lastSnapshot, so this dialog does not create a second polling loop.

import { $ } from './helpers.js';
import { senderOnline } from './mail-bridge.js';
import { toast, refresh, bindBackdropDiscard } from './refresh.js';

// replyCtx holds { id, agent, conv, label, subject, online } while the dialog is
// open, and null when closed. `sending` guards the in-flight window so a
// snapshot publish cannot re-enable Send mid-request.
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
  startReplySnapshotSync();
  $('#human-reply-modal').classList.add('show');
  setTimeout(() => $('#human-reply-body').focus(), 0);
}

// isReplyTargetOnline reports the sender's liveness from the latest accepted
// dashboard snapshot.
function isReplyTargetOnline() {
  return !!replyCtx && senderOnline(replyCtx.agent, replyCtx.conv);
}

// syncReplyOnline recomputes the sender's live/offline state and paints the
// status line + Send button. Called on open and on every snapshot publish, so the
// indication (and the Send gate) track the agent going offline/online. It
// never touches the button while a send is in flight — `sending` owns the
// disabled state then.
function syncReplyOnline() {
  if (!replyCtx) return;
  const online = isReplyTargetOnline();
  replyCtx.online = online;
  paintReplyStatus(online);
  if (!sending) {
    const submit = $('#human-reply-submit');
    if (submit) submit.disabled = !online;
  }
}

// paintReplyStatus writes the status line + Send-enabled state for a known
// liveness verdict. Split out from syncReplyOnline so the 409 path can
// force the server's "offline" verdict without re-deriving from a snapshot
// that may not have caught the drop yet.
function paintReplyStatus(online) {
  const statusEl = $('#human-reply-status');
  if (statusEl) {
    statusEl.className = 'human-reply-status ' + (online ? 'online' : 'offline');
    statusEl.textContent = online
      ? '🟢 Online — your reply is delivered to its inbox and its terminal is nudged.'
      : '⚫ Offline — this agent has no live session, so it can’t receive a reply. Replying is disabled until it’s back online.';
  }
}

function startReplySnapshotSync() {
  stopReplySnapshotSync();
  document.addEventListener('tclaude:snapshot', syncReplyOnline);
}

function stopReplySnapshotSync() {
  document.removeEventListener('tclaude:snapshot', syncReplyOnline);
}

function closeHumanReplyModal() {
  stopReplySnapshotSync();
  $('#human-reply-modal').classList.remove('show');
  replyCtx = null;
  sending = false;
}

// submitReply POSTs the reply to /api/human-messages/reply. It re-checks
// the online gate client-side first (fast, avoids a doomed round-trip),
// but the server is the authority — a 409 with code "offline" (the agent
// went offline mid-dialog, before our snapshot publish caught it) is trusted: the
// status line + Send button are forced to the offline verdict rather than
// re-derived from the possibly-stale snapshot.
async function submitReply() {
  // Re-entry guard: the ⌘/Ctrl+Enter shortcut calls this directly, bypassing
  // the Send button's disabled state, so key-repeat or a fast double-press
  // could fire concurrent POSTs — each a non-idempotent enqueue + nudge.
  // `sending` is the same flag that disables the button; check it here too.
  if (!replyCtx || sending) return;
  const errEl = $('#human-reply-error');
  errEl.textContent = '';
  const bodyText = $('#human-reply-body').value.trim();
  if (!bodyText) {
    errEl.textContent = 'Reply is required — type your answer.';
    return;
  }
  if (!isReplyTargetOnline()) {
    // Offline per our freshest snapshot — refuse without a round-trip and
    // re-sync so the status line + button reflect it.
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
      const raw = (await r.text()) || ('HTTP ' + r.status);
      let msg = raw, code = '';
      try { const j = JSON.parse(raw); if (j && j.error) msg = j.error; if (j && j.code) code = j.code; } catch (_) {}
      errEl.textContent = msg;
      sending = false;
      if (code === 'offline') {
        // The server is authoritative: it went offline before our snapshot
        // caught it. Force the offline paint + keep Send disabled rather
        // than re-deriving from a snapshot that still shows it online; the
        // next publish reconciles to the same verdict.
        if (replyCtx) replyCtx.online = false;
        paintReplyStatus(false);
      } else {
        syncReplyOnline();   // re-derive the button state for a retry
      }
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
}

export { openHumanReplyModal, bindHumanReplyModal };
