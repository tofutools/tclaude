// process-node-form.js -- pure form logic for the node edit dialogs
// (TCL-298). No DOM, no fetch: Node's test runner exercises the exact file
// shipped to the browser (jstest/process-node-form.test.mjs).
//
// The uniform performer contract (design §2) is a locked principle: every
// slot (work, plan, checks, review, decision decider) accepts the same
// human | agent | program spectrum, and the dialog renders ONE shared
// performer editor keyed by kind — never a special case per kind. The
// PERFORMER_FIELDS table below is the single place kind-specific fields are
// declared. Discipline rule: adding a field for one kind means defining it
// for all three or explicitly kind-scoping it in the Go model first
// (model/types.go + validate.go + schema.go), then mirroring it here.
//
// All helpers mutate a DRAFT node inside ProcessEditModel.updateNode's gate;
// they never touch the live model themselves.

export const PERFORMER_KINDS = ['human', 'agent', 'program'];

export const RETRY_ON_FAIL_MODES = ['feedback-same-session', 'fresh-attempt'];

export const PLAN_APPROVAL_MODES = ['auto', 'human'];

// One descriptor per performer field. `kinds` scopes a field; a field on
// every kind is common. `list` fields hold string arrays (edited one item
// per line), `multiline` fields render as textareas. `hint` is UI help text
// rendered escape-safe by the dialog.
export const PERFORMER_FIELDS = [
  { key: 'profile', label: 'profile', kinds: PERFORMER_KINDS, hint: 'Named profile: which human role / agent spawn profile / program context performs this slot' },
  { key: 'ask', label: 'ask', kinds: ['human'], multiline: true, hint: 'The question put to the human' },
  { key: 'choices', label: 'choices', kinds: ['human'], list: true, hint: 'Optional closed answer set, one per line' },
  { key: 'assignee', label: 'assignee', kinds: ['human'], hint: 'Optional specific person; defaults to whoever holds the profile' },
  { key: 'prompt', label: 'prompt', kinds: ['agent', 'human'], multiline: true, hint: 'Instruction text (agents) or long-form context (humans)' },
  { key: 'model', label: 'model', kinds: ['agent'], hint: 'Optional model override on top of the profile' },
  { key: 'effort', label: 'effort', kinds: ['agent'], hint: 'Optional reasoning-effort override on top of the profile' },
  // Program performers are command execution (design §10) — the dialog
  // surfaces that as an explicit security note next to these fields.
  { key: 'run', label: 'command', kinds: ['program'], hint: 'Program performers are command execution: this command runs on the host when the node activates' },
  { key: 'args', label: 'arguments', kinds: ['program'], list: true, hint: 'Command arguments, one per line (never shell-parsed)' },
  { key: 'timeout', label: 'timeout', kinds: PERFORMER_KINDS, hint: 'Optional Go duration (30s, 5m, 1h30m)' },
];

export function performerFieldsFor(kind) {
  return PERFORMER_FIELDS.filter((field) => field.kinds.includes(kind));
}

// defaultPerformer mints a minimal slot; the required kind-specific field
// (ask/prompt/run) stays absent until the author fills it — saves are
// advisory, so the missing-field diagnostic guides without blocking.
export function defaultPerformer(kind) {
  if (!PERFORMER_KINDS.includes(kind)) throw new Error(`unknown performer kind ${kind}`);
  return { kind };
}

// setPerformerKind switches a slot's kind and prunes fields the new kind does
// not define — a stale agent prompt must not ride along invisibly on a
// program performer (the Go model rejects wrong-kind fields as authoring
// errors). Common fields (profile, timeout, contact) survive the switch.
export function setPerformerKind(performer, kind) {
  if (!PERFORMER_KINDS.includes(kind)) throw new Error(`unknown performer kind ${kind}`);
  performer.kind = kind;
  for (const field of PERFORMER_FIELDS) {
    if (!field.kinds.includes(kind) && field.key in performer) delete performer[field.key];
  }
}

// setPerformerField assigns one descriptor-declared field; blank values (and
// empty lists) delete the key so omitempty keeps the YAML minimal.
export function setPerformerField(performer, key, value) {
  const field = PERFORMER_FIELDS.find((candidate) => candidate.key === key);
  if (!field) throw new Error(`unknown performer field ${key}`);
  if (field.list) {
    const list = Array.isArray(value) ? value : parseLines(value);
    if (list.length) performer[key] = list;
    else delete performer[key];
    return;
  }
  const text = (value || '').trim();
  if (text) performer[key] = text;
  else delete performer[key];
}

// setContactField edits the per-slot contact schedule (cadence, budget,
// escalation target). A schedule with every field blank is removed entirely
// (nil = the performer kind's runtime default).
export function setContactField(performer, key, value) {
  if (!['cadence', 'budget', 'escalationTarget'].includes(key)) throw new Error(`unknown contact field ${key}`);
  const contact = { ...(performer.contact || {}) };
  if (key === 'budget') {
    const budget = parsePositiveInt(value);
    if (budget) contact.budget = budget;
    else delete contact.budget;
  } else {
    const text = (value || '').trim();
    if (text) contact[key] = text;
    else delete contact[key];
  }
  if (Object.keys(contact).length) performer.contact = contact;
  else delete performer.contact;
}

// setRetryField edits a retry policy attached to `holder` (a node or a
// step). Clearing both fields removes the policy.
export function setRetryField(holder, key, value) {
  if (!['maxAttempts', 'onFail', 'backoff'].includes(key)) throw new Error(`unknown retry field ${key}`);
  const retry = { ...(holder.retry || {}) };
  if (key === 'maxAttempts') {
    const attempts = parsePositiveInt(value);
    if (attempts) retry.maxAttempts = attempts;
    else delete retry.maxAttempts;
  } else {
    const text = (value || '').trim();
    if (text) retry[key] = text;
    else delete retry[key];
  }
  if (Object.keys(retry).length) holder.retry = retry;
  else delete holder.retry;
}

// setStageEnabled toggles the optional plan/review stages. Enabling mints a
// step with a sensible default performer: plan inherits the work performer's
// profile (same agent population plans and does); a review gate defaults to
// a bare human slot — the work performer's profile is usually an agent spawn
// profile and would mislabel the human reviewer. Disabling drops the step.
export function setStageEnabled(node, stage, enabled) {
  if (stage !== 'plan' && stage !== 'review') throw new Error(`unknown stage ${stage}`);
  if (!enabled) {
    delete node[stage];
    return;
  }
  if (node[stage]) return;
  const performer = defaultPerformer(stage === 'review' ? 'human' : 'agent');
  if (stage === 'plan' && node.performer?.profile) performer.profile = node.performer.profile;
  if (stage === 'review') performer.ask = 'Approve?';
  const step = { id: stage === 'review' ? 'review' : 'plan', performer };
  if (stage === 'plan') step.approval = 'auto';
  node[stage] = step;
}

export function setPlanApproval(node, approval) {
  if (!node.plan) throw new Error('plan stage is not enabled');
  if (!PLAN_APPROVAL_MODES.includes(approval)) throw new Error(`invalid plan approval ${approval}`);
  node.plan.approval = approval;
  if (approval !== 'human') delete node.plan.approvalRetry;
}

// Checks are an ordered list; ids stay unique within the node because the
// engine derives child-stage ids from them.
export function addCheck(node, kind = 'program') {
  const checks = node.checks || [];
  const taken = new Set(checks.map((check) => check.id));
  let id = 'check';
  for (let n = 2; taken.has(id); n += 1) id = `check-${n}`;
  checks.push({ id, performer: defaultPerformer(kind) });
  node.checks = checks;
  return id;
}

export function removeCheck(node, index) {
  const checks = node.checks || [];
  if (index < 0 || index >= checks.length) throw new Error(`no check at ${index}`);
  checks.splice(index, 1);
  if (!checks.length) delete node.checks;
}

// moveCheck reorders (checks order is semantic — it feeds the derived child
// stages and the semantic hash). delta is ±1.
export function moveCheck(node, index, delta) {
  const checks = node.checks || [];
  const target = index + delta;
  if (index < 0 || index >= checks.length) throw new Error(`no check at ${index}`);
  if (target < 0 || target >= checks.length) return;
  const [check] = checks.splice(index, 1);
  checks.splice(target, 0, check);
}

export function setCheckID(node, index, id) {
  const checks = node.checks || [];
  if (index < 0 || index >= checks.length) throw new Error(`no check at ${index}`);
  const text = (id || '').trim();
  if (!text) throw new Error('check id is required');
  if (checks.some((check, i) => i !== index && check.id === text)) throw new Error(`duplicate check id ${text}`);
  checks[index].id = text;
}

// setCaptures replaces the node's published capture names (task nodes only;
// the Go model validates the scoping). Order-preserving de-dup keeps the
// dialog forgiving about repeated lines.
export function setCaptures(node, value) {
  const names = [];
  for (const name of Array.isArray(value) ? value : parseLines(value)) {
    if (!names.includes(name)) names.push(name);
  }
  if (names.length) node.captures = names;
  else delete node.captures;
}

// setWaitField edits a wait node's config; clearing every field removes it.
export function setWaitField(node, key, value) {
  if (!['duration', 'until', 'signal'].includes(key)) throw new Error(`unknown wait field ${key}`);
  const wait = { ...(node.wait || {}) };
  const text = (value || '').trim();
  if (text) wait[key] = text;
  else delete wait[key];
  if (Object.keys(wait).length) node.wait = wait;
  else delete node.wait;
}

// setNodeText edits the shared prose fields (name/description/doc) present on
// every node type; blank deletes.
export function setNodeText(node, key, value) {
  if (!['name', 'description', 'doc'].includes(key)) throw new Error(`unknown node text field ${key}`);
  const text = (value || '').trim();
  if (text) node[key] = text;
  else delete node[key];
}

// parsePositiveInt accepts only a whole positive decimal integer; anything
// else ("2.5", "3oops", "1e2") returns 0 so the caller clears the field
// instead of silently persisting a truncated number the author never typed.
function parsePositiveInt(value) {
  const text = String(value ?? '').trim();
  if (!/^\d+$/.test(text)) return 0;
  const n = Number(text);
  return Number.isSafeInteger(n) && n > 0 ? n : 0;
}

export function parseLines(text) {
  return String(text || '')
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.length > 0);
}

export function formatLines(list) {
  return (list || []).join('\n');
}
