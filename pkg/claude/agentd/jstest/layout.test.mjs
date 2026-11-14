// js/layout.js: the module-side reader of layout-boot.js's decision.
import test from 'node:test';
import assert from 'node:assert/strict';

const { readLayout, layoutDecision, layoutOverrideURL, setLayoutOverride, layout, isPhone } = await import('../dashboard/js/layout.js');

test('without a document (or without the mark) the layout is desktop', () => {
  assert.equal(layout, 'desktop');
  assert.equal(isPhone, false);
  assert.equal(readLayout(undefined), 'desktop');
  assert.equal(readLayout({ documentElement: { dataset: {} } }), 'desktop');
  assert.equal(readLayout({ documentElement: { dataset: { layout: 'tablet' } } }), 'desktop');
  assert.equal(readLayout({ documentElement: { dataset: { layout: 'phone' } } }), 'phone');
});

test('layoutDecision normalises what layout-boot.js recorded', () => {
  assert.deepEqual(layoutDecision({}), { layout: 'desktop', detected: 'desktop', override: null });
  assert.deepEqual(layoutDecision({ __tclaudeLayout: { layout: 'phone', detected: 'phone', override: 'phone' } }),
    { layout: 'phone', detected: 'phone', override: 'phone' });
  assert.deepEqual(layoutDecision({ __tclaudeLayout: { layout: 'x', detected: 'phone', override: 'x' } }),
    { layout: 'desktop', detected: 'phone', override: null });
});

test('switching the layout navigates to the same page with ?layout=', () => {
  assert.equal(layoutOverrideURL('desktop', 'http://h/messages?node=inst_abcd#m1'), '/messages?node=inst_abcd&layout=desktop#m1');
  assert.equal(layoutOverrideURL('auto', 'http://h/?layout=phone'), '/?layout=auto');
  assert.throws(() => layoutOverrideURL('tablet', 'http://h/'), RangeError);
  const assigned = [];
  setLayoutOverride('phone', { href: 'http://h/config', assign: (url) => assigned.push(url) });
  assert.deepEqual(assigned, ['/config?layout=phone']);
});
