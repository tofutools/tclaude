import { h } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  applySort, tableSortState, RETIRED_COLS, RETIRED_ACCESSORS,
  CONVERSATIONS_COLS, CONVERSATIONS_ACCESSORS, REPLACED_COLS, REPLACED_ACCESSORS,
  PENDING_COLS, PENDING_ACCESSORS,
} from './sort.js';
import { shortId, shortAgentId, idTooltip, relTime, shortCwd, groupShowOffline, offlineDefault } from './helpers.js';
import { dashPrefs } from './prefs.js';
import { isWizardActive } from './slop.js';
import { PAGE_SIZES, listLimit } from './list-paging.js';
import { activityModeViews, activitySummary } from './group-activity.js';
import { ActivityModes } from './activity-bots.js';
import {
  buildGroupTree, groupMembersView, realGroupOpen, virtualGroupOpen,
} from './groups-view-model.js';
import { ActionMenu, InlineEditor, useGroupsInteractions } from './groups-interactions.js';
import { MemberTable } from './groups-member-table.js';

const html = htm.bind(h);

function ThemeText({ regular, wizard }) {
  return html`<span class="theme-copy-regular">${regular}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function GroupActivity({ members, snapshot }) {
  const summary = activitySummary(members);
  const modes = activityModeViews(summary, snapshot?.activity_bots);
  if (!modes.length) return null;
  return html`<span class="group-activity"><${ActivityModes} modes=${modes} modeTitles /></span>`;
}

function MenuButton({ regular, wizard = regular, className, ...props }) {
  return h('button', { role: 'menuitem', class: className || undefined, ...props },
    h(ThemeText, { regular, wizard }));
}

function GroupMenuItems({ group, members, snapshot, actions }) {
  const interactions = useGroupsInteractions();
  const name = group.name;
  const shared = { 'data-group': name, 'data-label': name };
  const contextLength = group.default_context?.length || 0;
  const pinned = dashPrefs.getItem(`tclaude.dash.quickpin.${name}`) === '1';
  const quickFold = !snapshot || snapshot.group_quick_options !== 'expanded';
  const policy = group.remote_control_policy || 'inherit';
  const nextPolicy = policy === 'inherit' ? 'optin' : policy === 'optin' ? 'deny' : 'inherit';
  const notify = !!group.notify_enabled;
  const wizardMode = isWizardActive();
  const contextTitle = wizardMode
    ? (contextLength ? `Standing orders (${contextLength} chars) delivered to every familiar summoned into this party — click to edit` : 'No standing orders — click to decree them')
    : (contextLength ? `Startup context (${contextLength} chars) delivered to the inbox of agents spawned here — click to edit` : 'No startup context — click to set one');
  const notifyTitle = notify
    ? (wizardMode ? 'Omens are enabled for this party’s familiars — click to silence the whole party (a familiar’s own 🔔 boon still speaks)' : 'OS notifications on for this group’s agents — click to mute the whole group (a per-agent 🔔 override still notifies)')
    : (wizardMode ? 'Omens are SILENCED for this party’s familiars — click to restore them (familiars with their own 🔔 boon still speak)' : 'OS notifications MUTED for this group’s agents — click to unmute (members with a per-agent 🔔 override notify anyway)');
  const remoteTitle = policy === 'inherit'
    ? "Remote-control policy: inherit — defers to each spawn profile's own default. Click to set a group default: opt-in pre-checks Remote Access on, deny pre-checks it off, for agents spawned into this group (a per-spawn checkbox / flag still wins)."
    : policy === 'optin'
      ? 'Remote-control policy: opt-in — defaults Claude Code Remote Access ON for agents spawned into this group (overrides the profile default; a per-spawn checkbox / flag still wins). Click to cycle to deny.'
      : 'Remote-control policy: deny — defaults Remote Access OFF for agents spawned into this group (overrides the profile default; a per-spawn checkbox / flag still wins). Click to cycle back to inherit.';
  const quickPinTitle = wizardMode
    ? (pinned ? 'Quick options are pinned open for this party — click to let its enchanted header chips fold again.' : "Pin this party's quick options open so its enchanted header chips stay expanded.")
    : (pinned ? 'Quick options are pinned open for this group — its header chips stay expanded even though auto-fold is on. Click to let them fold to icons again.' : "Pin this group's quick options open — its header chips stay expanded (not folded to icons) even while auto-fold is on. Click to fold them with the rest.");
  return html`
    <${MenuButton} ...${shared} data-act="add-member" title=${wizardMode ? 'Invite an existing conversation to become a familiar in this party' : 'Add an existing conversation to this group'} regular="+ add member" wizard="+ add familiar" onClick=${(event) => {
      event.preventDefault();
      event.stopPropagation();
      interactions.closeMenu(true);
      actions.openAddMember(group);
    }} />
    <${MenuButton} data-act="cron-new" data-prefill=${JSON.stringify({ targetMode: 'group', groupName: name, scopeGroup: name })} data-label=${name} title=${wizardMode ? `Bind a recurring ritual to party ${name} — multicast every familiar, or nudge one` : `Schedule a recurring cron job scoped to ${name} — multicast the whole group, or nudge a single member`} regular="⏰ multicast" wizard="⏳ bind ritual" />
    <${MenuButton} data-act="message-new" data-prefill=${JSON.stringify({ targetMode: 'group', groupName: name })} data-label=${name} title=${wizardMode ? `Send a missive to party ${name} — every familiar, or a chosen subset` : `Send a one-shot message to ${name} — the whole group, or a ticked subset of its members`} regular="✉ message" wizard="✒ missive" />
    <${MenuButton} ...${shared} data-act="view-group-messages" title=${wizardMode ? "Open this party's missives in the Messages tab" : "Open this group's messages in the Messages tab — every message touching a member (sent or received) plus the group's own multicasts"} regular="🗂 view messages" wizard="🗂 view missives" />
    <${MenuButton} ...${shared} data-act="set-group-context"
      title=${contextTitle}
      regular=${contextLength ? `📋 startup context (${contextLength} chars)…` : '📋 set startup context…'}
      wizard=${contextLength ? `📜 standing orders (${contextLength} chars)…` : '📜 decree standing orders…'}
    />
    <button role="menuitem" ...${shared} data-act="set-group-permissions" title=${wizardMode ? 'Bestow party boons on every familiar in this party. Membership changes take effect immediately; a personal binding against one still wins.' : 'Grant permissions to every current member of this group. Membership changes take effect immediately; an agent-level Deny still wins.'}>🔑 <span class="group-perms-word-regular">group permissions</span><span class="group-perms-word-wizard">party boons</span>${group.permissions?.length ? ` (${group.permissions.length})` : ''}…</button>
    <${MenuButton} ...${shared} data-act="toggle-group-notify" data-enabled=${notify ? '1' : '0'} title=${notifyTitle} regular=${notify ? '🔔 notifications: on' : '🔕 notifications: muted'} wizard=${notify ? '🔔 omens: on' : '🔕 omens: silent'} />
    <button role="menuitem" ...${shared} data-act="set-group-remote-control" data-policy=${policy} data-next=${nextPolicy} title=${remoteTitle}>${policy === 'deny' ? '🚫' : '📱'} remote policy: ${policy === 'optin' ? 'opt-in' : policy}</button>
    ${quickFold ? html`<button role="menuitem" ...${shared} data-pinned=${pinned ? '1' : '0'} title=${quickPinTitle} onClick=${() => actions.toggleQuickPin(group)}>${pinned ? '📌 unpin quick options' : '📌 pin quick options open'}</button>` : null}
    <${MenuButton} ...${shared} data-act="rename-group" title=${wizardMode ? 'Rename this party' : 'Rename this group'} regular="rename" wizard="rename party" onClick=${(event) => {
      event.preventDefault();
      event.stopPropagation();
      const cog = event.currentTarget.closest('.action-menu')
        ?.parentElement?.querySelector('.cog-btn');
      interactions.beginEditor(`group:${name}:name`, cog);
    }} />
    <${MenuButton} ...${shared} data-act="nest-group" title=${wizardMode ? 'Nest this party under another party on the board — layout only' : 'Nest this group under another so it draws inside it on the board — collapse the parent to tuck the subgroup away. Board layout only.'} regular="📂 nest under…" wizard="📂 nest party under…" />
    ${group.parent ? html`<${MenuButton} ...${shared} data-act="unnest-group" data-parent=${group.parent} title=${`Move this ${wizardMode ? 'party' : 'group'} back to the top level (currently nested under ${group.parent}).`} regular=${`📂 un-nest (under ${group.parent})`} wizard=${`📂 un-nest party (under ${group.parent})`} />` : null}
    <${MenuButton} ...${shared} data-act="clone-group" title=${wizardMode ? 'Mirror this party — copy its lore, boons, owners, and optionally its familiars into a new party' : 'Clone this group — copy every setting (directory, description, startup context, default profile, group permissions, max-members, notify) and the owners into a new group. Optionally clone the member agents too.'} regular="⧉ clone…" wizard="⧉ mirror party…" />
    <${MenuButton} ...${shared} data-act="template-from-group" title=${wizardMode ? "Trace this party into a reusable summoning circle — preserve its classes, owners, familiars' grimoires, and standing orders" : 'Save this group as a reusable template — snapshot its roles, owners, per-agent permissions and startup context into a blueprint you can instantiate as a fresh team'} regular="⧉ save as template…" wizard="🕯 trace as circle…" />
    <${MenuButton} ...${shared} data-act="export-group" title=${wizardMode ? 'Seal this whole party — familiars, boons, missives, and conversation scrolls — into a portable archive' : 'Export this whole group — members, permissions, messages and every conversation — to a portable .zip archive'} regular="⤓ export" wizard="⤓ seal party archive" />
    <${MenuButton} ...${shared} data-act="cleanup-group" title=${wizardMode ? 'Remove confirmed-slumbering familiars from this party' : 'Remove confirmed-offline members from this group'} regular="🧹 cleanup" wizard="🧹 tidy party" />
    <button role="menuitem" ...${shared} data-act="cleanup-worktrees-group" title=${wizardMode ? "Clean up git worktrees in this party's repos. Main repo and live-familiar worktrees are protected." : "Clean up git worktrees — scan this group's repo(s) for stale worktrees (leftovers from retired/deleted agents and hand-made branches) and remove the ones you pick. Main repo and live-agent worktrees are protected."}>🧹 cleanup worktrees…</button>
    <${MenuButton} ...${shared} data-act="window-modal-group" title=${wizardMode ? 'Reveal or veil the scrying portals of familiars in this party. The familiars keep channeling either way.' : 'Focus / unfocus agent windows — open a modal to bulk-attach (focus) or bulk-detach (unfocus) the terminal windows of agents in this group. Window-only: the agents keep running either way.'} regular="🪟 windows…" wizard="👁 familiars…" />
    ${group.default_cwd ? html`<${MenuButton} ...${shared} data-act="group-web-term" title=${wizardMode ? `Open a browser scrying portal in this party's default directory (${group.default_cwd.trim()})` : `Open a terminal in this group's default directory (${group.default_cwd.trim()}), in the browser (always a web terminal — never a native window)`} regular="🖥 open web terminal" wizard="🔮 open party portal" />` : null}
    <${MenuButton} ...${shared} className="danger" data-act="delete-group" data-members=${members.length} title=${wizardMode ? 'Disband this party' : 'Delete this group'} regular="delete group" wizard="disband party" />
  `;
}

function SortHead({ table, columns }) {
  const active = tableSortState(table);
  return html`<thead><tr>${columns.map((column) => {
    const label = column.wizardLabel
      ? html`<${ThemeText} regular=${column.label} wizard=${column.wizardLabel} />`
      : (column.label || '');
    if (!column.col) return html`<th key=${column.key || column.label || 'blank'}>${label}</th>`;
    const selected = active?.col === column.col;
    return html`<th
      key=${column.key || column.col}
      class=${selected ? 'sortable sort-active' : 'sortable'}
      data-sort-table=${table}
      data-sort-col=${column.col}
      title=${`Sort by ${isWizardActive() && column.wizardLabel ? column.wizardLabel : column.label}`}
    >${label}<span class="sort-arrow">${selected ? (active.dir === 'asc' ? '▲' : '▼') : '▾'}</span></th>`;
  })}</tr></thead>`;
}

function Pager({ kind, paging }) {
  if (!paging) return null;
  const limit = listLimit(kind);
  const total = paging.total || 0;
  const offset = paging.offset || 0;
  if (total <= limit && offset === 0) return null;
  const first = offset <= 0;
  const last = offset + limit >= total;
  const button = (action, glyph, title, disabled) => html`<button
    key=${action} type="button" class="list-pager-btn" data-pager=${action}
    data-list=${kind} disabled=${disabled} title=${title} aria-label=${title}
  >${glyph}</button>`;
  return html`<div class="list-pager" data-list=${kind}>
    ${button('first', '«', 'First page', first)}
    ${button('prev', '‹', 'Previous page', first)}
    <span class="list-pager-count">${total ? offset + 1 : 0}–${Math.min(offset + limit, total)} of ${total}</span>
    ${button('next', '›', 'Next page', last)}
    ${button('last', '»', 'Last page', last)}
    <select class="list-pager-size" data-pager="size" data-list=${kind} title="Rows per page" value=${String(limit)}>
      ${PAGE_SIZES.map((size) => html`<option key=${size} value=${size}>${size}/page</option>`)}
    </select>
  </div>`;
}

function EditableGroupChip({ group, actions, field, value, className, action, title, children, type = 'text', inputClass, placeholder, inputProps, normalize = (next) => next.trim(), message }) {
  const editorKey = `group:${group.name}:${field}`;
  return html`<${InlineEditor}
    editorKey=${editorKey} value=${value} type=${type} className=${inputClass} placeholder=${placeholder}
    inputProps=${inputProps}
    onCommit=${async (raw) => {
      const next = normalize(raw);
      if (next === value) return false;
      await actions.patchGroup(group, field, next, message);
      return true;
    }}
    triggerProps=${{
      class: className, tabindex: '0', role: 'button', 'data-act': action,
      'data-group': group.name, 'data-label': group.name, 'data-editor-key': editorKey, title,
    }}
  >${children}<//>`;
}

const NEW_PROFILE = '/new-profile';

function GroupProfileChip({ group, actions, kind }) {
  const interactions = useGroupsInteractions();
  const sandbox = kind === 'sandbox';
  const editorKey = `group:${group.name}:${sandbox ? 'sandbox_profile' : 'default_profile'}`;
  const active = interactions.editorKey === editorKey;
  const current = sandbox ? (group.sandbox_profile || '') : (group.default_profile || '');
  const [choices, setChoices] = useState([]);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const selectRef = useRef(null);
  const triggerRef = useRef(null);
  const restoreFocusRef = useRef(false);
  useLayoutEffect(() => {
    if (!active) return;
    let live = true;
    setBusy(true);
    busyRef.current = true;
    setError('');
    actions.groupProfileChoices(kind).then((items) => {
      if (live) setChoices(items);
    }).catch((err) => {
      if (live) setError((err && err.message) || String(err));
    }).finally(() => {
      if (live) {
        setBusy(false);
        busyRef.current = false;
        queueMicrotask(() => selectRef.current?.focus());
      }
    });
    return () => { live = false; };
  }, [active, kind]);
  useLayoutEffect(() => {
    if (active || !restoreFocusRef.current) return;
    restoreFocusRef.current = false;
    triggerRef.current?.focus();
  }, [active]);
  if (active) {
    const missing = current && !choices.some((choice) => choice.value === current);
    return html`<select
      ref=${selectRef} class="group-default-profile-select" value=${current} disabled=${busy}
      aria-invalid=${error ? 'true' : undefined} title=${error || undefined}
      onKeyDown=${(event) => {
        if (event.key === 'Escape') {
          event.preventDefault();
          restoreFocusRef.current = true;
          interactions.endEditor(editorKey);
        }
      }}
      onBlur=${() => { if (!busyRef.current) interactions.endEditor(editorKey); }}
      onChange=${async (event) => {
        const name = event.currentTarget.value;
        if (name === NEW_PROFILE) {
          interactions.endEditor(editorKey);
          actions.openNewGroupProfile(kind, (created) => {
            void Promise.resolve()
              .then(() => actions.setGroupProfile(group, kind, created))
              .catch((error) => actions.reportError(error));
          });
          return;
        }
        if (name === current) {
          interactions.endEditor(editorKey);
          return;
        }
        busyRef.current = true;
        setBusy(true);
        setError('');
        try {
          await actions.setGroupProfile(group, kind, name);
          interactions.endEditor(editorKey);
        } catch (err) {
          setError((err && err.message) || String(err));
          busyRef.current = false;
          setBusy(false);
          queueMicrotask(() => selectRef.current?.focus());
        }
      }}
    >
      <option value=${NEW_PROFILE}>${sandbox ? '＋ new sandbox profile…' : (isWizardActive() ? '＋ new pattern…' : '＋ new profile…')}</option>
      <option value="">${sandbox ? '(inherit)' : '(none)'}</option>
      ${choices.map((choice) => html`<option key=${choice.value} value=${choice.value}>${choice.label}</option>`)}
      ${missing ? html`<option value=${current}>${current} (missing)</option>` : null}
    </select>`;
  }
  const className = sandbox
    ? `group-sandbox-profile${current ? '' : ' unset'}`
    : `group-default-model${current ? '' : ' unset'}`;
  const title = sandbox
    ? current ? `Sandbox profile for ${group.name}: ${current} — composes after the global sandbox profile for newly launched agents. Click to change.` : 'No group sandbox profile — newly launched agents get the global one only. Click to set one.'
    : current ? `Default spawn profile for agents spawned into this group: ${current} — fills blank launch fields at spawn. Click to change.` : 'No default spawn profile — click to set one. (Spawns use their own fields until set.)';
  return html`<span
    ref=${triggerRef}
    class=${className} tabindex="0" role="button"
    data-act=${sandbox ? 'set-group-sandbox-profile' : 'set-group-profile'}
    data-group=${group.name} data-label=${group.name}
    data-profile=${sandbox ? undefined : current} data-sandbox-profile=${sandbox ? current : undefined}
    data-editor-key=${editorKey} title=${title}
    onClick=${(event) => { event.preventDefault(); event.stopPropagation(); interactions.beginEditor(editorKey, event.currentTarget); }}
    onKeyDown=${(event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault(); interactions.beginEditor(editorKey, event.currentTarget);
    }}
  >${sandbox ? '🛡' : '🧠'}<span class="qo-text">${current ? ` ${current}` : ''}</span></span>`;
}

function ProcessChip({ group }) {
  const process = group.process;
  if (!process?.phases?.length) return null;
  const index = typeof process.phase_index === 'number' ? process.phase_index : -1;
  const text = index >= 0
    ? `◆ phase ${index + 1}/${process.phase_count}: ${process.current_phase}`
    : `◆ ${process.current_phase}`;
  const lines = process.phases.map((phase, i) => `${phase.current ? '▸ ' : '  '}${i + 1}. ${phase.name} [${phase.roles?.length ? phase.roles.join(', ') : 'any'}]`);
  if (process.transitions?.length) lines.push('', 'transitions:', ...process.transitions.map((item) => `${item.from || '(start)'} → ${item.to}`));
  const next = index >= 0 && index + 1 < process.phase_count ? process.phases[index + 1].name : '';
  return html`<span class="group-process-chip" title=${['Advisory process — tracked, not enforced', '', ...lines].join('\n')}>${text}</span>
    ${next ? html`<button class="group-process-advance" data-act="advance-phase" data-group=${group.name} data-label=${group.name} title=${`Advance to the next phase (${next})`}>▸ advance</button>` : null}`;
}

function GroupLinkChips({ group, snapshot }) {
  const links = snapshot?.links || [];
  const outgoing = links.filter((link) => link.from === group.name);
  const incoming = links.filter((link) => link.to === group.name);
  if (!outgoing.length && !incoming.length) return null;
  const chip = (link, direction) => {
    const other = direction === 'out' ? link.to : link.from;
    const title = direction === 'out'
      ? `Members of this group can message "${other}" (${link.mode}) — click to manage links`
      : `Members of "${other}" can message this group (${link.mode}) — click to manage links`;
    return html`<span key=${`${direction}-${link.id}`} class=${`group-link-chip ${direction}`} data-act="links-manage" title=${title}>${direction === 'out' ? '→' : '←'} ${other || '(deleted)'}</span>`;
  };
  return html`<span class="group-link-chips" tabindex="0" role="button" data-act="links-manage" title="Inter-group links — click to manage">🔗<span class="qo-text">${outgoing.map((link) => chip(link, 'out'))}${incoming.map((link) => chip(link, 'in'))}</span></span>`;
}

function RealGroupSummary({ group, activity, membersView, snapshot, actions }) {
  const interactions = useGroupsInteractions();
  const { members, hiddenOffline } = membersView;
  const online = group.online || 0;
  const full = !!group.max_members && members.length >= group.max_members;
  const cap = group.max_members || '∞';
  const count = online === members.length ? `${members.length}/${cap}` : `${online}/${members.length}/${cap}`;
  const titleParts = [`${members.length} member${members.length === 1 ? '' : 's'} (${online} online)`, group.max_members ? `cap ${group.max_members}` : 'no cap'];
  if (full) titleParts.push('group is full, spawns refused');
  if (hiddenOffline) titleParts.push(`${hiddenOffline} offline hidden in this view`);
  const groupEditing = interactions.editorKey.startsWith(`group:${group.name}:`);
  const renameKey = `group:${group.name}:name`;
  const maxValue = group.max_members || 0;
  return html`<summary draggable=${!groupEditing} data-group-reorder=${group.name} title="Drag this header to reorder the group">
    ${interactions.editorKey === renameKey ? html`<${InlineEditor}
      editorKey=${renameKey} value=${group.name} className="group-rename-input"
      onCommit=${(value) => actions.renameGroup(group, value)}
      triggerProps=${{}}
    >${group.name}<//>` : html`<strong class="group-name" data-group-name=${group.name}>${group.name}</strong>`}
    <${GroupActivity} members=${activity} snapshot=${snapshot} />
    <${ProcessChip} group=${group} />
    ${group.waves?.pending_waves ? html`<span class="group-waves-chip" title=${`Staged spawn — ${group.waves.pending_agents} agent(s) in ${group.waves.pending_waves} more wave(s) will spawn as each wave settles${group.waves.deadline_at ? `\nnext wave by ${group.waves.deadline_at} at the latest` : ''}`}>🌊 wave ${group.waves.current_wave}/${group.waves.total_waves} pending</span>` : null}
    ${group.pending?.length ? html`<span class="group-pending-chip" title=${`${group.pending.length} pending spawn${group.pending.length === 1 ? '' : 's'} waiting for startup`}>⏳ ${group.pending.length} pending spawn${group.pending.length === 1 ? '' : 's'}</span>` : null}
    <${ActionMenu} menuKey=${`group:${group.name}`} kind="group-menu" wrapperClass="group-actions group-header-cog"><${GroupMenuItems} group=${group} members=${members} snapshot=${snapshot} actions=${actions} /><//>
    <${EditableGroupChip}
      className=${`group-descr${group.descr ? '' : ' unset'}`} action="set-group-descr" group=${group} actions=${actions}
      field="descr" value=${group.descr || ''} inputClass="group-descr-input" placeholder="group description — empty clears it"
      title=${group.descr ? 'Group description — click to edit' : 'No description — click to set one'}
      message=${(value) => value ? `${group.name}: description → ${value}` : `${group.name}: description cleared`}
    >📝<span class="qo-text"> ${group.descr || 'no description'}</span><//>
    <${EditableGroupChip}
      className=${`group-default-cwd${group.default_cwd ? '' : ' unset'}`} action="set-group-dir" group=${group} actions=${actions}
      field="default_cwd" value=${group.default_cwd || ''} inputClass="group-default-cwd-input" placeholder="absolute path (~ OK) — empty clears the default"
      title=${group.default_cwd ? `Default spawn directory: ${group.default_cwd} — click the text to edit, the 📁 to browse` : 'No default spawn directory — click the text to type one, the 📁 to browse'}
      message=${(value) => value ? `${group.name}: default dir → ${value}` : `${group.name}: default dir cleared`}
    ><span class="gdc-pick" tabindex="0" role="button" data-act="pick-group-dir" data-group=${group.name} data-label=${group.name} data-cwd=${group.default_cwd || ''} title="Browse for a directory with a native picker" onClick=${(event) => {
      event.preventDefault();
      event.stopPropagation();
      void actions.pickGroupDirectory(group).catch((error) => actions.reportError(error));
    }}>📁</span><span class="qo-text"> ${group.default_cwd ? shortCwd(group.default_cwd) : 'no default dir'}</span><//>
    <${EditableGroupChip}
      className=${`group-max-members${full ? ' full' : ''}${group.max_members ? '' : ' unset'}`} action="set-group-max-members" group=${group} actions=${actions}
      field="max_members" value=${maxValue} type="number" inputClass="group-max-members-input"
      inputProps=${{ min: '0', step: '1', title: '0 clears the cap (unlimited)' }}
      normalize=${(raw) => {
        const value = Number.parseInt(raw, 10);
        if (!Number.isInteger(value) || value < 0) throw new Error('max members must be a non-negative integer (0 = unlimited)');
        return value;
      }}
      title=${`${titleParts.join(' · ')}${group.max_members ? ' — click to edit cap' : ' — click to set a cap'}`}
      message=${(value) => value > 0 ? `${group.name}: member cap → ${value}` : `${group.name}: member cap cleared`}
    >👥 ${count}<//>
    <${GroupProfileChip} group=${group} actions=${actions} kind="profile" />
    <${GroupProfileChip} group=${group} actions=${actions} kind="sandbox" />
    <${GroupLinkChips} group=${group} snapshot=${snapshot} />
  </summary>`;
}

function SpawnIcon({ subgroup = false }) {
  return subgroup
    ? html`<svg class="subgroup-ico" viewBox="0 0 28 24" width="17" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="7" cy="7" r="3"></circle><circle cx="15" cy="7" r="3"></circle><path d="M1 21v-2a5 5 0 0 1 5-5h2a5 5 0 0 1 5 5v2"></path><path d="M12 14h3a5 5 0 0 1 5 5v2"></path><line x1="24" y1="7" x2="24" y2="13"></line><line x1="27" y1="10" x2="21" y2="10"></line></svg>`
    : html`<svg class="spawn-ico" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"></path><circle cx="8.5" cy="7" r="4"></circle><line x1="20" y1="8" x2="20" y2="14"></line><line x1="23" y1="11" x2="17" y2="11"></line></svg>`;
}

function isForce(group) {
  return !!(group.source_template || group.mission || group.process?.phases?.length || group.waves);
}

function GroupActions({ group, actions }) {
  const folded = dashPrefs.getItem(`tclaude.dash.forcefold.${group.name}`) === '1';
  const shared = { 'data-group': group.name, 'data-label': group.name };
  return html`
    <button class="spawn-btn" ...${shared} data-act="spawn-agent" title="Spawn agent — start a new tclaude session and join this group" aria-label="Spawn agent into this group"><${SpawnIcon} /><span class="spawn-btn-label-wizard" aria-hidden="true">🔮</span></button>
    <button class="spawn-btn subgroup-btn" ...${shared} data-act="create-subgroup" title="Create subgroup — make a new group nested under this group" aria-label="Create a subgroup under this group"><${SpawnIcon} subgroup=${true} /><span class="subgroup-icon-wizard" aria-hidden="true">⚔＋</span></button>
    <span class="group-actions">
      <button ...${shared} data-act="power-on-group" aria-label="Awaken — power on every offline agent in this group" title="Power on — resume every offline agent in this group. Each offline conversation is restarted in a fresh tmux session; agents already running are left alone. Resume only: nothing new is created."><span class="pwr-label-regular" aria-hidden="true">🟢</span><span class="pwr-label-wizard" aria-hidden="true">✨</span></button>
      <button class="warn" ...${shared} data-act="shutdown-group" aria-label="Slumber — shutdown every running agent in this group" title="Shutdown — stop every running agent in this group. Sends /exit, then force-kills any agent still alive after a grace period. Stop only: nothing is deleted, every session can simply be resumed."><span class="pwr-label-regular" aria-hidden="true">🛑</span><span class="pwr-label-wizard" aria-hidden="true">🌙</span></button>
      ${isForce(group) ? html`<button class=${`force-fold-btn${folded ? ' folded' : ''}`} ...${shared} data-folded=${folded ? '1' : '0'} aria-pressed=${folded ? 'true' : 'false'} title=${folded ? 'Task force info card is hidden — click to show it again (mission, phase, roles, re-brief / stand-down controls). Per-browser view state.' : 'Hide the task force info card (mission, phase, roles, controls). The 🎯 button stays here to bring it back. Per-browser view state.'} onClick=${() => actions.toggleForceFold(group)}>🎯<span class="force-fold-label-regular">${folded ? ' show info' : ' hide info'}</span><span class="force-fold-label-wizard">${folded ? ' reveal quest' : ' hide quest'}</span></button>` : null}
    </span>`;
}

function ForceBlock({ group }) {
  if (!isForce(group) || dashPrefs.getItem(`tclaude.dash.forcefold.${group.name}`) === '1') return null;
  const members = group.members || [];
  const roles = new Map();
  for (const member of members) {
    const role = member.role || '(no role)';
    roles.set(role, [...(roles.get(role) || []), member]);
  }
  const live = members.filter((member) => member.online);
  const stalling = live.length > 0 && live.every((member) => member.state?.status === 'idle');
  const process = group.process;
  return html`<div class="group-force-block">
    ${group.mission ? html`<div class="force-mission"><span class="force-mission-label-regular">🎯 Mission</span><span class="force-mission-label-wizard">🗺 Quest</span>: <span class="force-mission-text">${group.mission}</span>${group.source_template ? html` <span class="force-from">from ${group.source_template}</span>` : null}</div>` : group.source_template ? html`<div class="force-mission force-mission-unset">Deployed from template <strong>${group.source_template}</strong> — no mission recorded</div>` : null}
    ${process?.phases?.length ? html`<div class="force-phase"><span class="force-phase-label">◆ ${typeof process.phase_index === 'number' && process.phase_index >= 0 ? `phase ${process.phase_index + 1}/${process.phase_count}: ${process.current_phase}` : process.current_phase}</span>${process.transitions?.length ? html`<span class="force-phase-history" title=${process.transitions.map((item) => `${item.from || '(start)'} → ${item.to}${item.at ? `  ${item.at}` : ''}`).join('\n')}>history (${process.transitions.length})</span>` : null}</div>` : null}
    ${members.length ? html`<div class="force-roles"><div class="force-roles-head"><span class="force-roles-label">Roles</span>${stalling ? html`<span class="force-stalling" title="Every live member is idle — nothing appears to be in flight. The force may be waiting on a nudge, a decision, or the next phase.">⚠ stalling</span>` : null}</div>${[...roles].map(([role, list]) => html`<div key=${role} class="force-role-row"><span class="force-role-name">${role}</span><span class="force-role-members">${list.map((member) => {
      const liveness = !member.online ? 'dead' : member.state?.status === 'idle' ? 'idle' : 'working';
      const pct = Math.round(Number(member.state?.context_pct || 0));
      const name = member.title || member.conv_id?.slice(0, 8) || '(unnamed)';
      return html`<span key=${member.agent_id || member.conv_id} class=${`force-member force-member-${liveness}`} title=${`${name} — ${liveness}${pct ? ` · context ${pct}%` : ''}`}>${liveness === 'working' ? '●' : liveness === 'idle' ? '○' : '✕'} ${name}${pct ? html` <span class="force-member-ctx">${pct}%</span>` : null}</span>`;
    })}</span></div>`)}</div>` : null}
    <div class="force-controls"><button class="force-rebrief-btn" data-act="rebrief-force" data-group=${group.name} data-label=${group.name} title="Re-brief the force — re-deliver the source template's current work pattern to the live roster, with the mission interpolated. Useful when the roster drifted or the original briefing scrolled out of context.">↻ re-brief</button><button class="force-standdown-btn" data-act="stand-down-force" data-group=${group.name} data-label=${group.name} title="Stand down the force — the mirror of deploy. Retires every member and sweeps the deploy-seeded rhythms + pending waves, keeping the group as a dormant record (mission & history preserved). Not a delete.">⏻ stand down</button></div>
  </div>`;
}

function GroupLinksSection({ group, snapshot }) {
  const links = snapshot?.links || [];
  const rows = [
    ...links.filter((link) => link.from === group.name).map((link) => ({ link, direction: 'out' })),
    ...links.filter((link) => link.to === group.name).map((link) => ({ link, direction: 'in' })),
  ];
  return html`<div class="group-links-section">
    ${rows.length ? html`<strong style="font-size:11px"><${ThemeText} regular="Links" wizard="Arcane channels" /></strong><table><thead><tr><th></th><th><${ThemeText} regular="Other group" wizard="Other party" /></th><th>Mode</th><th></th></tr></thead><tbody>${rows.map(({ link, direction }) => {
      const other = direction === 'out' ? link.to : link.from;
      return html`<tr key=${`${direction}-${link.id}`}><td><span class="muted">${direction === 'out' ? '→' : '←'}</span></td><td><span class="rowname">${other || '(deleted)'}</span></td><td><span class="id">${link.mode}</span></td><td><div class="row-actions"><button data-act="link-edit" data-id=${link.id} data-from=${link.from} data-to=${link.to} data-mode=${link.mode} title=${isWizardActive() ? 'Rebind this arcane channel' : "Change this link's mode"}><${ThemeText} regular="edit" wizard="rebind" /></button><button class="danger" data-act="link-delete" data-id=${link.id} data-group=${group.name} data-from=${link.from} data-to=${link.to} title=${isWizardActive() ? 'Sever this arcane channel' : 'Remove this link'}>×</button></div></td></tr>`;
    })}</tbody></table>` : html`<span class="muted" style="font-size:11px"><${ThemeText} regular="No links involving this group." wizard="No arcane channels are woven to or from this party." /></span>`}
    <button data-act="link-new" data-from=${group.name} data-label=${group.name} title=${isWizardActive() ? 'Weave an outbound arcane channel from this party' : 'Add an outbound link from this group'}><${ThemeText} regular="+ link" wizard="+ weave channel" /></button>
  </div>`;
}

function RealGroup({ node, snapshot, actions, hoveredGroupKey }) {
  const { group } = node;
  const view = groupMembersView(group, groupShowOffline(group.name));
  const quickPinned = dashPrefs.getItem(`tclaude.dash.quickpin.${group.name}`) === '1';
  const classes = [
    quickPinned ? 'quick-pinned' : '',
    !quickPinned && hoveredGroupKey === group.name ? 'quick-hover' : '',
  ].filter(Boolean).join(' ');
  return html`<details
    class=${classes || undefined} data-group-key=${group.name} data-dnd-target-group=${group.name}
    open=${realGroupOpen(group, dashPrefs)}
  >
    <${RealGroupSummary} group=${group} activity=${node.activity} membersView=${view} snapshot=${snapshot} actions=${actions} />
    <div class="subtable">
      ${node.children.length ? html`<div class="group-subgroups">${node.children.map((child) => html`<${GroupNode} key=${child.key} node=${child} snapshot=${snapshot} actions=${actions} hoveredGroupKey=${hoveredGroupKey} />`)}</div>` : null}
      ${group.pending?.length ? html`<div class="group-pending-block"><div class="group-pending-title"><span class="group-pending-title-regular">Pending spawns</span><span class="group-pending-title-wizard">Currently summoning...</span></div><${PendingTable} rows=${group.pending} /></div>` : null}
      <div class="group-header-actions"><${GroupActions} group=${group} actions=${actions} /></div>
      <${ForceBlock} group=${group} />
      ${!view.members.length
        ? html`<div class="muted">(no members yet)</div>`
        : !view.visible.length
          ? html`<div class="muted">(${view.hiddenOffline} offline member${view.hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${view.hiddenOffline === 1 ? 'it' : 'them'})</div>`
          : html`<${MemberTable} members=${view.visible} group=${group} tableKey=${`group:${group.name}`} snapshot=${snapshot} actions=${actions} SortHead=${SortHead} />`}
      <${GroupLinksSection} group=${group} snapshot=${snapshot} />
    </div>
  </details>`;
}

function OnlineDot({ online }) {
  return html`<span class=${online ? 'online' : 'offline'} title=${online ? 'online' : 'offline'}>${online ? '●' : '○'}</span>`;
}

function VirtualShell({ group, target, children, summary }) {
  return html`<details
    class="group-virtual" data-group-key=${group.key || group.name}
    data-dnd-target-ungrouped=${target === 'ungrouped' ? '1' : undefined}
    data-dnd-target-retired=${target === 'retired' ? '1' : undefined}
    open=${virtualGroupOpen(group, dashPrefs)}
  ><summary>${summary}</summary><div class="subtable">${children}</div></details>`;
}

function VirtualBadge({ regularTitle, wizardTitle }) {
  return html`<span class="group-virtual-badge" title=${isWizardActive() ? wizardTitle : regularTitle}><${ThemeText} regular="virtual" wizard="ethereal" /></span>`;
}

function VirtualUngrouped({ group, snapshot, actions }) {
  const view = groupMembersView(group, offlineDefault('groups'));
  const summary = html`<strong class="group-name"><${ThemeText} regular=${group.name} wizard="Unbound" /></strong>
    <${GroupActivity} members=${view.members} snapshot=${snapshot} />
    <${VirtualBadge} regularTitle="A virtual group, not a real one — it can't be renamed, deleted, messaged or scheduled. It just collects agents that aren't in any group." wizardTitle="An ethereal party, not a true one — it cannot be renamed, dispelled, whispered to, or scheduled. It gathers familiars bound to no party." />
    <span class="muted">— <${ThemeText}
      regular=${`${view.members.length} agent${view.members.length === 1 ? '' : 's'} not in any group${view.hiddenOffline ? ` · ${view.hiddenOffline} offline hidden` : ''}`}
      wizard=${`${view.members.length} unbound familiar${view.members.length === 1 ? '' : 's'}${view.hiddenOffline ? ` · ${view.hiddenOffline} slumbering hidden` : ''}`}
    /></span>`;
  const body = !view.members.length
    ? html`<div class="muted"><${ThemeText} regular="(no ungrouped agents)" wizard="(no unbound familiars)" /></div>`
    : !view.visible.length
      ? html`<div class="muted"><${ThemeText}
        regular=${`(${view.hiddenOffline} offline agent${view.hiddenOffline === 1 ? '' : 's'} hidden — toggle "show offline" to see ${view.hiddenOffline === 1 ? 'it' : 'them'})`}
        wizard=${`(${view.hiddenOffline} slumbering familiar${view.hiddenOffline === 1 ? '' : 's'} hidden — enable "show slumbering" to reveal ${view.hiddenOffline === 1 ? 'it' : 'them'})`}
      /></div>`
      : html`<${MemberTable} members=${view.visible} tableKey="virtual:ungrouped" ungrouped=${true} snapshot=${snapshot} actions=${actions} SortHead=${SortHead} />`;
  return html`<${VirtualShell} group=${group} target="ungrouped" summary=${summary}>${body}<//>`;
}

function ConversationsTable({ rows }) {
  return html`<table><${SortHead} table="conversations" columns=${CONVERSATIONS_COLS} /><tbody>
    ${applySort('conversations', rows, CONVERSATIONS_ACCESSORS).map((row) => html`<tr
      key=${row.conv_id} class="dnd-draggable" draggable=${true} data-key=${row.conv_id}
      data-dnd-source-conversation="1" data-dnd-conv=${row.conv_id}
      data-dnd-agent=${row.agent_id || row.conv_id} data-dnd-label=${row.title || row.conv_id}
    ><td><${OnlineDot} online=${row.online} /></td><td class="id">${shortId(row.conv_id)}</td><td><span class="rowname">${row.title || '(untitled)'}</span></td><td><span class="last-hook">${row.modified ? relTime(row.modified) : ''}</span></td><td><div class="row-actions"><button class="primary" data-act="promote-agent" data-conv=${row.conv_id} data-label=${row.title || row.conv_id} title=${isWizardActive() ? 'Awaken this plain scroll as a familiar' : 'Promote this conversation into an agent'}>${isWizardActive() ? 'awaken' : 'promote'}</button></div></td></tr>`)}
  </tbody></table>`;
}

function RetiredTable({ rows }) {
  return html`<table><${SortHead} table="retired" columns=${RETIRED_COLS} /><tbody>
    ${applySort('retired', rows, RETIRED_ACCESSORS).map((row) => html`<tr
      key=${row.conv_id} class="dnd-draggable" draggable=${true} data-key=${row.conv_id}
      data-dnd-source-retired="1" data-dnd-conv=${row.conv_id} data-dnd-agent=${row.agent_id || row.conv_id} data-dnd-label=${row.title || row.conv_id}
    ><td><${OnlineDot} online=${row.online} /></td><td class="id" title=${idTooltip(row.agent_id, row.conv_id)}>${shortAgentId(row.agent_id, row.conv_id)}</td><td><span class="rowname">${row.title || '(untitled)'}</span></td><td><span class="last-hook">${row.retired_at ? relTime(row.retired_at) : ''}</span></td><td title=${row.retired_by || undefined}>${row.retired_by_display || row.retired_by || ''}</td><td class="muted">${row.retire_reason || ''}</td><td><div class="row-actions"><button class="primary" data-act="reinstate-agent" data-conv=${row.conv_id} data-agent=${row.agent_id || row.conv_id} data-label=${row.title || row.conv_id} title=${isWizardActive() ? 'Restore this banished familiar to active status' : 'Reinstate this agent back to active status'}>${isWizardActive() ? 'restore' : 'reinstate'}</button></div></td></tr>`)}
  </tbody></table>`;
}

function ReplacedTable({ rows }) {
  return html`<table><${SortHead} table="replaced" columns=${REPLACED_COLS} /><tbody>
    ${applySort('replaced', rows, REPLACED_ACCESSORS).map((row) => {
      const actor = row.actor_title || shortId(row.actor_conv_id);
      return html`<tr key=${row.conv_id} data-key=${row.conv_id}><td><${OnlineDot} online=${row.online} /></td><td class="id">${shortId(row.conv_id)}</td><td><span class="rowname">${row.title || '(untitled)'}</span></td><td><span class="muted" title=${`${row.actor_title || row.actor_conv_id}${row.actor_retired ? ' (retired actor)' : ''}`}>${actor}${row.actor_retired ? ' 🪦' : ''}</span></td><td><span class="last-hook" title=${row.replaced_at || ''}>${row.reason || 'replaced'}${row.replaced_at ? ` · ${relTime(row.replaced_at)}` : ''}</span></td><td><div class="row-actions"><button data-act="copy-generation-id" data-conv=${row.conv_id} data-label=${row.title || row.conv_id} title="Copy this generation's full conv-id — inspect it out-of-band with 'claude --resume <id>' from its dir, or 'tclaude agent seance --target <id>'">copy id</button><button class="danger" data-act="delete-generation" data-conv=${row.conv_id} data-label=${row.title || row.conv_id} data-actor=${actor} title=${isWizardActive() ? 'Forever erase only this past incarnation. The living familiar and its other incarnations remain untouched.' : 'Permanently delete just this past generation (its .jsonl + DB rows). The live agent and its other generations are untouched.'}>${isWizardActive() ? 'erase incarnation' : 'delete generation'}</button></div></td></tr>`;
    })}
  </tbody></table>`;
}

function PendingTable({ rows }) {
  return html`<table><${SortHead} table="pending" columns=${PENDING_COLS} /><tbody>
    ${applySort('pending', rows, PENDING_ACCESSORS).map((row) => html`<tr
      key=${row.label} data-key=${row.label} class="dnd-draggable" draggable=${true}
      data-dnd-pending="1" data-dnd-conv=${row.label} data-dnd-label=${row.label}
    ><td><${OnlineDot} online=${row.online} /></td><td class="id" title=${idTooltip(row.agent_id, row.agent_id ? '' : row.label)}>${shortAgentId(row.agent_id, '') || row.label}</td><td><span class="rowname">${row.name || row.role || '(unnamed)'}</span></td><td>${row.group || '(none)'}</td><td><span class="muted" title=${row.cwd || ''}>${row.cwd ? shortCwd(row.cwd) : ''}</span></td><td><span class="last-hook">${row.created_at ? relTime(row.created_at) : ''}</span></td><td><div class="row-actions"><button class="primary" disabled=${!row.online} data-act=${row.online ? 'focus-pending' : undefined} data-label=${row.online ? row.label : undefined} title=${row.online
      ? (isWizardActive() ? "Open this would-be familiar's scrying portal so you can clear its summoning gate. Once cleared, it joins the living roster." : "Open this spawn's pane so you can clear its startup gate — trust the dir, dismiss the new-config prompt, or finish OpenAI auth. Once cleared it takes its first turn and becomes a normal agent.")
      : (isWizardActive() ? 'This would-be familiar’s portal has vanished and will soon fade from the summoning gate.' : "This spawn's tmux pane is gone — it can no longer be focused, and will clear from this list shortly.")}>${isWizardActive() ? 'open portal' : 'focus'}</button><button class="danger" data-act="delete-pending" data-label=${row.label} title=${isWizardActive() ? 'Dispel this failed summoning and close its portal. It never became a familiar, so no conversation scroll is lost.' : 'Delete this stuck spawn — kills its pane (if any) and removes it from the pending list. Use when a spawn will never clear its startup gate. It never became a real agent, so there is no conversation to keep.'}>${isWizardActive() ? '🪄 dispel' : '🗑 delete'}</button></div></td></tr>`)}
  </tbody></table>`;
}

function VirtualList({ group, kind, Table, names, target }) {
  const rows = group.members || [];
  const total = group.paging?.total ?? rows.length;
  const summary = html`<strong class="group-name"><${ThemeText} regular=${group.name} wizard=${names.wizardName} /></strong>
    <${VirtualBadge} regularTitle=${names.regularTitle} wizardTitle=${names.wizardTitle} />
    <span class="muted">— <${ThemeText} regular=${names.regularCount(total)} wizard=${names.wizardCount(total)} /></span>`;
  const empty = html`<div class="muted"><${ThemeText} regular=${names.regularEmpty} wizard=${names.wizardEmpty} /></div>`;
  return html`<${VirtualShell} group=${group} target=${target} summary=${summary}>
    ${rows.length ? html`<${Table} rows=${rows} />` : empty}
    <${Pager} kind=${kind} paging=${group.paging} />
  <//>`;
}

const VIRTUAL_NAMES = {
  conversations: {
    wizardName: 'Plain scrolls', regularEmpty: '(no non-agent conversations)', wizardEmpty: '(no plain scrolls)',
    regularTitle: "A virtual group, not a real one — recent conversations that aren't agents. Drag one onto a group, or click promote, to make it an agent.",
    wizardTitle: 'An ethereal party of plain scrolls without familiars. Drag one onto a party, or awaken it, to call forth a familiar.',
    regularCount: (n) => `${n} conversation${n === 1 ? '' : 's'} that aren't agents`, wizardCount: (n) => `${n} plain scroll${n === 1 ? '' : 's'} awaiting ${n === 1 ? 'a familiar' : 'familiars'}`,
  },
  retired: {
    wizardName: 'Banished', regularEmpty: '(no retired agents)', wizardEmpty: '(no banished familiars)',
    regularTitle: 'A virtual group, not a real one — agents that were retired (demoted back to plain conversations). Drag an agent here to retire it; drag a retired row onto a group, or click reinstate, to bring it back.',
    wizardTitle: 'An ethereal party of banished familiars returned to plain scrolls. Drag a familiar here to banish it; drag one onto a party, or restore it, to bring it back.',
    regularCount: (n) => `${n} retired agent${n === 1 ? '' : 's'}`, wizardCount: (n) => `${n} banished familiar${n === 1 ? '' : 's'}`,
  },
  replaced: {
    wizardName: 'Past incarnations', regularEmpty: '(no replaced generations)', wizardEmpty: '(no past incarnations)',
    regularTitle: 'A virtual group, not a real one — superseded past generations of agents (left behind by reincarnate / /clear). Archival and read-mostly: copy a conv-id to inspect it, or delete a generation to prune it. The live agent is never affected.',
    wizardTitle: 'An ethereal archive of superseded incarnations left by reincarnate / /clear. Copy a conv-id to scry one, or erase an incarnation to prune it. The living familiar is never affected.',
    regularCount: (n) => `${n} replaced generation${n === 1 ? '' : 's'}`, wizardCount: (n) => `${n} past incarnation${n === 1 ? '' : 's'}`,
  },
  pending: {
    wizardName: 'Summoning', regularEmpty: '(no pending spawns)', wizardEmpty: '(no familiars awaiting summoning)',
    regularTitle: "A virtual group, not a real one — dashboard spawns waiting to clear a startup gate (untrusted dir / config prompt / OpenAI auth). Click a row's focus button to open its pane and clear the gate; it then becomes a normal agent.",
    wizardTitle: "An ethereal antechamber for familiars caught at the summoning gate. Open a familiar's portal to clear the ward; it then joins the living roster.",
    regularCount: (n) => `${n} pending spawn${n === 1 ? '' : 's'}`, wizardCount: (n) => `${n} familiar${n === 1 ? '' : 's'} caught at the summoning gate`,
  },
};

function GroupNode({ node, snapshot, actions, hoveredGroupKey }) {
  const group = node.group;
  if (!group.virtual) return html`<${RealGroup} node=${node} snapshot=${snapshot} actions=${actions} hoveredGroupKey=${hoveredGroupKey} />`;
  if (group.conversations) return html`<${VirtualList} group=${group} kind="conversations" Table=${ConversationsTable} names=${VIRTUAL_NAMES.conversations} />`;
  if (group.retired) return html`<${VirtualList} group=${group} kind="retired" Table=${RetiredTable} names=${VIRTUAL_NAMES.retired} target="retired" />`;
  if (group.replaced) return html`<${VirtualList} group=${group} kind="replaced" Table=${ReplacedTable} names=${VIRTUAL_NAMES.replaced} />`;
  if (group.pending) return html`<${VirtualList} group=${group} kind="pending" Table=${PendingTable} names=${VIRTUAL_NAMES.pending} />`;
  return html`<${VirtualUngrouped} group=${group} snapshot=${snapshot} actions=${actions} />`;
}

export function GroupsNativeList({ groups, snapshot, actions, hoveredGroupKey = null }) {
  if (!groups?.length) return html`<div class="empty">No groups yet. Create one with the <strong><span class="group-create-label-regular">+ new group</span><span class="group-create-label-wizard">⚔ Form a party</span></strong> button above.</div>`;
  const tree = buildGroupTree(groups, (group) => realGroupOpen(group, dashPrefs));
  return tree.map((node) => html`<${GroupNode} key=${node.key} node=${node} snapshot=${snapshot} actions=${actions} hoveredGroupKey=${hoveredGroupKey} />`);
}
