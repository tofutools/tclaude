// Compatibility entry points and shared readback helpers for the Preact-owned
// template/group management feature. The feature owns all dialog rendering and
// state; callers elsewhere in the dashboard retain these small stable APIs.

import { lastSnapshot } from './dashboard.js';
import { managementController } from './management-controller.js';
import { openTermModal } from './terminals-tab.js';
import { toast } from './refresh.js';
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

export function openFromGroupModal(groupName) {
  return managementController().openTemplateFromGroup(groupName || '');
}

export function openGroupContextModal(groupName) {
  return managementController().openGroupContext(groupName);
}

export function openGroupCloneModal(groupName) {
  return managementController().openGroupClone(groupName);
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

export function bindGroupImportModal() {
  document.querySelector('#group-import-open')?.addEventListener('click', () => managementController().openGroupImport());
}

const SCRIBE_NAME = 'circle-scribe';
const SCRIBE_SLUGS = ['templates.manage'];

function scribeLibraryBrief() {
  return [
    'You are a scribe: your job is editing this daemon’s tclaude group templates (a.k.a. summoning circles) by chat, on the human’s behalf.',
    'Read the `agent-circles` skill for the full workflow and the template JSON wire shape.',
    'Discover templates with `tclaude agent templates ls`, inspect one with `tclaude agent templates show <name> --json`, then edit with the safe show-json → edit --file loop — `edit` is a FULL REPLACE, so always post the whole desired state.',
    'Verify with `tclaude agent templates show <name>` after each edit. You already hold the templates.manage grant.',
    'The human will tell you which circle to change and how — wait for their instructions.',
  ].join('\n\n');
}

function scribeTemplateBrief(name) {
  return [
    `You are a scribe: your job is editing the tclaude group template (summoning circle) named "${name}" by chat, on the human’s behalf.`,
    'Read the `agent-circles` skill for the workflow and the template JSON wire shape.',
    `Start by loading its current state: \`tclaude agent templates show "${name}" --json\`. Then edit with the safe show-json → edit --file loop — \`edit\` is a FULL REPLACE, so always post the whole desired state.`,
    `Verify with \`tclaude agent templates show "${name}"\` after each edit. You already hold the templates.manage grant.`,
    'The human will tell you what to change — wait for their instructions.',
  ].join('\n\n');
}

function scribeNewTemplateBrief() {
  return [
    'You are a scribe: your job is creating a new tclaude group template (summoning circle) by chat, on the human’s behalf.',
    'Read the `agent-circles` skill for the workflow and the template JSON wire shape — a minimal template is just a name plus an agents roster (each with a name and an initial_message).',
    'Gather the roster and any choreography from the human, write the JSON, and create it with `tclaude agent templates create --file <path>`.',
    'Verify with `tclaude agent templates show <name>` after creating. You already hold the templates.manage grant.',
    'Wait for the human to describe the team they want.',
  ].join('\n\n');
}

async function summonScribe(brief) {
  try {
    const response = await fetch('/api/scribe', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: SCRIBE_NAME, slugs: SCRIBE_SLUGS, brief }),
    });
    if (!response.ok) { toast((await response.text()) || `HTTP ${response.status}`, true); return; }
    const result = await response.json().catch(() => ({}));
    const name = result.name || SCRIBE_NAME;
    if (result.focus_mode === 'browser' && result.focus_ws) {
      openTermModal({ wsPath: result.focus_ws, label: name, hideConv: result.conv_id || null });
      toast(`summoned scribe ${name} — opened in-browser terminal`);
    } else {
      toast(`summoned scribe ${name} — opening its terminal`);
    }
  } catch (error) {
    toast(error?.message || String(error), true);
  }
}

export function summonTemplateScribe({ scope = 'library', name = '' } = {}) {
  if (scope === 'template') return summonScribe(name ? scribeTemplateBrief(name) : scribeNewTemplateBrief());
  return summonScribe(scribeLibraryBrief());
}
