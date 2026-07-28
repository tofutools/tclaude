import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('group attachments enforce http(s) again at the render boundary', async (t) => {
  const harness = await createPreactHarness(t);
  const [
    { GroupsNativeList, safeGroupAttachmentURL },
    { GroupsInteractionProvider },
    { createActionDialogState },
    { ActionDialogApp },
  ] = await Promise.all([
    harness.importDashboardModule('js/groups-list.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
    harness.importDashboardModule('js/action-dialog-state.js'),
    harness.importDashboardModule('js/action-dialog-island.js'),
  ]);
  const state = createActionDialogState();
  const featureSnapshot = harness.signals.signal({
    group_attachments_mode: 'off',
  });
  const actions = {
    openGroupAttachment: state.openGroupAttachment,
    close: state.close,
    setGroupAttachment: async () => {},
  };
  const normalizedHostCases = [
    ['one-slash', 'https:/evil.example'],
    ['three-slashes', 'https:///evil.example'],
    ['no-slashes', 'https:evil.example'],
  ];
  for (const [, malformedURL] of normalizedHostCases) {
    assert.equal(safeGroupAttachmentURL(malformedURL), '',
      'browser URL normalization must not repair a malformed authority');
  }
  const groups = [{
    name: 'safe', members: [], online: 0,
    attachment_url: 'https://example.com/project',
    attachment_label: 'Safe project',
  }, {
    name: 'unsafe', members: [], online: 0,
    attachment_url: 'javascript:alert(document.domain)',
    attachment_label: 'Legacy bad row',
  }, {
    name: 'empty', members: [], online: 0,
  }, {
    name: 'hostless', members: [], online: 0,
    attachment_url: 'https://',
    attachment_label: 'No host',
  }];
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const view = (mode) => {
    featureSnapshot.value = { group_attachments_mode: mode };
    return harness.html`<${GroupsInteractionProvider}>
      <${GroupsNativeList}
        groups=${groups}
        snapshot=${{
          activity_bots: {},
          links: [],
          sudo: [],
          group_attachments_mode: mode,
        }}
        actions=${actions}
      />
      <${ActionDialogApp}
        state=${state}
        actions=${actions}
        snapshot=${featureSnapshot}
        confirmDiscard=${async () => true}
      />
    <//>`;
  };
  const mounted = await harness.mount(view('off'), host);

  const attachment = (name) => host.querySelector(
    `details[data-group-key="${name}"] > summary .group-attachment`);
  const assertTabReachable = (element) => {
    assert.ok(element?.matches('a[href], button:not([disabled])'));
    assert.notEqual(element?.getAttribute('tabindex'), '-1');
    assert.equal(element?.hasAttribute('hidden'), false);
  };
  assert.equal(
    host.querySelector('.group-attachment'),
    null,
    'the absent/default-off feature mode keeps every stored attachment dark',
  );

  await mounted.rerender(view('float'));
  const safe = attachment('safe');
  assert.ok(safe.classList.contains('group-attachment-float'));
  assert.equal(safe.querySelector('.group-attachment-label'), null,
    'float mode stays icon-only');
  assert.equal(safe.querySelector('a')?.getAttribute('href'), 'https://example.com/project');
  assert.equal(safe.querySelector('a')?.textContent, '📎', 'the floating control stays icon-only');
  assert.match(safe.querySelector('a')?.getAttribute('title'), /Safe project/);
  assertTabReachable(safe.querySelector('a'));

  const unsafe = attachment('unsafe');
  assert.equal(unsafe.querySelector('a'), null, 'a bad stored row must never become a live link');
  assert.equal(unsafe.querySelector('.group-attachment-invalid').textContent, '📎');
  assert.match(
    unsafe.querySelector('.group-attachment-invalid').getAttribute('aria-label'),
    /unsafe stored attachment/,
  );
  assertTabReachable(unsafe.querySelector('.group-attachment-invalid'));
  assert.ok(unsafe.querySelector('.group-attachment-edit'), 'bad rows remain editable/clearable');

  const empty = attachment('empty');
  assert.equal(empty?.tagName, 'BUTTON');
  assert.equal(empty?.textContent, '📎');
  assertTabReachable(empty);
  const emptySummary = host.querySelector('details[data-group-key="empty"] > summary');
  empty.click();
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.ok(host.querySelector('#task-link-modal'), 'the paperclip opens the real attachment editor');
  assert.equal(harness.document.activeElement?.id, 'task-link-url');
  const escape = new harness.window.Event('keydown', { bubbles: true });
  Object.defineProperty(escape, 'key', { value: 'Escape' });
  harness.document.dispatchEvent(escape);
  await harness.act(() => Promise.resolve());
  assert.equal(host.querySelector('#task-link-modal'), null, 'Escape closes the attachment editor');
  assert.equal(
    harness.document.activeElement,
    emptySummary,
    'Escape restores focus to the summary instead of pinning the overlay open',
  );

  empty.click();
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.ok(host.querySelector('#task-link-modal'), 'the editor can reopen while enabled');
  await harness.act(() => {
    featureSnapshot.value = { group_attachments_mode: 'off' };
  });
  assert.equal(
    host.querySelector('#task-link-modal'),
    null,
    'a live enabled-to-disabled snapshot immediately hides the open editor',
  );
  assert.equal(
    state.view.value.dialog,
    null,
    'disabling also releases dialog ownership instead of leaving hidden stale state',
  );

  await mounted.rerender(view('fixed'));
  assert.equal(host.querySelector('.group-attachment-float'), null,
    'fixed mode does not retain the floating overlay');

  const fixedSafe = attachment('safe');
  assert.ok(fixedSafe.classList.contains('group-attachment-fixed'));
  assert.equal(fixedSafe.querySelector('a')?.getAttribute('href'), 'https://example.com/project');
  assert.equal(fixedSafe.querySelector('.group-attachment-label')?.textContent, 'Safe project',
    'fixed mode keeps the link/ticket label in the DOM');
  assert.equal(fixedSafe.querySelector('.qo-text'), null,
    'the fixed label does not participate in quick-item auto-folding');
  const safeSummary = host.querySelector('details[data-group-key="safe"] > summary');
  assert.equal(safeSummary.lastElementChild, fixedSafe,
    'fixed mode is the far-right group quick item');

  const fixedEmpty = attachment('empty');
  assert.equal(fixedEmpty?.tagName, 'BUTTON');
  assert.equal(fixedEmpty.textContent, '📎',
    'an unset fixed attachment stays paperclip-only');
  assert.equal(fixedEmpty.querySelector('.group-attachment-label'), null);
  fixedEmpty.focus();
  fixedEmpty.click();
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.ok(host.querySelector('#task-link-modal'), 'the fixed quick item opens the editor');
  harness.document.dispatchEvent(escape);
  await harness.act(() => Promise.resolve());
  assert.equal(harness.document.activeElement, fixedEmpty,
    'fixed mode restores focus to its stable quick item');

  const fixedHostless = attachment('hostless');
  assert.equal(fixedHostless.querySelector('a'), null,
    'http(s) without a host must remain inert in fixed mode');
  assertTabReachable(fixedHostless.querySelector('.group-attachment-invalid'));
  await mounted.unmount();
});
