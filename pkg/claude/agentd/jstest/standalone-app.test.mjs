// js/standalone-app.js: the dashboard installed as an app. A link to another
// site opens in the browser, and a pull down on the phone's header reloads
// (Chrome's own pull-to-refresh is off in a standalone window). In a browser
// tab neither is installed: the browser's own behaviour stays.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// appWindow is the harness's window, as the installed app (standalone) or a
// browser tab, with window.open and reload recorded.
function appWindow(harness, standalone) {
  const opened = [];
  const win = {
    document: harness.document,
    location: { origin: 'https://dash.example', reload: () => opened.push(['reload']) },
    matchMedia: (query) => ({ matches: standalone && query === '(display-mode: standalone)' }),
    navigator: {},
    open: (...args) => { opened.push(args); return null; },
  };
  return { win, opened };
}

function click(harness, element, init = {}) {
  const event = new harness.window.Event('click', { bubbles: true, cancelable: true });
  Object.assign(event, { button: 0, ctrlKey: false, metaKey: false, shiftKey: false, altKey: false }, init);
  element.dispatchEvent(event);
  return event;
}

function touch(harness, type, target, x, y) {
  const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
  const point = { clientX: x, clientY: y, target };
  event.touches = type === 'touchend' ? [] : [point];
  target.dispatchEvent(event);
}

test('isStandalone: the display-mode media feature, or iOS\'s home-screen flag', async (t) => {
  const harness = await createPreactHarness(t);
  const { isStandalone } = await harness.importDashboardModule('js/standalone-app.js');
  assert.equal(isStandalone(appWindow(harness, true).win), true);
  assert.equal(isStandalone(appWindow(harness, false).win), false);
  assert.equal(isStandalone({ matchMedia: () => ({ matches: false }), navigator: { standalone: true } }), true);
  assert.equal(isStandalone({}), false);
});

test('installed as an app, a link to another site opens in the browser; the dashboard\'s own links stay', async (t) => {
  const harness = await createPreactHarness(t);
  const { installExternalLinks } = await harness.importDashboardModule('js/standalone-app.js');
  const { win, opened } = appWindow(harness, true);
  const uninstall = installExternalLinks(win);
  t.after(uninstall);
  harness.document.body.innerHTML = `
    <a id="pr" href="https://github.com/acme/web/pull/1">PR</a>
    <a id="claude" href="https://claude.ai/code/x"><span id="inner">deep</span></a>
    <a id="own" href="https://dash.example/groups">groups</a>
    <a id="blank" href="https://github.com/acme" target="_blank">new window</a>
    <a id="mail" href="mailto:x@example.com">mail</a>
    <a id="file" href="https://example.com/a.zip" download>zip</a>`;
  // jsdom resolves relative hrefs against its own URL; these are absolute.
  const $ = (id) => harness.document.getElementById(id);
  // Navigation itself is jsdom's to (not) do; the test reads what was stopped.
  const stopped = (id, init) => click(harness, $(id), init).defaultPrevented;

  assert.equal(stopped('pr'), true);
  assert.equal(stopped('inner'), true, 'a click inside the link');
  assert.deepEqual(opened, [
    ['https://github.com/acme/web/pull/1', '_blank', 'noopener'],
    ['https://claude.ai/code/x', '_blank', 'noopener'],
  ]);
  opened.length = 0;
  for (const id of ['own', 'blank', 'mail', 'file']) assert.equal(stopped(id), false, `${id} is left alone`);
  assert.equal(stopped('pr', { ctrlKey: true }), false, 'a modified click is the browser\'s');
  assert.deepEqual(opened, []);
});

test('in a browser tab nothing is installed', async (t) => {
  const harness = await createPreactHarness(t);
  const { installExternalLinks, installHeaderPull } = await harness.importDashboardModule('js/standalone-app.js');
  const { win, opened } = appWindow(harness, false);
  installExternalLinks(win)();
  installHeaderPull({ win })();
  harness.document.body.innerHTML = '<header class="phone-bar"></header><a id="pr" href="https://github.com/x">PR</a>';
  assert.equal(click(harness, harness.document.getElementById('pr')).defaultPrevented, false);
  assert.deepEqual(opened, []);
});

test('installed as an app, a pull down on the header reloads once it went far enough', async (t) => {
  const harness = await createPreactHarness(t);
  const { installHeaderPull, PULL_RELOAD_PX } = await harness.importDashboardModule('js/standalone-app.js');
  const { win } = appWindow(harness, true);
  let reloads = 0;
  const uninstall = installHeaderPull({ win, reload: () => { reloads += 1; } });
  t.after(uninstall);
  harness.document.body.innerHTML = '<header class="phone-bar"><h1 id="title">Now</h1></header><main id="main"></main>';
  const title = harness.document.getElementById('title');
  const hint = () => harness.document.querySelector('.phone-pull');

  touch(harness, 'touchstart', title, 100, 20);
  touch(harness, 'touchmove', title, 100, 60);
  assert.equal(hint().textContent, '↓ Pull to reload');
  assert.equal(hint().classList.contains('ready'), false);
  touch(harness, 'touchend', title);
  assert.equal(reloads, 0, 'not far enough');
  assert.equal(hint(), null, 'the hint goes with the finger');

  touch(harness, 'touchstart', title, 100, 20);
  touch(harness, 'touchmove', title, 104, 20 + PULL_RELOAD_PX + 5);
  assert.equal(hint().textContent, '↻ Release to reload');
  assert.equal(hint().classList.contains('ready'), true);
  touch(harness, 'touchend', title);
  assert.equal(reloads, 1);

  // Pulled far, then back up: nothing.
  touch(harness, 'touchstart', title, 100, 20);
  touch(harness, 'touchmove', title, 100, 20 + PULL_RELOAD_PX + 5);
  touch(harness, 'touchmove', title, 100, 30);
  touch(harness, 'touchend', title);
  assert.equal(reloads, 1, 'a pull taken back does not reload');

  // A swipe sideways on the header is not a pull.
  touch(harness, 'touchstart', title, 100, 20);
  touch(harness, 'touchmove', title, 220, 60);
  touch(harness, 'touchmove', title, 220, 20 + PULL_RELOAD_PX + 5);
  touch(harness, 'touchend', title);
  assert.equal(reloads, 1, 'sideways first');

  // Off the header (the page's own scroll) is not a pull.
  const main = harness.document.getElementById('main');
  touch(harness, 'touchstart', main, 100, 200);
  touch(harness, 'touchmove', main, 100, 200 + PULL_RELOAD_PX + 5);
  touch(harness, 'touchend', main);
  assert.equal(reloads, 1);
});
