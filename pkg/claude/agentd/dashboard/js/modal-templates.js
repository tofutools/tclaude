// Compatibility entry points and shared readback helpers for the Preact-owned
// template/group management feature. The feature owns all dialog rendering and
// state; callers elsewhere in the dashboard retain these small stable APIs.

import { esc } from './helpers.js';
import { lastSnapshot } from './dashboard.js';
import { managementController } from './management-controller.js';
import { openTermModal } from './terminals-tab.js';
import { cachedProfiles, profileSummary, findProfileByHandle } from './profiles.js';
import { toast } from './refresh.js';
import { wizWord } from './slop.js';

export function renderTemplatesTab() {
  if (!lastSnapshot) return;
  // The first snapshot can land while the lazy Preact feature is still loading.
  // A later refresh will deliver the same data once its controller is mounted.
  try {
    managementController().updateTemplates(lastSnapshot.templates || [], lastSnapshot.groups || []);
  } catch (_) {}
}

export function openTemplatesManageModal() {
  return managementController().openTemplatesManageModal();
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

function templateWaveCount(template) {
  return new Set((template?.agents || []).map((agent) => agent.wave || 0)).size;
}

function agentInheritsDeployDefault(agent) {
  const inline = [agent.harness, agent.model, agent.effort, agent.sandbox, agent.approval].some(Boolean);
  return !agent.role_ref && !inline && !agent.profile_inline;
}

function templateAgentIsOwner(agent) {
  let owner = false;
  const profile = agent.spawn_profile && findProfileByHandle(cachedProfiles(), agent.spawn_profile);
  if (profile && typeof profile.is_owner === 'boolean') owner = profile.is_owner;
  if (agent.profile_inline && typeof agent.profile_inline.is_owner === 'boolean') owner = agent.profile_inline.is_owner;
  return owner || !!agent.is_owner;
}

export function templateReadbackBadges(template) {
  if (!template) return '';
  const count = (template.agents || []).length;
  const chips = [`<span class="tc-count">${count} ${wizWord('agent', 'familiar')}${count === 1 ? '' : 's'}</span>`];
  const waves = templateWaveCount(template);
  if (waves > 1) chips.push(`<span class="tc-count" title="${wizWord('staged-spawn waves', 'marching ranks')}">🌊 ${waves} ${wizWord('waves', 'ranks')}</span>`);
  if (template.process?.length) chips.push(`<span class="tc-count" title="${wizWord('process phases', 'quest chapters')}">◆ ${template.process.length}-${wizWord('phase', 'chapter')}</span>`);
  if (template.rhythms?.length) chips.push(`<span class="tc-count" title="${wizWord('seeded rhythms', 'drumbeats')}">🥁 ${template.rhythms.length}</span>`);
  if (template.work_pattern?.length) chips.push(`<span class="tc-count" title="${wizWord('work-pattern steps', 'rite verses')}">⇶ ${template.work_pattern.length}</span>`);
  return chips.join(' ');
}

export function templateRosterRowsHTML(template, prefix, defaultProfile) {
  const agents = template?.agents || [];
  if (!agents.length) return `<span class="tp-empty">${wizWord('this template has no agents', 'this circle names no familiars')}</span>`;
  const shown = prefix?.trim() || wizWord('‹group›', '‹party›');
  const defaultName = defaultProfile?.trim() || '';
  const defaultValue = defaultName && findProfileByHandle(cachedProfiles(), defaultName);
  return agents.map((agent) => {
    const adoptsDefault = !!defaultValue && !agent.spawn_profile && agentInheritsDeployDefault(agent);
    const inline = agent.profile_inline || null;
    const owner = templateAgentIsOwner(agent) || (adoptsDefault && defaultValue.is_owner)
      ? '<span class="tp-owner" title="group owner">★ owner</span>' : '';
    let permissions = (agent.permissions || []).length;
    if (inline?.permission_overrides) permissions += Object.keys(inline.permission_overrides).length;
    if (adoptsDefault && defaultValue.permission_overrides) permissions += Object.keys(defaultValue.permission_overrides).length;
    const launch = inline
      ? `⚙ <span title="${esc(profileSummary(inline) || 'custom launch config')}">${wizWord('custom', 'bespoke')}</span>${agent.spawn_profile ? ` <span class="tp-default-tag" title="fields the custom config leaves blank fall through to this profile">(over ${esc(agent.spawn_profile)})</span>` : ''}`
      : agent.spawn_profile
        ? `⚙ ${esc(agent.spawn_profile)}`
        : adoptsDefault
          ? `⚙ ${esc(defaultName)} <span class="tp-default-tag" title="from the deploy default profile — applies its launch config and birth-time permissions/ownership">(default)</span>`
          : [agent.harness, agent.model, agent.effort].filter(Boolean).map(esc).join('/');
    const meta = [agent.role ? esc(agent.role) : '', launch, permissions ? `+${permissions}🔑` : '', owner].filter(Boolean).join(' · ');
    return `<div class="tp-row"><span class="tp-name">${esc(shown)}-${esc(agent.name)}</span>${meta ? ` <span class="tp-meta">${meta}</span>` : ''}</div>`;
  }).join('');
}
