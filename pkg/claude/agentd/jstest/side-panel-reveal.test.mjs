import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A folded side panel floats in when the pointer rests at the right edge and
// goes when the pointer leaves it (side-panel-reveal.js). These drive the real
// shell (side-panel.js) and the real watcher with fake timers, so the dwell and
// the hover-out delay are counted, not waited for.

const WIDTH = 1000;

// fakeTimers runs timeouts when the test advances the clock.
function fakeTimers() {
  let now = 0;
  let next = 1;
  const pending = new Map();
  return {
    setTimeout(fn, ms) {
      const id = next++;
      pending.set(id, { fn, at: now + ms });
      return id;
    },
    clearTimeout(id) { pending.delete(id); },
    advance(ms) {
      now += ms;
      for (const [id, timer] of [...pending].sort((a, b) => a[1].at - b[1].at)) {
        if (timer.at > now || !pending.has(id)) continue;
        pending.delete(id);
        timer.fn();
      }
    },
  };
}

// shell stands up the side panel the way dashboard.html does, with the Groups
// palette, the Collab list and a third panel that did not opt in, and binds
// the reveal. It returns what the tests drive.
async function shell(t, { fine = true } = {}) {
  const harness = await createPreactHarness(t);
  const doc = harness.document;
  const win = doc.defaultView || globalThis;
  const before = win.innerWidth;
  win.innerWidth = WIDTH;
  t.after(() => { win.innerWidth = before; });
  for (const id of ['tab-groups', 'tab-terminals', 'tab-other']) doc.body.appendChild(doc.createElement('section')).id = id;
  const dock = doc.body.appendChild(doc.createElement('aside'));
  dock.id = 'agent-dock';
  dock.appendChild(doc.createElement('button')).id = 'dock-toggle';
  const collapse = dock.appendChild(doc.createElement('button'));
  collapse.id = 'dock-collapse';
  for (const id of ['dock-body', 'dock-collab-body', 'dock-other-body']) dock.appendChild(doc.createElement('div')).id = id;
  const find = doc.getElementById('dock-body').appendChild(doc.createElement('input'));
  const menuButton = doc.getElementById('dock-body').appendChild(doc.createElement('button'));
  menuButton.setAttribute('aria-haspopup', 'menu');
  menuButton.setAttribute('aria-expanded', 'false');
  const content = doc.getElementById('tab-groups').appendChild(doc.createElement('div'));

  const [side, reveal, watch] = await Promise.all([
    harness.importDashboardModule('js/side-panel.js'),
    harness.importDashboardModule('js/side-panel-reveal.js'),
    harness.importDashboardModule('js/popover-watch.js'),
  ]);
  side.registerSidePanel('groups', { pane: 'tab-groups', pref: 'test.groups.open', open: false, body: '#dock-body', autoReveal: true,
    titles: [['Hide', 'Hide'], ['Show', 'Show']] });
  side.registerSidePanel('collab', { pane: 'tab-terminals', pref: 'test.collab.open', open: false, body: '#dock-collab-body', needsOffer: true,
    autoReveal: true, titles: [['Hide C', 'Hide C'], ['Show C', 'Show C']] });
  // Like the notifications reader, a panel that never opts in.
  side.registerSidePanel('other', { pane: 'tab-other', pref: 'test.other.open', open: false, body: '#dock-other-body',
    titles: [['Hide O', 'Hide O'], ['Show O', 'Show O']] });
  const timers = fakeTimers();
  side.bindSidePanel(doc);
  reveal.bindEdgeReveal(doc, { timers, finePointer: () => fine });
  reveal.setEdgeRevealEnabled(true, doc);

  const activate = (pane) => {
    for (const id of ['tab-groups', 'tab-terminals', 'tab-other']) doc.getElementById(id).classList.toggle('active', id === pane);
    side.applySidePanel(doc);
  };
  activate('tab-groups');
  const move = (target, clientX, init = {}) => harness.fireEvent(target, 'pointermove', { clientX, clientY: 300, buttons: 0, pointerType: 'mouse', ...init });
  const atEdge = (init) => move(content, WIDTH - 4, init);
  const revealed = () => doc.body.classList.contains('dock-revealed');
  const restAtEdge = () => {
    atEdge();
    timers.advance(reveal.EDGE_REVEAL_DWELL_MS);
  };
  return { harness, doc, dock, collapse, find, menuButton, content, side, reveal, timers, activate, move, atEdge, restAtEdge, revealed,
    hoverClose: watch.HOVER_CLOSE_MS };
}

test('resting at the right edge floats a folded panel in, over the content', async (t) => {
  const s = await shell(t);
  s.atEdge();
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS - 1);
  assert.equal(s.revealed(), false, 'not before the dwell');
  s.timers.advance(1);
  assert.equal(s.revealed(), true, 'after the dwell');
  assert.equal(s.doc.body.classList.contains('dock-open'), false, 'it floats: dock-open, which reserves its width, stays off');
  assert.equal(s.side.isSidePanelOpen('groups'), false, 'and nothing is remembered');
});

test('a pass through the edge zone that does not stay is not a reveal', async (t) => {
  const s = await shell(t);
  s.atEdge();
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS / 2);
  s.move(s.content, WIDTH - 200);
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
  assert.equal(s.revealed(), false);
  s.move(s.content, WIDTH - s.reveal.EDGE_REVEAL_PX - 1);
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
  assert.equal(s.revealed(), false, 'just outside the zone is outside it');
});

test('no reveal with a button held, during a drag, on a coarse pointer, or when the panel is unfolded', async (t) => {
  for (const [why, run] of [
    ['a button held', (s) => s.atEdge({ buttons: 1 })],
    ['a button pressed during the dwell', (s) => { s.atEdge(); s.harness.fireEvent(s.content, 'pointerdown'); }],
    ['an HTML5 drag under way', (s) => { s.harness.fireEvent(s.content, 'dragstart'); s.atEdge(); }],
    ['a touch pointer', (s) => s.atEdge({ pointerType: 'touch' })],
    ['the panel unfolded', (s) => { s.side.setSidePanelOpen(true, 'groups'); s.atEdge(); }],
  ]) {
    await t.test(why, async (t) => {
      const s = await shell(t);
      run(s);
      s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
      assert.equal(s.revealed(), false);
    });
  }
  await t.test('a pointer that cannot hover', async (t) => {
    const s = await shell(t, { fine: false });
    s.restAtEdge();
    assert.equal(s.revealed(), false);
  });
});

test('only the Groups and Collab panels reveal; a panel that did not opt in, like the notifications reader, never does', async (t) => {
  const s = await shell(t);
  s.activate('tab-terminals');
  s.restAtEdge();
  assert.equal(s.revealed(), false, 'Collab offers no panel until its view is mounted');
  s.side.offerSidePanel('collab', true);
  s.restAtEdge();
  assert.equal(s.revealed(), true, 'the Collab panel reveals');
  assert.equal(s.doc.getElementById('dock-collab-body').hidden, false, 'showing its own body');
  s.activate('tab-other');
  assert.equal(s.revealed(), false, 'a tab switch ends the reveal');
  s.restAtEdge();
  assert.equal(s.revealed(), false, 'a panel without autoReveal never floats in');
  s.activate('tab-groups');
  s.restAtEdge();
  assert.equal(s.revealed(), true, 'the Groups palette reveals');
});

test('leaving the panel sends it away after the standard hover-out delay, and coming back keeps it', async (t) => {
  const s = await shell(t);
  s.restAtEdge();
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  s.move(s.content, 300);
  s.timers.advance(s.hoverClose - 1);
  assert.equal(s.revealed(), true, 'not before the delay');
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), true, 'back over it in time, it stays');
  s.move(s.content, 300);
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'gone after the delay');
});

test('it stays while it has focus, an open menu, or a drag that started in it', async (t) => {
  await t.test('focus', async (t) => {
    const s = await shell(t);
    s.restAtEdge();
    s.find.focus();
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), true, 'typing in Find keeps it');
    s.find.blur();
    s.harness.fireEvent(s.find, 'focusout');
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'focus leaving, with the pointer away, lets it go');
  });
  await t.test('an open menu', async (t) => {
    const s = await shell(t);
    s.restAtEdge();
    s.menuButton.setAttribute('aria-expanded', 'true');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), true, 'a menu open in it keeps it');
    s.menuButton.setAttribute('aria-expanded', 'false');
    s.move(s.content, 310);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'the menu closed, it goes');
  });
  await t.test('a context menu placed outside it', async (t) => {
    const s = await shell(t);
    s.restAtEdge();
    const menu = s.doc.body.appendChild(s.doc.createElement('div'));
    menu.setAttribute('role', 'menu');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), true);
    menu.remove();
    s.move(s.content, 310);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false);
  });
  await t.test('a drag from it', async (t) => {
    const s = await shell(t);
    s.restAtEdge();
    s.harness.fireEvent(s.doc.getElementById('dock-body'), 'dragstart');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), true, 'a card dragged out of it onto a group keeps it until the drop');
    s.harness.fireEvent(s.doc.getElementById('dock-body'), 'dragend');
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false);
  });
});

test('Escape sends it away; its handle pins it open; its Hide button only sends it away', async (t) => {
  const s = await shell(t);
  s.restAtEdge();
  s.harness.fireEvent(s.doc.body, 'keydown', { key: 'Escape' });
  assert.equal(s.revealed(), false, 'Escape');

  s.restAtEdge();
  s.harness.fireEvent(s.collapse, 'click');
  assert.equal(s.revealed(), false, 'Hide sends it away');
  assert.equal(s.side.isSidePanelOpen('groups'), false, 'without unfolding it');

  s.restAtEdge();
  s.harness.fireEvent(s.doc.getElementById('dock-toggle'), 'click');
  assert.equal(s.side.isSidePanelOpen('groups'), true, 'the handle unfolds it');
  assert.equal(s.doc.body.classList.contains('dock-open'), true);
  assert.equal(s.revealed(), false, 'pinned, not floating');
  s.move(s.content, 300);
  s.timers.advance(s.hoverClose);
  assert.equal(s.side.isSidePanelOpen('groups'), true, 'and leaving it does not fold it');
});

test('the config switch turns it off, and off takes a floating panel away', async (t) => {
  const s = await shell(t);
  s.restAtEdge();
  assert.equal(s.revealed(), true);
  s.reveal.setEdgeRevealEnabled(false, s.doc);
  assert.equal(s.revealed(), false, 'off sends it away');
  s.restAtEdge();
  assert.equal(s.revealed(), false, 'and none comes back');
  s.reveal.setEdgeRevealEnabled(true, s.doc);
  s.restAtEdge();
  assert.equal(s.revealed(), true);
});
