// groups-quickstart.js -- the Groups › Processes quick start (mocks Q1/Q2,
// tcl-7p6bgt). GetStarted is an empty group's first-run panel: starters that
// arrive filled from the group's roster, library templates to reuse, a blank
// step list or the full graph editor. StepListEditor edits a process as
// numbered steps (process-step-list.js) with a live preview, its start
// inputs and who it would use now, and starts it in the group, saves it to
// the library, or hands it to the full graph editor. Nothing runs until
// Start.

import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { templateStartRequirements, unauthorizedProfiles } from './process-context.js';
import {
  STARTERS, fallbackChoices, fallbackFromValue, fallbackNote, fallbackValue, ownFallback, setStepFallback, stepFallbackValue,
  whoFromValue, whoNow, whoOptions, whoValue,
} from './process-quickstart.js';
import {
  YOU_CONTINUE, blankStepList, forwardTargets, insertStep, moveStep, newStep, removeStep, replaceStep,
  collapseParamShorthand, setStepChoices, setStepName, setStepText, setStepWho, stepListFlow, stepListToView,
  stepTargetLabel, stepText, stepWho, backTargetLabel, backTargets, setStepLoop, stepLoop, stepAnswers, AGENT_VERDICTS,
} from './process-step-list.js';
import {
  LOOP_DEFAULT_ROUNDS, LOOP_MAX_ROUNDS, LOOP_MIN_ROUNDS, loopSummary, normalizeRounds,
} from './process-loops.js';
import { handOffProcessDraft } from './process-draft-handoff.js';
import { PROCESS_SCRIBE_PROMPT_MAX } from './process-scribe.js';
import { dashPrefs } from './prefs.js';

const html = htm.bind(h);

const GLYPHS = Object.freeze({ agent: '🤖', you: '👤', command: '⌘', decision: '◇', parallel: '‖' });
const VALIDATE_DELAY_MS = 400;

// createdEditView is the stored template's edit view after a create, as the
// graph editor opens a stored template.
function createdEditView(view, id, body) {
  return {
    ...view, template: { ...view.template, id },
    currentRef: String(body?.ref || ''), sourceHash: String(body?.sourceHash || ''), semanticHash: String(body?.semanticHash || ''),
    diagnostics: body?.diagnostics || [],
  };
}

// openInGraphEditor stores the draft as a template (the store mints its id,
// exactly as the Processes tab's create dialog does) and opens it in the full
// graph editor.
export async function openInGraphEditor(api, view, name) {
  const named = { ...view, template: { ...view.template, name: name || view.template.name || 'untitled process' } };
  const body = await api.createTemplate(named);
  const id = String(body?.id || '');
  if (!id) throw new Error('the template was not created');
  markQuickStart();
  handOffProcessDraft({ id, name: named.template.name, view: createdEditView(named, id, body) });
  return id;
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

// scribeDraftAsked reads the operator's own words back out of that request.
export function scribeDraftAsked(body) {
  return String(body || '').split(DRAFT_GROUP_CONTEXT)[0].trim();
}

// QUICKSTART_PREF remembers that the operator is past the first run: 'used'
// once they created or started a process here, 'dismissed' once they closed
// the Get started panel. Either folds an empty group's panel to one line.
export const QUICKSTART_PREF = 'tclaude.dash.groupsProcesses.getStarted';

export function quickStartSeen() {
  try { return !!dashPrefs.getItem(QUICKSTART_PREF); } catch { return false; }
}

export function markQuickStart(value = 'used') {
  try { if (!dashPrefs.getItem(QUICKSTART_PREF)) dashPrefs.setItem(QUICKSTART_PREF, value); } catch { /* best effort */ }
}

export function defaultNotify(message, isError = false) {
  void import('./refresh.js').then(({ toast }) => toast(message, isError)).catch(() => {});
}

function stepKind(step) {
  const mode = stepWho(step).mode;
  if (mode === 'you') return 'you';
  if (mode === 'command') return 'command';
  return 'agent';
}

// GetStarted is the first-run panel of a group without processes.
export function GetStarted({ group, roster, wizard = false, api, onEditList, onOpenGraph, onDraft = null, onImport = null, onReuse, onDismiss }) {
  const [templates, setTemplates] = useState(null);
  const [description, setDescription] = useState('');
  // pending holds the panel while a starter is stored for the graph editor,
  // so a double click creates one template, not two.
  const [pending, setPending] = useState(false);
  useEffect(() => {
    let live = true;
    api.listTemplates().then((rows) => { if (live) setTemplates(rows); }).catch(() => { if (live) setTemplates([]); });
    return () => { live = false; };
  }, [api]);
  const pick = (starter) => {
    const built = starter.build(roster);
    if (!built.view) { onEditList(built.list, starter.title); return; }
    if (pending) return;
    setPending(true);
    void Promise.resolve(onOpenGraph(built.view, starter.key)).finally(() => setPending(false));
  };
  const draft = (event) => {
    event?.preventDefault?.();
    const text = description.trim();
    if (!text || pending || !onDraft) return;
    setPending(true);
    void Promise.resolve(onDraft(text)).finally(() => setPending(false));
  };
  const shown = (templates || []).slice(0, 3);
  return html`<section class="gq-start" aria-label=${wizard ? 'Begin a first rite' : 'Get started with processes'} data-quickstart=${group.name}>
    <div class="gq-start-head">
      <h3>${wizard ? `Set ${group.name}'s heroes walking a rite` : `Get ${group.name}'s agents working through a process`}</h3>
      ${onDismiss && html`<button type="button" class="gq-link" data-quickstart-dismiss title="Fold this panel to one line in every empty group" onClick=${onDismiss}>✕ not now</button>`}
    </div>
    <p class="gq-sub">A process is a set of steps. Each step is done by an agent of this group, by you, or by a command. Steps run in order, and your answers can branch them.</p>
    <form class="gq-ask" data-quickstart-draft onSubmit=${draft}>
      <input type="text" class="gq-ask-in" aria-label="Describe the process in plain words" maxlength=${DRAFT_DESCRIPTION_MAX}
        disabled=${!onDraft || pending} value=${description} onInput=${(event) => setDescription(event.currentTarget.value)}
        placeholder="Describe the flow in plain words, e.g. implement the issue, have the reviewer check it, I approve, then merge" />
      <button type="submit" class="gq-btn primary" disabled=${!onDraft || pending || !description.trim()}
        title=${wizard
          ? 'Ask a process scribe, working in the background, to set down the rite\'s steps from your words. It uses a model run. You grant its access first, and you review the steps before anything is performed.'
          : 'Asks a process scribe, in the background, to draft the steps from your words. It uses a model run. You approve its access first and review the steps here; nothing runs until you press Start.'}
      >✨ Draft steps</button>
    </form>
    ${onImport && html`<p class="gq-import"><button type="button" class="gq-link" data-quickstart-import disabled=${pending} onClick=${onImport}
      title="Import a shared .pwf workflow file: you review what it runs before anything is stored, and it never starts a run">⤒ Import a .pwf</button> or drop one on this group</p>`}
    <div class="gq-sec">Or start from</div>
    <div class="gq-starters">
      ${STARTERS.map((starter) => {
        const uses = starter.build(roster).uses;
        return html`<button key=${starter.key} type="button" class="gq-starter" data-starter=${starter.key} disabled=${pending} onClick=${() => pick(starter)}>
          <span class="gq-starter-title">${starter.title}</span>
          <span class="gq-flow">${starter.chain.map(([kind, label], i) => html`${i > 0 && html`<i aria-hidden="true">→</i>`}<span>${GLYPHS[kind] || ''} ${label}</span>`)}</span>
          <span class="gq-note">${starter.note}</span>
          <span class="gq-tag">uses: ${uses.length ? uses.join(', ') : 'pick who later'}</span>
        </button>`;
      })}
    </div>
    <div class="gq-sec">Or reuse</div>
    <div class="gq-reuse">
      ${templates === null && html`<span class="gq-note">Loading the template library…</span>`}
      ${templates !== null && !shown.length && html`<span class="gq-note">The template library is empty.</span>`}
      ${shown.map((template) => html`<button key=${template.id} type="button" class="gq-chip" data-reuse-template=${template.id}
        title=${`Start ${template.name} in ${group.name}`} onClick=${() => onReuse(template.id)}>▣ ${template.name}</button>`)}
      ${shown.length > 0 && html`<span class="gq-note">from the template library ·</span>
        <button type="button" class="gq-link" onClick=${() => onReuse('')}>browse all ›</button>`}
      <span class="gq-spacer"></span>
      <button type="button" class="gq-btn" data-quickstart-blank onClick=${() => onEditList(blankStepList(''), '')}>blank step list</button>
      <button type="button" class="gq-btn" data-quickstart-graph disabled=${pending} onClick=${() => onOpenGraph(null, '')}>⤢ full graph editor</button>
    </div>
  </section>`;
}

export function WhoPicker({ step, roster, roles, onChange }) {
  const who = stepWho(step);
  const value = whoValue(who);
  const options = whoOptions(roster, { roles });
  const known = options.some((option) => option.value === value);
  const groups = [['agent', 'Specific agent'], ['role', 'By role'], ['other', 'Not an agent']];
  return html`<select class="gq-who-select" aria-label="Who does this step" data-who=${value} value=${value}
    onChange=${(event) => onChange(whoFromValue(event.currentTarget.value))}>
    ${!known && html`<option value=${value}>${who.mode === 'agent' ? `${who.agent} (not in this group)` : who.mode === 'role' ? `any ${who.role}` : 'nobody yet'}</option>`}
    ${groups.map(([key, label]) => {
      const inGroup = options.filter((option) => option.group === key);
      return inGroup.length > 0 && html`<optgroup key=${key} label=${label}>
        ${inGroup.map((option) => html`<option key=${option.value} value=${option.value} disabled=${option.disabled}>${option.label}${option.detail ? ` — ${option.detail}` : ''}</option>`)}
      </optgroup>`;
    })}
  </select>`;
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
export function FallbackPicker({ value, inherit = '', defaultSpawnProfile = '', profiles = [], label, onChange }) {
  const options = fallbackChoices({ current: value, inherit, defaultSpawnProfile, profiles });
  return html`<select class="gq-fallback-select" aria-label=${label} data-fallback-value=${value} value=${value}
    onChange=${(event) => onChange(fallbackFromValue(event.currentTarget.value))}>
    ${options.map((option) => html`<option key=${option.value} value=${option.value} disabled=${option.disabled}>${option.label}</option>`)}
  </select>`;
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
  const agent = step.node.performer?.kind !== 'human';
  const answers = stepAnswers(step);
  const [draft, setDraft] = useState(answers.join(', '));
  useEffect(() => { setDraft(answers.join(', ')); }, [answers.join('\u0000')]);
  const index = list.steps.findIndex((s) => s.id === step.id);
  const following = list.steps[index + 1]?.id || list.mainEnd;
  const targets = forwardTargets(list, step.id);
  const commit = () => {
    const next = draft.split(',').map((answer) => answer.trim()).filter(Boolean);
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
    <label class="gq-answers-in">${agent ? 'verdicts' : 'answers'} <input type="text" value=${draft}
      aria-label=${agent ? 'Verdicts the agent picks from, comma separated' : 'Answers you can give, comma separated'}
      onInput=${(event) => setDraft(event.currentTarget.value)} onBlur=${commit}
      onKeyDown=${(event) => { if (event.key === 'Enter') { event.preventDefault(); commit(); } }} /></label>
    <div class="gq-then">${answers.map((answer) => {
      const to = linkFor(answer);
      const options = [...new Set([...(to ? [to] : []), ...targets, ...back])];
      const bound = back.includes(to) ? stepLoop(step, answer) : null;
      const others = answers.filter((a) => a !== answer && !back.includes(linkFor(a)));
      return html`<div key=${answer} class="gq-then-row"><label class="gq-then-link"><span class="gq-answer">${answer}</span> →
        <select aria-label=${`After ${answer}`} value=${to} onChange=${(event) => retarget(answer, event.currentTarget.value)}>
          ${options.map((id) => html`<option key=${id} value=${id}>${back.includes(id) ? backTargetLabel(list, id) : stepTargetLabel(list, id)}</option>`)}
        </select></label>
        ${bound && html`<span class="gq-loop" data-loop=${answer}>at most
          <input type="number" class="gq-loop-rounds" min=${LOOP_MIN_ROUNDS} max=${LOOP_MAX_ROUNDS} value=${bound.maxIterations}
            aria-label=${`Most rounds ${answer} goes back for`} onChange=${(event) => setRounds(answer, event.currentTarget)} />
          rounds, then
          <select aria-label=${`When ${answer} has used its rounds`} value=${bound.onExhausted}
            onChange=${(event) => setExhausted(answer, event.currentTarget.value)}>
            ${others.map((a) => html`<option key=${a} value=${a}>${a}</option>`)}
            <option value="">ask again</option>
          </select></span>`}</div>`;
    })}</div>
  </div>`;
}

function StepRow({ list, step, index, roster, roles, now, problems, fallbackInfo, onChange, onMove, onRemove }) {
  const kind = stepKind(step);
  const decision = step.node.type === 'decision';
  // Prompts read {{name}}, the step list's shorthand; edits expand it back.
  const text = collapseParamShorthand(stepText(step));
  const placeholder = { agent: 'What should the agent do? {{name}} makes a start input', you: 'What do you decide?', command: 'Command line, e.g. make test' }[kind];
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
      <input type="text" class="gq-step-name" aria-label=${`Step ${index + 1} name`} value=${step.node.name || ''} placeholder=${step.id}
        onInput=${(event) => onChange(setStepName(step, event.currentTarget.value))} />
      <textarea class="gq-step-text" rows="2" aria-label=${`Step ${index + 1} ${kind === 'command' ? 'command' : 'instruction'}`} placeholder=${placeholder}
        value=${text} onChange=${(event) => onChange(setStepText(step, event.currentTarget.value))}></textarea>
      ${decision && kind !== 'command' && html`<${Answers} list=${list} step=${step} onChange=${onChange} />`}
      ${!decision && kind === 'agent' && html`<button type="button" class="gq-link gq-decides" data-step-decides=${step.id}
        title="The agent reports a verdict (approve or changes) instead of just finishing; send changes back to an earlier step for a review loop"
        onClick=${() => onChange(setStepChoices({ ...step, cont: AGENT_VERDICTS[0] }, [...AGENT_VERDICTS], following))}>◇ agent decides</button>`}
      ${taskLinks && html`<div class="gq-then gq-then-ro">then: ${step.links.map((link) => `${link.outcome} → ${stepTargetLabel(list, link.to)}`).join(' · ')}</div>`}
      ${problems.map((problem) => html`<div key=${problem} class="gq-problem" role="note">${problem}</div>`)}
    </div>
    <div class="gq-who">
      <${WhoPicker} step=${step} roster=${roster} roles=${roles} onChange=${setWho} />
      <div class=${`gq-res${now.waits ? ' waits' : ''}`}>${now.detail}</div>
      ${kind === 'agent' && html`<label class="gq-step-fallback" data-step-fallback=${step.id} title="What this step does when no member can take it">
        <span aria-hidden="true">↯</span>
        <${FallbackPicker} value=${stepFallbackValue(step)} label=${`If no member can take step ${index + 1}`}
          inherit=${inheritLabel(fallbackInfo.fallback, fallbackInfo.defaultSpawnProfile)}
          defaultSpawnProfile=${fallbackInfo.defaultSpawnProfile} profiles=${fallbackInfo.profiles}
          onChange=${(next) => onChange(setStepFallback(step, next))} />
      </label>`}
    </div>
    <div class="gq-row-act">
      <button type="button" aria-label=${`Move step ${index + 1} up`} disabled=${index === 0} onClick=${() => onMove(-1)}>↑</button>
      <button type="button" aria-label=${`Move step ${index + 1} down`} disabled=${index === list.steps.length - 1} onClick=${() => onMove(1)}>↓</button>
      <button type="button" aria-label=${`Remove step ${index + 1}`} onClick=${onRemove}>✕</button>
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

// StepListEditor is the inline step-list editor (mock Q2). With `saved` it
// edits one of the group's saved processes ({templateId, sourceHash}):
// saving writes a new version of that template instead of a new one.
// `groupInfo` is the group's process context ({roles, defaultSpawnProfile}).
export function StepListEditor({ group, roster, list: initial, origin = '', saved = null, groupInfo = null, runAgent = null, api, source, wizard = false, onClose, onGraph = null, onNewer = null, notify = defaultNotify }) {
  const [list, setList] = useState(initial);
  const [name, setName] = useState(initial.template?.name || '');
  const [values, setValues] = useState({});
  const [authorized, setAuthorized] = useState(() => new Set());
  const [extraProfiles, setExtraProfiles] = useState([]);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [diagnostics, setDiagnostics] = useState([]);
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
    created.current = saved
      ? { key: saved.changed ? '' : viewKey, id: saved.templateId, sourceHash: saved.sourceHash || '', ref: saved.ref || '', pinned: true, listed: !!saved.listed }
      : { key: '', id: '' };
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
  const nowOf = (step) => previewNow(served?.[step.id]) || whoNow(step, roster, { fallback, defaultSpawnProfile });
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
  const ownFallbacks = agentSteps.filter(ownFallback).length;
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
    if (current.id) {
      const body = await api.saveTemplate(current.id, view, { sourceHash: current.sourceHash, ...(listed ? { listed } : {}), ...(pinGroup && !current.pinned ? { pinGroup } : {}) });
      created.current = { ...current, key: viewKey, body, sourceHash: String(body?.sourceHash || ''), ref: String(body?.ref || ''), pinned: current.pinned || !!pinGroup, listed: current.listed || !!listed };
    } else {
      const body = await api.createTemplate(view, { ...(listed === undefined ? {} : { listed }), ...(pinGroup ? { pinGroup } : {}) });
      const id = String(body?.id || '');
      if (!id) throw new Error('the template was not created');
      created.current = { key: viewKey, id, body, sourceHash: String(body?.sourceHash || ''), pinned: !!pinGroup, listed: listed !== false };
    }
    return created.current.id;
  };
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
      const templateId = await ensureSaved(saved ? {} : { listed: false });
      // Only the inputs the steps still use: a renamed {{param}} leaves its
      // old value behind in `values`.
      const params = Object.fromEntries(needs.params.filter((param) => values[param.name] !== undefined).map((param) => [param.name, values[param.name]]));
      const ref = saved ? created.current.ref : '';
      await api.startGroupRun({
        templateId, ...(runAgent ? { agent: runAgent.id } : { group: group.name }), params, authorizeProgramProfiles: programProfiles.filter((p) => authorized.has(p)),
        ...(ref ? { [reviewed ? 'templateRef' : 'expectedRef']: ref } : {}),
      });
      markQuickStart();
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
      markQuickStart();
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
      await ensureSaved(saved ? { pinGroup: group.name } : { listed: false, pinGroup: group.name });
      markQuickStart();
      notify(saved ? `Saved ${label}` : `Saved ${label} to ${group.name}`);
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
      markQuickStart();
      const body = created.current.body;
      handOffProcessDraft(body ? { id, name: name.trim(), view: createdEditView(view, id, body) } : { id, name: name.trim() });
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
  const title = saved ? (wizard ? 'Rite' : 'Process') : (wizard ? 'New rite' : 'New process');
  return html`<section class="gq-editor" aria-label=${saved ? `Edit ${label} of ${group.name}` : `New process for ${group.name}`} data-step-editor=${group.name}
    data-step-editor-saved=${saved?.templateId}>
    <div class="gq-main">
      <div class="gq-top">
        <span class="gq-dim">${title}</span>
        <input type="text" class="gq-name" aria-label="Process name" placeholder="name it, e.g. ship-feature" value=${name}
          onInput=${(event) => setName(event.currentTarget.value)} />
        <span class="gq-tag">${saved ? `saved in ${group.name} · editing` : 'draft · not saved'}</span>
        ${onGraph && html`<span class="gm-toggle" role="group" aria-label="View">
          <button type="button" aria-pressed="true" class="on">☰ Steps</button>
          <button type="button" data-mini-graph title="Show the same draft as a graph"
            onClick=${() => onGraph({ view, changed: created.current.key !== viewKey, sourceHash: created.current.sourceHash, ref: created.current.ref })}>◈ Graph</button>
        </span>`}
        <span class="gq-spacer"></span>
        ${origin && html`<span class="gq-dim">from starter ${origin}</span>`}
      </div>
      ${list.steps.map((step, index) => html`<${StepRow} key=${step.id} fallbackInfo=${fallbackInfo} list=${list} step=${step} index=${index} roster=${roster} roles=${roles} now=${whoRows[index].now}
        problems=${stepProblems.get(step.id) || []} onChange=${change}
        onMove=${(delta) => update(moveStep(list, step.id, delta))} onRemove=${() => update(removeStep(list, step.id))} />`)}
      ${!list.steps.length && html`<div class="gq-empty">No steps yet. Add the first one.</div>`}
      <div class="gq-add">
        <span>+ add step:</span>
        ${[['agent', '🤖 agent'], ['you', '👤 you'], ['command', '⌘ command']].map(([kind, text]) => html`<button key=${kind} type="button" class="gq-btn" data-add-step=${kind}
          onClick=${() => update(insertStep(list, newStep(list, { kind })))}>${text}</button>`)}
      </div>
      ${agentSteps.length > 0 && html`<div class="gq-fallback" data-process-fallback=${fallbackAction}>
        <span aria-hidden="true">↯</span>
        <label>If no member can take an agent step:
          <${FallbackPicker} value=${fallbackValue(fallback) || 'wait'} label="When no member can take a step"
            defaultSpawnProfile=${defaultSpawnProfile} profiles=${profiles} onChange=${setFallback} /></label>
        ${ownFallbacks > 0 && html`<span class="gq-dim" data-own-fallbacks=${ownFallbacks}>· ${ownFallbacks === 1 ? '1 step sets its own' : `${ownFallbacks} steps set their own`}</span>`}
      </div>`}
    </div>
    <div class="gq-side">
      <div class="gq-sec">Preview</div>
      <${FlowPreview} flow=${stepListFlow(list, { whoOf: (step) => { const now = nowOf(step); const note = now.spawns ? '' : fallbackNote(step, fallbackInfo); return note ? `${now.label} ${note}` : now.label; } })} wizard=${wizard} />
      ${needs.params.length > 0 && html`<div class="gq-sec">Start inputs <span class="gq-dim">· found in the steps</span></div>
        <div class="gq-params">${needs.params.map((param) => html`<label key=${param.name} class="gq-param">
          <span>${param.name}${param.required ? html`<b aria-label="required"> *</b>` : ''}</span>
          <input type="text" data-start-param=${param.name} value=${values[param.name] ?? ''}
            onInput=${(event) => { const value = event.currentTarget.value; setValues((current) => ({ ...current, [param.name]: value })); }} />
        </label>`)}</div>`}
      ${consents.length > 0 && html`<div class="gq-profiles" role="group" aria-label="Allow commands to run">
        <span class="gq-dim">Commands run on this host. Start needs your consent for each:</span>
        ${consents.map(({ profile, steps }) => html`<label key=${profile || '(default)'} class="gq-profile" data-consent-profile=${profile}>
          <input type="checkbox" data-start-profile=${profile} checked=${authorized.has(profile)}
            onChange=${(event) => { const on = event.currentTarget.checked; setAuthorized((current) => { const next = new Set(current); if (on) next.add(profile); else next.delete(profile); return next; }); }} />
          <span>Allow ${consentSubject({ profile, steps })} to run${steps.length === 1 && steps[0].command ? html` <code>${steps[0].command}</code>` : ''}
            <span class="gq-dim"> (program profile: ${profile || 'default'})</span></span></label>`)}
      </div>`}
      <div class="gq-sec">Who it will use now</div>
      <ul class="gq-whonow">${whoRows.map(({ step, i, now }) => html`<li key=${step.id} class=${now.waits ? 'waits' : ''}>
        <b>${now.label}</b> · step ${i + 1}${now.waits || now.spawns ? html` · <span>${now.detail}</span>` : ''}</li>`)}
        ${!whoRows.length && html`<li class="gq-dim">nobody yet</li>`}</ul>
      ${general.length > 0 && html`<div class="gq-problems" role="alert">${general.map((problem) => html`<div key=${problem}>${problem}</div>`)}</div>`}
      ${newer && html`<div class="gq-newer" role="alert" data-step-newer>
        <p>A newer version of ${label} was saved after you opened it, so nothing started. The steps here are the older version.</p>
        <div class="gq-newer-acts">
          ${onNewer && html`<button type="button" class="gq-btn primary" data-step-newer-review disabled=${!!busy} onClick=${reviewNewer}>${busy === 'newer' ? 'opening…' : 'Review the newer version'}</button>`}
          <button type="button" class="gq-btn" data-step-newer-start disabled=${!!busy || !!blockedWhy} title=${blockedWhy || 'Run the steps shown here. The newer version stays the current one.'}
            onClick=${() => start({ reviewed: true })}>${busy === 'start' ? 'starting…' : '▶ Start this older version'}</button>
        </div>
      </div>`}
      <div class="gq-foot">
        <button type="button" class="gq-btn primary" data-step-start disabled=${!!busy || !!blockedWhy || newer} title=${newer ? 'Choose a version above' : blockedWhy}
          onClick=${() => start()}>${busy === 'start' ? 'starting…' : '▶ Start now'}</button>
        <button type="button" class="gq-btn" data-step-save-group disabled=${!!busy || !name.trim() || hasErrors}
          title=${saved ? `Save a new version of ${label}` : `Save to ${group.name}'s processes, for one-click reruns; the template library lists it only once you publish it`}
          onClick=${saveToGroup}>${busy === 'group' ? 'saving…' : saved ? 'Save' : `Save to ${group.name}`}</button>
        ${!saved?.listed && html`<button type="button" class="gq-btn" data-step-save-library disabled=${!!busy || !name.trim() || hasErrors}
          title=${saved ? 'Save and list it in the template library, for every group to reuse' : 'Save it to the template library, for every group to reuse'}
          onClick=${saveToLibrary}>${busy === 'save' ? 'saving…' : saved ? 'Save and publish' : 'Save to library'}</button>`}
        <button type="button" class="gq-btn" data-step-open-graph disabled=${!!busy || !name.trim()} title=${name.trim() ? 'Save the steps as a template and open it in the full graph editor' : 'Name the process first'}
          onClick=${openGraph}>${busy === 'graph' ? 'opening…' : '⤢ open in graph editor'}</button>
        <button type="button" class="gq-link" onClick=${onClose}>cancel</button>
      </div>
      ${blockedWhy && html`<div class="gq-why" data-start-blocked>▶ ${blockedWhy}</div>`}
      <p class="gq-hint">${saved
    ? `Start now saves any changes as a new version, then starts it in ${group.name}. Nothing runs before Start.`
    : `Start now runs the steps once in ${group.name} without adding them to its processes or the library; Save to ${group.name} keeps them for one-click reruns. Nothing runs before Start.`}</p>
      ${error && html`<div class="gq-error" role="alert">${error}</div>`}
    </div>
  </section>`;
}
