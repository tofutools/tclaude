import { signal } from '@preact/signals';

export function createToolbarProfilePickerState() {
  const dialog = signal(null);
  let generation = 0;

  function open(input = {}) {
    const kind = input.kind === 'sandbox' ? 'sandbox' : 'profile';
    const currentGeneration = ++generation;
    const descriptor = Object.freeze({
      key: `toolbar-profile:${currentGeneration}`,
      generation: currentGeneration,
      kind,
      current: String(input.current || ''),
      producerId: String(input.producerId || ''),
    });
    dialog.value = descriptor;
    return descriptor;
  }

  function close(descriptor) {
    if (!descriptor || dialog.value !== descriptor) return false;
    generation += 1;
    dialog.value = null;
    return true;
  }

  function dispose() {
    generation += 1;
    dialog.value = null;
  }

  return Object.freeze({ dialog, open, close, dispose });
}
