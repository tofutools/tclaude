// The standard Preact controls (js/ui-components.js) render the ui-* classes,
// states and roles from ui.css, pass other props through, and hand their DOM
// element to domRef.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByLabelText, getByRole } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const ui = await harness.importDashboardModule('js/ui-components.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  return { harness, ui, host };
}

test('uiButtonClass builds the class list innerHTML paths stamp', async (t) => {
  const { ui } = await setup(t);
  assert.equal(ui.uiButtonClass(), 'ui-btn');
  assert.equal(ui.uiButtonClass({ variant: 'primary' }), 'ui-btn ui-btn-primary');
  assert.equal(ui.uiButtonClass({ variant: 'ghost danger', size: 'sm', icon: true }), 'ui-btn ui-btn-ghost ui-btn-danger ui-sm ui-btn-icon');
  assert.equal(ui.uiButtonClass({ variant: 'bogus' }), 'ui-btn', 'unknown variants are dropped');
});

test('Button renders variant, size, states and passes props through', async (t) => {
  const { harness, ui, host } = await setup(t);
  let clicks = 0;
  let element = null;
  const mounted = await harness.mount(harness.html`
    <div>
      <${ui.Button} id="save" variant="primary" class="extra" domRef=${(node) => { element = node; }} onClick=${() => { clicks += 1; }}>Save</${ui.Button}>
      <${ui.Button} id="busy" busy disabled>Saving…</${ui.Button}>
      <${ui.Button} id="toggle" size="sm" pressed=${false}>Pin</${ui.Button}>
      <${ui.Button} id="submit" type="submit" variant="ghost danger" icon aria-label="Remove">×</${ui.Button}>
      <${ui.Button} id="raw" aria-busy="true" aria-pressed="true">Raw</${ui.Button}>
    </div>
  `, host);
  const save = host.querySelector('#save');
  assert.equal(save.className, 'ui-btn ui-btn-primary extra');
  assert.equal(save.getAttribute('type'), 'button', 'buttons never submit by default');
  assert.equal(element, save, 'domRef receives the button');
  save.click();
  assert.equal(clicks, 1);

  const busy = host.querySelector('#busy');
  assert.equal(busy.getAttribute('aria-busy'), 'true');
  assert.equal(busy.disabled, true);
  assert.equal(save.hasAttribute('aria-busy'), false);
  assert.equal(save.hasAttribute('aria-pressed'), false, 'a plain button is not a toggle');

  const toggle = host.querySelector('#toggle');
  assert.equal(toggle.className, 'ui-btn ui-sm');
  assert.equal(toggle.getAttribute('aria-pressed'), 'false');

  const submit = getByRole(host, 'button', { name: 'Remove' });
  assert.equal(submit.getAttribute('type'), 'submit');
  assert.equal(submit.className, 'ui-btn ui-btn-ghost ui-btn-danger ui-btn-icon');

  const raw = host.querySelector('#raw');
  assert.equal(raw.getAttribute('aria-busy'), 'true', 'a raw aria-busy survives without the busy prop');
  assert.equal(raw.getAttribute('aria-pressed'), 'true', 'a raw aria-pressed survives without the pressed prop');
  mounted.unmount();
});

test('fields carry the standard classes, size and invalid state', async (t) => {
  const { harness, ui, host } = await setup(t);
  let changed = '';
  const mounted = await harness.mount(harness.html`
    <div>
      <label for="harness">Harness</label>
      <${ui.Select} id="harness" size="sm" value="codex" onChange=${(event) => { changed = event.target.value; }}>
        <option value="claude">claude</option>
        <option value="codex">codex</option>
      </${ui.Select}>
      <${ui.Input} id="name" invalid placeholder="Name" />
      <${ui.Textarea} id="brief" rows="3" />
    </div>
  `, host);
  const select = host.querySelector('#harness');
  assert.equal(select.className, 'ui-select ui-sm');
  assert.equal(select.hasAttribute('aria-invalid'), false);
  select.querySelector('option[value="claude"]').selected = true;
  await harness.act(() => harness.fireEvent(select, 'change'));
  assert.equal(changed, 'claude');

  const input = host.querySelector('#name');
  assert.equal(input.className, 'ui-input');
  assert.equal(input.getAttribute('type'), 'text');
  assert.equal(input.getAttribute('aria-invalid'), 'true');
  assert.equal(host.querySelector('#brief').className, 'ui-textarea');
  mounted.unmount();
});

test('Check labels its input, Switch is a switch, Seg groups buttons', async (t) => {
  const { harness, ui, host } = await setup(t);
  const mounted = await harness.mount(harness.html`
    <div>
      <${ui.Check} name="drafts" checked>Include drafts</${ui.Check}>
      <${ui.Check} type="radio" name="where" value="tab">Tab</${ui.Check}>
      <${ui.Switch} aria-label="Auto focus" checked />
      <${ui.Seg} aria-label="Unit">
        <${ui.Button} pressed=${true}>$</${ui.Button}>
        <${ui.Button} pressed=${false}>tok</${ui.Button}>
      </${ui.Seg}>
    </div>
  `, host);
  const drafts = getByLabelText(host, 'Include drafts');
  assert.equal(drafts.type, 'checkbox');
  // LinkeDOM has no checked property, so Preact sets the attribute instead.
  assert.ok(drafts.checked === true || drafts.hasAttribute('checked'), 'checked reaches the input');
  assert.equal(drafts.closest('label').className, 'ui-check');
  assert.equal(getByLabelText(host, 'Tab').type, 'radio');

  const toggle = getByRole(host, 'switch', { name: 'Auto focus' });
  assert.equal(toggle.className, 'ui-switch');
  assert.equal(toggle.type, 'checkbox');

  const seg = getByRole(host, 'group', { name: 'Unit' });
  assert.equal(seg.className, 'ui-seg');
  assert.deepEqual([...seg.querySelectorAll('button')].map((b) => b.getAttribute('aria-pressed')), ['true', 'false']);
  mounted.unmount();
});

test('Menu renders a menu of items, separators and headings', async (t) => {
  const { harness, ui, host } = await setup(t);
  let picked = '';
  const mounted = await harness.mount(harness.html`
    <${ui.Menu} aria-label="Agent actions" size="sm">
      <${ui.MenuHeading}>Reach</${ui.MenuHeading}>
      <${ui.MenuItem} onClick=${() => { picked = 'focus'; }}>Focus pane</${ui.MenuItem}>
      <${ui.MenuSep} />
      <${ui.MenuItem} warn>Retire</${ui.MenuItem}>
      <${ui.MenuItem} danger warn disabled>Kill</${ui.MenuItem}>
    </${ui.Menu}>
  `, host);
  const menu = getByRole(host, 'menu', { name: 'Agent actions' });
  assert.equal(menu.className, 'ui-menu ui-sm', 'size="sm" packs the rows');
  const items = [...menu.querySelectorAll('[role="menuitem"]')];
  assert.deepEqual(items.map((item) => item.className), ['ui-menu-item', 'ui-menu-item ui-menu-warn', 'ui-menu-item ui-btn-danger'], 'danger wins over warn');
  assert.equal(items[2].disabled, true);
  items[0].click();
  assert.equal(picked, 'focus');
  assert.equal(menu.querySelector('hr').className, 'ui-menu-sep');
  assert.equal(menu.querySelector('.ui-menu-heading').textContent, 'Reach');
  mounted.unmount();
});
