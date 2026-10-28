import { h } from 'preact';
import htm from 'htm';
import { PRChecksBadge } from './pr-checks-hover.js';
import { PRReviewBadge } from './pr-review-badge.js';
import { PRLinkHover } from './pr-link-hover.js';

const html = htm.bind(h);

// PR states that have their own badge colour. Anything else — including a
// cache entry written before a state was recorded — falls back to unknown.
const PR_STATE_CLASSES = ['open', 'draft', 'merged', 'closed'];
export const prStateClass = (state) => (PR_STATE_CLASSES.includes(state) ? `pr-state-${state}` : 'pr-state-unknown');

// PRChain is one PR's number, CI pill and review icon as a single unbreakable
// run (.pr-chain) — the Groups member table and the group message dialog's
// recipient rows show a member's PRs the same way.
export function PRChain({ pr, className = '', describe = '' }) {
  const stateClass = prStateClass(pr.state);
  const stateLabel = pr.state ? pr.state[0].toUpperCase() + pr.state.slice(1) : 'Pull request';
  const label = pr.number ? `#${pr.number}` : pr.summary || 'PR';
  const description = describe || (pr.number ? `${stateLabel} pull request #${pr.number}`
    : pr.summary ? `${pr.summary} — ${pr.url}` : `Presented pull request — ${pr.url}`);
  return html`<span class=${`pr-chain${className ? ` ${className}` : ''}`}><${PRLinkHover} url=${pr.url} prNumber=${pr.number} title=${pr.title || ''} prState=${pr.state || ''} className=${`${stateClass}${pr.number ? '' : ' pr-label'}`} label=${label} describe=${description} /><${PRChecksBadge} url=${pr.url} prNumber=${pr.number} summary=${pr.checks} prState=${pr.state || ''} /><${PRReviewBadge} url=${pr.url} prNumber=${pr.number} reviews=${pr.reviews} prState=${pr.state || ''} /></span>`;
}
