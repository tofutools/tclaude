// process-step-gate.js -- an agreement gate in the step list (tk_bd629fb5):
// the graph inspector's gate edits (process-mini-edit.js) applied to one step
// of a list. Pure: no DOM, no fetch.
//
// A gate edit reads only the step's node and its own outcomes, and changes
// only the node, so each runs on a view of that one step and the step takes
// the node back. Its outcomes and their targets stay the list's.
import {
  addDecider, canAddDecider, deciderWho, gateDeciders, removeDecider, setAgree, setDecider,
} from './process-mini-edit.js';
import { loopBound } from './process-loops.js';

export { deciderWho, gateDeciders };

// isGateStep says whether a step is an agreement gate.
export function isGateStep(step) {
  return step?.node?.type === 'decision' && (step.node.deciders?.length || 0) > 0;
}

function stepView(list, step) {
  const index = list.steps.findIndex((s) => s.id === step.id);
  const following = list.steps[index + 1]?.id || list.mainEnd;
  const edges = [
    ...(step.cont && following ? [{ from: step.id, outcome: step.cont, to: following }] : []),
    ...step.links.map((link) => ({ from: step.id, outcome: link.outcome, to: link.to })),
  ];
  return { template: { nodes: { [step.id]: step.node } }, edges, layout: {} };
}

function apply(list, step, edit) {
  const { view } = edit(stepView(list, step), step.id);
  return { ...step, node: view.template.nodes[step.id] };
}

// canAddStepDecider says why a step cannot take another decider, or ''.
export function canAddStepDecider(list, step) {
  return canAddDecider(stepView(list, step), step.id);
}

// addStepDecider gives a decision step another decider: a plain decision
// becomes a gate.
export function addStepDecider(list, step, who) {
  return apply(list, step, (view, id) => addDecider(view, id, who));
}

// removeStepDecider drops decider `index`; down to one, the gate is a plain
// decision again.
export function removeStepDecider(list, step, index) {
  return apply(list, step, (view, id) => removeDecider(view, id, index));
}

// setStepDecider makes decider `index` who, keeping what it is asked.
export function setStepDecider(list, step, index, who) {
  return apply(list, step, (view, id) => setDecider(view, id, index, who));
}

// setStepAgree sets the outcome every decider must choose.
export function setStepAgree(list, step, outcome) {
  return apply(list, step, (view, id) => setAgree(view, id, outcome));
}

// agreeChoices are the outcomes a gate can ask all to choose: those that do
// not go back.
export function agreeChoices(step, answers) {
  return answers.filter((answer) => !loopBound(step.node, answer));
}
