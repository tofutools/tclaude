import { esc } from './helpers.js';
import {
  cachedProfiles, profileSummary, findProfileByHandle,
} from './profiles.js';
import { wizWord } from './slop.js';

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

export function templateReadbackBadgeModels(template) {
  if (!template) return [];
  const count = (template.agents || []).length;
  const chips = [{
    key: 'agents', className: 'tc-count',
    text: `${count} ${wizWord('agent', 'familiar')}${count === 1 ? '' : 's'}`,
  }];
  const waves = templateWaveCount(template);
  if (waves > 1) chips.push({ key: 'waves', className: 'tc-count', title: wizWord('staged-spawn waves', 'marching ranks'), text: `🌊 ${waves} ${wizWord('waves', 'ranks')}` });
  if (template.process?.length) chips.push({ key: 'process', className: 'tc-count', title: wizWord('process phases', 'quest chapters'), text: `◆ ${template.process.length}-${wizWord('phase', 'chapter')}` });
  if (template.rhythms?.length) chips.push({ key: 'rhythms', className: 'tc-count', title: wizWord('seeded rhythms', 'drumbeats'), text: `🥁 ${template.rhythms.length}` });
  if (template.work_pattern?.length) chips.push({ key: 'work-pattern', className: 'tc-count', title: wizWord('work-pattern steps', 'rite verses'), text: `⇶ ${template.work_pattern.length}` });
  return chips;
}

// Imperative template editors still consume trusted renderer markup. Build it
// from the same structured badge model the Dock renders natively so wording
// and counts cannot drift between the two surfaces.
export function templateReadbackBadges(template) {
  const chips = templateReadbackBadgeModels(template);
  if (!chips.length) return '';
  return chips.map((chip) => `<span class="${chip.className}"${chip.title ? ` title="${esc(chip.title)}"` : ''}>${esc(chip.text)}</span>`).join(' ');
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
