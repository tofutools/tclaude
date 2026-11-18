// agent-dependents-choice.js: the retire and delete dialogs' "also remove or
// repair what refers to the agent" row. Retiring or deleting an agent removes
// what exists only for it (its own cron jobs, standing orders and automations,
// and the cron jobs and standing orders addressed to it), cancels the runs
// scoped to it and clears template steps pinned to it. The row lists that,
// checked by default; unchecking it keeps them (keep_dependents).
import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';

const html = htm.bind(h);

const KIND_WORDS = {
  cron: ['cron job', 'cron jobs'],
  standing_order: ['standing order', 'standing orders'],
  trigger_rule: ['automation', 'automations'],
};

function count(n, [one, many]) {
  return `${n} ${n === 1 ? one : many}`;
}

// dependentsSummary folds one or more previews (GET
// /api/agents/{conv}/dependents) into what the row says: the counts per kind,
// and one line per item for the details list.
export function dependentsSummary(previews) {
  const kinds = { cron: 0, standing_order: 0, trigger_rule: 0 };
  let runs = 0;
  let steps = 0;
  const lines = [];
  for (const preview of previews || []) {
    for (const dep of preview?.dependents || []) {
      if (dep.kind in kinds) kinds[dep.kind] += 1;
      const via = dep.via === 'target' ? 'addressed to it' : 'its own';
      lines.push(`${KIND_WORDS[dep.kind]?.[0] || dep.kind} ${dep.name || `#${dep.id}`} (${via})`);
    }
    for (const run of preview?.runs || []) {
      runs += 1;
      lines.push(`run ${run.id} (${run.template_ref})`);
    }
    for (const step of preview?.pinned_steps || []) {
      steps += 1;
      lines.push(`template ${step.template} · step ${step.node} (${step.slot})`);
    }
  }
  const removes = Object.entries(kinds).filter(([, n]) => n > 0).map(([kind, n]) => count(n, KIND_WORDS[kind]));
  const parts = [];
  if (removes.length) parts.push(`remove ${removes.join(', ')}`);
  if (runs) parts.push(`cancel ${count(runs, ['run', 'runs'])} scoped to it`);
  if (steps) parts.push(`clear ${count(steps, ['template step', 'template steps'])} pinned to it`);
  return { total: lines.length, text: parts.join(' · '), lines };
}

// useAgentDependents loads the previews for the given agents (at most four
// at a time) and returns {loading, summary}. A node or agent that cannot
// answer is left out: the row is advisory, the server decides.
export function useAgentDependents(actions, convs, node = '') {
  const key = (convs || []).join(',');
  const [state, setState] = useState({ loading: !!key, summary: dependentsSummary([]) });
  useEffect(() => {
    if (!key || typeof actions?.loadAgentDependents !== 'function') {
      setState({ loading: false, summary: dependentsSummary([]) });
      return undefined;
    }
    const controller = new AbortController();
    const queue = key.split(',');
    const previews = [];
    const worker = async () => {
      while (queue.length && !controller.signal.aborted) {
        const conv = queue.shift();
        try {
          previews.push(await actions.loadAgentDependents(conv, { signal: controller.signal, node }));
        } catch (_) { /* advisory: leave it out */ }
      }
    };
    setState({ loading: true, summary: dependentsSummary([]) });
    Promise.all(Array.from({ length: Math.min(4, queue.length) }, worker)).then(() => {
      if (!controller.signal.aborted) setState({ loading: false, summary: dependentsSummary(previews) });
    });
    return () => controller.abort();
  }, [key, node]);
  return state;
}

// DependentsChoice is the row itself. Nothing to remove or repair renders
// nothing; while the previews load it says so.
export function DependentsChoice({ id, loading, summary, checked, disabled, onChange }) {
  if (loading) {
    return html`<div class="delete-agent-wt disabled" id=${`${id}-row`}>
      <span class="wt-note">Checking what refers to it…</span>
    </div>`;
  }
  if (!summary?.total) return null;
  return html`
    <label class=${`delete-agent-wt${disabled ? ' disabled' : ''}`} id=${`${id}-row`}>
      <input type="checkbox" id=${id} checked=${checked} disabled=${disabled} onChange=${onChange} />
      <span id=${`${id}-label`}>Also ${summary.text}
        <span class="wt-note">Unchecked, they are kept: its own cron jobs and standing orders are disabled, the rest left as they are.</span>
        <details class="dependents-list" id=${`${id}-details`}>
          <summary>${summary.total === 1 ? '1 item' : `${summary.total} items`}</summary>
          <ul>${summary.lines.map((line) => html`<li>${line}</li>`)}</ul>
        </details>
      </span>
    </label>
  `;
}
