// groups-quickstart.js -- the Groups › Processes step list (mock Q2,
// tcl-7p6bgt) and the pickers it shares with the inline graph.
// StepListEditor edits a stored process as numbered steps
// (process-step-list.js) with a live preview, its start inputs and who it
// would use now, and starts it in the group, saves it, publishes it to the
// library, or hands it to the full graph editor. Nothing runs until Start.

import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { templateStartRequirements, unauthorizedProfiles } from './process-context.js';
import {
  fallbackAction, fallbackChoices, fallbackFromValue, fallbackNote, fallbackValue, firstSpawn, ownFallback, profileChoices,
  setStepFallback, stepFallbackValue, whoFromValue, whoNow, whoOptions, whoValue,
} from './process-quickstart.js';
import {
  YOU_CONTINUE, forwardTargets, insertStep, moveStep, newStep, removeStep, replaceStep,
  collapseParamShorthand, setStepChoices, setStepName, setStepText, setStepWho, stepListFlow, stepListToView,
  stepTargetLabel, stepText, stepWho, backTargetLabel, backTargets, setStepLoop, stepLoop, stepAnswers, AGENT_VERDICTS,
  deciderTexts, setDeciderText, MESSAGE_MAX_RECIPIENTS, messageRecipientOptions, messageRecipientWords, setStepRecipients,
} from './process-step-list.js';
import {
  addStepDecider, agreeChoices, canAddStepDecider, isGateStep, removeStepDecider, setStepAgree, setStepDecider,
} from './process-step-gate.js';
import {
  LOOP_DEFAULT_ROUNDS, LOOP_MAX_ROUNDS, LOOP_MIN_ROUNDS, loopSummary, normalizeRounds,
} from './process-loops.js';
import { UNTITLED_PROCESS, openProcessDraft } from './process-draft-handoff.js';
import { PROCESS_SCRIBE_PROMPT_MAX } from './process-scribe.js';
import { Button, Select } from './ui-components.js';
import { TypedField } from './typed-field.js';
import { effectiveLifetime, lifetimeChoices } from './process-spawn-lifetime.js';

const html = htm.bind(h);

const GLYPHS = Object.freeze({ agent: '🤖', you: '👤', command: '⌘', message: '✉', gate: '⚖', decision: '◇', parallel: '‖' });
const VALIDATE_DELAY_MS = 400;
// A draft saves itself this long after its last change.
export const AUTOSAVE_DELAY_MS = 800;

export { TypedField };

export { UNTITLED_PROCESS };

// createdEditView is the stored template's edit view after a create, as the
// graph editor opens a stored template.
export function createdEditView(view, id, body) {
  return {
    ...view, template: { ...view.template, id },
    currentRef: String(body?.ref || ''), sourceHash: String(body?.sourceHash || ''), semanticHash: String(body?.semanticHash || ''),
    diagnostics: body?.diagnostics || [],
  };
}

// The operator's description is bounded so that, with the group context
// scribeDraftRequest appends, the request fits the scribe's prompt limit.
export const DRAFT_DESCRIPTION_MAX = 1200;
const DRAFT_NAME_MAX = 60;
const DRAFT_GROUP_CONTEXT = '\n\nThis process is for the agent group "';

// scribeDraftName names the draft template after the description's first
// line, so it is recognisable in the group and the editor.
export function scribeDraftName(text) {
  const line = String(text || '').split('\n')[0].replace(/\s+/g, ' ').trim();
  if (!line) return 'untitled process';
  return line.length <= DRAFT_NAME_MAX ? line : `${line.slice(0, DRAFT_NAME_MAX - 1).trimEnd()}…`;
}

// scribeDraftRequest is what the scribe is asked: the operator's words, then
// the group the process is for and who is in it, so the steps it drafts use
// this group's agents and roles. Members past the prompt limit are counted,
// not listed.
export function scribeDraftRequest(text, groupName, roster = []) {
  const request = String(text || '').trim().slice(0, DRAFT_DESCRIPTION_MAX);
  const head = `${request}${DRAFT_GROUP_CONTEXT}${groupName}". Draft its steps for this group's agents, using a role rather than a name wherever a role fits, and steps for me (the human) or commands where the description asks for them. Leave it as a saved draft: do not start it.`;
  const members = roster.map((member) => `- ${member.name}${member.role ? ` (role: ${member.role})` : ''}`);
  if (!members.length) return `${head}\nThe group has no members yet.`;
  let body = `${head}\nIts members:`;
  let listed = 0;
  for (const line of members) {
    const rest = members.length - listed - 1;
    const tail = rest > 0 ? `\n…and ${rest} more` : '';
    if (body.length + 1 + line.length + tail.length > PROCESS_SCRIBE_PROMPT_MAX) break;
    body += `\n${line}`;
    listed += 1;
  }
  return listed < members.length ? `${body}\n…and ${members.length - listed} more` : body;
}

export function defaultNotify(message, isError = false) {
  void import('./refresh.js').then(({ toast }) => toast(message, isError)).catch(() => {});
}

function stepKind(step) {
  if (isGateStep(step)) return 'gate';
  const mode = stepWho(step).mode;
  if (mode === 'you') return 'you';
  if (mode === 'command') return 'command';
  if (mode === 'message') return 'message';
  return 'agent';
}

// deciderStep is one decider of a gate as a step of its own, for the who
// picker and whoNow.
function deciderStep(step, index) {
  return { id: `${step.id}.vote.${index + 1}`, node: { type: 'decision', performer: step.node.deciders[index] }, cont: '', links: [] };
}

// gateNow is who decides a gate now: each decider as whoNow reads it.
export function gateNow(step, roster, options) {
  const each = step.node.deciders.map((_, index) => whoNow(deciderStep(step, index), roster, options));
  return {
    label: `${each.length} deciders · all ${step.node.agree || '…'}`,
    detail: each.map((now) => now.label).join(', '),
    waits: each.some((now) => now.waits),
    each,
  };
}

// GateDeciders edits a gate's deciders and the outcome all must choose, as
// the graph inspector does: each decider a role, anyone, the lead, an agent
// or you; + adds one, ✕ removes one (down to one, the step is a plain
// decision again).
function GateDeciders({ list, step, index, roster, roles, now, onChange }) {
  const deciders = step.node.deciders;
  const noMore = canAddStepDecider(list, step);
  const answers = stepAnswers(step);
  const agreeable = agreeChoices(step, answers);
  return html`<div class="gq-gate" data-step-deciders=${step.id}>
    ${deciders.map((_, i) => html`<div key=${i} class="gq-decider" data-step-decider=${i}>
      <span class="gq-decider-n" aria-hidden="true">${i + 1}</span>
      <${WhoPicker} step=${deciderStep(step, i)} roster=${roster} roles=${roles} spawn=${false} label=${`Decider ${i + 1} of step ${index + 1}`}
        onChange=${(who) => { if (who.mode !== 'command') onChange(setStepDecider(list, step, i, who)); }} />
      <${Button} size="sm" icon=${true} variant="ghost danger" aria-label=${`Remove decider ${i + 1} of step ${index + 1}`}
        onClick=${() => onChange(removeStepDecider(list, step, i))}>✕<//>
      <div class=${`gq-res${now.each[i]?.waits ? ' waits' : ''}`}>${now.each[i]?.detail || ''}</div>
    </div>`)}
    <${Button} size="sm" variant="link" class="gq-link" data-step-add-decider=${step.id} disabled=${!!noMore}
      title=${noMore || 'Add a decider: all must agree'} onClick=${() => onChange(addStepDecider(list, step, { mode: 'nobody' }))}>+ decider<//>
    <label class="gq-agree" data-step-agree=${step.id}>all must choose
      <${Select} size="sm" aria-label=${`The outcome every decider of step ${index + 1} must choose`} data-agree=${step.node.agree || ''} value=${step.node.agree || ''}
        onChange=${(event) => onChange(setStepAgree(list, step, event.currentTarget.value))}>
        ${!agreeable.includes(step.node.agree) && html`<option value=${step.node.agree || ''}>${step.node.agree || '…'}</option>`}
        ${agreeable.map((answer) => html`<option key=${answer} value=${answer}>${answer}</option>`)}
      <//></label>
    <div class="gq-dim gq-gate-note">Otherwise the first decider, in this order, who chose something else decides.</div>
  </div>`;
}

// `spawn` offers "a new agent…" (a step's Who, not a decider's), with the
// spawn profile picked beside it from `profiles` or the group's default.
export function WhoPicker({ step, roster, roles, label = 'Who does this step', spawn = true, rule = false, profiles = [], defaultSpawnProfile = '', onChange }) {
  const who = stepWho(step);
  const value = whoValue(who);
  // Only a task can send a message: a decision waits for an answer.
  const options = whoOptions(roster, { roles, spawn, rule, message: step?.node?.type === 'task' });
  const known = options.some((option) => option.value === value);
  const groups = [['agent', 'Specific agent'], ['role', 'By role'], ['spawn', 'New agent'], ['other', 'Not an agent']];
  const unknown = { agent: `${who.agent} (not in this group)`, role: `any ${who.role}`, spawn: 'a new agent' }[who.mode] || 'nobody yet';
  const select = html`<${Select} size="sm" class="gq-who-select" aria-label=${label} data-who=${value} value=${value}
    onChange=${(event) => onChange(whoFromValue(event.currentTarget.value))}>
    ${!known && html`<option value=${value}>${unknown}</option>`}
    ${groups.map(([key, label]) => {
      const inGroup = options.filter((option) => option.group === key);
      return inGroup.length > 0 && html`<optgroup key=${key} label=${label}>
        ${inGroup.map((option) => html`<option key=${option.value} value=${option.value} disabled=${option.disabled}>${option.label}${option.detail ? ` — ${option.detail}` : ''}</option>`)}
      </optgroup>`;
    })}
  <//>`;
  if (who.mode !== 'spawn' || !spawn) return select;
  return html`<span class="gq-pick-pair" data-who-spawn=${who.profile}>${select}
    <${ProfilePicker} value=${who.profile} defaultSpawnProfile=${defaultSpawnProfile} profiles=${profiles} label=${`${label}: spawn profile`}
      onChange=${(profile) => onChange({ mode: 'spawn', profile })} /></span>`;
}

// RecipientPicker is who a message step tells: a chip per recipient, ✕ to
// drop one, and a picker of the rest (you, the lead, the whole group, a role,
// an agent of the group).
export function RecipientPicker({ to = [], roster = [], roles = null, label = 'Tell', onChange }) {
  const rest = messageRecipientOptions(roster, roles).filter((option) => !to.includes(option.value));
  const full = to.length >= MESSAGE_MAX_RECIPIENTS;
  return html`<span class="gq-recipients" role="group" aria-label=${label} data-recipients=${to.join(' ')}>
    ${to.map((value) => html`<span key=${value} class="ui-tag gq-recipient" data-recipient=${value}>${messageRecipientWords(value)}
      <button data-ui-exempt type="button" class="gq-recipient-x" aria-label=${`Do not tell ${messageRecipientWords(value)}`}
        onClick=${() => onChange(to.filter((other) => other !== value))}>✕</button></span>`)}
    ${!full && rest.length > 0 && html`<${Select} size="sm" class="gq-recipient-add" aria-label=${`${label}: add someone`} value=""
      onChange=${(event) => { const picked = event.currentTarget.value; if (picked) onChange([...to, picked]); }}>
      <option value="">${to.length ? '+ also tell…' : 'tell who…'}</option>
      ${rest.map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
    <//>`}
  </span>`;
}

// ProfilePicker picks a spawn profile: '' is the group's default.
export function ProfilePicker({ value = '', defaultSpawnProfile = '', profiles = [], label, onChange }) {
  return html`<${Select} size="sm" class="gq-profile-select" aria-label=${label} data-profile=${value} value=${value}
    onChange=${(event) => onChange(event.currentTarget.value)}>
    ${profileChoices({ current: value, defaultSpawnProfile, profiles }).map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
  <//>`;
}

// LifetimePicker picks how long a new agent stays once spawned
// (process-spawn-lifetime.js); it shows the default when none is set.
export function LifetimePicker({ fallback, decider = false, label = 'Afterwards', onChange }) {
  return html`<${Select} size="sm" class="gq-lifetime-select" aria-label=${label} data-lifetime=${effectiveLifetime(fallback, { decider })}
    value=${effectiveLifetime(fallback, { decider })} onChange=${(event) => onChange(event.currentTarget.value)}>
    ${lifetimeChoices({ decider }).map((option) => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}
  <//>`;
}

// useSpawnProfiles reads the node's spawn profile names for the fallback
// pickers. Without them a picker still offers the group's default.
export function useSpawnProfiles(api) {
  const [profiles, setProfiles] = useState([]);
  useEffect(() => {
    let live = true;
    Promise.resolve(api?.spawnProfileNames?.()).then((names) => { if (live && names) setProfiles(names); }).catch(() => {});
    return () => { live = false; };
  }, [api]);
  return profiles;
}

// FallbackPicker chooses what happens when no member can take an agent step.
// `inherit` is the label of following the process; without it the picker is
// the process's own.
// A spawn picks its profile beside the action (ProfilePicker).
export function FallbackPicker({ value, inherit = '', defaultSpawnProfile = '', profiles = [], label, onChange }) {
  const options = fallbackChoices({ current: value, inherit, defaultSpawnProfile, profiles });
  const action = fallbackAction(value);
  const pick = (next) => onChange(next === 'spawn' ? firstSpawn({ defaultSpawnProfile, profiles }) : fallbackFromValue(next));
  const select = html`<${Select} size="sm" class="gq-fallback-select" aria-label=${label} data-fallback-value=${value} value=${action}
    onChange=${(event) => pick(event.currentTarget.value)}>
    ${options.map((option) => html`<option key=${option.value} value=${option.value} disabled=${option.disabled}>${option.label}</option>`)}
  <//>`;
  if (action !== 'spawn') return select;
  return html`<span class="gq-pick-pair">${select}
    <${ProfilePicker} value=${value.startsWith('spawn:') ? value.slice(6) : ''} defaultSpawnProfile=${defaultSpawnProfile} profiles=${profiles}
      label=${`${label}: spawn profile`} onChange=${(profile) => onChange(profile ? { action: 'spawn', profile } : { action: 'spawn' })} /></span>`;
}

// inheritLabel names what a step gets by following the process's fallback.
export function inheritLabel(fallback, defaultSpawnProfile = '') {
  const action = fallback?.action || 'wait';
  if (action === 'spawn') {
    const profile = fallback.profile || defaultSpawnProfile;
    return profile ? `as the process: spawn ${profile}` : 'as the process: wait (no default spawn profile)';
  }
  return action === 'fail' ? 'as the process: fail' : 'as the process: wait';
}

// Answers edits a decision step's answers: a person's, or the verdicts an
// agent decider picks between. Emptying an agent's verdicts makes it a plain
// agent step again; a person always keeps one answer.
function Answers({ list, step, onChange }) {
  const gate = isGateStep(step);
  const agent = step.node.performer?.kind !== 'human';
  const answers = stepAnswers(step);
  const [draft, setDraft] = useState(answers.join(', '));
  // Why the last edit of the outcomes was not taken ('' when it was).
  const [refused, setRefused] = useState('');
  useEffect(() => { setDraft(answers.join(', ')); }, [answers.join('\u0000')]);
  const index = list.steps.findIndex((s) => s.id === step.id);
  const following = list.steps[index + 1]?.id || list.mainEnd;
  const targets = forwardTargets(list, step.id);
  const commit = () => {
    const next = draft.split(',').map((answer) => answer.trim()).filter(Boolean);
    if (gate && new Set(next).size < 2) {
      setDraft(answers.join(', '));
      setRefused('A gate needs at least two outcomes: one all must choose, and one to ask for changes.');
      return;
    }
    setRefused('');
    onChange(setStepChoices(step, next.length || agent ? next : [YOU_CONTINUE], following));
  };
  const back = backTargets(list, step.id);
  const linkFor = (answer) => (answer === step.cont ? following : step.links.find((link) => link.outcome === answer)?.to || '');
  // Retargeting keeps the continuing answer continuing only while it still
  // goes to the next step. Going back to an earlier step is a bounded loop:
  // it starts at the default round limit, and going forward again drops it.
  const retarget = (answer, to) => {
    const targetOf = Object.fromEntries(answers.map((a) => [a, a === answer ? to : linkFor(a)]));
    const cont = step.cont && targetOf[step.cont] === following ? step.cont : '';
    let next = { ...step, cont, links: answers.filter((a) => a !== cont).map((a) => ({ outcome: a, to: targetOf[a] })) };
    const goesBack = back.includes(to);
    if (goesBack && !stepLoop(step, answer)) next = setStepLoop(next, answer, { maxIterations: LOOP_DEFAULT_ROUNDS, onExhausted: '' });
    if (!goesBack) next = setStepLoop(next, answer, null);
    onChange(next);
  };
  const setRounds = (answer, input) => {
    const rounds = normalizeRounds(input.value);
    const bound = stepLoop(step, answer);
    if (!bound) return;
    // Out of range: show the kept count again rather than a number that was
    // never stored (the controlled value did not change, so no render would).
    if (rounds == null) { input.value = String(bound.maxIterations); return; }
    onChange(setStepLoop(step, answer, { ...bound, maxIterations: rounds }));
  };
  const setExhausted = (answer, onExhausted) => {
    const bound = stepLoop(step, answer);
    if (bound) onChange(setStepLoop(step, answer, { ...bound, onExhausted }));
  };
  return html`<div class="gq-answers">
    <label class="gq-answers-in">${gate ? 'outcomes' : agent ? 'verdicts' : 'answers'} <input name="groups-quickstart-input" type="text" value=${draft}
      aria-label=${gate ? 'Outcomes the deciders vote between, comma separated (at least two)' : agent ? 'Verdicts the agent picks from, comma separated' : 'Answers you can give, comma separated'}
      onInput=${(event) => setDraft(event.currentTarget.value)} onBlur=${commit}
      onKeyDown=${(event) => { if (event.key === 'Enter') { event.preventDefault(); commit(); } }} /></label>
    ${refused && html`<div class="gq-problem" role="note" data-answers-refused>${refused}</div>`}
    <div class="gq-then">${answers.map((answer) => {
      const to = linkFor(answer);
      const options = [...new Set([...(to ? [to] : []), ...targets, ...back])];
      const bound = back.includes(to) ? stepLoop(step, answer) : null;
      const others = answers.filter((a) => a !== answer && !back.includes(linkFor(a)));
      return html`<div key=${answer} class="gq-then-row"><label class="gq-then-link"><span class="gq-answer">${answer}</span> →
        <${Select} size="sm" aria-label=${`After ${answer}`} value=${to} onChange=${(event) => retarget(answer, event.currentTarget.value)}>
          ${options.map((id) => html`<option key=${id} value=${id}>${back.includes(id) ? backTargetLabel(list, id) : stepTargetLabel(list, id)}</option>`)}
        <//></label>
        ${bound && html`<span class="gq-loop" data-loop=${answer}>at most
          <${TypedField} name="gq-loop-rounds" type="number" class="gq-loop-rounds" min=${LOOP_MIN_ROUNDS} max=${LOOP_MAX_ROUNDS} value=${bound.maxIterations}
            aria-label=${`Most rounds ${answer} goes back for`} onChange=${(event) => setRounds(answer, event.currentTarget)} />
          rounds, then
          <${Select} size="sm" aria-label=${`When ${answer} has used its rounds`} value=${bound.onExhausted}
            onChange=${(event) => setExhausted(answer, event.currentTarget.value)}>
            ${others.map((a) => html`<option key=${a} value=${a}>${a}</option>`)}
            ${!gate && html`<option value="">ask again</option>`}
          <//></span>`}</div>`;
    })}</div>
  </div>`;
}

function StepRow({ list, step, index, roster, roles, now, problems, fallbackInfo, onChange, onMove, onRemove }) {
  const kind = stepKind(step);
  const decision = step.node.type === 'decision';
  // A gate whose deciders are asked different things shows each one's text.
  const texts = deciderTexts(step);
  const ownTexts = texts.length > 1 && texts.some((t) => t !== texts[0]);
  const noMoreDeciders = decision && kind !== 'gate' && kind !== 'command' ? canAddStepDecider(list, step) : '';
  // Prompts read {{name}}, the step list's shorthand; edits expand it back.
  const text = collapseParamShorthand(stepText(step));
  const placeholder = { agent: 'What should the agent do? {{name}} makes a start input', you: 'What do you decide?', command: 'Command line, e.g. make test',
    message: 'What to tell them. {{name}} is a start input, {{ outputs.pr }} a step\'s output', gate: 'What do the deciders decide?' }[kind];
  const following = list.steps[index + 1]?.id || list.mainEnd;
  // A decision keeps its answers across a switch between a person and an
  // agent (an agent's lone "continue" is no decision); a command does not
  // decide.
  const setWho = (who) => {
    let next = setStepWho(step, who);
    const answers = stepAnswers(step);
    if (who.mode === 'you' && next.node.type !== 'decision') next = setStepChoices({ ...next, cont: YOU_CONTINUE }, [YOU_CONTINUE], '');
    else if (who.mode === 'you') next = setStepChoices(next, answers, following);
    else if (next.node.type === 'decision' && (who.mode === 'command' || (answers.length === 1 && answers[0] === YOU_CONTINUE))) next = setStepChoices(next, [], '');
    else if (next.node.type === 'decision') next = setStepChoices(next, answers, following);
    onChange(next);
  };
  const taskLinks = !decision && step.links.length > 0;
  return html`<div class=${`gq-step${problems.length ? ' has-problem' : ''}`} data-step=${step.id} data-step-kind=${kind}>
    <span class="gq-n">${index + 1}</span>
    <span class="gq-glyph" aria-hidden="true">${GLYPHS[kind]}</span>
    <div class="gq-step-body">
      <input name="gq-step-name" type="text" class="gq-step-name" aria-label=${`Step ${index + 1} name`} value=${step.node.name || ''} placeholder=${step.id}
        onInput=${(event) => onChange(setStepName(step, event.currentTarget.value))} />
      ${ownTexts
    ? texts.map((own, i) => html`<label key=${i} class="gq-decider-text" data-step-decider-text=${i}><span class="gq-decider-n">${i + 1}</span>
        <${TypedField} as="textarea" name="gq-decider-text" class="gq-step-text" rows="2" aria-label=${`Step ${index + 1} decider ${i + 1} instruction`} placeholder=${placeholder}
          value=${collapseParamShorthand(own)} onChange=${(event) => onChange(setDeciderText(step, i, event.currentTarget.value))} /></label>`)
    : html`<${TypedField} as="textarea" name="gq-step-text" class="gq-step-text" rows="2" aria-label=${`Step ${index + 1} ${{ command: 'command', message: 'message' }[kind] || 'instruction'}`} placeholder=${placeholder}
        title=${kind === 'gate' ? 'Every decider is asked this' : undefined}
        value=${text} onChange=${(event) => onChange(setStepText(step, event.currentTarget.value))} />`}
      ${decision && kind !== 'command' && html`<${Answers} list=${list} step=${step} onChange=${onChange} />`}
      ${!decision && kind === 'agent' && html`<${Button} size="sm" variant="link" class="gq-link gq-decides" data-step-decides=${step.id}
        title="The agent reports a verdict (approve or changes) instead of just finishing; send changes back to an earlier step for a review loop"
        onClick=${() => onChange(setStepChoices({ ...step, cont: AGENT_VERDICTS[0] }, [...AGENT_VERDICTS], following))}>◇ agent decides<//>`}
      ${taskLinks && html`<div class="gq-then gq-then-ro">then: ${step.links.map((link) => `${link.outcome} → ${stepTargetLabel(list, link.to)}`).join(' · ')}</div>`}
      ${problems.map((problem) => html`<div key=${problem} class="gq-problem" role="note">${problem}</div>`)}
    </div>
    <div class="gq-who">
      ${kind === 'gate'
    ? html`<${GateDeciders} list=${list} step=${step} index=${index} roster=${roster} roles=${roles} now=${now} onChange=${onChange} />`
    : html`<${WhoPicker} step=${step} roster=${roster} roles=${roles} profiles=${fallbackInfo.profiles}
        defaultSpawnProfile=${fallbackInfo.defaultSpawnProfile} onChange=${setWho} />
      ${kind === 'message'
    ? html`<${RecipientPicker} to=${stepWho(step).to || []} roster=${roster} roles=${roles} label=${`Step ${index + 1} tells`}
          onChange=${(to) => onChange(setStepRecipients(step, to))} />`
    : html`<div class=${`gq-res${now.waits ? ' waits' : ''}`}>${now.detail}</div>`}`}
      ${decision && kind !== 'gate' && kind !== 'command' && html`<${Button} size="sm" variant="link" class="gq-link" data-step-add-decider=${step.id}
        disabled=${!!noMoreDeciders} title=${noMoreDeciders || 'Several decide together: all must agree'}
        onClick=${() => onChange(addStepDecider(list, step, { mode: 'nobody' }))}>+ another decider<//>`}
      ${kind === 'agent' && stepWho(step).mode !== 'spawn' && html`<label class="gq-step-fallback" data-step-fallback=${step.id} title="What this step does when no member can take it">
        <span aria-hidden="true">↯</span>
        <${FallbackPicker} value=${stepFallbackValue(step)} label=${`If no member can take step ${index + 1}`}
          inherit=${inheritLabel(fallbackInfo.fallback, fallbackInfo.defaultSpawnProfile)}
          defaultSpawnProfile=${fallbackInfo.defaultSpawnProfile} profiles=${fallbackInfo.profiles}
          onChange=${(next) => onChange(setStepFallback(step, next))} />
      </label>`}
    </div>
    <div class="gq-row-act">
      <${Button} size="sm" icon=${true} aria-label=${`Move step ${index + 1} up`} disabled=${index === 0} onClick=${() => onMove(-1)}>↑<//>
      <${Button} size="sm" icon=${true} aria-label=${`Move step ${index + 1} down`} disabled=${index === list.steps.length - 1} onClick=${() => onMove(1)}>↓<//>
      <${Button} size="sm" icon=${true} variant="ghost danger" aria-label=${`Remove step ${index + 1}`} onClick=${onRemove}>✕<//>
    </div>
  </div>`;
}

// problemsByStep files server diagnostics under the step whose node path they
// name; the rest are the process's.
function problemsByStep(diagnostics, list) {
  const out = new Map(list.steps.map((step) => [step.id, []]));
  const general = [];
  for (const diagnostic of diagnostics) {
    if (diagnostic.severity !== 'error') continue;
    const match = /^nodes\.([^.]+)/.exec(diagnostic.path || '');
    const id = match?.[1] || diagnostic.targetId || '';
    if (out.has(id)) out.get(id).push(diagnostic.message);
    else general.push(diagnostic.message);
  }
  return { out, general };
}

// FlowPreview draws the step list top to bottom at the side panel's width:
// a box per step (number, kind, wrapped name, who it would use now, and its
// answers that jump elsewhere), a labelled arrow to the step below, and the
// jumps as rails on the right. Row heights are fixed per kind so the rails
// need no layout measuring; a long list scrolls with the panel.
const FLOW = Object.freeze({ cap: 26, step: 56, answers: 16, gap: 22, lane: 11, rail: 9 });

function FlowPreview({ flow, wizard }) {
  const heights = flow.rows.map((row, i) => (row.kind === 'step'
    ? FLOW.step + (flow.jumps.some((jump) => jump.from === i && jump.label) ? FLOW.answers : 0)
    : FLOW.cap));
  const tops = [];
  let y = 0;
  for (const height of heights) { tops.push(y); y += height + FLOW.gap; }
  const total = Math.max(0, y - FLOW.gap);
  const railWidth = flow.lanes ? FLOW.rail + flow.lanes * FLOW.lane : 0;
  const outs = new Map();
  const ins = new Map();
  const port = (map, row) => { const k = map.get(row) || 0; map.set(row, k + 1); return k; };
  const paths = flow.jumps.map((jump) => {
    const fromY = tops[jump.from] + Math.min(heights[jump.from] - 6, 12 + port(outs, jump.from) * 7);
    const toY = tops[jump.to] + Math.min(heights[jump.to] - 4, (flow.rows[jump.to].kind === 'step' ? 22 : 13) + port(ins, jump.to) * 7);
    const x = FLOW.rail + jump.lane * FLOW.lane;
    return { ...jump, toY, d: `M0 ${fromY} H${x} V${toY} H5` };
  });
  return html`<div class=${`gq-flow-preview${wizard ? ' wizard' : ''}`} data-step-flow>
    ${flow.rows.map((row, i) => {
      const answers = flow.jumps.filter((jump) => jump.from === i && jump.label);
      return html`<div key=${row.id} class=${`gq-flow-row ${row.kind}`} data-flow-row=${row.id} style=${`top:${tops[i]}px;height:${heights[i]}px;right:${railWidth}px`}>
        ${row.kind === 'step' ? html`
          <div class="gq-flow-head"><b>${row.number}</b> <span aria-hidden="true">${GLYPHS[stepKind(row.step)]}</span> <span class="gq-flow-name">${row.label}</span></div>
          <div class="gq-flow-who">${row.who}</div>
          ${answers.length > 0 && html`<div class="gq-flow-answers">${answers.map((jump) => html`<span key=${jump.label}>${jump.label} → ${jump.loop ? '\u21ba back to ' : ''}${flow.rows[jump.to].kind === 'step' ? flow.rows[jump.to].number : flow.rows[jump.to].label}${jump.loop ? ` (${loopSummary(jump.loop)})` : ''}</span>`)}</div>`}`
        : html`<span aria-hidden="true">${row.kind === 'start' ? '▶' : '■'}</span> ${row.label}`}
      </div>${i < flow.rows.length - 1 && row.down !== null && html`<div key=${`${row.id}-down`} class="gq-flow-down" style=${`top:${tops[i] + heights[i]}px;height:${FLOW.gap}px`}>
        <span aria-hidden="true">↓</span>${row.down && html` <em>${row.down}</em>`}</div>`}`;
    })}
    <div class="gq-flow-spacer" style=${`height:${total}px`}></div>
    ${flow.lanes > 0 && html`<svg class="gq-flow-rails" aria-hidden="true" width=${railWidth} height=${total} viewBox=${`0 0 ${railWidth} ${total}`}>
      ${paths.map((path, i) => html`<path key=${i} class=${path.back ? 'back' : ''} d=${path.d} />
        <path key=${`${i}-head`} class="head" d=${`M1 ${path.toY} l6 -4 v8 z`} />`)}
    </svg>`}
  </div>`;
}

// commandSteps are the command steps that run under a program profile ('' is
// the default profile), numbered as the list shows them.
function commandSteps(list, profile) {
  return list.steps.map((step, i) => ({ step, number: i + 1 }))
    .filter(({ step }) => step.node.performer?.kind === 'program' && String(step.node.performer.profile || '') === profile)
    .map(({ step, number }) => ({ number, command: stepText(step) }));
}

// consentSubject names what a consent checkbox allows: "step 6", "steps 3
// and 6", or "commands" when no step of the list names the profile.
function consentSubject({ steps }) {
  if (!steps.length) return 'commands';
  if (steps.length === 1) return `step ${steps[0].number}`;
  const numbers = steps.map((step) => step.number);
  return `steps ${numbers.slice(0, -1).join(', ')} and ${numbers.at(-1)}`;
}

function withName(list, name) {
  return { ...list, template: { ...list.template, name } };
}

// previewNow reads a resolve-preview answer for one step as whoNow does.
function previewNow(node) {
  if (!node) return null;
  if (node.name || node.agentId) return { label: node.name || node.agentId, detail: node.reason || '' };
  if (node.spawnProfile) return { label: `a new ${node.spawnProfile}`, detail: node.reason || '', spawns: node.spawnProfile };
  return { label: 'nobody now', detail: node.reason || '', waits: true, fails: !!node.fails };
}

// StepListEditor is the inline step-list editor (mock Q2) of one of the
// group's stored processes (`saved`: {templateId, sourceHash, ref, listed}):
// saving writes a new version of that template instead of a new one.
// `groupInfo` is the group's process context ({roles, defaultSpawnProfile}).
// A `draft` (a new process, from New process) saves itself as it changes.
export function StepListEditor({ group, roster, list: initial, origin = '', saved, draft = false, groupInfo = null, runAgent = null, api, source, wizard = false, onClose, onGraph = null, onNewer = null, notify = defaultNotify }) {
  const [list, setList] = useState(initial);
  const [name, setName] = useState(initial.template?.name || '');
  const [values, setValues] = useState({});
  const [authorized, setAuthorized] = useState(() => new Set());
  const [extraProfiles, setExtraProfiles] = useState([]);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [diagnostics, setDiagnostics] = useState([]);
  const [autosave, setAutosave] = useState({ state: '', error: '' });
  // newerKey is the view Start was refused for: a newer version of the saved
  // process exists, and these steps are not it. Editing them leaves the choice
  // behind, because a save then meets the template's own stale-version error.
  const [newerKey, setNewerKey] = useState('');
  // preview is the server's "who it will use now" for the view it was asked
  // about ({key, nodes}); a stale one is ignored until the next answer.
  const [preview, setPreview] = useState({ key: '', nodes: null });
  const view = useMemo(() => stepListToView(withName(list, name.trim())), [list, name]);
  const viewKey = JSON.stringify(view);
  // created is the template these exact steps are stored as: a saved
  // process starts out stored as opened, so starting it unchanged writes no
  // new version.
  const created = useRef(null);
  if (!created.current) {
    // `changed`: the graph view handed over unsaved edits, so these steps
    // are not what is stored.
    created.current = { key: saved.changed ? '' : viewKey, id: saved.templateId, sourceHash: saved.sourceHash || '', ref: saved.ref || '', pinned: true, listed: !!saved.listed };
  }
  const needs = templateStartRequirements(view.template);
  const programProfiles = [...new Set([...needs.programProfiles, ...extraProfiles])].sort();
  const consents = programProfiles.map((profile) => ({ profile, steps: commandSteps(list, profile) }));
  const unconsented = programProfiles.filter((profile) => !authorized.has(profile));
  useEffect(() => {
    let live = true;
    if (typeof api.validateTemplate !== 'function') return undefined;
    const timer = setTimeout(() => {
      api.validateTemplate(view).then((found) => { if (live) setDiagnostics(found); }).catch(() => { if (live) setDiagnostics([]); });
    }, VALIDATE_DELAY_MS);
    return () => { live = false; clearTimeout(timer); };
  }, [viewKey]);
  useEffect(() => {
    let live = true;
    if (typeof api.resolvePreview !== 'function') return undefined;
    const key = viewKey;
    const timer = setTimeout(() => {
      api.resolvePreview(view, group.name).then((body) => { if (live) setPreview({ key, nodes: body?.nodes || {} }); })
        .catch(() => { if (live) setPreview({ key: '', nodes: null }); });
    }, VALIDATE_DELAY_MS);
    return () => { live = false; clearTimeout(timer); };
  }, [viewKey, group.name]);
  const fallback = list.template?.fallback || null;
  const defaultSpawnProfile = String(groupInfo?.defaultSpawnProfile || '');
  const served = preview.key === viewKey ? preview.nodes : null;
  const nowOf = (step) => (isGateStep(step) ? gateNow(step, roster, { fallback, defaultSpawnProfile })
    : previewNow(served?.[step.id]) || whoNow(step, roster, { fallback, defaultSpawnProfile }));
  const roles = groupInfo?.roles || null;
  const profiles = useSpawnProfiles(api);
  const fallbackInfo = { fallback, defaultSpawnProfile, profiles };
  const { out: stepProblems, general } = problemsByStep(diagnostics, list);
  const missing = needs.params.filter((param) => param.required && !String(values[param.name] ?? '').trim());
  const hasErrors = diagnostics.some((d) => d.severity === 'error');
  const update = (next) => { setError(''); setList(next); };
  const change = (step) => update(replaceStep(list, step));
  // The process fallback (template.fallback) covers every agent step that
  // names none of its own: wait (the default, left unset), spawn the group's
  // default spawn profile, or fail the step.
  const agentSteps = list.steps.filter((step) => step.node.performer?.kind === 'agent');
  const fallbackAction = fallback?.action || 'wait';
  // A new agent's spawn is its who: the process's fallback never reaches it.
  const fallbackSteps = agentSteps.filter((step) => stepWho(step).mode !== 'spawn');
  const ownFallbacks = fallbackSteps.filter(ownFallback).length;
  const setFallback = (next) => {
    const template = { ...list.template };
    if (!next || next.action === 'wait') delete template.fallback;
    else template.fallback = next;
    update({ ...list, template });
  };
  // ensureSaved stores the steps once per edit and returns the template id.
  // The first save creates the template with {listed, pinGroup}; edits
  // after it (a refused start keeps the editor open) and a saved process
  // get a new version of that same template. A pin or a publish asked for after the steps
  // are already stored is applied to that template.
  const ensureSaved = async ({ listed, pinGroup = '' } = {}) => {
    const current = created.current;
    if (current.key !== viewKey || !current.id) {
      try {
        return await storeSteps(current, { listed, pinGroup });
      } catch (cause) {
        // Saved, but its listing or pin failed: keep that version, so a
        // retry only redoes the metadata instead of a second create or a
        // stale-hash 409.
        if (!cause.body?.ref || !cause.body?.id) throw cause;
        const head = await api.getTemplate(cause.body.id).catch(() => null);
        const listingFailed = cause.body.code === 'process_template_listing';
        created.current = {
          key: viewKey, id: String(cause.body.id), sourceHash: String(head?.sourceHash || ''), ref: String(head?.currentRef || ''), body: null,
          pinned: !!current.pinned, listed: listingFailed ? !!current.listed : (!!current.listed || !!listed),
        };
        throw cause;
      }
    }
    if (pinGroup && !current.pinned) {
      await api.pinProcess(pinGroup, current.id);
      current.pinned = true;
    }
    if (listed && !current.listed) {
      await api.publishTemplate(current.id);
      current.listed = true;
    }
    return current.id;
  };
  // storeSteps writes the steps: a new version of the template already
  // stored, or a new template.
  const storeSteps = async (current, { listed, pinGroup }) => {
    const body = await api.saveTemplate(current.id, view, { sourceHash: current.sourceHash, ...(listed ? { listed } : {}), ...(pinGroup && !current.pinned ? { pinGroup } : {}) });
    created.current = { ...current, key: viewKey, body, sourceHash: String(body?.sourceHash || ''), ref: String(body?.ref || ''), pinned: current.pinned || !!pinGroup, listed: current.listed || !!listed };
    return created.current.id;
  };
  // A draft saves itself a moment after each change; a save already under
  // way (a button's) leaves it for the next change. A name is needed first.
  useEffect(() => {
    if (!draft || busy || !name.trim() || created.current.key === viewKey) return undefined;
    const timer = setTimeout(() => {
      setAutosave({ state: 'saving', error: '' });
      ensureSaved({}).then(() => setAutosave({ state: 'saved', error: '' }))
        .catch((cause) => setAutosave({ state: 'failed', error: cause.message }));
    }, AUTOSAVE_DELAY_MS);
    return () => clearTimeout(timer);
  }, [draft, viewKey, busy]);
  const label = name.trim() || 'this process';
  // The start names the version on screen, so a save made elsewhere since it
  // opened cannot run in its place, with command consent given for other
  // steps. `reviewed` is the operator's answer to that refusal: run this
  // version anyway.
  const start = async ({ reviewed = false } = {}) => {
    if (busy || !name.trim() || missing.length || hasErrors || unconsented.length) return;
    setBusy('start');
    setError('');
    try {
      // A one-off start leaves no trace in the library or the group: only
      // Save to group pins, and the library lists only what is published.
      const templateId = await ensureSaved({});
      // Only the inputs the steps still use: a renamed {{param}} leaves its
      // old value behind in `values`.
      const params = Object.fromEntries(needs.params.filter((param) => values[param.name] !== undefined).map((param) => [param.name, values[param.name]]));
      const ref = created.current.ref;
      await api.startGroupRun({
        templateId, ...(runAgent ? { agent: runAgent.id } : { group: group.name }), params, authorizeProgramProfiles: programProfiles.filter((p) => authorized.has(p)),
        ...(ref ? { [reviewed ? 'templateRef' : 'expectedRef']: ref } : {}),
      });
      notify(runAgent ? `Started ${label} for ${runAgent.name}` : `Started ${label} in ${group.name}`);
      void source?.refresh?.();
      onClose();
    } catch (cause) {
      if (cause.body?.code === 'process_run_template_moved') {
        setNewerKey(viewKey);
        return;
      }
      const named = unauthorizedProfiles(cause.message);
      if (named.length) setExtraProfiles((current) => [...new Set([...current, ...named])]);
      setError(cause.message);
    } finally {
      setBusy('');
    }
  };
  // Reopens the editor on the head; what the operator saw there is gone, so
  // they review the newer version before it can start.
  const reviewNewer = async () => {
    if (busy) return;
    setBusy('newer');
    setError('');
    try {
      onNewer(await api.getTemplate(created.current.id));
    } catch (cause) {
      setError(cause.message);
      setBusy('');
    }
  };
  const saveToLibrary = async () => {
    if (busy || !name.trim() || hasErrors) return;
    setBusy('save');
    setError('');
    try {
      await ensureSaved({ listed: true });
      notify(`Saved ${label} to the template library`);
      void source?.refresh?.();
      onClose();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setBusy('');
    }
  };
  // saveToGroup pins the process to the group: a one-click rerun from its
  // saved processes. A new one stays out of the library until published.
  const saveToGroup = async () => {
    if (busy || !name.trim() || hasErrors) return;
    setBusy('group');
    setError('');
    try {
      await ensureSaved({ pinGroup: group.name });
      notify(`Saved ${label}`);
      void source?.refresh?.();
      onClose();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setBusy('');
    }
  };
  // The graph editor opens stored templates, so the draft is saved first.
  const openGraph = async () => {
    if (busy || !name.trim()) return;
    setBusy('graph');
    setError('');
    try {
      const id = await ensureSaved({});
      const body = created.current.body;
      await openProcessDraft(body ? { id, name: name.trim(), view: createdEditView(view, id, body) } : { id, name: name.trim() });
      onClose();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setBusy('');
    }
  };
  const blockedWhy = !name.trim() ? 'Name the process first'
    : hasErrors ? 'Fix the problems first'
      : missing.length ? `Fill ${missing.map((param) => param.name).join(', ')} first`
        : unconsented.length ? `Allow ${consentSubject(consents.find((c) => c.profile === unconsented[0]))} to run first` : '';
  const newer = !!newerKey && newerKey === viewKey;
  const whoRows = list.steps.map((step, i) => ({ step, i, now: nowOf(step) }));
  const title = wizard ? 'Rite' : 'Process';
  const tag = !draft ? `saved in ${group.name} · editing`
    : autosave.state === 'saving' ? 'draft · saving…'
      : autosave.state === 'failed' ? 'draft · not saved'
        : created.current.key === viewKey ? `draft · autosaved to ${group.name}` : 'draft';
  return html`<section class="gq-editor" aria-label=${`Edit ${label} of ${group.name}`} data-step-editor=${group.name}
    data-step-editor-saved=${saved.templateId}>
    <div class="gq-main">
      <div class="gq-top">
        <span class="gq-dim">${title}</span>
        <input name="process-name" type="text" class="gq-name" aria-label="Process name" placeholder="name it, e.g. ship-feature" value=${name}
          onInput=${(event) => setName(event.currentTarget.value)} />
        <span class="gq-tag" data-step-autosave=${draft ? autosave.state : undefined} title=${autosave.error || ''}>${tag}</span>
        ${onGraph && html`<span class="gm-toggle" role="group" aria-label="View">
          <button data-ui-exempt type="button" aria-pressed="true" class="on">☰ Steps</button>
          <button type="button" data-ui-exempt data-mini-graph title="Show the same draft as a graph"
            onClick=${() => onGraph({ view, changed: created.current.key !== viewKey, sourceHash: created.current.sourceHash, ref: created.current.ref })}>◈ Graph</button>
        </span>`}
        <span class="gq-spacer"></span>
        ${origin && html`<span class="gq-dim">from starter ${origin}</span>`}
      </div>
      ${list.steps.map((step, index) => html`<${StepRow} key=${step.id} fallbackInfo=${fallbackInfo} list=${list} step=${step} index=${index} roster=${roster} roles=${roles} now=${whoRows[index].now}
        problems=${stepProblems.get(step.id) || []} onChange=${change}
        onMove=${(delta) => update(moveStep(list, step.id, delta))} onRemove=${() => update(removeStep(list, step.id))} />`)}
      ${!list.steps.length && html`<div class="gq-empty ui-empty">No steps yet. Add the first one.</div>`}
      <div class="gq-add">
        <span>+ add step:</span>
        ${[['agent', '🤖 agent'], ['you', '👤 you'], ['command', '⌘ command'], ['message', '✉ message']].map(([kind, text]) => html`<${Button} size="sm" key=${kind} data-add-step=${kind}
          onClick=${() => update(insertStep(list, newStep(list, { kind })))}>${text}<//>`)}
      </div>
      ${fallbackSteps.length > 0 && html`<div class="gq-fallback" data-process-fallback=${fallbackAction}>
        <span aria-hidden="true">↯</span>
        <label>If no member can take an agent step:
          <${FallbackPicker} value=${fallbackValue(fallback) || 'wait'} label="When no member can take a step"
            defaultSpawnProfile=${defaultSpawnProfile} profiles=${profiles} onChange=${setFallback} /></label>
        ${ownFallbacks > 0 && html`<span class="gq-dim" data-own-fallbacks=${ownFallbacks}>· ${ownFallbacks === 1 ? '1 step sets its own' : `${ownFallbacks} steps set their own`}</span>`}
      </div>`}
    </div>
    <div class="gq-side">
      <div class="ui-eyebrow gq-sec">Preview</div>
      <${FlowPreview} flow=${stepListFlow(list, { whoOf: (step) => { const now = nowOf(step); const note = now.spawns ? '' : fallbackNote(step, fallbackInfo); return note ? `${now.label} ${note}` : now.label; } })} wizard=${wizard} />
      ${needs.params.length > 0 && html`<div class="ui-eyebrow gq-sec">Start inputs <span class="gq-dim">· found in the steps</span></div>
        <div class="gq-params">${needs.params.map((param) => html`<label key=${param.name} class="gq-param">
          <span>${param.name}${param.required ? html`<b aria-label="required"> *</b>` : ''}</span>
          <input name="groups-quickstart-input" type="text" data-start-param=${param.name} value=${values[param.name] ?? ''}
            onInput=${(event) => { const value = event.currentTarget.value; setValues((current) => ({ ...current, [param.name]: value })); }} />
        </label>`)}</div>`}
      ${consents.length > 0 && html`<div class="gq-profiles" role="group" aria-label="Allow commands to run">
        <span class="gq-dim">Commands run on this host. Start needs your consent for each:</span>
        ${consents.map(({ profile, steps }) => html`<label key=${profile || '(default)'} class="gq-profile" data-consent-profile=${profile}>
          <input name="groups-quickstart-input" type="checkbox" data-start-profile=${profile} checked=${authorized.has(profile)}
            onChange=${(event) => { const on = event.currentTarget.checked; setAuthorized((current) => { const next = new Set(current); if (on) next.add(profile); else next.delete(profile); return next; }); }} />
          <span>Allow ${consentSubject({ profile, steps })} to run${steps.length === 1 && steps[0].command ? html` <code>${steps[0].command}</code>` : ''}
            <span class="gq-dim"> (program profile: ${profile || 'default'})</span></span></label>`)}
      </div>`}
      <div class="ui-eyebrow gq-sec">Who it will use now</div>
      <ul class="gq-whonow">${whoRows.map(({ step, i, now }) => html`<li key=${step.id} class=${now.waits ? 'waits' : ''}>
        <b>${now.label}</b> · step ${i + 1}${now.waits || now.spawns ? html` · <span>${now.detail}</span>` : ''}</li>`)}
        ${!whoRows.length && html`<li class="gq-dim">nobody yet</li>`}</ul>
      ${general.length > 0 && html`<div class="gq-problems" role="alert">${general.map((problem) => html`<div key=${problem}>${problem}</div>`)}</div>`}
      ${newer && html`<div class="gq-newer" role="alert" data-step-newer>
        <p>A newer version of ${label} was saved after you opened it, so nothing started. The steps here are the older version.</p>
        <div class="gq-newer-acts">
          ${onNewer && html`<${Button} size="sm" variant="primary" data-step-newer-review disabled=${!!busy} onClick=${reviewNewer}>${busy === 'newer' ? 'opening…' : 'Review the newer version'}<//>`}
          <${Button} size="sm" data-step-newer-start disabled=${!!busy || !!blockedWhy} title=${blockedWhy || 'Run the steps shown here. The newer version stays the current one.'}
            onClick=${() => start({ reviewed: true })}>${busy === 'start' ? 'starting…' : '▶ Start this older version'}<//>
        </div>
      </div>`}
      <div class="gq-foot">
        <${Button} size="sm" variant="primary" data-step-start disabled=${!!busy || !!blockedWhy || newer} title=${newer ? 'Choose a version above' : blockedWhy}
          onClick=${() => start()}>${busy === 'start' ? 'starting…' : '▶ Start now'}<//>
        <${Button} size="sm" data-step-save-group disabled=${!!busy || !name.trim() || hasErrors}
          title=${`Save a new version of ${label}`}
          onClick=${saveToGroup}>${busy === 'group' ? 'saving…' : 'Save'}<//>
        ${!saved.listed && html`<${Button} size="sm" data-step-save-library disabled=${!!busy || !name.trim() || hasErrors}
          title="Save and list it in the template library, for every group to reuse"
          onClick=${saveToLibrary}>${busy === 'save' ? 'saving…' : 'Save and publish'}<//>`}
        <${Button} size="sm" data-step-open-graph disabled=${!!busy || !name.trim()} title=${name.trim() ? 'Save the steps as a template and open it in the full graph editor' : 'Name the process first'}
          onClick=${openGraph}>${busy === 'graph' ? 'opening…' : '⤢ open in graph editor'}<//>
        <${Button} size="sm" variant="link" class="gq-link" data-step-close onClick=${onClose}>${draft ? 'close' : 'cancel'}<//>
      </div>
      ${blockedWhy && html`<div class="gq-why" data-start-blocked>▶ ${blockedWhy}</div>`}
      <p class="gq-hint ui-hint">Start now saves any changes as a new version, then starts it in ${group.name}. Nothing runs before Start.</p>
      ${error && html`<div class="gq-error ui-error" role="alert">${error}</div>`}
    </div>
  </section>`;
}
