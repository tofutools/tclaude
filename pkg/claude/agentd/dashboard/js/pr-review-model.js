// Pure helpers for the PR review pill (see pr-review-badge.js) and the Open
// PRs footer filters. The snapshot's `reviews` object comes from prreviews.go:
// GitHub's reviewDecision plus counts over each reviewer's latest opinionated
// review (human reviewers only; bots are listed but never counted).

// prReviewState collapses a summary into what the pill shows:
//   changes  — someone requested changes (blocks the merge)
//   approved — approved, either by GitHub's verdict or by human approvals
//   required — branch protection still requires a review
//   none     — the reviewer list was fetched and holds nothing
//   ''       — nothing known yet: render no pill at all
export function prReviewState(reviews) {
  if (!reviews) return '';
  const approved = Number(reviews.approved || 0);
  const changes = Number(reviews.changes_requested || 0);
  switch (reviews.decision) {
    case 'changes_requested': return 'changes';
    case 'approved': return 'approved';
    case 'review_required': return changes > 0 ? 'changes' : 'required';
    default:
      if (changes > 0) return 'changes';
      if (approved > 0) return 'approved';
      return reviews.reviewers_known ? 'none' : '';
  }
}

// prReviewLabel is the count after the pill's person icon. The icon's colour
// carries the state, so only approvals and change requests add a number.
export function prReviewLabel(reviews) {
  const state = prReviewState(reviews);
  const approved = Number(reviews?.approved || 0);
  const changes = Number(reviews?.changes_requested || 0);
  switch (state) {
    case 'approved': return approved > 0 ? String(approved) : '';
    case 'changes': return changes > 0 ? String(changes) : '';
    case 'required': return '';
    case 'none': return '';
    default: return '';
  }
}

export function prReviewDecisionText(decision) {
  switch (decision) {
    case 'approved': return 'GitHub: approved';
    case 'changes_requested': return 'GitHub: changes requested';
    case 'review_required': return 'GitHub: review required';
    default: return 'no required reviews';
  }
}

export function prReviewSummaryLine(reviews) {
  const parts = [];
  const approved = Number(reviews?.approved || 0);
  const changes = Number(reviews?.changes_requested || 0);
  if (approved) parts.push(`${approved} approval${approved === 1 ? '' : 's'}`);
  if (changes) parts.push(`${changes} change request${changes === 1 ? '' : 's'}`);
  if (parts.length) return parts.join(' · ');
  return reviews?.reviewers_known ? 'No reviews yet' : 'Reviewer list not fetched yet';
}

// readyToMerge: an open, non-draft PR that is approved with green CI.
export function readyToMerge(pr) {
  return !pr?.draft && pr?.checks?.state === 'passing' && prReviewState(pr?.reviews) === 'approved';
}

export function prReviewStateText(state) {
  switch (state) {
    case 'approved': return 'approved';
    case 'changes_requested': return 'changes requested';
    case 'dismissed': return 'dismissed';
    default: return state || '';
  }
}

export function relativeAge(iso, now = Date.now()) {
  const at = Date.parse(iso || '');
  if (!Number.isFinite(at)) return '';
  const seconds = Math.max(0, Math.round((now - at) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}
