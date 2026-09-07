'use strict';
const $ = id => document.getElementById(id);
const el = (tag, text, cls) => { const n=document.createElement(tag); if(text!==undefined)n.textContent=text; if(cls)n.className=cls; return n; };
let snapshot = {}, submitting = false;
for(const tab of document.querySelectorAll('[data-tab]'))tab.disabled=true;
document.querySelector('main').inert=true;
const requestID = () => 'r_' + crypto.randomUUID();
const terminals = new TerminalWorkspace({requestID});
const presentation = new PresentationWorkspace({api});
const authorityWorkspace = new AuthorityWorkspace({host:$('access-list'),api,el,button,edit,getSnapshot:()=>snapshot,report:showError});
const rosterWorkspace = new RosterWorkspace({host:$('roster'),api,el,button,edit,refresh});
function showError(error) { const target=$('editor').open?$('editor-error'):$('error');target.textContent=error.message || String(error);target.hidden=false; }
async function api(path, body, method) {
 const response=await fetch(path,{method:method || (body===undefined?'GET':'POST'),credentials:'same-origin',headers:body===undefined?{}:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});
 if(!response.ok){let code=await response.text();try{code=JSON.parse(code).code}catch{};const error=new Error(({conflict:'The saved state changed. Refresh and review before trying again.',unsupported:'This operation is not supported by the configured provider or host.',forbidden:'Your current authority does not allow this operation.',uncertain:'The effect is uncertain. Inspect its state before attempting another operation.',invalid_request:'Some inputs are invalid. Check the values and required fields.'})[code] || code || `Request failed (${response.status})`);error.code=code;error.status=response.status;throw error}
 if(response.status===204)return;
 return response.json();
}
function button(text, action) { const b=el('button',presentation.label(text));b.dataset.uiText=text;const id=requestID();b.type='button';b.onclick=async()=>{b.disabled=true;$('error').hidden=true;try{await action(id)}catch(e){showError(e)}finally{b.disabled=false}};return b; }
function empty(parent,text){parent.append(el('p',text,'empty'))}
async function refresh(){
 snapshot=await api('/v2/snapshot');render();$('connection').textContent=`Updated ${new Date().toLocaleTimeString()}`;
}
function edit(title,fields,save,{skipUnchanged=false}={}){
 $('editor-title').textContent=presentation.label(title);$('editor-fields').replaceChildren();$('editor-error').hidden=true;let fingerprint='',submissionID='';
 for(const field of fields){
  const label=el('label',field.label);let input;
  if(field.options){input=el('select');for(const option of field.options){const o=el('option',typeof option==='string'?option:option.label);o.value=typeof option==='string'?option:option.value;input.append(o)}}
  else input=el(field.multiline?'textarea':'input');
  if(field.file)input.type='file';
  if(field.multiple)input.multiple=true;input.name=field.name;input.setAttribute('aria-label',field.label);
  if(field.options&&field.value!==undefined){const values=field.multiple?(field.value||[]):[String(field.value)];for(const value of values){if(!Array.from(input.options).some(o=>o.value===String(value))){const o=el('option','Retained: '+value);o.value=value;input.append(o)}}if(field.multiple){for(const o of input.options)o.selected=values.includes(o.value)}else input.value=field.value}
  else if(!field.file&&!field.options)input.value=field.value??'';
  input.required=field.required!==false;label.append(input);$('editor-fields').append(label);
 }
 const readForm=()=>{const data=new FormData($('editor-form')),form=Object.fromEntries(data);for(const field of fields)if(field.multiple)form[field.name]=data.getAll(field.name);return form};const initial=JSON.stringify(readForm());
 $('editor-form').onsubmit=async e=>{e.preventDefault();if(submitting)return;submitting=true;const submit=e.submitter;if(submit)submit.disabled=true;
  try{const form=readForm();if(skipUnchanged&&JSON.stringify(form)===initial){$('editor').close();return}const next=JSON.stringify(form,(_,value)=>value instanceof File?{name:value.name,size:value.size,modified:value.lastModified}:value);if(fingerprint!==next){fingerprint=next;submissionID=requestID()}form.requestID=submissionID;await save(form);$('editor').close();await refresh()}catch(error){showError(error)}finally{submitting=false;if(submit)submit.disabled=false}
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
 if(agent.ConfigurationProfile)row.append(el('span','Saved configuration revision','muted'));
 const actions=el('div',undefined,'actions');
 actions.append(button('Activity',async()=>{activityTarget={AgentID:agent.ID};await selectTab('activity')}));
 if(execution?.conversation_id)actions.append(button('Usage',async()=>{usageTarget={ConversationID:execution.conversation_id};await selectTab('usage')}));
 if(agent.Lifecycle==='retired'){row.append(el('span','retired','status'));actions.append(button('Reactivate',async()=>{await api(`/v2/agents/${encodeURIComponent(agent.ID)}/reactivate`,{expected_revision:agent.Revision});await refresh()}));row.append(actions);return row}
 actions.append(button('Configure',()=>edit('Configure agent',[...desiredFields({...agent.Desired,name:agent.Name}),...agentMetadataFields(agent)],f=>api(`/v2/agents/${encodeURIComponent(agent.ID)}`,{name:f.name,desired:configuration(f),task_reference:f.task,notifications:{DirectMessage:f.notify},expected_revision:agent.Revision},'PUT'))));
 if(!execution || ['exited','failed'].includes(execution.state)){
  actions.append(button('Retire',()=>edit('Retire agent',[{name:'reason',label:'Reason',multiline:true}],f=>api(`/v2/agents/${encodeURIComponent(agent.ID)}/retire`,{expected_revision:agent.Revision,reason:f.reason}))));
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
 actions.append(button('Clone configuration',()=>edit('Create independent agent',[{name:'name',label:'Name',value:agent.Name+' copy'}],f=>api('/v2/agents',{id:f.requestID,name:f.name,clone_source_agent_id:agent.ID,...(agent.ConfigurationProfile?{configuration_profile:agent.ConfigurationProfile}:{desired:agent.Desired})}))));
 row.append(actions);return row;
}
function render(){
 presentation.update(snapshot);
 renderGroupControls(snapshot,{host:$('group-management'),el,button,edit,api,refresh});
 rosterWorkspace.update(snapshot,agentRow);
 const spaces=$('workspace-list');spaces.replaceChildren();
 for(const workspace of snapshot.workspaces||[])spaces.append(workspaceCard(workspace));
 if(!snapshot.workspaces?.length)empty(spaces,'No registered workspaces.');
 const work=$('work-list');work.replaceChildren();
 for(const result of snapshot.work_runs||[])work.append(workCard(result));
 if(!snapshot.work_runs?.length)empty(work,'No work runs.');
 const messages=$('message-list');messages.replaceChildren();
 for(const message of [...snapshot.messages||[]].reverse()){
  const card=el('article',undefined,'card');card.append(el('strong',message.Subject||'Message'),el('p',`${message.Sender.AgentID||message.Sender.Kind} · ${new Date(message.CreatedAt).toLocaleString()}`,'muted'),el('pre',message.Body));
  card.append(el('p',(message.Recipients||[]).map(r=>`${r.Audience==='cc'?'CC':'To'} ${r.AddressKind==='operator'?'Operator':r.AgentID} · ${r.ReadAt?'read':'unread'}${r.NotificationOutcome?' · notice '+r.NotificationOutcome.replaceAll('_',' '):''}`).join(', '),'muted'));
  if(message.ParentMessageID)card.append(el('p','Reply in an existing thread','muted'));
  card.append(button('Reply all',()=>composeMessage(message)));
  if((message.Recipients||[]).some(r=>r.AddressKind==='operator'&&!r.ReadAt))card.append(button('Mark read',async id=>{await api(`/v2/messages/${encodeURIComponent(message.ID)}/read`,{request_id:id,operator:true});await refresh()}));
  for(const attachment of message.Attachments||[])card.append(button(`Download ${attachment.Filename}`,()=>downloadAttachment(attachment)));
  messages.append(card);
 }
 if(!snapshot.messages?.length)empty(messages,'No messages.');
}
async function selectTab(tab){
 for(const n of document.querySelectorAll('main > section'))n.hidden=n.id!==tab;
 for(const n of document.querySelectorAll('[data-tab]'))n.setAttribute('aria-current',String(n.dataset.tab===tab));
 if(tab==='configurations')await renderConfigurations();
 if(tab==='usage')await renderUsage();
 if(tab==='activity')await renderActivity();

 if(tab==='processes')await renderDefinitions();
 if(tab==='automation')await renderAutomation();
 if(tab==='decisions')await renderDecisions();
 if(tab==='access')await authorityWorkspace.render();
}
$('refresh').onclick=()=>refresh().catch(showError);
$('cancel').onclick=()=>$('editor').close();
$('logout').onclick=async()=>{try{await api('/session',undefined,'DELETE');closeTerminal();presentation.stop();snapshot={};render();$('connection').textContent='Signed out';showError(new Error('Open a new dashboard login link to sign in.'))}catch(e){showError(e)}};
for(const tab of document.querySelectorAll('[data-tab]'))tab.onclick=()=>selectTab(tab.dataset.tab).catch(showError);
$('new-agent').onclick=()=>edit('New agent',[...desiredFields(),...agentMetadataFields()],f=>api('/v2/agents',{id:f.requestID,name:f.name,desired:configuration(f),task_reference:f.task,notifications:{DirectMessage:f.notify}}));
$('new-group').onclick=()=>edit('New group',[{name:'name',label:'Name'},{name:'members',label:'Members',multiple:true,required:false,options:(snapshot.agents||[]).map(a=>({value:a.ID,label:a.Name}))}],f=>api('/v2/groups',{id:f.requestID,name:f.name,members:f.members}));
$('compose').onclick=()=>composeMessage();
$('search-history').onsubmit=async e=>{e.preventDefault();try{
 const data=await api('/v2/history/search',{query:new FormData(e.target).get('query')});const list=$('histories');list.replaceChildren();
 for(const entry of data.Entries||[])list.append(historyCard(entry));
 if(!data.Entries?.length)empty(list,'No matching catalogued histories. Source coverage may be incomplete.');
}catch(error){showError(error)}};
(async()=>{
 const fragment=new URLSearchParams(location.hash.slice(1));const token=fragment.get('login'),requested=new URLSearchParams(location.search).get('terminal');history.replaceState(null,'',location.pathname+location.search);
 if(token)await api('/session',{token});await presentation.load();await refresh();
 if(requested){
  document.body.classList.add('terminal-window');
  const execution=(snapshot.executions||[]).find(e=>e.id===requested);if(!execution)throw new Error('This execution is not available in the current workspace.');
  const nonce=fragment.get('handoff');
  terminals.onAttached=entry=>{if(nonce&&entry.id===requested)window.opener?.postMessage({type:'terminal-attached',nonce,executionID:entry.id},location.origin)};
  await attach(execution);
 }else{terminals.restore(snapshot.executions||[],snapshot.agents||[]);await selectTab('groups')}

})().catch(e=>{$('connection').textContent='Not connected';showError(e)}).finally(()=>{for(const tab of document.querySelectorAll('[data-tab]'))tab.disabled=false;document.querySelector('main').inert=false});

async function attach(execution){
 await selectTab('terminals');
 const agent=(snapshot.agents||[]).find(a=>a.PrimaryExecutionID===execution.id);
 terminals.open(execution,agent?.Name||execution.id);
}
function closeTerminal(){terminals.closeAll()}
window.addEventListener('pagehide',()=>terminals.suspend());

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
 for(const evidence of result.node_evidence||[]){const proof=el('div',undefined,'card');proof.append(el('strong',`${evidence.attempt.NodeID} · ${evidence.kind}`),el('p',`Evidence ${evidence.id} · revision ${evidence.revision}`,'muted'),el('p',`${evidence.reporter.agent_id||evidence.reporter.kind} · attempt ${evidence.attempt.Attempt}`),el('pre',evidence.detail));if(evidence.artifact_revision)proof.append(el('p',`Artifact ${evidence.artifact_revision}`));if(evidence.passed!==undefined)proof.append(el('p',evidence.passed?'Verification passed':'Verification failed'));card.append(proof)}
 for(const decision of result.decisions||[])card.append(el('p',`${decision.Question||decision.ID}: ${decision.State}`));
 if(result.decision)card.append(el('strong',`${result.decision.decision}: ${result.decision.reason}`));
 const actions=el('div',undefined,'actions');
 if(run.graph)actions.append(button('Inspect process graph',async()=>{const {openProcessMonitor}=await import('/process-monitor.js');await openProcessMonitor(run.id,{api,el,button,openDecisions:()=>selectTab('decisions')})}));
 const pending=(run.attempts||[]).find(a=>a.step==='await_evidence'&&a.state==='pending');
 if(run.state==='waiting'&&pending)actions.append(button('Record evidence',()=>edit('Record attributed evidence',[{name:'detail',label:'Evidence',multiline:true},{name:'artifact',label:'Artifact revision',required:false}],f=>api('/v2/work/evidence',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,step:pending.step,attempt:pending.attempt,kind:'artifact',detail:f.detail,artifact_revision:f.artifact}))));
 const evaluate=(run.attempts||[]).find(a=>a.step==='evaluate'&&a.state==='pending');
 if(evaluate&&run.state==='waiting')actions.append(button('Decide outcome',()=>edit('Decide outcome',[{name:'decision',label:'Decision',options:['accept','reject']},{name:'reason',label:'Reason',multiline:true}],f=>api('/v2/work/decision',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,step:evaluate.step,attempt:evaluate.attempt,decision:f.decision,reason:f.reason}))));
 if(!['succeeded','failed','cancelled'].includes(run.state))actions.append(button('Cancel work',()=>edit('Cancel work',[{name:'reason',label:'Reason',multiline:true}],f=>api('/v2/work/cancel',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,reason:f.reason}))));
 if(run.state==='uncertain')actions.append(button('Resolve uncertainty',()=>edit('Confirm effect did not occur',[{name:'confirm',label:'Type the work ID to confirm no effect occurred'},{name:'reason',label:'Evidence supporting this conclusion',multiline:true}],f=>{if(f.confirm!==run.id)throw new Error('Work ID does not match');return api('/v2/work/resolve',{request_id:f.requestID,work_run_id:run.id,expected_revision:run.revision,reason:f.reason})})));
 card.append(actions);return card;
}



async function renderConfigurations(){
 const [entries,defaults]=await Promise.all([api('/v2/configuration-profiles'),api('/v2/configuration-defaults')]),list=$('configuration-list');list.replaceChildren();
 const choices=[...(defaults.Global?[['global',defaults.Global]]:[]),...Object.entries(defaults.Harnesses||{})];
 if(choices.length){const card=el('article',undefined,'card');card.append(el('strong','Defaults'),el('p','Defaults select a saved revision for new agents. Existing agents keep their settings.','muted'));
 for(const [name,ref] of choices){const row=el('div',undefined,'row');const profile=entries.find(p=>p.ID===ref.ProfileID);row.append(el('span',`${name}: ${profile?.Name||ref.ProfileID}`));
 row.append(button('Create from '+name,()=>edit('Create agent from default',[{name:'name',label:'Agent name'}],f=>api('/v2/agents',{id:f.requestID,name:f.name,configuration_profile:ref}))));
 row.append(button('Clear '+name,async id=>{const harnesses={...(defaults.Harnesses||{})};delete harnesses[name];await api('/v2/configuration-defaults',{request_id:id,expected_revision:defaults.Revision,global:name==='global'?null:defaults.Global,harnesses});await renderConfigurations()}));card.append(row)}list.append(card)}
 for(const profile of entries){
  const card=el('article',undefined,'card');card.append(el('strong',profile.Name),el('p',`Revision ${profile.Revision}`,'muted'));
  card.append(button('Create agent',async()=>{
   const selected=await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);
   edit('Create agent from configuration',[{name:'name',label:'Agent name',value:profile.Name}],f=>api('/v2/agents',{id:f.requestID,name:f.name,configuration_profile:selected.Revision.Ref}));
  }),button('Edit configuration',async()=>{
   const selected=await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);
   edit('Save new configuration revision',desiredFields({...selected.Revision.Desired,name:profile.Name}),async f=>{
    await api('/v2/configuration-profiles',{request_id:f.requestID,id:profile.ID,revision_id:f.requestID,expected_revision:profile.Revision,name:f.name,desired:configuration(f)});await renderConfigurations();
   });
  }),button('Use as default',async()=>{
   const selected=await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);
   edit('Set default configuration',[{name:'scope',label:'Default scope',options:[{value:'global',label:'Global'},{value:selected.Revision.Desired.Harness,label:selected.Revision.Desired.Harness}]}],async f=>{
    const harnesses={...(defaults.Harnesses||{})};if(f.scope!=='global')harnesses[f.scope]=selected.Revision.Ref;
    await api('/v2/configuration-defaults',{request_id:f.requestID,expected_revision:defaults.Revision,global:f.scope==='global'?selected.Revision.Ref:defaults.Global,harnesses});await renderConfigurations();
   });
  }));list.append(card);
 }
 if(!entries.length)empty(list,'No saved configurations. Save one to reuse its exact settings for new agents.');
}
$('new-configuration').onclick=()=>edit('Save configuration',desiredFields(),async f=>{
 await api('/v2/configuration-profiles',{request_id:f.requestID,id:f.requestID,revision_id:f.requestID,name:f.name,desired:configuration(f)});await renderConfigurations();
});

function agentMetadataFields(agent={}){return[
 {name:'task',label:'Task reference',value:agent.TaskReference||'',required:false},
 {name:'notify',label:'Message notification',value:agent.Notifications?.DirectMessage||'if_available',options:[{value:'if_available',label:'Notify when available'},{value:'none',label:'Inbox only'}]}
]}
function audienceSelection(values){return{AgentIDs:values.filter(v=>v!=='operator'),Operator:values.includes('operator')}}
function composeMessage(parent){
 const available=(snapshot.agents||[]).filter(a=>a.Lifecycle!=='retired'),options=[{value:'operator',label:'Operator'},...available.map(a=>({value:a.ID,label:a.Name}))];
 const cache=new Map();
 edit(parent?'Reply to thread':'Compose message',[
  {name:'to',label:'To',options,multiple:true,required:false},
  {name:'cc',label:'CC',options,multiple:true,required:false},
  {name:'subject',label:'Subject',value:parent?(parent.Subject.startsWith('Re: ')?parent.Subject:'Re: '+parent.Subject):''},
  {name:'body',label:'Message',multiline:true},
  {name:'files',label:'Attachments (up to four; 5 MiB each)',file:true,multiple:true,required:false}
 ],async f=>{
  const files=f.files.filter(file=>file instanceof File&&file.name);if(files.length>4||files.some(file=>file.size>5*1024*1024)||files.reduce((sum,file)=>sum+file.size,0)>10*1024*1024)throw new Error('Attachments exceed the displayed size limits.');
  const attachments=[];
  for(const file of files){
   const data=await file.arrayBuffer(),hash=Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256',data)),v=>v.toString(16).padStart(2,'0')).join(''),key=JSON.stringify([file.name,file.type,hash]);
   let claim=cache.get(key);
   if(!claim){const content=await new Promise((resolve,reject)=>{const reader=new FileReader();reader.onload=()=>resolve(String(reader.result).split(',')[1]);reader.onerror=reject;reader.readAsDataURL(file)});claim=await api('/v2/attachment-claims',{filename:file.name,media_type:file.type||'application/octet-stream',content});cache.set(key,claim)}
   attachments.push({Claim:{ClaimID:claim.ID,AttachmentID:claim.Attachment.ID,Filename:claim.Attachment.Filename,MediaType:claim.Attachment.MediaType,Size:claim.Attachment.Size,SHA256:claim.Attachment.SHA256}});
  }
  await api('/v2/messages',{request_id:f.requestID,subject:f.subject,parent_message_id:parent?.ID||'',to:audienceSelection(f.to),cc:audienceSelection(f.cc),body:f.body,attachments});
 });
 if(parent){const recipients=new Set((parent.Recipients||[]).filter(r=>r.AddressKind==='agent').map(r=>r.AgentID));if(parent.Sender.AgentID)recipients.add(parent.Sender.AgentID);for(const option of $('editor-fields').querySelector('[name=to]').options)option.selected=recipients.has(option.value)}
}
async function downloadAttachment(attachment){
 const result=await api(`/v2/attachments/${encodeURIComponent(attachment.ID)}`),bytes=Uint8Array.from(atob(result.Content),c=>c.charCodeAt(0));
 const url=URL.createObjectURL(new Blob([bytes],{type:'application/octet-stream'})),link=el('a');link.href=url;link.download=attachment.Filename;document.body.append(link);link.click();link.remove();setTimeout(()=>URL.revokeObjectURL(url),1000);
}

let usageTarget=null,activityTarget=null;
function targetFields(kind){return[{name:'kind',label:'Target type',options:kind==='usage'?['ConversationID','ExecutionID']:['AgentID','ConversationID','ExecutionID','WorkRunID']},{name:'id',label:'Target ID'}]}
$('select-usage').onclick=()=>edit('Select usage target',targetFields('usage'),async f=>{usageTarget={[f.kind]:f.id};await renderUsage()});
$('select-activity').onclick=()=>edit('Select activity target',targetFields('activity'),async f=>{activityTarget={[f.kind]:f.id};await renderActivity()});
async function renderUsage(cursor='',append=false){
 const list=$('usage-list');if(!append)list.replaceChildren();if(!usageTarget){empty(list,'Select a conversation or execution to inspect its usage.');return}
 const result=await api('/v2/usage/query',{filter:{Target:usageTarget,Limit:25,Cursor:cursor}});
 if(!append)list.append(button('Refresh native usage',async()=>{await api('/v2/usage/refresh',{target:usageTarget});await renderUsage()}));
 for(const observation of result.Observations||[]){const card=el('article',undefined,'card');
 card.append(el('strong',`${observation.Harness} · ${observation.Attribution.Precision}`),el('p',`${observation.Source} · ${new Date(observation.ObservedAt).toLocaleString()}`,'muted'));
 for(const counter of observation.Counters||[])card.append(el('p',`${counter.Unit.replaceAll('_',' ')}: ${counter.Value}`));
 if(observation.Cost)card.append(el('p',`${observation.Cost.Amount} ${observation.Cost.Currency} · ${observation.Cost.Kind.replaceAll('_',' ')}`));
 card.append(el('p',`Counters: ${observation.Coverage.Counters}; cost: ${observation.Coverage.Cost}`,'muted'));if(observation.Coverage.Reason)card.append(el('p',observation.Coverage.Reason));list.append(card)}
 if(!result.Observations?.length)empty(list,'No recorded observations. Missing usage is not zero usage.');
 if(result.NextCursor)list.append(button('More observations',async()=>renderUsage(result.NextCursor,true)));
}
async function renderActivity(cursor='',append=false){
 const list=$('activity-list');if(!append)list.replaceChildren();if(!activityTarget){empty(list,'Select an agent, conversation, execution or Work Run.');return}
 const result=await api('/v2/activity/query',{filter:{Target:activityTarget,Limit:25,Cursor:cursor}});
 for(const record of result.Records||[]){const card=el('article',undefined,'card');card.append(el('strong',`${record.Kind.replaceAll('_',' ')} · ${record.Outcome}`),el('p',`${record.Actor.AgentID||record.Actor.Kind} · ${new Date(record.StartedAt).toLocaleString()}`,'muted'));if(record.Reason)card.append(el('p',record.Reason));if(record.Historical)card.append(el('p','Imported historical record','muted'));list.append(card)}
 if(!result.Records?.length)empty(list,'No recorded activity for this target.');
 if(result.NextCursor)list.append(button('More activity',async()=>renderActivity(result.NextCursor,true)));
}

async function renderDecisions(){
 const [results,access]=await Promise.all([api('/v2/decisions'),api('/v2/access-requests')]),list=$('decision-list');list.replaceChildren();
 for(const result of results||[]){
  const window=result.Window,card=el('article',undefined,'card');
  card.append(el('h2',window.Question||'Decision'),el('p',`${window.Attempt.RunID} · ${window.Attempt.NodeID}`),el('p',`Expires ${new Date(window.ExpiresAt).toLocaleString()}`,'muted'));
  card.append(button('Answer',async()=>{
   const blocked=window.Kind==='blocked',work=blocked?await api(`/v2/work/${encodeURIComponent(window.Attempt.RunID)}`):null;
   edit(blocked?'Resolve blocked work':'Answer decision',[{name:'answer',label:'Answer',options:window.PermittedAnswers||[]},{name:'reason',label:'Reason',multiline:true}],async f=>{
    const body={request_id:f.requestID,decision_id:window.ID,expected_window_revision:window.Revision,reason:f.reason};
    if(blocked){Object.assign(body,{attempt:window.Attempt,expected_run_revision:work.run.revision,action:f.answer});}
    else body.answer=f.answer;
    await api(blocked?'/v2/processes/resolve-blocked':'/v2/decisions/submit',body);await renderDecisions();
   });
  }));
  list.append(card);
 }
 for(const item of access.requests||[]){
  const request=item.request,decision=item.decision,card=el('article',undefined,'card');
  card.dataset.accessRequest=request.id;
  card.append(el('h2','Access request'),el('strong',request.action),el('p',`${request.requester.agent_id||request.requester.execution_id} · ${request.state}`),el('p',request.reason));
  card.append(el('p',`Expires ${new Date(request.expires_at).toLocaleString()}`,'muted'),el('pre',JSON.stringify({resource:request.resource,bounds:request.bounds,requested_configuration:request.requested_configuration},null,2)));
  if(decision.state==='open')card.append(button('Decide access',()=>edit('Decide exact access request',[
   {name:'answer',label:'Decision',options:[{value:'deny',label:'Deny'},{value:'approve',label:'Approve requested access'}]},
   {name:'reason',label:'Reason',multiline:true}
  ],async f=>{await api(`/v2/access-requests/${encodeURIComponent(request.id)}/decision`,{request_id:f.requestID,expected_window_revision:decision.revision,answer:f.answer,reason:f.reason});await renderDecisions()})));
  if(decision.submission)card.append(el('p',`${decision.submission.answer} · ${decision.submission.actor.kind}: ${decision.submission.reason}`));
  list.append(card);
 }
 if(!list.childNodes.length)empty(list,'No decisions or access requests.');
}
async function launchProcessEditor(result){
 const {openProcessEditor}=await import('/process-editor.js');
 await openProcessEditor({api,result,agents:(snapshot.agents||[]).filter(a=>a.Lifecycle!=='retired'),onSaved:renderDefinitions});
}
async function launchTeamEditor(result){
 const {openTeamEditor}=await import('/team-editor.js');
 await openTeamEditor({api,result,onSaved:renderDefinitions});
}
async function renderDefinitions(){
 const definitions=await api('/v2/definitions'),list=$('definition-list');list.replaceChildren();
 list.append(button('New process',()=>launchProcessEditor()),button('New team template',()=>launchTeamEditor()));
 for(const definition of definitions||[]){
  const card=el('article',undefined,'card');card.append(el('h2',definition.Name),el('p',`${definition.Kind} · revision ${definition.Revision}`,'muted'));
  card.append(button('Inspect definition',async()=>{const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID));card.append(el('pre',result.Revision.Source))}));
  if(definition.Kind==='process')card.append(button('Edit process',async()=>launchProcessEditor(await api('/v2/definitions/'+encodeURIComponent(definition.ID)))));
 if(definition.Kind==='process')card.append(button('Start process',async()=>{
   const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID));
   const revision=result.Revision;
   const memberKeys=[...new Set((revision.Process.Graph.Nodes||[]).map(n=>n.Performer?.Agent?.MemberKey).filter(Boolean))];
   const agents=(snapshot.agents||[]).filter(a=>a.Lifecycle!=='retired');
   if(memberKeys.length&&!agents.length)throw new Error('Create an active agent before binding this process.');
   const bindingFields=memberKeys.map(key=>({name:'binding_'+key,label:'Agent for '+key,options:agents.map(a=>({value:a.ID,label:a.Name}))}));
   const fields=[{name:'workspace',label:'Workspace',options:(snapshot.workspaces||[]).filter(w=>w.State==='available').map(w=>({value:w.ID,label:w.Intent.Name||w.Observation.ActualPath||w.ID}))},{name:'minutes',label:'Maximum run time in minutes',value:'60'},...parameterFields(revision.Parameters||[]),...bindingFields];
   edit('Start pinned process',fields,async f=>{
    const minutes=Number(f.minutes);if(!Number.isFinite(minutes)||minutes<=0||minutes>10080)throw new Error('Choose a run duration between 1 and 10080 minutes.');
    const programs=(revision.Process.Graph.Nodes||[]).filter(n=>n.Performer?.Program).map(n=>n.Performer.Program.Profile);
    await api('/v2/processes',{request_id:f.requestID,id:f.requestID,start:{Definition:{DefinitionID:definition.ID,RevisionID:revision.ID,ContentHash:revision.ContentHash,Kind:'process'},Scope:{WorkspaceID:f.workspace},Parameters:parameterValues(revision.Parameters||[],f),PerformerBindings:Object.fromEntries(memberKeys.map(key=>[key,{Kind:'agent',Agent:{AgentID:f['binding_'+key]}}])),AuthorizedProgramProfiles:programs,Deadline:new Date(Date.now()+minutes*60000).toISOString()}});
   });
  }));
  if(definition.Kind==='team')card.append(button('Edit team template',async()=>launchTeamEditor(await api('/v2/definitions/'+encodeURIComponent(definition.ID)))));
  if(definition.Kind==='team')card.append(button('Deploy team',async()=>{
   const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID)),revision=result.Revision;
   const spaces=(snapshot.workspaces||[]).filter(w=>w.State==='available');
   if(!spaces.length)throw new Error('Create a checkout in Workspaces before deploying this team.');
   const shared=revision.Team.WorkspacePolicy==='shared';
   const workspaceFields=(shared?[{Key:'shared',Name:'Shared team'}]:revision.Team.Members).map(member=>({name:'workspace_'+member.Key,label:member.Name+' workspace',options:spaces.map(w=>({value:w.ID,label:w.Intent.Name||w.Observation.ActualPath||w.ID}))}));
   edit('Deploy pinned team',[{name:'mission',label:'Mission',multiline:true},{name:'group',label:'Group',options:[{value:'',label:'Create a new group'},...(snapshot.groups||[]).map(g=>({value:g.ID,label:g.Name}))]},...workspaceFields,...parameterFields(revision.Parameters||[])],f=>{
    const selection=key=>{const w=spaces.find(w=>w.ID===f['workspace_'+key]);if(!w)throw new Error('Select a current workspace.');return{WorkspaceID:w.ID,ExpectedRevision:w.Revision}};
    const workspaces=shared?{Shared:selection('shared')}:{Members:Object.fromEntries(revision.Team.Members.map(m=>[m.Key,selection(m.Key)]))};
    return api('/v2/teams/deploy',{request_id:f.requestID,deployment_id:f.requestID,instantiation:{Definition:{DefinitionID:definition.ID,RevisionID:revision.ID,ContentHash:revision.ContentHash,Kind:'team'},Mission:f.mission,Target:{Kind:f.group?'existing_group':'new_group',GroupID:f.group||'group_'+f.requestID},Workspaces:workspaces,Parameters:parameterValues(revision.Parameters||[],f)}});
   });
  }));
  list.append(card);
 }
 if(!definitions?.length)empty(list,'No saved definitions. Create a process to begin authoring.');
 const deployments=await api('/v2/teams/deployments');
 if(deployments?.length)list.append(el('h2','Deployed teams'));
 for(const result of deployments||[]){
  const d=result.Deployment,card=el('article',undefined,'card');card.dataset.deployment=d.ID;
  card.append(el('h3',d.Mission||d.ID),el('p',`${d.State} · group ${d.GroupID} · phase ${d.AdvisoryPhase} · revision ${d.Revision}`));
  card.append(el('p',Object.entries(d.Members||{}).map(([key,id])=>`${key}: ${id}`).join(' · ')));
  card.append(el('p',`${Object.keys(d.Workspaces||{}).length} workspace bindings · ${(d.OwnedAutomationRuleIDs||[]).length} owned rhythms · ${(d.Rebriefs||[]).length} rebriefs`,'muted'));
  if(d.State!=='stopped'){
   card.append(button('Stand down',()=>edit('Stand down team',[{name:'reason',label:'Reason',multiline:true}],async f=>{
    await api('/v2/teams/stand-down',{request_id:f.requestID,deployment_id:d.ID,expected_revision:d.Revision,reason:f.reason});await renderDefinitions();
   })));
  }
  if(d.State==='ready'){
   card.append(button('Advance advisory phase',()=>edit('Advance advisory phase',[],async f=>{
    await api('/v2/teams/advance-phase',{request_id:f.requestID,deployment_id:d.ID,expected_revision:d.Revision});await renderDefinitions();
   })));
   card.append(button('Rebrief',async()=>{
    const selected=await api('/v2/definitions/'+encodeURIComponent(d.Definition.DefinitionID)),r=selected.Revision;
    edit('Rebrief revision '+r.ID,[],async f=>{
     await api('/v2/teams/rebrief',{request_id:f.requestID,deployment_id:d.ID,expected_revision:d.Revision,definition:{DefinitionID:r.DefinitionID,RevisionID:r.ID,ContentHash:r.ContentHash,Kind:'team'}});await renderDefinitions();
    });
   }));
  }
  list.append(card);
 }

}

function parameterFields(parameters){return parameters.map((p,index)=>{
 const value=p.Default===undefined||p.Default===null?'':p.Type==='string'?p.Default:JSON.stringify(p.Default);
 const field={name:'parameter_'+index,label:p.Description||p.Name,value,required:p.Required,multiline:p.Type==='object'||p.Type==='array'};
 if(p.Type==='boolean')field.options=p.Required?['true','false']:['','true','false'];return field;
})}
function parameterValues(parameters,form){const values={};parameters.forEach((p,index)=>{
 const text=form['parameter_'+index];if(text===''&&!p.Required)return;
 let value=text;
 if(p.Type!=='string'){try{value=JSON.parse(text)}catch{throw new Error(`Enter a valid ${p.Type} for ${p.Name}.`)}}
 const valid=p.Type==='string'?typeof value==='string':p.Type==='number'?typeof value==='number'&&Number.isFinite(value):p.Type==='boolean'?typeof value==='boolean':p.Type==='array'?Array.isArray(value):value!==null&&typeof value==='object'&&!Array.isArray(value);
 if(!valid)throw new Error(`Enter a valid ${p.Type} for ${p.Name}.`);values[p.Name]=value;
});return values}

let automationUI;
async function renderAutomation(){
 if(!automationUI){const {automationWorkspace}=await import('/automation.js');automationUI=automationWorkspace({api,el,button,edit,getSnapshot:()=>snapshot,openWork:async id=>{const result=await api('/v2/work/'+encodeURIComponent(id));await selectTab('work');$('work-list').replaceChildren(workCard(result));}})}
 await automationUI.render($('automation-list'));
}
