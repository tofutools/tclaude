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
// Gap between badge and panel, and the margin the panel keeps from the edges
// of the usable area. PANEL_MAX_H mirrors the 320px arm of the CSS cap; the
// 52vh arm is applied in JS too, because the inline max-height this code
// writes overrides the whole CSS declaration.
const PANEL_GAP = 7;
const PANEL_MARGIN = 8;
const PANEL_MAX_H = 320;
const PANEL_MIN_H = 120;

// placePRChecksPanel decides where the panel goes, given the badge's rect, the
// panel's width and the usable area (the viewport minus whatever fixed chrome
// owns its edges). Three rules:
//
//   - The panel is positioned relative to the VIEWPORT (fixed), not the row.
//     An absolutely-positioned panel hanging off a row near the bottom of a
//     long table stretches the document's scroll area, so merely opening a
//     tooltip made the whole dashboard scrollable further down.
//   - Below by default — that is where the eye expects it — flipping above
//     only when the panel cannot fit below AND there is genuinely more room
//     above, so a badge mid-screen keeps the familiar downward placement.
//   - The side is decided against the panel's MAXIMUM height, never its
//     current content height. When the fetch resolves, an empty "Fetching
//     checks…" panel becomes a full list; sizing the decision on what is
//     rendered right now would flip the panel to the other side mid-hover,
//     yanking it out from under a pointer already travelling toward it (and
//     unmounting it, which aborts the poll). Worst-case sizing means the
//     answer at open is the answer for the whole hover.
//
// Whatever side wins, the result is clamped into the usable area and capped
// to the height actually available, so a long list scrolls inside the panel
// instead of running off-screen or under the footer.
export function placePRChecksPanel({ anchor, panel, area }) {
  const cap = Math.max(PANEL_MIN_H, Math.min(PANEL_MAX_H, Math.round(area.height * 0.52)));
  const spaceBelow = area.bottom - anchor.bottom - PANEL_GAP - PANEL_MARGIN;
  const spaceAbove = anchor.top - area.top - PANEL_GAP - PANEL_MARGIN;
  const above = cap > spaceBelow && spaceAbove > spaceBelow;
  const room = above ? spaceAbove : spaceBelow;
  const maxHeight = Math.max(PANEL_MIN_H, Math.min(cap, room));
  const left = clamp(anchor.left, area.left + PANEL_MARGIN, area.right - panel.width - PANEL_MARGIN);
  // The min-height floor above can exceed the room on a very short viewport,
  // so the final position is clamped rather than trusted: off-screen and
  // behind the footer are both worse than overlapping the badge.
  const top = clamp(
    above ? anchor.top - PANEL_GAP - maxHeight : anchor.bottom + PANEL_GAP,
    area.top + PANEL_MARGIN,
    Math.max(area.top + PANEL_MARGIN, area.bottom - PANEL_MARGIN - maxHeight),
  );
  // The bridge is the transparent strip that keeps the pointer "inside" the
  // hover root while it crosses the gap. It spans both boxes horizontally,
  // because a clamped panel can sit well to the side of its badge and a
  // diagonal travel would otherwise fall outside both.
  const bridge = {
    left: Math.min(left, anchor.left),
    width: Math.max(left + panel.width, anchor.right) - Math.min(left, anchor.left),
    top: above ? top + maxHeight : anchor.bottom,
    height: Math.max(0, above ? anchor.top - (top + maxHeight) : top - anchor.bottom),
  };
  return { top, left, maxHeight, bridge, placement: above ? 'above' : 'below' };
}

function clamp(value, min, max) {
  return Math.max(min, Math.min(value, max));
}

function samePlacement(a, b) {
  if (!a || !b) return a === b;
  return a.top === b.top && a.left === b.left && a.maxHeight === b.maxHeight
    && a.placement === b.placement && a.hidden === b.hidden
    && a.bridge.left === b.bridge.left && a.bridge.width === b.bridge.width
    && a.bridge.top === b.bridge.top && a.bridge.height === b.bridge.height;
}

// usableArea is the viewport minus the fixed chrome that owns its edges: the
// footer bar along the bottom and, on the Groups tab, the agent dock down the
// right. Both paint above the panel (higher z-index), so a panel merely
// "inside the viewport" can still have its last rows and its GitHub link
// hidden behind them. Measured from the elements themselves rather than their
// CSS variables, so a hidden or collapsed dock costs nothing.
//
// clientWidth/clientHeight rather than innerWidth/innerHeight: this dashboard
// always reserves a document scrollbar, and innerWidth counts it.
function usableArea() {
  const root = document.documentElement;
  const area = {
    top: 0,
    left: 0,
    right: root?.clientWidth || 0,
    bottom: root?.clientHeight || 0,
  };
  const footer = document.querySelector('footer');
  if (footer) {
    const rect = footer.getBoundingClientRect();
    if (rect.height > 0 && rect.top < area.bottom) area.bottom = Math.max(0, rect.top);
  }
  const dock = document.getElementById('agent-dock');
  if (dock) {
    const rect = dock.getBoundingClientRect();
    if (rect.width > 0 && rect.left < area.right) area.right = Math.max(0, rect.left);
  }
  area.height = Math.max(0, area.bottom - area.top);
  area.width = Math.max(0, area.right - area.left);
  return area;
}

// usePanelPlacement measures the badge and keeps the placement current while
// the panel is open: on scroll (the page and any inner pane), on resize, and
// on every render, since the Groups table re-renders every snapshot and a row
// inserted above the badge would otherwise leave the panel behind.
//
// Measurements are coalesced into an animation frame and compared field-wise
// before publishing, so a wheel gesture costs one layout read per frame and
// re-renders the check list only when the panel actually moves.
function usePanelPlacement(rootRef, panelRef, open) {
  const [placement, setPlacement] = useState(null);
  const frame = useRef(0);

  const measureNow = useCallback(() => {
    frame.current = 0;
    const root = rootRef.current;
    const panel = panelRef.current;
    if (!root || !panel || typeof root.getBoundingClientRect !== 'function') return;
    const anchor = root.getBoundingClientRect();
    const box = panel.getBoundingClientRect();
    const area = usableArea();
    const next = placePRChecksPanel({
      anchor,
      panel: { width: box.width || 360 },
      area,
    });
    // A badge scrolled out of the usable area takes its panel with it. With
    // the old absolute positioning the panel left with its row; a fixed one
    // would otherwise hang around over unrelated content.
    next.hidden = anchor.bottom < area.top || anchor.top > area.bottom;
    setPlacement((current) => (samePlacement(current, next) ? current : next));
  }, [rootRef, panelRef]);

  const measure = useCallback(() => {
    if (frame.current) return;
    if (typeof requestAnimationFrame !== 'function') {
      measureNow();
      return;
    }
    frame.current = requestAnimationFrame(measureNow);
  }, [measureNow]);

  // Layout effect: place before paint, so the panel never appears in the
  // wrong spot and then jumps.
  useLayoutEffect(() => {
    if (!open) {
      setPlacement(null);
      return undefined;
    }
    measureNow();
    // Capture phase: the dashboard scrolls inner panes as well as the page,
    // and a panel pinned to the viewport has to follow either one. Scrolling
    // the panel's OWN list moves nothing, so that one is ignored.
    const onScroll = (event) => {
      if (panelRef.current?.contains(event.target)) return;
      measure();
    };
    const onResize = () => measure();
    window.addEventListener('scroll', onScroll, { passive: true, capture: true });
    window.addEventListener('resize', onResize, { passive: true });
    return () => {
      window.removeEventListener('scroll', onScroll, { capture: true });
      window.removeEventListener('resize', onResize);
      if (frame.current && typeof cancelAnimationFrame === 'function') {
        cancelAnimationFrame(frame.current);
      }
      frame.current = 0;
    };
  }, [open, measure, measureNow, panelRef]);

  // Every render while open: the snapshot repaints the table on its own
  // cadence, and the badge can move without any scroll or resize event.
  useLayoutEffect(() => {
    if (open) measure();
  });

  return { placement };
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

// The label has to carry both jobs this control does: report the counts, and
// say where activating it goes. aria-haspopup is deliberately absent — the
// panel opens on hover/focus, so promising a popup on activation would be a
// lie to anyone who presses Enter and lands on GitHub instead.
function badgeTitle(summary, prNumber) {
  const who = prNumber ? `#${prNumber}` : 'this pull request';
  return `CI checks for ${who} — ${summaryLine(summary)}. Opens the build on GitHub.`;
}

// prChecksPageURL is the fallback click target: the PR's own checks tab. Used
// when no check offered a workflow run to jump to — a PR checked only by
// external apps, say — so the badge is still useful, never a dead pill. The
// PR URL is server-validated as a GitHub PR before it ever reaches a badge,
// but it is guarded here too: this function feeds an href, and a guard that
// covers one branch of badgeHref and not the other is the asymmetry that rots.
export function prChecksPageURL(prURL) {
  const safe = safeCheckURL(prURL);
  return safe ? `${safe.replace(/\/+$/, '')}/checks` : '';
}

// badgeHref: the run that explains the badge's state when the server could
// name one (red -> the failing build), else the PR's checks page.
export function badgeHref(summary, prURL) {
  return safeCheckURL(summary?.run_url) || prChecksPageURL(prURL);
}

// touchOnly reports a device with no hover — a phone or tablet. There, the
// pointer never rests on the badge, so nothing would ever open the panel and
// a tap would just leave the dashboard. Such a tap opens the panel instead;
// its rows and footer link are then ordinary tappable links.
function touchOnly() {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function'
    && window.matchMedia('(hover: none)').matches;
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

function PRChecksPanel({ url, prNumber, summary, panelID, headingID, state, panelRef, placement }) {
  const { data, error, loading } = state;
  const live = data?.summary && (data.summary.total || 0) > 0 ? data.summary : summary;
  const checks = data?.checks || [];
  const now = Date.now();
  // Until the first measurement lands the panel is hidden rather than drawn
  // at a guessed position and moved: a tooltip that jumps on open reads as a
  // glitch. It is one layout effect away, so nothing perceptible is lost.
  // The same applies once the badge scrolls out of the usable area.
  const style = placement && !placement.hidden
    ? `top:${placement.top}px;left:${placement.left}px;max-height:${placement.maxHeight}px`
    : 'visibility:hidden';
  return html`
    <div ref=${panelRef} class="ci-panel" style=${style}
      id=${panelID} role="dialog" aria-labelledby=${headingID}>
      <div class="ci-panel-heading" id=${headingID}>
        <a href=${badgeHref(summary, url)} target="_blank" rel="noopener noreferrer">
          <span class="theme-copy-regular">${prNumber ? `#${prNumber} · checks` : 'Pull request checks'}</span>
          <span class="theme-copy-wizard">${prNumber ? `#${prNumber} · omens of the rite` : 'Omens of the rite'}</span>
        </a>
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
        <a href=${prChecksPageURL(url)} target="_blank" rel="noopener noreferrer">
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
  // tapped exists only for hoverless devices, where a tap opens the panel
  // instead of navigating. On a pointer device it is never set.
  const [tapped, setTapped] = useState(false);
  const open = hovered || focused || tapped;
  const state = usePRChecks(open ? url : '', open);
  const panelRef = useRef(null);
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
      <a
        class=${`ci-badge ci-${stateName}`}
        href=${badgeHref(summary, url)}
        target="_blank"
        rel="noopener noreferrer"
        draggable=${false}
        aria-expanded=${open ? 'true' : 'false'}
        aria-controls=${panelID}
        aria-label=${badgeTitle(summary, prNumber)}
        onClick=${(event) => {
          if (!touchOnly()) return; // pointer devices: let the link navigate
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
        <span class="ci-glyph" aria-hidden="true">${checkStateGlyph(stateName)}</span>
        <span class="ci-count">${summary.passed}/${denominator}</span>
      </a>
      ${open ? html`
        <${PRChecksPanel} url=${url} prNumber=${prNumber} summary=${summary}
          panelID=${panelID} headingID=${headingID} state=${state}
          panelRef=${panelRef} placement=${placement} />
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
