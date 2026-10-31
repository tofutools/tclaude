import { h, render } from 'preact';
import { SpendLimitBanner } from './spend-limits.js';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { syncBotAnimations } from './helpers.js';
import { ActivityModes } from './activity-bots.js';
import { ActivityHover } from './activity-hover.js';
import {
  footerMetaView,
  authoredOpenPRsView,
  ignorePR,
  OPEN_PR_IGNORE_DURATIONS,
  parseIgnoredPRs,
  serializeIgnoredPRs,
  unignorePR,
  globalActivityView,
  messagesBadgeView,
  usageView,
  WHAT_IF_HINT,
} from './shell-model.js';
import { PRChecksBadge } from './pr-checks-hover.js';
import { PRReviewBadge } from './pr-review-badge.js';
import { PRLinkHover } from './pr-link-hover.js';
import { nodeFetch } from './node-api.js';
import {
  hasUnreadHumanNotifications, openHumanNotificationReader, unreadHumanMessages,
} from './human-notification-attention.js';
import { dashPrefs } from './prefs.js';
import { featureNeeds, featureState, needsSuffix } from './peer-view-limits.js';
import { UsageNodePill, peerUsageState, usagePeak, useUsageNode } from './usage-nodes.js';
import { fleetState } from './fleet-state.js';
import { tickedNodes } from './fleet-scope.js';
import { fusedDock } from './dock-nodes.js';
import { useDismissablePopover, useExclusivePopover, useLeaveGrace } from './hover-popover.js';

const html = htm.bind(h);
const OPEN_PRS_INCLUDE_DRAFTS_PREF = 'tclaude.dash.open-prs.include-drafts-attention';
const OPEN_PRS_IGNORED_PREF = 'tclaude.dash.open-prs.ignored';
// Title hover cards are on unless the operator turns them off: a card can
// cover a good part of the list, so the popover keeps a checkbox for it.
const OPEN_PRS_TITLE_DETAILS_PREF = 'tclaude.dash.open-prs.title-details';
// Colleague tabs: the daemon owns the tracked list (it spends this node's
// GitHub quota polling it), so edits go to its endpoint, not to a pref.
const TRACKED_AUTHORS_PATH = '/api/open-prs/tracked-authors';
const TRACKED_AUTHORS_MAX = 8;
const GITHUB_LOGIN_RE = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/;

async function postTrackedAuthor(action, login) {
  const response = await nodeFetch('')(TRACKED_AUTHORS_PATH, {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action, login }),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(payload?.error || `HTTP ${response.status}`);
  return Array.isArray(payload?.authors) ? payload.authors : [];
}

// PeopleTabs switches the popover between the operator's own pull requests
// and each tracked colleague's, and adds or removes colleagues inline.
function PeopleTabs({ active, mine, people, onSelect, onAdd, onRemove }) {
  const [adding, setAdding] = useState(false);
  const [login, setLogin] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const inputRef = useRef(null);
  useLayoutEffect(() => { if (adding) inputRef.current?.focus(); }, [adding]);
  const cancel = () => { setAdding(false); setLogin(''); setError(''); };
  const submit = async (event) => {
    event.preventDefault();
    const value = login.trim().replace(/^@/, '');
    if (!GITHUB_LOGIN_RE.test(value)) {
      setError('Not a GitHub username');
      return;
    }
    setBusy(true);
    try {
      await onAdd(value);
      cancel();
      onSelect(value);
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      setBusy(false);
    }
  };
  const tab = (key, label, count, extra = null) => html`
    <span key=${key || 'self'} class=${`open-prs-person${active.toLowerCase() === key.toLowerCase() ? ' active' : ''}`}>
      <button type="button" role="tab" aria-selected=${active.toLowerCase() === key.toLowerCase() ? 'true' : 'false'}
        onClick=${() => onSelect(key)}>
        ${label} <span class="open-prs-person-n">${count ?? '…'}</span>
      </button>
      ${extra}
    </span>
  `;
  return html`
    <div class="open-prs-people">
      <div role="tablist" aria-label="Whose pull requests" class="open-prs-people-tabs">
        ${tab('', 'You', mine)}
        ${people.map((p) => tab(p.login, p.login, p.total, html`
          <button type="button" class="open-prs-person-x" aria-label=${`Stop tracking ${p.login}`}
            title=${`Stop tracking ${p.login}`} onClick=${() => onRemove(p.login)}>✕</button>
        `))}
      </div>
      ${adding ? html`
        <form class="open-prs-add-form" onSubmit=${submit}
          onKeyDown=${(event) => {
            if (event.key !== 'Escape') return;
            event.preventDefault();
            event.stopPropagation();
            cancel();
          }}>
          <input ref=${inputRef} type="text" aria-label="GitHub username" placeholder="GitHub username"
            maxlength="40" autocomplete="off" spellcheck="false" value=${login} disabled=${busy}
            onInput=${(event) => { setLogin(event.currentTarget.value); setError(''); }} />
          <button type="submit" class="open-prs-add-ok" disabled=${busy || !login.trim()}>Add</button>
          <button type="button" class="open-prs-add-cancel" aria-label="Cancel adding a person" onClick=${cancel}>✕</button>
        </form>
      ` : people.length < TRACKED_AUTHORS_MAX ? html`
        <button type="button" class="open-prs-add-person" title="Track another GitHub user’s open pull requests"
          onClick=${() => setAdding(true)}>＋ Add person</button>
      ` : null}
      ${error ? html`<span class="open-prs-people-error" role="alert">${error}</span>` : null}
    </div>
  `;
}

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
  if (token.kind === 'units') {
    return html`<span class="uw uunits"><strong>${token.used.toLocaleString('en-US')}</strong>
      <span class="urem">/ ${token.limit.toLocaleString('en-US')} AIC ${token.remaining}</span></span>`;
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

function Usage({ state, fleet = fleetState, fetchImpl = undefined }) {
  const prefKey = (provider) => `tclaude.dash.headerUsageMode.${provider}`;
  const [modes, setModes] = useState({});
  // A fused view (always this node's page) picks whose quota the bars show.
  const fleetValue = fleet.fleet.value;
  const fused = fleet.fused.value;
  const nodes = fleetValue && fused && !globalThis.__tclaudeRemoteNode ? tickedNodes(fleetValue, fused) : [];
  const selfID = fleetValue?.self?.id || '';
  const picker = useUsageNode({ nodes, selfID, ...(fetchImpl ? { fetchImpl } : {}) });
  const multi = nodes.length > 1;
  const shownNode = multi ? nodes.find((n) => n.id === picker.selected) : null;
  const peerShown = shownNode && !shownNode.local ? peerUsageState(shownNode, picker.peer(shownNode.id)) : null;
  const usage = peerShown ? peerShown.usage : state.snapshot.value?.usage;
  const pill = multi ? html`<${UsageNodePill} nodes=${nodes} selected=${shownNode?.id || selfID} onSelect=${picker.select} onOpen=${picker.refreshAll}
    peakOf=${(id) => {
      if (id === selfID) return { value: usagePeak(state.snapshot.value?.usage) };
      const node = nodes.find((n) => n.id === id);
      const st = peerUsageState(node, picker.peer(id));
      return st.why ? st : { value: usagePeak(st.usage) };
    }} />` : null;
  if (peerShown?.why) return html`<span id="usage" class="meta na">${pill}<span title=${peerShown.why}>${peerShown.pending ? peerShown.why : `usage not shared · ${shownNode.name}`}</span></span>`;
  const savedModes = Object.fromEntries((usageView(usage).lines || [])
    .map((line) => [line.key, dashPrefs.getItem(prefKey(line.key)) || 'usage']));
  const view = usageView(usage, { ...savedModes, ...modes });
  const setMode = (provider, mode) => {
    setModes((current) => ({ ...current, [provider]: mode }));
    dashPrefs.setItem(prefKey(provider), mode);
  };
  // A peer view (remote-node.js) of a peer that does not share usage: say so
  // rather than implying the node has none.
  const remote = globalThis.__tclaudeRemoteNode?.id;
  // The peer zeroes usage it does not share (it still serializes as
  // {available:false}), so ask its peer_view metadata, not the payload.
  if (remote && featureState('costs', state.snapshot.value?.peer_view) !== 'shared') {
    const name = globalThis.document?.documentElement?.dataset.remoteNodeName || remote.slice(0, 13);
    return html`<span id="usage" class="meta na peer-view-na" title=${`${name} does not share usage or costs with you${needsSuffix([featureNeeds('costs', state.snapshot.value?.peer_view)])}`}>usage and costs not shared by ${name}</span>`;
  }
  if (view.na) return html`<span id="usage" class="meta na" title=${view.title}>${pill}${view.text}</span>`;
  const readout = html`
    <span id="usage" class=${`meta${view.multiline ? ' multiline' : ''}`} title=${view.title}>
      ${view.lines.map((line) => view.multiline ? html`
        <span key=${line.key} class="uline">
          <span class="usage-source">
            <span class="usage-unit-switch-slot">
              ${line.modes?.length ? html`<span class="usage-unit-switch" role="group" aria-label=${`${line.label.replace(':', '')} usage units`}>
                ${line.modes.map((mode) => html`<button type="button" class=${line.mode === mode ? 'active' : ''}
                  aria-pressed=${line.mode === mode} onClick=${() => setMode(line.key, mode)}>${mode === 'usage' ? '%' : mode === 'cost' ? '≈$' : 'AIC'}</button>`)}
              </span>` : null}
            </span>
            <span class="usrc">${line.label}${line.whatIf ? html`<span class="uwhatif" title=${WHAT_IF_HINT}
              aria-label=${WHAT_IF_HINT}>?</span>` : null}</span>
          </span>
          <span class="usage-values">
            ${line.tokens.map((token) => html`<${UsageToken} key=${token.key} token=${token} />`)}
          </span>
        </span>
      ` : line.tokens.map((token) => html`<${UsageToken} key=${token.key} token=${token} />`))}
    </span>
  `;
  // Beside the two-line readout, not above it: the header keeps its height.
  return pill ? html`<span class="usage-with-node">${pill}${readout}</span>` : readout;
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
  // The hint opens the quick reader on the first unread notification. It is a
  // sibling of the activity trigger rather than inside it — a button may not
  // nest another — and stays a plain marker when no unread row names an agent
  // to open (only the count is known, or only daemon notices such as worktree
  // cleanup, which carry no sender for the reader to key on).
  const first = hasNotifications
    ? unreadHumanMessages(snapshot).find((message) => message.from_agent || message.from_conv) || null
    : null;
  const openReader = (event) => {
    event.preventDefault();
    event.stopPropagation();
    openHumanNotificationReader({
      sender: { agent: first.from_agent || '', conv: first.from_conv || '', label: first.from_title || '' },
      messageID: first.id,
      launcher: event.currentTarget,
      returnFocus: event.currentTarget.parentElement?.querySelector('.activity-hover-trigger') || null,
    });
  };
  const hintLabel = 'One or more agents have unread notifications — open the latest in the quick reader';
  const badge = first
    ? html`<button type="button" class="human-notification-hint human-notification-hint-button"
        title=${hintLabel} aria-label=${hintLabel} onClick=${openReader}>!</button>`
    : hasNotifications
      ? html`<span class="human-notification-hint" aria-hidden="true"
          title="One or more agents have unread notifications">!</span>`
      : null;
  return html`
    <${ActivityHover}
      id="global-activity"
      className="global-activity"
      label=${label}
      title=${view.title || null}
      details=${view.details}
      wizard=${wizard}
      badge=${badge}
    >
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

function ignoredUntilLabel(ignored) {
  return ignored.until ? `ignored until ${new Date(ignored.until).toLocaleDateString()}` : 'ignored permanently';
}

// A closed/merged row is dotted by its terminal state instead of its CI state
// — checks on something already landed say nothing useful — and it drops the
// "no active agent" note, which only means something for open work. An
// ignored row says until when instead, and offers only its way back.
function OpenPRRow({ pr, showFinalCI = false, titleDetails = true, onIgnore = null, onRestore = null }) {
  const terminal = pr.state === 'merged' || pr.state === 'closed';
  const draft = !!pr.draft && !terminal;
  const ignored = pr.ignored || null;
  const dot = terminal ? pr.state : (draft ? 'draft' : (pr?.checks?.state || 'unknown'));
  const titleText = pr.title || `Pull request #${pr.number}`;
  const prState = terminal ? pr.state : (draft ? 'draft' : 'open');
  return html`
    <li class=${`open-pr-row${draft ? ' is-draft' : ''}${ignored ? ' is-ignored' : ''}`}>
      <span class=${`open-pr-state open-pr-state-${dot}`} aria-hidden="true"></span>
      <span class="open-pr-main">
        ${titleDetails
          ? html`<${PRLinkHover} url=${pr.url} prNumber=${pr.number} title=${pr.title || ''} prState=${prState}
              linkClass="open-pr-title" label=${titleText} describe=${titleText} />`
          : html`<a class="open-pr-title" href=${pr.url} target="_blank" rel="noopener noreferrer">${titleText}</a>`}
        <span class="open-pr-meta">
          <span>${pr.repository}</span><span> · #${pr.number}</span>
          ${ignored ? html`<span> · ${ignoredUntilLabel(ignored)}</span>` : null}
          ${!ignored && terminal ? html`<span> · ${pr.state}${pr.closed_at ? ` ${new Date(pr.closed_at).toLocaleDateString()}` : ''}</span>` : null}
          ${!ignored && !terminal && pr.agent_title ? html`<span> · ${pr.agent_title}</span>` : null}
          ${!ignored && !terminal && !pr.agent_title ? html`<span> · no active agent</span>` : null}
          ${!ignored && !terminal && pr.local ? html`<span title="Known from local agent activity; not yet in GitHub's authored search"> · via agent</span>` : null}
          ${!ignored && draft ? html`<span class="open-pr-draft-label">Draft</span>` : null}
        </span>
      </span>
      <span class="open-pr-badges">
        ${ignored ? html`<button type="button" class="open-pr-restore"
            aria-label=${`Bring back pull request #${pr.number}`} onClick=${() => onRestore?.(pr)}>Restore</button>` : html`
          <${PRChecksBadge} url=${pr.url} prNumber=${pr.number}
            summary=${terminal && !showFinalCI ? null : pr.checks} />
          ${terminal || draft ? null : html`<${PRReviewBadge} url=${pr.url} prNumber=${pr.number} reviews=${pr.reviews} />`}
          ${onIgnore && !terminal ? html`<button type="button" class="open-pr-ignore"
            aria-label=${`Ignore pull request #${pr.number}…`} title="Ignore…"
            onClick=${() => onIgnore(pr)}>⊘</button>` : null}
        `}
      </span>
    </li>
  `;
}

// IgnorePRDialog confirms an ignore and picks how long it lasts. It is drawn
// inside the popover rather than as the page's modal, so the popover's own
// hover and outside-click handling keep treating it as part of the popover.
function IgnorePRDialog({ pr, onCancel, onConfirm }) {
  const [duration, setDuration] = useState('7d');
  const okRef = useRef(null);
  useLayoutEffect(() => { okRef.current?.focus(); }, []);
  const chosen = OPEN_PR_IGNORE_DURATIONS.find((d) => d.id === duration) || OPEN_PR_IGNORE_DURATIONS[0];
  return html`
    <div class="open-prs-confirm-backdrop" onClick=${(event) => { if (event.currentTarget === event.target) onCancel(); }}>
      <div class="open-prs-confirm" role="alertdialog" aria-modal="true"
        aria-labelledby="open-prs-confirm-title" aria-describedby="open-prs-confirm-body"
        onKeyDown=${(event) => {
          if (event.key === 'Tab') {
            // aria-modal: keep Tab inside the dialog.
            const focusable = [...event.currentTarget.querySelectorAll('input:checked, button')];
            const first = focusable[0];
            const last = focusable[focusable.length - 1];
            if (event.shiftKey ? document.activeElement === first : document.activeElement === last) {
              event.preventDefault();
              (event.shiftKey ? last : first)?.focus();
            }
            return;
          }
          if (event.key !== 'Escape') return;
          event.preventDefault();
          event.stopPropagation();
          onCancel();
        }}>
        <h4 id="open-prs-confirm-title">Ignore pull request #${pr.number}?</h4>
        <p id="open-prs-confirm-body">
          <span class="open-prs-confirm-pr">${pr.title || pr.url}</span>
          It leaves these lists and counts and waits under Ignored, where you can bring it back.
        </p>
        <fieldset class="open-prs-confirm-durations">
          <legend>Ignore for</legend>
          ${OPEN_PR_IGNORE_DURATIONS.map((d) => html`
            <label key=${d.id}>
              <input type="radio" name="open-prs-ignore-duration" value=${d.id}
                checked=${duration === d.id} onChange=${() => setDuration(d.id)} />
              <span>${d.label}</span>
            </label>
          `)}
        </fieldset>
        <div class="open-prs-confirm-buttons">
          <button type="button" onClick=${onCancel}>Cancel</button>
          <button ref=${okRef} type="button" class="confirm-danger" onClick=${() => onConfirm(chosen.ms)}>Ignore</button>
        </div>
      </div>
    </div>
  `;
}

function OpenPRs({ state }) {
  const [filter, setFilter] = useState('all');
  const [includeDraftsInAttention, setIncludeDraftsInAttention] = useState(
    () => dashPrefs.getItem(OPEN_PRS_INCLUDE_DRAFTS_PREF) === '1',
  );
  // Closed/merged work is history, so its last CI result stays quiet unless
  // the operator explicitly asks for it. This is intentionally local UI
  // state: enabling it reveals the cached snapshot summary but changes no
  // daemon setting and triggers no terminal-PR refresh on its own.
  const [showFinalCI, setShowFinalCI] = useState(false);
  const [ignored, setIgnored] = useState(() => parseIgnoredPRs(dashPrefs.getItem(OPEN_PRS_IGNORED_PREF)));
  const [confirmIgnore, setConfirmIgnore] = useState(null);
  // Where focus goes back to once the ignore dialog closes or a Restore
  // removes its own row: the ⊘ button that opened the dialog while it is
  // still mounted, the popover trigger otherwise.
  const returnFocus = useRef(null);
  const [focusBack, setFocusBack] = useState(0);
  const [titleDetails, setTitleDetails] = useState(
    () => dashPrefs.getItem(OPEN_PRS_TITLE_DETAILS_PREF) !== '0',
  );
  const [person, setPerson] = useState('');
  // The tracked list as the daemon last answered an edit, held until the
  // next snapshot (which the daemon builds from the saved list): a new tab
  // shows at once, a removed one goes at once.
  const [edited, setEdited] = useState(null);
  const [hovered, setHovered] = useState(false);
  const [pinned, setPinned] = useState(false);
  const rootRef = useRef(null);
  // In the fused Groups view the peers' agents are on screen too; their PRs
  // count as attached (the page's daemon only knows its own agents).
  const dock = fusedDock.value;
  const peers = dock ? (dock.nodes || []).filter((n) => n.id !== dock.page)
    .map((n) => ({ name: n.name, snapshot: dock.snapshots?.[n.id] })) : [];
  const view = authoredOpenPRsView(state.snapshot.value, filter, includeDraftsInAttention, peers, ignored);
  const snapshotAuthors = Array.isArray(state.snapshot.value?.tracked_pr_authors)
    ? state.snapshot.value.tracked_pr_authors.filter((entry) => GITHUB_LOGIN_RE.test(entry?.login || '')) : [];
  const editedAuthors = edited && edited.snapshot === state.snapshot.value ? edited.authors : null;
  const tracked = editedAuthors
    ? editedAuthors.map((login) => snapshotAuthors.find((entry) => entry.login.toLowerCase() === login.toLowerCase())
      || { login, available: false, items: [], recent: [] })
    : snapshotAuthors;
  const personEntry = person ? tracked.find((entry) => entry.login.toLowerCase() === person.toLowerCase()) : null;
  // A colleague's PRs are not this node's agents' work, so their tab has no
  // Unattached filter; the operator's own view keeps driving the trigger.
  const panelFilter = personEntry && filter === 'unattached' ? 'all' : filter;
  const colleagueView = (entry, which = 'all') => authoredOpenPRsView(
    { authored_open_prs: { ...entry, available: true } }, which, includeDraftsInAttention, peers, ignored,
    Date.now(), entry.login);
  const panel = personEntry ? colleagueView(personEntry, panelFilter) : view;
  const people = tracked.map((entry) => ({ login: entry.login, total: entry.available ? colleagueView(entry).total : null }));
  const addPerson = async (login) => {
    const authors = await postTrackedAuthor('add', login);
    setEdited({ authors, snapshot: state.snapshot.value });
  };
  const removePerson = async (login) => {
    try {
      const authors = await postTrackedAuthor('remove', login);
      setEdited({ authors, snapshot: state.snapshot.value });
      if (person.toLowerCase() === login.toLowerCase()) setPerson('');
    } catch (_) {
      // The tab stays; the next snapshot shows the daemon's list either way.
    }
  };
  const saveIgnored = (next) => {
    const value = serializeIgnoredPRs(next);
    // Re-read what was stored: serialization may have evicted old entries.
    setIgnored(parseIgnoredPRs(value));
    if (value) dashPrefs.setItem(OPEN_PRS_IGNORED_PREF, value);
    else dashPrefs.removeItem(OPEN_PRS_IGNORED_PREF);
  };
  const askIgnore = (pr) => {
    returnFocus.current = document.activeElement;
    setConfirmIgnore({ pr, owner: personEntry ? personEntry.login : '' });
  };
  const closeIgnoreDialog = () => {
    setConfirmIgnore(null);
    setFocusBack((n) => n + 1);
  };
  // The indicator is permanent by default (dashboard.always_show_open_prs), so
  // the popover must open at zero open PRs too — that is where the recently
  // closed list lives. It still stays out of the footer entirely until the
  // daemon has resolved a GitHub identity, since a fixed "Open PRs 0" would be
  // a lie when `gh` is missing or logged out.
  // Ignored PRs keep it reachable too: they can only be brought back from it.
  const visible = view.available && (view.total > 0 || view.alwaysShow || view.ignoredCount > 0);
  // An open ignore confirmation holds the popover open: the pointer may
  // wander off while the operator picks a duration.
  const open = visible && (hovered || pinned || !!confirmIgnore);

  const hoverClose = useLeaveGrace(() => setHovered(false));
  const cancelHoverClose = hoverClose.cancel;
  const enter = () => {
    cancelHoverClose();
    setHovered(true);
  };
  const leave = hoverClose.schedule;
  // Pinned, or holding an ignore confirmation, it stays put when another
  // popover opens; hover-opened, it gives way.
  useExclusivePopover(rootRef, open, () => { cancelHoverClose(); setHovered(false); }, pinned || !!confirmIgnore);
  useEffect(() => {
    if (!focusBack) return;
    const target = returnFocus.current;
    returnFocus.current = null;
    if (target?.isConnected && rootRef.current?.contains(target)) target.focus();
    else rootRef.current?.querySelector('.open-prs-trigger')?.focus();
  }, [focusBack]);

  useDismissablePopover(rootRef, pinned || !!confirmIgnore, () => {
    cancelHoverClose();
    setPinned(false);
    setHovered(false);
    setConfirmIgnore(null);
  }, { escape: false, hold: () => !!confirmIgnore });
  useEffect(() => {
    if (!open) return undefined;
    const escape = (event) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      if (confirmIgnore) {
        closeIgnoreDialog();
        return;
      }
      setPinned(false);
      setHovered(false);
      rootRef.current?.querySelector('.open-prs-trigger')?.focus();
    };
    document.addEventListener('keydown', escape);
    return () => document.removeEventListener('keydown', escape);
  }, [open, confirmIgnore]);

  if (!visible) return null;
  // A rolling 24h window is not "today" — a PR closed at 23:00 yesterday is in
  // it — so every window reads as a span, including the 1-day one.
  const recentLabel = `Closed ${(personEntry ? panel : view).recentWindowDays}d`;
  // "recent" selected while the window is configured off falls back to the
  // open list, so the Open chip — not a vanished chip — must read as active.
  const openFilterActive = !panel.showingRecent && !panel.showingIgnored && panelFilter !== 'attention' && panelFilter !== 'ready' && panelFilter !== 'unattached';
  const panelAge = panel.updatedAt ? new Date(panel.updatedAt).toLocaleTimeString() : '';
  const heading = personEntry ? `${personEntry.login}’s pull requests` : 'Your pull requests';
  // A colleague not searched yet, or one GitHub refused, has no list to show.
  const panelNotice = personEntry && !personEntry.available
    ? (personEntry.error ? `GitHub could not search ${personEntry.login}’s pull requests: ${personEntry.error}`
      : `Searching GitHub for ${personEntry.login}’s pull requests…`)
    : '';
  // A refusal after an earlier good search keeps that list, marked stale.
  const panelWarning = personEntry?.available && personEntry.error
    ? `Last search failed (${personEntry.error}); showing the list from ${panelAge || 'earlier'}.` : '';
  return html`
    <span ref=${rootRef} class=${`open-prs${open ? ' is-open' : ''}${view.total > 0 ? '' : ' is-empty'}${view.attention > 0 ? ' has-attention' : ''}`}
      onMouseEnter=${enter} onMouseLeave=${leave}>
      <button type="button" class="open-prs-trigger" aria-haspopup="dialog"
        aria-expanded=${open ? 'true' : 'false'} aria-controls="open-prs-popover"
        onClick=${() => setPinned((value) => !value)}>
        <span class="open-prs-dot" aria-hidden="true"></span>
        <span>Open PRs</span><span class="open-prs-count">${view.total}</span>
        <span class="open-prs-chevron" aria-hidden="true">⌃</span>
      </button>
      ${open ? html`
        <div id="open-prs-popover" class="open-prs-popover" role="dialog" aria-label=${heading}>
          <div class="open-prs-head"><strong>${heading}</strong><span class="open-prs-count">${panel.total}</span>${panelAge ? html`<span class="open-prs-age">updated ${panelAge}</span>` : null}</div>
          <${PeopleTabs} active=${personEntry ? personEntry.login : ''} mine=${view.total} people=${people}
            onSelect=${setPerson} onAdd=${addPerson} onRemove=${removePerson} />
          ${panelWarning ? html`<p class="open-prs-stale" role="status">${panelWarning}</p>` : null}
          ${panelNotice ? html`<p class="open-prs-empty">${panelNotice}</p>` : panel.items.length
            ? html`<ul class="open-pr-list">${panel.items.map((pr) => html`<${OpenPRRow} key=${pr.url} pr=${pr}
                showFinalCI=${panel.showingRecent && showFinalCI} titleDetails=${titleDetails}
                onIgnore=${askIgnore} onRestore=${(target) => {
                  saveIgnored(unignorePR(ignored, target.url));
                  setFocusBack((n) => n + 1);
                }} />`)}</ul>`
            : html`<p class="open-prs-empty">${panel.showingIgnored
              ? 'No ignored pull requests.'
              : panel.showingRecent
              ? `Nothing merged or closed in the last ${panel.recentWindowDays} day(s).`
              : (openFilterActive && panel.total <= 0 ? 'No open pull requests.' : 'No pull requests match this filter.')}</p>`}
          <div class="open-prs-filters" role="group" aria-label="Filter pull requests">
            <button class=${openFilterActive ? 'active' : ''} aria-pressed=${openFilterActive} onClick=${() => setFilter('all')}>Open ${panel.total}</button>
            <button class=${filter === 'attention' ? 'active' : ''} aria-pressed=${filter === 'attention'} onClick=${() => setFilter('attention')}>Needs attention ${panel.attention}</button>
            <button class=${filter === 'ready' ? 'active' : ''} aria-pressed=${filter === 'ready'}
              title="Approved with all CI checks passing"
              onClick=${() => setFilter('ready')}>Ready to merge ${panel.ready}</button>
            ${personEntry ? null : html`<button class=${filter === 'unattached' ? 'active' : ''} aria-pressed=${filter === 'unattached'} onClick=${() => setFilter('unattached')}>Unattached ${panel.unattached}</button>`}
            ${panel.recentWindowDays > 0 ? html`<button class=${panel.showingRecent ? 'active' : ''} aria-pressed=${panel.showingRecent}
              title=${`Pull requests ${personEntry ? `${personEntry.login} merged or closed` : 'you merged or closed'} in the last ${panel.recentWindowDays} day(s)`}
              onClick=${() => setFilter('recent')}>${recentLabel} ${panel.recentCount}</button>` : null}
            ${panel.ignoredCount > 0 || panel.showingIgnored ? html`<button class=${panel.showingIgnored ? 'active' : ''} aria-pressed=${panel.showingIgnored}
              title="Pull requests you set aside; bring them back from here"
              onClick=${() => setFilter('ignored')}>Ignored ${panel.ignoredCount}</button>` : null}
          </div>
          <div class="open-prs-foot">
            <span>${panel.truncated ? html`<span>Showing the first ${panel.items.length} · </span>` : null}${panel.searchURL ? html`<a href=${panel.searchURL} target="_blank" rel="noopener noreferrer">${panel.showingRecent ? 'See them all on GitHub ↗' : 'Open all on GitHub ↗'}</a>` : null}</span>
            ${panel.showingRecent ? html`<label class="open-prs-final-ci"
              title="Show the last cached CI result. Closed and merged pull requests are never refreshed.">
              <input type="checkbox" checked=${showFinalCI}
                onChange=${(event) => setShowFinalCI(event.currentTarget.checked)} />
              <span>Final CI</span>
            </label>` : null}
            ${filter === 'attention' && panel.hasDrafts ? html`<label class="open-prs-include-drafts"
              title="Include draft pull requests in Needs attention">
              <input type="checkbox" checked=${includeDraftsInAttention}
                onChange=${(event) => {
                  const checked = event.currentTarget.checked;
                  setIncludeDraftsInAttention(checked);
                  dashPrefs.setItem(OPEN_PRS_INCLUDE_DRAFTS_PREF, checked ? '1' : '0');
                }} />
              <span>Include drafts</span>
            </label>` : null}
            <label class="open-prs-title-details"
              title="Show a pull request's details card when hovering its title">
              <input type="checkbox" checked=${titleDetails}
                onChange=${(event) => {
                  const checked = event.currentTarget.checked;
                  setTitleDetails(checked);
                  dashPrefs.setItem(OPEN_PRS_TITLE_DETAILS_PREF, checked ? '1' : '0');
                }} />
              <span>Hover details</span>
            </label>
          </div>
          ${confirmIgnore ? html`<${IgnorePRDialog} pr=${confirmIgnore.pr}
            onCancel=${closeIgnoreDialog}
            onConfirm=${(ms) => {
              saveIgnored(ignorePR(ignored, confirmIgnore.pr, ms, Date.now(), confirmIgnore.owner));
              closeIgnoreDialog();
            }} />` : null}
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
    if (!model || busy) return;
    okRef.current?.focus();
  }, [model?.seq, busy]);
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
        ${model?.checks?.length ? html`<div class="confirm-checks">
          ${model.checks.map((check, i) => html`<label key=${i} class="confirm-check">
            <input type="checkbox" checked=${!!check.checked} disabled=${busy}
              onChange=${(event) => {
                check.checked = event.currentTarget.checked;
                feedback.confirmation.value = { ...model };
              }} />
            <span>${check.label}${check.hint ? html`<span class="confirm-check-hint">${check.hint}</span>` : null}</span>
          </label>`)}
        </div>` : null}
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
    [hosts.spendLimitHost, html`<${SpendLimitBanner} state=${state} />`],
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
