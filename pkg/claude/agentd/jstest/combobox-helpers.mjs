// Test helpers for the shared agent picker (agent-picker.js) and other
// FilterCombobox boxes: what a box offers, and choosing one row of it.

const comboInput = (box) => box.querySelector('input[role="combobox"]');

// open focuses the box and clicks it: a box that stands in for a <select>
// opens on a click, not on focus alone.
async function open(harness, box) {
  await harness.act(() => harness.fireEvent(comboInput(box), 'focus'));
  await harness.act(() => harness.fireEvent(comboInput(box), 'click'));
}

// comboKeys opens the box and returns the keys of the rows it offers.
export async function comboKeys(harness, box) {
  await open(harness, box);
  return [...box.querySelectorAll('[role="option"]')].map((row) => row.dataset.key);
}

// comboLabels opens the box and returns the labels of the rows it offers.
export async function comboLabels(harness, box) {
  await open(harness, box);
  return [...box.querySelectorAll('[role="option"] .rowname')].map((row) => row.textContent);
}

// comboPick opens the box and chooses the row keyed key.
export async function comboPick(harness, box, key) {
  await open(harness, box);
  const row = box.querySelector(`[role="option"][data-key="${key}"]`);
  if (!row) throw new Error(`no row ${key} in the box`);
  await harness.act(() => harness.fireEvent(row, 'mousedown'));
}
