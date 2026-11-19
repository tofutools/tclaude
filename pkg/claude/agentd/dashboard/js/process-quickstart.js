// process-quickstart.js -- the quick-start's pure pieces (mocks Q1/Q2,
// tcl-7p6bgt): a group's roster, the "Who" options it offers, and the
// client-side guess of who a step will use right now. (A new process starts
// from the built-in ways of working the daemon serves, tk_d02b0f4c.) No DOM and no fetch; the components live in
// groups-quickstart.js.

import { messageRecipientWords, stepWho } from './process-step-list.js';
import { keepLifetime, withLifetime } from './process-spawn-lifetime.js';

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

// whoOptions are the Who picker's choices for a roster: specific agents, the
// roles the group holds (`roles`, the process context's [{role, count}], else
// the roster's), anyone free, you, and a command.
export function whoOptions(roster, { roles = null, spawn = true, rule = false, message = false } = {}) {
  return [
    ...roster.map((m) => ({ value: `agent:${m.name}`, group: 'agent', label: m.name, detail: `${m.role || 'member'} · ${memberState(m)}` })),
    ...(roles?.length ? roles : groupRoles(roster)).map(({ role, count }) => ({ value: `role:${role}`, group: 'role', label: `any ${role}`, detail: `${count} in group` })),
    { value: 'anyone', group: 'role', label: 'anyone free in the group', detail: 'idle first, then the least busy' },
    { value: 'lead', group: 'role', label: "the group's lead", detail: 'the member with role lead, else an owner' },
    ...(spawn ? [{ value: 'spawn', group: 'spawn', label: 'a new agent…', detail: '' }] : []),
    { value: 'you', group: 'other', label: 'you (a question or sign-off)', detail: '' },
    { value: 'command', group: 'other', label: 'a command', detail: '' },
    // A task can send a message and go on (model.PerformerMessage).
    ...(message ? [{ value: 'message', group: 'other', label: 'a message', detail: 'tell someone, then go on' }] : []),
    // A decision can decide by itself (process-rules.js).
    ...(rule ? [{ value: 'rule', group: 'other', label: 'a rule', detail: 'decides by itself, nobody is asked' }] : []),
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
  if (value === 'spawn') return { mode: 'spawn', profile: '' };
  if (value.startsWith('agent:')) return { mode: 'agent', agent: value.slice(6) };
  if (value.startsWith('role:')) return { mode: 'role', role: value.slice(5) };
  return { mode: value };
}

// stepFallback is what an agent step does when no member can take it, in the
// runtime's order (engine.AgentCommandFor): its own fallback; else the
// legacy spawn opt-in, its own choice (spawn its profile, or wait without
// one); else the process's; else wait.
export function stepFallback(step, templateFallback = null) {
  const performer = step?.node?.performer || {};
  if (performer.fallback?.action) return { action: performer.fallback.action, profile: performer.fallback.profile || '' };
  if (performer.spawn === 'allowed') return performer.profile ? { action: 'spawn', profile: performer.profile } : { action: 'wait', profile: '' };
  if (templateFallback?.action) return { action: templateFallback.action, profile: templateFallback.profile || '' };
  return { action: 'wait', profile: '' };
}

// ownFallback says an agent step decides its own fallback, so the process's
// does not apply to it.
export function ownFallback(step) {
  const performer = step?.node?.performer || {};
  return !!performer.fallback?.action || performer.spawn === 'allowed';
}

// fallbackValue / fallbackFromValue map a fallback to a picker's value and
// back: '' for none, 'wait', 'fail', 'spawn' for the group's default spawn
// profile, and 'spawn:<profile>' for a named one.
export function fallbackValue(fallback) {
  if (!fallback?.action) return '';
  return fallback.action === 'spawn' && fallback.profile ? `spawn:${fallback.profile}` : fallback.action;
}

export function fallbackFromValue(value) {
  if (!value) return null;
  if (value.startsWith('spawn:')) return { action: 'spawn', profile: value.slice(6) };
  return { action: value };
}

// stepFallbackValue is the picker's value for a step's own fallback. A legacy
// spawn opt-in reads as what it does.
export function stepFallbackValue(step) {
  const performer = step?.node?.performer || {};
  if (!ownFallback(step)) return '';
  return fallbackValue(performer.fallback?.action ? performer.fallback : stepFallback(step));
}

// setStepFallback returns the step with its own fallback set, or cleared so
// the process's applies. The legacy spawn field goes with it: the template
// validator refuses a step carrying both.
export function setStepFallback(step, fallback) {
  const node = JSON.parse(JSON.stringify(step.node));
  const performer = node.performer || {};
  const before = performer.fallback;
  delete performer.fallback;
  if (fallback || performer.spawn === 'allowed') delete performer.spawn;
  if (fallback) performer.fallback = keepLifetime(before, fallback);
  return { ...step, node };
}

// setStepLifetime sets the lifetime of a step's own spawn fallback, as
// picked: the default clears it (withLifetime), which keepLifetime must not
// undo.
export function setStepLifetime(step, lifetime) {
  const node = JSON.parse(JSON.stringify(step.node));
  if (node.performer?.fallback?.action !== 'spawn') return step;
  node.performer.fallback = withLifetime(node.performer.fallback, lifetime);
  return { ...step, node };
}

// fallbackChoices lists a fallback picker's actions: `inherit` adds the
// per-step choice of following the process, then wait, spawn and fail. Which
// profile a spawn uses is profileChoices', beside it. Spawning needs a
// profile: the group's default or a named one, unless it already spawns.
export function fallbackChoices({ current = '', inherit = '', defaultSpawnProfile = '', profiles = [] } = {}) {
  const spawnable = !!defaultSpawnProfile || profiles.some(Boolean) || current.startsWith('spawn');
  return [
    ...(inherit ? [{ value: '', label: inherit }] : []),
    { value: 'wait', label: 'wait for one' },
    { value: 'spawn', label: spawnable ? 'spawn a new agent' : 'spawn a new agent (no spawn profiles)', disabled: !spawnable },
    { value: 'fail', label: 'fail the step' },
  ];
}

// fallbackAction is a picker value's action ('spawn' for 'spawn:<profile>').
export function fallbackAction(value) {
  return String(value || '').startsWith('spawn') ? 'spawn' : String(value || '');
}

// profileChoices lists a spawn's profiles: '' for the group's default spawn
// profile, then the node's profiles. The current one stays listed even if it
// no longer exists, so opening a step never rewrites it.
export function profileChoices({ current = '', defaultSpawnProfile = '', profiles = [] } = {}) {
  const options = [
    { value: '', label: defaultSpawnProfile ? `group default (${defaultSpawnProfile})` : 'group default (none set)' },
    ...[...new Set(profiles)].filter(Boolean).map((name) => ({ value: name, label: name })),
  ];
  if (current && !options.some((option) => option.value === current)) options.push({ value: current, label: `${current} (not on this node)` });
  return options;
}

// firstSpawn is the fallback a picker switched to spawn starts on: the
// group's default, else the first profile, since with neither nothing spawns.
export function firstSpawn({ defaultSpawnProfile = '', profiles = [] } = {}) {
  const named = profiles.find(Boolean);
  return !defaultSpawnProfile && named ? { action: 'spawn', profile: named } : { action: 'spawn' };
}

// fallbackNote is the short mark an agent step's node carries when it does
// not simply wait, so what happens without a member reads off the graph.
export function fallbackNote(step, { fallback = null, defaultSpawnProfile = '' } = {}) {
  // A new agent's spawn is its who, not a fallback.
  if (step?.node?.performer?.kind !== 'agent' || stepWho(step).mode === 'spawn') return '';
  const { action, profile } = stepFallback(step, fallback);
  if (action === 'fail') return '↯ fail';
  // A bare spawn in a group with no default spawn profile waits at runtime.
  if (action === 'spawn' && (profile || defaultSpawnProfile)) return `↯ spawn ${profile || defaultSpawnProfile}`;
  return '';
}

// whoNow guesses who a step uses if the run started now: the named agent, the
// first idle member with the role, or what its fallback does when nobody can.
// The server's resolve-preview replaces this guess once it answers.
export function whoNow(step, roster, { fallback = null, defaultSpawnProfile = '' } = {}) {
  const who = stepWho(step);
  const nobody = (label, why) => {
    const { action, profile } = stepFallback(step, fallback);
    const spawned = profile || defaultSpawnProfile;
    if (action === 'spawn' && spawned) return { label: `a new ${spawned}`, detail: `${why}: spawns profile ${spawned}`, spawns: spawned };
    if (action === 'fail') return { label, detail: `${why}: the step fails`, waits: true, fails: true };
    return { label, detail: `${why}: the step waits`, waits: true };
  };
  if (who.mode === 'you') return { label: 'you', detail: 'the run waits for your answer' };
  if (who.mode === 'command') return { label: 'a command', detail: 'runs on this host' };
  if (who.mode === 'message') return { label: 'a message', detail: `to ${(who.to || []).map(messageRecipientWords).join(', ') || 'nobody yet'}` };
  if (who.mode === 'agent') {
    const member = roster.find((m) => m.name === who.agent || m.id === who.agent);
    if (!member) return nobody(who.agent, 'not in this group');
    return { label: member.name, detail: memberState(member), member };
  }
  if (who.mode === 'role') {
    const matching = roster.filter((m) => m.role.toLowerCase() === who.role.toLowerCase() && m.online);
    const member = matching.find((m) => !m.busy) || matching[0];
    if (member) return { label: member.name, detail: member.busy ? `${memberState(member)} (any ${who.role})` : `idle (any ${who.role})`, member };
    return nobody(`any ${who.role}`, 'nobody has the role');
  }
  if (who.mode === 'lead') {
    const member = roster.find((m) => m.role.toLowerCase() === 'lead' && m.online);
    return member ? { label: member.name, detail: "the group's lead", member }
      : { label: "the group's lead", detail: 'an owner of the group, when no member has the role lead' };
  }
  if (who.mode === 'spawn') {
    const spawned = nobody('a new agent', 'a new agent for every group run');
    return spawned.waits ? { label: 'a new agent', detail: 'the group has no default spawn profile: the step waits', waits: true } : spawned;
  }
  if (who.mode === 'anyone') {
    const member = roster.find((m) => m.online && !m.busy) || roster.find((m) => m.online);
    return member ? { label: member.name, detail: member.busy ? 'least busy member' : 'first free member', member } : nobody('anyone', 'nobody is online');
  }
  return { label: 'nobody yet', detail: 'pick who does this step', waits: true };
}
