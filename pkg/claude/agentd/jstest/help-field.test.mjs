import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

// The dashboard collapses per-mode help behind a [?] but keeps the text from a
// "⚠" onward visible, so the marker's placement decides what an operator sees.
// mode-help-fixture.json is generated from the Go harness descriptors and kept
// in step by TestModeHelpFixtureMatchesHarness, so these run against the real
// copy rather than literals that drift.
const modeHelp = JSON.parse(readFileSync(new URL('./mode-help-fixture.json', import.meta.url), 'utf8'));

test('helpCaveat keeps every ⚠ warning whole', async (t) => {
  const harness = await createPreactHarness(t);
  const { helpCaveat } = await harness.importDashboardModule('js/help-field.js');

  assert.equal(helpCaveat('Plain help with no caveat.'), '');
  assert.equal(helpCaveat(''), '');
  assert.equal(helpCaveat(undefined), '');

  // The neutral lead-in is dropped; everything from the ⚠ survives.
  const plan = modeHelp['claude/approval/plan'];
  assert.equal(helpCaveat(plan), '⚠ Still prompts on a write, so a detached agent can block if it tries one.');

  // A caveat spanning several sentences must not be truncated at the first
  // sentence break. For bypassPermissions the second sentence carries the
  // consequence; losing it turns a warning into a feature list.
  const bypass = modeHelp['claude/approval/bypassPermissions'];
  assert.match(helpCaveat(bypass), /no guardrails/);
  assert.match(helpCaveat(bypass), /cannot run as root/);

  // Same shape for sandbox-off, where the consequence is the trailing sentence.
  assert.match(helpCaveat(modeHelp['claude/sandbox/off']), /runs unconfined/);
  assert.match(helpCaveat(modeHelp['codex/sandbox/read-only']), /CANNOT run/);
});

// Every mode-help string the dashboard can render either carries no ⚠ (and so
// collapses entirely) or yields a caveat that still names the consequence.
test('every harness mode help splits cleanly into collapsed copy and visible caveat', async (t) => {
  const harness = await createPreactHarness(t);
  const { helpCaveat } = await harness.importDashboardModule('js/help-field.js');
  const { approvalReviewerHelp } = await harness.importDashboardModule('js/approval-controls.js');

  const entries = Object.entries(modeHelp);
  assert.ok(entries.length > 15, 'the fixture covers the whole catalog');
  let warned = 0;
  for (const [key, help] of entries) {
    const caveat = helpCaveat(help);
    if (!help.includes('⚠')) {
      assert.equal(caveat, '', `${key} has no ⚠ and must collapse entirely`);
      continue;
    }
    warned += 1;
    // A caveat is always a suffix of the full help, so the collapsed popover
    // and the visible line can never contradict each other.
    assert.ok(help.endsWith(caveat), `${key}: caveat must be a suffix of the help`);
    assert.ok(caveat.startsWith('⚠'), `${key}: caveat starts at the marker`);
    assert.ok(caveat.length > 20, `${key}: caveat must carry the actual warning`);
  }
  assert.ok(warned >= 8, 'the dangerous modes still carry their markers');

  // The reviewer help is computed in JS rather than served by the catalog.
  const reviewer = approvalReviewerHelp('auto_review', 'never');
  assert.match(reviewer, /⚠/, 'auto-review under a never-ask policy is a caveat');
  assert.match(helpCaveat(reviewer), /Choose an interactive policy/, 'the remedy stays visible');
});

test('HelpField collapses help behind a [?] that opens on click and on keyboard focus', async (t) => {
  const harness = await createPreactHarness(t);
  const { HelpField } = await harness.importDashboardModule('js/help-field.js');
  let open = '';
  const setOpen = (value) => { open = value; };
  const node = () => harness.preact.h(HelpField, {
    id: 'demo',
    label: 'Approval policy',
    value: 'never',
    options: [{ value: 'never', label: 'Never ask' }],
    onChange() {},
    help: 'Some long help. ⚠ And a caveat that must stay visible.',
    open: open === 'demo',
    setOpen,
  });
  const { container: host, rerender } = await harness.mount(node());

  const select = host.querySelector('#demo');
  const button = host.querySelector('.spawn-field-help-trigger');
  const description = host.querySelector('#demo-hint');

  // Hover help, accessible description, and the disclosure all point at the
  // same copy — the full text, not the caveat.
  assert.equal(select.getAttribute('title'), 'Some long help. ⚠ And a caveat that must stay visible.');
  assert.equal(select.getAttribute('aria-describedby'), 'demo-hint');
  assert.equal(description.textContent, 'Some long help. ⚠ And a caveat that must stay visible.');
  assert.equal(button.getAttribute('aria-expanded'), 'false');

  // Replay the browser's actual pointer sequence: mousedown, then focus unless
  // the handler prevented the default, then click. Without the preventDefault
  // the focus would open the disclosure and the click would immediately toggle
  // it shut, leaving [?] unusable with a mouse.
  // `open` is lifted state in the real dialogs, so Preact re-renders the button
  // with the new prop between the focus and the click. Rerendering here is what
  // makes the toggle-shut regression observable.
  const pointerPress = async () => {
    const target = host.querySelector('.spawn-field-help-trigger');
    let down;
    await harness.act(() => { down = harness.fireEvent(target, 'mousedown'); });
    if (!down.defaultPrevented) {
      await harness.act(() => harness.fireEvent(target, 'focus'));
      await rerender(node());
    }
    await harness.act(() => harness.fireEvent(host.querySelector('.spawn-field-help-trigger'), 'click'));
    await rerender(node());
  };
  await pointerPress();
  assert.equal(open, 'demo', 'clicking [?] opens the disclosure');
  assert.equal(host.querySelector('.spawn-field-help-trigger').getAttribute('aria-expanded'), 'true');

  await pointerPress();
  assert.equal(open, '', 'clicking again closes it');

  // Keyboard users never fire mousedown, so focus alone must open it.
  await harness.act(() => harness.fireEvent(host.querySelector('.spawn-field-help-trigger'), 'focus'));
  assert.equal(open, 'demo', 'tabbing to [?] opens the disclosure');
});

test('HelpField keeps the ⚠ caveat visible outside the popover anchor', async (t) => {
  const harness = await createPreactHarness(t);
  const { HelpField } = await harness.importDashboardModule('js/help-field.js');
  const props = {
    id: 'demo',
    label: 'Permission mode',
    value: 'bypassPermissions',
    options: [{ value: 'bypassPermissions', label: 'Bypass permissions' }],
    onChange() {},
    help: '⚠ Bypass ALL permission checks: auto-approve everything. No guardrails.',
    open: false,
    setOpen() {},
  };
  const { container: host, rerender } = await harness.mount(harness.preact.h(HelpField, props));

  const caveat = host.querySelector('#demo-caveat');
  assert.ok(caveat, 'a ⚠ mode renders a persistent caveat line');
  assert.equal(caveat.textContent, '⚠ Bypass ALL permission checks: auto-approve everything. No guardrails.');
  assert.match(caveat.getAttribute('class'), /\bwarn\b/, 'the caveat is warn-styled');

  // The popover is absolutely positioned against .spawn-field-with-help. If the
  // caveat lived inside that box it would grow the anchor and shove the popover
  // up off its own control, so it must be a sibling.
  assert.equal(caveat.closest('.spawn-field-with-help'), null);
  assert.ok(host.querySelector('.spawn-field-help-column').contains(caveat));

  // The describedby span already announces the full help, which contains this
  // same sentence; a second live region would read the warning twice.
  assert.equal(caveat.getAttribute('aria-hidden'), 'true');
  assert.equal(caveat.getAttribute('aria-live'), null);

  // Help with no ⚠ renders no caveat line at all.
  await rerender(harness.preact.h(HelpField, { ...props, help: 'Never request approval; failures return to the model.' }));
  assert.equal(host.querySelector('#demo-caveat'), null);

  // Help can be transiently empty while the sandbox-profile preview loads. That
  // must leave nothing behind: an empty description would be a focusable,
  // unnamed, blank tooltip in the tab order, and a dangling aria-describedby.
  await rerender(harness.preact.h(HelpField, { ...props, help: '' }));
  assert.equal(host.querySelector('.spawn-field-help-trigger'), null);
  assert.equal(host.querySelector('#demo-hint'), null, 'no empty tooltip is left in the tab order');
  assert.equal(host.querySelector('#demo').getAttribute('aria-describedby'), null,
    'aria-describedby does not dangle');
  // The trigger column stays reserved, so the select does not resize when the
  // help arrives a moment later.
  assert.equal(host.querySelector('.spawn-field-with-help').getAttribute('class'), 'spawn-field-with-help');
});
