// The yellow "!" of an agent with unread human notifications is drawn on the
// Groups member row AND on that agent's tab in the Terminals tab strip. Both
// raise the SAME quick reader, which is why the reader is mounted once on a
// body-level host instead of inside the Groups section: a fixed drawer
// rendered into a hidden tab section would never be visible.
import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

function installHosts(harness) {
  const nav = harness.document.body.appendChild(harness.document.createElement('nav'));
  const main = harness.document.body.appendChild(harness.document.createElement('main'));
  const terminals = main.appendChild(harness.document.createElement('section'));
  terminals.id = 'tab-terminals';
  terminals.classList.add('active');
  const host = terminals.appendChild(harness.document.createElement('div'));
  host.id = 'terminals-root';
  const badgeHost = nav.appendChild(harness.document.createElement('span'));
  badgeHost.id = 'terminals-badge-root';
  const modalHost = harness.document.body.appendChild(harness.document.createElement('div'));
  modalHost.id = 'terminal-session-root';
  return { host, badgeHost, modalHost };
}

function widgetFactory() {
  return (options) => ({
    connect() { options.onStatus('connected'); return Promise.resolve(true); },
    copy() { return Promise.resolve(); },
    fit() {},
    focus() {},
    setActive() {},
    status() { return 'connected'; },
    dispose() {},
  });
}

const older = {
  id: 41,
  from_agent: 'agt_one',
  from_conv: 'conv-one-gen2',
  from_title: 'builder',
  subject: 'earlier question',
  body: 'still open',
  read: false,
  created_at: '2026-07-31T09:00:00Z',
};

const notification = {
  id: 42,
  from_agent: 'agt_one',
  from_conv: 'conv-one-gen2',
  from_title: 'builder',
  subject: 'need a decision',
  body: 'which layout?',
  read: false,
  created_at: '2026-07-31T10:00:00Z',
};

test('a terminal tab raises the shared reader for its agent’s unread notifications', async (t) => {
  const harness = await createPreactHarness(t);
  const { host } = installHosts(harness);
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: widgetFactory(),
    confirm: async () => true,
    fetchImpl: async () => ({ ok: true }),
  });

  await harness.act(async () => {
    controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    controller.openTerminalPane({ ws: '/two', key: 'two', label: 'two', agent: 'agt_two' });
    await Promise.resolve();
  });
  assert.equal(host.querySelectorAll('.mux-tab-attention').length, 0,
    'no snapshot yet means no agent is known to have anything unread');

  await harness.act(() => {
    dashboardState.snapshot.value = { messages_unread: 1, messages: [notification] };
  });
  const glyphs = host.querySelectorAll('.mux-tab-attention');
  assert.equal(glyphs.length, 1, 'only the tab of the sending agent is marked');
  assert.equal(glyphs[0].closest('[role="tab"]').dataset.paneKey, 'one');
  assert.match(glyphs[0].getAttribute('title'), /1 unread notification from one/);
  assert.match(glyphs[0].getAttribute('title'), /need a decision/);

  const opened = [];
  const listen = (event) => opened.push(event.detail);
  harness.document.addEventListener('tclaude:open-human-notification', listen);
  t.after(() => harness.document.removeEventListener('tclaude:open-human-notification', listen));
  harness.fireEvent(glyphs[0], 'click');
  assert.equal(opened.length, 1);
  assert.equal(opened[0].messageId, 42);
  // The pane seed holds one selector; the message carries both identities, so
  // the reader is handed the pair rather than whichever id the launcher had.
  assert.deepEqual(
    { agent: opened[0].sender.agent, conv: opened[0].sender.conv, label: opened[0].sender.label },
    { agent: 'agt_one', conv: 'conv-one-gen2', label: 'builder' },
  );

  await harness.act(() => {
    dashboardState.snapshot.value = {
      messages_unread: 0,
      messages: [{ ...notification, read: true }],
    };
  });
  assert.equal(host.querySelectorAll('.mux-tab-attention').length, 0,
    'reading the notification clears the tab glyph');
  await harness.act(() => cleanup());
});

test('a collapsed tab stack carries the alert of the tabs it hides', async (t) => {
  const harness = await createPreactHarness(t);
  const { host } = installHosts(harness);
  const { mountTerminalsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const controller = await harness.importDashboardModule('js/terminals-tab.js');
  const { terminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const cleanup = await mountTerminalsFeature({
    widgetFactory: widgetFactory(),
    confirm: async () => true,
    fetchImpl: async () => ({ ok: true }),
  });

  await harness.act(async () => {
    controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    controller.openTerminalPane({ ws: '/two', key: 'two', label: 'two', agent: 'agt_two' });
    dashboardState.snapshot.value = { messages_unread: 1, messages: [notification] };
    await Promise.resolve();
  });
  const group = terminalShellState.createGroup({ name: 'work', keys: ['one', 'two'] });
  await harness.act(() => {
    terminalShellState.activatePane('two');
    terminalShellState.toggleGroupCollapsed(group.id);
  });

  assert.equal(host.querySelectorAll('.mux-tab-attention').length, 0,
    'the marked tab is folded away');
  assert.equal(host.querySelectorAll('.mux-group-attention').length, 1);
  assert.match(host.querySelector('.mux-group-pill').getAttribute('aria-label'),
    /1 unread notification in a collapsed tab/);

  await harness.act(() => terminalShellState.toggleGroupCollapsed(group.id));
  assert.equal(host.querySelectorAll('.mux-group-attention').length, 0,
    'an expanded stack shows the alert on the member tab itself');
  const glyphs = host.querySelectorAll('.mux-tab-attention');
  assert.equal(glyphs.length, 1, 'the snapshot reaches tabs rendered inside a stack');
  assert.equal(glyphs[0].closest('[role="tab"]').dataset.paneKey, 'one');
  await harness.act(() => cleanup());
});

test('the quick reader mounts on its own body-level host and answers every surface', async (t) => {
  const harness = await createPreactHarness(t);
  const { HumanNotificationReader } =
    await harness.importDashboardModule('js/human-notification-reader-island.js');
  const { openHumanNotificationReader } =
    await harness.importDashboardModule('js/human-notification-attention.js');
  const snapshot = { value: { messages_unread: 2, messages: [notification, older] } };
  const state = { snapshot, publish(next) { snapshot.value = next; } };
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: true });
  t.after(() => { globalThis.fetch = savedFetch; });

  const launcher = harness.document.body.appendChild(harness.document.createElement('button'));
  t.after(() => launcher.remove());
  const mounted = await harness.mount(harness.html`
    <${HumanNotificationReader} state=${state} actions=${{ reportError: () => {} }}
      closeAnimationMs=${10} />
  `);
  assertAbsent(mounted.container.querySelector('.human-notification-drawer'));

  await harness.act(() => {
    openHumanNotificationReader({
      sender: { agent: 'agt_one', conv: 'conv-one-gen2', label: 'builder' },
      messageID: 42,
      launcher,
      documentRef: harness.document,
    });
  });
  const drawer = mounted.container.querySelector('.human-notification-drawer');
  assert.ok(drawer, 'the event alone opens the reader — no Groups row involved');
  assert.equal(drawer.dataset.messageId, '42');

  await harness.act(() => {
    harness.fireEvent(drawer.querySelector('.human-notification-drawer-close'), 'click');
  });
  // The panel slides out before it goes: it stays mounted, marked .closing, for
  // the length of the exit animation. Focus does not wait for it.
  assert.ok(mounted.container.querySelector('.human-notification-drawer.closing'),
    'closing plays the exit animation instead of vanishing');
  assert.equal(harness.document.activeElement, launcher,
    'closing hands focus back to whatever raised the reader');
  await harness.act(async () => { await new Promise((done) => setTimeout(done, 40)); });
  assertAbsent(mounted.container.querySelector('.human-notification-drawer'));

  // Escape belongs to whoever already handled it — a live terminal, say.
  await harness.act(() => {
    openHumanNotificationReader({
      sender: { agent: 'agt_one', conv: 'conv-one-gen2', label: 'builder' },
      messageID: 42,
      documentRef: harness.document,
    });
  });
  assert.ok(mounted.container.querySelector('.human-notification-drawer'));
  await harness.act(() => {
    const escape = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
    Object.defineProperty(escape, 'key', { value: 'Escape' });
    escape.preventDefault();
    harness.document.dispatchEvent(escape);
  });
  assert.ok(mounted.container.querySelector('.human-notification-drawer'),
    'an already-handled Escape does not close the reader');

  // …and the same Escape, unhandled, still closes it — so the assertion above
  // is about defaultPrevented and not about the listener being absent.
  await harness.act(() => {
    const escape = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
    Object.defineProperty(escape, 'key', { value: 'Escape' });
    harness.document.dispatchEvent(escape);
  });
  assert.ok(mounted.container.querySelector('.human-notification-drawer.closing'));

  // A different notification raised mid-exit takes the panel over rather than
  // opening behind a drawer that is about to unmount itself.
  await harness.act(() => {
    openHumanNotificationReader({
      sender: { agent: 'agt_one', conv: 'conv-one-gen2', label: 'builder' },
      messageID: 41,
      documentRef: harness.document,
    });
  });
  const retaken = mounted.container.querySelector('.human-notification-drawer');
  assert.ok(retaken, 'the panel is still there');
  assert.equal(retaken.classList.contains('closing'), false,
    'reopening while the panel is sliding out cancels the exit');
  assert.equal(retaken.dataset.messageId, '41', 'and it shows the new notification');
  assert.ok(retaken.contains(harness.document.activeElement),
    'a reopen that reuses the mounted panel still moves focus into it');
  await harness.act(async () => { await new Promise((done) => setTimeout(done, 40)); });
  assert.ok(mounted.container.querySelector('.human-notification-drawer'),
    'and the cancelled exit does not fire late and close the reopened panel');
});

test('a dismissed panel stops competing for Escape, and reduced motion skips the hold', async (t) => {
  const harness = await createPreactHarness(t);
  const { HumanNotificationReader } =
    await harness.importDashboardModule('js/human-notification-reader-island.js');
  const { openHumanNotificationReader } =
    await harness.importDashboardModule('js/human-notification-attention.js');
  const snapshot = { value: { messages_unread: 1, messages: [notification] } };
  const state = { snapshot, publish(next) { snapshot.value = next; } };
  const savedFetch = globalThis.fetch;
  globalThis.fetch = async () => ({ ok: true });
  t.after(() => { globalThis.fetch = savedFetch; });

  const open = () => harness.act(() => openHumanNotificationReader({
    sender: { agent: 'agt_one', conv: 'conv-one-gen2', label: 'builder' },
    messageID: 42,
    documentRef: harness.document,
  }));
  const escape = () => {
    const event = new harness.window.Event('keydown', { bubbles: true, cancelable: true });
    Object.defineProperty(event, 'key', { value: 'Escape' });
    harness.act(() => harness.document.dispatchEvent(event));
    return event;
  };

  const mounted = await harness.mount(harness.html`
    <${HumanNotificationReader} state=${state} actions=${{ reportError: () => {} }}
      closeAnimationMs=${10} />
  `);
  await open();
  assert.equal(escape().defaultPrevented, true, 'the open panel owns Escape');
  assert.ok(mounted.container.querySelector('.human-notification-drawer.closing'));
  // Still on screen, but already dismissed: a second Escape belongs to whatever
  // else the operator meant it for, so the sliding-out panel must not eat it.
  assert.equal(escape().defaultPrevented, false,
    'a panel that is sliding out no longer consumes Escape');
  await harness.act(async () => { await new Promise((done) => setTimeout(done, 40)); });

  // Reduced motion: there is no exit animation to wait for, so holding the
  // panel there for the animation's duration would just read as a hang.
  const savedMatchMedia = globalThis.matchMedia;
  globalThis.matchMedia = (query) => ({ matches: query.includes('reduced-motion') });
  t.after(() => { globalThis.matchMedia = savedMatchMedia; });
  await open();
  assert.ok(mounted.container.querySelector('.human-notification-drawer'));
  escape();
  await harness.act(async () => { await new Promise((done) => setTimeout(done, 0)); });
  assertAbsent(mounted.container.querySelector('.human-notification-drawer'), 'reduced motion unmounts at once instead of holding for an animation');
});

test('the reader ignores an open request that names no message', async (t) => {
  const harness = await createPreactHarness(t);
  const { openHumanNotificationReader } =
    await harness.importDashboardModule('js/human-notification-attention.js');
  const seen = [];
  harness.document.addEventListener('tclaude:open-human-notification', (e) => seen.push(e));
  assert.equal(openHumanNotificationReader({
    sender: { agent: 'agt_one' }, messageID: null, documentRef: harness.document,
  }), false);
  assert.equal(openHumanNotificationReader({
    sender: null, messageID: 42, documentRef: harness.document,
  }), false);
  assert.equal(seen.length, 0);
});
