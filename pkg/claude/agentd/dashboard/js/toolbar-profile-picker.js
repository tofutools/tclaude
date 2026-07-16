// Controller seam for the Preact-owned dashboard profile picker.
let controller = null;

export function registerToolbarProfilePickerController(value) {
  if (!value || typeof value.open !== 'function') {
    throw new TypeError('toolbar profile picker controller is required');
  }
  if (controller) throw new Error('toolbar profile picker is already registered');
  controller = value;
  return () => { if (controller === value) controller = null; };
}

export function openToolbarProfilePicker(descriptor) {
  if (!controller) throw new Error('toolbar profile picker is not ready');
  return controller.open(descriptor);
}
