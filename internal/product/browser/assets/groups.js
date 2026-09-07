'use strict';
function renderGroupControls(snapshot,{host,el,button,edit,api,refresh}) {
 host.replaceChildren();const agents=snapshot.agents||[];
 for(const group of snapshot.groups||[]){
  const card=el('article',undefined,'card');card.append(el('h3',group.Name));
  const controls=el('div',undefined,'toolbar');
  controls.append(button('Edit name and members',()=>{
   const ordered=[...(group.Members||[]).map(id=>agents.find(a=>a.ID===id)).filter(Boolean),...agents.filter(a=>!group.Members?.includes(a.ID)&&a.Lifecycle!=='retired')];
   edit('Edit group',[{name:'name',label:'Group name',value:group.Name},{name:'members',label:'Members (removing a member does not stop or retire it)',multiple:true,required:false,value:group.Members||[],options:ordered.map(a=>({value:a.ID,label:a.Name+(a.ID===group.OwnerAgentID?' (owner)':'')}))}],f=>{
    if(group.OwnerAgentID&&!f.members.includes(group.OwnerAgentID))throw new Error('Change or clear the owner before removing that member.');
    return api('/v2/groups/'+encodeURIComponent(group.ID),{name:f.name,members:f.members,expected_revision:group.Revision},'PUT');
   },{skipUnchanged:true});
  }),button('Change owner',async()=>{
   const authority=await api('/v2/authority');
   const prior=(authority.Assignments||[]).find(a=>a.RoleID==='group_owner'&&a.Subject?.Kind==='agent'&&a.Resource?.Kind==='group_members'&&a.Subject?.AgentID===group.OwnerAgentID&&a.Resource?.GroupID===group.ID)?.Bounds||{};
   const lines=value=>(value||[]).join('\n');
   edit('Group owner and launch limits',[
    {name:'owner',label:'Owner (replaces this group’s owner role)',value:group.OwnerAgentID||'',required:false,options:[{value:'',label:'No owner'},...(group.Members||[]).map(id=>({value:id,label:agents.find(a=>a.ID===id)?.Name||id}))]},
    {name:'harnesses',label:'Allowed harnesses, one per line (empty means unrestricted)',multiline:true,required:false,value:lines(prior.Harnesses)},
    {name:'models',label:'Allowed models, one per line (empty means unrestricted)',multiline:true,required:false,value:lines(prior.Models)},
    {name:'roots',label:'Working directory roots, one per line (empty means unrestricted)',multiline:true,required:false,value:lines(prior.WorkingDirectoryRoots)},
    {name:'approvals',label:'Approval modes (empty means unrestricted)',multiple:true,required:false,value:prior.ApprovalModes||[],options:['supervised','automatic']},
    {name:'sandboxes',label:'Confinement modes (empty means unrestricted)',multiple:true,required:false,value:prior.SandboxModes||[],options:['read_only','workspace_write','unconfined']}
   ],f=>{const split=v=>v.split('\n').map(x=>x.trim()).filter(Boolean);return api('/v2/groups/'+encodeURIComponent(group.ID)+'/owner',{owner_agent_id:f.owner,expected_revision:group.Revision,bounds:{Harnesses:split(f.harnesses),Models:split(f.models),WorkingDirectoryRoots:split(f.roots),ApprovalModes:f.approvals,SandboxModes:f.sandboxes}},'PUT')},{skipUnchanged:true});
  }));card.append(controls);
  for(const [index,id]of (group.Members||[]).entries()){
   const row=el('div',undefined,'row');row.append(el('span',(agents.find(a=>a.ID===id)?.Name||id)+(id===group.OwnerAgentID?' · owner':'')));
   for(const [label,delta]of [['Move up',-1],['Move down',1]]){const move=button(label,async()=>{const members=[...group.Members],next=index+delta;[members[index],members[next]]=[members[next],members[index]];await api('/v2/groups/'+encodeURIComponent(group.ID),{name:group.Name,members,expected_revision:group.Revision},'PUT');await refresh()});move.disabled=index+delta<0||index+delta>=group.Members.length;row.append(move)}
   card.append(row);
  }host.append(card);
 }
 if(!snapshot.groups?.length)host.append(el('p','Create a group to organize agents.'));
}
