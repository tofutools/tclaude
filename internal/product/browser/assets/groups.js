'use strict';
function renderGroupControls(snapshot,{host,el,button,edit,api,refresh,presentation,attach,displayLabelFields}) {
 host.replaceChildren();const agents=snapshot.agents||[],groups=[...(snapshot.groups||[])],cards=new Map(),rank=new Map((presentation?.prefs.GroupOrder||[]).map((id,index)=>[id,index]));groups.sort((a,b)=>(rank.get(a.ID)??Number.MAX_SAFE_INTEGER)-(rank.get(b.ID)??Number.MAX_SAFE_INTEGER));
 const orderStatus=el('p',presentation?.saved.textContent||'','muted');orderStatus.id='group-order-status';orderStatus.setAttribute('role','status');host.append(orderStatus);if(presentation)host.append(button('Reload saved group order',()=>presentation.load(false)));
 for(const group of groups){
  const card=el('article',undefined,'card');card.dataset.groupId=group.ID;cards.set(group.ID,card);card.append(el('h3',group.Name),el('code',group.ID));
 const parent=groups.find(g=>g.ID===group.ParentGroupID);card.append(el('p',parent?'Parent: '+parent.Name+' · '+parent.ID:'Top level'));
 card.append(button('Move group',()=>{edit('Move group',[{name:'parent',label:'Parent group (organization only; no inherited authority)',required:false,value:group.ParentGroupID||'',options:[{value:'',label:'Top level'},...groups.filter(g=>g.ID!==group.ID).map(g=>({value:g.ID,label:g.Name+' · '+g.ID}))]}],f=>{return api('/v2/groups/'+encodeURIComponent(group.ID)+'/parent',{request_id:f.requestID,parent_group_id:f.parent,expected_revision:group.Revision},'PUT')},{skipUnchanged:true})}));
 const details=group.Details||{};appendGroupDetails(card,details,el);
 const activeMembers=(group.Members||[]).filter(id=>agents.some(a=>a.ID===id&&a.Lifecycle==='active')).length,cap=group.MaxActiveMembers||0;
 card.append(el('p',`${activeMembers} active direct members · ${cap?'limit '+cap:'no configured limit'}${cap&&activeMembers>cap?' · over limit':''}`));
  const controls=el('div',undefined,'toolbar');
 controls.append(button('Disband group',()=>{
  edit('Disband group',[{name:'confirm',label:'Type the exact group ID to disband: '+group.ID}],f=>{
   if(f.confirm!==group.ID)throw new Error('Enter the exact group ID shown above.');
   return api('/v2/groups/'+encodeURIComponent(group.ID)+'/disband',{request_id:f.requestID,expected_revision:group.Revision});
  });
  const fields=document.getElementById('editor-fields');
  fields.append(el('p','Removes this group and its membership. Group-target schedules are archived and disabled. Child groups become top-level groups. Agents, messages, work history and checkouts are retained. This cannot be undone; a new group has a new identity.'));
  fields.append(el('p','Stand down this group’s team deployments and settle its work runs and group shells first. To retire members too, use the roster Stop and Retire controls before disbanding.'));
  const members=el('ul');
  for(const id of group.Members||[]){const shared=groups.filter(g=>g.ID!==group.ID&&g.Members?.includes(id));members.append(el('li',(agents.find(a=>a.ID===id)?.Name||id)+' · '+id+(shared.length?' — also in '+shared.map(g=>g.Name+' · '+g.ID).join(', '):' — retained without this group')))}
  fields.append(el('p','Members retained:'),members);
 }));

 controls.append(button('Member limit',()=>edit('Group member limit',[{name:'limit',label:'Maximum active direct members (0 = no configured limit)',type:'number',value:String(cap)}],f=>{const value=Number(f.limit);if(!Number.isInteger(value)||value<0||value>2147483647)throw new Error('Enter a whole number from 0 to 2147483647');return api('/v2/groups/'+encodeURIComponent(group.ID)+'/capacity',{max_active_members:value,expected_revision:group.Revision},'PUT')},{skipUnchanged:true})));

 controls.append(button('Edit group details',()=>edit('Group details',[
  {name:'description',label:'Description',multiline:true,required:false,value:details.Description||''},
  {name:'mission',label:'Mission (descriptive; not sent to agents)',multiline:true,required:false,value:details.Mission||''},
  {name:'url',label:'Task or repository link (HTTP/HTTPS)',required:false,value:details.LinkURL||''},
  {name:'label',label:'Link label',required:false,value:details.LinkLabel||''}
 ],f=>api('/v2/groups/'+encodeURIComponent(group.ID)+'/details',{expected_revision:group.Revision,details:{Description:f.description,Mission:f.mission,LinkURL:f.url,LinkLabel:f.label}},'PUT'),{skipUnchanged:true})));

 controls.append(button('Save group as team template',async()=>{
  const capturedGroup=structuredClone(group),capturedAgents=structuredClone(agents);
  const {openTeamEditor,teamDraftFromGroup}=await import('/team-editor.js');
  if(!card.isConnected)return;
  const draft=teamDraftFromGroup(capturedGroup,capturedAgents);
  await openTeamEditor({api,draft,canOpen:()=>card.isConnected,onSaved:refresh});
 }));

 controls.append(button('Clone group',async()=>{
  const [current,profiles]=await Promise.all([api('/v2/groups/'+encodeURIComponent(group.ID)+'/configuration'),api('/v2/configuration-profiles')]);if(!card.isConnected)return;
  const members=(group.Members||[]).map(id=>agents.find(a=>a.ID===id)),active=members.filter(a=>a?.Lifecycle==='active');
  const available=ref=>!ref||profiles.some(p=>p.ID===ref.ProfileID&&!p.Archived),blocked=active.filter(a=>!available(a.ConfigurationProfile)),canCopy=!blocked.length&&members.every(Boolean),canDefault=available(current.Profile)&&(current.Profile||Object.keys(current.Environment||{}).length);
  edit('Clone group',[
   {name:'name',label:'New group name',value:group.Name+' copy'},
   {name:'members',label:'Member configurations',value:canCopy?'copy':'none',options:[...(canCopy?[{value:'copy',label:'Copy active members as new offline agents'}]:[]),{value:'none',label:'Create an empty group'}]},
   {name:'defaults',label:'Group launch default',value:'none',options:[{value:'none',label:'No default'},...(canDefault?[{value:'copy',label:current.Profile?'Copy selected configuration '+(profiles.find(p=>p.ID===current.Profile.ProfileID)?.Name||current.Profile.ProfileID)+' and group environment':'Copy group environment'}]:[])]},
   {name:'limit',label:'Maximum active direct members (0 = no configured limit)',type:'number',value:String(cap)}
  ],f=>{
   const limit=Number(f.limit),copy=f.members==='copy';if(!Number.isInteger(limit)||limit<0||limit>2147483647)throw new Error('Enter a whole number from 0 to 2147483647');
   if(copy&&members.some(a=>!a))throw new Error('Reload the group before copying its members.');
   return api('/v2/groups/'+encodeURIComponent(group.ID)+'/clone',{request_id:f.requestID,id:f.requestID,name:f.name,expected_group_revision:group.Revision,expected_default_revision:f.defaults==='copy'?current.Revision:0,expected_members:copy?Object.fromEntries(members.map(a=>[a.ID,a.Revision])):{},copy_members:copy,copy_default:f.defaults==='copy',max_active_members:limit});
  });
  document.getElementById('editor-fields').append(el('p','Creates a separate top-level group. Details and selected configurations are copied; retired members are skipped. Ownership, permissions, running work, messages and automation are not copied. Nothing starts.'));
  const preview=el('ul');for(const member of active)preview.append(el('li',member.Name+' · '+member.ID+(blocked.includes(member)?' — cannot copy: archived or unavailable configuration '+member.ConfigurationProfile.ProfileID:'')));document.getElementById('editor-fields').append(el('p','Active source members:'),preview);
  if(!canCopy)document.getElementById('editor-fields').append(el('p','Member copying is unavailable. Restore the listed archived configurations in Configurations or update those agents to active configurations, then reopen this dialog. You can create an empty group now.'));
  if(current.Profile&&canDefault)document.getElementById('editor-fields').append(el('p','New members use the selected configuration’s current settings, including later profile edits. Copied existing members retain their settings.'));
  if(current.Profile&&!canDefault)document.getElementById('editor-fields').append(el('p','The selected default '+current.Profile.ProfileID+' is archived or unavailable. Restore it before copying the default, or continue with no default.'));
 }));

 controls.append(button('Open group shell',async()=>{
  const config=await api('/v2/groups/'+encodeURIComponent(group.ID)+'/configuration');if(!card.isConnected)return;
  const workspaces=(snapshot.workspaces||[]).filter(w=>w.State==='available'&&w.Observation?.ActualPath);
  if(!workspaces.length)throw new Error('Create or register an available checkout in Workspaces first.');
  const sandbox=await sandboxSelectionInput(api);if(!card.isConnected)return;
  edit('Open group shell',[
   {name:'workspace',label:'Checkout',options:workspaces.map(w=>({value:w.ID,label:w.Observation.ActualPath+' · '+w.ID}))},
   {name:'environment',label:'Explicit shell environment overrides',environment:true,value:{},inherited:config.Environment||{}},
   sandbox.field
  ],async f=>{const workspace=workspaces.find(w=>w.ID===f.workspace);if(!workspace)throw new Error('Select a listed checkout.');return api('/v2/shells',{request_id:f.requestID,workspace_id:workspace.ID,expected_revision:workspace.Revision,sandbox:'unconfined',host_sandbox:await sandbox.read(f.host_sandbox),environment:f.environment,group:{GroupID:group.ID,Revision:group.Revision,ConfigurationRevision:config.Revision}})});
  document.getElementById('editor-fields').append(el('p','Uses this group’s environment plus your explicit overrides. Agent launch profiles do not apply. The shell retains these settings; later group edits do not change it.'));
 }));
 for(const execution of snapshot.executions||[]){
  if(execution.workload!=='shell'||execution.spec?.ShellGroup?.GroupID!==group.ID)continue;
  const row=el('div',undefined,'row');row.append(el('code',execution.id),el('span',execution.state+' · '+execution.spec.WorkingDirectory));
  if(!['exited','failed'].includes(execution.state))row.append(button('Attach group shell',()=>attach(execution)),button('Stop group shell',async id=>{await api('/v2/stop',{request_id:id,execution_id:execution.id,force:false});await refresh()}));
  card.append(row);
 }

 controls.append(button('Sandbox profile',()=>editSandboxDefaults({api,edit,group,canOpen:()=>card.isConnected,onSaved:refresh})));
 controls.append(button('Launch defaults',async()=>{
  const [current,profiles]=await Promise.all([api('/v2/groups/'+encodeURIComponent(group.ID)+'/configuration'),api('/v2/configuration-profiles')]);
  if(!card.isConnected)return;
  const choices=profiles.filter(p=>!p.Archived).map(p=>({key:'current:'+p.ID,label:p.Name+' · '+p.ID,ref:{ProfileID:p.ID,RevisionID:p.CurrentRevisionID}}));let value='';
  if(current.Profile){const exact=choices.find(p=>p.ref.ProfileID===current.Profile.ProfileID);if(exact)value=exact.key;else{value='pinned';choices.push({key:value,label:'Unavailable configuration '+current.Profile.ProfileID,ref:current.Profile})}}
  edit('Group launch defaults',[{name:'environment',label:'Group environment (saved configuration and explicit member values override matching names)',environment:true,value:current.Environment||{}},{name:'profile',label:'Saved configuration for new members (existing agents keep their settings)',required:false,value,options:[{value:'',label:'No group default'},...choices.map(p=>({value:p.key,label:p.label}))]}],async f=>{let profile=null;if(f.profile){const selected=choices.find(p=>p.key===f.profile);if(!selected)throw new Error('Select a listed configuration');profile=(await api('/v2/configuration-profiles/'+encodeURIComponent(selected.ref.ProfileID)+'?revision_id='+encodeURIComponent(selected.ref.RevisionID))).Revision.Ref}return api('/v2/groups/'+encodeURIComponent(group.ID)+'/configuration',{profile,environment:f.environment,expected_revision:current.Revision},'PUT')},{skipUnchanged:true});
 }),button('Create member from default',async()=>{
  const current=await api('/v2/groups/'+encodeURIComponent(group.ID)+'/configuration');if(!card.isConnected)return;if(!current.Profile)throw new Error('Choose a group launch default first.');
  const saved=await api('/v2/configuration-profiles/'+encodeURIComponent(current.Profile.ProfileID));if(!card.isConnected)return;
  edit('Create group member',[{name:'environment',label:'Explicit environment overrides',environment:true,value:{},inherited:{...current.Environment,...saved.Revision.Desired.Environment}},{name:'name',label:'Agent name · '+saved.Profile.Name,value:saved.Revision.Startup?.AgentName||saved.Profile.Name},...displayLabelFields(saved.Revision.Startup)],f=>api('/v2/groups/'+encodeURIComponent(group.ID)+'/agents',{request_id:f.requestID,id:f.requestID,name:f.name,labels:{Role:f.role_label,Description:f.description},environment:f.environment,expected_group_revision:group.Revision,expected_default_revision:current.Revision}));
  document.getElementById('editor-fields').append(el('p',saved.Revision.Desired.Harness+' · '+saved.Revision.Desired.Model+' · '+saved.Revision.Desired.WorkingDirectory+' — creates the agent and membership; start remains explicit.'));
 }));

 const siblings=groups.filter(g=>(g.ParentGroupID||'')===(group.ParentGroupID||'')),position=siblings.findIndex(g=>g.ID===group.ID);
 for(const [label,delta] of [['Move group earlier',-1],['Move group later',1]]){const move=button(label,async()=>{const ids=groups.map(g=>g.ID),a=ids.indexOf(group.ID),b=ids.indexOf(siblings[position+delta].ID);[ids[a],ids[b]]=[ids[b],ids[a]];await presentation.change({GroupOrder:ids})});move.disabled=!presentation?.loaded||position+delta<0||position+delta>=siblings.length;controls.append(move)}
  controls.append(button('Edit name and members',()=>{
   const ordered=[...(group.Members||[]).map(id=>agents.find(a=>a.ID===id)).filter(Boolean),...agents.filter(a=>!group.Members?.includes(a.ID)&&a.Lifecycle!=='retired')];
   edit('Edit group',[{name:'name',label:'Group name',value:group.Name},{name:'members',label:'Members (removing a member does not stop or retire it)',multiple:true,required:false,value:group.Members||[],options:ordered.map(a=>({value:a.ID,label:a.Name+((group.OwnerAgentIDs||[group.OwnerAgentID]).includes(a.ID)?' (owner)':'')}))}],f=>{
    if((group.OwnerAgentIDs||[group.OwnerAgentID]).filter(Boolean).some(id=>!f.members.includes(id)))throw new Error('Remove ownership before removing an owner from the group.');
    return api('/v2/groups/'+encodeURIComponent(group.ID),{name:f.name,members:f.members,expected_revision:group.Revision},'PUT');
   },{skipUnchanged:true});
  }),button('Change owner',async()=>{
   const authority=await api('/v2/authority');
   const prior=(authority.Assignments||[]).find(a=>a.RoleID==='group_owner'&&a.Subject?.Kind==='agent'&&a.Resource?.Kind==='group_members'&&a.Subject?.AgentID===group.OwnerAgentID&&a.Resource?.GroupID===group.ID)?.Bounds||{};
   const lines=value=>(value||[]).join('\n');
   const enabled=['Harnesses','Models','WorkingDirectoryRoots','ApprovalModes','SandboxModes'].every(key=>prior[key]?.length);
   edit('Group owners and shared launch limits',[
    {name:'owners',label:'Group owners',multiple:true,value:group.OwnerAgentIDs||(group.OwnerAgentID?[group.OwnerAgentID]:[]),required:false,options:(group.Members||[]).map(id=>({value:id,label:agents.find(a=>a.ID===id)?.Name||id}))},
    {name:'configuration',label:'Launch and configuration authority for all selected owners',value:enabled?'listed':'disabled',options:[{value:'disabled',label:'No launch or configuration changes'},{value:'listed',label:'Allow only the complete lists below'}]},
    {name:'harnesses',label:'Allowed harnesses, one per line (required for launch/configuration authority)',multiline:true,required:false,value:lines(prior.Harnesses)},
    {name:'models',label:'Allowed models, one per line (required for launch/configuration authority)',multiline:true,required:false,value:lines(prior.Models)},
    {name:'roots',label:'Working directory roots, one per line (required for launch/configuration authority)',multiline:true,required:false,value:lines(prior.WorkingDirectoryRoots)},
    {name:'approvals',label:'Approval modes (required for launch/configuration authority)',multiple:true,required:false,value:prior.ApprovalModes||[],options:['supervised','automatic','deny']},
    {name:'host_policies',label:'Allowed sandbox profiles',sandboxPolicies:true,value:prior.HostSandboxProfiles||[]},
    {name:'environments',label:'Exact allowed launch environments',environmentSets:true,value:prior.Environments||[]},
    {name:'sandboxes',label:'Confinement modes (required for launch/configuration authority)',multiple:true,required:false,value:prior.SandboxModes||[],options:['read_only','workspace_write','unconfined']}
   ],f=>{const split=v=>v.split('\n').map(x=>x.trim()).filter(Boolean);let bounds={};
    if(f.configuration==='listed'){bounds={Harnesses:split(f.harnesses),Models:split(f.models),WorkingDirectoryRoots:split(f.roots),ApprovalModes:f.approvals,SandboxModes:f.sandboxes};if(Object.values(bounds).some(values=>!values.length))throw new Error('Supply all five allow-lists, or choose no launch or configuration changes.');bounds.Environments=f.environments;bounds.HostSandboxProfiles=f.host_policies;}
    return api('/v2/groups/'+encodeURIComponent(group.ID)+'/owner',{owner_agent_ids:f.owners,expected_revision:group.Revision,bounds},'PUT')},{skipUnchanged:true});
  }));card.append(controls);
  for(const [index,id]of (group.Members||[]).entries()){
   const row=el('div',undefined,'row');row.append(el('span',(agents.find(a=>a.ID===id)?.Name||id)+((group.OwnerAgentIDs||[group.OwnerAgentID]).includes(id)?' · owner':'')));
   for(const [label,delta]of [['Move up',-1],['Move down',1]]){const move=button(label,async()=>{const members=[...group.Members],next=index+delta;[members[index],members[next]]=[members[next],members[index]];await api('/v2/groups/'+encodeURIComponent(group.ID),{name:group.Name,members,expected_revision:group.Revision},'PUT');await refresh()});move.disabled=index+delta<0||index+delta>=group.Members.length;row.append(move)}
   card.append(row);
  }
 }
 for(const group of groups){const card=cards.get(group.ID),parent=cards.get(group.ParentGroupID);if(parent&&parent!==card){let children=parent.querySelector(':scope > .group-children');if(!children){children=el('div',undefined,'group-children');children.style.marginInlineStart='1rem';parent.append(children)}children.append(card)}else host.append(card)}
 if(!snapshot.groups?.length)host.append(el('p','Create a group to organize agents.'));
}

function appendGroupDetails(host,details,el){
 for(const value of [details.Description,details.Mission])if(value){const p=el('p',value);p.style.whiteSpace='pre-wrap';host.append(p)}
 if(details.LinkURL&&/^https?:\/\//.test(details.LinkURL)){const link=el('a',details.LinkLabel||details.LinkURL);link.href=details.LinkURL;link.target='_blank';link.rel='noopener noreferrer';host.append(link)}
}
