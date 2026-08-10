// Behavioural contract for the compact Groups-tab harness marks. Known
// products get recognizable fixed-slot SVGs; an unknown future harness must
// remain legible as text instead of vanishing. Full names are both ordinary
// hover titles and accessible names.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('known harnesses render named product marks and unknown ones retain text', async (t) => {
  const harness = await createPreactHarness(t);
  const { HarnessMark } = await harness.importDashboardModule('js/harness-mark.js');

  const known = [
    ['claude', 'CC', 'Claude Code'],
    ['codex', 'Codex', 'Codex CLI'],
    ['copilot', 'COP', 'GitHub Copilot CLI'],
    ['opencode', 'OC', 'OpenCode'],
  ];
  for (const [name, shortLabel, longLabel] of known) {
    await t.test(name, async () => {
      const mounted = await harness.mount(harness.html`
        <${HarnessMark} name=${name} shortLabel=${shortLabel} longLabel=${longLabel} />
      `);
      try {
        const mark = mounted.container.querySelector(`.harness-mark[data-harness-mark="${name}"]`);
        assert.ok(mark, `${name} renders a known product mark`);
        assert.equal(mark.getAttribute('role'), 'img');
        assert.equal(mark.getAttribute('aria-label'), longLabel);
        assert.equal(mark.title, longLabel, 'ordinary hover exposes the full harness name');
        assert.ok(mark.querySelector('svg path'), 'the mark carries its product silhouette');
        assert.equal(mounted.container.querySelector('.harness-name'), null,
          'known harnesses do not retain the uneven text prefix');
      } finally {
        await mounted.unmount();
      }
    });
  }

  await t.test('unknown harness', async () => {
    const mounted = await harness.mount(harness.html`
      <${HarnessMark} name="future" shortLabel="future" longLabel="Future Harness" />
    `);
    try {
      assert.equal(mounted.container.querySelector('.harness-mark'), null);
      const fallback = mounted.container.querySelector('.harness-name');
      assert.equal(fallback.textContent, 'future');
      assert.equal(fallback.title, 'Future Harness');
    } finally {
      await mounted.unmount();
    }
  });
});

test('a tooltip override leaves the product accessible name intact', async (t) => {
  const harness = await createPreactHarness(t);
  const { HarnessMark } = await harness.importDashboardModule('js/harness-mark.js');
  const mounted = await harness.mount(harness.html`
    <${HarnessMark} name="codex" shortLabel="Codex" longLabel="Codex CLI"
      tooltip="Harness: Codex CLI — Drive: Codex app-server ready" />
  `);
  try {
    const mark = mounted.container.querySelector('.harness-mark');
    assert.equal(mark.title, 'Harness: Codex CLI — Drive: Codex app-server ready');
    assert.equal(mark.getAttribute('aria-label'), 'Codex CLI');
  } finally {
    await mounted.unmount();
  }
});
