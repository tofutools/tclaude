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
    // No pointer events fire during an HTML5 drag: a dwell under way stops at
    // its start, and dragging over the edge zone is not a reveal.
    ['an HTML5 drag under way', (s) => { s.atEdge(); s.harness.fireEvent(s.content, 'dragstart');
      s.harness.fireEvent(s.content, 'dragover', { clientX: WIDTH - 4, clientY: 300 }); }],
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
  await t.test('but not the focus a click leaves on a button', async (t) => {
    const s = await shell(t);
    s.restAtEdge();
    s.menuButton.focus();
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false, 'a clicked button keeps focus in Chrome; that does not hold the panel');
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
    s.restAtEdge();
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

// The review's finds: each was a way to get stuck or to slide in at the wrong
// moment, and each passed the pins above.

test('closed menus the page keeps in the DOM do not hold it', async (t) => {
  const s = await shell(t);
  // Hidden by a class, not the hidden attribute, like .action-menu:not(.open).
  const closed = s.doc.body.appendChild(s.doc.createElement('div'));
  closed.className = 'ui-menu action-menu';
  closed.setAttribute('role', 'menu');
  closed.getClientRects = () => [];
  s.restAtEdge();
  s.move(s.content, 300);
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'it still goes');
  s.restAtEdge();
  s.harness.fireEvent(s.doc.body, 'keydown', { key: 'Escape' });
  assert.equal(s.revealed(), false, 'and Escape still works');
});

test('a drag whose dragend never arrives does not leave the reveal dead', async (t) => {
  const s = await shell(t);
  // A drop that re-renders the dragged group header: no dragend reaches the
  // document. The next pointer move proves the drag is over.
  s.harness.fireEvent(s.content, 'dragstart');
  s.restAtEdge();
  assert.equal(s.revealed(), true, 'the next pointer move ends the drag, so resting at the edge reveals again');
  await t.test('nor holds a panel it was dragged from', async (t) => {
    const s = await shell(t);
    s.restAtEdge();
    s.harness.fireEvent(s.doc.getElementById('dock-body'), 'dragstart');
    s.move(s.content, 300);
    s.timers.advance(s.hoverClose);
    assert.equal(s.revealed(), false);
  });
});

test('resting on the folded handle does not slide the panel in under the click', async (t) => {
  const s = await shell(t);
  s.move(s.doc.getElementById('dock-toggle'), WIDTH - 4);
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS * 4);
  assert.equal(s.revealed(), false);
  s.harness.fireEvent(s.doc.getElementById('dock-toggle'), 'click');
  assert.equal(s.side.isSidePanelOpen('groups'), true, 'the click pins it, as aimed');
});

test('a reveal does not carry over from one tab\'s panel to another\'s', async (t) => {
  const s = await shell(t);
  s.side.offerSidePanel('collab', true);
  s.restAtEdge();
  assert.equal(s.revealed(), true);
  s.activate('tab-terminals');
  assert.equal(s.revealed(), false, 'the Collab panel does not appear already floating');
});

test('leaving the panel straight into a frame still sends it away', async (t) => {
  const s = await shell(t);
  s.restAtEdge();
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
  s.move(s.content, WIDTH - 4, { clientY: 50 });
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
  assert.equal(s.revealed(), false, 'above the panel (the top bar) is not the edge zone');
  s.move(s.content, WIDTH - 4, { clientY: 300 });
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
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
  reveal.bindEdgeReveal(doc, { timers, finePointer: () => true });
  side.applySidePanel(doc);
  const main = doc.getElementById('tab-groups');
  const move = (target, clientX) => harness.fireEvent(target, 'pointermove', { clientX, clientY: 300, buttons: 0, pointerType: 'mouse' });

  move(main, WIDTH - 4);
  timers.advance(reveal.EDGE_REVEAL_DWELL_MS);
  assert.ok(doc.body.classList.contains('dock-revealed'), 'it floats in');
  move(doc.getElementById('dock-body'), WIDTH - 100);
  move(main, 300);
  timers.advance(watch.HOVER_CLOSE_MS);
  assert.ok(!doc.body.classList.contains('dock-revealed'), 'and goes after the hover-out delay');
  move(main, WIDTH - 4);
  timers.advance(reveal.EDGE_REVEAL_DWELL_MS);
  harness.fireEvent(doc.body, 'keydown', { key: 'Escape' });
  assert.ok(!doc.body.classList.contains('dock-revealed'), 'Escape sends it away');
});

// tcl-vv7jn8: the page's own vertical scrollbar sits at the right edge,
// right of the fixed panel. A pointer on it targets <html>, not the panel.
const SCROLLBAR = 15;
const PANEL_W = 264;

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

test('the pointer on the page scrollbar, under the edge zone, reveals the panel and does not flicker it', async (t) => {
  const s = await shell(t);
  const { onScrollbar } = withPageScrollbar(s);
  onScrollbar();
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
  assert.equal(s.revealed(), true, 'resting on the scrollbar strip is resting at the edge');
  // The loop was: on the scrollbar, not "over" the panel, so it went after the
  // hover-out delay, and the dwell brought it straight back.
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
  s.move(s.content, right - s.reveal.EDGE_REVEAL_PX - 1);
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
  assert.equal(s.revealed(), false, 'just outside');
  s.move(s.content, right - s.reveal.EDGE_REVEAL_PX);
  s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
  assert.equal(s.revealed(), true, 'at the zone\'s inner edge');
});

test('dragging the panel\'s own scrollbar, or the page scrollbar beside it, does not retract it', async (t) => {
  await t.test('its own scrollbar, dragged out past its left edge', async (t) => {
    const s = await shell(t);
    withPageScrollbar(s);
    s.restAtEdge();
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
    onScrollbar();
    s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
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
  s.restAtEdge();
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
    s.move(s.content, WIDTH - s.reveal.EDGE_REVEAL_PX - 1);
    s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
    assert.equal(s.revealed(), false, 'just outside the zone');
    s.move(s.content, WIDTH - 5);
    s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
    assert.equal(s.revealed(), true, 'the same zone reveals');
    // The panel now reaches the window edge, so the pointer is on it.
    await steady(s, (init) => s.move(s.doc.getElementById('dock-body'), WIDTH - 5, init), 'no scrollbar');
  });
  await t.test('an overlay scrollbar: no width, but a pointer on it targets <html>', async (t) => {
    const s = await shell(t);
    const { onScrollbar } = withPageScrollbar(s, 0);
    onScrollbar();
    s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
    assert.equal(s.revealed(), true);
    await steady(s, onScrollbar, 'overlay scrollbar');
  });
  await t.test('a scrollbar that appears, then goes, while the panel floats', async (t) => {
    const s = await shell(t);
    const { onScrollbar, setWidth } = withPageScrollbar(s, 0);
    s.move(s.content, WIDTH - 5);
    s.timers.advance(s.reveal.EDGE_REVEAL_DWELL_MS);
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
  s.restAtEdge();
  const dialog = s.doc.body.appendChild(s.doc.createElement('div'));
  s.move(s.doc.getElementById('dock-body'), WIDTH - 100);
  s.move(dialog, WIDTH - 100);
  s.timers.advance(s.hoverClose);
  assert.equal(s.revealed(), false, 'over something else in its band, it goes after the delay');
});
