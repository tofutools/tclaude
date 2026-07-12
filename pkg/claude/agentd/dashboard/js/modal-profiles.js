// Compatibility entry points for profile-management callers outside the
// Preact-owned management root (spawn, templates, dock, palette, row actions).
import { managementController } from './management-controller.js';

export function bindProfilesUI() {
  document.querySelector('#profiles-manage-open')?.addEventListener('click', () => openProfilesManageModal());
}
export function openProfilesManageModal() { return managementController().openProfilesManageModal(); }
export function openProfileEditor(seed, options) { return managementController().openProfileEditor(seed, options); }
export function removeProfile(name) { return managementController().removeProfile(name); }
