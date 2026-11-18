// agent-dependents-choice.js: the retire and delete dialogs' "also remove or
// repair what refers to the agent" row. Retiring or deleting an agent removes
// what exists only for it (its own cron jobs, standing orders and automations,
// and the cron jobs and standing orders addressed to it), cancels the runs
// scoped to it and clears template steps pinned to it. The row lists that,
// checked by default; unchecking it keeps them (keep_dependents).
import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
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
// at a time) and returns {loading, summary, failed, remote}. Each agent's
// preview is fetched once per dialog, so changing the selection only fetches
// the agents not seen yet. An agent that cannot answer is counted in failed:
// the row is advisory, the server decides. An agent on another node has no
// preview and no opt-out (the peer retire and delete take no such option), so
// the hook reports remote and fetches nothing.
export function useAgentDependents(actions, convs, node = '') {
  const key = (convs || []).join(',');
  const cache = useRef(new Map());
  const empty = { loading: false, summary: dependentsSummary([]), failed: 0, remote: !!node };
  const [state, setState] = useState(node || !key ? empty : { ...empty, loading: true });
  useEffect(() => {
    if (node || !key || typeof actions?.loadAgentDependents !== 'function') {
      setState(empty);
      return undefined;
    }
    const wanted = key.split(',');
    const fold = () => {
      const previews = [];
      let failed = 0;
      for (const conv of wanted) {
        const entry = cache.current.get(conv);
        if (entry?.ok) previews.push(entry.preview);
        else if (entry) failed += 1;
      }
      return { loading: false, summary: dependentsSummary(previews), failed, remote: false };
    };
    const queue = wanted.filter((conv) => !cache.current.has(conv));
    if (!queue.length) {
      setState(fold());
      return undefined;
    }
    const controller = new AbortController();
    const worker = async () => {
      while (queue.length && !controller.signal.aborted) {
        const conv = queue.shift();
        try {
          const preview = await actions.loadAgentDependents(conv, { signal: controller.signal });
          cache.current.set(conv, { ok: true, preview });
        } catch (_) {
          if (!controller.signal.aborted) cache.current.set(conv, { ok: false });
        }
      }
    };
    setState((current) => ({ ...current, loading: true }));
    Promise.all(Array.from({ length: Math.min(4, queue.length) }, worker)).then(() => {
      if (!controller.signal.aborted) setState(fold());
    });
    return () => controller.abort();
  }, [key, node]);
  return state;
}

// DependentsChoice is the row itself. The checkbox stays on screen while the
// previews load, so the choice that is sent is always the one shown. Nothing
// to remove or repair, and nothing unchecked, renders nothing.
export function DependentsChoice({ id, loading, summary, failed = 0, remote = false, checked, disabled, onChange }) {
  if (remote) {
    return html`<div class="delete-agent-wt" id=${`${id}-row`}>
      <span class="wt-note" id=${`${id}-label`}>On its node, this also removes what refers to the agent there (its cron jobs, standing orders and automations, the runs scoped to it and template steps pinned to it). Keeping them is only possible from that node.</span>
    </div>`;
  }
  if (!loading && !summary?.total && !failed && checked) return null;
  const lead = loading
    ? 'Also remove what refers to it (checking…)'
    : summary?.total ? `Also ${summary.text}` : 'Also remove what refers to it';
  return html`
    <label class=${`delete-agent-wt${disabled ? ' disabled' : ''}`} id=${`${id}-row`}>
      <input type="checkbox" id=${id} checked=${checked} disabled=${disabled} onChange=${onChange} />
      <span id=${`${id}-label`}>${lead}
        <span class="wt-note">Unchecked, they are kept: its own cron jobs and standing orders are disabled, the rest left as they are.${failed
          ? ` ${failed === 1 ? '1 agent' : `${failed} agents`} could not be checked; the choice applies to them too.` : ''}</span>
        ${summary?.total ? html`<details class="dependents-list" id=${`${id}-details`}>
          <summary>${summary.total === 1 ? '1 item' : `${summary.total} items`}</summary>
          <ul>${summary.lines.map((line) => html`<li>${line}</li>`)}</ul>
        </details>` : null}
      </span>
    </label>
  `;
}
