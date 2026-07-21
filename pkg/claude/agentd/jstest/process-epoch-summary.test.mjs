import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function epochEnvelope() {
  return {
    run: { id: 'run-epoch', effectiveStatus: 'running', templateRef: 'tmpl@sha256:aa' },
    schema: 'epoch_v8',
    adapted: true,
    viewerV2: { stateSchemaVersion: 8, pathProtocol: 'path_v1_epoch', routingAvailable: false, routingUnavailableReason: 'epoch_v8_summary' },
    lineage: {
      originalTemplateRef: 'tmpl@sha256:aa', currentTemplateRef: 'tmpl@sha256:bb',
      totalEpochs: 2, epochs: [{ ordinal: 0, templateRef: 'tmpl@sha256:aa' }, { ordinal: 1, templateRef: 'tmpl@sha256:bb' }],
    },
    structuralSummary: { nodes: 3, edges: 2, changedFromOriginal: true },
    authorityCounts: { total: 5, active: 2, terminal: 3, states: { verifiedUnclaimed: 1, claimed: 0, active: 1, completed: 3, failed: 0, canceled: 0, handedOff: 0 } },
    currentBinding: { revision: 4, digest: 'd'.repeat(64) },
    epochReport: {
      entries: [{ id: 'wi8_abc', ownerEpochOrdinal: 0, kind: 'waiting', nodeId: 'hold', attempt: 1, status: 'pending' }],
      timeline: [{ revision: 2, kind: 'apply', epochOrdinal: 1, appliedAt: '2026-07-21T00:00:00Z', reasonCode: 'unlock_apply', actorClass: 'human' }],
      timelineTotal: 2,
      truncated: true,
    },
  };
}

test('epochV8Summary projects the safe schema-8 envelope and nothing else', async (t) => {
  const harness = await createPreactHarness(t);
  const { epochV8Summary, viewerUnavailable, ROUTING_UNAVAILABLE } = await harness.importDashboardModule('js/process-viewer-core.js');

  assert.equal(epochV8Summary({ schema: 'legacy' }), null);
  assert.equal(epochV8Summary(null), null);

  const summary = epochV8Summary(epochEnvelope());
  assert.equal(summary.adapted, true);
  assert.equal(summary.totalEpochs, 2);
  assert.deepEqual(summary.epochs.map((entry) => entry.ordinal), [0, 1]);
  assert.equal(summary.structural.changedFromOriginal, true);
  assert.equal(summary.binding.revision, 4);
  // Zero-count states are dropped so chips stay glyph+text and scannable.
  assert.deepEqual(summary.stateChips, [['Unclaimed', 1], ['Active', 1], ['Completed', 3]]);
  assert.equal(summary.entries.length, 1);
  assert.equal(summary.entries[0].id, 'wi8_abc');
  assert.equal(summary.entries[0].ownerEpochOrdinal, 0);
  assert.equal(summary.timeline[0].reasonCode, 'unlock_apply');
  assert.equal(summary.timeline[0].actorClass, 'human');
  assert.equal(summary.timelineTotal, 2);
  assert.equal(summary.timelineTruncated, true);

  // The schema-8 restriction is named honestly in the closed vocabulary.
  assert.ok(ROUTING_UNAVAILABLE.epoch_v8_summary);
  const unavailable = viewerUnavailable(epochEnvelope().viewerV2);
  assert.equal(unavailable.reason, 'epoch_v8_summary');
  assert.match(unavailable.title, /safe summary/i);
});

test('viewer island renders the adaptation summary panel for schema-8 runs', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  const envelope = epochEnvelope();
  const actions = { loadRunView: async () => envelope };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-epoch', key: 'run-epoch' }} actions=${actions}
    setTimeoutImpl=${() => 0} clearTimeoutImpl=${() => {}} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const root = mounted.container;

  const panel = root.querySelector('.process-epoch-summary');
  assert.ok(panel, 'schema-8 runs render the adaptation summary');
  assert.ok(panel.getAttribute('aria-labelledby'));
  const badge = root.querySelector('.process-adapted-badge');
  assert.ok(badge, 'adapted badge is present');
  assert.match(badge.textContent, /adapted/, 'badge is glyph+text, not color-only');
  assert.match(panel.textContent, /epoch 0/);
  assert.match(panel.textContent, /hold/);
  assert.match(panel.textContent, /unlock_apply/);
  // The safe panel never renders restricted material markers.
  assert.doesNotMatch(root.innerHTML, /candidateSource|reason.txt|applyToken/);
  const unavailable = root.querySelector('.process-viewer-unavailable.reason-epoch_v8_summary');
  assert.ok(unavailable, 'restriction banner names the epoch_v8_summary reason');
  await mounted.unmount();
});

test('unlock panel preserves the dirty draft and invalidates tokens on stale binding', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  const envelope = epochEnvelope();
  const previews = [];
  const actions = {
    loadRunView: async () => envelope,
    previewUnlock: async (_id, payload) => {
      previews.push(payload);
      return { status: 409, ok: false, body: { status: 'stale', currentBinding: { revision: 9, digest: 'e'.repeat(64) } } };
    },
    applyUnlock: async () => { throw new Error('apply must not be reachable'); },
    loadExactArtifact: async () => ({ status: 200, ok: true, text: '' }),
  };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-epoch', key: 'run-epoch' }} actions=${actions}
    setTimeoutImpl=${() => 0} clearTimeoutImpl=${() => {}} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const root = mounted.container;

  const sourceInput = root.querySelector('#process-unlock-source');
  assert.ok(sourceInput, 'unlock panel renders a candidate source field');
  await harness.input(sourceInput, 'id: draft-template');
  const previewButton = [...root.querySelectorAll('.process-unlock-panel button')].find((b) => /preview/.test(b.textContent));
  await harness.act(() => { previewButton.click(); return Promise.resolve(); });
  for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve());

  assert.equal(previews.length, 1);
  assert.equal(previews[0].candidateSource, 'id: draft-template');
  const stale = root.querySelector('.process-unlock-stale');
  assert.ok(stale, 'stale banner is a visible status region');
  assert.equal(stale.getAttribute('role'), 'status');
  assert.match(stale.textContent, /revision 9/);
  assert.match(stale.textContent, /draft is unchanged/i);
  // The dirty draft survives verbatim; no preview result or apply affordance remains.
  assert.equal(root.querySelector('#process-unlock-source').value, 'id: draft-template');
  assert.equal(root.querySelector('.process-unlock-preview'), null);
  assert.equal(root.querySelector('.process-unlock-apply'), null);
  // Draft material never leaks into persisted client state.
  assert.equal(Object.keys(globalThis.localStorage || {}).length ? 'has-keys' : 'empty', 'empty');
  await mounted.unmount();
});

test('exact artifact drill-down renders the restricted denial as a bounded error state', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  const envelope = epochEnvelope();
  envelope.lineage.epochs = [
    { ordinal: 0, templateRef: 'tmpl@sha256:aa', epochId: 'a'.repeat(64) },
    { ordinal: 1, templateRef: 'tmpl@sha256:bb', epochId: 'b'.repeat(64) },
  ];
  const requested = [];
  const actions = {
    loadRunView: async () => envelope,
    previewUnlock: async () => ({ status: 500, ok: false, body: {} }),
    applyUnlock: async () => ({ status: 500, ok: false, body: {} }),
    loadExactArtifact: async (id, epochId, kind) => {
      requested.push({ id, epochId, kind });
      return { status: 403, ok: false, text: '{"secret":"denied-body"}' };
    },
  };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-epoch', key: 'run-epoch' }} actions=${actions}
    setTimeoutImpl=${() => 0} clearTimeoutImpl=${() => {}} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const root = mounted.container;

  const diffButton = [...root.querySelectorAll('.process-epoch-artifact-row button')].find((b) => /exact diff/.test(b.textContent));
  assert.ok(diffButton, 'applied epochs offer an exact diff drill-down');
  await harness.act(() => { diffButton.click(); return Promise.resolve(); });
  for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve());

  assert.deepEqual(requested, [{ id: 'run-epoch', epochId: 'b'.repeat(64), kind: 'diff' }], 'only the applied epoch is addressable');
  const denial = root.querySelector('.process-epoch-artifact-view .island-error');
  assert.ok(denial, 'denial renders as an explicit alert state');
  assert.equal(denial.getAttribute('role'), 'alert');
  assert.match(denial.textContent, /process\.runs\.unlock\.read/);
  // The denied response body is never rendered.
  assert.doesNotMatch(root.innerHTML, /denied-body/);
  await mounted.unmount();
});

test('an in-flight preview resolving after the binding moved is discarded, preserving the draft', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  let revision = 4;
  const envelopeAt = () => {
    const envelope = epochEnvelope();
    envelope.currentBinding = { revision, digest: String(revision).repeat(64).slice(0, 64) };
    return envelope;
  };
  let resolvePreview;
  const actions = {
    loadRunView: async () => envelopeAt(),
    previewUnlock: () => new Promise((resolve) => { resolvePreview = resolve; }),
    applyUnlock: async () => { throw new Error('apply must not be reachable'); },
    loadExactArtifact: async () => ({ status: 200, ok: true, text: '' }),
  };
  const polls = [];
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-epoch', key: 'run-epoch' }} actions=${actions}
    setTimeoutImpl=${(fn) => { polls.push(fn); return polls.length; }} clearTimeoutImpl=${() => {}} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const root = mounted.container;

  const sourceInput = root.querySelector('#process-unlock-source');
  await harness.input(sourceInput, 'id: race-draft');
  const previewButton = [...root.querySelectorAll('.process-unlock-panel button')].find((b) => /preview/.test(b.textContent));
  await harness.act(() => { previewButton.click(); return Promise.resolve(); });

  // The polling refresh observes a NEW binding while the preview request is
  // still in flight.
  revision = 9;
  await harness.act(async () => { for (const poll of polls.splice(0)) poll(); });
  for (let i = 0; i < 4; i++) await harness.act(() => Promise.resolve());

  // Now the deferred preview resolves as if it had succeeded against rev 4.
  await harness.act(async () => {
    resolvePreview({ status: 200, ok: true, body: { status: 'valid', applyToken: 'f'.repeat(64), baseBinding: {}, currentBinding: {}, graphSummary: {}, lineage: {}, authorityCounts: {} } });
  });
  for (let i = 0; i < 4; i++) await harness.act(() => Promise.resolve());

  // The stale response and its token are discarded; the draft survives.
  assert.equal(root.querySelector('.process-unlock-preview'), null, 'stale preview result must not install');
  assert.equal(root.querySelector('.process-unlock-apply'), null, 'no apply affordance from a stale token');
  const stale = root.querySelector('.process-unlock-stale');
  assert.ok(stale, 'stale status region announces the discarded preview');
  assert.match(stale.textContent, /revision 9/);
  assert.equal(root.querySelector('#process-unlock-source').value, 'id: race-draft');
  assert.doesNotMatch(root.innerHTML, /ffffffff/, 'the stale applyToken never reaches the DOM');
  await mounted.unmount();
});

test('over-budget candidate sources are refused before any request is serialized', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary, MAX_UNLOCK_SOURCE_BYTES } = await harness.importDashboardModule('js/process-viewer-island.js');
  const previews = [];
  const actions = {
    loadRunView: async () => epochEnvelope(),
    previewUnlock: async (_id, payload) => { previews.push(payload); return { status: 200, ok: true, body: { status: 'valid', applyToken: 'a'.repeat(64), graphSummary: {}, blockers: [] } }; },
    applyUnlock: async () => ({ status: 500, ok: false, body: {} }),
    loadExactArtifact: async () => ({ status: 200, ok: true, text: '' }),
  };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-epoch', key: 'run-epoch' }} actions=${actions}
    setTimeoutImpl=${() => 0} clearTimeoutImpl=${() => {}} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const root = mounted.container;

  const sourceInput = root.querySelector('#process-unlock-source');
  // Re-query per click: the error banner render can recreate sibling nodes.
  const clickPreview = () => harness.act(() => {
    [...root.querySelectorAll('.process-unlock-panel button')].find((b) => /preview/.test(b.textContent)).click();
    return Promise.resolve();
  });

  // One byte over the ceiling — with a multibyte character straddling the
  // boundary, so the check must count encoded bytes, not string length.
  // 'é' encodes to 2 bytes: (MAX-1) ASCII + 'é' = MAX+1 bytes but MAX chars.
  const overBudget = 'a'.repeat(MAX_UNLOCK_SOURCE_BYTES - 1) + 'é';
  await harness.input(sourceInput, overBudget);
  await clickPreview();
  for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve());
  assert.equal(previews.length, 0, 'over-budget drafts are never serialized into a request');
  const budgetError = root.querySelector('.process-unlock-panel .island-error');
  assert.ok(budgetError, 'budget refusal renders as a bounded error');
  assert.match(budgetError.textContent, /4 MiB/);

  // Exactly at the ceiling passes the client bound and reaches the server.
  const atBudget = 'a'.repeat(MAX_UNLOCK_SOURCE_BYTES - 2) + 'é';
  await harness.input(root.querySelector('#process-unlock-source'), atBudget);
  await clickPreview();
  for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve());
  assert.equal(previews.length, 1, 'at-budget drafts are sent');
  await mounted.unmount();
});

test('the adaptation panel renders bounded report rows with honest totals', async (t) => {
  const harness = await createPreactHarness(t);
  const { epochV8Summary } = await harness.importDashboardModule('js/process-viewer-core.js');
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  const envelope = epochEnvelope();
  envelope.epochReport.entries = Array.from({ length: 64 }, (_, index) => ({ id: `wi8_${index}`, ownerEpochOrdinal: 0, kind: 'waiting', nodeId: `n${index}`, attempt: 1, status: 'pending' }));
  envelope.epochReport.entriesTotal = 65;
  envelope.epochReport.entriesTruncated = true;

  const summary = epochV8Summary(envelope);
  assert.equal(summary.entries.length, 64);
  assert.equal(summary.entriesTotal, 65);
  assert.equal(summary.entriesTruncated, true);

  const actions = { loadRunView: async () => envelope };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-epoch', key: 'run-epoch' }} actions=${actions}
    setTimeoutImpl=${() => 0} clearTimeoutImpl=${() => {}} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const root = mounted.container;
  assert.equal(root.querySelectorAll('.process-epoch-entries li').length, 64);
  assert.match(root.querySelector('.process-epoch-summary').textContent, /first 64 of 65/);
  await mounted.unmount();
});
