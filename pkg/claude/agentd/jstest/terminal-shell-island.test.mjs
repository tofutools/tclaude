import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function installHosts(harness) {
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  for (const name of ['groups', 'terminals']) {
    const button = nav.appendChild(harness.document.createElement('a'));
    button.dataset.tab = name;
    button.href = `/${name === 'groups' ? '' : name}`;
    button.textContent = name;
    button.addEventListener('click', (event) => {
      event.preventDefault();
      for (const item of nav.querySelectorAll('[data-tab]')) item.classList.toggle('active', item === button);
      for (const section of harness.document.querySelectorAll('main section')) {
        section.classList.toggle('active', section.id === `tab-${name}`);
      }
    });
  }
  nav.querySelector('[data-tab="groups"]').classList.add('active');
  const main = harness.document.body.appendChild(harness.document.createElement('main'));
  const groups = main.appendChild(harness.document.createElement('section'));
  groups.id = 'tab-groups';
  groups.classList.add('active');
  const terminals = main.appendChild(harness.document.createElement('section'));
  terminals.id = 'tab-terminals';
  const host = terminals.appendChild(harness.document.createElement('div'));
  host.id = 'terminals-root';
  const badgeHost = nav.appendChild(harness.document.createElement('span'));
  badgeHost.id = 'terminals-badge-root';
  const modalHost = harness.document.body.appendChild(harness.document.createElement('div'));
  modalHost.id = 'terminal-session-root';
  return { host, badgeHost, modalHost, terminals };
}

function fakeWidgetFactory(harness) {
  const widgets = [];
  const factory = (options) => {
    const child = harness.document.createElement('textarea');
    child.dataset.opaqueXterm = String(widgets.length + 1);
    options.host.append(child);
    let disposed = false;
    const widget = {
      options,
      child,
      disposeCount: 0,
      connectCount: 0,
      copyCount: 0,
      activeEdges: [],
      currentStatus: 'disconnected',
      connect() {
        this.connectCount += 1;
        this.currentStatus = 'connected';
        options.onStatus('connected');
        options.onReconnectChange(false);
        return Promise.resolve(true);
      },
      copy() { this.copyCount += 1; return Promise.resolve(); },
      setActive(value) { this.activeEdges.push(!!value); },
      status() { return this.currentStatus; },
      dispose() {
        if (disposed) return;
        disposed = true;
        this.disposeCount += 1;
      },
    };
    widgets.push(widget);
    return widget;
  };
  return { factory, widgets };
}

test('dashboard terminal feature owns three hosts while preserving opaque xterm descendants', async (t) => {
  const harness = await createPreactHarness(t);
  const { host, badgeHost, modalHost, terminals } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const requests = [];
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    confirm: async () => true,
    fetchImpl: async (url) => { requests.push(url); return { ok: true }; },
  });
  await harness.act(() => {});

  assert.equal(host.dataset.islandOwner, 'terminals');
  assert.equal(badgeHost.dataset.islandOwner, 'terminals');
  assert.equal(modalHost.dataset.islandOwner, 'terminals');
  assert.equal(harness.document.body.classList.contains('hide-terminals'), true);

  await harness.act(async () => {
    controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    await Promise.resolve();
  });
  assert.equal(harness.document.body.classList.contains('hide-terminals'), false);
  assert.equal(terminals.classList.contains('active'), true, 'opening reveals the dashboard tab');
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '1');
  assert.equal(host.querySelectorAll('[role="tab"]').length, 1);
  assert.equal(fake.widgets.length, 1);
  const opaqueChild = fake.widgets[0].child;
  assert.equal(opaqueChild.parentElement.classList.contains('mux-pane-xterm'), true);

  await harness.act(() => fake.widgets[0].options.onStatus('copied'));
  assert.equal(host.querySelector('.mux-pane-status').textContent, 'copied');
  assert.equal(fake.widgets[0].child, opaqueChild, 'a chrome rerender never reconciles xterm descendants');
  await harness.act(() => fake.widgets[0].options.onSelectionChange(true));
  const copy = getByRole(host, 'button', { name: /Copy selected terminal text/ });
  harness.fireEvent(copy, 'click');
  assert.equal(fake.widgets[0].copyCount, 1);

  await harness.act(async () => {
    controller.openTerminalPane({ ws: '/two', key: 'two', label: 'two', agent: 'agt_two' });
    await Promise.resolve();
  });
  assert.equal(host.querySelectorAll('[role="tab"]').length, 2);
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '2');
  assert.equal(fake.widgets[0].activeEdges.at(-1), false);
  assert.equal(fake.widgets[1].activeEdges.at(-1), true);
  await harness.act(() => controller.focusTerminalForConv(['agt_one']));
  assert.equal(fake.widgets[0].activeEdges.at(-1), true);

  const closeOne = getByRole(host, 'button', { name: 'Close one' });
  await harness.act(() => harness.fireEvent(closeOne, 'click'));
  assert.equal(fake.widgets[0].disposeCount, 1);
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '1');

  await harness.act(() => controller.openTermModal({
    wsPath: '/modal-live', label: 'agent one', hideConv: 'agt_one',
  }));
  assert.equal(modalHost.querySelector('#term-session-title').textContent, 'Terminal — agent one');
  assert.ok(modalHost.querySelector('#term-session-detach'));
  assert.equal(fake.widgets.length, 3);
  const modalOpaque = fake.widgets[2].child;
  await harness.act(() => fake.widgets[2].options.onStatus('connected'));
  assert.equal(fake.widgets[2].child, modalOpaque);
  await harness.act(async () => {
    harness.fireEvent(modalHost.querySelector('#term-session-close'), 'click');
    await Promise.resolve();
    await Promise.resolve();
  });
  assert.equal(modalHost.childElementCount, 0);
  assert.equal(fake.widgets[2].disposeCount, 1);
  assert.deepEqual(requests, ['/api/hide/agt_one']);

  cleanup();
  cleanup();
  assert.equal(fake.widgets[1].disposeCount, 1, 'feature cleanup disposes the remaining pane once');
  assert.equal(host.childElementCount, 0);
  assert.equal(badgeHost.childElementCount, 0);
  assert.equal(modalHost.childElementCount, 0);
});

test('throwaway modal omits Detach, ignores Escape, and confirms backdrop close', async (t) => {
  const harness = await createPreactHarness(t);
  const { modalHost } = installHosts(harness);
  const fake = fakeWidgetFactory(harness);
  const confirmations = [];
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const { openTermModal } = await harness.importDashboardModule('js/terminals-tab.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: fake.factory,
    confirm: async (options) => { confirmations.push(options); return true; },
    fetchImpl: async () => { throw new Error('throwaway close must not detach'); },
  });
  await harness.act(() => openTermModal({ wsPath: '/scratch', label: 'scratch' }));
  assert.equal(modalHost.querySelector('#term-session-detach'), null);
  const overlay = modalHost.querySelector('#term-session-modal');
  const escape = harness.fireEvent(overlay, 'keydown', { key: 'Escape' });
  assert.equal(escape.defaultPrevented, false, 'Escape remains terminal input, not a shell close key');
  assert.ok(modalHost.querySelector('#term-session-modal'));
  await harness.act(async () => {
    harness.fireEvent(overlay, 'click');
    await Promise.resolve();
    await Promise.resolve();
  });
  assert.equal(confirmations[0].okLabel, 'Close terminal');
  assert.equal(modalHost.childElementCount, 0);
  cleanup();
});
