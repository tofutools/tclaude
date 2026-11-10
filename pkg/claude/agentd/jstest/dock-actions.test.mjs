import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The Groups toolbar's global controls live in the side panel's head while the
// Groups panel is fully open: pinned (dock-open) or floated in by the edge
// reveal (dock-revealed), never for a peek (tcl-2k2om1).

async function setup(t) {
  const harness = await createPreactHarness(t);
  const doc = harness.document;
  const toolbar = doc.body.appendChild(doc.createElement('div'));
  toolbar.id = 'toolbar';
  toolbar.innerHTML = '<input id="search"> <button id="group-create-open">+ new group</button> <span class="filter-bar-cog"><button id="cog">⚙</button></span> <span id="queued-spawns-control"></span> <span id="dashboard-default-profile-control"><select id="profile-editor"><option>a</option></select></span> <span id="dashboard-default-sandbox-profile-control"></span> <span id="tail"></span>';
  const head = doc.body.appendChild(doc.createElement('div'));
  head.innerHTML = '<div id="dock-actions-primary"></div><div id="dock-actions-profile"></div>';
  doc.body.dataset.dockPanel = 'groups';
  // A browser drops the focus of a node that moves; this DOM keeps it, so
  // the fixture does what the browser does.
  for (const name of ['appendChild', 'insertBefore']) {
    const proto = Object.getPrototypeOf(head);
    const real = proto[name];
    t.after(() => { proto[name] = real; });
    proto[name] = function moved(node, ...rest) {
      const focused = doc.activeElement;
      const out = real.call(this, node, ...rest);
      if (focused && node.contains?.(focused)) focused.blur();
      return out;
    };
  }
  const actions = await harness.importDashboardModule('js/dock-actions.js');
  const where = () => ({
    create: doc.getElementById('group-create-open').parentNode.id,
    cog: doc.querySelector('.filter-bar-cog').parentNode.id,
    profile: doc.getElementById('dashboard-default-profile-control').parentNode.id,
    queued: doc.getElementById('queued-spawns-control').parentNode.id,
    sandbox: doc.getElementById('dashboard-default-sandbox-profile-control').parentNode.id,
  });
  const homeAll = { create: 'toolbar', cog: 'toolbar', profile: 'toolbar', queued: 'toolbar', sandbox: 'toolbar' };
  const set = (...classes) => {
    doc.body.classList.remove('dock-open', 'dock-revealed', 'dock-peeking');
    doc.body.classList.add(...classes);
    actions.syncDockActions(doc);
  };
  return { doc, toolbar, actions, where, set, homeAll };
}

test('the controls move into the panel when it is revealed, and back when it is concealed', async (t) => {
  const { toolbar, where, set, homeAll } = await setup(t);
  const before = toolbar.innerHTML;
  set('dock-tab');
  assert.deepEqual(where(), homeAll, 'folded: in the toolbar');
  set('dock-tab', 'dock-revealed');
  assert.deepEqual(where(), {
    create: 'dock-actions-primary', cog: 'dock-actions-primary', queued: 'dock-actions-primary',
    profile: 'dock-actions-profile', sandbox: 'dock-actions-profile',
  }, 'revealed: in the panel head');
  set('dock-tab');
  assert.deepEqual(where(), homeAll, 'concealed: back');
  assert.equal(toolbar.innerHTML, before, 'back in their exact slots');
});

test('pinned open moves them too, and a peek does not', async (t) => {
  const { where, set } = await setup(t);
  set('dock-tab', 'dock-peeking');
  assert.equal(where().create, 'toolbar', 'a peek moves nothing');
  set('dock-tab', 'dock-open');
  assert.equal(where().create, 'dock-actions-primary', 'pinned open');
  assert.equal(where().cog, 'dock-actions-primary');
});

test('only the Groups panel takes them', async (t) => {
  const { doc, where, set } = await setup(t);
  doc.body.dataset.dockPanel = 'collab';
  set('dock-tab', 'dock-revealed');
  assert.equal(where().create, 'toolbar');
});

test('the class observer follows a reveal with no paint listener involved', async (t) => {
  const { doc, actions, where } = await setup(t);
  doc.body.classList.add('dock-tab');
  actions.bindDockActions(doc);
  doc.body.classList.add('dock-revealed');
  const deadline = Date.now() + 2000;
  while (where().create !== 'dock-actions-primary' && Date.now() < deadline) await new Promise((r) => setTimeout(r, 5));
  assert.equal(where().create, 'dock-actions-primary', 'the observer moved them');
});

test('a focused control keeps its focus across the move', async (t) => {
  const { doc, set } = await setup(t);
  const cog = doc.getElementById('cog');
  cog.focus();
  assert.equal(doc.activeElement, cog, 'this DOM tracks focus');
  set('dock-tab', 'dock-revealed');
  assert.equal(doc.getElementById('cog').parentNode.parentNode.id, 'dock-actions-primary');
  assert.equal(doc.activeElement, cog, 'refocused in the panel');
  set('dock-tab');
  assert.equal(doc.activeElement, cog, 'refocused back in the toolbar');
});

test('an open profile editor is not moved from under the user; the move waits for focus to leave', async (t) => {
  const { doc, actions, where } = await setup(t);
  doc.body.classList.add('dock-tab');
  actions.bindDockActions(doc);
  const editor = doc.getElementById('profile-editor');
  editor.focus();
  doc.body.classList.add('dock-revealed');
  actions.syncDockActions(doc);
  assert.equal(where().profile, 'toolbar', 'held while the editor has focus');
  assert.equal(doc.activeElement, editor);
  editor.blur();
  doc.dispatchEvent(new doc.defaultView.Event('focusout'));
  const deadline = Date.now() + 2000;
  while (where().profile !== 'dock-actions-profile' && Date.now() < deadline) await new Promise((r) => setTimeout(r, 5));
  assert.equal(where().profile, 'dock-actions-profile', 'moved once focus left');
});
