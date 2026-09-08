import {processEscalations} from './process-escalation.js';
// Authoring state is separate from the graph renderer and from v2 execution.
export const clone = value => structuredClone(value);
export const freshID = prefix => prefix + crypto.randomUUID();
export const edgeID = (edge, index) => `${index}:${edge.From}:${edge.To}:${edge.Verdict || ''}`;
export const edgeKey = edge => JSON.stringify([edge.From, edge.To, edge.Verdict || ""]);
export const seconds = value => Number(value || 0) * 1e9;
export const lines = text => text.split('\n').map(value => value.trim()).filter(Boolean);

export function newProcess() {
  const end = freshID('node_');
  return {ID: freshID('definition_'), Name: 'New process', Kind: 'process', SchemaVersion: 1,
    Source: 'Created in the process editor.', Parameters: [], Dependencies: [],
    EditorLayout: {Nodes: {[end]: {X: 200, Y: 150}}},
    Process: {Graph: {CompilerVersion: '1', EntryNodeID: end,
      Nodes: [{ID: end, Name: 'Done', Kind: 'end', End: {Outcome: 'verified'}}], Edges: [], Outcome: {}}}};
}

export function draftFromResult(result) {
  const r = result.Revision;
  return {ID: result.Definition.ID, Name: result.Definition.Name, Kind: 'process',
    SchemaVersion: r.SchemaVersion, Source: r.Source, Parameters: clone(r.Parameters || []),
    Dependencies: clone(r.Dependencies || []), Process: clone(r.Process),
    EditorLayout: clone(r.EditorLayout || {Nodes: {}})};
}

export function defaultNode(kind) {
  const node = {ID: freshID('node_'), Name: kind[0].toUpperCase() + kind.slice(1), Kind: kind};
  if (kind === 'task') node.Performer = {Kind: 'agent', Agent: {MemberKey: 'worker', ContextPolicy: 'fresh', Brief: ''}};
  if (kind === 'decision') node.Decision = {Kind: 'work', Audience: [{Subject: {Kind: 'operator'}}], PermittedAnswers: ['approve', 'reject'], ExpiresAfter: seconds(3600)};
  if (kind === 'wait') node.Wait = {Duration: seconds(60)};
  if (kind === 'join') node.Join = {Mode: 'all'};
  if (kind === 'end') node.End = {Outcome: 'verified'};
  return node;
}

// The reused renderer receives a presentation projection, never a native session
// or execution identity. Fork/join share its parallel shape with distinct labels.
export function graphView(draft) {
  const graph = draft.Process.Graph;
  const escalation = processEscalations(graph);
  const labels = new Map((draft.EditorLayout?.EdgeLabels || []).map(label => [edgeKey(label.Edge), label.Pinned]));
  return {nodes: graph.Nodes.map(node => {
    const position = draft.EditorLayout?.Nodes?.[node.ID];
    return {id: node.ID, type: ['fork', 'join'].includes(node.Kind) ? 'parallel' : node.Kind === 'task_complete' ? 'task' : node.Kind,
      label: node.Name || node.ID, subtitle: node.ID === graph.EntryNodeID ? 'Entry' : node.Kind,
      pinned: position ? {x: position.X, y: position.Y} : undefined};
  }), edges: (graph.Edges || []).map((edge, index) => ({id: edgeID(edge, index), from: edge.From,
    to: edge.To, outcome: edge.Verdict || 'pass', back: escalation.retries.has(index), pinned: labels.get(edgeKey(edge))}))};
}

export class ProcessDraft {
  constructor(draft) {
    this.value = clone(draft);
    this.value.Parameters ||= []; this.value.Dependencies ||= [];
    this.value.EditorLayout ||= {Nodes: {}}; this.value.EditorLayout.Nodes ||= {};
    this.value.Process.Graph.Edges ||= []; this.value.Process.Graph.Outcome ||= {};
    this.undoStack = []; this.redoStack = [];
  }
  change(edit) {
    const before = clone(this.value), after = clone(before);
    edit(after);
    if(after.EditorLayout?.EdgeLabels) {
      const edges=new Set(after.Process.Graph.Edges.map(edgeKey));
      after.EditorLayout.EdgeLabels=after.EditorLayout.EdgeLabels.filter(label=>edges.has(edgeKey(label.Edge)));
      if(!after.EditorLayout.EdgeLabels.length)delete after.EditorLayout.EdgeLabels;
    }
    if (JSON.stringify(before) === JSON.stringify(after)) return;
    this.undoStack.push(before); if (this.undoStack.length > 100) this.undoStack.shift();
    this.redoStack = []; this.value = after;
  }
  undo() { if (this.undoStack.length) { this.redoStack.push(this.value); this.value = this.undoStack.pop(); } }
  redo() { if (this.redoStack.length) { this.undoStack.push(this.value); this.value = this.redoStack.pop(); } }
  remove(ids) {
    this.change(draft => {
      const graph = draft.Process.Graph;
      graph.Nodes = graph.Nodes.filter(node => !ids.has(node.ID));
      graph.Edges = (graph.Edges || []).filter(edge => !ids.has(edge.From) && !ids.has(edge.To));
      graph.Outcome.RequiredNodes = (graph.Outcome.RequiredNodes || []).filter(id => !ids.has(id));
      for (const id of ids) delete draft.EditorLayout.Nodes[id];
      if (ids.has(graph.EntryNodeID)) graph.EntryNodeID = graph.Nodes[0]?.ID || '';
    });
  }
}

export function validationMessages(draft) {
  const messages = [], graph = draft.Process?.Graph;
  if (!draft.Name.trim()) messages.push('Give the process a name.');
  if (!graph?.Nodes?.length) return [...messages, 'Add at least one node.'];
  if (draft.Process.ParameterSyntax === 'mustache-v1') {
    const declared = new Set((draft.Parameters || []).map(p => p.Name));
    for (const key of declared) if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) messages.push('Parameter keys must be ASCII identifiers when expansion is enabled.');
    const texts = taskPerformers(graph).flatMap(p => [p.Agent?.Brief, p.Human?.Prompt, ...(p.Program?.Arguments || [])]);
    for (const node of graph.Nodes) texts.push(node.Decision?.Question, node.Stages?.PlanApproval?.Question);
    for (const text of texts.filter(v => typeof v === 'string')) {
      const remaining = text.replace(/\{\{[ \t\r\n\f]*params\.([A-Za-z_][A-Za-z0-9_]*)[ \t\r\n\f]*\}\}/g, (_, key) => {
        if (!declared.has(key)) messages.push(`Input references undeclared parameter ${key}.`);
        return '';
      });
      if (/\{\{[ \t\r\n\f]*params\b/.test(remaining)) messages.push('Malformed parameter reference: use {{ params.key }}.');
    }
  }
  const nodes = new Map(graph.Nodes.map(n => [n.ID, n]));
  if (!nodes.has(graph.EntryNodeID)) messages.push('Choose an entry node.');
  const incoming = new Map(), outgoing = new Map(), edgeKeys = new Set();
  for (const edge of graph.Edges || []) {
    const key = edgeKey(edge);
    if (edgeKeys.has(key)) messages.push("Duplicate connection: source, destination and answer/outcome must be unique.");
    edgeKeys.add(key);
    if (!nodes.has(edge.From) || !nodes.has(edge.To) || edge.From === edge.To) messages.push('Connections must join two different existing nodes.');
    const source = nodes.get(edge.From);
    if (source?.Kind === 'decision' && edge.Verdict && !source.Decision.PermittedAnswers.includes(edge.Verdict)) messages.push(`${source.Name || source.ID}: connection answer "${edge.Verdict}" is no longer permitted. Edit or delete that connection.`);
    incoming.set(edge.To, (incoming.get(edge.To) || 0) + 1);
    outgoing.set(edge.From, [...(outgoing.get(edge.From) || []), edge]);
  }
  const escalation = processEscalations(graph);
  messages.push(...escalation.errors);
  const visited = new Set(), visiting = new Set();
  function visit(id) {
    if (visiting.has(id)) { messages.push('Ordinary connections cannot form a cycle. Use node retry settings for bounded retries.'); return; }
    if (visited.has(id)) return;
    visited.add(id); visiting.add(id);
    for (const edge of outgoing.get(id) || []) { if (!escalation.retries.has(graph.Edges.indexOf(edge))) visit(edge.To); }
    visiting.delete(id);
  }
  visit(graph.EntryNodeID);
  for (const node of graph.Nodes) {
    const name = node.Name || node.ID;
    if (!visited.has(node.ID)) messages.push(`${name}: connect this node to the entry path.`);
    if (node.Kind === 'fork' && (outgoing.get(node.ID)?.length || 0) < 2) messages.push(`${name}: a fork needs at least two outgoing branches.`);
    if (node.Kind === 'join' && (incoming.get(node.ID) || 0) < 2) messages.push(`${name}: a join needs at least two incoming branches.`);
    if (node.Kind === 'end' && outgoing.has(node.ID)) messages.push(`${name}: an end cannot have outgoing connections.`);
    if (node.Kind === 'wait') {
      if (!node.Wait || node.Wait.Duration < 0 || (!(node.Wait.Duration > 0) && !node.Wait.Until && !node.Wait.Signal?.trim())) messages.push(`${name}: set a positive duration, timestamp or signal.`);
      if (node.Wait?.Until && !validWaitTimestamp(node.Wait.Until)) messages.push(`${name}: timestamp must be RFC3339.`);
    }
    for (const performer of taskPerformers({Nodes:[node]})) {
      if (performer.Kind === 'agent' && !performer.Agent?.Brief?.trim()) messages.push(`${name}: add every worker brief, including task stages.`);
      if (performer.Kind === 'human' && !performer.Human?.Prompt?.trim()) messages.push(`${name}: add every human stage prompt.`);
    }
    if (node.Kind === 'decision' && !node.Decision?.PermittedAnswers?.length) messages.push(`${name}: add at least one permitted answer.`);
  }
  return [...new Set(messages)];
}

// Stage performers participate in the same launch binding/authority controls as work.
export function taskPerformers(graph) {
  return (graph?.Nodes || []).flatMap(node => [node.Performer, node.Decision?.Decider, node.Stages?.Plan?.Performer, ...(node.Stages?.Checks || []).map(s => s.Performer), node.Stages?.Review?.Performer].filter(Boolean));
}

function validWaitTimestamp(value) {
  // Match Go strings.TrimSpace without rewriting the authored field.
  const trimmed=value.replace(/^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g,'');
  const match=/^(\d{4})-(\d{2})-(\d{2})T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/.exec(trimmed);
  if(!match)return false;
  const year=Number(match[1]),month=Number(match[2]),day=Number(match[3]);
  const days=[31,year%4===0&&(year%100!==0||year%400===0)?29:28,31,30,31,30,31,31,30,31,30,31];
  return month>=1&&month<=12&&day>=1&&day<=days[month-1];
}
