import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('group attachments enforce http(s) again at the render boundary', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ GroupsNativeList }, { GroupsInteractionProvider }] = await Promise.all([
    harness.importDashboardModule('js/groups-list.js'),
    harness.importDashboardModule('js/groups-interactions.js'),
  ]);
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
  }];
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${GroupsInteractionProvider}>
    <${GroupsNativeList}
      groups=${groups}
      snapshot=${{ activity_bots: {}, links: [], sudo: [] }}
      actions=${{ openGroupAttachment: () => {} }}
    />
  <//>`, host);

  const attachment = (name) => host.querySelector(
    `details[data-group-key="${name}"] > summary .group-attachment`);
  const safe = attachment('safe');
  assert.equal(safe.querySelector('a')?.getAttribute('href'), 'https://example.com/project');
  assert.equal(safe.querySelector('a')?.textContent, '📎', 'the floating control stays icon-only');
  assert.match(safe.querySelector('a')?.getAttribute('title'), /Safe project/);

  const unsafe = attachment('unsafe');
  assert.equal(unsafe.querySelector('a'), null, 'a bad stored row must never become a live link');
  assert.equal(unsafe.querySelector('.group-attachment-invalid').textContent, '📎');
  assert.match(
    unsafe.querySelector('.group-attachment-invalid').getAttribute('aria-label'),
    /unsafe stored attachment/,
  );
  assert.ok(unsafe.querySelector('.group-attachment-edit'), 'bad rows remain editable/clearable');

  const empty = attachment('empty');
  assert.equal(empty?.tagName, 'BUTTON');
  assert.equal(empty?.textContent, '📎');
  await mounted.unmount();
});
