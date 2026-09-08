import {addCheck, removeCheck, moveCheck} from './process-stages.js';
import {ProcessSnippetLibrary} from './process-snippets.js';
import {ProcessGraphAdapter} from './processgraph/process-graph-adapter.js';
import {clone, freshID, edgeKey, seconds, lines, newProcess, draftFromResult, defaultNode, graphView, ProcessDraft, validationMessages} from './process-model.js';

const element = (tag, text) => { const node = document.createElement(tag); if (text !== undefined) node.textContent = text; return node; };
const action = (label, handler) => { const node = element('button', label); node.type = 'button'; node.onclick = handler; return node; };
const option = (value, label = value) => ({value, label});

export async function openProcessEditor({api, result, agents = [], onSaved}) {
  const profiles = await api('/v2/program-profiles');
  const revisions = await Promise.all((profiles || []).map(profile => api('/v2/program-profiles/' + encodeURIComponent(profile.ID))));
  return new ProcessEditor({api, result, agents, revisions, onSaved});
}

class ProcessEditor {
  constructor({api, result, agents, revisions, onSaved}) {
    this.api = api; this.agents = agents; this.revisions = revisions; this.onSaved = onSaved;
    this.model = new ProcessDraft(result ? draftFromResult(result) : newProcess());
    this.baseRevision = result?.Definition.Revision || 0;
    this.saved = JSON.stringify(this.model.value); this.selection = new Set(); this.clipboard = null;
    this.pending = null; this.busy = false; this.unapplied = false;
    this.dialog = element('dialog'); this.dialog.id = 'process-editor';
    this.dialog.setAttribute('aria-labelledby', 'process-editor-heading');
    const heading = element('h2', 'Process editor'); heading.id = 'process-editor-heading';
    this.message = element('p'); this.message.id = 'process-editor-message'; this.message.role = 'status';
    this.errors = element('ul'); this.errors.id = 'process-editor-errors'; this.errors.role = 'alert';
    this.header = element('div'); this.header.className = 'process-toolbar';
    this.name = element('input'); this.name.value = this.model.value.Name; this.name.setAttribute('aria-label', 'Process name');
    this.name.onchange = () => this.change(draft => { draft.Name = this.name.value; });
    this.undoButton = action('Undo', () => this.history('undo'));
    this.redoButton = action('Redo', () => this.history('redo'));
    this.validateButton = action('Validate', () => this.validate());
    this.saveButton = action('Save revision', () => this.save());
    this.header.append(this.name, this.undoButton, this.redoButton, this.validateButton, this.saveButton,
      action('Export', () => this.export()), action('Import copy', () => this.import()), action('Close editor', () => this.close()));
    const palette = element('div'); palette.className = 'process-toolbar'; palette.setAttribute('aria-label', 'Node palette');
    for (const kind of ['task', 'decision', 'fork', 'join', 'wait', 'end']) palette.append(action('Add ' + kind, () => this.add(kind)));
    palette.append(action('Saved snippets', () => this.snippets()), action('Copy nodes', () => this.copy()), action('Paste nodes', () => this.paste()), action('Delete selected', () => this.remove()),
      action('Overview', () => this.overview()), action('Parameters', () => this.parameters()), action('Outcome', () => this.outcome()), action('Source', () => this.source()));
    this.entry = element('select'); this.entry.setAttribute('aria-label', 'Entry node'); this.entry.onchange = () => this.change(draft => { draft.Process.Graph.EntryNodeID = this.entry.value; });
    const label = element('label', 'Entry'); label.append(this.entry); palette.append(label);
    const body = element('div'); body.className = 'process-editor-body';
    this.canvas = element('div'); this.canvas.id = 'process-editor-canvas';
    this.inspector = element('aside'); this.inspector.id = 'process-inspector';
    body.append(this.canvas, this.inspector);
    this.dialog.append(heading, this.header, palette, this.message, this.errors, body);
    document.body.append(this.dialog); this.dialog.showModal();
    this.graph = new ProcessGraphAdapter(this.canvas, {graph: graphView(this.model.value), ariaLabel: 'Editable process graph', events: {
      nodeClick: ({node, event}) => this.select(node.id, event.shiftKey || event.ctrlKey || event.metaKey),
      nodeDoubleClick: ({node}) => this.select(node.id),
      canvasClick: () => this.select(null),
      edgeClick: ({edge}) => this.editEdge(edge),
      marqueeSelection: ({items}) => { if (!this.busy && this.discardUnapplied()) { this.selection = new Set(items.filter(i => i.type === 'node').map(i => i.id)); this.render(); } },
      nodeDragEnd: ({starts, delta, moved}) => { if (moved) this.change(draft => { for (const start of starts) draft.EditorLayout.Nodes[start.id] = {X: start.x + delta.x, Y: start.y + delta.y}; }); },
      portDragEnd: payload => this.connectGesture(payload),
    }});
    this.dialog.addEventListener('cancel', event => { event.preventDefault(); this.close(); });
    this.dialog.addEventListener('keydown', event => this.key(event));
    this.beforeUnload = event => { if (this.dirty() || this.unapplied) { event.preventDefault(); event.returnValue = ''; } };
    window.addEventListener('beforeunload', this.beforeUnload);
    this.render(); this.graph.fit();
  }

  dirty() { return (this.name && this.name.value !== this.model.value.Name) || this.saved !== JSON.stringify(this.model.value) || this.baseRevision === 0; }
  discardUnapplied() { if (this.unapplied && !confirm('Discard unapplied field changes?')) return false; this.unapplied = false; return true; }
  change(edit) {
    if (this.busy) return;
    if (this.unapplied) {
      this.name.value = this.model.value.Name;
      this.graph.setGraph(graphView(this.model.value));
      this.fail(new Error('Apply the field changes before editing the graph.')); return;
    }
    this.model.change(edit); this.render();
  }
  history(method) { if (!this.busy && this.discardUnapplied()) { this.model[method](); this.render(); } }
  select(id, extend = false) {
    if (this.busy || !this.discardUnapplied()) return;
    if (!extend) this.selection.clear();
    if (id) { if (extend && this.selection.has(id)) this.selection.delete(id); else this.selection.add(id); }
    this.render();
  }
  render() {
    const draft = this.model.value, graph = draft.Process.Graph;
    this.name.value = draft.Name;
    this.entry.replaceChildren(...graph.Nodes.map(node => { const o = element('option', node.Name || node.ID); o.value = node.ID; return o; }));
    this.entry.value = graph.EntryNodeID;
    this.undoButton.disabled = !this.model.undoStack.length || this.busy;
    this.redoButton.disabled = !this.model.redoStack.length || this.busy;
    this.message.textContent = `${this.baseRevision ? 'Revision ' + this.baseRevision : 'New process'}${this.dirty() ? ' · unsaved changes' : ' · saved'}`;
    this.selection = new Set([...this.selection].filter(id => graph.Nodes.some(n => n.ID === id)));
    this.graph.setGraph(graphView(draft));
    this.graph.setSelection({type: 'multi', items: [...this.selection].map(id => ({type: 'node', id}))});
    this.errors.replaceChildren();
    if (this.selection.size === 1) this.nodeForm(graph.Nodes.find(n => this.selection.has(n.ID)));
    else {
      this.inspector.replaceChildren(element('h3', this.selection.size ? `${this.selection.size} nodes selected` : 'Graph'));
      this.inspector.append(element('p', 'Select a node to edit it. Drag nodes to arrange them. Drag between ports to connect, or use Connect below. Shift-click or drag a selection box to select several nodes.'));
      this.connectionForm();
    }
  }
  form(title, fields, apply, label = 'Apply changes') {
    this.inspector.replaceChildren(element('h3', title));
    const form = element('form');
    for (const field of fields) {
      const wrapper = element('label', field.label); let input;
      if (field.options) {
        input = element('select'); input.multiple = !!field.multiple;
        const opts = [...field.options];
        const wanted = field.multiple ? field.value || [] : [String(field.value ?? '')];
        for (const value of wanted) if (value && !opts.some(o => String(o.value) === String(value))) opts.push(option(value, 'Pinned: ' + value));
        for (const o of opts) { const node = element('option', o.label); node.value = o.value; node.selected = wanted.includes(String(o.value)); input.append(node); }
      } else { input = element(field.multiline ? 'textarea' : 'input'); if (!field.multiline) input.type = field.type || 'text'; input.value = field.value ?? ''; }
      if (field.type === 'checkbox') input.checked = !!field.value;
      input.name = field.name; input.setAttribute('aria-label', field.label);
      input.required = !!field.required; if (field.min !== undefined) input.min = field.min;
      if (field.max !== undefined) input.max = field.max;
      if (field.step !== undefined) input.step = field.step;
      wrapper.append(input); form.append(wrapper);
    }
    form.oninput = () => { this.unapplied = true; };
    const submit = element('button', label); submit.type = 'submit'; form.append(submit);
    form.onsubmit = event => {
      event.preventDefault(); if (this.busy) return;
      try {
        const data = new FormData(form), values = Object.fromEntries(data);
        for (const field of fields) { if (field.multiple) values[field.name] = data.getAll(field.name); if (field.type === 'checkbox') values[field.name] = form.elements[field.name].checked; }
        this.unapplied = false; apply(values);
      } catch (error) { this.unapplied = true; this.fail(error); }
    };
    this.inspector.append(form); return form;
  }
  nodeForm(node, stageContext = null) {
    const resolve = draft => {
      const parent = draft.Process.Graph.Nodes.find(n => n.ID === (stageContext?.parentID || node.ID));
      return stageContext ? stageContext.kind === 'Checks' ? parent.Stages.Checks.find(s => s.ID === stageContext.id) : parent.Stages[stageContext.kind] : parent;
    };
    const update = edit => this.change(draft => edit(resolve(draft)));
    const fields = [{name: 'name', label: 'Node name', value: node.Name, required: true},
      {name: 'description', label: 'Description', value: node.Description || '', multiline: true},
      {name: 'doc', label: 'Documentation', value: node.Doc || '', multiline: true}];
    const changes = [];
    if (node.Kind === 'wait') {
      fields.push({name: 'duration', label: 'Wait seconds (0 when unused)', type: 'number', min: 0, step: 'any', value: (node.Wait?.Duration || 0) / 1e9},
        {name:'until',label:'Wait until (RFC3339, authoring only)',value:node.Wait?.Until||''},
        {name:'signal',label:'Wait for signal (authoring only)',value:node.Wait?.Signal||''});
      changes.push((n, f) => { n.Wait = {Duration: seconds(f.duration),Until:f.until};if(f.signal)n.Wait.Signal=f.signal; });
    }
    if (node.Kind === 'end') {
      fields.push({name: 'outcome', label: 'End outcome', options: ['verified', 'waived', 'rejected', 'cancelled'].map(v => option(v)), value: node.End?.Outcome});
      changes.push((n, f) => { n.End.Outcome = f.outcome; });
    }
    if (node.Kind === 'join') {
      fields.push({name: 'mode', label: 'Wait for branches', options: [option('all', 'All branches'), option('any', 'First branch (others drain)')], value: node.Join.Mode});
      changes.push((n, f) => { n.Join.Mode = f.mode; });
    }
    if (node.Kind === 'decision') {
      const audience = [...new Map([{Subject: {Kind: 'operator'}}, ...(node.Decision.Audience || []), ...this.agents.map(a => ({Subject: {Kind: 'agent', AgentID: a.ID}}))].map(a => [JSON.stringify(a), a])).values()];
      fields.push({name:'question',label:'Decision question (optional; defaults to node name)',multiline:true,value:node.Decision.Question||''}, {name: 'audience', label: 'Decision audience', multiple: true, value: node.Decision.Audience.map(a => JSON.stringify(a)), options: audience.map(a => option(JSON.stringify(a), a.Subject.Kind === 'operator' ? 'Operator' : this.agents.find(agent => agent.ID === a.Subject.AgentID)?.Name || a.Subject.AgentID || a.RoleID || 'Scoped audience'))},
        {name: 'answers', label: 'Permitted answers (one per line)', multiline: true, value: node.Decision.PermittedAnswers.join('\n'), required: true},
        {name: 'expires', label: 'Decision expires after seconds', type: 'number', min: 1, value: node.Decision.ExpiresAfter / 1e9, required: true});
      changes.push((n, f) => { n.Decision = {...n.Decision, Question:f.question||undefined, Audience: f.audience.map(value => JSON.parse(value)), PermittedAnswers: lines(f.answers), ExpiresAfter: seconds(f.expires)}; });
    }
    if (node.Kind === 'task') {
      if(!stageContext) {
        fields.push({name:'captures',label:'Published output names (authoring only; one per line)',multiline:true,value:(node.Captures||[]).join('\n')});
        changes.push((n,f)=>{const names=[...new Set(lines(f.captures))];if(names.length)n.Captures=names;else delete n.Captures;});
      }
      const performer = node.Performer;
      fields.push({name:'timeout',label:'Performer timeout (e.g. 30s; program execution maximum 1h)',value:performer.Timeout||''});
      changes.push((n,f)=>{if(f.timeout)n.Performer.Timeout=f.timeout;else delete n.Performer.Timeout;});
      fields.push({name:'contact_cadence',label:'Contact cadence (authoring only, e.g. 30m)',value:performer.Contact?.Cadence||''},
        {name:'contact_budget',label:'Contact budget',type:'number',min:1,max:10000,value:performer.Contact?.Budget||''},
        {name:'contact_target',label:'Escalation target (authoring only)',value:performer.Contact?.EscalationTarget||''});
      changes.push((n,f)=>{if(!f.contact_cadence&&!f.contact_budget&&!f.contact_target)delete n.Performer.Contact;else n.Performer.Contact={Cadence:f.contact_cadence,Budget:Number(f.contact_budget),EscalationTarget:f.contact_target};});
      const select = element('select'); select.setAttribute('aria-label', 'Performer kind');
      for (const kind of ['agent', 'program', 'human']) { const o = element('option', kind); o.value = kind; select.append(o); }
      select.value = performer.Kind;
      select.onchange = () => {
        if (!this.discardUnapplied()) return;
        update(n => {
          const contact = n.Performer.Contact, timeout = n.Performer.Timeout;
          n.Performer = select.value === 'agent' ? {Kind: 'agent', Agent: {MemberKey: 'worker', Brief: '', ContextPolicy: 'fresh'}}
            : select.value === 'program' ? {Kind: 'program', Program: {Profile: {}, Arguments: []}}
              : {Kind: 'human', Human: {Operator: true, AgentID: '', RoleID: '', Prompt: ''}};
          if(contact) n.Performer.Contact = contact;
          if(timeout) n.Performer.Timeout = timeout;
        });
        if (stageContext) this.showStage(stageContext);
      };
      if (performer.Kind === 'agent') {
        const a = performer.Agent;
        fields.push({name: 'agent', label: 'Worker', options: [option('', 'Bind worker when starting'), ...this.agents.map(a => option(a.ID, a.Name))], value: a.AgentID || ''},
          {name: 'binding', label: 'Worker binding key', value: a.MemberKey || ''},
          {name: 'context', label: 'Conversation context', options: [option('fresh', 'Fresh context'), option('reuse', 'Reuse context')], value: a.ContextPolicy || 'fresh'},
          {name: 'brief', label: 'Worker brief', multiline: true, value: a.Brief, required: true});
        changes.push((n, f) => { n.Performer.Agent = {...n.Performer.Agent, AgentID: f.agent, MemberKey: f.agent ? '' : f.binding, ContextPolicy: f.context, Brief: f.brief}; if (!f.agent && !f.binding.trim()) throw new Error('Choose a worker or a binding key.'); });
      } else if (performer.Kind === 'program') {
        const p = performer.Program, refs = this.revisions.map(r => ({ref: {ProfileID: r.Profile.ID, RevisionID: r.Revision.ID, ContentHash: r.Revision.ContentHash}, label: `${r.Profile.Name} · revision ${r.Revision.Number}`}));
        if (p.Profile?.RevisionID && !refs.some(r => r.ref.RevisionID === p.Profile.RevisionID)) refs.push({ref: p.Profile, label: 'Previously pinned program revision'});
        fields.push({name: 'profile', label: 'Program profile', options: [option('', 'Choose a saved profile'), ...refs.map(r => option(r.ref.RevisionID, r.label))], value: p.Profile?.RevisionID || '', required: true},
          {name: 'arguments', label: 'Arguments (one per line)', multiline: true, value: (p.Arguments || []).join('\n')},
          {name: 'input', label: 'Program input (JSON)', multiline: true, value: p.Input ? JSON.stringify(p.Input, null, 2) : ''});
        changes.push((n, f) => { n.Performer.Program = {...n.Performer.Program, Profile: clone(refs.find(r => r.ref.RevisionID === f.profile).ref), Arguments: f.arguments ? f.arguments.split('\n') : [], Input: f.input.trim() ? JSON.parse(f.input) : null}; });
      } else {
        fields.push({name:'operator', label:'Operator performs this task', type:'checkbox', value:performer.Human.Operator}, {name: 'agent', label: 'Human task recipient agent', options: [option('', 'Use role instead'), ...this.agents.map(a => option(a.ID, a.Name))], value: performer.Human.AgentID || ''},
          {name: 'role', label: 'Human task role ID', value: performer.Human.RoleID || ''},
          {name: 'prompt', label: 'Human task prompt', multiline: true, required: true, value: performer.Human.Prompt},
          {name:'choices',label:'Human task answers (one per line)',multiline:true,value:(performer.Human.Choices||[]).join('\n')},
          {name:'outcomes',label:'Answer outcomes (pass or fail, matching line order)',multiline:true,value:(performer.Human.Choices||[]).map(c=>performer.Human.ChoiceOutcomes?.[c]||'').join('\n')});
        changes.push((n, f) => {
          const choices=f.choices ? f.choices.split(/\r?\n/) : [], outcomes=f.outcomes ? f.outcomes.split(/\r?\n/) : [];
          if(choices.length>64 || choices.length!==outcomes.length || choices.some((c,i)=>!c || /[\r\n\0]/.test(c) || c.trim()!==c || new TextEncoder().encode(c).length>256 || choices.slice(0,i).some(p=>p.toLowerCase()===c.toLowerCase()) || !['pass','fail'].includes(outcomes[i]))) throw new Error('Each unique trimmed answer needs a matching pass or fail line.');
          n.Performer.Human = {...n.Performer.Human, Operator: f.operator, AgentID: f.operator ? '' : f.agent, RoleID: f.operator ? '' : f.role, Prompt: f.prompt};
          delete n.Performer.Human.Choices; delete n.Performer.Human.ChoiceOutcomes;
          if(choices.length) { n.Performer.Human.Choices=choices; n.Performer.Human.ChoiceOutcomes=Object.fromEntries(choices.map((c,i)=>[c,outcomes[i]])); }
        });
      }
      this.performerSelect = select;
    }
    if (node.Kind === 'task' || node.Kind === 'decision') {
      fields.push({name: 'attempts', label: node.Stages ? 'Maximum work attempts (0 permits one work attempt)' : 'Maximum attempts (0 disables retries)', type: 'number', min: 0, max: 100, value: node.Retry?.MaxAttempts || 0},
        {name: 'backoff', label: 'Retry delay seconds', type: 'number', min: 0, value: (node.Retry?.Backoff || 0) / 1e9},
        {name: 'budget', label: 'Attempt budget seconds (0 uses run deadline)', type: 'number', min: 0, value: (node.Retry?.AttemptBudget || 0) / 1e9},
        {name:'retry_mode',label:'Retry mode',options:[option('','Default fresh attempt'),option('fresh-attempt'),option('feedback-same-session','Feedback in same session (authoring only)')],value:node.Retry?.OnFail||''},
        {name: 'retryable', label: 'Retry these outcomes', multiple: true, options: ['program_failed', 'agent_rejected', 'human_rejected'].map(v => option(v)), value: node.Retry?.Retryable || []},
        {name: 'waivable', label: 'Allow explicit waiver when blocked', type: 'checkbox', value: node.Waivable});
      changes.push((n, f) => { if(f.retry_mode&&!Number(f.attempts))throw new Error('Retry mode requires positive max attempts.'); n.Waivable = f.waivable; n.Retry = Number(f.attempts) ? {MaxAttempts: Number(f.attempts), Backoff: seconds(f.backoff), AttemptBudget: seconds(f.budget), Retryable: f.retryable,...(f.retry_mode?{OnFail:f.retry_mode}:{})} : {}; });
    }
    if (stageContext?.kind === 'Plan' && this.model.value.Process.Graph.Nodes.find(n=>n.ID===stageContext.parentID)?.Stages?.PlanApproval) {
      fields.push({name:'approval_attempts',label:'Maximum approval attempts (authoring only)',type:'number',min:1,max:4294967295,value:node.ApprovalRetry?.MaxAttempts??''},
        {name:'approval_backoff',label:'Approval retry backoff (e.g. 30s)',value:node.ApprovalRetry?.Backoff||''},
        {name:'approval_mode',label:'Approval retry mode',options:[option('','Default'),option('fresh-attempt'),option('feedback-same-session')],value:node.ApprovalRetry?.OnFail||''});
      changes.push((n,f)=>{if(!f.approval_attempts&&!f.approval_backoff&&!f.approval_mode){delete n.ApprovalRetry;return}const attempts=Number(f.approval_attempts);if(!Number.isInteger(attempts)||attempts<1||attempts>4294967295)throw new Error('Approval retry requires positive max attempts.');n.ApprovalRetry={MaxAttempts:attempts,Backoff:f.approval_backoff,OnFail:f.approval_mode};});
    }
    this.form(`${node.Kind} · ${node.Name || "Unnamed"}`, fields, values => update(n => {
      n.Name = values.name;
      if(values.description) n.Description = values.description; else delete n.Description;
      if(values.doc) n.Doc = values.doc; else delete n.Doc;
      for (const apply of changes) apply(n, values);
      if (stageContext) delete n.Waivable;
    }));
    if (node.Kind === 'task') this.inspector.prepend(this.performerSelect);
    if(node.Kind === 'wait') this.inspector.append(element('p','Absolute-time and signal waits can be saved, but cannot start. Clear both to run a duration wait.'));
    if(node.Captures?.length) this.inspector.append(element('p','Output names are retained for authoring and export. Running this process is unavailable until capture execution is supported.'));
    if(node.Performer?.Timeout) this.inspector.append(element('p',node.Performer.Kind==='program'?'Timeout starts when this program node becomes ready, includes admission delay, and cannot extend the run or saved program limit.':'Timeout is retained for authoring. Clear it to start: agent and human timeout execution is unavailable.'));
    if(node.Performer?.Contact) this.inspector.append(element('p','Contact schedules are retained for authoring and export. Clear all three contact fields to remove a schedule. Running this process is unavailable until scheduled performer contact is supported.'));
    if(stageContext && stageContext.kind!=='Plan')this.inspector.append(element('p','Independent check/review retries can be authored and saved, but cannot run. Clear the retry policy to use the shared work retry budget.'));
    if(node.Retry?.OnFail==='feedback-same-session')this.inspector.append(element('p','Feedback in the same session is retained for authoring only. Choose a fresh attempt to run.'));
    if(node.ApprovalRetry)this.inspector.append(element('p','Approval retry policy is retained for authoring and export. Clear its fields to run this process; approval retry execution is unavailable.'));
    if (stageContext) { this.inspector.append(action('Back to task stages', () => this.render())); return; }
    if (node.Kind === 'task') this.stageControls(node);
    this.inspector.append(action('Make entry', () => this.change(d => { d.Process.Graph.EntryNodeID = node.ID; })));
    this.connectionForm(node.ID);
  }


  showStage(context) {
    const parent = this.model.value.Process.Graph.Nodes.find(n => n.ID === context.parentID);
    const stage = context.kind === 'Checks' ? parent.Stages.Checks.find(s => s.ID === context.id) : parent.Stages[context.kind];
    this.nodeForm({...stage, Kind:'task'}, context);
  }
  stageControls(node) {
    const host = element('section'); host.className = 'process-task-stages';
    host.append(element('h4','Plan, checks and review'), element('p','Checks run in order. A failed check or review sends work back through the task and its checks. The work attempt limit is shared.'));
    const edit = fn => { if (!this.discardUnapplied()) return; this.change(d => { const n=d.Process.Graph.Nodes.find(n=>n.ID===node.ID); n.Stages ||= {}; fn(n.Stages,n); }); };
    const open = (kind,id) => { if(this.discardUnapplied()) this.showStage({parentID:node.ID,kind,id}); };
    for (const kind of ['Plan','Review']) {
      const stage = node.Stages?.[kind];
      if (stage) host.append(element('p',`${kind}: ${stage.Name} · ${stage.Performer.Kind}`),action('Edit '+kind.toLowerCase(),()=>open(kind)),action('Remove '+kind.toLowerCase(),()=>edit(s=>{delete s[kind];if(kind==='Plan')delete s.PlanApproval;})));
      else host.append(action('Add '+kind.toLowerCase(),()=>edit((s,n)=>{s[kind]={ID:kind.toLowerCase(),Name:kind,Performer:kind==='Plan'?clone(n.Performer):{Kind:'human',Human:{Operator:true,Prompt:'Review the task result'}}};})));
    }
    if (node.Stages?.Plan) {
      host.append(element('p',node.Stages.PlanApproval?'Plan requires explicit approval':'Plan continues automatically'),action(node.Stages.PlanApproval?'Remove plan approval':'Require plan approval',()=>edit(s=>{if(s.PlanApproval){delete s.PlanApproval;delete s.Plan.ApprovalRetry;}else s.PlanApproval={Kind:'work',Audience:[{Subject:{Kind:'operator'}}],PermittedAnswers:['approve','rework'],ExpiresAfter:seconds(3600)};})));
    }
    for (const [i,check] of (node.Stages?.Checks || []).entries()) {
      const row=element('div');row.append(element('span',`${i+1}. ${check.Name} · ${check.Performer.Kind}`),action('Edit check '+(i+1),()=>open('Checks',check.ID)),action('Move check '+(i+1)+' up',()=>edit(s=>moveCheck(s,i,-1))),action('Move check '+(i+1)+' down',()=>edit(s=>moveCheck(s,i,1))),action('Remove check '+(i+1),()=>edit(s=>removeCheck(s,i))));host.append(row);
    }
    host.append(action('Add check',()=>edit(s=>addCheck(s,{Kind:'human',Human:{Operator:true,Prompt:'Verify the task result'}}))));
    this.inspector.append(host);
  }

  add(kind) {
    if (!this.discardUnapplied()) return;
    const node = defaultNode(kind), center = this.graph.canvasCenter();
    const positions = Object.values(this.model.value.EditorLayout.Nodes);
    while (positions.some(p => Math.abs(p.X - center.x) < 180 && Math.abs(p.Y - center.y) < 160)) center.x += 220;
    this.selection = new Set([node.ID]);
    this.change(draft => { draft.Process.Graph.Nodes.push(node); draft.EditorLayout.Nodes[node.ID] = {X: center.x, Y: center.y}; }); this.graph.fit();
  }
  connectionForm(from = '') {
    const form = element('form'); form.className = 'process-connections';
    const nodes = this.model.value.Process.Graph.Nodes;
    const source = element('select'), target = element('select'), verdict = element('input');
    source.setAttribute('aria-label', 'Connect from'); target.setAttribute('aria-label', 'Connect to'); verdict.setAttribute('aria-label', 'Connection answer or outcome');
    for (const node of nodes) for (const select of [source, target]) { const o = element('option', node.Name || node.ID); o.value = node.ID; select.append(o); }
    if (from) source.value = from;
    if (target.value === source.value) target.value = nodes.find(n => n.ID !== source.value)?.ID || '';
    verdict.placeholder = 'Any successful outcome';
    const submit = element('button', 'Connect'); submit.type = 'submit'; form.append(element('h4', 'Connection'), source, target, verdict, submit);
    form.onsubmit = event => { event.preventDefault(); this.addEdge(source.value, target.value, verdict.value); };
    this.inspector.append(form);
  }
  addEdge(from, to, verdict = '') {
    if (!from || !to || from === to) return this.fail(new Error('Choose two different nodes.'));
    if (this.model.value.Process.Graph.Edges.some(e => e.From === from && e.To === to && e.Verdict === verdict)) return this.fail(new Error('That connection already exists.'));
    this.change(d => d.Process.Graph.Edges.push({From: from, To: to, Verdict: verdict}));
  }
  connectGesture(p) {
    if (!p.targetNodeId || p.nodeId === p.targetNodeId || p.port === p.targetPort) return;
    const from = p.port === 'out' ? p.nodeId : p.targetNodeId, to = p.port === 'out' ? p.targetNodeId : p.nodeId;
    const node = this.model.value.Process.Graph.Nodes.find(n => n.ID === from);
    if (node.Kind === 'decision') { this.select(from); this.fail(new Error('Choose the decision answer in Connection, then connect.')); return; }
    this.addEdge(from, to);
  }
  editEdge({id, inputIndex: index}) {
    if (this.busy || !this.discardUnapplied()) return;
    const edges = this.model.value.Process.Graph.Edges;
    if (!Number.isInteger(index) || !edges[index]) return;
    this.selection.clear(); this.graph.setSelection({type: 'edge', id});
    const pinned=this.model.value.EditorLayout.EdgeLabels?.find(label=>edgeKey(label.Edge)===edgeKey(edges[index]))?.Pinned;
    this.form('Edit connection', [{name: 'verdict', label: 'Answer or outcome (blank for any success)', value: edges[index].Verdict},
      {name:'label_visibility',label:'Connector label',options:[option('auto','Automatic'),option('show','Always show'),option('hide','Hide unless selected')],value:pinned===undefined?'auto':pinned?'show':'hide'}], f => this.change(d => {
        const edge=d.Process.Graph.Edges[index],oldKey=edgeKey(edge);
        const updated={...edge,Verdict:f.verdict};
        if(d.Process.Graph.Edges.some((candidate,i)=>i!==index&&edgeKey(candidate)===edgeKey(updated))) throw new Error("That connection already exists. Choose a different answer/outcome.");
        d.EditorLayout.EdgeLabels=(d.EditorLayout.EdgeLabels||[]).filter(label=>edgeKey(label.Edge)!==oldKey);
        edge.Verdict=f.verdict;
        if(f.label_visibility!=='auto')d.EditorLayout.EdgeLabels.push({Edge:clone(edge),Pinned:f.label_visibility==='show'});
      }));
    this.inspector.append(action('Delete connection', () => this.change(d => d.Process.Graph.Edges.splice(index, 1))));
  }
  remove() { if (this.busy || !this.discardUnapplied()) return; this.model.remove(this.selection); this.selection.clear(); this.render(); }
  snippets() {
    if (this.busy || !this.discardUnapplied()) return;
    this.copy();
    this.snippetLibrary = new ProcessSnippetLibrary({api:this.api,selection:{version:1,...clone(this.clipboard)},insert:selection=>{this.clipboard=selection;this.paste();}});
  }
  copy() {
    const draft = this.model.value;
    this.clipboard = {nodes: clone(draft.Process.Graph.Nodes.filter(n => this.selection.has(n.ID))),
      edges: clone(draft.Process.Graph.Edges.filter(e => this.selection.has(e.From) && this.selection.has(e.To))), edgeLabels:clone((draft.EditorLayout.EdgeLabels||[]).filter(label=>this.selection.has(label.Edge.From)&&this.selection.has(label.Edge.To))), positions: Object.fromEntries([...this.selection].filter(id=>draft.EditorLayout.Nodes[id]).map(id=>[id,clone(draft.EditorLayout.Nodes[id])]))};
  }
  paste() {
    if (!this.clipboard?.nodes.length || !this.discardUnapplied()) return;
    const ids = new Map(this.clipboard.nodes.map(n => [n.ID, freshID('node_')])); this.selection = new Set(ids.values());
    this.change(d => {
      for (const original of this.clipboard.nodes) {
        const node = clone(original); node.ID = ids.get(original.ID); node.Name = (node.Name || node.Kind) + ' copy'; d.Process.Graph.Nodes.push(node);
        const p = this.clipboard.positions[original.ID]; d.EditorLayout.Nodes[node.ID] = {X: (p?.X ?? 200) + 40, Y: (p?.Y ?? 150) + 40};
      }
      for (const edge of this.clipboard.edges) d.Process.Graph.Edges.push({...edge, From: ids.get(edge.From), To: ids.get(edge.To)});
      for(const label of this.clipboard.edgeLabels||[]) {
        d.EditorLayout.EdgeLabels ||= [];
        d.EditorLayout.EdgeLabels.push({...clone(label),Edge:{...label.Edge,From:ids.get(label.Edge.From),To:ids.get(label.Edge.To)}});
      }
    });
  }
  overview() {
    if(!this.discardUnapplied()) return;
    const graph=this.model.value.Process.Graph;
    this.form('Process overview', [{name:'description',label:'Process description',multiline:true,value:graph.Description||''},{name:'doc',label:'Process documentation',multiline:true,value:graph.Doc||''},{name:'parameter_syntax',label:'Expand {{ params.key }} in performer input',type:'checkbox',value:this.model.value.Process.ParameterSyntax==='mustache-v1'}], f=>this.change(d=>{for(const [key,value] of [['Description',f.description],['Doc',f.doc]]){if(value)d.Process.Graph[key]=value;else delete d.Process.Graph[key];}if(f.parameter_syntax)d.Process.ParameterSyntax='mustache-v1';else delete d.Process.ParameterSyntax;}));
    this.inspector.append(element('p','Expansion uses exact parameter keys in agent briefs, human/decision questions and individual program arguments. Configuration, routes, documentation and JSON input stay literal.'));
  }
  parameters() {
    if (!this.discardUnapplied()) return;
    this.inspector.replaceChildren(element('h3', 'Parameters'));
    for (const p of this.model.value.Parameters) {
      const row = element('div'); row.append(element('span', `${p.Name} · ${p.Type}${p.Required ? ' · required' : ''}`), action('Edit ' + p.Name, () => this.parameterForm(p)), action('Remove ' + p.Name, () => { this.change(d => { d.Parameters = d.Parameters.filter(q => q.Name !== p.Name); }); this.parameters(); })); this.inspector.append(row);
    }
    this.inspector.append(action('Add parameter', () => this.parameterForm()));
  }
  parameterForm(parameter) {
    this.form('Parameter', [{name: 'name', label: 'Parameter name', value: parameter?.Name, required: true},
      {name: 'type', label: 'Parameter type', options: ['string', 'number', 'boolean', 'object', 'array'].map(v => option(v)), value: parameter?.Type || 'string'},
      {name: 'required', label: 'Required parameter', type: 'checkbox', value: parameter?.Required},
      {name: 'display_name', label: 'Display name (optional)', value: parameter?.DisplayName},
      {name: 'description', label: 'Description', value: parameter?.Description},
      {name: 'doc', label: 'Parameter documentation', multiline:true, value: parameter?.Doc},
      {name: 'default', label: 'Default value (JSON, optional)', value: parameter?.Default === undefined ? '' : JSON.stringify(parameter.Default)}], f => {
        if (this.model.value.Parameters.some(p => p.Name === f.name && p.Name !== parameter?.Name)) throw new Error('Parameter names must be unique.');
        const p = {Name: f.name, Type: f.type, Required: f.required, Description: f.description}; if(f.display_name)p.DisplayName=f.display_name;if(f.doc)p.Doc=f.doc; if (f.default.trim()) p.Default = JSON.parse(f.default);
        this.change(d => { const index = d.Parameters.findIndex(p => p.Name === parameter?.Name); if (index < 0) d.Parameters.push(p); else d.Parameters[index] = p; }); this.parameters();
      });
  }
  outcome() {
    if (!this.discardUnapplied()) return;
    const graph = this.model.value.Process.Graph;
    this.form('Evidence required for success', [
      {name: 'required', label: 'Required nodes', multiple: true, options: graph.Nodes.map(n => option(n.ID, n.Name || n.ID)), value: graph.Outcome?.RequiredNodes || []},
      {name: 'artifact', label: 'Required artifact revision', value: graph.Outcome?.ArtifactRevision},
      {name: 'human', label: 'Require human judgment', type: 'checkbox', value: graph.Outcome?.HumanJudgment}], f => this.change(d => { d.Process.Graph.Outcome = {RequiredNodes: f.required, ArtifactRevision: f.artifact, HumanJudgment: f.human}; }));
  }
  source() { if (this.discardUnapplied()) this.form('Preserved authoring source', [{name: 'source', label: 'Source', multiline: true, value: this.model.value.Source, required: true}], f => this.change(d => { d.Source = f.source; })); }
  key(event) {
    if (event.target.closest('input,textarea,select,[contenteditable=true]')) return;
    const modifier = event.ctrlKey || event.metaKey;
    if (modifier && event.key.toLowerCase() === 'z') { event.preventDefault(); this.history(event.shiftKey ? 'redo' : 'undo'); }
    if (modifier && event.key.toLowerCase() === 'c') { event.preventDefault(); this.copy(); }
    if (modifier && event.key.toLowerCase() === 'v') { event.preventDefault(); this.paste(); }
    if (event.key === 'Delete' || event.key === 'Backspace') { event.preventDefault(); this.remove(); }
  }
  fail(error) { this.errors.replaceChildren(element('li', error.message || String(error))); }
  setBusy(value) {
    this.busy = value;
    // Keep all editable fields stable while validation/save is in flight.
    this.dialog.querySelectorAll('input,textarea,select,button').forEach(control => { control.disabled = value; });
    if (!value) { this.undoButton.disabled = !this.model.undoStack.length; this.redoButton.disabled = !this.model.redoStack.length; }
  }
  async checkDraft() {
    if (this.unapplied) throw new Error('Apply the field changes before validating or saving.');
    const messages = validationMessages(this.model.value);
    this.errors.replaceChildren(...messages.map(m => element('li', m)));
    if (messages.length) return null;
    return this.api('/v2/definitions/validate', {draft: clone(this.model.value)});
  }
  async validate() {
    if (this.busy) return;
    this.setBusy(true);
    try { if (await this.checkDraft()) this.message.textContent = 'Validation passed'; }
    catch (error) { this.fail(error); }
    finally { this.setBusy(false); }
  }
  async save() {
    if (this.busy) return;
    if (!this.dirty() && !this.unapplied) return;
    this.setBusy(true);
    try {
      const validated = await this.checkDraft(); if (!validated) return;
      const fingerprint = JSON.stringify(this.model.value);
      if (!this.pending || this.pending.fingerprint !== fingerprint) this.pending = {fingerprint, requestID: freshID('request_'), revisionID: freshID('revision_')};
      const result = await this.api('/v2/definitions', {request_id: this.pending.requestID, expected_revision: this.baseRevision, draft: {...clone(this.model.value), RevisionID: this.pending.revisionID}});
      // An identical lost-response retry can return a newer current head. Do not
      // silently replace the local draft with a different author's revision.
      if (result.Revision.ContentHash !== validated.Revision.ContentHash) {
        const error = new Error('This save was admitted, but a newer revision is now current. Local edits are retained.'); error.code = 'conflict'; throw error;
      }
      this.model = new ProcessDraft(draftFromResult(result)); this.baseRevision = result.Definition.Revision;
      this.saved = JSON.stringify(this.model.value); this.pending = null; this.render(); await this.onSaved();
    } catch (error) {
      this.fail(error);
      if (error.code === 'conflict') {
        const reload = action('Load saved revision (discard local edits)', async () => {
          if (this.busy || !confirm('Discard local edits and load the saved revision?')) return;
          this.setBusy(true);
          try { const r = await this.api('/v2/definitions/' + encodeURIComponent(this.model.value.ID)); this.model = new ProcessDraft(draftFromResult(r)); this.baseRevision = r.Definition.Revision; this.saved = JSON.stringify(this.model.value); this.pending = null; this.render(); }
          catch (e) { this.fail(e); } finally { this.setBusy(false); }
        });
        this.errors.append(reload);
      }
    } finally { this.setBusy(false); }
  }
  export() {
    const blob = new Blob([JSON.stringify({format: 'tclaude-process-v2', draft: this.model.value}, null, 2)], {type: 'application/json'});
    const url = URL.createObjectURL(blob), link = element('a'); link.href = url; link.download = 'process.json'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  import() {
    if (this.busy || !this.discardUnapplied()) return;
    const file = element('input'); file.type = 'file'; file.accept = '.json,application/json';
    file.onchange = async () => {
      try {
        if (!file.files[0]) return;
        if (file.files[0].size > 512 * 1024) throw new Error('Process import exceeds 512 KiB.');
        const value = JSON.parse(await file.files[0].text());
        if (value.format !== 'tclaude-process-v2' || value.draft?.Kind !== 'process' || !Array.isArray(value.draft.Process?.Graph?.Nodes)) throw new Error('Choose an exported v2 process.');
        if (this.dirty() && !confirm('Replace this draft with an imported copy?')) return;
        const draft = value.draft; draft.ID = freshID('definition_'); delete draft.RevisionID; draft.EditorLayout ||= {Nodes: {}};
        this.setBusy(true);
        const validated = await this.api('/v2/definitions/validate', {draft});
        this.model = new ProcessDraft(draftFromResult(validated)); this.baseRevision = 0; this.pending = null; this.selection.clear(); this.render(); this.graph.fit();
      } catch (error) { this.fail(error); } finally { this.setBusy(false); }
    }; file.click();
  }
  close() {
    if (this.busy) return;
    if ((this.dirty() || this.unapplied) && !confirm('Discard unsaved process changes?')) return;
    this.snippetLibrary?.close(); window.removeEventListener('beforeunload', this.beforeUnload); this.graph.dispose(); this.dialog.close(); this.dialog.remove();
  }
}
