// process-quickstart.js -- the quick-start's pure pieces (mocks Q1/Q2,
// tcl-7p6bgt): the starters that turn a group's roster into a ready step
// list, the "Who" options a group offers, and the client-side guess of who a
// step will use right now. No DOM and no fetch; the components live in
// groups-quickstart.js.

import {
  blankStepList, insertStep, newStep, replaceStep, setStepChoices, setStepLinks, stepWho,
} from './process-step-list.js';

export function rosterMember(member) {
  return {
    name: String(member?.title || member?.agent_id || member?.conv_id || ''),
    id: String(member?.agent_id || member?.conv_id || ''),
    role: String(member?.role || ''),
    online: member?.online !== false,
  };
}

// groupRoster reads a group's members into {name, id, role, online, busy}.
export function groupRoster(group, busy = new Set()) {
  return (group?.members || []).map(rosterMember).filter((m) => m.name && m.id)
    .map((m) => ({ ...m, busy: busy.has(m.id) }));
}

// groupRoles counts the roles a roster holds, most common first.
export function groupRoles(roster) {
  const counts = new Map();
  for (const member of roster) if (member.role) counts.set(member.role, (counts.get(member.role) || 0) + 1);
  return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0], 'en')).map(([role, count]) => ({ role, count }));
}

// pickWho fills a starter slot: the first wanted role the group has, else
// another member than the ones already used, else nobody yet.
function pickWho(roster, wanted, used) {
  const roles = groupRoles(roster).map((r) => r.role);
  const role = wanted.find((w) => roles.some((r) => r.toLowerCase() === w));
  if (role) {
    const picked = roles.find((r) => r.toLowerCase() === role);
    used.add(`role:${picked.toLowerCase()}`);
    return { mode: 'role', role: picked };
  }
  // Prefer a member no earlier slot uses, by name or by role.
  const free = (m) => !used.has(m.name) && !(m.role && used.has(`role:${m.role.toLowerCase()}`));
  const member = roster.find(free) || roster.find((m) => !used.has(m.name)) || roster[0];
  if (member) {
    used.add(member.name);
    return { mode: 'agent', agent: member.name };
  }
  return { mode: 'nobody' };
}

function whoLabel(who) {
  if (who.mode === 'role') return who.role;
  if (who.mode === 'agent') return who.agent;
  return '';
}

// STARTERS are the Get started presets. `build(roster)` returns the step list
// (or, for a shape the list cannot show, a graph view to open in the editor)
// and the roles or agents it uses.
export const STARTERS = Object.freeze([
  {
    key: 'build-review-ship',
    title: 'Build → review → ship',
    chain: [['agent', 'Implement'], ['agent', 'Review'], ['you', 'Approve'], ['command', 'Merge']],
    note: 'you can ask for one rework round, then you decide',
    build(roster) {
      const used = new Set();
      const dev = pickWho(roster, ['dev', 'developer', 'implementer', 'coder'], used);
      const reviewer = pickWho(roster, ['reviewer', 'review', 'qa'], used);
      let list = blankStepList('build-review-ship');
      list = insertStep(list, newStep(list, { name: 'Implement', text: 'Implement {{issue}} on a branch and open a PR.', who: dev }));
      list = insertStep(list, newStep(list, { name: 'Review', text: 'Review the PR for {{issue}} and post your verdict and notes on it.', who: reviewer }));
      list = insertStep(list, newStep(list, { kind: 'you', name: 'Approve', text: 'Ship {{issue}}? Read the review on the PR.' }));
      list = insertStep(list, newStep(list, { name: 'Rework', text: 'Address the review notes on the PR for {{issue}}. This is the one rework round; the human decides next.', who: dev }));
      list = insertStep(list, newStep(list, { kind: 'you', name: 'Final say', text: 'Ship the reworked {{issue}}?' }));
      list = insertStep(list, newStep(list, { kind: 'command', name: 'Merge', text: 'gh pr merge --squash' }));
      const [, , approve, rework, finalSay, merge] = list.steps;
      list = replaceStep(list, setStepChoices({ ...approve, cont: '' }, ['ship', 'rework', 'hold'], merge.id));
      list = replaceStep(list, setStepLinks(list.steps[2], [{ outcome: 'ship', to: merge.id }, { outcome: 'rework', to: rework.id }, { outcome: 'hold', to: merge.id }]));
      list = withStopEnd(list, approve.id, 'hold');
      list = replaceStep(list, setStepChoices({ ...finalSay, cont: 'ship' }, ['ship', 'hold'], merge.id));
      list = withStopEnd(list, finalSay.id, 'hold');
      return { list, uses: [whoLabel(dev), whoLabel(reviewer)].filter(Boolean) };
    },
  },
  {
    key: 'plan-fan-out-merge',
    title: 'Plan → fan out → merge',
    chain: [['agent', 'Plan'], ['parallel', '×3'], ['agent', 'Integrate']],
    note: 'one lead plans, members take a part each · opens in the graph editor',
    graph: true,
    build(roster) {
      const used = new Set();
      const lead = pickWho(roster, ['lead', 'planner', 'architect'], used);
      const dev = pickWho(roster, ['dev', 'developer', 'implementer'], used);
      return { view: fanOutView(lead, dev), uses: [whoLabel(lead), whoLabel(dev) && `${whoLabel(dev)} ×3`].filter(Boolean) };
    },
  },
  {
    key: 'check-fix',
    title: 'Check → fix if red',
    chain: [['agent', 'Check & fix'], ['command', 'Verify']],
    note: 'good on a schedule (nightly)',
    build(roster) {
      const dev = pickWho(roster, ['dev', 'developer', 'implementer'], new Set());
      let list = blankStepList('check-fix');
      list = insertStep(list, newStep(list, { name: 'Check & fix', text: 'Run the tests. If any are red, fix them and push; if all are green, say so.', who: dev }));
      list = insertStep(list, newStep(list, { kind: 'command', name: 'Verify', text: 'make test' }));
      return { list, uses: [whoLabel(dev)].filter(Boolean) };
    },
  },
  {
    key: 'ask-me-first',
    title: 'Ask me first',
    chain: [['you', 'Go?'], ['agent', 'Do it'], ['you', 'Accept']],
    note: 'you give the go-ahead and sign off; one rework round if needed',
    build(roster) {
      const who = pickWho(roster, ['dev', 'developer', 'implementer'], new Set());
      let list = blankStepList('ask-me-first');
      list = insertStep(list, newStep(list, { kind: 'you', name: 'Brief', text: 'Go ahead with {{task}}?' }));
      list = insertStep(list, newStep(list, { name: 'Do it', text: 'Do {{task}}.', who }));
      list = insertStep(list, newStep(list, { kind: 'you', name: 'Accept', text: 'Accept the result of {{task}}?' }));
      list = insertStep(list, newStep(list, { name: 'Rework', text: 'The human asked for a rework of {{task}}. This is the one rework round.', who }));
      list = insertStep(list, newStep(list, { kind: 'you', name: 'Final say', text: 'Accept the reworked {{task}}?' }));
      const [brief, , accept, rework, finalSay] = list.steps;
      list = replaceStep(list, setStepChoices({ ...finalSay, cont: 'accept' }, ['accept', 'reject'], list.mainEnd));
      list = withStopEnd(list, finalSay.id, 'reject');
      list = replaceStep(list, setStepChoices({ ...brief, cont: 'go' }, ['go', 'stop'], list.mainEnd));
      list = withStopEnd(list, brief.id, 'stop');
      list = replaceStep(list, setStepChoices({ ...accept, cont: '' }, ['accept', 'rework'], list.mainEnd));
      const acceptNow = list.steps.find((s) => s.id === accept.id);
      list = replaceStep(list, setStepLinks(acceptNow, [{ outcome: 'accept', to: list.mainEnd }, { outcome: 'rework', to: rework.id }]));
      return { list, uses: [whoLabel(who)].filter(Boolean) };
    },
  },
]);

// withStopEnd points a step's `outcome` link at a canceled end, adding one.
function withStopEnd(list, stepID, outcome) {
  let id = list.ends.find((e) => e.node.result === 'canceled' && e.node.name === 'stopped')?.id;
  let next = list;
  if (!id) {
    id = 'stopped';
    for (let i = 2; list.ends.some((e) => e.id === id) || list.steps.some((st) => st.id === id); i += 1) id = `stopped-${i}`;
    next = { ...list, ends: [...list.ends, { id, node: { type: 'end', result: 'canceled', name: 'stopped' } }] };
  }
  const step = next.steps.find((s) => s.id === stepID);
  return replaceStep(next, setStepLinks(step, step.links.map((l) => (l.outcome === outcome ? { ...l, to: id } : l))));
}

function agentPerformer(who, prompt) {
  const performer = { kind: 'agent', prompt };
  if (who.mode === 'agent') performer.assignee = who.agent;
  if (who.mode === 'role') performer.profile = who.role;
  return performer;
}

// fanOutView is the plan → parallel ×3 → integrate graph. Its fan-out needs a
// parallel node, which the step list does not show yet, so it opens in the
// full editor.
function fanOutView(lead, dev) {
  const nodes = {
    start: { type: 'start' },
    plan: { type: 'task', name: 'Plan', performer: agentPerformer(lead, 'Plan {{params.goal}} as three independent parts and post them.') },
    'fan-out': { type: 'parallel', name: 'Fan out' },
    integrate: { type: 'task', name: 'Integrate', join: 'all', performer: agentPerformer(lead, 'Integrate the three parts of {{params.goal}}.') },
    end: { type: 'end' },
  };
  const edges = [
    { from: '', outcome: 'start', to: 'start' },
    { from: 'start', outcome: 'next', to: 'plan' },
    { from: 'plan', outcome: 'next', to: 'fan-out' },
  ];
  for (const part of [1, 2, 3]) {
    const id = `part-${part}`;
    nodes[id] = { type: 'task', name: `Part ${part}`, performer: agentPerformer(dev, `Do part ${part} of the plan for {{params.goal}}.`) };
    edges.push({ from: 'fan-out', outcome: `part-${part}`, to: id }, { from: id, outcome: 'next', to: 'integrate' });
  }
  edges.push({ from: 'integrate', outcome: 'next', to: 'end' });
  return {
    template: {
      apiVersion: 'tclaude.dev/v1alpha1', kind: 'ProcessTemplate', id: '', name: 'plan-fan-out-merge',
      params: { goal: { type: 'string', required: true } }, start: 'start', nodes,
    },
    edges, layout: { nodes: {} },
  };
}

// whoOptions are the Who picker's choices for a roster: specific agents, the
// roles it holds, anyone free (once the backend has it), you, and a command.
export function whoOptions(roster, { anyone = false } = {}) {
  return [
    ...roster.map((m) => ({ value: `agent:${m.name}`, group: 'agent', label: m.name, detail: `${m.role || 'member'} · ${memberState(m)}` })),
    ...groupRoles(roster).map(({ role, count }) => ({ value: `role:${role}`, group: 'role', label: `any ${role}`, detail: `${count} in group` })),
    { value: 'anyone', group: 'role', label: 'anyone free in the group', detail: anyone ? 'first idle' : 'coming soon', disabled: !anyone },
    { value: 'you', group: 'other', label: 'you (a question or sign-off)', detail: '' },
    { value: 'command', group: 'other', label: 'a command', detail: '' },
  ];
}

function memberState(member) {
  if (!member.online) return 'offline';
  return member.busy ? 'working' : 'idle';
}

// whoValue / whoFromValue map a step's Who to the picker's value and back.
export function whoValue(who) {
  if (who.mode === 'agent') return `agent:${who.agent}`;
  if (who.mode === 'role') return `role:${who.role}`;
  return who.mode;
}

export function whoFromValue(value) {
  if (value.startsWith('agent:')) return { mode: 'agent', agent: value.slice(6) };
  if (value.startsWith('role:')) return { mode: 'role', role: value.slice(5) };
  return { mode: value };
}

// whoNow guesses who a step uses if the run started now: the named agent, the
// first idle member with the role, or what happens when nobody can.
export function whoNow(step, roster) {
  const who = stepWho(step);
  const spawn = step.node?.performer?.spawn === 'allowed';
  if (who.mode === 'you') return { label: 'you', detail: 'the run waits for your answer' };
  if (who.mode === 'command') return { label: 'a command', detail: 'runs on this host' };
  if (who.mode === 'agent') {
    const member = roster.find((m) => m.name === who.agent || m.id === who.agent);
    if (!member) return { label: who.agent, detail: 'not in this group: the step waits', waits: true };
    return { label: member.name, detail: memberState(member), member };
  }
  if (who.mode === 'role') {
    const matching = roster.filter((m) => m.role.toLowerCase() === who.role.toLowerCase() && m.online);
    const member = matching.find((m) => !m.busy) || matching[0];
    if (member) return { label: member.name, detail: member.busy ? `${memberState(member)} (any ${who.role})` : `idle (any ${who.role})`, member };
    if (spawn) return { label: `a new ${who.role}`, detail: `spawns profile ${who.role}`, spawns: who.role };
    return { label: `any ${who.role}`, detail: 'nobody has the role: the step waits', waits: true };
  }
  if (who.mode === 'anyone') {
    const member = roster.find((m) => m.online && !m.busy) || roster.find((m) => m.online);
    return member ? { label: member.name, detail: 'first free member', member } : { label: 'anyone', detail: 'nobody is online: the step waits', waits: true };
  }
  return { label: 'nobody yet', detail: 'pick who does this step', waits: true };
}
