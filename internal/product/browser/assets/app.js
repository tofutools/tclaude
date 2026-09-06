'use strict';
const $ = id => document.getElementById(id);
const el = (tag, text, cls) => { const n=document.createElement(tag); if(text!==undefined)n.textContent=text; if(cls)n.className=cls; return n; };
let snapshot = {}, submitting = false, terminal, terminalSocket;
const requestID = () => 'r_' + crypto.randomUUID();
function showError(error) { const target=$('editor').open?$('editor-error'):$('error');target.textContent=error.message || String(error);target.hidden=false; }
async function api(path, body, method) {
 const response=await fetch(path,{method:method || (body===undefined?'GET':'POST'),credentials:'same-origin',headers:body===undefined?{}:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});
 if(!response.ok){let code=await response.text();try{code=JSON.parse(code).code}catch{};throw new Error(({conflict:'The saved state changed. Refresh and review before trying again.',unsupported:'This operation is not supported by the configured provider or host.',forbidden:'Your current authority does not allow this operation.',uncertain:'The effect is uncertain. Inspect its state before attempting another operation.',invalid_request:'Some inputs are invalid. Check the values and required fields.'})[code] || code || `Request failed (${response.status})`)}
 if(response.status===204)return;
 return response.json();
}
function button(text, action) { const b=el('button',text);const id=requestID();b.type='button';b.onclick=async()=>{b.disabled=true;$('error').hidden=true;try{await action(id)}catch(e){showError(e)}finally{b.disabled=false}};return b; }
function empty(parent,text){parent.append(el('p',text,'empty'))}
async function refresh(){
 snapshot=await api('/v2/snapshot');render();$('connection').textContent=`Updated ${new Date().toLocaleTimeString()}`;
}
function edit(title,fields,save){
 $('editor-title').textContent=title;$('editor-fields').replaceChildren();$('editor-error').hidden=true;let fingerprint='',submissionID='';
 for(const field of fields){
  const label=el('label',field.label);let input;
  if(field.options){input=el('select');for(const option of field.options){const o=el('option',typeof option==='string'?option:option.label);o.value=typeof option==='string'?option:option.value;input.append(o)}}
  else input=el(field.multiline?'textarea':'input');
  if(field.multiple)input.multiple=true;input.name=field.name;input.setAttribute('aria-label',field.label);if(field.value!==undefined || !field.options)input.value=field.value??'';input.required=field.required!==false;label.append(input);$('editor-fields').append(label);
 }
 $('editor-form').onsubmit=async e=>{e.preventDefault();if(submitting)return;submitting=true;const submit=e.submitter;if(submit)submit.disabled=true;
  try{const data=new FormData(e.target),form=Object.fromEntries(data);for(const field of fields)if(field.multiple)form[field.name]=data.getAll(field.name);const next=JSON.stringify(form);if(fingerprint!==next){fingerprint=next;submissionID=requestID()}form.requestID=submissionID;await save(form);$('editor').close();await refresh()}catch(error){showError(error)}finally{submitting=false;if(submit)submit.disabled=false}
 };
 $('editor').showModal();
}
function desiredFields(desired={}){return[
 {name:'name',label:'Name',value:desired.name},
 {name:'harness',label:'Harness',value:desired.Harness||'claude',options:['claude','codex','opencode','copilot']},
 {name:'model',label:'Model',value:desired.Model},
 {name:'cwd',label:'Working directory',value:desired.WorkingDirectory},
 {name:'approval',label:'Approval',value:desired.Approval||'supervised',options:['supervised','automatic']},
 {name:'sandbox',label:'Confinement',value:desired.Sandbox||'workspace_write',options:['read_only','workspace_write','unconfined']}
]}
function configuration(form){return{Harness:form.harness,Model:form.model,WorkingDirectory:form.cwd,Approval:form.approval,Sandbox:form.sandbox}}
function agentRow(agent){
 const row=el('div',undefined,'row');row.append(el('span',agent.Name,'name'));
 const execution=(snapshot.executions||[]).find(e=>e.id===agent.PrimaryExecutionID);
 row.append(el('span',execution?`${execution.state} · context ${execution.context_readiness}`:'offline','status'));
 row.append(el('span',`${agent.Desired.Harness} / ${agent.Desired.Model}`,'muted'));
 const actions=el('div',undefined,'actions');
 actions.append(button('Configure',()=>edit('Configure agent',desiredFields({...agent.Desired,name:agent.Name}),f=>api(`/v2/agents/${encodeURIComponent(agent.ID)}`,{name:f.name,desired:configuration(f),expected_revision:agent.Revision},'PUT'))));
 if(!execution || ['exited','failed'].includes(execution.state)){
  actions.append(button('Start',async id=>{await api('/v2/launch',{request_id:id,target:{agent:{agent_id:agent.ID,expected_revision:agent.Revision}}});await refresh()}));
  const associations=(snapshot.associations||[]).filter(a=>a.AgentID===agent.ID);
  if(associations.length)actions.append(button('Resume',()=>edit('Resume history',[{name:'conversation',label:'Conversation',options:associations.map(a=>({value:a.ConversationID,label:a.ConversationID}))}],async f=>{
   const a=associations.find(a=>a.ConversationID===f.conversation);
   await api('/v2/resume',{request_id:f.requestID,target:{agent:{agent_id:agent.ID,expected_revision:agent.Revision}},conversation_id:a.ConversationID,expected_association_revision:a.Revision});
  })));
 }else{
  actions.append(button('Attach',()=>attach(execution)));
  actions.append(button('Observe',async()=>{await api('/v2/observe',{execution_id:execution.id});await refresh()}));
  actions.append(button('Send input',()=>edit('Send input',[{name:'text',label:'Input',multiline:true}],f=>api('/v2/interact',{request_id:f.requestID,execution_id:execution.id,text:f.text}))));
  actions.append(button('Stop',async id=>{await api('/v2/stop',{request_id:id,execution_id:execution.id,force:false});await refresh()}));
 }
 row.append(actions);return row;
}
function render(){
 const roster=$('roster');roster.replaceChildren();const agents=snapshot.agents||[];const groups=snapshot.groups||[];
 const grouped=new Set(groups.flatMap(g=>g.Members||[]));
 for(const group of [...groups,{Name:'Ungrouped',Members:agents.filter(a=>!grouped.has(a.ID)).map(a=>a.ID)}]){
  if(!group.Members?.length && !group.ID)continue;
  const card=el('div',undefined,'group');card.append(el('h2',group.Name));
  for(const id of group.Members||[]){const a=agents.find(a=>a.ID===id);if(a)card.append(agentRow(a))}roster.append(card);
 }
 if(!agents.length)empty(roster,'No agents yet. Create an agent to save its configuration before starting work.');
 const spaces=$('workspace-list');spaces.replaceChildren();
 for(const workspace of snapshot.workspaces||[])spaces.append(workspaceCard(workspace));
 if(!snapshot.workspaces?.length)empty(spaces,'No registered workspaces.');
 const work=$('work-list');work.replaceChildren();
 for(const result of snapshot.work_runs||[])work.append(workCard(result));
 if(!snapshot.work_runs?.length)empty(work,'No work runs.');
 const messages=$('message-list');messages.replaceChildren();
 for(const message of [...snapshot.messages||[]].reverse()){
  const card=el('article',undefined,'card');card.append(el('strong',message.Sender.AgentID||message.Sender.Kind),el('p',new Date(message.CreatedAt).toLocaleString(),'muted'),el('pre',message.Body));
  card.append(el('p',(message.Recipients||[]).map(r=>`${r.AgentID} · ${r.ReadAt?'read':'unread'}`).join(', '),'muted'));messages.append(card)
 }
 if(!snapshot.messages?.length)empty(messages,'No messages.');
}
async function selectTab(tab){
 for(const n of document.querySelectorAll('main > section'))n.hidden=n.id!==tab;
 for(const n of document.querySelectorAll('[data-tab]'))n.setAttribute('aria-current',String(n.dataset.tab===tab));
 if(tab==='processes')await renderDefinitions();
 if(tab==='decisions')await renderDecisions();
 if(tab==='access'){
  const data=await api('/v2/authority');const list=$('access-list');list.replaceChildren();
  for(const grant of data.Grants||[]){const row=el('div',undefined,'row');row.append(el('strong',grant.Action),el('span',grant.Subject.AgentID||grant.Subject.Kind),el('span',grant.Resource.Kind),button('Revoke',async()=>{await api(`/v2/authority/grants/${encodeURIComponent(grant.ID)}`,{expected_revision:grant.Revision},'DELETE');await selectTab('access')}));list.append(row)}
  for(const role of data.Roles||[]){const card=el('div',undefined,'card');card.append(el('strong',role.Name),el('p',(role.Actions||[]).join(', ')));list.append(card)}
  if(!list.childNodes.length)empty(list,'No grants or roles.');
 }
}
$('refresh').onclick=()=>refresh().catch(showError);
$('cancel').onclick=()=>$('editor').close();
$('logout').onclick=async()=>{try{await api('/session',undefined,'DELETE');closeTerminal();snapshot={};render();$('connection').textContent='Signed out';showError(new Error('Open a new dashboard login link to sign in.'))}catch(e){showError(e)}};
for(const tab of document.querySelectorAll('[data-tab]'))tab.onclick=()=>selectTab(tab.dataset.tab).catch(showError);
$('new-agent').onclick=()=>edit('New agent',desiredFields(),f=>api('/v2/agents',{id:f.requestID,name:f.name,desired:configuration(f)}));
$('new-group').onclick=()=>edit('New group',[{name:'name',label:'Name'},{name:'members',label:'Members',multiple:true,required:false,options:(snapshot.agents||[]).map(a=>({value:a.ID,label:a.Name}))}],f=>api('/v2/groups',{id:f.requestID,name:f.name,members:f.members}));
$('compose').onclick=()=>edit('Compose message',[{name:'recipient',label:'Recipient',options:(snapshot.agents||[]).map(a=>({value:a.ID,label:a.Name}))},{name:'body',label:'Message',multiline:true}],f=>api('/v2/messages',{request_id:f.requestID,recipients:[f.recipient],body:f.body}));
$('search-history').onsubmit=async e=>{e.preventDefault();try{
 const data=await api('/v2/history/search',{query:new FormData(e.target).get('query')});const list=$('histories');list.replaceChildren();
 for(const entry of data.Entries||[])list.append(historyCard(entry));
 if(!data.Entries?.length)empty(list,'No matching catalogued histories. Source coverage may be incomplete.');
}catch(error){showError(error)}};
(async()=>{
 const fragment=new URLSearchParams(location.hash.slice(1));const token=fragment.get('login');history.replaceState(null,'',location.pathname);
 if(token)await api('/session',{token});await refresh();await selectTab('groups');
})().catch(e=>{$('connection').textContent='Not connected';showError(e)});

async function attach(execution){
 closeTerminal();await selectTab('terminals');
 $('terminal-status').textContent=`Connecting to ${execution.id}…`;
 terminal=new Terminal({cols:80,rows:24,convertEol:false,theme:{background:'#0f1419',foreground:'#d4d4d4'}});
 terminal.open($('terminal'));terminal.focus();
 const url=new URL('/v2/attach',location.href);url.protocol=location.protocol==='https:'?'wss:':'ws:';
 url.searchParams.set('execution_id',execution.id);url.searchParams.set('request_id',requestID());
 const socket=new WebSocket(url,'tclaude.terminal.v1');terminalSocket=socket;socket.binaryType='arraybuffer';
 terminal.onData(data=>{if(socket.readyState===WebSocket.OPEN)socket.send(new TextEncoder().encode(data))});
 socket.onopen=()=>{$('terminal-status').textContent=`Attached to ${execution.id}. Disconnecting leaves the workload running.`};
 socket.onmessage=event=>{if(terminalSocket!==socket)return;if(typeof event.data==='string'){try{const info=JSON.parse(event.data);if(info.type==='capabilities')$('resize-terminal').disabled=!info.resize}catch{showError(new Error('Invalid terminal control response'))}}else terminal.write(new Uint8Array(event.data))};
 socket.onerror=()=>{if(terminalSocket===socket)$('terminal-status').textContent='Attachment unavailable. The workload state is unchanged.'};
 socket.onclose=()=>{if(terminalSocket===socket)$('terminal-status').textContent='Disconnected. Refresh the roster to inspect workload state.'};
}
function closeTerminal(){$('resize-terminal').disabled=true;if(terminalSocket){terminalSocket.close();terminalSocket=undefined}if(terminal){terminal.dispose();terminal=undefined}$('terminal').replaceChildren();$('terminal-status').textContent='Disconnected. Workload state is unchanged.'}
$('close-terminal').onclick=closeTerminal;
window.addEventListener('pagehide',closeTerminal);

$('terminal-size').onsubmit=e=>{e.preventDefault();if(!terminal||!terminalSocket||terminalSocket.readyState!==WebSocket.OPEN)return;const form=new FormData(e.target),columns=Number(form.get('columns')),rows=Number(form.get('rows'));terminalSocket.send(JSON.stringify({type:'resize',columns,rows}));terminal.resize(columns,rows)};
function workspaceCard(space){
 const card=el('div',undefined,'card');card.append(el('strong',space.ID),el('p',space.Observation?.ActualPath||space.Intent?.IntendedPath||''),el('span',space.State,'status'));
 const actions=el('div',undefined,'actions');
 actions.append(button('Inspect',async()=>{await api(`/v2/workspaces/${encodeURIComponent(space.ID)}`);await refresh()}));
 if(space.State==='available'){
  actions.append(button('Open shell',()=>edit('Open unconfined shell',[{name:'confirm',label:'This shell runs without OS confinement',options:['Start unconfined shell']}],f=>api('/v2/shells',{request_id:f.requestID,workspace_id:space.ID,expected_revision:space.Revision,sandbox:'unconfined'}))));
  if(space.Intent?.Ownership==='owned')actions.append(button('Remove checkout',()=>edit('Remove checkout',[{name:'confirm',label:'Type the workspace ID to confirm removal'},{name:'dirty',label:'Uncommitted changes',options:[{value:'false',label:'Refuse if dirty'},{value:'true',label:'Discard uncommitted changes'}]}],f=>{if(f.confirm!==space.ID)throw new Error('Workspace ID does not match');return api('/v2/workspaces/remove',{request_id:f.requestID,workspace_id:space.ID,expected_revision:space.Revision,destructive:f.dirty==='true'})})));
 }else if(space.State==='removed')actions.append(button('Restore checkout',async id=>{await api('/v2/workspaces/restore',{request_id:id,workspace_id:space.ID,expected_revision:space.Revision});await refresh()}));
 for(const execution of snapshot.executions||[]){if((snapshot.workspace_uses||[]).some(u=>u.WorkspaceID===space.ID&&u.ExecutionID===execution.id&&!u.ReleasedAt)&& !['exited','failed'].includes(execution.state))actions.append(button('Attach shell',()=>attach(execution)),button('Stop shell',async id=>{await api('/v2/stop',{request_id:id,execution_id:execution.id,force:false});await refresh()}))}
 card.append(actions);return card;
}
const workspaceFields=[{name:'repository',label:'Repository path'},{name:'path',label:'Checkout path'},{name:'base',label:'Base commit or branch',value:'HEAD'},{name:'branch',label:'Worker branch'}];
$('create-checkout').onclick=()=>edit('Create owned checkout',workspaceFields,f=>api('/v2/workspaces/create',{request_id:f.requestID,id:f.requestID,intent:{Repository:f.repository,IntendedPath:f.path,BaseRevision:f.base,Branch:f.branch,Provenance:'platform_created',Ownership:'owned',RetainOnFinish:true}}));
$('register-workspace').onclick=()=>edit('Register existing directory',[{name:'path',label:'Directory path'}],f=>api('/v2/workspaces/register',{request_id:f.requestID,id:f.requestID,intent:{IntendedPath:f.path,Provenance:'registered',Ownership:'external',RetainOnFinish:true}}));
$('refresh-history').onclick=()=>edit('Refresh configured history source',[{name:'harness',label:'Harness',options:['claude','codex','opencode','copilot']},{name:'source',label:'Configured source name'}],f=>api('/v2/history/refresh',{harness:f.harness,source:f.source}));
function selection(entry,point){return{ConversationID:entry.ConversationID,ExpectedConversationRevision:entry.Revision,PointID:point?.ID||'',ExpectedPointRevision:point?.Revision||0}}
function historyCard(entry){
 const card=el('article',undefined,'card');card.append(el('strong',entry.Title||entry.ConversationID),el('p',`${entry.Harness} · ${entry.Availability||'unknown'}`));
 card.append(button('Read',async()=>{
  const read=await api('/v2/history/read',{selection:selection(entry)});const content=el('div');
  for(const turn of read.Turns||[]){const text=(turn.Parts||[]).map(p=>p.Text||'').join('\n');content.append(el('strong',turn.Role),el('pre',text))}
  card.append(content,button('Start work from this history',()=>startWork(read)));
 }),button('Edit title',()=>edit('History title',[{name:'title',label:'Title',value:entry.Title||'',required:false}],f=>api('/v2/history/metadata',{request_id:f.requestID,conversation_id:entry.ConversationID,expected_revision:entry.Revision,title:f.title,archived:!!entry.Archived}))));return card;
}
function startWork(read){
 const spaces=(snapshot.workspaces||[]).filter(s=>s.State==='available');const agents=(snapshot.agents||[]).filter(a=>{const e=(snapshot.executions||[]).find(e=>e.id===a.PrimaryExecutionID);return !e||['exited','failed'].includes(e.state)});
 if(!spaces.length||!agents.length)throw new Error('Create an available workspace and an offline worker first.');
 edit('Start bounded work',[
  {name:'workspace',label:'Workspace',options:spaces.map(s=>({value:s.ID,label:s.Observation.ActualPath||s.ID}))},
  {name:'worker',label:'Worker',options:agents.map(a=>({value:a.ID,label:a.Name}))},
  {name:'mode',label:'History use',options:[{value:'fresh_handoff',label:'Fresh conversation with handoff'},{value:'fork',label:'Exact fork (requires provider support)'}]},
  {name:'point',label:'History point',options:[{value:'',label:'Persisted head'},...(read.Points||[]).map(p=>({value:p.ID,label:`${p.Kind} · ${p.ID}`}))]},
  {name:'handoff',label:'Handoff context (used for fresh conversation)',multiline:true,required:false},
  {name:'brief',label:'Work request and acceptance criteria',multiline:true}
 ],f=>{const space=spaces.find(s=>s.ID===f.workspace),worker=agents.find(a=>a.ID===f.worker),point=(read.Points||[]).find(p=>p.ID===f.point);return api('/v2/work',{request_id:f.requestID,id:f.requestID,spec:{SourceMode:f.mode,History:selection(read.Entry,point),FreshHandoff:f.handoff,WorkspaceID:space.ID,WorkspaceRevision:space.Revision,WorkerAgentID:worker.ID,WorkerAgentRevision:worker.Revision,WorkerDesired:{...worker.Desired,WorkingDirectory:space.Observation.ActualPath},Brief:f.brief,Outcome:{Mode:'human_decision'}}})});
}
function workCard(result){
 const run=result.run,card=el('article',undefined,'card');card.append(el('strong',run.id),el('p',run.state),el('pre',run.spec.Brief||''));
 for(const attempt of run.node_attempts||[]){const node=el('div',undefined,'row');node.append(el('strong',attempt.Ref.NodeID),el('span',attempt.State),el('span',attempt.Outcome||attempt.Detail||''));if(attempt.DecisionID)node.append(button('Open decision',()=>selectTab('decisions')));card.append(node)}
 for(const evidence of result.evidence||[])card.append(el('p',`${evidence.kind} · ${evidence.reporter.agent_id||evidence.reporter.kind}`),el('pre',evidence.detail));
 if(result.decision)card.append(el('strong',`${result.decision.decision}: ${result.decision.reason}`));
 const actions=el('div',undefined,'actions');
 const pending=(run.attempts||[]).find(a=>a.step==='await_evidence'&&a.state==='pending');
 if(run.state==='waiting'&&pending)actions.append(button('Record evidence',()=>edit('Record attributed evidence',[{name:'detail',label:'Evidence',multiline:true},{name:'artifact',label:'Artifact revision',required:false}],f=>api('/v2/work/evidence',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,step:pending.step,attempt:pending.attempt,kind:'artifact',detail:f.detail,artifact_revision:f.artifact}))));
 const evaluate=(run.attempts||[]).find(a=>a.step==='evaluate'&&a.state==='pending');
 if(evaluate&&run.state==='waiting')actions.append(button('Decide outcome',()=>edit('Decide outcome',[{name:'decision',label:'Decision',options:['accept','reject']},{name:'reason',label:'Reason',multiline:true}],f=>api('/v2/work/decision',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,step:evaluate.step,attempt:evaluate.attempt,decision:f.decision,reason:f.reason}))));
 if(!['succeeded','failed','cancelled'].includes(run.state))actions.append(button('Cancel work',()=>edit('Cancel work',[{name:'reason',label:'Reason',multiline:true}],f=>api('/v2/work/cancel',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,reason:f.reason}))));
 if(run.state==='uncertain')actions.append(button('Resolve uncertainty',()=>edit('Confirm effect did not occur',[{name:'confirm',label:'Type the work ID to confirm no effect occurred'},{name:'reason',label:'Evidence supporting this conclusion',multiline:true}],f=>{if(f.confirm!==run.id)throw new Error('Work ID does not match');return api('/v2/work/resolve',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,reason:f.reason})})));
 card.append(actions);return card;
}

$('new-grant').onclick=()=>edit('Grant agent permission',[
 {name:'subject',label:'Agent receiving permission',options:(snapshot.agents||[]).map(a=>({value:a.ID,label:a.Name}))},
 {name:'action',label:'Action',options:['message.send','status.read','execution.stop','execution.interact','execution.attach']},
 {name:'target',label:'Target agent',options:(snapshot.agents||[]).map(a=>({value:a.ID,label:a.Name}))}
],async f=>{await api(`/v2/authority/grants/${encodeURIComponent(f.requestID)}`,{subject:{Kind:'agent',AgentID:f.subject},action:f.action,resource:{Kind:'agent',AgentID:f.target},expected_revision:0},'PUT');await selectTab('access')});

async function renderDecisions(){
 const results=await api('/v2/decisions'),list=$('decision-list');list.replaceChildren();
 for(const result of results||[]){
  const window=result.Window,card=el('article',undefined,'card');
  card.append(el('h2',window.Question||'Decision'),el('p',`${window.Attempt.RunID} · ${window.Attempt.NodeID}`),el('p',`Expires ${new Date(window.ExpiresAt).toLocaleString()}`,'muted'));
  card.append(button('Answer',()=>edit('Answer decision',[{name:'answer',label:'Answer',options:window.PermittedAnswers||[]},{name:'reason',label:'Reason',multiline:true}],async f=>{
   await api('/v2/decisions/submit',{request_id:f.requestID,decision_id:window.ID,expected_window_revision:window.Revision,answer:f.answer,reason:f.reason});await renderDecisions();
  })));
  list.append(card);
 }
 if(!results?.length)empty(list,'No decisions awaiting your answer.');
}
async function renderDefinitions(){
 const definitions=await api('/v2/definitions'),list=$('definition-list');list.replaceChildren();
 for(const definition of definitions||[]){
  const card=el('article',undefined,'card');card.append(el('h2',definition.Name),el('p',`${definition.Kind} · revision ${definition.Revision}`,'muted'));
  card.append(button('Inspect definition',async()=>{const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID));card.append(el('pre',result.Revision.Source))}));
  if(definition.Kind==='process')card.append(button('Start process',async()=>{
   const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID));
   const revision=result.Revision;
   if(revision.Parameters?.length)throw new Error('This process requires typed parameters. Use the process start command with its pinned definition and explicit parameters.');
   const fields=[{name:'workspace',label:'Workspace',options:(snapshot.workspaces||[]).filter(w=>w.State==='available').map(w=>({value:w.ID,label:w.Intent.Name||w.Observation.ActualPath||w.ID}))},{name:'minutes',label:'Maximum run time in minutes',value:'60'}];
   edit('Start pinned process',fields,async f=>{
    const minutes=Number(f.minutes);if(!Number.isFinite(minutes)||minutes<=0||minutes>10080)throw new Error('Choose a run duration between 1 and 10080 minutes.');
    const programs=(revision.Process.Graph.Nodes||[]).filter(n=>n.Performer?.Program).map(n=>n.Performer.Program.Profile);
    await api('/v2/processes',{request_id:f.requestID,id:f.requestID,start:{Definition:{DefinitionID:definition.ID,RevisionID:revision.ID,ContentHash:revision.ContentHash,Kind:'process'},Scope:{WorkspaceID:f.workspace},AuthorizedProgramProfiles:programs,Deadline:new Date(Date.now()+minutes*60000).toISOString()}});
   });
  }));
  list.append(card);
 }
 if(!definitions?.length)empty(list,'No saved definitions. Author a definition with the definition save command.');
}
