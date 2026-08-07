import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// MarkdownDocument fetches its parser over the network on first use, so its
// non-happy states are real states an operator can land in — not defensive
// dead code. They are exercised here by swapping the model module the
// component imports, which is the only seam that can make the on-demand
// import fail without breaking the vendored parser for every other suite.

// A stub stands in for the whole model module, so it has to offer the whole
// export surface: a missing named export is a module-load error, not a quiet
// undefined. The parts these tests do not exercise live here once.
const MODEL_REST = `
  export const REMOTE_IMAGE = 'remote-image';
  export function remoteImageSources() { return []; }
`;

async function mountWith(t, modelSource, source = '# Title\n') {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/markdown-model.js', modelSource);
  const { MarkdownDocument } = await harness.importDashboardModule('js/markdown-document.js');
  const mounted = await harness.mount(harness.html`<${MarkdownDocument} source=${source} />`);
  for (let i = 0; i < 3; i += 1) {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
  return mounted;
}

test('a parser that will not load leaves the download as the way through', async (t) => {
  const mounted = await mountWith(t, `
    export function loadMarkdownParser() { return Promise.reject(new Error('offline')); }
    export function markdownToTree() { return []; }
    ${MODEL_REST}
  `);
  const state = mounted.container.querySelector('.markdown-document-state');
  assert.ok(state);
  assert.equal(state.getAttribute('role'), 'alert');
  assert.match(state.textContent, /could not be loaded/i);
  assert.match(state.textContent, /download the file/i);
  await mounted.unmount();
});

test('a document that fails to parse says so instead of blanking', async (t) => {
  const mounted = await mountWith(t, `
    export function loadMarkdownParser() { return Promise.resolve({}); }
    export function markdownToTree() { throw new Error('bad tokens'); }
    ${MODEL_REST}
  `);
  const state = mounted.container.querySelector('.markdown-document-state');
  assert.ok(state);
  assert.equal(state.getAttribute('role'), 'alert');
  assert.match(state.textContent, /could not be rendered/i);
  await mounted.unmount();
});

test('an empty document is reported as empty, not as a failure', async (t) => {
  const mounted = await mountWith(t, `
    export function loadMarkdownParser() { return Promise.resolve({}); }
    export function markdownToTree() { return []; }
    ${MODEL_REST}
  `, '   \n');
  const state = mounted.container.querySelector('.markdown-document-state');
  assert.ok(state);
  assert.equal(state.getAttribute('role'), 'status', 'an empty file is not an error');
  assert.match(state.textContent, /empty/i);
  await mounted.unmount();
});

// The retry contract the model documents: one failed load must not make every
// later viewer in the session fail too.
test('a failed parser load is not cached', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/markdown-model.js', `
    let attempts = 0;
    export function attemptCount() { return attempts; }
    export function loadMarkdownParser() {
      attempts += 1;
      return attempts === 1 ? Promise.reject(new Error('offline')) : Promise.resolve({});
    }
    export function markdownToTree() { return [{ tag: 'p', attrs: {}, children: ['second try'] }]; }
    ${MODEL_REST}
  `);
  const model = await harness.importDashboardModule('js/markdown-model.js');
  const { MarkdownDocument } = await harness.importDashboardModule('js/markdown-document.js');

  const first = await harness.mount(harness.html`<${MarkdownDocument} source="# a" />`);
  await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  assert.match(first.container.textContent, /could not be loaded/i);
  await first.unmount();

  const second = await harness.mount(harness.html`<${MarkdownDocument} source="# a" />`);
  for (let i = 0; i < 3; i += 1) {
    await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  }
  assert.equal(model.attemptCount(), 2, 'the next viewer retries the load');
  assert.match(second.container.textContent, /second try/);
  await second.unmount();
});
