// PRChain: a member's PR in the Branch column — the link, its CI pill and its
// review icon. A GitLab merge request is named as GitLab names it (!N), keeps
// the same state colours as a pull request, and, having no hover card, carries
// its title in the tooltip.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const MR = 'https://gitlab.example.test:6080/acme/app/-/merge_requests/7';
const PR = 'https://github.com/acme/app/pull/42';

test('a merge request is !N, coloured by state, with its title in the tooltip', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRChain } = await harness.importDashboardModule('js/pr-chain.js');
  for (const state of ['open', 'draft', 'merged', 'closed']) {
    const mounted = await harness.mount(harness.html`
      <${PRChain} pr=${{ number: 7, url: MR, state, title: 'Speed up the importer' }} />
    `);
    try {
      const link = mounted.container.querySelector('a.pr-link');
      assert.ok(link, `${state}: the merge request is a link`);
      assert.equal(link.textContent, '!7', `${state}: GitLab's own numbering`);
      assert.ok(link.classList.contains(`pr-state-${state}`), `${state}: the same state colour as a pull request`);
      assert.equal(link.getAttribute('href'), MR);
      assert.equal(link.title, `${state[0].toUpperCase()}${state.slice(1)} merge request !7 — Speed up the importer`);
    } finally {
      await mounted.unmount();
    }
  }
});

test('a merge request without a known state or title still says what it is', async (t) => {
  const harness = await createPreactHarness(t);
  const { PRChain } = await harness.importDashboardModule('js/pr-chain.js');
  const mounted = await harness.mount(harness.html`<${PRChain} pr=${{ number: 7, url: MR }} />`);
  try {
    const link = mounted.container.querySelector('a.pr-link');
    assert.equal(link.textContent, '!7');
    assert.ok(link.classList.contains('pr-state-unknown'));
    assert.equal(link.title, 'Merge request !7');
  } finally {
    await mounted.unmount();
  }
});

test('a presented merge request keeps the agent summary and adds its title', async (t) => {
  const harness = await createPreactHarness(t);
  const { prDescription } = await harness.importDashboardModule('js/pr-chain.js');
  assert.equal(prDescription({ number: 7, url: MR, state: 'open', title: 'Speed up the importer', summary: 'Ready for review' }, { presented: true }),
    'Ready for review — Open merge request !7 — Speed up the importer');
  assert.equal(prDescription({ number: 7, url: MR }, { presented: true }), 'Presented merge request !7');
  assert.equal(prDescription({ url: MR }, { presented: true }), `Presented merge request — ${MR}`);
});

test('a GitHub pull request keeps #N and its short tooltip', async (t) => {
  const harness = await createPreactHarness(t);
  const { prDescription, prRef } = await harness.importDashboardModule('js/pr-chain.js');
  assert.equal(prRef({ number: 42, url: PR }), '#42');
  assert.equal(prRef({ number: 7, url: `${MR}/diffs?view=parallel` }), '!7', 'a sub-page or query is still the merge request');
  assert.equal(prDescription({ number: 42, url: PR, state: 'merged', title: 'Ignored here' }), 'Merged pull request #42',
    'its hover card shows the title');
  assert.equal(prDescription({ number: 42, url: PR, summary: 'Follow-up' }, { presented: true }), `Follow-up — ${PR}`);
  assert.equal(prDescription({ number: 42, url: PR }, { presented: true }), `Presented pull request — ${PR}`);
});
