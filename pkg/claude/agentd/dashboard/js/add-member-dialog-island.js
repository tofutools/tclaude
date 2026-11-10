import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { shortId } from './helpers.js';
import { Dialog } from './ui-dialog.js';
import { isPageNode, nodeSnapshot } from './node-api.js';
import { FilterCombobox } from './filter-combobox.js';
import { PICKER_CAP, agentPickerItem } from './agent-picker.js';

const html = htm.bind(h);

function memberMeta(snapshot, convID) {
  for (const group of snapshot?.groups || []) {
    for (const member of group.members || []) {
      if (member.conv_id === convID) {
        return { role: member.role || '', descr: member.descr || '' };
      }
    }
  }
  return { role: '', descr: '' };
}

export function buildAddMemberCandidates({
  snapshot,
  promotionPool = [],
  group,
  includeOffline = false,
  query = '',
}) {
  const existing = new Set(
    ((snapshot?.groups || []).find((candidate) => candidate.name === group)?.members || [])
      .map((member) => member.conv_id),
  );
  const seen = new Set();
  const candidates = [];
  const push = (candidate, promote = false) => {
    const conv = candidate?.conv_id;
    if (!conv || seen.has(conv) || existing.has(conv)) return;
    if (!includeOffline && !candidate.online) return;
    seen.add(conv);
    const meta = memberMeta(snapshot, conv);
    candidates.push({ ...candidate, _promote: promote, _role: meta.role, _descr: meta.descr });
  };
  for (const candidate of snapshot?.ungrouped || []) push(candidate);
  for (const candidate of snapshot?.agents || []) push(candidate);
  for (const candidate of promotionPool) push(candidate, true);
  candidates.sort((left, right) => {
    if (!!left.online !== !!right.online) return left.online ? -1 : 1;
    return (left.title || '').localeCompare(right.title || '');
  });
  const needle = String(query || '').toLowerCase();
  if (!needle) return candidates;
  return candidates.filter((candidate) =>
    (candidate.title || '').toLowerCase().includes(needle) ||
    (candidate.conv_id || '').toLowerCase().includes(needle) ||
    candidate._role.toLowerCase().includes(needle) ||
    candidate._descr.toLowerCase().includes(needle) ||
    (candidate.groups || []).some((name) => name.toLowerCase().includes(needle)));
}

// addMemberItem is a candidate as the shared agent picker's row: its
// conversation is what joins, and its role and description in another group
// and whether adding promotes it to an agent trail the row.
export function addMemberItem(candidate) {
  // A promotion-pool conversation that says nothing of presence is offline.
  const item = agentPickerItem({ ...candidate, online: !!candidate.online }, { identity: 'conv' });
  return {
    ...item,
    hint: shortId(candidate.conv_id),
    keywords: `${item.keywords} ${candidate._role || ''} ${candidate._descr || ''}`,
    tail: html`${candidate._role ? html`<span class="role">${candidate._role}</span>` : null}${candidate._descr ? html`<span class="descr">${candidate._descr}</span>` : null}${item.tail}${candidate._promote
      ? html`<span class="groups-tag promote-tag" title="Not an agent / hero yet — adding it here promotes it"><span class="theme-copy-regular">promotes to agent</span><span class="theme-copy-wizard">awakens as hero</span></span>` : null}`,
  };
}

export function AddMemberDialog({ descriptor, state, actions, confirmDiscard }) {
  // The descriptor key and the picker's own query and highlight survive
  // snapshot polling: the shared picker (agent-picker.js, on FilterCombobox)
  // highlights a row by its conversation, so a poll that reorders the rows
  // cannot retarget Enter, and one that removes the row leaves none.
  const [query, setQuery] = useState('');
  const [includeOffline, setIncludeOffline] = useState(false);
  const [promotionPool, setPromotionPool] = useState([]);
  const [poolLoading, setPoolLoading] = useState(true);
  const [poolError, setPoolError] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [addError, setAddError] = useState('');
  const [failedCandidate, setFailedCandidate] = useState(null);
  const poolRequest = useRef(0);
  const poolBusyRef = useRef(false);
  // Another node's group lists that node's agents, never the page's.
  const currentSnapshot = isPageNode(descriptor.node) ? state.snapshot.value : nodeSnapshot(descriptor.node);
  // Who was added to another node's group here, until its next snapshot
  // shows them (this dialog does not re-render on that node's polls).
  const [added, setAdded] = useState(() => new Set());
  const candidates = useMemo(() => buildAddMemberCandidates({
    snapshot: currentSnapshot,
    promotionPool,
    group: descriptor.group,
    includeOffline,
  }).filter((candidate) => !added.has(candidate.conv_id)), [currentSnapshot, promotionPool, descriptor.group, includeOffline, added]);
  const items = useMemo(() => candidates.map(addMemberItem), [candidates]);
  const dirty = query !== '' || includeOffline;

  const loadPool = async () => {
    if (poolBusyRef.current) return;
    poolBusyRef.current = true;
    const request = ++poolRequest.current;
    setPoolLoading(true);
    setPoolError('');
    try {
      const rows = await actions.loadAddMemberPromotionPool(descriptor);
      if (request === poolRequest.current) setPromotionPool(rows);
    } catch (error) {
      if (request === poolRequest.current) {
        setPoolError((error && error.message) || String(error));
      }
    } finally {
      if (request === poolRequest.current) setPoolLoading(false);
      poolBusyRef.current = false;
    }
  };

  useEffect(() => {
    void loadPool();
    return () => { poolRequest.current++; };
  }, []);

  const addOne = async (candidate) => {
    if (!candidate || busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setAddError('');
    setFailedCandidate(null);
    try {
      await actions.addExistingMember(descriptor, candidate);
      if (!isPageNode(descriptor.node)) setAdded((current) => new Set(current).add(candidate.conv_id));
    } catch (error) {
      setAddError((error && error.message) || String(error));
      setFailedCandidate(candidate);
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const emptyRetry = includeOffline
    ? '(Try a different filter.)'
    : '(Try ticking "Include offline / archived" for a wider pool.)';
  const wizardEmptyRetry = includeOffline
    ? '(Try a different filter.)'
    : '(Try ticking "Include slumbering / archived" for a wider pool.)';
  const emptyHint = !items.length && poolLoading
    ? html`<span role="status">Loading conversations…</span>`
    : html`<span class="theme-copy-regular">No matching conversations. ${emptyRetry}</span><span class="theme-copy-wizard">No matching conversations. ${wizardEmptyRetry}</span>`;
  return html`<${Dialog}
    id="add-member-modal" dialogClass="add-member-modal" labelledby="add-member-title"
    onClose=${state.closeAddMember} dirty=${dirty} blocked=${busy}
    confirmDiscard=${confirmDiscard} guardBackdropDrag=${true}
  >
    <div class="agent-picker">
      <h3 id="add-member-title"><span class="theme-copy-regular">Add member to</span><span class="theme-copy-wizard">Invite hero into party</span> <span class="muted" id="add-member-group">${descriptor.group}</span></h3>
      <${FilterCombobox} id="add-member-list" inputId="add-member-search" items=${items} value="" multi=${false}
        alwaysOpen autoFocus cap=${PICKER_CAP} ariaLabel="Filter conversations" disabled=${busy} busy=${busy || poolLoading}
        placeholder="Filter by title / role or class / descr / conv-id…" emptyHint=${emptyHint} onQuery=${setQuery}
        onChange=${(conv) => { if (conv) void addOne(candidates.find((candidate) => candidate.conv_id === conv)); }} />
      ${poolError ? html`<div class="ui-empty-block" role="alert">${poolError} <${Button} size="sm" id="add-member-pool-retry" disabled=${poolLoading || busy} onClick=${() => void loadPool()}>Retry<//></div>` : null}
      ${addError ? html`<div class="cron-create-error ui-error-box" id="add-member-error" role="alert">${addError}${failedCandidate ? html` <${Button} size="sm" id="add-member-retry" disabled=${busy} onClick=${() => addOne(failedCandidate)}>Retry<//>` : null}</div>` : null}
      <div class="add-member-foot">
        <label title="Include archived / never-online conversations in the candidate list">
          <input id="add-member-all" type="checkbox" checked=${includeOffline} disabled=${busy}
            onChange=${(event) => setIncludeOffline(event.currentTarget.checked)} /><span class="theme-copy-regular">Include offline / archived</span><span class="theme-copy-wizard">Include slumbering / archived</span>
        </label>
        <span class="spacer"></span>
        <span><kbd>↑↓</kbd> nav · <kbd>Enter</kbd> <span class="theme-copy-regular">add</span><span class="theme-copy-wizard">invite</span> · <kbd>Esc</kbd> close</span>
      </div>
    </div>
  </${Dialog}>`;
}

export function GroupsAddMemberDialog({ state, actions, confirmDiscard }) {
  const descriptor = state.addMemberDialog.value;
  return descriptor ? html`<${AddMemberDialog}
    key=${descriptor.launchID} descriptor=${descriptor} state=${state}
    actions=${actions} confirmDiscard=${confirmDiscard}
  />` : null;
}

export function mountGroupsAddMemberDialog({
  host,
  state,
  actions,
  confirmDiscard,
  registerCleanup,
}) {
  if (typeof confirmDiscard !== 'function') throw new TypeError('add member dialog requires confirmDiscard');
  render(html`<${GroupsAddMemberDialog}
    state=${state} actions=${actions} confirmDiscard=${confirmDiscard}
  />`, host);
  registerCleanup(() => {
    state.closeAddMember();
    render(null, host);
  });
}
