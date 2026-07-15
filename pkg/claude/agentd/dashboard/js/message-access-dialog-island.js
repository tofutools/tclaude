import { Fragment, h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { registerMessageAccessDialogController } from './message-access-dialog-controller.js';
import {
  agentCandidates, groupMembers, groupsForPicker, permissionRows,
  permissionSeed, senderOnline, sudoByConv, sudoSlugRows,
} from './message-access-dialog-model.js';
import { idTooltip, shortAgentId } from './helpers.js';
import { wizWord } from './slop.js';

const html = htm.bind(h);

function Words({ plain, wizard }) {
  return html`<span class="theme-copy-regular">${plain}</span><span class="theme-copy-wizard">${wizard}</span>`;
}

function errorText(error) { return error?.message || String(error); }

function memberSelectionKeys(member) {
  return [...new Set([member.agent_id, member.conv_id, member.key].filter(Boolean))];
}

function memberIsSelected(selection, member) {
  return memberSelectionKeys(member).some((key) => selection.has(key));
}

function ErrorLine({ id, value, className = 'cron-create-error' }) {
  return html`<div id=${id} class=${className} role="alert">${value || ''}</div>`;
}

function fieldSubmit(submit) {
  return (event) => {
    if (event.key !== 'Enter' || (!event.ctrlKey && !event.metaKey) || event.isComposing || event.keyCode === 229) return;
    event.preventDefault();
    void submit();
  };
}

function useLiveTheme() {
  const [, setRevision] = useState(0);
  useEffect(() => {
    const refreshThemeCopy = () => setRevision((value) => value + 1);
    document.addEventListener('tclaude:wizard', refreshThemeCopy);
    return () => document.removeEventListener('tclaude:wizard', refreshThemeCopy);
  }, []);
}

export function TargetPicker({ prefix, value, onChange, snapshot, pickAgent }) {
  useLiveTheme();
  const scope = value.scopeGroup || '';
  const groups = groupsForPicker(snapshot, scope);
  const members = scope ? groupMembers(snapshot, scope) : [];
  const groupOptions = value.groupName && !groups.includes(value.groupName)
    ? [value.groupName, ...groups] : groups;
  const memberOptions = value.target && !members.some((member) => member.key === value.target)
    ? [{ key: value.target, title: `${value.target} (missing)`, online: false }, ...members] : members;
  const modeName = `${prefix}-target-mode`;
  const setMode = (mode) => onChange({ ...value, mode });
  const choose = async () => {
    const picked = await pickAgent({ title: 'Pick target', identity: 'agent' });
    if (picked) onChange({ ...value, target: picked });
  };
  return html`<div class="cron-create-target" id=${`${prefix}-target-picker`}>
    <div class="cron-target-modes">
      <label><input type="radio" name=${modeName} value="solo" checked=${value.mode === 'solo'}
        onChange=${() => setMode('solo')} /> <${Words} plain="Solo agent" wizard="Solo familiar"/></label>
      <label><input type="radio" name=${modeName} value="group" checked=${value.mode === 'group'}
        onChange=${() => setMode('group')} /> <${Words} plain="Group (multicast)" wizard="Party (multicast)"/></label>
    </div>
    ${value.mode === 'solo' && !scope && html`<div class="cron-target-input-row" id=${`${prefix}-target-solo`}>
      <input id=${`${prefix}-target`} type="text" value=${value.target}
        placeholder="agt_ id / title / conv-id / 8+-char prefix" autocomplete="off" spellcheck="false"
        onInput=${(event) => onChange({ ...value, target: event.currentTarget.value })} />
      <button type="button" id=${`${prefix}-target-pick`} title="Pick from the agent / familiar list" onClick=${choose}>🔍</button>
    </div>`}
    ${value.mode === 'solo' && scope && html`<div class="cron-target-input-row" id=${`${prefix}-target-scoped`}>
      <select id=${`${prefix}-scoped-member`} value=${value.target}
        onChange=${(event) => onChange({ ...value, target: event.currentTarget.value })}>
        ${memberOptions.length
          ? html`<${Fragment}><option value="">${wizWord('(pick a member)', '(pick a familiar)')}</option>${memberOptions.map((member) => html`<option key=${member.key} value=${member.key}>${member.title || member.conv_id}${member.online ? '' : ' (offline)'}</option>`)}</${Fragment}>`
          : html`<option value="">${wizWord('(no members in this group)', '(no familiars in this party)')}</option>`}
      </select>
    </div>`}
    ${value.mode === 'group' && html`<div class="cron-target-input-row" id=${`${prefix}-target-group`}>
      <select id=${`${prefix}-group`} value=${value.groupName} disabled=${!!scope}
        onChange=${(event) => onChange({ ...value, groupName: event.currentTarget.value })}>
        ${groupOptions.length
          ? html`<${Fragment}><option value="">${wizWord('(pick a group)', '(pick a party)')}</option>${groupOptions.map((name) => html`<option key=${name} value=${name}>${name}${!groups.includes(name) ? ' (missing)' : ''}</option>`)}</${Fragment}>`
          : html`<option value="">${wizWord('(no groups — create one first)', '(no parties — form one first)')}</option>`}
      </select>
    </div>`}
  </div>`;
}

function AgentPicker({ descriptor, state, snapshot, confirmDiscard }) {
  const [query, setQuery] = useState('');
  const [includeOffline, setIncludeOffline] = useState(false);
  const [highlight, setHighlight] = useState(0);
  const searchRef = useRef(null);
  const highlightedRef = useRef(null);
  const candidates = agentCandidates(snapshot, { includeOffline, query });
  const activeSudo = sudoByConv(snapshot);
  const bounded = Math.max(0, Math.min(highlight, Math.max(0, candidates.length - 1)));
  const activeID = candidates[bounded] ? `cron-pick-target-option-${bounded}` : undefined;
  const activeKey = candidates[bounded]?.agent_id || candidates[bounded]?.conv_id || '';
  useEffect(() => { if (bounded !== highlight) setHighlight(bounded); }, [bounded, highlight]);
  useEffect(() => { highlightedRef.current?.scrollIntoView?.({ block: 'nearest' }); }, [bounded, activeKey]);
  const choose = (agent) => state.finishPicker(descriptor.identity === 'conv'
    ? agent.conv_id : (agent.agent_id || agent.conv_id));
  const onKeyDown = (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === 'ArrowDown') { event.preventDefault(); setHighlight(Math.min(bounded + 1, candidates.length - 1)); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); setHighlight(Math.max(bounded - 1, 0)); }
    else if (event.key === 'Enter' && candidates[bounded]) { event.preventDefault(); choose(candidates[bounded]); }
  };
  return html`<${Overlay} id="cron-pick-target-modal" dialogClass="add-member-modal"
    labelledby="cron-pick-target-title" onClose=${() => state.finishPicker('')}
    dirty=${false} blocked=${false} confirmDiscard=${confirmDiscard}>
    <h3 id="cron-pick-target-title">${descriptor.title} <span class="muted"><${Words} plain="— pick agent" wizard="— pick familiar"/></span></h3>
    <input ref=${searchRef} id="cron-pick-target-search" class="add-member-search" type="text"
      value=${query} placeholder="Filter by title / role / descr / conv-id / group…" autocomplete="off" spellcheck="false"
      role="combobox" aria-label="Filter agents" aria-controls="cron-pick-target-list" aria-expanded="true"
      aria-autocomplete="list" aria-activedescendant=${activeID}
      onInput=${(event) => { setQuery(event.currentTarget.value); setHighlight(0); }} onKeyDown=${onKeyDown} />
    <div class="add-member-list" id="cron-pick-target-list" role="listbox">
      ${candidates.length === 0 ? html`<div class="add-member-empty">No matching conversations. ${includeOffline ? '(Try a different filter.)' : '(Try ticking “Include offline / archived” for a wider pool.)'}</div>`
        : candidates.map((agent, index) => html`<div key=${agent.agent_id || agent.conv_id}
          ref=${index === bounded ? highlightedRef : null} id=${`cron-pick-target-option-${index}`}
          role="option" aria-selected=${index === bounded ? 'true' : 'false'}
          class=${`add-member-row${index === bounded ? ' highlighted' : ''}`} data-i=${index}
          onMouseDown=${() => choose(agent)}>
          <span class=${agent.online ? 'online' : 'offline'} title=${agent.online ? 'online' : 'offline'}>${agent.online ? '●' : '○'}</span>
          <span class="rowname">${agent.title || '(unnamed)'}</span>
          <span class="id" title=${idTooltip(agent.agent_id, agent.conv_id)}>${shortAgentId(agent.agent_id, agent.conv_id)}</span>
          ${descriptor.showSudo && activeSudo.get(agent.conv_id)?.length
            ? html`<span class="sudo-badge" title=${`${activeSudo.get(agent.conv_id).length} active sudo grant(s)`}>🔓</span>` : null}
          ${agent.memberships.length ? html`<span class="groups-tag">in: ${agent.memberships.map((item) => item.group).join(', ')}</span>` : null}
        </div>`)}
    </div>
    <div class="add-member-foot">
      <label title=${descriptor.includeOfflineHint || 'Include offline / archived agents'}>
        <input id="cron-pick-target-all" type="checkbox" checked=${includeOffline}
          onChange=${(event) => { setIncludeOffline(event.currentTarget.checked); setHighlight(0); }} />Include offline / archived
      </label>
      <span class="spacer"></span><span><kbd>↑↓</kbd> nav · <kbd>Enter</kbd> pick · <kbd>Esc</kbd> close</span>
    </div>
  </${Overlay}>`;
}

function MessageDialog({ descriptor, state, actions, snapshot, confirmDiscard }) {
  useLiveTheme();
  const initial = descriptor.prefill || {};
  const scopedGroup = initial.targetMode === 'group' && initial.groupName ? initial.groupName : '';
  const [from, setFrom] = useState(initial.from || '');
  const [target, setTarget] = useState(() => ({
    mode: initial.targetMode === 'group' ? 'group' : 'solo', target: initial.target || '',
    groupName: initial.groupName || '', scopeGroup: '',
  }));
  const [subject, setSubject] = useState('');
  const [body, setBody] = useState('');
  const [role, setRole] = useState(initial.role || '');
  const [customized, setCustomized] = useState(false);
  const [selected, setSelected] = useState(() => new Set(
    groupMembers(snapshot, scopedGroup).flatMap(memberSelectionKeys),
  ));
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const members = scopedGroup ? groupMembers(snapshot, scopedGroup) : [];
  const groupExists = !scopedGroup || (snapshot?.groups || []).some((group) => group.name === scopedGroup);
  const selectedMembers = customized ? members.filter((member) => memberIsSelected(selected, member)) : members;
  const initialMode = initial.targetMode === 'group' ? 'group' : 'solo';
  const dirty = from !== (initial.from || '') || !!subject || !!body || role !== (initial.role || '') || customized || (!scopedGroup && (
    target.mode !== initialMode || target.target !== (initial.target || '') || target.groupName !== (initial.groupName || '')
  ));
  const chooseFrom = async () => {
    const picked = await state.pickAgent({ title: 'Pick sender', identity: 'agent' });
    if (picked) setFrom(picked);
  };
  const toggleMember = (member, checked) => {
    const next = new Set(customized ? selected : members.flatMap(memberSelectionKeys));
    for (const key of memberSelectionKeys(member)) {
      if (checked) next.add(key); else next.delete(key);
    }
    setSelected(next); setCustomized(true);
  };
  const submit = async () => {
    if (busyRef.current) return;
    setError('');
    if (!from.trim()) { setError(wizWord('From is required — type a sender agent or use 🔍 to pick.', 'From is required — name a sending familiar or use 🔍 to pick.')); return; }
    if (!body) { setError(wizWord('Body is required (the message text to send).', 'Missive text is required.')); return; }
    let to = '', explicit = null;
    if (scopedGroup) {
      if (!groupExists) { setError(`Group “${scopedGroup}” no longer exists — choose a new launcher context.`); return; }
      if (!selectedMembers.length) { setError(wizWord('Pick at least one recipient — tick the members this message should reach.', 'Pick at least one recipient — tick the familiars this missive should reach.')); return; }
      to = `group:${scopedGroup}`;
      if (customized) explicit = selectedMembers.map((member) => member.agent_id || member.conv_id);
    } else if (target.mode === 'group') {
      if (!target.groupName) { setError(wizWord('Pick a group from the dropdown (or create one first via the Groups tab).', 'Pick a party from the dropdown (or form one first via the Parties tab).')); return; }
      if (!(snapshot?.groups || []).some((group) => group.name === target.groupName)) {
        setError(`Group “${target.groupName}” no longer exists — it was not retargeted.`); return;
      }
      to = `group:${target.groupName}`;
    } else {
      to = target.target.trim();
      if (!to) { setError(wizWord('Target is required — type a title / conv-id or use 🔍 to pick.', 'Recipient is required — name a familiar or use 🔍 to pick.')); return; }
    }
    busyRef.current = true;
    setBusy(true);
    try {
      const payload = { from: from.trim(), to, subject: subject.trim(), body };
      if (explicit) payload.members = explicit;
      if (to.startsWith('group:') && role.trim()) payload.role = role.trim();
      await actions.sendMessage(payload);
      state.close();
    } catch (cause) { setError(errorText(cause)); }
    finally { busyRef.current = false; setBusy(false); }
  };
  return html`<${Overlay} id="message-create-modal" labelledby="message-create-title"
    onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}>
    <h3 id="message-create-title"><${Words}
      plain=${scopedGroup ? `Send a message to group “${scopedGroup}”` : 'Send a message'}
      wizard=${scopedGroup ? `Send a missive to party “${scopedGroup}”` : 'Send a missive'}/></h3>
    <p id="message-create-desc" class="modal-hint"><${Words}
      plain=${scopedGroup ? `Delivers one immediate message to the members of “${scopedGroup}” ticked below.` : 'Delivers one immediate message to a single agent, or multicasts it to every member of a group.'}
      wizard=${scopedGroup ? `Delivers one immediate missive to the familiars of “${scopedGroup}” ticked below.` : 'Delivers one immediate missive to a familiar, or multicasts it to every familiar in a party.'}/></p>
    <label class="cron-create-row"><span class="cron-create-label">From</span><div class="cron-create-target"><div class="cron-target-input-row">
      <input id="message-create-from" type="text" value=${from} placeholder="sender — agt_ id / title / conv-id / 8+-char prefix"
        autocomplete="off" spellcheck="false" onInput=${(event) => setFrom(event.currentTarget.value)} />
      <button type="button" id="message-create-from-pick" title="Pick from the agent / familiar list" onClick=${chooseFrom}>🔍</button>
    </div></div></label>
    ${!scopedGroup ? html`<label class="cron-create-row" id="message-create-target-row"><span class="cron-create-label"><${Words} plain="Target" wizard="Recipient"/></span>
      <${TargetPicker} prefix="message-create" value=${target} onChange=${setTarget} snapshot=${snapshot} pickAgent=${state.pickAgent}/>
    </label>` : html`<div class="cron-create-row" id="message-create-group-row"><span class="cron-create-label">Recipients</span><div class="cron-create-target">
      <p class="cleanup-hint" id="message-create-group-hint">${groupExists
        ? `${members.length} current member${members.length === 1 ? '' : 's'} — ${customized ? 'custom selection is retained across live updates.' : 'all selected follows live membership.'}`
        : `Group “${scopedGroup}” is missing; sending is blocked.`}</p>
      <div class="cleanup-toolbar"><button type="button" id="message-create-members-all" onClick=${() => { setCustomized(false); setSelected(new Set(members.map((member) => member.key))); }}>select all</button>
        <button type="button" id="message-create-members-none" onClick=${() => { setCustomized(true); setSelected(new Set()); }}>select none</button>
        <span class="spacer"></span><span class="cleanup-count" id="message-create-members-count">${selectedMembers.length} of ${members.length} selected</span></div>
      <div class="cleanup-list" id="message-create-members">${members.length ? members.map((member) => html`<div class="cleanup-row" key=${member.key}><label>
        <input type="checkbox" data-conv=${member.conv_id} checked=${!customized || memberIsSelected(selected, member)}
          onChange=${(event) => toggleMember(member, event.currentTarget.checked)} />
        <span class="title">${member.title || '(untitled)'}</span><span class="id" title=${idTooltip(member.agent_id, member.conv_id)}>${shortAgentId(member.agent_id, member.conv_id)}</span>
        ${member.online ? html`<span class="cleanup-badge online">online</span>` : null}
      </label></div>`) : html`<div class="cleanup-empty">no members in this group</div>`}</div>
    </div></div>`}
    ${(scopedGroup || target.mode === 'group') && html`<label class="cron-create-row" id="message-create-role-row"><span class="cron-create-label"
      title="Optional group role filter; blank or all reaches every selected member"><${Words} plain="Role filter" wizard="Class filter"/></span>
      <input id="message-create-role" type="text" value=${role} placeholder="optional — blank / all = entire target (e.g. dev)"
        autocomplete="off" spellcheck="false" onInput=${(event) => setRole(event.currentTarget.value)} /></label>`}
    <label class="cron-create-row"><span class="cron-create-label">Subject</span><input id="message-create-subject" type="text" maxlength="100"
      value=${subject} placeholder="optional, shows in inbox listings" autocomplete="off" spellcheck="false" onInput=${(event) => setSubject(event.currentTarget.value)} /></label>
    <label class="cron-create-row"><span class="cron-create-label"><${Words} plain="Body" wizard="Missive"/></span><textarea id="message-create-body" rows="4"
      value=${body} placeholder=${wizWord('message text (required)', 'missive text (required)')} spellcheck="false" onInput=${(event) => setBody(event.currentTarget.value)} onKeyDown=${fieldSubmit(submit)}></textarea></label>
    <${ErrorLine} id="message-create-error" value=${error}/>
    <div class="modal-buttons"><button id="message-create-cancel" type="button" disabled=${busy} onClick=${state.close}><${Words} plain="Cancel" wizard="Dispel"/></button>
      <span class="spacer"></span><button id="message-create-submit" class="primary" type="button" disabled=${busy || (scopedGroup && (!groupExists || !selectedMembers.length))} onClick=${submit}>
        <${Words} plain=${busy ? 'Sending…' : 'Send'} wizard=${busy ? 'Sending…' : '✒ Send missive'}/></button></div>
  </${Overlay}>`;
}

function HumanReplyDialog({ descriptor, state, actions, snapshot, confirmDiscard }) {
  const context = descriptor.context || {};
  const [body, setBody] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const [serverOffline, setServerOffline] = useState(false);
  useEffect(() => { setServerOffline(false); }, [snapshot]);
  const online = !serverOffline && senderOnline(snapshot, context.agent || '', context.conv || '');
  const label = context.label || context.conv || '(agent)';
  const submit = async () => {
    if (busyRef.current) return;
    const clean = body.trim(); setError('');
    if (!clean) { setError('Reply is required — type your answer.'); return; }
    if (!online) { setError('The agent is offline — it has no live session to receive a reply.'); return; }
    busyRef.current = true;
    setBusy(true);
    try { await actions.replyHuman({ id: Number(context.id), body: clean, label }); state.close(); }
    catch (cause) { if (cause?.code === 'offline') setServerOffline(true); setError(errorText(cause)); }
    finally { busyRef.current = false; setBusy(false); }
  };
  return html`<${Overlay} id="human-reply-modal" labelledby="human-reply-title"
    onClose=${state.close} dirty=${!!body} blocked=${busy} confirmDiscard=${confirmDiscard}>
    <h3 id="human-reply-title"><span class="human-reply-title-regular">Reply to agent</span><span class="human-reply-title-wizard">✒ Answer the familiar</span></h3>
    <p id="human-reply-desc" class="modal-hint">Sends your answer to this agent's inbox and nudges its terminal. Delivered as a message from you, the operator.</p>
    <label class="cron-create-row"><span class="cron-create-label">To</span><div class="cron-create-target"><div id="human-reply-to">
      <div class="human-reply-to-name">${label}</div>${context.subject ? html`<div class="human-reply-to-subject">re: ${context.subject}</div>` : null}
    </div><div id="human-reply-status" class=${`human-reply-status ${online ? 'online' : 'offline'}`}>${online
      ? '🟢 Online — your reply is delivered to its inbox and its terminal is nudged.'
      : '⚫ Offline — this agent has no live session, so it can’t receive a reply. Replying is disabled until it’s back online.'}</div></div></label>
    <label class="cron-create-row"><span class="cron-create-label">Reply</span><textarea id="human-reply-body" rows="4" value=${body}
      placeholder="your reply (required) — ⌘/Ctrl+Enter to send" spellcheck="false" onInput=${(event) => setBody(event.currentTarget.value)} onKeyDown=${fieldSubmit(submit)}></textarea></label>
    <${ErrorLine} id="human-reply-error" value=${error}/><div class="modal-buttons">
      <button id="human-reply-cancel" type="button" disabled=${busy} onClick=${state.close}>Cancel</button><span class="spacer"></span>
      <button id="human-reply-submit" class="primary" type="button" disabled=${busy || !online} onClick=${submit}>${busy ? 'Sending…' : 'Send reply'}</button>
    </div>
  </${Overlay}>`;
}

function SudoGrantDialog({ descriptor, state, actions, snapshot, confirmDiscard }) {
  const [conv, setConv] = useState(descriptor.conv || '');
  const [selected, setSelected] = useState(() => new Set());
  const [duration, setDuration] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const rows = sudoSlugRows(snapshot);
  const dirty = conv !== (descriptor.conv || '') || selected.size > 0 || !!duration || !!reason;
  const toggle = (slug, checked) => setSelected((current) => {
    const next = new Set(current); if (checked) next.add(slug); else next.delete(slug); return next;
  });
  const submit = async () => {
    if (busyRef.current) return;
    setError('');
    if (!conv.trim()) { setError('Conv is required.'); return; }
    if (!selected.size) { setError('Pick at least one slug.'); return; }
    busyRef.current = true;
    setBusy(true);
    try { await actions.grantSudo({ conv: conv.trim(), slugs: [...selected], duration: duration.trim(), reason: reason.trim() }); state.close(); }
    catch (cause) { setError(errorText(cause)); }
    finally { busyRef.current = false; setBusy(false); }
  };
  return html`<${Overlay} id="sudo-grant-modal" dialogClass="sudo-grant-modal" labelledby="sudo-grant-title"
    onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}>
    <h3 id="sudo-grant-title">Grant sudo</h3><p class="sudo-grant-hint"><${Words}
      plain="Proactively elevate an agent for a bounded window." wizard="Bestow a bounded sudo boon on a familiar."/> Same blocklist + duration cap as the agent-initiated path; granted_by records <code>${'<human-dashboard>:proactive'}</code> on the audit row.</p>
    <label class="sudo-grant-row"><span class="sudo-grant-label">Conv</span><input id="sudo-grant-conv" type="text" value=${conv}
      placeholder="title / conv-id / 8+-char prefix" autocomplete="off" spellcheck="false" onInput=${(event) => setConv(event.currentTarget.value)} /></label>
    <label class="sudo-grant-row"><span class="sudo-grant-label">Slugs</span><div class="sudo-grant-slugs-wrap"><div class="sudo-grant-slugs-toolbar">
      <button type="button" id="sudo-grant-select-all" title="Select every slug except blocklisted ones" onClick=${() => setSelected(new Set(rows.filter((row) => !row.blocked).map((row) => row.slug)))}>all</button>
      <button type="button" id="sudo-grant-select-none" title="Clear the slug selection" onClick=${() => setSelected(new Set())}>none</button></div>
      <div id="sudo-grant-slugs" class="sudo-grant-slugs">${rows.map((row) => html`<label key=${row.slug} class=${`${row.blocked ? 'blocked' : ''}${selected.has(row.slug) ? ' checked' : ''}`} title=${row.descr || row.description || ''}>
        <input type="checkbox" value=${row.slug} disabled=${row.blocked} checked=${selected.has(row.slug)} onChange=${(event) => toggle(row.slug, event.currentTarget.checked)} />${row.slug}</label>`)}</div>
    </div></label>
    <label class="sudo-grant-row"><span class="sudo-grant-label">Duration</span><input id="sudo-grant-duration" type="text" value=${duration}
      placeholder="5m (default), 30m, 1h" autocomplete="off" spellcheck="false" onInput=${(event) => setDuration(event.currentTarget.value)} /></label>
    <label class="sudo-grant-row"><span class="sudo-grant-label">Reason</span><input id="sudo-grant-reason" type="text" value=${reason}
      placeholder="optional — surfaced in the audit row" autocomplete="off" spellcheck="false" onInput=${(event) => setReason(event.currentTarget.value)} onKeyDown=${fieldSubmit(submit)} /></label>
    <${ErrorLine} id="sudo-grant-error" className="sudo-grant-error" value=${error}/><div class="modal-buttons">
      <button id="sudo-grant-cancel" type="button" disabled=${busy} onClick=${state.close}>Cancel</button>
      <button id="sudo-grant-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>${busy ? 'Granting…' : 'Grant'}</button>
    </div>
  </${Overlay}>`;
}

function PermissionsDialog({ descriptor, state, actions, snapshot, confirmDiscard }) {
  // Seed and dirty comparison tuple are frozen for this keyed launch. Snapshot
  // updates may change rows/effective sources, never the draft baseline.
  const [baseline] = useState(() => permissionSeed(snapshot, descriptor));
  const [selection, setSelection] = useState(() => ({ ...baseline }));
  const [filter, setFilter] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const rows = permissionRows(snapshot, descriptor, selection);
  const visible = rows.filter((row) => !filter.trim() || [row.slug, row.description, row.descr]
    .some((value) => String(value || '').toLowerCase().includes(filter.trim().toLowerCase())));
  const currentEffect = (slug) => selection[slug] || 'default';
  const baselineEffect = (slug) => baseline[slug] || 'default';
  const dirty = rows.some((row) => currentEffect(row.slug) !== baselineEffect(row.slug));
  const groupMode = descriptor.mode === 'group';
  const setEffect = (slug, effect) => setSelection((current) => ({ ...current, [slug]: effect }));
  const submit = async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true); setError('');
    const full = Object.fromEntries(rows.map((row) => [row.slug, currentEffect(row.slug)]));
    try { await actions.savePermissions(descriptor, full); state.close(); }
    catch (cause) { setError(errorText(cause)); }
    finally { busyRef.current = false; setBusy(false); }
  };
  const shortConv = String(descriptor.conv || '').slice(0, 8);
  const subtitle = groupMode ? `Group: ${descriptor.group} · every current member receives these grants immediately`
    : descriptor.mode === 'agent' ? `Agent: ${descriptor.label || shortConv} · ${shortConv}`
    : `New agent${descriptor.label ? ` “${descriptor.label}”` : ''}${descriptor.group ? ` → ${descriptor.group}` : ''} · applied when it spawns`;
  const wizardSubtitle = groupMode ? `Party: ${descriptor.group} · every familiar receives these boons immediately`
    : descriptor.mode === 'agent' ? `Familiar: ${descriptor.label || shortConv} · ${shortConv}`
    : `New familiar${descriptor.label ? ` “${descriptor.label}”` : ''}${descriptor.group ? ` → ${descriptor.group}` : ''} · bestowed when summoned`;
  return html`<${Overlay} id="perm-edit-modal" dialogClass="perm-edit-modal" labelledby="perm-edit-title"
    onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}>
    <h3 id="perm-edit-title"><span class="perm-edit-title-regular">${groupMode ? 'Edit group permissions' : 'Edit permanent permissions'}</span>
      <span class="perm-edit-title-wizard">${groupMode ? '✨ Party Boons' : '📕 The Grimoire'}</span></h3>
    <div class="perm-edit-banner" id="perm-edit-banner">${groupMode
      ? html`<${Words} plain=${html`<strong>GROUP GRANTS</strong> — selected permissions apply immediately to every current member. An agent-level <strong>Deny</strong> still wins.`}
          wizard=${html`<strong>PARTY BOONS</strong> — bestow capabilities on every familiar in this party. A personal binding against one still wins.`}/>`
      : html`<${Words} plain=${html`<strong>PERMANENT</strong> — these per-agent overrides persist until changed. <strong>Grant</strong> adds a slug, <strong>Deny</strong> blocks inherited sources, and <strong>Default</strong> inherits them.`}
          wizard=${html`<strong>THE GRIMOIRE</strong> — these bindings follow this familiar until changed. <strong>Grant</strong> bestows a slug, <strong>Deny</strong> seals inherited boons away, and <strong>Default</strong> inherits them.`}/>`}</div>
    <p class="perm-edit-subtitle" id="perm-edit-subtitle"><${Words} plain=${subtitle} wizard=${wizardSubtitle}/></p>
    ${rows.some((row) => row.ownedGroups?.length) && html`<div class="perm-edit-owner-note" id="perm-edit-owner-note">👑 Owner-implied permissions are shown with their owned-group source; an explicit Deny remains the final veto.</div>`}
    <div class="perm-edit-toolbar"><input id="perm-edit-filter" type="text" value=${filter} placeholder="Filter slugs…" autocomplete="off" spellcheck="false"
      onInput=${(event) => setFilter(event.currentTarget.value)} /><button type="button" id="perm-edit-reset" title="Set every slug back to Default (inherit)"
      onClick=${() => setSelection(Object.fromEntries(rows.map((row) => [row.slug, 'default'])))}><span class="pe-btn-regular">${groupMode ? 'none granted' : 'all default'}</span><span class="pe-btn-wizard">unbind all</span></button></div>
    <div id="perm-edit-list" class="perm-edit-list">${visible.length ? visible.map((row) => html`<div class="perm-row" key=${row.slug} data-slug=${row.slug}>
      <div class="perm-row-info"><span class="perm-row-slug">${row.slug}${row.owner_implied ? html` <span class="owner-badge" title="Group ownership can confer this slug">👑 owner</span>` : null}</span>
        <span class="perm-row-desc" title=${row.description || row.descr || ''}>${row.description || row.descr || ''}</span></div>
      <div class="perm-tristate"><button type="button" data-effect="default" class=${currentEffect(row.slug) === 'default' ? 'active' : ''} onClick=${() => setEffect(row.slug, 'default')}>${groupMode ? html`<${Words} plain="Not granted" wizard="Unbound"/>` : 'Default'}</button>
        <button type="button" data-effect="grant" class=${currentEffect(row.slug) === 'grant' ? 'active' : ''} onClick=${() => setEffect(row.slug, 'grant')}>${groupMode ? html`<${Words} plain="Grant" wizard="Bestow"/>` : 'Grant'}</button>
        ${!groupMode && html`<button type="button" data-effect="deny" class=${currentEffect(row.slug) === 'deny' ? 'active' : ''} onClick=${() => setEffect(row.slug, 'deny')}>Deny</button>`}</div>
      <span class=${`perm-row-eff ${row.granted ? 'granted' : 'denied'}`}>${groupMode
        ? html`<${Words} plain=${row.granted ? '✓ via group' : '— not via group'} wizard=${row.granted ? '✨ boon active' : '— no boon'}/>`
        : currentEffect(row.slug) === 'deny' ? '✗ denied (explicit veto)'
        : row.granted ? `✓ ${row.sources.join(' + ')}` : '✗ denied (no source)'}</span>
    </div>`) : html`<div class="empty" style="padding:10px">${rows.length ? 'No matching permission slugs.' : 'No permission slugs registered.'}</div>`}</div>
    <${ErrorLine} id="perm-edit-error" className="sudo-grant-error" value=${error}/><div class="modal-buttons">
      <button id="perm-edit-cancel" type="button" disabled=${busy} onClick=${state.close}><span class="pe-btn-regular">Cancel</span><span class="pe-btn-wizard">Dispel</span></button>
      <button id="perm-edit-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>${busy ? 'Saving…' : 'Save'}</button>
    </div>
  </${Overlay}>`;
}

export function MessageAccessDialogApp({ state, actions, snapshot, confirmDiscard }) {
  const current = state.view.value;
  const descriptor = current.dialog;
  let parent = null;
  if (descriptor?.kind === 'message') parent = html`<${MessageDialog} key=${`message:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'human-reply') parent = html`<${HumanReplyDialog} key=${`reply:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'sudo-grant') parent = html`<${SudoGrantDialog} key=${`sudo:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'permissions') parent = html`<${PermissionsDialog} key=${`permissions:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  return html`<${Fragment}>${parent}${current.picker && html`<${AgentPicker} key=${`picker:${current.picker.launchID}`}
    descriptor=${current.picker} state=${state} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`}</${Fragment}>`;
}

export function mountMessageAccessDialogIsland({
  dialogHost, state, actions, snapshot, confirmDiscard, registerCleanup,
}) {
  const controller = {
    openMessage: state.openMessage,
    openHumanReply: state.openHumanReply,
    openSudoGrant: state.openSudoGrant,
    openAgentPermissions: state.openAgentPermissions,
    openGroupPermissions: state.openGroupPermissions,
    openBufferedPermissions: state.openBufferedPermissions,
    pickAgent: state.pickAgent,
  };
  let unregister = null;
  let unsubscribe = null;
  let cleaned = false;
  const cleanup = () => {
    if (cleaned) return;
    const failures = [];
    const attempt = (step) => { try { step(); } catch (error) { failures.push(error); } };
    attempt(() => { unsubscribe?.(); unsubscribe = null; });
    attempt(() => { unregister?.(); unregister = null; });
    attempt(() => state.dispose());
    attempt(() => render(null, dialogHost));
    if (failures.length) throw new AggregateError(failures, 'message/access dialog cleanup failed');
    cleaned = true;
  };
  try {
    unregister = registerMessageAccessDialogController(controller);
    render(html`<${MessageAccessDialogApp} state=${state} actions=${actions} snapshot=${snapshot.value} confirmDiscard=${confirmDiscard}/>` , dialogHost);
    // Signals referenced by a component trigger Preact updates only when read
    // during render. The root above receives snapshot.value, so subscribe once
    // and rerender it through a signal effect owned by this feature.
    unsubscribe = snapshot.subscribe((value) => {
      render(html`<${MessageAccessDialogApp} state=${state} actions=${actions} snapshot=${value} confirmDiscard=${confirmDiscard}/>` , dialogHost);
    });
    registerCleanup(cleanup);
  } catch (error) {
    try { cleanup(); } catch (cleanupError) {
      throw new AggregateError([error, cleanupError], 'message/access dialog initialization failed');
    }
    throw error;
  }
}
