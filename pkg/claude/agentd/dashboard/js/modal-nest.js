// modal-nest.js — the "nest this group under another" dialog (n-level
// groups-in-groups, JOH-392).
//
// Opened from a group's ⚙ menu (data-act="nest-group"). It presents a single
// <select> of eligible parents plus a "— top level —" option that clears the
// nesting. Eligible parents are every OTHER real group that is not already a
// descendant of this one — excluding descendants client-side keeps the picker
// honest, though the server is the real cycle guard (it 409s a loop and this
// dialog surfaces that inline).
//
// The PUT is idempotent and board-only: it writes agent_groups.parent_id and
// nothing else. A successful save force-refreshes so the tree re-lays-out at
// once instead of a poll-tick later.

import { $ } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { toast, refresh, bindBackdropDiscard } from './refresh.js';

// The group currently being re-parented; null while the dialog is closed.
let nestState = null;

// descendantsOf returns the set of group names reachable downward from `name`
// via the snapshot's parent edges (name itself included). Used to keep a group
// from being offered its own child/grandchild as a parent (which would loop).
function descendantsOf(name, groups) {
  const childrenOf = new Map();
  for (const g of groups) {
    if (g.parent) {
      if (!childrenOf.has(g.parent)) childrenOf.set(g.parent, []);
      childrenOf.get(g.parent).push(g.name);
    }
  }
  const out = new Set();
  const stack = [name];
  while (stack.length) {
    const cur = stack.pop();
    if (out.has(cur)) continue; // guard against a pre-existing corrupt loop
    out.add(cur);
    for (const c of (childrenOf.get(cur) || [])) stack.push(c);
  }
  return out;
}

// openNestModal opens the parent picker for the group named `group`.
export function openNestModal({ group }) {
  if (!group) { toast('no group', true); return; }
  const groups = ((lastSnapshot && lastSnapshot.groups) || []);
  const me = groups.find(g => g.name === group);
  const currentParent = (me && me.parent) || '';
  // Eligible parents: every other real group that isn't a descendant of `group`.
  const blocked = descendantsOf(group, groups);
  const candidates = groups.map(g => g.name).filter(n => !blocked.has(n)).sort((a, b) => a.localeCompare(b));

  nestState = { group };
  $('#group-nest-title').textContent = `Nest group: ${group}`;
  const sel = $('#group-nest-parent');
  const opts = ['<option value="">— top level (no parent) —</option>']
    .concat(candidates.map(n => `<option value="${encodeURIComponent(n)}"${n === currentParent ? ' selected' : ''}></option>`));
  sel.innerHTML = opts.join('');
  // Fill option text via .textContent (not the HTML above) so a group name with
  // markup-significant characters can't break out — the value carries the
  // percent-encoded name, decoded on submit.
  Array.from(sel.options).forEach((o, i) => {
    if (i === 0) return; // the "top level" sentinel keeps its literal label
    o.textContent = decodeURIComponent(o.value);
  });
  $('#group-nest-error').textContent = '';
  $('#group-nest-modal').classList.add('show');
  setTimeout(() => sel.focus(), 0);
}

function closeNestModal() {
  $('#group-nest-modal').classList.remove('show');
  nestState = null;
}

async function submitNest() {
  const errEl = $('#group-nest-error');
  errEl.textContent = '';
  if (!nestState) { errEl.textContent = 'group not found — reopen the dialog'; return; }
  const { group } = nestState;
  const sel = $('#group-nest-parent');
  const parent = sel.value ? decodeURIComponent(sel.value) : '';
  const btn = $('#group-nest-submit');
  btn.disabled = true;
  try {
    const r = await fetch(`/api/groups/${encodeURIComponent(group)}/parent`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ parent }),
    });
    if (!r.ok) {
      const t = await r.text();
      errEl.textContent = t || `error ${r.status}`;
      return;
    }
    closeNestModal();
    toast(parent ? `${group}: nested under ${parent}` : `${group}: moved to top level`);
    refresh({ force: true });
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

// bindNestModal wires the dialog's controls once at boot.
export function bindNestModal() {
  const modal = $('#group-nest-modal');
  if (!modal) return;
  $('#group-nest-cancel').addEventListener('click', closeNestModal);
  $('#group-nest-submit').addEventListener('click', submitNest);
  bindBackdropDiscard('group-nest-modal', closeNestModal);
  modal.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    if ($('#group-nest-submit').disabled) return;
    e.preventDefault();
    submitNest();
  });
}
