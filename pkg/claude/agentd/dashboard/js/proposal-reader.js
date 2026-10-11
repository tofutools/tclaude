import { Fragment, h } from 'preact';
import { useState } from 'preact/hooks';
import htm from 'htm';
import { relTime, shortId } from './helpers.js';
import { dashboardState } from './snapshot-store.js';
import {
  LINK_MODES, decisionFor, initialDraft, itemText, modeText, proposalLines,
  proposalStateText, proposalTurn, withdrawLabel,
} from './proposals-model.js';

const html = htm.bind(h);

function HeaderRow({ label, children }) {
  return html`<div class="mail-hrow"><span class="mail-hlabel">${label}</span><span class="mail-hval">${children}</span></div>`;
}

function ItemStatus({ item }) {
  if (item.status === 'accepted') return html`<span class="proposal-pill ok">accepted</span>`;
  if (item.status === 'declined') return html`<span class="proposal-pill off">declined</span>`;
  return null;
}

// ProposalItemRow is one tickable item. Shares show the member count snapshot
// taken at propose time (never names); a share that includes the member list
// can be narrowed to name + description only. Link edges can change mode,
// which turns the reply into a counter-proposal.
function ProposalItemRow({ item, peer, draft, setDraft, open }) {
  const locked = !open || item.status !== 'pending';
  const ticked = !!draft.ticked[item.id];
  const set = (key, value) => setDraft({ ...draft, [key]: { ...draft[key], [item.id]: value } });
  return html`<label class=${`proposal-item${ticked ? '' : ' off'}`} data-item=${item.id}>
    <input type="checkbox" class="proposal-tick" checked=${ticked} disabled=${locked}
      onChange=${(event) => set('ticked', event.currentTarget.checked)} />
    <span class="grow">${itemText(item, peer)}</span>
    ${item.kind === 'share_visibility' && item.include_members && html`<span class="proposal-members"
      title="The member list (agent names) is shared too. Untick to share only the name, description and member count.">
      <input type="checkbox" class="proposal-members-tick" checked=${!!draft.members[item.id]} disabled=${locked || !ticked}
        onChange=${(event) => set('members', event.currentTarget.checked)} /> member list</span>`}
    ${item.kind === 'group_link' && (open && !locked
      ? html`<select class="proposal-mode" value=${draft.modes[item.id]} disabled=${!ticked}
          onChange=${(event) => set('modes', event.currentTarget.value)}>
          ${LINK_MODES.map((mode) => html`<option key=${mode} value=${mode}>${modeText(mode)}</option>`)}</select>`
      : html`<span class="proposal-mode-text">${modeText(item.mode)}</span>`)}
    <${ItemStatus} item=${item} />
  </label>`;
}

// ShareBack lets the receiver offer some of its own groups in return. Any
// share-back goes back to the proposer as a counter-proposal.
function ShareBack({ view, draft, setDraft }) {
  const offered = new Set((view.items || []).filter((item) => item.kind === 'share_visibility' && item.host === 'us').map((item) => item.group_name));
  const groups = (dashboardState.snapshot.value?.groups || []).filter((group) => !offered.has(group.name));
  if (!groups.length) return null;
  const toggle = (name, on) => setDraft({ ...draft, shareBack: on ? [...draft.shareBack, name] : draft.shareBack.filter((value) => value !== name) });
  return html`<details class="proposal-shareback">
    <summary>Share some of your groups back with ${view.peer_label || view.peer}…</summary>
    ${groups.map((group) => html`<label key=${group.name} class="proposal-item">
      <input type="checkbox" class="proposal-shareback-tick" data-group=${group.name} checked=${draft.shareBack.includes(group.name)}
        onChange=${(event) => toggle(group.name, event.currentTarget.checked)} />
      <span class="grow">${group.name} · ${(group.members || []).length} members</span></label>`)}
    <div class="proposal-hint">They see the name, description and member count; no member list, mail or presence.</div>
  </details>`;
}

// ProposalReader renders a multi-item peer proposal in Messages → Requests.
// While it is this node's turn the operator ticks what to accept, can narrow
// member lists, change link modes or share back (the last two send a
// counter-proposal). The originator can withdraw while the peer decides.
export function ProposalReader({ request, controller }) {
  const view = request.proposal || {};
  const peer = view.peer_label || view.peer || request.origin_peer || 'peer';
  const turn = proposalTurn(view);
  const open = turn === 'us';
  const [draft, setDraft] = useState(() => initialDraft(view));
  const [busy, setBusy] = useState(false);
  const decide = async (body) => {
    if (busy) return;
    setBusy(true);
    try { await controller.decideProposal(request.id, body); } finally { setBusy(false); }
  };
  const primary = open ? decisionFor(view, draft) : null;
  const lines = proposalLines(view);
  const items = view.items || [];
  return html`<${Fragment}>
    <div class="mail-reader-head">
      <div class="mail-subject">🤝 Proposal ${view.ours ? 'to' : 'from'} ${peer} <span class="mail-id">#${shortId(request.id)}</span></div>
      <div class="mail-headers">
        <${HeaderRow} label=${view.ours ? 'To peer' : 'From peer'}>${peer} ${request.origin_peer && html`<span class="mail-cid">${request.origin_peer}</span>`}</${HeaderRow}>
        <${HeaderRow} label="Status">${open
          ? html`<span class="mail-state-pending">waiting for you · auto-declines ${controller.accessCountdown(view.deadline || request.deadline)}</span>`
          : turn === 'them'
            ? html`<span class="mail-state-pending">waiting for ${peer}'s operator · ${controller.accessCountdown(view.deadline || request.deadline)}</span>`
            : html`<span class="proposal-state">${proposalStateText(view)}</span>`}</${HeaderRow}>
        ${view.note && html`<${HeaderRow} label="Note">“${view.note}”</${HeaderRow}>`}
        ${view.revision > 1 && html`<${HeaderRow} label="Revision">${view.revision} (countered)</${HeaderRow}>`}
      </div>
    </div>
    <div class="mail-reader-body access-reader-body">
      <div class="proposal-summary">
        <b>${view.ours ? 'You proposed to' : `${peer} wants to`}:</b>
        <ul>${lines.map((line, index) => html`<li key=${index}>${line}</li>`)}</ul>
      </div>
      <div class="proposal-items">${items.map((item) => html`<${ProposalItemRow} key=${item.id}
        item=${item} peer=${peer} draft=${draft} setDraft=${setDraft} open=${open} />`)}</div>
      ${open && html`<${ShareBack} view=${view} draft=${draft} setDraft=${setDraft} />`}
      ${open && html`<div class="proposal-hint">Each side applies only its own items. Accepting changes nothing else: no broader messaging and no further links.</div>`}
    </div>
    <div class="mail-reader-actions access-reader-actions">
      ${open ? html`<${Fragment}>
          <span class="access-countdown" title="If you don't decide, this proposal is automatically declined.">${controller.accessCountdown(view.deadline || request.deadline)}</span>
          <span class="grow"></span>
          <button class="access-btn deny" disabled=${busy} onClick=${() => decide({ revision: view.revision, decision: 'decline' })}>Decline all</button>
          ${primary && html`<button class=${`access-btn ${primary.body.decision === 'counter' ? 'always' : 'approve'}`} id="proposal-primary"
            disabled=${busy} onClick=${() => decide(primary.body)}>${primary.label}</button>`}</${Fragment}>`
        : view.ours && turn === 'them' ? html`<${Fragment}>
          <span class="grow"></span>
          <button class="access-btn deny" id="proposal-withdraw" disabled=${busy}
            title="Cancel the items the peer has not decided. Already accepted links stay until you remove them in Inter-group links."
            onClick=${() => decide({ revision: view.revision, decision: 'withdraw' })}>${withdrawLabel(view)}</button></${Fragment}>`
          : html`<${Fragment}><span class="proposal-state">${proposalStateText(view)}</span>${request.decided_at && html`<span class="access-decided-at">${relTime(request.decided_at)}</span>`}</${Fragment}>`}
    </div>
  </${Fragment}>`;
}
