import { h } from 'preact';
import htm from 'htm';
import { PRChecksBadge, isGitLabMRURL } from './pr-checks-hover.js';
import { PRReviewBadge } from './pr-review-badge.js';
import { PRLinkHover } from './pr-link-hover.js';

const html = htm.bind(h);

// PR states that have their own badge colour. Anything else — including a
// cache entry written before a state was recorded — falls back to unknown.
const PR_STATE_CLASSES = ['open', 'draft', 'merged', 'closed'];
export const prStateClass = (state) => (PR_STATE_CLASSES.includes(state) ? `pr-state-${state}` : 'pr-state-unknown');

const capitalise = (word) => word[0].toUpperCase() + word.slice(1);

// prRef is the number the way its forge writes it: !N for a GitLab merge
// request, #N for a pull request, '' without one.
export function prRef(pr) {
  if (!pr?.number) return '';
  return `${isGitLabMRURL(pr.url) ? '!' : '#'}${pr.number}`;
}

// prDescription is the link's tooltip and accessible description. A GitHub
// pull request keeps its short text, since its hover card shows the title; a
// merge request has no card, so its title goes in the tooltip. `presented`
// is a PR an agent presented, which may carry the agent's own summary.
export function prDescription(pr, { presented = false } = {}) {
  if (isGitLabMRURL(pr.url)) {
    const ref = prRef(pr);
    const kind = pr.state ? `${capitalise(pr.state)} merge request` : presented ? 'Presented merge request' : 'Merge request';
    return [presented ? pr.summary : '', ref ? `${kind} ${ref}` : kind, pr.title || (ref ? '' : pr.url)]
      .filter(Boolean).join(' — ');
  }
  if (presented) return pr.summary ? `${pr.summary} — ${pr.url}` : `Presented pull request — ${pr.url}`;
  const stateLabel = pr.state ? capitalise(pr.state) : 'Pull request';
  return pr.number ? `${stateLabel} pull request #${pr.number}`
    : pr.summary ? `${pr.summary} — ${pr.url}` : `Presented pull request — ${pr.url}`;
}

// PRChain is one PR's number, CI pill and review icon as a single unbreakable
// run (.pr-chain) — the Groups member table and the group message dialog's
// recipient rows show a member's PRs the same way.
export function PRChain({ pr, className = '', describe = '' }) {
  const stateClass = prStateClass(pr.state);
  const label = prRef(pr) || pr.summary || (isGitLabMRURL(pr.url) ? 'MR' : 'PR');
  const description = describe || prDescription(pr);
  return html`<span class=${`pr-chain${className ? ` ${className}` : ''}`}><${PRLinkHover} url=${pr.url} prNumber=${pr.number} title=${pr.title || ''} prState=${pr.state || ''} className=${`${stateClass}${pr.number ? '' : ' pr-label'}`} label=${label} describe=${description} /><${PRChecksBadge} url=${pr.url} prNumber=${pr.number} summary=${pr.checks} prState=${pr.state} /><${PRReviewBadge} url=${pr.url} prNumber=${pr.number} reviews=${pr.reviews} prState=${pr.state || ''} /></span>`;
}
