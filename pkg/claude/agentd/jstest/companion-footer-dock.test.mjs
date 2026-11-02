// The Steward's chips resting at footer-right and the overlay dock's pills beside
// them never overlap, whichever lands in the footer first. Parking the chips
// makes the dock keep their room, which moves its pills by restyling the dock;
// the chips must measure the bar again after that, not keep the room they saw
// with the pills where they were (an operator report: they overlapped until
// hovered).
//
// linkedom has no layout, so the footer is laid out here the way dashboard.css
// lays it out: Open PRs at the right end, and the dock filling the rest, its
// buttons from its right edge leftwards (row-reverse) inside its padding.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const BAR = { left: 0, right: 1000, top: 700, bottom: 729 };
const OPEN_PRS = { left: 900, right: 1000 };
const DOCK = { left: 0, right: 890 };
const GAP = 6;
const CHIP_WIDTH = 120;
const WIDTHS = { 'overlay-dock-menu': 30, 'overlay-dock-more': 30, 'overlay-dock-pill': 100 };

const box = (left, right) => ({ left, right, top: BAR.top + 3, bottom: BAR.bottom - 3, width: right - left, height: BAR.bottom - BAR.top - 6, x: left, y: BAR.top + 3 });
const px = (style, prop) => Number(new RegExp(`${prop}:\\s*(-?[\\d.]+)px`).exec(style || '')?.[1] || 0);
const kindOf = (node) => Object.keys(WIDTHS).find((cls) => node.classList?.contains(cls));

function layOut(node) {
  const cls = node.classList;
  if (cls?.contains('footer-inner')) return box(BAR.left, BAR.right);
  if (cls?.contains('open-prs-button')) return box(OPEN_PRS.left, OPEN_PRS.right);
  if (cls?.contains('companion-chips')) {
    const left = px(node.getAttribute('style'), 'left');
    return box(left, left + CHIP_WIDTH);
  }
  if (cls?.contains('overlay-dock') && cls.contains('in-footer')) return box(DOCK.left, DOCK.right);
  const item = node.closest?.('.overlay-dock-menu, .overlay-dock-more, .overlay-dock-pill');
  if (!item) return box(0, 0);
  const parent = item.parentElement;
  if (parent.classList.contains('overlay-dock-measure')) return box(-10000, -10000 + WIDTHS[kindOf(item)]);
  // row-reverse: the first child sits at the right end, inside the padding.
  let right = DOCK.right - px(parent.getAttribute('style'), 'padding-right');
  for (const child of parent.children) {
    const width = WIDTHS[kindOf(child)];
    if (!width) continue;
    if (child === item) return box(right - width, right);
    right -= width + GAP;
  }
  return box(0, 0);
}

const START = { id: 'companion-start', tabs: [{ id: 'companion-start', kind: 'start', label: 'Steward' }], active: 'companion-start', minimized: false, place: 'p.start' };
const TERMINAL = { id: 'terminal:/api/term-ws/abc', tabs: [{ id: 'terminal:/api/term-ws/abc', kind: 'terminal', paneKey: '/api/term-ws/abc', ws: '/api/term-ws/abc', agent: '', label: 'repo shell' }], active: 'terminal:/api/term-ws/abc', minimized: false, place: 'p.term' };

// linkedom hands an observer attribute records it never asked for; a browser
// doesn't, and that is what hid the overlap. Keep only the records the options
// ask for, as a browser does. (linkedom's window hands out its own class, so the
// class is patched rather than replaced.)
function strictMutationObserver(MutationObserver) {
  const { observe } = MutationObserver.prototype;
  MutationObserver.prototype.observe = function strictObserve(target, options = {}) {
    if (!this.strict) {
      const callback = this.callback;
      this.strict = true;
      this.callback = (records, observer) => {
        const kept = records.filter((r) => r.type !== 'attributes' || observer.wants?.(r.attributeName));
        if (kept.length) callback(kept, observer);
      };
    }
    this.wants = (name) => !!(options.attributes || options.attributeFilter) && (!options.attributeFilter || options.attributeFilter.includes(name));
    return observe.call(this, target, options);
  };
  return () => { MutationObserver.prototype.observe = observe; };
}

function memoryPrefs(initial = {}) {
  const store = new Map(Object.entries(initial));
  return { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)) };
}

async function setup(t) {
  const harness = await createPreactHarness(t);
  const { window } = harness;
  const { document } = window;
  t.after(strictMutationObserver(window.MutationObserver));
  const { getBoundingClientRect } = window.HTMLElement.prototype;
  window.HTMLElement.prototype.getBoundingClientRect = function laidOut() { return layOut(this); };
  t.after(() => { window.HTMLElement.prototype.getBoundingClientRect = getBoundingClientRect; });
  document.createRange = () => ({ selectNodeContents() {}, getClientRects: () => [] });
  const footer = document.body.appendChild(document.createElement('footer'));
  footer.innerHTML = '<div class="footer-inner"><div class="dock-host"></div><button type="button" class="open-prs-button" data-ui-exempt>Open PRs</button></div>';
  const instances = await harness.importDashboardModule('js/overlay-instances.js');
  const { CompanionApp } = await harness.importDashboardModule('js/companion-island.js');
  const { FooterDock } = await harness.importDashboardModule('js/overlay-dock.js');
  instances.overlayWindows.value = [START, TERMINAL];
  const snapshot = harness.signals.signal({ groups: [] });
  const activeTab = harness.signals.signal('groups');
  const view = await harness.mount(harness.html`<${CompanionApp} snapshot=${snapshot} activeTab=${activeTab} prefs=${memoryPrefs({ 'tclaude.dash.companion.chip-spot': 'footer-right' })}
    sessionPrefs=${memoryPrefs()} widgetFactory=${() => ({ connect: async () => {}, dispose() {}, setActive() {}, focus() {} })}
    registerController=${() => () => {}} />`);
  await harness.mount(harness.html`<${FooterDock} snapshot=${snapshot} win=${window} />`, footer.querySelector('.dock-host'));
  // Observers and the renders they ask for run after the act that set them off.
  const settle = async () => {
    for (let i = 0; i < 5; i++) await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  };
  const minimize = async (id) => {
    await harness.act(() => {
      instances.overlayWindows.value = instances.overlayWindows.value.map((w) => (w.id === id ? { ...w, minimized: true } : w));
    });
    await settle();
  };
  t.after(() => { instances.overlayWindows.value = []; });
  const row = () => view.container.querySelector('.companion-chips');
  const dockButtons = () => [...footer.querySelectorAll('.overlay-dock.in-footer > :is(.overlay-dock-menu, .overlay-dock-pill, .overlay-dock-more)')];
  return { row, dockButtons, minimize, settle };
}

function assertSideBySide({ row, dockButtons }) {
  const chips = row();
  assert.ok(chips, 'the chips show');
  assert.match(chips.className, /compact/, 'the chips rest in the footer');
  const at = chips.getBoundingClientRect();
  assert.ok(at.left >= DOCK.left && at.right <= OPEN_PRS.left, `the chips (${at.left}-${at.right}) sit left of Open PRs`);
  const items = dockButtons();
  assert.ok(items.some((node) => node.classList.contains('overlay-dock-pill')), 'the terminal has a pill');
  for (const node of items) {
    const rect = node.getBoundingClientRect();
    assert.ok(rect.right <= at.left || rect.left >= at.right, `${node.className} (${rect.left}-${rect.right}) overlaps the chips (${at.left}-${at.right})`);
    assert.ok(rect.right <= at.left, `${node.className} sits left of the chips`);
  }
}

test('a terminal minimized before the Steward: its pill and the chips sit side by side', async (t) => {
  const s = await setup(t);
  await s.settle();
  await s.minimize(TERMINAL.id);
  assert.equal(s.row(), null, 'the Steward still shows, so no chips yet');
  await s.minimize(START.id);
  assertSideBySide(s);
});

test('the Steward minimized before a terminal: the chips and its pill sit side by side', async (t) => {
  const s = await setup(t);
  await s.settle();
  await s.minimize(START.id);
  await s.minimize(TERMINAL.id);
  assertSideBySide(s);
});
