import { signal } from '@preact/signals';

export function createToolbarProfilePickerState() {
  const editor = signal(null);
  const values = Object.freeze({ profile: signal(''), sandbox: signal('') });
  let generation = 0;

  function open(input = {}) {
    const kind = input.kind === 'sandbox' ? 'sandbox' : 'profile';
    const openedCurrent = Object.prototype.hasOwnProperty.call(input, 'current')
      ? String(input.current || '')
      : values[kind].value;
    values[kind].value = openedCurrent;
    const currentGeneration = ++generation;
    const descriptor = Object.freeze({
      key: `toolbar-profile:${currentGeneration}`,
      generation: currentGeneration,
      kind,
      current: openedCurrent,
    });
    editor.value = descriptor;
    return descriptor;
  }

  function close(descriptor) {
    if (!descriptor || editor.value !== descriptor) return false;
    generation += 1;
    editor.value = null;
    return true;
  }

  function update(kind, name) {
    values[kind === 'sandbox' ? 'sandbox' : 'profile'].value = String(name || '');
  }

  function dispose() {
    generation += 1;
    editor.value = null;
  }

  return Object.freeze({ editor, values, open, close, update, dispose });
}
