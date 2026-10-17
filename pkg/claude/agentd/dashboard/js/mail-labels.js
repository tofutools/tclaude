// mail-labels.js — how the Messages views name a message's parties and
// preview it. Shared by this node's mail client (mail.js) and the fused
// Messages view (fused-mail.js), so a message reads the same in both.
import { shortAgentId } from './helpers.js';

// HUMAN_SENDER labels mail the human/operator sent to an agent. Those rows
// carry no from_conv, so before the backend flagged them (operator_authored)
// they rendered party-less and read as sender-less system mail — in the
// aggregate "all" folder they were effectively invisible as human mail.
export const HUMAN_SENDER = 'human operator';

// senderLabel names a row's sender, or '' when there is no real sender to
// name. Operator-authored mail is named explicitly. A message with no
// originating conv and no operator marker is an internal system handoff, not
// an agent: it returns '' so the caller drops the party rather than showing
// a misleading "(unknown)". A genuinely unresolvable but real conv still
// falls back to its short conv-id. Shared by the aggregate rows, the
// per-agent rows (via counterparty) and the reader's "From" header, so all
// three name the same message the same way.
export function senderLabel(m) {
  if (m.operator_authored) return wz(HUMAN_SENDER, 'the Archmage');
  return m.from_title || shortAgentId(m.from_agent, m.from_conv);
}

// counterparty returns the name to show in a non-aggregate message-list
// row — the OTHER party relative to the selected mailbox. For a received
// message that's the sender; for a sent one, the recipient.
export function counterparty(m) {
  if (m.direction === 'out') {
    return m.to_title || shortAgentId(m.to_agent, m.to_conv) || '(unknown)';
  }
  return senderLabel(m);
}

// allRecipientLabel names a row's recipient in the aggregate "all" view,
// collapsing a multicast to "first +N".
export function allRecipientLabel(m) {
  if (m.to_title) return m.to_title;
  if (m.to_conv) return shortAgentId(m.to_agent, m.to_conv);
  const rs = m.to_recipients || [];
  if (rs.length) {
    const first = rs[0].title || shortAgentId(rs[0].agent_id, rs[0].conv_id);
    return rs.length > 1 ? `${first} +${rs.length - 1}` : first;
  }
  return '(group)';
}

export function msgPreview(m) {
  if (m.subject) return m.subject;
  const firstNonBlank = (m.body || '').split('\n').find(l => l.trim() !== '');
  return firstNonBlank || '(no subject)';
}

// msgKind classifies a message into one of the wizard-theme "scroll kinds",
// which style the list row + reader per correspondence type. Purely cosmetic
// and theme-agnostic: the data-kind is emitted in EVERY theme, and only the
// body.wizard CSS reacts to it (regular / slop modes ignore it), matching how
// the rest of the wizard re-skin is a pure CSS opt-in over unchanged markup.
//   decree       — the operator/Archmage channel (the Human folder). Regal
//                  arcane slab; the human's word reads as a royal decree.
//   proclamation — a multicast addressed to many. A weathered broadsheet.
//   reply        — a threaded reply (carries a parent). A continued scroll.
//   raven        — the default 1:1 agent-to-agent missive. Plain parchment.
// Precedence: decree > proclamation > reply > raven. Decree covers the whole
// Human folder (agent → human) plus any operator-authored row wherever it
// surfaces, so the human's word reads the same in the "all" firehose and in
// an agent's own folder as it does in the Human folder.
export function messageKind(m, humanFolder = false) {
  if (humanFolder || m.operator_authored) return 'decree';
  if ((m.to_recipients || []).length > 1) return 'proclamation';
  if (m.parent_id) return 'reply';
  return 'raven';
}

// wz picks wizard-voice copy when the 🧙 theme is on, else the plain string.
// The mail panes are repainted on every tclaude:wizard flip (see initMail), so
// the copy tracks the live theme without a data refresh.
export function wz(plain, wizardly) {
  return document.body.classList.contains('wizard') ? wizardly : plain;
}
