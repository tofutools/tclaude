import { h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { GROUP_VIEW_OPTIONS } from './groups-state.js';
import { trustedHTMLToVNodes } from './html-vnodes.js';
import { syncBotAnimations, syncWizardOrbit } from './helpers.js';

const html = htm.bind(h);

function ViewOption({ option, state, queueRefresh }) {
  const checked = state.visibility.value[option.key];
  return html`
    <label class="filter-toggle" title=${option.title}>
      <input
        id=${`filter-groups-${option.key}`}
        type="checkbox"
        checked=${checked}
        onChange=${(event) => {
          state.setVisible(option.key, event.currentTarget.checked);
          queueRefresh();
        }}
      />
      <span>${option.label}</span>
    </label>
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

  const count = current.query
    ? `${current.shownReal} / ${current.total}`
    : `${current.total} group${current.total === 1 ? '' : 's'}`;

  return html`
    <input
      ref=${inputRef}
      id="filter-groups"
      type="text"
      aria-label="Filter groups"
      placeholder="Filter (group name + member title/role/descr/cwd/branch)"
      autocomplete="off"
      spellcheck=${false}
      value=${current.query}
      onInput=${(event) => {
        state.setQuery(event.currentTarget.value);
        queueRefresh();
      }}
    />
    <span class="filter-count" id="filter-groups-count" aria-live="polite">${count}</span>
    <button
      class="clear-filter"
      id="filter-groups-clear"
      title="Clear filter"
      aria-label="Clear group filter"
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
        title="Choose which members and virtual groups to show on this tab"
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
          />
        `)}
        <div class="view-menu-sep" role="separator"></div>
        <div
          class="view-menu-heading"
          title="Show or hide individual columns in the member tables. The Name and controls columns always stay. Hiding the ID column moves its agent-id / conv-id hover onto the Name."
        >Columns</div>
        <div id="filter-groups-cols" class="view-cols">
          ${columns.map((column) => html`
            <label class="filter-toggle" title=${`Show the "${column.label}" column`} key=${column.key}>
              <input
                id=${`filter-groups-col-${column.key}`}
                type="checkbox"
                checked=${column.shown}
                onChange=${(event) => state.setColumnShown(
                  column.key, event.currentTarget.checked,
                )}
              />
              <span>${column.label}</span>
            </label>
          `)}
        </div>
      </div>
    </div>
  `;
}

export function GroupsList({ host, state, actions, renderGroupsHTML }) {
  const current = state.view.value;
  const markup = renderGroupsHTML(current.groups);

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

  return trustedHTMLToVNodes(markup);
}

export function mountGroupsIsland({
  filterHost, listHost, state, actions, renderGroupsHTML, registerCleanup,
}) {
  state.initialize();
  render(html`<${GroupsControls} state=${state} actions=${actions} />`, filterHost);
  registerCleanup(() => render(null, filterHost));
  render(html`
    <${GroupsList}
      host=${listHost}
      state=${state}
      actions=${actions}
      renderGroupsHTML=${renderGroupsHTML}
    />
  `, listHost);
  registerCleanup(() => render(null, listHost));
}
