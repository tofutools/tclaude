// Helpers for the name dialog a new drawing board or page asks first
// (resource-name-dialog.js): mount its host beside what a test drives, and
// wait for it, polling with a deadline rather than sleeping.

import assert from 'node:assert/strict';

export async function mountNameHost(harness) {
  const mod = await harness.importDashboardModule('js/resource-name-dialog.js');
  const mounted = await harness.mount(harness.html`<${mod.ResourceNameHost} />`);
  return { ...mod, container: mounted.container };
}

export async function waitFor(harness, condition, message, timeoutMs = 3000) {
  const deadline = Date.now() + timeoutMs;
  while (!condition()) {
    if (Date.now() > deadline) assert.fail(message);
    await harness.act(() => new Promise((resolve) => setTimeout(resolve, 5)));
  }
}

export const nameDialog = (harness) => harness.document.getElementById('resource-name-modal');
export const nameInput = (harness) => harness.document.getElementById('resource-name-input');

export async function waitForNameDialog(harness, message = 'the name dialog opens') {
  await waitFor(harness, () => !!nameInput(harness), message);
  return { dialog: nameDialog(harness), input: nameInput(harness) };
}

// submitName types name into the open dialog and presses Enter in the field.
export async function submitName(harness, name) {
  const { input } = await waitForNameDialog(harness);
  if (name !== undefined) await harness.input(input, name);
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  await waitFor(harness, () => !nameDialog(harness), 'the name dialog closes once the resource is made');
}
