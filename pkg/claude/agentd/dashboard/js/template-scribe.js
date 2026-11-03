// template-scribe.js -- the circle scribe behind the templates manager's
// "Edit with agent" buttons: a chat agent pre-briefed to edit group templates
// (summoning circles), holding templates.manage. It starts through Get help's
// shared summon (tcl-i5vaex): the operator confirms the grant, and it opens in
// its regular overlay chat window (a terminal tab without the companion).

import { summonHelper } from './get-help.js';
import { shellToast } from './shell-state.js';

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

// summonScribe starts a circle scribe through Get help's shared summon: the
// operator confirms its templates.manage grant, and it opens in its regular
// overlay chat window (a terminal tab without the companion).
async function summonScribe(brief, what, { toast, ...helperOptions }) {
  try {
    const result = await summonHelper({
      name: SCRIBE_NAME, slugs: SCRIBE_SLUGS, brief,
      confirmation: {
        title: 'Grant circle-scribe access?',
        body: `This starts a scribe agent to ${what} by chat. It is granted templates.manage, which can create, edit and delete any group template; revoke it in Access when it is done.`,
        meta: SCRIBE_SLUGS.join(', '),
        okLabel: 'Grant & summon',
      },
      ...helperOptions,
    });
    if (result) toast(`summoned scribe ${result.name}`);
    return result;
  } catch (error) {
    toast(error?.message || String(error), true);
    return null;
  }
}

export function summonTemplateScribe({ scope = 'library', name = '' } = {}, { toast = shellToast, ...helperOptions } = {}) {
  if (scope === 'template') {
    return name
      ? summonScribe(scribeTemplateBrief(name), `edit the circle "${name}"`, { toast, ...helperOptions })
      : summonScribe(scribeNewTemplateBrief(), 'create a new circle', { toast, ...helperOptions });
  }
  return summonScribe(scribeLibraryBrief(), 'edit your circles', { toast, ...helperOptions });
}
