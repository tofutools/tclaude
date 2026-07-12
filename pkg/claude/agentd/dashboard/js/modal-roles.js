// Compatibility entry points for role-management callers outside the
// Preact-owned management root (templates, dock, palette).
import { managementController } from './management-controller.js';

export function bindRolesUI() {
  document.querySelector('#roles-manage-open')?.addEventListener('click', () => openRolesManageModal());
}
export function openRolesManageModal() { return managementController().openRolesManageModal(); }
export function openRoleEditor(seed) { return managementController().openRoleEditor(seed); }
export function removeRole(name) { return managementController().removeRole(name); }
