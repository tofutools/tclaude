import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

// Where an agent arriving from a peer (clone, move or teleport) starts on this
// node. The daemon resolves it in a fixed order — the operator's choice, a
// matching repo from this node's Fleet repos, the sender's path if it exists
// here, the group's default dir, the teleport landing policy — and returns the
// result with every other valid candidate as `landing` on the import preview.
// The sender's cwd and repo URL are hints only and are shown as text.

const REASONS = Object.freeze({
  explicit: 'your choice',
  same_path: 'same path exists here',
  group_default: 'group default dir',
  landing_policy: 'teleport landing policy',
});

// landingReason names why a landing (or a candidate) was picked.
export function landingReason(l) {
  if (!l) return '';
  if (l.reason === 'repo_match') return `matched repo ${l.repo?.name || l.repo?.id || ''}`.trim();
  return REASONS[l.reason] || l.reason || '';
}

// landingResolved says whether the daemon found a directory to start in.
export function landingResolved(l) {
  return !!(l && l.cwd && l.reason && l.reason !== 'none');
}

// landingMissing marks a chosen directory that does not exist here. A repo
// checkout the start will create is not missing.
export function landingMissing(l) {
  return !!(l && l.cwd && l.exists === false && !l.checkout_required);
}

// landingBlocks says whether this landing cannot start an agent as previewed.
export function landingBlocks(l) {
  return !!l && (!landingResolved(l) || landingMissing(l));
}

function checkoutText(l) {
  const repo = l.repo?.name || l.repo?.id || 'the repo';
  return `a new isolated checkout of ${repo}${l.repo?.ref ? ` at ${l.repo.ref}` : ''}, made when the agent starts`;
}

export function candidateText(c) {
  const note = c.checkout_required ? ' (new checkout)' : c.exists === false ? ' (missing)' : '';
  return `${c.cwd} — ${landingReason(c)}${note}`;
}

// landingSentence is the one-line summary the confirm repeats.
export function landingSentence(l) {
  if (!landingResolved(l)) return '';
  return ` in ${l.cwd} (${landingReason(l)}${l.checkout_required ? `; ${checkoutText(l)}` : ''})`;
}

// LandingPicker shows where the agent will start and lets the operator pick
// another candidate or type a path. choice is a candidate ID ('' = automatic),
// cwd a typed path; a typed path wins, as on the CLI (--cwd over --landing).
export function LandingPicker({ landing: l, choice = '', cwd = '', onChoose, onCwd }) {
  const candidates = l.candidates || [];
  return html`<div class="fa-landing" id="fleet-offer-landing">
    <div class="fa-of-row"><span class="fa-k">starts in</span>
      ${landingResolved(l)
        ? html`<span id="fleet-landing-resolved">Will start in <code>${l.cwd}</code> <span class="muted">(${landingReason(l)})</span>${l.checkout_required ? html` <span class="muted">— ${checkoutText(l)}</span>` : ''}</span>`
        : html`<span id="fleet-landing-resolved" class="fa-danger">No directory on this node fits — choose one below or type a path.</span>`}
    </div>
    ${landingMissing(l) && html`<div class="fa-warn" id="fleet-landing-missing" role="alert"><code>${l.cwd}</code> does not exist on this node — create it, or choose another directory.</div>`}
    ${candidates.length > 0 && html`<label class="fa-of-row"><span class="fa-k">choose</span>
      <select id="fleet-landing-choice" value=${choice} onChange=${(e) => onChoose(e.currentTarget.value)}>
        <option value="">automatic (first match)</option>
        ${candidates.map((c) => html`<option key=${c.id} value=${c.id}>${candidateText(c)}</option>`)}
      </select></label>`}
    <label class="fa-of-row"><span class="fa-k">or path</span><input id="fleet-offer-cwd" value=${cwd} placeholder="any directory on this node you own" autocomplete="off" spellcheck="false" onInput=${(e) => onCwd(e.currentTarget.value)} /></label>
    ${(l.source_cwd || l.source_repo) && html`<div class="muted fa-wrap" id="fleet-landing-source">On the sender: ${l.source_cwd ? html`<code>${l.source_cwd}</code>` : ''}${l.source_cwd && l.source_repo ? ' · ' : ''}${l.source_repo ? html`repo <code>${l.source_repo}</code>` : ''} — a hint only; this node decides.</div>`}
  </div>`;
}

// receiverDecides is the sender-side note: the peer picks the directory.
export function receiverDecides(peer) {
  return `${peer} picks the starting directory when it accepts: a matching repo from its own allowlist, the same path if it exists there, or the group's default dir. Paths here are only a hint.`;
}

// sentLanding is the toast tail after a share, move or teleport.
export function sentLanding(r, peer) {
  return ` — ${peer} chooses the directory on accept${r?.source_repo ? ` (repo hint ${r.source_repo})` : ''}`;
}
