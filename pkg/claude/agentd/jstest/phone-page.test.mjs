// js/phone-page.js: a tab's pushed page in the phone shell's top bar.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('pageKey changes with the look of a page, not with its handlers', async (t) => {
  const { pageKey } = await (await createPreactHarness(t)).importDashboardModule('js/phone-page.js');
  const page = { title: 'A', hideNav: true, actions: [{ key: 'x', label: 'X', onClick: () => 1 }] };
  assert.equal(pageKey(null), '');
  assert.equal(pageKey(page), pageKey({ ...page, onBack: () => 2, actions: [{ key: 'x', label: 'X', onClick: () => 3 }] }));
  assert.notEqual(pageKey(page), pageKey({ ...page, title: 'B' }));
  assert.notEqual(pageKey(page), pageKey({ ...page, actions: [{ key: 'x', label: 'X', disabled: true }] }));
});

test('usePhonePage sets the page once, calls the latest handlers, and clears only its own page', async (t) => {
  const harness = await createPreactHarness(t);
  const { phonePage, usePhonePage } = await harness.importDashboardModule('js/phone-page.js');
  const { signal } = harness.signals;
  const props = signal({ title: 'Mail', back: 'first', active: true });
  const backs = [];
  let sets = 0;
  const stop = phonePage.subscribe(() => { sets += 1; });
  t.after(stop);
  sets = 0;
  function Page() {
    const { title, back, active } = props.value;
    usePhonePage({ title, hideNav: true, onBack: () => backs.push(back), actions: [] }, active);
    return null;
  }
  await harness.mount(harness.html`<${Page} />`);
  assert.equal(phonePage.value.title, 'Mail');
  assert.equal(sets, 1);

  await harness.act(() => { props.value = { ...props.value, back: 'second' }; });
  assert.equal(sets, 1, 'a re-render with a new handler does not replace the page');
  phonePage.value.onBack();
  assert.deepEqual(backs, ['second'], 'the shell calls the latest handler');

  await harness.act(() => { props.value = { ...props.value, active: false }; });
  assert.equal(phonePage.value, null, 'an inactive view takes its page down');

  await harness.act(() => { props.value = { ...props.value, active: true }; });
  const other = { title: 'Other', actions: [] };
  phonePage.value = other;
  await harness.act(() => { props.value = { ...props.value, active: false }; });
  assert.equal(phonePage.value, other, 'another view\'s page is left alone');
});

test('bars stack: a view on another tab setting its bar later does not hide this one, and each takes out only its own', async (t) => {
  const harness = await createPreactHarness(t);
  const { barFor, phoneBar, usePhoneBar } = await harness.importDashboardModule('js/phone-page.js');
  const { signal } = harness.signals;
  const shown = signal({ access: true, agents: true });
  function Bar({ tab, name }) {
    usePhoneBar({ tab, actions: [{ key: name, icon: name, label: name }] }, shown.value[tab]);
    return null;
  }
  // Access sets its bar first; Agents, mounted the whole time, after.
  await harness.mount(harness.html`<${Bar} tab="access" name="grant" /><${Bar} tab="groups" name="spawn" />`);
  await harness.act(() => { shown.value = { ...shown.value, groups: true }; });
  assert.equal(barFor(phoneBar.value, 'access')?.actions[0].key, 'grant', 'still found under the later bar');
  assert.equal(barFor(phoneBar.value, 'groups')?.actions[0].key, 'spawn');
  assert.equal(barFor(phoneBar.value, 'messages'), null);

  await harness.act(() => { shown.value = { ...shown.value, access: false }; });
  assert.equal(barFor(phoneBar.value, 'access'), null, 'taken out from under the later bar');
  assert.equal(barFor(phoneBar.value, 'groups')?.actions[0].key, 'spawn', 'the other is left alone');
  await harness.act(() => { shown.value = { ...shown.value, groups: false }; });
  assert.equal(phoneBar.value, null);
});
