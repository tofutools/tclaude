// groups-quickstart.js -- the Groups › Processes quick start (mocks Q1/Q2,
// tcl-7p6bgt). GetStarted is an empty group's first-run panel: starters that
// arrive filled from the group's roster, library templates to reuse, a blank
// step list or the full graph editor. StepListEditor edits a process as
// numbered steps (process-step-list.js) with a live preview, its start
// inputs and who it would use now, and starts it in the group, saves it to
// the library, or hands it to the full graph editor. Nothing runs until
// Start.

import { h } from 'preact';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ProcessMiniGraph } from './process-kit.js';
import { templateStartRequirements, unauthorizedProfiles } from './process-context.js';
import {
  STARTERS, whoFromValue, whoNow, whoOptions, whoValue,
} from './process-quickstart.js';
import {
  YOU_CONTINUE, blankStepList, forwardTargets, insertStep, moveStep, newStep, removeStep, replaceStep,
  collapseParamShorthand, setStepChoices, setStepName, setStepText, setStepWho, stepListPreviewRun, stepListToView,
  stepTargetLabel, stepText, stepWho,
} from './process-step-list.js';
import { handOffProcessDraft } from './process-draft-handoff.js';

const html = htm.bind(h);

const GLYPHS = Object.freeze({ agent: '🤖', you: '👤', command: '⌘', decision: '◇', parallel: '‖' });
const VALIDATE_DELAY_MS = 400;
const MIN_PREVIEW_SCALE = 0.6;

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
  handOffProcessDraft({ id, name: named.template.name, view: createdEditView(named, id, body) });
  return id;
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
export function GetStarted({ group, roster, wizard = false, api, onEditList, onOpenGraph, onReuse }) {
  const [templates, setTemplates] = useState(null);
  useEffect(() => {
    let live = true;
    api.listTemplates().then((rows) => { if (live) setTemplates(rows); }).catch(() => { if (live) setTemplates([]); });
    return () => { live = false; };
  }, [api]);
  const pick = (starter) => {
    const built = starter.build(roster);
    if (built.view) onOpenGraph(built.view, starter.key);
    else onEditList(built.list, starter.title);
  };
  const shown = (templates || []).slice(0, 3);
  return html`<section class="gq-start" aria-label=${wizard ? 'Begin a first rite' : 'Get started with processes'} data-quickstart=${group.name}>
    <h3>${wizard ? `Set ${group.name}'s familiars walking a rite` : `Get ${group.name}'s agents working through a process`}</h3>
    <p class="gq-sub">A process is a set of steps. Each step is done by an agent of this group, by you, or by a command. Steps run in order, and your answers can branch them.</p>
    <div class="gq-ask">
      <input type="text" class="gq-ask-in" disabled aria-label="Describe the process in plain words"
        placeholder="Describe the flow in plain words, e.g. implement the issue, have the reviewer check it, I approve, then merge" />
      <button type="button" class="gq-btn primary" disabled title="Coming soon: a background scribe drafts the steps from your words">✨ Draft steps</button>
    </div>
    <div class="gq-sec">Or start from</div>
    <div class="gq-starters">
      ${STARTERS.map((starter) => {
        const uses = starter.build(roster).uses;
        return html`<button key=${starter.key} type="button" class="gq-starter" data-starter=${starter.key} onClick=${() => pick(starter)}>
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
      <button type="button" class="gq-btn" data-quickstart-graph onClick=${() => onOpenGraph(null, '')}>⤢ full graph editor</button>
    </div>
  </section>`;
}

function WhoPicker({ step, roster, onChange }) {
  const who = stepWho(step);
  const value = whoValue(who);
  const options = whoOptions(roster);
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

function Answers({ list, step, onChange }) {
  const answers = step.node.performer?.choices || [];
  const [draft, setDraft] = useState(answers.join(', '));
  useEffect(() => { setDraft(answers.join(', ')); }, [answers.join('\u0000')]);
  const index = list.steps.findIndex((s) => s.id === step.id);
  const following = list.steps[index + 1]?.id || list.mainEnd;
  const targets = forwardTargets(list, step.id);
  const commit = () => {
    const next = draft.split(',').map((answer) => answer.trim()).filter(Boolean);
    onChange(setStepChoices(step, next.length ? next : [YOU_CONTINUE], following));
  };
  const linkFor = (answer) => (answer === step.cont ? following : step.links.find((link) => link.outcome === answer)?.to || '');
  // Retargeting keeps the continuing answer continuing only while it still
  // goes to the next step.
  const retarget = (answer, to) => {
    const targetOf = Object.fromEntries(answers.map((a) => [a, a === answer ? to : linkFor(a)]));
    const cont = step.cont && targetOf[step.cont] === following ? step.cont : '';
    onChange({ ...step, cont, links: answers.filter((a) => a !== cont).map((a) => ({ outcome: a, to: targetOf[a] })) });
  };
  return html`<div class="gq-answers">
    <label class="gq-answers-in">answers <input type="text" value=${draft} aria-label="Answers you can give, comma separated"
      onInput=${(event) => setDraft(event.currentTarget.value)} onBlur=${commit}
      onKeyDown=${(event) => { if (event.key === 'Enter') { event.preventDefault(); commit(); } }} /></label>
    <div class="gq-then">${answers.map((answer) => {
      const to = linkFor(answer);
      const options = [...new Set([...(to ? [to] : []), ...targets])];
      return html`<label key=${answer} class="gq-then-link"><span class="gq-answer">${answer}</span> →
        <select aria-label=${`After ${answer}`} value=${to} onChange=${(event) => retarget(answer, event.currentTarget.value)}>
          ${options.map((id) => html`<option key=${id} value=${id}>${stepTargetLabel(list, id)}</option>`)}
        </select></label>`;
    })}</div>
  </div>`;
}

function StepRow({ list, step, index, roster, problems, onChange, onMove, onRemove }) {
  const kind = stepKind(step);
  const decision = step.node.type === 'decision';
  const now = whoNow(step, roster);
  // Prompts read {{name}}, the step list's shorthand; edits expand it back.
  const text = collapseParamShorthand(stepText(step));
  const placeholder = { agent: 'What should the agent do? {{name}} makes a start input', you: 'What do you decide?', command: 'Command line, e.g. make test' }[kind];
  const setWho = (who) => {
    let next = setStepWho(step, who);
    if (who.mode === 'you' && next.node.type !== 'decision') next = setStepChoices({ ...next, cont: YOU_CONTINUE }, [YOU_CONTINUE], '');
    if (who.mode !== 'you' && next.node.type === 'decision') next = setStepChoices(next, [], '');
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
      ${decision && kind === 'you' && html`<${Answers} list=${list} step=${step} onChange=${onChange} />`}
      ${taskLinks && html`<div class="gq-then gq-then-ro">then: ${step.links.map((link) => `${link.outcome} → ${stepTargetLabel(list, link.to)}`).join(' · ')}</div>`}
      ${problems.map((problem) => html`<div key=${problem} class="gq-problem" role="note">${problem}</div>`)}
    </div>
    <div class="gq-who">
      <${WhoPicker} step=${step} roster=${roster} onChange=${setWho} />
      <div class=${`gq-res${now.waits ? ' waits' : ''}`}>${now.detail}</div>
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

// FittedPreview scales the mini graph down toward the panel's width (never
// below MIN_PREVIEW_SCALE, so labels stay legible; wider graphs scroll).
function FittedPreview({ run, wizard, compact }) {
  const box = useRef(null);
  const [fit, setFit] = useState({ scale: 1, height: 0 });
  useLayoutEffect(() => {
    const graph = box.current?.querySelector?.('.pk-mini-graph');
    const width = graph?.offsetWidth || 0;
    const room = box.current?.clientWidth || 0;
    const scale = width > 0 && room > 0 && width > room ? Math.max(MIN_PREVIEW_SCALE, room / width) : 1;
    const height = Math.ceil((graph?.offsetHeight || 0) * scale);
    if (Math.abs(scale - fit.scale) > 0.001 || height !== fit.height) setFit({ scale, height });
  });
  const scaled = fit.scale < 1;
  return html`<div ref=${box} class="gq-preview" style=${scaled ? `height:${fit.height}px` : ''}>
    <div class="gq-preview-fit" style=${scaled ? `transform:scale(${fit.scale.toFixed(3)})` : ''}><${ProcessMiniGraph} run=${run} compact=${compact} wizard=${wizard} /></div>
  </div>`;
}

function withName(list, name) {
  return { ...list, template: { ...list.template, name } };
}

// StepListEditor is the inline step-list editor (mock Q2).
export function StepListEditor({ group, roster, list: initial, origin = '', api, source, wizard = false, onClose, notify = defaultNotify }) {
  const [list, setList] = useState(initial);
  const [name, setName] = useState(initial.template?.name || '');
  const [values, setValues] = useState({});
  const [authorized, setAuthorized] = useState(() => new Set());
  const [extraProfiles, setExtraProfiles] = useState([]);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [diagnostics, setDiagnostics] = useState([]);
  const created = useRef({ key: '', id: '' });
  const view = useMemo(() => stepListToView(withName(list, name.trim())), [list, name]);
  const viewKey = JSON.stringify(view);
  const needs = templateStartRequirements(view.template);
  const programProfiles = [...new Set([...needs.programProfiles, ...extraProfiles])].sort();
  useEffect(() => {
    let live = true;
    if (typeof api.validateTemplate !== 'function') return undefined;
    const timer = setTimeout(() => {
      api.validateTemplate(view).then((found) => { if (live) setDiagnostics(found); }).catch(() => { if (live) setDiagnostics([]); });
    }, VALIDATE_DELAY_MS);
    return () => { live = false; clearTimeout(timer); };
  }, [viewKey]);
  const { out: stepProblems, general } = problemsByStep(diagnostics, list);
  const missing = needs.params.filter((param) => param.required && !String(values[param.name] ?? '').trim());
  const hasErrors = diagnostics.some((d) => d.severity === 'error');
  const update = (next) => { setError(''); setList(next); };
  const change = (step) => update(replaceStep(list, step));
  const performerOf = (id) => {
    const step = list.steps.find((s) => s.id === id);
    const member = step ? whoNow(step, roster).member : null;
    return member ? { id: member.id, label: member.name } : null;
  };
  const roleSteps = list.steps.filter((step) => stepWho(step).mode === 'role');
  const spawning = roleSteps.length > 0 && roleSteps.every((step) => step.node.performer?.spawn === 'allowed');
  const setSpawning = (on) => {
    let next = list;
    for (const step of roleSteps) {
      const performer = { ...step.node.performer };
      if (on) performer.spawn = 'allowed';
      else delete performer.spawn;
      next = replaceStep(next, { ...step, node: { ...step.node, performer } });
    }
    update(next);
  };
  const ensureSaved = async (listed) => {
    if (created.current.key === viewKey && created.current.id) return created.current.id;
    const body = await api.createTemplate(view, listed === undefined ? {} : { listed });
    const id = String(body?.id || '');
    if (!id) throw new Error('the template was not created');
    created.current = { key: viewKey, id, body };
    return id;
  };
  const label = name.trim() || 'this process';
  const start = async () => {
    if (busy || !name.trim() || missing.length || hasErrors) return;
    setBusy('start');
    setError('');
    try {
      const templateId = await ensureSaved(undefined);
      await api.startGroupRun({ templateId, group: group.name, params: values, authorizeProgramProfiles: [...authorized] });
      notify(`Started ${label} in ${group.name}`);
      void source?.refresh?.();
      onClose();
    } catch (cause) {
      const named = unauthorizedProfiles(cause.message);
      if (named.length) setExtraProfiles((current) => [...new Set([...current, ...named])]);
      setError(cause.message);
    } finally {
      setBusy('');
    }
  };
  const saveToLibrary = async () => {
    if (busy || !name.trim() || hasErrors) return;
    setBusy('save');
    setError('');
    try {
      await ensureSaved(undefined);
      notify(`Saved ${label} to the template library`);
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
      const id = await ensureSaved(undefined);
      handOffProcessDraft({ id, name: name.trim(), view: createdEditView(view, id, created.current.body) });
      onClose();
    } catch (cause) {
      setError(cause.message);
    } finally {
      setBusy('');
    }
  };
  const blockedWhy = !name.trim() ? 'Name the process first'
    : hasErrors ? 'Fix the problems first'
      : missing.length ? `Fill ${missing.map((param) => param.name).join(', ')} first` : '';
  const whoRows = list.steps.map((step, i) => ({ step, i, now: whoNow(step, roster) }));
  return html`<section class="gq-editor" aria-label=${`New process for ${group.name}`} data-step-editor=${group.name}>
    <div class="gq-main">
      <div class="gq-top">
        <span class="gq-dim">${wizard ? 'New rite' : 'New process'}</span>
        <input type="text" class="gq-name" aria-label="Process name" placeholder="name it, e.g. ship-feature" value=${name}
          onInput=${(event) => setName(event.currentTarget.value)} />
        <span class="gq-tag">draft · not saved</span>
        <span class="gq-spacer"></span>
        ${origin && html`<span class="gq-dim">from starter ${origin}</span>`}
      </div>
      ${list.steps.map((step, index) => html`<${StepRow} key=${step.id} list=${list} step=${step} index=${index} roster=${roster}
        problems=${stepProblems.get(step.id) || []} onChange=${change}
        onMove=${(delta) => update(moveStep(list, step.id, delta))} onRemove=${() => update(removeStep(list, step.id))} />`)}
      ${!list.steps.length && html`<div class="gq-empty">No steps yet. Add the first one.</div>`}
      <div class="gq-add">
        <span>+ add step:</span>
        ${[['agent', '🤖 agent'], ['you', '👤 you'], ['command', '⌘ command']].map(([kind, text]) => html`<button key=${kind} type="button" class="gq-btn" data-add-step=${kind}
          onClick=${() => update(insertStep(list, newStep(list, { kind })))}>${text}</button>`)}
      </div>
      ${roleSteps.length > 0 && html`<div class="gq-fallback">
        <span aria-hidden="true">↯</span>
        <label>If nobody with the role can take an agent step:
          <select aria-label="When nobody can take a step" value=${spawning ? 'spawn' : 'wait'} onChange=${(event) => setSpawning(event.currentTarget.value === 'spawn')}>
            <option value="wait">wait for one</option>
            <option value="spawn">spawn one with the role's profile</option>
          </select></label>
      </div>`}
    </div>
    <div class="gq-side">
      <div class="gq-sec">Preview</div>
      <${FittedPreview} run=${stepListPreviewRun(list, { name: label, performerOf })} compact=${list.steps.length > 4} wizard=${wizard} />
      ${needs.params.length > 0 && html`<div class="gq-sec">Start inputs <span class="gq-dim">· found in the steps</span></div>
        <div class="gq-params">${needs.params.map((param) => html`<label key=${param.name} class="gq-param">
          <span>${param.name}${param.required ? html`<b aria-label="required"> *</b>` : ''}</span>
          <input type="text" data-start-param=${param.name} value=${values[param.name] ?? ''}
            onInput=${(event) => { const value = event.currentTarget.value; setValues((current) => ({ ...current, [param.name]: value })); }} />
        </label>`)}</div>`}
      ${programProfiles.length > 0 && html`<div class="gq-profiles" role="group" aria-label="Program authorization">
        <span class="gq-dim">Commands run on this host under:</span>
        ${programProfiles.map((profile) => html`<label key=${profile || '(default)'} class="gq-profile">
          <input type="checkbox" data-start-profile=${profile} checked=${authorized.has(profile)}
            onChange=${(event) => { const on = event.currentTarget.checked; setAuthorized((current) => { const next = new Set(current); if (on) next.add(profile); else next.delete(profile); return next; }); }} />
          authorize <code>${profile || '(default profile)'}</code></label>`)}
      </div>`}
      <div class="gq-sec">Who it will use now</div>
      <ul class="gq-whonow">${whoRows.map(({ step, i, now }) => html`<li key=${step.id} class=${now.waits ? 'waits' : ''}>
        <b>${now.label}</b> · step ${i + 1}${now.waits || now.spawns ? html` · <span>${now.detail}</span>` : ''}</li>`)}
        ${!whoRows.length && html`<li class="gq-dim">nobody yet</li>`}</ul>
      ${general.length > 0 && html`<div class="gq-problems" role="alert">${general.map((problem) => html`<div key=${problem}>${problem}</div>`)}</div>`}
      <div class="gq-foot">
        <button type="button" class="gq-btn primary" data-step-start disabled=${!!busy || !!blockedWhy} title=${blockedWhy}
          onClick=${start}>${busy === 'start' ? 'starting…' : '▶ Start now'}</button>
        <button type="button" class="gq-btn" disabled title="Coming soon: saving a process to the group, for one-click reruns">Save to ${group.name}</button>
        <button type="button" class="gq-btn" data-step-save-library disabled=${!!busy || !name.trim() || hasErrors}
          onClick=${saveToLibrary}>${busy === 'save' ? 'saving…' : 'Save to library'}</button>
        <button type="button" class="gq-btn" data-step-open-graph disabled=${!!busy || !name.trim()} title=${name.trim() ? 'Save the steps as a template and open it in the full graph editor' : 'Name the process first'}
          onClick=${openGraph}>${busy === 'graph' ? 'opening…' : '⤢ open in graph editor'}</button>
        <button type="button" class="gq-link" onClick=${onClose}>cancel</button>
      </div>
      <p class="gq-hint">Start now saves the steps as a template, then starts it in ${group.name}; the graph editor also opens it saved. Nothing runs before Start.</p>
      ${error && html`<div class="gq-error" role="alert">${error}</div>`}
    </div>
  </section>`;
}
