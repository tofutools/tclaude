// process-rules.js: a rule decision's conditions in the dashboard. A rule
// decides a decision by its cases (performer.kind "rule": cases of
// {when, then}, and else), with nobody asked; the engine's language is in
// pkg/claude/process/model/rule.go, and this is the same grammar: comparisons
// `<operand> <op> <value>` joined by and, or and not, with parentheses, where
// a lone operand means it is true.
//
// Here it is parsed for two things: plain words ("merge is yes and CI
// succeeded"), and the editor's condition rows, which hold a condition that
// is only comparisons joined by and.

const OPS = ['==', '!=', '<=', '>=', '<', '>'];

// OP_WORDS are the rows' names for the comparisons.
export const OP_WORDS = { '==': 'is', '!=': 'is not', '<': 'is below', '<=': 'is at most', '>': 'is above', '>=': 'is at least' };

// STEP_RESULTS are what steps.<id>.result can be.
export const STEP_RESULTS = ['succeeded', 'failed', 'skipped'];

function lex(text) {
  const tokens = [];
  const s = String(text || '');
  for (let i = 0; i < s.length;) {
    const c = s[i];
    if (/\s/.test(c)) { i++; continue; }
    if (c === '(' || c === ')') { tokens.push({ text: c }); i++; continue; }
    if (c === '"' || c === "'") {
      const end = s.indexOf(c, i + 1);
      if (end < 0) throw new Error('a quoted value is not closed');
      tokens.push({ text: s.slice(i + 1, end), quoted: true });
      i = end + 1;
      continue;
    }
    if ('=!<>'.includes(c)) {
      const op = s[i + 1] === '=' ? c + '=' : c;
      if (!OPS.includes(op)) throw new Error(`unknown operator ${op}`);
      tokens.push({ text: op });
      i += op.length;
      continue;
    }
    let end = i;
    while (end < s.length && !/\s/.test(s[end]) && !'()=!<>"\''.includes(s[end])) end++;
    tokens.push({ text: s.slice(i, end) });
    i = end;
  }
  return tokens;
}

// parseOperand reads params.x, steps.x.result, steps.x.exitCode, outputs.x.
export function parseOperand(text) {
  const parts = String(text || '').split('.');
  if (parts.length === 2 && (parts[0] === 'params' || parts[0] === 'outputs') && parts[1]) return { kind: parts[0], name: parts[1] };
  if (parts.length === 3 && parts[0] === 'steps' && parts[1] && (parts[2] === 'result' || parts[2] === 'exitCode')) return { kind: parts[2], name: parts[1] };
  return null;
}

export function operandText(operand) {
  return operand.kind === 'result' || operand.kind === 'exitCode' ? `steps.${operand.name}.${operand.kind}` : `${operand.kind}.${operand.name}`;
}

// parseCondition returns the condition's tree, or throws on what the engine
// would refuse: {op: 'and'|'or', left, right}, {op: 'not', inner}, or a
// comparison {operand, cmp, value, bare}.
export function parseCondition(text) {
  const tokens = lex(text);
  if (!tokens.length) throw new Error('the condition is empty');
  let at = 0;
  const peek = () => tokens[at];
  const keyword = (word) => {
    const token = peek();
    if (token && !token.quoted && token.text.toLowerCase() === word) { at++; return true; }
    return false;
  };
  const or = () => {
    let left = and();
    while (keyword('or')) left = { op: 'or', left, right: and() };
    return left;
  };
  const and = () => {
    let left = not();
    while (keyword('and')) left = { op: 'and', left, right: not() };
    return left;
  };
  const not = () => (keyword('not') ? { op: 'not', inner: not() } : primary());
  const primary = () => {
    const token = peek();
    if (!token) throw new Error('the condition ends too early');
    if (!token.quoted && token.text === '(') {
      at++;
      const inner = or();
      if (peek()?.text !== ')' || peek()?.quoted) throw new Error('a ( is not closed');
      at++;
      return inner;
    }
    at++;
    const operand = token.quoted ? null : parseOperand(token.text);
    if (!operand) throw new Error(`${token.text} is not something a rule can read`);
    const op = peek();
    if (!op || (!op.quoted && (op.text === ')' || ['and', 'or'].includes(op.text.toLowerCase())))) return { operand, cmp: '==', value: 'true', bare: true };
    if (op.quoted || !OPS.includes(op.text)) throw new Error(`${token.text} needs a comparison`);
    at++;
    const value = peek();
    if (!value || (!value.quoted && ['(', ')', ...OPS].includes(value.text))) throw new Error(`${token.text} ${op.text} needs a value`);
    at++;
    return { operand, cmp: op.text, value: value.text };
  };
  const tree = or();
  if (at < tokens.length) throw new Error(`unexpected ${tokens[at].text}`);
  return tree;
}

// toRows is a condition as the editor's rows (comparisons joined by and),
// or null when it uses or, not or parentheses the rows cannot show.
export function toRows(text) {
  let tree;
  try {
    tree = parseCondition(text);
  } catch {
    return null;
  }
  const rows = [];
  const walk = (node) => {
    if (node.op === 'and') return walk(node.left) && walk(node.right);
    if (node.op) return false;
    rows.push({ operand: operandText(node.operand), cmp: node.cmp, value: node.value });
    return true;
  };
  return walk(tree) ? rows : null;
}

function quote(value) {
  const text = String(value ?? '');
  if (/^[A-Za-z0-9_.+-]+$/.test(text) && !['and', 'or', 'not'].includes(text.toLowerCase())) return text;
  return text.includes('"') ? `'${text}'` : `"${text}"`;
}

// fromRows writes the editor's rows back as a condition.
export function fromRows(rows) {
  return rows.map((row) => `${row.operand} ${row.cmp} ${quote(row.value)}`).join(' and ');
}

// operandWords names what an operand reads: the param or output by name,
// a step by its name.
export function operandWords(operand, stepName = (id) => id) {
  const parsed = typeof operand === 'string' ? parseOperand(operand) : operand;
  if (!parsed) return String(operand);
  if (parsed.kind === 'result') return stepName(parsed.name);
  if (parsed.kind === 'exitCode') return `${stepName(parsed.name)} exit code`;
  return parsed.name;
}

function comparisonWords(node, stepName) {
  const who = operandWords(node.operand, stepName);
  if (node.bare) return who;
  if (node.operand.kind === 'result') {
    const did = { succeeded: ['succeeded', 'did not succeed'], failed: ['failed', 'did not fail'], skipped: ['was skipped', 'was not skipped'] }[node.value];
    if (did && node.cmp === '==') return `${who} ${did[0]}`;
    if (did && node.cmp === '!=') return `${who} ${did[1]}`;
  }
  return `${who} ${OP_WORDS[node.cmp]} ${node.value}`;
}

// conditionWords is a condition in plain words: "merge is yes and CI
// succeeded". What does not parse is shown as written.
export function conditionWords(text, stepName = (id) => id) {
  let tree;
  try {
    tree = parseCondition(text);
  } catch {
    return String(text || '');
  }
  const words = (node, parent) => {
    if (node.op === 'not') return `not ${words(node.inner, 'not')}`;
    if (node.op === 'and' || node.op === 'or') {
      const inner = `${words(node.left, node.op)} ${node.op} ${words(node.right, node.op)}`;
      return parent && parent !== node.op ? `(${inner})` : inner;
    }
    return comparisonWords(node, stepName);
  };
  return words(tree, '');
}

// ruleWords is a whole rule in plain words: each case and its outcome, then
// what happens otherwise.
export function ruleWords(rule, stepName = (id) => id) {
  const cases = (rule?.cases || []).map((c) => `${conditionWords(c.when, stepName)} → ${c.then}`);
  if (rule?.else) cases.push(`otherwise → ${rule.else}`);
  return cases.join(' · ');
}

// choiceWords is what a rule chose, in plain words, and the values it read:
// { said: "merge is yes and CI succeeded → merge", read: "merge = yes · CI = succeeded" }.
export function choiceWords(rule, stepName = (id) => id) {
  const choice = rule?.choice;
  if (!choice) return null;
  const when = choice.case >= 0 ? rule.cases?.[choice.case]?.when : '';
  const said = `${when ? conditionWords(when, stepName) : 'otherwise'} → ${choice.outcome}`;
  const read = Object.entries(choice.read || {}).map(([operand, value]) => `${operandWords(operand, stepName)} = ${value}`).join(' · ');
  return { said, read };
}

// ruleOperands are what a rule at id can read, for the rows' first picker:
// the results (and a command's exit code) of the steps before it, nearest
// first, the outputs they report, and the process's params. Each has the
// values it can take when they are few (a step's result, a boolean param),
// or null for free text. view is {template, edges}: an inline editor view
// or the full editor's model.
export function ruleOperands(view, id) {
  const nodes = view.template.nodes;
  // Breadth-first from the rule, so the step just before it comes first.
  const distance = new Map();
  let frontier = [id];
  for (let hops = 1; frontier.length; hops += 1) {
    const next = [];
    for (const at of frontier) {
      for (const edge of view.edges) {
        if (edge.to === at && edge.from && edge.from !== id && !distance.has(edge.from)) {
          distance.set(edge.from, hops);
          next.push(edge.from);
        }
      }
    }
    frontier = next;
  }
  const upstream = [...distance.keys()].sort((a, b) => distance.get(a) - distance.get(b) || a.localeCompare(b));
  const name = (stepID) => nodes[stepID]?.name || stepID;
  const operands = [];
  for (const stepID of upstream) {
    const step = nodes[stepID];
    if (step?.type !== 'task') continue;
    operands.push({ value: `steps.${stepID}.result`, label: `${name(stepID)} · result`, values: STEP_RESULTS });
    if (step.performer?.kind === 'program' && !step.plan && !step.checks?.length && !step.review) {
      operands.push({ value: `steps.${stepID}.exitCode`, label: `${name(stepID)} · exit code`, values: null });
    }
  }
  for (const stepID of upstream) {
    for (const output of nodes[stepID]?.captures || []) operands.push({ value: `outputs.${output}`, label: `output · ${output}`, values: null });
  }
  for (const [param, spec] of Object.entries(view.template.params || {}).sort(([a], [b]) => a.localeCompare(b))) {
    const boolean = spec?.type === 'boolean' || spec?.type === 'bool';
    operands.push({ value: `params.${param}`, label: `param · ${param}`, values: boolean ? ['true', 'false'] : null });
  }
  return operands;
}

// seedRule is a new rule's cases and else for decision id: one case on the
// nearest thing it can read, picking its first outcome, and else its next
// way that does not loop back (loopsBack(outcome)).
export function seedRule(view, id, loopsBack = () => false) {
  const outcomes = view.edges.filter((edge) => edge.from === id).map((edge) => edge.outcome);
  const first = ruleOperands(view, id)[0];
  const when = first ? `${first.value} == ${first.values?.[0] || 'yes'}` : '';
  const ways = outcomes.filter((outcome) => !loopsBack(outcome));
  return { cases: [{ when, then: outcomes[0] || '' }], else: ways.find((o) => o !== outcomes[0]) || ways[0] || outcomes[0] || '' };
}

// blankRow is the row "+ and" or "+ another case" adds: the nearest operand
// the case does not read yet.
export function blankRow(operands, rows = []) {
  const used = new Set(rows.map((row) => row.operand));
  const pick = operands.find((operand) => !used.has(operand.value)) || operands[0];
  return { operand: pick?.value || '', cmp: '==', value: pick?.values?.[0] || 'yes' };
}

// The rows' edits, as RuleCases makes them, on a rule ({cases, else}); each
// returns the new rule. A case whose condition the rows cannot show is left
// alone by the row edits.
const mapCase = (rule, index, change) => ({
  ...rule, cases: (rule.cases || []).map((c, i) => (i === index && toRows(c.when) ? { ...c, ...change(c) } : c)),
});

// withRow changes one row of case `index`: picking what it reads also
// picks that operand's first value when it has few.
export function withRow(rule, operands, index, row, change) {
  return mapCase(rule, index, (c) => {
    const rows = toRows(c.when) || [];
    const picked = change.operand && operands.find((o) => o.value === change.operand);
    rows[row] = { ...rows[row], ...change, ...(picked?.values && change.value === undefined ? { value: picked.values[0], cmp: '==' } : {}) };
    return { when: fromRows(rows) };
  });
}

// withoutRow drops one row of case `index`.
export function withoutRow(rule, index, row) {
  return mapCase(rule, index, (c) => ({ when: fromRows((toRows(c.when) || []).filter((_, j) => j !== row)) }));
}

// withAnd is a case's "+ and": a row on the nearest thing it does not read yet.
export function withAnd(rule, operands, index) {
  return mapCase(rule, index, (c) => {
    const rows = toRows(c.when) || [];
    return { when: fromRows([...rows, blankRow(operands, rows)]) };
  });
}

// withCase is "+ another case", picking `then`.
export function withCase(rule, operands, then) {
  return { ...rule, cases: [...(rule.cases || []), { when: fromRows([blankRow(operands)]), then }] };
}
