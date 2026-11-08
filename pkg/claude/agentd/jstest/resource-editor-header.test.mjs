import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The resource editors' shared header (tk_034e2afc): identity, a File · Edit ·
// View · Share menu bar, and the people at the right.

const wait = (ms = 0) => new Promise((resolve) => setTimeout(resolve, ms));

async function mountHeader(t, props) {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/resource-editor-header.js');
  const view = await harness.mount(harness.html`<${mod.ResourceEditorHeader} ...${props(mod, harness)} />`);
  const c = view.container;
  const label = (key) => c.querySelector(`[data-editor-menu="${key}"]`);
  const items = () => [...c.querySelectorAll('.reh-menu [role^="menuitem"]')].map((b) => b.querySelector('.reh-menu-label')?.textContent.trim());
  const open = async (key) => harness.act(async () => { harness.fireEvent(label(key), 'click'); await wait(); });
  return { harness, mod, c, label, items, open };
}

test('a menu with nothing in it is left out, and separators and headings with nothing under them are dropped', async (t) => {
  const ran = [];
  const { c, items, open, label } = await mountHeader(t, () => ({
    title: 'Plan',
    menus: [
      { key: 'file', label: 'File', items: [{ heading: 'Export' }, { key: 'dl', label: 'Download', run: () => ran.push('dl') }, { sep: true }, { heading: 'Nothing' }, { sep: true }] },
      { key: 'edit', label: 'Edit', items: [] },
      { key: 'view', label: 'View', items: () => [{ sep: true }, { key: 'fit', label: 'Zoom to fit', shortcut: '⇧1', run: () => ran.push('fit') }] },
    ],
  }));
  assert.deepEqual([...c.querySelectorAll('.reh-menubar-label')].map((b) => b.textContent), ['File', 'View'], 'Edit has nothing and is left out');
  await open('file');
  assert.deepEqual(items(), ['Download']);
  assert.deepEqual([...c.querySelectorAll('.reh-menu-heading')].map((h) => h.textContent), ['Export']);
  assert.equal(c.querySelectorAll('.reh-menu .ui-menu-sep').length, 0, 'no separator dangles at the end');
  assert.equal(label('file').getAttribute('aria-expanded'), 'true');
  await open('view');
  assert.deepEqual(items(), ['Zoom to fit'], 'one menu open at a time; a leading separator is dropped');
  assert.equal(c.querySelector('.reh-menu-key').textContent, '⇧1');
});

test('picking an item closes the menu and runs it; a children item opens them in its place with a way back', async (t) => {
  const ran = [];
  const { harness, c, items, open } = await mountHeader(t, () => ({
    title: 'Plan',
    menus: [{ key: 'share', label: 'Share', items: [
      { key: 'invite', label: 'Invite agent', children: () => [{ key: 'a', label: 'alpha', run: () => ran.push('alpha') }] },
    ] }],
  }));
  await open('share');
  const click = (text) => harness.act(async () => {
    harness.fireEvent([...c.querySelectorAll('.reh-menu [role="menuitem"]')].find((b) => b.textContent.includes(text)), 'click');
    await wait();
  });
  await click('Invite agent');
  assert.deepEqual(items(), ['‹ Back', 'alpha']);
  await click('Back');
  assert.deepEqual(items(), ['Invite agent ›']);
  await click('Invite agent');
  await click('alpha');
  assert.deepEqual(ran, ['alpha']);
  assert.equal(c.querySelector('.reh-menu'), null, 'the menu closed');
});

test('the menu bar moves with the arrow keys, and Escape closes and goes back to the label', async (t) => {
  const { harness, c, label, items, open } = await mountHeader(t, () => ({
    title: 'Plan',
    menus: [
      { key: 'file', label: 'File', items: [{ key: 'a', label: 'A', run() {} }] },
      { key: 'view', label: 'View', items: [{ key: 'b', label: 'B', run() {} }, { key: 'c', label: 'C', run() {} }] },
    ],
  }));
  await open('file');
  await harness.act(async () => { harness.fireEvent(label('file'), 'keydown', { key: 'ArrowRight' }); await wait(); });
  assert.deepEqual(items(), ['B', 'C'], 'with a menu open, → opens the next one');
  assert.equal(harness.document.activeElement, label('view'));
  await harness.act(async () => { harness.fireEvent(label('view'), 'keydown', { key: 'ArrowDown' }); await wait(5); });
  assert.equal(harness.document.activeElement.textContent.trim(), 'B', '↓ goes into the menu');
  await harness.act(async () => { harness.fireEvent(harness.document.activeElement, 'keydown', { key: 'ArrowDown' }); await wait(); });
  assert.equal(harness.document.activeElement.textContent.trim(), 'C');
  await harness.act(async () => { harness.fireEvent(harness.document.activeElement, 'keydown', { key: 'Escape' }); await wait(); });
  assert.equal(c.querySelector('.reh-menu'), null);
  assert.equal(harness.document.activeElement, label('view'));
});

test('editorMoveItems words every host\'s moves the same, and leaves out where it cannot go', async (t) => {
  const harness = await createPreactHarness(t);
  const { editorMoveItems } = await harness.importDashboardModule('js/resource-editor-header.js');
  const go = () => {};
  assert.deepEqual(editorMoveItems({ what: 'this drawing board', toOverlay: go, toWindow: go }).map((i) => [i.label, i.title]), [
    ['Open in overlay', 'Float this drawing board in a window over the dashboard'],
    ['Open in separate window', 'Move this drawing board to a browser window of its own'],
  ]);
  assert.deepEqual(editorMoveItems({ toCollab: go }).map((i) => i.label), ['Open in the Collab tab']);
  assert.deepEqual(editorMoveItems({}), []);
});

test('the people stack names everyone, shows a few, and opens what it is given', async (t) => {
  let opened = 0;
  const people = ['alpha', 'beta', 'gamma', 'delta', 'eps'].map((name, i) => ({ id: `a${i}`, name, offline: i === 4, state: i === 1 ? 'drawing' : '' }));
  const { harness, c } = await mountHeader(t, (mod, h) => ({
    title: 'Plan', menus: [],
    people: h.html`<${mod.PeopleStack} people=${people} max=${3} onClick=${() => { opened += 1; }} />`,
  }));
  const stack = c.querySelector('.reh-stack');
  assert.match(stack.getAttribute('title'), /alpha, beta, gamma, delta, eps/);
  assert.deepEqual([...stack.querySelectorAll('.reh-avatar')].map((a) => a.textContent), ['A', 'B', 'G']);
  assert.ok(stack.querySelector('.reh-avatar.drawing'));
  assert.equal(stack.querySelector('.reh-stack-count').textContent, '+2');
  harness.fireEvent(stack, 'click');
  assert.equal(opened, 1);
});

test('a control in a menu keeps its own keys: arrows stay in it, and its Escape closes only its own list', async (t) => {
  let listOpen = true;
  const { harness, c, open } = await mountHeader(t, (mod, h) => ({
    title: 'Plan',
    menus: [{ key: 'share', label: 'Share', items: [
      { key: 'invite', label: 'Invite agent', run() {} },
      { key: 'picker', node: h.html`<input class="picker" onKeyDown=${(event) => {
        if (event.key === 'Escape' && listOpen) { event.preventDefault(); event.stopPropagation(); listOpen = false; }
      }} />` },
    ] }],
  }));
  await open('share');
  const input = () => c.querySelector('.picker');
  input().focus();
  await harness.act(async () => { harness.fireEvent(input(), 'keydown', { key: 'ArrowDown' }); await wait(); });
  assert.equal(harness.document.activeElement, input(), 'the arrow is the picker\'s');
  await harness.act(async () => { harness.fireEvent(input(), 'keydown', { key: 'Escape' }); await wait(); });
  assert.ok(c.querySelector('.reh-menu'), 'the picker closed its list; the menu stays');
  await harness.act(async () => { harness.fireEvent(input(), 'keydown', { key: 'Escape' }); await wait(); });
  assert.equal(c.querySelector('.reh-menu'), null, 'a second Escape closes the menu');
});

test('the people stack toggles Share: a click while it is open closes it', async (t) => {
  const { harness, c } = await mountHeader(t, () => ({ title: 'Plan', menus: [] }));
  const mod = await harness.importDashboardModule('js/resource-editor-header.js');
  function Host() {
    const [open, setOpen] = harness.hooks.useState('');
    return harness.html`<${mod.ResourceEditorHeader} title="Plan" openMenu=${open} onOpenMenu=${setOpen}
      menus=${[{ key: 'share', label: 'Share', items: [{ key: 'x', label: 'X', run() {} }] }]}
      people=${harness.html`<${mod.PeopleStack} people=${[{ id: 'a', name: 'alpha' }]} onClick=${() => setOpen((o) => (o === 'share' ? '' : 'share'))} />`} />`;
  }
  const view = await harness.mount(harness.html`<${Host} />`);
  const stack = view.container.querySelector('.reh-stack');
  const press = async () => harness.act(async () => {
    harness.fireEvent(stack, 'pointerdown', { button: 0 });
    harness.fireEvent(stack, 'click');
    await wait();
  });
  await press();
  assert.ok(view.container.querySelector('.reh-menu'), 'opens Share');
  await press();
  assert.equal(view.container.querySelector('.reh-menu'), null, 'and closes it again');
  void c;
});
