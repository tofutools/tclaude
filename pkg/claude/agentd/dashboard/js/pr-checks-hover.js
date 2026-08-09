import { h } from 'preact';
import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

// The CI indicator that trails a PR badge in the Groups tab, plus the hover
// panel listing every individual check.
//
// The snapshot carries counts only (branch_checks / startup_checks / a
// presented PR's checks), which is what the compact n/m badge needs. The
// per-check list is fetched from /api/pr-checks only while a human is
// actually looking at one PR: hovering (or focusing) the badge fires an
// immediate fetch and then re-polls on PR_CHECKS_POLL_MS until the pointer
// leaves. That is the whole reason the endpoint exists separately from the
// snapshot — it keeps a watched run moving without putting a per-PR
// subprocess on the 2s snapshot path.
//
// The panel lives inside the same hover root as the trigger (the
// ActivityHover arrangement), so moving the cursor onto the panel to read or
// scroll it never closes it.

const PR_CHECKS_POLL_MS = 6000;
// Gap between badge and panel, and the margin the panel keeps from the
// viewport edges. PANEL_MAX_H matches the CSS cap; placement narrows it
// further when the chosen side has less room than that.
const PANEL_GAP = 7;
const PANEL_MARGIN = 8;
const PANEL_MAX_H = 320;

// placePRChecksPanel decides where the panel goes, given the badge's rect and
// the panel's natural size. Two things drive it:
//
//   - The panel is positioned relative to the VIEWPORT (fixed), not the row.
//     An absolutely-positioned panel hanging off a row near the bottom of a
//     long table stretches the document's scroll area, so merely opening a
//     tooltip made the whole dashboard scrollable further down.
//   - Below is the default because that is where the eye expects it, but a
//     badge low on the screen gets it above instead — and only when there is
//     genuinely more room up there, so a badge in the middle of a tall
//     viewport keeps the familiar downward placement.
//
// Whatever side wins, the panel is clamped into the viewport and told how
// tall it may be, so it scrolls internally rather than running off-screen.
export function placePRChecksPanel({ anchor, panel, viewport }) {
  const spaceBelow = viewport.height - anchor.bottom - PANEL_GAP - PANEL_MARGIN;
  const spaceAbove = anchor.top - PANEL_GAP - PANEL_MARGIN;
  const wanted = Math.min(panel.height || PANEL_MAX_H, PANEL_MAX_H);
  const above = wanted > spaceBelow && spaceAbove > spaceBelow;
  const room = Math.max(80, above ? spaceAbove : spaceBelow);
  const maxHeight = Math.min(wanted, room);
  const left = Math.max(
    PANEL_MARGIN,
    Math.min(anchor.left, viewport.width - panel.width - PANEL_MARGIN),
  );
  const top = above
    ? Math.max(PANEL_MARGIN, anchor.top - PANEL_GAP - maxHeight)
    : anchor.bottom + PANEL_GAP;
  return { top, left, maxHeight, placement: above ? 'above' : 'below' };
}

// usePanelPlacement measures the badge and panel and keeps the placement
// current while the panel is open. Scroll/resize listeners exist only for
// that window — a closed badge costs nothing.
function usePanelPlacement(rootRef, panelRef, open) {
  const [placement, setPlacement] = useState(null);

  const measure = useCallback(() => {
    const root = rootRef.current;
    const panel = panelRef.current;
    if (!root || !panel || typeof root.getBoundingClientRect !== 'function') return;
    const anchor = root.getBoundingClientRect();
    const box = panel.getBoundingClientRect();
    setPlacement(placePRChecksPanel({
      anchor,
      panel: { width: box.width || 360, height: panel.scrollHeight || box.height || 0 },
      viewport: { width: window.innerWidth || 0, height: window.innerHeight || 0 },
    }));
  }, [rootRef, panelRef]);

  // Layout effect: place before paint so the panel never shows up in the
  // wrong spot and jump.
  useLayoutEffect(() => {
    if (!open) {
      setPlacement(null);
      return undefined;
    }
    measure();
    // Capture phase: the dashboard scrolls inner panes as well as the page,
    // and a panel pinned to the viewport has to follow either one.
    const onViewportChange = () => measure();
    window.addEventListener('scroll', onViewportChange, { passive: true, capture: true });
    window.addEventListener('resize', onViewportChange, { passive: true });
    return () => {
      window.removeEventListener('scroll', onViewportChange, { capture: true });
      window.removeEventListener('resize', onViewportChange);
    };
  }, [open, measure]);

  return { placement, measure };
}

async function fetchPRChecks(url, signal) {
  const response = await fetch(`/api/pr-checks?url=${encodeURIComponent(url)}`, {
    credentials: 'same-origin', signal,
  });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  return response.json();
}

// checkDenominator excludes skipped checks: "12/14" should mean twelve of the
// fourteen checks that actually had to run, not count two path-filtered jobs
// as outstanding work.
export function checkDenominator(summary) {
  return Math.max(0, (summary?.total || 0) - (summary?.skipped || 0));
}

export function checkStateGlyph(state) {
  switch (state) {
    case 'passing': return '✓';
    case 'failing': return '✕';
    case 'pending': return '◐';
    default: return '·';
  }
}

// safeCheckURL mirrors the server-side scheme allowlist in prchecks.go. A
// check's details link is attacker-influenced (a commit status's target_url
// is set by whoever posted it), and this renders it as a live href in the
// dashboard origin — so neither side is allowed to be the only guard.
export function safeCheckURL(raw) {
  return /^https?:\/\//i.test(String(raw || '').trim()) ? String(raw).trim() : '';
}

function bucketGlyph(bucket) {
  switch (bucket) {
    case 'pass': return '✓';
    case 'fail': return '✕';
    case 'skipped': return '⊘';
    default: return '◐';
  }
}

// elapsed renders a check's runtime: completed checks keep their final
// duration, a still-running one counts up from its start. Returns '' when the
// check never reported a start time.
export function elapsed(check, now = Date.now()) {
  const started = Date.parse(check?.started_at || '');
  if (!Number.isFinite(started)) return '';
  const ended = Date.parse(check?.completed_at || '');
  const end = Number.isFinite(ended) ? ended : now;
  const seconds = Math.max(0, Math.round((end - started) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${String(seconds % 60).padStart(2, '0')}s`;
  return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, '0')}m`;
}

export function summaryLine(summary) {
  const parts = [];
  if (summary?.passed) parts.push(`${summary.passed} passed`);
  if (summary?.failed) parts.push(`${summary.failed} failed`);
  if (summary?.pending) parts.push(`${summary.pending} running`);
  if (summary?.skipped) parts.push(`${summary.skipped} skipped`);
  return parts.join(' · ') || 'no checks';
}

function badgeTitle(summary, prNumber) {
  const who = prNumber ? `#${prNumber}` : 'this pull request';
  return `CI checks for ${who} — ${summaryLine(summary)}`;
}

// usePRChecks owns the poll lifecycle: it fetches on open, re-polls while the
// panel stays open, and drops both on close. The last good payload is kept
// across polls so a transient failure (or the daemon's own cold cache) never
// blanks a panel the human is reading.
function usePRChecks(url, open) {
  const [data, setData] = useState(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const seq = useRef(0);

  useEffect(() => {
    if (!open || !url) return undefined;
    const controller = new AbortController();
    const mine = ++seq.current;
    let timer = 0;
    let cancelled = false;

    const poll = async () => {
      if (cancelled) return;
      setLoading(true);
      try {
        const payload = await fetchPRChecks(url, controller.signal);
        if (cancelled || seq.current !== mine) return;
        setData(payload);
        setError('');
      } catch (cause) {
        if (cancelled || controller.signal.aborted) return;
        setError(cause?.message || String(cause));
      } finally {
        if (!cancelled) setLoading(false);
      }
      if (!cancelled) timer = globalThis.setTimeout(poll, PR_CHECKS_POLL_MS);
    };
    void poll();

    return () => {
      cancelled = true;
      controller.abort();
      if (timer) globalThis.clearTimeout(timer);
    };
  }, [url, open]);

  return { data, error, loading };
}

function CheckRow({ check, now }) {
  const time = elapsed(check, now);
  const href = safeCheckURL(check.url);
  const body = html`
    <span class="ci-check-body">
      <span class="ci-check-name">${check.name}</span>
      ${check.source || check.conclusion ? html`
        <span class="ci-check-meta">${[check.source, check.conclusion].filter(Boolean).join(' · ')}</span>
      ` : null}
    </span>
  `;
  return html`
    <li class=${`ci-check ci-check-${check.bucket || 'pending'}`}>
      <span class=${`ci-check-icon ci-icon-${check.bucket || 'pending'}`} aria-hidden="true">${bucketGlyph(check.bucket)}</span>
      ${href
        ? html`<a class="ci-check-link" href=${href} target="_blank" rel="noopener noreferrer">${body}</a>`
        : body}
      <span class="ci-check-time">${time || '—'}</span>
    </li>
  `;
}

function PRChecksPanel({ url, prNumber, summary, panelID, headingID, state, panelRef, placement, onContentChange }) {
  const { data, error, loading } = state;
  const live = data?.summary && (data.summary.total || 0) > 0 ? data.summary : summary;
  const checks = data?.checks || [];
  const now = Date.now();
  // A poll that adds rows changes how tall the panel wants to be, which can
  // change which side it belongs on. Re-measure when the content does.
  useLayoutEffect(() => { onContentChange(); }, [checks.length, error, loading]);
  // Until the first measurement lands the panel is hidden rather than drawn
  // at a guessed position and moved: a tooltip that jumps on open reads as a
  // glitch. It is one layout effect away, so nothing perceptible is lost.
  const style = placement
    ? `top:${placement.top}px;left:${placement.left}px;max-height:${placement.maxHeight}px`
    : 'visibility:hidden';
  return html`
    <div ref=${panelRef} class="ci-panel" style=${style}
      id=${panelID} role="dialog" aria-labelledby=${headingID}>
      <div class="ci-panel-heading" id=${headingID}>
        <span class="theme-copy-regular">${prNumber ? `#${prNumber} · checks` : 'Pull request checks'}</span>
        <span class="theme-copy-wizard">${prNumber ? `#${prNumber} · omens of the rite` : 'Omens of the rite'}</span>
      </div>
      <div class="ci-panel-summary">${summaryLine(live)}</div>
      ${checks.length ? html`
        <ul class="ci-checks">
          ${checks.map((check, index) => html`
            <${CheckRow} key=${`${check.name}:${index}`} check=${check} now=${now} />
          `)}
        </ul>
      ` : html`
        <p class="ci-panel-empty">
          ${error ? `Could not load checks — ${error}`
            : loading || data?.refreshing ? 'Fetching checks…'
            : 'No checks reported for this pull request.'}
        </p>
      `}
      ${checks.length && error ? html`<p class="ci-panel-empty">Refresh failed — ${error}</p>` : null}
      <p class="ci-panel-note">
        <a href=${`${url.replace(/\/+$/, '')}/checks`} target="_blank" rel="noopener noreferrer">
          <span class="theme-copy-regular">Open checks on GitHub ↗</span>
          <span class="theme-copy-wizard">Read the full omens ↗</span>
        </a>
      </p>
    </div>
  `;
}

// PRChecksBadge is the whole affordance: the n/m pill plus its panel. Renders
// nothing when the snapshot carried no summary — a PR whose checks haven't
// resolved yet, or a repo with no CI at all, keeps the bare PR link it always
// had rather than growing an empty indicator.
export function PRChecksBadge({ url, prNumber, summary }) {
  const rootRef = useRef(null);
  const instanceID = useId();
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [pinned, setPinned] = useState(false);
  const open = hovered || focused || pinned;
  const state = usePRChecks(open ? url : '', open);
  const panelRef = useRef(null);
  const { placement, measure } = usePanelPlacement(rootRef, panelRef, open);

  const close = () => { setPinned(false); setHovered(false); setFocused(false); };

  useEffect(() => {
    if (!pinned) return undefined;
    const onPointerDown = (event) => {
      if (!rootRef.current?.contains(event.target)) close();
    };
    document.addEventListener('pointerdown', onPointerDown);
    return () => document.removeEventListener('pointerdown', onPointerDown);
  }, [pinned]);

  if (!url || !summary || !(summary.total > 0)) return null;
  const denominator = checkDenominator(summary);
  const panelID = `ci-panel-${instanceID.replace(/[^a-zA-Z0-9_-]+/g, '-')}`;
  const headingID = `${panelID}-heading`;
  const stateName = summary.state || 'unknown';

  return html`
    <span
      ref=${rootRef}
      class=${`ci-hover${open ? ' is-open' : ''}${placement ? ` ci-place-${placement.placement}` : ''}`}
      onMouseEnter=${() => setHovered(true)}
      onMouseLeave=${() => setHovered(false)}
      onFocusIn=${() => setFocused(true)}
      onFocusOut=${(event) => { if (!rootRef.current?.contains(event.relatedTarget)) setFocused(false); }}
    >
      <button
        type="button"
        class=${`ci-badge ci-${stateName}`}
        aria-haspopup="dialog"
        aria-expanded=${open ? 'true' : 'false'}
        aria-controls=${panelID}
        aria-label=${badgeTitle(summary, prNumber)}
        title=${badgeTitle(summary, prNumber)}
        onClick=${() => (pinned ? close() : setPinned(true))}
        onKeyDown=${(event) => {
          if (event.key !== 'Escape') return;
          event.preventDefault();
          event.stopPropagation();
          close();
        }}
      >
        <span class="ci-glyph" aria-hidden="true">${checkStateGlyph(stateName)}</span>
        <span class="ci-count">${summary.passed}/${denominator}</span>
      </button>
      ${open ? html`
        <${PRChecksPanel} url=${url} prNumber=${prNumber} summary=${summary}
          panelID=${panelID} headingID=${headingID} state=${state}
          panelRef=${panelRef} placement=${placement} onContentChange=${measure} />
      ` : null}
    </span>
  `;
}
