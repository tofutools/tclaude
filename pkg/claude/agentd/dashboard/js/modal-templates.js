import { refuseInPeerView } from './peer-view-guard.js';
import { bindPAFDrop } from './paf-drop.js';
// Compatibility entry points and shared readback helpers for the Preact-owned
// template/group management feature. The feature owns all dialog rendering and
// state; callers elsewhere in the dashboard retain these small stable APIs.

import { lastSnapshot } from './dashboard.js';
import { managementController } from './management-controller.js';
import { openGroupCloneModal as openUnifiedGroupCloneModal } from './group-create-controller.js';
import {
  templateReadbackBadges, templateRosterRowsHTML,
} from './template-readback.js';
export { templateReadbackBadges, templateRosterRowsHTML };

export function renderTemplatesTab() {
  if (!lastSnapshot) return;
  // The first snapshot can land while the lazy Preact feature is still loading.
  // A later refresh will deliver the same data once its controller is mounted.
  try {
    managementController().updateTemplates(lastSnapshot.templates || [], lastSnapshot.groups || []);
  } catch (_) {}
}

export function openTemplatesManageModal(options) {
  return managementController().openTemplatesManageModal(options);
}

export function openGroupImport(file = null, landing = null) {
  return managementController().openGroupImport(file, landing);
}

export function openTemplateEditor(template, options = {}) {
  return managementController().openTemplateEditor(template || null, options);
}

export function openDuplicateModal(name) {
  return managementController().openTemplateDuplicate(name);
}

export function deleteTemplate(name) {
  return managementController().removeTemplate(name);
}

export function openSummonForDrop(templateName, dropGroup) {
  return managementController().openTemplateDeploy(templateName, (dropGroup || '').trim());
}

// node names another node's group (blank: the page's): the template is
// saved on that node.
export function openFromGroupModal(groupName, node = '') {
  return managementController().openTemplateFromGroup(groupName || '', node);
}

// node names another node's group (the fused Groups view); blank is the page's.
export function openGroupContextModal(groupName, node = '') {
  return managementController().openGroupContext(groupName, node);
}

export function openGroupCloneModal(groupName, placement = null, node = '') {
  return openUnifiedGroupCloneModal(groupName, placement, node);
}

export function groupDefaultContext(groupName) {
  const group = (lastSnapshot?.groups || []).find((item) => item.name === groupName);
  return group?.default_context || '';
}

// Static dashboard triggers are outside the island. Everything after the click
// is rendered and handled by Preact.
export function bindTemplatesUI() {
  document.querySelector('#templates-manage-open')?.addEventListener('click', openTemplatesManageModal);
}

export function openAgentExport(value) {
  return managementController().openAgentExport(value);
}

export function openSnapshots(preset = {}) {
  return managementController().openSnapshots(preset);
}

export function openGroupExport(group, node = '') {
  return managementController().openGroupExport(group, node);
}

export function bindGroupImportModal() {
  document.querySelector('#group-import-open')?.addEventListener('click', () => openGroupImport());
  document.querySelector('#snapshots-open')?.addEventListener('click', () => openSnapshots());
  bindPAFDrop({ open: openGroupImport, refuse: () => refuseInPeerView('Import') });
}


