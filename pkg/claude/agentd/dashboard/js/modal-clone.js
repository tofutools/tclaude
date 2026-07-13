// modal-clone.js — the generic "clone a preset under a new name" dialog for the
// palette dock (the right-side panel).
//
// Each dock card's ⚙ opens a small menu (Edit / Clone — see dock.js). "Clone"
// lands here: a lightweight NEW-NAME dialog (the operator asked for exactly a
// name selector, not a second full editor) that makes a full-fidelity copy of a
// preset — a spawn PROFILE or a ROLE — under a fresh name. TEMPLATES clone via
// their own richer duplicate dialog (modal-templates.js openDuplicateModal), so
// this shared dialog serves the two kinds that had no clone before.
//
// There is no dedicated clone backend: each preset already round-trips through
// its create endpoint, and the dock reads FULL objects off the live snapshot
// (dashboard.go carries the whole spawnProfileJSON / roleJSON), so a clone is
// just "re-POST the source object with the name swapped" — every field carried,
// nothing hand-copied that could silently drift as the struct grows. The caller
// (dock.js SECTIONS) supplies the source object plus the kind's create() fn;
// this module owns only the dialog and the name-swap POST. The server's
// 409-on-existing-name is the collision guard, surfaced inline so the dialog
// stays open on a clash.

import { $ } from './helpers.js';
import { wizWord } from './slop.js';
import { toast, refresh, bindBackdropDiscard } from './refresh.js';

// The pending clone: the source object, the kind's create() and the kind nouns.
// Null while the dialog is closed.
let cloneState = null;

// openCloneModal opens the name dialog for a preset clone. opts:
//   kind        the regular-mode noun ("profile" / "role") shown in the title
//   kindWizard  the wizard-mode noun ("pattern" / "class")
//   source      the FULL preset object (carries .name plus every field to copy)
//   create      async fn(payload) → POSTs the copy (throws the server text !ok)
export function openCloneModal({ kind, kindWizard, source, create }) {
  if (!source || !source.name || typeof create !== 'function') {
    toast('nothing to clone', true);
    return;
  }
  cloneState = { source, create };
  // Title + submit carry the arcane vocabulary in wizard mode; the dialog
  // CHROME re-skins via the body.wizard #clone-modal CSS block. Set the copy in
  // JS (like the profile / role editors) so a single dialog serves both kinds.
  $('#clone-modal-title').textContent =
    wizWord(`Clone ${kind}: ${source.name}`, `Mirror ${kindWizard}: ${source.name}`);
  $('#clone-modal-blurb').textContent = wizWord(
    'A full copy under a new name — every setting is carried over; only the name changes.',
    'An identical twin under a new name — every rune is carried over; only the name changes.');
  $('#clone-modal-submit').textContent = wizWord('Create copy', 'Mirror it');
  $('#clone-modal-name').value = `${source.name}-copy`;
  $('#clone-modal-error').textContent = '';
  $('#clone-modal').classList.add('show');
  // Focus + select the suggested name so a single keystroke replaces it.
  setTimeout(() => {
    const inp = $('#clone-modal-name');
    inp.focus();
    inp.select();
  }, 0);
}

function closeCloneModal() {
  $('#clone-modal').classList.remove('show');
  cloneState = null;
}

async function submitClone() {
  const errEl = $('#clone-modal-error');
  errEl.textContent = '';
  if (!cloneState) { errEl.textContent = 'source not found — reopen the dialog'; return; }
  const { source, create } = cloneState;
  const name = $('#clone-modal-name').value.trim();
  if (!name) { errEl.textContent = 'name is required'; return; }
  if (name === source.name) { errEl.textContent = 'pick a different name for the copy'; return; }
  // Full-fidelity clone: re-POST the source object with the name swapped.
  // created_at/updated_at are response-only (the server ignores them on input);
  // dropping them keeps the payload honest rather than re-emitting stale stamps.
  const payload = { ...source, name };
  delete payload.created_at;
  delete payload.updated_at;
  const btn = $('#clone-modal-submit');
  btn.disabled = true;
  try {
    await create(payload);
    closeCloneModal();
    toast(`cloned: ${name}`);
    // The dock renders off the live snapshot, so re-fetch to surface the
    // new card at once (the 2s poll would catch it a tick later regardless).
    refresh();
  } catch (err) {
    errEl.textContent = (err && err.message) || String(err);
  } finally {
    btn.disabled = false;
  }
}

// bindCloneModal wires the dialog's controls once at boot.
export function bindCloneModal() {
  const modal = $('#clone-modal');
  if (!modal) return;
  $('#clone-modal-cancel').addEventListener('click', closeCloneModal);
  $('#clone-modal-submit').addEventListener('click', submitClone);
  bindBackdropDiscard('clone-modal', closeCloneModal);
  // Keyboard submit: Ctrl/Cmd+Enter anywhere, plain Enter in the single-line
  // name field (no newline possible → Enter is unambiguously "submit"). Guard on
  // the submit button's disabled state so a held/repeated Enter can't double-POST
  // while the first create is in flight (a double-create would 409 and read
  // confusingly as "name taken").
  modal.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    if ($('#clone-modal-submit').disabled) return;
    const modifier = e.ctrlKey || e.metaKey;
    const onNameInput = e.target === $('#clone-modal-name');
    if (modifier || onNameInput) { e.preventDefault(); submitClone(); }
  });
}
