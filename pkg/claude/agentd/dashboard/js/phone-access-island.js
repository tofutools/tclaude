// phone-access-island.js -- the Access tab on a phone (data-layout="phone").
//
// Four views behind a row of tabs: Requests (access requests waiting on the
// operator, answered with the same card as the Now view, and the sudo grants
// running), Sudo (time-bounded grants, revoked here), Agents (the per-agent
// overrides, read-only) and Slugs (the registry, searched rather than listed
// in full). "+ Grant" in the top bar grants sudo from any of them. It runs on the desktop island's state and actions (access-state.js,
// access-actions.js); Agents, Sudo and Slugs are that state's subtabs and keep
// their /access/<subtab> routes. Requests is the phone's own and the default
// while any request waits.

import { h, render } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { fmtRemaining } from './access-model.js';
import { companionAgents, pendingAccess, pendingApprovals } from './companion-model.js';
import { relTime, shortAgentId } from './helpers.js';
import { PhoneApprovalCard } from './phone-approval.js';
import { DesktopOnlyNote } from './phone-desktop-only.js';
import { usePhoneBar } from './phone-page.js';
import { Section } from './phone-ui.js';
import { Button, Input } from './ui-components.js';
import { Tabs } from './ui-tabs.js';

const html = htm.bind(h);

// SLUGS_SHOWN caps the slug list until the operator asks for all of it or
// searches: the registry is long, and a phone is not the place to read it all.
export const SLUGS_SHOWN = 40;

const VIEWS = [
  { key: 'requests', label: 'Requests' },
  { key: 'sudo', label: 'Sudo' },
  { key: 'permissions', label: 'Agents' },
  { key: 'slugs', label: 'Slugs' },
];

// navigated tells the history router the location changed: /access/<subtab>,
// or the bare /access for Requests, which has no route of its own.
function navigated(subtab) {
  const location = subtab ? { tab: 'access', subtab } : { tab: 'access' };
  document.dispatchEvent(new CustomEvent('tclaude:navigated', { detail: { location } }));
}

// Requests is what waits on the operator, each answered with the phone's
// approval card (as on Now), then the sudo grants running.
function Requests({ snapshot, current, actions, fetchImpl, notify, openInMail }) {
  const requests = pendingApprovals(snapshot);
  const elsewhere = pendingAccess(snapshot).length - requests.length;
  const agents = companionAgents(snapshot);
  return html`<div class="phone-access-requests">
    <${Section} title="Waiting on you">
      ${!requests.length && !elsewhere && html`<p class="ui-empty phone-empty">No access request is waiting for you.</p>`}
      ${requests.map((request) => {
        const agent = agents.get(request.agent_id);
        return html`<${PhoneApprovalCard} key=${request.id} request=${request} fetchImpl=${fetchImpl} notify=${notify}
          from=${agent?.name || request.conv_title || ''} group=${agent?.group || ''} onMore=${() => openInMail(request.id)} />`;
      })}
      ${elsewhere > 0 && html`<${Button} class="phone-btn phone-wide" onClick=${() => openInMail()}>
        ${elsewhere} more access request${elsewhere === 1 ? '' : 's'} in Mail ›<//>`}
    <//>
    ${current.sudo.length > 0 && html`<${Section} title="Sudo grants" count=${current.sudo.length}>
      <${SudoRows} current=${current} actions=${actions} />
    <//>`}
  </div>`;
}

// SudoRows are the sudo grants running: who has what, by whom, the time left
// (and the uses left, when the grant has a use limit) and why, each with its
// Revoke.
function SudoRows({ current, actions }) {
  return html`${current.mutation.error && html`<div role="alert" class="ui-error-box">${current.mutation.error}</div>`}
    <ul class="phone-list">${current.sudo.map((grant) => {
      const busy = current.mutation.busy.has(`revoke:${grant.id}`);
      return html`<li key=${grant.id} class="phone-access-row" data-key=${`sudo-${grant.id}`}>
        <div class="phone-access-text">
          <span class="phone-access-name">${grant.conv_title || shortAgentId(grant.agent_id, grant.conv_id) || '(unknown)'}</span>
          <span class="phone-access-sub"><span class="slug">${grant.slug}</span>${grant.granted_by ? ` · by ${grant.granted_by}` : ''} · ${relTime(grant.granted_at)}</span>
          <span class="phone-access-left">${fmtRemaining(grant.remaining_seconds)} left${grant.uses_limit > 0 ? ` · ${grant.uses_left} of ${grant.uses_limit} uses left` : ''}${grant.reason ? ` · "${grant.reason}"` : ''}</span>
        </div>
        <${Button} class="phone-btn phone-btn-no phone-access-revoke" disabled=${busy}
          aria-label=${`Revoke ${grant.slug} from ${grant.conv_title || grant.agent_id || 'this agent'}`}
          onClick=${() => actions.revoke(grant)}>${busy ? 'Revoking…' : 'Revoke'}<//>
      </li>`;
    })}</ul>`;
}

function Sudo({ current, actions }) {
  return html`<div class="phone-access-sudo">
    ${!current.sudo.length
      ? html`${current.mutation.error && html`<div role="alert" class="ui-error-box">${current.mutation.error}</div>`}
        <p class="ui-empty phone-empty">${current.sudoAvailable ? 'No active sudo grants. + Grant gives one.' : 'Sudo grants are not in the latest snapshot.'}</p>`
      : html`<${SudoRows} current=${current} actions=${actions} />`}
  </div>`;
}

// SlugTags are an agent's overrides as small chips: green + for a grant,
// red − for a denial.
function SlugTags({ slugs, denied = false }) {
  return slugs.map((slug) => html`<span key=${slug} class=${`phone-access-slug${denied ? ' deny' : ''}`}>${denied ? '−' : '+'} ${slug}</span>`);
}

function Agents({ current }) {
  const [defaults, setDefaults] = useState(false);
  if (!current.permissions) return html`<p class="ui-empty phone-empty">Permissions are not in the latest snapshot.</p>`;
  const count = current.defaults.length;
  return html`<div class="phone-access-agents">
    <button data-ui-exempt type="button" class="phone-access-defaults" aria-expanded=${defaults ? 'true' : 'false'}
      onClick=${() => setDefaults(!defaults)}>Everyone: ${count} default slug${count === 1 ? '' : 's'}
      <span aria-hidden="true">${defaults ? '⌄' : '›'}</span></button>
    ${defaults && html`<div class="phone-access-tags phone-access-default-tags">${current.defaults.map((slug) => html`<span key=${slug} class="phone-access-slug base">${slug}</span>`)}</div>`}
    ${!current.permissionRows.length
      ? html`<p class="ui-empty phone-empty">No agent has grants or denials beyond the defaults.</p>`
      : html`<ul class="phone-list">${current.permissionRows.map((row) => html`<li key=${row.convId} class="phone-access-row" data-key=${row.convId}>
        <div class="phone-access-text">
          <span class="phone-access-name">${row.title} <span class="phone-access-id">· ${shortAgentId(row.agentId, row.convId)}</span></span>
          <span class="phone-access-tags"><${SlugTags} slugs=${row.granted} /><${SlugTags} slugs=${row.denied} denied=${true} /></span>
        </div>
      </li>`)}</ul>`}
    <${DesktopOnlyNote} what="Editing per-agent overrides is" />
  </div>`;
}

function Slugs({ state, current }) {
  const [all, setAll] = useState(false);
  if (!current.slugs) return html`<p class="ui-empty phone-empty">The slug registry is not in the latest snapshot.</p>`;
  const shown = all || current.slugQuery ? current.slugs : current.slugs.slice(0, SLUGS_SHOWN);
  return html`<div class="phone-access-slugs">
    <${Input} class="phone-access-search" type="search" aria-label="Search permission slugs" placeholder="Search slugs"
      autocomplete="off" spellcheck=${false} value=${current.slugQuery}
      onInput=${(event) => state.setSlugQuery(event.currentTarget.value)} />
    ${!current.slugs.length && html`<p class="ui-empty phone-empty">${current.slugTotal ? 'No slug matches.' : 'No slugs registered.'}</p>`}
    <ul class="phone-list">${shown.map((slug) => html`<li key=${slug.slug} class="phone-access-row" data-key=${slug.slug}>
      <div class="phone-access-text">
        <span class="phone-access-name slug">${slug.slug}${slug.owner_implied ? html` <span class="ui-tag ui-tag-own">👑 owner</span>` : ''}${slug.member_implied ? html` <span class="ui-tag ui-tag-own">👥 member</span>` : ''}</span>
        ${slug.description && html`<span class="phone-access-desc">${slug.description}</span>`}
      </div>
    </li>`)}</ul>
    ${shown.length < current.slugs.length && html`<${Button} class="phone-wide" onClick=${() => setAll(true)}>Show all ${current.slugs.length} slugs<//>`}
  </div>`;
}

// PhoneAccessApp renders the Access tab. `snapshot` is the dashboard snapshot
// signal, for the access requests; `openInMail` opens a request (or all of
// them) in Mail, as the Now view does.
export function PhoneAccessApp({ state, actions, snapshot, fetchImpl = globalThis.fetch, notify = () => {}, openInMail = () => {} }) {
  const current = state.view.value;
  const data = snapshot.value;
  const waiting = data ? pendingAccess(data).length : 0;
  // Requests leads if any wait when the first snapshot lands; after that the
  // operator's pick (or a route naming a subtab) decides. null is undecided.
  const [requests, setRequests] = useState(null);
  const routed = useRef(current.subtab);
  useEffect(() => {
    // Functional: a route restored between this render and the effect has
    // already decided, and this render's `requests` would not know it.
    if (current.snapshotLoaded) setRequests((decided) => (decided === null ? waiting > 0 : decided));
  }, [current.snapshotLoaded]);
  useEffect(() => {
    if (routed.current === current.subtab) return;
    routed.current = current.subtab;
    setRequests(false);
  }, [current.subtab]);
  useEffect(() => {
    const timer = setInterval(() => state.tick(), 1000);
    return () => clearInterval(timer);
  }, []);

  const shown = requests ? 'requests' : current.subtab;
  const select = (key) => {
    if (key === 'requests') {
      if (!requests) navigated('');
      setRequests(true);
      return;
    }
    const changed = state.setSubtab(key);
    routed.current = key;
    if (changed || requests) navigated(key);
    setRequests(false);
  };
  // "+ Grant" sits in the top bar, as in the mock, whichever view is open,
  // once the snapshot it picks an agent from has landed.
  usePhoneBar({ tab: 'access', actions: current.snapshotLoaded
    ? [{ key: 'grant', text: '+ Grant', label: 'Grant sudo', onClick: () => actions.openGrant() }] : [] });
  const items = VIEWS.map((view) => ({
    key: view.key,
    label: view.key === 'requests' && waiting > 0
      ? html`${view.label} <span class="ui-badge ui-badge-danger">${waiting}</span>`
      : view.label,
    // The router restores /access/<subtab> by clicking .access-subtab
    // (nav-history.js), as on the desktop.
    href: view.key === 'requests' ? '/access' : `/access/${view.key}`,
    class: view.key === 'requests' ? 'access-requests-tab' : 'access-subtab',
    attrs: view.key === 'requests' ? undefined : { 'data-subtab': view.key },
  }));
  return html`<div class="phone-access">
    <${Tabs} class="phone-chips phone-access-chips" label="Access views" value=${shown} onSelect=${select} items=${items} />
    ${!current.snapshotLoaded
      ? html`<p class="ui-empty phone-empty">Loading…</p>`
      : html`<div role="tabpanel" aria-label=${VIEWS.find((view) => view.key === shown)?.label}>
        ${shown === 'requests' && html`<${Requests} snapshot=${data} current=${current} actions=${actions} fetchImpl=${fetchImpl} notify=${notify} openInMail=${openInMail} />`}
        ${shown === 'sudo' && html`<${Sudo} current=${current} actions=${actions} />`}
        ${shown === 'permissions' && html`<${Agents} current=${current} />`}
        ${shown === 'slugs' && html`<${Slugs} state=${state} current=${current} />`}
      </div>`}
  </div>`;
}

export function mountPhoneAccessIsland({ host, state, actions, snapshot, notify, openInMail, registerCleanup }) {
  state.initialize();
  render(html`<${PhoneAccessApp} state=${state} actions=${actions} snapshot=${snapshot} notify=${notify} openInMail=${openInMail} />`, host);
  registerCleanup(() => render(null, host));
}
