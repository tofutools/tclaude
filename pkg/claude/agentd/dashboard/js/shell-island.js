import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { syncBotAnimations } from './helpers.js';
import { ActivityModes } from './activity-bots.js';
import { ActivityHover } from './activity-hover.js';
import {
  footerMetaView,
  authoredOpenPRsView,
  globalActivityView,
  messagesBadgeView,
  usageView,
} from './shell-model.js';
import { PRChecksBadge } from './pr-checks-hover.js';
import { hasUnreadHumanNotifications } from './human-notification-attention.js';

const html = htm.bind(h);

function UsageToken({ token }) {
  if (token.kind === 'cost') {
    return html`
      <span class="uw ucost" data-goto-tab="costs">
        <span class="ulabel">${token.label}</span>
        ${token.today ? html`<span class="ucost-amt">${token.today}</span> <span class="urem">(today)</span>` : null}
        <span class="ucost-amt">${token.mtd}</span> <span class="urem">(mtd)</span>
      </span>
    `;
  }
  const blocks = [];
  for (let index = 0; index < 8; index++) {
    blocks.push(html`<span key=${index} class=${index < token.filled ? 'ubar-fill' : 'ubar-empty'} style=${index < token.filled ? `color:${token.color}` : ''}>█</span>`);
  }
  return html`
    <span class=${`uw${token.hidden ? ' umissing' : ''}`} aria-hidden=${token.hidden ? 'true' : null}>
      <span class="ulabel">${token.label}</span>
      <span class="ubar">${blocks}</span>
      <span class="upct">${token.pct}%</span>
      <span class="urem">${token.remaining}</span>
    </span>
  `;
}

function Usage({ state }) {
  const view = usageView(state.snapshot.value?.usage);
  if (view.na) return html`<span id="usage" class="meta na" title=${view.title}>${view.text}</span>`;
  return html`
    <span id="usage" class=${`meta${view.multiline ? ' multiline' : ''}`} title=${view.title}>
      ${view.lines.map((line) => view.multiline ? html`
        <span key=${line.key} class="uline">
          <span class="usrc">${line.label}</span>
          ${line.tokens.map((token) => html`<${UsageToken} key=${token.key} token=${token} />`)}
        </span>
      ` : line.tokens.map((token) => html`<${UsageToken} key=${token.key} token=${token} />`))}
    </span>
  `;
}

function GlobalActivity({ state, groupsState }) {
  const snapshot = state.snapshot.value;
  const visibility = groupsState?.visibility.value;
  const [wizard, setWizard] = useState(() => document.body.classList.contains('wizard'));
  const view = globalActivityView(snapshot, wizard, visibility);
  useEffect(() => {
    const update = () => setWizard(document.body.classList.contains('wizard'));
    document.addEventListener('tclaude:wizard', update);
    return () => document.removeEventListener('tclaude:wizard', update);
  }, []);
  useLayoutEffect(() => syncBotAnimations(), [view.animationKey]);
  if (!view.modes.length) return null;
  const hasNotifications = hasUnreadHumanNotifications(snapshot);
  const label = hasNotifications
    ? 'Activity across all groups · one or more agents have unread notifications'
    : 'Activity across all groups';
  return html`
    <${ActivityHover}
      id="global-activity"
      className="global-activity"
      label=${label}
      title=${view.title || null}
      details=${view.details}
      wizard=${wizard}
    >
      ${hasNotifications ? html`<span class="human-notification-hint"
        aria-hidden="true" title="One or more agents have unread notifications">!</span>` : null}
      <${ActivityModes} modes=${view.modes} />
    </${ActivityHover}>
  `;
}

function Status({ feedback }) {
  const current = feedback.status.value;
  const classes = ['meta'];
  if (current.error) classes.push('error');
  else if (current.text) classes.push('live');
  return html`<span class=${classes.join(' ')} id="status">${current.text}</span>`;
}

function MessagesBadge({ state }) {
  const view = messagesBadgeView(state.snapshot.value);
  return html`
    <span id="messages-badge" class=${`tab-badge${view.blink ? ' blink' : ''}`} hidden=${view.hidden}>${view.text}</span>
  `;
}

function FooterMeta({ state }) {
  const view = footerMetaView(state.snapshot.value);
  if (!view) return html`<span class="meta" id="meta">loading…</span>`;
  return html`
    <span class="meta" id="meta">
      <span class="meta-version">tclaude version ${view.version}</span>
      <span class="meta-sep"> · </span>refreshed <span class="meta-time">${new Date(view.generatedAt).toLocaleTimeString()}</span>
    </span>
  `;
}

// A closed/merged row is dotted by its terminal state instead of its CI state
// — checks on something already landed say nothing useful — and it drops the
// "no active agent" note, which only means something for open work.
function OpenPRRow({ pr }) {
  const terminal = pr.state === 'merged' || pr.state === 'closed';
  const dot = terminal ? pr.state : (pr?.checks?.state || 'unknown');
  return html`
    <li class="open-pr-row">
      <span class=${`open-pr-state open-pr-state-${dot}`} aria-hidden="true"></span>
      <span class="open-pr-main">
        <a class="open-pr-title" href=${pr.url} target="_blank" rel="noopener noreferrer">${pr.title || `Pull request #${pr.number}`}</a>
        <span class="open-pr-meta">
          <span>${pr.repository}</span><span> · #${pr.number}</span>
          ${terminal ? html`<span> · ${pr.state}${pr.closed_at ? ` ${new Date(pr.closed_at).toLocaleDateString()}` : ''}</span>` : null}
          ${!terminal && pr.agent_title ? html`<span> · ${pr.agent_title}</span>` : null}
          ${!terminal && !pr.agent_title ? html`<span> · no active agent</span>` : null}
          ${pr.draft && !terminal ? html`<span> · draft</span>` : null}
        </span>
      </span>
      <${PRChecksBadge} url=${pr.url} prNumber=${pr.number} summary=${pr.checks} />
    </li>
  `;
}

function OpenPRs({ state }) {
  const [filter, setFilter] = useState('all');
  const [hovered, setHovered] = useState(false);
  const [pinned, setPinned] = useState(false);
  const rootRef = useRef(null);
  const view = authoredOpenPRsView(state.snapshot.value, filter);
  // The indicator is permanent by default (dashboard.always_show_open_prs), so
  // the popover must open at zero open PRs too — that is where the recently
  // closed list lives. It still stays out of the footer entirely until the
  // daemon has resolved a GitHub identity, since a fixed "Open PRs 0" would be
  // a lie when `gh` is missing or logged out.
  const visible = view.available && (view.total > 0 || view.alwaysShow);
  const open = visible && (hovered || pinned);

  useEffect(() => {
    if (!pinned) return undefined;
    const outside = (event) => {
      if (!rootRef.current?.contains(event.target)) setPinned(false);
    };
    document.addEventListener('pointerdown', outside);
    return () => document.removeEventListener('pointerdown', outside);
  }, [pinned]);
  useEffect(() => {
    if (!open) return undefined;
    const escape = (event) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      setPinned(false);
      setHovered(false);
      rootRef.current?.querySelector('.open-prs-trigger')?.focus();
    };
    document.addEventListener('keydown', escape);
    return () => document.removeEventListener('keydown', escape);
  }, [open]);

  if (!visible) return null;
  const age = view.updatedAt ? new Date(view.updatedAt).toLocaleTimeString() : '';
  // A rolling 24h window is not "today" — a PR closed at 23:00 yesterday is in
  // it — so every window reads as a span, including the 1-day one.
  const recentLabel = `Closed ${view.recentWindowDays}d`;
  // "recent" selected while the window is configured off falls back to the
  // open list, so the Open chip — not a vanished chip — must read as active.
  const openFilterActive = !view.showingRecent && filter !== 'attention' && filter !== 'unattached';
  return html`
    <span ref=${rootRef} class=${`open-prs${open ? ' is-open' : ''}${view.total > 0 ? '' : ' is-empty'}`}
      onMouseEnter=${() => setHovered(true)} onMouseLeave=${() => setHovered(false)}>
      <button type="button" class="open-prs-trigger" aria-haspopup="dialog"
        aria-expanded=${open ? 'true' : 'false'} aria-controls="open-prs-popover"
        onClick=${() => setPinned((value) => !value)}>
        <span class="open-prs-dot" aria-hidden="true"></span>
        <span>Open PRs</span><span class="open-prs-count">${view.total}</span>
        <span class="open-prs-chevron" aria-hidden="true">⌃</span>
      </button>
      ${open ? html`
        <div id="open-prs-popover" class="open-prs-popover" role="dialog" aria-label="Your pull requests">
          <div class="open-prs-head"><strong>Your pull requests</strong><span class="open-prs-count">${view.total}</span>${age ? html`<span class="open-prs-age">updated ${age}</span>` : null}</div>
          <div class="open-prs-filters" role="group" aria-label="Filter pull requests">
            <button class=${openFilterActive ? 'active' : ''} aria-pressed=${openFilterActive} onClick=${() => setFilter('all')}>Open ${view.total}</button>
            <button class=${filter === 'attention' ? 'active' : ''} aria-pressed=${filter === 'attention'} onClick=${() => setFilter('attention')}>Needs attention ${view.attention}</button>
            <button class=${filter === 'unattached' ? 'active' : ''} aria-pressed=${filter === 'unattached'} onClick=${() => setFilter('unattached')}>Unattached ${view.unattached}</button>
            ${view.recentWindowDays > 0 ? html`<button class=${view.showingRecent ? 'active' : ''} aria-pressed=${view.showingRecent}
              title=${`Pull requests you merged or closed in the last ${view.recentWindowDays} day(s)`}
              onClick=${() => setFilter('recent')}>${recentLabel} ${view.recentCount}</button>` : null}
          </div>
          ${view.items.length
            ? html`<ul class="open-pr-list">${view.items.map((pr) => html`<${OpenPRRow} key=${pr.url} pr=${pr} />`)}</ul>`
            : html`<p class="open-prs-empty">${view.showingRecent
              ? `Nothing merged or closed in the last ${view.recentWindowDays} day(s).`
              : (openFilterActive && view.total <= 0 ? 'No open pull requests.' : 'No pull requests match this filter.')}</p>`}
          ${view.searchURL ? html`<div class="open-prs-foot">${view.truncated ? html`<span>Showing the first ${view.items.length} · </span>` : null}<a href=${view.searchURL} target="_blank" rel="noopener noreferrer">${view.showingRecent ? 'See them all on GitHub ↗' : 'Open all on GitHub ↗'}</a></div>` : null}
        </div>
      ` : null}
    </span>
  `;
}

function Disconnect({ state }) {
  const disconnected = state.connection.value.status === 'disconnected';
  // Do not leave the full-viewport backdrop-filter subtree mounted after a
  // reconnect. Chromium can retain the activated backdrop/animation
  // compositor layers after an ancestor flips back to display:none; moving a
  // transform underneath them (notably #dnd-pill during a native drag) then
  // falls into a visibly throttled repaint path until the page is reloaded.
  // Removing the subtree on the connected edge makes the browser tear those
  // layers down deterministically.
  if (!disconnected) return null;
  return html`
    <div class="disconnect-overlay show" id="disconnect-overlay">
      <div class="disconnect-card" role="alert" aria-live="assertive">
        <div class="disconnect-icon" aria-hidden="true">⚠️</div>
        <h2 class="disconnect-title" id="disconnect-title">Disconnected from agentd</h2>
        <p class="disconnect-body">The dashboard can’t reach the tclaude agentd daemon. Everything below may be stale, and the music has been stopped.</p>
        <p class="disconnect-status" id="disconnect-status">Reconnecting…</p>
      </div>
    </div>
  `;
}

function Toast({ feedback }) {
  const current = feedback.toast.value;
  return html`<div class=${`toast${current.error ? ' error' : ''}${current.visible ? ' show' : ''}`} id="toast">${current.message}</div>`;
}

function Confirm({ feedback }) {
  const model = feedback.confirmation.value;
  // Busy means a confirmed blocking action is still in flight. The dialog stays
  // up with both buttons disabled and the primary swapped to its spinner label
  // — the same busy vocabulary the transaction dialogs already use.
  const busy = !!model?.busy;
  const okRef = useRef(null);
  useLayoutEffect(() => {
    if (model && !busy) okRef.current?.focus();
  }, [model, busy]);
  useEffect(() => {
    if (!model || busy) return undefined;
    const onKey = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopImmediatePropagation();
        feedback.resolveConfirmation(false);
        return;
      }
      if (event.key !== 'Enter' || (!event.ctrlKey && !event.metaKey)
        || event.isComposing || event.keyCode === 229) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      okRef.current?.click();
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
  }, [model, busy, feedback]);
  // A busy modal is not dismissible: the work is already in flight, so Escape
  // or a backdrop click would only hide it, not stop it — and hiding it puts
  // the operator right back to "did anything happen?".
  const dismissBackdrop = (event) => {
    if (busy) return;
    if (event.currentTarget === event.target) feedback.resolveConfirmation(false);
  };
  return html`
    <div class=${`modal-overlay${model ? ' show' : ''}`} id="confirm-modal" onClick=${dismissBackdrop}>
      <div class="modal" role="dialog" aria-modal="true" aria-labelledby="confirm-title">
        <h3 id="confirm-title">${model?.title || ''}</h3>
        <p id="confirm-body" class=${model?.preformatted ? 'confirm-body-preformatted' : ''}>${model?.body || ''}</p>
        <div class="modal-meta" id="confirm-meta" style=${`display:${model?.meta ? 'block' : 'none'}`}>${model?.meta || ''}</div>
        <div class="modal-buttons">
          ${model?.informational ? null : html`<button id="confirm-cancel" disabled=${busy} onClick=${() => feedback.resolveConfirmation(false)}>${model?.cancelLabel || 'Cancel'}</button>`}
          <button ref=${okRef} id="confirm-ok" class=${model?.informational ? '' : 'confirm-danger'}
            disabled=${busy} aria-busy=${busy ? 'true' : undefined}
            onClick=${() => feedback.resolveConfirmation(true)}>
            ${busy
              ? html`<span class="btn-spinner" aria-hidden="true"></span>${model?.busyLabel || 'Working…'}`
              : (model?.okLabel || 'Confirm')}
          </button>
        </div>
      </div>
    </div>
  `;
}

export function mountShellIsland({ hosts, state, groupsState, feedback, registerCleanup }) {
  const roots = [
    [hosts.activityHost, html`<${GlobalActivity} state=${state} groupsState=${groupsState} />`],
    [hosts.usageHost, html`<${Usage} state=${state} />`],
    [hosts.statusHost, html`<${Status} feedback=${feedback} />`],
    [hosts.messagesBadgeHost, html`<${MessagesBadge} state=${state} />`],
    [hosts.metaHost, html`<${FooterMeta} state=${state} />`],
    [hosts.openPRsHost, html`<${OpenPRs} state=${state} />`],
    [hosts.disconnectHost, html`<${Disconnect} state=${state} />`],
    [hosts.toastHost, html`<${Toast} feedback=${feedback} />`],
    [hosts.confirmHost, html`<${Confirm} feedback=${feedback} />`],
  ];
  const mounted = [];
  registerCleanup(() => {
    feedback.dispose();
    for (const host of mounted.slice().reverse()) render(null, host);
  });
  for (const [host, vnode] of roots) {
    mounted.push(host);
    render(vnode, host);
  }
}

export { Confirm, Disconnect, FooterMeta, GlobalActivity, MessagesBadge, OpenPRs, Status, Toast, Usage };
