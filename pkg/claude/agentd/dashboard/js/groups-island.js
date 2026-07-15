import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { GROUP_VIEW_OPTIONS } from './groups-state.js';
import { GroupsNativeList } from './groups-list.js';
import { syncBotAnimations, syncWizardOrbit } from './helpers.js';
import { isWizardActive } from './slop.js';

const html = htm.bind(h);

// The Groups island stays mounted while the cosmetic theme cycles. Copy pairs
// switch through CSS, but placeholder/title/aria attributes are single-valued
// and the legacy list renderer chooses them at render time, so both bounded
// components subscribe to the wizard edge and repaint immediately.
function useWizardTheme() {
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
  const current = state.view.value;
  const badge = state.deviationCount.value;
  const columns = state.columnOptions.value;
  const wizard = useWizardTheme();

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

  const regularCount = current.query
    ? `${current.shownReal} / ${current.total}`
    : `${current.total} group${current.total === 1 ? '' : 's'}`;
  const wizardCount = current.query
    ? `${current.shownReal} / ${current.total}`
    : `${current.total} ${current.total === 1 ? 'party' : 'parties'}`;

  return html`
    <input
      ref=${inputRef}
      id="filter-groups"
      type="text"
      aria-label=${wizard ? 'Filter parties' : 'Filter groups'}
      placeholder=${wizard
        ? 'Filter (party name + familiar title/class/lore/grove/branch)'
        : 'Filter (group name + member title/role/descr/cwd/branch)'}
      autocomplete="off"
      spellcheck=${false}
      value=${current.query}
      onInput=${(event) => {
        state.setQuery(event.currentTarget.value);
        queueRefresh();
      }}
    />
    <span class="filter-count" id="filter-groups-count" aria-live="polite">
      <span class="theme-copy-regular">${regularCount}</span>
      <span class="theme-copy-wizard">${wizardCount}</span>
    </span>
    <button
      class="clear-filter"
      id="filter-groups-clear"
      title=${wizard ? 'Clear party filter' : 'Clear filter'}
      aria-label=${wizard ? 'Clear party filter' : 'Clear group filter'}
      onClick=${() => {
        state.setQuery('');
        queueRefresh();
        inputRef.current?.focus();
      }}
    >×</button>
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
        ${GROUP_VIEW_OPTIONS.map((option) => html`
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

export function GroupsList({ host, state, actions, presentation }) {
  useWizardTheme();
  const current = state.view.value;

  useEffect(() => {
    syncBotAnimations();
    syncWizardOrbit();
  });

  useEffect(() => {
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

  return html`<${GroupsNativeList} groups=${current.groups} presentation=${presentation} snapshot=${state.snapshot.value} />`;
}

export function mountGroupsIsland({
  filterHost, listHost, state, actions, presentation, registerCleanup,
}) {
  state.initialize();
  render(html`<${GroupsControls} state=${state} actions=${actions} />`, filterHost);
  registerCleanup(() => render(null, filterHost));
  render(html`
    <${GroupsList}
      host=${listHost}
      state=${state}
      actions=${actions}
      presentation=${presentation}
    />
  `, listHost);
  registerCleanup(() => render(null, listHost));
}
