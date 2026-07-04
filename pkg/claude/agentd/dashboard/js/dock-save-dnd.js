// dock-save-dnd.js — REVERSE-direction palette DnD (JOH-393): drag a LIVE agent
// row or a LIVE group header onto the palette dock to CAPTURE it as a spawn
// profile / group template. The exact mirror of dock-dnd.js (which drags a
// palette card the OTHER way — onto a group to spawn from it).
//
// This module adds NO new drag SOURCE. It reuses the two existing ones and only
// makes the dock a DROP TARGET for them, routing the drop to the right editor:
//   - a member row   (dnd.js, MIME application/x-tclaude-member) →
//       openProfileEditor(seed, {editExisting:false}) with the agent's live
//       launch config captured server-side (POST /api/spawn-profiles/from-agent);
//   - a group header (group-reorder.js, MIME application/x-tclaude-group) →
//       openTemplateEditor(snapshot, {asNew:true}) with the group's live roster
//       captured server-side (POST /api/templates/from-group, preview mode).
// Both open the FULL editor pre-filled but UNSAVED, so the human previews and
// edits the captured blueprint before an explicit Save creates it. Nothing is
// written until that Save — a cancelled drop leaves no profile/template behind.
//
// Coexistence with the three OTHER document-level DnD modules (dnd.js,
// group-reorder.js, dock-dnd.js) is deliberate and total, mirroring how those
// three already coexist:
//   - This module's dragover/drop self-gate on the two REVERSE-source flags
//     (dndDragActive || groupReorderActive) AND only act when the cursor is over
//     the dock. Off the dock it does nothing and the source module owns the
//     gesture. A dock-card drag (dock-dnd.js, dockDragActive) is the FORWARD
//     direction and is excluded, so dropping a card back onto the dock is inert.
//   - It shares the #dnd-pill hint chip. Over the dock the cursor is OFF the
//     source modules' targets, where dnd.js / group-reorder.js set the pill to
//     null; to win that, bindDockSaveDnd() is registered AFTER bindDnd() /
//     bindGroupReorder() (see dashboard.js) so its dragover runs LAST and its
//     pill assignment is the final one for the event.
//   - Its hover highlight uses a DISTINCT class (.dock-save-over), so the other
//     modules' dragleave/dragend cleanup (which strips only their own classes)
//     never fights it. This module clears .dock-save-over on its own dragend.
//   - The member/group source module's own drop handler runs first and bails on
//     a dock drop (the dock is in neither's target selector), returning WITHOUT
//     preventDefault — so this handler is the one that acts.
//
// Survives the 2s poll for free: dndDragActive / groupReorderActive already keep
// refreshSuspended() from rebuilding the DOM mid-drag, so the dock (drop target)
// and the dragged row/header (drag source) stay attached through the gesture.

import { $, $$ } from './helpers.js';
import { toast } from './refresh.js';
import { wizWord } from './slop.js';
import { openProfileEditor } from './modal-profiles.js';
import { openTemplateEditor } from './modal-templates.js';
// Live-binding flag imports (exported as `let`), read at EVENT time — never at
// module-eval time — so the benign import cycles through dashboard.js are
// TDZ-safe, exactly like dnd.js reading lastSnapshot.
import { dndDragActive } from './dnd.js';
import { groupReorderActive } from './group-reorder.js';

// The two reverse-source drag MIMEs (authoritative payload read on drop; browsers
// gate getData to the drop event, so dragover can't read them — it keys off the
// active flags instead). Kept in lockstep with dnd.js / group-reorder.js.
const MEMBER_MIME = 'application/x-tclaude-member';
const GROUP_MIME = 'application/x-tclaude-group';

// The dock drop target: the open panel (#agent-dock) AND its edge tab
// (#dock-toggle). The tab rides the collapse translate to the viewport's right
// edge, so listing it here lets a COLLAPSED dock still accept a save without the
// human first expanding it.
const DOCK_TARGET_SEL = '#agent-dock, #dock-toggle';

// reverseActive: a member drag (dnd.js) or a group-header drag (group-reorder.js)
// is in flight — the only two gestures this module reacts to.
function reverseActive() {
  return dndDragActive || groupReorderActive;
}

// dockUnder resolves the dock element under the cursor (the panel or its edge
// tab), or null when the cursor is elsewhere.
function dockUnder(e) {
  return e.target.closest(DOCK_TARGET_SEL);
}

// clearDockSaveHighlight strips the reverse-drop highlight from every box that
// carries it (at most the dock panel or its tab).
function clearDockSaveHighlight() {
  $$('.dock-save-over').forEach(el => el.classList.remove('dock-save-over'));
}

// dockSavePill reuses the shared #dnd-pill hint chip. Null text hides it.
function dockSavePill(e, text) {
  const pill = $('#dnd-pill');
  if (!pill) return;
  if (!text) { pill.classList.remove('show', 'clone'); return; }
  pill.textContent = text;
  pill.classList.remove('clone');
  pill.classList.add('show');
  // Offset from the cursor like the other DnD pills so it doesn't sit under the
  // pointer; dragover clientX/clientY jitter is masked by the offset.
  pill.style.transform = `translate(${e.clientX + 12}px, ${e.clientY + 12}px)`;
}

// pillText composes the save hint for the active reverse drag — a group becomes
// a template, an agent becomes a profile (both vocab modes via wizWord).
function pillText() {
  return groupReorderActive
    ? wizWord('⧉ save group as template', '⧉ inscribe party as circle')
    : wizWord('⚙ save agent as profile', '⚙ bind familiar as pattern');
}

function bindDockSaveDnd() {
  // No dock on the page → nothing to bind (mirrors the other dock modules).
  if (!$('#agent-dock')) return;

  document.addEventListener('dragover', (e) => {
    if (!reverseActive()) return;
    // Repaint from scratch each move so a box we've left goes dark even if its
    // dragleave was swallowed (Firefox occasionally drops the final dragleave).
    // Cheap: at most one element carries the class.
    clearDockSaveHighlight();
    const dock = dockUnder(e);
    // Off the dock: do NOT touch the pill — the source module (which ran just
    // before us) owns the hint for its own targets. Just leave.
    if (!dock) return;
    e.preventDefault(); // required for `drop` to fire on this element
    e.dataTransfer.dropEffect = 'copy'; // a capture copies config; it never moves the row
    dock.classList.add('dock-save-over');
    dockSavePill(e, pillText());
  });

  // dragend clears THIS module's highlight on every drag-end outcome (a
  // successful drop, an Escape-cancel, or a release over nothing). The source
  // module clears its own flags + the shared pill; we own only .dock-save-over.
  document.addEventListener('dragend', clearDockSaveHighlight);

  document.addEventListener('drop', (e) => {
    if (!reverseActive()) return;
    if (!dockUnder(e)) return;
    e.preventDefault();
    // Tear down our hover highlight before opening the editor; the source
    // module's dragend still fires afterwards and clears its pill/flags.
    clearDockSaveHighlight();
    const types = e.dataTransfer.types;
    // A group reorder carries the bare group name under GROUP_MIME.
    if (types.includes(GROUP_MIME)) {
      const group = e.dataTransfer.getData(GROUP_MIME);
      if (group) saveGroupAsTemplate(group);
      return;
    }
    // A member row carries a JSON payload under MEMBER_MIME (text/plain is the
    // fallback channel dnd.js also writes).
    if (types.includes(MEMBER_MIME) || types.includes('text/plain')) {
      const raw = e.dataTransfer.getData(MEMBER_MIME) || e.dataTransfer.getData('text/plain');
      let payload;
      try { payload = JSON.parse(raw); } catch (_) { return; }
      if (!payload || !payload.conv) return;
      // Route by the rotation-immune agent_id (conv-id fallback), the same
      // selector dnd.js's endpoint calls resolve server-side.
      saveAgentAsProfile(payload.agent || payload.conv, payload.label || payload.conv);
    }
  });
}

// saveGroupAsTemplate fetches an UNSAVED template snapshot of the live group
// (from-group in preview mode — traces roles, owners, per-agent permissions,
// launch shape + context WITHOUT persisting) and opens the template editor
// pre-filled in CREATE mode, so the human previews + edits before Save creates
// it. The template name seeds from the group's name as a starting suggestion.
async function saveGroupAsTemplate(group) {
  try {
    const r = await fetch('/api/templates/from-group', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ group, template_name: group, preview: true }),
    });
    if (!r.ok) { toast(`save "${group}" as template failed: ${await r.text()}`, true); return; }
    const tmpl = await r.json();
    openTemplateEditor(tmpl, { asNew: true });
  } catch (err) {
    toast(`save as template failed: ${err && err.message || err}`, true);
  }
}

// saveAgentAsProfile fetches an UNSAVED profile seed of the live agent (from-agent
// — traces its observable harness/model/effort/sandbox + granted permissions
// WITHOUT persisting) and opens the profile editor pre-filled in CREATE mode, so
// the human previews + names the new profile before Save creates it.
async function saveAgentAsProfile(sel, label) {
  try {
    const r = await fetch('/api/spawn-profiles/from-agent', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agent: sel }),
    });
    if (!r.ok) { toast(`save "${label}" as profile failed: ${await r.text()}`, true); return; }
    const seed = await r.json();
    openProfileEditor(seed, { editExisting: false });
  } catch (err) {
    toast(`save as profile failed: ${err && err.message || err}`, true);
  }
}

export { bindDockSaveDnd };
