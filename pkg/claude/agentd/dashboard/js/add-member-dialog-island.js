import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { shortId } from './helpers.js';
import { ManagementOverlay as Overlay } from './management-overlay.js';

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

function CandidateRow({ candidate, index, highlighted, busy, onHighlight, onAdd }) {
  const display = candidate.title || '(unnamed)';
  return html`<div
    id=${`add-member-option-${index}`}
    class=${`add-member-row${highlighted ? ' highlighted' : ''}`}
    role="option" aria-selected=${highlighted ? 'true' : 'false'}
    aria-disabled=${busy ? 'true' : 'false'} data-i=${index}
    onMouseMove=${() => onHighlight(candidate.conv_id)}
    onClick=${() => { if (!busy) void onAdd(candidate); }}
  >
    <span class=${candidate.online ? 'online' : 'offline'} title=${candidate.online ? 'online' : 'offline'}>${candidate.online ? '●' : '○'}</span>
    <span class="rowname">${display}</span>
    <span class="id">${shortId(candidate.conv_id)}</span>
    ${candidate._role ? html`<span class="role">${candidate._role}</span>` : null}
    ${candidate._descr ? html`<span class="descr">${candidate._descr}</span>` : null}
    ${(candidate.groups || []).length ? html`<span class="groups-tag"><span class="theme-copy-regular">in:</span><span class="theme-copy-wizard">parties:</span> ${(candidate.groups || []).join(', ')}</span>` : null}
    ${candidate._promote ? html`<span class="groups-tag promote-tag" title="Not an agent / familiar yet — adding it here promotes it"><span class="theme-copy-regular">promotes to agent</span><span class="theme-copy-wizard">awakens as familiar</span></span>` : null}
  </div>`;
}

export function AddMemberDialog({ descriptor, state, actions, confirmDiscard }) {
  // The descriptor key and local controls survive snapshot polling. Polls may
  // change the candidate source, but never reset the user's query, toggle or
  // keyboard selection while this transaction stays mounted. Selection is an
  // identity, not an index: a same-length poll reorder cannot retarget Enter.
  const [query, setQuery] = useState('');
  const [includeOffline, setIncludeOffline] = useState(false);
  // null requests the legacy initial-first selection after an explicit filter
  // gesture; '' means polling removed the selected identity and selection must
  // stay empty until Arrow navigation or pointer hover chooses a row.
  const [highlightConv, setHighlightConv] = useState(null);
  const [promotionPool, setPromotionPool] = useState([]);
  const [poolLoading, setPoolLoading] = useState(true);
  const [poolError, setPoolError] = useState('');
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [addError, setAddError] = useState('');
  const [failedCandidate, setFailedCandidate] = useState(null);
  const listRef = useRef(null);
  const poolRequest = useRef(0);
  const poolBusyRef = useRef(false);
  const currentSnapshot = state.snapshot.value;
  const candidates = useMemo(() => buildAddMemberCandidates({
    snapshot: currentSnapshot,
    promotionPool,
    group: descriptor.group,
    includeOffline,
    query,
  }), [currentSnapshot, promotionPool, descriptor.group, includeOffline, query]);
  const selectedIndex = candidates.findIndex(
    (candidate) => candidate.conv_id === highlightConv);
  const dirty = query !== '' || includeOffline;

  const loadPool = async () => {
    if (poolBusyRef.current) return;
    poolBusyRef.current = true;
    const request = ++poolRequest.current;
    setPoolLoading(true);
    setPoolError('');
    try {
      const rows = await actions.loadAddMemberPromotionPool();
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

  const retryPool = () => {
    if (!candidates.length && selectedIndex < 0) setHighlightConv(null);
    void loadPool();
  };

  useEffect(() => {
    void loadPool();
    return () => { poolRequest.current++; };
  }, []);

  useEffect(() => {
    if (!candidates.length) {
      if (highlightConv === null && poolLoading) return;
      if (highlightConv !== '') setHighlightConv('');
      return;
    }
    if (highlightConv === null) {
      setHighlightConv(candidates[0].conv_id);
      return;
    }
    if (highlightConv && selectedIndex < 0) {
      setHighlightConv('');
    }
  }, [candidates, highlightConv, poolLoading, selectedIndex]);

  useLayoutEffect(() => {
    listRef.current?.querySelector('.add-member-row.highlighted')
      ?.scrollIntoView?.({ block: 'nearest' });
  }, [highlightConv, candidates.length]);

  const addOne = async (candidate) => {
    if (!candidate || busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setAddError('');
    setFailedCandidate(null);
    try {
      await actions.addExistingMember(descriptor, candidate);
    } catch (error) {
      setAddError((error && error.message) || String(error));
      setFailedCandidate(candidate);
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const navigate = (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      if (!candidates.length) return;
      let next;
      if (selectedIndex < 0) {
        next = event.key === 'ArrowDown' ? 0 : candidates.length - 1;
      } else {
        const delta = event.key === 'ArrowDown' ? 1 : -1;
        next = (selectedIndex + delta + candidates.length) % candidates.length;
      }
      setHighlightConv(candidates[next].conv_id);
      return;
    }
    if (event.key !== 'Enter' || event.target.closest('button')) return;
    event.preventDefault();
    if (!busyRef.current && selectedIndex >= 0) void addOne(candidates[selectedIndex]);
  };

  const emptyRetry = includeOffline
    ? '(Try a different filter.)'
    : '(Try ticking "Include offline / archived" for a wider pool.)';
  const wizardEmptyRetry = includeOffline
    ? '(Try a different filter.)'
    : '(Try ticking "Include slumbering / archived" for a wider pool.)';
  return html`<${Overlay}
    id="add-member-modal" dialogClass="add-member-modal" labelledby="add-member-title"
    onClose=${state.closeAddMember} dirty=${dirty} blocked=${busy}
    confirmDiscard=${confirmDiscard} guardBackdropDrag=${true}
  >
    <div>
      <h3 id="add-member-title"><span class="theme-copy-regular">Add member to</span><span class="theme-copy-wizard">Invite familiar into party</span> <span class="muted" id="add-member-group">${descriptor.group}</span></h3>
      <input id="add-member-search" class="add-member-search" type="text"
        value=${query} disabled=${busy} autofocus data-select-on-focus
        placeholder="Filter by title / role or class / descr / conv-id…"
        autocomplete="off" spellcheck=${false}
        role="combobox" aria-autocomplete="list" aria-controls="add-member-list"
        aria-expanded="true"
        aria-activedescendant=${selectedIndex >= 0 ? `add-member-option-${selectedIndex}` : undefined}
        onKeyDown=${navigate}
        onInput=${(event) => { setQuery(event.currentTarget.value); setHighlightConv(null); }} />
      <div
        class="add-member-list" id="add-member-list" ref=${listRef}
        role="listbox" aria-busy=${busy || poolLoading ? 'true' : 'false'}
      >
        ${poolError ? html`<div class="add-member-empty" role="alert">${poolError} <button id="add-member-pool-retry" type="button" disabled=${poolLoading || busy} onClick=${retryPool}>Retry</button></div>` : null}
        ${candidates.map((candidate, index) => html`<${CandidateRow}
          key=${candidate.conv_id} candidate=${candidate} index=${index}
          highlighted=${index === selectedIndex} busy=${busy}
          onHighlight=${setHighlightConv} onAdd=${addOne}
        />`)}
        ${!candidates.length && poolLoading ? html`<div class="add-member-empty" role="status">Loading conversations…</div>` : null}
        ${!candidates.length && !poolLoading ? html`<div class="add-member-empty"><span class="theme-copy-regular">No matching conversations. ${emptyRetry}</span><span class="theme-copy-wizard">No matching conversations. ${wizardEmptyRetry}</span></div>` : null}
      </div>
      ${addError ? html`<div class="cron-create-error" id="add-member-error" role="alert">${addError}${failedCandidate ? html` <button id="add-member-retry" type="button" disabled=${busy} onClick=${() => addOne(failedCandidate)}>Retry</button>` : null}</div>` : null}
      <div class="add-member-foot">
        <label title="Include archived / never-online conversations in the candidate list">
          <input id="add-member-all" type="checkbox" checked=${includeOffline} disabled=${busy}
            onChange=${(event) => { setIncludeOffline(event.currentTarget.checked); setHighlightConv(null); }} /><span class="theme-copy-regular">Include offline / archived</span><span class="theme-copy-wizard">Include slumbering / archived</span>
        </label>
        <span class="spacer"></span>
        <span><kbd>↑↓</kbd> nav · <kbd>Enter</kbd> <span class="theme-copy-regular">add</span><span class="theme-copy-wizard">invite</span> · <kbd>Esc</kbd> close</span>
      </div>
    </div>
  </${Overlay}>`;
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
