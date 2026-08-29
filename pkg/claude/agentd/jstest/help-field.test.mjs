import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
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

test('HelpField opens help only on activation and dismisses it after keyboard reading', async (t) => {
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
  assert.equal(description.getAttribute('tabindex'), '-1', 'closed help stays out of passive Tab traversal');

  // Tabbing through the form highlights the button but must not leave a trail
  // of popovers covering later fields.
  await harness.act(() => harness.fireEvent(button, 'focus'));
  await rerender(node());
  assert.equal(open, '', 'focus alone does not open help');
  assert.equal(host.querySelector('.spawn-field-help-trigger').getAttribute('aria-expanded'), 'false');

  // Native button activation covers pointer clicks plus Enter/Space. Focus no
  // longer mutates state, so an ordinary click needs no mousedown workaround.
  await harness.act(() => harness.fireEvent(host.querySelector('.spawn-field-help-trigger'), 'click'));
  await rerender(node());
  assert.equal(open, 'demo', 'clicking [?] opens the disclosure');
  assert.equal(host.querySelector('.spawn-field-help-trigger').getAttribute('aria-expanded'), 'true');
  assert.equal(host.querySelector('#demo-hint').getAttribute('tabindex'), '0',
    'opened help is reachable for keyboard reading');

  // Moving from the trigger into its description keeps the disclosure open;
  // moving on to the next form control dismisses it.
  const openButton = host.querySelector('.spawn-field-help-trigger');
  const openDescription = host.querySelector('#demo-hint');
  await harness.act(() => {
    harness.fireEvent(openButton, 'blur', { relatedTarget: openDescription });
    harness.fireEvent(openDescription, 'focus');
  });
  assert.equal(open, 'demo', 'focus can enter the opened help');
  await harness.act(() => harness.fireEvent(openDescription, 'blur', { relatedTarget: select }));
  await rerender(node());
  assert.equal(open, '', 'leaving the trigger and description closes help');
  assert.equal(host.querySelector('#demo-hint').getAttribute('tabindex'), '-1');

  // Escape dismisses an explicitly opened disclosure without escaping the
  // containing modal, and returns focus to its trigger.
  await harness.act(() => harness.fireEvent(host.querySelector('.spawn-field-help-trigger'), 'click'));
  await rerender(node());
  const escape = harness.fireEvent(host.querySelector('#demo-hint'), 'keydown', { key: 'Escape' });
  await rerender(node());
  assert.equal(escape.defaultPrevented, true);
  assert.equal(open, '');
  assert.equal(harness.document.activeElement, host.querySelector('.spawn-field-help-trigger'));

  await harness.act(() => harness.fireEvent(host.querySelector('.spawn-field-help-trigger'), 'click'));
  await rerender(node());
  await harness.act(() => harness.fireEvent(host.querySelector('.spawn-field-help-trigger'), 'click'));
  await rerender(node());
  assert.equal(open, '', 'activating an open help button toggles it closed');
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
  assertAbsent(caveat.closest('.spawn-field-with-help'));
  assert.ok(host.querySelector('.spawn-field-help-column').contains(caveat));

  // The describedby span already announces the full help, which contains this
  // same sentence; a second live region would read the warning twice.
  assert.equal(caveat.getAttribute('aria-hidden'), 'true');
  assert.equal(caveat.getAttribute('aria-live'), null);

  // Help with no ⚠ renders no caveat line at all.
  await rerender(harness.preact.h(HelpField, { ...props, help: 'Never request approval; failures return to the model.' }));
  assertAbsent(host.querySelector('#demo-caveat'));

  // Help can be transiently empty while the sandbox-profile preview loads. That
  // must leave nothing behind: an empty description would be a focusable,
  // unnamed, blank tooltip in the tab order, and a dangling aria-describedby.
  await rerender(harness.preact.h(HelpField, { ...props, help: '' }));
  assertAbsent(host.querySelector('.spawn-field-help-trigger'));
  assertAbsent(host.querySelector('#demo-hint'), 'no empty tooltip is left in the tab order');
  assert.equal(host.querySelector('#demo').getAttribute('aria-describedby'), null,
    'aria-describedby does not dangle');
  // The trigger column stays reserved, so the select does not resize when the
  // help arrives a moment later.
  assert.equal(host.querySelector('.spawn-field-with-help').getAttribute('class'), 'spawn-field-with-help');
});

// A caveat that came out of a dialog body has to stay findable. `warn` is the
// only thing carrying that: without it the trigger is indistinguishable from
// the ordinary field help beside it, and the copy is effectively gone.
test('HelpDisclosure marks a caveat in colour, glyph, and accessible name', async (t) => {
  const harness = await createPreactHarness(t);
  const { HelpDisclosure } = await harness.importDashboardModule('js/help-field.js');
  const props = {
    id: 'demo-impl',
    descriptionID: 'demo-impl-help',
    label: 'Sandbox',
    help: 'This implementation cannot enforce a profile’s TCP/UDP rules.',
    open: false,
    setOpen() {},
  };
  const { container: host, rerender } = await harness.mount(
    harness.preact.h(HelpDisclosure, props),
  );

  const plain = host.querySelector('.spawn-field-help-trigger');
  assert.equal(plain.textContent, '?');
  assert.equal(plain.classList.contains('warn'), false);
  assert.equal(plain.getAttribute('aria-label'), 'Show Sandbox help');
  assert.equal(plain.getAttribute('title'), 'Show Sandbox help');
  // Whatever the glyph, the popover has to remain the trigger's DOM sibling:
  // the reveal is expressed purely as `trigger[aria-expanded="true"] +
  // description`, so anything between them silently stops it opening.
  assert.equal(plain.nextElementSibling.id, 'demo-impl-help');
  assert.equal(plain.getAttribute('aria-controls'), 'demo-impl-help');

  await rerender(harness.preact.h(HelpDisclosure, { ...props, warn: true }));
  const warned = host.querySelector('.spawn-field-help-trigger');
  assert.equal(warned.textContent, '!');
  assert.ok(warned.classList.contains('warn'));
  // aria-label wins over content, so the [!] is not announced. The name is what
  // gives a screen-reader user the cue the colour gives a sighted one.
  assert.equal(warned.getAttribute('aria-label'), 'Show Sandbox warning');
  assert.equal(warned.getAttribute('title'), 'Show Sandbox warning');
  assert.equal(warned.nextElementSibling.id, 'demo-impl-help');
  assert.match(warned.nextElementSibling.textContent, /cannot enforce/);

  // Warn is a presentation of the same disclosure, not a second mechanism: an
  // empty one still leaves nothing focusable and unnamed behind.
  await rerender(harness.preact.h(HelpDisclosure, { ...props, warn: true, help: '' }));
  assertAbsent(host.querySelector('.spawn-field-help-trigger'));
  assertAbsent(host.querySelector('#demo-impl-help'));
});
