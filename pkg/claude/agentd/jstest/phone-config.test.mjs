// js/phone-config-island.js: the phone's Settings list, and each section as a
// page over the desktop form (edited, or read-only with Desktop site).
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t, { initialOpen = '', panel = 'appearance', dirtyPanels = [] } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createDashboardState }, island, configPanelModule, { CONFIG_PANELS }] = await Promise.all([
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/phone-config-island.js'),
    harness.importDashboardModule('js/config-panel.js'),
    harness.importDashboardModule('js/config-form-markup.js'),
  ]);
  const { configPanel, setConfigPanel } = configPanelModule;
  configPanel.value = panel;
  const state = createDashboardState();
  state.setActiveTab('config');
  const calls = [];
  let wizard = false;
  const actions = {
    showSection: (id) => { calls.push(['showSection', id]); return setConfigPanel(id); },
    back: () => calls.push(['back']),
    desktopSite: () => calls.push(['desktopSite']),
    isWizard: () => wizard,
    toggleWizard: () => { wizard = !wizard; harness.document.dispatchEvent(new harness.window.Event('tclaude:wizard')); },
    remember: (open) => calls.push(['remember', open]),
  };
  const [{ phonePage }] = await Promise.all([harness.importDashboardModule('js/phone-page.js')]);
  const config = { dirtyPanels: harness.signals.signal(dirtyPanels) };
  const mounted = await harness.mount(harness.html`<${island.PhoneConfig} state=${state} config=${config} panels=${CONFIG_PANELS}
    actions=${actions} initialOpen=${initialOpen} />`);
  const c = mounted.container;
  const root = harness.document.documentElement;
  const row = (title) => [...c.querySelectorAll('.phone-config-row')].find((b) => b.querySelector('.phone-config-title').textContent.startsWith(title));
  const tap = (element) => harness.act(() => element.click());
  return { harness, island, configPanel, calls, c, root, row, tap, phonePage, actionsWizard: () => wizard };
}

test('the list: this phone, the sections a phone edits, and the rest to view', async (t) => {
  const { c, root, row, tap, calls, actionsWizard, harness } = await setup(t);
  assert.equal(root.dataset.phoneConfig, 'list');
  const groups = [...c.querySelectorAll('.phone-section')].map((g) => g.getAttribute('aria-label'));
  assert.deepEqual(groups, ['This phone', 'Settings', 'On the desktop']);
  const titles = (label) => [...c.querySelectorAll(`[aria-label="${label}"] .phone-config-row .phone-config-title`)].map((n) => n.firstChild.textContent);
  assert.deepEqual(titles('Settings').sort(), ['Notifications', 'Steward', 'Usage & costs']);
  assert.ok(titles('On the desktop').includes('Daemon & network'));
  assert.equal(row('Daemon').querySelector('.phone-config-tag').textContent, 'view');
  assert.equal(row('Notifications').querySelector('.phone-config-tag'), null);

  await tap([...c.querySelectorAll('button')].find((b) => b.textContent === 'Desktop site'));
  assert.deepEqual(calls.at(-1), ['desktopSite']);
  const wizard = c.querySelector('#phone-config-wizard');
  assert.equal(wizard.getAttribute('role'), 'switch');
  assert.equal(c.querySelector('label[for="phone-config-wizard"]').textContent.includes('Wizard theme'), true);
  await harness.act(() => harness.fireEvent(wizard, 'change'));
  assert.equal(actionsWizard(), true, 'the switch toggles the theme');
});

test('an editable section opens as a page; ‹ steps back to the list', async (t) => {
  const { c, root, row, tap, calls, configPanel, phonePage, harness } = await setup(t);
  await tap(row('Notifications'));
  assert.deepEqual(calls.filter((call) => call[0] === 'showSection'), [['showSection', 'notifications']]);
  assert.equal(configPanel.value, 'notifications');
  assert.equal(root.dataset.phoneConfig, 'edit', 'the desktop form shows that section');
  assert.equal(c.querySelector('.phone-config'), null);
  assert.equal(phonePage.value.title, 'Notifications');
  assert.equal(phonePage.value.hideNav, true, 'the save bar takes the bottom edge');
  assert.deepEqual(calls.at(-1), ['remember', 'notifications']);
  await harness.act(() => phonePage.value.onBack());
  assert.deepEqual(calls.filter((call) => call[0] === 'back'), [['back']], 'it pushed a history entry, so ‹ is Back');
  assert.equal(root.dataset.phoneConfig, 'list');
  assert.equal(phonePage.value, null);
});

test('a read-only section says so, and Back (the location moving) shows the list', async (t) => {
  const { c, root, row, tap, calls, configPanel, harness } = await setup(t);
  await tap(row('Daemon'));
  assert.equal(root.dataset.phoneConfig, 'read');
  const note = c.querySelector('.phone-config-readonly');
  assert.ok(note.textContent.includes('Read-only on a phone'));
  await tap(note.querySelector('button'));
  assert.deepEqual(calls.at(-1), ['desktopSite']);
  // The phone's Back: the router restores the section before.
  await harness.act(() => { configPanel.value = 'appearance'; });
  assert.equal(root.dataset.phoneConfig, 'list');
});

test('a section a link named opens directly, and ‹ shows the list in place', async (t) => {
  const { c, root, calls, phonePage, harness } = await setup(t, { initialOpen: 'git', panel: 'git' });
  assert.equal(root.dataset.phoneConfig, 'read');
  assert.equal(phonePage.value.title, 'Git & forges');
  assert.equal(phonePage.value.hideNav, false, 'a read-only page keeps the nav');
  await harness.act(() => phonePage.value.onBack());
  assert.equal(calls.some((call) => call[0] === 'back'), false, 'nothing to step back to');
  assert.ok(c.querySelector('.phone-config'));
});

test('the list marks a section that holds unsaved edits', async (t) => {
  const { row } = await setup(t, { dirtyPanels: ['notifications', 'git'] });
  const tags = (title) => [...row(title).querySelectorAll('.phone-config-tag')].map((tag) => tag.textContent);
  assert.deepEqual(tags('Notifications'), ['unsaved']);
  assert.deepEqual(tags('Git'), ['view', 'unsaved']);
  assert.deepEqual(tags('Steward'), []);
});
