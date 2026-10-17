import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { GROUP_VIEW_OPTIONS } from './groups-state.js';
import { GroupsNativeList } from './groups-list.js';
import { GroupStandingOrdersDialog } from './groups-standing-orders-dialog.js';
import { GroupsInteractionProvider } from './groups-interactions.js';
import { syncBotAnimations, syncWizardOrbit } from './helpers.js';
import { isWizardActive } from './slop.js';
import {
  GroupsFocusSwitch, effectiveGroupsFocus, useGroupsFocusRestore,
} from './groups-focus.js';
import { GroupsProcessesFocus } from './groups-processes.js';
import { processContext } from './process-context.js';

const html = htm.bind(h);

// The Groups island stays mounted while the cosmetic theme cycles. Copy pairs
// switch through CSS, but placeholder/title/aria attributes are single-valued
// and the native list chooses them at render time, so both bounded
// components subscribe to the wizard edge and repaint immediately.
export function useWizardTheme() {
  const [wizard, setWizard] = useState(isWizardActive());
  useEffect(() => {
    const onWizard = (event) => setWizard(
      event.detail?.active == null ? isWizardActive() : Boolean(event.detail.active),
    );
    document.addEventListener('tclaude:wizard', onWizard);
    return () => document.removeEventListener('tclaude:wizard', onWizard);
  }, []);
  return wizard;
}

function ViewOption({ option, state, queueRefresh, wizard }) {
  const checked = state.visibility.value[option.key];
  return html`
    <label class="filter-toggle" title=${wizard ? (option.wizardTitle || option.title) : option.title}>
      <input
        id=${`filter-groups-${option.key}`}
        type="checkbox"
        checked=${checked}
        onChange=${(event) => {
          state.setVisible(option.key, event.currentTarget.checked);
          queueRefresh();
        }}
      />
      <span><span class="theme-copy-regular">${option.label}</span><span class="theme-copy-wizard">${option.wizardLabel || option.label}</span></span>
    </label>
  `;
}

function ColumnLabel({ column }) {
  if (!column.wizardLabel) return column.label;
  return html`
    <span class="theme-copy-regular">${column.label}</span>
    <span class="theme-copy-wizard">${column.wizardLabel}</span>
  `;
}

export function GroupsControls({ state, actions }) {
  const inputRef = useRef(null);
  const refreshTimer = useRef(null);
  const menuRef = useRef(null);
  const buttonRef = useRef(null);
  // While the fused Groups view is on screen these controls drive it, and
  // count what it lists.
  const listed = state.listed.value;
  const current = listed ? { ...state.view.value, ...listed } : state.view.value;
  // The node-only options are not offered while fused, so their changes do
  // not count there.
  const badge = state.deviationCount.value - (listed ? GROUP_VIEW_OPTIONS
    .filter((option) => option.nodeOnly && state.visibility.value[option.key] !== option.defaultValue).length : 0);
  const columns = state.columnOptions.value;
  const wizard = useWizardTheme();
  useGroupsFocusRestore(state);

  const queueRefresh = () => {
    clearTimeout(refreshTimer.current);
    refreshTimer.current = setTimeout(() => void actions.refresh(), 250);
  };

  useEffect(() => {
    const close = (focusButton = false) => {
      if (!state.viewOpen.value) return;
      state.viewOpen.value = false;
      if (focusButton) buttonRef.current?.focus();
    };
    const onClick = (event) => {
      if (!menuRef.current?.parentElement?.contains(event.target)) close();
    };
    const onKeyDown = (event) => {
      if (event.key !== 'Escape' || !state.viewOpen.value) return;
      event.preventDefault();
      close(true);
    };
    document.addEventListener('click', onClick);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      clearTimeout(refreshTimer.current);
      document.removeEventListener('click', onClick);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, []);

  // The match count and × live inside the search box, shown only while
  // filtering. The count's live region stays mounted (hidden, empty) so the
  // first count after typing is announced.
  const filtering = Boolean(current.query);

  return html`
    <span class=${`groups-search${filtering ? ' filtering' : ''}`}>
      <span class="groups-search-icon" aria-hidden="true">🔍</span>
      <input
        ref=${inputRef}
        id="filter-groups"
        type="text"
        aria-label=${wizard ? 'Filter parties' : 'Filter groups'}
        placeholder=${wizard ? 'Filter parties & familiars' : 'Filter groups & members'}
        title=${wizard
          ? 'Matches party name + familiar title/class/lore/grove/branch'
          : 'Matches group name + member title/role/descr/cwd/branch'}
        autocomplete="off"
        spellcheck=${false}
        value=${current.query}
        onInput=${(event) => {
          state.setQuery(event.currentTarget.value);
          queueRefresh();
        }}
      />
      <span class="filter-count" id="filter-groups-count" aria-live="polite" hidden=${!filtering}
        title=${wizard ? 'Parties shown / all parties' : 'Groups shown / all groups'}
      >${filtering ? `${current.shownReal} / ${current.total}` : ''}</span>
      ${filtering && html`
        <button
          class="clear-filter"
          id="filter-groups-clear"
          type="button"
          title=${wizard ? 'Clear party filter' : 'Clear filter'}
          aria-label=${wizard ? 'Clear party filter' : 'Clear group filter'}
          onClick=${() => {
            state.setQuery('');
            queueRefresh();
            inputRef.current?.focus();
          }}
        >×</button>
      `}
    </span>
    <${GroupsFocusSwitch} state=${state} groups=${current.groups} wizard=${wizard} />
    <div class="view-popover-wrap">
      <button
        ref=${buttonRef}
        id="filter-groups-view-btn"
        class="view-btn"
        type="button"
        aria-haspopup="menu"
        aria-expanded=${state.viewOpen.value ? 'true' : 'false'}
        aria-controls="filter-groups-view-menu"
        title=${wizard
          ? 'Choose which familiars and ethereal parties to show on this tab'
          : 'Choose which members and virtual groups to show on this tab'}
        onClick=${() => { state.viewOpen.value = !state.viewOpen.value; }}
      >
        ▾ view
        <span id="filter-groups-view-badge" class="view-badge" hidden=${badge === 0}>${badge || ''}</span>
      </button>
      <div
        ref=${menuRef}
        id="filter-groups-view-menu"
        class=${`view-menu${state.viewOpen.value ? ' open' : ''}`}
        role="menu"
      >
        ${GROUP_VIEW_OPTIONS.filter((option) => !listed || !option.nodeOnly).map((option) => html`
          <${ViewOption}
            key=${option.key}
            option=${option}
            state=${state}
            queueRefresh=${queueRefresh}
            wizard=${wizard}
          />
        `)}
        <div class="view-menu-sep" role="separator"></div>
        <div
          class="view-menu-heading"
          title="Show or hide individual columns in the member tables. The Name and controls columns always stay. Hiding the ID column moves its agent-id / conv-id hover onto the Name."
        >Columns</div>
        <div id="filter-groups-cols" class="view-cols">
          ${columns.map((column) => html`
            <label
              class="filter-toggle"
              title=${`Show the "${wizard && column.wizardLabel ? column.wizardLabel : column.label}" column`}
              key=${column.key}
            >
              <input
                id=${`filter-groups-col-${column.key}`}
                type="checkbox"
                checked=${column.shown}
                onChange=${(event) => state.setColumnShown(
                  column.key, event.currentTarget.checked,
                )}
              />
              <span><${ColumnLabel} column=${column} /></span>
            </label>
          `)}
        </div>
      </div>
    </div>
  `;
}

// useGroupsTableControls wires the member tables' sort headers and the
// virtual groups' pagers under `host`.
export function useGroupsTableControls(host, state, actions) {
  useEffect(() => {
    if (!host) return undefined;
    const onClick = (event) => {
      const sortHeader = event.target.closest('th[data-sort-table]');
      if (sortHeader && host.contains(sortHeader)) {
        actions.sort(sortHeader.dataset.sortTable, sortHeader.dataset.sortCol);
        return;
      }
      const pager = event.target.closest('button[data-pager]');
      if (!pager || pager.disabled || !host.contains(pager)) return;
      const kind = pager.dataset.list;
      const total = state.snapshot.value?.paging?.[kind]?.total || 0;
      actions.page(kind, pager.dataset.pager, total);
    };
    const onChange = (event) => {
      const pager = event.target.closest('select[data-pager="size"]');
      if (!pager || !host.contains(pager)) return;
      actions.setPageSize(pager.dataset.list, pager.value);
    };
    host.addEventListener('click', onClick);
    host.addEventListener('change', onChange);
    return () => {
      host.removeEventListener('click', onClick);
      host.removeEventListener('change', onChange);
    };
  }, [host]);
}

// useGroupsHover is the group header the pointer is over, under `host`.
// Hover is local Groups presentation state. Delegate from the stable island
// host so keyed group nodes can move across polls without acquiring their
// own listeners. relatedTarget suppresses churn while the pointer moves
// between descendants of the same summary.
export function useGroupsHover(host, groups) {
  const [hoveredGroupKey, setHoveredGroupKey] = useState(null);
  useEffect(() => {
    if (!host) return undefined;
    const summaryFor = (node) => node?.closest?.('details[data-group-key] > summary') || null;
    const onMouseOver = (event) => {
      const summary = summaryFor(event.target);
      if (summary === summaryFor(event.relatedTarget)) return;
      setHoveredGroupKey(summary?.parentElement?.getAttribute('data-group-key') || null);
    };
    const onMouseLeave = () => setHoveredGroupKey(null);
    host.addEventListener('mouseover', onMouseOver);
    host.addEventListener('mouseleave', onMouseLeave);
    return () => {
      host.removeEventListener('mouseover', onMouseOver);
      host.removeEventListener('mouseleave', onMouseLeave);
    };
  }, [host]);

  useEffect(() => {
    if (!hoveredGroupKey) return;
    const present = groups.some((group) =>
      (group.key || group.name) === hoveredGroupKey);
    if (!present) setHoveredGroupKey(null);
  }, [groups, hoveredGroupKey]);
  return hoveredGroupKey;
}

// GroupsLensList is the Groups listing through the chosen lens: the one
// groups tree, framed by the Processes or Both lens when one is chosen. One
// node's Groups view and the fused Groups view both render it, from their
// own groups, snapshot and actions; `rosters` are the groups as the run
// context names them (see groups-process-scope.js).
export function GroupsLensList({
  state, groups, rosters, snapshot, actions, triggers = [], hoveredGroupKey = null, processSource = undefined, wizard,
  focus = effectiveGroupsFocus(state),
}) {
  // Every lens renders the same groups tree; Processes frames it and swaps
  // each group's member details for its runs, Both frames it and shows the
  // two as sections of each group.
  const tree = html`<${GroupsNativeList} groups=${groups} snapshot=${snapshot} actions=${actions}
    triggers=${triggers} hoveredGroupKey=${hoveredGroupKey} />`;
  if (focus === 'agents') return tree;
  return html`<${GroupsProcessesFocus} groups=${groups} allGroups=${rosters} wizard=${wizard}
    filter=${state.processFilter.value} source=${processSource} fused=${focus === 'both'}
    focusedRun=${state.focusedRun.value} onFocusRun=${state.setFocusedRun}>${tree}<//>`;
}

export function GroupsList({ host, state, actions, processSource = undefined }) {
  const wizard = useWizardTheme();
  const focus = effectiveGroupsFocus(state);
  const [triggers, setTriggers] = useState([]);
  const current = state.view.value;
  const triggersEnabled = state.snapshot.value?.triggers_enabled === true;

  useEffect(() => {
    syncBotAnimations();
    syncWizardOrbit();
  });

  useEffect(() => {
    if (!triggersEnabled) {
      setTriggers([]);
      return undefined;
    }
    if (typeof actions.loadTriggers !== 'function') return undefined;
    let active = true;
    actions.loadTriggers().then((rows) => { if (active) setTriggers(rows); })
      .catch((error) => actions.reportError(error));
    return () => { active = false; };
  }, [triggersEnabled]);

  useGroupsTableControls(host, state, actions);
  const hoveredGroupKey = useGroupsHover(host, current.groups);

  // Both lenses read the process context: Processes for its cards, Agents for
  // the G1 header chips and row badges. One fetch per snapshot poll.
  const processesEnabled = state.snapshot.value?.processes_enabled === true;
  useEffect(() => {
    if (!processesEnabled) return undefined;
    const source = processSource || processContext;
    void source.refresh();
    const poll = () => { void source.refresh(); };
    document.addEventListener('tclaude:snapshot', poll);
    return () => document.removeEventListener('tclaude:snapshot', poll);
  }, [processesEnabled, processSource]);
  // The Process member column is available only while processes are on
  // (body.hide-processes, which signals cannot track), so the column menu's
  // cached options recompute when the feature flips.
  const processesSeen = useRef(processesEnabled);
  useEffect(() => {
    if (processesSeen.current === processesEnabled) return;
    processesSeen.current = processesEnabled;
    state.rerender();
  }, [processesEnabled]);

  // The drag-and-drop hints and member-table chrome belong to the Agents lens.
  useEffect(() => {
    document.body.classList.toggle('groups-focus-processes', focus === 'processes');
    return () => document.body.classList.remove('groups-focus-processes');
  }, [focus]);

  // While fused this list is hidden and the fused Groups view
  // (fleet-merged-island.js) shows the standing-orders dialog and the runs:
  // one copy only, so a process start handed to a group (takeProcessStart)
  // opens where it is seen.
  return html`<${GroupsInteractionProvider}>
    <${BrokerRefusalNotice} snapshot=${state.snapshot.value} />
    <${GroupsLensList} state=${state} groups=${current.groups} rosters=${state.snapshot.value?.groups} snapshot=${state.snapshot.value}
      actions=${actions} triggers=${triggers} hoveredGroupKey=${hoveredGroupKey} processSource=${processSource} wizard=${wizard}
      focus=${state.listed.value ? 'agents' : focus} />
    ${state.standingOrdersDialog.value && !document.documentElement.classList.contains('scope-fused') && html`
      <${GroupStandingOrdersDialog}
        key=${state.standingOrdersDialog.value.launchID}
        descriptor=${state.standingOrdersDialog.value}
        actions=${actions}
      />
    `}
  <//>`;
}

// BrokerRefusalNotice is the machine-level half of TCL-761, and the reason
// the per-agent 🚫 badge is not the whole feature.
//
// Two things it covers that a badge cannot. First, refusals the daemon could
// not place on any row at all — a brokered caller whose process ancestry
// reaches no recorded session. Second, and less obvious: a badge is attached
// to the row the daemon RESOLVED, which in the pid-reuse case is a dead
// session — so it can be recorded and then drawn nowhere, because the
// operator hides offline agents or that conv was never in a group. The total
// makes the condition visible even then.
//
// It stays a bare counter. The refused request names a session id, and
// putting THAT on screen would print an unauthenticated string from a caller
// the daemon just declined to identify — the same reason it is never allowed
// to decide which row gets a badge. So the notice says only that it is
// happening and where to go looking, which is all the daemon honestly knows.
//
// The × dismisses the instance on screen. The daemon keeps the dismissal (not
// localStorage, whose origin changes with the dashboard port) and brings the
// notice back as soon as a newer refusal is recorded; per-row badges stay.
export function BrokerRefusalNotice({ snapshot }) {
  const total = Number(snapshot?.broker_refusals_total || 0);
  const unplaceable = Number(snapshot?.broker_refusals_unplaceable || 0);
  // Hide at once on click rather than waiting for the next 2s poll; cleared
  // when the counts change, so a newer refusal shows without delay.
  const [dismissedAt, setDismissedAt] = useState(null);
  const key = `${total}/${unplaceable}`;
  if (dismissedAt !== null && dismissedAt !== key) setDismissedAt(null);
  if (dismissedAt === key) return null;
  // A harness can race one final callback against teardown after its session
  // row has gone away. That caller is necessarily unplaceable, and one lost
  // teardown event is not evidence that a live agent's telemetry is stuck.
  // Wait for a repeat before raising the machine-wide alarm. A refusal the
  // daemon DID place remains immediately actionable: it means a caller was
  // resolved to a row that disagreed with its claim, and that row may be
  // hidden even though its per-agent badge was recorded.
  const attributed = Math.max(0, total - unplaceable);
  if (!(total > 0 && (attributed > 0 || unplaceable > 1))) return null;
  const plural = total === 1 ? '' : 's';
  const title = 'These callbacks carry telemetry — status, cost, context, directory — for some agent, and it is being dropped;'
    + ' the affected agent keeps working, so nothing else on the dashboard will look wrong.'
    + ' agentd identifies a brokered caller by walking its process ancestry to a recorded session.'
    + ' Where that resolved to a row, the row is marked 🚫 — but the resolved row can be a dead session, which you will not see if offline agents are hidden or it was never in a group.'
    + ' Where it resolved to nothing there is no row to mark at all. Usual causes: a session row recorded against the same pid as a live agent’s pane, a sandboxed agent launched outside tclaude, or a row deleted while its pane kept running.'
    + ' The daemon log records each refusal with the caller pid.';
  const detail = unplaceable === total
    ? ` from ${total === 1 ? 'a caller' : 'callers'} it could not place at all`
    : unplaceable > 0
      ? ` (${unplaceable} from callers it could not place at all)`
      : '';
  const dismiss = async () => {
    setDismissedAt(key);
    try {
      const r = await fetch('/api/broker-refusals/dismiss', { method: 'POST', credentials: 'same-origin' });
      if (!r.ok) throw new Error(await r.text());
    } catch (err) {
      console.warn('dismissing the broker refusal notice failed', err);
      setDismissedAt(null);
    }
  };
  return html`<div class="broker-refusal-notice" role="status" title=${title}>
    <span class="broker-refusal-notice-glyph" aria-hidden="true">🚫</span>
    <span>agentd refused ${total} brokered callback${plural}${detail} in the last 15 minutes — telemetry for at least one agent is being lost. Look for a 🚫 on an agent row; if there is none, the affected row is hidden or unplaceable, and the daemon log has the caller pid.</span>
    <button type="button" class="broker-refusal-notice-dismiss" aria-label="Dismiss this warning"
      title="Dismiss this warning. It comes back if agentd refuses another callback." onClick=${dismiss}>×</button>
  </div>`;
}

export function mountGroupsIsland({
  filterHost, listHost, state, actions, registerCleanup,
}) {
  state.initialize();
  render(html`<${GroupsControls} state=${state} actions=${actions} />`, filterHost);
  registerCleanup(() => render(null, filterHost));
  render(html`
    <${GroupsList}
      host=${listHost}
      state=${state}
      actions=${actions}
    />
  `, listHost);
  registerCleanup(() => render(null, listHost));
}
