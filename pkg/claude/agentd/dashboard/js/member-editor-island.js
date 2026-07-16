import { h, render } from 'preact';
import { useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';

const html = htm.bind(h);

export function validMemberTitle(value) {
  const title = String(value || '');
  return !!title && title.length <= 64 && !title.includes('  ')
    && /^[A-Za-z0-9_\-[\]{}() ]+$/.test(title);
}

export function parseMemberTags(value) {
  const seen = new Set();
  const tags = [];
  for (const raw of String(value || '').split(',')) {
    const tag = raw.trim();
    if (tag && !seen.has(tag)) {
      seen.add(tag);
      tags.push(tag);
    }
  }
  return tags;
}

function sameTagSet(left, right) {
  if (left.length !== right.length) return false;
  const expected = new Set(right);
  return left.every((tag) => expected.has(tag));
}

export function memberEditorChanges(baseline, draft, auto) {
  const changes = {};
  if (auto) {
    changes.rename = { auto: true };
  } else {
    const title = draft.title.trim();
    if (title !== baseline.title) {
      if (!validMemberTitle(title)) {
        throw new Error('title must be 1-64 chars of letters, digits, space or _ - [ ] { } ( ) — no double spaces');
      }
      changes.rename = { title };
    }
  }
  if (draft.role !== baseline.role) changes.role = draft.role;
  if (draft.descr !== baseline.descr) changes.descr = draft.descr;
  const tags = parseMemberTags(draft.tags);
  if (!sameTagSet(tags, baseline.tags)) changes.tags = tags;
  if (draft.owner !== baseline.owner) changes.owner = draft.owner;
  return changes;
}

function initialBaseline(descriptor) {
  return {
    title: descriptor.title,
    role: descriptor.role,
    descr: descriptor.descr,
    tags: [...descriptor.tags],
    owner: descriptor.owner,
  };
}

function initialDraft(descriptor) {
  return {
    title: descriptor.title,
    role: descriptor.role,
    descr: descriptor.descr,
    tags: descriptor.tags.join(', '),
    owner: descriptor.owner,
  };
}

const FAILURE_LABELS = {
  rename: 'title', membership: 'role / description', tags: 'tags', owner: 'ownership',
};

export function MemberEditorDialog({ descriptor, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  // The descriptor and first baseline are keyed by launchID. Snapshot polling
  // updates the rows behind this overlay, never this local transaction.
  const [baseline, setBaseline] = useState(() => initialBaseline(descriptor));
  const [draft, setDraft] = useState(() => initialDraft(descriptor));
  const [auto, setAuto] = useState(false);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState('');
  const changes = useMemo(() => {
    try { return memberEditorChanges(baseline, draft, auto); }
    catch (_) { return { invalid: true }; }
  }, [baseline, draft, auto]);
  const dirty = Object.keys(changes).length > 0;
  const update = (key, value) => setDraft((current) => ({ ...current, [key]: value }));

  const submit = async () => {
    if (busyRef.current) return;
    setError('');
    let pending;
    try {
      pending = memberEditorChanges(baseline, draft, auto);
    } catch (cause) {
      setError((cause && cause.message) || String(cause));
      return;
    }
    if (!Object.keys(pending).length) {
      actions.noMemberChanges();
      state.closeMemberEditor();
      return;
    }
    busyRef.current = true;
    setBusy(true);
    try {
      const result = await actions.saveMemberEditor(descriptor, pending);
      const succeeded = new Set(result.succeeded || []);
      const next = { ...baseline };
      if (succeeded.has('rename')) {
        if (pending.rename?.title) next.title = pending.rename.title;
        setAuto(false);
      }
      if (succeeded.has('membership')) {
        if ('role' in pending) next.role = pending.role;
        if ('descr' in pending) next.descr = pending.descr;
      }
      if (succeeded.has('tags')) next.tags = [...pending.tags];
      if (succeeded.has('owner')) next.owner = pending.owner;
      setBaseline(next);
      if (!result.errors?.length) {
        state.closeMemberEditor();
        return;
      }
      setError(result.errors.map((item) =>
        `${FAILURE_LABELS[item.key] || item.key}: ${item.message}`).join('\n'));
    } catch (cause) {
      setError((cause && cause.message) || String(cause));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const autofocus = (field) => descriptor.focus === field;
  return html`<${Overlay}
    id="edit-member-modal" dialogClass="modal" labelledby="edit-member-title"
    onClose=${state.closeMemberEditor} onSubmitHotkey=${submit}
    dirty=${dirty} blocked=${busy} confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}
    guardBackdropDrag=${true}
  >
    <h3 id="edit-member-title"><span class="edit-member-title-regular">Edit agent</span><span class="edit-member-title-wizard">Enchant this familiar</span></h3>
    <div class="modal-meta" id="edit-member-meta">${descriptor.label} → ${descriptor.group}</div>
    <div class="field">
      <label for="edit-member-title-input">Title</label>
      <input id="edit-member-title-input" type="text" value=${draft.title}
        disabled=${auto || busy} autofocus=${autofocus('title')} data-select-on-focus
        autocomplete="off" spellcheck=${false}
        placeholder="1-64 chars, [A-Za-z0-9_-[]{}() ] only"
        onInput=${(event) => update('title', event.currentTarget.value)} />
      <label class="edit-member-auto-row" title="Ask this agent / familiar to pick a descriptive title itself via the agent-rename skill / CLI.">
        <input id="edit-member-auto" type="checkbox" checked=${auto} disabled=${busy}
          onChange=${(event) => setAuto(event.currentTarget.checked)} /> <span class="theme-copy-regular">Auto — let the agent choose its own title</span><span class="theme-copy-wizard">Auto — let the familiar choose its own title</span>
      </label>
    </div>
    <div class="field">
      <label for="edit-member-role"><span class="theme-copy-regular">Role</span><span class="theme-copy-wizard">Class</span></label>
      <input id="edit-member-role" type="text" value=${draft.role} disabled=${busy}
        autofocus=${autofocus('role')} data-select-on-focus autocomplete="off" spellcheck=${false}
        onInput=${(event) => update('role', event.currentTarget.value)} />
      <label class="edit-member-owner-row" title="Group / party owners gain the implicit power to manage the other agents / familiars in it without a per-slug grant.">
        <input id="edit-member-owner" type="checkbox" checked=${draft.owner} disabled=${busy}
          onChange=${(event) => update('owner', event.currentTarget.checked)} /> <span class="theme-copy-regular">Group owner</span><span class="theme-copy-wizard">Party owner</span>
      </label>
    </div>
    <div class="field">
      <label for="edit-member-descr">Description</label>
      <textarea id="edit-member-descr" rows="3" value=${draft.descr} disabled=${busy} spellcheck=${false}
        onInput=${(event) => update('descr', event.currentTarget.value)}></textarea>
      <label for="edit-member-tags">Tags</label>
      <input id="edit-member-tags" type="text" value=${draft.tags} disabled=${busy}
        autofocus=${autofocus('descr')} data-select-on-focus autocomplete="off" spellcheck=${false}
        placeholder="comma-separated, e.g. tf:squad, priority"
        title="Short labels rendered as chips in the Description column. They follow this agent / familiar across groups / parties."
        onInput=${(event) => update('tags', event.currentTarget.value)} />
    </div>
    <div class="cron-create-error" id="edit-member-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button id="edit-member-perms" type="button" disabled=${busy}
        title="Open this agent / familiar's permanent permission grimoire (grant / deny / inherit per slug)"
        onClick=${() => actions.openMemberPermissions(descriptor)}><span class="em-btn-regular">Permissions…</span><span class="em-btn-wizard">Grimoire…</span></button>
      <span class="spacer"></span>
      <button id="edit-member-cancel" type="button" disabled=${busy}
        onClick=${() => { void requestClose(); }}><span class="em-btn-regular">Cancel</span><span class="em-btn-wizard">Dispel</span></button>
      <button id="edit-member-save" type="button" disabled=${busy} onClick=${submit}>${busy ? 'Saving…' : 'Save'}</button>
    </div>
  </${Overlay}>`;
}

export function GroupsMemberDialog({ state, actions, confirmDiscard }) {
  const descriptor = state.memberEditor.value;
  return descriptor ? html`<${MemberEditorDialog}
    key=${descriptor.launchID} descriptor=${descriptor} state=${state}
    actions=${actions} confirmDiscard=${confirmDiscard}
  />` : null;
}

export function mountGroupsMemberEditor({ host, state, actions, confirmDiscard, registerCleanup }) {
  if (typeof confirmDiscard !== 'function') throw new TypeError('member editor requires confirmDiscard');
  render(html`<${GroupsMemberDialog} state=${state} actions=${actions} confirmDiscard=${confirmDiscard} />`, host);
  registerCleanup(() => {
    state.closeMemberEditor();
    render(null, host);
  });
}
