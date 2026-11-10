// OpenCode's tool-governance value is a base mode, optionally followed — after
// `all` only — by per-tool entries: `all,question=deny,plan=deny,env=allow`.
// The Go side (harness/opencode_tools.go) owns the grammar and canonical
// order; this mirrors it so a select can show the base and checkboxes the
// entries. Defaults: question and plan allowed, .env reads denied.
export const OPENCODE_TOOLS_ALL = 'all';

const ENTRY_DEFAULTS = Object.freeze({ question: 'allow', plan: 'allow', env: 'deny' });
const ENTRY_ORDER = ['question', 'plan', 'env'];

export function openCodeToolsBase(value) {
  return String(value || '').split(',')[0].trim();
}

export function parseOpenCodeTools(value) {
  const [base, ...rest] = String(value || '').split(',').map((part) => part.trim());
  const entries = { ...ENTRY_DEFAULTS };
  for (const field of rest) {
    const [name, action] = field.split('=').map((part) => part.trim());
    if (name in entries && (action === 'allow' || action === 'deny')) entries[name] = action;
  }
  return { base, entries };
}

// composeOpenCodeTools renders the canonical spelling: entries only after
// `all`, only when they differ from their default, in the Go side's order.
export function composeOpenCodeTools(base, entries = {}) {
  if (base !== OPENCODE_TOOLS_ALL) return base;
  const parts = [base];
  for (const name of ENTRY_ORDER) {
    const action = entries[name] || ENTRY_DEFAULTS[name];
    if (action !== ENTRY_DEFAULTS[name]) parts.push(`${name}=${action}`);
  }
  return parts.join(',');
}

// withOpenCodeToolsBase changes the base and keeps the entries when the new
// base still carries them.
export function withOpenCodeToolsBase(value, base) {
  return composeOpenCodeTools(base, parseOpenCodeTools(value).entries);
}

