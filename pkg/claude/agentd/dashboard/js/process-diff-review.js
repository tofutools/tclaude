// process-diff-review.js -- the proposal review bar of the process editor
// (E4, tcl-v7byug). While a review is open the canvas shows another author's
// saved version (typically the docked scribe's) as an overlay on the live
// draft; this bar counts the changes, walks them, and settles each one. The
// controller (process-editor.js) owns the decisions and the final merge.

import { h } from 'preact';
import htm from 'htm';

const html = htm.bind(h);

const OP_GLYPH = { added: '+', changed: '~', removed: '−' };

function shortHash(hash) { return hash ? hash.slice(0, 8) : ''; }

export function DiffReviewBar({ controller, view }) {
  const diff = view.diff;
  if (!diff?.open) return null;
  const { counts, current } = diff;
  const decided = counts.accepted + counts.rejected;
  return html`<div class="process-diff-review" role="region" aria-label="Proposal review" data-diff-review>
    <div class="pdr-summary">
      <strong class="pdr-title">Proposal ${shortHash(diff.semanticHash)}</strong>
      <span class="pdr-chip is-added" title="added">+${counts.added} added</span>
      <span class="pdr-chip is-changed" title="changed">~${counts.changed} changed</span>
      <span class="pdr-chip is-removed" title="removed">−${counts.removed} removed</span>
      ${counts.conflicts > 0 && html`<span class="pdr-chip is-conflict" title="Also changed in your unsaved draft">⚠ ${counts.conflicts} conflict${counts.conflicts === 1 ? '' : 's'}</span>`}
      <span class="pdr-progress">${decided} of ${counts.total} decided</span>
      <span class="spacer"></span>
      <button type="button" class="process-action" data-diff-action="reject-all" onClick=${() => controller.decideAllDiff('reject')}>reject all</button>
      <button type="button" class="process-action" data-diff-action="step" title="Next undecided change (J)" onClick=${() => controller.stepDiff(1)}>step through ↓</button>
      <button type="button" class="process-action primary" data-diff-action="accept-all" onClick=${() => controller.decideAllDiff('accept')}>✓ accept all</button>
      <button type="button" class="pdr-close" aria-label="Close review without merging" title="Close the review; nothing is merged (Esc)"
        onClick=${() => controller.closeDiffReview()}>✕</button>
    </div>
    ${current && html`<div class=${`pdr-current is-${current.op} is-${current.state}${current.conflict ? ' is-conflict' : ''}`} data-diff-current=${current.id}>
      <button type="button" class="pdr-step" aria-label="Previous change (K)" onClick=${() => controller.stepDiff(-1)}>‹</button>
      <span class="pdr-index">${diff.index + 1}/${counts.total}</span>
      <span class="pdr-op" aria-hidden="true">${OP_GLYPH[current.op] || '~'}</span>
      <span class="pdr-label">${current.kind === 'template' ? 'template settings' : current.label}</span>
      ${current.fields.length > 0 && html`<span class="pdr-fields">${current.fields.join(', ')}</span>`}
      ${current.previousTo && html`<span class="pdr-fields">was → ${current.previousTo}</span>`}
      ${current.conflict && html`<span class="pdr-conflict" title="You changed this item in your unsaved draft too">conflicts with your edit</span>`}
      ${current.linked > 0 && html`<span class="pdr-linked" title="Accepting this also accepts the changes it depends on">+${current.linked} linked</span>`}
      <span class="spacer"></span>
      ${current.state === 'pending' ? html`
        <button type="button" class="process-action process-action-danger" data-diff-action="reject" title="Reject (R)" onClick=${() => controller.decideDiff(current.id, 'reject')}>reject</button>
        <button type="button" class="process-action primary" data-diff-action="accept" title="Accept (A)" onClick=${() => controller.decideDiff(current.id, 'accept')}>accept</button>`
      : html`<span class=${`pdr-verdict is-${current.state}`}>${current.state}</span>
        <button type="button" class="process-action" data-diff-action="undo" onClick=${() => controller.undoDiffDecision(current.id)}>undo</button>`}
      <button type="button" class="pdr-step" aria-label="Next change (J)" onClick=${() => controller.stepDiff(1)}>›</button>
    </div>`}
  </div>`;
}
