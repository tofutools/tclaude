import { h } from 'preact';
import { useEffect, useId, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { safeCheckURL, touchOnly, usePanelPlacement } from './pr-checks-hover.js';
import { relativeAge } from './pr-review-model.js';

const html = htm.bind(h);

// The PR link itself (#1234 in the Groups tab's Branch column), plus a hover
// card answering "what is this PR": title, state, author, branches, diffstat
// and the opening of its description. The CI pill and the review pill that
// follow the link keep their own panels; this one shares their placement,
// hover-root and bridge mechanics and their .ci-panel styling.
//
// The card is fetched from /api/pr-details on open. Unlike the CI panel there
// is nothing to watch move, so it does not keep polling: it re-polls only
// while the daemon reports a read in flight, to pick up that answer.

// Re-poll cadence while the daemon is resolving a card, and how many times
// before giving up on a cold one (gh can be slow or unavailable).
const PR_DETAILS_RETRY_MS = 700;
const PR_DETAILS_MAX_RETRIES = 20;

// The last card served per PR URL, so a second hover paints immediately
// instead of flashing "Loading…" while it asks again.
const detailsCache = new Map();

async function fetchPRDetails(url, signal) {
  const response = await fetch(`/api/pr-details?url=${encodeURIComponent(url)}`, {
    credentials: 'same-origin', signal,
  });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  return response.json();
}

function usePRDetails(url, open) {
  const [details, setDetails] = useState(() => detailsCache.get(url) || null);
  const [error, setError] = useState('');
  const [pending, setPending] = useState(false);

  useEffect(() => {
    if (!open || !url) return undefined;
    const controller = new AbortController();
    let cancelled = false;
    let timer = 0;
    let tries = 0;
    setDetails(detailsCache.get(url) || null);
    const poll = async () => {
      setPending(true);
      try {
        const payload = await fetchPRDetails(url, controller.signal);
        if (cancelled) return;
        if (payload?.details) {
          detailsCache.set(url, payload.details);
          setDetails(payload.details);
        }
        setError('');
        // A cold card keeps asking while it is unresolved, even when this
        // poll landed on someone else's in-flight read (refreshing=false).
        const again = (payload?.refreshing || !payload?.resolved) && ++tries < PR_DETAILS_MAX_RETRIES;
        if (again) {
          timer = globalThis.setTimeout(poll, PR_DETAILS_RETRY_MS);
          return;
        }
      } catch (cause) {
        if (cancelled || controller.signal.aborted) return;
        setError(cause?.message || String(cause));
      }
      setPending(false);
    };
    void poll();
    return () => {
      cancelled = true;
      controller.abort();
      if (timer) globalThis.clearTimeout(timer);
    };
  }, [url, open]);

  return { details, error, pending };
}

const STATE_LABELS = { open: 'Open', draft: 'Draft', merged: 'Merged', closed: 'Closed' };

// diffBlocks splits five blocks between additions and deletions the way
// GitHub's diffstat does: any non-zero side gets at least one block.
export function diffBlocks(additions, deletions) {
  const add = Math.max(0, additions || 0);
  const del = Math.max(0, deletions || 0);
  const total = add + del;
  if (!total) return { add: 0, del: 0 };
  let a = Math.round((add / total) * 5);
  if (add && !a) a = 1;
  if (del && a === 5) a = 4;
  return { add: a, del: 5 - a };
}

function plural(n, word) {
  return `${n.toLocaleString('en-US')} ${word}${n === 1 ? '' : 's'}`;
}

// ageLine is the "who and when" next to the state chip: merged and closed
// PRs say when they ended, open ones when they were opened.
function ageLine(details, now) {
  const when = details.state === 'merged' ? ['merged', details.merged_at]
    : details.state === 'closed' ? ['closed', details.closed_at]
      : ['opened', details.created_at];
  const age = relativeAge(when[1], now);
  return [details.author, age ? `${when[0]} ${age}` : ''].filter(Boolean).join(' · ');
}

function PRDetailsBody({ details, now }) {
  const blocks = diffBlocks(details.additions, details.deletions);
  const commits = details.commits >= 100 ? '100+ commits' : plural(details.commits || 0, 'commit');
  return html`
    <div class="prd-meta">
      ${STATE_LABELS[details.state] ? html`<span class=${`prd-state prd-state-${details.state}`}>${STATE_LABELS[details.state]}</span>` : null}
      <span>${ageLine(details, now)}</span>
    </div>
    ${details.head_ref ? html`
      <div class="prd-branches">${details.head_ref}${details.base_ref ? ` → ${details.base_ref}` : ''}</div>
    ` : null}
    <div class="prd-stat">
      <span class="prd-add">+${(details.additions || 0).toLocaleString('en-US')}</span>
      <span class="prd-del">−${(details.deletions || 0).toLocaleString('en-US')}</span>
      <span class="prd-bar" aria-hidden="true">
        ${[0, 1, 2, 3, 4].map((i) => html`<i key=${i} class=${i < blocks.add ? 'a' : i < blocks.add + blocks.del ? 'd' : ''}></i>`)}
      </span>
      <span class="prd-counts">${plural(details.changed_files || 0, 'file')} · ${commits}</span>
    </div>
    ${details.excerpt
      ? html`<p class="prd-excerpt">${details.excerpt}</p>`
      : html`<p class="prd-excerpt prd-excerpt-empty">No description provided.</p>`}
  `;
}

function PRDetailsPanel({ prURL, prNumber, state, panelID, headingID, panelRef, placement }) {
  const { details, error, pending } = state;
  const now = Date.now();
  const style = placement && !placement.hidden
    ? `top:${placement.top}px;left:${placement.left}px;max-height:${placement.maxHeight}px;` +
      (placement.placement === 'above' ? 'transform:translateY(-100%)' : '')
    : 'visibility:hidden';
  const updated = details ? relativeAge(details.fetched_at, now) : '';
  return html`
    <div ref=${panelRef} class="ci-panel prd-panel" style=${style}
      id=${panelID} role="dialog" aria-labelledby=${headingID}>
      <div class="prd-title" id=${headingID}>
        <a href=${prURL} target="_blank" rel="noopener noreferrer">
          ${details?.title || (prNumber ? `Pull request #${prNumber}` : 'Pull request')}
          ${details?.title && prNumber ? html` <span class="prd-num">#${prNumber}</span>` : null}
        </a>
      </div>
      ${details ? html`<${PRDetailsBody} details=${details} now=${now} />` : html`
        <p class="ci-panel-empty">${error ? `Could not load pull request — ${error}`
          : pending ? 'Fetching pull request…' : 'Pull request details are unavailable.'}</p>
      `}
      <p class="ci-panel-note">
        <a href=${prURL} target="_blank" rel="noopener noreferrer">Conversation ↗</a>
        <span class="rv-note-sep"> · </span>
        <a href=${`${prURL}/files`} target="_blank" rel="noopener noreferrer">Files changed ↗</a>
        ${updated ? html`<span class="rv-note-age">updated ${updated}</span>` : null}
      </p>
    </div>
  `;
}

// PRLinkHover renders the PR link with its hover card. On a hoverless device
// a tap opens the card instead of navigating; the card's title and footer
// links then lead to GitHub.
export function PRLinkHover({ url, prNumber, className = '', label, describe = '' }) {
  const rootRef = useRef(null);
  const panelRef = useRef(null);
  const instanceID = useId();
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [tapped, setTapped] = useState(false);
  const open = hovered || focused || tapped;
  const prURL = safeCheckURL(url).replace(/\/+$/, '');
  const state = usePRDetails(open ? prURL : '', open);
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

  if (!prURL) return null;
  const panelID = `prd-panel-${instanceID.replace(/[^a-zA-Z0-9_-]+/g, '-')}`;
  const headingID = `${panelID}-heading`;

  return html`
    <span
      ref=${rootRef}
      class=${`ci-hover prd-hover${open ? ' is-open' : ''}`}
      onMouseEnter=${() => setHovered(true)}
      onMouseLeave=${() => setHovered(false)}
      onFocusIn=${() => setFocused(true)}
      onFocusOut=${(event) => { if (!rootRef.current?.contains(event.relatedTarget)) setFocused(false); }}
    >
      <a
        class=${`pr-link ${className}`.trim()}
        href=${prURL}
        target="_blank"
        rel="noopener noreferrer"
        draggable=${false}
        aria-expanded=${open ? 'true' : 'false'}
        aria-controls=${panelID}
        aria-label=${`${describe || 'Pull request'} — opens it on GitHub.`}
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
      >${label}</a>
      ${open ? html`
        <${PRDetailsPanel} prURL=${prURL} prNumber=${prNumber} state=${state}
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
