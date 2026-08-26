import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function mountSelect(t, optionOverrides = []) {
  const harness = await createPreactHarness(t);
  const { SelectControl } = await harness.importDashboardModule('js/select-control.js');
  const selections = [];
  const options = [
    { value: 'alpha', label: 'Alpha' },
    { value: 'blocked', label: 'Blocked', disabled: true },
    { value: 'beta', label: 'Beta' },
    ...optionOverrides,
  ];
  function Fixture() {
    const [open, setOpen] = harness.hooks.useState(false);
    const [value, setValue] = harness.hooks.useState('alpha');
    return harness.html`<${SelectControl}
      id="test-select" ariaLabel="Test profile" value=${value}
      options=${options} open=${open}
      onOpenChange=${setOpen}
      onValueChange=${(next) => {
        selections.push(next);
        setValue(next);
        setOpen(false);
      }}
    >${value}</${SelectControl}>`;
  }
  const mounted = await harness.mount(harness.html`<${Fixture}/>`);
  return { harness, mounted, selections };
}

test('shared Select exposes a controlled top-layer listbox and skips disabled options', async (t) => {
  const fixture = await mountSelect(t);
  const { harness, mounted } = fixture;
  const trigger = mounted.container.querySelector('button');
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  await harness.act(async () => {});
  const listbox = mounted.container.querySelector('[role="listbox"]');
  assert.equal(listbox.getAttribute('popover'), 'auto');
  assert.equal(trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(harness.document.activeElement, listbox);

  await harness.act(() => harness.fireEvent(listbox, 'keydown', { key: 'ArrowDown' }));
  const active = listbox.getAttribute('aria-activedescendant');
  assert.match(listbox.querySelector(`#${active}`).textContent, /Beta/,
    'ArrowDown skips the disabled middle option');
  await harness.act(() => harness.fireEvent(listbox, 'keydown', { key: 'Enter' }));
  assert.deepEqual(fixture.selections, ['beta']);
  assert.equal(trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(listbox.hidden, true);
});

test('shared Select supports typeahead, Escape focus restoration, and light dismissal', async (t) => {
  const fixture = await mountSelect(t, [{ value: 'gamma', label: 'Gamma' }]);
  const { harness, mounted } = fixture;
  const trigger = mounted.container.querySelector('button');
  await harness.act(() => harness.fireEvent(trigger, 'click'));
  await harness.act(async () => {});
  const listbox = mounted.container.querySelector('[role="listbox"]');

  await harness.act(() => harness.fireEvent(listbox, 'keydown', { key: 'g' }));
  const active = listbox.getAttribute('aria-activedescendant');
  assert.match(listbox.querySelector(`#${active}`).textContent, /Gamma/);
  await harness.act(() => harness.fireEvent(listbox, 'keydown', { key: 'Escape' }));
  assert.equal(trigger.getAttribute('aria-expanded'), 'false');
  assert.equal(harness.document.activeElement, trigger);

  await harness.act(() => harness.fireEvent(trigger, 'click'));
  await harness.act(() => harness.fireEvent(listbox, 'toggle', { newState: 'closed' }));
  assert.equal(trigger.getAttribute('aria-expanded'), 'false',
    'browser light dismissal feeds back into controlled state');
});
