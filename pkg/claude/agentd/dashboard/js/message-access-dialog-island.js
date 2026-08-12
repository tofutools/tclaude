import { Fragment, h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import { registerMessageAccessDialogController } from './message-access-dialog-controller.js';
import {
  agentCandidates, groupMembers, groupsForPicker, permissionRows,
  permissionScopeSeed, permissionSeed, scopeChips, scopeDimOptions, scopeDimRows,
  scopeSupported, senderOnline, sudoByAgent, sudoSlugRows, unreadableScopeSlugs,
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

function operatorFileName(file, index) {
  return file?.name || `pasted-image-${index + 1}.png`;
}

function OperatorAttachmentList({ files, busy, remove }) {
  return html`<ul class="spawn-attachments-list" id="operator-message-attachments-list">
    ${files.map((file, index) => html`<li key=${`${index}:${operatorFileName(file, index)}:${file?.size || 0}`}>
      <span class="att-name">${operatorFileName(file, index)}</span>
      <span class="att-size">${file?.size || 0} B</span>
      <button type="button" class="att-remove" disabled=${busy}
        aria-label=${`Remove ${operatorFileName(file, index)}`} onClick=${() => remove(index)}>✕</button>
    </li>`)}
  </ul>`;
}

function OperatorMessageDialog({ descriptor, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [subject, setSubject] = useState('');
  const [body, setBody] = useState('');
  const [files, setFiles] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const busyRef = useRef(false);
  const bodyRef = useRef(null);
  const fileInputRef = useRef(null);
  const allLive = !!descriptor.allLive;
  const dirty = !!subject || !!body || files.length > 0;

  const addFiles = (incoming) => {
    if (busyRef.current) return;
    setFiles((current) => [...current, ...Array.from(incoming || []).filter(Boolean)].slice(0, 8));
  };
  const removeFile = (index) => {
    if (busyRef.current) return;
    setFiles((current) => current.filter((_, candidate) => candidate !== index));
  };
  const submit = async () => {
    if (busyRef.current) return;
    const draft = Object.freeze({
      to: descriptor.agent,
      subject,
      body,
      files: Object.freeze([...files]),
      ...(allLive ? { allLive: true } : {}),
    });
    setError('');
    if (!draft.body.trim() && (allLive || !draft.files.length)) {
      setError(allLive ? 'Write an announcement.' : 'Write a message or attach a file.');
      return;
    }
    busyRef.current = true;
    setBusy(true);
    try {
      await actions.sendOperatorMessage(draft);
      state.close();
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  return html`<${Overlay}
    id="operator-message-modal"
    overlayClass="operator-message-overlay"
    dialogClass="cron-create-modal operator-message-modal"
    labelledby="operator-message-title"
    describedby="operator-message-desc"
    onClose=${state.close}
    onSubmitHotkey=${submit}
    dirty=${dirty}
    blocked=${busy}
    confirmDiscard=${confirmDiscard}
    resizeKey="tclaude.dash.modalSize.operator-message"
    initialFocusRef=${bodyRef}
    registerClose=${registerClose}
    guardBackdropDrag=${true}
    onPaste=${(event) => {
      if (allLive) return;
      const pasted = Array.from(event.clipboardData?.files || []);
      if (!pasted.length) return;
      event.preventDefault();
      addFiles(pasted);
    }}
    onDragOver=${(event) => {
      if (allLive) return;
      if (event.dataTransfer?.types?.includes('Files')) event.preventDefault();
    }}
    onDrop=${(event) => {
      if (allLive) return;
      const dropped = Array.from(event.dataTransfer?.files || []);
      if (!dropped.length) return;
      event.preventDefault();
      addFiles(dropped);
    }}>
    <h3 id="operator-message-title"><${Words}
      plain=${allLive ? 'Announce to all live agents' : 'Send message to agent'}
      wizard=${allLive ? '📯 Proclaim to all channeling familiars' : '✒ Send a missive to the familiar'}/></h3>
    <p class="modal-hint" id="operator-message-desc"><${Words}
      plain=${allLive
        ? 'Sent once to every agent that is live when you submit. Offline agents are not queued.'
        : 'Queued through the agent mailbox, so incoming agent output cannot interfere with what you type.'}
      wizard=${allLive
        ? 'Sounded once to every familiar channeling when you proclaim; slumbering familiars will not be stirred.'
        : "Sealed into the familiar's mailbox, beyond the meddling of terminal omens."}/></p>
    <div class="cron-create-row"><span class="cron-create-label"><${Words} plain="To" wizard="Familiar"/></span>
      <div class="cron-create-target"><strong id="operator-message-to" title=${allLive ? 'Roster recalculated when sent' : descriptor.agent}>${descriptor.label}</strong></div></div>
    <label class="cron-create-row"><span class="cron-create-label"><${Words} plain="Subject" wizard="Seal"/></span>
      <input id="operator-message-subject" type="text" maxlength="256" placeholder="optional" autocomplete="off"
        value=${subject} readOnly=${busy} onInput=${(event) => setSubject(event.currentTarget.value)}/></label>
    <label class="cron-create-row operator-message-body-row"><span class="cron-create-label"><${Words} plain="Message" wizard="Missive"/></span>
      <textarea ref=${bodyRef} id="operator-message-body" rows="6" maxlength="16384"
        placeholder="Message text — Ctrl/Cmd+Enter to send" spellcheck=${true} value=${body} readOnly=${busy}
        onInput=${(event) => setBody(event.currentTarget.value)}></textarea></label>
    ${!allLive && html`<div class="cron-create-row"><span class="cron-create-label"><${Words} plain="Attachments" wizard="Enclosures"/></span>
      <div class="cron-create-target spawn-attachments"><div class="spawn-attachments-controls">
        <button type="button" id="operator-message-attach-btn" disabled=${busy}
          onClick=${() => fileInputRef.current?.click()}><${Words} plain="📎 Attach files…" wizard="📎 Bind relics…"/></button>
        <input ref=${fileInputRef} type="file" id="operator-message-attach-input" multiple hidden disabled=${busy}
          onChange=${(event) => { addFiles(event.currentTarget.files); event.currentTarget.value = ''; }}/>
        <span class="spawn-attachments-hint"><${Words} plain="…or drag files here / paste a screenshot" wizard="…or draw relics here / paste a captured vision"/></span>
      </div><${OperatorAttachmentList} files=${files} busy=${busy} remove=${removeFile}/></div></div>`}
    <${ErrorLine} id="operator-message-error" value=${error}/>
    <div class="modal-buttons"><button id="operator-message-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}><${Words} plain="Cancel" wizard="Dispel"/></button>
      <span class="spacer"></span><button id="operator-message-submit" class="primary operator-message-submit" type="button" disabled=${busy} onClick=${submit}>
        <${Words} plain=${busy ? 'Queueing…' : allLive ? 'Announce' : 'Send'} wizard=${busy ? '✒ Sending missive…' : allLive ? '📯 Proclaim' : '✒ Send missive'}/></button></div>
  </${Overlay}>`;
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
  const activeSudo = sudoByAgent(snapshot);
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
          ${descriptor.showSudo && activeSudo.get(agent.agent_id || agent.conv_id)?.length
            ? html`<span class="sudo-badge" title=${`${activeSudo.get(agent.agent_id || agent.conv_id).length} active sudo grant(s)`}>🔓</span>` : null}
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
  const { requestClose, registerClose } = useGuardedOverlayClose();
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
    // A blank From multicasts AS THE OPERATOR (tagged "human operator"); a solo
    // send still needs a sender agent to attribute and reply to.
    const isGroup = to.startsWith('group:');
    if (!from.trim() && !isGroup) {
      setError(wizWord('From is required for a solo message — leave it blank only to multicast to a group as you, the operator.', 'A sending familiar is required for a solo missive — leave it blank only to multicast to a party as yourself.'));
      return;
    }
    busyRef.current = true;
    setBusy(true);
    try {
      const payload = { to, subject: subject.trim(), body };
      if (from.trim()) payload.from = from.trim();
      if (explicit) payload.members = explicit;
      if (isGroup && role.trim()) payload.role = role.trim();
      await actions.sendMessage(payload);
      state.close();
    } catch (cause) { setError(errorText(cause)); }
    finally { busyRef.current = false; setBusy(false); }
  };
  return html`<${Overlay} id="message-create-modal" labelledby="message-create-title"
    onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}>
    <h3 id="message-create-title"><${Words}
      plain=${scopedGroup ? `Send a message to group “${scopedGroup}”` : 'Send a message'}
      wizard=${scopedGroup ? `Send a missive to party “${scopedGroup}”` : 'Send a missive'}/></h3>
    <p id="message-create-desc" class="modal-hint"><${Words}
      plain=${scopedGroup ? `Delivers one immediate message to the members of “${scopedGroup}” ticked below. Leave From blank to send it as you, the operator.` : 'Delivers one immediate message to a single agent, or multicasts it to every member of a group. Leave From blank on a group send to multicast as you, the operator.'}
      wizard=${scopedGroup ? `Delivers one immediate missive to the familiars of “${scopedGroup}” ticked below. Leave the sender blank to send it as yourself.` : 'Delivers one immediate missive to a familiar, or multicasts it to every familiar in a party. Leave the sender blank on a party send to multicast as yourself.'}/></p>
    <label class="cron-create-row"><span class="cron-create-label" title="Leave blank to multicast as the human operator; a solo message needs a sender agent">From</span><div class="cron-create-target"><div class="cron-target-input-row">
      <input id="message-create-from" type="text" value=${from} placeholder="blank = you (the operator) · or a sender: agt_ id / title / conv-id"
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
    <div class="modal-buttons"><button id="message-create-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}><${Words} plain="Cancel" wizard="Dispel"/></button>
      <span class="spacer"></span><button id="message-create-submit" class="primary" type="button" disabled=${busy || (scopedGroup && (!groupExists || !selectedMembers.length))} onClick=${submit}>
        <${Words} plain=${busy ? 'Sending…' : 'Send'} wizard=${busy ? 'Sending…' : '✒ Send missive'}/></button></div>
  </${Overlay}>`;
}

function HumanReplyDialog({ descriptor, state, actions, snapshot, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
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
    onClose=${state.close} dirty=${!!body} blocked=${busy} confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}>
    <h3 id="human-reply-title"><span class="human-reply-title-regular">Reply to agent</span><span class="human-reply-title-wizard">✒ Answer the familiar</span></h3>
    <p id="human-reply-desc" class="modal-hint">Queues your answer in this agent's inbox for delivery when its pane is ready. Delivered as a message from you, the operator.</p>
    <label class="cron-create-row"><span class="cron-create-label">To</span><div class="cron-create-target"><div id="human-reply-to">
      <div class="human-reply-to-name">${label}</div>${context.subject ? html`<div class="human-reply-to-subject">re: ${context.subject}</div>` : null}
    </div><div id="human-reply-status" class=${`human-reply-status ${online ? 'online' : 'offline'}`}>${online
      ? '🟢 Online — your reply will be queued and delivered when its pane is ready.'
      : '⚫ Offline — this agent has no live session, so it can’t receive a reply. Replying is disabled until it’s back online.'}</div></div></label>
    <label class="cron-create-row"><span class="cron-create-label">Reply</span><textarea id="human-reply-body" rows="4" value=${body}
      placeholder="your reply (required) — ⌘/Ctrl+Enter to send" spellcheck="false" onInput=${(event) => setBody(event.currentTarget.value)} onKeyDown=${fieldSubmit(submit)}></textarea></label>
    <${ErrorLine} id="human-reply-error" value=${error}/><div class="modal-buttons">
      <button id="human-reply-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button><span class="spacer"></span>
      <button id="human-reply-submit" class="primary" type="button" disabled=${busy || !online} onClick=${submit}>${busy ? 'Sending…' : 'Send reply'}</button>
    </div>
  </${Overlay}>`;
}

function SudoGrantDialog({ descriptor, state, actions, snapshot, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [agentID, setAgentID] = useState(descriptor.agentID || '');
  const [selected, setSelected] = useState(() => new Set());
  const [duration, setDuration] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const rows = sudoSlugRows(snapshot);
  const dirty = agentID !== (descriptor.agentID || '') || selected.size > 0 || !!duration || !!reason;
  const toggle = (slug, checked) => setSelected((current) => {
    const next = new Set(current); if (checked) next.add(slug); else next.delete(slug); return next;
  });
  const submit = async () => {
    if (busyRef.current) return;
    setError('');
    if (!agentID.trim()) { setError('Agent ID is required.'); return; }
    if (!selected.size) { setError('Pick at least one slug.'); return; }
    busyRef.current = true;
    setBusy(true);
    try { await actions.grantSudo({ agentID: agentID.trim(), slugs: [...selected], duration: duration.trim(), reason: reason.trim() }); state.close(); }
    catch (cause) { setError(errorText(cause)); }
    finally { busyRef.current = false; setBusy(false); }
  };
  return html`<${Overlay} id="sudo-grant-modal" dialogClass="sudo-grant-modal" labelledby="sudo-grant-title"
    onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}>
    <h3 id="sudo-grant-title">Grant sudo</h3><p class="sudo-grant-hint"><${Words}
      plain="Proactively elevate an agent for a bounded window." wizard="Bestow a bounded sudo boon on a familiar."/> Same blocklist + duration cap as the agent-initiated path; granted_by records <code>${'<human-dashboard>:proactive'}</code> on the audit row.</p>
    <label class="sudo-grant-row"><span class="sudo-grant-label">Agent ID</span><input id="sudo-grant-agent-id" type="text" value=${agentID}
      placeholder="agt_…" autocomplete="off" spellcheck="false" onInput=${(event) => setAgentID(event.currentTarget.value)} /></label>
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
      <button id="sudo-grant-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
      <button id="sudo-grant-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>${busy ? 'Granting…' : 'Grant'}</button>
    </div>
  </${Overlay}>`;
}

// ScopeSummary is the row's read-at-a-glance scope: one chip per constrained
// dimension, or an explicit "unscoped" marker. Saying "unscoped" out loud
// matters — a granted slug with no chips would otherwise read as "scope not
// loaded" rather than "this grant applies everywhere".
//
// It rides ON the tristate line rather than under the slug, so a row reads
// left-to-right as "this slug, narrowed to this, granted". The effective
// source that used to sit at the right edge moves into the drawer: the row
// cannot carry chips AND that column at this dialog's width without one of
// them truncating, and the chips are the part that changes what the grant
// actually does.
function ScopeSummary({ scope }) {
  const chips = scopeChips(scope);
  return html`<span class="perm-scope-chips">
    ${chips.length
    // The title carries the full text: a long matcher (a git remote pattern,
    // a long group name) is ellipsized in the row rather than allowed to
    // squeeze the slug column, and hover has to still answer "narrowed to what?".
    ? chips.map((chip) => html`<span key=${chip.dim} class="perm-scope-chip"
      title=${`${chip.dim} = ${chip.values.join(', ')}`}><span class="dim">${chip.dim}=</span>${chip.values.join(', ')}</span>`)
    : html`<span class="perm-scope-chip unscoped" title="This grant applies to every target — open ▸ to narrow it">unscoped</span>`}
  </span>`;
}

// ScopeTwisty is the row's disclosure control. Every row in a dialog that has
// any scopable slug gets the gutter so the list stays aligned; a row with
// nothing to disclose gets an inert placeholder rather than a dead control.
function ScopeTwisty({ scopable, open, toggle, slug }) {
  if (!scopable) return html`<span class="perm-scope-twisty empty" aria-hidden="true"></span>`;
  return html`<button type="button" class="perm-scope-twisty" aria-expanded=${open ? 'true' : 'false'}
    aria-label=${`${open ? 'Hide' : 'Show'} the scope editor for ${slug}`}
    title=${open ? 'Hide the scope editor' : 'Narrow this grant to particular targets'}
    onClick=${toggle}>${open ? '▾' : '▸'}</button>`;
}

// holdFocus keeps the focused element focused through a mousedown on a
// control that acts on it. The + beside a scope dimension's free-text box is
// the case: browsers that focus a button on mousedown would blur the box
// first, commit there, and then never dispatch the click to the (now inert)
// button, while browsers that leave focus alone commit on the click — same
// outcome, but only ever one of the two paths per browser, and the button
// visibly goes inert under the cursor mid-click. Suppressing the focus move
// makes the click the one gesture that commits, everywhere.
const holdFocus = (event) => event.preventDefault();

// ScopeDimEditor edits ONE dimension's matcher list. It is deliberately
// ignorant of which dimension it is: the label, the suggestions and the
// selectors all arrive from the daemon, so a dimension added by a later phase
// is editable here the moment the daemon advertises it.
//
// The free-text field is the ONLY way to narrow a dimension the daemon has no
// catalogue for (remote, process_template, target_agent), so a value typed
// there must not depend on the operator knowing to press Enter. The rule is:
// a typed value commits whenever the box is left (Enter, the + button, blur),
// and onDraft reports whatever is still uncommitted so Save can flush it
// instead of silently discarding it. A box that leaves the screen without
// committing withdraws its draft (the unmount cleanup below), so what Save
// writes is always what the dialog was showing.
function ScopeDimEditor({ dim, values, options, onChange, declared = true, onDraft }) {
  const [draft, setDraft] = useState('');
  const remove = (value) => onChange(values.filter((item) => item !== value));
  const add = (value) => {
    const clean = String(value || '').trim();
    if (!clean || values.includes(clean)) return;
    onChange([...values, clean].sort());
  };
  // onDraft is a fresh closure every render, so the unmount cleanup reads it
  // through a ref rather than capturing the one from the first render.
  const onDraftRef = useRef(onDraft);
  onDraftRef.current = onDraft;
  const editDraft = (value) => {
    setDraft(value);
    onDraft?.(dim, value);
  };
  const commitDraft = () => {
    if (!draft.trim()) return;
    add(draft);
    editDraft('');
  };
  // Every way this editor can vanish other than the twisty — the slug moved
  // off Grant, a stale dimension emptied out of scopeDimRows, the drawer
  // switching slugs — takes the box off screen with its draft still armed.
  // Withdraw it here, at the one point common to all of them, so Save cannot
  // write a matcher the operator abandoned and can no longer see.
  useEffect(() => () => onDraftRef.current?.(dim, ''), [dim]);
  const unused = [...options.values, ...options.selectors].filter((value) => !values.includes(value));
  return html`<div class="perm-scope-dim" data-dim=${dim}>
    <span class="perm-scope-dim-label"><code>${dim}</code>${declared ? null : html` <span class="perm-scope-stale"
      title="This slug no longer accepts this dimension. Saving is refused until it is emptied.">⚠ not accepted</span>`}</span>
    <div class="perm-scope-dim-body">
      <span class="perm-scope-chips">
        ${values.map((value) => html`<span key=${value} class="perm-scope-chip">${value}
          <button type="button" class="x" aria-label=${`Remove ${dim}=${value}`} onClick=${() => remove(value)}>×</button></span>`)}
        ${unused.length ? html`<select class="perm-scope-add" aria-label=${`Add a ${dim} value`}
          onChange=${(event) => add(event.currentTarget.value)}>
          <option value="" selected>+ add…</option>
          ${unused.map((value) => html`<option key=${value} value=${value}>${value}</option>`)}
        </select>` : null}
        <input type="text" class="perm-scope-free" value=${draft}
          placeholder=${unused.length ? 'or type a value' : 'type a value'}
          autocomplete="off" spellcheck=${false} aria-label=${`Type a ${dim} value`}
          onInput=${(event) => editDraft(event.currentTarget.value)}
          onBlur=${commitDraft}
          onKeyDown=${(event) => {
    if (event.key !== 'Enter' || event.isComposing) return;
    event.preventDefault();
    commitDraft();
  }} />
        <button type="button" class="perm-scope-free-add" aria-disabled=${draft.trim() ? 'false' : 'true'}
          aria-label=${`Add the typed ${dim} value`} title="Add the typed value"
          onMouseDown=${holdFocus} onClick=${commitDraft}>+</button>
      </span>
      ${values.length ? null : html`<div class="perm-scope-hint">empty — unconstrained on this dimension</div>`}
    </div>
  </div>`;
}

function ScopeDrawer({ row, scope, snapshot, onChange, effective, onDraft }) {
  const dims = scopeDimRows(row, scope);
  const setDim = (dim, values) => {
    const next = { ...scope };
    // An empty dimension is an ABSENT one, never a stored empty list: the
    // scope writer rejects a dimension with no matchers, and "unconstrained"
    // is exactly what absence means at the gate.
    if (values.length) next[dim] = values; else delete next[dim];
    onChange(next);
  };
  return html`<div class="perm-scope-drawer" data-slug=${row.slug}>
    <div class="perm-scope-drawer-head">
      <span class="perm-scope-drawer-hint">Dimensions <b>AND</b> together; values within one dimension <b>OR</b>.
        A dimension left empty is unconstrained, and a grant with no dimensions at all applies everywhere.</span>
      ${effective ? html`<span class=${`perm-row-eff ${effective.granted ? 'granted' : 'denied'}`}>${effective.text}</span>` : null}
    </div>
    ${dims.map(({ dim, declared }) => html`<${ScopeDimEditor} key=${dim} dim=${dim} declared=${declared}
      values=${scope[dim] || []} options=${scopeDimOptions(snapshot, dim)}
      onDraft=${onDraft} onChange=${(values) => setDim(dim, values)} />`)}
  </div>`;
}

function PermissionsDialog({ descriptor, state, actions, snapshot, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  // Seed and dirty comparison tuple are frozen for this keyed launch. Snapshot
  // updates may change rows/effective sources, never the draft baseline.
  const [baseline] = useState(() => permissionSeed(snapshot, descriptor));
  const [selection, setSelection] = useState(() => ({ ...baseline }));
  const [scopeBaseline] = useState(() => permissionScopeSeed(snapshot, descriptor));
  const [unreadable] = useState(() => unreadableScopeSlugs(snapshot, descriptor));
  const [scopes, setScopes] = useState(() => ({ ...scopeBaseline }));
  const [openScope, setOpenScope] = useState('');
  // Bumped when Save folds an uncommitted free-text draft into the scopes, to
  // remount the drawer's boxes empty (see submit).
  const [draftEpoch, setDraftEpoch] = useState(0);
  const [filter, setFilter] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const rows = permissionRows(snapshot, descriptor, selection);
  const visible = rows.filter((row) => !filter.trim() || [row.slug, row.description, row.descr]
    .some((value) => String(value || '').toLowerCase().includes(filter.trim().toLowerCase())));
  const currentEffect = (slug) => selection[slug] || 'default';
  const baselineEffect = (slug) => baseline[slug] || 'default';
  const scopeOf = (slug) => scopes[slug] || {};
  const scopeJSON = (scope) => JSON.stringify(scopeChips(scope));
  const rowsDirty = rows.some((row) => currentEffect(row.slug) !== baselineEffect(row.slug));
  const scopesDirty = rows.some((row) => scopeJSON(scopeOf(row.slug)) !== scopeJSON(scopeBaseline[row.slug] || {}));
  const groupMode = descriptor.mode === 'group';
  const grantOnly = descriptor.mode === 'buffer' && !!descriptor.grantOnly;
  const roleMode = grantOnly && descriptor.subject === 'role';
  const scopesEditable = scopeSupported(descriptor);
  // The twisty gutter is reserved for the whole visible list as soon as any
  // slug in it can carry a scope, so rows stay left-aligned instead of
  // jittering sideways as grants are toggled on and off.
  const anyScopable = scopesEditable && visible.some((row) => !!row.scope_dims?.length);
  // Owner scopes (TCL-1071) are a small JSON document, not a per-slug
  // tri-state, so they get a textarea rather than a row control — deliberately
  // the least machinery that makes the field reachable from the dashboard.
  // An empty box means "no narrowing"; the daemon validates the map and
  // rejects the whole PATCH with a readable message if it is wrong.
  const [ownerScopesText, setOwnerScopesText] = useState(
    () => (Object.keys(descriptor.ownerScopes || {}).length
      ? JSON.stringify(descriptor.ownerScopes, null, 2) : ''));
  const [ownerScopesBaseline] = useState(() => ownerScopesText);
  const ownerScopesDirty = groupMode && ownerScopesText !== ownerScopesBaseline;
  const dirty = rowsDirty || scopesDirty || ownerScopesDirty;
  const setEffect = (slug, effect) => setSelection((current) => ({ ...current, [slug]: effect }));
  const setScope = (slug, scope) => setScopes((current) => {
    const next = { ...current };
    if (Object.keys(scope).length) next[slug] = scope; else delete next[slug];
    return next;
  });
  // A matcher still sitting in a dimension's free-text box when Save is
  // pressed. The box commits on Enter, on + and on blur, but Save must not
  // depend on any of those having happened: a dimension with no catalogue
  // (remote) is typed into and nothing else, and silently dropping the typed
  // value writes the grant back UNSCOPED — the widest possible reading of what
  // the operator just narrowed. Only one drawer is open at a time, so this is
  // one slug's dimensions.
  const pendingScopeRef = useRef({ slug: '', dims: {} });
  const noteScopeDraft = (slug, dim, value) => {
    const pending = pendingScopeRef.current;
    if (pending.slug !== slug) {
      // A withdrawal arriving from an editor that has already been replaced
      // (its unmount cleanup runs after the new drawer has mounted) must not
      // adopt the old slug and wipe what is on screen now.
      if (!String(value || '').trim()) return;
      pendingScopeRef.current = { slug, dims: {} };
    }
    pendingScopeRef.current.dims[dim] = value;
  };
  const flushScopeDrafts = (current) => {
    const { slug, dims } = pendingScopeRef.current;
    const typed = Object.entries(dims).filter(([, value]) => String(value || '').trim());
    if (!slug || !typed.length) return current;
    const merged = { ...(current[slug] || {}) };
    for (const [dim, value] of typed) {
      const clean = String(value).trim();
      const values = merged[dim] || [];
      if (!values.includes(clean)) merged[dim] = [...values, clean].sort();
    }
    pendingScopeRef.current = { slug, dims: {} };
    return { ...current, [slug]: merged };
  };
  const submit = async () => {
    if (busyRef.current) return;
    const unreadableGroupGrants = groupMode
      ? [...unreadable].filter((slug) => currentEffect(slug) === 'grant') : [];
    if (unreadableGroupGrants.length) {
      setError(`Cannot save while these group grants have unreadable scopes: ${unreadableGroupGrants.join(', ')}. Remove them first or edit them with a newer tclaude build.`);
      return;
    }
    let ownerScopes = null;
    if (groupMode) {
      const raw = ownerScopesText.trim();
      if (raw === '') ownerScopes = {};
      else {
        try { ownerScopes = JSON.parse(raw); }
        catch (cause) { setError(`Owner scopes: not valid JSON — ${cause.message}`); return; }
        if (!ownerScopes || typeof ownerScopes !== 'object' || Array.isArray(ownerScopes)) {
          setError('Owner scopes: expected a JSON object mapping a permission slug to a scope.');
          return;
        }
      }
    }
    busyRef.current = true;
    setBusy(true); setError('');
    // Fold an uncommitted free-text matcher in before anything reads the
    // scopes, and put it back into state too: a failed save leaves the dialog
    // open, and the chip has to be there rather than the text the operator
    // typed having vanished.
    const effectiveScopes = flushScopeDrafts(scopes);
    if (effectiveScopes !== scopes) {
      setScopes(effectiveScopes);
      // Remount the drawer so the boxes come back empty. Without this a
      // rejected save leaves the value on screen twice — once as the chip the
      // flush just made, once as the text still sitting in the box — and
      // correcting the leftover text would add a second matcher rather than
      // replace the first.
      setDraftEpoch((epoch) => epoch + 1);
    }
    const scopeAt = (slug) => effectiveScopes[slug] || {};
    // Buffered blueprints can outlive the registry version that authored
    // them. Keep unknown legacy slugs in the emitted draft even though there
    // is no registry metadata with which to render an editable row. Live
    // agent/group saves remain registry-bounded batches.
    const full = descriptor.mode === 'buffer'
      ? { ...selection, ...Object.fromEntries(rows.map((row) => [row.slug, currentEffect(row.slug)])) }
      : Object.fromEntries(rows.map((row) => [row.slug, currentEffect(row.slug)]));
    // Only a granted slug carries a scope: a deny is unconditional by design,
    // and a slug back at Default has no row to attach one to. Sending a scope
    // for either is a 400 from the daemon, so drop them here where the user's
    // intent ("I set this one back to Default") is still legible.
    //
    // Every granted, scopable slug is sent EXPLICITLY, including as {}: the
    // daemon reads a missing key as "keep the stored scope" (so an unrelated
    // save cannot strip a narrowing), which means clearing the last chip has
    // to be said out loud to take effect.
    const scoped = scopesEditable ? Object.fromEntries(rows
      .filter((row) => currentEffect(row.slug) === 'grant'
        && (!!row.scope_dims?.length || Object.keys(scopeAt(row.slug)).length)
        && !unreadable.has(row.slug))
      .map((row) => [row.slug, scopeAt(row.slug)])) : {};
    // As above, an unknown buffered slug is not editable, but its canonical
    // scope must round-trip exactly. Otherwise opening and saving a role made
    // by a newer build would silently widen that grant to unscoped.
    if (descriptor.mode === 'buffer') {
      const known = new Set(rows.map((row) => row.slug));
      for (const [slug, scope] of Object.entries(effectiveScopes)) {
        if (!known.has(slug) && full[slug] === 'grant') scoped[slug] = scope;
      }
    }
    // Only send the map when the box was actually EDITED. A save that merely
    // flipped a grant must not carry owner_scopes at all: the daemon treats an
    // absent field as "unchanged", and sending the box's current value would
    // clear a stored narrowing this build could not decode into it.
    const ownerScopePayload = groupMode && ownerScopesDirty ? ownerScopes : null;
    try { await actions.savePermissions(descriptor, full, scoped, ownerScopePayload); state.close(); }
    catch (cause) { setError(errorText(cause)); }
    finally { busyRef.current = false; setBusy(false); }
  };
  const shortConv = String(descriptor.conv || '').slice(0, 8);
  const subtitle = groupMode ? `Group: ${descriptor.group} · every current member receives these grants immediately`
    : descriptor.mode === 'agent' ? `Agent: ${descriptor.label || shortConv} · ${shortConv}`
    : roleMode ? `Role${descriptor.label ? `: ${descriptor.label}` : ''} · granted when the role is assigned`
    : `New agent${descriptor.label ? ` “${descriptor.label}”` : ''}${descriptor.group ? ` → ${descriptor.group}` : ''} · fully composed from defaults, group, roles, ownership, and overrides`;
  const wizardSubtitle = groupMode ? `Party: ${descriptor.group} · every familiar receives these boons immediately`
    : descriptor.mode === 'agent' ? `Familiar: ${descriptor.label || shortConv} · ${shortConv}`
    : roleMode ? `Class${descriptor.label ? `: ${descriptor.label}` : ''} · bestowed when the class is assigned`
    : `New familiar${descriptor.label ? ` “${descriptor.label}”` : ''}${descriptor.group ? ` → ${descriptor.group}` : ''} · bestowed when summoned`;
  return html`<${Overlay} id="perm-edit-modal" dialogClass="perm-edit-modal" labelledby="perm-edit-title"
    onClose=${state.close} dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}>
    <h3 id="perm-edit-title"><span class="perm-edit-title-regular">${groupMode ? 'Edit group permissions' : roleMode ? 'Edit role permissions' : 'Edit permanent permissions'}</span>
      <span class="perm-edit-title-wizard">${groupMode ? '✨ Party Boons' : roleMode ? '📕 Class Boons' : '📕 The Grimoire'}</span></h3>
    <div class="perm-edit-banner" id="perm-edit-banner">${groupMode
      ? html`<${Words} plain=${html`<strong>GROUP GRANTS</strong> — selected permissions apply immediately to every current member. An agent-level <strong>Deny</strong> still wins.`}
          wizard=${html`<strong>PARTY BOONS</strong> — bestow capabilities on every familiar in this party. A personal binding against one still wins.`}/>`
      : roleMode ? html`<${Words} plain=${html`<strong>ROLE GRANTS</strong> — granted permissions are applied whenever this role is assigned. Narrow a grant with its scope controls, or leave it unscoped to apply everywhere.`}
          wizard=${html`<strong>CLASS BOONS</strong> — these powers accompany every familiar assigned this class. Bind a boon with its scope controls, or leave it unbound to apply everywhere.`}/>`
      : html`<${Words} plain=${html`<strong>PERMANENT</strong> — these per-agent overrides persist until changed. <strong>Grant</strong> adds a slug, <strong>Deny</strong> blocks inherited sources, and <strong>Default</strong> inherits them.`}
          wizard=${html`<strong>THE GRIMOIRE</strong> — these bindings follow this familiar until changed. <strong>Grant</strong> bestows a slug, <strong>Deny</strong> seals inherited boons away, and <strong>Default</strong> inherits them.`}/>`}</div>
    <p class="perm-edit-subtitle" id="perm-edit-subtitle"><${Words} plain=${subtitle} wizard=${wizardSubtitle}/></p>
    ${rows.some((row) => row.ownedGroups?.length) && html`<div class="perm-edit-owner-note" id="perm-edit-owner-note">👑 Owner-implied permissions are shown with their owned-group source; an explicit Deny remains the final veto.</div>`}
    <div class="perm-edit-toolbar"><input id="perm-edit-filter" type="text" value=${filter} placeholder="Filter slugs…" autocomplete="off" spellcheck="false"
      onInput=${(event) => setFilter(event.currentTarget.value)} /><button type="button" id="perm-edit-reset" title="Set every slug back to Default (inherit)"
      onClick=${() => setSelection(Object.fromEntries(rows.map((row) => [row.slug, 'default'])))}><span class="pe-btn-regular">${groupMode || grantOnly ? 'none granted' : 'all default'}</span><span class="pe-btn-wizard">unbind all</span></button></div>
    <div id="perm-edit-list" class="perm-edit-list">${visible.length ? visible.map((row) => {
    // The scope controls appear only where they can mean something: a slug
    // that declares dimensions, granted, in an editor whose save path can
    // persist a scope. Everywhere else the row renders exactly as before.
    // A grant whose stored scope the daemon could not decode is shown as
    // exactly that and left alone: it authorizes nothing today, and the one
    // thing the editor must not do is quietly rewrite it into a blanket grant.
    const unreadableScope = unreadable.has(row.slug) && currentEffect(row.slug) === 'grant';
    const scopable = !unreadableScope && scopesEditable && !!row.scope_dims?.length
      && currentEffect(row.slug) === 'grant';
    const effText = groupMode
      ? html`<${Words} plain=${row.granted ? '✓ via group' : '— not via group'} wizard=${row.granted ? '✨ boon active' : '— no boon'}/>`
      : currentEffect(row.slug) === 'deny' ? '✗ denied (explicit veto)'
        : row.granted ? `✓ ${row.sources.join(' + ')}` : '✗ denied (no source)';
    return html`<${Fragment} key=${row.slug}><div class="perm-row" data-slug=${row.slug}>
      ${anyScopable && html`<${ScopeTwisty} scopable=${scopable} slug=${row.slug} open=${openScope === row.slug}
        toggle=${() => setOpenScope(openScope === row.slug ? '' : row.slug)} />`}
      <div class="perm-row-info"><span class="perm-row-slug">${row.slug}${row.owner_implied ? html` <span class="owner-badge" title="Group ownership can confer this slug">👑 owner</span>` : null}</span>
        <span class="perm-row-desc" title=${row.description || row.descr || ''}>${row.description || row.descr || ''}</span></div>
      ${scopable && html`<${ScopeSummary} scope=${scopeOf(row.slug)} />`}
      ${unreadableScope && html`<span class="perm-scope-chips"><span class="perm-scope-chip unreadable"
        title="This grant's stored scope cannot be read by this build, so it authorizes nothing at the gate. Saving will not overwrite it — edit it with the tclaude agent permissions CLI, or set the slug back to Default first.">unreadable scope</span></span>`}
      <div class="perm-tristate"><button type="button" data-effect="default" class=${currentEffect(row.slug) === 'default' ? 'active' : ''} onClick=${() => setEffect(row.slug, 'default')}>${groupMode ? html`<${Words} plain="Not granted" wizard="Unbound"/>` : 'Default'}</button>
        <button type="button" data-effect="grant" class=${currentEffect(row.slug) === 'grant' ? 'active' : ''} onClick=${() => setEffect(row.slug, 'grant')}>${groupMode ? html`<${Words} plain="Grant" wizard="Bestow"/>` : 'Grant'}</button>
        ${!groupMode && !grantOnly && html`<button type="button" data-effect="deny" class=${currentEffect(row.slug) === 'deny' ? 'active' : ''} onClick=${() => setEffect(row.slug, 'deny')}>Deny</button>`}</div>
      ${scopable
    // The column is kept but emptied rather than removed: dropping it would
    // let a scopable row's tristate buttons sit further right than its
    // neighbours', and a column of buttons that steps sideways row by row
    // reads as a rendering bug. The text itself lives in the drawer.
    ? html`<span class="perm-row-eff empty" aria-hidden="true"></span>`
    : html`<span class=${`perm-row-eff ${row.granted ? 'granted' : 'denied'}`}>${effText}</span>`}
    </div>
    ${scopable && openScope === row.slug && html`<${ScopeDrawer} key=${`${row.slug}:${draftEpoch}`}
      row=${row} scope=${scopeOf(row.slug)}
      snapshot=${snapshot} onChange=${(scope) => setScope(row.slug, scope)}
      onDraft=${(dim, value) => noteScopeDraft(row.slug, dim, value)}
      effective=${{ granted: row.granted, text: effText }} />`}
    </${Fragment}>`;
  }) : html`<div class="empty" style="padding:10px">${rows.length ? 'No matching permission slugs.' : 'No permission slugs registered.'}</div>`}</div>
    ${groupMode && html`<div class="perm-edit-owner-scopes" id="perm-edit-owner-scopes">
      <label for="perm-edit-owner-scopes-input"><${Words}
        plain=${html`👑 <strong>Owner-grant constraints</strong> — adds constraints to the automatic permission grants contributed by owning this group, e.g. <code>{"groups.members.spawn": {"spawn_profile": ["reviewer"]}}</code>. Empty = no extra constraints. Other grants are unaffected.`}
        wizard=${html`👑 <strong>Bind the crown</strong> — adds constraints to the boons granted by wearing this party's crown, e.g. <code>{"groups.members.spawn": {"spawn_profile": ["reviewer"]}}</code>. Empty = unbound. Other boons are untouched.`}/></label>
      <textarea id="perm-edit-owner-scopes-input" rows="4" spellcheck="false" autocomplete="off"
        placeholder=${'{\n  "groups.members.spawn": { "spawn_profile": ["reviewer"] }\n}'}
        value=${ownerScopesText} onInput=${(event) => setOwnerScopesText(event.currentTarget.value)}></textarea>
    </div>`}
    <${ErrorLine} id="perm-edit-error" className="sudo-grant-error" value=${error}/><div class="modal-buttons">
      <button id="perm-edit-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}><span class="pe-btn-regular">Cancel</span><span class="pe-btn-wizard">Dispel</span></button>
      <button id="perm-edit-submit" class="primary" type="button" disabled=${busy} onClick=${submit}>${busy ? 'Saving…' : 'Save'}</button>
    </div>
  </${Overlay}>`;
}

// ContextFeaturesDialog edits which parts of Claude Code's startup context an
// agent loads (TCL-597) — the "Context…" twin of PermissionsDialog.
//
// It is a pure buffer editor: the draft belongs to the spawn form or the profile
// editor, and Save hands the map back through descriptor.onSave rather than
// writing anything itself. The three states mirror the permission editor's
// tri-state so the two dialogs read the same way — Default (leave the harness
// alone), Keep, Trim — and only the non-default entries are handed back, keeping
// a saved profile's map sparse.
function ContextFeaturesDialog({ descriptor, state, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const catalog = descriptor.catalog || [];
  const [baseline] = useState(() => ({ ...(descriptor.selection || {}) }));
  const [selection, setSelection] = useState(() => ({ ...baseline }));
  const [filter, setFilter] = useState('');
  const needle = filter.trim().toLowerCase();
  const visible = catalog.filter((row) => !needle
    || [row.slug, row.label, row.descr].some((value) => String(value || '').toLowerCase().includes(needle)));
  const currentState = (slug) => selection[slug] || 'default';
  const dirty = catalog.some((row) => currentState(row.slug) !== (baseline[row.slug] || 'default'));
  const trimmed = catalog.filter((row) => currentState(row.slug) === 'off').length;
  const kept = catalog.filter((row) => currentState(row.slug) === 'on').length;
  const setState = (slug, next) => setSelection((current) => {
    const draft = { ...current };
    // Keep the handed-back map sparse: a row set back to Default is an ABSENT
    // entry, not a stored "default", so a profile only ever persists real intent.
    if (next === 'default') delete draft[slug];
    else draft[slug] = next;
    return draft;
  });
  const submit = () => {
    descriptor.onSave?.({ ...selection });
    state.close();
  };
  return html`<${Overlay} id="context-features-modal" dialogClass="perm-edit-modal context-features-modal"
    labelledby="context-features-title" onClose=${state.close} dirty=${dirty} confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}>
    <h3 id="context-features-title"><span class="perm-edit-title-regular">Startup context</span>
      <span class="perm-edit-title-wizard">🧹 Trim the Tome</span></h3>
    <div class="perm-edit-banner" id="context-features-banner"><${Words}
      plain=${html`<strong>FOCUS</strong> — every capability the harness advertises is context the agent reads past before it reaches your brief. <strong>Trim</strong> what this agent will not use; <strong>Keep</strong> overrides a profile that trimmed it; <strong>Default</strong> leaves the harness alone.`}
      wizard=${html`<strong>FOCUS</strong> — every spell in the tome is a page the familiar must turn before it finds your task. <strong>Tear out</strong> what it will not cast; <strong>Keep</strong> restores a page a profile removed; <strong>Default</strong> leaves the tome untouched.`}/></div>
    <p class="perm-edit-subtitle" id="context-features-subtitle">${
      descriptor.label ? `New agent “${descriptor.label}”` : 'New agent'} · applied at launch${
      trimmed || kept ? ` · ${[trimmed ? `${trimmed} trimmed` : '', kept ? `${kept} kept` : ''].filter(Boolean).join(' · ')}` : ''}</p>
    <div class="perm-edit-toolbar">
      <input id="context-features-filter" type="text" value=${filter} placeholder="Filter features…"
        autocomplete="off" spellcheck="false" onInput=${(event) => setFilter(event.currentTarget.value)} />
      <button type="button" id="context-features-lean" title="Trim every feature flagged as a large startup-context win, and leave the rest alone"
        onClick=${() => setSelection((current) => {
    const draft = { ...current };
    for (const row of catalog) if (row.heavy) draft[row.slug] = 'off';
    return draft;
  })}>lean</button>
      <button type="button" id="context-features-reset" title="Set every feature back to Default (leave the harness alone)"
        onClick=${() => setSelection({})}>all default</button>
    </div>
    <div id="context-features-list" class="perm-edit-list">${visible.length ? visible.map((row) => html`
      <div class="perm-row" key=${row.slug} data-slug=${row.slug}>
        <div class="perm-row-info">
          <span class="perm-row-slug">${row.label || row.slug}${row.heavy
    ? html` <span class="owner-badge" title="One of the largest startup-context wins">★ heavy</span>` : null}</span>
          <span class="perm-row-desc" title=${row.descr || ''}>${row.descr || ''}</span>
          ${row.caution ? html`<span class="perm-row-desc context-features-caution" title=${row.caution}>⚠ ${row.caution}</span>` : null}
        </div>
        <div class="perm-tristate">
          <button type="button" data-state="default" class=${currentState(row.slug) === 'default' ? 'active' : ''}
            onClick=${() => setState(row.slug, 'default')}>Default</button>
          <button type="button" data-state="on" class=${currentState(row.slug) === 'on' ? 'active' : ''}
            onClick=${() => setState(row.slug, 'on')}>Keep</button>
          <button type="button" data-state="off" class=${currentState(row.slug) === 'off' ? 'active' : ''}
            onClick=${() => setState(row.slug, 'off')}>Trim</button>
        </div>
        <span class=${`perm-row-eff ${currentState(row.slug) === 'off' ? 'denied' : 'granted'}`}>${
  currentState(row.slug) === 'off' ? '✂ trimmed'
    : currentState(row.slug) === 'on' ? '✓ kept'
      : '— harness default'}</span>
      </div>`) : html`<div class="empty" style="padding:10px">${catalog.length
    ? 'No matching features.'
    : 'This harness has no steerable startup-context features.'}</div>`}</div>
    <div class="modal-buttons">
      <button id="context-features-cancel" type="button" onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="context-features-submit" class="primary" type="button" onClick=${submit}>Save</button>
    </div>
  </${Overlay}>`;
}

export function MessageAccessDialogApp({ state, actions, snapshot, confirmDiscard }) {
  const current = state.view.value;
  const descriptor = current.dialog;
  let parent = null;
  if (descriptor?.kind === 'operator-message') parent = html`<${OperatorMessageDialog} key=${`operator:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'message') parent = html`<${MessageDialog} key=${`message:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'human-reply') parent = html`<${HumanReplyDialog} key=${`reply:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'sudo-grant') parent = html`<${SudoGrantDialog} key=${`sudo:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'permissions') parent = html`<${PermissionsDialog} key=${`permissions:${descriptor.launchID}`} descriptor=${descriptor} state=${state} actions=${actions} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`;
  else if (descriptor?.kind === 'context-features') parent = html`<${ContextFeaturesDialog} key=${`context-features:${descriptor.launchID}`} descriptor=${descriptor} state=${state} confirmDiscard=${confirmDiscard}/>`;
  return html`<${Fragment}>${parent}${current.picker && html`<${AgentPicker} key=${`picker:${current.picker.launchID}`}
    descriptor=${current.picker} state=${state} snapshot=${snapshot} confirmDiscard=${confirmDiscard}/>`}</${Fragment}>`;
}

export function mountMessageAccessDialogIsland({
  dialogHost, state, actions, snapshot, confirmDiscard, registerCleanup,
}) {
  const controller = {
    openMessage: state.openMessage,
    openOperatorMessage: state.openOperatorMessage,
    dialogKind: state.dialogKind,
    openHumanReply: state.openHumanReply,
    openSudoGrant: state.openSudoGrant,
    openAgentPermissions: state.openAgentPermissions,
    openGroupPermissions: state.openGroupPermissions,
    openBufferedPermissions: state.openBufferedPermissions,
    openContextFeatures: state.openContextFeatures,
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
