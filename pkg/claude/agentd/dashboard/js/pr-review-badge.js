import { h } from 'preact';
import { useEffect, useId, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { safeCheckURL, touchOnly, usePanelPlacement } from './pr-checks-hover.js';
import {
  prReviewDecisionText, prReviewGlyph, prReviewLabel, prReviewState, prReviewStateText,
  prReviewSummaryLine, relativeAge,
} from './pr-review-model.js';

const html = htm.bind(h);

// The review pill that follows the CI pill on a PR badge, plus its hover
// panel. It shares the CI panel's placement, hover-root and bridge mechanics
// (and its .ci-panel styling) so both popovers behave the same.
//
// Unlike the CI panel this one never fetches: the reviewer list is small and
// already rides the snapshot, and is refreshed by GitHub reads the daemon
// makes anyway (prreviews.go). Hovering costs no GitHub request.

function reviewerGlyph(state) {
  switch (state) {
    case 'approved': return '✓';
    case 'changes_requested': return '±';
    default: return '⊘';
  }
}

function reviewerIconClass(state) {
  switch (state) {
    case 'approved': return 'ci-icon-pass';
    case 'changes_requested': return 'ci-icon-fail';
    default: return 'ci-icon-skipped';
  }
}

function ReviewerRow({ reviewer, prURL, now }) {
  const href = safeCheckURL(reviewer.url) || prURL;
  const meta = [prReviewStateText(reviewer.state), reviewer.bot ? 'bot · not counted' : ''].filter(Boolean).join(' · ');
  const body = html`
    <span class="ci-check-body">
      <span class="ci-check-name">${reviewer.login}</span>
      <span class="ci-check-meta">${meta}</span>
    </span>
  `;
  return html`
    <li class="ci-check">
      <span class=${`ci-check-icon ${reviewerIconClass(reviewer.state)}`} aria-hidden="true">${reviewerGlyph(reviewer.state)}</span>
      ${href ? html`<a class="ci-check-link" href=${href} target="_blank" rel="noopener noreferrer">${body}</a>` : body}
      <span class="ci-check-time">${relativeAge(reviewer.submitted_at, now)}</span>
    </li>
  `;
}

function PRReviewPanel({ prURL, prNumber, reviews, panelID, headingID, panelRef, placement }) {
  const now = Date.now();
  const reviewers = reviews.reviewers || [];
  const style = placement && !placement.hidden
    ? `top:${placement.top}px;left:${placement.left}px;max-height:${placement.maxHeight}px;` +
      (placement.placement === 'above' ? 'transform:translateY(-100%)' : '')
    : 'visibility:hidden';
  const updated = relativeAge(reviews.reviewers_fetched_at, now);
  return html`
    <div ref=${panelRef} class="ci-panel rv-panel" style=${style}
      id=${panelID} role="dialog" aria-labelledby=${headingID}>
      <div class="ci-panel-heading" id=${headingID}>
        <a href=${prURL} target="_blank" rel="noopener noreferrer">
          <span class="theme-copy-regular">${prNumber ? `#${prNumber} · reviews` : 'Pull request reviews'}</span>
          <span class="theme-copy-wizard">${prNumber ? `#${prNumber} · judgement of the council` : 'Judgement of the council'}</span>
        </a>
      </div>
      <div class="ci-panel-summary">${prReviewSummaryLine(reviews)} · ${prReviewDecisionText(reviews.decision)}</div>
      ${reviewers.length ? html`
        <ul class="ci-checks">
          ${reviewers.map((reviewer) => html`
            <${ReviewerRow} key=${reviewer.login} reviewer=${reviewer} prURL=${prURL} now=${now} />
          `)}
        </ul>
      ` : html`<p class="ci-panel-empty">${reviews.reviewers_known
        ? 'Nobody has approved or requested changes yet.'
        : 'The reviewer list arrives with the next GitHub poll.'}</p>`}
      <p class="ci-panel-note">
        <a href=${prURL} target="_blank" rel="noopener noreferrer">Conversation ↗</a>
        <span class="rv-note-sep"> · </span>
        <a href=${`${prURL}/files`} target="_blank" rel="noopener noreferrer">Files changed ↗</a>
        ${updated ? html`<span class="rv-note-age">reviewers updated ${updated}</span>` : null}
      </p>
    </div>
  `;
}

// PRReviewBadge renders nothing until something is known about the PR's
// reviews, and nothing for a draft or a closed/merged PR — none of those is
// waiting on a review decision.
export function PRReviewBadge({ url, prNumber, reviews, prState = '' }) {
  const rootRef = useRef(null);
  const panelRef = useRef(null);
  const instanceID = useId();
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [tapped, setTapped] = useState(false);
  const open = hovered || focused || tapped;
  const { placement } = usePanelPlacement(rootRef, panelRef, open);
  const close = () => { setHovered(false); setFocused(false); setTapped(false); };

  useEffect(() => {
    if (!tapped) return undefined;
    const onPointerDown = (event) => {
      if (!rootRef.current?.contains(event.target)) close();
    };
    document.addEventListener('pointerdown', onPointerDown);
    return () => document.removeEventListener('pointerdown', onPointerDown);
  }, [tapped]);

  const prURL = safeCheckURL(url).replace(/\/+$/, '');
  const state = prReviewState(reviews);
  if (!prURL || !state || ['draft', 'merged', 'closed'].includes(prState)) return null;
  const panelID = `rv-panel-${instanceID.replace(/[^a-zA-Z0-9_-]+/g, '-')}`;
  const headingID = `${panelID}-heading`;
  const glyph = prReviewGlyph(state);
  const label = prReviewLabel(reviews);
  const who = prNumber ? `#${prNumber}` : 'this pull request';

  return html`
    <span
      ref=${rootRef}
      class=${`ci-hover rv-hover${open ? ' is-open' : ''}`}
      onMouseEnter=${() => setHovered(true)}
      onMouseLeave=${() => setHovered(false)}
      onFocusIn=${() => setFocused(true)}
      onFocusOut=${(event) => { if (!rootRef.current?.contains(event.relatedTarget)) setFocused(false); }}
    >
      <a
        class=${`ci-badge rv-badge rv-${state}`}
        href=${prURL}
        target="_blank"
        rel="noopener noreferrer"
        draggable=${false}
        aria-expanded=${open ? 'true' : 'false'}
        aria-controls=${panelID}
        aria-label=${`Reviews for ${who} — ${prReviewSummaryLine(reviews)}. Opens the pull request on GitHub.`}
        onClick=${(event) => {
          if (!touchOnly()) return;
          event.preventDefault();
          if (tapped) close(); else setTapped(true);
        }}
        onKeyDown=${(event) => {
          if (event.key !== 'Escape') return;
          event.preventDefault();
          event.stopPropagation();
          close();
        }}
      >
        ${glyph ? html`<span class="ci-glyph" aria-hidden="true">${glyph}</span>` : null}
        ${label ? html`<span class="ci-count">${label}</span>` : null}
      </a>
      ${open ? html`
        <${PRReviewPanel} prURL=${prURL} prNumber=${prNumber} reviews=${reviews}
          panelID=${panelID} headingID=${headingID} panelRef=${panelRef} placement=${placement} />
        ${placement && !placement.hidden ? html`
          <span class="ci-bridge" aria-hidden="true" style=${
            `top:${placement.bridge.top}px;left:${placement.bridge.left}px;` +
            `width:${placement.bridge.width}px;height:${placement.bridge.height}px`
          }></span>
        ` : null}
      ` : null}
    </span>
  `;
}
