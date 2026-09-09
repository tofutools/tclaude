'use strict';
const {ExactJSON,stringifyExact,parameterDefaultText}=globalThis.ExactJSONTools;
const $ = id => document.getElementById(id);
const el = (tag, text, cls) => { const n=document.createElement(tag); if(text!==undefined)n.textContent=text; if(cls)n.className=cls; return n; };
let snapshot = {}, submitting = false, refreshSequence=0;
for(const tab of document.querySelectorAll('[data-tab]'))tab.disabled=true;
document.querySelector('main').inert=true;
const requestID = () => 'r_' + crypto.randomUUID();
const terminals = new TerminalWorkspace({requestID});
const navigation = new WorkspaceNavigation({select:tab=>selectTab(tab,false),report:showError});
const presentation = new PresentationWorkspace({api});
document.addEventListener('presentation-save-state',()=>{const status=$('group-order-status');if(status)status.textContent=presentation.saved.textContent});
document.addEventListener('group-order-changed',()=>{renderGroupControls(snapshot,{host:$('group-management'),el,button,edit,api,refresh,presentation,attach,displayLabelFields});rosterWorkspace.update(snapshot,agentRow,presentation.prefs.GroupOrder||[])});
const terminalTools = new TerminalTools({host:$('terminal-tools'),workspace:terminals,el,button});
const terminalDownloads = new TerminalDownloads({host:$('terminal-downloads'),workspace:terminals,el,button});
const terminalFiles = new TerminalFiles({host:$('terminal-files'),workspace:terminals,tools:terminalTools,api,el,button,requestID});
const authorityWorkspace = new AuthorityWorkspace({host:$('access-list'),api,el,button,edit,getSnapshot:()=>snapshot,report:showError});
const sandboxProfiles = new SandboxProfilesWorkspace({host:$('sandbox-profiles'),api,el,button,edit,requestID});
const programProfiles = new ProgramProfilesWorkspace({host:$('program-profiles'),api,el,button,edit,requestID});
const attachmentPreview = new AttachmentPreview({api,el,button});
const messageWorkspace = new MessageWorkspace({host:$('message-list'),el,button,api,refresh,card:messageCard});
const attention = new AttentionWorkspace({host:$('attention'),api,el,button,refresh,select:tab=>selectTab(tab)});

const usageSummary = new UsageSummaryWorkspace({host:$('usage-summary'),api,el,button,getSnapshot:()=>snapshot});
const usageWorkspace = new UsageWorkspace({host:$('usage-list'),api,el,button,getSnapshot:()=>snapshot,setTarget:target=>{usageTarget=target}});

const historyWorkspace = new HistoryWorkspace({host:$('histories'),api,el,button,edit,startWork,selection});
const activityWorkspace = new ActivityWorkspace({host:$('activity-list'),api,el,button,getSnapshot:()=>snapshot,setTarget:target=>{activityTarget=target}});
const workspaceBrowser = new WorkspaceBrowser({host:$('workspace-list'),api,el,button,refresh,row:workspaceCard});
const rosterWorkspace = new RosterWorkspace({host:$('roster'),api,el,button,edit,refresh});
function showError(error) { const target=$('editor').open?$('editor-error'):$('error');target.textContent=error.message || String(error);target.hidden=false; }
async function api(path, body, method) {
 const response=await fetch(path,{method:method || (body===undefined?'GET':'POST'),credentials:'same-origin',headers:body===undefined?{}:{'Content-Type':'application/json'},body:body===undefined?undefined:stringifyExact(body)});
 if(!response.ok){let code=await response.text(),failure;try{failure=JSON.parse(code);code=failure.code}catch{};const error=new Error((['group_busy','profile_disabled'].includes(code)&&typeof failure?.message==='string'?failure.message:null) || ({conflict:'The saved state changed. Refresh and review before trying again.',unsupported:'This operation is not supported by the configured provider or host.',forbidden:'Your current authority does not allow this operation.',uncertain:'The effect is uncertain. Inspect its state before attempting another operation.',invalid_request:'Some inputs are invalid. Check the values and required fields.'})[code] || code || `Request failed (${response.status})`);error.code=code;error.status=response.status;throw error}
 if(response.status===204)return;
 return response.json();
}
function button(text, action) { const b=el('button',presentation.label(text));b.dataset.uiText=text;const id=requestID();b.type='button';b.onclick=async()=>{b.disabled=true;$('error').hidden=true;try{await action(id)}catch(e){showError(e)}finally{b.disabled=false}};return b; }
function empty(parent,text){parent.append(el('p',text,'empty'))}
async function refresh({automatic=false}={}){
 const sequence=++refreshSequence,data=await api('/v2/snapshot');if(sequence!==refreshSequence)return;const changed=data.revision!==snapshot.revision;snapshot=data;if(!automatic||changed)render();attention.update(snapshot);$('connection').textContent=`Updated ${new Date().toLocaleTimeString()}`;
}
function edit(title,fields,save,{skipUnchanged=false}={}){
 $('editor-title').textContent=presentation.label(title);$('editor-fields').replaceChildren();$('editor-error').hidden=true;let fingerprint='',submissionID='';
 for(const field of fields){
  const label=el('label',field.label);let input;
  if(field.sandboxPolicies){field.control=new SandboxProfileAllowList(api,field.value);const container=el('fieldset');container.append(el('legend',field.label),field.control.host);$('editor-fields').append(container);continue}
  if(field.environment||field.environmentSets){field.control=field.environmentSets?new LaunchEnvironmentSets(field.value):new LaunchEnvironment(field.value,{inherited:field.inherited||{}});const container=el('fieldset');container.append(el('legend',field.label),field.control.host);$('editor-fields').append(container);continue}
  if(field.sandboxSelection){field.control=new SandboxSelectionControl(api,field.value);input=field.control.host;}
  else if(field.options){input=el('select');for(const option of field.options){const o=el('option',typeof option==='string'?option:option.label);o.value=typeof option==='string'?option:option.value;input.append(o)}}
  else input=el(field.multiline?'textarea':'input');
  if(field.file)input.type='file';
  if(field.multiple)input.multiple=true;input.name=field.name;input.setAttribute('aria-label',field.label);
  if(field.options&&field.value!==undefined){const values=field.multiple?(field.value||[]):[String(field.value)];for(const value of values){if(!Array.from(input.options).some(o=>o.value===String(value))){const o=el('option','Retained: '+value);o.value=value;input.append(o)}}if(field.multiple){for(const o of input.options)o.selected=values.includes(o.value)}else input.value=field.value}
  else if(!field.file&&!field.options&&!field.sandboxSelection)input.value=field.value??'';
  input.required=field.required!==false;label.append(input);if(field.sandboxSelection)label.append(field.control.status);$('editor-fields').append(label);
  if(field.help){const help=el('pre',field.help);help.id='editor-help-'+field.name;input.setAttribute('aria-describedby',help.id);label.append(help)}
  if(field.name==='cwd')label.append(button('Browse directories',async()=>{const {pickDirectory}=await import('./directory-picker.js');if(!input.isConnected||!$('editor').open)return;const selected=await pickDirectory({api,initial:input.value});if(selected!==null&&input.isConnected&&$('editor').open){input.value=selected;input.dispatchEvent(new Event('input',{bubbles:true}));}}));
 }
 attachToolGovernanceControl($('editor-fields'));
 const readForm=()=>{const data=new FormData($('editor-form')),form=Object.fromEntries(data);for(const field of fields){if(field.multiple)form[field.name]=data.getAll(field.name);if(field.environment||field.environmentSets||field.sandboxPolicies)form[field.name]=field.control.read();}return form};const initial=JSON.stringify(readForm());
 $('editor-form').onsubmit=async e=>{e.preventDefault();if(submitting)return;submitting=true;$('cancel').disabled=true;const submit=e.submitter;if(submit)submit.disabled=true;
  try{const form=readForm();if(skipUnchanged&&JSON.stringify(form)===initial){$('editor').close();return}for(const field of fields){if(field.sandboxSelection)form[field.name]=await field.control.read(form[field.name]);}const next=JSON.stringify(form,(_,value)=>value instanceof File?{name:value.name,size:value.size,modified:value.lastModified}:value);if(fingerprint!==next){fingerprint=next;submissionID=requestID()}form.requestID=submissionID;await save(form);await refresh();$('editor').close()}catch(error){showError(error)}finally{submitting=false;$('cancel').disabled=false;if(submit)submit.disabled=false}
 };
 $('editor').showModal();
 attachLaunchSupportPreview({host:$('editor-fields'),api});
}
function desiredFields(desired={}){return[
 {name:'name',label:'Name',value:desired.name},
 {name:'harness',label:'Harness',value:desired.Harness||'claude',options:['claude','codex','opencode','copilot']},
 {name:'model',label:'Model',value:desired.Model},
 {name:'host_sandbox',label:'Host sandbox profile',sandboxSelection:true,value:desired.HostSandbox||null},
 {name:'environment',label:'Environment — literal values for future launches',environment:true,value:desired.Environment||{}},
 {name:'tool_governance',label:'OpenCode tool governance (bash, glob, grep, lsp, task, skill)',value:desired.ToolGovernance||'',options:launchToolGovernanceChoices(),required:false},
 {name:'effort',label:'Requested native effort / variant (optional)',value:desired.Effort||'',required:false},
 {name:'cwd',label:'Working directory',value:desired.WorkingDirectory},
 {name:'approval',label:'Approval',value:desired.Approval||'supervised',options:launchApprovalChoices()},
 {name:'sandbox',label:'Confinement',value:desired.Sandbox||'workspace_write',options:['read_only','workspace_write','unconfined']}
]}
function configuration(form){if(form.effort&&!/^[a-z0-9][a-z0-9_-]{0,63}$/.test(form.effort))throw new Error('Requested native effort must start with a letter or digit and contain at most 64 lowercase letters, digits, underscores or hyphens.');return{...(form.host_sandbox?{HostSandbox:form.host_sandbox}:{}),Environment:form.environment||{},Harness:form.harness,Model:form.model,Effort:form.effort,ToolGovernance:form.tool_governance||undefined,WorkingDirectory:form.cwd,Approval:form.approval,Sandbox:form.sandbox}}
async function startWithBrief(agent){
 const ref=agent.ConfigurationProfile;
 const saved=ref?await api(`/v2/configuration-profiles/${encodeURIComponent(ref.ProfileID)}?revision_id=${encodeURIComponent(ref.RevisionID)}`):null;
 const startup=saved?.Revision.Startup||{};
 edit('Start with initial brief',[
  {name:'context',label:'Startup context (review before launch)',multiline:true,value:startup.Context||'',required:false},
  {name:'brief',label:'Initial brief (delivered before first work, at most 32 KiB)',multiline:true,value:startup.InitialMessage||'',required:false}
 ],f=>{const body=f.context&&f.brief?f.context+'\n\n'+f.brief:f.context||f.brief;if(!body||new TextEncoder().encode(body).length>32768||body.includes('\0'))throw new Error('Context and brief together must contain 1–32768 UTF-8 bytes without NUL.');return api('/v2/launch',{request_id:f.requestID,initial_message:body,target:{agent:{agent_id:agent.ID,expected_revision:agent.Revision}}})});
}
function startupFields(startup={}){return[
 {name:'startup_role',label:'Default display role (optional)',value:startup.Role||'',required:false},
 {name:'startup_description',label:'Default agent description (optional)',value:startup.Description||'',multiline:true,required:false},
 {name:'startup_name',label:'Suggested agent name (optional)',value:startup.AgentName||'',required:false},
 {name:'startup_context',label:'Suggested startup context (optional)',value:startup.Context||'',multiline:true,required:false},
 {name:'startup_brief',label:'Suggested initial brief (optional; context and brief together at most 32 KiB)',value:startup.InitialMessage||'',multiline:true,required:false}
]}
function profileStartup(f){const name=f.startup_name||'',context=f.startup_context||'',brief=f.startup_brief||'',body=context&&brief?context+'\n\n'+brief:context||brief;if(new TextEncoder().encode(name).length>256||new TextEncoder().encode(body).length>32768||/[\0\r\n]/.test(name)||(name&&!name.trim())||body.includes('\0'))throw new Error('Suggested name must be at most 256 UTF-8 bytes; context and brief together at most 32768 bytes, without NUL.');if((f.startup_role||'').includes('\0')||(f.startup_description||'').includes('\0'))throw new Error('Display labels cannot contain NUL.');return{Role:f.startup_role||'',Description:f.startup_description||'',AgentName:name,Context:context,InitialMessage:brief}}
function editedAgentLabels(agent,groupID,form){
 const value={Role:form.role_label,Description:form.description},labels={...agent.Labels};
 if(groupID&&groupID!=='__ungrouped')return {...labels,Groups:{...labels.Groups,[groupID]:value}};
 return {...labels,...value};
}
function agentRow(agent,groupID){
 const display=agent.Labels?.Groups?.[groupID]||agent.Labels||{};
 const row=el('div',undefined,'row');row.append(el('span',agent.Name,'name'));
 if(display.Role)row.append(el('span',display.Role,'muted'));
 if(display.Description)row.append(el('span',display.Description,'muted'));
 const execution=(snapshot.executions||[]).find(e=>e.id===agent.PrimaryExecutionID);
 row.append(el('span',execution?`${execution.state} · context ${execution.context_readiness}`:'offline','status'));
 row.append(el('span',`${agent.Desired.Harness} / ${agent.Desired.Model}`,'muted'));
 if(agent.ConfigurationProfile)row.append(el('span','Saved configuration revision','muted'));
 const actions=el('div',undefined,'actions');
 actions.append(button('Activity',async()=>{activityTarget={AgentID:agent.ID};await selectTab('activity')}));
 if(execution?.conversation_id)actions.append(button('Usage',async()=>{usageTarget={ConversationID:execution.conversation_id};await selectTab('usage')}));
 if(agent.Lifecycle==='retired'){row.append(el('span','retired','status'));actions.append(button('Reactivate',async()=>{await api(`/v2/agents/${encodeURIComponent(agent.ID)}/reactivate`,{expected_revision:agent.Revision});await refresh()}));row.append(actions);return row}
 actions.append(button('Save settings as configuration',()=>saveConfigurationDraft({...agent.Desired,name:agent.Name+' configuration'},{AgentName:agent.Name,Role:display.Role,Description:display.Description})));
 actions.append(button('Configure',()=>edit('Configure agent',[...desiredFields({...agent.Desired,name:agent.Name}),...agentMetadataFields({...agent,Labels:display})],f=>api(`/v2/agents/${encodeURIComponent(agent.ID)}`,{name:f.name,desired:configuration(f),task_reference:f.task,labels:editedAgentLabels(agent,groupID,f),notifications:{DirectMessage:f.notify},expected_revision:agent.Revision},'PUT'))));
 if(!execution || ['exited','failed'].includes(execution.state)){
  actions.append(button('Retire',()=>edit('Retire agent',[{name:'reason',label:'Reason',multiline:true}],f=>api(`/v2/agents/${encodeURIComponent(agent.ID)}/retire`,{expected_revision:agent.Revision,reason:f.reason}))));
  actions.append(button('Start',async id=>{await api('/v2/launch',{request_id:id,target:{agent:{agent_id:agent.ID,expected_revision:agent.Revision}}});await refresh()}));
  actions.append(button('Start with brief',()=>startWithBrief(agent)));
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
 actions.append(button('Clone configuration',()=>edit('Create independent agent',[{name:'name',label:'Name',value:agent.Name+' copy'}],f=>api('/v2/agents',{id:f.requestID,name:f.name,clone_source_agent_id:agent.ID,labels:display,desired:agent.Desired}))));
 row.append(actions);return row;
}
function messageCard(message){
  const card=el('article',undefined,'card');card.append(el('strong',message.Subject||'Message'),el('p',`${message.Sender.AgentID||message.Sender.Kind} · ${new Date(message.CreatedAt).toLocaleString()}`,'muted'),el('pre',message.Body));
  card.append(el('p',(message.Recipients||[]).map(r=>`${r.Audience==='cc'?'CC':'To'} ${r.AddressKind==='operator'?'Operator':r.AgentID} · ${r.ReadAt?'read':'unread'}${r.NotificationOutcome?' · notice '+r.NotificationOutcome.replaceAll('_',' '):''}`).join(', '),'muted'));
  if(message.ParentMessageID)card.append(el('p','Reply in an existing thread','muted'));
  card.append(button('Reply all',()=>composeMessage(message)));
  if((message.Recipients||[]).some(r=>r.AddressKind==='operator'&&!r.ReadAt))card.append(button('Mark read',async id=>{await api(`/v2/messages/${encodeURIComponent(message.ID)}/read`,{request_id:id,operator:true});await refresh()}));
  for(const attachment of message.Attachments||[]){if(attachmentPreview.kind(attachment))card.append(button(`Preview ${attachment.Filename}`,()=>attachmentPreview.open(attachment)));card.append(button(`Download ${attachment.Filename}`,()=>downloadAttachment(attachment)));}
 return card;
}
function render(){
 presentation.update(snapshot);
 renderGroupControls(snapshot,{host:$('group-management'),el,button,edit,api,refresh,presentation,attach,displayLabelFields});
 rosterWorkspace.update(snapshot,agentRow,presentation.prefs.GroupOrder||[]);
 workspaceBrowser.update(snapshot);
 const work=$('work-list');work.replaceChildren();
 for(const result of snapshot.work_runs||[])work.append(workCard(result));
 if(!snapshot.work_runs?.length)empty(work,'No work runs.');
 messageWorkspace.update(snapshot);
}
async function selectTab(tab,record=true){
 if(!navigation.tabs().some(n=>n.dataset.tab===tab))tab="groups";
 if(record)navigation.record(tab,record==='replace');
 for(const n of document.querySelectorAll('main > section'))n.hidden=n.id!==tab;
 for(const n of document.querySelectorAll('[data-tab]'))n.setAttribute('aria-current',String(n.dataset.tab===tab));
 if(tab==='configurations'){await renderConfigurations();await sandboxProfiles.load();}
 if(tab==='history')await historyWorkspace.load();
 if(tab==='usage')await renderUsage();
 if(tab==='activity')await renderActivity();

 if(tab==='processes'){await renderDefinitions();await programProfiles.load();}
 if(tab==='automation')await renderAutomation();
 if(tab==='decisions')await renderDecisions();
 if(tab==='access')await authorityWorkspace.render();
}
$('refresh').onclick=()=>refresh().catch(showError);
$('cancel').onclick=()=>{if(!submitting)$('editor').close()};
$('editor').addEventListener('cancel',event=>{if(submitting)event.preventDefault()});
$('logout').onclick=async()=>{try{await api('/session',undefined,'DELETE');document.dispatchEvent(new Event('workspace-signout'));closeTerminal();presentation.stop();attention.clear();usageWorkspace.clear();activityWorkspace.clear();programProfiles.clear();sandboxProfiles.clear();historyWorkspace.clear();workspaceBrowser.clear();refreshSequence++;snapshot={};render();$('connection').textContent='Signed out';showError(new Error('Open a new dashboard login link to sign in.'))}catch(e){showError(e)}};
for(const tab of document.querySelectorAll('[data-tab]'))tab.onclick=()=>selectTab(tab.dataset.tab).catch(showError);
$('new-agent').onclick=()=>edit('New agent',[...desiredFields(),...agentMetadataFields()],f=>api('/v2/agents',{id:f.requestID,name:f.name,desired:configuration(f),task_reference:f.task,labels:{Role:f.role_label,Description:f.description},notifications:{DirectMessage:f.notify}}));
$('new-group').onclick=()=>edit('New group',[{name:'name',label:'Name'},{name:'members',label:'Members',multiple:true,required:false,options:(snapshot.agents||[]).map(a=>({value:a.ID,label:a.Name}))}],f=>api('/v2/groups',{id:f.requestID,name:f.name,members:f.members}));
$('compose').onclick=()=>composeMessage();
(async()=>{
 const fragment=new URLSearchParams(location.hash.slice(1));const token=fragment.get('login'),requested=new URLSearchParams(location.search).get('terminal');history.replaceState(null,'',location.pathname+location.search);
 if(token)await api('/session',{token});await presentation.load();await refresh();
 if(requested){
  document.body.classList.add('terminal-window');
  const execution=(snapshot.executions||[]).find(e=>e.id===requested);if(!execution)throw new Error('This execution is not available in the current workspace.');
  const nonce=fragment.get('handoff');
  terminals.onAttached=entry=>{if(nonce&&entry.id===requested)window.opener?.postMessage({type:'terminal-attached',nonce,executionID:entry.id},location.origin)};
  await attach(execution,{record:false});
 }else{terminals.restore(snapshot.executions||[],snapshot.agents||[]);await selectTab(navigation.initialTab(),'replace');attention.start()}

})().catch(e=>{$('connection').textContent='Not connected';showError(e)}).finally(()=>{for(const tab of document.querySelectorAll('[data-tab]'))tab.disabled=false;document.querySelector('main').inert=false});

async function attach(execution,{record=true}={}){
 await selectTab('terminals',record);
 const agent=(snapshot.agents||[]).find(a=>a.PrimaryExecutionID===execution.id);
 terminals.open(execution,agent?.Name||execution.id);
}
function closeTerminal(){terminals.closeAll();terminalTools.clear()}
window.addEventListener('pagehide',()=>{terminals.suspend();attention.stop()});
window.addEventListener('pageshow',event=>{if(event.persisted&&snapshot.revision!==undefined&&!document.body.classList.contains('terminal-window'))attention.start()});

function workspaceCard(space){
 const card=el('div',undefined,'card');card.append(el('strong',space.ID),el('p',space.Observation?.ActualPath||space.Intent?.IntendedPath||''),el('span',space.State,'status'));
 card.dataset.workspaceId=space.ID;
 const observed=space.Observation||{},claims=(snapshot.workspace_uses||[]).filter(u=>u.WorkspaceID===space.ID&&!u.ReleasedAt),hasObservation=Date.parse(observed.ObservedAt)>0;
 const details=el('dl');for(const[label,value]of [['Ownership',space.Intent.Ownership],['Repository',observed.RepositoryRoot||space.Intent.Repository||'Not recorded'],['Branch',observed.Branch||space.Intent.Branch||'Not recorded'],['Base',space.Intent.BaseRevision||'Not recorded'],['Commit',observed.Revision||'Not recorded'],['Git status',!observed.RepositoryRoot||!hasObservation?'Not observed':observed.Dirty?'Changes observed':'Clean when observed'],['Observed',hasObservation?new Date(observed.ObservedAt).toLocaleString():'Not yet observed'],['Retain on finish',space.Intent.RetainOnFinish?'Yes':'No']])details.append(el('dt',label),el('dd',value));card.append(details);
 for(const use of claims)card.append(el('p',`Active claim ${use.ID} · execution ${use.ExecutionID||'not issued'} · work ${use.WorkRunID||'none'}`));
 const actions=el('div',undefined,'actions');
 actions.append(button('Inspect',async()=>{await api(`/v2/workspaces/${encodeURIComponent(space.ID)}`);await refresh()}));
 if(space.State==='available'){
  actions.append(button('Open shell',async()=>{const sandbox=await sandboxSelectionInput(api);if(!actions.isConnected)return;edit('Open shell',[sandbox.field],async f=>api('/v2/shells',{request_id:f.requestID,workspace_id:space.ID,expected_revision:space.Revision,sandbox:'unconfined',host_sandbox:await sandbox.read(f.host_sandbox)}))}));
  if(space.Intent?.Ownership==='owned'){const remove=button('Remove checkout',()=>edit('Remove checkout',[{name:'confirm',label:'Type the workspace ID to confirm removal'},{name:'dirty',label:'Uncommitted changes',options:[{value:'false',label:'Refuse if dirty'},{value:'true',label:'Discard uncommitted changes'}]}],f=>{if(f.confirm!==space.ID)throw new Error('Workspace ID does not match');return api('/v2/workspaces/remove',{request_id:f.requestID,workspace_id:space.ID,expected_revision:space.Revision,destructive:f.dirty==='true'})}));remove.disabled=claims.length>0;actions.append(remove);if(claims.length)card.append(el('p','Cleanup is blocked by active claims. Stop or settle the owning work before removal.'));}
 }else if(space.State==='removed')actions.append(button('Restore checkout',async id=>{await api('/v2/workspaces/restore',{request_id:id,workspace_id:space.ID,expected_revision:space.Revision});await refresh()}));
 for(const execution of snapshot.executions||[]){if((snapshot.workspace_uses||[]).some(u=>u.WorkspaceID===space.ID&&u.ExecutionID===execution.id&&!u.ReleasedAt)&& !['exited','failed'].includes(execution.state))actions.append(button(execution.workload==='shell'?'Attach shell':'Attach terminal',()=>attach(execution)),button(execution.workload==='shell'?'Stop shell':'Stop execution',async id=>{await api('/v2/stop',{request_id:id,execution_id:execution.id,force:false});await refresh()}))}
 card.append(actions);return card;
}
const workspaceFields=[{name:'repository',label:'Repository path'},{name:'path',label:'Checkout path'},{name:'base',label:'Base commit or branch',value:'HEAD'},{name:'branch',label:'Worker branch'}];
$('create-checkout').onclick=()=>edit('Create owned checkout',workspaceFields,f=>api('/v2/workspaces/create',{request_id:f.requestID,id:f.requestID,intent:{Repository:f.repository,IntendedPath:f.path,BaseRevision:f.base,Branch:f.branch,Provenance:'platform_created',Ownership:'owned',RetainOnFinish:true}}));
$('register-workspace').onclick=()=>edit('Register existing directory',[{name:'path',label:'Directory path'}],f=>api('/v2/workspaces/register',{request_id:f.requestID,id:f.requestID,intent:{IntendedPath:f.path,Provenance:'registered',Ownership:'external',RetainOnFinish:true}}));
$('refresh-history').onclick=()=>edit('Refresh configured history source',[{name:'harness',label:'Harness',options:['claude','codex','opencode','copilot']},{name:'source',label:'Configured source name'}],async f=>{await api('/v2/history/refresh',{harness:f.harness,source:f.source});await historyWorkspace.load()});
function selection(entry,point){return{ConversationID:entry.ConversationID,ExpectedConversationRevision:entry.Revision,PointID:point?.ID||'',ExpectedPointRevision:point?.Revision||0}}
function startWork(read){
 const spaces=(snapshot.workspaces||[]).filter(s=>s.State==='available');const agents=(snapshot.agents||[]).filter(a=>{const e=(snapshot.executions||[]).find(e=>e.id===a.PrimaryExecutionID);return !e||['exited','failed'].includes(e.state)});
 if(!spaces.length||!agents.length)throw new Error('Create an available workspace and an offline worker first.');
 edit('Start bounded work',[
  {name:'workspace',label:'Workspace',options:spaces.map(s=>({value:s.ID,label:s.Observation.ActualPath||s.ID}))},
  {name:'worker',label:'Worker',options:agents.map(a=>({value:a.ID,label:a.Name}))},
  {name:'mode',label:'History use',options:[{value:'fresh_handoff',label:'Fresh conversation with handoff'},{value:'fork',label:'Exact fork (requires provider support)'}]},
  {name:'point',label:'History point',value:read.Point?.ID||'',options:[{value:'',label:'Persisted head'},...(read.Points||[]).map(p=>({value:p.ID,label:`${p.Kind} · ${p.ID}`}))]},
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



let configurationStatus='active';
async function renderConfigurations(){
 const [entries,defaults]=await Promise.all([api('/v2/configuration-profiles'),api('/v2/configuration-defaults')]),list=$('configuration-list');list.replaceChildren();
 const status=el('select');status.setAttribute('aria-label','Configuration status');for(const value of ['active','archived','all']){const option=el('option',value[0].toUpperCase()+value.slice(1));option.value=value;status.append(option)}status.value=configurationStatus;status.onchange=()=>{configurationStatus=status.value;renderConfigurations().catch(showError)};list.append(status);
 list.append(button('Export configurations',async()=>{const sequence=refreshSequence;const {openProfileTransfer}=await import('./profile-transfer.js');if(sequence!==refreshSequence)return;openProfileTransfer({mode:'export',profiles:entries,api,refresh:renderConfigurations})}),button('Import configurations',async()=>{const sequence=refreshSequence;const {openProfileTransfer}=await import('./profile-transfer.js');if(sequence!==refreshSequence)return;openProfileTransfer({mode:'import',profiles:entries,api,refresh:renderConfigurations})}));
 const choices=[...(defaults.Global?[['global',defaults.Global]]:[]),...Object.entries(defaults.Harnesses||{})];
 if(choices.length){const card=el('article',undefined,'card');card.append(el('strong','Defaults'),el('p','Defaults use the current saved configuration for new agents. Existing agents keep their settings.','muted'));
 for(const [name,ref] of choices){const row=el('div',undefined,'row');const profile=entries.find(p=>p.ID===ref.ProfileID);row.append(el('span',`${name}: ${profile?.Name||ref.ProfileID}`));
 row.append(button('Create from '+name,async()=>{const saved=await api(`/v2/configuration-profiles/${encodeURIComponent(ref.ProfileID)}`);edit('Create agent from default',[{name:'name',label:'Agent name',value:saved.Revision.Startup?.AgentName||''},...displayLabelFields(saved.Revision.Startup)],f=>api('/v2/agents',{id:f.requestID,name:f.name,labels:{Role:f.role_label,Description:f.description},configuration_default:name}))}));
 row.append(button('Clear '+name,async id=>{const harnesses={...(defaults.Harnesses||{})};delete harnesses[name];await api('/v2/configuration-defaults',{request_id:id,expected_revision:defaults.Revision,global:name==='global'?null:defaults.Global,harnesses});await renderConfigurations()}));card.append(row)}list.append(card)}
 const shown=entries.filter(p=>configurationStatus==='all'||Boolean(p.Archived)===(configurationStatus==='archived'));
 for(const profile of shown){
  const card=el('article',undefined,'card');card.append(el('strong',profile.Name),el('p',`Revision ${profile.Revision}`,'muted'));
  card.append(button('Inspect saved revision',async()=>{const selected=await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);let detail=card.querySelector('pre');if(!detail){detail=el('pre');card.append(detail)}detail.textContent=JSON.stringify(selected.Revision,null,2)}));
  if(profile.Archived){card.append(el('p','Archived · existing agents retain their pinned settings.','muted'),button('Restore configuration',async id=>{await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}/archive`,{request_id:id,expected_revision:profile.Revision,archived:false});await renderConfigurations()}));list.append(card);continue}
  card.append(el('p',profile.Disabled?'Disabled for new agents':'Enabled for new agents','muted'));
  if(profile.DisabledReason)card.append(el('p',profile.DisabledReason));
  card.append(button(profile.Disabled?'Enable configuration':'Disable configuration',()=>edit(profile.Disabled?'Enable configuration':'Disable configuration',[{name:'reason',label:'Disable reason',type:'textarea',required:false,value:profile.DisabledReason||''}],async f=>{
   await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}/availability`,{request_id:f.requestID,expected_revision:profile.Revision,disabled:!profile.Disabled,reason:f.reason});await renderConfigurations();
  })));
  const isDefault=choices.some(([,ref])=>ref.ProfileID===profile.ID),archive=button('Archive configuration',async id=>{await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}/archive`,{request_id:id,expected_revision:profile.Revision,archived:true});await renderConfigurations()});archive.disabled=isDefault;card.append(archive);if(isDefault)card.append(el('p','Clear or replace its default selections before archiving.','muted'));
  card.append(button('Create agent',async()=>{
   const selected=await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);
   edit('Create agent from configuration',[{name:'name',label:'Agent name',value:selected.Revision.Startup?.AgentName||profile.Name},...displayLabelFields(selected.Revision.Startup)],f=>api('/v2/agents',{id:f.requestID,name:f.name,labels:{Role:f.role_label,Description:f.description},configuration_profile:selected.Revision.Ref}));
  }),button('Edit configuration',async()=>{
   const selected=await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);
   edit('Save new configuration revision',[...desiredFields({...selected.Revision.Desired,name:profile.Name}),...startupFields(selected.Revision.Startup)],async f=>{
    await api('/v2/configuration-profiles',{request_id:f.requestID,id:profile.ID,revision_id:f.requestID,expected_revision:profile.Revision,name:f.name,desired:configuration(f),startup:profileStartup(f)});await renderConfigurations();
   });
  }),button('Use as default',async()=>{
   const selected=await api(`/v2/configuration-profiles/${encodeURIComponent(profile.ID)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);
   edit('Set default configuration',[{name:'scope',label:'Default scope',options:[{value:'global',label:'Global'},{value:selected.Revision.Desired.Harness,label:selected.Revision.Desired.Harness}]}],async f=>{
    const harnesses={...(defaults.Harnesses||{})};if(f.scope!=='global')harnesses[f.scope]=selected.Revision.Ref;
    await api('/v2/configuration-defaults',{request_id:f.requestID,expected_revision:defaults.Revision,global:f.scope==='global'?selected.Revision.Ref:defaults.Global,harnesses});await renderConfigurations();
   });
  }));list.append(card);
 }
 if(!shown.length)list.append(el('p','No '+configurationStatus+' configurations. Archived revisions remain available for inspection and restoration.'));
}
function saveConfigurationDraft(desired={},startup={}){
 edit('Save configuration',[...desiredFields(desired),...startupFields(startup)],async f=>{
  await api('/v2/configuration-profiles',{request_id:f.requestID,id:f.requestID,revision_id:f.requestID,name:f.name,desired:configuration(f),startup:profileStartup(f)});await renderConfigurations();
 });
}
$('new-configuration').onclick=()=>saveConfigurationDraft();

function displayLabelFields(labels={}){return[
 {name:'role_label',label:'Display role',value:labels?.Role||'',required:false},
 {name:'description',label:'Description',value:labels?.Description||'',multiline:true,required:false}
]}
function agentMetadataFields(agent={}){return[
 ...displayLabelFields(agent.Labels),
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
async function renderUsage(){await usageWorkspace.show(usageTarget)}
async function renderActivity(){await activityWorkspace.show(activityTarget)}

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
let definitionStatus='active',definitionLoadSequence=0;
document.addEventListener('workspace-signout',()=>{definitionLoadSequence++;definitionStatus='active';$('definition-list').replaceChildren()});
async function renderDefinitions(){
 const sequence=++definitionLoadSequence;
 const definitions=await api('/v2/definitions?include_tombstoned=true'),list=$('definition-list');if(sequence!==definitionLoadSequence)return;list.replaceChildren();
 const status=el('select');status.setAttribute('aria-label','Template status');for(const value of ['active','archived','all']){const option=el('option',value[0].toUpperCase()+value.slice(1));option.value=value;status.append(option)}status.value=definitionStatus;status.onchange=()=>{definitionStatus=status.value;renderDefinitions().catch(showError)};list.append(status);
 const shown=(definitions||[]).filter(d=>definitionStatus==='all'||Boolean(d.Tombstoned)===(definitionStatus==='archived'));
 list.append(button('New process',()=>launchProcessEditor()),button('New team template',()=>launchTeamEditor()),button('Import legacy process',async()=>{const sequence=refreshSequence;const {openLegacyProcessImport}=await import('./process-import.js');if(sequence!==refreshSequence)return;openLegacyProcessImport({api,agents:(snapshot.agents||[]).filter(a=>a.Lifecycle!=='retired'),onSaved:renderDefinitions})}));
 for(const definition of shown){
  const card=el('article',undefined,'card');card.dataset.definition=definition.ID;card.append(el('h2',definition.Name),el('code',definition.ID),el('p',`${definition.Kind} · library revision ${definition.Revision}`,'muted'));
  card.append(button(definition.Tombstoned?'Restore template':'Archive template',()=>edit(definition.Tombstoned?'Restore template':'Archive template',[{name:'confirm',label:'Type '+definition.ID+' to confirm'}],async f=>{if(f.confirm!==definition.ID)throw new Error('Template ID does not match');await api('/v2/definitions/'+encodeURIComponent(definition.ID)+'/archive',{request_id:f.requestID,expected_revision:definition.Revision,archived:!definition.Tombstoned});await renderDefinitions()})));
  card.append(button('Inspect definition',async()=>{const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID));card.append(el('pre',result.Revision.Source))}));
  if(definition.Tombstoned){card.append(el('p','Archived from the library. Immutable revisions and exact pinned references remain available. Restore before editing or selecting it here.'));list.append(card);continue}
  if(definition.Kind==='process')card.append(button('Edit process',async()=>launchProcessEditor(await api('/v2/definitions/'+encodeURIComponent(definition.ID)))));
 if(definition.Kind==='process')card.append(button('Start process',async()=>{
   const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID));
   const revision=result.Revision;
   const {taskPerformers}=await import("./process-model.js");
   const performers=taskPerformers(revision.Process.Graph);
   const memberKeys=[...new Set(performers.map(p=>p.Agent?.MemberKey).filter(Boolean))];
   const agents=(snapshot.agents||[]).filter(a=>a.Lifecycle!=='retired');
   if(memberKeys.length&&!agents.length)throw new Error('Create an active agent before binding this process.');
   const bindingFields=memberKeys.map(key=>({name:'binding_'+key,label:'Agent for '+key,options:agents.map(a=>({value:a.ID,label:a.Name}))}));
   const needsWorkspace=performers.some(p=>p.Program);
   const fields=[{name:'workspace',label:'Workspace',required:needsWorkspace,options:[...(needsWorkspace?[]:[{value:'',label:'No shared workspace'}]),...(snapshot.workspaces||[]).filter(w=>w.State==='available').map(w=>({value:w.ID,label:w.Intent.Name||w.Observation.ActualPath||w.ID}))]},{name:'minutes',label:'Maximum run time in minutes',value:'60'},...parameterFields(revision.Parameters||[]),...bindingFields];
   edit('Start pinned process',fields,async f=>{
    const minutes=Number(f.minutes);if(!Number.isFinite(minutes)||minutes<=0||minutes>10080)throw new Error('Choose a run duration between 1 and 10080 minutes.');
    const programs=performers.filter(p=>p.Program).map(p=>p.Program.Profile);
    await api('/v2/processes',{request_id:f.requestID,id:f.requestID,start:{Definition:{DefinitionID:definition.ID,RevisionID:revision.ID,ContentHash:revision.ContentHash,Kind:'process'},Scope:{WorkspaceID:f.workspace},Parameters:parameterValues(revision.Parameters||[],f),PerformerBindings:Object.fromEntries(memberKeys.map(key=>[key,{Kind:'agent',Agent:{AgentID:f['binding_'+key]}}])),AuthorizedProgramProfiles:programs,Deadline:new Date(Date.now()+minutes*60000).toISOString()}});
   });
   $('editor-fields').prepend(...[revision.Process.Graph.Description,revision.Process.Graph.Doc].filter(Boolean).map(text=>el('pre',text)));
  }));
  if(definition.Kind==='team')card.append(button('Edit team template',async()=>launchTeamEditor(await api('/v2/definitions/'+encodeURIComponent(definition.ID)))));
  if(definition.Kind==='team')card.append(button('Deploy team',async()=>{
   const result=await api('/v2/definitions/'+encodeURIComponent(definition.ID)),revision=result.Revision;
   const spaces=(snapshot.workspaces||[]).filter(w=>w.State==='available');
   if(!spaces.length)throw new Error('Create a checkout in Workspaces before deploying this team.');
   const shared=revision.Team.WorkspacePolicy==='shared';
   const workspaceFields=(shared?[{Key:'shared',Name:'Shared team'}]:revision.Team.Members).map(member=>({name:'workspace_'+member.Key,label:member.Name+' workspace',options:spaces.map(w=>({value:w.ID,label:w.Intent.Name||w.Observation.ActualPath||w.ID}))}));
   edit('Deploy pinned team',[{name:'mission',label:'Mission',multiline:true},{name:'group',label:'Group',required:false,options:[{value:'',label:'Create a new group'},...(snapshot.groups||[]).map(g=>({value:g.ID,label:g.Name}))]},...workspaceFields,...parameterFields(revision.Parameters||[])],async f=>{
    const selection=key=>{const w=spaces.find(w=>w.ID===f['workspace_'+key]);if(!w)throw new Error('Select a current workspace.');return{WorkspaceID:w.ID,ExpectedRevision:w.Revision}};
    const workspaces=shared?{Shared:selection('shared')}:{Members:Object.fromEntries(revision.Team.Members.map(m=>[m.Key,selection(m.Key)]))};
    await api('/v2/teams/deploy',{request_id:f.requestID,deployment_id:f.requestID,instantiation:{Definition:{DefinitionID:definition.ID,RevisionID:revision.ID,ContentHash:revision.ContentHash,Kind:'team'},Mission:f.mission,Target:{Kind:f.group?'existing_group':'new_group',GroupID:f.group||'group_'+f.requestID},Workspaces:workspaces,Parameters:parameterValues(revision.Parameters||[],f)}});
    await renderDefinitions();
   });
  }));
  list.append(card);
 }
 if(!shown.length)empty(list,'No '+definitionStatus+' templates.');
 const deployments=await api('/v2/teams/deployments');if(sequence!==definitionLoadSequence)return;
 if(deployments?.length)list.append(el('h2','Deployed teams'));
 for(const result of deployments||[]){
  const d=result.Deployment,card=el('article',undefined,'card');card.dataset.deployment=d.ID;
  const phases=result.Phases||[],phase=phases[d.AdvisoryPhase];
  card.append(el('h3',d.Mission||d.ID),el('p',`${d.State} · group ${d.GroupID} · ${phase?`phase ${d.AdvisoryPhase+1}/${phases.length}: ${phase.Name}`:'no advisory process'} · revision ${d.Revision}`));
  if(phases.length){
   const details=el('details'),ordered=el('ol');details.append(el('summary','Advisory process and transitions'),el('p','Guidance only. Phases do not change permissions or gate work.'));
   for(const [index,p]of phases.entries()){const item=el('li');if(index===d.AdvisoryPhase)item.setAttribute('aria-current','step');item.append(el('strong',p.Name+(index===d.AdvisoryPhase?' · current':'')),el('p','Active roles: '+((p.Roles||[]).join(', ')||'none specified')));if(p.Criteria)item.append(el('pre',p.Criteria));ordered.append(item)}
   details.append(ordered);
   for(const move of d.PhaseHistory||[])details.append(el('p',`${move.From} → ${move.To} · ${move.ActorAgentID||move.ActorKind} · ${move.At}`));
   card.append(details);
   if(d.State!=='stopped'&&d.State!=='standing_down')card.append(button('Advance advisory phase',async()=>{
    const current=await api('/v2/teams/deployments/'+encodeURIComponent(d.ID)),currentPhases=current.Phases||[],currentDeployment=current.Deployment;
    if(!currentPhases.length)throw new Error('This deployment has no advisory phases.');
    edit('Advance advisory phase',[{name:'phase',label:'Enter phase',value:currentPhases[Math.min(currentDeployment.AdvisoryPhase+1,currentPhases.length-1)].Name,options:currentPhases.map(p=>({value:p.Name,label:p.Name}))}],async f=>{
     await api('/v2/teams/advance-phase',{request_id:f.requestID,deployment_id:d.ID,expected_revision:currentDeployment.Revision,phase:f.phase});await renderDefinitions();
    });
   }));
  }
  card.append(el('p',Object.entries(d.Members||{}).map(([key,id])=>`${key}: ${id}`).join(' · ')));
  card.append(el('p',`${Object.keys(d.Workspaces||{}).length} workspace bindings · ${(d.OwnedAutomationRuleIDs||[]).length} owned rhythms · ${(d.Rebriefs||[]).length} rebriefs`,'muted'));
  if(d.State!=='stopped'){
   card.append(button('Stand down',()=>edit('Stand down team',[{name:'reason',label:'Reason',multiline:true}],async f=>{
    await api('/v2/teams/stand-down',{request_id:f.requestID,deployment_id:d.ID,expected_revision:d.Revision,reason:f.reason});await renderDefinitions();
   })));
  }
  if(d.State==='ready'){
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
 const raw=parameterDefaultText(p);const value=raw===''?'':p.Type==='string'?JSON.parse(raw):raw;
 const field={name:'parameter_'+index,label:p.DisplayName?`${p.DisplayName} (${p.Name})`:p.Description||p.Name,help:[p.DisplayName?p.Description:'',p.Doc].filter(Boolean).join('\n\n'),value,required:p.Required,multiline:p.Type==='object'||p.Type==='array'};
 if(p.Type==='boolean')field.options=p.Required?['true','false']:['','true','false'];return field;
})}
function parameterValues(parameters,form){const values={};parameters.forEach((p,index)=>{
 const text=form['parameter_'+index];if(text===''&&!p.Required)return;
 let value=text;
 if(p.Type!=='string'){try{value=JSON.parse(text)}catch{throw new Error(`Enter a valid ${p.Type} for ${p.Name}.`)}}
 const valid=p.Type==='string'?typeof value==='string':p.Type==='number'?typeof value==='number'&&Number.isFinite(value):p.Type==='boolean'?typeof value==='boolean':p.Type==='array'?Array.isArray(value):value!==null&&typeof value==='object'&&!Array.isArray(value);
 if(!valid)throw new Error(`Enter a valid ${p.Type} for ${p.Name}.`);values[p.Name]=p.Type==='string'?value:new ExactJSON(text);
});return values}

let automationUI;
async function renderAutomation(){
 if(!automationUI){const {automationWorkspace}=await import('/automation.js');automationUI=automationWorkspace({api,el,button,edit,getSnapshot:()=>snapshot,openWork:async id=>{const result=await api('/v2/work/'+encodeURIComponent(id));await selectTab('work');$('work-list').replaceChildren(workCard(result));}})}
 await automationUI.render($('automation-list'));
}
