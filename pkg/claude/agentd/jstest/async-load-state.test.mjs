import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

test('async load state exposes accessible loading, failure, stale, and retry behavior', async (t) => {
  const harness = await createPreactHarness(t);
  const { AsyncLoadState } = await harness.importDashboardModule('js/async-load-state.js');
  let retries = 0;
  const loading = await harness.mount(harness.html`<${AsyncLoadState}
    label="Plugins" request=${{ phase: 'loading', hasLoaded: false, error: null }} retry=${() => {}} />`);
  assert.match(getByRole(loading.container, 'status').textContent, /Loading plugins/);
  await loading.unmount();

  const failed = await harness.mount(harness.html`<${AsyncLoadState}
    label="Plugins" request=${{ phase: 'error', hasLoaded: true, error: 'offline' }}
    retry=${() => { retries += 1; }} />`);
  const alert = getByRole(failed.container, 'alert');
  assert.match(alert.textContent, /Plugins refresh failed: offline/);
  assert.match(alert.textContent, /Showing the last successful page/);
  await harness.act(() => harness.fireEvent(getByRole(alert, 'button', { name: 'Retry' }), 'click'));
  assert.equal(retries, 1);
  await failed.unmount();
});
