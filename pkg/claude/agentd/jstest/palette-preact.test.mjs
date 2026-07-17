import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function commands(count, calls = []) {
  return Array.from({ length: count }, (_, index) => ({
    icon: index ? '•' : '',
    label: `Command ${index}`,
    hint: index % 2 ? `Hint ${index}` : '',
    keywords: index === count - 1 ? 'last-result' : '',
    run: () => calls.push(index),
  }));
}

test('palette state rebuilds from the injected snapshot, ranks, navigates, and closes before run', async (t) => {
  const harness = await createPreactHarness(t);
  const { createPaletteState } = await harness.importDashboardModule('js/palette-state.js');
  const snapshot = harness.signals.signal({ count: 12 });
  const calls = [];
  const errors = [];
  const builds = [];
  const state = createPaletteState({
    snapshot,
    commandBuilder: (value) => {
      builds.push(value);
      return commands(value.count, calls);
    },
    onError: (...args) => errors.push(args),
  });

  state.show();
  assert.equal(state.open.value, true);
  assert.equal(state.filtered.value.length, 12);
  assert.equal(builds[0], snapshot.value, 'command construction receives the accepted snapshot');
  state.move(-1);
  assert.equal(state.selected.value, 11, 'ArrowUp wraps to the end');
  state.move(1);
  assert.equal(state.selected.value, 0, 'ArrowDown wraps back to the start');
  state.movePage(1, 10);
  assert.equal(state.selected.value, 10);
  state.movePage(1, 10);
  assert.equal(state.selected.value, 11, 'PageDown clamps at the end');
  state.movePage(-1, 10);
  state.movePage(-1, 10);
  assert.equal(state.selected.value, 0, 'PageUp clamps at the start');

  state.setQuery('last-result');
  assert.equal(state.filtered.value.length, 1);
  assert.equal(state.filtered.value[0].label, 'Command 11');
  assert.equal(state.selected.value, 0, 'typing resets selection');
  const order = [];
  state.runSelected({ beforeRun: () => order.push(state.open.value ? 'open' : 'closed') });
  assert.deepEqual(order, ['closed'], 'palette closes before focus restoration and action execution');
  assert.deepEqual(calls, [11]);

  snapshot.value = { count: 3 };
  state.show();
  state.setQuery('Command 2');
  state.rebuild();
  assert.equal(state.query.value, 'Command 2', 'theme-style rebuild preserves the live query');
  assert.equal(state.selected.value, 0, 'rebuild resets selection');
  assert.equal(state.commands.value.length, 3);

  state.commands.value = [{ label: 'Broken', run: () => { throw new Error('boom'); } }];
  state.setQuery('');
  state.runSelected();
  assert.deepEqual(errors, [['command failed: boom', true]]);

  state.commands.value = [{ label: 'Unavailable', enabled: false, run: () => calls.push('bad') }];
  state.open.value = true;
  assert.equal(state.runSelected(), false, 'disabled commands do not execute');
  assert.equal(state.open.value, true, 'disabled commands keep the chooser open so its reason remains visible');
  assert.deepEqual(calls, [11]);
});

test('palette island preserves markup, keyboard/mouse behavior, theme copy, focus, and cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPaletteState }, island] = await Promise.all([
    harness.importDashboardModule('js/palette-state.js'),
    harness.importDashboardModule('js/palette-island.js'),
  ]);
  const snapshot = harness.signals.signal({ count: 12 });
  const runs = [];
  let wizard = false;
  const state = createPaletteState({
    snapshot,
    commandBuilder: (value) => commands(value.count, runs).map((command) => ({
      ...command,
      label: wizard ? `Cast ${command.label}` : command.label,
    })),
  });
  const buttonHost = harness.document.body.appendChild(harness.document.createElement('span'));
  const modalHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  const listeners = [];
  const removals = [];
  const add = harness.document.addEventListener.bind(harness.document);
  const remove = harness.document.removeEventListener.bind(harness.document);
  harness.document.addEventListener = (type, listener, options) => {
    listeners.push([type, listener]);
    add(type, listener, options);
  };
  harness.document.removeEventListener = (type, listener, options) => {
    removals.push([type, listener]);
    remove(type, listener, options);
  };

  await harness.act(() => island.mountPaletteIsland({
    buttonHost,
    modalHost,
    state,
    registerCleanup: (cleanup) => cleanups.push(cleanup),
    documentRef: harness.document,
    wizardActive: () => wizard,
  }));
  assert.equal(cleanups.length, 1);
  const button = buttonHost.querySelector('#command-palette-btn');
  const overlay = modalHost.querySelector('#command-palette-modal');
  assert.equal(button.getAttribute('aria-haspopup'), 'dialog');
  assert.equal(overlay.className, 'modal-overlay palette-overlay');
  assert.equal(getByRole(overlay, 'dialog', { name: 'Command palette' }).getAttribute('aria-modal'), 'true');

  const ownedInput = harness.document.body.appendChild(harness.document.createElement('input'));
  ownedInput.type = 'text';
  ownedInput.focus();
  let ownedEvent;
  await harness.act(() => {
    ownedEvent = harness.fireEvent(ownedInput, 'keydown', { key: 'k', ctrlKey: true });
  });
  assert.equal(ownedEvent.defaultPrevented, false, 'an input keeps Ctrl/Cmd-K');
  assert.equal(state.open.value, false, 'the global palette does not open over a typing field');
  ownedInput.remove();

  button.focus();
  let openEvent;
  await harness.act(() => {
    openEvent = harness.fireEvent(harness.document, 'keydown', { key: 'K', ctrlKey: true });
  });
  assert.equal(openEvent.defaultPrevented, true);
  assert.equal(overlay.classList.contains('show'), true);
  const input = modalHost.querySelector('#palette-input');
  assert.equal(harness.document.activeElement, input);
  assert.equal(input.getAttribute('placeholder'), island.DEFAULT_PLACEHOLDER);
  assert.equal(input.getAttribute('aria-activedescendant'), 'palette-opt-0');
  assert.equal(overlay.querySelectorAll('[role="option"]').length, 12);
  assert.equal(overlay.querySelector('[role="option"]').getAttribute('aria-selected'), 'true');

  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'ArrowUp' }));
  assert.equal(input.getAttribute('aria-activedescendant'), 'palette-opt-11');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'PageUp' }));
  assert.equal(input.getAttribute('aria-activedescendant'), 'palette-opt-1', 'fallback page size is ten');
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'PageUp' }));
  assert.equal(input.getAttribute('aria-activedescendant'), 'palette-opt-0', 'PageUp clamps');
  await harness.act(() => harness.fireEvent(overlay.querySelector('[data-idx="4"]'), 'mousemove'));
  assert.equal(input.getAttribute('aria-activedescendant'), 'palette-opt-4');

  await harness.input(input, 'last-result');
  assert.equal(overlay.querySelectorAll('[role="option"]').length, 1);
  assert.equal(input.getAttribute('aria-activedescendant'), 'palette-opt-0');
  await harness.input(input, 'nothing matches');
  assert.equal(overlay.querySelector('.palette-empty').textContent, 'No matching commands');
  assert.equal(input.hasAttribute('aria-activedescendant'), false);

  await harness.input(input, 'Command 3');
  wizard = true;
  await harness.act(() => harness.fireEvent(harness.document, 'tclaude:wizard'));
  assert.equal(input.value, 'Command 3', 'theme rebuild preserves query');
  assert.equal(input.getAttribute('placeholder'), island.WIZARD_PLACEHOLDER);
  assert.match(overlay.querySelector('.palette-item').textContent, /Cast Command 3/);

  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.deepEqual(runs, [3]);
  assert.equal(overlay.classList.contains('show'), false);
  assert.equal(harness.document.activeElement, button, 'run restores previous focus before the action');

  const blocker = harness.document.body.appendChild(harness.document.createElement('div'));
  blocker.className = 'manage-overlay show';
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'k', metaKey: true }));
  assert.equal(state.open.value, false, 'another visible modal blocks the hotkey');
  blocker.remove();

  const chooser = harness.document.body.appendChild(harness.document.createElement('div'));
  chooser.className = 'process-node-chooser';
  const chooserCancel = chooser.appendChild(harness.document.createElement('button'));
  let chooserHotkey;
  await harness.act(() => {
    chooserHotkey = harness.fireEvent(chooserCancel, 'keydown', { key: 'k', ctrlKey: true });
  });
  assert.equal(chooserHotkey.defaultPrevented, true, 'the anchored chooser claims Ctrl/Cmd-K');
  assert.equal(state.open.value, false, 'the palette cannot stack over the anchored chooser');
  chooser.remove();

  await harness.act(() => harness.fireEvent(button, 'click'));
  assert.equal(state.open.value, true, 'the discoverable button opens without hotkey gating');
  await harness.act(() => harness.fireEvent(overlay, 'click'));
  assert.equal(state.open.value, false, 'backdrop closes');
  await harness.act(() => harness.fireEvent(button, 'click'));
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Escape' }));
  assert.equal(state.open.value, false, 'Escape closes');

  await harness.act(() => harness.fireEvent(button, 'click'));
  assert.equal(state.open.value, true);
  await harness.act(() => cleanups[0]());
  assert.equal(state.open.value, false, 'island cleanup closes owned state');
  assert.equal(buttonHost.childElementCount, 0);
  assert.equal(modalHost.childElementCount, 0);
  for (const type of ['keydown', 'tclaude:wizard', 'tclaude:command-palette-open']) {
    const installed = listeners.find(([candidate]) => candidate === type);
    assert.ok(installed, `${type} listener installed`);
    assert.ok(removals.some(([candidate, listener]) =>
      candidate === type && listener === installed[1]), `${type} listener removed`);
  }
  harness.fireEvent(harness.document, 'keydown', { key: 'k', ctrlKey: true });
  assert.equal(state.open.value, false, 'unmounted island no longer handles the hotkey');
  buttonHost.remove();
  modalHost.remove();
});
