import { signal } from '@preact/signals';

export function createToolbarProfilePickerState() {
  const dialog = signal(null);
  let generation = 0;

  function open(input = {}) {
    const kind = input.kind === 'sandbox' ? 'sandbox' : 'profile';
    const currentGeneration = ++generation;
    dialog.value = Object.freeze({
      key: `toolbar-profile:${currentGeneration}`,
      generation: currentGeneration,
      kind,
      current: String(input.current || ''),
      producerId: String(input.producerId || ''),
    });
  }

  function close() {
    generation += 1;
    dialog.value = null;
  }

  return Object.freeze({ dialog, open, close, dispose: close });
}
