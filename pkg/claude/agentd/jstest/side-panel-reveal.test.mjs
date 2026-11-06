import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A folded side panel peeks in when the pointer heads for the right edge,
// floats in when the pointer moves onto the peek, and goes when the pointer
// leaves it (side-panel-reveal.js). These drive the real shell (side-panel.js)
// and the real watcher with fake timers, so the peek's retract and the
// hover-out delay are counted, not waited for.

const WIDTH = 1000;
// REST is a long rest: no edge dwell opens the panel any more (tcl-bg5qm1).
const REST = 1000;

// fakeTimers runs timeouts when the test advances the clock.
function fakeTimers() {
  let now = 0;
  let next = 1;
  const pending = new Map();
  return {
    now: () => now,
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
async function shell(t, { fine = true, grow = false } = {}) {
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
  reveal.bindEdgeReveal(doc, { timers, now: timers.now, finePointer: () => fine, debug: false });
  reveal.setEdgeRevealEnabled(true, doc);
  // The growing peek is off by default (tcl-bg5qm1); its tests turn it on.
  if (grow) {
    reveal.setSidePanelTuning({ growing_peek: true }, doc);
    t.after(() => reveal.setSidePanelTuning(null, doc));
  }

  const activate = (pane) => {
    for (const id of ['tab-groups', 'tab-terminals', 'tab-other']) doc.getElementById(id).classList.toggle('active', id === pane);
    side.applySidePanel(doc);
  };
  activate('tab-groups');
  const move = (target, clientX, init = {}) => harness.fireEvent(target, 'pointermove', { clientX, clientY: 300, buttons: 0, pointerType: 'mouse', ...init });
  const atEdge = (init) => move(content, WIDTH - 4, init);
  const revealed = () => doc.body.classList.contains('dock-revealed');
  const peeking = () => doc.body.classList.contains('dock-peeking');
  // flick moves the pointer from x0 to x1 (and y0 to y1) in steps every
  // stepMs, advancing the clock between them, as a real mousemove stream does.
  // ease, when set, brakes the motion (each step a fraction of the last), as a
  // move that is stopping does.
  const flick = ({ x0, x1, y0 = 300, y1 = y0, steps = 6, stepMs = 16, ease = 0, target = content, init = {} }) => {
    const weights = Array.from({ length: steps }, (_, i) => (ease ? (1 - ease) ** i : 1));
    const total = weights.reduce((a, b) => a + b, 0);
    let x = x0; let y = y0;
    move(target, x, { clientY: y, ...init });
    for (const w of weights) {
      timers.advance(stepMs);
      x += ((x1 - x0) * w) / total; y += ((y1 - y0) * w) / total;
      move(target, x, { clientY: y, ...init });
    }
  };
  // openAtEdge opens the folded panel the way a pointer does (tcl-bg5qm1):
  // a snap to the edge peeks it, and a move onto the peek opens it.
  const openAtEdge = (init = {}) => {
    timers.advance(200);
    move(content, 60, init);
    timers.advance(16);
    move(content, WIDTH - 4, init);
    timers.advance(16);
    move(dock, WIDTH - 10, init);
  };
  return { harness, doc, dock, collapse, find, menuButton, content, side, reveal, timers, activate, move, atEdge, openAtEdge, revealed, peeking, flick,
    hoverClose: watch.HOVER_CLOSE_MS };
}

test('resting at the right edge opens nothing; a peek does, moved onto, over the content', async (t) => {
  const s = await shell(t);
  s.atEdge();
  s.timers.advance(REST);
  assert.equal(s.revealed() || s.peeking(), false, 'the edge alone opens nothing, so the controls along it stay reachable (tcl-bg5qm1)');
  s.openAtEdge();
  assert.equal(s.revealed(), true, 'a snap peeks it, and a move onto the peek opens it');
  assert.equal(s.doc.body.classList.contains('dock-open'), false, 'it floats: dock-open, which reserves its width, stays off');
  assert.equal(s.side.isSidePanelOpen('groups'), false, 'and nothing is remembered');
});

test('a pass through the edge zone that does not stay is not a reveal', async (t) => {
  const s = await shell(t);
  s.atEdge();
  s.timers.advance(REST / 2);
  s.move(s.content, WIDTH - 200);
  s.timers.advance(REST);
  assert.equal(s.revealed(), false);
  s.move(s.content, WIDTH - s.reveal.EDGE_REVEAL_PX - 1);
  s.timers.advance(REST);
  assert.equal(s.revealed(), false, 'just outside the zone is outside it');
});

test('no reveal with a button held, during a drag, on a coarse pointer, or when the panel is unfolded', async (t) => {
  for (const [why, run] of [
    ['a button held', (s) => s.atEdge({ buttons: 1 })],
    ['a button pressed at the edge', (s) => { s.atEdge(); s.harness.fireEvent(s.content, 'pointerdown'); }],
    // No pointer events fire during an HTML5 drag: dragging over the edge
    // zone is not a reveal.
    ['an HTML5 drag under way', (s) => { s.atEdge(); s.harness.fireEvent(s.content, 'dragstart');
      s.harness.fireEvent(s.content, 'dragover', { clientX: WIDTH - 4, clientY: 300 }); }],
    ['a touch pointer', (s) => s.atEdge({ pointerType: 'touch' })],
    ['the panel unfolded', (s) => { s.side.setSidePanelOpen(true, 'groups'); s.atEdge(); }],
  ]) {
    await t.test(why, async (t) => {
      const s = await shell(t);
      run(s);
      s.timers.advance(REST);
      assert.equal(s.revealed(), false);
    });
  }
  await t.test('a pointer that cannot hover', async (t) => {
    const s = await shell(t, { fine: false });
    s.openAtEdge();
    assert.equal(s.revealed(), false);
  });
});

test('only the Groups and Collab panels reveal; a panel that did not opt in, like the notifications reader, never does', async (t) => {
  const s = await shell(t);
  s.activate('tab-terminals');
  s.openAtEdge();
  assert.equal(s.revealed(), false, 'Collab offers no panel until its view is mounted');
  s.side.offerSidePanel('collab', true);
  s.openAtEdge();
  assert.equal(s.revealed(), true, 'the Collab panel reveals');
  assert.equal(s.doc.getElementById('dock-collab-body').hidden, false, 'showing its own body');
  s.activate('tab-other');
  assert.equal(s.revealed(), false, 'a tab switch ends the reveal');
  s.openAtEdge();
  assert.equal(s.revealed(), false, 'a panel without autoReveal never floats in');
  s.activate('tab-groups');
  s.openAtEdge();
  assert.equal(s.revealed(), true, 'the Groups palette reveals');
});

test('leaving the panel sends it away after the standard hover-out delay, and coming back keeps it', async (t) => {
  const s = await shell(t);
  s.openAtEdge();
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
    s.openAtEdge();
    s.find.focus();
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), true, 'typing in Find keeps it');
    s.find.blur();
    s.harness.fireEvent(s.find, 'focusout');
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'focus leaving, with the pointer away, lets it go');
  });
  await t.test('but not the focus a click leaves on a button', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    s.menuButton.focus();
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'a clicked button keeps focus in Chrome; that does not hold the panel');
  });
  await t.test('an open menu', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
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
    s.openAtEdge();
    const menu = s.doc.body.appendChild(s.doc.createElement('div'));
    menu.setAttribute('role', 'menu');
    // On screen: a context menu is rendered only while open, so it lays out.
    menu.getClientRects = () => [{ width: 120, height: 80 }];
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
    s.openAtEdge();
    s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
    // The card leaves the panel as it is dragged (the panel's leave), and the
    // page sees no pointer events until the drop.
    s.harness.fireEvent(s.doc.getElementById('dock-body'), 'dragstart');
    s.harness.fireEvent(s.dock, 'pointerleave');
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), true, 'a card dragged out of it onto a group keeps it until the drop');
    s.harness.fireEvent(s.doc.getElementById('dock-body'), 'dragend');
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false);
  });
});

test('Escape sends it away; its handle pins it open; its Hide button only sends it away', async (t) => {
  const s = await shell(t);
  s.openAtEdge();
  s.harness.fireEvent(s.doc.body, 'keydown', { key: 'Escape' });
  assert.equal(s.revealed(), false, 'Escape');

  s.openAtEdge();
  s.harness.fireEvent(s.collapse, 'click');
  assert.equal(s.revealed(), false, 'Hide sends it away');
  assert.equal(s.side.isSidePanelOpen('groups'), false, 'without unfolding it');

  s.openAtEdge();
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
  s.openAtEdge();
  assert.equal(s.revealed(), true);
  s.reveal.setEdgeRevealEnabled(false, s.doc);
  assert.equal(s.revealed(), false, 'off sends it away');
  s.openAtEdge();
  assert.equal(s.revealed(), false, 'and none comes back');
  s.reveal.setEdgeRevealEnabled(true, s.doc);
  s.openAtEdge();
  assert.equal(s.revealed(), true);
});

// The review's finds: each was a way to get stuck or to slide in at the wrong
// moment, and each passed the pins above.

test('closed menus the page keeps in the DOM do not hold it', async (t) => {
  const s = await shell(t);
  // Hidden by a class, not the hidden attribute, like .action-menu:not(.open).
  const closed = s.doc.body.appendChild(s.doc.createElement('div'));
  closed.className = 'ui-menu action-menu';
  closed.setAttribute('role', 'menu');
  closed.getClientRects = () => [];
  s.openAtEdge();
  s.move(s.content, 300);
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'it still goes');
  s.openAtEdge();
  s.harness.fireEvent(s.doc.body, 'keydown', { key: 'Escape' });
  assert.equal(s.revealed(), false, 'and Escape still works');
});

test('a drag whose dragend never arrives does not leave the reveal dead', async (t) => {
  const s = await shell(t);
  // A drop that re-renders the dragged group header: no dragend reaches the
  // document. The next pointer move proves the drag is over.
  s.harness.fireEvent(s.content, 'dragstart');
  s.openAtEdge();
  assert.equal(s.revealed(), true, 'the next pointer move ends the drag, so resting at the edge reveals again');
  await t.test('nor holds a panel it was dragged from', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    s.harness.fireEvent(s.doc.getElementById('dock-body'), 'dragstart');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false);
  });
});

// On the handle nothing opens or closes by itself (tcl-bg5qm1): a pointer
// there is about to pin or unpin the panel. Moving off it resumes the rest.
test('on the handle the panel neither opens nor closes by itself; its click pins; moving off resumes', async (t) => {
  let s = await shell(t);
  const handle = () => s.doc.getElementById('dock-toggle');
  s.move(handle(), WIDTH - 4);
  s.timers.advance(REST * 4);
  assert.equal(s.revealed(), false, 'resting on the folded handle opens nothing');
  s.harness.fireEvent(handle(), 'click');
  assert.equal(s.side.isSidePanelOpen('groups'), true, 'and its click pins the panel');

  s = await shell(t);
  s.move(handle(), WIDTH - 4);
  s.timers.advance(REST);
  s.openAtEdge();
  assert.equal(s.revealed(), true, 'off the handle, the panel opens as anywhere');
  s.move(handle(), WIDTH - 4);
  s.move(s.content, 300);
  s.move(handle(), WIDTH - 4);
  s.timers.advance(s.hoverClose * 4);
  assert.equal(s.revealed(), true, 'back on the revealed panel’s handle, it does not go');
  s.harness.fireEvent(handle(), 'click');
  assert.equal(s.side.isSidePanelOpen('groups'), true, 'and its click pins it');
});

test('a reveal does not carry over from one tab\'s panel to another\'s', async (t) => {
  const s = await shell(t);
  s.side.offerSidePanel('collab', true);
  s.openAtEdge();
  assert.equal(s.revealed(), true);
  s.activate('tab-terminals');
  assert.equal(s.revealed(), false, 'the Collab panel does not appear already floating');
});

// Leaving the window freezes the reveal (tcl-bg5qm1): nothing opens or closes
// on the way out, by any edge, and nothing pending acts until the pointer
// moves back in, when the normal rules take over.
test('leaving the window freezes the panel: nothing opens or closes until the pointer is back', async (t) => {
  const { PEEK_RETRACT_MS } = (await (await createPreactHarness(t)).importDashboardModule('js/side-panel-predict.js')).PREDICT;
  // leaveWindow is the pointer going out of the window: the panel's leave
  // (when it was on it), then the document's.
  const leaveWindow = (s, { fromPanel = false, y = 300 } = {}) => {
    if (fromPanel) s.dock.dispatchEvent(new s.harness.window.Event('pointerleave', { bubbles: false }));
    s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { clientX: WIDTH + 5, clientY: y });
  };

  let s = await shell(t);
  s.openAtEdge();
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  leaveWindow(s, { fromPanel: true });
  s.timers.advance(s.hoverClose * 4);
  assert.equal(s.revealed(), true, 'out past the right edge from the panel, it stays');
  s.move(s.content, 300);
  assert.equal(s.revealed(), true, 'back onto the page, the hover-out delay starts afresh');
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'and then it goes');

  s = await shell(t);
  s.openAtEdge();
  s.move(s.content, 300);
  leaveWindow(s, { y: -5 });
  s.timers.advance(s.hoverClose * 4);
  assert.equal(s.revealed(), true, 'a hide pending when the pointer left by the top does not fire while it is out');
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  s.timers.advance(s.hoverClose * 4);
  assert.equal(s.revealed(), true, 'back in onto the panel, it stays');

  s = await shell(t);
  s.atEdge();
  leaveWindow(s);
  s.timers.advance(REST);
  assert.equal(s.revealed() || s.peeking(), false, 'resting at the edge and leaving opens nothing');
  s.openAtEdge();
  assert.equal(s.revealed(), true, 'back in, the usual rules: a peek moved onto opens it');

  s = await shell(t);
  withPageScrollbar(s);
  s.flick(SHORT_FLING);
  assert.equal(s.peeking(), true);
  leaveWindow(s, { y: -5 });
  s.timers.advance(PEEK_RETRACT_MS * 4);
  assert.equal(s.peeking(), true, 'a peek holds while the pointer is out');
  s.move(s.content, 300);
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking(), false, 'back in away from it, it runs out');

  s = await shell(t);
  withPageScrollbar(s);
  s.flick(SHORT_FLING);
  leaveWindow(s, { y: -5 });
  s.harness.fireEvent(s.doc, 'keydown', { key: 'Escape' });
  assert.equal(s.peeking(), false, 'Escape still takes a peek back while the pointer is out');

  s = await shell(t);
  s.openAtEdge();
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  leaveWindow(s, { fromPanel: true });
  s.harness.fireEvent(s.dock, 'focusout');
  s.timers.advance(s.hoverClose * 4);
  assert.equal(s.revealed(), true, 'the window losing focus while the pointer is out does not send it away');
  s.harness.fireEvent(s.dock, 'focusout', { relatedTarget: s.content });
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'but a focus move out of it, by keyboard, still does');

  s = await shell(t);
  s.openAtEdge();
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  leaveWindow(s, { fromPanel: true });
  const frame = s.doc.body.appendChild(s.doc.createElement('iframe'));
  s.harness.fireEvent(frame, 'pointerover');
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'back in straight over a frame, away from the panel, it goes after the delay');

  s = await shell(t);
  s.openAtEdge();
  s.move(s.content, 300);
  s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { pointerType: 'touch' });
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'a touch lifting is no window exit: the pending hide still fires');

  s = await shell(t);
  snap(s, [420, 860]);
  s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { clientX: 999, clientY: 300 });
  assert.equal(s.peeking(), true, 'a snap running off the edge peeks the panel on the way out');
  assert.equal(s.revealed(), false, 'and opens nothing');
  s.timers.advance(PEEK_RETRACT_MS * 4);
  assert.equal(s.peeking(), true, 'the peek stays while the pointer is out');
  s.move(s.content, 300);
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking(), false, 'back on the page, it runs out');
});

test('leaving the panel straight into a frame still sends it away', async (t) => {
  const s = await shell(t);
  s.openAtEdge();
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  // Into an iframe: the page gets no further pointermove, only the panel's
  // leave, which does not bubble.
  s.dock.dispatchEvent(new s.harness.window.Event('pointerleave', { bubbles: false }));
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false);
});

test('the edge zone is the panel\'s own height band', async (t) => {
  const s = await shell(t);
  s.dock.getBoundingClientRect = () => ({ top: 100, bottom: 600, left: WIDTH, right: WIDTH + 264, width: 264, height: 500 });
  s.openAtEdge({ clientY: 50 });
  assert.equal(s.revealed() || s.peeking(), false, 'a snap above the panel (the top bar) is not into the edge zone');
  s.openAtEdge({ clientY: 300 });
  assert.equal(s.revealed(), true);
});

// End to end over the real dashboard.html body: its closed menus are in the
// DOM (the Groups filter bar's cog menu, hidden by a class), and the panel
// still auto-hides and Escape still sends it away.
test('end to end over the real dashboard markup: the panel auto-hides', async (t) => {
  const { readFile } = await import('node:fs/promises');
  const { fileURLToPath } = await import('node:url');
  const harness = await createPreactHarness(t);
  const doc = harness.document;
  const page = await readFile(fileURLToPath(new URL('../dashboard/dashboard.html', import.meta.url)), 'utf8');
  doc.body.innerHTML = page.slice(page.indexOf('<body'), page.lastIndexOf('</body>')).replace(/^<body[^>]*>/, '').replace(/<script[\s\S]*?<\/script>/g, '');
  assert.ok(doc.querySelector('.action-menu[role="menu"]'), 'fixture: a closed menu the page keeps in the DOM');
  assert.ok(doc.getElementById('agent-dock') && doc.getElementById('dock-toggle') && doc.getElementById('tab-groups'), 'fixture: the real shell');
  const win = doc.defaultView || globalThis;
  const before = win.innerWidth;
  win.innerWidth = WIDTH;
  t.after(() => { win.innerWidth = before; });
  const [side, reveal, watch] = await Promise.all([
    harness.importDashboardModule('js/side-panel.js'),
    harness.importDashboardModule('js/side-panel-reveal.js'),
    harness.importDashboardModule('js/popover-watch.js'),
  ]);
  side.registerSidePanel('groups', { pane: 'tab-groups', pref: 'test.e2e.open', open: false, body: '#dock-body', autoReveal: true,
    titles: [['Hide', 'Hide'], ['Show', 'Show']] });
  const timers = fakeTimers();
  side.bindSidePanel(doc);
  reveal.bindEdgeReveal(doc, { timers, now: timers.now, finePointer: () => true });
  side.applySidePanel(doc);
  const main = doc.getElementById('tab-groups');
  const move = (target, clientX) => harness.fireEvent(target, 'pointermove', { clientX, clientY: 300, buttons: 0, pointerType: 'mouse' });
  // A snap to the edge peeks the panel; a move onto the peek opens it.
  const open = () => {
    timers.advance(200);
    move(main, 60);
    timers.advance(16);
    move(main, WIDTH - 4);
    timers.advance(16);
    move(doc.getElementById('dock-body'), WIDTH - 10);
  };

  open();
  assert.ok(doc.body.classList.contains('dock-revealed'), 'it floats in');
  move(doc.getElementById('dock-body'), WIDTH - 100);
  move(main, 300);
  timers.advance(watch.HOVER_CLOSE_MS);
  assert.ok(!doc.body.classList.contains('dock-revealed'), 'and goes after the hover-out delay');
  open();
  harness.fireEvent(doc.body, 'keydown', { key: 'Escape' });
  assert.ok(!doc.body.classList.contains('dock-revealed'), 'Escape sends it away');
});

// tcl-vv7jn8: the page's own vertical scrollbar sits at the right edge,
// right of the fixed panel. A pointer on it targets <html>, not the panel.
const SCROLLBAR = 15;
const PANEL_W = 264;

// creep moves the pointer slowly (20px every 50ms, under any fling speed) from
// where it is to x, so arriving at the edge after it is no snap.
function creep(s, from, x) {
  for (let at = from; at < x; at = Math.min(x, at + 20)) {
    s.timers.advance(50);
    s.move(s.content, Math.min(x, at + 20));
  }
}

// withPageScrollbar gives the page a scrollbar and the panel its real box: off
// screen right of the content while folded, left of the scrollbar while it
// floats in.
// width is the scrollbar's layout width: 0 for no scrollbar and for an
// overlay scrollbar (macOS), which takes no width but still sits at the edge.
// setWidth changes it live, as a scrollbar appearing or going does.
function withPageScrollbar(s, width = SCROLLBAR) {
  let bar = width;
  const contentRight = () => WIDTH - bar;
  Object.defineProperty(s.doc.documentElement, 'clientWidth', { configurable: true, get: contentRight });
  s.dock.getBoundingClientRect = () => {
    const left = s.revealed() ? contentRight() - PANEL_W : contentRight();
    return { top: 100, bottom: 600, left, right: left + PANEL_W, width: PANEL_W, height: 500 };
  };
  const onScrollbar = (init) => s.move(s.doc.documentElement, WIDTH - 5, init);
  return { right: contentRight(), onScrollbar, setWidth: (w) => { bar = w; } };
}

test('the pointer on the page scrollbar, under the edge zone, opens nothing, and does not flicker an open panel', async (t) => {
  const s = await shell(t);
  const { onScrollbar } = withPageScrollbar(s);
  onScrollbar();
  s.timers.advance(REST);
  assert.equal(s.revealed() || s.peeking(), false, 'resting on the scrollbar strip opens nothing: it stays the page\'s to grab');
  s.openAtEdge();
  onScrollbar();
  assert.equal(s.revealed(), true, 'an open panel stays with the pointer on the scrollbar beside it');
  // The loop was: on the scrollbar, not "over" the panel, so it went after the
  // hover-out delay, and the old edge dwell brought it straight back.
  for (let i = 0; i < 10; i++) {
    onScrollbar({ clientY: 300 + (i % 2 ? 3 : -3) });
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), true, `still shown after move ${i + 1}: the scrollbar beside it counts as over it`);
  }
});

test('the edge zone is EDGE_REVEAL_PX wide from the content edge, plus the page scrollbar', async (t) => {
  const s = await shell(t);
  const { right } = withPageScrollbar(s);
  assert.ok(s.reveal.EDGE_REVEAL_PX >= 24 && s.reveal.EDGE_REVEAL_PX <= 40, 'a zone a quick flick can hit');
  const snapTo = (x) => { s.timers.advance(200); s.move(s.content, 60); s.timers.advance(16); s.move(s.content, x); };
  snapTo(right - s.reveal.EDGE_REVEAL_PX - 1);
  assert.equal(s.peeking(), false, 'a snap to just outside it peeks nothing');
  snapTo(right - s.reveal.EDGE_REVEAL_PX);
  assert.equal(s.peeking(), true, 'one to the zone\'s inner edge peeks the panel');
});

test('dragging the panel\'s own scrollbar, or the page scrollbar beside it, does not retract it', async (t) => {
  await t.test('its own scrollbar, dragged out past its left edge', async (t) => {
    const s = await shell(t);
    withPageScrollbar(s);
    s.openAtEdge();
    const body = s.doc.getElementById('dock-body');
    s.move(body, WIDTH - 30);
    s.harness.fireEvent(body, 'pointerdown', { clientX: WIDTH - 30, clientY: 300, buttons: 1, pointerType: 'mouse' });
    s.harness.fireEvent(s.dock, 'pointerleave');
    s.move(s.content, 300, { buttons: 1 });
    s.timers.advance(s.hoverClose * 3);
    assert.equal(s.revealed(), true, 'held while the press that started on it is down');
    s.harness.fireEvent(s.content, 'pointerup', { clientX: 300, clientY: 300, pointerType: 'mouse' });
    s.timers.advance(s.hoverClose - 1);
    assert.equal(s.revealed(), true, 'released away from it, it waits the hover-out delay');
    s.timers.advance(1);
    assert.equal(s.revealed(), false, 'then goes');
  });
  await t.test('the page scrollbar, dragged below the panel', async (t) => {
    const s = await shell(t);
    const { onScrollbar } = withPageScrollbar(s);
    s.openAtEdge();
    onScrollbar();
    s.harness.fireEvent(s.doc.documentElement, 'pointerdown', { clientX: WIDTH - 5, clientY: 300, buttons: 1, pointerType: 'mouse' });
    onScrollbar({ clientY: 700, buttons: 1 });
    s.timers.advance(s.hoverClose * 3);
    assert.equal(s.revealed(), true, 'a scrollbar drag that started beside it holds it');
    // Chrome may not deliver the pointerup of a scrollbar drag: the next plain
    // move ends the press.
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'and the next plain move away lets it go after the delay');
  });
});

test('leaving the panel retracts it after exactly the popover hover-out delay', async (t) => {
  const s = await shell(t);
  withPageScrollbar(s);
  s.openAtEdge();
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  s.move(s.content, 300);
  s.timers.advance(s.hoverClose - 1);
  assert.equal(s.revealed(), true, 'still shown just before HOVER_CLOSE_MS');
  s.timers.advance(1);
  assert.equal(s.revealed(), false, 'gone at HOVER_CLOSE_MS');
});

test('the zone and the no-flicker hold work the same with no scrollbar, an overlay scrollbar, or one that comes and goes', async (t) => {
  const steady = async (s, at, what) => {
    for (let i = 0; i < 6; i++) {
      at({ clientY: 300 + (i % 2 ? 3 : -3) });
      s.timers.advance(s.hoverClose);
      assert.equal(s.revealed(), true, `${what}: still shown after move ${i + 1}`);
    }
  };
  await t.test('no vertical scrollbar (clientWidth equals innerWidth)', async (t) => {
    const s = await shell(t);
    withPageScrollbar(s, 0);
    s.openAtEdge();
    assert.equal(s.revealed(), true, 'the same peek opens it');
    // The panel now reaches the window edge, so the pointer is on it.
    await steady(s, (init) => s.move(s.doc.getElementById('dock-body'), WIDTH - 5, init), 'no scrollbar');
  });
  await t.test('an overlay scrollbar: no width, but a pointer on it targets <html>', async (t) => {
    const s = await shell(t);
    const { onScrollbar } = withPageScrollbar(s, 0);
    s.openAtEdge();
    onScrollbar();
    assert.equal(s.revealed(), true);
    await steady(s, onScrollbar, 'overlay scrollbar');
  });
  await t.test('a scrollbar that appears, then goes, while the panel floats', async (t) => {
    const s = await shell(t);
    const { onScrollbar, setWidth } = withPageScrollbar(s, 0);
    s.openAtEdge();
    assert.equal(s.revealed(), true);
    setWidth(SCROLLBAR); // the panel shifts left; the pointer is now on the new scrollbar
    await steady(s, onScrollbar, 'scrollbar appeared');
    setWidth(0); // the panel shifts back under the pointer
    await steady(s, (init) => s.move(s.doc.getElementById('dock-body'), WIDTH - 5, init), 'scrollbar gone');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'and it still goes once the pointer leaves');
  });
});

test('a dialog or popover drawn over the panel\'s band is not the panel', async (t) => {
  const s = await shell(t);
  withPageScrollbar(s);
  s.openAtEdge();
  const dialog = s.doc.body.appendChild(s.doc.createElement('div'));
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  s.move(dialog, WIDTH - 100);
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'over something else in its band, it goes after the delay');
});

// The predictive peek (tcl-o4nims): a pointer heading for the edge fast shows
// the folded panel's leading edge before it gets there. The false positives
// are the risk, so most of these say when it must NOT peek.

const FLING = { x0: 300, x1: 800 }; // 500px in ~96ms: a fling at the edge
// With the panel's real box (withPageScrollbar), the open panel's left edge
// would be at FULL_LEFT; SHORT_FLING stops short of it, so it peeks without
// crossing into the full panel's area (which reveals).
const FULL_LEFT = WIDTH - SCROLLBAR - PANEL_W;
const SHORT_FLING = { x0: 300, x1: 700 };

test('a fast, level fling at the edge peeks the panel before the pointer gets there', async (t) => {
  const s = await shell(t);
  s.flick(FLING);
  assert.equal(s.peeking(), true, 'it peeks while the pointer is still 200px out');
  assert.equal(s.revealed(), false, 'a peek is not a reveal');
  assert.equal(s.doc.body.classList.contains('dock-open'), false, 'and nothing reflows or is remembered');
});

test('moving onto the peeking strip reveals at once; the page scrollbar beside it opens nothing', async (t) => {
  let s = await shell(t);
  s.flick(FLING);
  s.move(s.dock, WIDTH - 20);
  assert.equal(s.revealed(), true, 'on the strip: the prediction was right');
  assert.equal(s.peeking(), false, 'and the peek is over');

  s = await shell(t);
  const { onScrollbar } = withPageScrollbar(s);
  s.flick(FLING);
  assert.equal(s.peeking(), true);
  onScrollbar();
  s.timers.advance(REST);
  assert.equal(s.revealed(), false, 'on the scrollbar, the one part of the edge zone the strip leaves, nothing opens: it stays the page\'s to grab');
  assert.equal(s.peeking(), true, 'and the peek holds while the pointer is by the edge');
  s.move(s.dock, WIDTH - SCROLLBAR - 20);
  assert.equal(s.revealed(), true, 'a move onto the peek opens it');
});

// No exception at the folded handle's height (tcl-bg5qm1): a fling there
// peeks like any other, and the handle's click still pins the panel.
test('a fling at the handle’s height peeks like any other, and the click still pins the panel open', async (t) => {
  const s = await shell(t);
  const handle = s.doc.getElementById('dock-toggle');
  handle.getBoundingClientRect = () => ({ top: 273, bottom: 327, height: 54, left: WIDTH - 22, right: WIDTH, width: 22 });
  s.flick({ ...FLING, y0: 300 });
  assert.equal(s.peeking(), true, 'it peeks: the prediction does not know the handle');
  s.move(handle, WIDTH - 10);
  assert.equal(s.revealed(), false, 'reaching the handle does not turn the peek into a reveal');
  handle.click();
  assert.equal(s.side.isSidePanelOpen('groups'), true, 'the handle’s click unfolds it');
});

// A press on the strip while it only peeks means the strip slid under a still
// pointer (a moving one would have revealed it): the press was meant for the
// page there, so it acts on nothing in the panel.
test('a press on a strip that only peeks is swallowed with its click, and ends the peek', async (t) => {
  const s = await shell(t);
  const card = s.doc.getElementById('dock-body').appendChild(s.doc.createElement('button'));
  s.flick(FLING);
  assert.equal(s.peeking(), true);
  const down = s.harness.fireEvent(card, 'pointerdown', { clientX: WIDTH - 40, clientY: 300, buttons: 1, pointerType: 'mouse' });
  assert.equal(s.peeking(), false, 'the peek goes');
  assert.equal(s.revealed(), false, 'and is no reveal');
  assert.equal(down.defaultPrevented, true, 'the press is cancelled, so it starts nothing in the panel');
  assert.equal(s.harness.fireEvent(card, 'click').defaultPrevented, true, 'and so is its click');
  assert.equal(s.harness.fireEvent(card, 'click').defaultPrevented, false, 'only that one click');
});

// The handle rides the peek, and a press on it is the operator pinning
// (tcl-bg5qm1): it is never swallowed, and the peek holds until its click.
test('a press on the handle of a peeking panel pins it', async (t) => {
  const { PEEK_RETRACT_MS } = (await (await createPreactHarness(t)).importDashboardModule('js/side-panel-predict.js')).PREDICT;
  const s = await shell(t);
  const handle = s.doc.getElementById('dock-toggle');
  s.flick(FLING);
  assert.equal(s.peeking(), true);
  const down = s.harness.fireEvent(handle, 'pointerdown', { clientX: WIDTH - 40, clientY: 300, buttons: 1, pointerType: 'mouse' });
  assert.equal(down.defaultPrevented, false, 'the press goes through');
  s.harness.fireEvent(handle, 'pointermove', { clientX: WIDTH - 39, clientY: 301, buttons: 1, pointerType: 'mouse' });
  assert.equal(s.peeking(), true, 'a real click\'s jitter, button held, keeps the peek under it');
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking(), true, 'and the peek holds under it until the click');
  const click = s.harness.fireEvent(handle, 'click');
  assert.equal(click.defaultPrevented, false, 'its click is not swallowed');
  assert.equal(s.side.isSidePanelOpen('groups'), true, 'and pins the panel open');
  assert.equal(s.peeking(), false, 'which ends the peek');
  s.harness.fireEvent(handle, 'pointerdown', { clientX: WIDTH - 40, clientY: 300, buttons: 1, pointerType: 'mouse' });
  s.harness.fireEvent(handle, 'click');
  assert.equal(s.side.isSidePanelOpen('groups'), false, 'and a second click unpins it');
});

test('a press on the peeking panel\'s handle dragged off it lets the peek go', async (t) => {
  const s = await shell(t);
  const handle = s.doc.getElementById('dock-toggle');
  s.flick(FLING);
  s.harness.fireEvent(handle, 'pointerdown', { clientX: WIDTH - 40, clientY: 300, buttons: 1, pointerType: 'mouse' });
  s.move(s.content, WIDTH - 200, { buttons: 1 });
  assert.equal(s.peeking(), false);
  assert.equal(s.side.isSidePanelOpen('groups'), false);
});

// Chrome sends a move at the same position when the layout changes under a
// still pointer, as the strip sliding in does: that is no move onto it.
test('the strip sliding under a still pointer does not reveal the panel; a real move onto it does', async (t) => {
  const s = await shell(t);
  s.flick(FLING);
  s.move(s.content, 800);
  s.move(s.dock, 800);
  assert.equal(s.revealed(), false, 'same position as the last move');
  s.harness.fireEvent(s.dock, 'pointerenter');
  assert.equal(s.revealed(), false, 'nor does the enter that comes with it');
  s.move(s.dock, 806);
  assert.equal(s.revealed(), true, 'a real move onto the strip');
});

test('sustained motion keeps a peek; when it stops short the peek retracts after PEEK_RETRACT_MS', async (t) => {
  const s = await shell(t);
  const { PEEK_RETRACT_MS } = (await s.harness.importDashboardModule('js/side-panel-predict.js')).PREDICT;
  assert.ok(PEEK_RETRACT_MS >= 400 && PEEK_RETRACT_MS <= 600, 'about half a second');
  s.flick(FLING);
  for (let i = 0; i < 4; i++) {
    s.timers.advance(PEEK_RETRACT_MS - 100);
    s.flick({ x0: 300, x1: 800, y0: 300 + i });
    assert.equal(s.peeking(), true, `kept alive by fling ${i + 2}`);
  }
  s.timers.advance(PEEK_RETRACT_MS - 1);
  assert.equal(s.peeking(), true, 'not before the delay');
  s.timers.advance(1);
  assert.equal(s.peeking(), false, 'the motion stopped short, so it goes');
  s.timers.advance(5000);
  assert.equal(s.revealed(), false, 'and nothing comes back on its own');
});

test('no peek for a move that is not a fling at the edge', async (t) => {
  for (const [why, run] of [
    ['a fast move braking to stop short of the edge', (s) => s.flick({ x0: 300, x1: 850, ease: 0.55 })],
    ['a vertical flick', (s) => s.flick({ x0: 600, x1: 600, y0: 100, y1: 600 })],
    ['a leftward flick', (s) => s.flick({ x0: 800, x1: 300 })],
    ['an ordinary move across the page', (s) => s.flick({ x0: 600, x1: 800, steps: 20 })],
    ['one jump', (s) => { s.move(s.content, 300); s.timers.advance(16); s.move(s.content, 900); }],
    ['a fling with a button held (a selection or a drag)', (s) => s.flick({ ...FLING, init: { buttons: 1 } })],
    ['a fling from right beside the edge (too short to count)', (s) => s.flick({ x0: 900, x1: 955, steps: 4, stepMs: 10 })],
  ]) {
    const s = await shell(t);
    run(s);
    assert.equal(s.peeking(), false, `${why}: no peek`);
    s.timers.advance(2000);
    assert.equal(s.revealed(), false, `${why}: and no reveal`);
  }
});

test('no peek on a coarse pointer, with the setting off, on a panel that did not opt in, or unfolded', async (t) => {
  let s = await shell(t, { fine: false });
  s.flick(FLING);
  assert.equal(s.peeking(), false, 'coarse pointer');
  s = await shell(t);
  s.reveal.setEdgeRevealEnabled(false, s.doc);
  s.flick(FLING);
  assert.equal(s.peeking(), false, 'dashboard.side_panel_edge_reveal off');
  s = await shell(t);
  s.activate('tab-other');
  s.flick(FLING);
  assert.equal(s.peeking(), false, 'a panel that did not opt in');
  s = await shell(t);
  s.side.setSidePanelOpen(true, 'groups');
  assert.equal(s.side.isSidePanelOpen('groups'), true);
  s.flick(FLING);
  assert.equal(s.peeking(), false, 'an unfolded panel');
});

test('unfolding a peeking panel ends the peek', async (t) => {
  const s = await shell(t);
  s.flick(FLING);
  assert.equal(s.peeking(), true);
  s.side.setSidePanelOpen(true, 'groups');
  assert.equal(s.peeking(), false);
  assert.equal(s.doc.body.classList.contains('dock-peeking'), false);
});

test('a press, a drag, Escape, a tab switch or the setting going off takes a peek back at once', async (t) => {
  for (const [why, run] of [
    ['a press on the content', (s) => s.harness.fireEvent(s.content, 'pointerdown')],
    ['a drag starting', (s) => s.harness.fireEvent(s.content, 'dragstart')],
    ['Escape', (s) => s.harness.fireEvent(s.doc, 'keydown', { key: 'Escape' })],
    ['a tab switch', (s) => s.activate('tab-other')],
    ['the setting off', (s) => s.reveal.setEdgeRevealEnabled(false, s.doc)],
    ['a move with a button held', (s) => s.move(s.content, 820, { buttons: 1 })],
  ]) {
    const s = await shell(t);
    s.flick(FLING);
    assert.equal(s.peeking(), true, `${why}: peeking first`);
    run(s);
    assert.equal(s.peeking(), false, `${why}: the peek goes at once`);
    s.timers.advance(2000);
    assert.equal(s.revealed(), false, `${why}: and no reveal follows`);
  }
});

test('a fling to the page scrollbar peeks, and a press there takes it back with no reveal', async (t) => {
  const s = await shell(t);
  const { onScrollbar } = withPageScrollbar(s);
  s.flick(SHORT_FLING);
  assert.equal(s.peeking(), true);
  // Then on to the scrollbar in a short move after a pause: a long fast
  // arrival there would be a snap, which reveals at once.
  s.timers.advance(100);
  s.move(s.content, 860);
  s.timers.advance(100);
  onScrollbar();
  assert.equal(s.peeking(), true, 'still peeking on the scrollbar');
  s.timers.advance(REST / 2);
  s.harness.fireEvent(s.doc.documentElement, 'pointerdown');
  assert.equal(s.peeking(), false, 'grabbing the scrollbar ends the peek');
  s.timers.advance(REST * 4);
  assert.equal(s.revealed(), false, 'and is no reveal');
});

test('a peek is not a reveal: it runs no hover-out, so repeated short flings never loop it into a reveal', async (t) => {
  const s = await shell(t);
  const { PEEK_RETRACT_MS } = (await s.harness.importDashboardModule('js/side-panel-predict.js')).PREDICT;
  withPageScrollbar(s);
  for (let i = 0; i < 6; i++) {
    s.flick({ ...SHORT_FLING, y0: 300 + i });
    assert.equal(s.peeking(), true, `fling ${i + 1} peeks`);
    s.move(s.content, 680, { clientY: 300 + i });
    s.timers.advance(PEEK_RETRACT_MS + s.hoverClose);
    assert.equal(s.peeking(), false, `fling ${i + 1}: stopped short, it retracts`);
    assert.equal(s.revealed(), false, `fling ${i + 1}: never a reveal`);
  }
});

// v2 (tcl-wdwbm2): the peek grows, crossing into or stopping in the full
// panel's area reveals, a cancel shrinks before it retracts, and a motion is
// judged from where it started. All on the panel's real box: the open panel
// would start at FULL_LEFT (721), and the edge zone at 953.

const peekW = (s) => parseFloat(s.doc.body.style.getPropertyValue('--dock-peek-w')) || 0;

test('the peek grows as the pointer closes in on the open panel’s edge, up to the panel’s width', async (t) => {
  const s = await shell(t, { grow: true });
  withPageScrollbar(s);
  const widths = [];
  const { x0, x1 } = SHORT_FLING;
  s.move(s.content, x0);
  for (let i = 1; i <= 6; i++) {
    s.timers.advance(16);
    s.move(s.content, x0 + ((x1 - x0) * i) / 6);
    if (s.peeking()) widths.push(peekW(s));
  }
  assert.ok(widths.length >= 2, `it peeks mid-fling (${widths})`);
  assert.equal(widths[0], 48, 'it starts at the peek’s start width');
  assert.ok(widths.every((w, i) => i === 0 || w >= widths[i - 1]), `and only grows (${widths})`);
  assert.ok(widths.at(-1) > 200 && widths.at(-1) <= PANEL_W, `nearly the panel’s width 21px out (${widths.at(-1)})`);
});

test('crossing where the open panel’s edge would be reveals the panel', async (t) => {
  const s = await shell(t, { grow: true });
  withPageScrollbar(s);
  s.flick(FLING);
  assert.equal(s.revealed(), true, 'past FULL_LEFT with the prediction holding');
  assert.equal(s.peeking(), false);
  assert.equal(peekW(s), 0, 'the peek width is cleared');
});

test('a stop inside the full panel’s area, or just short of the peek, reveals; a stop well short does not', async (t) => {
  const { STOP_MS, PEEK_RETRACT_MS } = (await (await createPreactHarness(t)).importDashboardModule('js/side-panel-predict.js')).PREDICT;
  let s = await shell(t, { grow: true });
  withPageScrollbar(s);
  s.flick({ x0: 300, x1: 715 });
  assert.equal(s.peeking(), true, 'peeking, 6px short of FULL_LEFT');
  s.timers.advance(STOP_MS - 1);
  assert.equal(s.revealed(), false, 'not before STOP_MS');
  s.timers.advance(1);
  assert.equal(s.revealed(), true, 'stopped just short of the grown peek: a reveal, not a retract');

  s = await shell(t, { grow: true });
  withPageScrollbar(s);
  s.reveal.setSidePanelTuning({ growing_peek: false }, s.doc);
  t.after(() => s.reveal.setSidePanelTuning(null, s.doc));
  s.flick({ x0: 300, x1: 715 });
  s.timers.advance(STOP_MS);
  assert.equal(s.revealed(), false, 'with no growth, 6px short of the panel is far from the 48px peek');

  s = await shell(t, { grow: true });
  withPageScrollbar(s);
  s.flick({ x0: 300, x1: 600 });
  assert.equal(s.peeking(), true);
  s.timers.advance(PEEK_RETRACT_MS + 10);
  assert.equal(s.revealed(), false, 'a stop 121px short of the panel lets the peek go');
  assert.equal(s.peeking(), false);
});

test('a braking stop on content under the would-be panel does not peek or reveal', async (t) => {
  for (const [why, x1] of [['inside the full panel’s area', 820], ['just short of it', 715]]) {
    const s = await shell(t);
    withPageScrollbar(s);
    s.flick({ x0: 300, x1, ease: 0.55, steps: 8 });
    assert.equal(s.peeking(), false, `${why}: braking, no peek`);
    s.timers.advance(2000);
    assert.equal(s.revealed(), false, `${why}: and no reveal, by crossing or by the stop`);
  }
});

test('a prediction that stops holding shrinks the peek back before it retracts', async (t) => {
  const s = await shell(t, { grow: true });
  withPageScrollbar(s);
  const { PEEK_RETRACT_MS, PEEK_MIN_PX } = s.reveal.REVEAL_DEFAULTS;
  s.flick(SHORT_FLING);
  assert.ok(peekW(s) > 200, `grown (${peekW(s)})`);
  s.timers.advance(16);
  s.move(s.content, 690);
  assert.equal(s.peeking(), true, 'still peeking');
  assert.equal(peekW(s), PEEK_MIN_PX, 'shrunk back to the start width at once');
  s.timers.advance(PEEK_RETRACT_MS);
  assert.equal(s.peeking(), false, 'then it retracts');
  assert.equal(peekW(s), 0);
});

test('moving about near the panel does not peek: wiggling, drifting or vertical moves', async (t) => {
  for (const [why, run] of [
    ['wiggling', (s) => { for (let i = 0; i < 12; i++) { s.timers.advance(16); s.move(s.content, i % 2 ? 900 : 650); } }],
    ['a slow drift', (s) => s.flick({ x0: 630, x1: 900, steps: 50, stepMs: 16 })],
    ['a vertical move', (s) => s.flick({ x0: 700, x1: 700, y0: 120, y1: 580 })],
  ]) {
    const s = await shell(t);
    withPageScrollbar(s);
    run(s);
    assert.equal(s.peeking(), false, `${why}: no peek`);
    assert.equal(s.revealed(), false, `${why}: and no reveal`);
    s.timers.advance(2000);
    assert.equal(s.revealed(), false, `${why}: nor later`);
  }
  // With no near band (tcl-bg5qm1) a fast flick from there is judged like
  // any other: it peeks (and, with the growing peek on, crossing where the
  // panel's edge would be reveals it).
  let s = await shell(t);
  withPageScrollbar(s);
  s.flick({ x0: 640, x1: 920, steps: 5 });
  assert.equal(s.peeking(), true, 'a fast flick from 81px out of the panel’s edge peeks');
  assert.equal(s.revealed(), false, 'and with the growing peek off (the default), crossing does not reveal');
  s = await shell(t, { grow: true });
  withPageScrollbar(s);
  s.flick({ x0: 640, x1: 920, steps: 5 });
  assert.equal(s.revealed(), true, 'with it on, the crossing reveals');
});

test('the closer to the panel a motion starts, the less speed it needs; a slow drift still never peeks', async (t) => {
  // 0.875 px/ms from 161px out (just outside the band): well under the
  // 1.2 px/ms a fling from across the page needs.
  let s = await shell(t);
  withPageScrollbar(s);
  s.flick({ x0: 560, x1: 700, steps: 10, stepMs: 16 });
  assert.equal(s.peeking(), true, 'a short-runway fling peeks');
  // The same speed from across the page is not enough.
  s = await shell(t);
  withPageScrollbar(s);
  // It is judged from where it started all the way in: its last few moves,
  // near the panel, would pass on their own.
  s.flick({ x0: 100, x1: 702, steps: 43, stepMs: 16 });
  assert.equal(s.peeking(), false, 'the same speed from 600px+ out does not, even as it nears the panel');
  // A slow drift from just outside the band, towards the edge.
  s = await shell(t);
  withPageScrollbar(s);
  s.flick({ x0: 560, x1: 940, steps: 60, stepMs: 16 });
  assert.equal(s.peeking(), false, 'a slow drift near the edge does not');
  s.timers.advance(2000);
  assert.equal(s.revealed(), false);
});

test('the config switches and values take effect, and the form’s defaults are the dashboard’s', async (t) => {
  let s = await shell(t);
  withPageScrollbar(s);
  t.after(() => s.reveal.setSidePanelTuning(null, s.doc));
  s.flick(SHORT_FLING);
  assert.equal(s.peeking(), true);
  s.reveal.setSidePanelTuning({ predictive_peek: false }, s.doc);
  assert.equal(s.peeking(), false, 'turning the peek off takes it back');
  s.flick({ ...SHORT_FLING, y0: 310 });
  assert.equal(s.peeking(), false, 'and no more peeks');

  s = await shell(t);
  withPageScrollbar(s);
  s.reveal.setSidePanelTuning({ min_travel_px: 500 }, s.doc);
  s.flick(SHORT_FLING);
  assert.equal(s.peeking(), false, 'a longer minimum travel refuses a 400px fling');
  s.reveal.setSidePanelTuning({ peek_min_px: 80, growing_peek: false, edge_zone_px: 60 }, s.doc);
  s.flick({ ...SHORT_FLING, y0: 320 });
  assert.equal(peekW(s), 80, 'the peek’s width');
  s.timers.advance(16);
  s.move(s.content, WIDTH - SCROLLBAR - 85);
  s.timers.advance(REST);
  assert.equal(s.revealed() || s.peeking(), false, 'outside the wider zone, the peek runs out');
  s.flick({ ...SHORT_FLING, y0: 320 });
  s.timers.advance(16);
  s.move(s.content, WIDTH - SCROLLBAR - 55);
  s.timers.advance(REST);
  assert.equal(s.peeking(), true, 'inside the wider zone, it holds');
  assert.equal(s.revealed(), false, 'and opens nothing by itself');

  const { SIDE_PANEL_TUNING_FIELDS } = await s.harness.importDashboardModule('js/side-panel-tuning-fields.js');
  const defaults = Object.fromEntries(SIDE_PANEL_TUNING_FIELDS.map((f) => [f.key, f.def]));
  // Each field maps onto the constant it tunes, in that constant's unit.
  const map = {
    edge_zone_px: ['EDGE_PX', 1], retract_ms: ['PEEK_RETRACT_MS', 1],
    predict_min_speed: ['MIN_VX', 1000], predict_speed_distance_px: ['RUNWAY_FULL_PX', 1],
    predict_min_speed_floor: ['MIN_VX_FLOOR', 1000], predict_min_readings: ['MIN_SAMPLES', 1],
    min_travel_px: ['MIN_TRAVEL_PX', 1], brake_percent: ['BRAKE_RATIO', 100], peek_min_px: ['PEEK_MIN_PX', 1],
  };
  assert.deepEqual(Object.keys(map).sort(), Object.keys(defaults).sort(), 'every field is mapped');
  const { REVEAL_DEFAULTS } = s.reveal;
  for (const [key, [constant, scale]] of Object.entries(map)) {
    assert.equal(defaults[key] / scale, REVEAL_DEFAULTS[constant], `${key} defaults to ${constant}`);
    s.reveal.setSidePanelTuning({ [key]: defaults[key] + 7 }, s.doc);
    assert.equal(s.reveal.sidePanelTuning()[constant], (defaults[key] + 7) / scale, `${key} sets ${constant}`);
  }
  s.reveal.setSidePanelTuning({ predictive_peek: false, growing_peek: false }, s.doc);
  assert.equal(s.reveal.sidePanelTuning().PREDICTIVE, false);
  assert.equal(s.reveal.sidePanelTuning().GROWING, false);
  s.reveal.setSidePanelTuning(null, s.doc);
  assert.deepEqual({ ...s.reveal.sidePanelTuning() }, { ...REVEAL_DEFAULTS }, 'no tuning is the defaults');
});

test('a steady move that starts peeking and then brakes onto content under the would-be panel shrinks the peek back, with no reveal', async (t) => {
  const s = await shell(t, { grow: true });
  withPageScrollbar(s);
  const { PEEK_MIN_PX } = s.reveal.REVEAL_DEFAULTS;
  s.flick({ x0: 300, x1: 660, steps: 6 });
  assert.equal(s.peeking(), true, 'steady and aimed past FULL_LEFT: it peeks');
  assert.ok(peekW(s) > PEEK_MIN_PX, `and has grown (${peekW(s)})`);
  // Then it brakes into a button at about x=760, crossing FULL_LEFT on the way.
  let x = 660;
  for (const step of [36, 22, 14, 9, 6, 4, 2, 1]) {
    s.timers.advance(16);
    x += step;
    s.move(s.content, x);
    assert.equal(s.revealed(), false, `braking at ${x}: no reveal by crossing`);
  }
  assert.ok(x > FULL_LEFT, `it ended inside the would-be panel (${x})`);
  assert.equal(peekW(s), s.peeking() ? PEEK_MIN_PX : 0, 'the peek shrank back');
  s.timers.advance(2000);
  assert.equal(s.revealed(), false, 'and the stop there is no reveal');
  assert.equal(s.peeking(), false, 'the peek retracted');
});

test('a move crossing the open panel’s edge while braking a little keeps peeking rather than revealing', async (t) => {
  const s = await shell(t);
  withPageScrollbar(s);
  let x = 400;
  s.move(s.content, x);
  // Steady, then easing off to about 0.83 of its speed: enough to keep the
  // peek, not enough to reveal by crossing.
  for (const step of [60, 60, 60, 60, 45, 45]) {
    s.timers.advance(16);
    x += step;
    s.move(s.content, x);
  }
  assert.ok(x > FULL_LEFT, `crossed (${x})`);
  assert.equal(s.revealed(), false, 'no reveal by crossing');
  assert.equal(s.peeking(), true, 'but the peek holds');
  s.timers.advance(s.reveal.REVEAL_DEFAULTS.STOP_MS + 10);
  assert.equal(s.revealed(), false, 'nor by stopping there: the stop rule needs the same steady move');
  s.timers.advance(2000);
  assert.equal(s.revealed(), false);
});

test('with the growing peek off, the peek is v1’s: no growth, no reveal by crossing or stopping', async (t) => {
  const s = await shell(t);
  withPageScrollbar(s);
  s.reveal.setSidePanelTuning({ growing_peek: false }, s.doc);
  t.after(() => s.reveal.setSidePanelTuning(null, s.doc));
  s.flick(FLING);
  assert.equal(s.revealed(), false, 'crossing FULL_LEFT does not reveal');
  assert.equal(s.peeking(), true);
  assert.equal(peekW(s), 48, 'and the peek does not grow');
  s.timers.advance(s.reveal.REVEAL_DEFAULTS.STOP_MS + 10);
  assert.equal(s.revealed(), false, 'a stop inside the panel’s area does not reveal either');
  s.move(s.dock, WIDTH - SCROLLBAR - 20);
  assert.equal(s.revealed(), true, 'moving onto the strip still does');
});

test('the peek is never wider than the panel', async (t) => {
  const s = await shell(t);
  withPageScrollbar(s);
  s.reveal.setSidePanelTuning({ peek_min_px: 400 }, s.doc);
  t.after(() => s.reveal.setSidePanelTuning(null, s.doc));
  s.flick(SHORT_FLING);
  assert.equal(s.peeking(), true);
  assert.equal(peekW(s), PANEL_W, 'clamped to the panel’s width');
  s.reveal.setSidePanelTuning({ predictive_peek: false }, s.doc);
  assert.equal(peekW(s), 0, 'turning the peek off clears its width');
});


// A snap (tcl-bg5qm1): the operator's fast move from the left is over in a
// few frames, too few for the prediction's readings. An arrival that travelled far, fast, peeks the panel (it opens only
// from a peek): moving onto the peek opens it.
const snap = (s, xs, { from = 60, frameMs = 16, y = 300 } = {}) => {
  s.move(s.content, from, { clientY: y });
  s.timers.advance(500); // at rest
  for (const x of xs) { s.timers.advance(frameMs); s.move(s.content, x, { clientY: y }); }
};

test('a three-frame snap from the left into the edge zone peeks the panel, and a move onto the peek opens it', async (t) => {
  const { PEEK_RETRACT_MS } = (await (await createPreactHarness(t)).importDashboardModule('js/side-panel-predict.js')).PREDICT;
  let s = await shell(t);
  snap(s, [420, 860, 990]);
  assert.equal(s.peeking(), true, 'peeked on arrival');
  assert.equal(s.revealed(), false, 'not opened: the panel opens only from a peek');
  s.timers.advance(REST);
  assert.equal(s.peeking(), true, 'the peek holds while the pointer stays by the edge');
  // The strip slid in under a pointer already at the edge: the move that
  // comes with the layout change (same position) is no move onto it, the
  // next real one is.
  s.move(s.dock, 990);
  assert.equal(s.revealed(), false, 'the strip sliding under a still pointer opens nothing');
  s.move(s.dock, 993);
  assert.equal(s.revealed(), true, 'the next move onto it opens the panel');

  s = await shell(t);
  snap(s, [600, 990]);
  assert.equal(s.peeking(), true, 'a two-frame snap: the first frame counts from where the pointer rested');
  s.move(s.content, 900);
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking() || s.revealed(), false, 'moving away from the peek without opening it, it runs out');

  s = await shell(t);
  const handle = s.doc.getElementById('dock-toggle');
  handle.getBoundingClientRect = () => ({ top: 273, bottom: 327, height: 54, left: WIDTH - 22, right: WIDTH, width: 22 });
  snap(s, [420, 860]);
  s.timers.advance(16);
  s.move(handle, 990);
  assert.equal(s.revealed() || s.peeking(), false, 'a snap onto the folded handle does not peek or open it: the pointer is there to pin');
  s.timers.advance(REST * 4);
  assert.equal(s.revealed() || s.peeking(), false, 'nor does resting there');
});

// Cold review of #4109: the handle holds a peek, and what reached the handle
// does not reveal once the pointer moves off it.
test('a peek holds while the pointer rests on the handle, and nothing that reached the handle reveals later', async (t) => {
  const { STOP_MS, PEEK_RETRACT_MS } = (await (await createPreactHarness(t)).importDashboardModule('js/side-panel-predict.js')).PREDICT;
  let s = await shell(t);
  withPageScrollbar(s);
  s.flick(SHORT_FLING);
  assert.equal(s.peeking(), true);
  s.move(s.doc.getElementById('dock-toggle'), 720);
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking(), true, 'the peek stays under a pointer resting on its handle');
  s.move(s.content, 400);
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking(), false, 'moving off, the peek runs out');

  s = await shell(t, { grow: true });
  withPageScrollbar(s);
  s.flick({ x0: 300, x1: 715 });
  assert.equal(s.peeking(), true);
  s.timers.advance(4);
  s.move(s.doc.getElementById('dock-toggle'), 716);
  s.timers.advance(STOP_MS * 2);
  assert.equal(s.revealed(), false, 'a growing peek\'s stop does not reveal on the handle');

  s = await shell(t);
  snap(s, [420, 860]);
  s.timers.advance(16);
  s.move(s.doc.getElementById('dock-toggle'), 990);
  s.timers.advance(16);
  s.move(s.content, 990, { clientY: 340 });
  assert.equal(s.revealed() || s.peeking(), false, 'a snap blocked on the handle does not peek off it');

  s = await shell(t);
  snap(s, [420, 860]);
  s.move(s.doc.getElementById('dock-toggle'), 990);
  s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { clientX: 999, clientY: 300 });
  assert.equal(s.revealed() || s.peeking(), false, 'nor does one leaving the window through the handle');
});

// Cold review: a peek that holds by the edge always has a way to run out.
test('a snap\'s peek runs out away from the edge with the prediction off, and into a frame', async (t) => {
  const { PEEK_RETRACT_MS } = (await (await createPreactHarness(t)).importDashboardModule('js/side-panel-predict.js')).PREDICT;
  let s = await shell(t);
  s.reveal.setSidePanelTuning({ predictive_peek: false }, s.doc);
  t.after(() => s.reveal.setSidePanelTuning(null, s.doc));
  snap(s, [420, 860, 990]);
  assert.equal(s.peeking(), true, 'with the prediction off, a snap still peeks');
  s.move(s.content, 300);
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking(), false, 'and its peek runs out once the pointer moves away');

  s = await shell(t);
  snap(s, [420, 860, 990]);
  assert.equal(s.peeking(), true);
  const frame = s.doc.body.appendChild(s.doc.createElement('iframe'));
  s.harness.fireEvent(s.content, 'pointerout', { relatedTarget: frame });
  s.timers.advance(PEEK_RETRACT_MS * 2);
  assert.equal(s.peeking(), false, 'into a frame, where the page sees no more moves, it runs out too');
});

test('a snap that runs past the window’s edge peeks when its next frame would have reached the zone', async (t) => {
  let s = await shell(t);
  snap(s, [420, 860]);
  s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { clientX: 999, clientY: 300 });
  assert.equal(s.peeking(), true, 'heading off the right at snap speed');

  s = await shell(t);
  snap(s, [600, 700, 800, 900, 960], { from: 500, frameMs: 120 });
  s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { clientX: 999, clientY: 300 });
  assert.equal(s.revealed(), false, 'a slow motion leaving by the edge is not one');

  s = await shell(t);
  snap(s, [200, 340, 480]);
  s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { clientX: 480, clientY: 300 });
  assert.equal(s.revealed(), false, 'a fast motion leaving well short of the edge (its next frame would not reach the zone)');

  s = await shell(t);
  snap(s, [420, 860]);
  s.harness.fireEvent(s.doc.documentElement, 'pointerleave', { clientX: 999, clientY: 300, buttons: 1 });
  assert.equal(s.revealed(), false, 'a drag running off the edge, button held, is not one');
});

// Only a rest the page saw starts a snap (cold review of #4107): anything that
// interrupts the moves it sees starts the next motion where the pointer is.
test('a gap the page did not see is no rest: after a drag, a frame, a held button or Escape, a jump to the edge is no snap', async (t) => {
  for (const [why, run] of [
    ['an HTML5 drag dropped by the edge', (s) => {
      s.move(s.content, 300);
      s.harness.fireEvent(s.content, 'dragstart');
      s.timers.advance(1000);
      s.harness.fireEvent(s.content, 'drop');
    }],
    ['a pass through a frame', (s) => {
      const frame = s.doc.body.appendChild(s.doc.createElement('iframe'));
      s.move(s.content, 300);
      s.harness.fireEvent(s.content, 'pointerout', { relatedTarget: frame });
      s.timers.advance(2000);
    }],
    ['a selection dragged with the button held', (s) => {
      s.move(s.content, 300);
      for (const x of [500, 700, 900]) { s.timers.advance(16); s.move(s.content, x, { buttons: 1 }); }
    }],
    ['Escape on a peek mid-snap', (s) => {
      s.flick(FLING);
      assert.equal(s.peeking(), true);
      s.harness.fireEvent(s.doc.body, 'keydown', { key: 'Escape' });
    }],
  ]) {
    const s = await shell(t);
    run(s);
    s.timers.advance(16);
    s.atEdge();
    assert.equal(s.revealed() || s.peeking(), false, `${why}: no peek`);
    s.timers.advance(REST);
    assert.equal(s.revealed() || s.peeking(), false, `${why}: and resting there opens nothing`);
  }
});

test('a slow approach, or a short one from near the edge, opens nothing', async (t) => {
  for (const [why, run] of [
    ['a slow approach', (s) => snap(s, [300, 500, 700, 900, 990], { frameMs: 120 })],
    ['a short quick move from near the edge', (s) => snap(s, [880, 990], { from: 850 })],
  ]) {
    const s = await shell(t);
    run(s);
    assert.equal(s.revealed() || s.peeking(), false, `${why}: no peek`);
    s.timers.advance(REST);
    assert.equal(s.revealed() || s.peeking(), false, `${why}: and resting there opens nothing (tcl-bg5qm1)`);
  }
});

test('the prediction judges the horizontal motion only, with no band where nothing runs', async (t) => {
  let s = await shell(t);
  s.flick({ x0: 300, x1: 700, y0: 100, y1: 500 });
  assert.equal(s.peeking(), true, 'a diagonal fling right peeks: only its rightward speed counts');

  s = await shell(t);
  withPageScrollbar(s);
  s.flick({ x0: 640, x1: 740, steps: 5, stepMs: 16 });
  assert.equal(s.revealed() || s.peeking(), true, 'a fast move from 80px left of the panel’s edge is judged, not refused by a band');

  s = await shell(t);
  withPageScrollbar(s);
  s.flick({ x0: 900, x1: 950, steps: 5, stepMs: 10 });
  assert.equal(s.peeking() || s.revealed(), false, 'a short move near the edge travels too little');
});

// tcl-xxci6h: a modal or dialog open above the page freezes the reveal, as
// leaving the window does. An action of the revealed panel's that asks to
// confirm (dragging an agent to another group) puts the pointer on the modal,
// not on the panel, and the panel must not go from under it. Tracking resumes
// with the first move after the last modal closes.
test('a modal open above the page freezes the revealed panel until the next move after it closes', async (t) => {
  // modal opens a dashboard-style modal: an overlay showing, with an
  // aria-modal dialog in it. It returns the overlay, to close it.
  const modal = (s) => {
    const overlay = s.doc.body.appendChild(s.doc.createElement('div'));
    overlay.className = 'modal-overlay show';
    const dialog = overlay.appendChild(s.doc.createElement('div'));
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');
    return overlay;
  };
  const offPanel = (s) => s.dock.dispatchEvent(new s.harness.window.Event('pointerleave', { bubbles: false }));

  await t.test('a confirm opened from the panel keeps it; after it closes the hover-out delay starts at the next move', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
    const confirm = modal(s);
    offPanel(s);
    s.move(confirm.firstChild, WIDTH - 100);
    s.timers.advance(s.hoverClose * 3);
    assert.equal(s.revealed(), true, 'under the confirm the panel stays');
    confirm.classList.remove('show');
    s.timers.advance(s.hoverClose * 3);
    assert.equal(s.revealed(), true, 'closing it decides nothing before the pointer moves');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose - 1);
    assert.equal(s.revealed(), true, 'back on the page, the delay starts from that move');
    s.timers.advance(1);
    assert.equal(s.revealed(), false, 'and then it goes');
  });

  await t.test('back on the panel after the modal, it stays', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    const confirm = modal(s);
    offPanel(s);
    s.move(confirm.firstChild, WIDTH - 100);
    confirm.remove();
    s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
    s.timers.advance(s.hoverClose * 3);
    assert.equal(s.revealed(), true);
  });

  await t.test('nested modals: frozen until the last one closes', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    const outer = modal(s);
    const inner = s.doc.body.appendChild(s.doc.createElement('dialog'));
    inner.setAttribute('open', '');
    offPanel(s);
    s.move(inner, 300);
    outer.classList.remove('show');
    s.move(inner, 310);
    s.timers.advance(s.hoverClose * 3);
    assert.equal(s.revealed(), true, 'one still open holds it');
    inner.removeAttribute('open');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'the last one closed, the page move lets it go');
  });

  await t.test('a hide already pending when the modal opens does not act under it', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    s.move(s.content, 300);
    const confirm = modal(s);
    s.timers.advance(s.hoverClose * 3);
    assert.equal(s.revealed(), true);
    confirm.remove();
    s.move(s.content, 310);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false);
  });

  await t.test('Escape under a modal is the modal\'s, not the panel\'s', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    modal(s);
    s.harness.fireEvent(s.doc, 'keydown', { key: 'Escape' });
    assert.equal(s.revealed(), true);
  });

  await t.test('a closed dialog kept mounted, or one not laid out, freezes nothing', async (t) => {
    const s = await shell(t);
    s.openAtEdge();
    const kept = s.doc.body.appendChild(s.doc.createElement('div'));
    kept.className = 'modal-overlay';
    const dialog = kept.appendChild(s.doc.createElement('div'));
    dialog.setAttribute('aria-modal', 'true');
    dialog.getClientRects = () => [];
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false);
  });
});
