// Controller seam for the Preact-owned dashboard profile controls. Snapshot
// painting can happen before the island mounts, so retain the latest values and
// replay them when the controller registers.
let controller = null;
const current = { profile: '', sandbox: '' };

export function registerToolbarProfilePickerController(value) {
  if (!value || typeof value.open !== 'function' || typeof value.update !== 'function') {
    throw new TypeError('toolbar profile picker controller is required');
  }
  if (controller) throw new Error('toolbar profile picker is already registered');
  controller = value;
  controller.update('profile', current.profile);
  controller.update('sandbox', current.sandbox);
  return () => { if (controller === value) controller = null; };
}

export function openToolbarProfilePicker(descriptor) {
  if (!controller) throw new Error('toolbar profile picker is not ready');
  return controller.open(descriptor);
}

export function updateToolbarProfileValue(kind, name) {
  const canonicalKind = kind === 'sandbox' ? 'sandbox' : 'profile';
  current[canonicalKind] = String(name || '');
  controller?.update(canonicalKind, current[canonicalKind]);
}
