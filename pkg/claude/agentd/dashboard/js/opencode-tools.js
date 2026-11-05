import { h } from 'preact';
import htm from 'htm';
import { OPENCODE_TOOLS_ALL, composeOpenCodeTools, parseOpenCodeTools } from './opencode-tools-model.js';

const html = htm.bind(h);

const TOGGLES = [
  { name: 'question', checkedWhen: 'deny', label: 'Deny the question tool',
    title: 'OpenCode\'s question tool stops and waits for your answer. Allowed by default: a detached agent that asks shows as needing you.' },
  { name: 'plan', checkedWhen: 'deny', label: 'Deny plan mode',
    title: 'OpenCode\'s plan_enter and plan_exit tools. Allowed by default.' },
  { name: 'env', checkedWhen: 'allow', label: 'Allow .env reads',
    title: 'Denied by default under all: OpenCode sub-agents cannot inherit an ask, and its explore sub-agent would read .env files without asking.' },
];

// OpenCodeToolEntries is the per-tool choices row shown under the Tool
// governance select while it is `all`.
export function OpenCodeToolEntries({ id, value, onChange, disabled = false }) {
  const { base, entries } = parseOpenCodeTools(value);
  if (base !== OPENCODE_TOOLS_ALL) return null;
  const set = (name, action) => onChange(composeOpenCodeTools(base, { ...entries, [name]: action }));
  return html`<div class="cron-create-row" id=${id}>
    <span class="cron-create-label">All tools, except</span>
    <div class="cron-create-target">
      ${TOGGLES.map((toggle) => {
        const other = toggle.checkedWhen === 'deny' ? 'allow' : 'deny';
        return html`<label class="ui-check" key=${toggle.name} title=${toggle.title}>
          <input type="checkbox" id=${`${id}-${toggle.name}`} disabled=${disabled}
            checked=${entries[toggle.name] === toggle.checkedWhen}
            onChange=${(event) => set(toggle.name, event.currentTarget.checked ? toggle.checkedWhen : other)} />
          ${toggle.label}</label>`;
      })}
    </div>
  </div>`;
}
