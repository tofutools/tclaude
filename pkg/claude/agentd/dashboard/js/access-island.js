import { Fragment, h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { ACCESS_SUBTABS, SUDO_COLUMNS, fmtRemaining } from './access-model.js';
import { idTooltip, relTime, shortAgentId } from './helpers.js';

const html = htm.bind(h);

function SlugTags({ values, denied = false }) {
  if (!values?.length) return html`<span class="muted">—</span>`;
  return html`<${Fragment}>${values.map((slug) => html`<span key=${slug} class=${`tag slug${denied ? ' deny' : ''}`}>${slug}</span> `)}</${Fragment}>`;
}

function PermissionsView({ current }) {
  if (!current.snapshotLoaded) return html`<div class="empty">Loading permissions…</div>`;
  if (current.snapshotLoaded && !current.permissions) {
    return html`<div role="alert" class="island-error">Permissions data is unavailable in the latest snapshot.</div>`;
  }
  return html`<${Fragment}>
    <h3 style="margin-top:0">Defaults <span class="muted" style="font-size:11px">— granted to every agent (config.json)</span></h3>
    ${current.defaults.length === 0
      ? html`<div class="empty">No defaults set.</div>`
      : html`<div>${current.defaults.map((slug) => html`<span key=${slug} class="tag default slug">${slug}</span> `)}</div>`}
    <h3>Per-agent overrides <span class="muted" style="font-size:11px">— permanent grant / deny on top of defaults (SQLite agent_permissions). Edit via the per-agent “permissions” button.</span></h3>
    ${current.permissionRows.length === 0
      ? html`<div class="empty">No per-agent overrides yet. Use the per-agent “permissions” button.</div>`
      : html`<table><thead><tr><th>ID</th><th>Title</th><th>Granted</th><th>Denied</th></tr></thead>
        <tbody>${current.permissionRows.map((row) => html`<tr key=${row.convId} data-key=${row.convId}>
          <td class="id" title=${idTooltip(row.agentId, row.convId)}>${shortAgentId(row.agentId, row.convId)}</td>
          <td class="rowname">${row.title}</td><td><${SlugTags} values=${row.granted} /></td><td><${SlugTags} values=${row.denied} denied=${true} /></td>
        </tr>`)}</tbody></table>`}
  </${Fragment}>`;
}

function SlugsView({ current }) {
  if (!current.snapshotLoaded) return html`<div class="empty">Loading slug registry…</div>`;
  if (current.snapshotLoaded && !current.slugs) {
    return html`<div role="alert" class="island-error">The permission slug registry is unavailable in the latest snapshot.</div>`;
  }
  if (!current.slugs?.length) return html`<div class="empty">No slugs registered.</div>`;
  return html`<${Fragment}>
    <div class="muted" style="font-size:11px;margin-bottom:6px">👑 = group ownership confers this slug for owned groups / their members, without an explicit grant (a per-agent deny still suppresses it).</div>
    <table><thead><tr><th>Slug</th><th>Owner</th><th>Description</th></tr></thead><tbody>
      ${current.slugs.map((slug) => html`<tr key=${slug.slug} data-key=${slug.slug}>
        <td><span class="slug">${slug.slug}</span></td>
        <td>${slug.owner_implied ? html`<span class="owner-badge" title="Conferred by group ownership">👑</span>` : html`<span class="muted">—</span>`}</td>
        <td>${slug.description || ''}</td>
      </tr>`)}
    </tbody></table>
  </${Fragment}>`;
}

function SudoHeader({ state, current }) {
  const activate = (event, key) => {
    if (!key) return;
    if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ' && event.key !== 'Spacebar') return;
    event.preventDefault();
    state.cycleSudoSort(key);
  };
  return html`<thead><tr>${SUDO_COLUMNS.map((column) => {
    const active = column.key && current.sudoSort?.key === column.key;
    return html`<th class=${column.key ? `sortable${active ? ' sort-active' : ''}` : undefined}
      tabIndex=${column.key ? '0' : undefined} aria-sort=${active ? (current.sudoSort.dir === 'asc' ? 'ascending' : 'descending') : column.key ? 'none' : undefined}
      title=${column.key ? `Sort by ${column.label}` : undefined}
      onClick=${(event) => activate(event, column.key)} onKeyDown=${(event) => activate(event, column.key)}>
      ${column.label}${column.key && html`<span class="sort-arrow">${active ? (current.sudoSort.dir === 'asc' ? '▲' : '▼') : '▾'}</span>`}
    </th>`;
  })}</tr></thead>`;
}

function SudoView({ state, actions, current }) {
  const filterRef = useRef(null);
  return html`<${Fragment}>
    <div class="filter-bar">
      <input ref=${filterRef} id="filter-sudo" type="text" aria-label="Filter active sudo grants"
        placeholder="Filter active sudo grants (matches conv title / id / slug / reason)" autocomplete="off" spellcheck=${false}
        value=${current.sudoQuery} onInput=${(event) => state.setSudoQuery(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Escape') state.setSudoQuery(''); }} />
      <span class="filter-count" id="filter-sudo-count" aria-live="polite">${current.sudoQuery
        ? `${current.sudo.length} / ${current.sudoTotal}`
        : `${current.sudoTotal} active grant${current.sudoTotal === 1 ? '' : 's'}`}</span>
      <button class="clear-filter" id="filter-sudo-clear" title="Clear filter" aria-label="Clear sudo filter"
        onClick=${() => { state.setSudoQuery(''); filterRef.current?.focus(); }}>×</button>
      <span class="spacer"></span>
      <button id="sudo-grant-open" class="primary" title="Proactively grant a time-bounded sudo elevation to an agent"
        onClick=${actions.openGrant}>+ Grant sudo</button>
    </div>
    ${current.mutation.error && html`<div role="alert" class="island-error access-mutation-error">${current.mutation.error}</div>`}
    <div id="sudo-list">
      ${!current.snapshotLoaded
        ? html`<div class="empty">Loading sudo grants…</div>`
        : !current.sudoAvailable
        ? html`<div role="alert" class="island-error">Sudo grant data is unavailable in the latest snapshot.</div>`
        : current.sudo.length === 0
        ? html`<div class="empty">No active sudo grants.</div>`
        : html`<table><${SudoHeader} state=${state} current=${current} /><tbody>
          ${current.sudo.map((grant) => {
            const key = `revoke:${grant.id}`;
            return html`<tr key=${grant.id} data-key=${`sudo-${grant.id}`}>
              <td><span class="rowname">${grant.conv_title || '(unknown)'}</span> <span class="id" title=${idTooltip(grant.agent_id, grant.conv_id)}>${shortAgentId(grant.agent_id, grant.conv_id)}</span></td>
              <td><span class="tag slug">${grant.slug}</span></td>
              <td><span class="last-hook">${relTime(grant.granted_at)}</span></td>
              <td><span class="last-hook" data-sudo-countdown=${grant.id}>${fmtRemaining(grant.remaining_seconds)}</span></td>
              <td>${grant.reason || ''}</td>
              <td><span class="muted" title=${grant.granted_by || ''}>${grant.granted_by || ''}</span></td>
              <td><button class="danger" disabled=${current.mutation.busy.has(key)} onClick=${() => actions.revoke(grant)}
                title="Revoke this grant">${current.mutation.busy.has(key) ? 'revoking…' : 'revoke'}</button></td>
            </tr>`;
          })}
        </tbody></table>`}
    </div>
  </${Fragment}>`;
}

function AccessSubnav({ state, current }) {
  const notifyNavigation = (subtab) => document.dispatchEvent(new CustomEvent('tclaude:navigated', {
    detail: { location: { tab: 'access', subtab } },
  }));
  const navigate = (event, subtab) => {
    if (event.button > 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    state.setSubtab(subtab);
    notifyNavigation(subtab);
  };
  const keyDown = (event, subtab) => {
    if (event.key !== ' ' && event.key !== 'Spacebar') return;
    event.preventDefault();
    state.setSubtab(subtab);
    notifyNavigation(subtab);
  };
  return html`<div class="access-subnav" role="tablist" aria-label="Access control views">
    ${ACCESS_SUBTABS.map((subtab) => html`<a key=${subtab} class=${`access-subtab${current.subtab === subtab ? ' active' : ''}`}
      data-subtab=${subtab} href=${`/access/${subtab}`} role="tab" aria-selected=${current.subtab === subtab ? 'true' : 'false'}
      aria-controls=${`access-${subtab}`} onClick=${(event) => navigate(event, subtab)} onKeyDown=${(event) => keyDown(event, subtab)}>
      ${{ permissions: 'Permissions', slugs: 'Slug registry', sudo: 'Sudo' }[subtab]}
    </a>`)}
  </div>`;
}

export function AccessApp({ state, actions }) {
  const current = state.view.value;
  useEffect(() => {
    const timer = setInterval(() => state.tick(), 1000);
    return () => clearInterval(timer);
  }, []);
  return html`<div class="access-island">
    <p class="access-intro">Everything that governs <strong>what an agent is allowed to do</strong>, in one place: the permanent <strong>Permissions</strong> roster, the <strong>Slug registry</strong> of every grantable capability, and time-bounded <strong>Sudo</strong> elevations.</p>
    <${AccessSubnav} state=${state} current=${current} />
    <div class=${`access-panel${current.subtab === 'permissions' ? ' active' : ''}`} id="access-permissions" role="tabpanel" aria-label="Permissions">
      <div id="permissions-body"><${PermissionsView} current=${current} /></div>
    </div>
    <div class=${`access-panel${current.subtab === 'slugs' ? ' active' : ''}`} id="access-slugs" role="tabpanel" aria-label="Slug registry">
      <p class="access-sub-intro">Every capability slug an agent can be granted — registered by the daemon, read-only here. Grant them as permanent overrides (Permissions) or temporary elevations (Sudo).</p>
      <div id="slugs-body"><${SlugsView} current=${current} /></div>
    </div>
    <div class=${`access-panel${current.subtab === 'sudo' ? ' active' : ''}`} id="access-sudo" role="tabpanel" aria-label="Sudo">
      <${SudoView} state=${state} actions=${actions} current=${current} />
    </div>
  </div>`;
}

export function mountAccessIsland({ host, state, actions, registerCleanup }) {
  state.initialize();
  render(html`<${AccessApp} state=${state} actions=${actions} />`, host);
  registerCleanup(() => render(null, host));
}
