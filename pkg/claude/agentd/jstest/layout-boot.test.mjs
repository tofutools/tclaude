// js/layout-boot.js: the once-per-load phone/desktop decision. Desktop mode
// must leave the page exactly as it was; phone mode marks <html>, adds the
// viewport meta and links phone.css; ?layout= overrides and is remembered per
// browser.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../dashboard/js/layout-boot.js', import.meta.url), 'utf8');

const IPHONE = { coarse: true, screen: { width: 390, height: 844 } };
const IPAD_MINI = { coarse: true, screen: { width: 744, height: 1133 } };
const LAPTOP = { coarse: false, screen: { width: 1440, height: 900 } };

function memoryStorage(initial = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: (key) => (data.has(key) ? data.get(key) : null),
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: (key) => data.delete(key),
  };
}

function boot({ device = LAPTOP, href = 'http://127.0.0.1:8123/messages', storage = memoryStorage(), userAgentData, brokenStorage = false } = {}) {
  const appended = [];
  const replaced = [];
  const dataset = {};
  const document = {
    documentElement: { dataset },
    head: { appendChild: (node) => appended.push(node) },
    createElement: (tag) => ({ tagName: tag.toUpperCase() }),
  };
  const window = {
    location: { href },
    history: { state: null, replaceState: (_state, _title, url) => replaced.push(url) },
    matchMedia: (query) => ({ matches: query === '(pointer: coarse) and (hover: none)' && device.coarse }),
  };
  Object.defineProperty(window, 'localStorage', {
    get() { if (brokenStorage) throw new Error('SecurityError'); return storage; },
  });
  vm.runInNewContext(source, {
    window, document, URL, Number, Math, Object,
    navigator: userAgentData ? { userAgentData } : {},
    screen: device.screen,
  });
  return { decision: window.__tclaudeLayout, dataset, appended, replaced, storage };
}

test('a desktop browser gets the desktop layout and the page is left untouched', () => {
  const { decision, dataset, appended, replaced } = boot();
  assert.deepEqual({ ...decision }, { layout: 'desktop', detected: 'desktop', override: null });
  assert.deepEqual(dataset, {});
  assert.deepEqual(appended, []);
  assert.deepEqual(replaced, [], 'no URL rewrite without ?layout=');
});

test('device detection: a phone is detected, tablets and fine-pointer screens are not', () => {
  assert.equal(boot({ device: IPHONE }).decision.detected, 'phone');
  assert.equal(boot({ device: { ...IPHONE, screen: { width: 844, height: 390 } } }).decision.detected, 'phone', 'held sideways');
  assert.equal(boot({ device: IPAD_MINI }).decision.detected, 'desktop', 'the smallest tablet is excluded');
  assert.equal(boot({ device: { coarse: false, screen: { width: 390, height: 844 } } }).decision.detected, 'desktop', 'a small fine-pointer screen');
  assert.equal(boot({ device: LAPTOP, userAgentData: { mobile: true } }).decision.detected, 'phone', 'Client Hints say mobile');
  assert.equal(boot({ device: IPHONE, userAgentData: { mobile: false } }).decision.detected, 'phone', 'mobile:false is not a veto');
});

test('a detected phone gets the phone layout without an override', () => {
  const { decision, dataset, appended, replaced } = boot({ device: IPHONE });
  assert.deepEqual({ ...decision }, { layout: 'phone', detected: 'phone', override: null });
  assert.equal(dataset.layout, 'phone');
  assert.deepEqual(appended.map((node) => node.tagName), ['META', 'LINK']);
  assert.deepEqual(replaced, [], 'no URL rewrite without ?layout=');
  const tablet = boot({ device: IPAD_MINI });
  assert.equal(tablet.decision.layout, 'desktop', 'a tablet keeps the desktop page');
  assert.deepEqual(tablet.appended, []);
});

test('?layout=phone switches to the phone layout, remembers it and strips the parameter', () => {
  const { decision, dataset, appended, replaced, storage } = boot({ href: 'http://127.0.0.1:8123/messages?node=inst_abcd&layout=phone#m1' });
  assert.deepEqual({ ...decision }, { layout: 'phone', detected: 'desktop', override: 'phone' });
  assert.equal(dataset.layout, 'phone');
  assert.deepEqual(appended.map((node) => node.tagName), ['META', 'LINK']);
  assert.equal(appended[0].name, 'viewport');
  assert.equal(appended[0].content, 'width=device-width, initial-scale=1, viewport-fit=cover');
  assert.equal(appended[1].rel, 'stylesheet');
  assert.equal(appended[1].href, '/static/phone.css');
  assert.equal(storage.getItem('tclaude.layout'), 'phone');
  assert.deepEqual(replaced, ['/messages?node=inst_abcd#m1']);
});

test('a remembered override applies without the parameter', () => {
  const phone = boot({ storage: memoryStorage({ 'tclaude.layout': 'phone' }) });
  assert.equal(phone.decision.layout, 'phone');
  assert.equal(phone.decision.override, 'phone');
  const desktop = boot({ device: IPHONE, storage: memoryStorage({ 'tclaude.layout': 'desktop' }) });
  assert.deepEqual({ ...desktop.decision }, { layout: 'desktop', detected: 'phone', override: 'desktop' });
  assert.deepEqual(desktop.appended, []);
});

test('?layout=auto forgets the override; junk values are ignored', () => {
  const auto = boot({ href: 'http://h/?layout=auto', storage: memoryStorage({ 'tclaude.layout': 'phone' }) });
  assert.equal(auto.decision.layout, 'desktop');
  assert.equal(auto.decision.override, null);
  assert.equal(auto.storage.getItem('tclaude.layout'), null);
  const junk = boot({ href: 'http://h/?layout=tablet', storage: memoryStorage({ 'tclaude.layout': 'garbage' }) });
  assert.equal(junk.decision.layout, 'desktop');
  assert.equal(junk.decision.override, null);
  assert.deepEqual(junk.replaced, ['/'], 'the parameter is still stripped');
});

test('without usable storage ?layout=phone still holds for this load', () => {
  const { decision, dataset } = boot({ href: 'http://h/?layout=phone', brokenStorage: true });
  assert.equal(decision.layout, 'phone');
  assert.equal(dataset.layout, 'phone');
  assert.equal(boot({ brokenStorage: true }).decision.layout, 'desktop');
});
