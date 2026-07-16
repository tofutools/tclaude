// The dashboard toolbar's two profile chips are static shell controls rather
// than Preact-owned feature UI. This module owns their one-shot <select>
// lifecycle and exposes only the refresh-suspension bit needed by refresh.js.

import { loadProfiles, findProfileByHandle, profileChoices } from './profiles.js';
import { openProfileEditor } from './modal-profiles.js';
import { wizWord } from './slop.js';
import { toast } from './refresh.js';
import { lastSnapshot, setLastSnapshot } from './dashboard.js';

// Historical name retained for refreshSuspended's compatibility contract.
let renameEditing = false;

const PROFILE_PICKER_NEW = '/new-profile';

async function openToolbarProfilePicker(chipEl, current, onCommit, opts = {}) {
  const loadList = opts.loadList || loadProfiles;
  const noneLabel = opts.noneLabel || '(none)';
  const newLabel = opts.newLabel || wizWord('＋ new profile…', '＋ new pattern…');
  const openNewEditor = opts.openNewEditor || ((onSaved) => openProfileEditor(null, { onSaved }));
  const prevSnapshot = lastSnapshot;

  // Fetch before suspending refresh or replacing the stable toolbar node. A
  // concurrent picker or poll may win while the request is pending.
  let profiles = [];
  try { profiles = await loadList(); } catch (_) { profiles = []; }
  if (renameEditing || !chipEl.isConnected) return;

  renameEditing = true;
  const select = document.createElement('select');
  select.className = 'group-default-profile-select';
  select.add(new Option(newLabel, PROFILE_PICKER_NEW));
  select.add(new Option(noneLabel, ''));
  for (const choice of profileChoices(profiles)) select.add(new Option(choice.label, choice.value));
  if (current && !findProfileByHandle(profiles, current)) {
    select.add(new Option(`${current} (missing)`, current));
  }
  select.value = current;
  chipEl.replaceWith(select);
  select.focus();

  let done = false;
  const cancel = (restoreFocus = false) => {
    if (done) return;
    done = true;
    // Restore this exact node: dock.js caches the toolbar controls by identity
    // while re-homing them between the filter bar and the open dock.
    if (select.parentNode) select.replaceWith(chipEl);
    renameEditing = false;
    setLastSnapshot(prevSnapshot);
    if (restoreFocus) chipEl.focus();
  };
  const commit = async () => {
    if (done) return;
    const name = select.value;
    if (name === PROFILE_PICKER_NEW) {
      done = true;
      if (select.parentNode) select.replaceWith(chipEl);
      renameEditing = false;
      openNewEditor((newName) => onCommit(newName));
      return;
    }
    if (name === current) { cancel(true); return; }
    done = true;
    if (select.parentNode) select.replaceWith(chipEl);
    renameEditing = false;
    try {
      const ok = await onCommit(name);
      if (!ok) setLastSnapshot(prevSnapshot);
    } catch (err) {
      toast((err && err.message) || String(err), true);
      setLastSnapshot(prevSnapshot);
    }
  };
  select.addEventListener('change', commit);
  select.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') { event.preventDefault(); cancel(true); }
  });
  select.addEventListener('blur', () => cancel());
}

export { openToolbarProfilePicker, renameEditing };
