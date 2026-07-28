import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('group attachments enforce http(s) again at the render boundary', async (t) => {
  const harness = await createPreactHarness(t);
  const [
    { GroupsNativeList },
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
  const actions = {
    openGroupAttachment: state.openGroupAttachment,
    close: state.close,
    setGroupAttachment: async () => {},
  };
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
  const view = (enabled) => harness.html`<${GroupsInteractionProvider}>
      <${GroupsNativeList}
        groups=${groups}
        snapshot=${{
          activity_bots: {},
          links: [],
          sudo: [],
          group_attachments_enabled: enabled,
        }}
        actions=${actions}
      />
      <${ActionDialogApp}
        state=${state}
        actions=${actions}
        confirmDiscard=${async () => true}
      />
    <//>`;
  const mounted = await harness.mount(view(false), host);

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
    'the absent/default-false feature flag keeps every stored attachment dark',
  );

  await mounted.rerender(view(true));
  const safe = attachment('safe');
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

  const hostless = attachment('hostless');
  assert.equal(hostless.querySelector('a'), null, 'http(s) without a host must remain inert');
  assertTabReachable(hostless.querySelector('.group-attachment-invalid'));
  await mounted.unmount();
});
