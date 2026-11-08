import { Fragment, h, render } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import htm from 'htm';
import { ACCESS_SUBTABS, SUDO_COLUMNS, fmtRemaining } from './access-model.js';
import { idTooltip, relTime, shortAgentId } from './helpers.js';
import { Button, Input } from './ui-components.js';
import { Tabs } from './ui-tabs.js';
import { TableHead, tableState } from './table-kit.js';

const html = htm.bind(h);

function SlugTags({ values, denied = false }) {
  if (!values?.length) return html`<span class="muted">—</span>`;
  return html`<${Fragment}>${values.map((slug) => html`<span key=${slug} class=${`tag slug${denied ? ' deny' : ''}`}>${slug}</span> `)}</${Fragment}>`;
}

function PermissionsView({ current }) {
  const placeholder = tableState({
    loading: !current.snapshotLoaded && 'Loading permissions…',
    error: !current.permissions && 'Permissions data is unavailable in the latest snapshot.',
  });
  if (placeholder) return placeholder;
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

export function ownerGrantTitle(scopeDims = []) {
  if (scopeDims.includes('group')) return 'Group ownership contributes one ordinary grant scoped to each group you own';
  return 'Group ownership contributes this ordinary global grant';
}

function ownerGrantLabel(scopeDims = []) {
  return scopeDims.includes('group') ? 'owned groups' : 'global';
}

function SlugsView({ state, current }) {
  const filterRef = useRef(null);
  const placeholder = tableState({
    loading: !current.snapshotLoaded && 'Loading slug registry…',
    error: !current.slugs && 'The permission slug registry is unavailable in the latest snapshot.',
  });
  if (placeholder) return placeholder;
  return html`<${Fragment}>
    <div class="filter-bar">
      <${Input} domRef=${filterRef} id="filter-slugs" aria-label="Filter permission slugs by name"
        placeholder="Filter by slug name" autocomplete="off" spellcheck=${false}
        value=${current.slugQuery} onInput=${(event) => {
          const normalized = state.setSlugQuery(event.currentTarget.value);
          // A whitespace-only edit can normalize to the signal's existing
          // empty value, which does not trigger a Preact rerender. Reconcile
          // the live control so it never displays a no-op persisted query.
          if (event.currentTarget.value !== normalized) event.currentTarget.value = normalized;
        }}
        onKeyDown=${(event) => { if (event.key === 'Escape') state.setSlugQuery(''); }} />
      <span class="filter-count" id="filter-slugs-count" aria-live="polite">${current.slugQuery
        ? `${current.slugs.length} / ${current.slugTotal}`
        : `${current.slugTotal} slug${current.slugTotal === 1 ? '' : 's'}`}</span>
      <button data-ui-exempt class="clear-filter" id="filter-slugs-clear" title="Clear filter" aria-label="Clear slug filter"
        onClick=${() => { state.setSlugQuery(''); filterRef.current?.focus(); }}>×</button>
    </div>
    <div class="muted" style="font-size:11px;margin-bottom:6px">👑 = group ownership automatically contributes this permission. <b>owned groups</b> means one ordinary grant scoped to each group you own; <b>global</b> is unscoped when you own any active group. 👥 = group membership contributes the permission for that group. A same-slug per-agent deny suppresses the contributed grant.</div>
    ${current.slugs.length === 0
      ? html`<div class="empty">${current.slugTotal ? 'No matching permission slugs.' : 'No slugs registered.'}</div>`
      : html`<table><thead><tr><th>Slug</th><th>Owner</th><th>Description</th></tr></thead><tbody>
      ${current.slugs.map((slug) => html`<tr key=${slug.slug} data-key=${slug.slug}>
        <td><span class="slug">${slug.slug}</span></td>
        <td>${slug.owner_implied || slug.member_implied
          ? html`<${Fragment}>
              ${slug.owner_implied ? html`<span class="ui-tag ui-tag-own owner-badge" title=${ownerGrantTitle(slug.scope_dims)}>👑 ${ownerGrantLabel(slug.scope_dims)}</span>` : ''}
              ${slug.member_implied ? html` <span class="ui-tag ui-tag-own owner-badge" title="Conferred by membership — only for the group the agent belongs to">👥 group</span>` : ''}
            </${Fragment}>`
          : html`<span class="muted">—</span>`}</td>
        <td>${slug.description || ''}</td>
      </tr>`)}
    </tbody></table>`}
  </${Fragment}>`;
}

function SudoHeader({ state, current }) {
  return html`<${TableHead} cols=${SUDO_COLUMNS} activeCol=${current.sudoSort?.key} dir=${current.sudoSort?.dir}
    isSortable=${(column) => !!column.key} colOf=${(column) => column.key} onSort=${(key) => state.cycleSudoSort(key)} />`;
}

function SudoView({ state, actions, current }) {
  const filterRef = useRef(null);
  return html`<${Fragment}>
    <div class="filter-bar">
      <${Input} domRef=${filterRef} id="filter-sudo" aria-label="Filter active sudo grants"
        placeholder="Filter active sudo grants (matches conv title / id / slug / reason)" autocomplete="off" spellcheck=${false}
        value=${current.sudoQuery} onInput=${(event) => state.setSudoQuery(event.currentTarget.value)}
        onKeyDown=${(event) => { if (event.key === 'Escape') state.setSudoQuery(''); }} />
      <span class="filter-count" id="filter-sudo-count" aria-live="polite">${current.sudoQuery
        ? `${current.sudo.length} / ${current.sudoTotal}`
        : `${current.sudoTotal} active grant${current.sudoTotal === 1 ? '' : 's'}`}</span>
      <button data-ui-exempt class="clear-filter" id="filter-sudo-clear" title="Clear filter" aria-label="Clear sudo filter"
        onClick=${() => { state.setSudoQuery(''); filterRef.current?.focus(); }}>×</button>
      <span class="spacer"></span>
      <${Button} id="sudo-grant-open" variant="primary" title="Proactively grant a time-bounded sudo elevation to an agent"
        onClick=${actions.openGrant}>+ Grant sudo<//>
    </div>
    ${current.mutation.error && html`<div role="alert" class="island-error access-mutation-error">${current.mutation.error}</div>`}
    <div id="sudo-list">
      ${tableState({
        loading: !current.snapshotLoaded && 'Loading sudo grants…',
        error: !current.sudoAvailable && 'Sudo grant data is unavailable in the latest snapshot.',
        empty: current.sudo.length === 0 && 'No active sudo grants.',
      }) || html`<table><${SudoHeader} state=${state} current=${current} /><tbody>
          ${current.sudo.map((grant) => {
            const key = `revoke:${grant.id}`;
            return html`<tr key=${grant.id} data-key=${`sudo-${grant.id}`}>
              <td><span class="rowname">${grant.conv_title || '(unknown)'}</span> <span class="id" title=${idTooltip(grant.agent_id, grant.conv_id)}>${shortAgentId(grant.agent_id, grant.conv_id)}</span></td>
              <td><span class="tag slug">${grant.slug}</span></td>
              <td><span class="last-hook">${relTime(grant.granted_at)}</span></td>
              <td><span class="last-hook" data-sudo-countdown=${grant.id}>${fmtRemaining(grant.remaining_seconds)}</span></td>
              <td>${grant.reason || ''}</td>
              <td><span class="muted" title=${grant.granted_by || ''}>${grant.granted_by || ''}</span></td>
              <td><${Button} size="sm" variant="ghost danger" disabled=${current.mutation.busy.has(key)} onClick=${() => actions.revoke(grant)}
                title="Revoke this grant">${current.mutation.busy.has(key) ? 'revoking…' : 'revoke'}<//></td>
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
  const select = (subtab) => {
    state.setSubtab(subtab);
    notifyNavigation(subtab);
  };
  const labels = { permissions: 'Permissions', slugs: 'Slug registry', sudo: 'Sudo' };
  return html`<${Tabs} class="access-subnav" label="Access control views" value=${current.subtab} onSelect=${select}
    items=${ACCESS_SUBTABS.map((subtab) => ({
      key: subtab, label: labels[subtab], href: `/access/${subtab}`, class: 'access-subtab',
      attrs: { 'data-subtab': subtab, 'aria-controls': `access-${subtab}` },
    }))} />`;
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
      <div id="slugs-body"><${SlugsView} state=${state} current=${current} /></div>
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
