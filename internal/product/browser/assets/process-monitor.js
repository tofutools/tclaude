import {ProcessGraph} from './processgraph/process-graph.js';
import {graphView} from './process-model.js';

// Only durable application states decorate this view. No browser timer or
// graph reachability is treated as execution or verification evidence.
export async function openProcessMonitor(id, {api, el, button, openDecisions}) {
  const dialog = el('dialog'); dialog.className = 'process-monitor';
  const header = el('header'), title = el('h2', 'Process ' + id), status = el('p');
  const body = el('div', undefined, 'process-monitor-body'), canvas = el('div', undefined, 'process-monitor-canvas'), detail = el('aside');
  const error = el('p', undefined, 'error'); error.hidden = true;
  header.append(title, button('Refresh process', load), button('Close process', () => dialog.close()));
  body.append(canvas, detail); dialog.append(header, status, error, body); document.body.append(dialog);
  let widget, result, selected, closed = false, generation = 0;
  dialog.addEventListener('close', () => { closed = true; widget?.destroy(); dialog.remove(); }, {once: true});
  dialog.showModal();
  async function load() {
    const request = ++generation;
    try {
      const next = await api('/v2/work/' + encodeURIComponent(id));
      if (closed || request !== generation) return;
      result = next; error.hidden = true;
      if (!result.run.graph) throw new Error('This work request has no process graph.');
      status.textContent = `${result.run.state} · ${result.run.control_state || ''} · ${result.run.outcome || 'outcome pending'} · revision ${result.run.revision}`;
      const graph = graphView({Process: {Graph: result.run.graph}});
      for (const node of graph.nodes) {
        const attempts = (result.run.node_attempts || []).filter(a => a.Ref.NodeID === node.id);
        const states = [...new Set(attempts.map(a => a.State))];
        node.overlay = {label: states.join(', ') || 'not activated', attempts: attempts.length};
      }
      if (widget) widget.setGraph(graph);
      else widget = new ProcessGraph(canvas, graph, {ariaLabel: 'Process execution graph', onNodeClick: event => { selected = event.node.id; showNode(); }});
      showNode();
    } catch (e) { if (!closed && request === generation) { error.textContent = e.message; error.hidden = false; } }
  }
  function showNode() {
    detail.replaceChildren();
    const run = result.run, node = run.graph.Nodes.find(n => n.ID === selected);
    if (!node) {
      detail.append(el('h3', 'Run details'), el('p', 'Select a node to inspect every activation, attempt, decision and evidence record.'));
      for (const ref of run.definition_closure || []) detail.append(el('p', `Pinned ${ref.DefinitionID} · ${ref.RevisionID}`));
      detail.append(el('p', `Deadline ${new Date(run.deadline).toLocaleString()}`)); return;
    }
    detail.append(el('h3', node.Name || node.ID), el('p', `${node.ID} · ${node.Kind}`));
    const attempts = (run.node_attempts || []).filter(a => a.Ref.NodeID === node.ID);
    if (!attempts.length) detail.append(el('p', 'This node has not been activated.'));
    for (const a of attempts) {
      const card = el('article', undefined, 'card');
      card.append(el('h4', `Attempt ${a.Ref.Attempt} · ${a.State}`), el('p', `Activation ${a.Ref.ActivationID}`), el('p', `Outcome: ${a.Outcome || 'pending'}`));
      if (a.Detail) card.append(el('pre', a.Detail));
      if (a.ExecutionID) card.append(el('p', 'Execution ' + a.ExecutionID));
      if (a.OperationID) card.append(el('p', 'Operation ' + a.OperationID));
      if (a.RetryAt) card.append(el('p', 'Retry at ' + new Date(a.RetryAt).toLocaleString()));
      if (a.DecisionID) {
        const saved = (result.decisions || []).find(d => d.ID === a.DecisionID);
        card.append(el('p', `Decision ${a.DecisionID}${saved ? ' · ' + saved.State : ''}`));
        const decision = el('div'); card.append(decision);
        card.append(button('Inspect decision', async () => {
          try {
            const record = await api('/v2/decisions/' + encodeURIComponent(a.DecisionID));
            if (closed || !decision.isConnected) return;
            const window = record.Window;
            decision.replaceChildren(el('h4', window.Question || 'Decision'), el('p', `${window.ID} · ${window.State}`), el('p', 'Expires ' + new Date(window.ExpiresAt).toLocaleString()));
            if (record.Submission) {
              const s = record.Submission;
              decision.append(el('strong', 'Answer: ' + s.Answer), el('pre', s.Reason), el('p', 'Decided by ' + (s.Actor.agent_id || s.Actor.kind)));
              for (const id of s.EvidenceRefs || []) decision.append(el('p', 'Evidence ' + id));
            }
            if (window.State === 'open') decision.append(button('Answer pending decision', () => { dialog.close(); return openDecisions(); }));
          } catch (e) { if (!closed && decision.isConnected) decision.replaceChildren(el('p', e.message)); }
        }));
      }
      for (const e of result.node_evidence || []) {
        if (e.attempt.NodeID !== a.Ref.NodeID || e.attempt.ActivationID !== a.Ref.ActivationID || e.attempt.Attempt !== a.Ref.Attempt) continue;
        card.append(el('strong', `Evidence ${e.id}`), el('p', `${e.kind} · ${e.reporter.agent_id || e.reporter.kind}`), el('pre', e.detail));
        if (e.artifact_revision) card.append(el('p', 'Artifact ' + e.artifact_revision));
        if (e.passed !== undefined) card.append(el('p', e.passed ? 'Verification passed' : 'Verification failed'));
      }
      detail.append(card);
    }
  }
  await load();
}
